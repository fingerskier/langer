package procx

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/fingerskier/langer/protocol"
)

// Spec describes a child process to start. Path is ABSOLUTE and has already
// been through Resolver.Resolve — Start refuses anything else, so there is no
// second, unguarded route to exec.
type Spec struct {
	Path string
	Args []string
	Dir  string
	Env  []string
	// Detached starts the child in a new session (setsid) so it survives the
	// parent's death. Used only for the daemon spawn (SPEC §8).
	Detached bool
}

// Process is a running child, always in its own process group.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Stderr MUST be drained for the lifetime of the process. An unread pipe
	// fills at 64 KiB and then blocks the language server forever.
	Stderr() io.Reader
	// Wait returns when the process exits. Safe to call concurrently with Kill
	// and safe to call more than once; every caller sees the same result.
	Wait() error
	// Kill terminates the whole process group. Idempotent.
	Kill() error
	PID() int
}

// Runner spawns processes.
type Runner interface {
	Start(ctx context.Context, spec Spec) (Process, error)
	// Output runs a short-lived command to completion and returns stdout.
	Output(ctx context.Context, spec Spec) ([]byte, error)
}

// NewRunner returns the production Runner.
func NewRunner() Runner { return runner{} }

type runner struct{}

func (runner) Start(ctx context.Context, spec Spec) (Process, error) {
	if err := checkSpec(spec); err != nil {
		return nil, err
	}

	// Own pipes rather than Cmd.StdoutPipe: os/exec closes the pipes it creates
	// inside Wait, which races any reader still draining the tail of a language
	// server's output. With *os.File ends we control the lifetime ourselves.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "stdin pipe: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW)
		return nil, protocol.NewErrorf(protocol.ErrInternal, "stdout pipe: %v", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW, stdoutR, stdoutW)
		return nil, protocol.NewErrorf(protocol.ErrInternal, "stderr pipe: %v", err)
	}

	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.SysProcAttr = sysProcAttr(spec.Detached)

	if err := cmd.Start(); err != nil {
		closeAll(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
		return nil, protocol.NewErrorf(protocol.ErrInternal, "starting %s: %v", spec.Path, err)
	}
	// The child holds its own descriptors now. Dropping ours is what makes the
	// reader see EOF when the child exits.
	closeAll(stdinR, stdoutW, stderrW)

	p := &process{
		cmd:    cmd,
		stdin:  stdinW,
		stdout: stdoutR,
		stderr: stderrR,
		done:   make(chan struct{}),
	}

	// One goroutine owns Wait; every caller of p.Wait reads its result. A second
	// goroutine turns context cancellation into a process-group kill.
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Kill()
		case <-p.done:
		}
	}()

	return p, nil
}

func (r runner) Output(ctx context.Context, spec Spec) ([]byte, error) {
	if err := checkSpec(spec); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.SysProcAttr = sysProcAttr(spec.Detached)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killGroup(cmd.Process.Pid)
	}

	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), protocol.NewErrorf(protocol.ErrInternal, "running %s: %v", spec.Path, err)
	}
	return stdout.Bytes(), nil
}

func checkSpec(spec Spec) error {
	if !filepath.IsAbs(spec.Path) {
		return protocol.NewErrorf(protocol.ErrInternal,
			"Spec.Path %q is not absolute; every command must go through Resolver.Resolve first", spec.Path)
	}
	return nil
}

func sysProcAttr(detached bool) *syscall.SysProcAttr {
	if detached {
		// A new session implies a new process group led by the child.
		return &syscall.SysProcAttr{Setsid: true}
	}
	return &syscall.SysProcAttr{Setpgid: true}
}

type process struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	stderr *os.File

	done    chan struct{}
	waitErr error

	killOnce sync.Once
	killErr  error
}

func (p *process) Stdin() io.WriteCloser { return p.stdin }
func (p *process) Stdout() io.Reader     { return p.stdout }
func (p *process) Stderr() io.Reader     { return p.stderr }
func (p *process) PID() int              { return p.cmd.Process.Pid }

func (p *process) Wait() error {
	<-p.done
	// Reads are finished once the process is gone; drop our ends so a crashed
	// server cannot leak three descriptors per restart.
	closeAll(p.stdin, p.stdout, p.stderr)
	return p.waitErr
}

func (p *process) Kill() error {
	select {
	case <-p.done:
		return nil // already exited; killing again is a no-op, not an error
	default:
	}
	p.killOnce.Do(func() { p.killErr = killGroup(p.cmd.Process.Pid) })
	return p.killErr
}

// killGroup signals the whole process group. Both Setpgid and Setsid make the
// child its own group leader, so the group id equals its pid.
func killGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	switch err {
	case nil, syscall.ESRCH:
		return nil
	case syscall.EPERM:
		// The group is gone and the pid was recycled into a group we do not
		// own; killing the pid directly is the safe fallback.
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return protocol.NewErrorf(protocol.ErrInternal, "killing pid %d: %v", pid, err)
		}
		return nil
	default:
		return protocol.NewErrorf(protocol.ErrInternal, "killing process group %d: %v", pid, err)
	}
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}
