// Package config loads langer's configuration per SPEC §6.
//
// Default locations follow XDG conventions:
//
//	config   ~/.config/lsp-mcp/config.toml
//	database ~/.local/share/lsp-mcp/index.db
//	socket   ~/.local/share/lsp-mcp/daemon.sock
//
// XDG_CONFIG_HOME and XDG_DATA_HOME are honoured when set. Environment
// overrides exist for the database path, the config path, and the log level.
//
// The language server registry is entirely declarative and entirely
// user-supplied: there are no built-in entries, because SPEC §9 requires that
// the daemon only ever execute language server binaries the user explicitly
// configured.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Environment variable names for the three overrides SPEC §6 requires.
const (
	// EnvConfigPath selects an explicit config file. When set, the file must
	// exist — a typo should fail loudly, not silently fall back to defaults.
	EnvConfigPath = "LANGER_CONFIG_PATH"
	// EnvDatabasePath overrides the SQLite index location.
	EnvDatabasePath = "LANGER_DB_PATH"
	// EnvLogLevel overrides the log level.
	EnvLogLevel = "LANGER_LOG_LEVEL"
)

// appDir is the XDG subdirectory name. It stays "lsp-mcp" (SPEC §6) even though
// the binary is called langer, so existing installs keep their index.
const appDir = "lsp-mcp"

// DefaultLogLevel is used when neither the file nor the environment says
// otherwise.
const DefaultLogLevel = "info"

// LogLevels are the accepted log level spellings, quietest last.
var LogLevels = []string{"debug", "info", "warn", "error"}

// LanguageServer is one declarative registry entry (SPEC §3.3, §6):
//
//	[[language_servers]]
//	name = "typescript"
//	command = "typescript-language-server"
//	args = ["--stdio"]
//	file_extensions = [".ts", ".tsx", ".js", ".jsx"]
//	root_markers = ["tsconfig.json", "package.json"]
type LanguageServer struct {
	Name           string   `toml:"name"`
	Command        string   `toml:"command"`
	Args           []string `toml:"args"`
	FileExtensions []string `toml:"file_extensions"`
	RootMarkers    []string `toml:"root_markers"`

	// InitializationOptions is passed verbatim as the LSP `initialize`
	// request's initializationOptions.
	//
	// SPEC §3.3 enumerates the declarative fields as "name, command, args, file
	// extensions, root markers", but typescript-language-server 5.3.0 provably
	// cannot start without initializationOptions.tsserver.path — see
	// testdata/README.md §1.9 for the captured failure.
	// [SPEC-ADDITION — docs/ARCHITECTURE.md §4.1a, §11 item 1]
	InitializationOptions map[string]any `toml:"initialization_options"`

	// AllowWorkspaceLocal opts this entry in to executing a binary that
	// resolves inside the workspace tree. Defaults to false. SPEC §9's
	// "without explicit opt-in" wording authorises exactly this knob; nothing
	// in the v0.1 test suite or fixtures may ever set it true.
	AllowWorkspaceLocal bool `toml:"allow_workspace_local"`
}

// Config is the fully resolved configuration: file values with environment
// overrides applied and defaults filled in.
type Config struct {
	DatabasePath string `toml:"database_path"`
	SocketPath   string `toml:"socket_path"`
	LogLevel     string `toml:"log_level"`
	// IdleTimeout is the SPEC §3.1 daemon sunset period as a Go duration
	// string (for example "30m"). Empty means the daemon default (30 minutes).
	IdleTimeout     string           `toml:"idle_timeout"`
	LanguageServers []LanguageServer `toml:"language_servers"`
}

// IdleDuration returns the configured idle sunset period, or 0 when unset so
// the daemon can apply its own default.
func (c *Config) IdleDuration() (time.Duration, error) {
	if c == nil || strings.TrimSpace(c.IdleTimeout) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(c.IdleTimeout))
	if err != nil {
		return 0, fmt.Errorf("idle_timeout %q: %w", c.IdleTimeout, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("idle_timeout %q must be positive", c.IdleTimeout)
	}
	return d, nil
}

// configHome returns $XDG_CONFIG_HOME, or ~/.config when it is unset or empty.
func configHome() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}

// dataHome returns $XDG_DATA_HOME, or ~/.local/share when it is unset or empty.
func dataHome() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share"), nil
}

// DefaultConfigPath returns ~/.config/lsp-mcp/config.toml (SPEC §6).
func DefaultConfigPath() (string, error) {
	dir, err := configHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, appDir, "config.toml"), nil
}

// DefaultDatabasePath returns ~/.local/share/lsp-mcp/index.db (SPEC §6).
func DefaultDatabasePath() (string, error) {
	dir, err := dataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, appDir, "index.db"), nil
}

// DefaultSocketPath returns ~/.local/share/lsp-mcp/daemon.sock (SPEC §6).
func DefaultSocketPath() (string, error) {
	dir, err := dataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, appDir, "daemon.sock"), nil
}

// ConfigPath reports which config file Load will read: the EnvConfigPath
// override when set, otherwise the XDG default.
func ConfigPath() (string, error) {
	if path := os.Getenv(EnvConfigPath); path != "" {
		return path, nil
	}
	return DefaultConfigPath()
}

// Load reads the configuration from the path ConfigPath reports.
//
// A missing file at the *default* location is not an error — langer runs on
// defaults. A missing file at an *explicit* location (EnvConfigPath) is an
// error, because the user asked for a specific file.
func Load() (*Config, error) {
	if path := os.Getenv(EnvConfigPath); path != "" {
		return LoadFrom(path)
	}
	path, err := DefaultConfigPath()
	if err != nil {
		return nil, err
	}
	return load(path, false)
}

// LoadFrom reads the configuration from an explicit path, which must exist.
func LoadFrom(path string) (*Config, error) {
	return load(path, true)
}

func load(path string, required bool) (*Config, error) {
	cfg := &Config{}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		md, decErr := toml.Decode(string(data), cfg)
		if decErr != nil {
			return nil, fmt.Errorf("parsing config %s: %w", path, decErr)
		}
		keys := make([]string, 0, len(md.Undecoded()))
		for _, k := range md.Undecoded() {
			// initialization_options is a free-form map passed verbatim to the
			// language server, so its interior keys are decoded into the map
			// but still reported as undecoded. Everything else stays strict:
			// a typo in a langer key must fail loudly.
			if isInitializationOption(k) {
				continue
			}
			keys = append(keys, k.String())
		}
		if len(keys) > 0 {
			return nil, fmt.Errorf("parsing config %s: unknown key(s): %s", path, strings.Join(keys, ", "))
		}
	case errors.Is(err, os.ErrNotExist) && !required:
		// No config file: defaults only. This is the normal first-run case.
	default:
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// isInitializationOption reports whether an undecoded key lives inside a
// language server's free-form initialization_options table.
func isInitializationOption(key toml.Key) bool {
	for _, segment := range key {
		if segment == "initialization_options" {
			return true
		}
	}
	return false
}

// applyEnv layers the SPEC §6 environment overrides on top of file values.
// An empty variable counts as unset so a test or wrapper script can neutralise
// an inherited override.
func (c *Config) applyEnv() error {
	if v := os.Getenv(EnvDatabasePath); v != "" {
		c.DatabasePath = v
	}
	if v := os.Getenv(EnvLogLevel); v != "" {
		c.LogLevel = v
	}
	return nil
}

// applyDefaults fills anything still empty with the XDG defaults.
func (c *Config) applyDefaults() error {
	if c.DatabasePath == "" {
		path, err := DefaultDatabasePath()
		if err != nil {
			return err
		}
		c.DatabasePath = path
	}
	if c.SocketPath == "" {
		path, err := DefaultSocketPath()
		if err != nil {
			return err
		}
		c.SocketPath = path
	}
	if c.LogLevel == "" {
		c.LogLevel = DefaultLogLevel
	}
	return nil
}

// Validate rejects configurations that would fail confusingly later.
func (c *Config) Validate() error {
	if !validLogLevel(c.LogLevel) {
		return fmt.Errorf("log_level %q is not one of %s", c.LogLevel, strings.Join(LogLevels, ", "))
	}
	if _, err := c.IdleDuration(); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(c.LanguageServers))
	for i, ls := range c.LanguageServers {
		if ls.Name == "" {
			return fmt.Errorf("language_servers[%d]: name is required", i)
		}
		if ls.Command == "" {
			if ls.AllowWorkspaceLocal {
				// An opt-in to executing workspace-local binaries with nothing
				// to execute is a configuration mistake worth naming loudly:
				// it is the one setting that relaxes a SPEC §9 invariant.
				return fmt.Errorf("language_servers[%d] (%s): allow_workspace_local is set but command is empty", i, ls.Name)
			}
			return fmt.Errorf("language_servers[%d] (%s): command is required", i, ls.Name)
		}
		key := strings.ToLower(ls.Name)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("language_servers[%d]: duplicate name %q", i, ls.Name)
		}
		seen[key] = struct{}{}

		for _, ext := range ls.FileExtensions {
			if !strings.HasPrefix(ext, ".") {
				return fmt.Errorf("language_servers[%d] (%s): file extension %q must start with a dot", i, ls.Name, ext)
			}
		}
	}
	return nil
}

func validLogLevel(level string) bool {
	for _, known := range LogLevels {
		if level == known {
			return true
		}
	}
	return false
}

// LanguageServerForFile returns the registry entry claiming path's extension.
// Matching is case-insensitive so ".TSX" and ".tsx" behave the same.
func (c *Config) LanguageServerForFile(path string) (LanguageServer, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return LanguageServer{}, false
	}
	for _, ls := range c.LanguageServers {
		for _, candidate := range ls.FileExtensions {
			if strings.ToLower(candidate) == ext {
				return ls, true
			}
		}
	}
	return LanguageServer{}, false
}

// LanguageServerByName returns the registry entry with the given name.
func (c *Config) LanguageServerByName(name string) (LanguageServer, bool) {
	for _, ls := range c.LanguageServers {
		if strings.EqualFold(ls.Name, name) {
			return ls, true
		}
	}
	return LanguageServer{}, false
}
