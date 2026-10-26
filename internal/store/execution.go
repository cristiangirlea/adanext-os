package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrStaleOwnership = errors.New("stale attempt ownership generation")

const attemptColumns = "id,task,actor,backend,model,effort,mode,executor,ownership_generation,engine_sha256,run_id,native_session,state,started,ended"

func scanAttempt(row interface{ Scan(...any) error }) (Attempt, error) {
	var a Attempt
	var run, session, ended sql.NullString
	err := row.Scan(&a.ID, &a.Task, &a.Actor, &a.Backend, &a.Model, &a.Effort, &a.Mode, &a.Executor, &a.OwnershipGeneration, &a.EngineSHA256, &run, &session, &a.State, &a.Started, &ended)
	a.RunID, a.NativeSession, a.Ended = run.String, session.String, ended.String
	return a, err
}

// AttemptByID is for the trusted local executor. HTTP callers use AttemptProject first.
func (s *Store) AttemptByID(ctx context.Context, id string) (Attempt, error) {
	a, err := scanAttempt(s.db.QueryRowContext(ctx, "SELECT "+attemptColumns+" FROM attempts WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	return a, err
}

// AttemptPrompt reads the immutable task text for a trusted executor attempt.
func (s *Store) AttemptPrompt(ctx context.Context, attemptID string) (string, error) {
	var objective, criteria string
	err := s.db.QueryRowContext(ctx, "SELECT t.objective,t.acceptance_criteria FROM tasks t JOIN attempts a ON a.task=t.id WHERE a.id=?", attemptID).Scan(&objective, &criteria)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return objective + "\n\nAcceptance criteria:\n" + criteria, nil
}

func (s *Store) PendingAttempts(ctx context.Context, generation int64) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+attemptColumns+" FROM attempts WHERE state='pending' AND ownership_generation=? ORDER BY rowid", generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

func (s *Store) Generation(ctx context.Context) (int64, error) {
	var generation int64
	err := s.db.QueryRowContext(ctx, "SELECT generation FROM coordinator WHERE id=1").Scan(&generation)
	return generation, err
}

// DoOwned checks both the attempt and coordinator generation in the same write
// transaction as fn. A prior coordinator can never append after a restart.
func (s *Store) DoOwned(ctx context.Context, attemptID string, generation int64, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var actor string
	var current, owner int64
	err = tx.QueryRowContext(ctx, "SELECT a.actor,a.ownership_generation,c.generation FROM attempts a CROSS JOIN coordinator c WHERE a.id=? AND c.id=1", attemptID).Scan(&actor, &owner, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current != generation || owner != generation {
		return ErrStaleOwnership
	}
	if err := fn(&Tx{tx: tx, userID: actor}); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendOwnedEvent is used only inside DoOwned, which has already checked ownership.
func (tx *Tx) AppendOwnedEvent(ctx context.Context, event Event) error {
	return appendEvent(ctx, tx.tx, event)
}

func (tx *Tx) TransitionOwned(ctx context.Context, taskID string, from, to State, revision int64) error {
	return transition(ctx, tx.tx, taskID, from, to, revision)
}

// BumpAndRecover atomically takes ownership of this coordinator generation and
// resolves abandoned intents without ever launching them again.
func (s *Store) BumpAndRecover(ctx context.Context) (int64, []string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE coordinator SET generation=generation+1 WHERE id=1"); err != nil {
		return 0, nil, err
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, "SELECT generation FROM coordinator WHERE id=1").Scan(&generation); err != nil {
		return 0, nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,task,ownership_generation,state FROM attempts WHERE state IN ('pending','launching','running') AND ownership_generation<?", generation)
	if err != nil {
		return 0, nil, err
	}
	type abandoned struct {
		id, task, state string
		old             int64
	}
	var all []abandoned
	for rows.Next() {
		var a abandoned
		if err := rows.Scan(&a.id, &a.task, &a.old, &a.state); err != nil {
			rows.Close()
			return 0, nil, err
		}
		all = append(all, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, nil, err
	}
	rows.Close()
	var recovered []string
	for _, a := range all {
		var state State
		var rev int64
		if err := tx.QueryRowContext(ctx, "SELECT state,revision FROM tasks WHERE id=?", a.task).Scan(&state, &rev); err != nil {
			return 0, nil, err
		}
		if state != Preparing && state != Running && state != CancelRequested {
			return 0, nil, fmt.Errorf("recover attempt %s: task is %s", a.id, state)
		}
		if err := transition(ctx, tx, a.task, state, Interrupted, rev); err != nil {
			return 0, nil, err
		}
		if err := transition(ctx, tx, a.task, Interrupted, RecoveryRequired, rev+1); err != nil {
			return 0, nil, err
		}
		// A worktree whose provider changes were never assessed is recorded as not assessed, so it is flagged.
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET state='recovery_required',ended=?,
			provider_changes=CASE WHEN provider_changes IS NULL AND EXISTS(SELECT 1 FROM workspaces WHERE attempt=attempts.id)
				THEN '{"changes":[],"truncated":true}' ELSE provider_changes END WHERE id=?`, now(), a.id); err != nil {
			return 0, nil, err
		}
		// A queued attempt (pending behind another) is marked like a running
		// one and never relaunched; the state it was left in is recorded.
		payload, _ := json.Marshal(map[string]any{"old_generation": a.old, "attempt_state": a.state})
		var seq int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE attempt=?", a.id).Scan(&seq); err != nil {
			return 0, nil, err
		}
		if err := appendEvent(ctx, tx, Event{Attempt: a.id, Seq: seq, Time: now(), Kind: "recovery", Payload: payload}); err != nil {
			return 0, nil, err
		}
		recovered = append(recovered, a.id)
	}
	verifyingRecovered, err := recoverVerifyingTasks(ctx, tx, generation)
	if err != nil {
		return 0, nil, err
	}
	recovered = append(recovered, verifyingRecovered...)
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return generation, recovered, nil
}
