package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
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
		if r, err := s.Open(hash); !errors.Is(err, ErrInvalidHash) {
			if r != nil {
				r.Close()
			}
			t.Fatalf("invalid hash %q: %v", hash, err)
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

func TestAChangedBlobWithTheRightSizeIsReplacedOnTheNextWrite(t *testing.T) {
	s := testStore(t)
	data := []byte("original")
	ref, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, ref.Hash[:2], ref.Hash)
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if err != nil || got != ref {
		t.Fatalf("repair: %+v %v", got, err)
	}
	repaired, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(repaired, data) {
		t.Fatalf("repaired content: %q %v", repaired, err)
	}
}

func TestAChangedBlobWithTheWrongSizeIsReplacedOnTheNextWrite(t *testing.T) {
	s := testStore(t)
	data := []byte("original")
	ref, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, ref.Hash[:2], ref.Hash)
	if err := os.WriteFile(path, []byte("wrong size"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if err != nil || got != ref {
		t.Fatalf("repair: %+v %v", got, err)
	}
	repaired, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(repaired, data) {
		t.Fatalf("repaired content: %q %v", repaired, err)
	}
}

func TestOpenRemovesOnlyStaleArtifactTemporaryFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(tmp, "artifact-old")
	fresh := filepath.Join(tmp, "artifact-fresh")
	unrelated := filepath.Join(tmp, "other-old")
	for _, path := range []string{old, fresh, unrelated} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ago := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{old, unrelated} {
		if err := os.Chtimes(path, ago, ago); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale temporary file: %v", err)
	}
	for _, path := range []string{fresh, unrelated} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("retained file %s: %v", path, err)
		}
	}
}

func TestOpenContinuesWhenAStaleTempCannotBeRemoved(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(tmp, "artifact-stale")
	otherStale := filepath.Join(tmp, "artifact-other-stale")
	for _, path := range []string{stale, otherStale} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ago := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{stale, otherStale} {
		if err := os.Chtimes(path, ago, ago); err != nil {
			t.Fatal(err)
		}
	}
	removeErr := errors.New("file held open")
	called := false
	_, err := openWithStaleTempRemover(dir, func(path string) error {
		if path == otherStale {
			return os.Remove(path)
		}
		if path != stale {
			t.Fatalf("removed unexpected path %s", path)
		}
		called = true
		return removeErr
	})
	if err != nil || !called {
		t.Fatalf("open with failed sweep: %v, remove called: %v", err, called)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("stale file should remain: %v", err)
	}
	if _, err := os.Stat(otherStale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("other stale file should be removed: %v", err)
	}
}

type renameRaceFS struct{ osFS }

func (f renameRaceFS) rename(old, new string) error {
	data, err := os.ReadFile(old)
	if err != nil {
		return err
	}
	if err := os.WriteFile(new, data, 0o600); err != nil {
		return err
	}
	return os.ErrPermission
}

func TestARenameFailureReusesAConcurrentMatchingBlob(t *testing.T) {
	s := testStore(t)
	s.fs = renameRaceFS{}
	data := []byte("same content")
	ref, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if err != nil || ref.Size != int64(len(data)) {
		t.Fatalf("concurrent reuse: %+v %v", ref, err)
	}
	if countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
		t.Fatal("temporary file remains")
	}
}

type corruptRenameRaceFS struct {
	osFS
	fault error
}

func (f corruptRenameRaceFS) rename(_, new string) error {
	if err := os.WriteFile(new, []byte("invalid"), 0o600); err != nil {
		return err
	}
	return f.fault
}

func TestARenameFailureReturnsErrorWhenDestinationDoesNotMatch(t *testing.T) {
	s := testStore(t)
	fault := errors.New("rename refused")
	s.fs = corruptRenameRaceFS{fault: fault}
	data := []byte("content")
	_, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if !errors.Is(err, fault) {
		t.Fatalf("rename error was masked: %v", err)
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	got, err := os.ReadFile(filepath.Join(s.dir, hash[:2], hash))
	if err != nil || string(got) != "invalid" {
		t.Fatalf("bad destination was changed: %q %v", got, err)
	}
	if countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
		t.Fatal("temporary file remains")
	}
}

type renameCountFS struct {
	osFS
	renames int
}

func (f *renameCountFS) rename(old, new string) error {
	f.renames++
	return f.osFS.rename(old, new)
}

type failingFS struct {
	osFS
	fail  string
	fault error
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
		if f.fault != nil {
			return f.fault
		}
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
	fs := &renameCountFS{}
	s.fs = fs
	data := []byte("orphaned until referenced")
	digest := sha256.Sum256(data)
	first := Ref{Hash: hex.EncodeToString(digest[:]), Size: int64(len(data))}
	fanout := filepath.Join(s.dir, first.Hash[:2])
	if err := os.Mkdir(fanout, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fanout, first.Hash), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "tmp", "stray"), []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if err != nil || first != second || countFiles(t, filepath.Join(s.dir, first.Hash[:2])) != 1 {
		t.Fatalf("reuse: %+v %+v %v", first, second, err)
	}
	if countFiles(t, filepath.Join(s.dir, "tmp")) != 1 {
		t.Fatal("write left an additional temporary file")
	}
	if fs.renames != 0 {
		t.Fatalf("reuse renamed the blob %d times", fs.renames)
	}
}

type syncFaultFS struct {
	osFS
	fail  bool
	calls []string
}

type cancelAfterRead struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r cancelAfterRead) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p[:1])
	r.cancel()
	return n, err
}

type noProgressReader struct{}

func (noProgressReader) Read([]byte) (int, error) { return 0, nil }

func TestCancellationLeavesNoTemporaryFile(t *testing.T) {
	for _, midStream := range []bool{false, true} {
		s := testStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		var reader io.Reader = strings.NewReader("content")
		if midStream {
			reader = cancelAfterRead{reader: reader, cancel: cancel}
		} else {
			cancel()
		}
		_, err := s.Write(ctx, reader, 100)
		if !errors.Is(err, context.Canceled) || countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
			t.Fatalf("mid-stream=%v: %v", midStream, err)
		}
		cancel()
	}
}

func TestWriteLimitBoundariesLeaveNoTemporaryFile(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		limit      int64
		wantErr    error
	}{
		{"zero empty", "", 0, nil},
		{"zero one byte", "x", 0, ErrTooLarge},
		{"exact limit", "abc", 3, nil},
		{"negative limit", "", -1, ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ref, err := s.Write(context.Background(), strings.NewReader(tc.data), tc.limit)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && ref.Size != int64(len(tc.data))) {
				t.Fatalf("ref=%+v error=%v", ref, err)
			}
			if countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
				t.Fatal("temporary file remains")
			}
		})
	}
}

func TestReaderWithoutProgressLeavesNoTemporaryFile(t *testing.T) {
	s := testStore(t)
	_, err := s.Write(context.Background(), noProgressReader{}, 100)
	if !errors.Is(err, io.ErrNoProgress) || countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
		t.Fatalf("no progress: %v", err)
	}
}

func TestExistingNonBlobDestinationIsCorrupt(t *testing.T) {
	s := testStore(t)
	data := []byte("content")
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	fanout := filepath.Join(s.dir, hash[:2])
	if err := os.Mkdir(fanout, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(fanout, hash), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if !errors.Is(err, ErrCorrupt) || countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
		t.Fatalf("directory destination: %v", err)
	}
}

type mkdirFaultFS struct{ osFS }

func (mkdirFaultFS) mkdirAll(path string, mode os.FileMode) error { return injectedDiskFull() }

func TestFanoutMkdirDiskFullLeavesNoTemporaryFile(t *testing.T) {
	s := testStore(t)
	s.fs = mkdirFaultFS{}
	_, err := s.Write(context.Background(), strings.NewReader("content"), 100)
	if !errors.Is(err, ErrDiskFull) || countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
		t.Fatalf("fanout mkdir: %v", err)
	}
}

func (f *syncFaultFS) syncDir(path string) error {
	f.calls = append(f.calls, path)
	if f.fail {
		return injectedDiskFull()
	}
	return nil
}

func TestFailedDirectorySyncIsRetriedWhenTheBlobIsReused(t *testing.T) {
	s := testStore(t)
	fs := &syncFaultFS{fail: true}
	s.fs = fs
	data := []byte("retry directory sync")
	_, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if !errors.Is(err, ErrDiskFull) || !errors.Is(err, injectedDiskFull()) {
		t.Fatalf("first write: %v", err)
	}
	if countFiles(t, filepath.Join(s.dir, "tmp")) != 0 {
		t.Fatal("temporary file remains after sync failure")
	}
	fs.fail = false
	fs.calls = nil
	ref, err := s.Write(context.Background(), bytes.NewReader(data), 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(s.dir, ref.Hash[:2]), s.dir}
	if !slices.Equal(fs.calls, want) {
		t.Fatalf("directory syncs = %v, want %v", fs.calls, want)
	}
	if countFiles(t, filepath.Join(s.dir, "tmp")) != 0 || countFiles(t, want[0]) != 1 {
		t.Fatal("unexpected blob or temporary file count")
	}
}

func TestOpenReportsStaleTemporaryFilesItCouldNotRemove(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(tmp, "artifact-stuck")
	removable := filepath.Join(tmp, "artifact-removable")
	ago := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{stuck, removable} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, ago, ago); err != nil {
			t.Fatal(err)
		}
	}
	removeErr := errors.New("file held open")
	s, err := openWithStaleTempRemover(dir, func(path string) error {
		if path == stuck {
			return removeErr
		}
		return os.Remove(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	skipped, first := s.StaleTemps()
	if skipped != 1 || !errors.Is(first, removeErr) || !strings.Contains(first.Error(), stuck) {
		t.Fatalf("skipped=%d first=%v", skipped, first)
	}
	clean, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if skipped, first := clean.StaleTemps(); skipped != 0 || first != nil {
		t.Fatalf("clean sweep reported skipped=%d first=%v", skipped, first)
	}
}

func TestRemoveDeletesAPublishedBlobAndRefusesABadHash(t *testing.T) {
	s := testStore(t)
	ref, err := s.Write(context.Background(), strings.NewReader("remove me"), 9)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ref.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ref.Hash); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open after remove: %v", err)
	}
	if err := s.Remove(ref.Hash); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second remove: %v", err)
	}
	for _, bad := range []string{strings.ToUpper(ref.Hash), ref.Hash[:63], "../" + ref.Hash[3:], ""} {
		if err := s.Remove(bad); !errors.Is(err, ErrInvalidHash) {
			t.Fatalf("remove %q: %v", bad, err)
		}
	}
}

func TestBlobsListsOnlyPublishedBlobs(t *testing.T) {
	s := testStore(t)
	var want []string
	for _, content := range []string{"first blob", "second blob"} {
		ref, err := s.Write(context.Background(), strings.NewReader(content), 64)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, ref.Hash)
	}
	slices.Sort(want)
	if err := os.WriteFile(filepath.Join(s.dir, "tmp", "artifact-123"), []byte("being written"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, want[0][:2], "notes"), []byte("not a blob"), 0o600); err != nil {
		t.Fatal(err)
	}
	misplaced := strings.Repeat("0", 64)
	if want[0][:2] == "00" {
		misplaced = strings.Repeat("f", 64)
	}
	if err := os.WriteFile(filepath.Join(s.dir, want[0][:2], misplaced), []byte("wrong fan-out"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.dir, want[0][:2], want[0][:2]+strings.Repeat("1", 62)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.dir, "zz"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	linked := "ee" + strings.Repeat("e", 62)
	if err := os.WriteFile(filepath.Join(outside, linked), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.dir, "ee")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		if output, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Fatalf("create junction: %v: %s", err, output)
		}
	}
	got, err := s.Blobs()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("blobs = %v, want %v", got, want)
	}
}
