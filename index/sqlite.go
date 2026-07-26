package index

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fingerskier/langer/internal/clock"
	"github.com/fingerskier/langer/protocol"

	_ "modernc.org/sqlite"
)

// Open opens the shared SQLite index as one read pool plus exactly one writer.
func Open(ctx context.Context, databasePath string, ck clock.Clock) (Store, error) {
	if ck == nil {
		ck = clock.New()
	}
	if databasePath == "" {
		return nil, internalError("opening index: database path is required")
	}
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, internalError("resolving index path %q: %v", databasePath, err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, internalError("creating index directory: %v", err)
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, internalError("creating index database: %v", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, internalError("securing index database: %v", err)
	}
	if err := file.Close(); err != nil {
		return nil, internalError("closing index bootstrap file: %v", err)
	}

	writer, err := sql.Open("sqlite", sqliteDSN(absolute, true))
	if err != nil {
		return nil, internalError("opening index writer: %v", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	if err := retrySQLiteBusy(ctx, ck, func() error {
		return writer.PingContext(ctx)
	}); err != nil {
		_ = writer.Close()
		return nil, internalError("connecting index writer: %v", err)
	}
	if err := retrySQLiteBusy(ctx, ck, func() error {
		return migrate(ctx, writer)
	}); err != nil {
		_ = writer.Close()
		return nil, internalError("migrating index database: %v", err)
	}

	read, err := sql.Open("sqlite", sqliteDSN(absolute, false))
	if err != nil {
		_ = writer.Close()
		return nil, internalError("opening index readers: %v", err)
	}
	readers := runtime.GOMAXPROCS(0)
	if readers < 4 {
		readers = 4
	}
	read.SetMaxOpenConns(readers)
	read.SetMaxIdleConns(readers)
	if err := retrySQLiteBusy(ctx, ck, func() error {
		return read.PingContext(ctx)
	}); err != nil {
		_ = closeDatabases(read, writer)
		return nil, internalError("connecting index readers: %v", err)
	}

	store := &sqliteStore{
		read:   read,
		writer: writer,
		path:   absolute,
		clock:  ck,
		owner:  newLeaseOwner(),
		stat:   os.Stat,
	}
	if err := store.secureDatabaseFiles(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func sqliteDSN(path string, immediate bool) string {
	slashPath := filepath.ToSlash(path)
	// url.URL treats "C:" before the first slash as an authority unless a
	// Windows drive path is rooted explicitly. SQLite rejects that authority;
	// file:///C:/... is the portable URI form it accepts.
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	dsn := url.URL{Scheme: "file", Path: slashPath}
	query := url.Values{}
	// busy_timeout must be installed before journal_mode: the latter can need
	// the schema lock while another daemon is concurrently opening/migrating.
	// Reversing these two makes startup itself fail fast with SQLITE_BUSY.
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	if immediate {
		query.Set("_txlock", "immediate")
	}
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func retrySQLiteBusy(ctx context.Context, ck clock.Clock, operation func() error) error {
	delay := 10 * time.Millisecond
	for attempt := 0; ; attempt++ {
		err := operation()
		if err == nil || !isSQLiteBusy(err) || attempt == 9 {
			return err
		}
		if err := ck.Sleep(ctx, delay); err != nil {
			return err
		}
		if delay < 250*time.Millisecond {
			delay *= 2
			if delay > 250*time.Millisecond {
				delay = 250 * time.Millisecond
			}
		}
	}
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "sqlite_busy") ||
		strings.Contains(message, "sqlite_locked") ||
		strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked")
}

func newLeaseOwner() string {
	var token [16]byte
	if _, err := rand.Read(token[:]); err == nil {
		return hex.EncodeToString(token[:])
	}
	// crypto/rand failure is extraordinarily rare. The SQLite lease still
	// needs an owner, and a timestamp plus process id remains process-local.
	return fmt.Sprintf("%d-%d", os.Getpid(), clock.New().Now().UnixNano())
}

func (s *sqliteStore) secureDatabaseFiles() error {
	for _, path := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return internalError("securing index file %s: %v", path, err)
		}
	}
	return nil
}

func (s *sqliteStore) Close() error {
	s.closeOnce.Do(func() {
		if err := closeDatabases(s.read, s.writer); err != nil {
			s.closeErr = internalError("closing index database: %v", err)
		}
	})
	return s.closeErr
}

func (s *sqliteStore) withWrite(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return internalError("beginning index transaction: %v", err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return internalError("committing index transaction: %v", err)
	}
	if err := s.secureDatabaseFiles(); err != nil {
		return err
	}
	return nil
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func workspaceNamespace(ctx context.Context, q rowQuerier, ws protocol.WorkspaceID) (string, error) {
	var namespace string
	err := q.QueryRowContext(ctx,
		`SELECT repo_namespace FROM workspaces WHERE id = ?`, ws).Scan(&namespace)
	if errors.Is(err, sql.ErrNoRows) {
		return "", workspaceUnknown(ws)
	}
	if err != nil {
		return "", internalError("reading workspace %q: %v", ws, err)
	}
	return namespace, nil
}
