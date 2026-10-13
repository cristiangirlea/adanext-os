package main

import (
	"bytes"
	"context"
	"fmt"
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
