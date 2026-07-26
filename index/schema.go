package index

import (
	"context"
	"database/sql"
	"fmt"
)

const currentSchemaVersion = 2

var migrationV1 = []string{
	`CREATE TABLE IF NOT EXISTS workspaces (
		id                    TEXT PRIMARY KEY,
		root_path             TEXT NOT NULL UNIQUE,
		repo_namespace        TEXT NOT NULL,
		name                  TEXT NOT NULL,
		created_at_unix_ms     INTEGER NOT NULL,
		last_indexed_unix_ms  INTEGER NOT NULL DEFAULT 0,
		missing_since_unix_ms INTEGER
	)`,
	`CREATE TABLE IF NOT EXISTS files (
		id                    INTEGER PRIMARY KEY,
		workspace_id          TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		path                  TEXT NOT NULL,
		absolute_path         TEXT NOT NULL,
		language_id           TEXT NOT NULL,
		content_hash          TEXT NOT NULL,
		size_bytes            INTEGER NOT NULL CHECK(size_bytes >= 0),
		mtime_unix_ms         INTEGER NOT NULL,
		last_indexed_unix_ms  INTEGER NOT NULL,
		UNIQUE(workspace_id, path)
	)`,
	`CREATE TABLE IF NOT EXISTS symbols (
		id                    INTEGER PRIMARY KEY,
		file_id               INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
		name                  TEXT NOT NULL,
		kind                  TEXT NOT NULL,
		detail                TEXT NOT NULL,
		start_line            INTEGER NOT NULL CHECK(start_line >= 0),
		start_col             INTEGER NOT NULL CHECK(start_col >= 0),
		end_line              INTEGER NOT NULL CHECK(end_line >= 0),
		end_col               INTEGER NOT NULL CHECK(end_col >= 0),
		selection_start_line  INTEGER NOT NULL CHECK(selection_start_line >= 0),
		selection_start_col   INTEGER NOT NULL CHECK(selection_start_col >= 0),
		selection_end_line    INTEGER NOT NULL CHECK(selection_end_line >= 0),
		selection_end_col     INTEGER NOT NULL CHECK(selection_end_col >= 0),
		container_name        TEXT NOT NULL,
		documentation         TEXT NOT NULL,
		stable_key            TEXT NOT NULL,
		symbol_key            TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS symbols_file ON symbols(file_id)`,
	`CREATE INDEX IF NOT EXISTS symbols_key ON symbols(symbol_key)`,
	`CREATE INDEX IF NOT EXISTS symbols_name ON symbols(name)`,
	`CREATE TABLE IF NOT EXISTS reference_sets (
		workspace_id          TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		symbol_key            TEXT NOT NULL,
		complete              INTEGER NOT NULL CHECK(complete IN (0, 1)),
		updated_at_unix_ms    INTEGER NOT NULL,
		PRIMARY KEY(workspace_id, symbol_key)
	)`,
	`CREATE TABLE IF NOT EXISTS "references" (
		id                    INTEGER PRIMARY KEY,
		workspace_id          TEXT NOT NULL,
		symbol_key            TEXT NOT NULL,
		ordinal               INTEGER NOT NULL,
		path                  TEXT NOT NULL,
		start_line            INTEGER NOT NULL CHECK(start_line >= 0),
		start_col             INTEGER NOT NULL CHECK(start_col >= 0),
		end_line              INTEGER NOT NULL CHECK(end_line >= 0),
		end_col               INTEGER NOT NULL CHECK(end_col >= 0),
		is_definition         INTEGER NOT NULL CHECK(is_definition IN (0, 1)),
		FOREIGN KEY(workspace_id, symbol_key)
			REFERENCES reference_sets(workspace_id, symbol_key) ON DELETE CASCADE,
		UNIQUE(workspace_id, symbol_key, ordinal)
	)`,
	`CREATE INDEX IF NOT EXISTS references_path ON "references"(workspace_id, path)`,
	`CREATE TABLE IF NOT EXISTS diagnostics (
		id                    INTEGER PRIMARY KEY,
		file_id               INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
		severity              TEXT NOT NULL,
		message               TEXT NOT NULL,
		code                  TEXT NOT NULL,
		source                TEXT NOT NULL,
		start_line            INTEGER NOT NULL CHECK(start_line >= 0),
		start_col             INTEGER NOT NULL CHECK(start_col >= 0),
		end_line              INTEGER NOT NULL CHECK(end_line >= 0),
		end_col               INTEGER NOT NULL CHECK(end_col >= 0),
		recorded_at_unix_ms   INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS diagnostics_file ON diagnostics(file_id)`,
	`CREATE INDEX IF NOT EXISTS diagnostics_recorded ON diagnostics(recorded_at_unix_ms)`,
}

var migrationV2 = []string{
	`ALTER TABLE workspaces
		ADD COLUMN reference_generation INTEGER NOT NULL DEFAULT 0`,
}

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create schema version table: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create meta table: %w", err)
	}

	// The marker documents the advisory migration lock in the same table used
	// for GC. The IMMEDIATE transaction supplied by the writer DSN makes the
	// compare/update process-wide and crash-safe; rollback removes a stale mark.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO meta(key, value) VALUES ('migration_lock', 'held')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}

	var version int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d",
			version, currentSchemaVersion)
	}
	if version < 1 {
		for _, statement := range migrationV1 {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply schema migration 1: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_version(version) VALUES (1)`); err != nil {
			return fmt.Errorf("record schema migration 1: %w", err)
		}
	}
	if version < 2 {
		for _, statement := range migrationV2 {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply schema migration 2: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_version(version) VALUES (2)`); err != nil {
			return fmt.Errorf("record schema migration 2: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key = 'migration_lock'`); err != nil {
		return fmt.Errorf("release migration lock: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}
