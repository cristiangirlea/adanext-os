package source

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const markerHook = `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo hook > .ada-hook-marker"}]}]}}`

func finding(r Inspection, code string) (Finding, bool) {
	for _, f := range r.Findings {
		if f.Code == code {
			return f, true
		}
	}
	return Finding{}, false
}

func TestInspectionReportsProviderConfigurationAndHooksWithoutBlocking(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"README.md":             "hello\n",
		"CLAUDE.md":             "instructions\n",
		".claude/settings.json": markerHook,
		".claude/hooks/mark.sh": "echo hook > .ada-hook-marker\n",
		".codex/config.toml":    "notify = [\"true\"]\n",
		"sub/.mcp.json":         "{}\n",
		"src/main.go":           "package main\n",
	})
	write(t, filepath.Join(repo, ".mcp.json"), "{}\n") // untracked, still in a snapshot that selects it
	r := inspect(t, newInspector(t), repo)
	f, ok := finding(r, FindingProviderConfiguration)
	if !ok {
		t.Fatalf("no provider configuration finding: %+v", r.Findings)
	}
	want := []string{".claude/hooks/mark.sh", ".claude/settings.json", ".codex/config.toml", ".mcp.json", "sub/.mcp.json"}
	if !slices.Equal(f.Paths, want) || f.Count != len(want) {
		t.Fatalf("paths = %v (%d), want %v", f.Paths, f.Count, want)
	}
	if len(r.Blockers) != 0 {
		t.Fatalf("provider configuration blocked the import: %+v", r.Blockers)
	}
	if !hasPath(r.Files, "CLAUDE.md") {
		t.Fatalf("CLAUDE.md is not listed as instructions: %v", r.Files)
	}
	var untracked []string
	for _, p := range r.ProviderPaths {
		if p.Untracked {
			untracked = append(untracked, p.Path)
		}
	}
	if !slices.Equal(untracked, []string{".mcp.json"}) {
		t.Fatalf("untracked provider paths = %v", untracked)
	}
}

func TestARepositoryWithoutProviderConfigurationHasNoFinding(t *testing.T) {
	r := inspect(t, newInspector(t), newRepo(t, map[string]string{"README.md": "hello\n", "docs/claude.md": "not config\n"}))
	if _, ok := finding(r, FindingProviderConfiguration); ok || r.Findings == nil {
		t.Fatalf("findings = %#v", r.Findings)
	}
}

func TestIsProviderPathMatchesWhatTheCLIsLoad(t *testing.T) {
	for p, want := range map[string]bool{
		".claude/settings.json": true, ".claude/settings.local.json": true, ".Claude/Settings.json": true,
		".codex/config.toml": true, ".mcp.json": true, "a/b/.mcp.json": true, "pkg/.claude/agents/x.md": true, ".claude": true,
		"CLAUDE.md": false, "claude/settings.json": false, ".claudeignore": false, "mcp.json": false, "x.mcp.json": false,
	} {
		if got := IsProviderPath(p); got != want {
			t.Errorf("IsProviderPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestProviderChangesAreFoundByWalkingTheWorktree(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".claude", "settings.json"), "{}")
	write(t, filepath.Join(root, ".codex", "config.toml"), "a = 1\n")
	write(t, filepath.Join(root, "src", "main.go"), "package main\n")
	write(t, filepath.Join(root, ".git"), "gitdir: elsewhere\n")
	before, truncated, err := ProviderState(root, 1000)
	if err != nil || truncated || len(before) != 2 {
		t.Fatalf("before = %v %v %v", before, truncated, err)
	}
	write(t, filepath.Join(root, ".claude", "settings.json"), markerHook)
	if err := os.Remove(filepath.Join(root, ".codex", "config.toml")); err != nil {
		t.Fatal(err)
	}
	// An ignored file is loaded all the same, so the walk does not ask git.
	write(t, filepath.Join(root, ".claude", "settings.local.json"), `{"permissions":{"allow":["Bash"]}}`)
	write(t, filepath.Join(root, "src", "main.go"), "package main // changed\n")
	after, truncated, err := ProviderState(root, 1000)
	if err != nil || truncated {
		t.Fatalf("after: %v %v", truncated, err)
	}
	got := DiffProviderState(before, after)
	want := []ProviderChange{{".claude/settings.json", "altered"}, {".claude/settings.local.json", "added"}, {".codex/config.toml", "removed"}}
	if !slices.Equal(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if again := DiffProviderState(after, after); len(again) != 0 {
		t.Fatalf("an unchanged tree reports %v", again)
	}
}

func TestAProviderLinkIsRecordedAsALinkAndNotFollowed(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	write(t, outside, "{}")
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".claude", "settings.json")); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	state, _, err := ProviderState(root, 1000)
	if err != nil || !strings.HasPrefix(state[".claude/settings.json"], "link:") {
		t.Fatalf("state = %v, %v", state, err)
	}
}

func TestAWalkPastItsLimitIsMarkedTruncated(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d"} {
		write(t, filepath.Join(root, name), "x")
	}
	if _, truncated, err := ProviderState(root, 2); err != nil || !truncated {
		t.Fatalf("truncated = %v, %v", truncated, err)
	}
}
