package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrInvalidArtifact  = errors.New("invalid artifact metadata")
	ErrArtifactConflict = errors.New("artifact size conflict")
)

// Tx is the transaction-scoped handle passed to Do. It is valid only during the
// callback. Its operations share one transaction; it exposes no commit method.
type Tx struct {
	tx     *sql.Tx
	userID string
}

// UserID is the authenticated user the transaction runs for.
func (tx *Tx) UserID() string { return tx.userID }

// ExecContext runs trusted coordinator SQL, including inserts for records.
// Raw SQL bypasses membership checks; never expose it to an untrusted caller.
func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.tx.ExecContext(ctx, query, args...)
}

func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.tx.QueryContext(ctx, query, args...)
}

func (tx *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.tx.QueryRowContext(ctx, query, args...)
}

func (tx *Tx) Transition(ctx context.Context, taskID string, from, to State, expectedRevision int64) error {
	if err := requireTaskMember(ctx, tx, tx.userID, taskID); err != nil {
		return err
	}
	return transition(ctx, tx.tx, taskID, from, to, expectedRevision)
}

func (tx *Tx) AppendEvent(ctx context.Context, event Event) error {
	if err := requireAttemptMember(ctx, tx, tx.userID, event.Attempt); err != nil {
		return err
	}
	return appendEvent(ctx, tx.tx, event)
}

// ReferenceArtifact records an artifact only after its content write has returned
// successfully. The caller enforces that ordering; this method only writes the
// reference in the current transaction.
func (tx *Tx) ReferenceArtifact(ctx context.Context, hash string, size int64, origin string) error {
	if !validArtifactHash(hash) || size < 0 || origin == "" {
		return fmt.Errorf("%w: hash, size, or origin", ErrInvalidArtifact)
	}
	if _, err := tx.tx.ExecContext(ctx, "INSERT INTO artifacts(hash,size,origin,created) VALUES(?,?,?,?) ON CONFLICT(hash) DO NOTHING", hash, size, origin, now()); err != nil {
		return err
	}
	var storedSize int64
	if err := tx.tx.QueryRowContext(ctx, "SELECT size FROM artifacts WHERE hash=?", hash).Scan(&storedSize); err != nil {
		return err
	}
	if storedSize != size {
		return fmt.Errorf("%w: artifact %s has size %d, requested %d", ErrArtifactConflict, hash, storedSize, size)
	}
	return nil
}

// ArtifactReference resolves hash to the size its record holds when a record of
// attemptID references it, that is when the row's origin is the one the executor
// writes, attempt:<id>:<record>. The table keeps the first origin of a hash, so
// content two attempts share resolves only for the attempt that stored it first.
// A hash the table would refuse is both invalid and not found.
func (s *Store) ArtifactReference(ctx context.Context, attemptID, hash string) (int64, error) {
	if !validArtifactHash(hash) {
		return 0, fmt.Errorf("%w: %w: hash", ErrNotFound, ErrInvalidArtifact)
	}
	var size int64
	var origin string
	err := s.db.QueryRowContext(ctx, "SELECT size,origin FROM artifacts WHERE hash=?", hash).Scan(&size, &origin)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !strings.HasPrefix(origin, "attempt:"+attemptID+":")) {
		return 0, fmt.Errorf("%w: artifact %s of attempt %s", ErrNotFound, hash, attemptID)
	}
	if err != nil {
		return 0, err
	}
	return size, nil
}

// ArtifactReferenced reports whether any committed record references hash, so a
// caller that wrote the blob and then failed to reference it knows whether the
// content is still someone else's before removing it.
func (s *Store) ArtifactReferenced(ctx context.Context, hash string) (bool, error) {
	if !validArtifactHash(hash) {
		return false, fmt.Errorf("%w: hash", ErrInvalidArtifact)
	}
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM artifacts WHERE hash=?", hash).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func validArtifactHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for i := 0; i < len(hash); i++ {
		if c := hash[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *Store) write(ctx context.Context, userID string, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&Tx{tx: tx, userID: userID}); err != nil {
		return err
	}
	return tx.Commit()
}
