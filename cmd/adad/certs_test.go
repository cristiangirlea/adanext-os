package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/api"
)

// symlinkOrSkip makes link name target; only Windows without the symlink privilege skips.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
			t.Skipf("creating a symlink needs a privilege this account lacks: %v", err)
		}
		t.Fatal(err)
	}
}

// pointerOf reads a pointer file's id.
func pointerOf(t *testing.T, dir, file string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "tls", file))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func writePointer(t *testing.T, dir, file, id string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "tls", file), []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAFailedSyncAfterTheCurrentPointerStillSwapsToTheNewPair(t *testing.T) {
	ca := newTestCA(t)
	dir := filepath.Join(t.TempDir(), "config")
	l := openListener(t, dir, (&testClock{now: testNow}).Now)
	address, _ := serving(t, l, ca)
	old := pointerOf(t, dir, "current")
	saved := syncDir
	t.Cleanup(func() { syncDir = saved })
	fired := false
	syncDir = func(path string) error {
		if path == filepath.Join(dir, "tls") && pointerOf(t, dir, "current") != old {
			fired = true
			return errors.New("injected sync failure")
		}
		return saved(path)
	}
	var logged bytes.Buffer
	l.pairs.log = &logged
	cert, key := ca.valid(t)
	status, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key))
	if err != nil || status.State != "serving" || status.Certificate.SHA256 != leafSHA256(t, cert) {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if !fired || !strings.Contains(logged.String(), "injected sync failure") {
		t.Fatalf("the fault fired %v, logged %q", fired, logged.String())
	}
	if served, err := handshake(t, address, ca); err != nil || served != leafSHA256(t, cert) {
		t.Fatalf("served %s, %v", served, err)
	}
}

func TestAnInstallRemovesOnlyStalePairDirectories(t *testing.T) {
	ca := newTestCA(t)
	dir := filepath.Join(t.TempDir(), "config")
	l := openListener(t, dir, (&testClock{now: testNow}).Now)
	serving(t, l, ca)
	pairs := filepath.Join(dir, "tls", "pairs")
	stale := filepath.Join(pairs, strings.Repeat("ef", 16))
	for _, d := range []string{stale, filepath.Join(pairs, "keep-me")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pairs, "notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, key := ca.valid(t)
	if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keep-me", "notes.txt"} {
		if _, err := os.Lstat(filepath.Join(pairs, name)); err != nil {
			t.Fatalf("%s was removed: %v", name, err)
		}
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stale pair stayed: %v", err)
	}
	// An unreadable pointer means no pair is known to be stale.
	current := filepath.Join(dir, "tls", "current")
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(current, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	l.pairs.removeStale()
	entries, err := os.ReadDir(pairs)
	if err != nil || len(entries) != 5 {
		t.Fatalf("%d entries left, %v", len(entries), err)
	}
}

func TestAnInstalledCertificateIsStillShownWhenItsKeyIsRefused(t *testing.T) {
	ca := newTestCA(t)
	dir := filepath.Join(t.TempDir(), "config")
	l := openListener(t, dir, (&testClock{now: testNow}).Now)
	_, sha := serving(t, l, ca)
	l.Close()
	loosen(t, filepath.Join(dir, "tls", "pairs", pointerOf(t, dir, "current"), "key.pem"))
	restarted := openListener(t, dir, (&testClock{now: testNow}).Now)
	if got := restarted.Status(); got.Reason != "certificate_invalid" || got.Certificate == nil || got.Certificate.SHA256 != sha {
		t.Fatalf("status = %+v", got)
	}
}

func TestAMalformedBlockBeforeAValidCertificateIsRefused(t *testing.T) {
	ca := newTestCA(t)
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	cert, key := ca.valid(t)
	malformed := append([]byte("-----BEGIN CERTIFICATE-----\n!!! not base64 !!!\n-----END CERTIFICATE-----\n"), cert...)
	var refusal *api.ListenerError
	if _, err := l.InstallCertificate(context.Background(), chosen(t, malformed), chosen(t, key)); !errors.As(err, &refusal) || refusal.Code != "certificate_invalid" {
		t.Fatalf("a malformed first block: %v", err)
	}
}
