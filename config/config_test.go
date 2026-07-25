package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// isolate points HOME and the XDG variables at a scratch dir and clears every
// langer env override, so tests never read the developer's real config.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv(EnvConfigPath, "")
	t.Setenv(EnvDatabasePath, "")
	t.Setenv(EnvLogLevel, "")
	return home
}

// TestDefaultsWhenNoConfigFile covers SPEC §6: XDG default locations must apply
// with no config file present at all.
func TestDefaultsWhenNoConfigFile(t *testing.T) {
	home := isolate(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with no config file returned error: %v", err)
	}

	want := &Config{
		DatabasePath:    filepath.Join(home, ".local", "share", "lsp-mcp", "index.db"),
		SocketPath:      filepath.Join(home, ".local", "share", "lsp-mcp", "daemon.sock"),
		LogLevel:        "info",
		LanguageServers: nil,
	}
	if diff := cmp.Diff(want, cfg); diff != "" {
		t.Errorf("default config mismatch (-want +got):\n%s", diff)
	}
}

// TestSpecDefaultPaths pins the literal paths written in SPEC §6.
func TestSpecDefaultPaths(t *testing.T) {
	home := isolate(t)

	tests := []struct {
		name string
		got  func() (string, error)
		want string
	}{
		{"config", DefaultConfigPath, filepath.Join(home, ".config", "lsp-mcp", "config.toml")},
		{"database", DefaultDatabasePath, filepath.Join(home, ".local", "share", "lsp-mcp", "index.db")},
		{"socket", DefaultSocketPath, filepath.Join(home, ".local", "share", "lsp-mcp", "daemon.sock")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.got()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestXDGOverridesHome(t *testing.T) {
	isolate(t)
	cfgHome := t.TempDir()
	dataHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	gotCfg, err := DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfgHome, "lsp-mcp", "config.toml"); gotCfg != want {
		t.Errorf("config path = %q, want %q", gotCfg, want)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dataHome, "lsp-mcp", "index.db"); cfg.DatabasePath != want {
		t.Errorf("database path = %q, want %q", cfg.DatabasePath, want)
	}
	if want := filepath.Join(dataHome, "lsp-mcp", "daemon.sock"); cfg.SocketPath != want {
		t.Errorf("socket path = %q, want %q", cfg.SocketPath, want)
	}
}

// TestLoadLanguageServerRegistry parses the exact TOML example from SPEC §6.
func TestLoadLanguageServerRegistry(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	const specExample = `
[[language_servers]]
name = "typescript"
command = "typescript-language-server"
args = ["--stdio"]
file_extensions = [".ts", ".tsx", ".js", ".jsx"]
root_markers = ["tsconfig.json", "package.json"]
`
	if err := os.WriteFile(path, []byte(specExample), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvConfigPath, path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	want := []LanguageServer{{
		Name:           "typescript",
		Command:        "typescript-language-server",
		Args:           []string{"--stdio"},
		FileExtensions: []string{".ts", ".tsx", ".js", ".jsx"},
		RootMarkers:    []string{"tsconfig.json", "package.json"},
	}}
	if diff := cmp.Diff(want, cfg.LanguageServers); diff != "" {
		t.Errorf("language_servers mismatch (-want +got):\n%s", diff)
	}
}

func TestLanguageServerLookup(t *testing.T) {
	cfg := &Config{LanguageServers: []LanguageServer{
		{Name: "typescript", Command: "typescript-language-server", FileExtensions: []string{".ts", ".TSX"}},
		{Name: "python", Command: "pyright-langserver", FileExtensions: []string{".py"}},
	}}

	t.Run("by extension", func(t *testing.T) {
		got, ok := cfg.LanguageServerForFile("/repo/src/user/service.ts")
		if !ok {
			t.Fatal("no language server matched .ts")
		}
		if got.Name != "typescript" {
			t.Errorf("name = %q, want typescript", got.Name)
		}
	})

	t.Run("extension match is case-insensitive", func(t *testing.T) {
		got, ok := cfg.LanguageServerForFile("/repo/src/App.tsx")
		if !ok {
			t.Fatal("no language server matched .tsx")
		}
		if got.Name != "typescript" {
			t.Errorf("name = %q, want typescript", got.Name)
		}
	})

	t.Run("unknown extension", func(t *testing.T) {
		if _, ok := cfg.LanguageServerForFile("/repo/README.md"); ok {
			t.Error("unexpectedly matched a language server for .md")
		}
	})

	t.Run("by name", func(t *testing.T) {
		got, ok := cfg.LanguageServerByName("python")
		if !ok {
			t.Fatal("no language server named python")
		}
		if got.Command != "pyright-langserver" {
			t.Errorf("command = %q, want pyright-langserver", got.Command)
		}
		if _, ok := cfg.LanguageServerByName("rust"); ok {
			t.Error("unexpectedly found a language server named rust")
		}
	})
}

// TestEnvOverrides covers SPEC §6: "Environment overrides are supported for
// database path, config path, and log level."
func TestEnvOverrides(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "database_path = \"/from/file/index.db\"\nlog_level = \"warn\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("config path override selects the file", func(t *testing.T) {
		t.Setenv(EnvConfigPath, path)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DatabasePath != "/from/file/index.db" {
			t.Errorf("database path = %q, want /from/file/index.db", cfg.DatabasePath)
		}
		if cfg.LogLevel != "warn" {
			t.Errorf("log level = %q, want warn", cfg.LogLevel)
		}
	})

	t.Run("env beats file", func(t *testing.T) {
		t.Setenv(EnvConfigPath, path)
		t.Setenv(EnvDatabasePath, "/from/env/index.db")
		t.Setenv(EnvLogLevel, "debug")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DatabasePath != "/from/env/index.db" {
			t.Errorf("database path = %q, want /from/env/index.db", cfg.DatabasePath)
		}
		if cfg.LogLevel != "debug" {
			t.Errorf("log level = %q, want debug", cfg.LogLevel)
		}
	})
}

func TestExplicitConfigPathMustExist(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "nope.toml")
	t.Setenv(EnvConfigPath, missing)

	if _, err := Load(); err == nil {
		t.Fatal("Load() with a missing explicit config path returned no error")
	}
}

func TestInvalidTOMLIsReported(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("this is not = = toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvConfigPath, path)

	if _, err := Load(); err == nil {
		t.Fatal("Load() with malformed TOML returned no error")
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "valid entry",
			body:    "[[language_servers]]\nname = \"go\"\ncommand = \"gopls\"\nfile_extensions = [\".go\"]\n",
			wantErr: false,
		},
		{
			name:    "missing name",
			body:    "[[language_servers]]\ncommand = \"gopls\"\nfile_extensions = [\".go\"]\n",
			wantErr: true,
		},
		{
			name:    "missing command",
			body:    "[[language_servers]]\nname = \"go\"\nfile_extensions = [\".go\"]\n",
			wantErr: true,
		},
		{
			name:    "duplicate names",
			body:    "[[language_servers]]\nname = \"go\"\ncommand = \"gopls\"\n\n[[language_servers]]\nname = \"go\"\ncommand = \"gopls2\"\n",
			wantErr: true,
		},
		{
			name:    "extension without leading dot",
			body:    "[[language_servers]]\nname = \"go\"\ncommand = \"gopls\"\nfile_extensions = [\"go\"]\n",
			wantErr: true,
		},
		{
			name:    "unknown log level",
			body:    "log_level = \"chatty\"\n",
			wantErr: true,
		},
		{
			name:    "known log level",
			body:    "log_level = \"error\"\n",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(EnvConfigPath, path)

			_, err := Load()
			if tt.wantErr && err == nil {
				t.Fatal("expected an error, got none")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestNoBuiltinLanguageServers guards SPEC §9: "The daemon only executes
// language server binaries that are explicitly configured." A default registry
// would execute binaries the user never named.
func TestNoBuiltinLanguageServers(t *testing.T) {
	isolate(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LanguageServers) != 0 {
		t.Errorf("expected no built-in language servers, got %d: %+v", len(cfg.LanguageServers), cfg.LanguageServers)
	}
}

func TestLoadFromExplicitPath(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "custom.toml")
	if err := os.WriteFile(path, []byte("log_level = \"debug\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log level = %q, want debug", cfg.LogLevel)
	}
}

// ---- M1 additions (docs/ARCHITECTURE.md §4.1a) ----
//
// testdata/README.md §1.9 proves, with a captured `initialize` failure, that
// typescript-language-server 5.3.0 cannot start without
// initializationOptions.tsserver.path. SPEC §6's example has no way to express
// that, so M1 acceptance is impossible without this field.
// [SPEC-ADDITION — docs/ARCHITECTURE.md §11 item 1]

func TestLoadInitializationOptions(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[[language_servers]]
name = "typescript"
command = "typescript-language-server"
args = ["--stdio"]
file_extensions = [".ts"]
root_markers = ["tsconfig.json"]

[language_servers.initialization_options.tsserver]
path = "/opt/ts5/lib/tsserver.js"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if len(cfg.LanguageServers) != 1 {
		t.Fatalf("got %d language servers, want 1", len(cfg.LanguageServers))
	}
	opts := cfg.LanguageServers[0].InitializationOptions
	if opts == nil {
		t.Fatal("initialization_options was not decoded")
	}
	tsserver, ok := opts["tsserver"].(map[string]any)
	if !ok {
		t.Fatalf("initialization_options.tsserver = %#v, want a table", opts["tsserver"])
	}
	if got, want := tsserver["path"], "/opt/ts5/lib/tsserver.js"; got != want {
		t.Fatalf("tsserver.path = %v, want %q", got, want)
	}
}

func TestInitializationOptionsDefaultToNil(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[[language_servers]]
name = "python"
command = "pyright-langserver"
args = ["--stdio"]
file_extensions = [".py"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.LanguageServers[0].InitializationOptions != nil {
		t.Fatalf("initialization_options = %#v, want nil when unset",
			cfg.LanguageServers[0].InitializationOptions)
	}
	if cfg.LanguageServers[0].AllowWorkspaceLocal {
		t.Fatal("allow_workspace_local defaulted to true; SPEC §9 requires opt-IN")
	}
}

// SPEC §9's "without explicit opt-in" wording authorises exactly this knob.
// Nothing in the v0.1 test suite or fixtures may ever set it true.
func TestAllowWorkspaceLocalIsOptIn(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[[language_servers]]
name = "vendored"
command = "./node_modules/.bin/some-server"
file_extensions = [".x"]
allow_workspace_local = true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !cfg.LanguageServers[0].AllowWorkspaceLocal {
		t.Fatal("allow_workspace_local = true was not decoded")
	}
}

func TestAllowWorkspaceLocalRequiresACommand(t *testing.T) {
	cfg := &Config{
		LogLevel: DefaultLogLevel,
		LanguageServers: []LanguageServer{
			{Name: "broken", AllowWorkspaceLocal: true},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("allow_workspace_local with an empty command was accepted")
	}
}

// The two new fields are additive: an entry using none of them still parses.
func TestSpecSixExampleStillParsesUnchanged(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[[language_servers]]
name = "typescript"
command = "typescript-language-server"
args = ["--stdio"]
file_extensions = [".ts", ".tsx", ".js", ".jsx"]
root_markers = ["tsconfig.json", "package.json"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	ls, ok := cfg.LanguageServerByName("typescript")
	if !ok {
		t.Fatal("typescript entry missing")
	}
	if ls.Command != "typescript-language-server" || len(ls.Args) != 1 {
		t.Fatalf("entry = %+v", ls)
	}
}
