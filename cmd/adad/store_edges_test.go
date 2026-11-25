package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func pairDirs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "tls", "pairs"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestAStartThatCannotMakeTheConfigurationDirectoryPrivateSaysSo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := newWebListener(dir, (&testClock{now: testNow}).Now, echoHandler, io.Discard)
	l.start()
	t.Cleanup(l.Close)
	if got := l.Status(); got.State != "off" || got.Reason != "certificate_missing" || !strings.Contains(got.Message, "configuration directory could not be made owner-only") {
		t.Fatalf("status = %+v", got)
	}
}

func TestAnEmptyDirectoryAtCurrentIsReplacedByAnInstall(t *testing.T) {
	ca := newTestCA(t)
	dir := filepath.Join(t.TempDir(), "config")
	address, _ := goodCurrent(ca)(t, dir)
	current := filepath.Join(dir, "tls", "current")
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(current, 0o700); err != nil {
		t.Fatal(err)
	}
	l := openListener(t, dir, (&testClock{now: testNow}).Now)
	cert, key := ca.valid(t)
	if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err != nil {
		t.Fatalf("the install: %v", err)
	}
	l.Close()
	restartServes(t, dir, address, ca, leafSHA256(t, cert))
}

// fellBackWithAGoodPrevious leaves current naming a second pair and previous the first, then
// returns the first's leaf, which a restart serves once current cannot be used.
func fellBackWithAGoodPrevious(t *testing.T, ca *testCA, dir string) (string, string) {
	t.Helper()
	l := openListener(t, dir, (&testClock{now: testNow}).Now)
	address, first := serving(t, l, ca)
	cert, key := ca.valid(t)
	if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err != nil {
		t.Fatal(err)
	}
	l.Close()
	return address, first
}

func TestADirectoryWithContentAtCurrentFailsTheInstallAndLeavesNoNewPair(t *testing.T) {
	ca := newTestCA(t)
	dir := filepath.Join(t.TempDir(), "config")
	address, first := fellBackWithAGoodPrevious(t, ca, dir)
	current := filepath.Join(dir, "tls", "current")
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(current, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := restartServes(t, dir, address, ca, first)
	before := pairDirs(t, dir)
	cert, key := ca.valid(t)
	if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err == nil {
		t.Fatal("the install went ahead")
	}
	if after := pairDirs(t, dir); !slices.Equal(before, after) {
		t.Fatalf("pair directories %v became %v", before, after)
	}
	l.Close()
	restartServes(t, dir, address, ca, first)
}

func TestAFailureAfterPreviousIsWrittenLeavesNoNewPairAndTheOldOneServed(t *testing.T) {
	ca := newTestCA(t)
	dir := filepath.Join(t.TempDir(), "config")
	address, old := goodCurrent(ca)(t, dir)
	l := restartServes(t, dir, address, ca, old)
	before := pairDirs(t, dir)
	tls := filepath.Join(dir, "tls")
	saved := syncDir
	t.Cleanup(func() { syncDir = saved })
	syncDir = func(path string) error {
		if _, err := os.Lstat(filepath.Join(tls, "previous")); path == tls && err == nil {
			return errors.New("injected sync failure")
		}
		return saved(path)
	}
	cert, key := ca.valid(t)
	if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err == nil {
		t.Fatal("the install went ahead")
	}
	syncDir = saved
	if after := pairDirs(t, dir); !slices.Equal(before, after) {
		t.Fatalf("pair directories %v became %v", before, after)
	}
	l.Close()
	restartServes(t, dir, address, ca, old)
}
