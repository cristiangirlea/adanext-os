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

func (s *Store) AppendEvent(ctx context.Context, event Event) error {
	return s.write(ctx, func(tx *Tx) error { return tx.AppendEvent(ctx, event) })
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
func (s *Store) Events(ctx context.Context, attempt string, afterSeq int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT attempt,seq,time,kind,payload FROM events WHERE attempt=? AND seq>? ORDER BY seq", attempt, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var event Event
		var payload string
		if err := rows.Scan(&event.Attempt, &event.Seq, &event.Time, &event.Kind, &payload); err != nil {
			return nil, err
		}
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	return events, rows.Err()
}
