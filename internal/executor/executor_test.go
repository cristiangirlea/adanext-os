package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/api"
	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func TestHelperFakeEngine(t *testing.T) {
	script := os.Getenv("ADA_FAKE_ENGINE")
	if script == "" {
		return
	}
	if script == "grandchild" {
		identity, err := processIdentity(os.Getpid())
		if err != nil {
			os.Exit(3)
		}
		f, err := os.OpenFile(os.Getenv("ADA_FAKE_MARKER"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(3)
		}
		fmt.Fprintf(f, "child:%d:%s\n", os.Getpid(), identity)
		f.Close()
		time.Sleep(2 * time.Second)
		os.Exit(0)
	}
	if script == "grandchild_stream" {
		identity, _ := processIdentity(os.Getpid())
		f, _ := os.OpenFile(os.Getenv("ADA_FAKE_MARKER"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		fmt.Fprintf(f, "child:%d:%s\n", os.Getpid(), identity)
		f.Close()
		// Exceed the 1 MiB budget promptly even with coarse Windows sleep ticks.
		chunk := strings.Repeat("x", 256<<10)
		for {
			fmt.Print(chunk)
			time.Sleep(20 * time.Millisecond)
		}
	}
	if script == "grandchild_slow_stream" {
		identity, _ := processIdentity(os.Getpid())
		f, _ := os.OpenFile(os.Getenv("ADA_FAKE_MARKER"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		fmt.Fprintf(f, "child:%d:%s\n", os.Getpid(), identity)
		f.Close()
		// A line at a time, each well inside a grace period, so neither the byte
		// budget nor a single blocked read ends the attempt.
		interval, _ := time.ParseDuration(os.Getenv("ADA_FAKE_INTERVAL"))
		for {
			time.Sleep(interval)
			fmt.Println("x")
		}
	}
	if script == "grandchild_valid_stream" {
		identity, _ := processIdentity(os.Getpid())
		f, _ := os.OpenFile(os.Getenv("ADA_FAKE_MARKER"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		fmt.Fprintf(f, "child:%d:%s\n", os.Getpid(), identity)
		f.Close()
		// Well-formed, correctly sequenced events after the engine's own three,
		// faster than a slowed store persists them: reads never block and the
		// byte budget is far away.
		interval, _ := time.ParseDuration(os.Getenv("ADA_FAKE_INTERVAL"))
		for seq := 6; ; seq++ {
			time.Sleep(interval)
			b, _ := json.Marshal(map[string]any{"v": 1, "seq": seq, "type": "event", "attempt_id": os.Getenv("ADA_FAKE_ATTEMPT"), "run_id": "fake-run", "event": map[string]any{"kind": "text", "text": "late"}})
			fmt.Println(string(b))
		}
	}
	if script == "backend_group" {
		child := exec.Command(os.Args[0], "-test.run=^TestHelperFakeEngine$")
		child.Env = append(os.Environ(), "ADA_FAKE_ENGINE=grandchild_hold")
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		script = "grandchild_hold"
	}
	if script == "grandchild_hold" {
		identity, _ := processIdentity(os.Getpid())
		f, _ := os.OpenFile(os.Getenv("ADA_FAKE_MARKER"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		fmt.Fprintf(f, "child:%d:%s\n", os.Getpid(), identity)
		f.Close()
		for {
			time.Sleep(time.Hour)
		}
	}
	marker := os.Getenv("ADA_FAKE_MARKER")
	mark := func(value string) {
		if marker != "" {
			f, _ := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			fmt.Fprintln(f, value)
			f.Close()
		}
	}
	mark(script)
	if script == "exit_immediately" {
		os.Exit(23)
	}
	if script == "no_hello" {
		for {
			time.Sleep(time.Hour)
		}
	}
	var names []string
	for _, entry := range os.Environ() {
		names = append(names, strings.SplitN(entry, "=", 2)[0])
	}
	mark("env:" + strings.Join(names, ","))
	line := func(seq int, kind string, fields map[string]any) {
		m := map[string]any{"v": 1, "seq": seq, "type": kind, "time": time.Now().UTC().Format(time.RFC3339Nano)}
		for k, v := range fields {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		fmt.Println(string(b))
	}
	backend := map[string]any{"name": "codex", "available": true, "modes": []string{"readonly", "auto", "yolo"}, "options": []string{"model", "mode", "effort"}, "approvals": false}
	hello := map[string]any{"protocol": "v1", "backends": []any{backend}}
	switch script {
	case "bad_protocol":
		hello["protocol"] = "v2"
	case "backend_missing":
		backend["name"] = "other"
	case "backend_unavailable":
		backend["available"] = false
	case "mode_missing":
		backend["modes"] = []string{"readonly"}
	case "no_model":
		backend["options"] = []string{"mode", "effort"}
	case "no_effort":
		backend["options"] = []string{"mode", "model"}
	}
	line(1, "hello", hello)
	if script == "hello_no_read" || script == "hello_partial_read" {
		if script == "hello_partial_read" {
			_, _ = io.ReadFull(os.Stdin, make([]byte, 4<<10))
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	var input struct {
		Type      string `json:"type"`
		AttemptID string `json:"attempt_id"`
		Task      struct {
			Prompt  string `json:"prompt"`
			Dir     string `json:"dir"`
			Backend string `json:"backend"`
			Mode    string `json:"mode"`
		} `json:"task"`
	}
	var rawStart json.RawMessage
	if err := json.NewDecoder(os.Stdin).Decode(&rawStart); err != nil {
		os.Exit(2)
	}
	mark("start:" + string(rawStart))
	if err := json.Unmarshal(rawStart, &input); err != nil {
		os.Exit(2)
	}
	if input.Type != "start" || input.Task.Prompt != "objective\n\nAcceptance criteria:\ncriteria" || input.Task.Backend != "codex" || input.Task.Mode != "auto" || input.Task.Dir != filepath.Join(filepath.Dir(os.Getenv("HRN_HOME")), "work") || strings.Contains(input.Task.Dir, "never-use") {
		fmt.Fprintln(os.Stderr, "invalid scratch task")
		os.Exit(2)
	}
	if info, err := os.Stat(input.Task.Dir); err != nil || !info.IsDir() {
		fmt.Fprintln(os.Stderr, "scratch missing")
		os.Exit(2)
	}
	if script == "reject" {
		line(2, "rejected", map[string]any{"attempt_id": input.AttemptID, "error": map[string]any{"code": "unsupported"}})
		os.Exit(2)
	}
	if script == "slow_accept" || script == "no_start_answer" {
		for {
			time.Sleep(time.Hour)
		}
	}
	base := map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run"}
	line(2, "accepted", base)
	if script == "unknown_event_kind" {
		line(3, "event", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "event": map[string]any{"kind": "mystery"}})
		return
	}
	if strings.HasPrefix(script, "fatal_ok") {
		line(3, "error", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "fatal": true, "error": map[string]any{"code": "storage", "message": "full"}})
		line(4, "result", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "result": map[string]any{"status": "ok"}})
		if script == "fatal_ok_bad_tail_block" {
			fmt.Println("bad json")
		}
		if script != "fatal_ok" {
			for {
				time.Sleep(time.Hour)
			}
		}
		os.Exit(0)
	}
	if script == "nonfatal_error" {
		line(3, "error", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "fatal": false, "error": map[string]any{"code": "bad_request", "message": "ignored cancel"}})
		line(4, "result", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "result": map[string]any{"status": "ok"}})
		os.Exit(0)
	}
	if script == "engine_error_event" {
		line(3, "event", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "event": map[string]any{"kind": "error", "text": "backend warning"}})
		line(4, "result", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "result": map[string]any{"status": "ok"}})
		os.Exit(0)
	}
	if script == "bad_event_id" || script == "unknown_type" || script == "missing_kind" || script == "violation_eof" {
		kind := "event"
		id := input.AttemptID
		event := map[string]any{"kind": "text"}
		if script == "bad_event_id" || script == "violation_eof" {
			id = "wrong"
		}
		if script == "unknown_type" {
			kind = "mystery"
		}
		if script == "missing_kind" {
			event = map[string]any{"text": "bad"}
		}
		line(3, kind, map[string]any{"attempt_id": id, "run_id": "fake-run", "event": event})
		if script == "violation_eof" {
			_, _ = os.Stdin.Read(make([]byte, 1))
			mark("stdin closed")
		}
		return
	}
	if script == "block" || script == "cancel" || script == "ignore_cancel" {
		if script == "ignore_cancel" {
			for {
				time.Sleep(time.Hour)
			}
		}
		var cancel struct {
			Type string `json:"type"`
		}
		_ = json.NewDecoder(os.Stdin).Decode(&cancel)
		line(3, "result", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "result": map[string]any{"status": "canceled"}})
		os.Exit(0)
	}
	for seq, event := range []map[string]any{{"kind": "text", "text": "one"}, {"kind": "session", "text": "native"}, {"kind": "info", "text": "three"}} {
		line(seq+3, "event", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "event": event})
		if script == "crash" && seq == 1 {
			os.Exit(3)
		}
	}
	if script == "truncated" {
		fmt.Print(`{"v":1,"seq":6,"type":"result"`)
		os.Exit(0)
	}
	if script == "burst_before_exit" {
		// Well-formed events in one write, twice what a 4 KiB Windows pipe
		// holds: the reader's buffer is full of them when the exit is seen.
		var burst strings.Builder
		for seq := 6; seq < 116; seq++ {
			fmt.Fprintf(&burst, `{"v":1,"seq":%d,"type":"event","attempt_id":%q,"event":{"kind":"text"}}`+"\n", seq, input.AttemptID)
		}
		_, _ = io.WriteString(os.Stdout, burst.String())
		mark("exiting")
		os.Exit(0)
	}
	if script == "gap" {
		line(7, "result", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "result": map[string]any{"status": "ok"}})
		os.Exit(0)
	}
	if script == "duplicate" {
		line(5, "result", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "result": map[string]any{"status": "ok"}})
		os.Exit(0)
	}
	if script == "fatal" {
		line(6, "error", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "fatal": true, "error": map[string]any{"code": "storage", "message": "full"}})
		line(7, "result", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "result": map[string]any{"status": "failed"}})
		os.Exit(0)
	}
	if script == "stderr_cap" {
		fmt.Fprint(os.Stderr, strings.Repeat("x", 2<<20))
	}
	if script == "child_stdout" {
		child := exec.Command(os.Args[0], "-test.run=^TestHelperFakeEngine$")
		child.Env = append(os.Environ(), "ADA_FAKE_ENGINE=grandchild")
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
	}
	if script == "stream_after_exit" || script == "slow_stream_after_exit" || script == "valid_stream_after_exit" || script == "grandchild_after_result" || script == "nested_tree" {
		child := exec.Command(os.Args[0], "-test.run=^TestHelperFakeEngine$")
		childScript := "grandchild_hold"
		if script == "stream_after_exit" {
			childScript = "grandchild_stream"
		}
		if script == "slow_stream_after_exit" {
			childScript = "grandchild_slow_stream"
		}
		if script == "valid_stream_after_exit" {
			childScript = "grandchild_valid_stream"
		}
		if script == "nested_tree" {
			childScript = "backend_group"
		}
		child.Env = append(os.Environ(), "ADA_FAKE_ENGINE="+childScript, "ADA_FAKE_ATTEMPT="+input.AttemptID)
		child.Stdout = os.Stdout
		var nested *processSupervisor
		if script == "nested_tree" {
			var err error
			// Mirror Harness: a separate backend process group on Unix, and a
			// nested kill-on-close Job Object (without breakaway) on Windows.
			nested, err = newProcessSupervisor(child)
			if err != nil {
				os.Exit(3)
			}
			defer nested.close()
		}
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if nested != nil {
			if err := nested.started(child); err != nil {
				_ = child.Process.Kill()
				os.Exit(3)
			}
		}
		wantChildren := 1
		if script == "nested_tree" {
			wantChildren = 2
		}
		for i := 0; i < 2000; i++ {
			if data, err := os.ReadFile(marker); err == nil && strings.Count(string(data), "child:") >= wantChildren {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if script == "stream_after_exit" || script == "slow_stream_after_exit" || script == "valid_stream_after_exit" {
			os.Exit(0)
		}
	}
	if script == "exit_with_child_stdout" || script == "child_stderr" {
		child := exec.Command(os.Args[0], "-test.run=^TestHelperFakeEngine$")
		child.Env = append(os.Environ(), "ADA_FAKE_ENGINE=grandchild")
		if script == "child_stderr" {
			child.Stderr = os.Stderr
		} else {
			child.Stdout = os.Stdout
		}
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		// Wait until the child has written its identity before the parent exits.
		for i := 0; i < 100; i++ {
			if data, err := os.ReadFile(marker); err == nil && strings.Contains(string(data), "child:") {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if script == "exit_with_child_stdout" {
			os.Exit(0)
		}
	}
	if script == "malformed_burst" {
		fmt.Fprintln(os.Stdout, "not json")
		chunk := []byte(strings.Repeat("x", 64<<10))
		for i := 0; i < 128; i++ {
			_, _ = os.Stdout.Write(chunk)
		}
		return
	}
	if script == "unterminated_long_line" {
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 5<<20))
		return
	}
	if script == "slow_storage" {
		time.Sleep(50 * time.Millisecond)
	}
	final := "done"
	if script == "big" || script == "big_block" {
		final = strings.Repeat("x", 100*1024)
	}
	line(6, "result", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "result": map[string]any{"status": "ok", "final": final}})
	if script == "after_result" {
		line(7, "event", map[string]any{"attempt_id": input.AttemptID, "event": map[string]any{"kind": "text"}})
	}
	if script == "ok_partial_tail_hold" {
		// An unterminated line after the result, then exit while a descendant
		// keeps stdout open: the partial bytes only surface when stdout is closed.
		fmt.Print(`{"v":1,"seq":7,"type":"event"`)
		child := exec.Command(os.Args[0], "-test.run=^TestHelperFakeEngine$")
		child.Env = append(os.Environ(), "ADA_FAKE_ENGINE=grandchild_hold")
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		for i := 0; i < 2000; i++ {
			if data, err := os.ReadFile(marker); err == nil && strings.Contains(string(data), "child:") {
				break
			}
			time.Sleep(time.Millisecond)
		}
		os.Exit(0)
	}
	if script == "big_block" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if script == "grandchild_after_result" || script == "nested_tree" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if script == "delayed_exit" {
		time.Sleep(20 * time.Millisecond)
	}
	os.Exit(0)
}

func testExecutor(t *testing.T, script string) (*Executor, *store.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateUser(ctx, "u", "user"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(ctx, "u", "project", "project", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "INSERT INTO projects(id,name,source_path,created) VALUES('p','p','/never-use','now')")
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "p", "u", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, "u", "task", "p", "objective", "criteria"); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, "u", "task", store.Draft, store.Ready, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, "u", "task", store.Ready, store.Preparing, 1); err != nil {
		t.Fatal(err)
	}
	generation, _, err := s.BumpAndRecover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	want := hex.EncodeToString(hash[:])
	if err := s.CreateAttempt(ctx, store.Attempt{ID: "attempt", Task: "task", Actor: "u", Backend: "codex", Mode: "auto", Executor: "local", OwnershipGeneration: generation, EngineSHA256: want, State: "pending"}); err != nil {
		t.Fatal(err)
	}
	a, err := artifact.Open(filepath.Join(dir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "marker")
	e := New(Config{EnginePath: os.Args[0], EngineSHA256: want, EngineArgs: []string{"-test.run=^TestHelperFakeEngine$"}, StateDir: dir, Artifacts: a, Store: s, Hub: api.NewHub(), Generation: generation, CancelGrace: 200 * time.Millisecond, Env: []string{"ADA_FAKE_ENGINE=" + script, "ADA_FAKE_MARKER=" + marker, "GORACE=atexit_sleep_ms=0"}})
	return e, s, dir, marker
}

func waitAttempt(t *testing.T, s *store.Store, want string) store.Attempt {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		a, err := s.AttemptByID(context.Background(), "attempt")
		if err != nil {
			t.Fatal(err)
		}
		if a.State == want {
			return a
		}
		select {
		case <-deadline:
			events, _ := s.Events(context.Background(), "u", "attempt", 0)
			t.Fatalf("attempt state = %s, want %s; events=%+v", a.State, want, events)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestHappyEngineRunRecordsOrderedEventsAndVerifiesTask(t *testing.T) {
	e, s, _, _ := testExecutor(t, "happy")
	notifications := &notificationCounter{}
	e.cfg.Hub = notifications
	e.LaunchAttempt("attempt")
	a := waitAttempt(t, s, "ended_ok")
	if a.RunID != "fake-run" || a.NativeSession != "native" {
		t.Fatalf("attempt: %+v", a)
	}
	task, err := s.Task(context.Background(), "u", "task")
	if err != nil || task.State != store.Verifying {
		t.Fatalf("task: %+v %v", task, err)
	}
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil || len(events) != 4 {
		t.Fatalf("events: %+v %v", events, err)
	}
	for i, kind := range []string{"text", "session", "info", "result"} {
		if events[i].Kind != kind || events[i].Seq != int64(i+1) {
			t.Fatalf("event %d: %+v", i, events[i])
		}
	}
	if got := notifications.count.Load(); got != 4 {
		t.Fatalf("notifications = %d", got)
	}
}

func TestResultBeforeNaturalExitAlwaysEndsOK(t *testing.T) {
	for i := 0; i < 50; i++ {
		e, s, _, _ := testExecutor(t, "delayed_exit")
		e.cfg.CancelGrace = 200 * time.Millisecond
		e.LaunchAttempt("attempt")
		waitAttempt(t, s, "ended_ok")
	}
}

func TestResultBeforeNaturalExitWithSlowStorageEndsOK(t *testing.T) {
	e, s, _, _ := testExecutor(t, "slow_storage")
	e.cfg.CancelGrace = 100 * time.Millisecond
	e.afterAppend = func() { time.Sleep(3 * e.cfg.CancelGrace) }
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil || len(events) != 4 || events[len(events)-1].Kind != "result" {
		t.Fatalf("events = %+v: %v", events, err)
	}
}

type notificationCounter struct{ count atomic.Int32 }

func (n *notificationCounter) Notify(string) { n.count.Add(1) }

func TestRejectedStartFailsBeforeRunning(t *testing.T) {
	e, s, _, _ := testExecutor(t, "reject")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	task, _ := s.Task(context.Background(), "u", "task")
	if task.State != store.Failed {
		t.Fatalf("task state = %s", task.State)
	}
}

func TestWrongEngineHashFailsWithoutLaunching(t *testing.T) {
	e, s, _, marker := testExecutor(t, "happy")
	e.cfg.EngineSHA256 = strings.Repeat("0", 64)
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("engine launched: %v", err)
	}
}

func TestBrokenProtocolStreamsFailWithAnErrorEvent(t *testing.T) {
	for _, script := range []string{"crash", "truncated", "gap", "duplicate", "fatal"} {
		t.Run(script, func(t *testing.T) {
			e, s, _, _ := testExecutor(t, script)
			e.LaunchAttempt("attempt")
			waitAttempt(t, s, "ended_failed")
			task, _ := s.Task(context.Background(), "u", "task")
			if task.State != store.Failed {
				t.Fatalf("task state = %s", task.State)
			}
			events, _ := s.Events(context.Background(), "u", "attempt", 0)
			if len(events) == 0 {
				t.Fatalf("events = %+v", events)
			}
			cause := map[string]string{"crash": "exit status 3", "truncated": "truncated", "gap": "seq", "duplicate": "seq", "fatal": "fatal"}[script]
			if script == "fatal" {
				events, _ := s.Events(context.Background(), "u", "attempt", 0)
				found := false
				for _, event := range events {
					if event.Kind == "engine_notice" && strings.Contains(string(event.Payload), `"fatal":true`) {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing fatal engine notice: %+v", events)
				}
			} else {
				errorEventContains(t, s, cause)
			}
		})
	}
}

func TestLargeFinalIsStoredAsAReferencedArtifact(t *testing.T) {
	e, s, _, _ := testExecutor(t, "big")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	events, _ := s.Events(context.Background(), "u", "attempt", 0)
	last := events[len(events)-1]
	var payload map[string]any
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	result := payload["result"].(map[string]any)
	hash, ok := result["final_artifact"].(string)
	if !ok {
		t.Fatalf("result = %s", last.Payload)
	}
	_, err := s.Do(context.Background(), "u", "check-artifact", "check-artifact", nil, func(tx *store.Tx) ([]byte, error) {
		var size int64
		err := tx.QueryRowContext(context.Background(), "SELECT size FROM artifacts WHERE hash=? AND origin='attempt:attempt:final'", hash).Scan(&size)
		if size != 100*1024 {
			t.Errorf("artifact size = %d", size)
		}
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOversizeAndEscapedEventPayloadsBecomeReferencedArtifacts(t *testing.T) {
	e, s, _, _ := testExecutor(t, "happy")
	a, err := s.AttemptByID(context.Background(), "attempt")
	if err != nil {
		t.Fatal(err)
	}
	for i, payload := range [][]byte{
		[]byte(`{"text":"` + strings.Repeat("x", 256<<10) + `"}`),
		[]byte(`{"text":"` + strings.Repeat("\u2028", 45000) + `"}`),
	} {
		if i == 1 && len(payload) >= 256<<10 {
			t.Fatalf("escaping case raw length %d", len(payload))
		}
		if err := e.append(a, "event", payload, ""); err != nil {
			t.Fatal(err)
		}
		events, err := s.Events(context.Background(), "u", "attempt", 0)
		if err != nil {
			t.Fatal(err)
		}
		var ref struct {
			Hash string `json:"payload_artifact"`
			Size int    `json:"payload_size"`
		}
		if err := json.Unmarshal(events[i].Payload, &ref); err != nil {
			t.Fatal(err)
		}
		if len(ref.Hash) != 64 || ref.Size != len(payload) {
			t.Fatalf("event %d: artifact hash length %d, size %d", i+1, len(ref.Hash), ref.Size)
		}
		_, err = s.Do(context.Background(), "u", fmt.Sprintf("artifact-check-%d", i), "check", nil, func(tx *store.Tx) ([]byte, error) {
			var size int
			err := tx.QueryRowContext(context.Background(), "SELECT size FROM artifacts WHERE hash=? AND origin=?", ref.Hash, fmt.Sprintf("attempt:attempt:event:%d", i+1)).Scan(&size)
			if size != len(payload) {
				t.Errorf("size %d", size)
			}
			return []byte("ok"), err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

type noSpaceFile struct{ *os.File }

func (f noSpaceFile) Write([]byte) (int, error) { return 0, syscall.ENOSPC }

func TestArtifactFileSystemDiskFullFailsTaskAndStopsEngine(t *testing.T) {
	e, s, dir, _ := testExecutor(t, "big_block")
	a, err := artifact.OpenWithTempFileCreator(filepath.Join(dir, "fault-artifacts"), func(d, p string) (artifact.TempFile, error) {
		f, err := os.CreateTemp(d, p)
		if err != nil {
			return nil, err
		}
		return noSpaceFile{f}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.Artifacts = a
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	task, _ := s.Task(context.Background(), "u", "task")
	if task.State != store.Failed {
		t.Fatalf("task state = %s", task.State)
	}
	errorEventContains(t, s, "disk is full")
	errorEventContains(t, s, "engine was killed after the grace period")
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	var causeIndex, killIndex = -1, -1
	for i, event := range events {
		if strings.Contains(string(event.Payload), "disk is full") {
			causeIndex = i
		}
		if strings.Contains(string(event.Payload), "engine was killed after the grace period") {
			killIndex = i
		}
	}
	if causeIndex < 0 || killIndex <= causeIndex {
		t.Fatalf("cause and kill event order: %+v", events)
	}
}

func TestCancelWaitsForEngineExitAndKillsAnIgnoringEngine(t *testing.T) {
	for _, script := range []string{"cancel", "ignore_cancel"} {
		t.Run(script, func(t *testing.T) {
			e, s, _, _ := testExecutor(t, script)
			e.LaunchAttempt("attempt")
			waitAttempt(t, s, "running")
			e.CancelTask("task")
			want := "canceled"
			if script == "ignore_cancel" {
				want = "recovery_required"
			}
			waitAttempt(t, s, want)
			task, _ := s.Task(context.Background(), "u", "task")
			if string(task.State) != want {
				t.Fatalf("task state = %s", task.State)
			}
			if script == "ignore_cancel" {
				errorEventContains(t, s, "engine was killed after the grace period")
			} else {
				events, err := s.Events(context.Background(), "u", "attempt", 0)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Kind == "executor_error" {
						t.Fatalf("clean cancellation recorded error: %+v", event)
					}
				}
			}
		})
	}
}

func TestOrderlyStopCancelsARunningAttempt(t *testing.T) {
	e, s, _, _ := testExecutor(t, "cancel")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "running")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := s.AttemptByID(context.Background(), "attempt")
	if err != nil || a.State != "canceled" || a.Ended == "" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	task, err := s.Task(context.Background(), "u", "task")
	if err != nil || task.State != store.Canceled {
		t.Fatalf("task = %+v: %v", task, err)
	}
}

func TestStopBeforeAcceptanceInterruptsThePreparingTask(t *testing.T) {
	e, s, _, _ := testExecutor(t, "slow_accept")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "launching")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := s.AttemptByID(context.Background(), "attempt")
	if err != nil || a.State != "recovery_required" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	task, err := s.Task(context.Background(), "u", "task")
	if err != nil || task.State != store.RecoveryRequired {
		t.Fatalf("task = %+v: %v", task, err)
	}
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil || len(events) < 2 || events[len(events)-2].Kind != "recovery" {
		t.Fatalf("events = %+v: %v", events, err)
	}
}

func setAttemptOptions(t *testing.T, s *store.Store, model, effort string) {
	t.Helper()
	_, err := s.Do(context.Background(), "u", "set-options", "set-options", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(context.Background(), "UPDATE attempts SET model=?,effort=? WHERE id='attempt'", model, effort)
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func markerLines(t *testing.T, marker string) []string {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func errorEventContains(t *testing.T, s *store.Store, cause string) {
	t.Helper()
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "executor_error" && strings.Contains(string(event.Payload), cause) {
			return
		}
	}
	t.Fatalf("no error containing %q in %+v", cause, events)
}

func TestStartOptionsFollowHello(t *testing.T) {
	for _, tc := range []struct {
		script, model, effort, field string
		accepted                     bool
	}{
		{"happy", "", "", "", true},
		{"happy", "model-a", "high", "both", true},
		{"no_model", "model-a", "", "model", false},
		{"no_effort", "", "high", "effort", false},
	} {
		t.Run(tc.script+"_"+tc.field, func(t *testing.T) {
			e, s, _, marker := testExecutor(t, tc.script)
			setAttemptOptions(t, s, tc.model, tc.effort)
			e.LaunchAttempt("attempt")
			want := "ended_failed"
			if tc.accepted {
				want = "ended_ok"
			}
			waitAttempt(t, s, want)
			lines := markerLines(t, marker)
			var start string
			for _, line := range lines {
				if strings.HasPrefix(line, "start:") {
					start = strings.TrimPrefix(line, "start:")
				}
			}
			if tc.accepted {
				if start == "" {
					t.Fatal("start missing")
				}
				var request struct {
					Task map[string]any `json:"task"`
				}
				if err := json.Unmarshal([]byte(start), &request); err != nil {
					t.Fatal(err)
				}
				for key, value := range map[string]string{"model": tc.model, "effort": tc.effort} {
					got, present := request.Task[key]
					if present != (value != "") || (present && got != value) {
						t.Fatalf("%s = %v, present %v", key, got, present)
					}
				}
			} else {
				if start != "" {
					t.Fatalf("unexpected start: %s", start)
				}
				errorEventContains(t, s, tc.field+" unsupported")
			}
		})
	}
}

func TestInvalidHelloFailsBeforeStart(t *testing.T) {
	for _, script := range []string{"bad_protocol", "backend_missing", "backend_unavailable", "mode_missing"} {
		t.Run(script, func(t *testing.T) {
			e, s, _, marker := testExecutor(t, script)
			e.LaunchAttempt("attempt")
			waitAttempt(t, s, "ended_failed")
			for _, line := range markerLines(t, marker) {
				if strings.HasPrefix(line, "start:") {
					t.Fatalf("unexpected %s", line)
				}
			}
			cause := "backend codex"
			if script == "bad_protocol" {
				cause = "hello protocol"
			}
			errorEventContains(t, s, cause)
		})
	}
}

func TestProtocolViolationsRecordCause(t *testing.T) {
	for script, cause := range map[string]string{"bad_event_id": "attempt_id mismatch", "after_result": "line after result", "unknown_type": "unexpected protocol type", "missing_kind": "event kind"} {
		t.Run(script, func(t *testing.T) {
			e, s, _, _ := testExecutor(t, script)
			e.LaunchAttempt("attempt")
			waitAttempt(t, s, "ended_failed")
			errorEventContains(t, s, cause)
		})
	}
}

func TestProtocolViolationClosesStdinBeforeKilling(t *testing.T) {
	e, s, _, marker := testExecutor(t, "violation_eof")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	if !strings.Contains(strings.Join(markerLines(t, marker), "\n"), "stdin closed") {
		t.Fatal("engine did not observe stdin EOF")
	}
}

func TestFatalErrorAndResultAreBothStored(t *testing.T) {
	e, s, _, _ := testExecutor(t, "fatal")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	var fatal, result bool
	for _, event := range events {
		if event.Kind == "engine_notice" && strings.Contains(string(event.Payload), `"fatal":true`) {
			fatal = true
		}
		if event.Kind == "result" {
			result = true
		}
	}
	if !fatal || !result {
		t.Fatalf("fatal=%v result=%v events=%+v", fatal, result, events)
	}
}

func TestEngineEnvironmentIsAllowListed(t *testing.T) {
	t.Setenv("SECRET_TEST_VAR", "must-not-leak")
	e, s, _, marker := testExecutor(t, "happy")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	for _, line := range markerLines(t, marker) {
		if !strings.HasPrefix(line, "env:") {
			continue
		}
		for _, name := range strings.Split(strings.TrimPrefix(line, "env:"), ",") {
			if !contains([]string{"PATH", "SystemRoot", "WINDIR", "ComSpec", "PATHEXT", "HRN_HOME", "ADA_FAKE_ENGINE", "ADA_FAKE_MARKER", "GORACE"}, name) {
				t.Fatalf("unexpected environment variable %s", name)
			}
		}
	}
}

func TestStderrIsCappedAndPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits")
	}
	e, s, dir, _ := testExecutor(t, "stderr_cap")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	info, err := os.Stat(filepath.Join(dir, "attempts", "attempt", "engine.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 1<<20 || info.Mode().Perm() != 0600 {
		t.Fatalf("stderr size=%d mode=%o", info.Size(), info.Mode().Perm())
	}
}

func TestDescendantHoldingStdoutDoesNotHangWait(t *testing.T) {
	e, s, _, marker := testExecutor(t, "child_stdout")
	cleanupGrandchild(t, marker)
	started := time.Now()
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("executor waited %s for descendant stdout", elapsed)
	}
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), "engine was killed after the grace period") {
			t.Fatalf("exited parent marked killed: %+v", event)
		}
	}
}

func cleanupGrandchild(t *testing.T, marker string) {
	t.Helper()
	t.Cleanup(func() {
		data, err := os.ReadFile(marker)
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			var pid int
			var identity string
			if _, err := fmt.Sscanf(line, "child:%d:%s", &pid, &identity); err != nil || pid <= 0 {
				continue
			}
			current, err := processIdentity(pid)
			if err != nil || current != identity {
				continue
			}
			if child, err := os.FindProcess(pid); err == nil {
				_ = child.Kill()
			}
		}
	})
}

func waitGrandchildMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(marker); err == nil && strings.Contains(string(data), "child:") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("grandchild did not record its process identity")
}

func TestExitedEngineWithDescendantStdoutEndsPromptly(t *testing.T) {
	e, s, _, marker := testExecutor(t, "exit_with_child_stdout")
	cleanupGrandchild(t, marker)
	e.cfg.CancelGrace = 200 * time.Millisecond
	started := time.Now()
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	waitGrandchildMarker(t, marker)
	if elapsed := time.Since(started); elapsed > 3*e.cfg.CancelGrace+time.Second {
		t.Fatalf("attempt took %s", elapsed)
	}
	errorEventContains(t, s, "engine exited without result")
}

func TestStderrDescendantFitsStopBudget(t *testing.T) {
	e, s, _, marker := testExecutor(t, "child_stderr")
	cleanupGrandchild(t, marker)
	e.cfg.CancelGrace = 200 * time.Millisecond
	if got, want := e.StopBudget(), 5*e.cfg.CancelGrace+5*time.Second; got != want {
		t.Fatalf("stop budget = %s, want %s", got, want)
	}
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "running")
	waitGrandchildMarker(t, marker)
	ctx, cancel := context.WithTimeout(context.Background(), e.StopBudget())
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestExitedEngineWithDescendantStderrEndsOK(t *testing.T) {
	e, s, _, marker := testExecutor(t, "child_stderr")
	cleanupGrandchild(t, marker)
	e.cfg.CancelGrace = 100 * time.Millisecond
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "info" && strings.Contains(string(event.Payload), "a descendant kept stderr open after the engine exited") {
			return
		}
	}
	t.Fatalf("missing stderr descendant info event: %+v", events)
}

func TestRecordCauseReportsAppendFailure(t *testing.T) {
	e, s, _, _ := testExecutor(t, "happy")
	a, err := s.AttemptByID(context.Background(), "attempt")
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.Generation++
	if e.recordCause(a, fmt.Errorf("lost ownership")) {
		t.Fatal("reported a cause recorded under stale ownership")
	}
}

func TestMalformedLineDrainsBurstWithoutKill(t *testing.T) {
	e, s, _, _ := testExecutor(t, "malformed_burst")
	e.cfg.CancelGrace = 500 * time.Millisecond
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	errorEventContains(t, s, "malformed protocol line")
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), "killed after the grace") {
			t.Fatalf("engine was marked killed: %+v", events)
		}
	}
}

func TestUnterminatedOversizedLineFailsPromptly(t *testing.T) {
	e, s, _, _ := testExecutor(t, "unterminated_long_line")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	errorEventContains(t, s, "line too long")
}

func TestEOFRecordsOneMissingResultCause(t *testing.T) {
	e, s, _, _ := testExecutor(t, "crash")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "executor_error" && strings.Contains(string(event.Payload), "engine exited without result") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("missing result causes = %d: %+v", count, events)
	}
}

func TestConcurrentLaunchOfSameAttemptStartsOnce(t *testing.T) {
	e, s, _, marker := testExecutor(t, "happy")
	other := New(e.cfg)
	go e.LaunchAttempt("attempt")
	go other.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := other.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range markerLines(t, marker) {
		if line == "happy" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("engine starts = %d", count)
	}
}

func TestWindowsEngineWithoutExeSuffixIsRefused(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows executable naming")
	}
	e, s, dir, marker := testExecutor(t, "happy")
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "engine-without-suffix")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	e.cfg.EnginePath = path
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	errorEventContains(t, s, ".exe")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("engine launched: %v", err)
	}
}
