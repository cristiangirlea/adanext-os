package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestListenAndDialRoundTripBytes(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "roundtrip"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	listener, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_, err = io.Copy(conn, conn)
		done <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ada")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ada" {
		t.Fatalf("got %q", buf)
	}
	conn.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestListenRequiresAHeldLock(t *testing.T) {
	var nilLock *Lock
	if listener, err := nilLock.Listen(); err == nil {
		listener.Close()
		t.Fatal("nil lock allowed Listen")
	}
	lock, err := Acquire(Config{StateDir: t.TempDir(), Name: "released"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if listener, err := lock.Listen(); err == nil {
		listener.Close()
		t.Fatal("released lock allowed Listen")
	}
}

func TestDialWithoutAListenerStopsAtTheContextDeadline(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "absent"}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	conn, err := Dial(ctx, cfg)
	if err == nil {
		conn.Close()
		t.Fatal("dial succeeded without a listener")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("dial exceeded bounded wait: %v", time.Since(start))
	}
}

func TestHelperHoldsCoordinatorLock(t *testing.T) {
	if os.Getenv("ADANEXT_IPC_HELPER") != "1" {
		return
	}
	cfg := Config{StateDir: os.Getenv("ADANEXT_IPC_STATE"), Name: "helper"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	_, _ = os.Stdout.WriteString("ready\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestADeadCoordinatorLockIsRecoveredAndALiveOneReportsItsPID(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "helper"}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsCoordinatorLock$")
	cmd.Env = append(os.Environ(), "ADANEXT_IPC_HELPER=1", "ADANEXT_IPC_STATE="+cfg.StateDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make([]byte, 6)
	if _, err := io.ReadFull(stdout, ready); err != nil || string(ready) != "ready\n" {
		t.Fatalf("helper did not acquire lock: %q, %v", ready, err)
	}
	lock, err := Acquire(cfg)
	if lock != nil {
		lock.Release()
	}
	var running *AlreadyRunningError
	if !errors.As(err, &running) || running.PID != cmd.Process.Pid {
		t.Fatalf("got %v, want AlreadyRunningError for PID %d", err, cmd.Process.Pid)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	lock, err = Acquire(cfg)
	if err != nil {
		t.Fatalf("stale lock was not recovered: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestALockWithReusedOrMissingPIDIsRecovered(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
	}{
		{"wrong creation time", os.Getpid()},
		{"missing PID", 99999999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{StateDir: t.TempDir(), Name: "stale"}
			body, err := json.Marshal(map[string]any{"pid": tc.pid, "created": "0"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.StateDir, "adad.lock"), body, 0600); err != nil {
				t.Fatal(err)
			}
			lock, err := Acquire(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Release()
			stored, err := os.ReadFile(filepath.Join(cfg.StateDir, "adad.lock"))
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				PID     int    `json:"pid"`
				Created string `json:"created"`
			}
			if err := json.Unmarshal(stored, &record); err != nil {
				t.Fatal(err)
			}
			if record.PID != os.Getpid() || record.Created == "0" {
				t.Fatalf("stale record remained: pid=%s created=%q", strconv.Itoa(record.PID), record.Created)
			}
		})
	}
}
