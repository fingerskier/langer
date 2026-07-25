// Command langer is the LSP-MCP bridge: a single binary that hosts both the
// MCP frontend and the workspace daemon.
//
// The v0.1 CLI is deliberately constrained to the three subcommands in
// SPEC §10. Anything else — HTTP transport, per-project databases, extra
// subcommands — is out of scope and must not be added here.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fingerskier/langer/config"
)

// Exit codes. Usage problems are distinguishable from runtime failures so a
// supervisor can tell "you invoked me wrong" from "I broke".
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// commands is the complete v0.1 subcommand set (SPEC §10).
var commands = []string{"mcp", "daemon", "status"}

const usageText = `langer — compiler-accurate code intelligence over MCP

Usage:
  langer mcp --stdio        MCP frontend on stdio (auto-starts the workspace daemon)
  langer daemon <root>      run the workspace daemon explicitly (normally auto-started)
  langer status             daemon and index status for the current workspace

Flags:
  -h, --help                show this help

Environment:
  ` + config.EnvConfigPath + `       path to config.toml (default ~/.config/lsp-mcp/config.toml)
  ` + config.EnvDatabasePath + `           path to the SQLite index (default ~/.local/share/lsp-mcp/index.db)
  ` + config.EnvLogLevel + `         one of debug, info, warn, error (default info)
`

// errHelp is the sentinel meaning "the user asked for help" — printed to
// stdout with a zero exit, unlike a usage error.
var errHelp = errors.New("help requested")

// usageError marks a caller mistake so run can pick exit code 2.
type usageError struct{ err error }

func (u usageError) Error() string { return u.err.Error() }
func (u usageError) Unwrap() error { return u.err }

func usagef(format string, args ...any) error {
	return usageError{err: fmt.Errorf(format, args...)}
}

// invocation is a fully parsed and validated command line.
type invocation struct {
	// Command is one of the entries in commands.
	Command string
	// Stdio is set by `mcp --stdio`. stdio is the only v0.1 transport.
	Stdio bool
	// Root is the absolute workspace root for `daemon <root>`. Workspaces are
	// identified by absolute path (SPEC §3.2), so it is resolved here.
	Root string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's testable body: no globals, no os.Exit, all output injected.
func run(args []string, stdout, stderr io.Writer) int {
	inv, err := parse(args)
	if err != nil {
		if errors.Is(err, errHelp) {
			fmt.Fprint(stdout, usageText)
			return exitOK
		}
		fmt.Fprintf(stderr, "langer: %v\n\n%s", err, usageText)
		var ue usageError
		if errors.As(err, &ue) {
			return exitUsage
		}
		return exitFailure
	}

	switch inv.Command {
	case "mcp":
		err = runMCP(inv, stdout, stderr)
	case "daemon":
		err = runDaemon(inv, stdout, stderr)
	case "status":
		err = runStatus(inv, stdout)
	default:
		// parse guarantees this is unreachable.
		err = fmt.Errorf("unhandled command %q", inv.Command)
	}
	if err != nil {
		fmt.Fprintf(stderr, "langer %s: %v\n", inv.Command, err)
		return exitFailure
	}
	return exitOK
}

// parse turns raw arguments (excluding the program name) into an invocation.
// It never writes to stdout or stderr; the caller decides where diagnostics go.
func parse(args []string) (*invocation, error) {
	if len(args) == 0 {
		return nil, usagef("no subcommand given; expected one of %s", strings.Join(commands, ", "))
	}

	switch args[0] {
	case "-h", "--help", "help":
		return nil, errHelp
	case "mcp":
		return parseMCP(args[1:])
	case "daemon":
		return parseDaemon(args[1:])
	case "status":
		return parseStatus(args[1:])
	default:
		return nil, usagef("unknown subcommand %q; expected one of %s", args[0], strings.Join(commands, ", "))
	}
}

// newFlagSet builds a flag set that reports errors instead of printing them and
// calling os.Exit, so parse stays a pure function.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

func parseMCP(args []string) (*invocation, error) {
	fs := newFlagSet("mcp")
	stdio := fs.Bool("stdio", false, "serve MCP over stdio")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errHelp
		}
		return nil, usagef("mcp: %v", err)
	}
	if fs.NArg() > 0 {
		return nil, usagef("mcp: unexpected argument %q", fs.Arg(0))
	}
	if !*stdio {
		// stdio is the only transport in v0.1 (SPEC §4.1); requiring the flag
		// keeps the invocation explicit and forward-compatible.
		return nil, usagef("mcp: --stdio is required")
	}
	return &invocation{Command: "mcp", Stdio: true}, nil
}

func parseDaemon(args []string) (*invocation, error) {
	fs := newFlagSet("daemon")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errHelp
		}
		return nil, usagef("daemon: %v", err)
	}
	switch fs.NArg() {
	case 0:
		return nil, usagef("daemon: a workspace root is required")
	case 1:
	default:
		return nil, usagef("daemon: one workspace root expected, got %d", fs.NArg())
	}

	root, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return nil, usagef("daemon: resolving %q: %v", fs.Arg(0), err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, usagef("daemon: workspace root %s: %v", root, err)
	}
	if !info.IsDir() {
		return nil, usagef("daemon: workspace root %s is not a directory", root)
	}
	return &invocation{Command: "daemon", Root: root}, nil
}

func parseStatus(args []string) (*invocation, error) {
	fs := newFlagSet("status")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errHelp
		}
		return nil, usagef("status: %v", err)
	}
	if fs.NArg() > 0 {
		return nil, usagef("status: unexpected argument %q", fs.Arg(0))
	}
	return &invocation{Command: "status"}, nil
}

// runMCP is the M4 entry point. Stubbed for M0.
func runMCP(_ *invocation, _, _ io.Writer) error {
	return errors.New("MCP frontend not implemented yet (milestone M4)")
}

// runDaemon is the M2 entry point. Stubbed for M0.
func runDaemon(_ *invocation, _, _ io.Writer) error {
	return errors.New("daemon not implemented yet (milestone M2)")
}

// runStatus reports what M0 can actually know: the resolved configuration and
// where the daemon's socket and index live. Live daemon and index reporting
// arrive with M2 and M3.
func runStatus(_ *invocation, stdout io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	configPath, err := config.ConfigPath()
	if err != nil {
		return err
	}
	configNote := ""
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		configNote = "  (not found — using defaults)"
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "workspace:  %s\n", cwd)
	fmt.Fprintf(stdout, "config:     %s%s\n", configPath, configNote)
	fmt.Fprintf(stdout, "database:   %s\n", cfg.DatabasePath)
	fmt.Fprintf(stdout, "socket:     %s\n", cfg.SocketPath)
	fmt.Fprintf(stdout, "log level:  %s\n", cfg.LogLevel)

	if len(cfg.LanguageServers) == 0 {
		fmt.Fprintf(stdout, "servers:    none configured\n")
	} else {
		for _, ls := range cfg.LanguageServers {
			fmt.Fprintf(stdout, "server:     %s -> %s %s\n", ls.Name, ls.Command, strings.Join(ls.Args, " "))
		}
	}
	fmt.Fprintf(stdout, "daemon:     unknown (daemon IPC arrives in milestone M2)\n")
	return nil
}
