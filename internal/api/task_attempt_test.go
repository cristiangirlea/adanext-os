package api

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/store"
)

func TestTaskReadsIncludeTheCurrentAttemptAndQueuePosition(t *testing.T) {
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}})
	draft := create(t, f, "draft", "draft")["id"].(string)
	id, attempt := startQueued(t, f, "queued")
	check := func(want any) {
		t.Helper()
		for _, path := range []string{"/v1/tasks/" + id, "/v1/tasks/" + draft, "/v1/tasks"} {
			code, body := f.request(t, "GET", path, "", "", nil)
			if code != 200 {
				t.Fatalf("%s: %d %s", path, code, body)
			}
			var tasks []map[string]any
			if path == "/v1/tasks" {
				if err := json.Unmarshal(body, &tasks); err != nil {
					t.Fatal(err)
				}
			} else {
				var task map[string]any
				if err := json.Unmarshal(body, &task); err != nil {
					t.Fatal(err)
				}
				tasks = append(tasks, task)
			}
			for _, task := range tasks {
				got, present := task["attempt"]
				if !present {
					t.Fatalf("attempt absent: %s", body)
				}
				if task["id"] == draft && got != nil {
					t.Fatalf("draft attempt: %v", got)
				}
				if task["id"] == id {
					g, _ := json.Marshal(got)
					w, _ := json.Marshal(want)
					if string(g) != string(w) {
						t.Fatalf("%s attempt = %s, want %s", path, g, w)
					}
				}
			}
		}
	}
	check(map[string]any{"id": attempt, "state": "pending", "queue_position": 1})
	// Latest means insertion order, including a terminal attempt, not lexicographic id or timestamp.
	ctx := context.Background()
	if err := f.s.CreateAttempt(ctx, store.Attempt{ID: "latest", Task: id, Actor: "alice", Backend: "codex", Mode: "auto", Executor: "local", State: "canceled"}); err != nil {
		t.Fatal(err)
	}
	check(map[string]any{"id": "latest", "state": "canceled"})
	if code, _ := do(t, asUser(f, "bob"), "GET", "/v1/tasks/"+id, "", "", nil); code != 404 {
		t.Fatal(code)
	}
}

func TestTaskAttemptsStayScopedToTheTaskAndCurrentMembership(t *testing.T) {
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}})
	ctx := context.Background()
	id, attempt := startQueued(t, f, "shared")
	if err := f.s.AddMember(ctx, "a", "bob", "member"); err != nil {
		t.Fatal(err)
	}
	bob := asUser(f, "bob")
	code, body := do(t, bob, "POST", "/v1/tasks", "foreign-create", "", []byte(`{"project":"b","objective":"foreign","criteria":"done"}`))
	if code != 201 {
		t.Fatalf("foreign create: %d %s", code, body)
	}
	var foreign struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &foreign); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateAttempt(ctx, store.Attempt{ID: "foreign-newer-attempt", Task: foreign.ID, Actor: "bob", Backend: "codex", Mode: "auto", Executor: "local", State: "canceled"}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"alice", "bob"} {
		for _, p := range []string{"/v1/tasks/" + id, "/v1/tasks"} {
			code, body := do(t, asUser(f, user), "GET", p, "", "", nil)
			if code != 200 {
				t.Fatalf("%s %s: %d %s", user, p, code, body)
			}
			var tasks []map[string]any
			if p == "/v1/tasks" {
				if err := json.Unmarshal(body, &tasks); err != nil {
					t.Fatal(err)
				}
			} else {
				var task map[string]any
				if err := json.Unmarshal(body, &task); err != nil {
					t.Fatal(err)
				}
				tasks = append(tasks, task)
			}
			found := false
			for _, task := range tasks {
				if task["id"] == id {
					found = true
					got, ok := task["attempt"].(map[string]any)
					if !ok || got["id"] != attempt {
						t.Fatalf("shared attempt: %s", body)
					}
				}
			}
			if !found {
				t.Fatalf("shared task missing: %s", body)
			}
			if user == "alice" && bytes.Contains(body, []byte("foreign-newer-attempt")) {
				t.Fatalf("foreign attempt leaked: %s", body)
			}
		}
	}
	if err := f.s.RemoveMember(ctx, "a", "bob"); err != nil {
		t.Fatal(err)
	}
	code, body = do(t, bob, "GET", "/v1/tasks/"+id, "", "", nil)
	if code != 404 || bytes.Contains(body, []byte(attempt)) {
		t.Fatalf("removed member: %d %s", code, body)
	}
	code, body = do(t, bob, "GET", "/v1/tasks", "", "", nil)
	if code != 200 || bytes.Contains(body, []byte(id)) || bytes.Contains(body, []byte(attempt)) {
		t.Fatalf("removed listing: %d %s", code, body)
	}
}
