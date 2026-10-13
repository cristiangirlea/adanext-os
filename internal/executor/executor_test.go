package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	marker := os.Getenv("ADA_FAKE_MARKER")
	if marker != "" {
		f, _ := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		fmt.Fprintln(f, script)
		f.Close()
	}
	line := func(seq int, kind string, fields map[string]any) {
		m := map[string]any{"v": 1, "seq": seq, "type": kind, "time": time.Now().UTC().Format(time.RFC3339Nano)}
		for k, v := range fields {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		fmt.Println(string(b))
	}
	line(1, "hello", map[string]any{"protocol": "v1", "backends": []any{map[string]any{"name": "codex", "available": true, "modes": []string{"readonly", "auto", "yolo"}, "options": []string{"model", "mode", "effort"}, "approvals": false}}})
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
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
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
	if script == "slow_accept" {
		for {
			time.Sleep(time.Hour)
		}
	}
	base := map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run"}
	line(2, "accepted", base)
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
	final := "done"
	if script == "big" || script == "big_block" {
		final = strings.Repeat("x", 100*1024)
	}
	line(6, "result", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "result": map[string]any{"status": "ok", "final": final}})
	if script == "big_block" {
		for {
			time.Sleep(time.Hour)
		}
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
	e := New(Config{EnginePath: os.Args[0], EngineSHA256: want, EngineArgs: []string{"-test.run=^TestHelperFakeEngine$"}, StateDir: dir, Artifacts: a, Store: s, Hub: api.NewHub(), Generation: generation, CancelGrace: 50 * time.Millisecond, Env: []string{"ADA_FAKE_ENGINE=" + script, "ADA_FAKE_MARKER=" + marker}})
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
			if len(events) == 0 || events[len(events)-1].Kind != "error" {
				t.Fatalf("events = %+v", events)
			}
			cause := map[string]string{"crash": "exit status 3", "truncated": "truncated", "gap": "seq", "duplicate": "seq", "fatal": "fatal"}[script]
			if !strings.Contains(string(events[len(events)-1].Payload), cause) {
				t.Fatalf("error = %s", events[len(events)-1].Payload)
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
	events, _ := s.Events(context.Background(), "u", "attempt", 0)
	last := events[len(events)-1]
	if last.Kind != "error" || !strings.Contains(string(last.Payload), "disk is full") {
		t.Fatalf("last event = %+v", last)
	}
}

func TestCancelWaitsForEngineExitAndKillsAnIgnoringEngine(t *testing.T) {
	for _, script := range []string{"cancel", "ignore_cancel"} {
		t.Run(script, func(t *testing.T) {
			e, s, _, _ := testExecutor(t, script)
			e.LaunchAttempt("attempt")
			waitAttempt(t, s, "running")
			e.CancelTask("task")
			waitAttempt(t, s, "canceled")
			task, _ := s.Task(context.Background(), "u", "task")
			if task.State != store.Canceled {
				t.Fatalf("task state = %s", task.State)
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
	if err != nil || a.State != "interrupted" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	task, err := s.Task(context.Background(), "u", "task")
	if err != nil || task.State != store.Interrupted {
		t.Fatalf("task = %+v: %v", task, err)
	}
}
