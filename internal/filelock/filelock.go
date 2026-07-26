// Package filelock provides the non-blocking advisory locks used to coordinate
// one daemon and one spawn attempt per workspace.
package filelock

import "errors"

// ErrContended means another process already owns the requested lock.
var ErrContended = errors.New("file lock is held by another process")
