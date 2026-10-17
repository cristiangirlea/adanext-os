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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

type Authenticator func(*http.Request) (string, error)

var (
	errEngineUnavailable = errors.New("engine unavailable")
	errQueueFull         = errors.New("queue full")
	errShuttingDown      = errors.New("shutting down")
)

// maxQueuedPerUser is the queue limit of the decisions log (2026-10-14, #9):
// a user's fourth queued start is refused with queue_full.
const maxQueuedPerUser = 3

// stopReason is recorded on every running and queued attempt the owner's stop
// ends, visible to its actor as the attempt's cancel event.
const stopReason = "coordinator stopped by the owner"

type handler struct {
	store    *store.Store
	hub      *Hub
	auth     Authenticator
	exec     Execution
	readPage func(context.Context, string, string, int64, int, int, func(store.Event) int) ([]store.Event, error)
	readLast func(context.Context, string, string) (int64, error)
	// admission serialises start against the owner's stop: a start holds it
	// shared from its transaction through the scheduler handoff, the stop
	// exclusively from before its transaction through the executor handoff.
	admission         sync.RWMutex
	stopping          bool        // Set under admission by a stop that committed; starts are then refused.
	beforeStopHandoff func()      // Test seam between the stop's commit and the executor handoff.
	capturing         atomic.Bool // Held by the one snapshot capturing and storing at a time.
}

// AttemptRunner is the narrow API-to-executor notification boundary.
type AttemptRunner interface {
	LaunchAttempt(string)
	CancelTask(string)
}

// ArtifactOpener streams a stored artifact's bytes; *artifact.Store satisfies it.
type ArtifactOpener interface {
	Open(hash string) (io.ReadCloser, error)
}

// ArtifactReferences resolves an artifact to the size its record holds when a
// record of the attempt references it (the artifacts table's origin), and
// returns store.ErrNotFound otherwise: an artifact no record references is
// readable by no one through /v1 (decisions log, 2026-10-14, #3).
type ArtifactReferences interface {
	ArtifactReference(ctx context.Context, attemptID, hash string) (int64, error)
}

// Stopper is the coordinator's orderly stop, "Quit Ada and stop work". The
// stop handler calls it once the owner's request has recorded its reason on
// every running and queued attempt: the executor then cancels the running ones
// through its cancel path, and the coordinator exits.
type Stopper interface{ StopWork() }

// Inspector reads a repository without running its code; *source.Inspector
// satisfies it.
type Inspector interface {
	Inspect(ctx context.Context, path string) (source.Inspection, error)
}

// Capturer snapshots a repository's work between two equal fingerprints;
// *source.Inspector satisfies it.
type Capturer interface {
	Capture(ctx context.Context, path string, sel source.Selection) (source.Capture, error)
}

// ArtifactWriter stores content by its hash; *artifact.Store satisfies it.
// Its lock is the store's lifecycle lock, shared with the executor.
type ArtifactWriter interface {
	Write(ctx context.Context, r io.Reader, limit int64) (artifact.Ref, error)
	Remove(hash string) error
	sync.Locker
}

type Execution struct {
	EngineSHA256 string
	Generation   int64
	Runner       AttemptRunner
	Artifacts    ArtifactOpener     // nil until the coordinator opens its artifact store
	References   ArtifactReferences // nil until the store resolves references by attempt
	Owner        string             // The coordinator's owner account, the only one that may stop it.
	Stopper      Stopper            // nil when there is no coordinator lifecycle to stop (tests).
	Inspector    Inspector          // nil when git is missing or too old
	Capturer     Capturer           // nil with Inspector
	Writer       ArtifactWriter     // stores snapshot content; nil until the artifact store opens
	// GitUnavailable is why Inspector is nil, the message of git_unavailable.
	GitUnavailable string
}

func New(s *store.Store, hub *Hub, auth Authenticator, execution ...Execution) http.Handler {
	h := &handler{store: s, hub: hub, auth: auth}
	if len(execution) != 0 {
		h.exec = execution[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tasks", h.create)
	mux.HandleFunc("GET /v1/tasks", h.list)
	mux.HandleFunc("GET /v1/tasks/{id}", h.get)
	mux.HandleFunc("POST /v1/tasks/{id}/ready", h.transition(store.Draft, store.Ready))
	mux.HandleFunc("POST /v1/tasks/{id}/start", h.start)
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", h.cancel)
	mux.HandleFunc("POST /v1/coordinator/stop", h.stop)
	mux.HandleFunc("POST /v1/projects/inspect", h.inspect)
	mux.HandleFunc("POST /v1/projects", h.importProject)
	mux.HandleFunc("POST /v1/projects/{id}/snapshots", h.snapshot)
	mux.HandleFunc("GET /v1/projects/{id}/snapshots/{sid}", h.getSnapshot)
	mux.HandleFunc("GET /v1/attempts/{id}/events", h.events)
	mux.HandleFunc("GET /v1/attempts/{id}/artifacts/{hash}", h.artifact)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var allowed []string
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			probe := r.Clone(r.Context())
			probe.Method = method
			if _, pattern := mux.Handler(probe); pattern != "/" {
				allowed = append(allowed, method)
			}
		}
		if len(allowed) != 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		writeError(w, http.StatusNotFound, "not_found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := newID()
		if err != nil {
			id = fmt.Sprintf("%d", time.Now().UnixNano())
		}
		mux.ServeHTTP(&requestWriter{ResponseWriter: w, id: id}, r)
	})
}

type requestWriter struct {
	http.ResponseWriter
	id string
}

func (w *requestWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *requestWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *requestWriter) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (h *handler) actor(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, err := h.auth(r)
	if err != nil || user == "" {
		writeInternalError(w, err)
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
	message := map[string]string{"bad_request": "invalid request", "too_large": "request body too large", "not_found": "not found", "method_not_allowed": "method not allowed", "stale_revision": "stale revision", "invalid_transition": "invalid transition", "idempotency_conflict": "idempotency conflict", "engine_unavailable": "engine unavailable", "internal": "internal error", "cursor_ahead": "cursor ahead", "too_many_streams": "too many streams", "shutting_down": "shutting down", "queue_full": "queue full", "forbidden": "forbidden"}[code]
	if status == 500 {
		writeInternalError(w, errors.New("unspecified internal error"))
		return
	}
	writeJSON(w, status, struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{code, message})
}

func writeInternalError(w http.ResponseWriter, err error) {
	id := "unknown"
	if rw, ok := w.(*requestWriter); ok {
		id = rw.id
	}
	fmt.Fprintf(os.Stderr, "adad: request %s: %v\n", id, err)
	writeJSON(w, 500, struct {
		Error     string `json:"error"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}{"internal", "internal error", id})
}
func storeError(w http.ResponseWriter, err error) {
	if e, ok := errors.AsType[*store.AlreadyImportedError](err); ok {
		writeAlreadyImported(w, e)
		return
	}
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrNotMember):
		writeError(w, 404, "not_found")
	case errors.Is(err, store.ErrStaleRevision):
		writeError(w, 409, "stale_revision")
	case errors.Is(err, store.ErrInvalidTransition):
		writeError(w, 409, "invalid_transition")
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeError(w, 422, "idempotency_conflict")
	case errors.Is(err, store.ErrStaleOwnership):
		writeError(w, 503, "engine_unavailable")
	case errors.Is(err, errEngineUnavailable):
		writeError(w, 503, "engine_unavailable")
	case errors.Is(err, errQueueFull):
		writeError(w, 409, "queue_full")
	case errors.Is(err, errShuttingDown):
		writeError(w, 503, "shutting_down")
	default:
		writeInternalError(w, err)
	}
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeInternalError(w, err)
		return nil, false
	}
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
func hash(r *http.Request, body []byte, revision string) string {
	sum := sha256.New()
	for _, part := range []string{r.Method, r.URL.Path, revision} {
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
	ID        string           `json:"id"`
	Project   string           `json:"project"`
	CreatedBy string           `json:"created_by"`
	Objective string           `json:"objective"`
	Criteria  string           `json:"criteria"`
	State     store.State      `json:"state"`
	Revision  int64            `json:"revision"`
	Created   string           `json:"created"`
	Updated   string           `json:"updated"`
	Attempt   *taskAttemptJSON `json:"attempt"`
	// QueuePosition is the task's 1-based place among the pending attempts of
	// all users while it is queued; absent otherwise. Only GET reports it.
	QueuePosition int `json:"queue_position,omitempty"`
}

type taskAttemptJSON struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	QueuePosition int    `json:"queue_position,omitempty"`
}

type startedTaskJSON struct {
	taskJSON
	AttemptID string `json:"attempt_id"`
}

func taskValue(t store.Task) taskJSON {
	v := taskJSON{ID: t.ID, Project: t.Project, CreatedBy: t.CreatedBy, Objective: t.Objective, Criteria: t.AcceptanceCriteria, State: t.State, Revision: t.Revision, Created: t.Created, Updated: t.Updated}
	if a := t.Attempt; a.ID != "" {
		v.Attempt = &taskAttemptJSON{ID: a.ID, State: a.State, QueuePosition: a.QueuePosition}
		// Keep the existing top-level field for /v1 clients.
		v.QueuePosition = a.QueuePosition
	}
	return v
}

type cachedResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func (h *handler) mutate(w http.ResponseWriter, r *http.Request, user, k string, scope *store.Scope, revision string, fn func(*store.Tx) (int, any, error)) bool {
	cached, err := h.commit(r, user, k, scope, revision, fn)
	if err != nil {
		storeError(w, err)
		return false
	}
	return writeCached(w, cached)
}

// commit is mutate's transaction, which returns the response to write.
func (h *handler) commit(r *http.Request, user, k string, scope *store.Scope, revision string, fn func(*store.Tx) (int, any, error)) ([]byte, error) {
	body := r.Context().Value(bodyKey{}).([]byte)
	return h.store.Do(r.Context(), user, k, hash(r, body, revision), scope, func(tx *store.Tx) ([]byte, error) {
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
}

func writeCached(w http.ResponseWriter, cached []byte) bool {
	var result cachedResponse
	if err := json.Unmarshal(cached, &result); err != nil {
		writeInternalError(w, err)
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(result.Status)
	_, _ = w.Write(append(result.Body, '\n'))
	return true
}

type bodyKey struct{}

// errNotReplayed rolls back replayed's probe: the key holds no response yet.
var errNotReplayed = errors.New("not a replay")

// replayed answers a request whose key already holds a response, after the
// membership check when scope is set, before the handler does slow work
// outside the transaction; it reports whether it answered. mutate still
// decides: a request that raced this one to the key gets its response there.
func (h *handler) replayed(w http.ResponseWriter, r *http.Request, user, k string, scope *store.Scope) bool {
	body := r.Context().Value(bodyKey{}).([]byte)
	cached, err := h.store.Do(r.Context(), user, k, hash(r, body, ""), scope, func(*store.Tx) ([]byte, error) { return nil, errNotReplayed })
	if errors.Is(err, errNotReplayed) {
		return false
	}
	if err != nil {
		storeError(w, err)
		return true
	}
	writeCached(w, cached)
	return true
}

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
	h.mutate(w, r, user, k, &store.Scope{ProjectID: input.Project}, "", func(tx *store.Tx) (int, any, error) {
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
	value := taskValue(task)
	w.Header().Set("ETag", fmt.Sprintf("\"%d\"", task.Revision))
	writeJSON(w, 200, value)
}

func matchRevision(value string) (int64, error) {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0, errors.New("invalid If-Match revision")
	}
	return strconv.ParseInt(value, 10, 64)
}

// transition accepts a bare or quoted decimal If-Match revision; * is refused.
func (h *handler) transition(from, to store.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.taskMutation(w, r, func(ctx context.Context, tx *store.Tx, id string, current store.State, revision int64) error {
			if current != from {
				return store.ErrInvalidTransition
			}
			return tx.Transition(ctx, id, from, to, revision)
		})
	}
}

// taskMutation is the shared shape of an empty-bodied task mutation at an
// If-Match revision: change runs in the idempotent transaction once the
// revision matched, and the response is the task as it then is. It reports
// whether change ran and committed, which a replay does not.
func (h *handler) taskMutation(w http.ResponseWriter, r *http.Request, change func(context.Context, *store.Tx, string, store.State, int64) error) (string, bool) {
	user, ok := h.actor(w, r)
	if !ok {
		return "", false
	}
	k, ok := key(w, r)
	if !ok {
		return "", false
	}
	match := r.Header.Get("If-Match")
	revision, err := matchRevision(match)
	if err != nil || match == "" || revision < 0 {
		writeError(w, 400, "bad_request")
		return "", false
	}
	body, ok := readBody(w, r)
	if !ok {
		return "", false
	}
	if len(body) != 0 {
		writeError(w, 400, "bad_request")
		return "", false
	}
	id := r.PathValue("id")
	project, err := h.store.TaskProject(r.Context(), user, id)
	if err != nil {
		storeError(w, err)
		return "", false
	}
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, body))
	changed := false
	ok = h.mutate(w, r, user, k, &store.Scope{ProjectID: project}, strconv.FormatInt(revision, 10), func(tx *store.Tx) (int, any, error) {
		var current store.State
		var currentRevision int64
		if err := tx.QueryRowContext(r.Context(), "SELECT state,revision FROM tasks WHERE id=?", id).Scan(&current, &currentRevision); err != nil {
			return 0, nil, err
		}
		if currentRevision != revision {
			return 0, nil, store.ErrStaleRevision
		}
		if err := change(r.Context(), tx, id, current, revision); err != nil {
			return 0, nil, err
		}
		changed = true
		row := tx.QueryRowContext(r.Context(), "SELECT id,project,created_by,objective,acceptance_criteria,state,revision,created,updated FROM tasks WHERE id=?", id)
		var task store.Task
		err := row.Scan(&task.ID, &task.Project, &task.CreatedBy, &task.Objective, &task.AcceptanceCriteria, &task.State, &task.Revision, &task.Created, &task.Updated)
		return 200, taskValue(task), err
	})
	return id, ok && changed
}

// cancel ends a running task through the executor's cancel path, and
// withdraws a queued one: its pending attempt and the task close as canceled
// in one transaction, with no process started. A cancel that finds the attempt
// already launching (the launch claim won the race on the attempt's row) is
// handed to the executor and follows the running path once the engine
// accepts, or ends failed or interrupted; the task is answered as it is.
func (h *handler) cancel(w http.ResponseWriter, r *http.Request) {
	notify := false
	id, changed := h.taskMutation(w, r, func(ctx context.Context, tx *store.Tx, id string, current store.State, revision int64) error {
		switch current {
		case store.Running:
			notify = true
			return tx.Transition(ctx, id, store.Running, store.CancelRequested, revision)
		case store.Preparing:
			_, err := tx.CancelQueued(ctx, id, revision, "withdrawn by "+tx.UserID())
			if errors.Is(err, store.ErrAttemptLaunched) {
				notify = true
				return nil
			}
			return err
		default:
			return store.ErrInvalidTransition
		}
	})
	if changed && notify && h.exec.Runner != nil {
		h.exec.Runner.CancelTask(id)
	}
}

type stopJSON struct {
	Running int    `json:"running"`
	Queued  int    `json:"queued"`
	Reason  string `json:"reason"`
}

// stop is "Quit Ada and stop work", for the coordinator's owner account alone:
// a project owner is a member like any other here. In one idempotent
// transaction every queued attempt is withdrawn and every running one gets
// the reason on record and its cancel requested; then the coordinator cancels
// the running attempts and exits. The counts say how much work the stop
// ended. Start admission is closed from before the transaction until the
// executor has been told, so no start commits in the window between the two,
// and none after. A replay, or a stop that commits nothing, tells the
// coordinator nothing: after a restart the key reaches a coordinator the
// cached counts were never about.
func (h *handler) stop(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	if h.exec.Owner == "" || user != h.exec.Owner {
		writeError(w, 403, "forbidden")
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
	if len(body) != 0 {
		writeError(w, 400, "bad_request")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, body))
	h.admission.Lock()
	defer h.admission.Unlock()
	wasStopping := h.stopping
	h.stopping = true
	var result store.StopResult
	stopped := false
	ok = h.mutate(w, r, user, k, nil, "", func(tx *store.Tx) (int, any, error) {
		if h.exec.Stopper == nil {
			return 0, nil, errEngineUnavailable
		}
		var generation int64
		if err := tx.QueryRowContext(r.Context(), "SELECT generation FROM coordinator WHERE id=1").Scan(&generation); err != nil {
			return 0, nil, err
		}
		if generation != h.exec.Generation {
			return 0, nil, store.ErrStaleOwnership
		}
		var err error
		result, err = tx.StopWork(r.Context(), h.exec.Generation, stopReason)
		if err != nil {
			return 0, nil, err
		}
		stopped = true
		return 202, stopJSON{result.Running, result.Queued, stopReason}, nil
	})
	if !ok || !stopped {
		h.stopping = wasStopping
		return
	}
	for _, id := range result.Attempts {
		h.hub.Notify(id)
	}
	if h.beforeStopHandoff != nil {
		h.beforeStopHandoff()
	}
	h.exec.Stopper.StopWork()
}

func (h *handler) start(w http.ResponseWriter, r *http.Request) {
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
		Backend string `json:"backend"`
		Model   string `json:"model"`
		Effort  string `json:"effort"`
		Mode    string `json:"mode"`
	}
	if err := decode(body, &input); err != nil || !validBackend(input.Backend) || !validModel(input.Model) || !printableASCII(input.Effort) || (input.Mode != "readonly" && input.Mode != "auto" && input.Mode != "yolo") {
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
	// Held through the transaction and the scheduler handoff: a stop waits
	// for both, and a start that finds the stop in or past its transaction
	// is refused rather than recorded for an executor that will not run it.
	h.admission.RLock()
	defer h.admission.RUnlock()
	var attemptID string
	ok = h.mutate(w, r, user, k, &store.Scope{ProjectID: project}, strconv.FormatInt(revision, 10), func(tx *store.Tx) (int, any, error) {
		if h.stopping {
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
		var current store.State
		var currentRevision int64
		if err := tx.QueryRowContext(r.Context(), "SELECT state,revision FROM tasks WHERE id=?", id).Scan(&current, &currentRevision); err != nil {
			return 0, nil, err
		}
		if currentRevision != revision {
			return 0, nil, store.ErrStaleRevision
		}
		if current != store.Ready {
			return 0, nil, store.ErrInvalidTransition
		}
		// The start queues when another attempt is launching or running: the
		// executor launches pending attempts one at a time in creation order.
		// The snapshot is revalidated at launch, not here; there is no
		// snapshot before M3, so that hook lives with the launch.
		queued, err := tx.QueuedAttempts(r.Context(), user)
		if err != nil {
			return 0, nil, err
		}
		if queued >= maxQueuedPerUser {
			return 0, nil, errQueueFull
		}
		if err := tx.Transition(r.Context(), id, store.Ready, store.Preparing, revision); err != nil {
			return 0, nil, err
		}
		attemptID, err = newID()
		if err != nil {
			return 0, nil, err
		}
		_, err = tx.ExecContext(r.Context(), "INSERT INTO attempts(id,task,actor,backend,model,effort,mode,executor,ownership_generation,engine_sha256,state,started) VALUES(?,?,?,?,?,?,?,?,?,?,'pending',?)", attemptID, id, user, input.Backend, input.Model, input.Effort, input.Mode, "local", h.exec.Generation, h.exec.EngineSHA256, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return 0, nil, err
		}
		row := tx.QueryRowContext(r.Context(), "SELECT id,project,created_by,objective,acceptance_criteria,state,revision,created,updated FROM tasks WHERE id=?", id)
		var task store.Task
		err = row.Scan(&task.ID, &task.Project, &task.CreatedBy, &task.Objective, &task.AcceptanceCriteria, &task.State, &task.Revision, &task.Created, &task.Updated)
		return 200, startedTaskJSON{taskValue(task), attemptID}, err
	})
	if ok && attemptID != "" && h.exec.Runner != nil {
		h.exec.Runner.LaunchAttempt(attemptID)
	}
}

func validBackend(value string) bool {
	if len(value) == 0 || len(value) > 32 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		c := value[i]
		if c < 'a' || c > 'z' {
			if c < '0' || c > '9' {
				if c != '_' && c != '-' {
					return false
				}
			}
		}
	}
	return true
}

// validModel accepts an empty model or one that is safe as an argument on the
// way to the backend CLI: ^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$. Harness
// passes the model into the CLI's argv, and on Windows that CLI is an npm
// .cmd shim run through cmd.exe, whose quoting leaves `&`, `|`, `^`, `%VAR%`
// and `"` live; the leading alphanumeric also rules out an option.
func validModel(value string) bool {
	if len(value) > 128 || (len(value) != 0 && !alphanumeric(value[0])) {
		return false
	}
	for i := 1; i < len(value); i++ {
		switch c := value[i]; {
		case alphanumeric(c), c == '.', c == '_', c == ':', c == '/', c == '@', c == '+', c == '-':
		default:
			return false
		}
	}
	return true
}

func alphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func printableASCII(value string) bool {
	if len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
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

const pageBytes = 1 << 20
const eventBytes = 256 << 10

func streamEvent(e store.Event) []byte {
	data, _ := json.Marshal(e.Payload)
	kind := strings.Map(func(c rune) rune {
		if c == '\r' || c == '\n' {
			return -1
		}
		return c
	}, e.Kind)
	return []byte(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", e.Seq, kind, data))
}

func jsonEvent(e store.Event) []byte {
	b, _ := json.Marshal(eventValue(e))
	return b
}

// EventExceedsLimit reports whether the encoding of an event, as a JSON page
// entry or as a stream frame, is over the size a page refuses to serve. The
// executor stores the payload of such an event as an artifact instead
// (ReferencedPayload); both sides use this one check so the two cannot drift.
func EventExceedsLimit(e store.Event) bool {
	return len(jsonEvent(e)) > eventBytes || len(streamEvent(e)) > eventBytes
}

func (h *handler) page(ctx context.Context, user, id string, after int64, stream bool) ([]store.Event, []byte, error) {
	encode := jsonEvent
	overhead := 1 // commas between JSON events
	if stream {
		encode, overhead = streamEvent, 0
	}
	size := func(e store.Event) int { return len(encode(e)) + overhead }
	read := h.readPage
	if read == nil {
		read = h.store.EventsPage
	}
	limit := pageBytes
	if !stream {
		limit -= 3
	} // brackets and newline
	events, err := read(ctx, user, id, after, 256, limit, size)
	if err != nil {
		return nil, nil, err
	}
	encoded := make([][]byte, len(events))
	total := 0
	if !stream {
		total = 3 // brackets and newline
		if len(events) > 1 {
			total += len(events) - 1 // commas
		}
	}
	for i, e := range events {
		encoded[i] = encode(e)
		if EventExceedsLimit(e) {
			return nil, nil, errors.New("stored event exceeds encoded limit")
		}
		total += len(encoded[i])
	}
	var page []byte
	if total != 0 {
		page = make([]byte, 0, total)
	}
	if !stream {
		page = append(page, '[')
	}
	for i, event := range encoded {
		if !stream && i != 0 {
			page = append(page, ',')
		}
		page = append(page, event...)
	}
	if !stream {
		page = append(page, ']', '\n')
	}
	return events, page, nil
}

func (h *handler) events(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	cursor := r.URL.Query().Get("after")
	if values := r.Header.Values("Last-Event-Id"); len(values) > 1 {
		writeError(w, 400, "bad_request")
		return
	} else if len(values) == 1 {
		if strings.Contains(values[0], ",") {
			writeError(w, 400, "bad_request")
			return
		}
		if values[0] != "" {
			cursor = values[0]
		}
	}
	var after int64
	if cursor != "" {
		var err error
		after, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || after < 0 || strings.Trim(cursor, "0123456789") != "" {
			writeError(w, 400, "bad_request")
			return
		}
	}
	// A non-member or an unknown attempt is answered before any slot or budget
	// is held; the reads below check membership again under the reservation.
	if _, err := h.store.AttemptProject(r.Context(), user, id); err != nil {
		storeError(w, err)
		return
	}
	stream := strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	if stream {
		release, ok := h.hub.reserveStream(user)
		if !ok {
			writeError(w, 429, "too_many_streams")
			return
		}
		defer release()
	}
	if !h.hub.reserveBytes(r.Context(), pageBytes) {
		if r.Context().Err() == nil {
			w.Header().Set("Retry-After", "2")
			writeError(w, 503, "shutting_down")
		}
		return
	}
	held := pageBytes
	defer func() {
		if held != 0 {
			h.hub.releaseBytes(held)
		}
	}()
	readLast := h.readLast
	if readLast == nil {
		readLast = h.store.LastEventSequence
	}
	last, err := readLast(r.Context(), user, id)
	if err != nil {
		storeError(w, err)
		return
	}
	if after > last {
		if !stream {
			writeJSON(w, 409, map[string]any{"error": "cursor_ahead", "message": "cursor ahead", "last_sequence": last})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		rc := http.NewResponseController(w)
		deadline := h.hub.WriteDeadline
		if deadline <= 0 {
			deadline = 10 * time.Second
		}
		if err := rc.SetWriteDeadline(time.Now().Add(deadline)); err != nil {
			return
		}
		w.WriteHeader(200)
		fmt.Fprintf(w, "retry: 2000\n\nevent: cursor_ahead\ndata: {\"last_sequence\":%d}\n\n", last)
		_ = rc.Flush()
		_ = rc.SetWriteDeadline(time.Time{})
		return
	}
	if !stream {
		_, page, err := h.page(r.Context(), user, id, after, false)
		if err != nil {
			storeError(w, err)
			return
		}
		h.hub.releaseBytes(held - len(page))
		held = len(page)
		w.Header().Set("Content-Type", "application/json")
		rc := http.NewResponseController(w)
		deadline := h.hub.WriteDeadline
		if deadline <= 0 {
			deadline = 10 * time.Second
		}
		_ = rc.SetWriteDeadline(time.Now().Add(deadline))
		w.WriteHeader(200)
		_, _ = w.Write(page)
		_ = rc.SetWriteDeadline(time.Time{})
		return
	}
	h.hub.releaseBytes(held)
	held = 0
	wake, unsubscribe := h.hub.subscribe(id)
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	rc := http.NewResponseController(w)
	deadline := h.hub.WriteDeadline
	if deadline <= 0 {
		deadline = 10 * time.Second
	}
	write := func(page []byte) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(deadline)); err != nil {
			return false
		}
		n, err := w.Write(page)
		if err == nil && n != len(page) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = rc.Flush()
		}
		_ = rc.SetWriteDeadline(time.Time{})
		return err == nil
	}
	if !write([]byte("retry: 2000\n\n")) {
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
		for {
			count, lastSeq, pageLen, ok := func() (int, int64, int, bool) {
				reserveCtx, cancel := context.WithTimeout(r.Context(), deadline)
				got := h.hub.reserveBytes(reserveCtx, pageBytes)
				cancel()
				if !got {
					return 0, after, 0, false
				}
				pageHeld := pageBytes
				defer func() { h.hub.releaseBytes(pageHeld) }()
				events, page, err := h.page(r.Context(), user, id, after, true)
				if err != nil {
					return 0, after, 0, false
				}
				h.hub.releaseBytes(pageHeld - len(page))
				pageHeld = len(page)
				count := len(events)
				lastSeq := after
				if count != 0 {
					lastSeq = events[count-1].Seq
				}
				if len(page) != 0 && !write(page) {
					return count, lastSeq, len(page), false
				}
				return count, lastSeq, len(page), true
			}()
			if !ok {
				return false
			}
			if count == 0 {
				return true
			}
			after = lastSeq
			if count < 256 && pageLen < pageBytes-eventBytes {
				return true
			}
		}
	}
	if !send() {
		return
	}
	for {
		// A drain seen before the send ends the stream after it: that send
		// reads everything committed before the drain began.
		final := false
		select {
		case <-r.Context().Done():
			return
		case <-h.hub.done:
			return
		case <-h.hub.draining:
			final = true
		case <-wake:
		case <-pollTicker.C:
		case <-pingTicker.C:
			if !write([]byte(": ping\n\n")) {
				return
			}
			continue
		}
		if !send() || final {
			return
		}
	}
}

// ReferencedPayload is the payload of an event whose own payload was stored as
// an artifact because its encoded event would exceed eventBytes. The marker
// payload_ref tells a client the payload is a reference and not the engine's
// own object; the bytes are served by GET /v1/attempts/{id}/artifacts/{hash}.
func ReferencedPayload(hash string, size int64) json.RawMessage {
	payload, _ := json.Marshal(struct {
		Ref  string `json:"payload_ref"`
		Hash string `json:"payload_artifact"`
		Size int64  `json:"payload_size"`
	}{"artifact", hash, size})
	return payload
}

// artifact serves the bytes of an artifact a record of the attempt references
// to a current member of the attempt's project, checked on this read.
func (h *handler) artifact(w http.ResponseWriter, r *http.Request) {
	user, ok := h.actor(w, r)
	if !ok {
		return
	}
	id, hash := r.PathValue("id"), r.PathValue("hash")
	if _, err := h.store.AttemptProject(r.Context(), user, id); err != nil {
		storeError(w, err)
		return
	}
	if h.exec.Artifacts == nil || h.exec.References == nil {
		writeError(w, 503, "engine_unavailable")
		return
	}
	size, err := h.exec.References.ArtifactReference(r.Context(), id, hash)
	if err != nil {
		storeError(w, err)
		return
	}
	blob, err := h.exec.Artifacts.Open(hash)
	if err != nil {
		writeInternalError(w, fmt.Errorf("artifact %s: %w", hash, err))
		return
	}
	defer blob.Close()
	// The reader verifies the blob only at EOF, so the body is sent one chunk
	// behind the read: a chunk goes out once the next read succeeded, the last
	// one only after a verified EOF with the byte count matching the reference.
	// A failure before the first write is a 500; after it, the body stays short
	// of Content-Length, so a client never holds a corrupt artifact as whole.
	rc := http.NewResponseController(w)
	deadline := h.hub.WriteDeadline
	if deadline <= 0 {
		deadline = 10 * time.Second
	}
	committed := false
	var sent int64
	write := func(chunk []byte) bool {
		if !committed {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.Header().Set("ETag", "\""+hash+"\"")
			w.Header().Set("Cache-Control", "private, no-cache")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(200)
			committed = true
		}
		if len(chunk) == 0 {
			return true
		}
		_ = rc.SetWriteDeadline(time.Now().Add(deadline))
		_, err := w.Write(chunk)
		sent += int64(len(chunk))
		return err == nil
	}
	fail := func(err error) {
		err = fmt.Errorf("artifact %s: %w", hash, err)
		if !committed {
			writeInternalError(w, err)
			return
		}
		requestID := "unknown"
		if rw, ok := w.(*requestWriter); ok {
			requestID = rw.id
		}
		fmt.Fprintf(os.Stderr, "adad: request %s: %v\n", requestID, err)
	}
	bufs := [2][]byte{make([]byte, 32<<10), make([]byte, 32<<10)}
	var held []byte // the last chunk read, in the buffer the next read leaves alone
	free, emptyReads := 0, 0
	for {
		n, readErr := blob.Read(bufs[free])
		chunk := bufs[free][:n]
		atEOF := errors.Is(readErr, io.EOF)
		if readErr != nil && !atEOF {
			fail(readErr)
			return
		}
		read := sent + int64(len(held)) + int64(n)
		if read > size {
			fail(fmt.Errorf("blob holds more than the %d bytes referenced", size))
			return
		}
		if atEOF {
			if read != size {
				fail(fmt.Errorf("blob holds %d bytes, %d referenced", read, size))
				return
			}
			if write(held) {
				write(chunk)
			}
			return
		}
		if n == 0 {
			if emptyReads++; emptyReads >= 100 {
				fail(io.ErrNoProgress)
				return
			}
			continue
		}
		emptyReads = 0
		if len(held) != 0 && !write(held) {
			return
		}
		held = chunk
		free ^= 1
	}
}

// validRepoPath is an absolute local path. A UNC or device path is refused
// before anything touches it: opening one sends credentials over SMB.
func validRepoPath(p string) bool {
	return !source.IsRemotePath(p) && filepath.IsAbs(p) && !strings.ContainsRune(p, 0)
}

func (h *handler) gitUnavailable(w http.ResponseWriter) {
	reason := h.exec.GitUnavailable
	if reason == "" {
		reason = "git unavailable"
	}
	writeJSON(w, 503, struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{"git_unavailable", reason})
}

// inspect stores nothing, so the idempotency rules of mutating requests do not apply.
func (h *handler) inspect(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.actor(w, r); !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Path string `json:"path"`
	}
	if err := decode(body, &input); err != nil || !validRepoPath(input.Path) {
		writeError(w, 400, "bad_request")
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
	writeJSON(w, 200, result)
}
