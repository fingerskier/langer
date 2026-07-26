package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fingerskier/langer/protocol"
)

type openDocumentInput struct {
	Path       string `json:"path" jsonschema:"workspace-relative path"`
	LanguageID string `json:"language_id,omitempty" jsonschema:"optional LSP language id"`
}

type emptyInput struct{}

func (s *Server) registerSessionTools() {
	sdk.AddTool(s.sdk, &sdk.Tool{Name: "open_document", Description: "Mark a document active and start its configured language server." + retryGuidance},
		func(ctx context.Context, req *sdk.CallToolRequest, in openDocumentInput) (*sdk.CallToolResult, map[string]any, error) {
			session, ws, err := s.workspace(ctx, req)
			if err != nil {
				return toolOutput(nil, err)
			}
			out, err := s.svc.OpenDocument(ctx, protocol.OpenDocumentParams{DocumentParams: protocol.DocumentParams{Session: session, Workspace: ws, Path: in.Path}, LanguageID: in.LanguageID})
			return toolOutput(out, err)
		})

	sdk.AddTool(s.sdk, &sdk.Tool{Name: "close_document", Description: "Release this session's active reference to a document."},
		func(ctx context.Context, req *sdk.CallToolRequest, in documentInput) (*sdk.CallToolResult, map[string]any, error) {
			session, ws, err := s.workspace(ctx, req)
			if err != nil {
				return toolOutput(nil, err)
			}
			out, err := s.svc.CloseDocument(ctx, protocol.DocumentParams{Session: session, Workspace: ws, Path: in.Path})
			return toolOutput(out, err)
		})

	sdk.AddTool(s.sdk, &sdk.Tool{Name: "index_status", Description: "Report index progress and language-server state." + retryGuidance},
		func(ctx context.Context, req *sdk.CallToolRequest, _ emptyInput) (*sdk.CallToolResult, map[string]any, error) {
			session, ws, err := s.workspace(ctx, req)
			if err != nil {
				return toolOutput(nil, err)
			}
			out, err := s.svc.IndexStatus(ctx, protocol.IndexStatusParams{Session: session, Workspace: ws})
			return toolOutput(out, err)
		})
}
