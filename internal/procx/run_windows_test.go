//go:build windows

package procx_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fingerskier/langer/internal/procx"
	"golang.org/x/sys/windows"
)

const helperEnv = "LANGER_PROCX_HELPER=1"

func TestWindowsRunnerRoundTripsStdio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := procx.NewRunner().Start(ctx, helperSpec(t, "echo"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Kill() }()

	if _, err := io.WriteString(p.Stdin(), "hello\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(line); got != "got:hello" {
		t.Fatalf("stdout = %q", got)
	}
	_ = p.Stdin().Close()
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsOutputHonoursContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := procx.NewRunner().Output(ctx, helperSpec(t, "sleep")); err == nil {
		t.Fatal("Output returned nil after its context deadline")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Output took %v after cancellation", elapsed)
	}
}

func TestWindowsKillTerminatesDescendantJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, err := procx.NewRunner().Start(ctx, helperSpec(t, "tree"))
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("descendant pid %q: %v", line, err)
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("opening descendant %d: %v", pid, err)
	}
	defer windows.CloseHandle(handle)
	if state, err := windows.WaitForSingleObject(handle, 0); err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("descendant was not alive before Kill: state=%d err=%v", state, err)
	}

	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = p.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := windows.WaitForSingleObject(handle, 0)
		if err == nil && state == windows.WAIT_OBJECT_0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d survived Job Object termination: state=%d err=%v", pid, state, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWindowsKillIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := procx.NewRunner().Start(ctx, helperSpec(t, "sleep"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = p.Wait()
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
}

func helperSpec(t *testing.T, mode string) procx.Spec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return procx.Spec{
		Path: exe,
		Args: []string{"-test.run=TestWindowsHelperProcess", "--", mode},
		Env:  append(os.Environ(), helperEnv),
	}
}

func TestWindowsHelperProcess(t *testing.T) {
	if os.Getenv("LANGER_PROCX_HELPER") != "1" {
		return
	}
	mode := ""
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	switch mode {
	case "echo":
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			fmt.Println("got:" + scanner.Text())
		}
	case "sleep":
		time.Sleep(time.Minute)
	case "tree":
		exe, err := os.Executable()
		if err != nil {
			os.Exit(20)
		}
		child := exec.Command(exe, "-test.run=TestWindowsHelperProcess", "--", "sleep")
		child.Env = append(os.Environ(), helperEnv)
		if err := child.Start(); err != nil {
			os.Exit(21)
		}
		fmt.Println(child.Process.Pid)
		time.Sleep(time.Minute)
	default:
		os.Exit(22)
	}
}
