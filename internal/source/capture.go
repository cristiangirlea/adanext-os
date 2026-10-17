package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Refusal codes for a selected untracked path.
const (
	PathInvalid   = "invalid_path"
	PathSensitive = "sensitive_path"
)

var (
	// ErrSourceChanged means the repository changed while it was captured.
	ErrSourceChanged = errors.New("the repository changed during capture")
	// ErrTooLarge means a patch, a file or the whole capture is over its cap.
	ErrTooLarge = errors.New("larger than a snapshot allows")
)

// Capture caps: patches and selected files are held in memory until the
// second fingerprint agrees, so nothing is stored for a refused capture.
const (
	maxPatchSize   = 64 << 20
	maxCaptureSize = 256 << 20
)

// PathError refuses one selected untracked path.
type PathError struct {
	Code   string
	Path   string
	Reason string
}

func (e *PathError) Error() string { return fmt.Sprintf("%s %q: %s", e.Code, e.Path, e.Reason) }

// BlockedError is an inspection with import blockers; nothing was captured.
type BlockedError struct{ Blockers []Blocker }

func (e *BlockedError) Error() string { return "the repository has import blockers" }

// Selection names the untracked files a snapshot takes. A path that looks
// like a credential, or that the repository ignores, is taken only when it is
// also in IncludeSensitive.
type Selection struct {
	Untracked        []string
	IncludeSensitive []string
}

// File is a selected untracked file: its path, its git mode (100644 or
// 100755) and its content.
type File struct {
	Path    string
	Mode    string
	Content []byte
}

// Capture is the repository's work as it was between two equal fingerprints:
// the inspection taken first, the staged and unstaged patches, and the
// selected untracked files in the order requested.
type Capture struct {
	Inspection Inspection
	Staged     []byte
	Unstaged   []byte
	Untracked  []File
}

// Capture inspects the repository at path, takes its staged and unstaged
// patches through the mirror and reads the selected untracked files, then
// inspects it again: a different fingerprint, or a selected file that no
// longer holds what was read, is ErrSourceChanged and nothing is returned.
func (in *Inspector) Capture(ctx context.Context, path string, sel Selection) (Capture, error) {
	if err := os.MkdirAll(in.MirrorRoot, 0o700); err != nil {
		return Capture{}, err
	}
	mirror, err := os.MkdirTemp(in.MirrorRoot, "c")
	if err != nil {
		return Capture{}, err
	}
	defer os.RemoveAll(mirror)
	first, err := in.inspect(ctx, path, mirror)
	if err != nil {
		return Capture{}, err
	}
	if len(first.Blockers) > 0 {
		return Capture{}, &BlockedError{Blockers: first.Blockers}
	}
	if in.afterInspect != nil {
		in.afterInspect()
	}
	root := first.Identity.Path
	selected, err := in.checkSelection(ctx, mirror, root, first.Status.Untracked, sel)
	if err != nil {
		return Capture{}, err
	}
	c := Capture{Inspection: first}
	if c.Staged, err = in.Git.runCapped(ctx, mirror, root, maxPatchSize, "diff", "--cached", "--binary", "--no-color"); err != nil {
		return Capture{}, err
	}
	if c.Unstaged, err = in.Git.runCapped(ctx, mirror, root, maxPatchSize, "diff", "--binary", "--no-color"); err != nil {
		return Capture{}, err
	}
	total := int64(len(c.Staged) + len(c.Unstaged))
	for _, p := range selected {
		f, err := in.readSelected(root, p)
		if err != nil {
			return Capture{}, err
		}
		if total += int64(len(f.Content)); total > maxCaptureSize {
			return Capture{}, fmt.Errorf("%w: the capture is over %d bytes", ErrTooLarge, maxCaptureSize)
		}
		c.Untracked = append(c.Untracked, f)
	}
	if in.beforeRecheck != nil {
		in.beforeRecheck()
	}
	second, err := in.Inspect(ctx, path)
	if err != nil {
		return Capture{}, err
	}
	if second.Fingerprint != first.Fingerprint || second.Identity.Path != root || second.Identity.CommonDir != first.Identity.CommonDir || len(second.Blockers) > 0 {
		return Capture{}, ErrSourceChanged
	}
	for _, f := range c.Untracked {
		// Ignored files are outside the fingerprint, and a file can change while it is read.
		if !slices.Equal(pathState(filepath.Join(root, filepath.FromSlash(f.Path))), f.state()) {
			return Capture{}, ErrSourceChanged
		}
	}
	return c, nil
}

// state is the file as pathState describes it.
func (f File) state() []string {
	sum := sha256.Sum256(f.Content)
	mode := "file"
	if f.Mode == "100755" {
		mode = "exec"
	}
	return []string{mode, strconv.Itoa(len(f.Content)), hex.EncodeToString(sum[:])}
}

// checkSelection refuses a malformed path, a path that is neither on the
// untracked list nor ignored, and a sensitive or ignored path the selection
// does not include; it returns the selected paths in order.
func (in *Inspector) checkSelection(ctx context.Context, mirror, root string, untracked []string, sel Selection) ([]string, error) {
	listed := map[string]bool{}
	for _, p := range untracked {
		listed[p] = true
	}
	included := map[string]bool{}
	for _, p := range sel.IncludeSensitive {
		included[p] = true
	}
	var unlisted []string
	for _, p := range sel.Untracked {
		if reason := badPath(p); reason != "" {
			return nil, &PathError{Code: PathInvalid, Path: p, Reason: reason}
		}
		if !listed[p] {
			unlisted = append(unlisted, p)
		}
	}
	ignored, err := in.ignored(ctx, mirror, root, unlisted, maxPatchSize)
	if err != nil {
		return nil, err
	}
	for _, p := range sel.Untracked {
		switch {
		case listed[p] && strings.HasSuffix(p, "/"):
			// An unregistered nested repository is listed as its directory.
			return nil, &PathError{Code: PathInvalid, Path: p, Reason: "a directory"}
		case listed[p] && sensitive(p) && !included[p]:
			return nil, &PathError{Code: PathSensitive, Path: p, Reason: "looks like a credential; list it in include_sensitive to take it"}
		case listed[p]:
		case ignored[p] && !included[p]:
			return nil, &PathError{Code: PathSensitive, Path: p, Reason: "ignored by the repository; list it in include_sensitive to take it"}
		case ignored[p]:
		default:
			return nil, &PathError{Code: PathInvalid, Path: p, Reason: "not an untracked file of the repository"}
		}
	}
	return sel.Untracked, nil
}

// ignored asks git, through the mirror's excludes, which of paths are
// existing ignored untracked files; pathspecs are literal and batched, and
// each listing is capped at limit bytes.
func (in *Inspector) ignored(ctx context.Context, mirror, root string, paths []string, limit int) (map[string]bool, error) {
	found := map[string]bool{}
	for len(paths) > 0 {
		args := []string{"status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=traditional", "--no-renames", "--"}
		size := 0
		for len(paths) > 0 && (size == 0 || size+len(paths[0]) < 16<<10) {
			args = append(args, ":(literal)"+paths[0])
			size += len(paths[0]) + 11
			paths = paths[1:]
		}
		out, err := in.Git.runCapped(ctx, mirror, root, limit, args...)
		if err != nil {
			return nil, err
		}
		for _, rec := range strings.Split(string(out), "\x00") {
			if p, ok := strings.CutPrefix(rec, "! "); ok {
				found[p] = true
			}
		}
	}
	return found, nil
}

// reserved are the Windows device names, which name a device in any
// directory and with any extension.
var reserved = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true, "conin$": true, "conout$": true}

func init() {
	for _, prefix := range []string{"com", "lpt"} {
		for _, digit := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
			reserved[prefix+digit] = true
		}
	}
}

// badPath is why p cannot name a work-tree file on every system Ada runs
// on, or "" when it can: git's untracked paths are slash-separated and
// relative, and Windows reads a stream, a drive, a device name or a trailing
// dot or space as something else. A .git component, or git~N (its Windows
// short name), would name a git directory.
func badPath(p string) string {
	switch {
	case p == "":
		return "empty"
	case strings.HasPrefix(p, "/"):
		return "absolute"
	case strings.ContainsAny(p, `\:`):
		return `a backslash, a drive or a stream (":")`
	case strings.ContainsAny(p, `<>"|?*`):
		return "a character Windows does not allow in a file name"
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "a control character"
		}
	}
	parts := strings.Split(strings.TrimSuffix(p, "/"), "/")
	for _, part := range parts {
		switch {
		case part == "" || part == "." || part == "..":
			return "an empty, . or .. component"
		case strings.HasSuffix(part, ".") || strings.HasSuffix(part, " "):
			return "a component ending in a dot or a space"
		}
		if lower := strings.ToLower(part); lower == ".git" || shortGitName(lower) {
			return "a .git directory or its short name"
		}
		stem, _, _ := strings.Cut(part, ".")
		if reserved[strings.ToLower(strings.TrimRight(stem, " "))] {
			return "a Windows device name"
		}
	}
	return ""
}

// shortGitName is git~N, the 8.3 short name Windows can give .git.
func shortGitName(lower string) bool {
	n, ok := strings.CutPrefix(lower, "git~")
	return ok && n != "" && strings.Trim(n, "0123456789") == ""
}

// sensitive reports a path that looks like a credential by its name or by
// a .aws or .ssh directory above it.
func sensitive(p string) bool {
	parts := strings.Split(strings.ToLower(p), "/")
	for _, dir := range parts[:len(parts)-1] {
		if dir == ".aws" || dir == ".ssh" {
			return true
		}
	}
	base := parts[len(parts)-1]
	switch base {
	case ".npmrc", ".netrc", "_netrc", ".pypirc", "credentials", ".git-credentials":
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".kdbx"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	for _, prefix := range []string{".env", "id_rsa", "id_ed25519", "id_ecdsa", "id_dsa", "credentials."} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}

// readSelected reads a selected file under root with no link or redirecting
// reparse point on its path, as a regular file within the size cap, and
// checks after the read that the path still leads, link-free, to the file
// read.
func (in *Inspector) readSelected(root, rel string) (File, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	refuse := func(err error) (File, error) {
		switch {
		case errors.Is(err, errLink):
			return File{}, &PathError{Code: PathInvalid, Path: rel, Reason: "a link or junction on its path"}
		case errors.Is(err, errNotRegular):
			return File{}, &PathError{Code: PathInvalid, Path: rel, Reason: "not a regular file"}
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			// Gone, or a directory on its path is now a file.
			return File{}, ErrSourceChanged
		}
		return File{}, err
	}
	if err := noLinks(root, full); err != nil {
		return refuse(err)
	}
	if in.beforeOpen != nil {
		in.beforeOpen()
	}
	f, err := openFile(full)
	if err != nil {
		return refuse(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return File{}, err
	}
	if len(data) > maxFileSize {
		return File{}, fmt.Errorf("%w: %s is over %d bytes", ErrTooLarge, rel, maxFileSize)
	}
	if err := noLinks(root, full); err != nil {
		return refuse(err)
	}
	if now, err := os.Lstat(full); err != nil || !os.SameFile(info, now) {
		return File{}, ErrSourceChanged
	}
	mode := "100644"
	if info.Mode().Perm()&0o100 != 0 {
		mode = "100755"
	}
	return File{Path: rel, Mode: mode, Content: data}, nil
}

// cappedBuffer fails a write past its limit, which ends the copy from git.
// The buffer is a field, not embedded: an embedded one's ReadFrom would let
// io.Copy bypass Write.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		b.over = true
		return 0, ErrTooLarge
	}
	return b.buf.Write(p)
}

// runCapped is run with its output capped at limit bytes.
func (g *Git) runCapped(ctx context.Context, mirror, workTree string, limit int, args ...string) ([]byte, error) {
	cmd := g.command(ctx, mirror, workTree, args...)
	out := &cappedBuffer{limit: limit}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	err := cmd.Run()
	if out.over {
		return nil, fmt.Errorf("%w: git %s output is over %d bytes", ErrTooLarge, args[0], limit)
	}
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out.buf.Bytes(), nil
}
