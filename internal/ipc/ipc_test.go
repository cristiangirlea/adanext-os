package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestListenAndDialRoundTripBytes(t *testing.T) {
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "roundtrip"}
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
	lock, err := Acquire(Config{StateDir: ownerOnlyDir(t), Name: "released"})
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
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "absent"}
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
	if os.Getenv("ADANEXT_IPC_EMPTY_RECORD") == "1" {
		if err := os.Truncate(filepath.Join(cfg.StateDir, "adad.lock"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("ADANEXT_IPC_MALFORMED_RECORD") == "1" {
		if err := os.WriteFile(filepath.Join(cfg.StateDir, "adad.lock"), []byte("not-json"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("ADANEXT_IPC_STALE_RECORD") == "1" {
		if err := os.WriteFile(filepath.Join(cfg.StateDir, "adad.lock"), []byte(`{"pid":99999999,"created":"0"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("ADANEXT_IPC_REPLACE_RECORD") == "1" {
		path := filepath.Join(cfg.StateDir, "adad.lock")
		originalRecord, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		writeRecord := func(body []byte) {
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			n, err := f.WriteAt(body, 0)
			if err != nil {
				t.Fatal(err)
			}
			if n != len(body) {
				t.Fatal(io.ErrShortWrite)
			}
			if err := f.Truncate(int64(len(body))); err != nil {
				t.Fatal(err)
			}
		}
		writeRecord([]byte(`{"pid":99999999,"created":"0"}`))
		_, _ = os.Stdout.WriteString("fake\n")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil || line != "restore\n" {
			t.Fatalf("restore signal: %q %v", line, err)
		}
		writeRecord(originalRecord)
		_, _ = os.Stdout.WriteString("restored\n")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	_, _ = os.Stdout.WriteString("ready\n")
	if os.Getenv("ADANEXT_IPC_STALE_RECORD") == "1" {
		time.Sleep(300 * time.Millisecond)
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestAlreadyRunningErrorReportsTheHolderIdentityOrUnknown(t *testing.T) {
	known := (&AlreadyRunningError{PID: 42, Created: "start-42"}).Error()
	if !strings.Contains(known, "PID 42") || !strings.Contains(known, "start-42") {
		t.Fatalf("known holder: %q", known)
	}
	unknown := (&AlreadyRunningError{}).Error()
	if !strings.Contains(unknown, "unknown") || strings.Contains(unknown, "PID 0") {
		t.Fatalf("unknown holder: %q", unknown)
	}
}

func TestConfigRefusesANameLongerThan64Bytes(t *testing.T) {
	for _, name := range []string{strings.Repeat("a", 65), strings.Repeat("a", 64)} {
		err := (Config{StateDir: ownerOnlyDir(t), Name: name}).validate()
		if (err == nil) != (len(name) == 64) {
			t.Fatalf("name length %d: %v", len(name), err)
		}
	}
}

func TestAnUnreadableHolderRecordReportsAnUnknownHolder(t *testing.T) {
	for _, tc := range []struct{ name, env string }{
		{"empty", "ADANEXT_IPC_EMPTY_RECORD=1"},
		{"malformed", "ADANEXT_IPC_MALFORMED_RECORD=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{StateDir: ownerOnlyDir(t), Name: "helper"}
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsCoordinatorLock$")
			cmd.Env = append(os.Environ(), "ADANEXT_IPC_HELPER=1", tc.env, "ADANEXT_IPC_STATE="+cfg.StateDir)
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
				t.Fatalf("helper ready: %q %v", ready, err)
			}
			lock, err := Acquire(cfg)
			if lock != nil {
				lock.Release()
			}
			var running *AlreadyRunningError
			if !errors.As(err, &running) || running.PID != 0 || running.Created != "" || !strings.Contains(running.Error(), "unknown") {
				t.Fatalf("got %v, want unknown holder", err)
			}
		})
	}
}

func TestADeadCoordinatorLockIsRecoveredAndALiveOneReportsItsIdentity(t *testing.T) {
	cfg := Config{StateDir: ownerOnlyDir(t), Name: "helper"}
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
	created, err := processCreated(cmd.Process.Pid)
	if err != nil || running.Created != created {
		t.Fatalf("creation time = %q, want %q: %v", running.Created, created, err)
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
			cfg := Config{StateDir: ownerOnlyDir(t), Name: "stale"}
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

// ownerOnlyDir is a t.TempDir made 0700, as Ada makes its state directory: under umask 022
// testing makes it 0755, which Acquire refuses.
func ownerOnlyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}
