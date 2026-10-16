// Package source reads a user's repository without running anything the
// repository configures: the layout, config and index are read in Go, and git
// runs only against a mirror git directory Ada writes (decisions log,
// 2026-10-16).
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Git is the git binary resolved once at start-up, with the scratch
// directories every call uses.
type Git struct {
	Path    string
	Version string
	home    string // HOME for git: empty, so no user file is found through it.
	hooks   string // core.hooksPath: an empty directory.
}

// Resolve finds git on PATH as an absolute path, refuses a version without
// the 2024 clone fixes, and prepares its scratch directories under scratch.
func Resolve(ctx context.Context, scratch string) (*Git, error) {
	path, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git not found on PATH: %w", err)
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("git resolved to a relative path %q", path)
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(path), ".exe") {
		// A .bat or .cmd runs through cmd.exe, which reads & in its arguments as a command.
		return nil, fmt.Errorf("git resolved to %q, not an .exe", path)
	}
	g := &Git{Path: path, home: filepath.Join(scratch, "home"), hooks: filepath.Join(scratch, "hooks")}
	for _, dir := range []string{g.home, g.hooks} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	out, err := g.command(ctx, "", "", "version").Output()
	if err != nil {
		return nil, fmt.Errorf("git version: %w", err)
	}
	g.Version = strings.TrimSpace(string(out))
	if err := checkVersion(g.Version); err != nil {
		return nil, err
	}
	return g, nil
}

// fixed lists, per 2.x minor release, the first patch release carrying the
// fixes of 2024-05-14 (CVE-2024-32004, CVE-2024-32465, GIT_NO_LAZY_FETCH).
var fixed = map[int]int{39: 4, 40: 2, 41: 1, 42: 2, 43: 4, 44: 1, 45: 1}

func checkVersion(output string) error {
	fields := strings.Fields(output)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return fmt.Errorf("unrecognised git version output %q", output)
	}
	parts := strings.SplitN(fields[2], ".", 4)
	var n [3]int
	for i := range n {
		if i >= len(parts) {
			return fmt.Errorf("unrecognised git version %q", fields[2])
		}
		v, err := strconv.Atoi(parts[i])
		if err != nil {
			return fmt.Errorf("unrecognised git version %q", fields[2])
		}
		n[i] = v
	}
	major, minor, patch := n[0], n[1], n[2]
	ok := major > 2 || (major == 2 && (minor > 45 || (fixed[minor] != 0 && patch >= fixed[minor])))
	if !ok {
		return fmt.Errorf("git %d.%d.%d lacks the 2024 clone fixes; 2.39.4, 2.40.2, 2.41.1, 2.42.2, 2.43.4, 2.44.1, 2.45.1 or later is required", major, minor, patch)
	}
	return nil
}

// allowedEnv is all of Ada's environment a git process sees.
var allowedEnv = []string{"PATH", "SystemRoot", "TEMP", "TMP", "LANG"}

func (g *Git) env() []string {
	var env []string
	for _, name := range allowedEnv {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"HOME="+g.home,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_ATTR_NOSYSTEM=1",
	)
}

// command builds a git call against the mirror git directory and the
// original work tree; both empty for a call that needs no repository.
func (g *Git) command(ctx context.Context, mirror, workTree string, args ...string) *exec.Cmd {
	argv := []string{"--no-pager",
		"-c", "core.hooksPath=" + g.hooks,
		"-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false",
		"-c", "credential.helper=",
		"-c", "protocol.allow=never",
	}
	if len(args) > 0 && (args[0] == "status" || args[0] == "diff") {
		argv = append(argv, "-c", "diff.ignoreSubmodules=all")
	}
	if mirror != "" {
		argv = append(argv, "--git-dir="+mirror, "--work-tree="+workTree)
	}
	if len(args) > 0 {
		argv = append(argv, args[0])
		switch args[0] {
		case "diff":
			argv = append(argv, "--no-ext-diff", "--no-textconv", "--ignore-submodules=all")
		case "status":
			argv = append(argv, "--ignore-submodules=all")
		}
		argv = append(argv, args[1:]...)
	}
	cmd := exec.CommandContext(ctx, g.Path, argv...)
	cmd.Env = g.env()
	cmd.Dir = g.home
	return cmd
}

// run returns a call's standard output, or an error carrying its standard error.
func (g *Git) run(ctx context.Context, mirror, workTree string, args ...string) ([]byte, error) {
	cmd := g.command(ctx, mirror, workTree, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
}
