package source

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FindingProviderConfiguration is the non-blocking finding for provider configuration and hooks
// in the repository: what claude or codex would load from a worktree of it (decisions log,
// 2026-10-25). CLAUDE.md and AGENTS.md are instructions and stay in Files.
const FindingProviderConfiguration = "provider_configuration"

// Finding is something inspection reports that does not block an import.
type Finding struct {
	Code  string   `json:"code"`
	Paths []string `json:"paths"`
	Count int      `json:"count"`
}

// ProviderPath is one provider configuration path of the work tree.
type ProviderPath struct {
	Path      string
	Untracked bool
}

// IsProviderPath reports whether a slash-separated work-tree path is provider configuration: any
// path through or naming .claude or .codex (a link there is loaded too), or a file named .mcp.json. Case is ignored, as on
// the file systems where the CLIs would load it either way.
func IsProviderPath(p string) bool {
	parts := strings.Split(p, "/")
	for _, part := range parts {
		if strings.EqualFold(part, ".claude") || strings.EqualFold(part, ".codex") {
			return true
		}
	}
	return strings.EqualFold(parts[len(parts)-1], ".mcp.json")
}

// findProvider records the provider paths among the index's entries and the untracked files.
func (r *Inspection) findProvider(idx *index) {
	seen := map[string]bool{}
	for _, e := range idx.entries {
		if IsProviderPath(e.path) && !seen[e.path] {
			seen[e.path] = true
			r.ProviderPaths = append(r.ProviderPaths, ProviderPath{Path: e.path})
		}
	}
	for _, p := range r.Status.Untracked {
		if IsProviderPath(p) && !seen[p] {
			seen[p] = true
			r.ProviderPaths = append(r.ProviderPaths, ProviderPath{Path: p, Untracked: true})
		}
	}
	if len(r.ProviderPaths) == 0 {
		return
	}
	sort.Slice(r.ProviderPaths, func(i, j int) bool { return r.ProviderPaths[i].Path < r.ProviderPaths[j].Path })
	paths := make([]string, 0, len(r.ProviderPaths))
	for _, p := range r.ProviderPaths {
		paths = append(paths, p.Path)
	}
	f := Finding{Code: FindingProviderConfiguration, Paths: paths, Count: len(paths)}
	if len(paths) > maxBlockerPaths {
		f.Paths = paths[:maxBlockerPaths]
	}
	r.Findings = append(r.Findings, f)
}

// maxProviderFile is how much of a provider file is hashed; a larger one is known by its size.
const maxProviderFile = 16 << 20

// ProviderState walks a worktree, without following links and skipping the root's .git, and
// returns each provider path with what it holds: a regular file's content hash, a link's target,
// or another type's mode. The walk counts every entry it visits; past limit it stops and reports
// unknown. A link at a provider path reports unknown too: what it leads to can change while the
// link stays the same. It reads the file system, not git, since an ignored settings file is loaded
// as well.
func ProviderState(root string, limit int) (state map[string]string, unknown bool, err error) {
	state = map[string]string{}
	visited, linked := 0, false
	stop := errors.New("limit")
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if visited++; visited > limit {
			return stop
		}
		if d.IsDir() || !IsProviderPath(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case isLink(path, info):
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			state[rel] = "link:" + target
			linked = true
		case info.Mode().IsRegular():
			sum, err := hashProviderFile(path)
			if err != nil {
				return err
			}
			state[rel] = sum
		default:
			state[rel] = "other:" + info.Mode().Type().String()
		}
		return nil
	})
	if errors.Is(err, stop) {
		return state, true, nil
	}
	return state, linked, err
}

func hashProviderFile(path string) (string, error) {
	f, err := openShared(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxProviderFile+1))
	if err != nil {
		return "", err
	}
	if n > maxProviderFile {
		info, err := f.Stat()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("large:%d:%s:%x", info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano), h.Sum(nil)), nil
	}
	return "file:" + hex.EncodeToString(h.Sum(nil)), nil
}

// ProviderChange is one provider path an attempt added, altered or removed.
type ProviderChange struct {
	Path   string `json:"path"`
	Change string `json:"change"`
}

// DiffProviderState lists what differs between two ProviderState results, in path order.
func DiffProviderState(before, after map[string]string) []ProviderChange {
	changes := []ProviderChange{}
	for p, now := range after {
		switch was, ok := before[p]; {
		case !ok:
			changes = append(changes, ProviderChange{p, "added"})
		case was != now:
			changes = append(changes, ProviderChange{p, "altered"})
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			changes = append(changes, ProviderChange{p, "removed"})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}
