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
