package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/ada-os/internal/engine"
	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

// Reviewer reads a task's changes and files from Ada's clone, commits an ended attempt's remaining
// changes and removes its worktree; *source.Workspaces satisfies it.
type Reviewer interface {
	Changes(ctx context.Context, project, from, to, configSHA256 string, lim source.ChangeLimits) (source.ChangeSet, error)
	Files(ctx context.Context, project, commit, path, configSHA256 string, maxEntries int, maxBytes int64) (source.FileView, error)
	CommitAgentChanges(ctx context.Context, m source.Manifest, hash string) (tested, imported string, err error)
	RemoveWorktree(project, attempt string) error
}

// TaskActivity reports a task whose attempt the executor still runs or tears down;
// *executor.Executor satisfies it.
type TaskActivity interface{ TaskActive(task string) bool }

// The caps of the files route.
const (
	maxFileEntries = 5000
	maxFileBytes   = 1 << 20
)

var (
	errNoChanges         = errors.New("the task has no agent-changes commit")
	errWorkspaceMismatch = errors.New("the workspace no longer matches what Ada recorded")
)

func reviewError(w http.ResponseWriter, err error) {
	if e, ok := errors.AsType[*store.ProviderUnacknowledgedError](err); ok {
		writeJSON(w, 409, struct {
			errorBody
			ProviderChanges store.ProviderChanges `json:"provider_changes"`
		}{errorBody{"provider_configuration_changed", e.Error()}, e.Changes})
		return
	}
	if _, ok := errors.AsType[*source.MismatchError](err); ok {
		err = errWorkspaceMismatch
	}
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{store.ErrNotAuthorized, 403, "forbidden"},
		{store.ErrStaleReview, 409, "stale_review"},
		{store.ErrAttemptActive, 409, "attempt_active"},
		{store.ErrWorktreeNotRemoved, 409, "attempt_active"},
		{store.ErrAttemptRecovery, 409, "recovery_required"},
		{store.ErrNoAttempt, 409, "no_attempt"},
		{store.ErrInvalidFinding, 400, "bad_request"},
		{store.ErrTooManyFindings, 409, "too_many_findings"},
		{source.ErrNoSuchPath, 404, "not_found"},
		{errNoChanges, 409, "no_changes"},
		{errWorkspaceMismatch, 409, "workspace_mismatch"},
	} {
		if errors.Is(err, c.err) {
			writeJSON(w, c.status, errorBody{c.code, c.err.Error()})
			return
		}
	}
	storeError(w, err)
}

// rawText is text as JSON carries it: the bytes themselves when they are UTF-8, else base64 in
// the _raw twin, so a path is always the bytes git reported.
func rawText(b string) (text, raw string) {
	if utf8.ValidString(b) {
		return b, ""
	}
	return b, base64.StdEncoding.EncodeToString([]byte(b))
}

type changedFileJSON struct {
	Path           string `json:"path"`
	PathRaw        string `json:"path_raw,omitempty"`
	OldPath        string `json:"old_path,omitempty"`
	OldPathRaw     string `json:"old_path_raw,omitempty"`
	Status         string `json:"status"`
	Similarity     int    `json:"similarity,omitempty"`
	OldMode        string `json:"old_mode"`
	NewMode        string `json:"new_mode"`
	Binary         bool   `json:"binary"`
	Added          int    `json:"added"`
	Deleted        int    `json:"deleted"`
	Patch          string `json:"patch"`
	PatchRaw       string `json:"patch_raw,omitempty"`
	PatchTruncated bool   `json:"patch_truncated"`
	// Provider marks provider configuration or hooks the CLI would load from the worktree.
	Provider bool `json:"provider"`
}

type changesJSON struct {
	Task            string                 `json:"task"`
	Attempt         string                 `json:"attempt"`
	Base            string                 `json:"base"`
	Commit          string                 `json:"commit"`
	Tip             string                 `json:"tip"`
	Stale           bool                   `json:"stale"`
	Generation      int64                  `json:"generation"`
	RenameThreshold string                 `json:"rename_threshold"`
	Limits          map[string]int64       `json:"limits"`
	Files           []changedFileJSON      `json:"files"`
	Total           int                    `json:"total"`
	FilesTruncated  bool                   `json:"files_truncated"`
	BytesTruncated  bool                   `json:"bytes_truncated"`
	TotalCapped     bool                   `json:"total_capped"` // total is a lower bound
	ProviderChanges *store.ProviderChanges `json:"provider_changes"`
	ProviderFlagged bool                   `json:"provider_flagged"`
	ProviderFiles   bool                   `json:"provider_files"`
	Checks          []store.CheckResult    `json:"checks"`
}

// changed is the task's newest agent-changes commit for a current member, with the clone
// config's recorded hash; errNoChanges when no attempt has one.
func (h *handler) changed(ctx context.Context, user, task string) (store.ReviewState, store.ReviewAttempt, string, error) {
	rs, err := h.store.ReviewState(ctx, user, task)
	if err != nil {
		return rs, store.ReviewAttempt{}, "", err
	}
	a, ok := rs.Changed()
	if !ok || a.ImportedCommit == "" {
		return rs, a, "", errNoChanges
	}
	if h.exec.Review == nil {
		return rs, a, "", errEngineUnavailable
	}
	cfg, err := h.store.CloneConfig(ctx, rs.Project)
	return rs, a, cfg, err
}

// changes serves the task's own changes, from the imported-changes commit to the newest
// agent-changes commit, to a current member.
func (h *handler) changes(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	rs, a, cfg, err := h.changed(r.Context(), user, id)
	if err != nil {
		reviewError(w, err)
		return
	}
	lim := source.DefaultChangeLimits
	set, err := h.exec.Review.Changes(r.Context(), rs.Project, a.ImportedCommit, a.TestedCommit, cfg, lim)
	if err != nil {
		reviewError(w, err)
		return
	}
	tip := h.tip(rs.Project, id)
	out := changesJSON{Task: id, Attempt: a.ID, Base: a.ImportedCommit, Commit: a.TestedCommit, Tip: tip, Stale: tip != a.TestedCommit, Generation: rs.Generation,
		RenameThreshold: source.RenameThreshold, Limits: map[string]int64{"files": int64(lim.Files), "file_bytes": lim.FileBytes, "total_bytes": lim.TotalBytes},
		Files: make([]changedFileJSON, 0, len(set.Files)), Total: set.Total, FilesTruncated: set.FilesTruncated, BytesTruncated: set.BytesTruncated, TotalCapped: set.TotalCapped,
		ProviderChanges: a.ProviderChanges, ProviderFlagged: a.ProviderFlagged()}
	for _, f := range set.Files {
		j := changedFileJSON{Status: f.Status, Similarity: f.Similarity, OldMode: f.OldMode, NewMode: f.NewMode, Binary: f.Binary, Added: f.Added, Deleted: f.Deleted, PatchTruncated: f.PatchTruncated,
			Provider: source.IsProviderPath(f.Path) || (f.OldPath != "" && source.IsProviderPath(f.OldPath))}
		j.Path, j.PathRaw = rawText(f.Path)
		j.OldPath, j.OldPathRaw = rawText(f.OldPath)
		j.Patch, j.PatchRaw = rawText(string(f.Patch))
		out.ProviderFiles = out.ProviderFiles || j.Provider
		out.Files = append(out.Files, j)
	}
	if out.Checks, err = h.store.CheckResults(r.Context(), user, id, h.tip); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, out)
}

type fileEntryJSON struct {
	Name    string `json:"name"`
	NameRaw string `json:"name_raw,omitempty"`
	Path    string `json:"path"`
	PathRaw string `json:"path_raw,omitempty"`
	Type    string `json:"type"`
	Mode    string `json:"mode"`
	Size    int64  `json:"size"`
	// Submodule is a submodule's recorded commit.
	Submodule string `json:"submodule,omitempty"`
}

type fileJSON struct {
	Task             string          `json:"task"`
	Commit           string          `json:"commit"`
	Path             string          `json:"path"`
	Type             string          `json:"type"`
	Mode             string          `json:"mode"`
	Entries          []fileEntryJSON `json:"entries,omitempty"`
	EntriesTruncated bool            `json:"entries_truncated,omitempty"`
	Size             int64           `json:"size"`
	Encoding         string          `json:"encoding,omitempty"`
	Content          *string         `json:"content,omitempty"`
	Truncated        bool            `json:"truncated"`
	Target           *string         `json:"target,omitempty"`
	Submodule        string          `json:"submodule,omitempty"`
	TargetRaw        string          `json:"target_raw,omitempty"`
}

// files lists a directory or reads a file of the newest agent-changes commit, ?path= naming it
// ("" for the root), through git alone, for a current member.
func (h *handler) files(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	rs, a, cfg, err := h.changed(r.Context(), user, id)
	if err != nil {
		reviewError(w, err)
		return
	}
	path := r.URL.Query().Get("path")
	view, err := h.exec.Review.Files(r.Context(), rs.Project, a.TestedCommit, path, cfg, maxFileEntries, maxFileBytes)
	if err != nil {
		reviewError(w, err)
		return
	}
	out := fileJSON{Task: id, Commit: view.Commit, Path: view.Path, Type: view.Type, Mode: view.Mode, EntriesTruncated: view.EntriesTruncated, Size: view.Size, Truncated: view.Truncated, Submodule: view.Submodule}
	switch view.Type {
	case "tree":
		out.Entries = make([]fileEntryJSON, 0, len(view.Entries))
		for _, e := range view.Entries {
			j := fileEntryJSON{Type: e.Type, Mode: e.Mode, Size: e.Size, Submodule: e.Submodule}
			j.Name, j.NameRaw = rawText(e.Name)
			j.Path, j.PathRaw = rawText(e.Path)
			out.Entries = append(out.Entries, j)
		}
	case "symlink":
		target, raw := rawText(view.Target)
		out.Target, out.TargetRaw = &target, raw
	case "blob":
		content := string(view.Content)
		out.Encoding = "utf-8"
		if !utf8.ValidString(content) {
			out.Encoding, content = "base64", base64.StdEncoding.EncodeToString(view.Content)
		}
		out.Content = &content
	}
	writeJSON(w, 200, out)
}

func (h *handler) listFindings(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	findings, err := h.store.Findings(r.Context(), user, r.PathValue("id"))
	if err != nil {
		reviewError(w, err)
		return
	}
	writeJSON(w, 200, findings)
}

// addFinding records a current member's finding on the attempt the task shows.
func (h *handler) addFinding(w http.ResponseWriter, r *http.Request) {
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
		Body string `json:"body"`
	}
	if err := decode(body, &input); err != nil {
		writeError(w, 400, "bad_request")
		return
	}
	id := r.PathValue("id")
	project, err := h.store.TaskProject(r.Context(), user, id)
	if err != nil {
		storeError(w, err)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, body))
	cached, err := h.commit(r, user, k, &store.Scope{ProjectID: project}, "", func(tx *store.Tx) (int, any, error) {
		f, err := tx.RecordFinding(r.Context(), id, input.Body)
		return 201, f, err
	})
	if err != nil {
		reviewError(w, err)
		return
	}
	writeCached(w, cached)
}

// revisionAndBody reads a review mutation's key, If-Match revision and body.
func revisionAndBody(w http.ResponseWriter, r *http.Request) (string, int64, []byte, bool) {
	k, ok := key(w, r)
	if !ok {
		return "", 0, nil, false
	}
	match := r.Header.Get("If-Match")
	revision, err := matchRevision(match)
	if err != nil || match == "" || revision < 0 {
		writeError(w, 400, "bad_request")
		return "", 0, nil, false
	}
	body, ok := readBody(w, r)
	return k, revision, body, ok
}

type acceptJSON struct {
	Task   taskJSON     `json:"task"`
	Review store.Review `json:"review"`
}

// accept is the attempt's actor's or a project owner's acceptance of the commit and generation
// they reviewed (#3); a commit or generation that is no longer current is refused.
func (h *handler) accept(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	k, revision, body, ok := revisionAndBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Commit     string `json:"commit"`
		Generation *int64 `json:"generation"`
	}
	if err := decode(body, &input); err != nil || input.Generation == nil || !printableASCII(input.Commit) {
		writeError(w, 400, "bad_request")
		return
	}
	id := r.PathValue("id")
	project, err := h.store.TaskProject(r.Context(), user, id)
	if err != nil {
		storeError(w, err)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, body))
	tip := h.tip(project, id)
	cached, err := h.commit(r, user, k, &store.Scope{ProjectID: project}, strconv.FormatInt(revision, 10), func(tx *store.Tx) (int, any, error) {
		review, err := tx.Accept(r.Context(), id, revision, input.Commit, *input.Generation, tip)
		if err != nil {
			return 0, nil, err
		}
		task, err := scanTaskTx(r.Context(), tx, id)
		return 200, acceptJSON{taskValue(task), review}, err
	})
	if err != nil {
		reviewError(w, err)
		return
	}
	writeCached(w, cached)
}

// refinedTaskJSON is a refine's answer: the started task and the ids of the findings the new
// attempt's prompt carries, from any member, by design.
type refinedTaskJSON struct {
	startedTaskJSON
	Findings []string `json:"findings"`
}

// refine starts a new attempt of the task as a fresh engine session of the requester, on their
// profile, continuing ada/<task> from its tip with the findings in its prompt. An ended attempt's
// remaining changes are committed and its worktree removed first; each of those steps is safe to
// repeat after a crash, so a refused or failed refine can be sent again.
func (h *handler) refine(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	k, revision, body, ok := revisionAndBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Backend     string `json:"backend"`
		Model       string `json:"model"`
		Effort      string `json:"effort"`
		Mode        string `json:"mode"`
		MaxTurns    int    `json:"max_turns"`
		Timeout     string `json:"timeout"`
		Acknowledge bool   `json:"acknowledge_provider_changes"`
	}
	if err := decode(body, &input); err != nil || !validBackend(input.Backend) || !validModel(input.Model) || !printableASCII(input.Effort) || !validBackend(input.Mode) || input.MaxTurns < 0 || input.MaxTurns > maxTurnLimit {
		writeError(w, 400, "bad_request")
		return
	}
	timeout, ok := startTimeout(input.Timeout)
	if !ok {
		writeError(w, 400, "bad_request")
		return
	}
	id := r.PathValue("id")
	project, err := h.store.TaskProject(r.Context(), user, id)
	if err != nil {
		storeError(w, err)
		return
	}
	if err := engine.Capabilities().Check(engine.StartOptions{Backend: input.Backend, Mode: input.Mode, Model: input.Model, Effort: input.Effort, MaxTurns: input.MaxTurns, Timeout: timeout}); err != nil {
		var unsupported *engine.UnsupportedError
		if errors.As(err, &unsupported) {
			writeJSON(w, 409, errorBody{"unsupported_options", unsupported.Reason})
			return
		}
		writeInternalError(w, err)
		return
	}
	limit := ""
	if timeout > 0 {
		limit = timeout.String()
	}
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, body))
	scope := &store.Scope{ProjectID: project}
	if h.replayedAt(w, r, user, k, scope, strconv.FormatInt(revision, 10)) {
		return
	}
	h.refineMu.Lock()
	defer h.refineMu.Unlock()
	if err := h.releaseEarlierWork(r.Context(), user, id, revision, input.Acknowledge); err != nil {
		reviewError(w, err)
		return
	}
	h.admission.mu.RLock()
	defer h.admission.mu.RUnlock()
	var attemptID string
	cached, err := h.commit(r, user, k, scope, strconv.FormatInt(revision, 10), func(tx *store.Tx) (int, any, error) {
		if h.admission.stopping {
			return 0, nil, errShuttingDown
		}
		if h.exec.EngineSHA256 == "" || h.exec.Runner == nil {
			return 0, nil, errEngineUnavailable
		}
		var generation int64
		if err := tx.QueryRowContext(r.Context(), "SELECT generation FROM coordinator WHERE id=1").Scan(&generation); err != nil {
			return 0, nil, err
		}
		if generation != h.exec.Generation {
			return 0, nil, store.ErrStaleOwnership
		}
		queued, err := tx.QueuedAttempts(r.Context(), user)
		if err != nil {
			return 0, nil, err
		}
		if queued >= maxQueuedPerUser {
			return 0, nil, errQueueFull
		}
		if attemptID, err = newID(); err != nil {
			return 0, nil, err
		}
		a := store.Attempt{ID: attemptID, Backend: input.Backend, Model: input.Model, Effort: input.Effort, Mode: input.Mode, Executor: "local",
			OwnershipGeneration: h.exec.Generation, EngineSHA256: h.exec.EngineSHA256, MaxTurns: input.MaxTurns, Timeout: limit}
		carried, err := tx.Refine(r.Context(), id, revision, a, input.Acknowledge)
		if err != nil {
			return 0, nil, err
		}
		task, err := scanTaskTx(r.Context(), tx, id)
		return 200, refinedTaskJSON{startedTaskJSON{h.taskAtTip(task), attemptID}, carried}, err
	})
	if err != nil {
		attemptID = ""
		reviewError(w, err)
		return
	}
	writeCached(w, cached)
	if attemptID != "" {
		h.exec.Runner.LaunchAttempt(attemptID)
	}
}

// releaseEarlierWork commits each ended attempt's remaining changes on ada/<task> and removes its
// worktree, once the refine would be allowed. An attempt whose worktree no longer matches its
// record moves to recovery_required, which refuses the refine.
func (h *handler) releaseEarlierWork(ctx context.Context, user, task string, revision int64, acknowledged bool) error {
	rs, err := h.store.ReviewState(ctx, user, task)
	if err != nil {
		return err
	}
	if err := rs.CheckRefine(user, revision, acknowledged, false); err != nil {
		return err
	}
	if active, ok := h.exec.Runner.(TaskActivity); ok && active.TaskActive(task) {
		return store.ErrAttemptActive
	}
	for _, a := range rs.Attempts {
		if !a.Workspace || a.Removed {
			continue
		}
		if h.exec.Review == nil {
			return errEngineUnavailable
		}
		if a.TestedCommit == "" {
			tested, err := h.commitRemaining(ctx, a.ID)
			if _, mismatch := errors.AsType[*source.MismatchError](err); mismatch {
				if rerr := h.store.RecoverEndedAttempt(ctx, a.ID, "workspace verification failed", err.Error()); rerr != nil {
					return rerr
				}
				if h.hub != nil {
					h.hub.Notify(a.ID)
				}
				return store.ErrAttemptRecovery
			}
			if err != nil {
				return err
			}
			if err := h.store.RecordAgentCommit(ctx, a.ID, tested); err != nil {
				return err
			}
		}
		if err := h.exec.Review.RemoveWorktree(rs.Project, a.ID); err != nil {
			return err
		}
		if err := h.store.MarkWorktreeRemoved(ctx, a.ID); err != nil {
			return err
		}
	}
	return nil
}

// commitRemaining commits what an ended attempt left in its worktree, as M4.2 commits an ok one.
func (h *handler) commitRemaining(ctx context.Context, attempt string) (string, error) {
	row, err := h.store.Workspace(ctx, attempt)
	if err != nil {
		return "", err
	}
	cfg, err := h.store.CloneConfig(ctx, row.Project)
	if err != nil {
		return "", err
	}
	m := source.Manifest{Version: row.ManifestVersion, Project: row.Project, Task: row.Task, TaskRevision: row.TaskRevision, Attempt: row.Attempt, Generation: row.Generation,
		Snapshot: row.Snapshot, BaseCommit: row.BaseCommit, Branch: row.Branch, Path: row.Path, Executor: row.Executor, Policy: row.Policy, ConfigSHA256: cfg}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tested, _, err := h.exec.Review.CommitAgentChanges(ctx, m, row.ManifestHash)
	return tested, err
}
