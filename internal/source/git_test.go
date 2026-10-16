package source

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestGitVersionsWithoutTheCloneFixesAreRefused(t *testing.T) {
	t.Parallel()
	for version, ok := range map[string]bool{
		"git version 2.39.3":                 false,
		"git version 2.39.4":                 true,
		"git version 2.40.1":                 false,
		"git version 2.40.2":                 true,
		"git version 2.41.0":                 false,
		"git version 2.41.1":                 true,
		"git version 2.42.1":                 false,
		"git version 2.42.2":                 true,
		"git version 2.43.3":                 false,
		"git version 2.43.4":                 true,
		"git version 2.44.0":                 false,
		"git version 2.44.1":                 true,
		"git version 2.45.0":                 false,
		"git version 2.45.1":                 true,
		"git version 2.46.0":                 true,
		"git version 2.49.0.windows.1":       true,
		"git version 2.39.5 (Apple Git-154)": true,
		"git version 2.38.9":                 false,
		"git version 1.9.9":                  false,
		"git version 3.0.0":                  true,
		"git version 2.45":                   false,
		"not git":                            false,
	} {
		if err := checkVersion(version); (err == nil) != ok {
			t.Errorf("%q: accepted %v, want %v (%v)", version, err == nil, ok, err)
		}
	}
}

// fakeGit puts the test binary on an otherwise empty PATH as a git that
// reports version.
func fakeGit(t *testing.T, version string) string {
	t.Helper()
	dir := tempDir(t)
	name := "git"
	if runtime.GOOS == "windows" {
		name = "git.exe"
	}
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "version.txt"), version+"\n")
	t.Setenv("PATH", dir)
	return filepath.Join(dir, name)
}

func TestResolveRefusesAnOldGitAtStartUp(t *testing.T) {
	fakeGit(t, "git version 2.45.0")
	if _, err := Resolve(context.Background(), tempDir(t)); err == nil || !strings.Contains(err.Error(), "2.45.0") {
		t.Fatalf("resolve = %v", err)
	}
}

func TestResolveAcceptsAFixedGitByAbsolutePath(t *testing.T) {
	path := fakeGit(t, "git version 2.39.4")
	g, err := Resolve(context.Background(), tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(g.Path) || !strings.EqualFold(g.Path, path) || g.Version != "git version 2.39.4" {
		t.Fatalf("git = %+v", g)
	}
}

func TestResolveReportsAMissingGit(t *testing.T) {
	t.Setenv("PATH", tempDir(t))
	if _, err := Resolve(context.Background(), tempDir(t)); err == nil {
		t.Fatal("resolved git from an empty PATH")
	}
}

func TestEveryGitCallGetsTheSafeEnvironmentAndOptions(t *testing.T) {
	t.Setenv("GIT_DIR", "/user/repo/.git")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor=evil'")
	t.Setenv("ADA_SECRET", "x")
	t.Setenv("LANG", "C")
	g := &Git{Path: "/usr/bin/git", home: "/ada/home", hooks: "/ada/hooks"}
	names := map[string]string{}
	cmd := g.command(context.Background(), "/ada/mirror", "/user/repo", "status", "-z")
	for _, kv := range cmd.Env {
		name, value, _ := strings.Cut(kv, "=")
		names[name] = value
	}
	for name := range names {
		if !slices.Contains(allowedEnv, name) && !strings.HasPrefix(name, "GIT_") && name != "HOME" {
			t.Errorf("environment passes %s", name)
		}
	}
	for name, want := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull, "HOME": "/ada/home",
		"GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0", "GIT_NO_LAZY_FETCH": "1",
		"GIT_NO_REPLACE_OBJECTS": "1", "GIT_ATTR_NOSYSTEM": "1", "LANG": "C",
	} {
		if names[name] != want {
			t.Errorf("%s = %q, want %q", name, names[name], want)
		}
	}
	for _, leaked := range []string{"GIT_DIR", "GIT_CONFIG_PARAMETERS", "ADA_SECRET"} {
		if _, ok := names[leaked]; ok {
			t.Errorf("%s reached git", leaked)
		}
	}
	args := strings.Join(cmd.Args[1:], " ")
	for _, want := range []string{
		"--no-pager -c core.hooksPath=/ada/hooks -c core.fsmonitor=false -c core.untrackedCache=false -c credential.helper= -c protocol.allow=never -c diff.ignoreSubmodules=all --git-dir=/ada/mirror --work-tree=/user/repo status --ignore-submodules=all -z",
	} {
		if args != want {
			t.Errorf("status args = %s\nwant %s", args, want)
		}
	}
	diff := strings.Join(g.command(context.Background(), "/m", "/w", "diff", "--cached").Args[1:], " ")
	if !strings.Contains(diff, " diff --no-ext-diff --no-textconv --ignore-submodules=all --cached") || !strings.Contains(diff, "-c diff.ignoreSubmodules=all") {
		t.Errorf("diff args = %s", diff)
	}
}
