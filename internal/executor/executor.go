// Package executor supervises one pinned Harness exec process per attempt.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/engine"
	"github.com/cristiangirlea/ada-os/internal/store"
)

type ArtifactWriter interface {
	Write(context.Context, io.Reader, int64) (artifact.Ref, error)
}
type Notifier interface{ Notify(string) }

type Config struct {
	EnginePath, EngineSHA256, StateDir string
	EngineArgs                         []string // nil uses hrn exec --protocol v1; tests supply the helper-process argument.
	Artifacts                          ArtifactWriter
	Store                              *store.Store
	Hub                                Notifier
	Generation                         int64
	CancelGrace                        time.Duration
	Env                                []string // Extra controlled environment entries for helper-process tests.
}

type active struct {
	task   string
	cancel chan struct{}
	once   sync.Once
}
type Executor struct {
	cfg     Config
	mu      sync.Mutex
	active  map[string]*active
	stopped bool
	wg      sync.WaitGroup
}

func New(cfg Config) *Executor {
	if cfg.CancelGrace <= 0 {
		cfg.CancelGrace = 10 * time.Second
	}
	return &Executor{cfg: cfg, active: make(map[string]*active)}
}

func (e *Executor) LaunchAttempt(id string) {
	a, err := e.cfg.Store.AttemptByID(context.Background(), id)
	if err != nil || a.State != "pending" || a.OwnershipGeneration != e.cfg.Generation {
		return
	}
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	if _, exists := e.active[id]; exists {
		e.mu.Unlock()
		return
	}
	run := &active{task: a.Task, cancel: make(chan struct{})}
	e.active[id] = run
	e.wg.Add(1)
	e.mu.Unlock()
	go func() {
		defer e.wg.Done()
		defer func() { e.mu.Lock(); delete(e.active, id); e.mu.Unlock() }()
		e.run(a, run)
	}()
}

func (e *Executor) CancelTask(taskID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range e.active {
		if a.task == taskID {
			a.once.Do(func() { close(a.cancel) })
		}
	}
}

func (e *Executor) Stop(ctx context.Context) error {
	e.mu.Lock()
	e.stopped = true
	for _, a := range e.active {
		a.once.Do(func() { close(a.cancel) })
	}
	e.mu.Unlock()
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type message struct {
	V         int    `json:"v"`
	Seq       int64  `json:"seq"`
	Type      string `json:"type"`
	Protocol  string `json:"protocol"`
	AttemptID string `json:"attempt_id"`
	RunID     string `json:"run_id"`
	Backends  []struct {
		Name      string   `json:"name"`
		Available bool     `json:"available"`
		Modes     []string `json:"modes"`
		Options   []string `json:"options"`
		Approvals *bool    `json:"approvals"`
	} `json:"backends"`
	Event struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	} `json:"event"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Fatal  bool `json:"fatal"`
	Result struct {
		Status string `json:"status"`
		Final  string `json:"final"`
	} `json:"result"`
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func (e *Executor) run(a store.Attempt, active *active) {
	ctx := context.Background()
	base := filepath.Join(e.cfg.StateDir, "attempts", a.ID)
	work := filepath.Join(base, "work")
	enginePath, err := filepath.Abs(e.cfg.EnginePath)
	if err != nil {
		e.fail(a, fmt.Errorf("engine path: %w", err))
		return
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(enginePath), ".exe") {
		e.fail(a, errors.New("engine path requires a .exe suffix on Windows"))
		return
	}
	if err := e.owned(a.ID, func(tx *store.Tx) error {
		result, err := tx.ExecContext(ctx, "UPDATE attempts SET state='launching' WHERE id=? AND state='pending'", a.ID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return store.ErrStaleOwnership
		}
		return nil
	}); err != nil {
		return
	}
	if err := os.MkdirAll(work, 0700); err != nil {
		e.fail(a, fmt.Errorf("scratch directory: %w", err))
		return
	}
	if err := os.Chmod(work, 0700); err != nil {
		e.fail(a, err)
		return
	}
	stderr, err := os.OpenFile(filepath.Join(base, "engine.stderr"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		e.fail(a, fmt.Errorf("stderr: %w", err))
		return
	}
	defer stderr.Close()
	prompt, err := e.cfg.Store.AttemptPrompt(ctx, a.ID)
	if err != nil {
		e.fail(a, fmt.Errorf("task prompt: %w", err))
		return
	}
	args := e.cfg.EngineArgs
	if args == nil {
		args = []string{"exec", "--protocol", "v1"}
	}
	cmd := exec.Command(enginePath, args...)
	cmd.Path = enginePath
	cmd.WaitDelay = e.cfg.CancelGrace
	cmd.Dir = work
	cmd.Env = e.environment(base)
	cmd.Stderr = &capWriter{file: stderr, left: 1 << 20}
	e.mu.Lock()
	stopped := e.stopped
	e.mu.Unlock()
	if stopped {
		e.fail(a, errors.New("coordinator stopped before launch"))
		return
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		e.fail(a, err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		e.fail(a, err)
		return
	}
	if err := engine.Verify(enginePath, e.cfg.EngineSHA256); err != nil {
		stdin.Close()
		stdout.Close()
		e.fail(a, fmt.Errorf("engine verification: %w", err))
		return
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		e.fail(a, fmt.Errorf("launch: %w", err))
		return
	}
	var inputMu sync.Mutex
	write := func(v any) error { inputMu.Lock(); defer inputMu.Unlock(); return json.NewEncoder(stdin).Encode(v) }
	var processMu sync.Mutex
	var exited, killRequested bool
	var exitState *os.ProcessState
	var exitErr error
	processDone := make(chan struct{})
	go func() {
		state, err := cmd.Process.Wait()
		processMu.Lock()
		exitState, exitErr, exited = state, err, true
		processMu.Unlock()
		close(processDone)
	}()
	kill := func() {
		processMu.Lock()
		if !exited && cmd.Process.Kill() == nil {
			killRequested = true
		}
		processMu.Unlock()
		// A descendant can retain stdout after the engine exits.
		_ = stdout.Close()
	}
	var timerMu sync.Mutex
	var closeTimer, killTimer *time.Timer
	shutdownStarted := false
	startShutdown := func() {
		timerMu.Lock()
		defer timerMu.Unlock()
		if shutdownStarted {
			return
		}
		shutdownStarted = true
		closeTimer = time.AfterFunc(e.cfg.CancelGrace, func() { _ = stdin.Close() })
		killTimer = time.AfterFunc(2*e.cfg.CancelGrace, kill)
	}
	stopShutdown := func() {
		timerMu.Lock()
		defer timerMu.Unlock()
		if closeTimer != nil {
			closeTimer.Stop()
		}
		if killTimer != nil {
			killTimer.Stop()
		}
	}
	// The reader calls cmd.Wait only after its final read has returned.
	waitExit := func() (bool, error) {
		<-processDone
		_ = cmd.Wait() // Process.Wait above supplies the exit status.
		stopShutdown()
		processMu.Lock()
		defer processMu.Unlock()
		if exitErr != nil {
			return killRequested, exitErr
		}
		if exitState == nil {
			return killRequested, errors.New("engine exit state missing")
		}
		if !exitState.Success() {
			return killRequested, fmt.Errorf("exit status %d", exitState.ExitCode())
		}
		return killRequested, nil
	}
	done := make(chan struct{})
	cancelDone := make(chan struct{})
	accepted := make(chan struct{})
	defer func() {
		close(done)
		<-cancelDone
		stopShutdown()
		stdin.Close()
	}()
	go func() {
		defer close(cancelDone)
		select {
		case <-active.cancel:
			select {
			case <-accepted:
				_ = e.requestCancel(a)
				_ = write(map[string]any{"type": "cancel", "attempt_id": a.ID})
			default:
			}
			startShutdown()
			select {
			case <-accepted:
				_ = e.requestCancel(a)
			case <-done:
			}
		case <-done:
		}
	}()
	reader := bufio.NewReaderSize(stdout, 64*1024)
	read := func() (message, []byte, error) {
		line, err := reader.ReadBytes('\n')
		if len(line) > 4<<20 {
			return message{}, nil, errors.New("protocol line exceeds 4 MiB")
		}
		if errors.Is(err, io.EOF) && len(line) != 0 {
			return message{}, nil, errors.New("truncated protocol line")
		}
		if err != nil {
			return message{}, nil, err
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			return m, line, fmt.Errorf("malformed protocol line: %w", err)
		}
		return m, line, nil
	}
	seq := int64(0)
	next := func(want string, matchID bool) (message, []byte, error) {
		m, line, err := read()
		if err != nil {
			return m, line, err
		}
		if m.V != 1 || m.Seq != seq+1 {
			return m, line, fmt.Errorf("protocol seq: got %d, want %d", m.Seq, seq+1)
		}
		seq = m.Seq
		if want != "" && m.Type != want {
			return m, line, fmt.Errorf("protocol type: got %s, want %s", m.Type, want)
		}
		if matchID && m.AttemptID != a.ID {
			return m, line, fmt.Errorf("protocol attempt_id mismatch")
		}
		return m, line, nil
	}
	drain := func() (error, bool, error) {
		startShutdown()
		for {
			_, _, err := read()
			if err != nil {
				killed, waitErr := waitExit()
				return err, killed, waitErr
			}
		}
	}
	abort := func(cause error) {
		e.recordCause(a, cause)
		_, killed, _ := drain()
		e.failFinal(a, cause, true, killed)
	}
	hello, _, err := next("hello", false)
	if err != nil {
		abort(fmt.Errorf("hello: %w", err))
		return
	}
	if hello.Protocol != "v1" {
		abort(fmt.Errorf("hello protocol %q", hello.Protocol))
		return
	}
	var options []string
	found := false
	for _, backend := range hello.Backends {
		if backend.Name == a.Backend && backend.Available && contains(backend.Modes, a.Mode) && contains(backend.Options, "mode") && backend.Approvals != nil && !*backend.Approvals {
			found = true
			options = backend.Options
			break
		}
	}
	if !found {
		abort(fmt.Errorf("backend %s mode %s unavailable", a.Backend, a.Mode))
		return
	}
	task := map[string]any{"prompt": prompt, "dir": work, "backend": a.Backend, "mode": a.Mode}
	if a.Model != "" {
		if !contains(options, "model") {
			abort(errors.New("model unsupported"))
			return
		}
		task["model"] = a.Model
	}
	if a.Effort != "" {
		if !contains(options, "effort") {
			abort(errors.New("effort unsupported"))
			return
		}
		task["effort"] = a.Effort
	}
	if err := write(map[string]any{"type": "start", "attempt_id": a.ID, "task": task}); err != nil {
		abort(fmt.Errorf("start write: %w", err))
		return
	}
	answer, _, err := next("", true)
	if err != nil {
		abort(fmt.Errorf("start response: %w", err))
		return
	}
	if answer.Type == "rejected" {
		abort(fmt.Errorf("rejected: %s", answer.Error.Code))
		return
	}
	if answer.Type != "accepted" || answer.RunID == "" {
		abort(errors.New("invalid accepted response"))
		return
	}
	if err := e.owned(a.ID, func(tx *store.Tx) error {
		var state store.State
		var rev int64
		if err := tx.QueryRowContext(ctx, "SELECT state,revision FROM tasks WHERE id=?", a.Task).Scan(&state, &rev); err != nil {
			return err
		}
		if err := tx.TransitionOwned(ctx, a.Task, store.Preparing, store.Running, rev); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE attempts SET state='running',run_id=? WHERE id=?", answer.RunID, a.ID)
		return err
	}); err != nil {
		abort(err)
		return
	}
	close(accepted)
	var fatalCause error
	for {
		m, line, err := next("", true)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if fatalCause == nil {
					e.recordCause(a, errors.New("engine exited without result"))
				}
				startShutdown()
				killed, waitErr := waitExit()
				cause := fmt.Errorf("engine exited without result: %v", waitErr)
				if fatalCause != nil {
					cause = fmt.Errorf("%w: %v", fatalCause, cause)
				}
				e.failFinal(a, cause, fatalCause != nil, killed)
				return
			}
			abort(fmt.Errorf("protocol stream: %w", err))
			return
		}
		switch m.Type {
		case "event":
			if m.Event.Kind == "" {
				abort(errors.New("event missing kind"))
				return
			}
			if err := e.append(a, m.Event.Kind, line, m.Event.Text); err != nil {
				abort(fmt.Errorf("event storage: %w", err))
				return
			}
		case "error":
			if err := e.append(a, "error", line, ""); err != nil {
				abort(fmt.Errorf("error storage: %w", err))
				return
			}
			if m.Fatal {
				fatalCause = fmt.Errorf("fatal error %s: %s", m.Error.Code, m.Error.Message)
				startShutdown()
			}
		case "result":
			if err := e.result(a, m, line); err != nil {
				abort(err)
				return
			}
			startShutdown()
			_, _, tailErr := read()
			if tailErr == nil {
				abort(errors.New("line after result"))
				return
			}
			if !errors.Is(tailErr, io.EOF) {
				cause := fmt.Errorf("result tail: %w", tailErr)
				if !errors.Is(tailErr, os.ErrClosed) {
					e.recordCause(a, cause)
				}
				killed, _ := waitExit()
				if killed {
					if fatalCause != nil {
						e.failFinal(a, fatalCause, true, true)
					} else {
						e.failFinal(a, cause, !errors.Is(tailErr, os.ErrClosed), true)
					}
				} else {
					e.failFinal(a, cause, !errors.Is(tailErr, os.ErrClosed), false)
				}
				return
			}
			killed, waitErr := waitExit()
			if killed {
				cause := errors.New("engine was killed after the grace period")
				e.failFinal(a, cause, false, false)
				return
			}
			if fatalCause != nil {
				e.failFinal(a, fatalCause, true, false)
				return
			}
			if waitErr != nil && m.Result.Status != "canceled" {
				e.fail(a, fmt.Errorf("engine exit: %w", waitErr))
				return
			}
			if err := e.finish(a, m.Result.Status); err != nil {
				e.fail(a, fmt.Errorf("finish storage: %w", err))
			}
			return
		default:
			abort(fmt.Errorf("unexpected protocol type %s", m.Type))
			return
		}
	}
}

func (e *Executor) environment(base string) []string {
	var env []string
	for _, key := range []string{"PATH", "SystemRoot", "WINDIR", "ComSpec", "PATHEXT"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	env = append(env, "HRN_HOME="+filepath.Join(base, "hrn"))
	return append(env, e.cfg.Env...)
}

type capWriter struct {
	file *os.File
	left int64
}

func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.left > 0 {
		limited := p
		if int64(len(limited)) > w.left {
			limited = limited[:w.left]
		}
		written, err := w.file.Write(limited)
		w.left -= int64(written)
		if err != nil {
			return written, err
		}
	}
	return n, nil
}

func (e *Executor) owned(id string, fn func(*store.Tx) error) error {
	return e.cfg.Store.DoOwned(context.Background(), id, e.cfg.Generation, fn)
}

func (e *Executor) append(a store.Attempt, kind string, payload []byte, session string) error {
	err := e.owned(a.ID, func(tx *store.Tx) error {
		var seq int64
		if err := tx.QueryRowContext(context.Background(), "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE attempt=?", a.ID).Scan(&seq); err != nil {
			return err
		}
		if err := tx.AppendOwnedEvent(context.Background(), store.Event{Attempt: a.ID, Seq: seq, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: kind, Payload: payload}); err != nil {
			return err
		}
		if kind == "session" {
			_, err := tx.ExecContext(context.Background(), "UPDATE attempts SET native_session=? WHERE id=?", session, a.ID)
			return err
		}
		return nil
	})
	if err == nil && e.cfg.Hub != nil {
		e.cfg.Hub.Notify(a.ID)
	}
	return err
}

func (e *Executor) result(a store.Attempt, m message, line []byte) error {
	var ref artifact.Ref
	if len(m.Result.Final) > 64<<10 {
		if e.cfg.Artifacts == nil {
			return errors.New("artifact store unavailable")
		}
		var err error
		ref, err = e.cfg.Artifacts.Write(context.Background(), strings.NewReader(m.Result.Final), int64(len(m.Result.Final)))
		if err != nil {
			return fmt.Errorf("final artifact: %w", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(line, &payload); err != nil {
			return err
		}
		result, ok := payload["result"].(map[string]any)
		if !ok {
			return errors.New("result object is missing")
		}
		delete(result, "final")
		result["final_artifact"], result["final_size"] = ref.Hash, ref.Size
		line, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	err := e.owned(a.ID, func(tx *store.Tx) error {
		if ref.Hash != "" {
			if err := tx.ReferenceArtifact(context.Background(), ref.Hash, ref.Size, "attempt:"+a.ID+":final"); err != nil {
				return err
			}
		}
		var seq int64
		if err := tx.QueryRowContext(context.Background(), "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE attempt=?", a.ID).Scan(&seq); err != nil {
			return err
		}
		return tx.AppendOwnedEvent(context.Background(), store.Event{Attempt: a.ID, Seq: seq, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "result", Payload: line})
	})
	if err == nil && e.cfg.Hub != nil {
		e.cfg.Hub.Notify(a.ID)
	}
	return err
}

func (e *Executor) requestCancel(a store.Attempt) error {
	return e.owned(a.ID, func(tx *store.Tx) error {
		var state store.State
		var rev int64
		if err := tx.QueryRowContext(context.Background(), "SELECT state,revision FROM tasks WHERE id=?", a.Task).Scan(&state, &rev); err != nil {
			return err
		}
		if state == store.Running {
			return tx.TransitionOwned(context.Background(), a.Task, store.Running, store.CancelRequested, rev)
		}
		return nil
	})
}

func (e *Executor) finish(a store.Attempt, status string) error {
	return e.owned(a.ID, func(tx *store.Tx) error {
		var state store.State
		var rev int64
		if err := tx.QueryRowContext(context.Background(), "SELECT state,revision FROM tasks WHERE id=?", a.Task).Scan(&state, &rev); err != nil {
			return err
		}
		var to store.State
		attemptState := "ended_failed"
		if state == store.CancelRequested {
			to = store.Canceled
			attemptState = "canceled"
		} else if status == "ok" {
			to = store.Verifying
			attemptState = "ended_ok"
		} else {
			to = store.Failed
		}
		if err := tx.TransitionOwned(context.Background(), a.Task, state, to, rev); err != nil {
			return err
		}
		_, err := tx.ExecContext(context.Background(), "UPDATE attempts SET state=?,ended=? WHERE id=?", attemptState, time.Now().UTC().Format(time.RFC3339Nano), a.ID)
		return err
	})
}

func (e *Executor) fail(a store.Attempt, cause error) {
	e.failFinal(a, cause, false, false)
}

func (e *Executor) recordCause(a store.Attempt, cause error) {
	payload, _ := json.Marshal(map[string]any{"error": cause.Error()})
	_ = e.append(a, "error", payload, "")
}

func (e *Executor) failFinal(a store.Attempt, cause error, causeRecorded, killed bool) {
	if errors.Is(cause, store.ErrStaleOwnership) {
		return
	}
	payload, _ := json.Marshal(map[string]any{"error": cause.Error()})
	_ = e.owned(a.ID, func(tx *store.Tx) error {
		var state store.State
		var rev int64
		if err := tx.QueryRowContext(context.Background(), "SELECT state,revision FROM tasks WHERE id=?", a.Task).Scan(&state, &rev); err != nil {
			return err
		}
		e.mu.Lock()
		stopped := e.stopped
		e.mu.Unlock()
		attemptState := "ended_failed"
		if state == store.Preparing && stopped {
			if err := tx.TransitionOwned(context.Background(), a.Task, store.Preparing, store.Interrupted, rev); err != nil {
				return err
			}
			if err := tx.TransitionOwned(context.Background(), a.Task, store.Interrupted, store.RecoveryRequired, rev+1); err != nil {
				return err
			}
			attemptState = "recovery_required"
		} else if state == store.Preparing || state == store.Running {
			if err := tx.TransitionOwned(context.Background(), a.Task, state, store.Failed, rev); err != nil {
				return err
			}
		}
		if state == store.CancelRequested {
			if err := tx.TransitionOwned(context.Background(), a.Task, state, store.Interrupted, rev); err != nil {
				return err
			}
			if err := tx.TransitionOwned(context.Background(), a.Task, store.Interrupted, store.RecoveryRequired, rev+1); err != nil {
				return err
			}
			attemptState = "recovery_required"
		}
		if _, err := tx.ExecContext(context.Background(), "UPDATE attempts SET state=?,ended=? WHERE id=?", attemptState, time.Now().UTC().Format(time.RFC3339Nano), a.ID); err != nil {
			return err
		}
		var seq int64
		if err := tx.QueryRowContext(context.Background(), "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE attempt=?", a.ID).Scan(&seq); err != nil {
			return err
		}
		if attemptState == "recovery_required" {
			if err := tx.AppendOwnedEvent(context.Background(), store.Event{Attempt: a.ID, Seq: seq, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "recovery", Payload: []byte(`{"reason":"engine exit not confirmed"}`)}); err != nil {
				return err
			}
			seq++
		}
		if !causeRecorded {
			if err := tx.AppendOwnedEvent(context.Background(), store.Event{Attempt: a.ID, Seq: seq, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "error", Payload: payload}); err != nil {
				return err
			}
			seq++
		}
		if killed {
			return tx.AppendOwnedEvent(context.Background(), store.Event{Attempt: a.ID, Seq: seq, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "error", Payload: []byte(`{"error":"engine was killed after the grace period"}`)})
		}
		return nil
	})
	if e.cfg.Hub != nil {
		e.cfg.Hub.Notify(a.ID)
	}
}
