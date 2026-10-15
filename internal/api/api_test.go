package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/ipc"
	"github.com/cristiangirlea/ada-os/internal/store"
)

type fixture struct {
	s      *store.Store
	hub    *Hub
	client *http.Client
	cfg    ipc.Config
	server *http.Server
}

func newFixture(t *testing.T, execution ...Execution) *fixture {
	t.Helper()
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "ada")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := store.Open(ctx, filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateUser(ctx, "alice", "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, "bob", "Bob"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a", "b"} {
		_, err := s.Do(ctx, "alice", "project-"+p, p, nil, func(tx *store.Tx) ([]byte, error) {
			_, err := tx.ExecContext(ctx, "INSERT INTO projects(id,name,source_path,created) VALUES(?,?,?,?)", p, p, p, time.Now().UTC().Format(time.RFC3339Nano))
			return []byte("ok"), err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddMember(ctx, "a", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "b", "bob", "owner"); err != nil {
		t.Fatal(err)
	}
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	cfg := ipc.Config{StateDir: dir, Name: "a" + id[:8]}
	lock, err := ipc.Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Release() })
	l, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	hub.PollInterval = 10 * time.Millisecond
	hub.HeartbeatInterval = 50 * time.Millisecond
	server := &http.Server{Handler: New(s, hub, func(*http.Request) (string, error) { return "alice", nil }, execution...)}
	server.RegisterOnShutdown(hub.Close)
	go server.Serve(l)
	t.Cleanup(func() { server.Shutdown(context.Background()) })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return ipc.Dial(ctx, cfg) }}}
	t.Cleanup(client.CloseIdleConnections)
	return &fixture{s: s, hub: hub, client: client, cfg: cfg, server: server}
}

func (f *fixture) request(t *testing.T, method, path, key, match string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://ipc"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if match != "" {
		req.Header.Set("If-Match", match)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type: %s", resp.Header.Get("Content-Type"))
	}
	return resp.StatusCode, data
}

func create(t *testing.T, f *fixture, key, objective string) map[string]any {
	t.Helper()
	code, body := f.request(t, "POST", "/v1/tasks", key, "", []byte(fmt.Sprintf(`{"project":"a","objective":%q,"criteria":"done"}`, objective)))
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var task map[string]any
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestCreateGetAndListTasksHideUnknownAndForeignIDs(t *testing.T) {
	f := newFixture(t)
	task := create(t, f, "create", "work")
	id := task["id"].(string)
	if code, _ := f.request(t, "GET", "/v1/tasks/"+id, "", "", nil); code != 200 {
		t.Fatal(code)
	}
	code, list := f.request(t, "GET", "/v1/tasks", "", "", nil)
	if code != 200 || !bytes.Contains(list, []byte(id)) {
		t.Fatalf("list: %d %s", code, list)
	}
	if err := f.s.CreateTask(context.Background(), "bob", "foreign", "b", "other", "done"); err != nil {
		t.Fatal(err)
	}
	code, unknown := f.request(t, "GET", "/v1/tasks/missing", "", "", nil)
	code2, foreign := f.request(t, "GET", "/v1/tasks/foreign", "", "", nil)
	if code != 404 || code2 != 404 || !bytes.Equal(unknown, foreign) {
		t.Fatalf("404 differs: %d %s / %d %s", code, unknown, code2, foreign)
	}
}

func TestCreateKeysReplayAndConcurrentClientsMakeOneTaskPerKey(t *testing.T) {
	f := newFixture(t)
	first := create(t, f, "same", "work")
	again := create(t, f, "same", "work")
	if first["id"] != again["id"] {
		t.Fatal("replay changed ID")
	}
	code, _ := f.request(t, "POST", "/v1/tasks", "same", "", []byte(`{"project":"a","objective":"different","criteria":"done"}`))
	if code != 422 {
		t.Fatal(code)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	ids := make(chan string, 10)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, body := f.request(t, "POST", "/v1/tasks", fmt.Sprintf("unique-%d", i), "", []byte(`{"project":"a","objective":"work","criteria":"done"}`))
			if code != 201 {
				errs <- fmt.Errorf("%d %s", code, body)
			}
		}(i)
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := f.request(t, "POST", "/v1/tasks", "concurrent-same", "", []byte(`{"project":"a","objective":"work","criteria":"done"}`))
			var task map[string]any
			json.Unmarshal(body, &task)
			if code != 201 {
				errs <- fmt.Errorf("%d %s", code, body)
			} else {
				ids <- task["id"].(string)
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Error(err)
	}
	var shared string
	for id := range ids {
		if shared != "" && shared != id {
			t.Fatal("multiple tasks for one key")
		}
		shared = id
	}
	_, body := f.request(t, "GET", "/v1/tasks", "", "", nil)
	var tasks []any
	if err := json.Unmarshal(body, &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 22 {
		t.Fatalf("got %d tasks", len(tasks))
	}
}

func TestReadyStartReplayAndRefusedCancelPreserveRevision(t *testing.T) {
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}})
	id := create(t, f, "create", "work")["id"].(string)
	code, ready := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "ready", "0", nil)
	if code != 200 {
		t.Fatalf("ready %d %s", code, ready)
	}
	code, invalid := f.request(t, "POST", "/v1/tasks/"+id+"/cancel", "cancel", "1", nil)
	if code != 409 || !bytes.Contains(invalid, []byte("invalid_transition")) {
		t.Fatalf("cancel %d %s", code, invalid)
	}
	policy := []byte(`{"backend":"codex","mode":"auto"}`)
	code, started := f.request(t, "POST", "/v1/tasks/"+id+"/start", "start", "1", policy)
	if code != 200 {
		t.Fatalf("start %d %s", code, started)
	}
	code, replay := f.request(t, "POST", "/v1/tasks/"+id+"/start", "start", "1", policy)
	if code != 200 || !bytes.Equal(started, replay) {
		t.Fatal("start replay differed")
	}
	code, stale := f.request(t, "POST", "/v1/tasks/"+id+"/start", "new-start", "1", policy)
	if code != 409 || !bytes.Contains(stale, []byte("stale_revision")) {
		t.Fatalf("stale %d %s", code, stale)
	}
	// A queued start is withdrawn by its cancel (decisions log, 2026-10-14, #9).
	code, withdrawn := f.request(t, "POST", "/v1/tasks/"+id+"/cancel", "withdraw", "2", nil)
	if code != 200 || !bytes.Contains(withdrawn, []byte(`"state":"canceled"`)) || !bytes.Contains(withdrawn, []byte(`"revision":3`)) {
		t.Fatalf("withdraw %d %s", code, withdrawn)
	}
}

func TestMalformedAndOversizeRequestsAreRejected(t *testing.T) {
	f := newFixture(t)
	id := create(t, f, "create", "work")["id"].(string)
	for _, tc := range []struct {
		name, path, key, match string
		body                   []byte
		want                   int
	}{
		{"malformed", "/v1/tasks", "k", "", []byte("{"), 400},
		{"actor field", "/v1/tasks", "k", "", []byte(`{"project":"a","objective":"x","criteria":"y","actor":"alice"}`), 400},
		{"missing key", "/v1/tasks", "", "", []byte(`{"project":"a","objective":"x","criteria":"y"}`), 400},
		{"missing match", "/v1/tasks/" + id + "/ready", "k", "", nil, 400},
		{"too large", "/v1/tasks", "k", "", bytes.Repeat([]byte("x"), 2<<20), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := f.request(t, "POST", tc.path, tc.key, tc.match, tc.body)
			if code != tc.want {
				t.Fatalf("got %d want %d", code, tc.want)
			}
		})
	}
}

func TestRemovedMemberCannotReplayAnEarlierKey(t *testing.T) {
	f := newFixture(t)
	create(t, f, "key", "work")
	if err := f.s.RemoveMember(context.Background(), "a", "alice"); err != nil {
		t.Fatal(err)
	}
	code, body := f.request(t, "POST", "/v1/tasks", "key", "", []byte(`{"project":"a","objective":"work","criteria":"done"}`))
	if code != 404 {
		t.Fatalf("got %d %s", code, body)
	}
}

func TestEventsResumeAndDisconnectReleasesSubscriber(t *testing.T) {
	f := newFixture(t)
	id := create(t, f, "create", "work")["id"].(string)
	ctx := context.Background()
	if err := f.s.CreateAttempt(ctx, store.Attempt{ID: "attempt", Task: id, Actor: "alice", Backend: "codex", Model: "test", Effort: "low", Executor: "local", EngineSHA256: "hash", State: "running"}); err != nil {
		t.Fatal(err)
	}
	appendEvent := func(seq int64) {
		t.Helper()
		if err := f.s.AppendEvent(ctx, "alice", store.Event{Attempt: "attempt", Seq: seq, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "update", Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, seq))}); err != nil {
			t.Fatal(err)
		}
		f.hub.Notify("attempt")
	}
	appendEvent(1)
	appendEvent(2)
	code, body := f.request(t, "GET", "/v1/attempts/attempt/events?after=1", "", "", nil)
	if code != 200 || bytes.Contains(body, []byte(`"seq":1`)) || !bytes.Contains(body, []byte(`"seq":2`)) {
		t.Fatalf("events %d %s", code, body)
	}
	stream := func(last string) (*http.Response, *bufio.Reader, context.CancelFunc) {
		c, cancel := context.WithCancel(ctx)
		req, _ := http.NewRequestWithContext(c, "GET", "http://ipc/v1/attempts/attempt/events?after=0", nil)
		req.Header.Set("Accept", "text/event-stream")
		if last != "" {
			req.Header.Set("Last-Event-ID", last)
		}
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp, bufio.NewReader(resp.Body), cancel
	}
	readID := func(r *bufio.Reader) int {
		t.Helper()
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, "id: ") {
				n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "id: ")))
				if err != nil {
					t.Fatal(err)
				}
				return n
			}
		}
	}
	resp, r, cancel := stream("")
	if got := readID(r); got != 1 {
		t.Fatal(got)
	}
	if got := readID(r); got != 2 {
		t.Fatal(got)
	}
	appendEvent(3)
	if got := readID(r); got != 3 {
		t.Fatal(got)
	}
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(time.Second)
	for f.hub.SubscriberCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.hub.SubscriberCount() != 0 {
		t.Fatal("subscriber leaked")
	}
	appendEvent(4)
	resp, r, cancel = stream("3")
	if got := readID(r); got != 4 {
		t.Fatal(got)
	}
	cancel()
	resp.Body.Close()
}

func TestUnknownAndForeignAttemptsHaveIdenticalResponses(t *testing.T) {
	f := newFixture(t)
	if err := f.s.CreateTask(context.Background(), "bob", "foreign-task", "b", "other", "done"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateAttempt(context.Background(), store.Attempt{ID: "foreign-attempt", Task: "foreign-task", Actor: "bob", Backend: "codex", Model: "test", Effort: "low", Executor: "local", EngineSHA256: "hash", State: "running"}); err != nil {
		t.Fatal(err)
	}
	code, unknown := f.request(t, "GET", "/v1/attempts/missing/events", "", "", nil)
	otherCode, foreign := f.request(t, "GET", "/v1/attempts/foreign-attempt/events", "", "", nil)
	if code != 404 || otherCode != 404 || !bytes.Equal(unknown, foreign) {
		t.Fatalf("unknown %d %s, foreign %d %s", code, unknown, otherCode, foreign)
	}
}

func TestCreateForeignAndUnknownProjectsHaveIdenticalResponses(t *testing.T) {
	f := newFixture(t)
	request := func(project, key string) (int, []byte) {
		return f.request(t, "POST", "/v1/tasks", key, "", []byte(fmt.Sprintf(`{"project":%q,"objective":"work"}`, project)))
	}
	unknownCode, unknown := request("missing", "unknown")
	foreignCode, foreign := request("b", "foreign")
	if unknownCode != 404 || foreignCode != 404 || !bytes.Equal(unknown, foreign) {
		t.Fatalf("unknown %d %s, foreign %d %s", unknownCode, unknown, foreignCode, foreign)
	}
}

func TestStartForeignAndUnknownTasksHaveIdenticalResponses(t *testing.T) {
	f := newFixture(t)
	if err := f.s.CreateTask(context.Background(), "bob", "foreign", "b", "other", "done"); err != nil {
		t.Fatal(err)
	}
	policy := []byte(`{"backend":"codex","mode":"auto"}`)
	unknownCode, unknown := f.request(t, "POST", "/v1/tasks/missing/start", "unknown", "0", policy)
	foreignCode, foreign := f.request(t, "POST", "/v1/tasks/foreign/start", "foreign", "0", policy)
	if unknownCode != 404 || foreignCode != 404 || !bytes.Equal(unknown, foreign) {
		t.Fatalf("unknown %d %s, foreign %d %s", unknownCode, unknown, foreignCode, foreign)
	}
}

func TestCancelRunningTask(t *testing.T) {
	f := newFixture(t)
	id := create(t, f, "create", "work")["id"].(string)
	for _, edge := range []struct {
		from, to store.State
		revision int64
	}{
		{store.Draft, store.Ready, 0}, {store.Ready, store.Preparing, 1}, {store.Preparing, store.Running, 2},
	} {
		if err := f.s.Transition(context.Background(), "alice", id, edge.from, edge.to, edge.revision); err != nil {
			t.Fatal(err)
		}
	}
	code, body := f.request(t, "POST", "/v1/tasks/"+id+"/cancel", "cancel", "3", nil)
	if code != 200 || !bytes.Contains(body, []byte(`"state":"cancel_requested"`)) || !bytes.Contains(body, []byte(`"revision":4`)) {
		t.Fatalf("cancel: %d %s", code, body)
	}
}

func TestRouterErrorsAreJSON(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		method, path string
		status       int
		code         string
		allow        string
	}{
		{"GET", "/v1/unknown", 404, "not_found", ""},
		{"PUT", "/v1/tasks", 405, "method_not_allowed", "GET, HEAD, POST"},
		{"PUT", "/v1/tasks/example/ready", 405, "method_not_allowed", "POST"},
	} {
		req, err := http.NewRequest(tc.method, "http://ipc"+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != tc.status || resp.Header.Get("Content-Type") != "application/json" || !bytes.Contains(body, []byte(`"error":"`+tc.code+`"`)) || resp.Header.Get("Allow") != tc.allow {
			t.Fatalf("%s %s: %d Allow=%q %s", tc.method, tc.path, resp.StatusCode, resp.Header.Get("Allow"), body)
		}
	}
}

func TestEmptyLastEventIDUsesAfterQuery(t *testing.T) {
	f := newFixture(t)
	id := create(t, f, "create", "work")["id"].(string)
	if err := f.s.CreateAttempt(context.Background(), store.Attempt{ID: "attempt", Task: id, Actor: "alice", Backend: "codex", Model: "test", Effort: "low", Executor: "local", EngineSHA256: "hash", State: "running"}); err != nil {
		t.Fatal(err)
	}
	for seq := int64(1); seq <= 3; seq++ {
		if err := f.s.AppendEvent(context.Background(), "alice", store.Event{Attempt: "attempt", Seq: seq, Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest("GET", "http://ipc/v1/attempts/attempt/events?after=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header["Last-Event-Id"] = []string{""}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || bytes.Contains(body, []byte(`"seq":1`)) || bytes.Contains(body, []byte(`"seq":2`)) || !bytes.Contains(body, []byte(`"seq":3`)) {
		t.Fatalf("events: %d %s", resp.StatusCode, body)
	}
}

func TestIdempotencyKeyRejectsInvalidBytesAndLength(t *testing.T) {
	f := newFixture(t)
	if status, body := f.request(t, "POST", "/v1/tasks", strings.Repeat("a", 255), "", []byte(`{"project":"a","objective":"work"}`)); status != 201 {
		t.Fatalf("255-byte key: %d %s", status, body)
	}
	for _, key := range []string{strings.Repeat("a", 256), "with space", "with\tcontrol"} {
		status, body := f.request(t, "POST", "/v1/tasks", key, "", []byte(`{"project":"a","objective":"work"}`))
		if status != 400 || !bytes.Contains(body, []byte(`"error":"bad_request"`)) {
			t.Fatalf("key %q: %d %s", key, status, body)
		}
	}
}

func TestEventsStreamHeartbeat(t *testing.T) {
	f := newFixture(t)
	id := create(t, f, "create", "work")["id"].(string)
	if err := f.s.CreateAttempt(context.Background(), store.Attempt{ID: "attempt", Task: id, Actor: "alice", Backend: "codex", Model: "test", Effort: "low", Executor: "local", EngineSHA256: "hash", State: "running"}); err != nil {
		t.Fatal(err)
	}
	f.hub.HeartbeatInterval = 10 * time.Millisecond
	f.hub.PollInterval = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://ipc/v1/attempts/attempt/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("heartbeat: %v", err)
		}
		if line == ": ping\n" {
			break
		}
	}
}

func TestEventsStreamPollsWithoutNotify(t *testing.T) {
	f := newFixture(t)
	id := create(t, f, "create", "work")["id"].(string)
	if err := f.s.CreateAttempt(context.Background(), store.Attempt{ID: "attempt", Task: id, Actor: "alice", Backend: "codex", Model: "test", Effort: "low", Executor: "local", EngineSHA256: "hash", State: "running"}); err != nil {
		t.Fatal(err)
	}
	f.hub.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://ipc/v1/attempts/attempt/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	if err := f.s.AppendEvent(context.Background(), "alice", store.Event{Attempt: "attempt", Seq: 1, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		if line == "id: 1\n" {
			break
		}
	}
}

func TestHubCloseIsIdempotent(t *testing.T) {
	hub := NewHub()
	hub.Close()
	hub.Close()
	select {
	case <-hub.done:
	default:
		t.Fatal("hub remains open")
	}
}
