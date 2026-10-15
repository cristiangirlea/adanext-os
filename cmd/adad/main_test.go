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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/artifact"
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
	// A cancel on stdin ends the run as canceled; anything else, stdin's end
	// included, leaves the engine blocked for the crash tests.
	var next struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&next); err == nil && next.Type == "cancel" {
		line(3, "result", map[string]any{"attempt_id": start.AttemptID, "run_id": "block-run", "result": map[string]any{"status": "canceled"}})
		os.Exit(0)
	}
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

func TestCanceledStartupStopsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	code := run(ctx, []string{"-state-dir", t.TempDir(), "-name", testEndpointName(t)}, &output, &output)
	if code != 0 || !strings.Contains(output.String(), "adad: stopped during start-up") {
		t.Fatalf("%d %s", code, output.String())
	}
}

func TestRelativeXDGStateHomeIsIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG applies on Unix")
	}
	t.Setenv("XDG_STATE_HOME", "relative/state")
	if got := defaultStateDir(); strings.Contains(got, "relative") || !filepath.IsAbs(got) {
		t.Fatal(got)
	}
}

func TestEmptyLocalAppDataRequiresExplicitStateDir(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows state directory")
	}
	t.Setenv("LOCALAPPDATA", "")
	if dir := defaultStateDir(); dir != "" {
		t.Fatalf("default state directory: %q", dir)
	}
	var output bytes.Buffer
	if code := run(context.Background(), nil, &output, &output); code != 2 || !strings.Contains(output.String(), "adad: LOCALAPPDATA is not set; pass -state-dir") {
		t.Fatalf("%d %s", code, output.String())
	}
}

func TestDefaultStateDirectoryRejectsWorldWritableExistingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	var output bytes.Buffer
	code := run(context.Background(), []string{"-state-dir", dir, "-name", testEndpointName(t)}, &output, &output)
	if code == 0 || !strings.Contains(output.String(), "ipc: state directory") || !strings.Contains(output.String(), "must be a real directory owned by the user and not group- or other-writable") {
		t.Fatalf("%d %s", code, output.String())
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
	var holder struct {
		Created string `json:"created"`
	}
	content, err := os.ReadFile(filepath.Join(dir, "adad.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &holder); err != nil || holder.Created == "" {
		t.Fatalf("holder record = %q: %v", content, err)
	}
	if !strings.Contains(stderr.String(), "created "+holder.Created) {
		t.Fatalf("second run omitted creation time %q: %q", holder.Created, stderr.String())
	}
	first.Process.Kill()
	first.Wait()
	second := startHelper(t, dir, name)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return ipc.Dial(ctx, ipc.Config{StateDir: dir, Name: name})
	}}}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://ipc/v1/tasks")
	if err != nil {
		t.Fatalf("restarted coordinator did not serve /v1: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("restarted coordinator /v1: %d", resp.StatusCode)
	}
	second.Process.Kill()
	second.Wait()
}

func TestSecondCoordinatorReportsUnknownHolderForAnEmptyRecord(t *testing.T) {
	dir := t.TempDir()
	name := testEndpointName(t)
	first := startHelper(t, dir, name)
	defer func() { _ = first.Process.Kill(); _ = first.Wait() }()
	if err := os.Truncate(filepath.Join(dir, "adad.lock"), 0); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := run(context.Background(), []string{"-state-dir", dir, "-name", name}, &out, &stderr)
	if code != 0 || !strings.Contains(stderr.String(), "holder unknown") {
		t.Fatalf("second run: %d %q", code, stderr.String())
	}
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
	done := make(chan int, 1)
	runFinished := false
	defer func() {
		cancel()
		if !runFinished {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("run did not stop after test failure")
			}
		}
	}()
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
		runFinished = true
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
	engineIdentity, err := testProcessIdentity(pid)
	if err != nil || engineIdentity == "" {
		t.Fatalf("identify engine %d: %q: %v", pid, engineIdentity, err)
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatalf("kill coordinator: %v", err)
	}
	_ = first.Wait()
	current, err := testProcessIdentity(pid)
	if err != nil {
		t.Fatalf("check engine %d: %v", pid, err)
	}
	// Windows owns the engine through a kill-on-close Job Object. Unix has no
	// equivalent coordinator-death guarantee, so either exit or survival is valid there.
	if runtime.GOOS == "windows" && current == engineIdentity {
		t.Errorf("engine %d survived coordinator death", pid) // Continue to identity-checked cleanup below.
	}
	if current == engineIdentity {
		engineProcess, err := os.FindProcess(pid)
		if err == nil {
			err = engineProcess.Kill()
		}
		if err != nil {
			current, checkErr := testProcessIdentity(pid)
			if checkErr != nil || current == engineIdentity {
				t.Fatalf("kill engine %d: %v (identity %q, check: %v)", pid, err, current, checkErr)
			}
		}
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, err = testProcessIdentity(pid)
		if err != nil {
			t.Fatalf("check engine %d after kill: %v", pid, err)
		}
		if current != engineIdentity {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if current == engineIdentity {
		t.Fatalf("engine %d with creation time %s is still running", pid, engineIdentity)
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
	current, err = testProcessIdentity(pid)
	if err != nil || current == engineIdentity {
		t.Fatalf("engine after restart: identity %q, check: %v", current, err)
	}
}

func testProcessIdentity(pid int) (string, error) {
	if runtime.GOOS == "windows" {
		command := fmt.Sprintf("try { $p = [System.Diagnostics.Process]::GetProcessById(%d); if (-not $p.HasExited) { $p.StartTime.ToUniversalTime().Ticks } } catch { if ($_.Exception.InnerException -is [System.ArgumentException] -or $_.Exception.InnerException -is [System.InvalidOperationException]) { exit 0 }; throw }; exit 0", pid)
		output, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("query process %d: %w: %s", pid, err, output)
		}
		return strings.TrimSpace(string(output)), nil
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", fmt.Errorf("missing process stat name terminator")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("short process stat")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return "", nil
	}
	return fields[19], nil // field 22, starttime
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

func TestOwnerStopEndpointCancelsRunningAndQueuedWorkAndExits(t *testing.T) {
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
	for _, task := range []string{"running", "queued"} {
		if err := s.CreateTask(ctx, "local", task, "p", "objective", "criteria"); err != nil {
			t.Fatal(err)
		}
		if err := s.Transition(ctx, "local", task, store.Draft, store.Ready, 0); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	name := testEndpointName(t)
	process := startHelper(t, dir, name, os.Args[0], hex.EncodeToString(digest[:]))
	defer func() {
		if process.ProcessState == nil {
			process.Process.Kill()
			process.Wait()
		}
	}()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return ipc.Dial(ctx, ipc.Config{StateDir: dir, Name: name})
	}}}
	defer client.CloseIdleConnections()
	post := func(path, key, match string, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest("POST", "http://ipc"+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Idempotency-Key", key)
		if match != "" {
			req.Header.Set("If-Match", match)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, data
	}
	if code, body := post("/v1/tasks/running/start", "start-running", "1", `{"backend":"codex","mode":"auto"}`); code != 200 {
		t.Fatalf("start = %d %s", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, err := s.Task(ctx, "local", "running")
		if err != nil {
			t.Fatal(err)
		}
		if task.State == store.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task = %+v", task)
		}
		time.Sleep(10 * time.Millisecond)
	}
	code, body := post("/v1/tasks/queued/start", "start-queued", "1", `{"backend":"codex","mode":"auto"}`)
	if code != 200 {
		t.Fatalf("queued start = %d %s", code, body)
	}
	var queued struct {
		AttemptID string `json:"attempt_id"`
	}
	if err := json.Unmarshal(body, &queued); err != nil {
		t.Fatal(err)
	}
	// The queued attempt's actor has its stream open before the stop: the
	// reason is the last event it receives before the coordinator closes it.
	streamReq, err := http.NewRequest("GET", "http://ipc/v1/attempts/"+queued.AttemptID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	streamReq.Header.Set("Accept", "text/event-stream")
	stream, err := client.Do(streamReq)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	streamed := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(stream.Body)
		streamed <- data
	}()
	code, body = post("/v1/coordinator/stop", "quit", "", "")
	if code != 202 || !bytes.Contains(body, []byte(`"running":1`)) || !bytes.Contains(body, []byte(`"queued":1`)) {
		t.Fatalf("stop = %d %s", code, body)
	}
	exited := make(chan error, 1)
	go func() { exited <- process.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("coordinator exit: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("coordinator did not exit after the owner's stop")
	}
	select {
	case data := <-streamed:
		frames := strings.Split(strings.TrimSuffix(string(data), "\n\n"), "\n\n")
		if last := frames[len(frames)-1]; last != "id: 1\nevent: cancel\ndata: {\"reason\":\"coordinator stopped by the owner\"}" {
			t.Fatalf("last frame of the stream = %q in %q", last, data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end with the coordinator")
	}
	for _, task := range []string{"running", "queued"} {
		got, err := s.Task(ctx, "local", task)
		if err != nil || got.State != store.Canceled {
			t.Fatalf("task %s = %+v: %v", task, got, err)
		}
		var attemptID string
		_, err = s.Do(ctx, "local", "read-"+task, "read", nil, func(tx *store.Tx) ([]byte, error) {
			err := tx.QueryRowContext(ctx, "SELECT id FROM attempts WHERE task=? AND state='canceled'", task).Scan(&attemptID)
			return []byte("ok"), err
		})
		if err != nil {
			t.Fatalf("attempt of %s: %v", task, err)
		}
		events, err := s.Events(ctx, "local", attemptID, 0)
		if err != nil {
			t.Fatal(err)
		}
		reason := false
		for _, event := range events {
			if event.Kind == "cancel" && bytes.Contains(event.Payload, []byte(`"reason":"coordinator stopped by the owner"`)) {
				reason = true
			}
		}
		if !reason {
			t.Fatalf("events of %s = %+v", task, events)
		}
		if task == "queued" {
			if _, err := os.Stat(filepath.Join(dir, "attempts", attemptID)); !os.IsNotExist(err) {
				t.Fatalf("queued attempt launched: %v", err)
			}
		}
	}
}

// seedReferencedArtifact stores one blob in dir's artifact store and references
// it from an ended attempt of two projects, p owned by local and q by other.
func seedReferencedArtifact(t *testing.T, ctx context.Context, s *store.Store, dir string) (artifact.Ref, []byte) {
	t.Helper()
	artifacts, err := artifact.Open(filepath.Join(dir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte(`{"text":"` + strings.Repeat("y", 300<<10) + `"}`)
	ref, err := artifacts.Write(ctx, bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"local", "other"} {
		if err := s.CreateUser(ctx, user, user); err != nil {
			t.Fatal(err)
		}
	}
	for _, project := range []struct{ id, owner string }{{"p", "local"}, {"q", "other"}} {
		_, err = s.Do(ctx, project.owner, "project-"+project.id, "project", nil, func(tx *store.Tx) ([]byte, error) {
			_, err := tx.ExecContext(ctx, "INSERT INTO projects(id,name,source_path,created) VALUES(?,?,'/never-use','now')", project.id, project.id)
			return []byte("ok"), err
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddMember(ctx, project.id, project.owner, "owner"); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateTask(ctx, project.owner, "task-"+project.id, project.id, "objective", "criteria"); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateAttempt(ctx, store.Attempt{ID: "attempt-" + project.id, Task: "task-" + project.id, Actor: project.owner, Backend: "codex", Mode: "auto", Executor: "local", State: "ended_ok"}); err != nil {
			t.Fatal(err)
		}
		_, err = s.Do(ctx, project.owner, "reference-"+project.id, "reference", nil, func(tx *store.Tx) ([]byte, error) {
			return []byte("ok"), tx.ReferenceArtifact(ctx, ref.Hash, ref.Size, "attempt:attempt-"+project.id+":event:1")
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return ref, blob
}

func TestCoordinatorServesAReferencedArtifactToAMemberOnly(t *testing.T) {
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
	ref, blob := seedReferencedArtifact(t, ctx, s, dir)
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	name := testEndpointName(t)
	helper := startHelper(t, dir, name, os.Args[0], hex.EncodeToString(digest[:]))
	defer func() {
		helper.Process.Kill()
		helper.Wait()
	}()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return ipc.Dial(ctx, ipc.Config{StateDir: dir, Name: name})
	}}}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://ipc/v1/attempts/attempt-p/artifacts/" + ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, blob) {
		t.Fatalf("member: %d %.80s %v", resp.StatusCode, body, err)
	}
	resp, err = client.Get("http://ipc/v1/attempts/attempt-q/artifacts/" + ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || !bytes.Contains(body, []byte(`"error":"not_found"`)) {
		t.Fatalf("non-member: %d %s", resp.StatusCode, body)
	}
}

func TestCoordinatorWithoutEngineFlagsStillServesAReferencedArtifact(t *testing.T) {
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
	ref, blob := seedReferencedArtifact(t, ctx, s, dir)
	name := testEndpointName(t)
	helper := startHelper(t, dir, name)
	defer func() {
		helper.Process.Kill()
		helper.Wait()
	}()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return ipc.Dial(ctx, ipc.Config{StateDir: dir, Name: name})
	}}}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://ipc/v1/attempts/attempt-p/artifacts/" + ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, blob) {
		t.Fatalf("member without an engine: %d %.80s %v", resp.StatusCode, body, err)
	}
}
