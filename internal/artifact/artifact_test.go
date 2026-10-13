package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestABlobRoundTripsAndTheSameContentStoresOneFile(t *testing.T) {
	s := testStore(t)
	data := []byte("durable evidence")
	first, err := s.Write(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Write(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil || first != second {
		t.Fatalf("refs: %+v %+v, %v", first, second, err)
	}
	if first.Hash != strings.ToLower(strings.TrimSpace(first.Hash)) || first.Size != int64(len(data)) {
		t.Fatalf("ref: %+v", first)
	}
	if countFiles(t, filepath.Join(s.dir, first.Hash[:2])) != 1 || countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
		t.Fatal("duplicate or temporary file remains")
	}
	r, err := s.Open(first.Hash)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(r)
	closeErr := r.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, data) {
		t.Fatalf("read: %q, %v, %v", got, readErr, closeErr)
	}
}

func TestABlobOverTheLimitLeavesNoTemporaryFile(t *testing.T) {
	s := testStore(t)
	_, err := s.Write(context.Background(), strings.NewReader("too large"), 3)
	if !errors.Is(err, ErrTooLarge) || countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
		t.Fatalf("limit: %v", err)
	}
}

func TestAChangedBlobIsCorruptAndInvalidOrUnknownHashesAreRefused(t *testing.T) {
	s := testStore(t)
	ref, err := s.Write(context.Background(), strings.NewReader("original"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, ref.Hash[:2], ref.Hash), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := s.Open(ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r)
	r.Close()
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("changed blob: %v", err)
	}
	for _, hash := range []string{"bad", strings.ToUpper(ref.Hash), "../" + ref.Hash} {
		if r, err := s.Open(hash); err == nil {
			r.Close()
			t.Fatalf("accepted %q", hash)
		}
	}
	unknown := strings.Repeat("0", 64)
	if r, err := s.Open(unknown); !errors.Is(err, os.ErrNotExist) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("unknown hash: %v", err)
	}
}

type failingFS struct {
	osFS
	fail string
}

type failingFile struct {
	file
	fail string
}

func (f failingFS) createTemp(dir, pattern string) (file, error) {
	created, err := f.osFS.createTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return failingFile{file: created, fail: f.fail}, nil
}

func (f failingFS) rename(old, new string) error {
	if f.fail == "rename" {
		return injectedDiskFull()
	}
	return f.osFS.rename(old, new)
}

func (f failingFile) Write(p []byte) (int, error) {
	if f.fail == "write" {
		return 0, injectedDiskFull()
	}
	return f.file.Write(p)
}

func (f failingFile) Sync() error {
	if f.fail == "sync" {
		return injectedDiskFull()
	}
	return f.file.Sync()
}

func (f failingFile) Close() error {
	err := f.file.Close()
	if f.fail == "close" {
		return errors.Join(err, injectedDiskFull())
	}
	return err
}

func TestDiskFullOnWriteSyncCloseOrRenameLeavesNoBlob(t *testing.T) {
	for _, point := range []string{"write", "sync", "close", "rename"} {
		t.Run(point, func(t *testing.T) {
			s := testStore(t)
			s.fs = failingFS{fail: point}
			data := []byte("evidence")
			_, err := s.Write(context.Background(), bytes.NewReader(data), int64(len(data)))
			if !errors.Is(err, ErrDiskFull) || !errors.Is(err, injectedDiskFull()) {
				t.Fatalf("disk full: %v", err)
			}
			if countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
				t.Fatal("temporary file remains")
			}
			hash := sha256.Sum256(data)
			path := filepath.Join(s.dir, hex.EncodeToString(hash[:1]), hex.EncodeToString(hash[:]))
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("final file: %v", statErr)
			}
		})
	}
}

func TestAnUnreferencedBlobIsReusedAfterAWriteOnlyCrash(t *testing.T) {
	s := testStore(t)
	data := []byte("orphaned until referenced")
	first, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if err != nil || first != second || countFiles(t, filepath.Join(s.dir, first.Hash[:2])) != 1 {
		t.Fatalf("reuse: %+v %+v %v", first, second, err)
	}
}
