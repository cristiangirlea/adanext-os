package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrAttemptLaunched reports a cancel that found the task's attempt no longer
// pending: the launch claim won, and the cancel follows the running path.
var ErrAttemptLaunched = errors.New("attempt has left the queue")

// A pending attempt is queued; the queue is ordered by creation across all
// users. The row id is that order: attempts are inserted under SQLite's single
// writer and never deleted. The started text is not sortable: RFC3339Nano
// drops trailing zeros, so "...00.12Z" would sort after "...00.123Z".
const queueOrder = "a.rowid"

// A QueuedAttempt is a pending attempt with the project its task belongs to,
// which the launcher needs for the one-writer-per-workspace rule.
type QueuedAttempt struct {
	Attempt
	Project string
}

// NextPendingAttempt returns the oldest pending attempt of this generation
// whose project busy does not report, in creation order. The launch claim
// (pending to launching under DoOwned) still decides a race between two
// coordinators or a cancel; this read only chooses the candidate.
func (s *Store) NextPendingAttempt(ctx context.Context, generation int64, busy func(project string) bool) (QueuedAttempt, bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT a."+strings.ReplaceAll(attemptColumns, ",", ",a.")+",t.project FROM attempts a JOIN tasks t ON t.id=a.task WHERE a.state='pending' AND a.ownership_generation=? ORDER BY "+queueOrder, generation)
	if err != nil {
		return QueuedAttempt{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var a QueuedAttempt
		var run, session, ended sql.NullString
		if err := rows.Scan(&a.ID, &a.Task, &a.Actor, &a.Backend, &a.Model, &a.Effort, &a.Mode, &a.Executor, &a.OwnershipGeneration, &a.EngineSHA256, &run, &session, &a.State, &a.Started, &ended, &a.Project); err != nil {
			return QueuedAttempt{}, false, err
		}
		a.RunID, a.NativeSession, a.Ended = run.String, session.String, ended.String
		if busy != nil && busy(a.Project) {
			continue
		}
		return a, true, nil
	}
	return QueuedAttempt{}, false, rows.Err()
}

// QueuePosition is the 1-based place of the task's pending attempt among all
// pending attempts, across users, in creation order; 0 when the task has no
// pending attempt (unknown, launched or ended).
func (s *Store) QueuePosition(ctx context.Context, taskID string) (int, error) {
	var position int
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM attempts p WHERE p.state='pending' AND p.rowid<a.rowid)+1
		FROM attempts a WHERE a.task=? AND a.state='pending'`, taskID).Scan(&position)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return position, err
}

// QueuedAttempts counts the user's pending attempts: the per-user queue limit
// is checked against it in the transaction that records a start.
func (tx *Tx) QueuedAttempts(ctx context.Context, userID string) (int, error) {
	var count int
	err := tx.tx.QueryRowContext(ctx, "SELECT count(*) FROM attempts WHERE actor=? AND state='pending'", userID).Scan(&count)
	return count, err
}

// CancelQueued withdraws a queued task for a current member: in this
// transaction the task's pending attempt is closed as canceled with a cancel
// event carrying the reason, and the task moves from preparing to canceled at
// the expected revision. ErrAttemptLaunched means the attempt is no longer
// pending, so the launch claim won and the cancel has to follow the running
// path; nothing is written then.
func (tx *Tx) CancelQueued(ctx context.Context, taskID string, expectedRevision int64, reason string) (string, error) {
	if err := requireTaskMember(ctx, tx, tx.userID, taskID); err != nil {
		return "", err
	}
	return cancelQueued(ctx, tx.tx, taskID, expectedRevision, reason)
}

func cancelQueued(ctx context.Context, tx *sql.Tx, taskID string, expectedRevision int64, reason string) (string, error) {
	var state State
	var revision int64
	err := tx.QueryRowContext(ctx, "SELECT state,revision FROM tasks WHERE id=?", taskID).Scan(&state, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: task %s", ErrNotFound, taskID)
	}
	if err != nil {
		return "", err
	}
	if revision != expectedRevision {
		return "", fmt.Errorf("%w: task %s, expected %d, current %d", ErrStaleRevision, taskID, expectedRevision, revision)
	}
	if state != Preparing {
		return "", fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, state, Canceled)
	}
	var attemptID string
	err = tx.QueryRowContext(ctx, "SELECT id FROM attempts WHERE task=? AND state='pending' ORDER BY rowid DESC LIMIT 1", taskID).Scan(&attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: task %s", ErrAttemptLaunched, taskID)
	}
	if err != nil {
		return "", err
	}
	if err := withdrawAttempt(ctx, tx, attemptID, reason); err != nil {
		return "", err
	}
	return attemptID, transition(ctx, tx, taskID, Preparing, Canceled, expectedRevision)
}

// withdrawAttempt closes a pending attempt as canceled and records the reason.
// The attempt's row serialises this against the launch claim: whichever of the
// two updates runs first in SQLite's single writer finds the row pending.
func withdrawAttempt(ctx context.Context, tx *sql.Tx, attemptID, reason string) error {
	result, err := tx.ExecContext(ctx, "UPDATE attempts SET state='canceled',ended=? WHERE id=? AND state='pending'", now(), attemptID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: attempt %s", ErrAttemptLaunched, attemptID)
	}
	return appendCancelEvent(ctx, tx, attemptID, reason)
}

// requestCancel moves a running task to cancel_requested at its current
// revision and leaves any other state alone.
func requestCancel(ctx context.Context, tx *sql.Tx, taskID string) error {
	var state State
	var revision int64
	if err := tx.QueryRowContext(ctx, "SELECT state,revision FROM tasks WHERE id=?", taskID).Scan(&state, &revision); err != nil {
		return err
	}
	if state != Running {
		return nil
	}
	return transition(ctx, tx, taskID, Running, CancelRequested, revision)
}

// CancelRecorded reports whether a cancel event is on the attempt: the
// owner's stop records one on a launching attempt, and the accept that
// advances its task has to honour it.
func (tx *Tx) CancelRecorded(ctx context.Context, attemptID string) (bool, error) {
	var n int
	err := tx.tx.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE attempt=? AND kind='cancel'", attemptID).Scan(&n)
	return n != 0, err
}

func appendCancelEvent(ctx context.Context, tx *sql.Tx, attemptID, reason string) error {
	payload, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE attempt=?", attemptID).Scan(&seq); err != nil {
		return err
	}
	return appendEvent(ctx, tx, Event{Attempt: attemptID, Seq: seq, Time: now(), Kind: "cancel", Payload: payload})
}

// A StopResult says what a stop touched: the attempts it recorded the reason
// on, for their open streams to be woken, and how many of them were running
// (launching included) or queued.
type StopResult struct {
	Attempts        []string
	Running, Queued int
}

// StopWork is the owner's stop, a trusted coordinator operation: every queued
// attempt of this generation is withdrawn with its task, and every launching
// or running one gets a cancel event with the reason, for the executor to end
// through the running cancel path. A running task moves to cancel_requested
// here, in the stop's own transaction, so a result the engine returns before
// the executor acts cannot end it ok; a cancel its actor already requested
// stays as it is. A launching attempt's task is still preparing: the executor
// honours the recorded cancel when acceptance advances it.
func (tx *Tx) StopWork(ctx context.Context, generation int64, reason string) (StopResult, error) {
	var result StopResult
	rows, err := tx.tx.QueryContext(ctx, "SELECT a.id,a.task,a.state FROM attempts a WHERE a.state IN ('pending','launching','running') AND a.ownership_generation=? ORDER BY "+queueOrder, generation)
	if err != nil {
		return result, err
	}
	type open struct{ id, task, state string }
	var all []open
	for rows.Next() {
		var a open
		if err := rows.Scan(&a.id, &a.task, &a.state); err != nil {
			rows.Close()
			return result, err
		}
		all = append(all, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	for _, a := range all {
		result.Attempts = append(result.Attempts, a.id)
		if a.state != "pending" {
			if err := appendCancelEvent(ctx, tx.tx, a.id, reason); err != nil {
				return result, err
			}
			if err := requestCancel(ctx, tx.tx, a.task); err != nil {
				return result, err
			}
			result.Running++
			continue
		}
		var revision int64
		if err := tx.tx.QueryRowContext(ctx, "SELECT revision FROM tasks WHERE id=?", a.task).Scan(&revision); err != nil {
			return result, err
		}
		if _, err := cancelQueued(ctx, tx.tx, a.task, revision, reason); err != nil {
			return result, err
		}
		result.Queued++
	}
	return result, nil
}
