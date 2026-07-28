//go:build !windows

package daemonctl

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0: existence check.
	err = p.Signal(syscall.Signal(0))
	return err == nil
}

func killProcessTree(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid")
	}
	// Daemon process group: kill the group first, then the pid.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		// ESRCH: already gone
		if err == syscall.ESRCH {
			return nil
		}
		// Fall through to SIGKILL
	}
	// Brief grace then hard kill.
	if processAlive(pid) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
	}
	return nil
}

func killAllLangerProcesses() (int, error) {
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		// Non-Linux: best-effort via pkill-like name match not available.
		// Kill by scanning is Linux-specific; other Unix: kill self-name via ps.
		return killAllLangerViaPS()
	}
	killed := 0
	var last error
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if !isLangerCmdline(string(cmdline)) {
			continue
		}
		if err := killProcessTree(pid); err != nil {
			last = err
			continue
		}
		killed++
	}
	return killed, last
}

func isLangerCmdline(cmdline string) bool {
	// /proc cmdline is NUL-separated.
	parts := strings.Split(cmdline, "\x00")
	if len(parts) == 0 {
		return false
	}
	base := filepath.Base(parts[0])
	return base == "langer" || strings.HasPrefix(base, "langer")
}

func killAllLangerViaPS() (int, error) {
	// macOS / BSD fallback: leave unimplemented hard-all without /proc.
	// Callers still have per-lock PID kill via StopAll --hard.
	return 0, fmt.Errorf("nuke is supported on Linux (/proc); use stop --all --hard for lock PIDs")
}
