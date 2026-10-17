package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func capture(in *Inspector, dir string, untracked []string, include ...string) (Capture, error) {
	return in.Capture(context.Background(), dir, Selection{Untracked: untracked, IncludeSensitive: include})
}

func pathCode(err error) string {
	if e, ok := errors.AsType[*PathError](err); ok {
		return e.Code
	}
	return ""
}

// workRepo has staged, unstaged and untracked work in text, binary and CRLF
// files, with a staged deletion and a staged new file.
func workRepo(t *testing.T) (dir, head string) {
	t.Helper()
	dir = newRepo(t, map[string]string{
		"text.txt": "one\ntwo\n",
		"bin.dat":  "\x00\x01\x02\xff\r\n\x00tail",
		"crlf.txt": "a\r\nb\r\n",
		"gone.txt": "bye\n",
	})
	head = git(t, dir, "rev-parse", "HEAD")
	write(t, filepath.Join(dir, "text.txt"), "one\ntwo\nstaged\n")
	write(t, filepath.Join(dir, "bin.dat"), "\x00\x09\x02\xfe\r\n\x00tail and more")
	write(t, filepath.Join(dir, "new.txt"), "new\r\nfile\n")
	git(t, dir, "add", "text.txt", "bin.dat", "new.txt")
	git(t, dir, "rm", "-q", "gone.txt")
	write(t, filepath.Join(dir, "text.txt"), "one\ntwo\nstaged\nunstaged\n")
	write(t, filepath.Join(dir, "crlf.txt"), "a\r\nb\r\nc\r\n")
	write(t, filepath.Join(dir, "u", "bin.raw"), "\x00\xff\r\n\x7fELF")
	write(t, filepath.Join(dir, "u", "crlf.txt"), "x\r\ny\r\n")
	write(t, filepath.Join(dir, "skip.txt"), "not selected\n")
	return dir, head
}

func TestCaptureReproducesStagedUnstagedAndSelectedUntrackedWork(t *testing.T) {
	t.Parallel()
	dir, head := workRepo(t)
	before := hashTree(t, dir)
	c, err := capture(newInspector(t), dir, []string{"u/bin.raw", "u/crlf.txt"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	sameTree(t, before, hashTree(t, dir))
	if c.Inspection.Identity.Head != head || c.Inspection.Fingerprint == "" {
		t.Fatalf("identity = %+v, fingerprint %q", c.Inspection.Identity, c.Inspection.Fingerprint)
	}
	if len(c.Untracked) != 2 || c.Untracked[0].Path != "u/bin.raw" || c.Untracked[1].Path != "u/crlf.txt" {
		t.Fatalf("untracked = %+v", c.Untracked)
	}

	// Applied to a clone of the base, the snapshot gives the original's work tree and index.
	clone := reproduce(t, dir, head, c)
	for _, p := range []string{"text.txt", "bin.dat", "crlf.txt", "gone.txt", "new.txt", "u/bin.raw", "u/crlf.txt"} {
		want, wantErr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		got, gotErr := os.ReadFile(filepath.Join(clone, filepath.FromSlash(p)))
		if (wantErr != nil) != (gotErr != nil) || !bytes.Equal(want, got) {
			t.Errorf("%s: original %q (%v), reproduced %q (%v)", p, want, wantErr, got, gotErr)
		}
	}
	if _, err := os.Stat(filepath.Join(clone, "skip.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an unselected file was reproduced: %v", err)
	}
	if want, got := git(t, dir, "ls-files", "-s"), git(t, clone, "ls-files", "-s"); want != got {
		t.Errorf("index:\noriginal\n%s\nreproduced\n%s", want, got)
	}
}

// reproduce applies a capture to a clone of dir at head: the staged patch to
// the index and work tree, the unstaged one to the work tree, and the files.
func reproduce(t *testing.T, dir, head string, c Capture, config ...string) string {
	t.Helper()
	clone := tempDir(t)
	git(t, clone, append(append([]string{"clone", "-q", "--no-hardlinks", "--no-checkout"}, config...), dir, ".")...)
	git(t, clone, "checkout", "-q", "--detach", head)
	patches := tempDir(t)
	for _, p := range []struct {
		name string
		data []byte
		args []string
	}{{"staged.patch", c.Staged, []string{"--index"}}, {"unstaged.patch", c.Unstaged, nil}} {
		if len(p.data) == 0 {
			continue
		}
		if err := os.WriteFile(filepath.Join(patches, p.name), p.data, 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, clone, append(append([]string{"apply"}, p.args...), filepath.Join(patches, p.name))...)
	}
	for _, f := range c.Untracked {
		if f.Mode != "100644" && f.Mode != "100755" {
			t.Errorf("%s: mode %q", f.Path, f.Mode)
		}
		write(t, filepath.Join(clone, filepath.FromSlash(f.Path)), string(f.Content))
	}
	return clone
}

func TestCaptureReproducesAStagedRename(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"a.txt": "one\ntwo\nthree\n"})
	head := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "mv", "a.txt", "b.txt")
	c, err := capture(newInspector(t), dir, nil)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !bytes.Contains(c.Staged, []byte("rename from a.txt\nrename to b.txt")) || len(c.Unstaged) != 0 {
		t.Fatalf("staged %q, unstaged %q", c.Staged, c.Unstaged)
	}
	clone := reproduce(t, dir, head, c)
	if want, got := git(t, dir, "ls-files", "-s"), git(t, clone, "ls-files", "-s"); want != got {
		t.Errorf("index:\noriginal\n%s\nreproduced\n%s", want, got)
	}
	if _, err := os.Stat(filepath.Join(clone, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the renamed file is still there: %v", err)
	}
}

func TestCaptureReproducesATrackedModeChange(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows files carry no executable bit")
	}
	dir := newRepo(t, map[string]string{"run.sh": "#!/bin/sh\n"})
	head := git(t, dir, "rev-parse", "HEAD")
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := capture(newInspector(t), dir, nil)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !bytes.Contains(c.Unstaged, []byte("old mode 100644\nnew mode 100755")) || len(c.Staged) != 0 {
		t.Fatalf("staged %q, unstaged %q", c.Staged, c.Unstaged)
	}
	info, err := os.Stat(filepath.Join(reproduce(t, dir, head, c), "run.sh"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("reproduced mode %v: %v", info.Mode(), err)
	}
}

func TestCaptureTakesAnIntentToAddFileAsNewInTheUnstagedPatch(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	head := git(t, dir, "rev-parse", "HEAD")
	write(t, filepath.Join(dir, "later.txt"), "later\n")
	git(t, dir, "add", "-N", "later.txt")
	c, err := capture(newInspector(t), dir, nil)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(c.Staged) != 0 || !bytes.Contains(c.Unstaged, []byte("new file mode 100644")) || !bytes.Contains(c.Unstaged, []byte("+later")) {
		t.Fatalf("staged %q, unstaged %q", c.Staged, c.Unstaged)
	}
	// The file is reproduced; its intent-to-add index entry is not.
	clone := reproduce(t, dir, head, c)
	if got, err := os.ReadFile(filepath.Join(clone, "later.txt")); err != nil || string(got) != "later\n" {
		t.Fatalf("reproduced %q: %v", got, err)
	}
	if listed := git(t, clone, "ls-files", "later.txt"); listed != "" {
		t.Fatalf("the reproduced index lists %q", listed)
	}
}

func TestCaptureWithAutocrlfHasNormalisedPatches(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"lines.txt": "a\nb\n"})
	head := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "config", "core.autocrlf", "true")
	write(t, filepath.Join(dir, "lines.txt"), "a\r\nb\r\nc\r\n")
	c, err := capture(newInspector(t), dir, nil)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if bytes.Contains(c.Unstaged, []byte("\r")) || !bytes.Contains(c.Unstaged, []byte("+c\n")) || bytes.Contains(c.Unstaged, []byte("-a")) {
		t.Fatalf("unstaged %q", c.Unstaged)
	}
	clone := reproduce(t, dir, head, c, "-c", "core.autocrlf=true")
	if got, err := os.ReadFile(filepath.Join(clone, "lines.txt")); err != nil || string(got) != "a\r\nb\r\nc\r\n" {
		t.Fatalf("reproduced %q: %v", got, err)
	}
}

func TestCaptureRefusesABlockerThatAppearsDuringCapture(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	in := newInspector(t)
	in.beforeRecheck = func() { write(t, filepath.Join(dir, ".git", "MERGE_HEAD"), git(t, dir, "rev-parse", "HEAD")+"\n") }
	if _, err := capture(in, dir, nil); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("capture = %v", err)
	}
}

func TestCaptureKeepsAnExecutableUntrackedFilesMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows files carry no executable bit")
	}
	dir := newRepo(t, nil)
	write(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := capture(newInspector(t), dir, []string{"run.sh"})
	if err != nil || len(c.Untracked) != 1 || c.Untracked[0].Mode != "100755" {
		t.Fatalf("capture = %+v, %v", c.Untracked, err)
	}
}

func TestCaptureOfAnUnbornBranchHasTheStagedFilesAsNew(t *testing.T) {
	t.Parallel()
	dir := tempDir(t)
	git(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	git(t, dir, "add", "a.txt")
	c, err := capture(newInspector(t), dir, nil)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if c.Inspection.Identity.Head != "" || !bytes.Contains(c.Staged, []byte("new file mode 100644")) || len(c.Unstaged) != 0 {
		t.Fatalf("head %q, staged %q, unstaged %q", c.Inspection.Identity.Head, c.Staged, c.Unstaged)
	}
}

func TestCaptureRefusesAChangeMadeDuringCapture(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(t *testing.T, dir string){
		"a tracked file edited": func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "crlf.txt"), "a\r\nb\r\nd\r\n")
		},
		"a selected untracked file edited": func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "u", "crlf.txt"), "X\r\ny\r\n")
		},
		"a file staged": func(t *testing.T, dir string) { git(t, dir, "add", "crlf.txt") },
		"a commit made": func(t *testing.T, dir string) { git(t, dir, "commit", "-q", "-m", "during") },
		"a selected ignored file edited": func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "build", "out.bin"), "changed\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, _ := workRepo(t)
			write(t, filepath.Join(dir, ".git", "info", "exclude"), "build/\n")
			write(t, filepath.Join(dir, "build", "out.bin"), "built\n")
			in := newInspector(t)
			in.beforeRecheck = func() { change(t, dir) }
			c, err := capture(in, dir, []string{"u/crlf.txt", "build/out.bin"}, "build/out.bin")
			if !errors.Is(err, ErrSourceChanged) {
				t.Fatalf("capture = %v", err)
			}
			if c.Staged != nil || c.Unstaged != nil || c.Untracked != nil || c.Inspection.Fingerprint != "" {
				t.Fatalf("a refused capture returned content: %+v", c)
			}
		})
	}
}

func TestCaptureRefusesEachInvalidUntrackedPath(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"tracked.txt": "t\n"})
	write(t, filepath.Join(dir, "ok.txt"), "ok\n")
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, nested, "init", "-q")
	write(t, filepath.Join(nested, "inner.txt"), "inner\n")
	in := newInspector(t)
	if r := inspect(t, in, dir); !hasPath(r.Status.Untracked, "nested/") {
		t.Fatalf("a nested repository is not an untracked directory: %v", r.Status.Untracked)
	}
	before := hashTree(t, dir)
	for _, p := range []string{
		"missing.txt", "tracked.txt", "OK.txt", "../ok.txt", "./ok.txt", "u//ok.txt", "/ok.txt",
		filepath.Join(dir, "ok.txt"), "C:/ok.txt", "C:ok.txt", `u\ok.txt`, "ok.txt:stream", "ok.txt::$DATA",
		"CON", "con.txt", "nul", "COM1.log", "LPT9", "ok.txt.", "ok.txt ", "a/./b", "", "nested/", "nested",
		"nested/inner.txt", "ok?.txt", "ok\x01.txt",
	} {
		_, err := capture(in, dir, []string{p})
		if pathCode(err) != PathInvalid {
			t.Errorf("%q: %v", p, err)
		}
	}
	sameTree(t, before, hashTree(t, dir))
}

func TestCaptureRefusesALinkOnTheSelectedPath(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	write(t, filepath.Join(dir, "inside", "file.txt"), "x\n")
	makeLink(t, filepath.Join(dir, "inside"), filepath.Join(dir, "link"))
	in := newInspector(t)
	r := inspect(t, in, dir)
	var selected []string
	for _, p := range r.Status.Untracked {
		if p == "link" || strings.HasPrefix(p, "link/") {
			selected = append(selected, p)
		}
	}
	if len(selected) == 0 {
		t.Fatalf("the link is not untracked: %v", r.Status.Untracked)
	}
	for _, p := range selected {
		if _, err := capture(in, dir, []string{p}); pathCode(err) != PathInvalid {
			t.Errorf("%s: %v", p, err)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink("inside/file.txt", filepath.Join(dir, "file-link")); err != nil {
			t.Fatal(err)
		}
		if _, err := capture(in, dir, []string{"file-link"}); pathCode(err) != PathInvalid {
			t.Errorf("a symlink to a file: %v", err)
		}
	}
}

func TestCaptureRefusesALinkSwappedInAfterInspection(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	write(t, filepath.Join(dir, "d", "f.txt"), "inside\n")
	outside := tempDir(t)
	write(t, filepath.Join(outside, "f.txt"), "inside\n")
	in := newInspector(t)
	in.afterInspect = func() {
		if err := os.RemoveAll(filepath.Join(dir, "d")); err != nil {
			t.Error(err)
		}
		makeLink(t, outside, filepath.Join(dir, "d"))
	}
	if _, err := capture(in, dir, []string{"d/f.txt"}); pathCode(err) != PathInvalid {
		t.Fatalf("capture = %v", err)
	}
}

func TestCaptureRefusesALinkSwappedInBetweenItsCheckAndItsOpen(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	write(t, filepath.Join(dir, "d", "f.txt"), "inside\n")
	outside := tempDir(t)
	write(t, filepath.Join(outside, "f.txt"), "inside\n")
	in := newInspector(t)
	in.beforeOpen = func() {
		if err := os.RemoveAll(filepath.Join(dir, "d")); err != nil {
			t.Error(err)
		}
		makeLink(t, outside, filepath.Join(dir, "d"))
	}
	// The read reaches the outside copy; only the check after it sees the link.
	if _, err := capture(in, dir, []string{"d/f.txt"}); pathCode(err) != PathInvalid {
		t.Fatalf("capture = %v", err)
	}
}

func TestCaptureRefusesAnotherFileRenamedOverTheReadPath(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the open handle blocks the rename on Windows")
	}
	dir := newRepo(t, nil)
	write(t, filepath.Join(dir, "f.txt"), "same\n")
	other := filepath.Join(dir, "other.tmp")
	in := newInspector(t)
	in.afterRead = func() {
		// The same content and mode in another file: only its identity differs.
		write(t, other, "same\n")
		if err := os.Rename(other, filepath.Join(dir, "f.txt")); err != nil {
			t.Error(err)
		}
	}
	if _, err := capture(in, dir, []string{"f.txt"}); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("capture = %v", err)
	}
}

func TestCaptureRefusesADirectoryReplacedByAFileAsChanged(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	write(t, filepath.Join(dir, "d", "f.txt"), "inside\n")
	in := newInspector(t)
	in.afterInspect = func() {
		if err := os.RemoveAll(filepath.Join(dir, "d")); err != nil {
			t.Error(err)
		}
		write(t, filepath.Join(dir, "d"), "now a file\n")
	}
	if _, err := capture(in, dir, []string{"d/f.txt"}); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("capture = %v", err)
	}
}

func TestCaptureRefusesAFileReplacedByADirectoryAfterInspection(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	write(t, filepath.Join(dir, "f.txt"), "file\n")
	in := newInspector(t)
	in.afterInspect = func() {
		if err := os.Remove(filepath.Join(dir, "f.txt")); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "f.txt"), 0o755); err != nil {
			t.Error(err)
		}
	}
	if _, err := capture(in, dir, []string{"f.txt"}); pathCode(err) != PathInvalid {
		t.Fatalf("capture = %v", err)
	}
}

func TestCaptureRefusesCredentialLookingPathsUnlessIncluded(t *testing.T) {
	t.Parallel()
	sensitive := []string{
		".env", ".env.local", "config/.ENV.production", "tls/server.pem", "server.key", "id_rsa", "id_rsa.pub",
		".npmrc", ".netrc", "_netrc", ".pypirc", "cert.p12", "cert.PFX", "vault.kdbx", ".aws/config",
		"home/.ssh/known_hosts", "credentials", "gcloud/credentials.json", ".git-credentials", "id_ed25519",
		"id_ed25519.pub", "id_ecdsa", "id_dsa",
	}
	dir := newRepo(t, nil)
	for _, p := range sensitive {
		write(t, filepath.Join(dir, filepath.FromSlash(p)), "secret "+p+"\n")
	}
	write(t, filepath.Join(dir, "keys.txt"), "not a credential by name\n")
	in := newInspector(t)
	for _, p := range sensitive {
		if _, err := capture(in, dir, []string{p}); pathCode(err) != PathSensitive {
			t.Errorf("%s: %v", p, err)
		}
		c, err := capture(in, dir, []string{p}, p)
		if err != nil || len(c.Untracked) != 1 || string(c.Untracked[0].Content) != "secret "+p+"\n" {
			t.Errorf("%s included: %+v, %v", p, c.Untracked, err)
		}
	}
	if _, err := capture(in, dir, []string{"keys.txt"}); err != nil {
		t.Errorf("keys.txt: %v", err)
	}
}

func TestCaptureRefusesAnIgnoredFileUnlessIncluded(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{".gitignore": "build/\n*.log\n"})
	write(t, filepath.Join(dir, "build", "out.bin"), "\x00built")
	write(t, filepath.Join(dir, "debug.log"), "log\n")
	in := newInspector(t)
	for _, p := range []string{"build/out.bin", "debug.log"} {
		if _, err := capture(in, dir, []string{p}); pathCode(err) != PathSensitive {
			t.Errorf("%s: %v", p, err)
		}
		c, err := capture(in, dir, []string{p}, p)
		if err != nil || len(c.Untracked) != 1 || c.Untracked[0].Path != p {
			t.Errorf("%s included: %+v, %v", p, c.Untracked, err)
		}
	}
	if _, err := capture(in, dir, []string{"build/missing.bin"}, "build/missing.bin"); pathCode(err) != PathInvalid {
		t.Errorf("a missing ignored path: %v", err)
	}
}

func TestCaptureRefusesARepositoryWithBlockers(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	write(t, filepath.Join(dir, ".git", "MERGE_HEAD"), git(t, dir, "rev-parse", "HEAD")+"\n")
	_, err := capture(newInspector(t), dir, nil)
	if e, ok := errors.AsType[*BlockedError](err); !ok || len(e.Blockers) == 0 || e.Blockers[0].Code != BlockMerge {
		t.Fatalf("capture = %v", err)
	}
}

func TestCaptureRefusesAnUntrackedFileOverTheCap(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	write(t, filepath.Join(dir, "big.bin"), strings.Repeat("x", maxFileSize+1))
	if _, err := capture(newInspector(t), dir, []string{"big.bin"}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("capture = %v", err)
	}
}

func TestGitOutputOverItsCapIsTooLarge(t *testing.T) {
	t.Parallel()
	in := newInspector(t)
	if _, err := in.Git.runCapped(context.Background(), "", "", 5, "version"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("capped run = %v", err)
	}
	if out, err := in.Git.runCapped(context.Background(), "", "", 1<<10, "version"); err != nil || !bytes.HasPrefix(out, []byte("git version")) {
		t.Fatalf("run = %q, %v", out, err)
	}
}

func TestBadPathRefusesAGitDirectoryAndItsShortName(t *testing.T) {
	t.Parallel()
	for _, p := range []string{".git", ".GIT/config", "a/.Git/hooks/x", "GIT~1/config", "a/git~12/x", "Git~3"} {
		if badPath(p) == "" {
			t.Errorf("%q is accepted", p)
		}
	}
	for _, p := range []string{".gitignore", "a/.github/x", "git~", "git~a", "xgit~1", "git~1x", ".git.txt"} {
		if reason := badPath(p); reason != "" {
			t.Errorf("%q: %s", p, reason)
		}
	}
}

func TestIgnoredFilesAreListedWithinTheCap(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{".gitignore": "node_modules/\n"})
	for i := range 20 {
		write(t, filepath.Join(dir, "node_modules", fmt.Sprintf("module-%02d.js", i)), "x\n")
	}
	in := newInspector(t)
	mirror := tempDir(t)
	if _, err := in.inspect(context.Background(), dir, mirror); err != nil {
		t.Fatal(err)
	}
	if _, err := in.ignored(context.Background(), mirror, dir, []string{"node_modules"}, 64); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ignored over the cap = %v", err)
	}
	found, err := in.ignored(context.Background(), mirror, dir, []string{"node_modules/module-03.js"}, 1<<10)
	if err != nil || !found["node_modules/module-03.js"] {
		t.Fatalf("ignored = %v, %v", found, err)
	}
}
