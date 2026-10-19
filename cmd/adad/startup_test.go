package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/ipc"
)

// A detached coordinator's stderr is null: its start-up errors are kept in an
// owner-only log in the state directory, replaced on every start.
func TestStartUpErrorsGoToAnOwnerOnlyStartupLogReplacedPerStart(t *testing.T) {
	dir := ownerOnlyDir(t)
	log := filepath.Join(dir, ipc.StartupLogName)
	if err := os.Mkdir(filepath.Join(dir, "adanext.db"), 0700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := run(context.Background(), []string{"-state-dir", dir, "-name", testEndpointName(t)}, &output, &output); code != 1 {
		t.Fatalf("run = %d: %s", code, output.String())
	}
	content, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "adad: open store:") || !strings.Contains(output.String(), "adad: open store:") {
		t.Fatalf("start-up log %q, stderr %q", content, output.String())
	}
	if info, err := os.Stat(log); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatalf("start-up log is not owner-only: %v %v", info, err)
	}
	// The next start replaces it, with only its own errors.
	if err := os.Remove(filepath.Join(dir, "adanext.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "artifacts"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if code := run(context.Background(), []string{"-state-dir", dir, "-name", testEndpointName(t)}, &output, &output); code != 1 {
		t.Fatalf("run = %d: %s", code, output.String())
	}
	content, err = os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "open store") || !strings.Contains(string(content), "adad: open artifacts:") {
		t.Fatalf("second start-up log %q", content)
	}
}

func TestStartupLogKeepsAtMostItsCapAndNothingAfterClose(t *testing.T) {
	var file bytes.Buffer
	log := &startupLog{w: &file, left: startupLogLimit}
	line := []byte(strings.Repeat("x", 1000) + "\n")
	for range 100 {
		if n, err := log.Write(line); n != len(line) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if file.Len() != startupLogLimit {
		t.Fatalf("start-up log holds %d bytes, cap %d", file.Len(), startupLogLimit)
	}
	file.Reset()
	log = &startupLog{w: &file, left: startupLogLimit}
	log.close()
	log.Write(line)
	if file.Len() != 0 {
		t.Fatal("the start-up log was written after start-up")
	}
}

// A second start must leave the running coordinator's start-up log as it is: the path the
// desktop names keeps that coordinator's own start.
func TestAStartWhileAnotherCoordinatorRunsLeavesItsStartupLogAlone(t *testing.T) {
	cfg := ipc.Config{StateDir: ownerOnlyDir(t), Name: testEndpointName(t)}
	lock, err := ipc.Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	log := filepath.Join(cfg.StateDir, ipc.StartupLogName)
	if err := os.WriteFile(log, []byte("first start\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := run(context.Background(), []string{"-state-dir", cfg.StateDir, "-name", cfg.Name}, &output, &output); code != 0 || !strings.Contains(output.String(), "already running") {
		t.Fatalf("run = %d: %s", code, output.String())
	}
	if content, err := os.ReadFile(log); err != nil || string(content) != "first start\n" {
		t.Fatalf("a start replaced the running coordinator's start-up log with %q %v", content, err)
	}
	if code := failBeforeLock(cfg, &output, "adad: detach failed"); code != 1 {
		t.Fatalf("failBeforeLock = %d", code)
	}
	if content, err := os.ReadFile(log); err != nil || string(content) != "first start\n" {
		t.Fatalf("a failed detach replaced the running coordinator's start-up log with %q %v", content, err)
	}
}

// Without a running coordinator, a failure before the lock is still kept in the log.
func TestAFailureBeforeTheLockIsLoggedWhenNoCoordinatorRuns(t *testing.T) {
	cfg := ipc.Config{StateDir: ownerOnlyDir(t), Name: testEndpointName(t)}
	var output bytes.Buffer
	if code := failBeforeLock(cfg, &output, "adad: detach failed"); code != 1 {
		t.Fatalf("failBeforeLock = %d", code)
	}
	content, err := os.ReadFile(filepath.Join(cfg.StateDir, ipc.StartupLogName))
	if err != nil || string(content) != "adad: detach failed\n" || output.String() != "adad: detach failed\n" {
		t.Fatalf("start-up log %q %v, stderr %q", content, err, output.String())
	}
	if lock, err := ipc.Acquire(cfg); err != nil {
		t.Fatalf("the lock was kept: %v", err)
	} else {
		lock.Release()
	}
}
