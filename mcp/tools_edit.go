package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fingerskier/langer/protocol"
)

type renameInput struct {
	Path      string `json:"path" jsonschema:"workspace-relative path"`
	Line      int    `json:"line" jsonschema:"0-based line"`
	Character int    `json:"character" jsonschema:"0-based UTF-16 character offset"`
	NewName   string `json:"new_name" jsonschema:"replacement symbol name"`
}

type applyEditInput struct {
	EditToken string `json:"edit_token" jsonschema:"token returned by rename_symbol dry-run"`
}

type simulateEditInput struct {
	Path    string `json:"path" jsonschema:"workspace-relative path"`
	NewText string `json:"new_text" jsonschema:"full file text to evaluate in memory"`
}

func (s *Server) registerEditTools() {
	sdk.AddTool(s.sdk, &sdk.Tool{
		Name: "rename_symbol",
		Description: "Compute a rename dry-run and return an edit_token that embeds " +
			"content hashes of every affected file. Nothing is written until apply_edit. " +
			"If apply_edit later returns STALE_EDIT, re-run this dry-run." + retryGuidance,
	}, func(ctx context.Context, req *sdk.CallToolRequest, in renameInput) (*sdk.CallToolResult, map[string]any, error) {
		session, ws, err := s.workspace(ctx, req)
		if err != nil {
			return toolOutput(nil, err)
		}
		out, err := s.svc.RenameSymbol(ctx, protocol.RenameParams{
			PositionParams: protocol.PositionParams{
				DocumentParams: protocol.DocumentParams{Session: session, Workspace: ws, Path: in.Path},
				Position:       protocol.Position{Line: in.Line, Character: in.Character},
			},
			NewName: in.NewName,
		})
		return toolOutput(out, err)
	})

	sdk.AddTool(s.sdk, &sdk.Tool{
		Name: "apply_edit",
		Description: "Apply a previously computed edit plan identified by edit_token. " +
			"Verifies content hashes against disk and rejects with STALE_EDIT if any " +
			"file changed since the dry-run; re-run rename_symbol in that case.",
	}, func(ctx context.Context, req *sdk.CallToolRequest, in applyEditInput) (*sdk.CallToolResult, map[string]any, error) {
		session, ws, err := s.workspace(ctx, req)
		if err != nil {
			return toolOutput(nil, err)
		}
		out, err := s.svc.ApplyEdit(ctx, protocol.ApplyEditParams{
			Session: session, Workspace: ws, EditToken: in.EditToken,
		})
		return toolOutput(out, err)
	})

	sdk.AddTool(s.sdk, &sdk.Tool{
		Name: "simulate_edit",
		Description: "Apply a full-file edit in a per-session in-memory overlay and return " +
			"the resulting diagnostics without writing to disk. Overlays are isolated " +
			"between sessions, expire after a short TTL, and become STALE_EDIT if the " +
			"real file changes. Never treat empty diagnostics as definitive while " +
			"NOT_READY is possible." + retryGuidance,
	}, func(ctx context.Context, req *sdk.CallToolRequest, in simulateEditInput) (*sdk.CallToolResult, map[string]any, error) {
		session, ws, err := s.workspace(ctx, req)
		if err != nil {
			return toolOutput(nil, err)
		}
		out, err := s.svc.SimulateEdit(ctx, protocol.SimulateEditParams{
			DocumentParams: protocol.DocumentParams{Session: session, Workspace: ws, Path: in.Path},
			NewText:        in.NewText,
		})
		return toolOutput(out, err)
	})
}
