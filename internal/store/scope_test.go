package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
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
