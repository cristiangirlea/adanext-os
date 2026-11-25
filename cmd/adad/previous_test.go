package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/artifact"
)

var installSteps = []string{"cert-written", "key-written", "checked", "previous-written", "current-written"}

// stopAt makes l's next install stop at step, as if the process had died there.
func stopAt(l *webListener, step string) error {
	crash := errors.New("stopped at " + step)
	l.pairs.step = func(s string) error {
		if s == step {
			return crash
		}
		return nil
	}
	return crash
}

// restartServes restarts from dir and checks the served leaf.
func restartServes(t *testing.T, dir, address string, ca *testCA, want string) *webListener {
	t.Helper()
	restarted := openListener(t, dir, (&testClock{now: testNow}).Now)
	if served, err := handshake(t, address, ca); err != nil || served != want {
		t.Fatalf("after the restart: served %s, %v; want %s", served, err, want)
	}
	return restarted
}

// installEachStep runs setup for a fresh directory, stops an install at each step, restarts and
// checks the served pair: the old one until current is written, the new one after.
func installEachStep(t *testing.T, ca *testCA, setup func(t *testing.T, dir string) (address, old string)) {
	for _, step := range installSteps {
		t.Run(step, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "config")
			address, old := setup(t, dir)
			l := openListener(t, dir, (&testClock{now: testNow}).Now)
			cert, key := ca.valid(t)
			crash := stopAt(l, step)
			if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); !errors.Is(err, crash) {
				t.Fatalf("the install: %v", err)
			}
			l.Close()
			want := old
			if step == "current-written" {
				want = leafSHA256(t, cert)
			}
			restartServes(t, dir, address, ca, want)
		})
	}
}

// goodCurrent leaves dir with one installed pair named by current, and returns its address and leaf.
func goodCurrent(ca *testCA) func(t *testing.T, dir string) (string, string) {
	return func(t *testing.T, dir string) (string, string) {
		l := openListener(t, dir, (&testClock{now: testNow}).Now)
		address, sha := serving(t, l, ca)
		l.Close()
		return address, sha
	}
}

// fellBack leaves dir with current naming a missing pair and previous the good one.
func fellBack(ca *testCA) func(t *testing.T, dir string) (string, string) {
	return func(t *testing.T, dir string) (string, string) {
		address, sha := goodCurrent(ca)(t, dir)
		good := pointerOf(t, dir, "current")
		writePointer(t, dir, "current", strings.Repeat("cd", 16))
		writePointer(t, dir, "previous", good)
		return address, sha
	}
}

func TestWithAGoodCurrentPreviousBecomesTheOldPair(t *testing.T) {
	ca := newTestCA(t)
	installEachStep(t, ca, goodCurrent(ca))
	dir := filepath.Join(t.TempDir(), "config")
	address, old := goodCurrent(ca)(t, dir)
	oldID := pointerOf(t, dir, "current")
	l := restartServes(t, dir, address, ca, old)
	cert, key := ca.valid(t)
	if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err != nil {
		t.Fatal(err)
	}
	if got := pointerOf(t, dir, "previous"); got != oldID {
		t.Fatalf("previous = %s, want %s", got, oldID)
	}
}

func TestWhenTheLoadFellBackToPreviousItStaysThePreviousPair(t *testing.T) {
	ca := newTestCA(t)
	installEachStep(t, ca, fellBack(ca))
	dir := filepath.Join(t.TempDir(), "config")
	address, old := fellBack(ca)(t, dir)
	goodID := pointerOf(t, dir, "previous")
	l := restartServes(t, dir, address, ca, old)
	cert, key := ca.valid(t)
	if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if got := pointerOf(t, dir, "previous"); got != goodID {
		t.Fatalf("previous = %s, want the good pair %s", got, goodID)
	}
	restartServes(t, dir, address, ca, leafSHA256(t, cert))
}

func TestWhenNothingLoadedAnInstallLeavesPreviousAsItIs(t *testing.T) {
	ca := newTestCA(t)
	fresh := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	cert, key := ca.valid(t)
	if _, err := fresh.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(fresh.dir, "tls", "previous")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a first install wrote previous: %v", err)
	}
	for name, content := range map[string][]byte{"not an id": []byte("not an id\n"), "a missing pair": []byte(strings.Repeat("ef", 16) + "\n")} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "config")
			if err := artifact.PrivateDirectory(filepath.Join(dir, "tls")); err != nil {
				t.Fatal(err)
			}
			previous := filepath.Join(dir, "tls", "previous")
			if err := os.WriteFile(previous, content, 0o600); err != nil {
				t.Fatal(err)
			}
			l := openListener(t, dir, (&testClock{now: testNow}).Now)
			cert, key := ca.valid(t)
			if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(previous); err != nil || !bytes.Equal(got, content) {
				t.Fatalf("previous became %q, %v", got, err)
			}
		})
	}
}

func TestAnUnreadablePreviousDoesNotStopAnInstallWhileCurrentIsGood(t *testing.T) {
	ca := newTestCA(t)
	dir := filepath.Join(t.TempDir(), "config")
	address, _ := goodCurrent(ca)(t, dir)
	oldID := pointerOf(t, dir, "current")
	if err := os.Mkdir(filepath.Join(dir, "tls", "previous"), 0o700); err != nil {
		t.Fatal(err)
	}
	l := openListener(t, dir, (&testClock{now: testNow}).Now)
	cert, key := ca.valid(t)
	if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err != nil {
		t.Fatalf("the install: %v", err)
	}
	if got := pointerOf(t, dir, "previous"); got != oldID {
		t.Fatalf("previous = %s, want %s", got, oldID)
	}
	l.Close()
	restartServes(t, dir, address, ca, leafSHA256(t, cert))
}

func TestAFailedSyncOfTheAncestorsLeavesNoNewPairDirectory(t *testing.T) {
	ca := newTestCA(t)
	dir := filepath.Join(t.TempDir(), "config")
	l := openListener(t, dir, (&testClock{now: testNow}).Now)
	serving(t, l, ca)
	pairs := filepath.Join(dir, "tls", "pairs")
	before, err := os.ReadDir(pairs)
	if err != nil {
		t.Fatal(err)
	}
	saved := syncDir
	t.Cleanup(func() { syncDir = saved })
	syncDir = func(path string) error {
		if path == dir {
			return errors.New("injected sync failure")
		}
		return saved(path)
	}
	cert, key := ca.valid(t)
	if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err == nil {
		t.Fatal("the install went ahead")
	}
	if after, err := os.ReadDir(pairs); err != nil || len(after) != len(before) {
		t.Fatalf("%d pair directories before, %d after: %v", len(before), len(after), err)
	}
}
