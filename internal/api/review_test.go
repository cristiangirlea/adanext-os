package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

var (
	reviewedCommit = strings.Repeat("a", 40)
	importedCommit = strings.Repeat("d", 40)
)

// fakeReviewer records what the review routes ask of the clone.
type fakeReviewer struct {
	mu       sync.Mutex
	calls    []string
	commitFn func() (string, error)
}

func (f *fakeReviewer) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeReviewer) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeReviewer) Changes(_ context.Context, project, from, to, _ string, _ source.ChangeLimits) (source.ChangeSet, error) {
	f.record("changes " + from[:1] + ".." + to[:1])
	return source.ChangeSet{From: from, To: to, Total: 2, Files: []source.ChangedFile{
		{Path: "main.go", Status: "M", OldMode: "100644", NewMode: "100644", Patch: []byte("diff --git a/main.go b/main.go\n")},
		{Path: ".claude/settings.json", Status: "A", OldMode: "000000", NewMode: "100644", Patch: []byte("diff --git a/.claude/settings.json b/.claude/settings.json\n")},
	}}, nil
}

func (f *fakeReviewer) Files(_ context.Context, _, commit, path, _ string, _ int, _ int64) (source.FileView, error) {
	f.record("files " + path)
	if path == "link" {
		return source.FileView{Commit: commit, Path: path, Type: "symlink", Mode: "120000", Target: "../outside"}, nil
	}
	return source.FileView{Commit: commit, Path: path, Type: "blob", Mode: "100644", Content: []byte("\xff\x00"), Size: 2}, nil
}

func (f *fakeReviewer) CommitAgentChanges(_ context.Context, m source.Manifest, _ string) (string, string, error) {
	f.record("commit " + m.Attempt)
	if f.commitFn != nil {
		tested, err := f.commitFn()
		return tested, "", err
	}
	return reviewedCommit, "", nil
}

func (f *fakeReviewer) RemoveWorktree(_, attempt string) error {
	f.record("remove " + attempt)
	return nil
}

type reviewFixture struct {
	*projectsFixture
	project, task string
	reviewer      *fakeReviewer
	runner        *recordingRunner
	tips          *mutableTip
}

// newReviewFixture is a task of an imported project created by carol, whose attempt by erin ended
// with a workspace in state; alice owns the project, and dave is a member who is none of these.
func newReviewFixture(t *testing.T, state store.State, attemptState, tested string) *reviewFixture {
	t.Helper()
	reviewer, runner, tips := &fakeReviewer{}, &recordingRunner{}, &mutableTip{}
	p := newProjectsFixture(t, func(e *Execution) {
		e.EngineSHA256, e.Runner, e.Generation, e.Review = "engine", runner, 0, reviewer
		e.Branches = tips
	})
	project := p.importRepo(t, "alice", "import", newRepo(t))
	snapshot := p.snapshotOf(t, "snap", project.ID)
	ctx := context.Background()
	for _, u := range []string{"carol", "dave", "erin"} {
		if err := p.s.CreateUser(ctx, u, u); err != nil {
			t.Fatal(err)
		}
		if err := p.s.AddMember(ctx, project.ID, u, "member"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := p.s.Do(ctx, "alice", "seed-review", "seed", nil, func(tx *store.Tx) ([]byte, error) {
		for _, q := range []struct {
			query string
			args  []any
		}{
			{"INSERT INTO tasks(id,project,created_by,objective,acceptance_criteria,state,revision,created,updated,snapshot) VALUES('rt',?,'carol','o','c',?,4,'now','now',?)", []any{project.ID, state, snapshot}},
			{"INSERT INTO attempts(id,task,actor,backend,model,effort,mode,executor,ownership_generation,engine_sha256,state,started,ended,provider_changes) VALUES('ra','rt','erin','codex','','','auto','local',0,'engine',?,'now','now','{\"changes\":[],\"truncated\":false}')", []any{attemptState}},
			{"INSERT INTO workspaces(attempt,project,task,snapshot,base_commit,branch,path,manifest_version,manifest_hash,executor,policy,created,tested_commit,imported_commit) VALUES('ra',?,'rt',?,?,'ada/rt','/w',2,?,'local','auto','now',?,?)", []any{project.ID, snapshot, strings.Repeat("c", 40), strings.Repeat("f", 64), tested, importedCommit}},
		} {
			if _, err := tx.ExecContext(ctx, q.query, q.args...); err != nil {
				return nil, err
			}
		}
		return []byte("ok"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return &reviewFixture{projectsFixture: p, project: project.ID, task: "rt", reviewer: reviewer, runner: runner, tips: tips}
}

// mutableTip is a BranchReader whose one tip a test moves.
type mutableTip struct {
	mu  sync.Mutex
	tip string
}

func (m *mutableTip) BranchTip(string, string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tip
}

func (f *reviewFixture) setTip(commit string) {
	f.tips.mu.Lock()
	f.tips.tip = commit
	f.tips.mu.Unlock()
}

// do sends a request as user with an optional If-Match revision.
func (f *reviewFixture) do(t *testing.T, user, method, path, key, match string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, f.url+path, reader)
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
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

var refineBody = map[string]any{"backend": "codex", "mode": "auto"}

func TestANonMemberGets404FromChangesAndFiles(t *testing.T) {
	f := newReviewFixture(t, store.AwaitingReview, "ended_ok", reviewedCommit)
	for _, path := range []string{"/changes", "/files", "/files?path=link", "/findings"} {
		if status, data := f.do(t, "bob", "GET", "/v1/tasks/rt"+path, "", "", nil); status != 404 {
			t.Errorf("%s by a non-member = %d %s", path, status, data)
		}
	}
	if calls := f.reviewer.recorded(); len(calls) != 0 {
		t.Fatalf("a non-member's read reached the clone: %v", calls)
	}
}

func TestChangesShowTheTasksOwnDiffMarkProviderFilesAndTheGenerationToAccept(t *testing.T) {
	f := newReviewFixture(t, store.AwaitingReview, "ended_ok", reviewedCommit)
	status, data := f.do(t, "dave", "GET", "/v1/tasks/rt/changes", "", "", nil)
	if status != 200 {
		t.Fatalf("changes = %d %s", status, data)
	}
	got := decodeJSON[changesJSON](t, data)
	if got.Base != importedCommit || got.Commit != reviewedCommit || got.Attempt != "ra" || got.RenameThreshold != "50%" || len(got.Files) != 2 || !got.ProviderFiles {
		t.Fatalf("changes = %+v", got)
	}
	if got.Files[0].Provider || !got.Files[1].Provider || got.Stale != (got.Tip != reviewedCommit) {
		t.Fatalf("files = %+v", got.Files)
	}
	if calls := f.reviewer.recorded(); len(calls) != 1 || calls[0] != "changes d..a" {
		t.Fatalf("calls %v", calls)
	}
	status, data = f.do(t, "dave", "GET", "/v1/tasks/rt/files?path=link", "", "", nil)
	if status != 200 || !bytes.Contains(data, []byte(`"type":"symlink"`)) || !bytes.Contains(data, []byte(`"target":"../outside"`)) || bytes.Contains(data, []byte(`"content"`)) {
		t.Fatalf("a link = %d %s", status, data)
	}
	status, data = f.do(t, "dave", "GET", "/v1/tasks/rt/files?path=bin", "", "", nil)
	if status != 200 || !bytes.Contains(data, []byte(`"encoding":"base64"`)) || !bytes.Contains(data, []byte(`"content":"/wA="`)) {
		t.Fatalf("a binary file = %d %s", status, data)
	}
}

func TestChangesOfATaskWithNoAgentCommitAreRefused(t *testing.T) {
	f := newReviewFixture(t, store.Failed, "ended_failed", "")
	if status, data := f.do(t, "alice", "GET", "/v1/tasks/rt/changes", "", "", nil); status != 409 || !bytes.Contains(data, []byte(`"no_changes"`)) {
		t.Fatalf("changes = %d %s", status, data)
	}
}

func TestFindingsFromANonMemberAreRefusedAndAnyMemberRecordsThem(t *testing.T) {
	f := newReviewFixture(t, store.AwaitingReview, "ended_ok", reviewedCommit)
	if status, data := f.do(t, "bob", "POST", "/v1/tasks/rt/findings", "f-bob", "", map[string]string{"body": "x"}); status != 404 {
		t.Fatalf("a non-member's finding = %d %s", status, data)
	}
	if status, data := f.do(t, "dave", "POST", "/v1/tasks/rt/findings", "f-dave", "", map[string]string{"body": "rename it"}); status != 201 {
		t.Fatalf("a member's finding = %d %s", status, data)
	}
	if status, data := f.do(t, "dave", "POST", "/v1/tasks/rt/findings", "f-empty", "", map[string]string{"body": ""}); status != 400 {
		t.Fatalf("an empty finding = %d %s", status, data)
	}
	status, data := f.do(t, "carol", "GET", "/v1/tasks/rt/findings", "", "", nil)
	if status != 200 || !bytes.Contains(data, []byte(`"author":"dave"`)) {
		t.Fatalf("findings = %d %s", status, data)
	}
}

func TestAnAcceptOrRefineByTheCreatorOrAReviewerWhoIsNeitherTheActorNorAnOwnerIsRefused(t *testing.T) {
	f := newReviewFixture(t, store.AwaitingReview, "ended_ok", reviewedCommit)
	f.setTip(reviewedCommit)
	for _, user := range []string{"carol", "dave"} {
		if status, data := f.do(t, user, "POST", "/v1/tasks/rt/accept", "accept-"+user, "4", map[string]any{"commit": reviewedCommit, "generation": 0}); status != 403 {
			t.Errorf("accept by %s = %d %s", user, status, data)
		}
		if status, data := f.do(t, user, "POST", "/v1/tasks/rt/refine", "refine-"+user, "4", refineBody); status != 403 {
			t.Errorf("refine by %s = %d %s", user, status, data)
		}
	}
	if status, data := f.do(t, "bob", "POST", "/v1/tasks/rt/accept", "accept-bob", "4", map[string]any{"commit": reviewedCommit, "generation": 0}); status != 404 {
		t.Errorf("accept by a non-member = %d %s", status, data)
	}
	if calls := f.reviewer.recorded(); len(calls) != 0 || len(f.runner.launches()) != 0 {
		t.Fatalf("a refused refine touched the clone %v or launched %v", calls, f.runner.launches())
	}
}

func TestAnAcceptAgainstAnOlderCommitOrGenerationIsRefusedAndTheCurrentOneAccepted(t *testing.T) {
	f := newReviewFixture(t, store.AwaitingReview, "ended_ok", reviewedCommit)
	f.setTip(reviewedCommit)
	for i, body := range []map[string]any{{"commit": importedCommit, "generation": 0}, {"commit": reviewedCommit, "generation": 1}} {
		if status, data := f.do(t, "erin", "POST", "/v1/tasks/rt/accept", fmt.Sprint("stale-", i), "4", body); status != 409 || !bytes.Contains(data, []byte(`"stale_review"`)) {
			t.Errorf("stale accept %v = %d %s", body, status, data)
		}
	}
	f.setTip(importedCommit) // the branch moved after the review
	if status, data := f.do(t, "erin", "POST", "/v1/tasks/rt/accept", "moved", "4", map[string]any{"commit": reviewedCommit, "generation": 0}); status != 409 {
		t.Errorf("accept after the branch moved = %d %s", status, data)
	}
	f.setTip(reviewedCommit)
	status, data := f.do(t, "erin", "POST", "/v1/tasks/rt/accept", "ok", "4", map[string]any{"commit": reviewedCommit, "generation": 0})
	if status != 200 || !bytes.Contains(data, []byte(`"state":"accepted"`)) || !bytes.Contains(data, []byte(`"reviewer":"erin"`)) {
		t.Fatalf("accept = %d %s", status, data)
	}
	if status, data := f.do(t, "alice", "POST", "/v1/tasks/rt/refine", "refine-accepted", "5", refineBody); status != 409 || !bytes.Contains(data, []byte(`"invalid_transition"`)) {
		t.Fatalf("refine of an accepted task = %d %s", status, data)
	}
}

func TestRefineOfAFailedOrCanceledTaskCommitsItsChangesRemovesItsWorktreeAndLaunchesTheRequestersAttempt(t *testing.T) {
	for state, attemptState := range map[store.State]string{store.Failed: "ended_failed", store.Canceled: "canceled"} {
		t.Run(string(state), func(t *testing.T) { refineEnded(t, state, attemptState) })
	}
}

func refineEnded(t *testing.T, state store.State, attemptState string) {
	f := newReviewFixture(t, state, attemptState, "")
	status, data := f.do(t, "alice", "POST", "/v1/tasks/rt/refine", "refine", "4", refineBody)
	if status != 200 {
		t.Fatalf("refine = %d %s", status, data)
	}
	started := decodeJSON[startedTaskJSON](t, data)
	if started.State != store.Preparing || started.AttemptID == "" {
		t.Fatalf("refine = %s", data)
	}
	if calls := f.reviewer.recorded(); len(calls) != 2 || calls[0] != "commit ra" || calls[1] != "remove ra" {
		t.Fatalf("calls %v", calls)
	}
	if got := f.runner.launches(); len(got) != 1 || got[0] != started.AttemptID {
		t.Fatalf("launches %v", got)
	}
	a, err := f.s.AttemptByID(context.Background(), started.AttemptID)
	if err != nil || a.Actor != "alice" {
		t.Fatalf("the refined attempt %+v %v", a, err)
	}
	rs, _ := f.s.ReviewState(context.Background(), "alice", "rt")
	if !rs.Attempts[0].Removed || rs.Attempts[0].TestedCommit != reviewedCommit {
		t.Fatalf("the earlier attempt %+v", rs.Attempts[0])
	}
	// A replay answers the same without touching the clone again.
	if status, again := f.do(t, "alice", "POST", "/v1/tasks/rt/refine", "refine", "4", refineBody); status != 200 || !bytes.Equal(again, data) || len(f.reviewer.recorded()) != 2 {
		t.Fatalf("replay = %d %s, calls %v", status, again, f.reviewer.recorded())
	}
}

func TestRefineOfAnAttemptThatAddedAProjectHookShowsItFirst(t *testing.T) {
	f := newReviewFixture(t, store.AwaitingReview, "ended_ok", reviewedCommit)
	if _, err := f.s.Do(context.Background(), "alice", "hook", "hook", nil, func(tx *store.Tx) ([]byte, error) {
		return []byte("ok"), tx.RecordProviderChanges(context.Background(), "ra", store.ProviderChanges{Changes: []store.ProviderChange{{Path: ".claude/settings.json", Change: "added"}}})
	}); err != nil {
		t.Fatal(err)
	}
	status, data := f.do(t, "erin", "POST", "/v1/tasks/rt/refine", "unacknowledged", "4", refineBody)
	if status != 409 || !bytes.Contains(data, []byte(`"provider_configuration_changed"`)) || !bytes.Contains(data, []byte(`.claude/settings.json`)) {
		t.Fatalf("unacknowledged refine = %d %s", status, data)
	}
	if calls := f.reviewer.recorded(); len(calls) != 0 {
		t.Fatalf("an unacknowledged refine touched the clone: %v", calls)
	}
	body := map[string]any{"backend": "codex", "mode": "auto", "acknowledge_provider_changes": true}
	if status, data := f.do(t, "erin", "POST", "/v1/tasks/rt/refine", "acknowledged", "4", body); status != 200 {
		t.Fatalf("acknowledged refine = %d %s", status, data)
	}
}

func TestRefineIsRefusedForARunningOrRecoveryRequiredAttempt(t *testing.T) {
	for state, code := range map[string]string{"running": "attempt_active", "recovery_required": "recovery_required"} {
		t.Run(state, func(t *testing.T) {
			f := newReviewFixture(t, store.Failed, state, "")
			if status, data := f.do(t, "erin", "POST", "/v1/tasks/rt/refine", "r", "4", refineBody); status != 409 || !bytes.Contains(data, []byte(`"`+code+`"`)) {
				t.Fatalf("refine = %d %s", status, data)
			}
			if calls := f.reviewer.recorded(); len(calls) != 0 {
				t.Fatalf("calls %v", calls)
			}
		})
	}
}

func TestAnAttemptWhoseRemainingChangesCannotBeCommittedGoesToRecoveryAndRefusesTheRefine(t *testing.T) {
	f := newReviewFixture(t, store.Failed, "ended_failed", "")
	f.reviewer.commitFn = func() (string, error) { return "", &source.MismatchError{Reason: "the clone config differs"} }
	if status, data := f.do(t, "erin", "POST", "/v1/tasks/rt/refine", "r", "4", refineBody); status != 409 || !bytes.Contains(data, []byte(`"recovery_required"`)) {
		t.Fatalf("refine = %d %s", status, data)
	}
	a, _ := f.s.AttemptByID(context.Background(), "ra")
	if a.State != "recovery_required" || len(f.runner.launches()) != 0 {
		t.Fatalf("attempt %+v, launches %v", a, f.runner.launches())
	}
}
