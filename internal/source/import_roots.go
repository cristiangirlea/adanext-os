package source

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

type ChildDirectory struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func rootError(code string) error {
	return &PathError{Code: code, Reason: "choose a local directory within the granted root"}
}

// Resolve the parent first so the final component can be opened without following links.
func CanonicalImportRoot(path string) (string, error) {
	if IsRemotePath(path) {
		return "", rootError(BlockRemotePath)
	}
	if !filepath.IsAbs(path) {
		return "", rootError(PathInvalid)
	}
	path = filepath.Clean(path)
	if !localImportVolume(volumeRoot(path)) {
		return "", rootError(BlockRemotePath)
	}
	parent, ok := resolvePath(volumeRoot(path), filepath.Dir(path), false, true, localImportVolume)
	if !ok {
		return "", rootError(BlockEscapingLink)
	}
	if filepath.Dir(path) != path {
		path = filepath.Join(parent, filepath.Base(path))
	}
	dir, err := OpenImportDirectory(path, "")
	if err != nil {
		return "", err
	}
	defer dir.Close()
	canonical, err := importDirectoryPath(dir)
	if _, ok := errors.AsType[*PathError](err); ok {
		return "", err
	}
	if err != nil || !filepath.IsAbs(canonical) || IsRemotePath(canonical) || !localImportVolume(volumeRoot(canonical)) {
		return "", rootError(PathInvalid)
	}
	return canonical, nil
}

func validChooserPath(rel string) bool {
	if rel == "" {
		return true
	}
	if !fs.ValidPath(rel) || rel == "." || strings.ContainsAny(rel, "\\:\x00") {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if !filepath.IsLocal(part) || (runtime.GOOS == "windows" && (strings.HasSuffix(part, ".") || strings.HasSuffix(part, " "))) {
			return false
		}
	}
	return true
}

// An index entry is repository input, including the name reported for a provider file.
func (inspection Inspection) ChooserFindings() []Finding {
	findings := []Finding{}
	for _, f := range inspection.Findings {
		if f.Code != FindingProviderConfiguration {
			continue
		}
		paths := make([]string, 0, len(f.Paths))
		for _, p := range f.Paths {
			if p != "" && validChooserPath(p) {
				paths = append(paths, p)
			}
		}
		f.Paths = paths
		findings = append(findings, f)
	}
	return findings
}

// Keep the selected directory open while its contents are being read.
func OpenImportDirectory(root, rel string) (*os.File, error) {
	return openImportDirectory(root, rel, false, true)
}

// OpenSourceDirectory binds an imported source's current directory identity.
func OpenSourceDirectory(path string) (*os.File, error) {
	return openImportDirectory(path, "", false, false)
}

func openImportDirectory(root, rel string, list, checkPermissions bool) (*os.File, error) {
	if IsRemotePath(root) || IsRemotePath(rel) {
		return nil, rootError(BlockRemotePath)
	}
	if !filepath.IsAbs(root) || !validChooserPath(rel) {
		return nil, rootError(PathInvalid)
	}
	volume := volumeRoot(root)
	if checkPermissions && !localImportVolume(volume) {
		return nil, rootError(BlockRemotePath)
	}
	path := root
	if rel != "" {
		path = filepath.Join(root, filepath.FromSlash(rel))
	}
	dir, err := walkImportDirectory(volume, importPathParts(path), len(importPathParts(root)), list, checkPermissions, nil)
	if errors.Is(err, errLink) {
		return nil, rootError(BlockEscapingLink)
	}
	if _, ok := errors.AsType[*PathError](err); ok {
		return nil, err
	}
	if err != nil {
		return nil, rootError(PathInvalid)
	}
	return dir, nil
}

func importPathParts(path string) []string {
	tail := strings.TrimPrefix(filepath.Clean(path), volumeRoot(path))
	if tail == "" {
		return nil
	}
	return strings.Split(tail, string(filepath.Separator))
}

// importPermissionCheck checks ownership and write access on an open directory.
// Its flags identify an ancestor of the grant and the top of the permission walk.
var importPermissionCheck = checkImportPermissions

var importPermissionTop = volumeRoot

// The permission walk can start below the volume in tests. The handle walk
// still starts at the volume so every component is checked for links.
func importPermissionTopDepth(volume string, parts []string, rootDepth int) int {
	root := filepath.Join(volume, filepath.Join(parts[:rootDepth]...))
	top := importPermissionTop(root)
	rel, err := filepath.Rel(top, root)
	if err != nil || !filepath.IsLocal(rel) {
		return 0
	}
	return len(importPathParts(top))
}

// openBoundImportDirectory checks ancestry on the same handle walk that opens
// the target. A spelling accepted by filepath.Rel may identify another folder.
func openBoundImportDirectory(root, path string, checkWalkPermissions bool) (*os.File, error) {
	if IsRemotePath(path) || (filepath.IsAbs(path) && !localImportVolume(volumeRoot(path))) {
		return nil, rootError(BlockRemotePath)
	}
	if !filepath.IsAbs(path) {
		return nil, errLink
	}
	anchor, err := OpenImportDirectory(root, "")
	if err != nil {
		return nil, err
	}
	defer anchor.Close()
	depth, parts := len(importPathParts(root)), importPathParts(path)
	if len(parts) < depth || !validChooserPath(strings.Join(parts[depth:], "/")) {
		return nil, errLink
	}
	return walkImportDirectory(volumeRoot(path), parts, depth, false, checkWalkPermissions, anchor)
}

// OpenBoundSourceDirectory opens a stored project's directory within its root.
func OpenBoundSourceDirectory(path, root string) (*os.File, error) {
	if root == "" {
		return OpenSourceDirectory(path)
	}
	dir, err := openBoundImportDirectory(root, path, true)
	if err == nil {
		return dir, nil
	}
	if _, ok := errors.AsType[*PathError](err); ok {
		return nil, err
	}
	if errors.Is(err, errLink) {
		return nil, rootError(BlockEscapingLink)
	}
	return nil, rootError(PathInvalid)
}

const BlockRootWritable = "root_writable_by_others"

func writableImportRoot() error {
	return &PathError{Code: BlockRootWritable, Reason: "remove other accounts' write access to the folder"}
}

// SameDirectory compares the identities recorded from two open handles.
func SameDirectory(a, b *os.File) bool {
	before, err := a.Stat()
	after, statErr := b.Stat()
	return err == nil && statErr == nil && before.IsDir() && after.IsDir() && os.SameFile(before, after)
}

func BrowseImportRoot(root, rel string) ([]ChildDirectory, bool, error) {
	dir, err := openImportDirectory(root, rel, true, true)
	if err != nil {
		return nil, false, err
	}
	defer dir.Close()
	const limit = 1000
	out := []ChildDirectory{}
	cut := false
	for {
		batch, err := dir.ReadDir(256)
		if err != nil && err != io.EOF {
			return nil, false, rootError(PathInvalid)
		}
		for _, entry := range batch {
			if !entry.IsDir() || entry.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
				continue
			}
			name := entry.Name()
			if !validChooserPath(name) {
				continue
			}
			child := name
			if rel != "" {
				child = rel + "/" + name
			}
			out = append(out, ChildDirectory{name, child})
		}
		slices.SortFunc(out, func(a, b ChildDirectory) int { return strings.Compare(a.Name, b.Name) })
		if len(out) > limit {
			cut = true
			out = out[:limit]
		}
		if err == io.EOF {
			current, err := OpenImportDirectory(root, rel)
			if err != nil {
				return nil, false, err
			}
			defer current.Close()
			if !SameDirectory(dir, current) {
				return nil, false, rootError(BlockEscapingLink)
			}
			return out, cut, nil
		}
	}
}
