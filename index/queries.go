package index

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/fingerskier/langer/protocol"
)

func (s *sqliteStore) EnsureWorkspace(
	ctx context.Context,
	root, repoNamespace string,
) (protocol.WorkspaceID, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", protocol.NewErrorf(protocol.ErrWorkspaceUnknown,
			"workspace root %q must be canonical and absolute", root)
	}
	if strings.TrimSpace(repoNamespace) == "" {
		return "", internalError("ensuring workspace %s: repository namespace is required", root)
	}
	id := protocol.WorkspaceIDForRoot(root)
	now := s.clock.Now().UnixMilli()
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		var storedID protocol.WorkspaceID
		var storedNamespace string
		err := tx.QueryRowContext(ctx, `
			SELECT id, repo_namespace
			FROM workspaces WHERE root_path = ?`, root).Scan(&storedID, &storedNamespace)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO workspaces(
					id, root_path, repo_namespace, name, created_at_unix_ms,
					last_indexed_unix_ms, missing_since_unix_ms
				) VALUES (?, ?, ?, ?, ?, 0, NULL)`,
				id, root, repoNamespace, filepath.Base(root), now); err != nil {
				return internalError("creating workspace %s: %v", root, err)
			}
		case err != nil:
			return internalError("reading workspace %s: %v", root, err)
		default:
			if storedID != id {
				return internalError("workspace id collision: root %s maps to %q but database contains %q",
					root, id, storedID)
			}
			if storedNamespace != repoNamespace {
				// Every SymbolKey embeds the namespace. Keeping the old symbol
				// rows or a successful content hash would falsely declare
				// those derived identities fresh after a remote change.
				if _, err := tx.ExecContext(ctx, `
					UPDATE workspaces
					SET reference_generation = reference_generation + 1
					WHERE id = ?`, id); err != nil {
					return internalError("advancing references after namespace change for %s: %v", root, err)
				}
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM reference_sets WHERE workspace_id = ?`, id); err != nil {
					return internalError("invalidating references after namespace change for %s: %v", root, err)
				}
				if _, err := tx.ExecContext(ctx, `
					DELETE FROM symbols
					WHERE file_id IN (SELECT id FROM files WHERE workspace_id = ?)`, id); err != nil {
					return internalError("invalidating symbols after namespace change for %s: %v", root, err)
				}
				if _, err := tx.ExecContext(ctx, `
					DELETE FROM diagnostics
					WHERE file_id IN (SELECT id FROM files WHERE workspace_id = ?)`, id); err != nil {
					return internalError("invalidating diagnostics after namespace change for %s: %v", root, err)
				}
				if _, err := tx.ExecContext(ctx, `
					UPDATE files SET content_hash = '' WHERE workspace_id = ?`, id); err != nil {
					return internalError("invalidating files after namespace change for %s: %v", root, err)
				}
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE workspaces
				SET repo_namespace = ?, name = ?, missing_since_unix_ms = NULL
				WHERE id = ?`, repoNamespace, filepath.Base(root), id); err != nil {
				return internalError("refreshing workspace %s: %v", root, err)
			}
		}
		return nil
	})
	return id, err
}

func (s *sqliteStore) FileState(
	ctx context.Context,
	ws protocol.WorkspaceID,
	filePath string,
) (hash string, found bool, err error) {
	relative, pathErr := normalizeRelativePath(filePath)
	if pathErr != nil {
		return "", false, internalError("reading file state: %v", pathErr)
	}
	err = s.read.QueryRowContext(ctx, `
		SELECT f.content_hash
		FROM files f
		WHERE f.workspace_id = ? AND f.path = ?`, ws, relative).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		if _, workspaceErr := workspaceNamespace(ctx, s.read, ws); workspaceErr != nil {
			return "", false, workspaceErr
		}
		return "", false, nil
	}
	if err != nil {
		return "", false, internalError("reading file state for %s: %v", relative, err)
	}
	return hash, true, nil
}

func (s *sqliteStore) PutFile(
	ctx context.Context,
	ws protocol.WorkspaceID,
	record FileRecord,
) error {
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		namespace, err := workspaceNamespace(ctx, tx, ws)
		if err != nil {
			return err
		}
		file, err := normalizeFile(record, namespace)
		if err != nil {
			return internalError("validating indexed file %q: %v", record.Path, err)
		}
		now := s.clock.Now().UnixMilli()

		var fileID int64
		err = tx.QueryRowContext(ctx, `
			INSERT INTO files(
				workspace_id, path, absolute_path, language_id, content_hash,
				size_bytes, mtime_unix_ms, last_indexed_unix_ms
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(workspace_id, path) DO UPDATE SET
				absolute_path = excluded.absolute_path,
				language_id = excluded.language_id,
				content_hash = excluded.content_hash,
				size_bytes = excluded.size_bytes,
				mtime_unix_ms = excluded.mtime_unix_ms,
				last_indexed_unix_ms = excluded.last_indexed_unix_ms
			RETURNING id`,
			ws, file.Path, file.AbsolutePath, file.LanguageID, file.ContentHash,
			file.SizeBytes, file.ModTime.UnixMilli(), now).Scan(&fileID)
		if err != nil {
			return internalError("upserting indexed file %s: %v", file.Path, err)
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM symbols WHERE file_id = ?`, fileID); err != nil {
			return internalError("replacing symbols for %s: %v", file.Path, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM diagnostics WHERE file_id = ?`, fileID); err != nil {
			return internalError("replacing diagnostics for %s: %v", file.Path, err)
		}

		symbolStatement, err := tx.PrepareContext(ctx, `
			INSERT INTO symbols(
				file_id, name, kind, detail,
				start_line, start_col, end_line, end_col,
				selection_start_line, selection_start_col,
				selection_end_line, selection_end_col,
				container_name, documentation, stable_key, symbol_key
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)`)
		if err != nil {
			return internalError("preparing symbol replacement: %v", err)
		}
		defer symbolStatement.Close()
		for _, symbol := range file.Symbols {
			r := symbol.Symbol.Range
			selection := symbol.SelectionRange
			if _, err := symbolStatement.ExecContext(ctx,
				fileID, symbol.Symbol.Name, symbol.Symbol.Kind, symbol.Symbol.Detail,
				r.Start.Line, r.Start.Character, r.End.Line, r.End.Character,
				selection.Start.Line, selection.Start.Character,
				selection.End.Line, selection.End.Character,
				symbol.Symbol.Container, symbol.StableKey, symbol.SymbolKey,
			); err != nil {
				return internalError("inserting symbol %s in %s: %v", symbol.Symbol.Name, file.Path, err)
			}
		}
		if err := pruneOrphanReferenceSets(ctx, tx, ws); err != nil {
			return err
		}

		diagnosticStatement, err := tx.PrepareContext(ctx, `
			INSERT INTO diagnostics(
				file_id, severity, message, code, source,
				start_line, start_col, end_line, end_col, recorded_at_unix_ms
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return internalError("preparing diagnostic replacement: %v", err)
		}
		defer diagnosticStatement.Close()
		for _, diagnostic := range file.Diagnostics {
			r := diagnostic.Range
			if _, err := diagnosticStatement.ExecContext(ctx,
				fileID, diagnostic.Severity, diagnostic.Message, diagnostic.Code, diagnostic.Source,
				r.Start.Line, r.Start.Character, r.End.Line, r.End.Character, now,
			); err != nil {
				return internalError("inserting diagnostic for %s: %v", file.Path, err)
			}
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE workspaces
			SET last_indexed_unix_ms = ?, missing_since_unix_ms = NULL
			WHERE id = ?`, now, ws); err != nil {
			return internalError("updating workspace index time: %v", err)
		}
		return nil
	})
}

func (s *sqliteStore) ReferenceGeneration(
	ctx context.Context,
	ws protocol.WorkspaceID,
) (uint64, error) {
	var generation int64
	err := s.read.QueryRowContext(ctx, `
		SELECT reference_generation
		FROM workspaces
		WHERE id = ?`, ws).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, workspaceUnknown(ws)
	}
	if err != nil {
		return 0, internalError("reading reference generation for %q: %v", ws, err)
	}
	if generation < 0 {
		return 0, internalError("reference generation for %q is negative", ws)
	}
	return uint64(generation), nil
}

func (s *sqliteStore) ReplaceReferencesBySymbolKey(
	ctx context.Context,
	ws protocol.WorkspaceID,
	symbolKey string,
	expectedGeneration uint64,
	refs []Reference,
) (bool, error) {
	if symbolKey == "" {
		return false, internalError("replacing references: symbol key is required")
	}
	normalized, err := normalizeReferences(refs)
	if err != nil {
		return false, internalError("replacing references for %q: %v", symbolKey, err)
	}
	committed := false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		if _, err := workspaceNamespace(ctx, tx, ws); err != nil {
			return err
		}
		var current int64
		if err := tx.QueryRowContext(ctx, `
			SELECT reference_generation
			FROM workspaces
			WHERE id = ?`, ws).Scan(&current); err != nil {
			return internalError("checking reference generation for %q: %v", ws, err)
		}
		if current < 0 {
			return internalError("reference generation for %q is negative", ws)
		}
		if uint64(current) != expectedGeneration {
			return nil
		}
		var defined int
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1
				FROM symbols s
				JOIN files f ON f.id = s.file_id
				WHERE f.workspace_id = ? AND s.symbol_key = ?
			)`, ws, symbolKey).Scan(&defined); err != nil {
			return internalError("checking reference definition for %q: %v", symbolKey, err)
		}
		if defined == 0 {
			return nil
		}
		now := s.clock.Now().UnixMilli()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO reference_sets(workspace_id, symbol_key, complete, updated_at_unix_ms)
			VALUES (?, ?, 0, ?)
			ON CONFLICT(workspace_id, symbol_key) DO UPDATE SET
				complete = 0,
				updated_at_unix_ms = excluded.updated_at_unix_ms`,
			ws, symbolKey, now); err != nil {
			return internalError("opening reference replacement for %q: %v", symbolKey, err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM "references"
			WHERE workspace_id = ? AND symbol_key = ?`, ws, symbolKey); err != nil {
			return internalError("clearing references for %q: %v", symbolKey, err)
		}

		statement, err := tx.PrepareContext(ctx, `
			INSERT INTO "references"(
				workspace_id, symbol_key, ordinal, path,
				start_line, start_col, end_line, end_col, is_definition
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return internalError("preparing reference replacement: %v", err)
		}
		defer statement.Close()
		for ordinal, ref := range normalized {
			if _, err := statement.ExecContext(ctx,
				ws, symbolKey, ordinal, ref.Path,
				ref.Range.Start.Line, ref.Range.Start.Character,
				ref.Range.End.Line, ref.Range.End.Character,
				boolInt(ref.IsDefinition),
			); err != nil {
				return internalError("inserting reference %d for %q: %v", ordinal, symbolKey, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE reference_sets
			SET complete = 1, updated_at_unix_ms = ?
			WHERE workspace_id = ? AND symbol_key = ?`,
			now, ws, symbolKey); err != nil {
			return internalError("completing reference replacement for %q: %v", symbolKey, err)
		}
		committed = true
		return nil
	})
	return committed, err
}

func (s *sqliteStore) InvalidateFile(
	ctx context.Context,
	ws protocol.WorkspaceID,
	filePath string,
) error {
	relative, err := normalizeRelativePath(filePath)
	if err != nil {
		return internalError("invalidating file: %v", err)
	}
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		if _, err := workspaceNamespace(ctx, tx, ws); err != nil {
			return err
		}
		if err := markReferenceSetsIncomplete(ctx, tx, ws, relative, s.clock.Now().UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM symbols
			WHERE file_id IN (
				SELECT id FROM files WHERE workspace_id = ? AND path = ?
			)`, ws, relative); err != nil {
			return internalError("invalidating symbols for %s: %v", relative, err)
		}
		if err := pruneOrphanReferenceSets(ctx, tx, ws); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM diagnostics
			WHERE file_id IN (
				SELECT id FROM files WHERE workspace_id = ? AND path = ?
			)`, ws, relative); err != nil {
			return internalError("invalidating diagnostics for %s: %v", relative, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE files SET content_hash = ''
			WHERE workspace_id = ? AND path = ?`, ws, relative); err != nil {
			return internalError("invalidating file metadata for %s: %v", relative, err)
		}
		return nil
	})
}

func (s *sqliteStore) DeleteFile(
	ctx context.Context,
	ws protocol.WorkspaceID,
	filePath string,
) error {
	relative, err := normalizeRelativePath(filePath)
	if err != nil {
		return internalError("deleting file: %v", err)
	}
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		if _, err := workspaceNamespace(ctx, tx, ws); err != nil {
			return err
		}
		if err := markReferenceSetsIncomplete(ctx, tx, ws, relative, s.clock.Now().UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM files WHERE workspace_id = ? AND path = ?`,
			ws, relative); err != nil {
			return internalError("deleting file %s: %v", relative, err)
		}
		if err := pruneOrphanReferenceSets(ctx, tx, ws); err != nil {
			return err
		}
		return nil
	})
}

func markReferenceSetsIncomplete(
	ctx context.Context,
	tx *sql.Tx,
	ws protocol.WorkspaceID,
	filePath string,
	now int64,
) error {
	if err := invalidateAllReferenceSets(ctx, tx, ws, now, filePath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM "references"
		WHERE workspace_id = ? AND path = ?`, ws, filePath); err != nil {
		return internalError("invalidating reference locations for %s: %v", filePath, err)
	}
	return nil
}

func invalidateAllReferenceSets(
	ctx context.Context,
	tx *sql.Tx,
	ws protocol.WorkspaceID,
	now int64,
	reason string,
) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE workspaces
		SET reference_generation = reference_generation + 1
		WHERE id = ?`, ws)
	if err != nil {
		return internalError("advancing reference generation for %s: %v", reason, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return internalError("counting reference generation update for %s: %v", reason, err)
	} else if changed != 1 {
		return workspaceUnknown(ws)
	}

	// A changed file can acquire a reference to any symbol even when the
	// previous snapshot contained no row connecting that path to the symbol.
	// There is therefore no sound narrower invalidation predicate: every
	// complete set in this workspace becomes suspect until independently
	// rebuilt. Keeping the old locations is harmless while complete=0 and
	// preserves a coherent pre-change snapshot for debugging.
	if _, err := tx.ExecContext(ctx, `
		UPDATE reference_sets
		SET complete = 0, updated_at_unix_ms = ?
		WHERE workspace_id = ?`, now, ws); err != nil {
		return internalError("marking reference sets incomplete for %s: %v", reason, err)
	}
	return nil
}

func pruneOrphanReferenceSets(
	ctx context.Context,
	tx *sql.Tx,
	ws protocol.WorkspaceID,
) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM reference_sets
		WHERE workspace_id = ?
			AND NOT EXISTS (
				SELECT 1
				FROM symbols s
				JOIN files f ON f.id = s.file_id
				WHERE f.workspace_id = ?
					AND s.symbol_key = reference_sets.symbol_key
			)`, ws, ws); err != nil {
		return internalError("pruning orphan reference sets for %q: %v", ws, err)
	}
	return nil
}

func (s *sqliteStore) ReconcileWorkspace(
	ctx context.Context,
	ws protocol.WorkspaceID,
	existingPaths []string,
) (int, error) {
	normalized := make([]string, 0, len(existingPaths))
	seen := make(map[string]struct{}, len(existingPaths))
	for _, filePath := range existingPaths {
		relative, err := normalizeRelativePath(filePath)
		if err != nil {
			return 0, internalError("reconciling workspace: %v", err)
		}
		if _, duplicate := seen[relative]; duplicate {
			continue
		}
		seen[relative] = struct{}{}
		normalized = append(normalized, relative)
	}
	sort.Strings(normalized)

	pruned := 0
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		if _, err := workspaceNamespace(ctx, tx, ws); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`CREATE TEMP TABLE IF NOT EXISTS reconcile_paths(path TEXT PRIMARY KEY)`); err != nil {
			return internalError("creating reconciliation set: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM reconcile_paths`); err != nil {
			return internalError("clearing reconciliation set: %v", err)
		}
		insert, err := tx.PrepareContext(ctx, `INSERT INTO reconcile_paths(path) VALUES (?)`)
		if err != nil {
			return internalError("preparing reconciliation set: %v", err)
		}
		for _, relative := range normalized {
			if _, err := insert.ExecContext(ctx, relative); err != nil {
				_ = insert.Close()
				return internalError("populating reconciliation set: %v", err)
			}
		}
		if err := insert.Close(); err != nil {
			return internalError("closing reconciliation statement: %v", err)
		}

		var missing int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM files
			WHERE workspace_id = ?
				AND NOT EXISTS (
					SELECT 1 FROM reconcile_paths p WHERE p.path = files.path
				)`, ws).Scan(&missing); err != nil {
			return internalError("counting reconciled files: %v", err)
		}
		if missing > 0 {
			if err := invalidateAllReferenceSets(
				ctx,
				tx,
				ws,
				s.clock.Now().UnixMilli(),
				"workspace reconciliation",
			); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM "references"
			WHERE workspace_id = ?
				AND NOT EXISTS (
					SELECT 1 FROM reconcile_paths p WHERE p.path = "references".path
				)`, ws); err != nil {
			return internalError("pruning reconciled reference locations: %v", err)
		}
		result, err := tx.ExecContext(ctx, `
			DELETE FROM files
			WHERE workspace_id = ?
				AND NOT EXISTS (
					SELECT 1 FROM reconcile_paths p WHERE p.path = files.path
				)`, ws)
		if err != nil {
			return internalError("pruning reconciled files: %v", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return internalError("counting reconciled files: %v", err)
		}
		pruned = int(count)
		if err := pruneOrphanReferenceSets(ctx, tx, ws); err != nil {
			return err
		}
		return nil
	})
	return pruned, err
}

func (s *sqliteStore) DocumentSymbols(
	ctx context.Context,
	ws protocol.WorkspaceID,
	filePath string,
) ([]protocol.Symbol, error) {
	relative, err := normalizeRelativePath(filePath)
	if err != nil {
		return nil, internalError("reading document symbols: %v", err)
	}
	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, internalError("beginning document-symbol read: %v", err)
	}
	defer tx.Rollback()
	if err := requireFileCacheReady(ctx, tx, ws, relative); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, symbolSelect+`
		WHERE f.workspace_id = ? AND f.path = ?
		ORDER BY s.start_line, s.start_col, s.end_line, s.end_col, s.id`,
		ws, relative)
	if err != nil {
		return nil, internalError("querying document symbols for %s: %v", relative, err)
	}
	symbols, err := scanSymbols(rows)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, internalError("committing document-symbol read: %v", err)
	}
	return symbols, nil
}

func (s *sqliteStore) SearchSymbols(
	ctx context.Context,
	ws protocol.WorkspaceID,
	query string,
	limit int,
) ([]protocol.Symbol, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, internalError("beginning symbol search: %v", err)
	}
	defer tx.Rollback()
	if err := requireWorkspaceCacheReady(ctx, tx, ws); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, symbolSelect+`
		WHERE f.workspace_id = ?
		ORDER BY f.path, s.start_line, s.start_col, s.id`, ws)
	if err != nil {
		return nil, internalError("searching symbols: %v", err)
	}
	symbols, err := scanSymbols(rows)
	if err != nil {
		return nil, err
	}
	symbols = fuzzyRankSymbols(symbols, query)
	if len(symbols) > limit {
		symbols = symbols[:limit]
	}
	if err := tx.Commit(); err != nil {
		return nil, internalError("committing symbol search: %v", err)
	}
	return symbols, nil
}

const symbolSelect = `
	SELECT s.name, s.kind, s.container_name, f.path,
		s.start_line, s.start_col, s.end_line, s.end_col, s.detail
	FROM symbols s
	JOIN files f ON f.id = s.file_id
`

type rowsScanner interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}

func scanSymbols(rows rowsScanner) ([]protocol.Symbol, error) {
	defer rows.Close()
	symbols := make([]protocol.Symbol, 0)
	for rows.Next() {
		var symbol protocol.Symbol
		if err := rows.Scan(
			&symbol.Name, &symbol.Kind, &symbol.Container, &symbol.Path,
			&symbol.Range.Start.Line, &symbol.Range.Start.Character,
			&symbol.Range.End.Line, &symbol.Range.End.Character,
			&symbol.Detail,
		); err != nil {
			return nil, internalError("scanning symbols: %v", err)
		}
		symbols = append(symbols, symbol)
	}
	if err := rows.Err(); err != nil {
		return nil, internalError("iterating symbols: %v", err)
	}
	return symbols, nil
}

type fuzzyRank struct {
	class      int
	gaps       int
	start      int
	nameLength int
}

type fuzzyRankedSymbol struct {
	symbol     protocol.Symbol
	rank       fuzzyRank
	foldedName string
	ordinal    int
}

const (
	fuzzyExact = iota
	fuzzyPrefix
	fuzzySubstring
	fuzzyBoundarySubsequence
	fuzzySubsequence
	fuzzyEmptyQuery
)

// fuzzyRankSymbols implements the workspace-symbol fuzzy contract in Go.
// SQLite's built-in lower() only handles ASCII, and applying LIMIT before
// fuzzy filtering can silently discard the best matches. Ranking all symbols
// first keeps Unicode case handling and result ordering deterministic.
func fuzzyRankSymbols(symbols []protocol.Symbol, query string) []protocol.Symbol {
	ranked := make([]fuzzyRankedSymbol, 0, len(symbols))
	for ordinal, symbol := range symbols {
		rank, matched := fuzzyRankName(symbol.Name, query)
		if !matched {
			continue
		}
		ranked = append(ranked, fuzzyRankedSymbol{
			symbol:     symbol,
			rank:       rank,
			foldedName: strings.ToLower(symbol.Name),
			ordinal:    ordinal,
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left, right := ranked[i], ranked[j]
		if left.rank.class != right.rank.class {
			return left.rank.class < right.rank.class
		}
		if left.rank.gaps != right.rank.gaps {
			return left.rank.gaps < right.rank.gaps
		}
		if left.rank.start != right.rank.start {
			return left.rank.start < right.rank.start
		}
		if left.rank.nameLength != right.rank.nameLength {
			return left.rank.nameLength < right.rank.nameLength
		}
		if left.foldedName != right.foldedName {
			return left.foldedName < right.foldedName
		}
		if left.symbol.Name != right.symbol.Name {
			return left.symbol.Name < right.symbol.Name
		}
		if left.symbol.Path != right.symbol.Path {
			return left.symbol.Path < right.symbol.Path
		}
		if compared := comparePosition(left.symbol.Range.Start, right.symbol.Range.Start); compared != 0 {
			return compared < 0
		}
		if compared := comparePosition(left.symbol.Range.End, right.symbol.Range.End); compared != 0 {
			return compared < 0
		}
		if left.symbol.Kind != right.symbol.Kind {
			return left.symbol.Kind < right.symbol.Kind
		}
		if left.symbol.Container != right.symbol.Container {
			return left.symbol.Container < right.symbol.Container
		}
		if left.symbol.Detail != right.symbol.Detail {
			return left.symbol.Detail < right.symbol.Detail
		}
		return left.ordinal < right.ordinal
	})

	result := make([]protocol.Symbol, len(ranked))
	for i := range ranked {
		result[i] = ranked[i].symbol
	}
	return result
}

func fuzzyRankName(name, query string) (fuzzyRank, bool) {
	original := []rune(name)
	candidate := []rune(strings.ToLower(name))
	needle := []rune(strings.ToLower(query))
	rank := fuzzyRank{nameLength: len(candidate)}
	if len(needle) == 0 {
		rank.class = fuzzyEmptyQuery
		rank.nameLength = 0
		return rank, true
	}
	if len(needle) > len(candidate) {
		return fuzzyRank{}, false
	}
	if len(needle) == len(candidate) && runesEqualAt(candidate, needle, 0) {
		rank.class = fuzzyExact
		return rank, true
	}
	if runesEqualAt(candidate, needle, 0) {
		rank.class = fuzzyPrefix
		return rank, true
	}
	if start := contiguousRuneIndex(candidate, needle); start >= 0 {
		rank.class = fuzzySubstring
		rank.start = start
		return rank, true
	}
	if start, gaps, ok := bestRuneSubsequence(original, candidate, needle, true); ok {
		rank.class = fuzzyBoundarySubsequence
		rank.start = start
		rank.gaps = gaps
		return rank, true
	}
	if start, gaps, ok := bestRuneSubsequence(original, candidate, needle, false); ok {
		rank.class = fuzzySubsequence
		rank.start = start
		rank.gaps = gaps
		return rank, true
	}
	return fuzzyRank{}, false
}

func runesEqualAt(candidate, needle []rune, start int) bool {
	if start < 0 || start+len(needle) > len(candidate) {
		return false
	}
	for i := range needle {
		if candidate[start+i] != needle[i] {
			return false
		}
	}
	return true
}

func contiguousRuneIndex(candidate, needle []rune) int {
	for start := 0; start+len(needle) <= len(candidate); start++ {
		if runesEqualAt(candidate, needle, start) {
			return start
		}
	}
	return -1
}

func bestRuneSubsequence(
	original, candidate, needle []rune,
	boundariesOnly bool,
) (bestStart, bestGaps int, found bool) {
	for start := range candidate {
		if candidate[start] != needle[0] ||
			(boundariesOnly && !isFuzzyBoundary(original, start)) {
			continue
		}
		position := start
		matched := true
		for queryIndex := 1; queryIndex < len(needle); queryIndex++ {
			next := position + 1
			for ; next < len(candidate); next++ {
				if candidate[next] == needle[queryIndex] &&
					(!boundariesOnly || isFuzzyBoundary(original, next)) {
					break
				}
			}
			if next == len(candidate) {
				matched = false
				break
			}
			position = next
		}
		if !matched {
			continue
		}
		gaps := position - start + 1 - len(needle)
		if !found || gaps < bestGaps || (gaps == bestGaps && start < bestStart) {
			bestStart = start
			bestGaps = gaps
			found = true
		}
	}
	return bestStart, bestGaps, found
}

func isFuzzyBoundary(name []rune, index int) bool {
	if index == 0 {
		return true
	}
	previous, current := name[index-1], name[index]
	previousAlphaNumeric := unicode.IsLetter(previous) || unicode.IsDigit(previous)
	currentAlphaNumeric := unicode.IsLetter(current) || unicode.IsDigit(current)
	if !previousAlphaNumeric && currentAlphaNumeric {
		return true
	}
	if unicode.IsLower(previous) && unicode.IsUpper(current) {
		return true
	}
	if unicode.IsDigit(previous) != unicode.IsDigit(current) {
		return true
	}
	// Treat the final capital in an initialism as the next word's boundary:
	// HTTPServer therefore has boundaries at H and S.
	return unicode.IsUpper(previous) && unicode.IsUpper(current) &&
		index+1 < len(name) && unicode.IsLower(name[index+1])
}

func requireFileCacheReady(
	ctx context.Context,
	q rowQuerier,
	ws protocol.WorkspaceID,
	filePath string,
) error {
	var contentHash string
	err := q.QueryRowContext(ctx, `
		SELECT content_hash
		FROM files
		WHERE workspace_id = ? AND path = ?`, ws, filePath).Scan(&contentHash)
	if errors.Is(err, sql.ErrNoRows) {
		if _, workspaceErr := workspaceNamespace(ctx, q, ws); workspaceErr != nil {
			return workspaceErr
		}
		return protocol.NewErrorf(protocol.ErrNotReady,
			"semantic cache for %s has not been indexed", filePath).WithRetryAfterMS(250)
	}
	if err != nil {
		return internalError("reading cache state for %s: %v", filePath, err)
	}
	if contentHash == "" {
		return protocol.NewErrorf(protocol.ErrNotReady,
			"semantic cache for %s was invalidated", filePath).WithRetryAfterMS(250)
	}
	return nil
}

func requireWorkspaceCacheReady(
	ctx context.Context,
	q rowQuerier,
	ws protocol.WorkspaceID,
) error {
	if _, err := workspaceNamespace(ctx, q, ws); err != nil {
		return err
	}
	var invalidated int
	if err := q.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM files
			WHERE workspace_id = ? AND content_hash = ''
		)`, ws).Scan(&invalidated); err != nil {
		return internalError("reading workspace cache state for %q: %v", ws, err)
	}
	if invalidated != 0 {
		return protocol.NewErrorf(protocol.ErrNotReady,
			"semantic cache for workspace %q is incomplete", ws).WithRetryAfterMS(250)
	}
	return nil
}

func (s *sqliteStore) SymbolKeyAt(
	ctx context.Context,
	ws protocol.WorkspaceID,
	filePath string,
	position protocol.Position,
) (string, bool, error) {
	return s.symbolKeyAtWithHook(ctx, ws, filePath, position, nil)
}

func (s *sqliteStore) symbolKeyAtWithHook(
	ctx context.Context,
	ws protocol.WorkspaceID,
	filePath string,
	position protocol.Position,
	afterCandidates func(),
) (string, bool, error) {
	relative, err := normalizeRelativePath(filePath)
	if err != nil {
		return "", false, internalError("looking up symbol key: %v", err)
	}
	if position.Line < 0 || position.Character < 0 {
		return "", false, internalError("looking up symbol key: position cannot be negative")
	}
	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", false, internalError("beginning symbol-key read: %v", err)
	}
	defer tx.Rollback()
	if err := requireFileCacheReady(ctx, tx, ws, relative); err != nil {
		return "", false, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT s.symbol_key
		FROM symbols s
		JOIN files f ON f.id = s.file_id
		WHERE f.workspace_id = ? AND f.path = ?
			AND (
				s.selection_start_line < ?
				OR (s.selection_start_line = ? AND s.selection_start_col <= ?)
			)
			AND (
				s.selection_end_line > ?
				OR (s.selection_end_line = ? AND s.selection_end_col > ?)
				OR (
					s.selection_start_line = s.selection_end_line
					AND s.selection_start_col = s.selection_end_col
					AND s.selection_start_line = ?
					AND s.selection_start_col = ?
				)
			)
		ORDER BY s.id
		LIMIT 2`,
		ws, relative,
		position.Line, position.Line, position.Character,
		position.Line, position.Line, position.Character,
		position.Line, position.Character,
	)
	if err != nil {
		return "", false, internalError("querying symbol key at %s: %v", relative, err)
	}
	var candidates []string
	for rows.Next() {
		var candidate string
		if err := rows.Scan(&candidate); err != nil {
			_ = rows.Close()
			return "", false, internalError("scanning symbol key at %s: %v", relative, err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", false, internalError("iterating symbol keys at %s: %v", relative, err)
	}
	if err := rows.Close(); err != nil {
		return "", false, internalError("closing symbol keys at %s: %v", relative, err)
	}
	if afterCandidates != nil {
		afterCandidates()
	}
	if len(candidates) != 1 {
		if err := tx.Commit(); err != nil {
			return "", false, internalError("committing symbol-key read at %s: %v", relative, err)
		}
		return "", false, nil
	}

	var identities int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM symbols s
		JOIN files f ON f.id = s.file_id
		WHERE f.workspace_id = ? AND f.path = ? AND s.symbol_key = ?`,
		ws, relative, candidates[0]).Scan(&identities); err != nil {
		return "", false, internalError("checking symbol-key uniqueness at %s: %v", relative, err)
	}
	if identities != 1 {
		if err := tx.Commit(); err != nil {
			return "", false, internalError("committing symbol-key read at %s: %v", relative, err)
		}
		return "", false, nil
	}
	if err := tx.Commit(); err != nil {
		return "", false, internalError("committing symbol-key read at %s: %v", relative, err)
	}
	return candidates[0], true, nil
}

func (s *sqliteStore) ReferencesBySymbolKey(
	ctx context.Context,
	ws protocol.WorkspaceID,
	symbolKey string,
) ([]protocol.Location, error) {
	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, internalError("beginning reference read: %v", err)
	}
	defer tx.Rollback()

	var complete int
	err = tx.QueryRowContext(ctx, `
		SELECT complete
		FROM reference_sets
		WHERE workspace_id = ? AND symbol_key = ?`, ws, symbolKey).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		if _, workspaceErr := workspaceNamespace(ctx, tx, ws); workspaceErr != nil {
			return nil, workspaceErr
		}
		return nil, protocol.NewErrorf(protocol.ErrNotReady,
			"references for symbol %q have not been indexed", symbolKey).WithRetryAfterMS(250)
	}
	if err != nil {
		return nil, internalError("reading reference-set state for %q: %v", symbolKey, err)
	}
	if complete != 1 {
		return nil, protocol.NewErrorf(protocol.ErrNotReady,
			"references for symbol %q were invalidated", symbolKey).WithRetryAfterMS(250)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT path, start_line, start_col, end_line, end_col, is_definition
		FROM "references"
		WHERE workspace_id = ? AND symbol_key = ?
		ORDER BY ordinal`, ws, symbolKey)
	if err != nil {
		return nil, internalError("querying references for %q: %v", symbolKey, err)
	}
	locations := make([]protocol.Location, 0)
	for rows.Next() {
		var location protocol.Location
		var definition int
		if err := rows.Scan(
			&location.Path,
			&location.Range.Start.Line, &location.Range.Start.Character,
			&location.Range.End.Line, &location.Range.End.Character,
			&definition,
		); err != nil {
			_ = rows.Close()
			return nil, internalError("scanning references for %q: %v", symbolKey, err)
		}
		location.IsDefinition = definition != 0
		locations = append(locations, location)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, internalError("iterating references for %q: %v", symbolKey, err)
	}
	if err := rows.Close(); err != nil {
		return nil, internalError("closing references for %q: %v", symbolKey, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, internalError("committing reference read for %q: %v", symbolKey, err)
	}
	return locations, nil
}

func (s *sqliteStore) Diagnostics(
	ctx context.Context,
	ws protocol.WorkspaceID,
	filePath string,
) ([]protocol.Diagnostic, error) {
	args := []any{ws}
	where := `WHERE f.workspace_id = ?`
	var relative string
	if filePath != "" {
		var err error
		relative, err = normalizeRelativePath(filePath)
		if err != nil {
			return nil, internalError("reading diagnostics: %v", err)
		}
		where += ` AND f.path = ?`
		args = append(args, relative)
	}
	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, internalError("beginning diagnostic read: %v", err)
	}
	defer tx.Rollback()
	if filePath == "" {
		err = requireWorkspaceCacheReady(ctx, tx, ws)
	} else {
		err = requireFileCacheReady(ctx, tx, ws, relative)
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT f.path, d.severity, d.code, d.source, d.message,
			d.start_line, d.start_col, d.end_line, d.end_col
		FROM diagnostics d
		JOIN files f ON f.id = d.file_id
		`+where+`
		ORDER BY f.path, d.start_line, d.start_col, d.id`, args...)
	if err != nil {
		return nil, internalError("querying diagnostics: %v", err)
	}
	diagnostics := make([]protocol.Diagnostic, 0)
	for rows.Next() {
		var diagnostic protocol.Diagnostic
		if err := rows.Scan(
			&diagnostic.Path, &diagnostic.Severity, &diagnostic.Code,
			&diagnostic.Source, &diagnostic.Message,
			&diagnostic.Range.Start.Line, &diagnostic.Range.Start.Character,
			&diagnostic.Range.End.Line, &diagnostic.Range.End.Character,
		); err != nil {
			_ = rows.Close()
			return nil, internalError("scanning diagnostics: %v", err)
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, internalError("iterating diagnostics: %v", err)
	}
	if err := rows.Close(); err != nil {
		return nil, internalError("closing diagnostics: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, internalError("committing diagnostic read: %v", err)
	}
	return diagnostics, nil
}

func (s *sqliteStore) Status(
	ctx context.Context,
	ws protocol.WorkspaceID,
) (protocol.IndexStatusResult, error) {
	var result protocol.IndexStatusResult
	if err := s.read.QueryRowContext(ctx, `
		SELECT root_path, last_indexed_unix_ms
		FROM workspaces WHERE id = ?`, ws).Scan(
		&result.Root, &result.LastIndexedUnixMS,
	); errors.Is(err, sql.ErrNoRows) {
		return protocol.IndexStatusResult{}, workspaceUnknown(ws)
	} else if err != nil {
		return protocol.IndexStatusResult{}, internalError("reading index status for %q: %v", ws, err)
	}

	if err := s.read.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN content_hash <> '' THEN 1 ELSE 0 END), 0)
		FROM files WHERE workspace_id = ?`, ws).Scan(
		&result.FilesTotal, &result.FilesIndexed,
	); err != nil {
		return protocol.IndexStatusResult{}, internalError("counting index status for %q: %v", ws, err)
	}
	switch {
	case result.LastIndexedUnixMS == 0 && result.FilesTotal == 0:
		result.State = protocol.IndexIdle
	case result.FilesIndexed < result.FilesTotal:
		result.State = protocol.IndexIndexing
	default:
		result.State = protocol.IndexReady
	}
	return result, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
