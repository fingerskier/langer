package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fingerskier/langer/protocol"
)

type positionInput struct {
	Path      string `json:"path" jsonschema:"workspace-relative path"`
	Line      int    `json:"line" jsonschema:"0-based line"`
	Character int    `json:"character" jsonschema:"0-based UTF-16 character offset"`
}

type documentInput struct {
	Path string `json:"path" jsonschema:"workspace-relative path"`
}

type workspaceSymbolsInput struct {
	Query string `json:"query" jsonschema:"fuzzy symbol query"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum results; zero uses the server default"`
}

type diagnosticsInput struct {
	Path string `json:"path,omitempty" jsonschema:"workspace-relative path; omit for workspace diagnostics"`
}

func (s *Server) registerNavigationTools() {
	sdk.AddTool(s.sdk, &sdk.Tool{Name: "get_definition", Description: "Find compiler-accurate definitions for a position." + retryGuidance},
		func(ctx context.Context, req *sdk.CallToolRequest, in positionInput) (*sdk.CallToolResult, map[string]any, error) {
			session, ws, err := s.workspace(ctx, req)
			if err != nil {
				return toolOutput(nil, err)
			}
			out, err := s.svc.GetDefinition(ctx, protocol.PositionParams{DocumentParams: protocol.DocumentParams{Session: session, Workspace: ws, Path: in.Path}, Position: protocol.Position{Line: in.Line, Character: in.Character}})
			return toolOutput(out, err)
		})

	sdk.AddTool(s.sdk, &sdk.Tool{Name: "get_references", Description: "Find semantic references, including the declaration." + retryGuidance},
		func(ctx context.Context, req *sdk.CallToolRequest, in positionInput) (*sdk.CallToolResult, map[string]any, error) {
			session, ws, err := s.workspace(ctx, req)
			if err != nil {
				return toolOutput(nil, err)
			}
			out, err := s.svc.GetReferences(ctx, protocol.PositionParams{DocumentParams: protocol.DocumentParams{Session: session, Workspace: ws, Path: in.Path}, Position: protocol.Position{Line: in.Line, Character: in.Character}})
			return toolOutput(out, err)
		})

	sdk.AddTool(s.sdk, &sdk.Tool{Name: "get_hover", Description: "Return the type signature and documentation at a position." + retryGuidance},
		func(ctx context.Context, req *sdk.CallToolRequest, in positionInput) (*sdk.CallToolResult, map[string]any, error) {
			session, ws, err := s.workspace(ctx, req)
			if err != nil {
				return toolOutput(nil, err)
			}
			out, err := s.svc.GetHover(ctx, protocol.PositionParams{DocumentParams: protocol.DocumentParams{Session: session, Workspace: ws, Path: in.Path}, Position: protocol.Position{Line: in.Line, Character: in.Character}})
			return toolOutput(out, err)
		})

	sdk.AddTool(s.sdk, &sdk.Tool{Name: "document_symbols", Description: "List the semantic outline of one document." + retryGuidance},
		func(ctx context.Context, req *sdk.CallToolRequest, in documentInput) (*sdk.CallToolResult, map[string]any, error) {
			session, ws, err := s.workspace(ctx, req)
			if err != nil {
				return toolOutput(nil, err)
			}
			out, err := s.svc.DocumentSymbols(ctx, protocol.DocumentParams{Session: session, Workspace: ws, Path: in.Path})
			return toolOutput(out, err)
		})

	sdk.AddTool(s.sdk, &sdk.Tool{Name: "workspace_symbols", Description: "Fuzzy-search symbols across the workspace." + retryGuidance},
		func(ctx context.Context, req *sdk.CallToolRequest, in workspaceSymbolsInput) (*sdk.CallToolResult, map[string]any, error) {
			session, ws, err := s.workspace(ctx, req)
			if err != nil {
				return toolOutput(nil, err)
			}
			out, err := s.svc.WorkspaceSymbols(ctx, protocol.WorkspaceSymbolsParams{Session: session, Workspace: ws, Query: in.Query, Limit: in.Limit})
			return toolOutput(out, err)
		})

	sdk.AddTool(s.sdk, &sdk.Tool{Name: "get_diagnostics", Description: "Return current compiler diagnostics for a file or the workspace." + retryGuidance},
		func(ctx context.Context, req *sdk.CallToolRequest, in diagnosticsInput) (*sdk.CallToolResult, map[string]any, error) {
			session, ws, err := s.workspace(ctx, req)
			if err != nil {
				return toolOutput(nil, err)
			}
			out, err := s.svc.GetDiagnostics(ctx, protocol.DiagnosticsParams{Session: session, Workspace: ws, Path: in.Path})
			return toolOutput(out, err)
		})
}
