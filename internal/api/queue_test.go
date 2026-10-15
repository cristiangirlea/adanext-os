package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/store"
)

// recordingRunner notes which tasks the API asked the executor to cancel.
type recordingRunner struct {
	mu       sync.Mutex
	canceled []string
}

func (r *recordingRunner) LaunchAttempt(string) {}
func (r *recordingRunner) CancelTask(task string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canceled = append(r.canceled, task)
}
func (r *recordingRunner) cancels() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.canceled...)
}

type recordingStopper struct{ calls atomicCounter }

func (s *recordingStopper) StopWork() { s.calls.add() }

type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCounter) add()     { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *atomicCounter) get() int { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

const policy = `{"backend":"codex","mode":"auto"}`

// startQueued creates a task, readies it and starts it as alice: with no real
// executor its attempt stays pending, which is the queued state.
func startQueued(t *testing.T, f *fixture, name string) (string, string) {
	t.Helper()
	id := create(t, f, name+"-create", "work")["id"].(string)
	if code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/ready", name+"-ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	code, body := f.request(t, "POST", "/v1/tasks/"+id+"/start", name+"-start", "1", []byte(policy))
	if code != 200 {
		t.Fatalf("start %s = %d %s", name, code, body)
	}
	var started struct {
		AttemptID string `json:"attempt_id"`
	}
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatal(err)
	}
	return id, started.AttemptID
}

func asUser(f *fixture, user string, execution ...Execution) http.Handler {
	return New(f.s, f.hub, func(*http.Request) (string, error) { return user, nil }, execution...)
}

func do(t *testing.T, h http.Handler, method, path, key, match string, body []byte) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if match != "" {
		req.Header.Set("If-Match", match)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

func TestTheFourthQueuedStartIsRefusedAsQueueFull(t *testing.T) {
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}})
	var queued []string
	for i := 0; i < 3; i++ {
		id, _ := startQueued(t, f, fmt.Sprintf("queued-%d", i))
		queued = append(queued, id)
	}
	id := create(t, f, "fourth-create", "work")["id"].(string)
	if code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "fourth-ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	code, body := f.request(t, "POST", "/v1/tasks/"+id+"/start", "fourth-start", "1", []byte(policy))
	if code != 409 || !bytes.Contains(body, []byte(`"error":"queue_full"`)) {
		t.Fatalf("fourth start = %d %s", code, body)
	}
	task, err := f.s.Task(context.Background(), "alice", id)
	if err != nil || task.State != store.Ready || task.Revision != 1 {
		t.Fatalf("task = %+v: %v", task, err)
	}
	// A queued task withdrawn frees the place.
	if code, body := f.request(t, "POST", "/v1/tasks/"+queued[0]+"/cancel", "withdraw-one", "2", nil); code != 200 {
		t.Fatalf("withdraw = %d %s", code, body)
	}
	if code, body := f.request(t, "POST", "/v1/tasks/"+id+"/start", "fourth-start-again", "1", []byte(policy)); code != 200 {
		t.Fatalf("start after a withdrawal = %d %s", code, body)
	}
}

func TestGetReportsAQueuedTasksPositionAcrossUsersWhileItIsQueued(t *testing.T) {
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}})
	ctx := context.Background()
	first, firstAttempt := startQueued(t, f, "first")
	// Another user's start in another project queues behind it.
	if err := f.s.CreateTask(ctx, "bob", "bobs-task", "b", "objective", "criteria"); err != nil {
		t.Fatal(err)
	}
	for i, edge := range [][2]store.State{{store.Draft, store.Ready}, {store.Ready, store.Preparing}} {
		if err := f.s.Transition(ctx, "bob", "bobs-task", edge[0], edge[1], int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.CreateAttempt(ctx, store.Attempt{ID: "bobs-attempt", Task: "bobs-task", Actor: "bob", Backend: "codex", Mode: "auto", Executor: "local", State: "pending"}); err != nil {
		t.Fatal(err)
	}
	third, _ := startQueued(t, f, "third")
	position := func(h http.Handler, id string) (int, bool) {
		t.Helper()
		code, body := do(t, h, "GET", "/v1/tasks/"+id, "", "", nil)
		if code != 200 {
			t.Fatalf("get %s = %d %s", id, code, body)
		}
		var task map[string]any
		if err := json.Unmarshal(body, &task); err != nil {
			t.Fatal(err)
		}
		value, ok := task["queue_position"]
		if !ok {
			return 0, false
		}
		return int(value.(float64)), true
	}
	alice, bob := asUser(f, "alice"), asUser(f, "bob")
	if got, ok := position(alice, first); !ok || got != 1 {
		t.Fatalf("first = %d, %v", got, ok)
	}
	if got, ok := position(bob, "bobs-task"); !ok || got != 2 {
		t.Fatalf("bob's task = %d, %v", got, ok)
	}
	if got, ok := position(alice, third); !ok || got != 3 {
		t.Fatalf("third = %d, %v", got, ok)
	}
	// Once the head launches, it has no position and the others move up.
	_, err := f.s.Do(ctx, "alice", "launch-first", "launch-first", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "UPDATE attempts SET state='launching' WHERE id=?", firstAttempt)
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := position(alice, first); ok {
		t.Fatalf("launched task still has a position: %d", got)
	}
	if got, ok := position(alice, third); !ok || got != 2 {
		t.Fatalf("third after the head launched = %d, %v", got, ok)
	}
	draft := create(t, f, "draft-create", "work")["id"].(string)
	if got, ok := position(alice, draft); ok {
		t.Fatalf("draft task has a position: %d", got)
	}
}

func TestCancellingAQueuedTaskWithdrawsItWithoutAProcess(t *testing.T) {
	runner := &recordingRunner{}
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: runner})
	ctx := context.Background()
	id, attempt := startQueued(t, f, "withdraw")
	code, body := f.request(t, "POST", "/v1/tasks/"+id+"/cancel", "withdraw-cancel", "2", nil)
	if code != 200 || !bytes.Contains(body, []byte(`"state":"canceled"`)) || !bytes.Contains(body, []byte(`"revision":3`)) {
		t.Fatalf("cancel = %d %s", code, body)
	}
	code, replay := f.request(t, "POST", "/v1/tasks/"+id+"/cancel", "withdraw-cancel", "2", nil)
	if code != 200 || !bytes.Equal(body, replay) {
		t.Fatalf("replay = %d %s", code, replay)
	}
	a, err := f.s.AttemptByID(ctx, attempt)
	if err != nil || a.State != "canceled" || a.Ended == "" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	events, err := f.s.Events(ctx, "alice", attempt, 0)
	if err != nil || len(events) != 1 || events[0].Kind != "cancel" || !bytes.Contains(events[0].Payload, []byte(`"reason":"withdrawn by alice"`)) {
		t.Fatalf("events = %+v: %v", events, err)
	}
	if cancels := runner.cancels(); len(cancels) != 0 {
		t.Fatalf("executor asked to cancel %v for a task that never launched", cancels)
	}
	if code, body := f.request(t, "POST", "/v1/tasks/"+id+"/cancel", "cancel-again", "3", nil); code != 409 || !bytes.Contains(body, []byte("invalid_transition")) {
		t.Fatalf("cancel of a canceled task = %d %s", code, body)
	}
}

func TestCancelOfALaunchingAttemptIsHandedToTheExecutor(t *testing.T) {
	runner := &recordingRunner{}
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: runner})
	ctx := context.Background()
	id, attempt := startQueued(t, f, "launching")
	_, err := f.s.Do(ctx, "alice", "launch", "launch", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "UPDATE attempts SET state='launching' WHERE id=?", attempt)
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	code, body := f.request(t, "POST", "/v1/tasks/"+id+"/cancel", "launching-cancel", "2", nil)
	if code != 200 || !bytes.Contains(body, []byte(`"state":"preparing"`)) || !bytes.Contains(body, []byte(`"revision":2`)) {
		t.Fatalf("cancel = %d %s", code, body)
	}
	if cancels := runner.cancels(); len(cancels) != 1 || cancels[0] != id {
		t.Fatalf("executor cancels = %v", cancels)
	}
	a, err := f.s.AttemptByID(ctx, attempt)
	if err != nil || a.State != "launching" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	events, err := f.s.Events(ctx, "alice", attempt, 0)
	if err != nil || len(events) != 0 {
		t.Fatalf("a launching attempt was withdrawn: %+v, %v", events, err)
	}
}

func TestStopIsRefusedForEveryAccountButTheOwner(t *testing.T) {
	stopper := &recordingStopper{}
	execution := Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}, Owner: "local", Stopper: stopper}
	f := newFixture(t, execution)
	ctx := context.Background()
	if err := f.s.CreateUser(ctx, "local", "Local"); err != nil {
		t.Fatal(err)
	}
	// alice and bob each own a project; neither owns the coordinator.
	if code, body := f.request(t, "POST", "/v1/coordinator/stop", "alice-stop", "", nil); code != 403 || !bytes.Contains(body, []byte(`"error":"forbidden"`)) {
		t.Fatalf("alice = %d %s", code, body)
	}
	if code, body := do(t, asUser(f, "bob", execution), "POST", "/v1/coordinator/stop", "bob-stop", "", nil); code != 403 || !bytes.Contains(body, []byte(`"error":"forbidden"`)) {
		t.Fatalf("bob = %d %s", code, body)
	}
	if stopper.calls.get() != 0 {
		t.Fatal("a refused stop reached the coordinator")
	}
	owner := asUser(f, "local", execution)
	if code, body := do(t, owner, "POST", "/v1/coordinator/stop", "", "", nil); code != 400 {
		t.Fatalf("stop without a key = %d %s", code, body)
	}
	if code, body := do(t, owner, "POST", "/v1/coordinator/stop", "owner-stop-body", "", []byte(`{}`)); code != 400 {
		t.Fatalf("stop with a body = %d %s", code, body)
	}
	code, body := do(t, owner, "POST", "/v1/coordinator/stop", "owner-stop", "", nil)
	if code != 202 || !bytes.Contains(body, []byte(`"running":0`)) || !bytes.Contains(body, []byte(`"queued":0`)) {
		t.Fatalf("owner = %d %s", code, body)
	}
	if stopper.calls.get() != 1 {
		t.Fatalf("stopper calls = %d", stopper.calls.get())
	}
}

func TestTheOwnersStopRecordsTheReasonOnRunningAndQueuedTasks(t *testing.T) {
	stopper := &recordingStopper{}
	execution := Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}, Owner: "local", Stopper: stopper}
	f := newFixture(t, execution)
	ctx := context.Background()
	if err := f.s.CreateUser(ctx, "local", "Local"); err != nil {
		t.Fatal(err)
	}
	running, runningAttempt := startQueued(t, f, "running")
	if err := f.s.Transition(ctx, "alice", running, store.Preparing, store.Running, 2); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.Do(ctx, "alice", "run", "run", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "UPDATE attempts SET state='running' WHERE id=?", runningAttempt)
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	queued, queuedAttempt := startQueued(t, f, "queued")
	owner := asUser(f, "local", execution)
	code, body := do(t, owner, "POST", "/v1/coordinator/stop", "owner-stop", "", nil)
	if code != 202 || !bytes.Contains(body, []byte(`"running":1`)) || !bytes.Contains(body, []byte(`"queued":1`)) {
		t.Fatalf("stop = %d %s", code, body)
	}
	code, replay := do(t, owner, "POST", "/v1/coordinator/stop", "owner-stop", "", nil)
	if code != 202 || !bytes.Equal(body, replay) {
		t.Fatalf("replay = %d %s", code, replay)
	}
	if stopper.calls.get() == 0 {
		t.Fatal("the coordinator was not told to stop")
	}
	const reason = `"reason":"coordinator stopped by the owner"`
	// The running task waits for the executor's cancel path; its reason is on record.
	task, err := f.s.Task(ctx, "alice", running)
	if err != nil || task.State != store.Running {
		t.Fatalf("running task = %+v: %v", task, err)
	}
	events, err := f.s.Events(ctx, "alice", runningAttempt, 0)
	if err != nil || len(events) != 1 || events[0].Kind != "cancel" || !bytes.Contains(events[0].Payload, []byte(reason)) {
		t.Fatalf("running attempt events = %+v: %v", events, err)
	}
	task, err = f.s.Task(ctx, "alice", queued)
	if err != nil || task.State != store.Canceled {
		t.Fatalf("queued task = %+v: %v", task, err)
	}
	a, err := f.s.AttemptByID(ctx, queuedAttempt)
	if err != nil || a.State != "canceled" {
		t.Fatalf("queued attempt = %+v: %v", a, err)
	}
	events, err = f.s.Events(ctx, "alice", queuedAttempt, 0)
	if err != nil || len(events) != 1 || events[0].Kind != "cancel" || !bytes.Contains(events[0].Payload, []byte(reason)) {
		t.Fatalf("queued attempt events = %+v: %v", events, err)
	}
	// The actor sees the reason as the last event of an open stream.
	req, err := http.NewRequest("GET", "http://ipc/v1/attempts/"+queuedAttempt+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	streamCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := f.client.Do(req.WithContext(streamCtx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	var stream []byte
	for !bytes.Contains(stream, []byte(reason)) {
		n, err := resp.Body.Read(buf)
		stream = append(stream, buf[:n]...)
		if err != nil {
			t.Fatalf("stream ended without the reason: %s: %v", stream, err)
		}
	}
	if !bytes.Contains(stream, []byte("event: cancel\n")) {
		t.Fatalf("stream = %s", stream)
	}
}

func TestStopWithoutACoordinatorToStopIsUnavailable(t *testing.T) {
	f := newFixture(t, Execution{Owner: "local"})
	if err := f.s.CreateUser(context.Background(), "local", "Local"); err != nil {
		t.Fatal(err)
	}
	if code, body := do(t, asUser(f, "local", Execution{Owner: "local"}), "POST", "/v1/coordinator/stop", "stop", "", nil); code != 503 {
		t.Fatalf("stop = %d %s", code, body)
	}
}
