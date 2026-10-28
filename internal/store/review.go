package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrNotAuthorized is a member who is neither the attempt's actor nor a project owner (#3).
	ErrNotAuthorized = errors.New("only the attempt's actor or a project owner may decide")
	// ErrStaleReview is an accept whose commit or generation is no longer the current one.
	ErrStaleReview = errors.New("the reviewed commit or ownership generation is not the current one")
	// ErrAttemptActive is an attempt of the task that has not ended.
	ErrAttemptActive = errors.New("an attempt of the task has not ended")
	// ErrAttemptRecovery is an attempt of the task that is recovery_required.
	ErrAttemptRecovery = errors.New("an attempt of the task is recovery_required")
	// ErrWorktreeNotRemoved is an earlier attempt's worktree a refine has not removed yet.
	ErrWorktreeNotRemoved = errors.New("an earlier attempt's worktree is still present")
	// ErrInvalidFinding is an empty or oversized finding.
	ErrInvalidFinding = errors.New("invalid finding")
	// ErrNoAttempt is a task with no attempt to review.
	ErrNoAttempt = errors.New("the task has no attempt")
)

// MaxFindingBytes bounds one finding; MaxFindingsPerTask bounds a task's findings.
const (
	MaxFindingBytes    = 16 << 10
	MaxFindingsPerTask = 200
)

// ProviderUnacknowledgedError is a refine of an attempt whose provider changes the requester has
// not acknowledged; they must be shown before the refine starts.
type ProviderUnacknowledgedError struct{ Changes ProviderChanges }

func (e *ProviderUnacknowledgedError) Error() string {
	return "the attempt changed provider configuration; acknowledge it to refine"
}

// Finding is a member's note on an attempt, carried into the prompt of the refine that follows it.
type Finding struct {
	ID      string `json:"id"`
	Task    string `json:"-"`
	Attempt string `json:"attempt"`
	Author  string `json:"author"`
	Body    string `json:"body"`
	Created string `json:"created"`
}

// Review is an acceptance: the commit, check results and ownership generation it binds.
type Review struct {
	ID         string        `json:"id"`
	Task       string        `json:"-"`
	Attempt    string        `json:"attempt"`
	Reviewer   string        `json:"reviewer"`
	Decision   string        `json:"decision"`
	Commit     string        `json:"commit"`
	Generation int64         `json:"generation"`
	Checks     []CheckResult `json:"checks"`
	Created    string        `json:"created"`
}

// ReviewAttempt is one attempt of a task as review and refine see it.
type ReviewAttempt struct {
	ID, Actor, State             string
	Workspace, Removed           bool
	TestedCommit, ImportedCommit string
	ProviderChanges              *ProviderChanges
}

// Ended reports an attempt whose engine and checks no longer run.
func (a ReviewAttempt) Ended() bool {
	return a.State == "ended_ok" || a.State == "ended_failed" || a.State == "canceled"
}

// ProviderFlagged is TaskAttempt's rule: unassessed with a worktree, or changes recorded.
func (a ReviewAttempt) ProviderFlagged() bool {
	return TaskAttempt{State: a.State, Workspace: a.Workspace, ProviderChanges: a.ProviderChanges}.ProviderFlagged()
}

// ReviewState is a task read for a review action: its attempts oldest first, and whether the
// reader owns the project.
type ReviewState struct {
	Project, Task string
	State         State
	Revision      int64
	Owner         bool
	Generation    int64 // The coordinator's ownership generation.
	Attempts      []ReviewAttempt
}

// Newest is the attempt the task shows; nil when it has none.
func (r ReviewState) Newest() *ReviewAttempt {
	if len(r.Attempts) == 0 {
		return nil
	}
	return &r.Attempts[len(r.Attempts)-1]
}

// MayDecide reports whether user may accept or refine: the newest attempt's actor or an owner (#3).
func (r ReviewState) MayDecide(user string) bool {
	n := r.Newest()
	return r.Owner || (n != nil && n.Actor == user)
}

// Changed is the newest attempt with an agent-changes commit, the one Changes and Files show.
func (r ReviewState) Changed() (ReviewAttempt, bool) {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if a := r.Attempts[i]; a.Workspace && a.TestedCommit != "" {
			return a, true
		}
	}
	return ReviewAttempt{}, false
}

// LastWorkspace is the newest attempt with a worktree: its provider changes are shown before a refine.
func (r ReviewState) LastWorkspace() (ReviewAttempt, bool) {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if r.Attempts[i].Workspace {
			return r.Attempts[i], true
		}
	}
	return ReviewAttempt{}, false
}

// ReviewState reads a task for a current member.
func (s *Store) ReviewState(ctx context.Context, userID, taskID string) (ReviewState, error) {
	return readReviewState(ctx, s.db, userID, taskID)
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readReviewState(ctx context.Context, q querier, userID, taskID string) (ReviewState, error) {
	r := ReviewState{Task: taskID}
	var role sql.NullString
	err := q.QueryRowContext(ctx, `SELECT t.project,t.state,t.revision,(SELECT role FROM project_members m WHERE m.project=t.project AND m.user=?),
		(SELECT generation FROM coordinator WHERE id=1) FROM tasks t WHERE t.id=?`, userID, taskID).Scan(&r.Project, &r.State, &r.Revision, &role, &r.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewState{}, fmt.Errorf("%w: task %s", ErrNotFound, taskID)
	}
	if err != nil {
		return ReviewState{}, err
	}
	if !role.Valid {
		return ReviewState{}, &NotMemberError{UserID: userID, ProjectID: r.Project}
	}
	r.Owner = role.String == "owner"
	rows, err := q.QueryContext(ctx, `SELECT a.id,a.actor,a.state,a.provider_changes,w.attempt IS NOT NULL,w.removed IS NOT NULL,COALESCE(w.tested_commit,''),COALESCE(w.imported_commit,'')
		FROM attempts a LEFT JOIN workspaces w ON w.attempt=a.id WHERE a.task=? ORDER BY a.rowid`, taskID)
	if err != nil {
		return ReviewState{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var a ReviewAttempt
		var changes sql.NullString
		if err := rows.Scan(&a.ID, &a.Actor, &a.State, &changes, &a.Workspace, &a.Removed, &a.TestedCommit, &a.ImportedCommit); err != nil {
			return ReviewState{}, err
		}
		if a.ProviderChanges, err = decodeJSON[ProviderChanges](changes); err != nil {
			return ReviewState{}, err
		}
		r.Attempts = append(r.Attempts, a)
	}
	return r, rows.Err()
}

func newReviewID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// RecordFinding records a current member's finding on the attempt the task shows.
func (tx *Tx) RecordFinding(ctx context.Context, taskID, body string) (Finding, error) {
	r, err := readReviewState(ctx, tx.tx, tx.userID, taskID)
	if err != nil {
		return Finding{}, err
	}
	if body == "" || len(body) > MaxFindingBytes || strings.ContainsRune(body, 0) {
		return Finding{}, ErrInvalidFinding
	}
	n := r.Newest()
	if n == nil {
		return Finding{}, ErrNoAttempt
	}
	var count int
	if err := tx.tx.QueryRowContext(ctx, "SELECT count(*) FROM findings WHERE task=?", taskID).Scan(&count); err != nil {
		return Finding{}, err
	}
	if count >= MaxFindingsPerTask {
		return Finding{}, ErrInvalidFinding
	}
	f := Finding{ID: newReviewID("fnd_"), Task: taskID, Attempt: n.ID, Author: tx.userID, Body: body, Created: now()}
	_, err = tx.tx.ExecContext(ctx, "INSERT INTO findings(id,task,attempt,author,body,created) VALUES(?,?,?,?,?,?)", f.ID, f.Task, f.Attempt, f.Author, f.Body, f.Created)
	return f, err
}

// Findings lists a task's findings, oldest first, for a current member.
func (s *Store) Findings(ctx context.Context, userID, taskID string) ([]Finding, error) {
	if _, err := s.ReviewState(ctx, userID, taskID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,task,attempt,author,body,created FROM findings WHERE task=? ORDER BY rowid", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Finding{}
	for rows.Next() {
		var f Finding
		if err := rows.Scan(&f.ID, &f.Task, &f.Attempt, &f.Author, &f.Body, &f.Created); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Accept records the actor's or an owner's acceptance of the newest attempt at revision: commit
// must be its agent-changes commit and tip, the branch's current commit; generation the
// coordinator's. The check results recorded are those of that attempt, marked against tip.
func (tx *Tx) Accept(ctx context.Context, taskID string, revision int64, commit string, generation int64, tip string) (Review, error) {
	r, err := readReviewState(ctx, tx.tx, tx.userID, taskID)
	if err != nil {
		return Review{}, err
	}
	if !r.MayDecide(tx.userID) {
		return Review{}, ErrNotAuthorized
	}
	if r.Revision != revision {
		return Review{}, fmt.Errorf("%w: task %s, expected %d, current %d", ErrStaleRevision, taskID, revision, r.Revision)
	}
	if r.State != AwaitingReview {
		return Review{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, r.State, Accepted)
	}
	n := r.Newest()
	if n == nil || n.TestedCommit == "" || commit != n.TestedCommit || tip != commit || generation != r.Generation {
		return Review{}, ErrStaleReview
	}
	checks, err := attemptCheckResultsQ(ctx, tx.tx, n.ID)
	if err != nil {
		return Review{}, err
	}
	markTip(checks, tip)
	data, err := json.Marshal(checks)
	if err != nil {
		return Review{}, err
	}
	review := Review{ID: newReviewID("rev_"), Task: taskID, Attempt: n.ID, Reviewer: tx.userID, Decision: "accepted", Commit: commit, Generation: generation, Checks: checks, Created: now()}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO reviews(id,task,attempt,reviewer,decision,reviewed_commit,ownership_generation,checks_json,created) VALUES(?,?,?,?,?,?,?,?,?)`,
		review.ID, review.Task, review.Attempt, review.Reviewer, review.Decision, review.Commit, review.Generation, string(data), review.Created); err != nil {
		return Review{}, err
	}
	return review, transition(ctx, tx.tx, taskID, AwaitingReview, Accepted, revision)
}

// Refine records a as a new attempt of the task, run by the requester, continuing the newest
// attempt, and moves the task to preparing at revision. It is refused for an accepted task, while
// any attempt has not ended or is recovery_required, while an earlier worktree is still present,
// and, until acknowledged, when the last attempt with a worktree changed provider configuration.
func (tx *Tx) Refine(ctx context.Context, taskID string, revision int64, a Attempt, acknowledged bool) error {
	r, err := readReviewState(ctx, tx.tx, tx.userID, taskID)
	if err != nil {
		return err
	}
	if err := r.CheckRefine(tx.userID, revision, acknowledged, true); err != nil {
		return err
	}
	var refines any
	if n := r.Newest(); n != nil {
		refines = n.ID
	}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO attempts(id,task,actor,backend,model,effort,mode,executor,ownership_generation,engine_sha256,state,started,max_turns,timeout,refines)
		VALUES(?,?,?,?,?,?,?,?,?,?,'pending',?,?,?,?)`, a.ID, taskID, tx.userID, a.Backend, a.Model, a.Effort, a.Mode, a.Executor, a.OwnershipGeneration, a.EngineSHA256, now(), a.MaxTurns, a.Timeout, refines); err != nil {
		return err
	}
	return transition(ctx, tx.tx, taskID, r.State, Preparing, revision)
}

// CheckRefine is Refine's refusal for user at revision, without writing; with removed, an
// earlier worktree still present refuses it too.
func (r ReviewState) CheckRefine(user string, revision int64, acknowledged, removed bool) error {
	if !r.MayDecide(user) {
		return ErrNotAuthorized
	}
	if r.Revision != revision {
		return fmt.Errorf("%w: task %s, expected %d, current %d", ErrStaleRevision, r.Task, revision, r.Revision)
	}
	if r.State != AwaitingReview && r.State != Failed && r.State != Canceled {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, r.State, Preparing)
	}
	for _, at := range r.Attempts {
		if at.State == "recovery_required" {
			return ErrAttemptRecovery
		}
	}
	for _, at := range r.Attempts {
		if !at.Ended() {
			return ErrAttemptActive
		}
		if removed && at.Workspace && !at.Removed {
			return ErrWorktreeNotRemoved
		}
	}
	if last, ok := r.LastWorkspace(); ok && last.ProviderFlagged() && !acknowledged {
		changes := ProviderChanges{Changes: []ProviderChange{}, Truncated: true}
		if last.ProviderChanges != nil {
			changes = *last.ProviderChanges
		}
		return &ProviderUnacknowledgedError{Changes: changes}
	}
	return nil
}

// RecordAgentCommit records the commit Ada made of an ended attempt's remaining changes; a
// commit already recorded is kept.
func (s *Store) RecordAgentCommit(ctx context.Context, attemptID, commit string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE workspaces SET tested_commit=? WHERE attempt=? AND tested_commit=''", commit, attemptID)
	return err
}

// MarkWorktreeRemoved records that an attempt's worktree and its admin directory are gone.
func (s *Store) MarkWorktreeRemoved(ctx context.Context, attemptID string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE workspaces SET removed=COALESCE(removed,?) WHERE attempt=?", now(), attemptID)
	return err
}

// RecoverEndedAttempt moves an ended attempt whose worktree no longer matches what Ada recorded to
// recovery_required, with a recovery event naming the reason and the mismatch.
func (s *Store) RecoverEndedAttempt(ctx context.Context, attemptID, reason, mismatch string) error {
	payload, err := json.Marshal(map[string]string{"reason": reason, "mismatch": mismatch})
	if err != nil {
		return err
	}
	return s.write(ctx, "", func(tx *Tx) error {
		result, err := tx.tx.ExecContext(ctx, "UPDATE attempts SET state='recovery_required' WHERE id=? AND state IN ('ended_ok','ended_failed','canceled')", attemptID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return errors.Join(err, ErrAttemptActive)
		}
		var seq int64
		if err := tx.tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE attempt=?", attemptID).Scan(&seq); err != nil {
			return err
		}
		return appendEvent(ctx, tx.tx, Event{Attempt: attemptID, Seq: seq, Time: now(), Kind: "recovery", Payload: payload})
	})
}

// Continuation is where a refined attempt starts: the newest earlier attempt with a worktree,
// its agent-changes commit, the branch's tip, and the task's imported-changes commit.
type Continuation struct {
	From, Tip, ImportedCommit string
}

// Continuation reads the earlier work a pending attempt continues; ok is false for a first attempt.
func (s *Store) Continuation(ctx context.Context, attemptID string) (Continuation, bool, error) {
	var c Continuation
	err := s.db.QueryRowContext(ctx, `SELECT w.attempt,w.tested_commit,w.imported_commit FROM attempts n JOIN attempts a ON a.task=n.task AND a.rowid<n.rowid
		JOIN workspaces w ON w.attempt=a.id WHERE n.id=? ORDER BY a.rowid DESC LIMIT 1`, attemptID).Scan(&c.From, &c.Tip, &c.ImportedCommit)
	if errors.Is(err, sql.ErrNoRows) {
		return Continuation{}, false, nil
	}
	return c, err == nil, err
}

// refineFindings are the findings a refined attempt carries: those on the attempts since the
// newest earlier one with a worktree, that one included.
func refineFindings(ctx context.Context, q querier, attemptID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT f.body FROM attempts n JOIN findings f ON f.task=n.task JOIN attempts fa ON fa.id=f.attempt
		WHERE n.id=? AND n.refines IS NOT NULL AND fa.rowid<n.rowid AND fa.rowid>=COALESCE((SELECT max(a.rowid) FROM attempts a JOIN workspaces w ON w.attempt=a.id
		WHERE a.task=n.task AND a.rowid<n.rowid),0) ORDER BY f.rowid`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		out = append(out, body)
	}
	return out, rows.Err()
}

// refinePrompt appends the findings to a task's prompt, one indented item each.
func refinePrompt(prompt string, findings []string) string {
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\nThis continues earlier work on the task, already committed on the branch you are on. Address these review findings:\n")
	if len(findings) == 0 {
		b.WriteString("- none were recorded; review the work against the acceptance criteria\n")
	}
	for _, f := range findings {
		b.WriteString("- ")
		b.WriteString(strings.ReplaceAll(strings.TrimRight(f, "\n"), "\n", "\n  "))
		b.WriteString("\n")
	}
	return b.String()
}
