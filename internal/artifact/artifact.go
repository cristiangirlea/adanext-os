// Package artifact stores durable blobs by their SHA-256 content hash.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrTooLarge    = errors.New("artifact exceeds size limit")
	ErrCorrupt     = errors.New("artifact is corrupt")
	ErrDiskFull    = errors.New("artifact disk is full")
	ErrInvalidHash = errors.New("invalid artifact hash")
	// ErrBusy means another handle holds the blob open, so a replacement or a
	// read was refused. It happens on Windows only, where an open file cannot be
	// replaced; retry once the reader has closed.
	ErrBusy = errors.New("artifact is in use")
)

type Ref struct {
	Hash string
	Size int64
}

type Store struct {
	dir          string
	fs           filesystem
	staleSkipped int
	staleErr     error
}

type file interface {
	io.Writer
	Sync() error
	Close() error
	Name() string
}

// TempFile is the narrow writable-file boundary used for filesystem fault tests.
type TempFile interface {
	io.Writer
	Sync() error
	Close() error
	Name() string
}

type tempFileFS struct {
	filesystem
	create func(string, string) (TempFile, error)
}

func (f tempFileFS) createTemp(dir, pattern string) (file, error) { return f.create(dir, pattern) }

// OpenWithTempFileCreator keeps directory and publication operations on the OS
// filesystem while allowing a caller to inject a failing temporary file.
func OpenWithTempFileCreator(dir string, create func(string, string) (TempFile, error)) (*Store, error) {
	if create == nil {
		return nil, errors.New("temporary file creator is nil")
	}
	s, err := Open(dir)
	if err != nil {
		return nil, err
	}
	s.fs = tempFileFS{filesystem: s.fs, create: create}
	return s, nil
}

type filesystem interface {
	createTemp(dir, pattern string) (file, error)
	mkdirAll(path string, mode os.FileMode) error
	rename(old, new string) error
	remove(path string) error
	lstat(path string) (os.FileInfo, error)
	open(path string) (*os.File, error)
	syncDir(path string) error
}

type osFS struct{}

func (osFS) createTemp(dir, pattern string) (file, error) { return os.CreateTemp(dir, pattern) }
func (osFS) mkdirAll(path string, mode os.FileMode) error { return os.MkdirAll(path, mode) }
func (osFS) rename(old, new string) error                 { return os.Rename(old, new) }
func (osFS) remove(path string) error                     { return os.Remove(path) }
func (osFS) lstat(path string) (os.FileInfo, error)       { return os.Lstat(path) }
func (osFS) open(path string) (*os.File, error)           { return openBlob(path) }
func (osFS) syncDir(path string) error                    { return syncDirectory(path) }

// Open prepares private artifact directories and removes stale temporary files.
// The coordinator must hold its singleton lock while calling Open, so cleanup
// cannot remove a temporary file belonging to another active writer.
func Open(dir string) (*Store, error) {
	return openWithStaleTempRemover(dir, os.Remove)
}

func openWithStaleTempRemover(dir string, remove func(string) error) (*Store, error) {
	if dir == "" {
		return nil, errors.New("artifact directory is empty")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	fs := osFS{}
	for _, path := range []string{absolute, filepath.Join(absolute, "tmp")} {
		if err := privateDirectory(fs, path); err != nil {
			return nil, err
		}
	}
	skipped, sweepErr := removeStaleTemps(filepath.Join(absolute, "tmp"), remove)
	return &Store{dir: absolute, fs: fs, staleSkipped: skipped, staleErr: sweepErr}, nil
}

// StaleTemps reports what Open's sweep of stale temporary files left behind:
// how many it could not remove and the first error it met. A file that stays
// skipped across restarts is stuck and needs a look.
func (s *Store) StaleTemps() (skipped int, first error) {
	return s.staleSkipped, s.staleErr
}

func removeStaleTemps(dir string, remove func(string) error) (skipped int, first error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("list temporary artifacts: %w", err)
	}
	cutoff := time.Now().Add(-time.Hour)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "artifact-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				skipped++
				if first == nil {
					first = fmt.Errorf("inspect stale temporary artifact %s: %w", path, err)
				}
			}
			continue
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			skipped++
			if first == nil {
				first = fmt.Errorf("remove stale temporary artifact %s: %w", path, err)
			}
		}
	}
	return skipped, first
}

func privateDirectory(fs filesystem, path string) error {
	if err := refuseSymlink(path); err != nil {
		return err
	}
	if err := fs.mkdirAll(path, 0o700); err != nil {
		return classify(err)
	}
	if err := refuseSymlink(path); err != nil {
		return err
	}
	return makePrivateDirectory(path)
}

// Write streams a blob to a private temporary file and publishes it by hash.
// A mismatched blob at the destination is replaced. On Windows that fails with
// ErrBusy while a reader holds the old blob open; the temporary file is removed
// and the blob is left as it was, so the caller retries after the reader closes.
func (s *Store) Write(ctx context.Context, r io.Reader, limit int64) (ref Ref, err error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	if limit < 0 {
		return Ref{}, ErrTooLarge
	}
	tmp, err := s.fs.createTemp(filepath.Join(s.dir, "tmp"), "artifact-")
	if err != nil {
		return Ref{}, classify(err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			if removeErr := s.fs.remove(tmpPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove temporary artifact: %w", classify(removeErr)))
			}
		}
	}()
	h := sha256.New()
	buf := make([]byte, 32*1024)
	emptyReads := 0
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
			break
		}
		n, readErr := r.Read(buf)
		if n > 0 {
			emptyReads = 0
			if int64(n) > limit-ref.Size {
				err = ErrTooLarge
				break
			}
			for written := 0; written < n; {
				count, writeErr := tmp.Write(buf[written:n])
				if count > 0 {
					_, _ = h.Write(buf[written : written+count])
					ref.Size += int64(count)
					written += count
				}
				if writeErr != nil {
					err = classify(writeErr)
					break
				}
				if count == 0 {
					err = io.ErrShortWrite
					break
				}
			}
			if err != nil {
				break
			}
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				err = io.ErrNoProgress
				break
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				err = readErr
			}
			break
		}
	}
	if err != nil {
		closeErr := tmp.Close()
		if closeErr != nil {
			err = errors.Join(err, classify(closeErr))
		}
		return Ref{}, err
	}
	if syncErr := tmp.Sync(); syncErr != nil {
		closeErr := tmp.Close()
		return Ref{}, errors.Join(classify(syncErr), classify(closeErr))
	}
	if closeErr := tmp.Close(); closeErr != nil {
		return Ref{}, classify(closeErr)
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	ref.Hash = hex.EncodeToString(h.Sum(nil))
	fanout := filepath.Join(s.dir, ref.Hash[:2])
	if err := privateDirectory(s.fs, fanout); err != nil {
		return Ref{}, err
	}
	dest := filepath.Join(fanout, ref.Hash)
	if info, statErr := s.fs.lstat(dest); statErr == nil {
		if !info.Mode().IsRegular() {
			return Ref{}, fmt.Errorf("existing artifact %s: %w", ref.Hash, ErrCorrupt)
		}
		if info.Size() == ref.Size {
			matches, err := s.blobMatches(dest, ref)
			if err != nil {
				return Ref{}, err
			}
			if matches {
				return s.reuse(tmpPath, fanout, ref)
			}
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Ref{}, classify(statErr)
	}
	if err := s.fs.rename(tmpPath, dest); err != nil {
		if info, statErr := s.fs.lstat(dest); statErr == nil && info.Mode().IsRegular() && info.Size() == ref.Size {
			matches, matchErr := s.blobMatches(dest, ref)
			if matchErr == nil && matches {
				return s.reuse(tmpPath, fanout, ref)
			}
		}
		return Ref{}, classify(replaceError(err, dest))
	}
	if err := s.fs.syncDir(fanout); err != nil {
		return Ref{}, classify(err)
	}
	if err := s.fs.syncDir(s.dir); err != nil {
		return Ref{}, classify(err)
	}
	return ref, nil
}

func (s *Store) blobMatches(path string, ref Ref) (bool, error) {
	f, err := s.fs.open(path)
	if err != nil {
		return false, classify(err)
	}
	h := sha256.New()
	n, readErr := io.Copy(h, f)
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return false, classify(err)
	}
	return n == ref.Size && hex.EncodeToString(h.Sum(nil)) == ref.Hash, nil
}

func (s *Store) reuse(tmpPath, fanout string, ref Ref) (Ref, error) {
	if err := s.fs.remove(tmpPath); err != nil {
		return Ref{}, classify(err)
	}
	if err := s.fs.syncDir(fanout); err != nil {
		return Ref{}, classify(err)
	}
	if err := s.fs.syncDir(s.dir); err != nil {
		return Ref{}, classify(err)
	}
	return ref, nil
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	if isDiskFull(err) {
		return fmt.Errorf("%w: %w", ErrDiskFull, err)
	}
	if isBusy(err) {
		return fmt.Errorf("%w: %w", ErrBusy, err)
	}
	return err
}

// Open returns a reader that checks the blob's size and hash only at EOF.
// Callers must read through EOF to verify content; closing early does not verify it.
func (s *Store) Open(hashString string) (io.ReadCloser, error) {
	if !validHash(hashString) {
		return nil, ErrInvalidHash
	}
	path := filepath.Join(s.dir, hashString[:2], hashString)
	info, err := s.fs.lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrCorrupt
	}
	f, err := s.fs.open(path)
	if err != nil {
		return nil, classify(err)
	}
	info, err = f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, ErrCorrupt
	}
	return &verifiedReader{file: f, hash: sha256.New(), expectedHash: hashString, expectedSize: info.Size()}, nil
}

// Remove deletes a published blob. The caller decides that nothing references
// it any more: a reference rolled back after the write, with no committed
// record sharing the content. A blob that is already gone is os.ErrNotExist.
func (s *Store) Remove(hashString string) error {
	if !validHash(hashString) {
		return ErrInvalidHash
	}
	return classify(s.fs.remove(filepath.Join(s.dir, hashString[:2], hashString)))
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

type verifiedReader struct {
	file         *os.File
	hash         hash.Hash
	expectedHash string
	expectedSize int64
	size         int64
}

func (r *verifiedReader) Read(p []byte) (int, error) {
	n, err := r.file.Read(p)
	if n > 0 {
		_, _ = r.hash.Write(p[:n])
		r.size += int64(n)
	}
	if errors.Is(err, io.EOF) && (r.size != r.expectedSize || hex.EncodeToString(r.hash.Sum(nil)) != r.expectedHash) {
		return n, ErrCorrupt
	}
	return n, err
}

func (r *verifiedReader) Close() error { return r.file.Close() }
