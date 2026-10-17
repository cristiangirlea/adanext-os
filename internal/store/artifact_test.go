package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestArtifactMigrationFromEmptyReachesVersionThree(t *testing.T) {
	s := openTestStore(t)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 5 {
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
	}); !errors.Is(err, ErrArtifactConflict) {
		t.Fatalf("size conflict: %v", err)
	}
}

func TestReferenceArtifactAndMigrationRefuseInvalidMetadata(t *testing.T) {
	s := openTestStore(t)
	valid := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, hash, origin string
	}{
		{"uppercase hash", strings.Repeat("A", 64), "test"},
		{"nonhex hash", strings.Repeat("g", 64), "test"},
		{"empty origin", valid, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.write(ctx, "u", func(tx *Tx) error {
				return tx.ReferenceArtifact(ctx, tc.hash, 1, tc.origin)
			})
			if !errors.Is(err, ErrInvalidArtifact) {
				t.Fatalf("ReferenceArtifact: %v", err)
			}
			err = s.write(ctx, "u", func(tx *Tx) error {
				_, err := tx.ExecContext(ctx, "INSERT INTO artifacts(hash,size,origin,created) VALUES(?,?,?,?)", tc.hash, 1, tc.origin, "now")
				return err
			})
			if err == nil {
				t.Fatal("direct INSERT accepted invalid metadata")
			}
		})
	}
}

func TestArtifactMigrationRefusesHashWithEmbeddedNUL(t *testing.T) {
	s := openTestStore(t)
	hash := strings.Repeat("a", 64) + "\x00zz"
	err := s.write(ctx, "u", func(tx *Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO artifacts(hash,size,origin,created) VALUES(?,?,?,?)", hash, 1, "test", "now")
		return err
	})
	if err == nil {
		t.Fatal("direct INSERT accepted hash with embedded NUL and trailing bytes")
	}
}

func TestReferenceArtifactRefusesNegativeSize(t *testing.T) {
	s := openTestStore(t)
	err := s.write(ctx, "u", func(tx *Tx) error {
		return tx.ReferenceArtifact(ctx, strings.Repeat("a", 64), -1, "test")
	})
	if !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("ReferenceArtifact negative size: %v", err)
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

func TestArtifactReferenceResolvesOnlyTheReferencingAttempt(t *testing.T) {
	s := openTestStore(t)
	hash := strings.Repeat("ab", 32)
	if err := s.write(ctx, "u", func(tx *Tx) error {
		return tx.ReferenceArtifact(ctx, hash, 12, "attempt:one:event:3")
	}); err != nil {
		t.Fatal(err)
	}
	size, err := s.ArtifactReference(ctx, "one", hash)
	if err != nil || size != 12 {
		t.Fatalf("referenced: size %d, %v", size, err)
	}
	if _, err := s.ArtifactReference(ctx, "one", strings.Repeat("cd", 32)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hash: %v", err)
	}
	if _, err := s.ArtifactReference(ctx, "two", hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another attempt's reference: %v", err)
	}
	if _, err := s.ArtifactReference(ctx, "on", hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attempt id prefix: %v", err)
	}
	for _, bad := range []string{strings.Repeat("AB", 32), strings.Repeat("g", 64), hash[:63], hash + "0", ""} {
		_, err := s.ArtifactReference(ctx, "one", bad)
		if !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrInvalidArtifact) {
			t.Fatalf("bad hash %q: %v", bad, err)
		}
	}
}

func TestArtifactReferencedReportsAnyCommittedReference(t *testing.T) {
	s := openTestStore(t)
	hash := strings.Repeat("ab", 32)
	if ok, err := s.ArtifactReferenced(ctx, hash); err != nil || ok {
		t.Fatalf("before: %v %v", ok, err)
	}
	if err := s.write(ctx, "u", func(tx *Tx) error {
		return tx.ReferenceArtifact(ctx, hash, 12, "attempt:one:final")
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ArtifactReferenced(ctx, hash); err != nil || !ok {
		t.Fatalf("after: %v %v", ok, err)
	}
	if _, err := s.ArtifactReferenced(ctx, strings.Repeat("AB", 32)); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("bad hash: %v", err)
	}
}
