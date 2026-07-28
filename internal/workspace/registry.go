// Package workspace is the per-root actor and the semantic brain of the daemon.
//
// Everything the daemon must NOT decide lives here: which language server
// answers a query, whether the server's copy of a file is still the file on
// disk, which SPEC §3.6 code a failure becomes, and (from M5) speculative
// overlays and edit tokens. daemon/ owns sockets, locks and lifecycle; it owns
// no semantics.
//
// M2 answers every query LIVE from the language server. The SQLite index and
// the file watcher arrive in M3; until then the freshness rule is implemented
// by re-reading the file before each query (see ensureDocument).
package workspace

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/index"
	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/internal/watch"
	"github.com/fingerskier/langer/lsp"
	"github.com/fingerskier/langer/protocol"
	"github.com/fingerskier/langer/tools"
)

// RegistryOptions configures a Registry.
//
// NewSupervisor is a factory rather than a value because a Registry may hold
// several workspaces, each of which owns its own language servers (SPEC §3.2:
// one language server per language per repo). It defaults to lsp.NewSupervisor;
// tests substitute it, exactly as the M3 NewScanner seam will.
type RegistryOptions struct {
	Config         *config.Config
	Store          index.Store
	Clock          clock.Clock
	Logger         *slog.Logger
	Resolver       procx.Resolver
	Runner         procx.Runner
	NewScanner     func(procx.Resolver, procx.Runner) watch.Scanner
	NewWatcher     func(root string, sc watch.Scanner, ck clock.Clock, debounce time.Duration) (watch.Watcher, error)
	OnFileActivity func()
	NewSupervisor  func(lsp.Options) (lsp.Supervisor, error)
	// Tools manages lazy language-server installs under ~/.langer/tools.
	// When nil, only user-configured language_servers are used.
	Tools *tools.Manager
	// OverlayTTL is how long a session's speculative text lives without use
	// (SPEC §4.2). Zero means defaultOverlayTTL (5 minutes).
	OverlayTTL time.Duration
}

func (o *RegistryOptions) applyDefaults() {
	if o.Config == nil {
		o.Config = &config.Config{LogLevel: config.DefaultLogLevel}
	}
	if o.Clock == nil {
		o.Clock = clock.New()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Resolver == nil {
		o.Resolver = procx.NewResolver()
	}
	if o.Runner == nil {
		o.Runner = procx.NewRunner()
	}
	if o.NewScanner == nil {
		o.NewScanner = watch.NewScanner
	}
	if o.NewWatcher == nil {
		o.NewWatcher = watch.NewWatcher
	}
	if o.NewSupervisor == nil {
		o.NewSupervisor = lsp.NewSupervisor
	}
	if o.OverlayTTL <= 0 {
		o.OverlayTTL = defaultOverlayTTL
	}
}

// Registry owns every open workspace in a daemon: one Workspace per absolute
// root (SPEC §3.2).
type Registry struct {
	opts RegistryOptions

	mu      sync.Mutex
	byID    map[protocol.WorkspaceID]*Workspace
	closing bool
}

// NewRegistry builds an empty Registry. It starts nothing.
func NewRegistry(opts RegistryOptions) *Registry {
	opts.applyDefaults()
	return &Registry{opts: opts, byID: map[protocol.WorkspaceID]*Workspace{}}
}

// WorkspaceIDFor is the deterministic identifier of a root. It is derived from
// the path rather than handed out sequentially so a reconnecting client that
// remembers an id from a previous daemon generation is not silently talking
// about a different workspace.
func WorkspaceIDFor(root string) protocol.WorkspaceID {
	return protocol.WorkspaceIDForRoot(root)
}

// Open opens (or joins) the workspace rooted at root. Roots are identified by
// absolute path with symlinks resolved, so /tmp and /private/tmp are one
// workspace rather than two half-indexed ones.
func (r *Registry) Open(ctx context.Context, root string) (protocol.WorkspaceID, error) {
	canonical, err := CanonicalRoot(root)
	if err != nil {
		return "", err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return "", protocol.NewError(protocol.ErrNotReady, "daemon is shutting down").WithRetryAfterMS(500)
	}
	id := WorkspaceIDFor(canonical)
	if _, ok := r.byID[id]; ok {
		return id, nil
	}

	// Construction starts the watcher and index actors. Keep it inside the
	// registry critical section so concurrent first opens cannot create two
	// independent actors mutating the same workspace rows. A daemon serves one
	// canonical root, so serializing this rare path has no cross-root cost in
	// production.
	ws, err := newWorkspace(ctx, id, canonical, r.opts)
	if err != nil {
		return "", err
	}
	r.byID[id] = ws
	return id, nil
}

// Get returns an open workspace, or WORKSPACE_UNKNOWN.
func (r *Registry) Get(id protocol.WorkspaceID) (*Workspace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ws, ok := r.byID[id]
	if !ok {
		return nil, protocol.NewErrorf(protocol.ErrWorkspaceUnknown,
			"workspace %q is not open; call open_workspace first", id)
	}
	return ws, nil
}

// Close tears one workspace down, stopping its language servers.
func (r *Registry) Close(ctx context.Context, id protocol.WorkspaceID) error {
	r.mu.Lock()
	ws, ok := r.byID[id]
	delete(r.byID, id)
	r.mu.Unlock()

	if !ok {
		// Closing something that is already closed is not a failure; it is the
		// state the caller asked for.
		return nil
	}
	return ws.shutdown(ctx)
}

// EndSession releases everything a session owns across every workspace. It is
// idempotent: an unknown session is a no-op, not an error.
func (r *Registry) EndSession(ctx context.Context, sid protocol.SessionID) {
	r.mu.Lock()
	all := make([]*Workspace, 0, len(r.byID))
	for _, ws := range r.byID {
		all = append(all, ws)
	}
	r.mu.Unlock()

	for _, ws := range all {
		ws.EndSession(ctx, sid)
	}
}

// Shutdown tears every workspace down and does not return until each one's
// language servers have exited.
func (r *Registry) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return nil
	}
	r.closing = true
	all := make([]*Workspace, 0, len(r.byID))
	for id, ws := range r.byID {
		all = append(all, ws)
		delete(r.byID, id)
	}
	r.mu.Unlock()

	var firstErr error
	for _, ws := range all {
		if err := ws.shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// CanonicalRoot resolves a workspace root to the absolute, symlink-free
// directory path SPEC §3.2 keys workspaces by.
func CanonicalRoot(root string) (string, error) {
	if root == "" {
		return "", protocol.NewError(protocol.ErrWorkspaceUnknown, "a workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown, "resolving workspace root %q: %v", root, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown, "workspace root %s: %v", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown, "workspace root %s: %v", resolved, err)
	}
	if !info.IsDir() {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown, "workspace root %s is not a directory", resolved)
	}
	return resolved, nil
}
