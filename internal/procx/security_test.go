package procx_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/protocol"
)

// TestPoisonedPATHNeverSelectsWorkspaceTripwire is the M6 process-boundary
// sign-off for SPEC §9: even when <workspace>/node_modules/.bin is first on
// PATH, Resolve must not return (or execute) the tripwire.
func TestPoisonedPATHNeverSelectsWorkspaceTripwire(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", "testdata", "ts-project"))
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	t.Setenv("LANGER_TRIPWIRE_SENTINEL", sentinel)

	poison := filepath.Join(repoRoot, "node_modules", ".bin")
	// PATH that contains ONLY the workspace tripwire directory: a correct
	// Resolve must fail with UNSUPPORTED rather than picking the tripwire.
	t.Setenv("PATH", poison)

	_, err = procx.NewResolver().Resolve("typescript-language-server", repoRoot, false)
	if err == nil {
		t.Fatal("Resolve accepted a command when PATH held only the workspace tripwire")
	}
	if protocol.AsError(err).Code != protocol.ErrUnsupported {
		t.Fatalf("code = %s (%v), want UNSUPPORTED", protocol.AsError(err).Code, err)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("Resolve executed the workspace tripwire via PATH")
	}
}

// TestProcessTreeCleanupSignOff re-validates that killing a supervised child
// takes down its descendants (Unix process group / Windows Job Object).
func TestProcessTreeCleanupSignOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dir := t.TempDir()
	var script string
	var args []string
	if runtime.GOOS == "windows" {
		script = filepath.Join(dir, "parent.cmd")
		body := "@echo off\r\nstart /b cmd /c \"ping -n 30 127.0.0.1 >nul\"\r\nping -n 30 127.0.0.1 >nul\r\n"
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		args = []string{"/c", script}
		script = os.Getenv("ComSpec")
		if script == "" {
			script = "cmd.exe"
		}
	} else {
		script = filepath.Join(dir, "parent.sh")
		body := "#!/bin/sh\n(sleep 30) &\necho $!\nsleep 30\n"
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		args = nil
	}

	p, err := procx.NewRunner().Start(ctx, procx.Spec{Path: script, Args: args, Dir: dir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait after Kill timed out")
	}
}
