//go:build fakeengine

package main

import (
	"crypto/tls"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCertWritesAnOwnerOnlyPairAndRefusesToOverwriteIt(t *testing.T) {
	// An existing, permissive directory: the pair must make it owner-only, not only create it.
	dir := filepath.Join(t.TempDir(), "certs")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cert(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")); err != nil {
		t.Fatalf("the pair does not load: %v", err)
	}
	if runtime.GOOS != "windows" {
		for name, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "cert.pem"): 0o600, filepath.Join(dir, "key.pem"): 0o600} {
			info, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != want {
				t.Errorf("%s has mode %o, want %o", name, got, want)
			}
		}
	}
	before, err := os.ReadFile(filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cert(dir); err == nil {
		t.Fatal("a second cert into the same directory overwrote the pair")
	}
	after, err := os.ReadFile(filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("the key changed")
	}
}

func TestCertLeavesNoPartialPairWhenAFileExists(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(key, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cert(dir); err == nil {
		t.Fatal("cert wrote over an existing key.pem")
	}
	if _, err := os.Stat(filepath.Join(dir, "cert.pem")); !os.IsNotExist(err) {
		t.Fatalf("cert.pem was left behind: %v", err)
	}
	if data, err := os.ReadFile(key); err != nil || string(data) != "not ours" {
		t.Fatalf("the existing key.pem changed: %q, %v", data, err)
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := cert(dir); err != nil {
		t.Fatalf("a retry after the existing file is gone failed: %v", err)
	}
}

func TestCertNamesAFileItCouldNotRemoveBesideTheFirstError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	removeFile = func(string) error { return errors.New("removal refused") }
	t.Cleanup(func() { removeFile = os.Remove })
	err := cert(dir)
	if err == nil {
		t.Fatal("cert wrote over an existing key.pem")
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("the original error is lost: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "removal refused") || !strings.Contains(msg, filepath.Join(dir, "cert.pem")) {
		t.Errorf("the removal's failure is not named with its path: %v", err)
	}
}
