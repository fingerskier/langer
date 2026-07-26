package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/fingerskier/langer/protocol"
)

const (
	gcLeaseMetaKey = "gc_lease"
	gcBatchSize    = 500
)

var errGCLeaseLost = errors.New("GC lease was lost")

// GC prunes expired diagnostics and roots that have remained missing for the
// full retention period. An expiring, renewable SQLite lease makes it safe for
// every per-repository daemon to notice that shared work is due.
func (s *sqliteStore) GC(ctx context.Context) (bool, GCStats, error) {
	acquired, err := s.tryAcquireGCLease(ctx)
	if err != nil || !acquired {
		return false, GCStats{}, err
	}

	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	heartbeatDone, _ := s.startGCLeaseHeartbeat(heartbeatCtx)
	var heartbeatErr error
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if heartbeatDone != nil {
			select {
			case heartbeatErr = <-heartbeatDone:
				heartbeatDone = nil
				if heartbeatErr != nil {
					return heartbeatErr
				}
			default:
			}
		}
		return nil
	}

	stats := GCStats{}
	runErr := s.pruneExpiredDiagnostics(ctx, &stats, check)
	if runErr == nil {
		runErr = s.reconcileMissingWorkspaces(ctx, &stats, check)
	}

	cancelHeartbeat()
	if heartbeatDone != nil {
		heartbeatErr = <-heartbeatDone
	}
	releaseErr := s.releaseGCLease(context.Background())
	if heartbeatErr != nil && runErr == nil {
		runErr = heartbeatErr
	}
	return true, stats, errors.Join(runErr, releaseErr)
}

func (s *sqliteStore) tryAcquireGCLease(ctx context.Context) (bool, error) {
	acquired := false
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		now := s.clock.Now().UnixMilli()
		var value string
		err := tx.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key = ?`, gcLeaseMetaKey).Scan(&value)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return internalError("reading GC lease: %v", err)
		default:
			_, expiry, err := parseGCLease(value)
			if err != nil {
				return internalError("reading GC lease: %v", err)
			}
			// Acquisition is not renewal: an unexpired lease is busy even
			// when the same Store owns it, so two scheduler ticks in one
			// daemon cannot run GC concurrently.
			if expiry > now {
				return nil
			}
		}

		value = formatGCLease(s.owner, now+DefaultGCLeaseDuration.Milliseconds())
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO meta(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			gcLeaseMetaKey, value); err != nil {
			return internalError("acquiring GC lease: %v", err)
		}
		acquired = true
		return nil
	})
	return acquired, err
}

func (s *sqliteStore) renewGCLease(ctx context.Context) error {
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		var value string
		if err := tx.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key = ?`, gcLeaseMetaKey).Scan(&value); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errGCLeaseLost
			}
			return internalError("reading GC lease for renewal: %v", err)
		}
		owner, _, err := parseGCLease(value)
		if err != nil {
			return internalError("reading GC lease for renewal: %v", err)
		}
		if owner != s.owner {
			return errGCLeaseLost
		}
		expiry := s.clock.Now().Add(DefaultGCLeaseDuration).UnixMilli()
		if _, err := tx.ExecContext(ctx,
			`UPDATE meta SET value = ? WHERE key = ?`,
			formatGCLease(s.owner, expiry), gcLeaseMetaKey); err != nil {
			return internalError("renewing GC lease: %v", err)
		}
		return nil
	})
}

func (s *sqliteStore) releaseGCLease(ctx context.Context) error {
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		var value string
		err := tx.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key = ?`, gcLeaseMetaKey).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return internalError("reading GC lease for release: %v", err)
		}
		owner, _, err := parseGCLease(value)
		if err != nil {
			return internalError("reading GC lease for release: %v", err)
		}
		if owner != s.owner {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM meta WHERE key = ?`, gcLeaseMetaKey); err != nil {
			return internalError("releasing GC lease: %v", err)
		}
		return nil
	})
}

// startGCLeaseHeartbeat returns a terminal error channel plus a test-visible
// renewal notification. Production ignores notifications; the fake-clock test
// uses them to prove the 20-second heartbeat actually extended the lease.
func (s *sqliteStore) startGCLeaseHeartbeat(ctx context.Context) (<-chan error, <-chan struct{}) {
	done := make(chan error, 1)
	renewed := make(chan struct{}, 1)
	go func() {
		ticker := s.clock.NewTicker(DefaultGCLeaseRenewal)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C():
				if err := s.renewGCLease(ctx); err != nil {
					if ctx.Err() != nil {
						done <- nil
						return
					}
					done <- err
					return
				}
				select {
				case renewed <- struct{}{}:
				default:
				}
			}
		}
	}()
	return done, renewed
}

func formatGCLease(owner string, expiryUnixMS int64) string {
	return owner + "|" + strconv.FormatInt(expiryUnixMS, 10)
}

func parseGCLease(value string) (owner string, expiryUnixMS int64, err error) {
	separator := strings.LastIndexByte(value, '|')
	if separator <= 0 || separator == len(value)-1 {
		return "", 0, fmt.Errorf("malformed value %q", value)
	}
	expiry, err := strconv.ParseInt(value[separator+1:], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("malformed expiry in %q: %w", value, err)
	}
	return value[:separator], expiry, nil
}

func (s *sqliteStore) pruneExpiredDiagnostics(
	ctx context.Context,
	stats *GCStats,
	check func() error,
) error {
	cutoff := s.clock.Now().Add(-DefaultDiagnosticRetention).UnixMilli()
	for {
		if err := check(); err != nil {
			return err
		}

		type expiredFile struct {
			id        int64
			workspace protocol.WorkspaceID
			path      string
		}
		var (
			expired           []expiredFile
			symbolsPruned     int
			diagnosticsPruned int
		)
		err := s.withWrite(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, `
				SELECT id, workspace_id, path
				FROM files
				WHERE content_hash <> '' AND last_indexed_unix_ms <= ?
				ORDER BY id
				LIMIT ?`, cutoff, gcBatchSize)
			if err != nil {
				return internalError("listing expired diagnostic snapshots: %v", err)
			}
			for rows.Next() {
				var file expiredFile
				if err := rows.Scan(&file.id, &file.workspace, &file.path); err != nil {
					_ = rows.Close()
					return internalError("scanning expired diagnostic snapshots: %v", err)
				}
				expired = append(expired, file)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return internalError("iterating expired diagnostic snapshots: %v", err)
			}
			if err := rows.Close(); err != nil {
				return internalError("closing expired diagnostic snapshots: %v", err)
			}

			now := s.clock.Now().UnixMilli()
			for _, file := range expired {
				// Diagnostics are one part of the atomic per-file semantic
				// snapshot. Expiring only their rows would leave a successful
				// content hash that falsely claims the now-partial cache is
				// ready, and an empty diagnostic set has no row timestamp at
				// all. Invalidate the entire snapshot using the file's index
				// timestamp so both non-empty and explicitly clean results age
				// under the same policy.
				if err := markReferenceSetsIncomplete(
					ctx, tx, file.workspace, file.path, now,
				); err != nil {
					return err
				}

				var fileSymbols, fileDiagnostics int
				if err := tx.QueryRowContext(ctx,
					`SELECT COUNT(*) FROM symbols WHERE file_id = ?`,
					file.id,
				).Scan(&fileSymbols); err != nil {
					return internalError("counting expired symbols for %s: %v", file.path, err)
				}
				if err := tx.QueryRowContext(ctx,
					`SELECT COUNT(*) FROM diagnostics WHERE file_id = ?`,
					file.id,
				).Scan(&fileDiagnostics); err != nil {
					return internalError("counting expired diagnostics for %s: %v", file.path, err)
				}
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM symbols WHERE file_id = ?`, file.id); err != nil {
					return internalError("expiring symbols for %s: %v", file.path, err)
				}
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM diagnostics WHERE file_id = ?`, file.id); err != nil {
					return internalError("expiring diagnostics for %s: %v", file.path, err)
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE files SET content_hash = '' WHERE id = ?`, file.id); err != nil {
					return internalError("invalidating expired file cache for %s: %v", file.path, err)
				}
				if err := pruneOrphanReferenceSets(ctx, tx, file.workspace); err != nil {
					return err
				}
				symbolsPruned += fileSymbols
				diagnosticsPruned += fileDiagnostics
			}
			return nil
		})
		if err != nil {
			return err
		}
		stats.SymbolsPruned += symbolsPruned
		stats.DiagnosticsPruned += diagnosticsPruned
		if len(expired) < gcBatchSize {
			return nil
		}
	}
}

type workspaceRetentionRow struct {
	id           protocol.WorkspaceID
	root         string
	missingSince sql.NullInt64
}

func (s *sqliteStore) reconcileMissingWorkspaces(
	ctx context.Context,
	stats *GCStats,
	check func() error,
) error {
	rows, err := s.read.QueryContext(ctx, `
		SELECT id, root_path, missing_since_unix_ms
		FROM workspaces
		ORDER BY id`)
	if err != nil {
		return internalError("listing workspaces for GC: %v", err)
	}
	var workspaces []workspaceRetentionRow
	for rows.Next() {
		var workspace workspaceRetentionRow
		if err := rows.Scan(&workspace.id, &workspace.root, &workspace.missingSince); err != nil {
			_ = rows.Close()
			return internalError("scanning workspaces for GC: %v", err)
		}
		workspaces = append(workspaces, workspace)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return internalError("iterating workspaces for GC: %v", err)
	}
	if err := rows.Close(); err != nil {
		return internalError("closing workspace GC rows: %v", err)
	}

	for _, workspace := range workspaces {
		if err := check(); err != nil {
			return err
		}
		if _, err := s.stat(workspace.root); err == nil {
			if workspace.missingSince.Valid {
				if err := s.clearMissingSince(ctx, workspace.id); err != nil {
					return err
				}
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			// Permission, I/O, and transient mount errors are not evidence
			// that a workspace is absent. They must never age toward deletion.
			continue
		}

		now := s.clock.Now().UnixMilli()
		if !workspace.missingSince.Valid {
			if err := s.markMissingSince(ctx, workspace.id, now); err != nil {
				return err
			}
			continue
		}
		if now-workspace.missingSince.Int64 < DefaultMissingWorkspaceRetention.Milliseconds() {
			continue
		}
		// Close the stat/delete race as far as a filesystem without a watcher
		// lock permits. A workspace that reappeared since the first stat wins.
		if _, err := s.stat(workspace.root); err == nil {
			if err := s.clearMissingSince(ctx, workspace.id); err != nil {
				return err
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := s.pruneMissingWorkspace(ctx, workspace, stats); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqliteStore) markMissingSince(
	ctx context.Context,
	ws protocol.WorkspaceID,
	now int64,
) error {
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE workspaces
			SET missing_since_unix_ms = COALESCE(missing_since_unix_ms, ?)
			WHERE id = ?`, now, ws); err != nil {
			return internalError("marking workspace %q missing: %v", ws, err)
		}
		return nil
	})
}

func (s *sqliteStore) clearMissingSince(
	ctx context.Context,
	ws protocol.WorkspaceID,
) error {
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE workspaces SET missing_since_unix_ms = NULL WHERE id = ?`, ws); err != nil {
			return internalError("clearing missing marker for workspace %q: %v", ws, err)
		}
		return nil
	})
}

func (s *sqliteStore) pruneMissingWorkspace(
	ctx context.Context,
	workspace workspaceRetentionRow,
	stats *GCStats,
) error {
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		var current sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT missing_since_unix_ms FROM workspaces WHERE id = ?`,
			workspace.id).Scan(&current); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return internalError("rechecking missing workspace %q: %v", workspace.id, err)
		}
		if !current.Valid || current.Int64 != workspace.missingSince.Int64 ||
			s.clock.Now().UnixMilli()-current.Int64 < DefaultMissingWorkspaceRetention.Milliseconds() {
			return nil
		}

		var files, symbols, diagnostics int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM files WHERE workspace_id = ?`,
			workspace.id).Scan(&files); err != nil {
			return internalError("counting files for missing workspace %q: %v", workspace.id, err)
		}
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM symbols s
			JOIN files f ON f.id = s.file_id
			WHERE f.workspace_id = ?`, workspace.id).Scan(&symbols); err != nil {
			return internalError("counting symbols for missing workspace %q: %v", workspace.id, err)
		}
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM diagnostics d
			JOIN files f ON f.id = d.file_id
			WHERE f.workspace_id = ?`, workspace.id).Scan(&diagnostics); err != nil {
			return internalError("counting diagnostics for missing workspace %q: %v", workspace.id, err)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, workspace.id)
		if err != nil {
			return internalError("pruning missing workspace %q: %v", workspace.id, err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return internalError("counting pruned workspace %q: %v", workspace.id, err)
		}
		if deleted == 1 {
			stats.WorkspacesPruned++
			stats.FilesPruned += files
			stats.SymbolsPruned += symbols
			stats.DiagnosticsPruned += diagnostics
		}
		return nil
	})
}

// Checkpoint truncates the WAL and VACUUMs only when strictly more than 25% of
// the database pages are reclaimable.
func (s *sqliteStore) Checkpoint(ctx context.Context) error {
	var busy, logPages, checkpointed int
	if err := s.writer.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(
		&busy, &logPages, &checkpointed,
	); err != nil {
		return internalError("checkpointing index WAL: %v", err)
	}
	if busy != 0 {
		return internalError("checkpointing index WAL: %d readers remained busy", busy)
	}
	var pages, free int
	if err := s.writer.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return internalError("reading index page count: %v", err)
	}
	if err := s.writer.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return internalError("reading index freelist count: %v", err)
	}
	if shouldVacuum(free, pages) {
		if _, err := s.writer.ExecContext(ctx, `VACUUM`); err != nil {
			return internalError("vacuuming index database: %v", err)
		}
	}
	if err := s.secureDatabaseFiles(); err != nil {
		return err
	}
	return nil
}

func shouldVacuum(freePages, totalPages int) bool {
	return totalPages > 0 && int64(freePages)*4 > int64(totalPages)
}
