// Package mcp exposes langer's daemon service as a lean MCP stdio server.
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fingerskier/langer/protocol"
)

const retryGuidance = " If the result is NOT_READY, wait for retry_after_ms and retry; never interpret an incomplete-index empty result as definitive."

type Server struct {
	svc     protocol.Service
	root    string
	session protocol.SessionID
	sdk     *sdk.Server
}

func NewServer(svc protocol.Service, root string) *Server {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{svc: svc, root: root, session: newSessionID()}
	s.sdk = sdk.NewServer(&sdk.Implementation{Name: "langer", Version: "0.1.0"}, &sdk.ServerOptions{
		Logger:       logger,
		Capabilities: &sdk.ServerCapabilities{},
	})
	s.registerNavigationTools()
	s.registerSessionTools()
	s.registerEditTools()
	return s
}

func (s *Server) Run(ctx context.Context) error {
	session, err := s.sdk.Connect(ctx, &sdk.StdioTransport{}, nil)
	if err != nil {
		return err
	}
	id := s.sessionID(session.ID())
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = s.svc.EndSession(cleanup, protocol.EndSessionParams{Session: id})
	}()

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = session.Close()
		<-done
		return nil
	}
}

func (s *Server) sessionID(sdkID string) protocol.SessionID {
	if sdkID != "" {
		return protocol.SessionID(sdkID)
	}
	return s.session
}

func newSessionID() protocol.SessionID {
	var token [16]byte
	if _, err := rand.Read(token[:]); err == nil {
		return protocol.SessionID("mcp-" + hex.EncodeToString(token[:]))
	}
	return protocol.SessionID(fmt.Sprintf("mcp-%d-%d", os.Getpid(), time.Now().UnixNano()))
}

func (s *Server) workspace(ctx context.Context, req *sdk.CallToolRequest) (protocol.SessionID, protocol.WorkspaceID, error) {
	session := s.sessionID(req.GetSession().ID())
	opened, err := s.svc.OpenWorkspace(ctx, protocol.OpenWorkspaceParams{Session: session, Root: s.root})
	return session, opened.Workspace, err
}

func toolOutput(value any, err error) (*sdk.CallToolResult, map[string]any, error) {
	if err != nil {
		structured := protocol.AsError(err)
		errorObject := map[string]any{
			"code":    string(structured.Code),
			"message": structured.Message,
		}
		if structured.RetryAfterMS > 0 {
			errorObject["retry_after_ms"] = structured.RetryAfterMS
		}
		return &sdk.CallToolResult{IsError: structured.Code != protocol.ErrNoResult}, map[string]any{"error": errorObject}, nil
	}
	data, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return toolOutput(nil, protocol.NewErrorf(protocol.ErrInternal, "encoding tool result: %v", marshalErr))
	}
	var output map[string]any
	if unmarshalErr := json.Unmarshal(data, &output); unmarshalErr != nil {
		return toolOutput(nil, protocol.NewErrorf(protocol.ErrInternal, "normalizing tool result: %v", unmarshalErr))
	}
	return nil, output, nil
}
