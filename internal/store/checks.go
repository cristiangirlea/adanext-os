package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	MaxChecksPerTask    = 16
	MaxCheckTimeout     = 30 * time.Minute
	MaxCheckOutputBytes = 16 << 20
)

var ErrInvalidCheck = errors.New("invalid check definition")

type CheckState string

const (
	CheckRunning     CheckState = "running"
	CheckPassed      CheckState = "passed"
	CheckFailed      CheckState = "failed"
	CheckTimedOut    CheckState = "timed_out"
	CheckKilled      CheckState = "killed"
	CheckNotRun      CheckState = "not_run"
	CheckInterrupted CheckState = "interrupted"
	CheckChangedTree CheckState = "changed_tree"
)

type CheckDefinition struct {
	Name           string   `json:"name,omitempty"`
	Argv           []string `json:"argv"`
	Workdir        string   `json:"workdir,omitempty"`
	Timeout        string   `json:"timeout,omitempty"`
	MaxOutputBytes int64    `json:"max_output_bytes,omitempty"`
	Env            []string `json:"env,omitempty"`
}

type CheckBaseline struct {
	ID             string     `json:"id"`
	Project        string     `json:"-"`
	Snapshot       string     `json:"snapshot,omitempty"`
	DefinitionHash string     `json:"-"`
	DefinitionJSON string     `json:"-"`
	State          CheckState `json:"state"`
	ExitCode       *int       `json:"exit_code,omitempty"`
	TestedCommit   string     `json:"tested_commit"`
	BoundCommit    string     `json:"bound_commit,omitempty"`
	OutputArtifact string     `json:"-"`
	OutputSize     int64      `json:"output_size"`
	Truncated      bool       `json:"truncated"`
	EnvKeys        []string   `json:"env_keys"`
	DurationMS     int64      `json:"duration_ms"`
	StartedAt      string     `json:"-"`
	EndedAt        string     `json:"-"`
}

type CheckResult struct {
	ID              string          `json:"id"`
	Task            string          `json:"-"`
	Attempt         string          `json:"-"`
	Ord             int             `json:"ord"`
	Name            string          `json:"name,omitempty"`
	DefinitionHash  string          `json:"-"`
	Definition      CheckDefinition `json:"definition"`
	State           CheckState      `json:"state"`
	ExitCode        *int            `json:"exit_code,omitempty"`
	TestedCommit    string          `json:"tested_commit,omitempty"`
	BoundCommit     string          `json:"bound_commit,omitempty"`
	Invalidated     bool            `json:"invalidated"`
	OutputArtifact  string          `json:"-"`
	OutputSize      int64           `json:"output_size"`
	Truncated       bool            `json:"truncated"`
	BaselineID      string          `json:"-"`
	Baseline        *CheckBaseline  `json:"baseline,omitempty"`
	NewFailure      bool            `json:"new_failure"`
	BaselineFailure bool            `json:"baseline_failure"`
	// EnvKeys names the variables the check's environment held, without values.
	EnvKeys    []string `json:"env_keys"`
	DurationMS int64    `json:"duration_ms"`
	StartedAt  string   `json:"-"`
	EndedAt    string   `json:"-"`
}

type CheckWorktreeLease struct {
	Project   string
	Attempt   string
	RunID     string
	AdminName string
}

// RefusedEnvKey reports whether key is a provider key, token, or Ada variable.
func RefusedEnvKey(key string) bool {
	u := strings.ToUpper(key)
	return strings.HasSuffix(u, "_API_KEY") || strings.HasSuffix(u, "_TOKEN") || strings.HasPrefix(u, "ADANEXT_") || strings.HasPrefix(u, "ADA_")
}

// CredentialProfileEnvKey reports whether key names a CLI credential profile directory.
func CredentialProfileEnvKey(key string) bool {
	return strings.EqualFold(key, "CLAUDE_CONFIG_DIR") || strings.EqualFold(key, "CODEX_HOME")
}

func validEnvKeyName(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		alpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		digit := c >= '0' && c <= '9'
		if i == 0 && !alpha {
			return false
		}
		if !alpha && !digit {
			return false
		}
	}
	return true
}

func validRelativeWorkdir(dir string) (string, error) {
	if dir == "" || dir == "." {
		return ".", nil
	}
	if len(dir) > 512 || strings.ContainsAny(dir, "\x00\\:") || strings.HasPrefix(dir, "/") || strings.HasSuffix(dir, "/") {
		return "", fmt.Errorf("%w: workdir %q", ErrInvalidCheck, dir)
	}
	parts := strings.Split(dir, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.EqualFold(p, ".git") {
			return "", fmt.Errorf("%w: workdir %q", ErrInvalidCheck, dir)
		}
	}
	return dir, nil
}

// NormalizeCheckDefinitions validates and canonicalizes a task's check declarations.
func NormalizeCheckDefinitions(defs []CheckDefinition) ([]CheckDefinition, error) {
	if len(defs) > MaxChecksPerTask {
		return nil, fmt.Errorf("%w: %d checks exceeds maximum %d", ErrInvalidCheck, len(defs), MaxChecksPerTask)
	}
	out := make([]CheckDefinition, 0, len(defs))
	for _, d := range defs {
		name := strings.TrimSpace(d.Name)
		if len(name) > 128 {
			return nil, fmt.Errorf("%w: check name too long", ErrInvalidCheck)
		}
		for i := 0; i < len(name); i++ {
			if name[i] < 0x20 || name[i] == 0x7f {
				return nil, fmt.Errorf("%w: invalid check name", ErrInvalidCheck)
			}
		}
		if len(d.Argv) == 0 || len(d.Argv) > 64 || d.Argv[0] == "" {
			return nil, fmt.Errorf("%w: argv must be non-empty", ErrInvalidCheck)
		}
		argv := make([]string, len(d.Argv))
		for i, a := range d.Argv {
			if len(a) > 4096 || strings.IndexByte(a, 0) >= 0 {
				return nil, fmt.Errorf("%w: invalid argv element", ErrInvalidCheck)
			}
			argv[i] = a
		}
		workdir, err := validRelativeWorkdir(d.Workdir)
		if err != nil {
			return nil, err
		}
		timeout := MaxCheckTimeout
		if d.Timeout != "" {
			dur, err := time.ParseDuration(d.Timeout)
			if err != nil || dur <= 0 || dur > MaxCheckTimeout {
				return nil, fmt.Errorf("%w: timeout %q", ErrInvalidCheck, d.Timeout)
			}
			timeout = dur
		}
		maxOut := int64(MaxCheckOutputBytes)
		if d.MaxOutputBytes < 0 || d.MaxOutputBytes > MaxCheckOutputBytes {
			return nil, fmt.Errorf("%w: max_output_bytes %d", ErrInvalidCheck, d.MaxOutputBytes)
		}
		if d.MaxOutputBytes > 0 {
			maxOut = d.MaxOutputBytes
		}
		var env []string
		seen := map[string]bool{}
		for _, k := range d.Env {
			if !validEnvKeyName(k) || RefusedEnvKey(k) || CredentialProfileEnvKey(k) {
				return nil, fmt.Errorf("%w: env variable %q is not allowed", ErrInvalidCheck, k)
			}
			if !seen[k] {
				seen[k] = true
				env = append(env, k)
			}
		}
		out = append(out, CheckDefinition{
			Name:           name,
			Argv:           argv,
			Workdir:        workdir,
			Timeout:        timeout.String(),
			MaxOutputBytes: maxOut,
			Env:            env,
		})
	}
	return out, nil
}

// CheckDefinitionHash returns the SHA-256 of the normalized check definition's canonical JSON.
func CheckDefinitionHash(d CheckDefinition) string {
	norm, err := NormalizeCheckDefinitions([]CheckDefinition{d})
	if err == nil && len(norm) == 1 {
		d = norm[0]
	}
	// Hash the execution semantics (argv, workdir, timeout, max_output_bytes, env).
	key := struct {
		Argv           []string `json:"argv"`
		Workdir        string   `json:"workdir"`
		Timeout        string   `json:"timeout"`
		MaxOutputBytes int64    `json:"max_output_bytes"`
		Env            []string `json:"env,omitempty"`
	}{
		Argv:           d.Argv,
		Workdir:        d.Workdir,
		Timeout:        d.Timeout,
		MaxOutputBytes: d.MaxOutputBytes,
		Env:            d.Env,
	}
	data, _ := json.Marshal(key)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func encodeChecksJSON(defs []CheckDefinition) (string, error) {
	norm, err := NormalizeCheckDefinitions(defs)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(norm)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func decodeChecksJSON(raw string) ([]CheckDefinition, error) {
	if raw == "" || raw == "[]" {
		return []CheckDefinition{}, nil
	}
	var defs []CheckDefinition
	if err := json.Unmarshal([]byte(raw), &defs); err != nil {
		return nil, err
	}
	if defs == nil {
		defs = []CheckDefinition{}
	}
	return defs, nil
}

// SetTaskChecks replaces a task's check declarations and bumps its revision.
func (tx *Tx) SetTaskChecks(ctx context.Context, taskID string, expectedRevision int64, defs []CheckDefinition) error {
	if err := requireTaskMember(ctx, tx, tx.userID, taskID); err != nil {
		return err
	}
	raw, err := encodeChecksJSON(defs)
	if err != nil {
		return err
	}
	var state State
	var rev int64
	if err := tx.QueryRowContext(ctx, "SELECT state,revision FROM tasks WHERE id=?", taskID).Scan(&state, &rev); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
		}
		return err
	}
	if rev != expectedRevision {
		return fmt.Errorf("%w: task %s, expected %d, current %d", ErrStaleRevision, taskID, expectedRevision, rev)
	}
	// Checks are part of the revision an attempt starts from.
	if state != Draft && state != Ready {
		return fmt.Errorf("%w: cannot edit checks while task is %s", ErrInvalidTransition, state)
	}
	_, err = tx.ExecContext(ctx, "UPDATE tasks SET checks_json=?,revision=revision+1,updated=? WHERE id=? AND revision=?", raw, now(), taskID, expectedRevision)
	return err
}

func (s *Store) TaskChecks(ctx context.Context, taskID string) ([]CheckDefinition, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT checks_json FROM tasks WHERE id=?", taskID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: task %s", ErrNotFound, taskID)
		}
		return nil, err
	}
	return decodeChecksJSON(raw)
}

func (s *Store) BaselineForCheck(ctx context.Context, snapshot, defHash string) (*CheckBaseline, error) {
	var b CheckBaseline
	var code sql.NullInt64
	var trunc int
	var envKeys string
	err := s.db.QueryRowContext(ctx, `SELECT id,project,snapshot,definition_hash,definition_json,tested_commit,bound_commit,state,exit_code,output_artifact,output_size,output_truncated,env_keys,duration_ms,started_at,ended_at
		FROM check_baselines WHERE snapshot=? AND definition_hash=?`, snapshot, defHash).Scan(
		&b.ID, &b.Project, &b.Snapshot, &b.DefinitionHash, &b.DefinitionJSON, &b.TestedCommit, &b.BoundCommit, &b.State, &code, &b.OutputArtifact, &b.OutputSize, &trunc, &envKeys, &b.DurationMS, &b.StartedAt, &b.EndedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if code.Valid {
		v := int(code.Int64)
		b.ExitCode = &v
	}
	b.Truncated = trunc != 0
	b.EnvKeys = decodeEnvKeys(envKeys)
	if b.State != CheckPassed && b.State != CheckFailed && b.State != CheckTimedOut && b.State != CheckChangedTree {
		return nil, nil
	}
	return &b, nil
}

func (tx *Tx) RecordCheckBaseline(ctx context.Context, b CheckBaseline) error {
	if b.StartedAt == "" {
		b.StartedAt = now()
	}
	if b.EndedAt == "" {
		b.EndedAt = now()
	}
	if b.DefinitionJSON == "" {
		b.DefinitionJSON = "{}"
	}
	var code any
	if b.ExitCode != nil {
		code = *b.ExitCode
	}
	trunc := 0
	if b.Truncated {
		trunc = 1
	}
	_, err := tx.tx.ExecContext(ctx, `INSERT INTO check_baselines(id,project,snapshot,definition_hash,definition_json,tested_commit,bound_commit,state,exit_code,output_artifact,output_size,output_truncated,env_keys,duration_ms,started_at,ended_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(snapshot,definition_hash) DO UPDATE SET
			id=excluded.id,tested_commit=excluded.tested_commit,bound_commit=excluded.bound_commit,state=excluded.state,
			exit_code=excluded.exit_code,output_artifact=excluded.output_artifact,output_size=excluded.output_size,
			output_truncated=excluded.output_truncated,env_keys=excluded.env_keys,duration_ms=excluded.duration_ms,
			started_at=excluded.started_at,ended_at=excluded.ended_at`,
		b.ID, b.Project, b.Snapshot, b.DefinitionHash, b.DefinitionJSON, b.TestedCommit, b.BoundCommit, b.State, code, b.OutputArtifact, b.OutputSize, trunc, encodeEnvKeys(b.EnvKeys), b.DurationMS, b.StartedAt, b.EndedAt)
	return err
}

func (tx *Tx) RecordCheckResult(ctx context.Context, r CheckResult) error {
	if r.StartedAt == "" {
		r.StartedAt = now()
	}
	if r.EndedAt == "" && r.State != CheckRunning {
		r.EndedAt = now()
	}
	defBytes, err := json.Marshal(r.Definition)
	if err != nil {
		return err
	}
	if r.DefinitionHash == "" {
		r.DefinitionHash = CheckDefinitionHash(r.Definition)
	}
	var code any
	if r.ExitCode != nil {
		code = *r.ExitCode
	}
	trunc := 0
	if r.Truncated {
		trunc = 1
	}
	_, err = tx.tx.ExecContext(ctx, `INSERT INTO check_results(id,task,attempt,ord,name,definition_hash,definition_json,tested_commit,bound_commit,state,exit_code,output_artifact,output_size,output_truncated,baseline_id,env_keys,duration_ms,started_at,ended_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(attempt,ord) DO UPDATE SET
			id=excluded.id,name=excluded.name,definition_hash=excluded.definition_hash,definition_json=excluded.definition_json,
			tested_commit=excluded.tested_commit,bound_commit=excluded.bound_commit,state=excluded.state,exit_code=excluded.exit_code,
			output_artifact=excluded.output_artifact,output_size=excluded.output_size,output_truncated=excluded.output_truncated,
			baseline_id=excluded.baseline_id,env_keys=excluded.env_keys,duration_ms=excluded.duration_ms,
			started_at=excluded.started_at,ended_at=excluded.ended_at`,
		r.ID, r.Task, r.Attempt, r.Ord, r.Name, r.DefinitionHash, string(defBytes), r.TestedCommit, r.BoundCommit, r.State, code, r.OutputArtifact, r.OutputSize, trunc, r.BaselineID, encodeEnvKeys(r.EnvKeys), r.DurationMS, r.StartedAt, r.EndedAt)
	return err
}

func encodeEnvKeys(keys []string) string {
	if keys == nil {
		keys = []string{}
	}
	data, _ := json.Marshal(keys)
	return string(data)
}

func decodeEnvKeys(raw string) []string {
	keys := []string{}
	_ = json.Unmarshal([]byte(raw), &keys)
	if keys == nil {
		keys = []string{}
	}
	return keys
}

func (tx *Tx) RecordTestedCommit(ctx context.Context, attemptID, commit string) error {
	_, err := tx.tx.ExecContext(ctx, "UPDATE workspaces SET tested_commit=? WHERE attempt=?", commit, attemptID)
	return err
}

// BranchTip reads the current commit of a task's branch ada/<task>; "" when unknown.
type BranchTip func(project, task string) string

// CheckResults reads the newest attempt's results, each invalidated unless it
// binds the commit tip returns for the task's branch.
func (s *Store) CheckResults(ctx context.Context, userID, taskID string, tip BranchTip) ([]CheckResult, error) {
	var project, attemptID string
	err := s.db.QueryRowContext(ctx, `SELECT t.project, COALESCE((SELECT a.id FROM attempts a WHERE a.task=t.id ORDER BY a.rowid DESC LIMIT 1), '')
		FROM tasks t WHERE t.id=?`, taskID).Scan(&project, &attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: task %s", ErrNotFound, taskID)
	}
	if err != nil {
		return nil, err
	}
	if err := requireMember(ctx, s.db, userID, project); err != nil {
		return nil, err
	}
	if attemptID == "" {
		return []CheckResult{}, nil
	}
	results, err := s.attemptCheckResults(ctx, attemptID)
	if err != nil || tip == nil {
		return results, err
	}
	markTip(results, tip(project, taskID))
	return results, nil
}

// checkInvalidated is a result that binds no commit, or not tip, the branch's
// current commit; an unknown tip invalidates every result.
func checkInvalidated(bound, tip string) bool {
	return bound == "" || tip == "" || bound != tip
}

func markTip(results []CheckResult, tip string) {
	for i := range results {
		results[i].Invalidated = checkInvalidated(results[i].BoundCommit, tip)
	}
}

// AtTip marks the attempt's results against tip, the current commit of the
// task's branch, as read through the source package.
func (a *TaskAttempt) AtTip(tip string) {
	if a.Checks != nil {
		markTip(*a.Checks, tip)
	}
}

// attemptCheckResults reads an attempt's results; with no tip known yet, each
// reads invalidated until AtTip or CheckResults marks it.
func (s *Store) attemptCheckResults(ctx context.Context, attemptID string) ([]CheckResult, error) {
	return attemptCheckResultsQ(ctx, s.db, attemptID)
}

func attemptCheckResultsQ(ctx context.Context, q querier, attemptID string) ([]CheckResult, error) {
	rows, err := q.QueryContext(ctx, `SELECT
		r.id,r.task,r.attempt,r.ord,r.name,r.definition_hash,r.definition_json,r.tested_commit,r.bound_commit,r.state,r.exit_code,r.output_artifact,r.output_size,r.output_truncated,r.baseline_id,r.env_keys,r.duration_ms,r.started_at,r.ended_at,
		COALESCE(b.id,''),COALESCE(b.snapshot,''),COALESCE(b.state,''),b.exit_code,COALESCE(b.tested_commit,''),COALESCE(b.bound_commit,''),COALESCE(b.output_size,0),COALESCE(b.output_truncated,0)
		FROM check_results r
		LEFT JOIN check_baselines b ON b.id=r.baseline_id
		WHERE r.attempt=? ORDER BY r.ord ASC`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := []CheckResult{}
	for rows.Next() {
		var r CheckResult
		var defJSON string
		var rCode, bCode sql.NullInt64
		var rTrunc, bTrunc int
		var b CheckBaseline
		var envKeys string
		if err := rows.Scan(
			&r.ID, &r.Task, &r.Attempt, &r.Ord, &r.Name, &r.DefinitionHash, &defJSON, &r.TestedCommit, &r.BoundCommit, &r.State, &rCode, &r.OutputArtifact, &r.OutputSize, &rTrunc, &r.BaselineID, &envKeys, &r.DurationMS, &r.StartedAt, &r.EndedAt,
			&b.ID, &b.Snapshot, &b.State, &bCode, &b.TestedCommit, &b.BoundCommit, &b.OutputSize, &bTrunc,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(defJSON), &r.Definition)
		if rCode.Valid {
			v := int(rCode.Int64)
			r.ExitCode = &v
		}
		r.Truncated = rTrunc != 0
		r.EnvKeys = decodeEnvKeys(envKeys)
		if b.ID != "" {
			if bCode.Valid {
				v := int(bCode.Int64)
				b.ExitCode = &v
			}
			b.Truncated = bTrunc != 0
			bCopy := b
			r.Baseline = &bCopy
		}
		failing := r.State == CheckFailed || r.State == CheckTimedOut || r.State == CheckChangedTree
		if failing {
			if r.Baseline != nil && r.Baseline.State != CheckPassed {
				r.BaselineFailure = true
			} else {
				r.NewFailure = true
			}
		}
		r.Invalidated = checkInvalidated(r.BoundCommit, "")
		results = append(results, r)
	}
	return results, rows.Err()
}

// CheckLog resolves a check or baseline run's log artifact after checking the
// caller's project membership; with taskID, only a run of that task, or a
// baseline one of its results was compared with.
func (s *Store) CheckLog(ctx context.Context, userID, taskID, runID string) (hash string, size int64, truncated bool, err error) {
	var project string
	var trunc int
	err = s.db.QueryRowContext(ctx, `SELECT t.project,r.output_artifact,r.output_size,r.output_truncated
		FROM check_results r JOIN tasks t ON t.id=r.task WHERE r.id=? AND (?='' OR r.task=?)`, runID, taskID, taskID).Scan(&project, &hash, &size, &trunc)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.QueryRowContext(ctx, `SELECT b.project,b.output_artifact,b.output_size,b.output_truncated
			FROM check_baselines b WHERE b.id=? AND (?='' OR EXISTS(SELECT 1 FROM check_results r WHERE r.baseline_id=b.id AND r.task=?))`, runID, taskID, taskID).Scan(&project, &hash, &size, &trunc)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, fmt.Errorf("%w: check run %s", ErrNotFound, runID)
	}
	if err != nil {
		return "", 0, false, err
	}
	if err := requireMember(ctx, s.db, userID, project); err != nil {
		return "", 0, false, err
	}
	return hash, size, trunc != 0, nil
}

func (s *Store) RecordCheckWorktree(ctx context.Context, lease CheckWorktreeLease) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO check_worktrees(project,attempt,run_id,admin_name,created)
		VALUES(?,?,?,?,?) ON CONFLICT(project,attempt,run_id) DO UPDATE SET admin_name=excluded.admin_name`,
		lease.Project, lease.Attempt, lease.RunID, lease.AdminName, now())
	return err
}

func (s *Store) DeleteCheckWorktree(ctx context.Context, project, attempt, runID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM check_worktrees WHERE project=? AND attempt=? AND run_id=?", project, attempt, runID)
	return err
}

func (s *Store) ListCheckWorktrees(ctx context.Context, project string) ([]CheckWorktreeLease, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT project,attempt,run_id,admin_name FROM check_worktrees WHERE (?='' OR project=?) ORDER BY created ASC", project, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckWorktreeLease
	for rows.Next() {
		var l CheckWorktreeLease
		if err := rows.Scan(&l.Project, &l.Attempt, &l.RunID, &l.AdminName); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func newCheckID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "chk_" + hex.EncodeToString(b[:])
}

func recoverVerifyingTasks(ctx context.Context, tx *sql.Tx, generation int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.id, t.revision, t.checks_json,
		COALESCE((SELECT a.id FROM attempts a WHERE a.task=t.id ORDER BY a.rowid DESC LIMIT 1), ''),
		COALESCE((SELECT a.ownership_generation FROM attempts a WHERE a.task=t.id ORDER BY a.rowid DESC LIMIT 1), -1),
		COALESCE((SELECT w.tested_commit FROM workspaces w JOIN attempts a ON a.id=w.attempt WHERE a.task=t.id ORDER BY a.rowid DESC LIMIT 1), '')
		FROM tasks t WHERE t.state='verifying'`)
	if err != nil {
		return nil, err
	}
	type vTask struct {
		id, checksJSON, attempt, testedCommit string
		rev, gen                              int64
	}
	var tasks []vTask
	for rows.Next() {
		var v vTask
		if err := rows.Scan(&v.id, &v.rev, &v.checksJSON, &v.attempt, &v.gen, &v.testedCommit); err != nil {
			rows.Close()
			return nil, err
		}
		if v.attempt != "" && v.gen < generation {
			tasks = append(tasks, v)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var recovered []string
	for _, v := range tasks {
		defs, err := decodeChecksJSON(v.checksJSON)
		if err != nil {
			return nil, err
		}
		for ord, def := range defs {
			var existingID, existingState string
			err := tx.QueryRowContext(ctx, "SELECT id,state FROM check_results WHERE attempt=? AND ord=?", v.attempt, ord).Scan(&existingID, &existingState)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if errors.Is(err, sql.ErrNoRows) {
				defBytes, _ := json.Marshal(def)
				if _, err := tx.ExecContext(ctx, `INSERT INTO check_results(id,task,attempt,ord,name,definition_hash,definition_json,tested_commit,bound_commit,state,started_at,ended_at)
					VALUES(?,?,?,?,?,?,?,?,'','not_run',?,?)`,
					newCheckID(), v.id, v.attempt, ord, def.Name, CheckDefinitionHash(def), string(defBytes), v.testedCommit, now(), now()); err != nil {
					return nil, err
				}
			} else if existingState == string(CheckRunning) {
				if _, err := tx.ExecContext(ctx, "UPDATE check_results SET state='interrupted',bound_commit='',ended_at=? WHERE id=?", now(), existingID); err != nil {
					return nil, err
				}
			}
		}
		if err := transition(ctx, tx, v.id, Verifying, AwaitingReview, v.rev); err != nil {
			return nil, err
		}
		recovered = append(recovered, v.attempt)
	}
	return recovered, nil
}

// RecordCloneConfig keeps the SHA-256 of the clone config Ada wrote for project.
func (tx *Tx) RecordCloneConfig(ctx context.Context, project, sha string) error {
	_, err := tx.tx.ExecContext(ctx, `INSERT INTO clone_configs(project,sha256,updated) VALUES(?,?,?)
		ON CONFLICT(project) DO UPDATE SET sha256=excluded.sha256,updated=excluded.updated`, project, sha, now())
	return err
}

// CloneConfig reads the SHA-256 of the clone config Ada last wrote for project; "" when none.
func (s *Store) CloneConfig(ctx context.Context, project string) (string, error) {
	var sha string
	err := s.db.QueryRowContext(ctx, "SELECT sha256 FROM clone_configs WHERE project=?", project).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return sha, err
}
