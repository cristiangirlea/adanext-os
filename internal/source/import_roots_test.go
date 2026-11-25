package source

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/artifact"
)

func TestBrowseListsOnlySortedChildDirectoriesAndReportsItsLimit(t *testing.T) {
	trustImportRoots(t)
	root := tempDir(t)
	for i := 1000; i >= 0; i-- {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("d%04d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	dirs, cut, err := BrowseImportRoot(root, "")
	if err != nil || !cut || len(dirs) != 1000 || dirs[0].Name != "d0000" || dirs[999].Path != "d0999" {
		t.Fatalf("listing: count=%d cut=%v err=%v", len(dirs), cut, err)
	}
	for _, path := range []string{"../outside", "d0000/..", tempDir(t), "/absolute", "C:relative", "C:/absolute", "//server/share", "d0000\x00", "a"} {
		if _, _, err := BrowseImportRoot(root, path); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
}

func TestChooserRefusesDirectoryLinksAndTheirDescendants(t *testing.T) {
	trustImportRoots(t)
	for _, kind := range []string{"symlink", "junction"} {
		t.Run(kind, func(t *testing.T) {
			root, out := tempDir(t), tempDir(t)
			if err := os.Mkdir(filepath.Join(out, "child"), 0700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "link")
			if kind == "junction" {
				if runtime.GOOS != "windows" {
					t.Skip("Windows junction")
				}
				makeLink(t, out, link)
			} else if err := os.Symlink(out, link); err != nil {
				if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
					t.Skip("missing symlink privilege")
				}
				t.Fatal(err)
			}
			_, err := CanonicalImportRoot(link)
			assertRootCode(t, err, BlockEscapingLink)
			for _, path := range []string{"link", "link/child"} {
				_, _, err := BrowseImportRoot(root, path)
				assertRootCode(t, err, BlockEscapingLink)
			}
			dirs, _, err := BrowseImportRoot(root, "")
			if err != nil || len(dirs) != 0 {
				t.Fatalf("listing: %v %v", dirs, err)
			}
		})
	}
}

func TestSnapshotCandidatesExcludePathsRejectedBySelection(t *testing.T) {
	root := tempDir(t)
	long := strings.Repeat(strings.Repeat("a", 100)+"/", 41) + "file"
	paths := []string{"eligible", long}
	write(t, filepath.Join(root, "eligible"), "content")
	if ValidSelectionPath(long) {
		t.Fatal("selection accepted the overlong path")
	}
	if runtime.GOOS == "windows" {
		write(t, filepath.Join(root, filepath.FromSlash(long)), "content")
	} else {
		invalid := "invalid-\xff"
		paths = append(paths, invalid)
		write(t, filepath.Join(root, invalid), "content")
	}
	_, _, candidates := fingerprint(root, "", &index{}, Status{Untracked: paths}, nil)
	if !slices.Equal(candidates, []string{"eligible"}) {
		t.Fatalf("offered %d paths, want only eligible", len(candidates))
	}
}

func TestChooserRepositoryPointersStayWithinRoot(t *testing.T) {
	trustImportRoots(t)
	for _, kind := range []string{"gitdir", "commondir", "alternates", "include", "excludes", "attributes"} {
		t.Run(kind, func(t *testing.T) {
			root := tempDir(t)
			repo := filepath.Join(root, "repo")
			outside := newRepo(t, nil)
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "gitdir":
				write(t, filepath.Join(repo, ".git"), "gitdir: "+filepath.ToSlash(filepath.Join(outside, ".git"))+"\n")
			case "commondir":
				write(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
				write(t, filepath.Join(repo, ".git", "commondir"), filepath.ToSlash(filepath.Join(outside, ".git"))+"\n")
			case "alternates", "include", "excludes", "attributes":
				git(t, repo, "init", "-q")
				if kind == "alternates" {
					write(t, filepath.Join(repo, ".git", "objects", "info", "alternates"), filepath.ToSlash(filepath.Join(outside, ".git", "objects"))+"\n")
				} else {
					key := map[string]string{"include": "include.path", "excludes": "core.excludesFile", "attributes": "core.attributesFile"}[kind]
					git(t, repo, "config", key, filepath.ToSlash(filepath.Join(outside, ".git", "config")))
				}
			}
			in := newInspector(t)
			got, err := in.InspectInRoot(t.Context(), repo, root)
			if err != nil || !slices.ContainsFunc(got.Blockers, func(b Blocker) bool { return b.Code == BlockEscapingLink }) {
				t.Fatalf("expected boundary blocker: %v %v", got.Blockers, err)
			}
			direct := inspect(t, in, repo)
			if slices.ContainsFunc(direct.Blockers, func(b Blocker) bool { return b.Code == BlockEscapingLink }) {
				t.Fatalf("direct inspection unexpectedly enforced the root: %v", direct.Blockers)
			}
		})
	}
}

func assertRootCode(t *testing.T, err error, code string) {
	t.Helper()
	path, ok := errors.AsType[*PathError](err)
	if !ok || path.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestImportPermissionTopDefaultsToVolumeRoot(t *testing.T) {
	volume := volumeRoot(tempDir(t))
	parts := []string{"granted", "child"}
	if depth := importPermissionTopDepth(volume, parts, 1); depth != 0 {
		t.Fatalf("permission walk starts at depth %d, want volume root", depth)
	}
}

func TestCanonicalImportRootUnderUserProfile(t *testing.T) {
	profileDir, err := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		profileDir, err = os.UserCacheDir()
	}
	if err != nil {
		t.Skipf("no user profile directory: %v", err)
	}
	root, err := os.MkdirTemp(profileDir, "ada-import-root-")
	if errors.Is(err, os.ErrPermission) {
		t.Skipf("cannot create a private folder under the user profile: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(root); err != nil {
			t.Error(err)
		}
	})
	if err := artifact.PrivateDirectory(root); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalImportRoot(root); err != nil {
		t.Fatal(err)
	}
}

// trustImportRoots skips owner, DACL and mode checks for chooser directories.
func trustImportRoots(t *testing.T) {
	t.Helper()
	original := *ImportPermissionCheck
	*ImportPermissionCheck = func(*os.File, bool, bool) error { return nil }
	t.Cleanup(func() { *ImportPermissionCheck = original })
}

func permissionTestTop(t *testing.T) string {
	t.Helper()
	top := tempDir(t)
	if err := artifact.PrivateDirectory(top); err != nil {
		t.Fatal(err)
	}
	original := importPermissionTop
	importPermissionTop = func(string) string { return top }
	t.Cleanup(func() { importPermissionTop = original })
	return top
}

func permissionTestChild(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testChooserChecksAlternateBoundaries(t *testing.T, makeWritable func(string)) {
	t.Helper()
	root := permissionTestChild(t, permissionTestTop(t), "root")
	git(t, root, "init", "-q", "-b", "main")
	git(t, root, "commit", "-q", "--allow-empty", "-m", "start")
	objects := filepath.Join(root, ".git", "objects")
	alternate := filepath.Join(root, "alternate")
	if err := os.Mkdir(alternate, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(objects, "info", "alternates")
	write(t, file, "../../alternate\n")
	for _, path := range []string{objects, alternate, file} {
		makeWritable(path)
	}
	got, err := newInspector(t).InspectInRoot(t.Context(), root, root)
	if err != nil || len(got.Blockers) != 0 {
		t.Fatalf("alternate within root: %v %v", got.Blockers, err)
	}
}

func TestChooserRefusesAmbiguousCorePaths(t *testing.T) {
	trustImportRoots(t)
	paths := []string{"relative"}
	if runtime.GOOS == "windows" {
		paths = append(paths, "C:drive-relative", `\drive-rooted`)
	}
	for _, key := range []string{"core.excludesFile", "core.attributesFile"} {
		for _, path := range paths {
			t.Run(key+"/"+path, func(t *testing.T) {
				root := tempDir(t)
				git(t, root, "init", "-q")
				git(t, root, "config", key, path)
				got, err := newInspector(t).InspectInRoot(t.Context(), root, root)
				if err != nil || !slices.ContainsFunc(got.Blockers, func(b Blocker) bool { return b.Code == BlockEscapingLink }) {
					t.Fatalf("expected boundary blocker: %v %v", got.Blockers, err)
				}
			})
		}
	}
}

func TestChooserAlternateChecksComponentsBeforeParent(t *testing.T) {
	trustImportRoots(t)
	root, outside := tempDir(t), tempDir(t)
	git(t, root, "init", "-q")
	for _, dir := range []string{filepath.Join(root, "target"), filepath.Join(outside, "child"), filepath.Join(outside, "target")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	makeLink(t, filepath.Join(outside, "child"), filepath.Join(root, "link"))
	write(t, filepath.Join(root, ".git", "objects", "info", "alternates"), "../../link/../target\n")
	got, err := newInspector(t).InspectInRoot(t.Context(), root, root)
	if err != nil || !slices.ContainsFunc(got.Blockers, func(b Blocker) bool { return b.Code == BlockEscapingLink }) {
		t.Fatalf("expected boundary blocker: %v %v", got.Blockers, err)
	}
}
