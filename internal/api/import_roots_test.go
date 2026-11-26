package api

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func TestChooserLimitsEachAccountToItsGrantedDirectories(t *testing.T) {
	trustImportRoots(t)
	git, err := source.Resolve(context.Background(), canonicalTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	f := newWebFixture(t, testUI, func(e *Execution) { e.Inspector = &source.Inspector{Git: git, MirrorRoot: canonicalTemp(t)} })
	root := newRepo(t)
	writeFile(t, filepath.Join(root, "sub", "file"), "content")
	writeFile(t, filepath.Join(root, ".mcp.json"), "{}")
	added := f.ownerWithKey(t, "POST", "/v1/import-roots", "root", map[string]string{"path": root, "label": "Work"})
	if added.status != 201 {
		t.Fatal(string(added.body))
	}
	id := added.json(t)["id"].(string)
	base := "/v1/import-roots/" + id
	member, code := f.account(t, "Member", false)
	session := f.mustEnroll(t, code, newSoftKey(t))
	owner := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	if r := f.owner(t, "PUT", base+"/grants/"+member, nil); r.status != 200 {
		t.Fatal(string(r.body))
	}
	all := f.owner(t, "GET", "/v1/import-roots/all", nil)
	var managed []struct {
		ID, Path, Label string
		Grants          []string
	}
	if err := json.Unmarshal(all.body, &managed); err != nil || len(managed) != 1 || managed[0].Path != root || !slices.Equal(managed[0].Grants, []string{member}) {
		t.Fatal(string(all.body))
	}
	if r := f.call(t, "GET", "/v1/import-roots", owner, nil); strings.TrimSpace(string(r.body)) != "[]" {
		t.Fatal(string(r.body))
	}
	roots := f.call(t, "GET", "/v1/import-roots", session, nil)
	var listed []map[string]string
	if err := json.Unmarshal(roots.body, &listed); err != nil || len(listed) != 1 || len(listed[0]) != 2 || listed[0]["id"] != id || listed[0]["label"] != "Work" {
		t.Fatal(string(roots.body))
	}
	for _, p := range []string{"../outside", root, "//network/share", "C:repo", "a\x00b"} {
		r := f.call(t, "GET", base+"/browse?path="+url.QueryEscape(p), session, nil)
		if r.status != 400 {
			t.Fatalf("browse %q: %d %s", p, r.status, r.body)
		}
	}
	for _, route := range []string{"inspect", "import"} {
		for _, bad := range []any{map[string]string{}, map[string]string{"url": "https://example.invalid/repo"}, map[string]string{"path": "../outside"}} {
			if r := f.keyed(t, "POST", base+"/"+route, session, "invalid-"+route, "", bad); r.status != 400 {
				t.Fatalf("%s %v: %d %s", route, bad, r.status, r.body)
			}
		}
		r := f.keyed(t, "POST", base+"/"+route, session, "blocked-"+route, "", map[string]string{"path": "sub"})
		var result struct{ Blockers []source.Blocker }
		if err := json.Unmarshal(r.body, &result); err != nil || len(result.Blockers) != 1 || result.Blockers[0].Code != source.BlockNotRepository || result.Blockers[0].Count != 1 || len(result.Blockers[0].Paths) != 0 {
			t.Fatalf("%s: %s", route, r.body)
		}
	}
	runGit(t, filepath.Join(root, "sub"), "init", "-q")
	runGit(t, filepath.Join(root, "sub"), "add", "file")
	runGit(t, filepath.Join(root, "sub"), "commit", "-qm", "nested")
	body := map[string]string{"path": ""}
	inspection := f.call(t, "POST", base+"/inspect", session, body)
	if !strings.Contains(string(inspection.body), "provider_configuration") || !strings.Contains(string(inspection.body), ".mcp.json") {
		t.Fatal(string(inspection.body))
	}
	var view struct{ Untracked []string }
	if err := json.Unmarshal(inspection.body, &view); err != nil || slices.Contains(view.Untracked, "sub/") {
		t.Fatalf("snapshot candidates: %v %v", view.Untracked, err)
	}
	imported := f.keyed(t, "POST", base+"/import", session, "create", "", body)
	if imported.status != 201 || imported.json(t)["role"] != "owner" {
		t.Fatalf("member import: %d %s", imported.status, imported.body)
	}
	if r := f.owner(t, "GET", "/v1/projects/"+imported.json(t)["id"].(string)+"/untracked", nil); r.status != 404 {
		t.Fatalf("non-member read: %d", r.status)
	}
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Rename(moved, root) })
	if r := f.keyed(t, "POST", base+"/import", session, "create", "", body); r.status != 400 {
		t.Fatalf("replay after root moved: %d %s", r.status, r.body)
	}
	if r := f.owner(t, "DELETE", base+"/grants/"+member, nil); r.status != 200 {
		t.Fatal(string(r.body))
	}
	if r := f.keyed(t, "POST", base+"/import", session, "create", "", body); r.status != 404 {
		t.Fatalf("revoked replay: %d", r.status)
	}
}

func TestChooserKeepsGitSetupFailuresOnTheHost(t *testing.T) {
	trustImportRoots(t)
	host := canonicalTemp(t)
	f := newWebFixture(t, testUI, func(e *Execution) { e.Inspector, e.Capturer, e.GitUnavailable = nil, nil, host })
	base := grantChooserRoot(t, f, host)
	owner := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	seedChooserProject(t, f, host)
	for _, route := range []struct {
		method, path string
		body         any
	}{
		{"POST", base + "/inspect", map[string]string{"path": ""}},
		{"POST", base + "/import", map[string]string{"path": ""}},
		{"GET", "/v1/projects/imported/untracked", nil},
		{"POST", "/v1/projects/imported/snapshots", map[string]any{}},
	} {
		r := f.keyed(t, route.method, route.path, owner, "git-unavailable-"+route.path, "", route.body)
		encoded, err := json.Marshal(host)
		if err != nil {
			t.Fatal(err)
		}
		if r.status != 503 || r.code(t) != "git_unavailable" ||
			strings.Contains(string(r.body), string(encoded[1:len(encoded)-1])) ||
			strings.Contains(string(r.body), filepath.ToSlash(host)) ||
			strings.Contains(string(r.body), filepath.Base(filepath.Dir(host))) {
			t.Fatalf("%s: %d %s", route.path, r.status, r.body)
		}
	}
}

func TestChooserOnlyPublishesRelativeProviderPaths(t *testing.T) {
	trustImportRoots(t)
	root := canonicalTemp(t)
	inspector := &fakeInspector{result: source.Inspection{Identity: source.Identity{Path: root}, Findings: []source.Finding{{Code: source.FindingProviderConfiguration, Count: 3, Paths: []string{".mcp.json", filepath.ToSlash(filepath.Join(root, ".mcp.json")), "../.mcp.json"}}}}}
	f := newNetFixture(t, func(e *Execution) { e.Inspector = inspector })
	add := f.owner(t, "POST", "/v1/import-roots", map[string]string{"path": root})
	base := "/v1/import-roots/" + add.json(t)["id"].(string)
	if r := f.owner(t, "PUT", base+"/grants/local", nil); r.status != 200 {
		t.Fatal(string(r.body))
	}
	r := f.owner(t, "POST", base+"/inspect", map[string]string{"path": ""})
	var got struct{ Findings []source.Finding }
	if err := json.Unmarshal(r.body, &got); err != nil || len(got.Findings) != 1 || !slices.Equal(got.Findings[0].Paths, []string{".mcp.json"}) || got.Findings[0].Count != 3 {
		t.Fatal(string(r.body))
	}
}

func grantChooserRoot(t *testing.T, f *netFixture, root string) string {
	t.Helper()
	add := f.owner(t, "POST", "/v1/import-roots", map[string]string{"path": root})
	if add.status != 201 {
		t.Fatal(string(add.body))
	}
	base := "/v1/import-roots/" + add.json(t)["id"].(string)
	if r := f.owner(t, "PUT", base+"/grants/local", nil); r.status != 200 {
		t.Fatal(string(r.body))
	}
	return base
}

func seedChooserProject(t *testing.T, f *netFixture, path string) {
	t.Helper()
	_, err := f.s.Do(t.Context(), "local", "seed-import", "seed-import", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ImportProject(t.Context(), "imported", "Imported", store.ProjectSource{Path: path, CommonDir: filepath.Join(path, ".git"), Fingerprint: "fixture"})
		return []byte("{}"), err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBrowseDoesNotReplyAfterGrantRevocationOrRootRemoval(t *testing.T) {
	trustImportRoots(t)
	for _, suffix := range []string{"/grants/local", ""} {
		t.Run(suffix, func(t *testing.T) {
			root := canonicalTemp(t)
			writeFile(t, filepath.Join(root, "unpublished", "file"), "content")
			f := newWebFixture(t, testUI)
			base := grantChooserRoot(t, f, root)
			owner := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
			entered, release := pauseAt(f, "browse-import-root")
			defer release()
			result := make(chan reply, 1)
			go func() { result <- f.call(t, "GET", base+"/browse", owner, nil) }()
			waitFor(t, entered, "browse finished listing")
			removed := f.owner(t, "DELETE", base+suffix, nil)
			release()
			if removed.status != 200 {
				t.Fatal(string(removed.body))
			}
			if r := <-result; r.status != 404 || strings.Contains(string(r.body), "unpublished") {
				t.Fatalf("browse returned %d %s", r.status, r.body)
			}
		})
	}
}

type swappingChooserInspector struct {
	afterRead func() error
}

func (in swappingChooserInspector) Inspect(_ context.Context, path string) (source.Inspection, error) {
	return source.Inspection{
		Identity:           source.Identity{Path: path, CommonDir: filepath.Join(path, ".git")},
		SnapshotCandidates: []string{"unpublished"},
	}, in.afterRead()
}

func TestChooserReadsRefuseReplacedDirectory(t *testing.T) {
	trustImportRoots(t)
	for _, route := range []string{"inspect", "untracked"} {
		t.Run(route, func(t *testing.T) {
			root := canonicalTemp(t)
			dir := filepath.Join(root, "selected")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			in := swappingChooserInspector{afterRead: func() error {
				if err := os.Rename(dir, filepath.Join(root, "moved")); err != nil {
					return err
				}
				return os.Mkdir(dir, 0700)
			}}
			f := newWebFixture(t, testUI, func(e *Execution) { e.Inspector = in })
			base := grantChooserRoot(t, f, root)
			owner := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
			var r reply
			if route == "inspect" {
				r = f.call(t, "POST", base+"/inspect", owner, map[string]string{"path": "selected"})
			} else {
				seedChooserProject(t, f, dir)
				r = f.call(t, "GET", "/v1/projects/imported/untracked", owner, nil)
			}
			if r.status != 400 && r.status != 409 || strings.Contains(string(r.body), "unpublished") {
				t.Fatalf("%s returned %d %s", route, r.status, r.body)
			}
		})
	}
}

func (f swappingChooserInspector) InspectInRoot(ctx context.Context, path, root string) (source.Inspection, error) {
	return f.Inspect(ctx, path)
}

func TestChooserProjectKeepsRootBoundary(t *testing.T) {
	trustImportRoots(t)
	root := newRepo(t)
	git, err := source.Resolve(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	in := &source.Inspector{Git: git, MirrorRoot: canonicalTemp(t)}
	artifacts, err := artifact.Open(canonicalTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	f := newWebFixture(t, testUI, func(e *Execution) {
		e.Inspector, e.Capturer, e.Drifter, e.Writer = in, in, in, artifacts
	})
	base := grantChooserRoot(t, f, root)
	owner := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	imported := f.keyed(t, "POST", base+"/import", owner, "create", "", map[string]string{"path": ""})
	if imported.status != 201 {
		t.Fatalf("import: %d %s", imported.status, imported.body)
	}
	project := "/v1/projects/" + imported.json(t)["id"].(string)
	if r := f.keyed(t, "POST", project+"/snapshots", owner, "first", "", map[string]any{}); r.status != 201 {
		t.Fatalf("first snapshot: %d %s", r.status, r.body)
	}
	outside := newRepo(t)
	file := filepath.Join(root, ".git", "objects", "info", "alternates")
	writeFile(t, file, filepath.ToSlash(filepath.Join(outside, ".git", "objects"))+"\n")
	for _, route := range []struct {
		method, tail string
		body         any
	}{
		{"POST", "/snapshots", map[string]any{}},
		{"GET", "/untracked", nil},
	} {
		r := f.keyed(t, route.method, project+route.tail, owner, "outside-"+route.tail, "", route.body)
		if r.status != 409 || !strings.Contains(string(r.body), source.BlockEscapingLink) || strings.Contains(string(r.body), filepath.Base(outside)) {
			t.Errorf("%s: %d %s", route.tail, r.status, r.body)
		}
	}
	if r := f.owner(t, "GET", project+"/drift", nil); r.status != 409 || !strings.Contains(string(r.body), source.BlockEscapingLink) {
		t.Errorf("drift: %d %s", r.status, r.body)
	}

	if r := f.owner(t, "DELETE", base, nil); r.status != 200 {
		t.Fatal(string(r.body))
	}
	if r := f.call(t, "GET", project+"/untracked", owner, nil); r.status != 400 || r.code(t) != "import_root_missing" {
		t.Fatalf("removed root: %d %s", r.status, r.body)
	}
}
