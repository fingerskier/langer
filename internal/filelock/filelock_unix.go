//go:build !windows

package filelock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Try takes an exclusive, non-blocking advisory lock on file.
func Try(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return ErrContended
	}
	return fmt.Errorf("flock: %w", err)
}

// Unlock releases the advisory lock taken by Try.
func Unlock(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("flock unlock: %w", err)
	}
	return nil
}
