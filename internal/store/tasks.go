package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type State string

const (
	Draft            State = "draft"
	Ready            State = "ready"
	Preparing        State = "preparing"
	Running          State = "running"
	Verifying        State = "verifying"
	AwaitingReview   State = "awaiting_review"
	Accepted         State = "accepted"
	Refinement       State = "refinement"
	Failed           State = "failed"
	CancelRequested  State = "cancel_requested"
	Canceled         State = "canceled"
	Interrupted      State = "interrupted"
	RecoveryRequired State = "recovery_required"
)

func allowedTransition(from, to State) bool {
	switch from {
	case Draft:
		return to == Ready
	case Ready:
		return to == Preparing
	case Preparing:
		return to == Running
	case Running:
		return to == Verifying || to == Failed || to == CancelRequested || to == Interrupted
	case Verifying:
		return to == AwaitingReview || to == Failed
	case AwaitingReview:
		return to == Accepted || to == Refinement
	case Refinement:
		return to == Ready
	case CancelRequested:
		return to == Canceled
	case Interrupted:
		return to == RecoveryRequired
	default:
		return false
	}
}

// Transition checks the plan's edges and atomically compares both the current
// state and revision. A state mismatch is stale even if the revision matches.
// Errors can be classified with errors.Is against the exported sentinels.
func (s *Store) Transition(ctx context.Context, taskID string, from, to State, expectedRevision int64) error {
	if !allowedTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET state=?,revision=revision+1,updated=? WHERE id=? AND state=? AND revision=?`, to, now(), taskID, from, expectedRevision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var revision int64
		err := tx.QueryRowContext(ctx, "SELECT revision FROM tasks WHERE id=?", taskID).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: task %s, expected %d, current %d", ErrStaleRevision, taskID, expectedRevision, revision)
	}
	return tx.Commit()
}
