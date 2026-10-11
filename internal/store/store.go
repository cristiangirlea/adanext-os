// Package store owns Ada's durable coordinator state. Mutations that must be
// retried safely use Do; its callback receives the transaction for hand-written
// SQL over the embedded schema. Timestamps are UTC RFC3339Nano strings. A Store
// is safe for concurrent use and must be closed by its owner.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var (
	ErrStaleRevision       = errors.New("stale task revision or state")
	ErrInvalidTransition   = errors.New("invalid task transition")
	ErrNotFound            = errors.New("record not found")
	ErrIdempotencyConflict = errors.New("idempotency key has a different request hash")
	ErrEventSequence       = errors.New("event sequence is not the next sequence")
)

type Store struct {
	db   *sql.DB
	path string
}

// Open requires a file database in an existing directory. It refuses databases
// where durability settings cannot be applied or the schema is newer than Ada.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is empty")
	}
	var dsn string
	if path == ":memory:" {
		dsn = path // The WAL read-back below deliberately refuses this.
	} else {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path = absolute
		uriPath := filepath.ToSlash(path)
		if filepath.VolumeName(path) != "" {
			uriPath = "/" + uriPath
		}
		u := url.URL{Scheme: "file", Path: uriPath, RawQuery: "_txlock=immediate"}
		dsn = u.String()
	}
	d := &sqlite.Driver{}
	db := sql.OpenDB(&connector{driver: d, dsn: dsn})
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	s := &Store{db: db, path: path}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := s.migrate(context.Background(), migrationFiles); err != nil {
		db.Close()
		return nil, fmt.Errorf("open store: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// A connector initializes and verifies every physical connection, including
// replacements created by database/sql after Open returns. No global driver
// hooks or unrelated SQLite users are affected.
type connector struct {
	driver *sqlite.Driver
	dsn    string
}

func (c *connector) Driver() driver.Driver { return c.driver }

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	if err := configure(ctx, conn.(sqlite.ExecQuerierContext)); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func configure(ctx context.Context, conn sqlite.ExecQuerierContext) error {
	for _, setting := range []struct{ name, value, want string }{
		{"busy_timeout", "5000", "5000"},
		{"journal_mode", "WAL", "wal"},
		{"foreign_keys", "ON", "1"},
		{"synchronous", "FULL", "2"},
	} {
		if err := setPragma(ctx, conn, "PRAGMA "+setting.name+"="+setting.value); err != nil {
			return fmt.Errorf("set %s: %w", setting.name, err)
		}
		rows, err := conn.QueryContext(ctx, "PRAGMA "+setting.name, nil)
		if err != nil {
			return fmt.Errorf("read %s: %w", setting.name, err)
		}
		values := make([]driver.Value, 1)
		err = rows.Next(values)
		closeErr := rows.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", setting.name, err)
		}
		if closeErr != nil {
			return fmt.Errorf("read %s: %w", setting.name, closeErr)
		}
		if got := fmt.Sprint(values[0]); got != setting.want {
			return fmt.Errorf("pragma %s = %s, require %s", setting.name, got, setting.want)
		}
	}
	return nil
}

func setPragma(ctx context.Context, conn sqlite.ExecQuerierContext, query string) error {
	// SQLite can report BUSY immediately when two new databases switch to WAL,
	// even with busy_timeout set. Retry that lock conflict within the same bound.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := conn.ExecContext(ctx, query, nil)
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || sqliteErr.Code()&255 != 5 || !time.Now().Before(deadline) {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) migrate(ctx context.Context, files fs.FS) error {
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return err
	}
	// ReadDir sorts names. Require contiguous numbers so the version has one
	// unambiguous meaning and files cannot silently be skipped.
	for i, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), fmt.Sprintf("%04d_", i+1)) || !strings.HasSuffix(entry.Name(), ".sql") {
			return fmt.Errorf("invalid migration name %q", entry.Name())
		}
	}
	for {
		tx, err := s.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE serializes competing openers.
		if err != nil {
			return err
		}
		complete, err := migrateNext(ctx, tx, files, entries)
		if err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if complete {
			return nil
		}
	}
}

func migrateNext(ctx context.Context, tx *sql.Tx, files fs.FS, entries []fs.DirEntry) (bool, error) {
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return false, err
	}
	if version > len(entries) {
		return false, fmt.Errorf("database schema version %d is newer than supported version %d", version, len(entries))
	}
	if version == len(entries) {
		return true, nil
	}
	if version < 0 {
		return false, fmt.Errorf("invalid database schema version %d", version)
	}
	name := "migrations/" + entries[version].Name()
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, string(data)); err != nil {
		return false, fmt.Errorf("migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version="+strconv.Itoa(version+1)); err != nil {
		return false, err
	}
	return false, nil
}

// Do commits a callback's writes and its response atomically. Repeats with the
// same hash return the original bytes without running fn. The callback must use
// only tx for database work, must not commit or roll it back, and must not make
// external side effects. A callback error rolls back both writes and the key.
func (s *Store) Do(ctx context.Context, key, requestHash string, fn func(*sql.Tx) ([]byte, error)) ([]byte, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var hash string
	var response []byte
	err = tx.QueryRowContext(ctx, "SELECT request_hash,response FROM idempotency WHERE key=?", key).Scan(&hash, &response)
	if err == nil {
		if hash != requestHash {
			return nil, ErrIdempotencyConflict
		}
		return response, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	response, err = fn(tx)
	if err != nil {
		return nil, err
	}
	if response == nil {
		response = []byte{}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO idempotency(key,request_hash,response,created) VALUES(?,?,?,?)", key, requestHash, response, now()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return response, nil
}

// Backup uses SQLite's live, consistent snapshot. destPath must not already
// contain a database. VACUUM INTO does not change the source or checkpoint it.
func (s *Store) Backup(ctx context.Context, destPath string) error {
	if destPath == "" {
		return errors.New("backup path is empty")
	}
	_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", destPath)
	return err
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
