package source

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// maxDriftCommits bounds the new commits a drift lists; NewCommitCount keeps the total.
const maxDriftCommits = 100

// Baseline is what a snapshot recorded of the original: its base commit
// (empty on an unborn branch), its branch (empty when detached) and its
// fingerprint's per-path entries.
type Baseline struct {
	BaseCommit, Branch string
	Paths              []PathEntry
}

// Drift is how the original differs from a snapshot of it, each kind apart.
// NewCommits are those on the snapshot's branch (on HEAD when the snapshot
// was detached or the branch is gone) that the base does not reach, newest
// first. Changed are the paths whose entries differ from the snapshot's,
// other than Untracked, the untracked paths the snapshot did not have.
type Drift struct {
	Drift          bool      `json:"drift"`
	BaseCommit     string    `json:"base_commit"`
	Head           string    `json:"head"`
	HeadMoved      bool      `json:"head_moved"`
	NewCommits     []Commit  `json:"new_commits"`
	NewCommitCount int       `json:"new_commit_count"`
	SnapshotBranch string    `json:"snapshot_branch"`
	Branch         string    `json:"branch"`
	Detached       bool      `json:"detached"`
	BranchChanged  bool      `json:"branch_changed"`
	Changed        []string  `json:"changed"`
	Untracked      []string  `json:"untracked"`
	Blockers       []Blocker `json:"blockers"`
}

// Drift inspects the repository at path again, through a mirror as Inspect
// does, and compares it with base. Every blocker the inspection finds is one
// that appeared: a snapshot is never taken with one. A blocker that stops the
// inspection leaves the commits and paths unread.
func (in *Inspector) Drift(ctx context.Context, path string, base Baseline) (Drift, error) {
	if base.BaseCommit != "" && ((len(base.BaseCommit) != 40 && len(base.BaseCommit) != 64) || !isHex(base.BaseCommit)) {
		return Drift{}, fmt.Errorf("base commit %q is not an object ID", base.BaseCommit)
	}
	if err := os.MkdirAll(in.MirrorRoot, 0o700); err != nil {
		return Drift{}, err
	}
	mirror, err := os.MkdirTemp(in.MirrorRoot, "d")
	if err != nil {
		return Drift{}, err
	}
	defer os.RemoveAll(mirror)
	r, src, err := in.inspectMirror(ctx, path, mirror)
	if err != nil {
		return Drift{}, err
	}
	id := r.Identity
	d := Drift{BaseCommit: base.BaseCommit, Head: id.Head, HeadMoved: id.Head != base.BaseCommit, NewCommits: []Commit{},
		SnapshotBranch: base.Branch, Branch: id.Branch, Detached: id.Detached, BranchChanged: id.Branch != base.Branch,
		Changed: []string{}, Untracked: []string{}, Blockers: r.Blockers}
	if src != nil {
		if d.NewCommits, d.NewCommitCount, err = in.newCommits(ctx, mirror, id.Head, base); err != nil {
			return Drift{}, err
		}
		d.Changed, d.Untracked = comparePaths(base.Paths, r.Paths)
	}
	d.Drift = d.HeadMoved || d.BranchChanged || d.NewCommitCount > 0 || len(d.Changed)+len(d.Untracked)+len(d.Blockers) > 0
	return d, nil
}

// newCommits lists, through the mirror, the commits on the snapshot's branch,
// or on HEAD, that the base does not reach. A base the original no longer
// has gives none: what is new cannot be told.
func (in *Inspector) newCommits(ctx context.Context, mirror, head string, base Baseline) ([]Commit, int, error) {
	tip := "HEAD"
	if base.Branch != "" {
		if _, err := in.Git.run(ctx, mirror, "", "rev-parse", "--verify", "-q", "refs/heads/"+base.Branch+"^{commit}"); err == nil {
			tip = "refs/heads/" + base.Branch
		}
	}
	if tip == "HEAD" && head == "" {
		return []Commit{}, 0, nil
	}
	revs := []string{tip}
	if base.BaseCommit != "" {
		if _, err := in.Git.run(ctx, mirror, "", "cat-file", "-e", base.BaseCommit+"^{commit}"); err != nil {
			return []Commit{}, 0, nil
		}
		revs = append(revs, "^"+base.BaseCommit)
	}
	out, err := in.Git.run(ctx, mirror, "", append([]string{"rev-list", "--count"}, append(revs, "--")...)...)
	if err != nil {
		return nil, 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, 0, fmt.Errorf("rev-list count %q: %w", out, err)
	}
	if count == 0 {
		return []Commit{}, 0, nil
	}
	args := []string{"log", "-n", strconv.Itoa(maxDriftCommits), "--no-decorate", "--format=%H%x1f%P%x1f%an%x1f%ae%x1f%aI%x1f%s%x1e"}
	if out, err = in.Git.run(ctx, mirror, "", append(append(args, revs...), "--")...); err != nil {
		return nil, 0, err
	}
	return parseLog(out), count, nil
}

// comparePaths lists the paths whose entries differ between was and now: a
// path only now present and untracked is new; any other difference, an
// untracked path edited or gone included, is a change.
func comparePaths(was, now []PathEntry) (changed, untracked []string) {
	before := map[string]PathEntry{}
	for _, e := range was {
		before[e.Path] = e
	}
	changed, untracked = []string{}, []string{}
	for _, e := range now {
		b, ok := before[e.Path]
		delete(before, e.Path)
		switch {
		case !ok && e.Untracked:
			untracked = append(untracked, e.Path)
		case !ok || b != e:
			changed = append(changed, e.Path)
		}
	}
	for p := range before {
		changed = append(changed, p)
	}
	sort.Strings(changed)
	sort.Strings(untracked)
	return changed, untracked
}
