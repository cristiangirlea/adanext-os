package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The test binary copied as git (or git.exe) is a fake that prints the
// version in version.txt beside it: Resolve gives git no Ada environment.
func init() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if name := strings.TrimSuffix(filepath.Base(exe), ".exe"); name != "git" {
		return
	}
	version, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "version.txt"))
	if err != nil {
		os.Exit(2)
	}
	os.Stdout.Write(version)
	os.Exit(0)
}

// fixtureEnv is the environment fixtures are built with: the system git,
// with the machine's own system and global config left out.
func fixtureEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Ada Test", "GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada Test", "GIT_COMMITTER_EMAIL=ada@example.com")
}

func fixtureGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-c", "init.defaultBranch=main", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	cmd.Env = fixtureEnv()
	return cmd.CombinedOutput()
}

// git runs the system git in dir to build or change a fixture.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := fixtureGit(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo is a repository with one commit of the given files.
func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := tempDir(t)
	git(t, dir, "init", "-q")
	if len(files) == 0 {
		files = map[string]string{"README.md": "hello\n"}
	}
	for name, content := range files {
		write(t, filepath.Join(dir, filepath.FromSlash(name)), content)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "first")
	return dir
}

// backdate moves a file's times an hour back, so its index stat data is
// stale and git must compare its content.
func backdate(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func newInspector(t *testing.T) *Inspector {
	t.Helper()
	g, err := Resolve(context.Background(), tempDir(t))
	if err != nil {
		t.Fatalf("resolve git: %v", err)
	}
	return &Inspector{Git: g, MirrorRoot: tempDir(t)}
}

func inspect(t *testing.T, in *Inspector, path string) Inspection {
	t.Helper()
	r, err := in.Inspect(context.Background(), path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	return r
}

// hashTree hashes every file under dir, .git included: content for files,
// the target for links.
func hashTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	sums := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0:
			target, _ := os.Readlink(path)
			sums[rel] = "link:" + target
			if d.IsDir() {
				return fs.SkipDir
			}
		case d.IsDir():
			sums[rel] = "dir"
		default:
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			h := sha256.New()
			if _, err := io.Copy(h, f); err != nil {
				return err
			}
			sums[rel] = hex.EncodeToString(h.Sum(nil))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sums
}

func sameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	for path, sum := range before {
		if after[path] != sum {
			t.Errorf("%s changed: %s -> %s", path, sum, after[path])
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s appeared", path)
		}
	}
}

func blocker(r Inspection, code string) (Blocker, bool) {
	for _, b := range r.Blockers {
		if b.Code == code {
			return b, true
		}
	}
	return Blocker{}, false
}

func hasPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// shPath is a path the sh that runs git's commands understands.
func shPath(p string) string { return filepath.ToSlash(p) }

func fileURL(p string) string {
	u := filepath.ToSlash(p)
	if runtime.GOOS == "windows" {
		u = "/" + u
	}
	return "file://" + u
}
