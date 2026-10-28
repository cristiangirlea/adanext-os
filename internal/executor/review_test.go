package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/api"
	"github.com/cristiangirlea/ada-os/internal/store"
)

// reviewAPI serves the API over the executor's store, workspaces and runner; X-Test-User names
// the caller. v is a second owner of project p, w a member who is no owner.
func reviewAPI(t *testing.T, e *Executor, s *store.Store) func(user, method, path, key, match string, body any) (int, []byte) {
	t.Helper()
	ctx := context.Background()
	for _, u := range []struct{ id, role string }{{"v", "owner"}, {"w", "member"}} {
		if err := s.CreateUser(ctx, u.id, u.id); err != nil {
			t.Fatal(err)
		}
		if err := s.AddMember(ctx, "p", u.id, u.role); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(api.New(s, api.NewHub(), func(r *http.Request) (string, error) { return r.Header.Get("X-Test-User"), nil },
		api.Execution{EngineSHA256: e.cfg.EngineSHA256, Generation: e.cfg.Generation, Runner: e, Branches: e.cfg.Workspaces, Review: e.cfg.Workspaces}))
	t.Cleanup(server.Close)
	return func(user, method, path, key, match string, body any) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			data, _ := json.Marshal(body)
			reader = bytes.NewReader(data)
		}
		req, err := http.NewRequest(method, server.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Test-User", user)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		if match != "" {
			req.Header.Set("If-Match", match)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, data
	}
}

// settled waits until the executor has let go of the task's attempt.
func settled(t *testing.T, e *Executor) {
	t.Helper()
	for deadline := time.Now().Add(preparationBound); e.TaskActive("task"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the attempt never left the executor")
		}
	}
}

func revisionOf(t *testing.T, s *store.Store) string {
	t.Helper()
	task, err := s.Task(context.Background(), "u", "task")
	if err != nil {
		t.Fatal(err)
	}
	return strconv.FormatInt(task.Revision, 10)
}

type changesBody struct {
	Base, Commit, Tip string
	Generation        int64
	ProviderFlagged   bool `json:"provider_flagged"`
	ProviderFiles     bool `json:"provider_files"`
	Files             []struct {
		Path, Status, Patch string
		Provider            bool
	}
}

func TestRefineStartsASecondAttemptOnTheRequestersProfileThatSeesTheFailedFirstAttemptsWork(t *testing.T) {
	e, s, marker, _, worktree := workspaceExecutor(t, "refine_fail")
	e.cfg.CancelGrace = naturalExitGrace
	call := reviewAPI(t, e, s)
	e.LaunchAttempt("attempt")
	waitTaskState(t, s, store.Failed)
	settled(t, e)
	if status, data := call("w", "POST", "/v1/tasks/task/findings", "finding", "", map[string]string{"body": "keep first.txt and add second.txt"}); status != 201 {
		t.Fatalf("finding = %d %s", status, data)
	}
	// A member who is neither the actor nor an owner may not refine; v, an owner, refines u's attempt.
	if status, data := call("w", "POST", "/v1/tasks/task/refine", "refine-w", revisionOf(t, s), map[string]string{"backend": "codex", "mode": "auto"}); status != 403 {
		t.Fatalf("refine by a reviewer = %d %s", status, data)
	}
	status, data := call("v", "POST", "/v1/tasks/task/refine", "refine", revisionOf(t, s), map[string]string{"backend": "codex", "mode": "auto"})
	if status != 200 {
		t.Fatalf("refine = %d %s", status, data)
	}
	var started struct {
		AttemptID string `json:"attempt_id"`
	}
	if err := json.Unmarshal(data, &started); err != nil || started.AttemptID == "" {
		t.Fatalf("refine = %s", data)
	}
	// The first attempt's worktree and admin directory are gone; its work is on the branch.
	for _, p := range []string{worktree, filepath.Join(e.cfg.Workspaces.Root, "p", "repo.git", "worktrees", "attempt")} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s remains: %v", p, err)
		}
	}
	task := waitTaskState(t, s, store.AwaitingReview)
	second, err := s.AttemptByID(context.Background(), started.AttemptID)
	if err != nil || second.Actor != "v" || second.State != "ended_ok" || task.Attempt.ID != second.ID {
		t.Fatalf("second attempt %+v %v, task attempt %s", second, err, task.Attempt.ID)
	}
	lines := strings.Join(markerLines(t, marker), "\n")
	if !strings.Contains(lines, "saw:from the first attempt") || !strings.Contains(lines, "keep first.txt and add second.txt") {
		t.Fatalf("the second attempt did not see the first one's work and findings:\n%s", lines)
	}
	settled(t, e)

	status, data = call("w", "GET", "/v1/tasks/task/changes", "", "", nil)
	var changes changesBody
	if status != 200 || json.Unmarshal(data, &changes) != nil {
		t.Fatalf("changes = %d %s", status, data)
	}
	paths := map[string]string{}
	for _, f := range changes.Files {
		paths[f.Path] = f.Status
	}
	if len(paths) != 2 || paths["first.txt"] != "A" || paths["second.txt"] != "A" || changes.Commit != changes.Tip || changes.Commit == "" {
		t.Fatalf("changes = %+v", changes)
	}
	status, data = call("w", "GET", "/v1/tasks/task/files?path=first.txt", "", "", nil)
	if status != 200 || !bytes.Contains(data, []byte(`"content":"from the first attempt\n"`)) {
		t.Fatalf("files = %d %s", status, data)
	}
	// An accept by the first attempt's actor, who is an owner too, binds what the changes showed.
	if status, data := call("u", "POST", "/v1/tasks/task/accept", "stale", revisionOf(t, s), map[string]any{"commit": changes.Base, "generation": changes.Generation}); status != 409 {
		t.Fatalf("an accept of an older commit = %d %s", status, data)
	}
	status, data = call("u", "POST", "/v1/tasks/task/accept", "accept", revisionOf(t, s), map[string]any{"commit": changes.Commit, "generation": changes.Generation})
	if status != 200 || !bytes.Contains(data, []byte(`"state":"accepted"`)) {
		t.Fatalf("accept = %d %s", status, data)
	}
	if status, data := call("u", "POST", "/v1/tasks/task/refine", "refine-accepted", revisionOf(t, s), map[string]string{"backend": "codex", "mode": "auto"}); status != 409 {
		t.Fatalf("refine of an accepted task = %d %s", status, data)
	}
}

func TestAnAttemptThatAddsAProjectHookIsMarkedInChangesAndBeforeRefine(t *testing.T) {
	e, s, marker, _, _ := workspaceExecutor(t, "refine_hook")
	e.cfg.CancelGrace = naturalExitGrace
	call := reviewAPI(t, e, s)
	e.LaunchAttempt("attempt")
	waitTaskState(t, s, store.AwaitingReview)
	settled(t, e)
	status, data := call("w", "GET", "/v1/tasks/task/changes", "", "", nil)
	var changes changesBody
	if status != 200 || json.Unmarshal(data, &changes) != nil {
		t.Fatalf("changes = %d %s", status, data)
	}
	marked := false
	for _, f := range changes.Files {
		marked = marked || (f.Path == ".claude/settings.json" && f.Provider)
	}
	if !marked || !changes.ProviderFlagged || !changes.ProviderFiles {
		t.Fatalf("the project hook is not marked: %s", data)
	}
	status, data = call("u", "POST", "/v1/tasks/task/refine", "unacknowledged", revisionOf(t, s), map[string]string{"backend": "codex", "mode": "auto"})
	if status != 409 || !bytes.Contains(data, []byte(`"provider_configuration_changed"`)) {
		t.Fatalf("unacknowledged refine = %d %s", status, data)
	}
	if _, err := os.Stat(e.cfg.Workspaces.WorktreePath("p", "attempt")); err != nil {
		t.Fatalf("a refused refine removed the worktree: %v", err)
	}
	status, data = call("u", "POST", "/v1/tasks/task/refine", "acknowledged", revisionOf(t, s), map[string]any{"backend": "codex", "mode": "auto", "acknowledge_provider_changes": true})
	if status != 200 {
		t.Fatalf("acknowledged refine = %d %s", status, data)
	}
	task := waitTaskState(t, s, store.AwaitingReview)
	if task.Attempt.ID == "attempt" {
		t.Fatal("the refined attempt is not the task's")
	}
	settled(t, e)
	if lines := strings.Join(markerLines(t, marker), "\n"); !strings.Contains(lines, "saw:from the first attempt") {
		t.Fatalf("marker:\n%s", lines)
	}
}

func TestARefinedAttemptsBaselineRunsOnTheTasksImportedChangesCommit(t *testing.T) {
	e, s, _, _, _ := workspaceExecutor(t, "refine_fail")
	e.cfg.CancelGrace = naturalExitGrace
	call := reviewAPI(t, e, s)
	// first.txt is absent from the imported-changes commit, and present once the first attempt's work is committed.
	setTaskChecksForTest(t, s, []store.CheckDefinition{{Name: "absent", Argv: checkHelperArgv("assert_absent", "first.txt")}})
	e.LaunchAttempt("attempt")
	waitTaskState(t, s, store.Failed)
	settled(t, e)
	if status, data := call("u", "POST", "/v1/tasks/task/refine", "refine", revisionOf(t, s), map[string]string{"backend": "codex", "mode": "auto"}); status != 200 {
		t.Fatalf("refine = %d %s", status, data)
	}
	task := waitTaskState(t, s, store.AwaitingReview)
	settled(t, e)
	results := task.Attempt.CheckResults()
	if len(results) != 1 || results[0].State != store.CheckFailed || results[0].Baseline == nil || results[0].Baseline.State != store.CheckPassed || !results[0].NewFailure {
		t.Fatalf("results = %+v", results)
	}
	status, data := call("u", "GET", "/v1/tasks/task/changes", "", "", nil)
	var changes changesBody
	if status != 200 || json.Unmarshal(data, &changes) != nil || results[0].Baseline.TestedCommit != changes.Base {
		t.Fatalf("baseline tested %s, the task's base %s (%d %s)", results[0].Baseline.TestedCommit, changes.Base, status, data)
	}
}
