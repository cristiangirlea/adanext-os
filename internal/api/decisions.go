package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/cristiangirlea/ada-os/internal/store"
)

type decisionScopeJSON struct {
	Backend   string `json:"backend"`
	Mode      string `json:"mode"`
	Workspace string `json:"workspace"`
}

type decisionJSON struct {
	Actor          string            `json:"actor"`
	Required       bool              `json:"required"`
	Scope          decisionScopeJSON `json:"scope"`
	ProjectDefault string            `json:"project_default"`
	Allowed        bool              `json:"allowed"`
	Denied         bool              `json:"denied"`
}

func decisionValue(d *store.Decision) *decisionJSON {
	if d == nil {
		return nil
	}
	return &decisionJSON{d.Actor, d.Required, decisionScopeJSON{d.Scope.Backend, d.Scope.Mode, d.Scope.Workspace}, d.ProjectDefault, d.Allowed, d.Denied}
}

// decide records the actor's allow or deny, or a project owner's deny, on a pending attempt
// that waits for one, and answers the task. An allow wakes the scheduler; a deny closes the
// attempt canceled and wakes its open streams.
func (h *handler) decide(w http.ResponseWriter, r *http.Request) {
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
		Decision string `json:"decision"`
	}
	if err := decode(body, &input); err != nil || (input.Decision != "allow" && input.Decision != "deny") {
		writeError(w, 400, "bad_request")
		return
	}
	id := r.PathValue("id")
	project, err := h.store.AttemptProject(r.Context(), user, id)
	if err != nil {
		storeError(w, err)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, body))
	var result store.DecisionResult
	decided := false
	ok = h.mutate(w, r, user, k, &store.Scope{ProjectID: project}, "", func(tx *store.Tx) (int, any, error) {
		at := h.now()
		var err error
		if result, err = tx.Decide(r.Context(), id, input.Decision, k, at); err != nil {
			return 0, nil, err
		}
		decided = true
		task, err := tx.TaskWithAttempt(r.Context(), result.Task, at)
		return 200, h.taskAtTip(task), err
	})
	if !ok || !decided {
		return
	}
	if result.Denied {
		h.hub.Notify(id)
		return
	}
	if h.exec.Runner != nil {
		h.exec.Runner.LaunchAttempt(id)
	}
}

type memberJSON struct {
	User  string `json:"user"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	Added string `json:"added"`
}

func membersValue(members []store.Member) []memberJSON {
	out := make([]memberJSON, 0, len(members))
	for _, m := range members {
		out = append(out, memberJSON{m.User, m.Name, m.Role, m.Added})
	}
	return out
}

func (h *handler) listMembers(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.ownerActor(w, r); !ok {
		return
	}
	members, err := h.store.Members(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, membersValue(members))
}

// setMember adds an account to a project or changes its role; a changed role ends every
// session of the account and closes their streams.
func (h *handler) setMember(w http.ResponseWriter, r *http.Request) {
	user, k, body, r, ok := h.ownerMutation(w, r)
	if !ok {
		return
	}
	var input struct {
		Role string `json:"role"`
	}
	if err := decode(body, &input); err != nil || (input.Role != "owner" && input.Role != "member") {
		writeError(w, 400, "bad_request")
		return
	}
	project, member := r.PathValue("id"), r.PathValue("user")
	var ended []string
	ok = h.mutate(w, r, user, k, nil, "", func(tx *store.Tx) (int, any, error) {
		var err error
		if ended, err = tx.SetMember(r.Context(), project, member, input.Role, h.now()); err != nil {
			return 0, nil, err
		}
		members, err := tx.Members(r.Context(), project)
		return 200, membersValue(members), err
	})
	if ok {
		h.closeSessions(ended)
	}
}

// removeMember removes a member in one transaction that denies their pending decisions in the
// project and cancels their attempts there; the executor is then told to cancel the launched ones.
func (h *handler) removeMember(w http.ResponseWriter, r *http.Request) {
	user, k, body, r, ok := h.ownerMutation(w, r)
	if !ok {
		return
	}
	if !emptyBody(body) {
		writeError(w, 400, "bad_request")
		return
	}
	project, member := r.PathValue("id"), r.PathValue("user")
	var removal store.Removal
	ok = h.mutate(w, r, user, k, nil, "", func(tx *store.Tx) (int, any, error) {
		var err error
		if removal, err = tx.RemoveMembership(r.Context(), project, member, k, h.now()); err != nil {
			return 0, nil, err
		}
		members, err := tx.Members(r.Context(), project)
		return 200, membersValue(members), err
	})
	if !ok {
		return
	}
	for _, id := range removal.Attempts {
		h.hub.Notify(id)
	}
	if h.exec.Runner != nil {
		for _, task := range removal.Running {
			h.exec.Runner.CancelTask(task)
		}
	}
}

// setDefaultMode sets a project's default mode. Nothing is cancelled: pending attempts are
// judged again at their claim, so the scheduler is woken for any a higher default now admits.
func (h *handler) setDefaultMode(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Idempotency-Key") == "" {
		// Setting a mode twice is harmless, so a request without a key gets one of its own.
		id, err := newID()
		if err != nil {
			writeInternalError(w, err)
			return
		}
		r.Header.Set("Idempotency-Key", "default-mode-"+id)
	}
	user, k, body, r, ok := h.ownerMutation(w, r)
	if !ok {
		return
	}
	var input struct {
		Mode string `json:"mode"`
	}
	if err := decode(body, &input); err != nil || !store.ValidMode(input.Mode) {
		writeError(w, 400, "bad_request")
		return
	}
	project := r.PathValue("id")
	ok = h.mutate(w, r, user, k, nil, "", func(tx *store.Tx) (int, any, error) {
		return 200, struct {
			ID          string `json:"id"`
			DefaultMode string `json:"default_mode"`
		}{project, input.Mode}, tx.SetDefaultMode(r.Context(), project, input.Mode)
	})
	if ok && h.exec.Runner != nil {
		h.exec.Runner.LaunchAttempt("")
	}
}

type stopCountsJSON struct {
	Running       int `json:"running"`
	Queued        int `json:"queued"`
	OthersRunning int `json:"others_running"`
	OthersQueued  int `json:"others_queued"`
}

func stopCountsValue(c store.StopCounts) stopCountsJSON {
	return stopCountsJSON{c.Running, c.Queued, c.OthersRunning, c.OthersQueued}
}

// stopCounts answers what the owner's stop would end, stopping nothing.
func (h *handler) stopCounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.ownerActor(w, r); !ok {
		return
	}
	counts, err := h.store.StopCounts(r.Context(), h.exec.Generation, h.exec.Owner)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, stopCountsValue(counts))
}

// staleStopError is a stop whose confirmed count of other users' work no longer holds.
type staleStopError struct{ counts store.StopCounts }

func (e *staleStopError) Error() string { return "stop count changed" }

func writeStaleStop(w http.ResponseWriter, err error) bool {
	stale, ok := errors.AsType[*staleStopError](err)
	if !ok {
		return false
	}
	writeJSON(w, 409, struct {
		errorBody
		stopCountsJSON
	}{errorBody{"stop_count_changed", errorMessage("stop_count_changed")}, stopCountsValue(stale.counts)})
	return true
}
