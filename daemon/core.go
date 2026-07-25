// Package daemon is the long-running process of SPEC §3.1 and §8: a Unix
// socket server, single-instance locking, the idle-sunset actor, session
// bookkeeping and shutdown ordering.
//
// It owns NO semantics. Every request resolves a workspace from the registry
// and delegates; what a query MEANS is internal/workspace's problem.
package daemon

import (
	"context"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/workspace"
	"github.com/fingerskier/langer/protocol"
)

// Core implements protocol.Service in-process.
//
// The MCP frontend programs against the same interface over a socket
// (daemonctl.Client), so the whole tool surface can be tested in-process
// against a Core with no socket and no language server.
type Core struct {
	reg *workspace.Registry
	// ck is the daemon's clock. Nothing in M2 reads it — every deadline the
	// spec names lives in lsp (settle, backoff) or in the sunset actor — but it
	// is part of the frozen constructor signature and M3's index status and
	// M5's overlay TTL are both clock-driven.
	ck clock.Clock
}

var _ protocol.Service = (*Core)(nil)

// NewCore wraps a workspace registry in the SPEC §3.5 method surface.
func NewCore(reg *workspace.Registry, ck clock.Clock) *Core {
	return &Core{reg: reg, ck: ck}
}

// OpenWorkspace opens (or joins) a workspace by absolute root path.
func (c *Core) OpenWorkspace(ctx context.Context, p protocol.OpenWorkspaceParams) (protocol.OpenWorkspaceResult, error) {
	id, err := c.reg.Open(ctx, p.Root)
	if err != nil {
		return protocol.OpenWorkspaceResult{}, protocol.AsError(err)
	}
	ws, err := c.reg.Get(id)
	if err != nil {
		return protocol.OpenWorkspaceResult{}, protocol.AsError(err)
	}
	return protocol.OpenWorkspaceResult{Workspace: id, Root: ws.Root()}, nil
}

// CloseWorkspace releases a workspace and stops its language servers.
func (c *Core) CloseWorkspace(ctx context.Context, p protocol.CloseWorkspaceParams) (protocol.EmptyResult, error) {
	if err := c.reg.Close(ctx, p.Workspace); err != nil {
		return protocol.EmptyResult{}, protocol.AsError(err)
	}
	return protocol.EmptyResult{}, nil
}

// OpenDocument tells the daemon a file is active in a session.
func (c *Core) OpenDocument(ctx context.Context, p protocol.OpenDocumentParams) (protocol.EmptyResult, error) {
	ws, err := c.reg.Get(p.Workspace)
	if err != nil {
		return protocol.EmptyResult{}, protocol.AsError(err)
	}
	if err := ws.OpenDocument(ctx, p.Session, p.Path, p.LanguageID); err != nil {
		return protocol.EmptyResult{}, protocol.AsError(err)
	}
	return protocol.EmptyResult{}, nil
}

// CloseDocument drops a session's claim on a file.
func (c *Core) CloseDocument(ctx context.Context, p protocol.DocumentParams) (protocol.EmptyResult, error) {
	ws, err := c.reg.Get(p.Workspace)
	if err != nil {
		return protocol.EmptyResult{}, protocol.AsError(err)
	}
	if err := ws.CloseDocument(ctx, p.Session, p.Path); err != nil {
		return protocol.EmptyResult{}, protocol.AsError(err)
	}
	return protocol.EmptyResult{}, nil
}

// GetDefinition answers SPEC §4.2's get_definition.
func (c *Core) GetDefinition(ctx context.Context, p protocol.PositionParams) (protocol.LocationsResult, error) {
	ws, err := c.reg.Get(p.Workspace)
	if err != nil {
		return protocol.LocationsResult{}, protocol.AsError(err)
	}
	locations, err := ws.Definition(ctx, p.Session, p.Path, p.Position)
	if err != nil {
		return protocol.LocationsResult{}, protocol.AsError(err)
	}
	return protocol.LocationsResult{Locations: locations}, nil
}

// GetReferences answers SPEC §4.2's get_references.
func (c *Core) GetReferences(ctx context.Context, p protocol.PositionParams) (protocol.LocationsResult, error) {
	ws, err := c.reg.Get(p.Workspace)
	if err != nil {
		return protocol.LocationsResult{}, protocol.AsError(err)
	}
	locations, err := ws.References(ctx, p.Session, p.Path, p.Position)
	if err != nil {
		return protocol.LocationsResult{}, protocol.AsError(err)
	}
	return protocol.LocationsResult{Locations: locations}, nil
}

// GetHover answers SPEC §4.2's get_hover.
func (c *Core) GetHover(ctx context.Context, p protocol.PositionParams) (protocol.HoverResult, error) {
	ws, err := c.reg.Get(p.Workspace)
	if err != nil {
		return protocol.HoverResult{}, protocol.AsError(err)
	}
	hover, err := ws.Hover(ctx, p.Session, p.Path, p.Position)
	if err != nil {
		return protocol.HoverResult{}, protocol.AsError(err)
	}
	return protocol.HoverResult{Hover: hover}, nil
}

// DocumentSymbols answers SPEC §4.2's document_symbols.
func (c *Core) DocumentSymbols(ctx context.Context, p protocol.DocumentParams) (protocol.SymbolsResult, error) {
	ws, err := c.reg.Get(p.Workspace)
	if err != nil {
		return protocol.SymbolsResult{}, protocol.AsError(err)
	}
	symbols, err := ws.DocumentSymbols(ctx, p.Session, p.Path)
	if err != nil {
		return protocol.SymbolsResult{}, protocol.AsError(err)
	}
	return protocol.SymbolsResult{Symbols: symbols}, nil
}

// WorkspaceSymbols answers SPEC §4.2's workspace_symbols.
func (c *Core) WorkspaceSymbols(ctx context.Context, p protocol.WorkspaceSymbolsParams) (protocol.SymbolsResult, error) {
	ws, err := c.reg.Get(p.Workspace)
	if err != nil {
		return protocol.SymbolsResult{}, protocol.AsError(err)
	}
	symbols, err := ws.WorkspaceSymbols(ctx, p.Session, p.Query, p.Limit)
	if err != nil {
		return protocol.SymbolsResult{}, protocol.AsError(err)
	}
	truncated := p.Limit > 0 && len(symbols) == p.Limit
	return protocol.SymbolsResult{Symbols: symbols, Truncated: truncated}, nil
}

// GetDiagnostics answers SPEC §4.2's get_diagnostics.
func (c *Core) GetDiagnostics(ctx context.Context, p protocol.DiagnosticsParams) (protocol.DiagnosticsResult, error) {
	ws, err := c.reg.Get(p.Workspace)
	if err != nil {
		return protocol.DiagnosticsResult{}, protocol.AsError(err)
	}
	diags, stale, err := ws.Diagnostics(ctx, p.Session, p.Path)
	if err != nil {
		return protocol.DiagnosticsResult{}, protocol.AsError(err)
	}
	return protocol.DiagnosticsResult{Diagnostics: diags, PossiblyStale: stale}, nil
}

// RenameSymbol computes a rename dry-run (SPEC §4.2: dry-run is the default for
// any mutating operation).
func (c *Core) RenameSymbol(ctx context.Context, p protocol.RenameParams) (protocol.EditPlanResult, error) {
	ws, err := c.reg.Get(p.Workspace)
	if err != nil {
		return protocol.EditPlanResult{}, protocol.AsError(err)
	}
	plan, err := ws.Rename(ctx, p.Session, p.Path, p.Position, p.NewName)
	if err != nil {
		return protocol.EditPlanResult{}, protocol.AsError(err)
	}
	return plan, nil
}

// ApplyEdit applies a plan whose files have not changed since the dry-run.
func (c *Core) ApplyEdit(ctx context.Context, p protocol.ApplyEditParams) (protocol.ApplyResult, error) {
	ws, err := c.reg.Get(p.Workspace)
	if err != nil {
		return protocol.ApplyResult{}, protocol.AsError(err)
	}
	applied, err := ws.ApplyEdit(ctx, p.Session, p.EditToken)
	if err != nil {
		return protocol.ApplyResult{}, protocol.AsError(err)
	}
	return protocol.ApplyResult{Applied: applied}, nil
}

// SimulateEdit reports the diagnostics a file would produce with different
// text, writing nothing to disk.
func (c *Core) SimulateEdit(ctx context.Context, p protocol.SimulateEditParams) (protocol.DiagnosticsResult, error) {
	ws, err := c.reg.Get(p.Workspace)
	if err != nil {
		return protocol.DiagnosticsResult{}, protocol.AsError(err)
	}
	diags, stale, err := ws.SimulateEdit(ctx, p.Session, p.Path, p.NewText)
	if err != nil {
		return protocol.DiagnosticsResult{}, protocol.AsError(err)
	}
	return protocol.DiagnosticsResult{Diagnostics: diags, PossiblyStale: stale}, nil
}

// IndexStatus reports indexing progress and language server supervision state.
// It never starts a language server.
func (c *Core) IndexStatus(ctx context.Context, p protocol.IndexStatusParams) (protocol.IndexStatusResult, error) {
	ws, err := c.reg.Get(p.Workspace)
	if err != nil {
		return protocol.IndexStatusResult{}, protocol.AsError(err)
	}
	status, err := ws.Status(ctx)
	if err != nil {
		return protocol.IndexStatusResult{}, protocol.AsError(err)
	}
	return status, nil
}

// EndSession releases everything a session owns. Ending an unknown session
// succeeds: it is the state the caller asked for, not WORKSPACE_UNKNOWN.
func (c *Core) EndSession(ctx context.Context, p protocol.EndSessionParams) (protocol.EmptyResult, error) {
	c.reg.EndSession(ctx, p.Session)
	return protocol.EmptyResult{}, nil
}
