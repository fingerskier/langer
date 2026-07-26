//go:build !windows

package procx

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/fingerskier/langer/protocol"
)

func configureCommand(cmd *exec.Cmd, detached bool) {
	if detached {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func newProcessController(process *os.Process, _ bool) (processController, error) {
	return unixProcessController{pid: process.Pid}, nil
}

type unixProcessController struct{ pid int }

func (c unixProcessController) Kill() error { return killGroup(c.pid) }
func (unixProcessController) Close() error  { return nil }

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
