package store

import (
	"context"
	"database/sql"
)

// Tx is the transaction-scoped handle passed to Do. It is valid only during the
// callback. Its operations share one transaction; it exposes no commit method.
type Tx struct{ tx *sql.Tx }

// ExecContext runs hand-written SQL, including inserts for coordinator records.
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
	return transition(ctx, tx.tx, taskID, from, to, expectedRevision)
}

func (tx *Tx) AppendEvent(ctx context.Context, event Event) error {
	return appendEvent(ctx, tx.tx, event)
}

func (s *Store) write(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&Tx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}
