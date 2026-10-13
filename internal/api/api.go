// Package api exposes the coordinator's versioned HTTP interface.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/ada-os/internal/store"
)

type Authenticator func(*http.Request) (string, error)

type handler struct {
	store *store.Store
	hub   *Hub
	auth  Authenticator
}

func New(s *store.Store, hub *Hub, auth Authenticator) http.Handler {
	h := &handler{store: s, hub: hub, auth: auth}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tasks", h.create)
	mux.HandleFunc("GET /v1/tasks", h.list)
	mux.HandleFunc("GET /v1/tasks/{id}", h.get)
	mux.HandleFunc("POST /v1/tasks/{id}/ready", h.transition(store.Draft, store.Ready))
	mux.HandleFunc("POST /v1/tasks/{id}/start", h.transition(store.Ready, store.Preparing))
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", h.transition(store.Running, store.CancelRequested))
	mux.HandleFunc("GET /v1/attempts/{id}/events", h.events)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			probe := r.Clone(r.Context())
			probe.Method = method
			if _, pattern := mux.Handler(probe); pattern != "/" {
				writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
		}
		writeError(w, http.StatusNotFound, "not_found")
	})
	return mux
}

func (h *handler) actor(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, err := h.auth(r)
	if err != nil || user == "" {
		writeError(w, http.StatusInternalServerError, "internal")
		return "", false
	}
	return user, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code string) {
	message := map[string]string{"bad_request": "invalid request", "too_large": "request body too large", "not_found": "not found", "method_not_allowed": "method not allowed", "stale_revision": "stale revision", "invalid_transition": "invalid transition", "idempotency_conflict": "idempotency conflict", "internal": "internal error"}[code]
	writeJSON(w, status, struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{code, message})
}
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrNotMember):
		writeError(w, 404, "not_found")
	case errors.Is(err, store.ErrStaleRevision):
		writeError(w, 409, "stale_revision")
	case errors.Is(err, store.ErrInvalidTransition):
		writeError(w, 409, "invalid_transition")
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeError(w, 422, "idempotency_conflict")
	default:
		writeError(w, 500, "internal")
	}
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, 413, "too_large")
		} else {
			writeError(w, 400, "bad_request")
		}
		return nil, false
	}
	return body, true
}
func decode(body []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func key(w http.ResponseWriter, r *http.Request) (string, bool) {
	k := r.Header.Get("Idempotency-Key")
	if len(k) == 0 || len(k) > 255 {
		writeError(w, 400, "bad_request")
		return "", false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			writeError(w, 400, "bad_request")
			return "", false
		}
	}
	return k, true
}
func hash(r *http.Request, body []byte) string {
	sum := sha256.New()
	for _, part := range []string{r.Method, r.URL.Path, r.Header.Get("If-Match")} {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	sum.Write(body)
	return hex.EncodeToString(sum.Sum(nil))
}
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type taskJSON struct {
	ID        string      `json:"id"`
	Project   string      `json:"project"`
	CreatedBy string      `json:"created_by"`
	Objective string      `json:"objective"`
	Criteria  string      `json:"criteria"`
	State     store.State `json:"state"`
	Revision  int64       `json:"revision"`
	Created   string      `json:"created"`
	Updated   string      `json:"updated"`
}

func taskValue(t store.Task) taskJSON {
	return taskJSON{t.ID, t.Project, t.CreatedBy, t.Objective, t.AcceptanceCriteria, t.State, t.Revision, t.Created, t.Updated}
}

type cachedResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func (h *handler) mutate(w http.ResponseWriter, r *http.Request, user, k string, scope *store.Scope, fn func(*store.Tx) (int, any, error)) {
	body := r.Context().Value(bodyKey{}).([]byte)
	cached, err := h.store.Do(r.Context(), user, k, hash(r, body), scope, func(tx *store.Tx) ([]byte, error) {
		status, value, err := fn(tx)
		if err != nil {
			return nil, err
		}
		payload, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return json.Marshal(cachedResponse{status, payload})
	})
	if err != nil {
		storeError(w, err)
		return
	}
	var result cachedResponse
	if err := json.Unmarshal(cached, &result); err != nil {
		writeError(w, 500, "internal")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(result.Status)
	_, _ = w.Write(append(result.Body, '\n'))
}

type bodyKey struct{}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
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
		Project   string `json:"project"`
		Objective string `json:"objective"`
		Criteria  string `json:"criteria"`
	}
	if err := decode(body, &input); err != nil || input.Project == "" || input.Objective == "" {
		writeError(w, 400, "bad_request")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, body))
	h.mutate(w, r, user, k, &store.Scope{ProjectID: input.Project}, func(tx *store.Tx) (int, any, error) {
		id, err := newID()
		if err != nil {
			return 0, nil, err
		}
		at := time.Now().UTC().Format(time.RFC3339Nano)
		_, err = tx.ExecContext(r.Context(), "INSERT INTO tasks(id,project,created_by,objective,acceptance_criteria,state,revision,created,updated) VALUES(?,?,?,?,?,'draft',0,?,?)", id, input.Project, user, input.Objective, input.Criteria, at, at)
		return 201, taskValue(store.Task{ID: id, Project: input.Project, CreatedBy: user, Objective: input.Objective, AcceptanceCriteria: input.Criteria, State: store.Draft, Revision: 0, Created: at, Updated: at}), err
	})
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	tasks, err := h.store.ListTasks(r.Context(), user)
	if err != nil {
		storeError(w, err)
		return
	}
	result := make([]taskJSON, 0, len(tasks))
	for _, task := range tasks {
		result = append(result, taskValue(task))
	}
	writeJSON(w, 200, result)
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	task, err := h.store.Task(r.Context(), user, r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, taskValue(task))
}

func (h *handler) transition(from, to store.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := h.actor(w, r)
		if !ok {
			return
		}
		k, ok := key(w, r)
		if !ok {
			return
		}
		match := r.Header.Get("If-Match")
		revision, err := strconv.ParseInt(match, 10, 64)
		if err != nil || match == "" || revision < 0 {
			writeError(w, 400, "bad_request")
			return
		}
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		if len(body) != 0 {
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
		h.mutate(w, r, user, k, &store.Scope{ProjectID: project}, func(tx *store.Tx) (int, any, error) {
			var current store.State
			var currentRevision int64
			if err := tx.QueryRowContext(r.Context(), "SELECT state,revision FROM tasks WHERE id=?", id).Scan(&current, &currentRevision); err != nil {
				return 0, nil, err
			}
			if currentRevision != revision {
				return 0, nil, store.ErrStaleRevision
			}
			if current != from {
				return 0, nil, store.ErrInvalidTransition
			}
			if err := tx.Transition(r.Context(), id, from, to, revision); err != nil {
				return 0, nil, err
			}
			row := tx.QueryRowContext(r.Context(), "SELECT id,project,created_by,objective,acceptance_criteria,state,revision,created,updated FROM tasks WHERE id=?", id)
			var task store.Task
			err := row.Scan(&task.ID, &task.Project, &task.CreatedBy, &task.Objective, &task.AcceptanceCriteria, &task.State, &task.Revision, &task.Created, &task.Updated)
			return 200, taskValue(task), err
		})
	}
}

type eventJSON struct {
	Attempt string          `json:"attempt"`
	Seq     int64           `json:"seq"`
	Time    string          `json:"time"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

func eventValue(e store.Event) eventJSON {
	return eventJSON{e.Attempt, e.Seq, e.Time, e.Kind, e.Payload}
}

func (h *handler) events(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := h.store.AttemptProject(r.Context(), user, id); err != nil {
		storeError(w, err)
		return
	}
	cursor := r.URL.Query().Get("after")
	if values, exists := r.Header[http.CanonicalHeaderKey("Last-Event-ID")]; exists && strings.Join(values, ",") != "" {
		cursor = strings.Join(values, ",")
	}
	after := int64(0)
	if cursor != "" {
		var err error
		after, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || after < 0 {
			writeError(w, 400, "bad_request")
			return
		}
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		events, err := h.store.Events(r.Context(), user, id, after)
		if err != nil {
			storeError(w, err)
			return
		}
		result := make([]eventJSON, 0, len(events))
		for _, e := range events {
			result = append(result, eventValue(e))
		}
		writeJSON(w, 200, result)
		return
	}
	wake, unsubscribe := h.hub.subscribe(id)
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	poll := h.hub.PollInterval
	if poll <= 0 {
		poll = time.Second
	}
	heartbeat := h.hub.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = 15 * time.Second
	}
	pollTicker := time.NewTicker(poll)
	defer pollTicker.Stop()
	pingTicker := time.NewTicker(heartbeat)
	defer pingTicker.Stop()
	send := func() bool {
		events, err := h.store.Events(r.Context(), user, id, after)
		if err != nil {
			return false
		}
		for _, e := range events {
			data, err := json.Marshal(e.Payload)
			if err != nil {
				return false
			}
			kind := strings.Map(func(c rune) rune {
				if c == '\r' || c == '\n' {
					return -1
				}
				return c
			}, e.Kind)
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, kind, data); err != nil {
				return false
			}
			after = e.Seq
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.hub.done:
			return
		case <-wake:
			if !send() {
				return
			}
		case <-pollTicker.C:
			if !send() {
				return
			}
		case <-pingTicker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
