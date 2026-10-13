package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/ipc"
	"github.com/cristiangirlea/ada-os/internal/store"
)

// The same test binary is the pinned fake hrn when started with its exec args.
func init() {
	if len(os.Args) < 4 || os.Args[1] != "exec" || os.Args[2] != "--protocol" || os.Args[3] != "v1" {
		return
	}
	line := func(seq int, kind string, other map[string]any) {
		m := map[string]any{"v": 1, "seq": seq, "type": kind, "time": time.Now().UTC().Format(time.RFC3339Nano)}
		for k, v := range other {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		fmt.Println(string(b))
	}
	line(1, "hello", map[string]any{"protocol": "v1", "backends": []any{map[string]any{"name": "codex", "available": true, "modes": []string{"auto"}, "options": []string{"mode"}, "approvals": false}}})
	var start struct {
		AttemptID string `json:"attempt_id"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&start); err != nil {
		os.Exit(2)
	}
	base := filepath.Dir(os.Getenv("HRN_HOME"))
	marker, _ := os.OpenFile(filepath.Join(base, "engine.marker"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if marker != nil {
		fmt.Fprintln(marker, os.Getpid())
		marker.Close()
	}
	line(2, "accepted", map[string]any{"attempt_id": start.AttemptID, "run_id": "block-run"})
	for {
		time.Sleep(time.Hour)
	}
}

func TestRunAcceptsEngineFlagsAndStopsWithItsContext(t *testing.T) {
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir, err := os.MkdirTemp("", "ada")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	var output bytes.Buffer
	name := testEndpointName(t)
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"-state-dir", dir, "-name", name, "-engine", os.Args[0], "-engine-sha256", hex.EncodeToString(hash[:])}, &output, &output)
	}()
	deadline := time.After(3 * time.Second)
	for {
		probe, c := context.WithTimeout(ctx, 50*time.Millisecond)
		conn, err := ipc.Dial(probe, ipc.Config{StateDir: dir, Name: name})
		c()
		if err == nil {
			conn.Close()
			break
		}
		select {
		case <-deadline:
			t.Fatalf("did not listen: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	code := <-done
	if code != 0 {
		t.Fatalf("run = %d: %s", code, output.String())
	}
}

func TestEngineFlagsRequirePairAndLowercaseSHA256(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"path only", []string{"-engine", "engine"}},
		{"hash only", []string{"-engine-sha256", strings.Repeat("a", 64)}},
		{"short hash", []string{"-engine", "engine", "-engine-sha256", "abc"}},
		{"uppercase hash", []string{"-engine", "engine", "-engine-sha256", strings.Repeat("A", 64)}},
		{"nonhex hash", []string{"-engine", "engine", "-engine-sha256", strings.Repeat("g", 64)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			args := append([]string{"-state-dir", t.TempDir()}, tc.args...)
			if code := run(context.Background(), args, &output, &output); code != 2 || !strings.Contains(output.String(), "engine") {
				t.Fatalf("run = %d: %s", code, output.String())
			}
		})
	}
}

func TestHelperServesCoordinator(t *testing.T) {
	if os.Getenv("ADA_ADAD_HELPER") != "1" {
		return
	}
	args := []string{"-state-dir", os.Getenv("ADA_ADAD_STATE"), "-name", os.Getenv("ADA_ADAD_NAME")}
	if path := os.Getenv("ADA_ADAD_ENGINE_PATH"); path != "" {
		args = append(args, "-engine", path, "-engine-sha256", os.Getenv("ADA_ADAD_ENGINE_HASH"))
	}
	os.Exit(run(context.Background(), args, os.Stdout, os.Stderr))
}

func testEndpointName(t *testing.T) string {
	t.Helper()
	var id [4]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("a%x", id)
}

func startHelper(t *testing.T, dir, name string, engine ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperServesCoordinator$")
	cmd.Env = append(os.Environ(), "ADA_ADAD_HELPER=1", "ADA_ADAD_STATE="+dir, "ADA_ADAD_NAME="+name)
	if len(engine) == 2 {
		cmd.Env = append(cmd.Env, "ADA_ADAD_ENGINE_PATH="+engine[0], "ADA_ADAD_ENGINE_HASH="+engine[1])
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cfg := ipc.Config{StateDir: dir, Name: name}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "adanext.db")); err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		conn, err := ipc.Dial(ctx, cfg)
		cancel()
		if err == nil {
			conn.Close()
			return cmd
		}
		time.Sleep(10 * time.Millisecond)
	}
	cmd.Process.Kill()
	cmd.Wait()
	t.Fatalf("helper did not listen: %s", output.String())
	return nil
}

func TestSecondCoordinatorReportsPIDAndStaleLockRecovers(t *testing.T) {
	dir := t.TempDir()
	name := testEndpointName(t)
	first := startHelper(t, dir, name)
	defer func() {
		if first.ProcessState == nil {
			first.Process.Kill()
			first.Wait()
		}
	}()
	if _, err := os.Stat(filepath.Join(dir, "adanext.db")); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), filepath.Join(dir, "adanext.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(context.Background(), "local", "test-local-user-exists", "probe", nil, func(tx *store.Tx) ([]byte, error) {
		var id string
		err := tx.QueryRowContext(context.Background(), "SELECT id FROM users WHERE id='local'").Scan(&id)
		return []byte(id), err
	})
	if err != nil {
		t.Fatalf("local user missing: %v", err)
	}
	s.Close()
	var out, stderr bytes.Buffer
	code := run(context.Background(), []string{"-state-dir", dir, "-name", name}, &out, &stderr)
	if code != 0 || !strings.Contains(stderr.String(), fmt.Sprintf("already running as PID %d", first.Process.Pid)) {
		t.Fatalf("second run: %d %q", code, stderr.String())
	}
	first.Process.Kill()
	first.Wait()
	second := startHelper(t, dir, name)
	second.Process.Kill()
	second.Wait()
}

func TestShutdownClosesOpenSSEStream(t *testing.T) {
	dir, err := os.MkdirTemp("", "ada")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(dir, "adanext.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, "local", "Local"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Do(ctx, "local", "project", "project", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "INSERT INTO projects(id,name,source_path,created) VALUES('p','Project','source','now')")
		return []byte("ok"), err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "p", "local", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, "local", "task", "p", "work", "done"); err != nil {
		t.Fatal(err)
	}
	for i, edge := range [][2]store.State{{store.Draft, store.Ready}, {store.Ready, store.Preparing}, {store.Preparing, store.Running}} {
		if err := s.Transition(ctx, "local", "task", edge[0], edge[1], int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateAttempt(ctx, store.Attempt{ID: "attempt", Task: "task", Actor: "local", Backend: "codex", Model: "test", Effort: "low", Executor: "local", EngineSHA256: "hash", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan int, 1)
	name := testEndpointName(t)
	var output bytes.Buffer
	go func() { done <- run(runCtx, []string{"-state-dir", dir, "-name", name}, &output, &output) }()
	cfg := ipc.Config{StateDir: dir, Name: name}
	deadline := time.Now().Add(2 * time.Second)
	for {
		probeCtx, probeCancel := context.WithTimeout(ctx, 100*time.Millisecond)
		conn, dialErr := ipc.Dial(probeCtx, cfg)
		probeCancel()
		if dialErr == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("adad did not listen: %v", dialErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return ipc.Dial(ctx, cfg) }}}
	t.Cleanup(client.CloseIdleConnections)
	request, err := http.NewRequest("GET", "http://ipc/v1/attempts/attempt/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "text/event-stream")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("stream: %d", response.StatusCode)
	}
	streamDone := make(chan struct{})
	go func() { io.Copy(io.Discard, response.Body); close(streamDone) }()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run returned %d: %s", code, output.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop within two seconds with an open SSE stream")
	}
	select {
	case <-streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE reader did not stop within two seconds")
	}
}

func TestCrashAndRestartRecoversTheRunningAttemptWithoutRelaunch(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "ada")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s, err := store.Open(ctx, filepath.Join(dir, "adanext.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateUser(ctx, "local", "Local"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(ctx, "local", "project", "project", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "INSERT INTO projects(id,name,source_path,created) VALUES('p','p','/never-use','now')")
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "p", "local", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, "local", "task", "p", "objective", "criteria"); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, "local", "task", store.Draft, store.Ready, 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	name := testEndpointName(t)
	first := startHelper(t, dir, name, os.Args[0], hex.EncodeToString(digest[:]))
	defer func() {
		if first.ProcessState == nil {
			first.Process.Kill()
			first.Wait()
		}
	}()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return ipc.Dial(ctx, ipc.Config{StateDir: dir, Name: name})
	}}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequest("POST", "http://ipc/v1/tasks/task/start", strings.NewReader(`{"backend":"codex","mode":"auto"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "start")
	req.Header.Set("If-Match", "1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	var attemptID string
	_, err = s.Do(ctx, "local", "read-attempt", "read-attempt", nil, func(tx *store.Tx) ([]byte, error) {
		err := tx.QueryRowContext(ctx, "SELECT id FROM attempts WHERE task='task'").Scan(&attemptID)
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "attempts", attemptID, "engine.marker")
	deadline := time.Now().Add(5 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		content, readErr := os.ReadFile(marker)
		if readErr == nil {
			fmt.Sscanf(string(content), "%d", &pid)
			task, _ := s.Task(ctx, "local", "task")
			if pid > 0 && task.State == store.Running {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("engine never started")
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatalf("kill coordinator: %v", err)
	}
	_ = first.Wait()
	engineProcess, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find engine: %v", err)
	}
	if err := engineProcess.Kill(); err != nil {
		t.Fatalf("kill engine: %v", err)
	}
	second := startHelper(t, dir, name, os.Args[0], hex.EncodeToString(digest[:]))
	defer func() { second.Process.Kill(); second.Wait() }()
	task, err := s.Task(ctx, "local", "task")
	if err != nil || task.State != store.RecoveryRequired {
		t.Fatalf("task = %+v: %v", task, err)
	}
	a, err := s.AttemptByID(ctx, attemptID)
	if err != nil || a.State != "recovery_required" || a.Ended == "" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	events, err := s.Events(ctx, "local", attemptID, 0)
	if err != nil || len(events) != 1 || events[0].Kind != "recovery" {
		t.Fatalf("events = %+v: %v", events, err)
	}
	content, err := os.ReadFile(marker)
	if err != nil || len(strings.Fields(string(content))) != 1 {
		t.Fatalf("launches = %q: %v", content, err)
	}
}

func TestPendingAttemptRecoversBeforeTheNewCoordinatorServes(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "ada")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s, err := store.Open(ctx, filepath.Join(dir, "adanext.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateUser(ctx, "local", "Local"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(ctx, "local", "project", "project", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "INSERT INTO projects(id,name,source_path,created) VALUES('p','p','/never-use','now')")
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "p", "local", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, "local", "task", "p", "objective", "criteria"); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, "local", "task", store.Draft, store.Ready, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, "local", "task", store.Ready, store.Preparing, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAttempt(ctx, store.Attempt{ID: "pending", Task: "task", Actor: "local", Backend: "codex", Mode: "auto", Executor: "local", OwnershipGeneration: 0, State: "pending"}); err != nil {
		t.Fatal(err)
	}
	name := testEndpointName(t)
	process := startHelper(t, dir, name)
	defer func() { process.Process.Kill(); process.Wait() }()
	task, err := s.Task(ctx, "local", "task")
	if err != nil || task.State != store.RecoveryRequired {
		t.Fatalf("task = %+v: %v", task, err)
	}
	a, err := s.AttemptByID(ctx, "pending")
	if err != nil || a.State != "recovery_required" {
		t.Fatalf("attempt = %+v: %v", a, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "attempts", "pending")); !os.IsNotExist(err) {
		t.Fatalf("pending attempt launched: %v", err)
	}
}

func TestCoordinatorWithoutEngineFlagsRefusesStartWithoutIntent(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "ada")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s, err := store.Open(ctx, filepath.Join(dir, "adanext.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateUser(ctx, "local", "Local"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(ctx, "local", "project", "project", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "INSERT INTO projects(id,name,source_path,created) VALUES('p','p','/never-use','now')")
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "p", "local", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, "local", "task", "p", "objective", "criteria"); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, "local", "task", store.Draft, store.Ready, 0); err != nil {
		t.Fatal(err)
	}
	name := testEndpointName(t)
	process := startHelper(t, dir, name)
	defer func() { process.Process.Kill(); process.Wait() }()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return ipc.Dial(ctx, ipc.Config{StateDir: dir, Name: name})
	}}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequest("POST", "http://ipc/v1/tasks/task/start", strings.NewReader(`{"backend":"codex","mode":"auto"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "start")
	req.Header.Set("If-Match", "1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	task, err := s.Task(ctx, "local", "task")
	if err != nil || task.State != store.Ready {
		t.Fatalf("task = %+v: %v", task, err)
	}
	_, err = s.Do(ctx, "local", "count", "count", nil, func(tx *store.Tx) ([]byte, error) {
		var n int
		err := tx.QueryRowContext(ctx, "SELECT count(*) FROM attempts WHERE task='task'").Scan(&n)
		if n != 0 {
			t.Errorf("attempts = %d", n)
		}
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
}
