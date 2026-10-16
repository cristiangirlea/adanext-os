package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

// Snapshot request limits: paths per request and bytes per path.
const (
	maxSelected   = 1000
	maxPathLength = 4096
	maxNameLength = 200
)

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

type blockedBody struct {
	errorBody
	Blockers []source.Blocker `json:"blockers"`
}

type pathBody struct {
	errorBody
	Path string `json:"path"`
}

type taskSummaryJSON struct {
	ID        string      `json:"id"`
	Objective string      `json:"objective"`
	State     store.State `json:"state"`
}

// writeAlreadyImported names the project holding the source, and its tasks,
// only to one of its members.
func writeAlreadyImported(w http.ResponseWriter, e *store.AlreadyImportedError) {
	body := errorBody{"already_imported", "this repository is already imported"}
	if e.Project == "" {
		writeJSON(w, 409, body)
		return
	}
	tasks := make([]taskSummaryJSON, 0, len(e.Tasks))
	for _, t := range e.Tasks {
		tasks = append(tasks, taskSummaryJSON{t.ID, t.Objective, t.State})
	}
	writeJSON(w, 409, struct {
		errorBody
		Project string            `json:"project"`
		Tasks   []taskSummaryJSON `json:"tasks"`
	}{body, e.Project, tasks})
}

type sourceJSON struct {
	Path        string `json:"path"`
	CommonDir   string `json:"common_dir"`
	Fingerprint string `json:"fingerprint"`
	Imported    string `json:"imported"`
}

type projectJSON struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Role   string     `json:"role"`
	Source sourceJSON `json:"source"`
}

func validName(name string) bool {
	if len(name) > maxNameLength || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// importProject inspects the repository and, without blockers, creates its
// project with the caller as owner. A replay answers before inspecting.
func (h *handler) importProject(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	k, ok := key(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := decode(body, &input); err != nil || !validRepoPath(input.Path) || !validName(input.Name) {
		writeError(w, 400, "bad_request")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, body))
	if h.replayed(w, r, user, k, nil) {
		return
	}
	if h.exec.Inspector == nil {
		h.gitUnavailable(w)
		return
	}
	result, err := h.exec.Inspector.Inspect(r.Context(), input.Path)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if len(result.Blockers) > 0 {
		writeJSON(w, 409, blockedBody{errorBody{"import_blocked", "the repository cannot be imported as it stands"}, result.Blockers})
		return
	}
	name := input.Name
	if name == "" {
		name = filepath.Base(result.Identity.Path)
	}
	h.mutate(w, r, user, k, nil, "", func(tx *store.Tx) (int, any, error) {
		id, err := newID()
		if err != nil {
			return 0, nil, err
		}
		src, err := tx.ImportProject(r.Context(), id, name, store.ProjectSource{Path: result.Identity.Path, CommonDir: result.Identity.CommonDir, Fingerprint: result.Fingerprint})
		if err != nil {
			return 0, nil, err
		}
		return 201, projectJSON{id, name, "owner", sourceJSON{src.Path, src.CommonDir, src.Fingerprint, src.Imported}}, nil
	})
}

type artifactJSON struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

type snapshotFileJSON struct {
	Path     string       `json:"path"`
	Mode     string       `json:"mode"`
	Artifact artifactJSON `json:"artifact"`
}

type snapshotJSON struct {
	ID          string             `json:"id"`
	Project     string             `json:"project"`
	BaseCommit  string             `json:"base_commit"`
	Fingerprint string             `json:"fingerprint"`
	Staged      artifactJSON       `json:"staged_patch"`
	Unstaged    artifactJSON       `json:"unstaged_patch"`
	Untracked   []snapshotFileJSON `json:"untracked"`
	CreatedBy   string             `json:"created_by"`
	Created     string             `json:"created"`
}

func snapshotValue(s store.Snapshot) snapshotJSON {
	files := make([]snapshotFileJSON, 0, len(s.Untracked))
	for _, f := range s.Untracked {
		files = append(files, snapshotFileJSON{f.Path, f.Mode, artifactJSON(f.Artifact)})
	}
	return snapshotJSON{s.ID, s.Project, s.BaseCommit, s.Fingerprint, artifactJSON(s.Staged), artifactJSON(s.Unstaged), files, s.CreatedBy, s.Created}
}

// validSelection refuses an oversized, duplicated or malformed list, and an
// included path the selection does not take.
func validSelection(sel source.Selection) bool {
	if len(sel.Untracked) > maxSelected || len(sel.IncludeSensitive) > len(sel.Untracked) {
		return false
	}
	seen := map[string]bool{}
	for _, p := range sel.Untracked {
		if p == "" || len(p) > maxPathLength || !utf8.ValidString(p) || strings.ContainsRune(p, 0) || seen[p] {
			return false
		}
		seen[p] = true
	}
	included := map[string]bool{}
	for _, p := range sel.IncludeSensitive {
		if !seen[p] || included[p] {
			return false
		}
		included[p] = true
	}
	return true
}

// captureError answers a capture that was refused or failed.
func captureError(w http.ResponseWriter, err error) {
	if e, ok := errors.AsType[*source.BlockedError](err); ok {
		writeJSON(w, 409, blockedBody{errorBody{"import_blocked", "the repository cannot be captured as it stands"}, e.Blockers})
		return
	}
	if e, ok := errors.AsType[*source.PathError](err); ok {
		writeJSON(w, 422, pathBody{errorBody{e.Code, e.Reason}, e.Path})
		return
	}
	switch {
	case errors.Is(err, source.ErrSourceChanged):
		writeJSON(w, 409, errorBody{"source_changed", "the repository changed during the snapshot; try again"})
	case errors.Is(err, source.ErrTooLarge):
		writeJSON(w, 413, errorBody{"capture_too_large", err.Error()})
	default:
		writeInternalError(w, err)
	}
}

// snapshot captures an imported project's staged, unstaged and selected
// untracked work for a member and stores it as artifacts. A non-member and a
// replay are answered before the repository is read; a refused capture
// stores nothing.
func (h *handler) snapshot(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	k, ok := key(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Untracked        []string `json:"untracked"`
		IncludeSensitive []string `json:"include_sensitive"`
	}
	sel := source.Selection{}
	if err := decode(body, &input); err != nil {
		writeError(w, 400, "bad_request")
		return
	}
	sel.Untracked, sel.IncludeSensitive = input.Untracked, input.IncludeSensitive
	if !validSelection(sel) {
		writeError(w, 400, "bad_request")
		return
	}
	project := r.PathValue("id")
	scope := &store.Scope{ProjectID: project}
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, body))
	if h.replayed(w, r, user, k, scope) {
		return
	}
	src, err := h.store.ProjectSource(r.Context(), user, project)
	if errors.Is(err, store.ErrNoSource) {
		writeJSON(w, 409, errorBody{"no_source", "the project has no imported repository"})
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	if h.exec.Capturer == nil {
		h.gitUnavailable(w)
		return
	}
	if h.exec.Writer == nil {
		writeJSON(w, 503, errorBody{"artifacts_unavailable", "the artifact store is not open"})
		return
	}
	c, err := h.exec.Capturer.Capture(r.Context(), src.Path, sel)
	if err != nil {
		captureError(w, err)
		return
	}
	if id := c.Inspection.Identity; id.Path != src.Path || id.CommonDir != src.CommonDir {
		// Another repository now stands at the imported path.
		captureError(w, source.ErrSourceChanged)
		return
	}
	snap := store.Snapshot{Project: project, BaseCommit: c.Inspection.Identity.Head, Fingerprint: c.Inspection.Fingerprint}
	if snap.ID, err = newID(); err != nil {
		writeInternalError(w, err)
		return
	}
	// Content is written before the transaction that references it. Blobs a
	// failed transaction leaves are unreferenced, so no /v1 read serves them;
	// they are not removed here, since the executor may be referencing the
	// same content under its own lock.
	write := func(data []byte) (store.ArtifactRef, error) {
		ref, err := h.exec.Writer.Write(r.Context(), bytes.NewReader(data), int64(len(data)))
		return store.ArtifactRef{Hash: ref.Hash, Size: ref.Size}, err
	}
	if snap.Staged, err = write(c.Staged); err == nil {
		snap.Unstaged, err = write(c.Unstaged)
	}
	for _, f := range c.Untracked {
		if err != nil {
			break
		}
		var ref store.ArtifactRef
		ref, err = write(f.Content)
		snap.Untracked = append(snap.Untracked, store.SnapshotFile{Path: f.Path, Mode: f.Mode, Artifact: ref})
	}
	if err != nil {
		writeInternalError(w, fmt.Errorf("snapshot artifact: %w", err))
		return
	}
	h.mutate(w, r, user, k, scope, "", func(tx *store.Tx) (int, any, error) {
		recorded, err := tx.RecordSnapshot(r.Context(), snap)
		if err != nil {
			return 0, nil, err
		}
		return 201, snapshotValue(recorded), nil
	})
}

func (h *handler) getSnapshot(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	snap, err := h.store.Snapshot(r.Context(), user, r.PathValue("id"), r.PathValue("sid"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, snapshotValue(snap))
}
