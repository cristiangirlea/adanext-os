package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"path/filepath"
	"testing"
	"testing/fstest"

	"modernc.org/sqlite"
)

func TestCachedKeyRefusesAnotherProjectAndNoProject(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateUser(ctx, "user", "User"); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"a", "b"} {
		if _, err := s.db.ExecContext(ctx, "INSERT INTO projects(id,name,source_path,created) VALUES(?,?,?,?)", project, project, project, now()); err != nil {
			t.Fatal(err)
		}
		if err := s.AddMember(ctx, project, "user", "owner"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Do(ctx, "user", "key", "hash", &Scope{ProjectID: "a"}, func(*Tx) ([]byte, error) { return []byte("cached"), nil }); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []*Scope{{ProjectID: "b"}, nil} {
		if _, err := s.Do(ctx, "user", "key", "hash", scope, func(*Tx) ([]byte, error) { t.Fatal("callback ran"); return nil, nil }); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("scope %v: got %v", scope, err)
		}
	}
}

func TestVersionOneUnscopedCacheSurvivesMigrationWithoutProjectScope(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	uriPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "_txlock=immediate"}).String()
	db := sql.OpenDB(&connector{driver: &sqlite.Driver{}, dsn: dsn})
	old := &Store{db: db, path: path}
	initial, err := fs.ReadFile(migrationFiles, "migrations/0001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := old.migrate(ctx, fstest.MapFS{"migrations/0001_initial.sql": &fstest.MapFile{Data: initial}}); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("initial version %d: %v", version, err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO users(id,name,created) VALUES('user','User','now')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO projects(id,name,source_path,created) VALUES('p','Project','source','now')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO project_members(project,user,role,added) VALUES('p','user','owner','now')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO idempotency(user,key,request_hash,response,created) VALUES('user','key','hash',?,'now')", []byte("cached")); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if err := upgraded.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("version %d: %v", version, err)
	}
	unexpected := func(*Tx) ([]byte, error) { t.Fatal("cached callback ran"); return nil, nil }
	if _, err := upgraded.Do(ctx, "user", "key", "hash", &Scope{ProjectID: "p"}, unexpected); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("scoped replay: %v", err)
	}
	response, err := upgraded.Do(ctx, "user", "key", "hash", nil, unexpected)
	if err != nil || string(response) != "cached" {
		t.Fatalf("unscoped replay: %q %v", response, err)
	}
}
