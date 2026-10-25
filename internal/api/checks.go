package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/cristiangirlea/ada-os/internal/store"
)

func scanTaskTx(ctx context.Context, tx *store.Tx, id string) (store.Task, error) {
	row := tx.QueryRowContext(ctx, "SELECT id,project,created_by,objective,acceptance_criteria,state,revision,created,updated,COALESCE(snapshot,''),COALESCE(checks_json,'[]') FROM tasks WHERE id=?", id)
	var task store.Task
	var checksJSON string
	if err := row.Scan(&task.ID, &task.Project, &task.CreatedBy, &task.Objective, &task.AcceptanceCriteria, &task.State, &task.Revision, &task.Created, &task.Updated, &task.Snapshot, &checksJSON); err != nil {
		return store.Task{}, err
	}
	if checksJSON != "" && checksJSON != "[]" {
		var defs []store.CheckDefinition
		if json.Unmarshal([]byte(checksJSON), &defs) == nil && len(defs) > 0 {
			task.Checks = &defs
		}
	}
	if task.Snapshot != "" {
		task.ProviderConfiguration, _ = tx.SnapshotProvider(ctx, task.Snapshot)
	}
	return task, nil
}

func (h *handler) ready(w http.ResponseWriter, r *http.Request) {
	h.taskMutation(w, r, func(ctx context.Context, tx *store.Tx, id string, current store.State, revision int64) error {
		if current != store.Draft && current != store.Refinement {
			return store.ErrInvalidTransition
		}
		return tx.Transition(ctx, id, current, store.Ready, revision)
	})
}

func (h *handler) setChecks(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	k, ok := key(w, r)
	if !ok {
		return
	}
	match := r.Header.Get("If-Match")
	revision, err := matchRevision(match)
	if err != nil || match == "" || revision < 0 {
		writeError(w, 400, "bad_request")
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Checks *[]store.CheckDefinition `json:"checks"`
	}
	if err := decode(body, &input); err != nil || input.Checks == nil {
		writeError(w, 400, "bad_request")
		return
	}
	if _, err := store.NormalizeCheckDefinitions(*input.Checks); err != nil {
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
	h.mutate(w, r, user, k, &store.Scope{ProjectID: project}, strconv.FormatInt(revision, 10), func(tx *store.Tx) (int, any, error) {
		if err := tx.SetTaskChecks(r.Context(), id, revision, *input.Checks); err != nil {
			return 0, nil, err
		}
		task, err := scanTaskTx(r.Context(), tx, id)
		return 200, taskValue(task), err
	})
}

func (h *handler) getChecks(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	results, err := h.store.CheckResults(r.Context(), user, r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, results)
}

func (h *handler) checkLog(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	hash, size, truncated, err := h.store.CheckLog(r.Context(), user, r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	w.Header().Set("X-Ada-Truncated", strconv.FormatBool(truncated))
	if hash == "" && size == 0 {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "0")
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(200)
		return
	}
	if h.exec.Artifacts == nil {
		writeError(w, 503, "engine_unavailable")
		return
	}
	h.streamBlob(w, hash, size)
}
