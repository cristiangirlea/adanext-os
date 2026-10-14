package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Event sequences start at 1 independently for each attempt.
type Event struct {
	Attempt string
	Seq     int64
	Time    string
	Kind    string
	Payload json.RawMessage
}

func (s *Store) AppendEvent(ctx context.Context, userID string, event Event) error {
	return s.write(ctx, userID, func(tx *Tx) error { return tx.AppendEvent(ctx, event) })
}

func appendEvent(ctx context.Context, tx *sql.Tx, event Event) error {
	if !json.Valid(event.Payload) {
		return errors.New("event payload must be valid JSON")
	}
	var id string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM attempts WHERE id=?", event.Attempt).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: attempt %s", ErrNotFound, event.Attempt)
		}
		return err
	}
	var next int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE attempt=?", event.Attempt).Scan(&next); err != nil {
		return err
	}
	if event.Seq != next {
		return fmt.Errorf("%w: attempt %s, got %d, require %d", ErrEventSequence, event.Attempt, event.Seq, next)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO events(attempt,seq,time,kind,payload) VALUES(?,?,?,?,?)", event.Attempt, event.Seq, event.Time, event.Kind, string(event.Payload)); err != nil {
		return err
	}
	return nil
}

// Events returns events strictly after afterSeq, ordered by sequence. An unknown
// attempt or an empty tail returns an empty slice.
func (s *Store) Events(ctx context.Context, userID, attempt string, afterSeq int64) ([]Event, error) {
	return s.EventsPage(ctx, userID, attempt, afterSeq, 0, 0, nil)
}

// LastEventSequence returns the last recorded sequence for an accessible attempt.
func (s *Store) LastEventSequence(ctx context.Context, userID, attempt string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := requireAttemptMember(ctx, tx, userID, attempt); err != nil {
		return 0, err
	}
	var last int64
	err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM events WHERE attempt=?", attempt).Scan(&last)
	return last, err
}

// EventsPage reads strictly after afterSeq and stops before the first event that
// would exceed maxBytes. A zero count or byte limit is unlimited for old callers.
func (s *Store) EventsPage(ctx context.Context, userID, attempt string, afterSeq int64, limit, maxBytes int, encodedSize func(Event) int) ([]Event, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireAttemptMember(ctx, tx, userID, attempt); err != nil {
		if errors.Is(err, ErrNotFound) {
			return []Event{}, nil
		}
		return nil, err
	}
	query := "SELECT attempt,seq,time,kind,payload FROM events WHERE attempt=? AND seq>? ORDER BY seq"
	args := []any{attempt, afterSeq}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	bytesUsed := 0
	for rows.Next() {
		var event Event
		var payload string
		if err := rows.Scan(&event.Attempt, &event.Seq, &event.Time, &event.Kind, &payload); err != nil {
			return nil, err
		}
		event.Payload = json.RawMessage(payload)
		if encodedSize != nil {
			size := encodedSize(event)
			if maxBytes > 0 && bytesUsed+size > maxBytes && len(events) != 0 {
				break
			}
			bytesUsed += size
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
