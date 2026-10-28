package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func TestChecksDeclaredOnCreateAndUpdatedAndCeilingsEnforced(t *testing.T) {
	f := newFixture(t)

	// Create a task with declared checks.
	code, body := f.request(t, "POST", "/v1/tasks", "create-with-checks", "", []byte(`{
		"project":"a",
		"objective":"verify checks",
		"criteria":"done",
		"checks":[
			{"name":"unit","argv":["go","test","./..."],"workdir":"sub","timeout":"2m","max_output_bytes":4096,"env":["CUSTOM_VAR"]}
		]
	}`))
	if code != 201 {
		t.Fatalf("create with checks: %d %s", code, body)
	}
	var created taskJSON
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Checks) != 1 || created.Checks[0].Name != "unit" || created.Checks[0].Timeout != "2m0s" || created.Checks[0].MaxOutputBytes != 4096 {
		t.Fatalf("created checks = %+v", created.Checks)
	}

	// Update checks via PUT /v1/tasks/{id}/checks and verify revision bumps.
	code, body = f.request(t, "PUT", "/v1/tasks/"+created.ID+"/checks", "update-checks", "0", []byte(`{
		"checks":[
			{"name":"lint","argv":["golangci-lint","run"]},
			{"name":"unit","argv":["go","test","./..."]}
		]
	}`))
	if code != 200 {
		t.Fatalf("PUT checks: %d %s", code, body)
	}
	var updated taskJSON
	if err := json.Unmarshal(body, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 1 || len(updated.Checks) != 2 || updated.Checks[0].Name != "lint" || updated.Checks[0].Timeout != "30m0s" || updated.Checks[0].MaxOutputBytes != store.MaxCheckOutputBytes {
		t.Fatalf("updated = %+v", updated)
	}

	// Refused: 17th check, timeout > 30m, output cap > 16 MiB, never-passed env variable, credential profile env variable.
	var seventeen []string
	for i := range 17 {
		seventeen = append(seventeen, fmt.Sprintf(`{"name":"c%d","argv":["bin"]}`, i))
	}
	badBodies := []string{
		`{"checks":[` + strings.Join(seventeen, ",") + `]}`,
		`{"checks":[{"name":"c","argv":["bin"],"timeout":"31m"}]}`,
		fmt.Sprintf(`{"checks":[{"name":"c","argv":["bin"],"max_output_bytes":%d}]}`, store.MaxCheckOutputBytes+1),
		`{"checks":[{"name":"c","argv":["bin"],"env":["ANTHROPIC_API_KEY"]}]}`,
		`{"checks":[{"name":"c","argv":["bin"],"env":["CLAUDE_CONFIG_DIR"]}]}`,
		`{"checks":[{"name":"c","argv":["bin"],"env":["CODEX_HOME"]}]}`,
		`{"checks":[{"name":"c","argv":["bin"],"workdir":"../outside"}]}`,
	}
	for i, b := range badBodies {
		c, resp := f.request(t, "PUT", "/v1/tasks/"+created.ID+"/checks", fmt.Sprintf("bad-%d", i), "1", []byte(b))
		if c != 400 {
			t.Fatalf("bad body %d expected 400, got %d: %s", i, c, resp)
		}
	}
}

func TestCheckResultsAndCheckLogEnforceMembershipOnEachRead(t *testing.T) {
	ctx := context.Background()
	artDir := filepath.Join(t.TempDir(), "artifacts")
	arts, err := artifact.Open(artDir)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, Execution{Artifacts: arts, Writer: arts})
	if err := f.s.AddMember(ctx, "a", "bob", "member"); err != nil {
		t.Fatal(err)
	}

	taskMap := create(t, f, "create-check-log", "log test")
	taskID := taskMap["id"].(string)

	logBytes := []byte("check log line 1\ncheck log line 2\n")
	ref, err := arts.Write(ctx, bytes.NewReader(logBytes), int64(len(logBytes)))
	if err != nil {
		t.Fatal(err)
	}

	exitZero := 0
	_, err = f.s.Do(ctx, "alice", "seed-check-attempt", "seed-1", nil, func(tx *store.Tx) ([]byte, error) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO attempts(id,task,actor,backend,model,effort,mode,executor,ownership_generation,engine_sha256,state,started) VALUES('att1',?,'alice','claude','sonnet','medium','readonly','local',1,'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','ended_ok','2026-10-25T00:00:00Z')", taskID); err != nil {
			return nil, err
		}
		if err := tx.ReferenceArtifact(ctx, ref.Hash, ref.Size, "check:chk_1"); err != nil {
			return nil, err
		}
		if err := tx.RecordCheckResult(ctx, store.CheckResult{
			ID:             "chk_1",
			Task:           taskID,
			Attempt:        "att1",
			Ord:            0,
			Name:           "unit",
			Definition:     store.CheckDefinition{Name: "unit", Argv: []string{"bin"}},
			State:          store.CheckPassed,
			ExitCode:       &exitZero,
			TestedCommit:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			BoundCommit:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			OutputArtifact: ref.Hash,
			OutputSize:     ref.Size,
			Truncated:      true,
		}); err != nil {
			return nil, err
		}
		return []byte("ok"), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Switch the server's authenticator to read as X-User header when set.
	f.server.Handler = New(f.s, f.hub, func(r *http.Request) (string, error) {
		if u := r.Header.Get("X-User"); u != "" {
			return u, nil
		}
		return "alice", nil
	}, Execution{Artifacts: arts, References: f.s, Writer: arts})

	getLog := func(user, path string) (int, string, []byte) {
		req, err := http.NewRequest("GET", "http://ipc"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-User", user)
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header.Get("X-Ada-Truncated"), b
	}

	// Member (bob) reads check results and check log.
	req, _ := http.NewRequest("GET", "http://ipc/v1/tasks/"+taskID+"/checks", nil)
	req.Header.Set("X-User", "bob")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("bob GET /v1/tasks/{id}/checks = %d", resp.StatusCode)
	}

	status, truncHeader, gotLog := getLog("bob", "/v1/checks/chk_1/log")
	if status != 200 || truncHeader != "true" || !bytes.Equal(gotLog, logBytes) {
		t.Fatalf("bob GET log = %d, trunc=%q, body=%q", status, truncHeader, gotLog)
	}

	// The task-scoped route serves only that task's runs.
	other := create(t, f, "create-other-task", "other task")["id"].(string)
	if status, _, _ := getLog("bob", "/v1/tasks/"+other+"/checks/chk_1/log"); status != 404 {
		t.Fatalf("GET another task's run through /v1/tasks/{other}/checks = %d, want 404", status)
	}
	if status, _, _ := getLog("bob", "/v1/tasks/"+taskID+"/checks/chk_1/log"); status != 200 {
		t.Fatalf("GET /v1/tasks/{task}/checks/{id}/log = %d, want 200", status)
	}

	// Remove bob from project "a": both routes now return 404 on every read.
	if err := f.s.RemoveMember(ctx, "a", "bob"); err != nil {
		t.Fatal(err)
	}
	status, _, _ = getLog("bob", "/v1/checks/chk_1/log")
	if status != 404 {
		t.Fatalf("removed member GET /v1/checks/chk_1/log = %d, want 404", status)
	}
	status, _, _ = getLog("bob", "/v1/tasks/"+taskID+"/checks/chk_1/log")
	if status != 404 {
		t.Fatalf("removed member GET /v1/tasks/{id}/checks/chk_1/log = %d, want 404", status)
	}
}

func setTaskState(t *testing.T, f *fixture, id string, state store.State, revision int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.s.Do(ctx, "alice", "state-"+id+string(state), "state", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "UPDATE tasks SET state=?,revision=? WHERE id=?", state, revision, id)
		return []byte("ok"), err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCancelDuringVerifyingAsksTheExecutorToStopTheChecks(t *testing.T) {
	runner := &recordingRunner{}
	f := newFixture(t, Execution{Runner: runner})
	id := create(t, f, "create-verifying", "verify")["id"].(string)
	setTaskState(t, f, id, store.Verifying, 3)
	if code, body := f.request(t, "POST", "/v1/tasks/"+id+"/cancel", "cancel-verifying", "3", nil); code != 200 {
		t.Fatalf("cancel while verifying = %d %s", code, body)
	}
	if got := runner.cancels(); len(got) != 1 || got[0] != id {
		t.Fatalf("cancels = %v, want [%s]", got, id)
	}
}

func TestATaskInTheUnusedRefinementStateCannotBeReadied(t *testing.T) {
	f := newFixture(t)
	id := create(t, f, "create-review", "review")["id"].(string)
	setTaskState(t, f, id, store.Refinement, 2)
	if code, body := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "ready-from-refinement", "2", nil); code == 200 {
		t.Fatalf("ready from refinement = %d %s, want refused", code, body)
	}
}

// fixedTips is a BranchReader over a map of project/task to tip.
type fixedTips map[string]string

func (f fixedTips) BranchTip(project, task string) string { return f[project+"/"+task] }

func TestCheckResultsAreInvalidatedUnlessTheyBindTheBranchTip(t *testing.T) {
	ctx := context.Background()
	tips := fixedTips{}
	f := newFixture(t, Execution{Branches: tips})
	id := create(t, f, "create-tip", "tip")["id"].(string)
	bound := strings.Repeat("a", 40)
	if _, err := f.s.Do(ctx, "alice", "seed-tip", "seed", nil, func(tx *store.Tx) ([]byte, error) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO attempts(id,task,actor,backend,model,effort,mode,executor,ownership_generation,engine_sha256,state,started) VALUES('att1',?,'alice','claude','sonnet','medium','readonly','local',1,'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','ended_ok','2026-10-25T00:00:00Z')", id); err != nil {
			return nil, err
		}
		for i, r := range []store.CheckResult{
			{ID: "chk_bound", State: store.CheckPassed, BoundCommit: bound},
			{ID: "chk_unbound", State: store.CheckChangedTree},
		} {
			r.Task, r.Attempt, r.Ord, r.TestedCommit = id, "att1", i, bound
			r.Definition = store.CheckDefinition{Argv: []string{"bin"}}
			if err := tx.RecordCheckResult(ctx, r); err != nil {
				return nil, err
			}
		}
		return []byte("ok"), nil
	}); err != nil {
		t.Fatal(err)
	}
	read := func() (list, attempt []store.CheckResult) {
		t.Helper()
		code, body := f.request(t, "GET", "/v1/tasks/"+id+"/checks", "", "", nil)
		if code != 200 || json.Unmarshal(body, &list) != nil {
			t.Fatalf("GET checks = %d %s", code, body)
		}
		code, body = f.request(t, "GET", "/v1/tasks/"+id, "", "", nil)
		var task taskJSON
		if code != 200 || json.Unmarshal(body, &task) != nil || task.Attempt == nil {
			t.Fatalf("GET task = %d %s", code, body)
		}
		return list, task.Attempt.Checks
	}
	for _, tc := range []struct {
		tip       string
		boundOK   bool
		situation string
	}{
		{"", false, "an unknown tip"},
		{bound, true, "the tip it binds"},
		{strings.Repeat("b", 40), false, "a moved tip"},
	} {
		tips["a/"+id] = tc.tip
		list, attempt := read()
		for _, results := range [][]store.CheckResult{list, attempt} {
			if len(results) != 2 || results[0].Invalidated == tc.boundOK || !results[1].Invalidated {
				t.Fatalf("at %s: %+v", tc.situation, results)
			}
		}
	}
	// A mutation's response reads any result it carries as GET does.
	tips["a/"+id] = bound
	_, want := read()
	for _, m := range []struct {
		method, path, key, match string
		body                     []byte
	}{
		{"PUT", "/v1/tasks/" + id + "/checks", "tip-checks", "0", []byte(`{"checks":[{"argv":["bin"]}]}`)},
		{"POST", "/v1/tasks/" + id + "/ready", "tip-ready", "1", nil},
	} {
		code, body := f.request(t, m.method, m.path, m.key, m.match, m.body)
		var task taskJSON
		if code != 200 || json.Unmarshal(body, &task) != nil {
			t.Fatalf("%s %s = %d %s", m.method, m.path, code, body)
		}
		if task.Attempt == nil {
			continue
		}
		for i, r := range task.Attempt.Checks {
			if i >= len(want) || r.Invalidated != want[i].Invalidated {
				t.Fatalf("%s %s: %+v, GET reads %+v", m.method, m.path, task.Attempt.Checks, want)
			}
		}
	}
}
