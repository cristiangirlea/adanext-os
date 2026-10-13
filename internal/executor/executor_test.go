package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
		time.Sleep(time.Second)
		return
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
	if script == "slow_accept" {
		for {
			time.Sleep(time.Hour)
		}
	}
	base := map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run"}
	line(2, "accepted", base)
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
	final := "done"
	if script == "big" || script == "big_block" {
		final = strings.Repeat("x", 100*1024)
	}
	line(6, "result", map[string]any{"attempt_id": input.AttemptID, "run_id": "fake-run", "result": map[string]any{"status": "ok", "final": final}})
	if script == "after_result" {
		line(7, "event", map[string]any{"attempt_id": input.AttemptID, "event": map[string]any{"kind": "text"}})
	}
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
		if event.Kind == "error" && strings.Contains(string(event.Payload), cause) {
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
	for script, cause := range map[string]string{"bad_event_id": "attempt_id mismatch", "after_result": "line after result", "unknown_type": "unexpected protocol type", "missing_kind": "event missing kind"} {
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
		if event.Kind == "error" && strings.Contains(string(event.Payload), `"fatal":true`) {
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
			if !contains([]string{"PATH", "SystemRoot", "WINDIR", "ComSpec", "PATHEXT", "HRN_HOME", "ADA_FAKE_ENGINE", "ADA_FAKE_MARKER"}, name) {
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
	e, s, _, _ := testExecutor(t, "child_stdout")
	started := time.Now()
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("executor waited %s for descendant stdout", elapsed)
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
