//go:build integration

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/internal/testutil"
	"github.com/fingerskier/langer/protocol"
)

func TestMCPNavigationAgainstTypeScriptAndPythonFixtures(t *testing.T) {
	exe := buildLanger(t)
	tests := []struct {
		name, fixture, language, path, wantPath, referencePath string
		line, character, referenceLine, referenceCharacter     int
	}{
		{"typescript", "ts-project", "typescript", "src/service.ts", "src/user.ts", "src/user.ts", 6, 21, 5, 16},
		{"python", "py-project", "python", "service.py", "user.py", "user.py", 9, 17, 9, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := testutil.FixtureRoot(t, tt.fixture)
			server := testutil.RequireLanguageServer(t, tt.language)
			cfg, configPath := integrationConfig(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			proc, err := procx.NewRunner().Start(ctx, procx.Spec{
				Path: exe, Args: []string{"mcp", "--stdio"}, Dir: root,
				Env: append(os.Environ(), config.EnvConfigPath+"="+configPath),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = proc.Kill()
				_ = proc.Wait()
				stopDaemon(t, cfg, root)
			})
			go func() { _, _ = io.Copy(io.Discard, proc.Stderr()) }()
			reader, ok := proc.Stdout().(io.ReadCloser)
			if !ok {
				t.Fatal("process stdout is not closable")
			}
			client := sdk.NewClient(&sdk.Implementation{Name: "m4-integration", Version: "1"}, nil)
			session, err := client.Connect(ctx, &sdk.IOTransport{Reader: reader, Writer: proc.Stdin()}, nil)
			if err != nil {
				_ = proc.Kill()
				t.Fatal(err)
			}

			_, err = callUntilReady(ctx, session, "open_document", map[string]any{"path": tt.path, "language_id": tt.language})
			if err != nil {
				t.Fatal(err)
			}
			result, err := callUntilReady(ctx, session, "get_definition", map[string]any{"path": tt.path, "line": tt.line, "character": tt.character})
			if err != nil {
				t.Fatal(err)
			}
			out := result.StructuredContent.(map[string]any)
			locations := out["locations"].([]any)
			if len(locations) != 1 || locations[0].(map[string]any)["path"] != tt.wantPath {
				t.Fatalf("definitions = %#v, want one in %s", locations, tt.wantPath)
			}

			if _, err := callUntilReady(ctx, session, "close_document", map[string]any{"path": tt.path}); err != nil {
				t.Fatal(err)
			}
			if err := waitForMCPIndex(ctx, session); err != nil {
				t.Fatal(err)
			}
			references, err := callUntilReady(ctx, session, "get_references", map[string]any{"path": tt.referencePath, "line": tt.referenceLine, "character": tt.referenceCharacter})
			if err != nil {
				t.Fatal(err)
			}
			if got := references.StructuredContent.(map[string]any)["locations"].([]any); len(got) < 2 {
				t.Fatalf("references = %#v, want declaration plus call sites", got)
			}
			hover, err := callUntilReady(ctx, session, "get_hover", map[string]any{"path": tt.path, "line": tt.line, "character": tt.character})
			if err != nil {
				t.Fatal(err)
			}
			if hover.StructuredContent.(map[string]any)["hover"] == nil {
				t.Fatal("hover result is nil")
			}

			_ = session.Close()
			_ = proc.Kill()
			_ = proc.Wait()
		})
	}
}

func waitForMCPIndex(ctx context.Context, session *sdk.ClientSession) error {
	for {
		result, err := callUntilReady(ctx, session, "index_status", map[string]any{})
		if err != nil {
			return err
		}
		out := result.StructuredContent.(map[string]any)
		switch out["state"] {
		case string(protocol.IndexReady):
			return nil
		case string(protocol.IndexFailed):
			return fmt.Errorf("index failed: %#v", out)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("index did not become ready: last status %#v: %w", out, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func callUntilReady(ctx context.Context, session *sdk.ClientSession, name string, args map[string]any) (*sdk.CallToolResult, error) {
	for {
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return nil, err
		}
		if !result.IsError {
			return result, nil
		}
		out, _ := result.StructuredContent.(map[string]any)
		errObj, _ := out["error"].(map[string]any)
		if errObj["code"] != string(protocol.ErrNotReady) {
			return nil, fmt.Errorf("%s: %#v", name, out)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func buildLanger(t *testing.T) string {
	t.Helper()
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	name := "langer"
	goExe := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		name += ".exe"
		goExe += ".exe"
	}
	exe := filepath.Join(t.TempDir(), name)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := procx.NewRunner().Output(ctx, procx.Spec{Path: goExe, Args: []string{"build", "-o", exe, "./cmd/langer"}, Dir: repo, Env: os.Environ()}); err != nil {
		t.Fatal(err)
	}
	return exe
}

func integrationConfig(t *testing.T, server config.LanguageServer) (*config.Config, string) {
	t.Helper()
	state, err := os.MkdirTemp("/tmp", "langer-mcp")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	path := filepath.Join(state, "config.toml")
	var b strings.Builder
	fmt.Fprintf(&b, "database_path = %s\nsocket_path = %s\nlog_level = %q\n\n", strconv.Quote(filepath.Join(state, "index.db")), strconv.Quote(filepath.Join(state, "daemon.sock")), "error")
	fmt.Fprintf(&b, "[[language_servers]]\nname = %q\ncommand = %s\n", server.Name, strconv.Quote(server.Command))
	fmt.Fprintf(&b, "args = [%s]\nfile_extensions = [%s]\nroot_markers = [%s]\n", quoted(server.Args), quoted(server.FileExtensions), quoted(server.RootMarkers))
	if ts, ok := server.InitializationOptions["tsserver"].(map[string]any); ok {
		fmt.Fprintf(&b, "initialization_options = { tsserver = { path = %s } }\n", strconv.Quote(ts["path"].(string)))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

func quoted(values []string) string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = strconv.Quote(value)
	}
	return strings.Join(out, ", ")
}

func stopDaemon(t *testing.T, cfg *config.Config, root string) {
	t.Helper()
	socket, err := cfg.WorkspaceSocketPath(root)
	if err != nil {
		t.Log(err)
		return
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	codec := protocol.NewCodec(conn)
	params, _ := json.Marshal(protocol.HandshakeParams{Session: "cleanup", ClientVersion: protocol.Version})
	if err := codec.WriteRequest(protocol.NewRequest(1, protocol.MethodHandshake, params)); err != nil {
		return
	}
	response, err := codec.ReadResponse()
	if err != nil || response.Error != nil {
		return
	}
	var handshake protocol.HandshakeResult
	if json.Unmarshal(response.Result, &handshake) != nil {
		return
	}
	if process, err := os.FindProcess(handshake.PID); err == nil {
		_ = process.Kill()
		_, _ = process.Wait()
	}
}
