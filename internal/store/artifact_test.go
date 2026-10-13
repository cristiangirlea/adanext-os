package store

import (
	"context"
	"errors"
	"testing"
)

func TestArtifactMigrationFromEmptyReachesVersionThree(t *testing.T) {
	s := openTestStore(t)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
		t.Fatalf("version %d: %v", version, err)
	}
}

func TestReferenceArtifactInsertsOnceAndRefusesADifferentSize(t *testing.T) {
	s := openTestStore(t)
	if err := s.write(context.Background(), "u", func(tx *Tx) error {
		if err := tx.ReferenceArtifact(ctx, "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", 12, "test"); err != nil {
			return err
		}
		return tx.ReferenceArtifact(ctx, "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", 12, "ignored")
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	var origin string
	if err := s.db.QueryRow("SELECT count(*),min(origin) FROM artifacts").Scan(&count, &origin); err != nil || count != 1 || origin != "test" {
		t.Fatalf("rows %d origin %q: %v", count, origin, err)
	}
	if err := s.write(ctx, "u", func(tx *Tx) error {
		return tx.ReferenceArtifact(ctx, "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", 13, "test")
	}); err == nil {
		t.Fatal("accepted different size")
	}
}

func TestReferenceArtifactRollsBackWithStoreDo(t *testing.T) {
	s := openTestStore(t)
	if err := s.CreateUser(ctx, "u", "user"); err != nil {
		t.Fatal(err)
	}
	hash := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if _, err := s.Do(ctx, "u", "artifact", "request", nil, func(tx *Tx) ([]byte, error) {
		return nil, tx.ReferenceArtifact(ctx, hash, 12, "test")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Do(ctx, "u", "rollback", "request", nil, func(tx *Tx) ([]byte, error) {
		return nil, tx.ReferenceArtifact(ctx, hash, 13, "test")
	}); err == nil {
		t.Fatal("accepted different size in Do")
	}
	rollback := errors.New("roll back")
	otherHash := "1111111111111111111111111111111111111111111111111111111111111111"
	if _, err := s.Do(ctx, "u", "later-error", "request", nil, func(tx *Tx) ([]byte, error) {
		if err := tx.ReferenceArtifact(ctx, otherHash, 4, "test"); err != nil {
			return nil, err
		}
		return nil, rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("rollback: %v", err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM artifacts").Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows %d: %v", count, err)
	}
}
