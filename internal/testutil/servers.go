package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/fingerskier/langer/config"
)

// EnvDevtools points the integration-server locator somewhere other than the
// default install. Useful on a machine that keeps the language servers
// elsewhere; the tests are otherwise unchanged.
const EnvDevtools = "LANGER_DEVTOOLS_DIR"

// DevtoolsDir returns the directory holding the out-of-repo language servers.
//
// They live OUTSIDE the repository on purpose: installing TypeScript into
// testdata/ts-project/node_modules would make the workspace-local search
// succeed, and that is precisely the behaviour SPEC §9 forbids langer from
// relying on (testdata/README.md §1.9).
func DevtoolsDir() string {
	if dir := os.Getenv(EnvDevtools); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "langer-devtools")
}

// RequireLanguageServer returns the registry entry for a real language server,
// or skips the test with an explicit reason when it is not installed.
//
// Absence must be VISIBLE. A silent pass on a machine with no language servers
// would let M1's acceptance criteria rot undetected.
func RequireLanguageServer(t *testing.T, name string) config.LanguageServer {
	t.Helper()

	devtools := DevtoolsDir()
	if devtools == "" {
		t.Skipf("cannot locate the language server devtools directory; set %s", EnvDevtools)
	}
	bin := filepath.Join(devtools, "node_modules", ".bin")

	switch name {
	case "typescript":
		command := platformCommand(filepath.Join(bin, "typescript-language-server"))
		requireFile(t, command, name)

		// typescript-language-server 5.3.0 cannot start without an explicit
		// tsserver.path against these fixtures (testdata/README.md §1.9). Note
		// the ts5/ segment: the sibling TypeScript 7.0.2 preview install ships
		// no tsserver.js at all.
		tsserver := filepath.Join(devtools, "ts5", "node_modules", "typescript", "lib", "tsserver.js")
		requireFile(t, tsserver, name)

		return config.LanguageServer{
			Name:                  "typescript",
			Command:               command,
			Args:                  []string{"--stdio"},
			FileExtensions:        []string{".ts", ".tsx", ".js", ".jsx"},
			RootMarkers:           []string{"tsconfig.json", "package.json"},
			InitializationOptions: map[string]any{"tsserver": map[string]any{"path": tsserver}},
		}

	case "python":
		command := platformCommand(filepath.Join(bin, "pyright-langserver"))
		requireFile(t, command, name)

		return config.LanguageServer{
			Name:           "python",
			Command:        command,
			Args:           []string{"--stdio"},
			FileExtensions: []string{".py", ".pyi"},
			RootMarkers:    []string{"pyrightconfig.json", "pyproject.toml"},
		}

	default:
		t.Fatalf("testutil: no integration server registered under %q", name)
		return config.LanguageServer{}
	}
}

func platformCommand(path string) string {
	if runtime.GOOS == "windows" {
		return path + ".cmd"
	}
	return path
}

func requireFile(t *testing.T, path, server string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("integration server %q unavailable: %s not found (%v); install it under %s",
			server, path, err, DevtoolsDir())
	}
}
