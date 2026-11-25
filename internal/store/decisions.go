package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PermissionPolicyVersion is the version of the rules a decision was made under; an allow
// recorded under another version matches no attempt.
const PermissionPolicyVersion = 1

// DecisionLifetime is how long an allow stays usable after it is made.
const DecisionLifetime = 24 * time.Hour

var (
	ErrLastOwner        = errors.New("the project's last owner")
	ErrDecisionRefused  = errors.New("not allowed to make this decision")
	ErrDecisionRequired = errors.New("the attempt needs an allow decision")
)

// Project modes from the least to the most an engine may do.
var modeRanks = map[string]int{"readonly": 0, "auto": 1, "yolo": 2}

// ValidMode reports whether mode can be a project's default.
func ValidMode(mode string) bool {
	_, ok := modeRanks[mode]
	return ok
}

// NeedsDecision reports whether an attempt in mode needs a decision under a project's default;
// a mode the ranking does not know always does.
func NeedsDecision(mode, projectDefault string) bool {
	rank, ok := modeRanks[mode]
	if !ok {
		return true
	}
	return rank > modeRanks[projectDefault]
}

// DecisionScope is what an allow permits: the backend, the mode and the workspace.
type DecisionScope struct{ Backend, Mode, Workspace string }

// Decision is an attempt's permission state as its actor and the project's owners see it.
type Decision struct {
	Actor          string // who may allow; a project owner may only deny
	Required       bool
	Scope          DecisionScope
	ProjectDefault string
	Allowed        bool // a matching, unexpired allow exists
	Denied         bool
}

// bindings are an attempt's current values of the five things a decision binds.
type bindings struct {
	attempt, task, project, actor, state string
	revision                             int64
	snapshot                             string
	scope                                DecisionScope
	projectDefault                       string
	generation                           int64 // the coordinator's current generation
}

func readBindings(ctx context.Context, q execQuerier, attemptID string) (bindings, error) {
	b := bindings{attempt: attemptID}
	err := q.QueryRowContext(ctx, `SELECT a.task,t.project,a.actor,a.state,t.revision,COALESCE(t.snapshot,''),a.backend,a.mode,a.executor,p.default_mode,c.generation
		FROM attempts a JOIN tasks t ON t.id=a.task JOIN projects p ON p.id=t.project CROSS JOIN coordinator c WHERE a.id=? AND c.id=1`, attemptID).Scan(
		&b.task, &b.project, &b.actor, &b.state, &b.revision, &b.snapshot, &b.scope.Backend, &b.scope.Mode, &b.scope.Workspace, &b.projectDefault, &b.generation)
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("%w: attempt %s", ErrNotFound, attemptID)
	}
	return b, err
}

func (b bindings) required() bool { return NeedsDecision(b.scope.Mode, b.projectDefault) }

// allowed reports whether an allow matching every binding is unexpired at at.
func allowed(ctx context.Context, q execQuerier, b bindings, at time.Time) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT expires FROM permission_decisions WHERE attempt=? AND decision='allow' AND task_revision=? AND snapshot=?
		AND backend=? AND mode=? AND workspace=? AND policy_version=? AND generation=?`,
		b.attempt, b.revision, b.snapshot, b.scope.Backend, b.scope.Mode, b.scope.Workspace, PermissionPolicyVersion, b.generation)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	ok := false
	for rows.Next() {
		var expires string
		if err := rows.Scan(&expires); err != nil {
			return false, err
		}
		until, err := time.Parse(time.RFC3339Nano, expires)
		if err != nil {
			return false, fmt.Errorf("decision expiry: %w", err)
		}
		if at.Before(until) {
			ok = true
		}
	}
	return ok, rows.Err()
}

// launchable reports whether the attempt may move to launching at at: it needs no decision, or
// a matching allow is unexpired.
func launchable(ctx context.Context, q execQuerier, attemptID string, at time.Time) (bool, error) {
	b, err := readBindings(ctx, q, attemptID)
	if err != nil {
		return false, err
	}
	if !b.required() {
		return true, nil
	}
	return allowed(ctx, q, b, at)
}

// ClaimLaunch is the one move of an attempt from pending to launching, for the trusted executor
// inside DoOwned: ErrDecisionRequired while it needs a decision with no matching allow at at,
// ErrAttemptLaunched when it is no longer pending. It returns the attempt as the claim judged it,
// which is what the launch uses.
func (tx *Tx) ClaimLaunch(ctx context.Context, attemptID string, at time.Time) (Attempt, error) {
	ok, err := launchable(ctx, tx.tx, attemptID, at)
	if err != nil {
		return Attempt{}, err
	}
	if !ok {
		return Attempt{}, fmt.Errorf("%w: attempt %s", ErrDecisionRequired, attemptID)
	}
	result, err := tx.tx.ExecContext(ctx, "UPDATE attempts SET state='launching' WHERE id=? AND state='pending'", attemptID)
	if err != nil {
		return Attempt{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Attempt{}, err
	}
	if n != 1 {
		return Attempt{}, fmt.Errorf("%w: attempt %s", ErrAttemptLaunched, attemptID)
	}
	row := tx.tx.QueryRowContext(ctx, "SELECT "+attemptColumns+",max_turns,timeout FROM attempts WHERE id=?", attemptID)
	var maxTurns int
	var timeout string
	a, err := scanAttempt(scanWith{row, []any{&maxTurns, &timeout}})
	a.MaxTurns, a.Timeout = maxTurns, timeout
	return a, err
}

// scanWith scans a row's leading columns into the destinations given and the rest into extra.
type scanWith struct {
	row   interface{ Scan(...any) error }
	extra []any
}

func (s scanWith) Scan(dest ...any) error { return s.row.Scan(append(dest, s.extra...)...) }

// decisionFor is the attempt's decision as user may see it: nil for anyone but its actor and the
// project's owners, and nil while it neither waits for a decision nor has one recorded.
func decisionFor(ctx context.Context, q execQuerier, user, attemptID string, at time.Time) (*Decision, error) {
	b, err := readBindings(ctx, q, attemptID)
	if err != nil {
		return nil, err
	}
	if user != b.actor {
		var role string
		err := q.QueryRowContext(ctx, "SELECT role FROM project_members WHERE project=? AND user=?", b.project, user).Scan(&role)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if role != "owner" {
			return nil, nil
		}
	}
	var recorded, denied bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM permission_decisions WHERE attempt=?),EXISTS(SELECT 1 FROM permission_decisions WHERE attempt=? AND decision='deny')", attemptID, attemptID).Scan(&recorded, &denied); err != nil {
		return nil, err
	}
	required := b.required()
	if !recorded && !(required && b.state == "pending") {
		return nil, nil
	}
	ok, err := allowed(ctx, q, b, at)
	if err != nil {
		return nil, err
	}
	return &Decision{Actor: b.actor, Required: required, Scope: b.scope, ProjectDefault: b.projectDefault, Allowed: ok, Denied: denied}, nil
}

// populateDecision sets the task's attempt decision as user sees it at at.
func populateDecision(ctx context.Context, q execQuerier, user string, task *Task, at time.Time) error {
	if task.Attempt.ID == "" {
		return nil
	}
	d, err := decisionFor(ctx, q, user, task.Attempt.ID, at)
	task.Attempt.Decision = d
	return err
}

// DeniedReason closes an attempt its actor or an owner denied.
const DeniedReason = "permission denied"

// A DecisionResult says what a decision did: the task it belongs to, and whether it denied.
type DecisionResult struct {
	Task   string
	Denied bool
}

// Decide records the transaction's user's decision, "allow" or "deny", on a pending attempt
// that needs one, at at. The actor may allow or deny; a project owner who is not the actor may
// only deny; anyone else is refused with ErrDecisionRefused, and a non-member with ErrNotFound.
// An attempt no longer pending or needing no decision is ErrInvalidTransition. A deny closes the
// attempt canceled with DeniedReason, and its task with it. The same key with the same attempt
// and decision returns the first result; a different one under the key is ErrIdempotencyConflict.
func (tx *Tx) Decide(ctx context.Context, attemptID, decision, key string, at time.Time) (DecisionResult, error) {
	if decision != "allow" && decision != "deny" {
		return DecisionResult{}, fmt.Errorf("decision %q", decision)
	}
	if replay, ok, err := decisionReplay(ctx, tx.tx, tx.userID, key, attemptID, decision); err != nil || ok {
		return replay, err
	}
	if err := requireAttemptMember(ctx, tx.tx, tx.userID, attemptID); err != nil {
		return DecisionResult{}, err
	}
	b, err := readBindings(ctx, tx.tx, attemptID)
	if err != nil {
		return DecisionResult{}, err
	}
	if tx.userID != b.actor {
		var role string
		if err := tx.tx.QueryRowContext(ctx, "SELECT role FROM project_members WHERE project=? AND user=?", b.project, tx.userID).Scan(&role); err != nil {
			return DecisionResult{}, err
		}
		if role != "owner" || decision != "deny" {
			return DecisionResult{}, ErrDecisionRefused
		}
	}
	if b.state != "pending" || !b.required() {
		return DecisionResult{}, fmt.Errorf("%w: attempt %s is %s and needs no decision", ErrInvalidTransition, attemptID, b.state)
	}
	if err := insertDecision(ctx, tx.tx, b, decision, tx.userID, key, at); err != nil {
		return DecisionResult{}, err
	}
	if decision == "deny" {
		if _, err := cancelQueued(ctx, tx.tx, b.task, b.revision, DeniedReason); err != nil {
			return DecisionResult{}, err
		}
	}
	return DecisionResult{Task: b.task, Denied: decision == "deny"}, nil
}

func decisionReplay(ctx context.Context, q execQuerier, user, key, attemptID, decision string) (DecisionResult, bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT d.attempt,d.decision,a.task FROM permission_decisions d JOIN attempts a ON a.id=d.attempt WHERE d.decided_by=? AND d.idempotency_key=?", user, key)
	if err != nil {
		return DecisionResult{}, false, err
	}
	defer rows.Close()
	var result DecisionResult
	found := false
	for rows.Next() {
		var attempt, recorded string
		if err := rows.Scan(&attempt, &recorded, &result.Task); err != nil {
			return DecisionResult{}, false, err
		}
		if attempt != attemptID || recorded != decision {
			return DecisionResult{}, false, ErrIdempotencyConflict
		}
		found = true
	}
	result.Denied = decision == "deny"
	return result, found, rows.Err()
}

func insertDecision(ctx context.Context, q execQuerier, b bindings, decision, by, key string, at time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO permission_decisions(id,attempt,task_revision,snapshot,backend,mode,workspace,policy_version,generation,decision,decided_by,decided,expires,idempotency_key)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, newReviewID("decision-"), b.attempt, b.revision, b.snapshot, b.scope.Backend, b.scope.Mode, b.scope.Workspace, PermissionPolicyVersion, b.generation,
		decision, by, stamp(at), stamp(at.Add(DecisionLifetime)), key)
	return err
}

// SetDefaultMode sets a project's default mode; pending attempts are judged again at their claim.
func (tx *Tx) SetDefaultMode(ctx context.Context, project, mode string) error {
	if !ValidMode(mode) {
		return fmt.Errorf("mode %q", mode)
	}
	result, err := tx.tx.ExecContext(ctx, "UPDATE projects SET default_mode=? WHERE id=?", mode, project)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%w: project %s", ErrNotFound, project)
	}
	return nil
}

// Member is a project member with the account's name.
type Member struct{ User, Name, Role, Added string }

// Members lists a project's members with their account names, oldest first.
func (s *Store) Members(ctx context.Context, project string) ([]Member, error) {
	return members(ctx, s.db, project)
}

// Members is Store.Members inside the transaction.
func (tx *Tx) Members(ctx context.Context, project string) ([]Member, error) {
	return members(ctx, tx.tx, project)
}

func members(ctx context.Context, q execQuerier, project string) ([]Member, error) {
	var exists bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)", project).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: project %s", ErrNotFound, project)
	}
	rows, err := q.QueryContext(ctx, "SELECT m.user,u.name,m.role,m.added FROM project_members m JOIN users u ON u.id=m.user WHERE m.project=? ORDER BY m.added,m.user", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.User, &m.Name, &m.Role, &m.Added); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMember adds an account to a project with role, or changes its role. A changed role ends
// every session of the account; their ids are returned. Demoting the last owner is ErrLastOwner.
func (tx *Tx) SetMember(ctx context.Context, project, user, role string, at time.Time) ([]string, error) {
	if role != "owner" && role != "member" {
		return nil, fmt.Errorf("role %q", role)
	}
	if _, err := members(ctx, tx.tx, project); err != nil {
		return nil, err
	}
	if _, err := account(ctx, tx.tx, "u.id=?", user); err != nil {
		return nil, err
	}
	var current string
	err := tx.tx.QueryRowContext(ctx, "SELECT role FROM project_members WHERE project=? AND user=?", project, user).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		_, err := tx.tx.ExecContext(ctx, "INSERT INTO project_members(project,user,role,added) VALUES(?,?,?,?)", project, user, role, stamp(at))
		return []string{}, err
	}
	if err != nil {
		return nil, err
	}
	if current == role {
		return []string{}, nil
	}
	if current == "owner" {
		if err := requireAnotherOwner(ctx, tx.tx, project); err != nil {
			return nil, err
		}
	}
	if _, err := tx.tx.ExecContext(ctx, "UPDATE project_members SET role=? WHERE project=? AND user=?", role, project, user); err != nil {
		return nil, err
	}
	return endSessions(ctx, tx.tx, user)
}

func requireAnotherOwner(ctx context.Context, q execQuerier, project string) error {
	var owners int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM project_members WHERE project=? AND role='owner'", project).Scan(&owners); err != nil {
		return err
	}
	if owners <= 1 {
		return ErrLastOwner
	}
	return nil
}

// Reasons recorded when a member is removed: on an attempt that waited for its actor's
// decision, and on every other attempt of theirs.
const (
	RevokedReason = "membership revoked"
	RemovedReason = "actor removed from the project"
)

// A Removal says what removing a member touched: the attempts given a cancel event, for their
// open streams to be woken, and the tasks of launched attempts, for the executor to cancel.
type Removal struct {
	Attempts, Running []string
}

// RemoveMembership removes user from project in one transaction: each of their pending attempts
// there that waits for a decision is denied under key, every pending one is withdrawn with its
// task, every launching or running one, and one whose task verifies its checks, gets its cancel
// recorded (and requested, when running) for the executor's cancel path, and the membership is
// deleted. The last owner is ErrLastOwner; a non-member ErrNotFound.
func (tx *Tx) RemoveMembership(ctx context.Context, project, user, key string, at time.Time) (Removal, error) {
	removal := Removal{Attempts: []string{}, Running: []string{}}
	var role string
	err := tx.tx.QueryRowContext(ctx, "SELECT role FROM project_members WHERE project=? AND user=?", project, user).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return removal, fmt.Errorf("%w: member %s of project %s", ErrNotFound, user, project)
	}
	if err != nil {
		return removal, err
	}
	if role == "owner" {
		if err := requireAnotherOwner(ctx, tx.tx, project); err != nil {
			return removal, err
		}
	}
	all, err := openAttempts(ctx, tx.tx, "a.actor=? AND a.task IN (SELECT id FROM tasks WHERE project=?)", user, project)
	if err != nil {
		return removal, err
	}
	for _, a := range all {
		removal.Attempts = append(removal.Attempts, a.id)
		if a.state != "pending" {
			if err := appendCancelEvent(ctx, tx.tx, a.id, RemovedReason); err != nil {
				return removal, err
			}
			if err := requestCancel(ctx, tx.tx, a.task); err != nil {
				return removal, err
			}
			removal.Running = append(removal.Running, a.task)
			continue
		}
		b, err := readBindings(ctx, tx.tx, a.id)
		if err != nil {
			return removal, err
		}
		// An attempt already allowed and only waiting for a slot has no decision left to deny.
		waiting := b.required()
		if waiting {
			if waiting, err = launchable(ctx, tx.tx, a.id, at); err != nil {
				return removal, err
			}
			waiting = !waiting
		}
		reason := RemovedReason
		if waiting {
			if err := insertDecision(ctx, tx.tx, b, "deny", tx.userID, key, at); err != nil {
				return removal, err
			}
			reason = RevokedReason
		}
		if _, err := cancelQueued(ctx, tx.tx, a.task, b.revision, reason); err != nil {
			return removal, err
		}
	}
	_, err = tx.tx.ExecContext(ctx, "DELETE FROM project_members WHERE project=? AND user=?", project, user)
	return removal, err
}

// StopCounts are the attempts a stop would end: running (launching included) and queued, all
// users' and those whose actor is not the owner account.
type StopCounts struct {
	Running, Queued, OthersRunning, OthersQueued int
}

// Others is how many attempts of other users the stop would end.
func (c StopCounts) Others() int { return c.OthersRunning + c.OthersQueued }

// StopCounts reads what a stop of this generation would end, owner being the owner account.
func (s *Store) StopCounts(ctx context.Context, generation int64, owner string) (StopCounts, error) {
	return stopCounts(ctx, s.db, generation, owner)
}

// StopCounts is Store.StopCounts inside the stop's transaction.
func (tx *Tx) StopCounts(ctx context.Context, generation int64, owner string) (StopCounts, error) {
	return stopCounts(ctx, tx.tx, generation, owner)
}

func stopCounts(ctx context.Context, q execQuerier, generation int64, owner string) (StopCounts, error) {
	var c StopCounts
	all, err := openAttempts(ctx, q, "a.ownership_generation=?", generation)
	if err != nil {
		return c, err
	}
	for _, a := range all {
		other := a.actor != owner
		switch {
		case a.state == "pending":
			c.Queued++
			if other {
				c.OthersQueued++
			}
		default:
			c.Running++
			if other {
				c.OthersRunning++
			}
		}
	}
	return c, nil
}

// openAttempt is an attempt a stop or a removal ends: pending, launching, running, or the
// newest attempt of a task verifying its checks.
type openAttempt struct{ id, task, actor, state string }

// openAttempts lists the open attempts matching where (on attempts a), in queue order.
func openAttempts(ctx context.Context, q execQuerier, where string, args ...any) ([]openAttempt, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id,a.task,a.actor,a.state FROM attempts a WHERE (a.state IN ('pending','launching','running')
		OR (a.state='ended_ok' AND EXISTS(SELECT 1 FROM tasks v WHERE v.id=a.task AND v.state='verifying')
			AND a.rowid=(SELECT max(n.rowid) FROM attempts n WHERE n.task=a.task))) AND `+where+" ORDER BY "+queueOrder, args...)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(a *openAttempt) []any { return []any{&a.id, &a.task, &a.actor, &a.state} })
}

// collect scans every row into a T and closes the cursor on every exit; an error the cursor met
// is returned before anything collected is used.
func collect[T any](rows *sql.Rows, fields func(*T) []any) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		var v T
		if err := rows.Scan(fields(&v)...); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, rows.Close()
}
