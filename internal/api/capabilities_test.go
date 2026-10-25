package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/engine"
	"github.com/cristiangirlea/ada-os/internal/store"
)

type fakeProber map[string]engine.Installed

func (f fakeProber) Installed(context.Context) map[string]engine.Installed { return f }

func TestCapabilitiesAreServedToAccountsWithoutPathsOrSecrets(t *testing.T) {
	prober := fakeProber{
		"claude": {Available: true, Version: "2.1.284", Modes: []string{"readonly", "auto", "yolo"}, Options: []string{"model", "mode", "effort", "timeout", "max_turns", "mcp"}},
		"codex":  {Reason: "the engine did not find the CLI or its sign-in"},
	}
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}, Capabilities: prober})
	code, body := f.request(t, "GET", "/v1/capabilities", "", "", nil)
	if code != 200 {
		t.Fatalf("capabilities = %d %s", code, body)
	}
	var m engine.Matrix
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.Checks.Status != engine.Unsandboxed || len(m.Backends) != 2 {
		t.Fatalf("matrix = %s", body)
	}
	cell := func(backend, mode, row string) engine.Cell {
		for _, b := range m.Backends {
			for _, md := range b.Modes {
				if b.Backend == backend && md.Mode == mode {
					return md.Cells[row]
				}
			}
		}
		return engine.Cell{}
	}
	if got := cell("claude", "readonly", engine.RowReadOnly); got.Status != engine.Requested {
		t.Fatalf("claude's recorded version: %+v", got)
	}
	if got := cell("codex", "readonly", engine.RowReadOnly); got.Status != engine.Unverified || got.Recorded != engine.Requested {
		t.Fatalf("codex not found: %+v", got)
	}
	for _, leak := range []string{`:\\`, "/home/", "/Users/", "probe-secret", "\"modes\":[\"readonly\",\"auto\",\"yolo\"],\"options\""} {
		if bytes.Contains(body, []byte(leak)) {
			t.Errorf("the matrix holds %q: %s", leak, body)
		}
	}
	if code, _ := f.request(t, "POST", "/v1/capabilities", "k", "", nil); code != 405 {
		t.Fatalf("POST = %d", code)
	}
}

func TestCapabilitiesAreRefusedToAnUnknownAccount(t *testing.T) {
	f := newFixture(t)
	h := New(f.s, f.hub, func(*http.Request) (string, error) { return "mallory", nil })
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/capabilities", nil))
	if w.Code != 403 || !strings.Contains(w.Body.String(), "forbidden") {
		t.Fatalf("unknown account = %d %s", w.Code, w.Body.String())
	}
}

func TestAStartTheMatrixRefusesIsAnswered409BeforeAnyPreparation(t *testing.T) {
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}})
	id := create(t, f, "matrix-create", "work")["id"].(string)
	if code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "matrix-ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	for i, body := range []string{
		`{"backend":"gemini","mode":"readonly"}`,
		`{"backend":"codex","mode":"ask"}`,
		`{"backend":"claude","mode":"ask"}`,
		`{"backend":"codex","mode":"plan"}`,
		`{"backend":"codex","mode":"auto","effort":"turbo"}`,
		`{"backend":"codex","mode":"readonly","max_turns":3}`,
	} {
		code, response := f.request(t, "POST", "/v1/tasks/"+id+"/start", "matrix-start-"+string(rune('a'+i)), "1", []byte(body))
		var refused struct{ Error, Message string }
		if err := json.Unmarshal(response, &refused); err != nil || code != 409 || refused.Error != "unsupported_options" || refused.Message == "" {
			t.Fatalf("%s = %d %s", body, code, response)
		}
	}
	task, err := f.s.Task(context.Background(), "alice", id)
	if err != nil || task.State != store.Ready || task.Revision != 1 || task.Attempt.ID != "" {
		t.Fatalf("a refused start changed the task: %+v, %v", task, err)
	}
}

func TestAStartRecordsItsTurnAndTimeLimits(t *testing.T) {
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}})
	id := create(t, f, "limits-create", "work")["id"].(string)
	if code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "limits-ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	path := "/v1/tasks/" + id + "/start"
	for i, body := range []string{
		`{"backend":"claude","mode":"readonly","max_turns":-1}`,
		`{"backend":"claude","mode":"readonly","max_turns":1001}`,
		`{"backend":"claude","mode":"readonly","timeout":"soon"}`,
		`{"backend":"claude","mode":"readonly","timeout":"0s"}`,
		`{"backend":"claude","mode":"readonly","timeout":"25h"}`,
		`{"backend":"claude","mode":"readonly","timeout":"-1m"}`,
	} {
		if code, response := f.request(t, "POST", path, "bad-limit-"+string(rune('a'+i)), "1", []byte(body)); code != 400 {
			t.Fatalf("%s = %d %s", body, code, response)
		}
	}
	code, response := f.request(t, "POST", path, "limits-start", "1", []byte(`{"backend":"claude","mode":"readonly","max_turns":3,"timeout":"3m"}`))
	if code != 200 {
		t.Fatalf("start = %d %s", code, response)
	}
	var started struct {
		AttemptID string `json:"attempt_id"`
	}
	if err := json.Unmarshal(response, &started); err != nil {
		t.Fatal(err)
	}
	if turns, timeout, err := f.s.AttemptLimits(context.Background(), started.AttemptID); err != nil || turns != 3 || timeout != "3m0s" {
		t.Fatalf("limits = %d %q %v", turns, timeout, err)
	}
}

func TestTheTaskShowsTheProviderConfigurationItsSnapshotHolds(t *testing.T) {
	p := newProjectsFixture(t)
	dir := canonicalTemp(t)
	runGit(t, dir, "init", "-q")
	writeFile(t, filepath.Join(dir, "README.md"), "hello\n")
	writeFile(t, filepath.Join(dir, ".claude", "settings.json"), `{"hooks":{"SessionStart":[]}}`)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "first")
	writeFile(t, filepath.Join(dir, ".mcp.json"), "{}\n")
	writeFile(t, filepath.Join(dir, ".codex", "config.toml"), "a = 1\n")
	status, data := p.call(t, "alice", "POST", "/v1/projects/inspect", "", map[string]string{"path": dir})
	if status != 200 {
		t.Fatalf("inspect = %d %s", status, data)
	}
	var inspection struct {
		Blockers []any `json:"blockers"`
		Findings []struct {
			Code  string   `json:"code"`
			Paths []string `json:"paths"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(data, &inspection); err != nil || len(inspection.Blockers) != 0 || len(inspection.Findings) != 1 || inspection.Findings[0].Code != "provider_configuration" || len(inspection.Findings[0].Paths) != 3 {
		t.Fatalf("inspection = %s", data)
	}
	project := p.importRepo(t, "alice", "provider-import", dir)
	// Only the selected untracked file enters the snapshot, and so the worktree.
	status, data = p.call(t, "alice", "POST", "/v1/projects/"+project.ID+"/snapshots", "provider-snap", map[string][]string{"untracked": {".mcp.json"}})
	if status != 201 {
		t.Fatalf("snapshot = %d %s", status, data)
	}
	var snap struct {
		ID       string                       `json:"id"`
		Provider *store.ProviderConfiguration `json:"provider_configuration"`
	}
	if err := json.Unmarshal(data, &snap); err != nil || snap.Provider == nil || strings.Join(snap.Provider.Paths, ",") != ".claude/settings.json,.mcp.json" {
		t.Fatalf("snapshot = %s", data)
	}
	status, data = p.call(t, "alice", "POST", "/v1/tasks", "provider-task", map[string]string{"project": project.ID, "objective": "o", "criteria": "c", "snapshot": snap.ID})
	if status != 201 {
		t.Fatalf("task = %d %s", status, data)
	}
	var task struct {
		ID       string                       `json:"id"`
		Provider *store.ProviderConfiguration `json:"provider_configuration"`
	}
	if err := json.Unmarshal(data, &task); err != nil {
		t.Fatal(err)
	}
	status, data = p.call(t, "alice", "GET", "/v1/tasks/"+task.ID, "", nil)
	if err := json.Unmarshal(data, &task); status != 200 || err != nil || task.Provider == nil || task.Provider.Count != 2 {
		t.Fatalf("task read = %d %s", status, data)
	}
}

func TestTheTaskShowsWhatItsAttemptChangedOfTheProviderConfiguration(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := create(t, f, "changes-create", "work")["id"].(string)
	if err := f.s.CreateAttempt(ctx, store.Attempt{ID: "changes-attempt", Task: id, Actor: "alice", Backend: "claude", Mode: "readonly", Executor: "local", EngineSHA256: "hash", State: "ended_ok"}); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		code, body := f.request(t, "GET", "/v1/tasks/"+id, "", "", nil)
		if code != 200 {
			t.Fatalf("read = %d %s", code, body)
		}
		var task map[string]any
		if err := json.Unmarshal(body, &task); err != nil {
			t.Fatal(err)
		}
		return task["attempt"].(map[string]any)
	}
	if attempt := read(); attempt["provider_changes"] != nil {
		t.Fatalf("unassessed attempt = %v", attempt)
	}
	_, err := f.s.Do(ctx, "alice", "record-changes", "record-changes", nil, func(tx *store.Tx) ([]byte, error) {
		return []byte("ok"), tx.RecordProviderChanges(ctx, "changes-attempt", store.ProviderChanges{Changes: []store.ProviderChange{{Path: ".claude/settings.json", Change: "added"}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt := read()
	changes, _ := attempt["provider_changes"].(map[string]any)
	if changes == nil || attempt["provider_flagged"] != true || len(changes["changes"].([]any)) != 1 {
		t.Fatalf("assessed attempt = %v", attempt)
	}
}
