//go:build !windows

package procx_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/procx"
	"golang.org/x/sys/unix"
)

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunnerRoundTripsStdio(t *testing.T) {
	script := writeScript(t, "while read line; do echo \"got:$line\"; done\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p, err := procx.NewRunner().Start(ctx, procx.Spec{Path: script})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Kill() }()

	if _, err := io.WriteString(p.Stdin(), "hello\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if got, want := strings.TrimSpace(line), "got:hello"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	_ = p.Stdin().Close()
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// SPEC-critical: an unread stderr pipe fills at 64 KiB and blocks the language
// server forever. Stderr must be a real, drainable stream.
func TestRunnerStderrIsDrainable(t *testing.T) {
	script := writeScript(t, "echo diagnostic-noise >&2\nexit 0\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p, err := procx.NewRunner().Start(ctx, procx.Spec{Path: script})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	got, err := io.ReadAll(p.Stderr())
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if strings.TrimSpace(string(got)) != "diagnostic-noise" {
		t.Fatalf("stderr = %q", got)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestRunnerWaitReportsExitFailure(t *testing.T) {
	script := writeScript(t, "exit 3\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p, err := procx.NewRunner().Start(ctx, procx.Spec{Path: script})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Wait(); err == nil {
		t.Fatal("Wait returned nil for an exit status of 3")
	}
}

func TestRunnerDirAndEnv(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, "pwd\necho \"$LANGER_PROBE\"\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := procx.NewRunner().Output(ctx, procx.Spec{
		Path: script,
		Dir:  dir,
		Env:  []string{"LANGER_PROBE=set", "PATH=/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("Output = %q, want two lines", out)
	}
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := filepath.EvalSymlinks(lines[0]); err != nil || got != wantDir {
		t.Fatalf("cwd = %q (%v), want %q", lines[0], err, wantDir)
	}
	if lines[1] != "set" {
		t.Fatalf("env LANGER_PROBE = %q, want %q", lines[1], "set")
	}
}

func TestOutputReturnsStdoutOnly(t *testing.T) {
	script := writeScript(t, "echo to-stdout\necho to-stderr >&2\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := procx.NewRunner().Output(ctx, procx.Spec{Path: script})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if strings.TrimSpace(string(out)) != "to-stdout" {
		t.Fatalf("Output = %q", out)
	}
}

func TestOutputHonoursContextDeadline(t *testing.T) {
	script := writeScript(t, "sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := procx.NewRunner().Output(ctx, procx.Spec{Path: script}); err == nil {
		t.Fatal("Output returned nil for a command that outlived its context")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Output blocked for %v after the context expired", elapsed)
	}
}

// typescript-language-server is a node wrapper script whose children survive a
// plain kill of the parent. Kill must take down the whole process group.
func TestKillTerminatesTheProcessGroup(t *testing.T) {
	script := writeScript(t, "sh -c 'sleep 60' &\necho $!\nsleep 60\n")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	p, err := procx.NewRunner().Start(ctx, procx.Spec{Path: script})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("parse grandchild pid %q: %v", line, err)
	}
	if err := syscall.Kill(grandchild, 0); err != nil {
		t.Fatalf("grandchild %d is not alive before the kill: %v", grandchild, err)
	}

	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	_ = p.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(grandchild, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the process-group kill (kill(pid,0) = %v)", grandchild, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestKillIsIdempotent(t *testing.T) {
	script := writeScript(t, "sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p, err := procx.NewRunner().Start(ctx, procx.Spec{Path: script})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("first Kill: %v", err)
	}
	_ = p.Wait()
	if err := p.Kill(); err != nil {
		t.Fatalf("second Kill: %v", err)
	}
}

func TestStartRejectsRelativePath(t *testing.T) {
	ctx := context.Background()
	if _, err := procx.NewRunner().Start(ctx, procx.Spec{Path: "sh"}); err == nil {
		t.Fatal("Start accepted a non-absolute path; Spec.Path must already be through Resolve")
	}
}

func TestDetachedProcessGetsItsOwnSession(t *testing.T) {
	script := writeScript(t, "sleep 5\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p, err := procx.NewRunner().Start(ctx, procx.Spec{Path: script, Detached: true})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Kill() }()

	sid, err := unix.Getsid(p.PID())
	if err != nil {
		t.Fatalf("Getsid: %v", err)
	}
	if sid != p.PID() {
		t.Fatalf("detached child session id = %d, want its own pid %d", sid, p.PID())
	}
}

func TestContextCancellationKillsTheProcess(t *testing.T) {
	script := writeScript(t, "sleep 60\n")
	ctx, cancel := context.WithCancel(context.Background())

	p, err := procx.NewRunner().Start(ctx, procx.Spec{Path: script})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()

	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = p.Kill()
		t.Fatal("cancelling the context did not stop the process")
	}
}
