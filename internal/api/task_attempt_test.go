package api

import (
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
