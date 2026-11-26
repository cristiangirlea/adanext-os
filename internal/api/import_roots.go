package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func redactedBlockers(in []source.Blocker) []source.Blocker {
	out := make([]source.Blocker, 0, len(in))
	for _, b := range in {
		out = append(out, source.Blocker{Code: b.Code, Count: b.Count, Paths: []string{}})
	}
	return out
}

func writeRootError(w http.ResponseWriter, err error) {
	if e, ok := errors.AsType[*source.PathError](err); ok {
		writeJSON(w, 400, errorBody{e.Code, e.Reason})
		return
	}
	writeInternalError(w, err)
}

func (h *handler) allImportRoots(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.ownerActor(w, r); !ok {
		return
	}
	roots, err := h.store.AllImportRoots(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, roots)
}

func (h *handler) addImportRoot(w http.ResponseWriter, r *http.Request) {
	user, k, body, r, ok := h.ownerMutation(w, r)
	if !ok {
		return
	}
	var in struct{ Path, Label string }
	if decode(body, &in) != nil || !validRepoPath(in.Path) || !validName(in.Label) {
		writeError(w, 400, "bad_request")
		return
	}
	if h.replayed(w, r, user, k, nil) {
		return
	}
	path, err := source.CanonicalImportRoot(in.Path)
	if err != nil {
		writeRootError(w, err)
		return
	}
	if in.Label == "" {
		in.Label = filepath.Base(path)
		if !validName(in.Label) || path == filepath.Dir(path) {
			in.Label = "Import root"
		}
	}
	h.mutate(w, r, user, k, nil, "", func(tx *store.Tx) (int, any, error) {
		id, err := newID()
		if err != nil {
			return 0, nil, err
		}
		return 201, store.ImportRoot{ID: id, Label: in.Label}, tx.AddImportRoot(r.Context(), id, path, in.Label)
	})
}

func (h *handler) changeImportRoot(w http.ResponseWriter, r *http.Request) {
	user, k, body, r, ok := h.ownerMutation(w, r)
	if !ok {
		return
	}
	if !emptyBody(body) {
		writeError(w, 400, "bad_request")
		return
	}
	id, account := r.PathValue("id"), r.PathValue("user")
	h.mutate(w, r, user, k, nil, "", func(tx *store.Tx) (int, any, error) {
		if account == "" {
			return 200, struct{}{}, tx.RemoveImportRoot(r.Context(), id)
		}
		return 200, struct{}{}, tx.SetImportRootGrant(r.Context(), id, account, r.Method == http.MethodPut)
	})
}

func (h *handler) importRoots(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	roots, err := h.store.GrantedImportRoots(r.Context(), user)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, roots)
}

func (h *handler) grantedRoot(w http.ResponseWriter, r *http.Request) (user, root string, ok bool) {
	user, ok = h.actor(w, r)
	if !ok {
		return
	}
	root, err := h.store.GrantedRootPath(r.Context(), user, r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		ok = false
	}
	return
}

func (h *handler) browseImportRoot(w http.ResponseWriter, r *http.Request) {
	user, root, ok := h.grantedRoot(w, r)
	if !ok {
		return
	}
	dirs, cut, err := source.BrowseImportRoot(root, r.URL.Query().Get("path"))
	if h.net != nil {
		h.net.at(r.Context(), "browse-import-root")
	}
	if _, grantErr := h.store.GrantedRootPath(r.Context(), user, r.PathValue("id")); grantErr != nil {
		storeError(w, grantErr)
		return
	}
	if err != nil {
		writeRootError(w, err)
		return
	}
	if cut {
		w.Header().Set("X-Ada-Truncated", "true")
	}
	writeJSON(w, 200, dirs)
}

func (h *handler) inspectGrantedDirectory(w http.ResponseWriter, r *http.Request, user, root, rel string, dir *os.File) (source.Inspection, bool) {
	var result source.Inspection
	if h.exec.Inspector == nil {
		writeJSON(w, 503, errorBody{"git_unavailable", "git is unavailable on the coordinator"})
		return result, false
	}
	// Swapping a directory during git's reads requires write access below the root, which the chooser refuses to other accounts.
	result, err := h.exec.Inspector.InspectInRoot(r.Context(), dir.Name(), root)
	if err != nil {
		writeInternalError(w, err)
		return result, false
	}
	current, err := source.OpenImportDirectory(root, rel)
	if err != nil {
		writeRootError(w, err)
		return result, false
	}
	defer current.Close()
	inspected, err := source.OpenBoundSourceDirectory(result.Identity.Path, root)
	if err == nil {
		defer inspected.Close()
	}
	if err != nil || !source.SameDirectory(dir, inspected) || !source.SameDirectory(dir, current) {
		writeJSON(w, 400, errorBody{source.BlockEscapingLink, "the chosen folder changed during inspection"})
		return result, false
	}
	if _, err = h.store.GrantedRootPath(r.Context(), user, r.PathValue("id")); err != nil {
		storeError(w, err)
		return result, false
	}
	result.Blockers = redactedBlockers(result.Blockers)
	return result, true
}

func (h *handler) inspectInRoot(w http.ResponseWriter, r *http.Request) {
	user, root, ok := h.grantedRoot(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in struct{ Path *string }
	if decode(body, &in) != nil || in.Path == nil {
		writeError(w, 400, "bad_request")
		return
	}
	dir, err := source.OpenImportDirectory(root, *in.Path)
	if err != nil {
		writeRootError(w, err)
		return
	}
	defer dir.Close()
	result, ok := h.inspectGrantedDirectory(w, r, user, root, *in.Path, dir)
	if !ok {
		return
	}
	writeJSON(w, 200, struct {
		Blockers  []source.Blocker `json:"blockers"`
		Findings  []source.Finding `json:"findings"`
		Untracked []string         `json:"untracked"`
	}{result.Blockers, result.ChooserFindings(), result.SnapshotCandidates})
}

func (h *handler) projectImportRoot(w http.ResponseWriter, r *http.Request, src store.ProjectSource) (string, bool) {
	if src.ImportRoot == "" {
		return "", true
	}
	root, err := h.store.ImportRootPath(r.Context(), src.ImportRoot)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, 400, errorBody{"import_root_missing", "the project's import root was removed"})
		return "", false
	}
	if err != nil {
		storeError(w, err)
		return "", false
	}
	return root, true
}

func (h *handler) openProjectDirectory(w http.ResponseWriter, src store.ProjectSource, root string) (*os.File, bool) {
	dir, err := source.OpenBoundSourceDirectory(src.Path, root)
	if err == nil {
		return dir, true
	}
	if root != "" {
		writeRootError(w, err)
	} else {
		h.captureError(w, source.ErrSourceChanged)
	}
	return nil, false
}

func (h *handler) recheckProjectDirectory(w http.ResponseWriter, r *http.Request, src store.ProjectSource, dir *os.File) bool {
	root, ok := h.projectImportRoot(w, r, src)
	if !ok {
		return false
	}
	current, ok := h.openProjectDirectory(w, src, root)
	if !ok {
		return false
	}
	defer current.Close()
	if !source.SameDirectory(dir, current) {
		h.captureError(w, source.ErrSourceChanged)
		return false
	}
	return true
}

func (h *handler) projectUntracked(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	src, err := h.store.ProjectSource(r.Context(), user, r.PathValue("id"))
	if errors.Is(err, store.ErrNoSource) {
		writeJSON(w, 409, errorBody{"no_source", "the project has no imported repository"})
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	if h.exec.Inspector == nil {
		writeJSON(w, 503, errorBody{"git_unavailable", "git is unavailable on the coordinator"})
		return
	}
	root, ok := h.projectImportRoot(w, r, src)
	if !ok {
		return
	}
	dir, ok := h.openProjectDirectory(w, src, root)
	if !ok {
		return
	}
	defer dir.Close()
	var found source.Inspection
	if root == "" {
		found, err = h.exec.Inspector.Inspect(r.Context(), src.Path)
	} else {
		found, err = h.exec.Inspector.InspectInRoot(r.Context(), src.Path, root)
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !h.recheckProjectDirectory(w, r, src, dir) {
		return
	}
	if len(found.Blockers) > 0 {
		h.captureError(w, &source.BlockedError{Blockers: found.Blockers})
		return
	}
	if found.Identity.Path != src.Path || found.Identity.CommonDir != src.CommonDir {
		h.captureError(w, source.ErrSourceChanged)
		return
	}
	writeJSON(w, 200, found.SnapshotCandidates)
}
