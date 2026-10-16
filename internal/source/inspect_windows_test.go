//go:build windows

package source

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnAutocrlfCheckoutInspectsCleanAndAOneLineEditIsOneLine(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, map[string]string{"text.txt": "one\ntwo\nthree\nfour\n", "run.sh": "#!/bin/sh\necho hi\n"})
	git(t, dir, "config", "core.autocrlf", "true")
	git(t, dir, "config", "core.filemode", "false")
	git(t, dir, "update-index", "--chmod=+x", "run.sh")
	git(t, dir, "commit", "-q", "-m", "executable")
	for _, name := range []string{"text.txt", "run.sh"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "checkout", "--", ".")
	text := filepath.Join(dir, "text.txt")
	data, err := os.ReadFile(text)
	if err != nil || !bytes.Contains(data, []byte("\r\n")) {
		t.Fatalf("the checkout has no CRLF: %q %v", data, err)
	}
	// Stale stat data makes git compare content, line endings and modes.
	backdate(t, text)
	backdate(t, filepath.Join(dir, "run.sh"))
	in := newInspector(t)
	r := inspect(t, in, dir)
	if len(r.Status.Staged) != 0 || len(r.Status.Unstaged) != 0 || len(r.Status.Untracked) != 0 {
		t.Fatalf("a clean checkout inspects as %+v", r.Status)
	}

	if err := os.WriteFile(text, bytes.Replace(data, []byte("two"), []byte("TWO"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	mirror := tempDir(t)
	r, err = in.inspect(context.Background(), dir, mirror)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Status.Unstaged) != 1 || r.Status.Unstaged[0] != (Change{"text.txt", "M"}) {
		t.Fatalf("status = %+v", r.Status)
	}
	out, err := in.Git.run(context.Background(), mirror, dir, "diff")
	if err != nil {
		t.Fatal(err)
	}
	var removed, added []string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"):
		case strings.HasPrefix(line, "-"):
			removed = append(removed, line)
		case strings.HasPrefix(line, "+"):
			added = append(added, line)
		}
	}
	if len(removed) != 1 || len(added) != 1 || removed[0] != "-two" || added[0] != "+TWO" {
		t.Fatalf("diff:\n%s", out)
	}
}

func TestTheUsersGitCanReplaceItsIndexWhileAdaHoldsItOpen(t *testing.T) {
	t.Parallel()
	dir := newRepo(t, nil)
	held, err := openShared(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	write(t, filepath.Join(dir, "new.txt"), "new\n")
	git(t, dir, "add", "new.txt")
	if !strings.Contains(git(t, dir, "ls-files"), "new.txt") {
		t.Fatal("the index was not replaced")
	}
}
