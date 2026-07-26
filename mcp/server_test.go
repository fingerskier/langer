package mcp

import (
	"context"
	"reflect"
	"sort"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fingerskier/langer/protocol"
)

func TestM5RegistersExactlyTwelveTools(t *testing.T) {
	_, client, closeAll := connectedServer(t, &fakeService{})
	defer closeAll()

	var names []string
	for tool, err := range client.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{
		"apply_edit", "close_document", "document_symbols", "get_definition",
		"get_diagnostics", "get_hover", "get_references", "index_status",
		"open_document", "rename_symbol", "simulate_edit", "workspace_symbols",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}

func TestEditToolsRelaySessionAndStructuredResults(t *testing.T) {
	fake := &fakeService{
		rename:   protocol.EditPlanResult{EditToken: "tok", Files: []protocol.FileEdit{{Path: "a.ts"}}},
		apply:    protocol.ApplyResult{Applied: []string{"a.ts"}},
		simulate: protocol.DiagnosticsResult{Diagnostics: []protocol.Diagnostic{{Path: "a.ts", Message: "boom"}}},
	}
	_, client, closeAll := connectedServer(t, fake)
	defer closeAll()

	result, err := client.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "rename_symbol",
		Arguments: map[string]any{"path": "a.ts", "line": 1, "character": 2, "new_name": "Person"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("rename error: %#v", result.StructuredContent)
	}
	if fake.renameIn.NewName != "Person" || fake.renameIn.Path != "a.ts" || fake.renameIn.Session == "" {
		t.Fatalf("rename params = %+v", fake.renameIn)
	}
	if out := result.StructuredContent.(map[string]any); out["edit_token"] != "tok" {
		t.Fatalf("rename result = %#v", out)
	}

	result, err = client.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "apply_edit",
		Arguments: map[string]any{"edit_token": "tok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.applyIn.EditToken != "tok" || fake.applyIn.Session == "" {
		t.Fatalf("apply params = %+v", fake.applyIn)
	}
	if out := result.StructuredContent.(map[string]any); out["applied"] == nil {
		t.Fatalf("apply result = %#v", out)
	}

	result, err = client.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "simulate_edit",
		Arguments: map[string]any{"path": "a.ts", "new_text": "const x = 1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.simulateIn.Path != "a.ts" || fake.simulateIn.NewText != "const x = 1" || fake.simulateIn.Session == "" {
		t.Fatalf("simulate params = %+v", fake.simulateIn)
	}
	if out := result.StructuredContent.(map[string]any); out["diagnostics"] == nil {
		t.Fatalf("simulate result = %#v", out)
	}
}

func TestDefinitionRelaysSessionRootAndStructuredResult(t *testing.T) {
	fake := &fakeService{definition: protocol.LocationsResult{Locations: []protocol.Location{{Path: "src/a.ts"}}}}
	_, client, closeAll := connectedServer(t, fake)
	defer closeAll()

	result, err := client.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "get_definition",
		Arguments: map[string]any{"path": "src/a.ts", "line": 3, "character": 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("result unexpectedly marked error: %#v", result.StructuredContent)
	}
	if fake.open.Root != `C:\repo` || fake.position.Path != "src/a.ts" || fake.position.Position.Line != 3 {
		t.Fatalf("relay params: open=%+v position=%+v", fake.open, fake.position)
	}
	if fake.open.Session == "" || fake.position.Session != fake.open.Session {
		t.Fatalf("session was not derived consistently: open=%q query=%q", fake.open.Session, fake.position.Session)
	}
	out := result.StructuredContent.(map[string]any)
	if _, ok := out["locations"]; !ok {
		t.Fatalf("structured result = %#v", out)
	}
}

func TestStructuredErrorsNeverBecomeProtocolErrors(t *testing.T) {
	fake := &fakeService{err: protocol.NewError(protocol.ErrNotReady, "indexing").WithRetryAfterMS(25)}
	_, client, closeAll := connectedServer(t, fake)
	defer closeAll()

	result, err := client.CallTool(context.Background(), &sdk.CallToolParams{Name: "index_status", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("handler leaked a Go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("NOT_READY was not marked as a tool error")
	}
	out := result.StructuredContent.(map[string]any)
	errObj := out["error"].(map[string]any)
	if errObj["code"] != string(protocol.ErrNotReady) {
		t.Fatalf("error output = %#v", out)
	}

	fake.err = protocol.NewError(protocol.ErrNoResult, "nothing found")
	result, err = client.CallTool(context.Background(), &sdk.CallToolParams{Name: "get_hover", Arguments: map[string]any{"path": "a.ts", "line": 0, "character": 0}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatal("NO_RESULT must not set MCP IsError")
	}
}

func connectedServer(t *testing.T, service protocol.Service) (*Server, *sdk.ClientSession, func()) {
	t.Helper()
	ctx := context.Background()
	server := NewServer(service, `C:\repo`)
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	ss, err := server.sdk.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	return server, cs, func() { _ = cs.Close(); _ = ss.Close() }
}

type fakeService struct {
	open       protocol.OpenWorkspaceParams
	position   protocol.PositionParams
	definition protocol.LocationsResult
	rename     protocol.EditPlanResult
	apply      protocol.ApplyResult
	simulate   protocol.DiagnosticsResult
	renameIn   protocol.RenameParams
	applyIn    protocol.ApplyEditParams
	simulateIn protocol.SimulateEditParams
	err        error
}

func (f *fakeService) OpenWorkspace(_ context.Context, p protocol.OpenWorkspaceParams) (protocol.OpenWorkspaceResult, error) {
	f.open = p
	return protocol.OpenWorkspaceResult{Workspace: "ws-test", Root: p.Root}, f.err
}
func (f *fakeService) CloseWorkspace(context.Context, protocol.CloseWorkspaceParams) (protocol.EmptyResult, error) {
	return protocol.EmptyResult{}, nil
}
func (f *fakeService) OpenDocument(context.Context, protocol.OpenDocumentParams) (protocol.EmptyResult, error) {
	return protocol.EmptyResult{}, f.err
}
func (f *fakeService) CloseDocument(context.Context, protocol.DocumentParams) (protocol.EmptyResult, error) {
	return protocol.EmptyResult{}, f.err
}
func (f *fakeService) GetDefinition(_ context.Context, p protocol.PositionParams) (protocol.LocationsResult, error) {
	f.position = p
	return f.definition, f.err
}
func (f *fakeService) GetReferences(context.Context, protocol.PositionParams) (protocol.LocationsResult, error) {
	return protocol.LocationsResult{}, f.err
}
func (f *fakeService) GetHover(context.Context, protocol.PositionParams) (protocol.HoverResult, error) {
	return protocol.HoverResult{}, f.err
}
func (f *fakeService) DocumentSymbols(context.Context, protocol.DocumentParams) (protocol.SymbolsResult, error) {
	return protocol.SymbolsResult{}, f.err
}
func (f *fakeService) WorkspaceSymbols(context.Context, protocol.WorkspaceSymbolsParams) (protocol.SymbolsResult, error) {
	return protocol.SymbolsResult{}, f.err
}
func (f *fakeService) GetDiagnostics(context.Context, protocol.DiagnosticsParams) (protocol.DiagnosticsResult, error) {
	return protocol.DiagnosticsResult{}, f.err
}
func (f *fakeService) RenameSymbol(_ context.Context, p protocol.RenameParams) (protocol.EditPlanResult, error) {
	f.renameIn = p
	return f.rename, f.err
}
func (f *fakeService) ApplyEdit(_ context.Context, p protocol.ApplyEditParams) (protocol.ApplyResult, error) {
	f.applyIn = p
	return f.apply, f.err
}
func (f *fakeService) SimulateEdit(_ context.Context, p protocol.SimulateEditParams) (protocol.DiagnosticsResult, error) {
	f.simulateIn = p
	return f.simulate, f.err
}
func (f *fakeService) IndexStatus(context.Context, protocol.IndexStatusParams) (protocol.IndexStatusResult, error) {
	return protocol.IndexStatusResult{}, f.err
}
func (f *fakeService) EndSession(context.Context, protocol.EndSessionParams) (protocol.EmptyResult, error) {
	return protocol.EmptyResult{}, nil
}
