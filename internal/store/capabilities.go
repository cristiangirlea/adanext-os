package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// ProviderConfiguration is the provider configuration and hooks a snapshot holds: work-tree paths
// under .claude/ or .codex/, or named .mcp.json, as source's inspection found them. Paths is
// bounded; Count is the total.
type ProviderConfiguration struct {
	Paths []string `json:"paths"`
	Count int      `json:"count"`
}

// ProviderChange is one provider configuration path an attempt added, altered or removed.
type ProviderChange struct {
	Path   string `json:"path"`
	Change string `json:"change"` // added, altered or removed
}

// ProviderChanges is what an attempt changed of the provider configuration in its worktree.
// Truncated means the assessment could not finish, so the attempt is flagged regardless.
type ProviderChanges struct {
	Changes   []ProviderChange `json:"changes"`
	Truncated bool             `json:"truncated"`
}

// Flagged reports whether the change set must be shown before the work is reviewed or refined.
func (c ProviderChanges) Flagged() bool { return c.Truncated || len(c.Changes) > 0 }

// decodeJSON reads a nullable JSON column; NULL is nil.
func decodeJSON[T any](column sql.NullString) (*T, error) {
	if !column.Valid {
		return nil, nil
	}
	var v T
	if err := json.Unmarshal([]byte(column.String), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// RecordProviderChanges records an attempt's provider changes, once its engine's whole tree has ended.
func (tx *Tx) RecordProviderChanges(ctx context.Context, attemptID string, changes ProviderChanges) error {
	if changes.Changes == nil {
		changes.Changes = []ProviderChange{}
	}
	data, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	result, err := tx.tx.ExecContext(ctx, "UPDATE attempts SET provider_changes=? WHERE id=?", string(data), attemptID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.Join(err, ErrNotFound)
	}
	return nil
}

// AttemptLimits reads a trusted executor attempt's turn and time limits; zero and "" are unset.
func (s *Store) AttemptLimits(ctx context.Context, attemptID string) (maxTurns int, timeout string, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT max_turns,timeout FROM attempts WHERE id=?", attemptID).Scan(&maxTurns, &timeout)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return maxTurns, timeout, err
}

// SnapshotProvider reads what a snapshot recorded of its provider configuration.
func (tx *Tx) SnapshotProvider(ctx context.Context, snapshotID string) (*ProviderConfiguration, error) {
	var column sql.NullString
	if err := tx.tx.QueryRowContext(ctx, "SELECT provider_configuration FROM source_snapshots WHERE id=?", snapshotID).Scan(&column); err != nil {
		return nil, err
	}
	return decodeJSON[ProviderConfiguration](column)
}

// UserExists reports whether id is an account of this coordinator.
func (s *Store) UserExists(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE id=?)", id).Scan(&exists)
	return exists, err
}
