package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/protocol"
)

// isolate points HOME at a scratch dir and clears the langer env overrides so
// `langer status` never reads the developer's real config.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv(config.EnvConfigPath, "")
	t.Setenv(config.EnvDatabasePath, "")
	t.Setenv(config.EnvLogLevel, "")
	return home
}

// TestUsageListsCoreSubcommands pins the documented CLI surface: SPEC §10's
// mcp/daemon/status plus the managed-tools subcommand from plan/PLAN.md.
func TestUsageListsCoreSubcommands(t *testing.T) {
	want := []string{
		"langer mcp --stdio",
		"langer daemon <root>",
		"langer status",
		"langer tools list",
		"langer stop",
	}
	for _, line := range want {
		if !strings.Contains(usageText, line) {
			t.Errorf("usage does not document %q:\n%s", line, usageText)
		}
	}

	for _, forbidden := range []string{"--http", "langer serve", "langer index", "langer reindex", "langer version"} {
		if strings.Contains(usageText, forbidden) {
			t.Errorf("usage mentions out-of-scope %q:\n%s", forbidden, usageText)
		}
	}

	if got, want := len(commands), 5; got != want {
		t.Errorf("len(commands) = %d, want %d: %v", got, want, commands)
	}
}

func TestParseStop(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name    string
		args    []string
		wantAll bool
		wantHard bool
		wantNuke bool
		wantRoot bool
		wantErr bool
	}{
		{name: "cwd", args: []string{"stop"}},
		{name: "root", args: []string{"stop", root}, wantRoot: true},
		{name: "all", args: []string{"stop", "--all"}, wantAll: true},
		{name: "hard", args: []string{"stop", "--hard"}, wantHard: true},
		{name: "all hard", args: []string{"stop", "--all", "--hard"}, wantAll: true, wantHard: true},
		{name: "nuke", args: []string{"stop", "--nuke"}, wantAll: true, wantHard: true, wantNuke: true},
		{name: "all with root", args: []string{"stop", "--all", root}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := parse(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if inv.Command != "stop" {
				t.Fatalf("command = %q", inv.Command)
			}
			if inv.StopAll != tt.wantAll || inv.StopHard != tt.wantHard || inv.StopNuke != tt.wantNuke {
				t.Fatalf("flags all=%v hard=%v nuke=%v want all=%v hard=%v nuke=%v",
					inv.StopAll, inv.StopHard, inv.StopNuke, tt.wantAll, tt.wantHard, tt.wantNuke)
			}
			if tt.wantRoot && inv.Root != root {
				t.Fatalf("root = %q", inv.Root)
			}
		})
	}
}

func TestParse(t *testing.T) {
	root := t.TempDir()

	tests := []struct {
		name    string
		args    []string
		want    invocation
		wantErr bool
	}{
		{
			name: "mcp with stdio",
			args: []string{"mcp", "--stdio"},
			want: invocation{Command: "mcp", Stdio: true},
		},
		{
			name: "mcp with single-dash stdio",
			args: []string{"mcp", "-stdio"},
			want: invocation{Command: "mcp", Stdio: true},
		},
		{
			name:    "mcp requires a transport flag",
			args:    []string{"mcp"},
			wantErr: true,
		},
		{
			name:    "mcp rejects deferred http transport",
			args:    []string{"mcp", "--http"},
			wantErr: true,
		},
		{
			name:    "mcp rejects extra operands",
			args:    []string{"mcp", "--stdio", "extra"},
			wantErr: true,
		},
		{
			name: "daemon with root",
			args: []string{"daemon", root},
			want: invocation{Command: "daemon", Root: root},
		},
		{
			name:    "daemon requires a root",
			args:    []string{"daemon"},
			wantErr: true,
		},
		{
			name:    "daemon rejects two roots",
			args:    []string{"daemon", root, root},
			wantErr: true,
		},
		{
			name:    "daemon rejects a nonexistent root",
			args:    []string{"daemon", filepath.Join(root, "nope")},
			wantErr: true,
		},
		{
			name: "status",
			args: []string{"status"},
			want: invocation{Command: "status"},
		},
		{
			name:    "status takes no operands",
			args:    []string{"status", "extra"},
			wantErr: true,
		},
		{
			name:    "unknown subcommand",
			args:    []string{"frobnicate"},
			wantErr: true,
		},
		{
			name:    "no subcommand",
			args:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parse(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parse(%q) = %+v, want an error", tt.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse(%q) returned error: %v", tt.args, err)
			}
			if *got != tt.want {
				t.Errorf("parse(%q) = %+v, want %+v", tt.args, *got, tt.want)
			}
		})
	}
}

// TestParseResolvesRootToAbsolute keeps every downstream workspace key
// canonical: SPEC §3.2 identifies a workspace by its absolute root path.
func TestParseResolvesRootToAbsolute(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	got, err := parse([]string{"daemon", "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got.Root) {
		t.Errorf("Root = %q, want an absolute path", got.Root)
	}
	// t.TempDir can hand back a symlinked path (/var → /private/var on macOS),
	// so compare the tail rather than the whole string.
	if !strings.HasSuffix(got.Root, string(filepath.Separator)+"repo") {
		t.Errorf("Root = %q, want it to end in /repo", got.Root)
	}
}

func TestHelpFlagsExitZeroOnStdout(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Errorf("run(%q) = %d, want 0 (stderr: %s)", args, code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "langer status") {
				t.Errorf("help did not go to stdout:\n%s", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("help wrote to stderr:\n%s", stderr.String())
			}
		})
	}
}

func TestUsageErrorsGoToStderrWithExitTwo(t *testing.T) {
	for _, args := range [][]string{nil, {"frobnicate"}, {"mcp"}, {"daemon"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != exitUsage {
				t.Errorf("run(%q) = %d, want %d", args, code, exitUsage)
			}
			if stderr.Len() == 0 {
				t.Errorf("run(%q) wrote no diagnostic to stderr", args)
			}
			if stdout.Len() != 0 {
				t.Errorf("run(%q) polluted stdout:\n%s", args, stdout.String())
			}
		})
	}
}

// TestStatusRuns covers the M0 acceptance criterion "`langer status` runs".
func TestStatusRuns(t *testing.T) {
	home := isolate(t)
	original := queryDaemonStatus
	queryDaemonStatus = func(context.Context, *config.Config, string) (protocol.IndexStatusResult, error) {
		return protocol.IndexStatusResult{State: protocol.IndexReady, FilesIndexed: 7, FilesTotal: 7}, nil
	}
	t.Cleanup(func() { queryDaemonStatus = original })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(status) = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		filepath.Join(home, ".config", "lsp-mcp", "config.toml"),
		filepath.Join(home, ".local", "share", "lsp-mcp", "index.db"),
		filepath.Join(home, ".local", "share", "lsp-mcp", "daemon.sock"),
		"daemon:     connected",
		"index:      ready (7/7 files)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output is missing %q:\n%s", want, out)
		}
	}
}

// TestStatusReportsBadConfig proves a broken config surfaces as a failure
// rather than silently reverting to defaults.
func TestStatusReportsBadConfig(t *testing.T) {
	isolate(t)
	bad := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(bad, []byte("log_level = \"chatty\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfigPath, bad)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"status"}, &stdout, &stderr); code != exitFailure {
		t.Errorf("run(status) with a bad config = %d, want %d", code, exitFailure)
	}
	if stderr.Len() == 0 {
		t.Error("no diagnostic written to stderr")
	}
}

func TestMCPSubcommandRunsTheFrontendWithoutPollutingInjectedStdout(t *testing.T) {
	isolate(t)
	original := serveMCP
	called := false
	serveMCP = func(context.Context, *config.Config, string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { serveMCP = original })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"mcp", "--stdio"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("run(mcp) = %d, want %d: %s", code, exitOK, stderr.String())
	}
	if !called {
		t.Fatal("MCP frontend was not invoked")
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("wrapper polluted output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// TestDaemonSubcommandServesAndDrains is the CLI half of M2: `langer daemon
// <root>` really listens, and it stops cleanly when asked to drain.
func TestDaemonSubcommandServesAndDrains(t *testing.T) {
	isolate(t)
	cfg := useShortSocketDir(t)
	root := t.TempDir()

	var stdout, stderr bytes.Buffer
	exit := make(chan int, 1)
	go func() { exit <- run([]string{"daemon", root}, &stdout, &stderr) }()

	socket := waitForSocket(t, cfg, root)
	drainDaemon(t, socket)

	select {
	case code := <-exit:
		if code != exitOK {
			t.Errorf("run(daemon) = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon did not exit after being asked to drain")
	}
	if stdout.Len() != 0 {
		t.Errorf("the daemon wrote to stdout:\n%s", stdout.String())
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket %s outlived the daemon (err=%v)", socket, err)
	}
}

// TestDaemonSubcommandReportsStartupFailures: a daemon that cannot bind must
// say so and exit non-zero, not linger.
func TestDaemonSubcommandReportsStartupFailures(t *testing.T) {
	isolate(t)
	// /dev/null is not a directory, so the runtime directory cannot be created.
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("socket_path = \"/dev/null/langer/daemon.sock\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfigPath, cfgPath)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"daemon", t.TempDir()}, &stdout, &stderr); code != exitFailure {
		t.Fatalf("run(daemon) = %d, want %d", code, exitFailure)
	}
	if stderr.Len() == 0 {
		t.Error("the daemon failed silently")
	}
}

// useShortSocketDir points the config at a socket directory short enough for
// sockaddr_un. macOS's per-user TMPDIR plus a test name routinely exceeds the
// ~104 byte limit, and the daemon refuses rather than letting the kernel
// truncate.
func useShortSocketDir(t *testing.T) *config.Config {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "langer")
	if err != nil {
		t.Skipf("cannot create a short temporary directory under /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	body := "socket_path = " + strconv.Quote(filepath.Join(dir, "daemon.sock")) + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfigPath, cfgPath)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func waitForSocket(t *testing.T, cfg *config.Config, root string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	socket, err := cfg.WorkspaceSocketPath(resolved)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			return socket
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the daemon never bound %s", socket)
	return ""
}

// drainDaemon speaks the wire protocol directly: SPEC §3.1's drain is a
// lifecycle request, not a Service method, so there is no client API for it.
func drainDaemon(t *testing.T, socket string) {
	t.Helper()
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		t.Fatalf("dialling %s: %v", socket, err)
	}
	defer conn.Close()

	codec := protocol.NewCodec(conn)
	params, err := json.Marshal(protocol.DrainParams{Session: "cli-test", Reason: "test over"})
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.WriteRequest(protocol.NewRequest(1, protocol.MethodDrain, params)); err != nil {
		t.Fatalf("writing drain: %v", err)
	}
	resp, err := codec.ReadResponse()
	if err != nil {
		t.Fatalf("reading the drain response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("drain failed: %v", resp.Error)
	}
}
