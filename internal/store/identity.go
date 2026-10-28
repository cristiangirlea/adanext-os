package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrNotMember = errors.New("user is not a project member")

// NotMemberError identifies the denied project and supports errors.Is.
type NotMemberError struct{ UserID, ProjectID string }

func (e *NotMemberError) Error() string {
	return fmt.Sprintf("%s: user %s, project %s", ErrNotMember, e.UserID, e.ProjectID)
}
func (e *NotMemberError) Unwrap() error { return ErrNotMember }

// Identity arguments come from the coordinator's authenticated context, never
// request bodies. Membership management is a trusted coordinator operation;
// the executor and review slices decide role-specific authorization.
func (s *Store) CreateUser(ctx context.Context, id, name string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO users(id,name,created) VALUES(?,?,?)", id, name, now())
	return err
}

func (s *Store) AddMember(ctx context.Context, project, user, role string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO project_members(project,user,role,added) VALUES(?,?,?,?)", project, user, role, now())
	return err
}

func (s *Store) RemoveMember(ctx context.Context, project, user string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM project_members WHERE project=? AND user=?", project, user)
	return err
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func requireMember(ctx context.Context, q rowQuerier, user, project string) error {
	var exists int
	if err := q.QueryRowContext(ctx, "SELECT 1 FROM project_members WHERE project=? AND user=?", project, user).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &NotMemberError{UserID: user, ProjectID: project}
		}
		return err
	}
	return nil
}

func requireTaskMember(ctx context.Context, q rowQuerier, user, task string) error {
	var project string
	if err := q.QueryRowContext(ctx, "SELECT project FROM tasks WHERE id=?", task).Scan(&project); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, task)
		}
		return err
	}
	return requireMember(ctx, q, user, project)
}

func requireAttemptMember(ctx context.Context, q rowQuerier, user, attempt string) error {
	var task string
	if err := q.QueryRowContext(ctx, "SELECT task FROM attempts WHERE id=?", attempt).Scan(&task); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: attempt %s", ErrNotFound, attempt)
		}
		return err
	}
	return requireTaskMember(ctx, q, user, task)
}

type Task struct {
	ID, Project, CreatedBy, Objective, AcceptanceCriteria string
	State                                                 State
	Revision                                              int64
	Created, Updated                                      string
	Snapshot                                              string // empty when the task is bound to none
	// ProviderConfiguration is what the task's snapshot holds; nil with no snapshot or one older than 0008.
	ProviderConfiguration *ProviderConfiguration
	Checks                *[]CheckDefinition
	Attempt               TaskAttempt
}

func (t Task) CheckDefinitions() []CheckDefinition {
	if t.Checks == nil || *t.Checks == nil {
		return []CheckDefinition{}
	}
	return *t.Checks
}

// TaskAttempt is the newest attempt, even after it ends. Task reads join it
// and its global queue position in the same SQLite snapshot as the task.
type TaskAttempt struct {
	ID, State     string
	QueuePosition int
	// ProviderChanges is nil until assessed, and for an attempt with no workspace.
	ProviderChanges *ProviderChanges
	Workspace       bool // whether the attempt has a worktree
	TestedCommit    string
	Checks          *[]CheckResult
	ChecksFailed    bool
}

func (a TaskAttempt) CheckResults() []CheckResult {
	if a.Checks == nil || *a.Checks == nil {
		return []CheckResult{}
	}
	return *a.Checks
}

// ProviderFlagged reports whether the attempt's provider changes must be shown before review or
// refine. An attempt that ended with a worktree but no assessment (a crash, a lost ownership) is
// flagged: nothing vouches for what it left.
func (a TaskAttempt) ProviderFlagged() bool {
	if a.ProviderChanges != nil {
		return a.ProviderChanges.Flagged()
	}
	return a.Workspace && a.State != "pending" && a.State != "launching" && a.State != "running"
}

const taskReadColumns = `t.id,t.project,t.created_by,t.objective,t.acceptance_criteria,t.state,t.revision,t.created,t.updated,COALESCE(t.snapshot,''),t.checks_json,
	(SELECT provider_configuration FROM source_snapshots WHERE id=t.snapshot),
	a.id,a.state,CASE WHEN a.state='pending' THEN (SELECT count(*)+1 FROM attempts p WHERE p.state='pending' AND p.rowid<a.rowid) ELSE 0 END,a.provider_changes,
	EXISTS(SELECT 1 FROM workspaces w WHERE w.attempt=a.id),
	COALESCE((SELECT w.tested_commit FROM workspaces w WHERE w.attempt=a.id),'')`
const taskReadFrom = ` FROM tasks t LEFT JOIN attempts a ON a.rowid=(SELECT max(latest.rowid) FROM attempts latest WHERE latest.task=t.id)`

func scanTaskRead(row interface{ Scan(...any) error }, extra ...any) (Task, error) {
	var task Task
	var checksJSON, testedCommit string
	var id, state, provider, changes sql.NullString
	var position int
	var workspace bool
	args := []any{&task.ID, &task.Project, &task.CreatedBy, &task.Objective, &task.AcceptanceCriteria, &task.State, &task.Revision, &task.Created, &task.Updated, &task.Snapshot, &checksJSON, &provider, &id, &state, &position, &changes, &workspace, &testedCommit}
	if err := row.Scan(append(args, extra...)...); err != nil {
		return task, err
	}
	var err error
	defs, err := decodeChecksJSON(checksJSON)
	if err != nil {
		return task, err
	}
	if len(defs) > 0 {
		task.Checks = &defs
	}
	if task.ProviderConfiguration, err = decodeJSON[ProviderConfiguration](provider); err != nil {
		return task, err
	}
	if id.Valid {
		task.Attempt = TaskAttempt{ID: id.String, State: state.String, QueuePosition: position, Workspace: workspace, TestedCommit: testedCommit}
		if task.Attempt.ProviderChanges, err = decodeJSON[ProviderChanges](changes); err != nil {
			return task, err
		}
	}
	return task, nil
}

const taskColumns = "id,project,created_by,objective,acceptance_criteria,state,revision,created,updated"

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var task Task
	err := row.Scan(&task.ID, &task.Project, &task.CreatedBy, &task.Objective, &task.AcceptanceCriteria, &task.State, &task.Revision, &task.Created, &task.Updated)
	return task, err
}

func (s *Store) ListTasks(ctx context.Context, userID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+taskReadColumns+taskReadFrom+" WHERE t.project IN (SELECT project FROM project_members WHERE user=?) ORDER BY t.id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []Task{}
	for rows.Next() {
		task, err := scanTaskRead(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range tasks {
		if err := s.populateAttemptChecks(ctx, &tasks[i]); err != nil {
			return nil, err
		}
	}
	return tasks, nil
}

func (s *Store) Task(ctx context.Context, userID, taskID string) (Task, error) {
	// A single query includes the access check, avoiding a write lock for reads.
	var member bool
	task, err := scanTaskRead(s.db.QueryRowContext(ctx, "SELECT "+taskReadColumns+",EXISTS(SELECT 1 FROM project_members WHERE project=t.project AND user=?)"+taskReadFrom+" WHERE t.id=?", userID, taskID), &member)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	if !member {
		return Task{}, &NotMemberError{UserID: userID, ProjectID: task.Project}
	}
	if err := s.populateAttemptChecks(ctx, &task); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (s *Store) populateAttemptChecks(ctx context.Context, task *Task) error {
	if task.Attempt.ID == "" {
		return nil
	}
	results, err := s.attemptCheckResults(ctx, task.Attempt.ID)
	if err != nil {
		return err
	}
	if len(results) > 0 {
		task.Attempt.Checks = &results
	}
	for _, r := range results {
		if r.State != CheckPassed {
			task.Attempt.ChecksFailed = true
			break
		}
	}
	return nil
}

// TaskProject and AttemptProject reveal a scope only to a current member.
// Unknown and inaccessible identifiers have the same result.
func (s *Store) TaskProject(ctx context.Context, userID, taskID string) (string, error) {
	var project string
	err := s.db.QueryRowContext(ctx, `SELECT t.project FROM tasks t JOIN project_members m ON m.project=t.project AND m.user=? WHERE t.id=?`, userID, taskID).Scan(&project)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return project, err
}

func (s *Store) AttemptProject(ctx context.Context, userID, attemptID string) (string, error) {
	var project string
	err := s.db.QueryRowContext(ctx, `SELECT t.project FROM attempts a JOIN tasks t ON t.id=a.task JOIN project_members m ON m.project=t.project AND m.user=? WHERE a.id=?`, userID, attemptID).Scan(&project)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return project, err
}

func (s *Store) CreateTask(ctx context.Context, userID, id, project, objective, criteria string) error {
	return s.write(ctx, userID, func(tx *Tx) error {
		if err := requireMember(ctx, tx, userID, project); err != nil {
			return err
		}
		time := now()
		_, err := tx.ExecContext(ctx, "INSERT INTO tasks(id,project,created_by,objective,acceptance_criteria,state,created,updated) VALUES(?,?,?,?,?,'draft',?,?)", id, project, userID, objective, criteria, time, time)
		return err
	})
}

type Attempt struct {
	ID, Task, Actor, Backend, Model, Effort, Mode, Executor string
	OwnershipGeneration                                     int64
	EngineSHA256, RunID, NativeSession, State               string
	Started, Ended                                          string
	MaxTurns                                                int    // Recorded by a refine; a start writes its own.
	Timeout                                                 string // Recorded by a refine; a start writes its own.
}

// CreateAttempt records intent with the authenticated actor. Continuations must
// retain their previous attempt's actor; lineage arrives with the executor.
func (s *Store) CreateAttempt(ctx context.Context, attempt Attempt) error {
	return s.write(ctx, attempt.Actor, func(tx *Tx) error {
		if err := requireTaskMember(ctx, tx, attempt.Actor, attempt.Task); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO attempts(id,task,actor,backend,model,effort,mode,executor,ownership_generation,engine_sha256,state,started) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", attempt.ID, attempt.Task, attempt.Actor, attempt.Backend, attempt.Model, attempt.Effort, attempt.Mode, attempt.Executor, attempt.OwnershipGeneration, attempt.EngineSHA256, attempt.State, now())
		return err
	})
}
