//go:build windows

package procx

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

func configureCommand(cmd *exec.Cmd, detached bool) {
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP)
	if detached {
		// The auto-started daemon must not share the agent's console or die when
		// that console closes.
		flags |= windows.DETACHED_PROCESS
	}
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: flags}
}

func newProcessController(process *os.Process, detached bool) (processController, error) {
	if detached {
		// A daemon must outlive the MCP client that spawned it. Putting it in a
		// kill-on-close Job owned by that client would violate SPEC §8.
		return detachedProcessController{process: process}, nil
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(process.Pid),
	)
	if err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("OpenProcess(%d): %w", process.Pid, err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(job, handle); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("AssignProcessToJobObject(%d): %w", process.Pid, err)
	}
	return &windowsJobController{job: job}, nil
}

type detachedProcessController struct{ process *os.Process }

func (c detachedProcessController) Kill() error {
	if err := c.process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("killing detached process %d: %w", c.process.Pid, err)
	}
	return nil
}
func (detachedProcessController) Close() error { return nil }

type windowsJobController struct {
	mu  sync.Mutex
	job windows.Handle
}

func (c *windowsJobController) Kill() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job == 0 {
		return nil
	}
	if err := windows.TerminateJobObject(c.job, 1); err != nil {
		return fmt.Errorf("TerminateJobObject: %w", err)
	}
	return nil
}

func (c *windowsJobController) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job == 0 {
		return nil
	}
	err := windows.CloseHandle(c.job)
	c.job = 0
	if err != nil {
		return fmt.Errorf("closing Job Object: %w", err)
	}
	return nil
}
