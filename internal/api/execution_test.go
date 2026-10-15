package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/store"
)

type noopRunner struct{}

func (noopRunner) LaunchAttempt(string) {}
func (noopRunner) CancelTask(string)    {}

func TestStartWithoutRunnerLeavesReadyTaskUntouched(t *testing.T) {
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64)})
	id := create(t, f, "no-runner-create", "work")["id"].(string)
	if code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "no-runner-ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/start", "no-runner-start", "1", []byte(`{"backend":"codex","mode":"auto"}`))
	if code != 503 {
		t.Fatalf("start = %d", code)
	}
	task, err := f.s.Task(context.Background(), "alice", id)
	if err != nil || task.State != store.Ready {
		t.Fatalf("task = %+v: %v", task, err)
	}
}

func TestStartWithoutEngineLeavesReadyTaskUntouched(t *testing.T) {
	f := newFixture(t)
	id := create(t, f, "unavailable-create", "work")["id"].(string)
	if code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "unavailable-ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	code, body := f.request(t, "POST", "/v1/tasks/"+id+"/start", "unavailable-start", "1", []byte(`{"backend":"codex","mode":"auto"}`))
	if code != 503 || !bytes.Contains(body, []byte("engine_unavailable")) {
		t.Fatalf("start = %d %s", code, body)
	}
	task, err := f.s.Task(httptest.NewRequest(http.MethodGet, "/", nil).Context(), "alice", id)
	if err != nil || task.State != store.Ready {
		t.Fatalf("task = %+v: %v", task, err)
	}
	_, err = f.s.Do(context.Background(), "alice", "unavailable-attempt-count", "unavailable-attempt-count", nil, func(tx *store.Tx) ([]byte, error) {
		var count int
		err := tx.QueryRowContext(context.Background(), "SELECT count(*) FROM attempts WHERE task=?", id).Scan(&count)
		if count != 0 {
			t.Errorf("attempts = %d", count)
		}
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStartRecordsPolicyOnceAndRefusesUnknownFields(t *testing.T) {
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Generation: 0, Runner: noopRunner{}})
	id := create(t, f, "policy-create", "work")["id"].(string)
	if code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "policy-ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	path := "/v1/tasks/" + id + "/start"
	if code, _ := f.request(t, "POST", path, "unknown", "1", []byte(`{"backend":"codex","mode":"auto","surprise":true}`)); code != 400 {
		t.Fatalf("unknown field = %d", code)
	}
	for _, invalid := range []string{`{"backend":"codex","mode":"ask"}`, `{"backend":"","mode":"auto"}`, `{"backend":"codex"}`} {
		if code, _ := f.request(t, "POST", path, "invalid-"+invalid, "1", []byte(invalid)); code != 400 {
			t.Fatalf("invalid policy %s = %d", invalid, code)
		}
	}
	policy := []byte(`{"backend":"codex","model":"m","effort":"high","mode":"auto"}`)
	code, first := f.request(t, "POST", path, "policy-start", "1", policy)
	if code != 200 {
		t.Fatalf("start = %d %s", code, first)
	}
	code, replay := f.request(t, "POST", path, "policy-start", "1", policy)
	if code != 200 || !bytes.Equal(first, replay) {
		t.Fatalf("replay = %d %s", code, replay)
	}
	if code, _ := f.request(t, "POST", path, "new-key", "1", policy); code != 409 {
		t.Fatalf("stale key = %d", code)
	}
	var response struct {
		AttemptID string `json:"attempt_id"`
	}
	if err := json.Unmarshal(first, &response); err != nil || response.AttemptID == "" {
		t.Fatalf("attempt id = %q: %v", response.AttemptID, err)
	}
	a, err := f.s.AttemptByID(context.Background(), response.AttemptID)
	if err != nil || a.Actor != "alice" || a.Backend != "codex" || a.Model != "m" || a.Effort != "high" || a.Mode != "auto" || a.State != "pending" || a.OwnershipGeneration != 0 {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	_, err = f.s.Do(context.Background(), "alice", "count-attempts", "count-attempts", nil, func(tx *store.Tx) ([]byte, error) {
		var count int
		err := tx.QueryRowContext(context.Background(), "SELECT count(*) FROM attempts WHERE task=?", id).Scan(&count)
		if count != 1 {
			t.Errorf("attempts = %d", count)
		}
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStartRejectsInvalidBackendModelAndEffort(t *testing.T) {
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}})
	id := create(t, f, "validation-create", "work")["id"].(string)
	if code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "validation-ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	path := "/v1/tasks/" + id + "/start"
	cases := []string{
		`{"backend":"Codex","mode":"auto"}`,
		`{"backend":"a b","mode":"auto"}`,
		`{"backend":"` + strings.Repeat("a", 33) + `","mode":"auto"}`,
		`{"backend":"codex","model":"` + strings.Repeat("m", 129) + `","mode":"auto"}`,
		`{"backend":"codex","model":"m\n","mode":"auto"}`,
		`{"backend":"codex","effort":"` + strings.Repeat("e", 129) + `","mode":"auto"}`,
		`{"backend":"codex","effort":"é","mode":"auto"}`,
	}
	for _, body := range cases {
		if code, response := f.request(t, "POST", path, body, "1", []byte(body)); code != 400 {
			t.Fatalf("body %q = %d %s", body, code, response)
		}
	}
	if code, response := f.request(t, "POST", path, "valid", "1", []byte(`{"backend":"a_0-","model":"`+strings.Repeat("m", 128)+`","effort":"","mode":"auto"}`)); code != 200 {
		t.Fatalf("valid body = %d %s", code, response)
	}
}

func TestStartRefusesModelsThatAShellCouldInterpret(t *testing.T) {
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}})
	id := create(t, f, "shell-model-create", "work")["id"].(string)
	if code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "shell-model-ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	path := "/v1/tasks/" + id + "/start"
	for i, model := range []string{"x&calc", "%PATH%", `a\"b`, "a b", "-m"} {
		body := []byte(`{"backend":"codex","model":"` + model + `","mode":"auto"}`)
		if code, response := f.request(t, "POST", path, fmt.Sprintf("shell-model-%d", i), "1", body); code != 400 || !bytes.Contains(response, []byte("bad_request")) {
			t.Fatalf("model %q = %d %s", model, code, response)
		}
	}
	for i, model := range []string{"claude-opus-5-5", "gpt-6.1-sol", "antigravity/claude-opus-5-5-high", "qwen3:32b", "claude-opus-5-5@20260601"} {
		id := create(t, f, fmt.Sprintf("plain-model-create-%d", i), "work")["id"].(string)
		if code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/ready", fmt.Sprintf("plain-model-ready-%d", i), "0", nil); code != 200 {
			t.Fatal(code)
		}
		body := []byte(`{"backend":"codex","model":"` + model + `","mode":"auto"}`)
		if code, response := f.request(t, "POST", "/v1/tasks/"+id+"/start", fmt.Sprintf("plain-model-start-%d", i), "1", body); code != 200 {
			t.Fatalf("model %q = %d %s", model, code, response)
		}
		// Withdrawn at once: without an executor each start would stay queued
		// and the fourth would meet the per-user queue limit instead.
		if code, response := f.request(t, "POST", "/v1/tasks/"+id+"/cancel", fmt.Sprintf("plain-model-withdraw-%d", i), "2", nil); code != 200 {
			t.Fatalf("withdraw %q = %d %s", model, code, response)
		}
	}
}

func TestStaleCoordinatorCannotCreateANewAttemptIntent(t *testing.T) {
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Generation: 7, Runner: noopRunner{}})
	id := create(t, f, "stale-create", "work")["id"].(string)
	if code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "stale-ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	code, body := f.request(t, "POST", "/v1/tasks/"+id+"/start", "stale-start", "1", []byte(`{"backend":"codex","mode":"auto"}`))
	if code != 503 || !bytes.Contains(body, []byte("engine_unavailable")) {
		t.Fatalf("start = %d %s", code, body)
	}
	task, err := f.s.Task(context.Background(), "alice", id)
	if err != nil || task.State != store.Ready {
		t.Fatalf("task = %+v: %v", task, err)
	}
}

func TestProjectMemberCanCancelAnotherMembersRunningTask(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.s.AddMember(ctx, "a", "bob", "member"); err != nil {
		t.Fatal(err)
	}
	id := create(t, f, "member-create", "work")["id"].(string)
	for i, edge := range [][2]store.State{{store.Draft, store.Ready}, {store.Ready, store.Preparing}, {store.Preparing, store.Running}} {
		if err := f.s.Transition(ctx, "alice", id, edge[0], edge[1], int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	h := New(f.s, f.hub, func(*http.Request) (string, error) { return "bob", nil })
	req := httptest.NewRequest("POST", "/v1/tasks/"+id+"/cancel", nil)
	req.Header.Set("Idempotency-Key", "bob-cancel")
	req.Header.Set("If-Match", "3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("cancel = %d %s", w.Code, w.Body.String())
	}
	task, err := f.s.Task(ctx, "bob", id)
	if err != nil || task.State != store.CancelRequested {
		t.Fatalf("task = %+v: %v", task, err)
	}
}
