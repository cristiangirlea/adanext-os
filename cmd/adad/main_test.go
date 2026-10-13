package main

import (
	"bytes"
	"context"
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

func TestHelperServesCoordinator(t *testing.T) {
	if os.Getenv("ADA_ADAD_HELPER") != "1" {
		return
	}
	os.Exit(run(context.Background(), []string{"-state-dir", os.Getenv("ADA_ADAD_STATE")}, os.Stdout, os.Stderr))
}

func startHelper(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperServesCoordinator$")
	cmd.Env = append(os.Environ(), "ADA_ADAD_HELPER=1", "ADA_ADAD_STATE="+dir)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cfg := ipc.Config{StateDir: dir, Name: "adad"}
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
	first := startHelper(t, dir)
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
	code := run(context.Background(), []string{"-state-dir", dir}, &out, &stderr)
	if code != 0 || !strings.Contains(stderr.String(), fmt.Sprintf("already running as PID %d", first.Process.Pid)) {
		t.Fatalf("second run: %d %q", code, stderr.String())
	}
	first.Process.Kill()
	first.Wait()
	second := startHelper(t, dir)
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
	if err := s.CreateAttempt(ctx, store.Attempt{ID: "attempt", Task: "task", Actor: "local", Backend: "codex", Model: "test", Effort: "low", Executor: "local", EngineSHA256: "hash", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan int, 1)
	var output bytes.Buffer
	go func() { done <- run(runCtx, []string{"-state-dir", dir}, &output, &output) }()
	cfg := ipc.Config{StateDir: dir, Name: "adad"}
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
			t.Fatalf("adad did not listen: %v %s", dialErr, output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return ipc.Dial(ctx, cfg) }}}
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
	<-streamDone
}
