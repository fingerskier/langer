package index

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/protocol"
)

// Store is the complete persistence boundary for the SQLite index. No caller
// imports the SQLite driver or depends on its connection model.
type Store interface {
	EnsureWorkspace(ctx context.Context, root, repoNamespace string) (protocol.WorkspaceID, error)
	FileState(ctx context.Context, ws protocol.WorkspaceID, path string) (hash string, found bool, err error)
	PutFile(ctx context.Context, ws protocol.WorkspaceID, file FileRecord) error
	ReferenceGeneration(ctx context.Context, ws protocol.WorkspaceID) (uint64, error)
	ReplaceReferencesBySymbolKey(
		ctx context.Context,
		ws protocol.WorkspaceID,
		symbolKey string,
		expectedGeneration uint64,
		refs []Reference,
	) (committed bool, err error)
	InvalidateFile(ctx context.Context, ws protocol.WorkspaceID, path string) error
	DeleteFile(ctx context.Context, ws protocol.WorkspaceID, path string) error
	// AbandonFile removes a path row without advancing reference_generation or
	// marking every reference set incomplete. Used when soft-skipping a failed
	// index attempt that already left (or would leave) a blank-hash orphan.
	AbandonFile(ctx context.Context, ws protocol.WorkspaceID, path string) error
	ReconcileWorkspace(ctx context.Context, ws protocol.WorkspaceID, existingPaths []string) (filesPruned int, err error)
	DocumentSymbols(ctx context.Context, ws protocol.WorkspaceID, path string) ([]protocol.Symbol, error)
	SearchSymbols(ctx context.Context, ws protocol.WorkspaceID, query string, limit int) ([]protocol.Symbol, error)
	SymbolKeyAt(ctx context.Context, ws protocol.WorkspaceID, path string, pos protocol.Position) (key string, unique bool, err error)
	ReferencesBySymbolKey(ctx context.Context, ws protocol.WorkspaceID, symbolKey string) ([]protocol.Location, error)
	Diagnostics(ctx context.Context, ws protocol.WorkspaceID, path string) ([]protocol.Diagnostic, error)
	Status(ctx context.Context, ws protocol.WorkspaceID) (protocol.IndexStatusResult, error)
	GC(ctx context.Context) (ran bool, stats GCStats, err error)
	Checkpoint(ctx context.Context) error
	Close() error
}

// FileRecord is one atomic per-file cache replacement. Reference sets are
// replaced separately because one set spans many files.
type FileRecord struct {
	Path         string
	AbsolutePath string
	LanguageID   string
	ContentHash  string
	SizeBytes    int64
	ModTime      time.Time
	Symbols      []SymbolRecord
	Diagnostics  []protocol.Diagnostic
}

// SymbolRecord retains the LSP selection range used for cursor identity while
// exposing only the public Symbol shape to callers.
type SymbolRecord struct {
	Symbol         protocol.Symbol
	SelectionRange protocol.Range
	StableKey      string
	SymbolKey      string
}

// Reference is one location in the complete set for a SymbolKey.
type Reference struct {
	Path         string
	Range        protocol.Range
	IsDefinition bool
}

// GCStats reports rows made unreachable by a completed GC pass.
type GCStats struct {
	FilesPruned       int
	SymbolsPruned     int
	DiagnosticsPruned int
	WorkspacesPruned  int
}

// Binding M3 policy defaults.
const (
	DefaultGCAttemptInterval         = time.Hour
	DefaultGCLeaseDuration           = 60 * time.Second
	DefaultGCLeaseRenewal            = 20 * time.Second
	DefaultDiagnosticRetention       = 7 * 24 * time.Hour
	DefaultMissingWorkspaceRetention = 30 * 24 * time.Hour
)

type sqliteStore struct {
	read   *sql.DB
	writer *sql.DB
	path   string
	clock  clock.Clock
	owner  string
	stat   func(string) (os.FileInfo, error)

	closeOnce sync.Once
	closeErr  error
}

type normalizedFile struct {
	FileRecord
	Path        string
	Symbols     []SymbolRecord
	Diagnostics []protocol.Diagnostic
}

func normalizeFile(record FileRecord, repoNamespace string) (normalizedFile, error) {
	relative, err := normalizeRelativePath(record.Path)
	if err != nil {
		return normalizedFile{}, err
	}
	if record.AbsolutePath == "" || !filepath.IsAbs(record.AbsolutePath) {
		return normalizedFile{}, fmt.Errorf("absolute path %q is not absolute", record.AbsolutePath)
	}
	if record.LanguageID == "" {
		return normalizedFile{}, errors.New("language id is required")
	}
	if len(record.ContentHash) != 64 {
		return normalizedFile{}, fmt.Errorf("content hash must be a 64-character SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(record.ContentHash); err != nil {
		return normalizedFile{}, fmt.Errorf("content hash is not hexadecimal: %w", err)
	}
	if record.SizeBytes < 0 {
		return normalizedFile{}, errors.New("size cannot be negative")
	}

	out := normalizedFile{
		FileRecord:  record,
		Path:        relative,
		Symbols:     make([]SymbolRecord, len(record.Symbols)),
		Diagnostics: make([]protocol.Diagnostic, len(record.Diagnostics)),
	}
	for i, symbol := range record.Symbols {
		if symbol.Symbol.Name == "" {
			return normalizedFile{}, fmt.Errorf("symbol %d has no name", i)
		}
		if err := validateRange(symbol.Symbol.Range); err != nil {
			return normalizedFile{}, fmt.Errorf("symbol %d range: %w", i, err)
		}
		if err := validateRange(symbol.SelectionRange); err != nil {
			return normalizedFile{}, fmt.Errorf("symbol %d selection range: %w", i, err)
		}
		wantStable := StableKey(symbol.Symbol)
		if symbol.StableKey != wantStable {
			return normalizedFile{}, fmt.Errorf("symbol %d stable key %q does not match %q", i, symbol.StableKey, wantStable)
		}
		wantSymbol := SymbolKey(repoNamespace, relative, wantStable)
		if symbol.SymbolKey != wantSymbol {
			return normalizedFile{}, fmt.Errorf("symbol %d symbol key %q does not match %q", i, symbol.SymbolKey, wantSymbol)
		}
		symbol.Symbol.Path = relative
		out.Symbols[i] = symbol
	}
	for i, diagnostic := range record.Diagnostics {
		if err := validateRange(diagnostic.Range); err != nil {
			return normalizedFile{}, fmt.Errorf("diagnostic %d range: %w", i, err)
		}
		diagnostic.Path = relative
		out.Diagnostics[i] = diagnostic
	}
	return out, nil
}

func normalizeReferences(refs []Reference) ([]Reference, error) {
	out := make([]Reference, len(refs))
	for i, ref := range refs {
		relative, err := normalizeRelativePath(ref.Path)
		if err != nil {
			return nil, fmt.Errorf("reference %d path: %w", i, err)
		}
		if err := validateRange(ref.Range); err != nil {
			return nil, fmt.Errorf("reference %d range: %w", i, err)
		}
		ref.Path = relative
		out[i] = ref
	}
	return out, nil
}

func normalizeRelativePath(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("path is required")
	}
	raw = strings.ReplaceAll(raw, `\`, "/")
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("path %q must be workspace-relative", raw)
	}
	// Drive-qualified paths remain absolute even when tests run on Unix.
	if len(raw) >= 2 && raw[1] == ':' {
		return "", fmt.Errorf("path %q must be workspace-relative", raw)
	}
	cleaned := path.Clean(raw)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q escapes the workspace", raw)
	}
	return cleaned, nil
}

func validateRange(r protocol.Range) error {
	if r.Start.Line < 0 || r.Start.Character < 0 || r.End.Line < 0 || r.End.Character < 0 {
		return errors.New("coordinates cannot be negative")
	}
	if comparePosition(r.Start, r.End) > 0 {
		return errors.New("start follows end")
	}
	return nil
}

func comparePosition(a, b protocol.Position) int {
	if a.Line < b.Line {
		return -1
	}
	if a.Line > b.Line {
		return 1
	}
	if a.Character < b.Character {
		return -1
	}
	if a.Character > b.Character {
		return 1
	}
	return 0
}

func internalError(format string, args ...any) error {
	return protocol.NewErrorf(protocol.ErrInternal, format, args...)
}

func workspaceUnknown(ws protocol.WorkspaceID) error {
	return protocol.NewErrorf(protocol.ErrWorkspaceUnknown, "workspace %q is not indexed", ws)
}

func closeDatabases(read, writer *sql.DB) error {
	var errs []error
	if read != nil {
		errs = append(errs, read.Close())
	}
	if writer != nil {
		errs = append(errs, writer.Close())
	}
	return errors.Join(errs...)
}

var _ Store = (*sqliteStore)(nil)
