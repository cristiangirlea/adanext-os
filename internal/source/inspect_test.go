package source

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// armMarkers writes scripts under markers that each leave <name>.marker when
// run, and returns the command git would run for each.
func armMarkers(t *testing.T, markers string, names ...string) map[string]string {
	t.Helper()
	commands := map[string]string{}
	for _, name := range names {
		script := filepath.Join(markers, name+".sh")
		body := "#!/bin/sh\n: > '" + shPath(filepath.Join(markers, name+".marker")) + "'\n"
		if strings.HasSuffix(name, "clean") || name == "smudge" {
			body += "exec cat\n" // a filter passes its content through
		}
		write(t, script, body)
		commands[name] = "sh '" + shPath(script) + "'"
	}
	return commands
}

func markersWritten(t *testing.T, markers string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(markers, "*.marker"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range found {
		found[i] = strings.TrimSuffix(filepath.Base(found[i]), ".marker")
	}
	sort.Strings(found)
	return found
}

// armedRepo is a repository whose config names a clean filter, a process
// filter, fsmonitor, textconv, diff.external and core.sshCommand, with
// pre-commit and post-index-change hooks, each writing a marker.
func armedRepo(t *testing.T, markers string) string {
	t.Helper()
	dir := newRepo(t, map[string]string{
		".gitattributes": "*.txt filter=ada diff=ada\n*.dat filter=adaproc\n",
		"file.txt":       "tracked\n",
		"data.dat":       "data\n",
	})
	cmd := armMarkers(t, markers, "clean", "smudge", "process", "textconv", "external", "fsmonitor", "ssh", "pre-commit", "post-index-change")
	for key, value := range map[string]string{
		"filter.ada.clean":       cmd["clean"],
		"filter.ada.smudge":      cmd["smudge"],
		"filter.adaproc.process": cmd["process"],
		"diff.ada.textconv":      cmd["textconv"],
		"diff.external":          cmd["external"],
		"core.fsmonitor":         cmd["fsmonitor"],
		"core.sshCommand":        cmd["ssh"],
	} {
		git(t, dir, "config", key, value)
	}
	for _, hook := range []string{"pre-commit", "post-index-change"} {
		path := filepath.Join(dir, ".git", "hooks", hook)
		write(t, path, "#!/bin/sh\n: > '"+shPath(filepath.Join(markers, hook+".marker"))+"'\n")
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, "untracked.txt"), "new\n")
	backdate(t, filepath.Join(dir, "file.txt"))
	backdate(t, filepath.Join(dir, "data.dat"))
	return dir
}

func TestInspectRunsNothingTheRepositoryConfigures(t *testing.T) {
	t.Parallel()
	// The control: the user's own git, given the same repository, runs its commands.
	controlMarkers := tempDir(t)
	control := armedRepo(t, controlMarkers)
	fixtureGit(control, "status") // fails once the process filter dies; its markers are what count
	if got := markersWritten(t, controlMarkers); !hasPath(got, "fsmonitor") || !hasPath(got, "clean") && !hasPath(got, "process") {
		t.Fatalf("the fixture is not armed: plain git status wrote %v", got)
	}

	markers := tempDir(t)
	dir := armedRepo(t, markers)
	before := hashTree(t, dir)
	r := inspect(t, newInspector(t), dir)
	if got := markersWritten(t, markers); len(got) != 0 {
		t.Fatalf("inspection ran repository commands: %v", got)
	}
	sameTree(t, before, hashTree(t, dir))
	if len(r.Blockers) != 0 {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
	if !hasPath(r.Status.Untracked, "untracked.txt") || len(r.Status.Unstaged) != 0 {
		t.Fatalf("status = %+v", r.Status)
	}
}

func TestInspectDoesNotEnterAPopulatedSubmodule(t *testing.T) {
	t.Parallel()
	build := func(markers string) string {
		sub := newRepo(t, map[string]string{".gitattributes": "* filter=ada\n", "inner.txt": "inner\n"})
		super := newRepo(t, nil)
		git(t, super, "submodule", "add", "-q", fileURL(sub), "sub")
		git(t, super, "commit", "-q", "-m", "add submodule")
		cmd := armMarkers(t, markers, "sub-clean", "sub-fsmonitor")
		subGit := filepath.Join(super, ".git", "modules", "sub")
		git(t, subGit, "config", "filter.ada.clean", cmd["sub-clean"])
		git(t, subGit, "config", "core.fsmonitor", cmd["sub-fsmonitor"])
		backdate(t, filepath.Join(super, "sub", "inner.txt"))
		return super
	}
	controlMarkers := tempDir(t)
	git(t, build(controlMarkers), "status")
	if got := markersWritten(t, controlMarkers); len(got) == 0 {
		t.Fatal("the fixture is not armed: plain git status wrote no marker in the submodule")
	}

	markers := tempDir(t)
	super := build(markers)
	before := hashTree(t, super)
	r := inspect(t, newInspector(t), super)
	if got := markersWritten(t, markers); len(got) != 0 {
		t.Fatalf("inspection ran submodule commands: %v", got)
	}
	sameTree(t, before, hashTree(t, super))
	if b, ok := blocker(r, BlockSubmodule); !ok || !hasPath(b.Paths, "sub") {
		t.Fatalf("blockers = %+v", r.Blockers)
	}
}

func TestMirrorConfigHoldsOnlyAllowListedKeys(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	git(t, dir, "config", "filter.repo.clean", "repo-clean")
	git(t, dir, "config", "core.fsmonitor", "repo-fsmonitor")
	git(t, dir, "config", "core.autocrlf", "input")
	git(t, dir, "config", "core.hooksPath", "/repo/hooks")
	cfgDir := tempDir(t)
	included := filepath.Join(cfgDir, "included")
	write(t, included, "[core]\n\tfilemode = false\n[filter \"inc\"]\n\tclean = included-clean\n")
	system := filepath.Join(cfgDir, "system")
	write(t, system, "[filter \"sys\"]\n\tclean = system-clean\n[core]\n\tfsmonitor = system-fsmonitor\n\teol = lf\n\tautocrlf = true\n")
	global := filepath.Join(cfgDir, "global")
	write(t, global, "[include]\n\tpath = "+shPath(included)+"\n[filter \"glob\"]\n\tprocess = global-process\n[core]\n\ttrustctime = false\n\tsshCommand = global-ssh\n[diff]\n\texternal = global-diff\n")
	in := newInspector(t)
	in.Config = ConfigFiles{System: []string{system}, Global: []string{global}}
	mirror := tempDir(t)
	if _, err := in.inspect(context.Background(), dir, mirror); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(mirror, "config"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"core.repositoryformatversion": true, "core.bare": true, "core.excludesfile": true, "core.attributesfile": true, "extensions.objectformat": true}
	for _, key := range mirrorKeys {
		allowed[strings.ToLower(key)] = true
	}
	for _, e := range c.entries {
		if key := e.section + "." + e.name; e.subsection != "" || !allowed[key] {
			t.Errorf("mirror config holds %s.%s.%s = %q", e.section, e.subsection, e.name, e.value)
		}
	}
	for key, want := range map[string]string{"core.autocrlf": "input", "core.eol": "lf", "core.trustctime": "false"} {
		if got, _ := c.get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if _, ok := c.get("core.checkStat"); ok {
		t.Error("core.checkStat came through an include")
	}
}
