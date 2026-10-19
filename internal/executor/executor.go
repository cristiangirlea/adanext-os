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
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cristiangirlea/ada-os/internal/api"
	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/engine"
	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

type ArtifactWriter interface {
	Write(context.Context, io.Reader, int64) (artifact.Ref, error)
	Open(hash string) (io.ReadCloser, error) // Verifies the content at EOF; a snapshot's content is read through it.
	Remove(hash string) error
	Blobs() ([]string, error)
	sync.Locker // The artifact lifecycle lock, shared with every other writer (artifact.Store.Lock).
}
type Notifier interface{ Notify(string) }

type Config struct {
	EnginePath, EngineSHA256, StateDir string
	EngineArgs                         []string // nil uses hrn exec --protocol v1; tests supply the helper-process argument.
	Artifacts                          ArtifactWriter
	Store                              *store.Store
	Hub                                Notifier
	Generation                         int64
	Concurrency                        int // Attempts launched at once across all users; 0 is the plan's default of 1.
	CancelGrace                        time.Duration
	PreparingDeadline                  time.Duration      // For hello and the start's answer; adad's -preparing-deadline, 0 is 2 minutes.
	Env                                []string           // Extra controlled environment entries for helper-process tests.
	Workspaces                         *source.Workspaces // nil when git is missing or too old: an imported project's attempt then fails.
}

type active struct {
	task, project string
	cancel        chan struct{}
	once          sync.Once
}
type Executor struct {
	cfg              Config
	mu               sync.Mutex
	active           map[string]*active
	stopped          bool
	wg               sync.WaitGroup
	beforeAccepted   func(*time.Timer, *atomic.Bool) // Test seam for the preparing deadline race.
	afterAppend      func()                          // Set only by tests to simulate slow event storage.
	beforeClaim      func(attemptID string)          // Test seam before the launch claim, for the cancel race.
	beforeCancel     func()                          // Test seam that pauses the cancel handling of a run.
	afterCancelCheck func()                          // Test seam after the cancel handling first checks whether the engine is accepted.
	afterRunning     func()                          // Test seam between storing the attempt running and accepting its events.
	beforeRemove     func(hash string)               // Test seam between discard's reference check and its removal.
	beforeCheck      func(attemptID string)          // Test seam between preparing the workspace and checking it before the engine starts.
}

func New(cfg Config) *Executor {
	if cfg.CancelGrace <= 0 {
		cfg.CancelGrace = 10 * time.Second
	}
	if cfg.PreparingDeadline <= 0 {
		cfg.PreparingDeadline = 2 * time.Minute
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	return &Executor{cfg: cfg, active: make(map[string]*active)}
}

// StopBudget covers an ignored cancel (stdin closed after one CancelGrace, the
// engine killed after two), the stderr WaitDelay, two post-exit checks of a
// descendant holding stdout, and state persistence.
func (e *Executor) StopBudget() time.Duration {
	return 5*e.cfg.CancelGrace + 5*time.Second
}

// LaunchAttempt tells the executor a pending attempt was recorded. Pending
// attempts are the queue: they launch one at a time (Config.Concurrency) in
// order of creation across all users, so the id is a nudge, not a choice.
func (e *Executor) LaunchAttempt(string) { e.schedule() }

// schedule fills the free launch slots from the queue in creation order,
// skipping a project with an attempt already active (one writer per
// workspace). The launch claim in run, pending to launching under the
// ownership generation, still decides a race with a cancel or another
// coordinator; a candidate whose claim fails is simply dropped here.
func (e *Executor) schedule() {
	for {
		// The queue is read without the lock: a run holds a store write
		// transaction while it reads e.stopped, so the lock never waits on
		// the store. What the read assumed is checked again before a launch.
		e.mu.Lock()
		if e.stopped || len(e.active) >= e.cfg.Concurrency {
			e.mu.Unlock()
			return
		}
		busy := e.busyProjects()
		e.mu.Unlock()
		next, ok, err := e.cfg.Store.NextPendingAttempt(context.Background(), e.cfg.Generation, func(project string) bool { return busy[project] })
		if err != nil || !ok {
			return
		}
		e.mu.Lock()
		if _, exists := e.active[next.ID]; exists || e.stopped || len(e.active) >= e.cfg.Concurrency || e.busyProjects()[next.Project] {
			// Taken by a concurrent schedule, stopped, or a slot or the
			// project filled meanwhile; the run that ends reschedules.
			e.mu.Unlock()
			return
		}
		run := &active{task: next.Task, project: next.Project, cancel: make(chan struct{})}
		e.active[next.ID] = run
		e.wg.Add(1)
		e.mu.Unlock()
		go func(a store.Attempt) {
			defer e.wg.Done()
			e.run(a, run)
			e.mu.Lock()
			delete(e.active, a.ID)
			e.mu.Unlock()
			e.schedule()
		}(next.Attempt)
	}
}

// busyProjects is called with e.mu held.
func (e *Executor) busyProjects() map[string]bool {
	busy := make(map[string]bool, len(e.active))
	for _, a := range e.active {
		busy[a.project] = true
	}
	return busy
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

// StopWork launches nothing more and asks every active attempt to cancel;
// Stop also waits for them. The owner's stop records its reason on every
// running and queued attempt first (store.Tx.StopWork), then calls this.
func (e *Executor) StopWork() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped = true
	for _, a := range e.active {
		a.once.Do(func() { close(a.cancel) })
	}
}

func (e *Executor) Stop(ctx context.Context) error {
	e.StopWork()
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

// readState is what the post-exit check knows about the stdout reader. Once
// the engine has exited, everything it wrote is already in the pipe, so a read
// that blocks is waiting on a descendant that inherited stdout; time the reader
// spends storing events between reads is not counted.
type readState struct {
	mu      sync.Mutex
	start   time.Time     // The read in progress; zero between reads.
	exit    time.Time     // When the engine's exit was observed; zero before.
	bytes   int64         // Bytes read since exit.
	blocked time.Duration // Time ended reads spent blocked since exit.
}

func (r *readState) begin() {
	r.mu.Lock()
	r.start = time.Now()
	r.mu.Unlock()
}

func (r *readState) end(n int) {
	r.mu.Lock()
	if !r.exit.IsZero() {
		r.bytes += int64(n)
		r.blocked += r.inProgressSinceExit(time.Now())
	}
	r.start = time.Time{}
	r.mu.Unlock()
}

func (r *readState) exited(now time.Time) {
	r.mu.Lock()
	r.exit = now
	r.mu.Unlock()
}

// afterExit returns the bytes read and the time spent blocked in reads since
// the engine exited, the read in progress included.
func (r *readState) afterExit() (int64, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytes, r.blocked + r.inProgressSinceExit(time.Now())
}

func (r *readState) inProgressSinceExit(now time.Time) time.Duration {
	if r.start.IsZero() || r.exit.IsZero() {
		return 0
	}
	from := r.start
	if r.exit.After(from) {
		from = r.exit
	}
	return now.Sub(from)
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
	if e.beforeClaim != nil {
		e.beforeClaim(a.ID)
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
	if err := os.MkdirAll(base, 0700); err != nil {
		e.fail(a, fmt.Errorf("attempt directory: %w", err))
		return
	}
	work, workspace, err := e.prepare(a, active, work)
	if mismatch, ok := errors.AsType[*artifactMismatch](err); ok {
		// Checked before anything is applied: the worktree holds none of it.
		e.recoverWorkspace(a, snapshotMismatch, mismatch.Error())
		return
	}
	if err != nil {
		e.fail(a, fmt.Errorf("workspace: %w", err))
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
	supervisor, err := newProcessSupervisor(cmd)
	if err != nil {
		e.fail(a, fmt.Errorf("process supervision: %w", err))
		return
	}
	defer supervisor.close()
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
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		stdin.Close()
		e.fail(a, err)
		return
	}
	cmd.Stdout = stdoutWriter
	if err := engine.Verify(enginePath, e.cfg.EngineSHA256); err != nil {
		stdin.Close()
		stdout.Close()
		stdoutWriter.Close()
		e.fail(a, fmt.Errorf("engine verification: %w", err))
		return
	}
	if workspace {
		if e.beforeCheck != nil {
			e.beforeCheck(a.ID)
		}
		if reason, mismatch := e.checkWorkspace(a); reason != "" {
			stdin.Close()
			stdout.Close()
			stdoutWriter.Close()
			e.recoverWorkspace(a, reason, mismatch)
			return
		}
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stdoutWriter.Close()
		e.fail(a, fmt.Errorf("launch: %w", err))
		return
	}
	if err := supervisor.started(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		stdin.Close()
		stdout.Close()
		stdoutWriter.Close()
		e.fail(a, fmt.Errorf("process supervision: %w", err))
		return
	}
	_ = stdoutWriter.Close()
	defer stdout.Close()
	var inputMu sync.Mutex
	write := func(v any) error { inputMu.Lock(); defer inputMu.Unlock(); return json.NewEncoder(stdin).Encode(v) }
	var processMu sync.Mutex
	var exited, killRequested bool
	var exitErr error
	processDone := make(chan struct{})
	kill := func() {
		processMu.Lock()
		killed := !exited && supervisor.kill(cmd) == nil
		if killed {
			killRequested = true
		}
		processMu.Unlock()
		if killed {
			_ = stdout.Close()
		}
	}
	var rs readState
	var timerMu sync.Mutex
	var closeTimer, killTimer, exitTimer *time.Timer
	shutdownStarted := false
	shutdownStopped := false
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
		shutdownStopped = true
		if closeTimer != nil {
			closeTimer.Stop()
		}
		if killTimer != nil {
			killTimer.Stop()
		}
		if exitTimer != nil {
			exitTimer.Stop()
		}
	}
	// Once the engine has exited, only a descendant that inherited stdout can
	// keep the reader busy. Close stdout when more than 1 MiB arrives after exit
	// (a streaming descendant: the pipe plus the bufio buffer hold less of the
	// engine's own output) or when reads have spent a whole CancelGrace blocked
	// since exit, in one read or across many (a silent or a slowly writing
	// descendant). Only blocked time counts, so slow storage still consumes
	// everything the engine wrote before it exited. A cancel or a Stop cannot
	// wait for either: a descendant feeding well-formed events faster than the
	// store persists them never blocks a read, so once a cancel is in progress
	// the next check closes stdout whatever arrives, and the kill timer no
	// longer applies to an exited engine. That close is also a cut-off for the
	// lines the reader has already buffered: each would cost a store write,
	// and enough of them would hold the stop past its budget.
	var stdoutClosedBecause string
	var stdoutCutOff bool
	var checkAfterExit func()
	checkAfterExit = func() {
		timerMu.Lock()
		defer timerMu.Unlock()
		if shutdownStopped {
			return
		}
		n, blocked := rs.afterExit()
		canceling := false
		select {
		case <-active.cancel:
			canceling = true
		default:
		}
		switch {
		case canceling:
			stdoutClosedBecause = "a cancel or stop arrived after the engine exited"
			stdoutCutOff = true
		case n > 1<<20:
			stdoutClosedBecause = "a descendant wrote more than 1 MiB after the engine exited"
		case blocked >= e.cfg.CancelGrace:
			stdoutClosedBecause = "reads blocked for a whole grace period after the engine exited"
		default:
			exitTimer = time.AfterFunc(e.cfg.CancelGrace, checkAfterExit)
			return
		}
		_ = stdout.Close()
	}
	closedBecause := func() string {
		timerMu.Lock()
		defer timerMu.Unlock()
		return stdoutClosedBecause
	}
	cutOff := func() bool {
		timerMu.Lock()
		defer timerMu.Unlock()
		return stdoutCutOff
	}
	go func() {
		err := cmd.Wait()
		rs.exited(time.Now())
		processMu.Lock()
		exitErr, exited = err, true
		processMu.Unlock()
		startShutdown()
		timerMu.Lock()
		if !shutdownStopped {
			exitTimer = time.AfterFunc(e.cfg.CancelGrace, checkAfterExit)
		}
		timerMu.Unlock()
		close(processDone)
	}()
	waitExit := func() (bool, error) {
		<-processDone
		stopShutdown()
		processMu.Lock()
		waitErr, killed := exitErr, killRequested
		processMu.Unlock()
		if waitErr != nil {
			if errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState.Success() {
				const detail = "a descendant kept stderr open after the engine exited"
				if err := e.append(a, "info", []byte(`{"event":{"kind":"info","text":"`+detail+`"}}`), ""); err != nil {
					return killed, fmt.Errorf("exit info storage: %w", err)
				}
				return killed, nil
			}
			return killed, waitErr
		}
		return killed, nil
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
			if e.beforeCancel != nil {
				e.beforeCancel()
			}
			sent := false
			select {
			case <-accepted:
				_ = e.requestCancel(a)
				_ = write(map[string]any{"type": "cancel", "attempt_id": a.ID})
				sent = true
			default:
			}
			if e.afterCancelCheck != nil {
				e.afterCancelCheck()
			}
			startShutdown()
			select {
			case <-accepted:
				// A cancel that came before the engine was accepted is sent
				// now, not left to the close timer.
				if !sent {
					_ = e.requestCancel(a)
					_ = write(map[string]any{"type": "cancel", "attempt_id": a.ID})
				}
			case <-done:
			}
		case <-done:
		}
	}()
	reader := bufio.NewReaderSize(stdout, 64*1024)
	read := func() (message, []byte, error) {
		var line []byte
		for {
			rs.begin()
			part, err := reader.ReadSlice('\n')
			rs.end(len(part))
			if len(line)+len(part) > 4<<20 {
				return message{}, nil, errors.New("line too long")
			}
			line = append(line, part...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil && len(line) != 0 {
				// Bytes without a newline at EOF, or when stdout was closed
				// under a descendant that held it, are a protocol violation.
				return message{}, nil, fmt.Errorf("truncated protocol line: %v", err)
			}
			if err != nil {
				return message{}, nil, err
			}
			break
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
				if !errors.Is(err, io.EOF) {
					buf := make([]byte, 32*1024)
					for {
						rs.begin()
						n, drainErr := reader.Read(buf)
						rs.end(n)
						if drainErr != nil {
							break
						}
					}
				}
				killed, waitErr := waitExit()
				return err, killed, waitErr
			}
		}
	}
	// One failure records one cause. An engine whose stream ended is exiting:
	// its exit status joins the cause, recorded once it is known. Otherwise the
	// cause is recorded before the shutdown, whose exit error it explains.
	abort := func(cause error) {
		causeRecorded := !errors.Is(cause, io.EOF) && e.recordCause(a, cause)
		_, killed, waitErr := drain()
		if waitErr != nil && !killed && !causeRecorded {
			cause = fmt.Errorf("%w; engine exit: %v", cause, waitErr)
		}
		e.failFinal(a, cause, causeRecorded, killed)
	}
	var preparingExpired atomic.Bool
	// Closing stdout fails the read of a missing hello or accepted; the
	// shutdown timers unblock a start write the engine never reads (stdin is
	// closed after one CancelGrace, the engine killed after two).
	preparingTimer := time.AfterFunc(e.cfg.PreparingDeadline, func() {
		preparingExpired.Store(true)
		startShutdown()
		_ = stdout.Close()
	})
	defer preparingTimer.Stop()
	preparingCause := func(cause error) error {
		if preparingExpired.Load() {
			return errors.New("preparing deadline exceeded")
		}
		return cause
	}
	hello, _, err := next("hello", false)
	if err != nil {
		abort(preparingCause(fmt.Errorf("hello: %w", err)))
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
		abort(preparingCause(fmt.Errorf("start write: %w", err)))
		return
	}
	answer, _, err := next("", true)
	if err != nil {
		abort(preparingCause(fmt.Errorf("start response: %w", err)))
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
	if e.beforeAccepted != nil {
		e.beforeAccepted(preparingTimer, &preparingExpired)
	}
	if !preparingTimer.Stop() {
		abort(errors.New("preparing deadline exceeded"))
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
		if _, err := tx.ExecContext(ctx, "UPDATE attempts SET state='running',run_id=? WHERE id=?", answer.RunID, a.ID); err != nil {
			return err
		}
		// A stop recorded while the attempt was launching is honoured in the
		// same transaction, so a result the engine returns before the cancel
		// below reaches it cannot end the task ok.
		canceled, err := tx.CancelRecorded(ctx, a.ID)
		if err != nil || !canceled {
			return err
		}
		return tx.TransitionOwned(ctx, a.Task, store.Running, store.CancelRequested, rev+1)
	}); err != nil {
		abort(err)
		return
	}
	if e.afterRunning != nil {
		e.afterRunning()
	}
	close(accepted)
	var fatalCause error
	for {
		m, line, err := next("", true)
		if err == nil && cutOff() {
			// A line still buffered after a cancel or Stop closed stdout
			// behind the exited engine is cut off, not ingested.
			err = os.ErrClosed
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				startShutdown()
				killed, waitErr := waitExit()
				cause := fmt.Errorf("engine exited without result: %v", waitErr)
				if reason := closedBecause(); reason != "" {
					cause = fmt.Errorf("%w; stdout closed: %s", cause, reason)
				}
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
			if !contains(protocolEventKinds, m.Event.Kind) {
				abort(fmt.Errorf("event kind %q is not in protocol v1", m.Event.Kind))
				return
			}
			if err := e.append(a, m.Event.Kind, line, m.Event.Text); err != nil {
				abort(fmt.Errorf("event storage: %w", err))
				return
			}
		case "error":
			if err := e.append(a, "engine_notice", line, ""); err != nil {
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
			if !errors.Is(tailErr, io.EOF) && !errors.Is(tailErr, os.ErrClosed) {
				cause := fmt.Errorf("result tail: %w", tailErr)
				causeRecorded := e.recordCause(a, cause)
				killed, _ := waitExit()
				if killed {
					if m.Result.Status == "ok" {
						e.recoverAfterResult(a)
					} else if fatalCause != nil {
						e.failFinal(a, fatalCause, true, true)
					} else {
						e.failFinal(a, cause, causeRecorded, true)
					}
				} else {
					e.failFinal(a, cause, causeRecorded, false)
				}
				return
			}
			killed, waitErr := waitExit()
			if killed {
				if m.Result.Status == "ok" {
					e.recoverAfterResult(a)
				} else {
					e.failFinal(a, errors.New("engine was killed after the grace period"), false, false)
				}
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

// protocolEventKinds are the event kinds protocol v1 defines.
var protocolEventKinds = []string{"text", "thinking", "tool_call", "tool_result", "info", "error", "session"}

// engineEnvironmentKeys are what the claude and codex CLIs need to find the
// owner's sign-ins and tools (decisions log, 2026-10-19).
func engineEnvironmentKeys(goos string) []string {
	keys := []string{"PATH", "SystemRoot", "WINDIR", "ComSpec", "PATHEXT", "LANG", "LC_ALL", "LC_CTYPE", "CLAUDE_CONFIG_DIR", "CODEX_HOME"}
	if goos == "windows" {
		return append(keys, "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "USERNAME", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "CommonProgramFiles", "CommonProgramFiles(x86)", "CommonProgramW6432", "ProgramData", "ALLUSERSPROFILE", "SystemDrive", "PUBLIC", "OS", "PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS", "COMPUTERNAME", "PSModulePath")
	}
	return append(keys, "HOME", "USER", "LOGNAME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR")
}

// refusedKey is a provider key or token or a variable of adad's own: never
// passed, even should the allow-list name one.
func refusedKey(key string) bool {
	key = strings.ToUpper(key)
	return strings.HasSuffix(key, "_API_KEY") || strings.HasSuffix(key, "_TOKEN") || strings.HasPrefix(key, "ADANEXT_") || strings.HasPrefix(key, "ADA_")
}

func (e *Executor) environment(base string) []string {
	var env []string
	for _, key := range engineEnvironmentKeys(runtime.GOOS) {
		if value := os.Getenv(key); value != "" && !refusedKey(key) {
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
	err := e.artifactLifecycle(func() error {
		event, ref, err := e.boundEvent(store.Event{Attempt: a.ID, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: kind, Payload: payload})
		if err != nil {
			return err
		}
		err = e.owned(a.ID, func(tx *store.Tx) error {
			if err := tx.QueryRowContext(context.Background(), "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE attempt=?", a.ID).Scan(&event.Seq); err != nil {
				return err
			}
			if err := e.appendBoundEvent(tx, event, ref); err != nil {
				return err
			}
			if kind == "session" {
				_, err := tx.ExecContext(context.Background(), "UPDATE attempts SET native_session=? WHERE id=?", session, a.ID)
				return err
			}
			return nil
		})
		if err != nil {
			return e.discard(err, ref)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if e.cfg.Hub != nil {
		e.cfg.Hub.Notify(a.ID)
	}
	if e.afterAppend != nil {
		e.afterAppend()
	}
	return nil
}

// artifactLifecycle runs fn under the artifact lifecycle lock
// (artifact.Store.Lock), which the snapshot handler shares: fn writes
// artifacts, runs the transaction that references them and discards them when
// it fails. Without an artifact store nothing is written, so nothing is locked.
// The in-transaction form (appendBoundedEvent) follows the same rule: a caller
// whose payload is fixed and small never reaches the store and runs without
// the lock; one whose payload may exceed the event limit (failFinal) takes it
// before its transaction opens. Config.Concurrency defaults to one attempt,
// so the lock is rarely contended; it exists for correctness.
func (e *Executor) artifactLifecycle(fn func() error) error {
	if e.cfg.Artifacts != nil {
		e.cfg.Artifacts.Lock()
		defer e.cfg.Artifacts.Unlock()
	}
	return fn()
}

// RemoveUnreferencedArtifacts removes the published blobs no committed record
// references: what a crash between a blob's publish and its commit, or a
// rolled-back in-transaction write, left behind. It holds the lifecycle lock,
// so no writer, the snapshot handler included, is between its publish and its
// commit, and blobs still being written are never listed. It reports how many
// it removed.
func (e *Executor) RemoveUnreferencedArtifacts(ctx context.Context) (removed int, err error) {
	if e.cfg.Artifacts == nil {
		return 0, nil
	}
	err = e.artifactLifecycle(func() error {
		hashes, err := e.cfg.Artifacts.Blobs()
		if err != nil {
			return err
		}
		var failed error
		for _, hash := range hashes {
			referenced, err := e.cfg.Store.ArtifactReferenced(ctx, hash)
			if err != nil {
				return errors.Join(failed, fmt.Errorf("artifact %s: %w", hash, err))
			}
			if referenced {
				continue
			}
			if err := e.cfg.Artifacts.Remove(hash); err != nil && !errors.Is(err, os.ErrNotExist) {
				// One blob held open (busy on Windows) must not keep the rest.
				failed = errors.Join(failed, fmt.Errorf("remove artifact %s: %w", hash, err))
				continue
			}
			removed++
		}
		return failed
	})
	return removed, err
}

// boundEvent returns the event as it is stored: unchanged when its encoding fits
// a page, else with its payload written as an artifact and replaced by the
// referenced-payload marker. It runs before the transaction that appends the
// event, where the sequence number is assigned, so the check uses the widest
// one an event can carry: what fits with that fits with its own. The caller
// passes the ref to appendBoundEvent, and to discard when the transaction fails.
func (e *Executor) boundEvent(event store.Event) (store.Event, artifact.Ref, error) {
	probe := event
	probe.Seq = math.MaxInt64
	if !api.EventExceedsLimit(probe) {
		return event, artifact.Ref{}, nil
	}
	if e.cfg.Artifacts == nil {
		return event, artifact.Ref{}, errors.New("artifact store unavailable")
	}
	ref, err := e.cfg.Artifacts.Write(context.Background(), bytes.NewReader(event.Payload), int64(len(event.Payload)))
	if err != nil {
		return event, artifact.Ref{}, fmt.Errorf("event artifact: %w", err)
	}
	event.Payload = api.ReferencedPayload(ref.Hash, ref.Size)
	return event, ref, nil
}

// appendBoundEvent appends an event boundEvent prepared, recording the
// reference to its artifact, if any, in the same transaction.
func (e *Executor) appendBoundEvent(tx *store.Tx, event store.Event, ref artifact.Ref) error {
	if ref.Hash != "" {
		if err := tx.ReferenceArtifact(context.Background(), ref.Hash, ref.Size, fmt.Sprintf("attempt:%s:event:%d", event.Attempt, event.Seq)); err != nil {
			return err
		}
	}
	return tx.AppendOwnedEvent(context.Background(), event)
}

// appendBoundedEvent is the in-transaction form for the recovery and failure
// paths, where the artifact is a safety net: a transaction that fails after it
// cannot remove what was written. A caller with a fixed, small payload runs
// without the lifecycle lock; a caller whose payload can exceed the event
// limit holds it around the whole transaction (see artifact.Store.Lock).
func (e *Executor) appendBoundedEvent(tx *store.Tx, event store.Event) error {
	event, ref, err := e.boundEvent(event)
	if err != nil {
		return err
	}
	return e.appendBoundEvent(tx, event, ref)
}

// discard removes the artifacts written for a transaction that then failed,
// unless a committed record references the same content, and returns the
// failure with anything the removal added.
func (e *Executor) discard(cause error, refs ...artifact.Ref) error {
	for _, ref := range refs {
		if ref.Hash == "" {
			continue
		}
		referenced, err := e.cfg.Store.ArtifactReferenced(context.Background(), ref.Hash)
		if err != nil {
			cause = errors.Join(cause, fmt.Errorf("artifact %s: %w", ref.Hash, err))
			continue
		}
		if referenced {
			continue
		}
		if e.beforeRemove != nil {
			e.beforeRemove(ref.Hash)
		}
		if err := e.cfg.Artifacts.Remove(ref.Hash); err != nil && !errors.Is(err, os.ErrNotExist) {
			cause = errors.Join(cause, fmt.Errorf("remove artifact %s: %w", ref.Hash, err))
		}
	}
	return cause
}

func (e *Executor) result(a store.Attempt, m message, line []byte) error {
	if err := e.artifactLifecycle(func() error { return e.storeResult(a, m, line) }); err != nil {
		return err
	}
	if e.cfg.Hub != nil {
		e.cfg.Hub.Notify(a.ID)
	}
	return nil
}

// storeResult writes the result's artifacts and records the result event in
// one transaction; the caller holds the lifecycle lock.
func (e *Executor) storeResult(a store.Attempt, m message, line []byte) error {
	var final artifact.Ref
	if len(m.Result.Final) > 64<<10 {
		if e.cfg.Artifacts == nil {
			return errors.New("artifact store unavailable")
		}
		var err error
		final, err = e.cfg.Artifacts.Write(context.Background(), strings.NewReader(m.Result.Final), int64(len(m.Result.Final)))
		if err != nil {
			return fmt.Errorf("final artifact: %w", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(line, &payload); err != nil {
			return e.discard(err, final)
		}
		result, ok := payload["result"].(map[string]any)
		if !ok {
			return e.discard(errors.New("result object is missing"), final)
		}
		delete(result, "final")
		result["final_artifact"], result["final_size"] = final.Hash, final.Size
		line, err = json.Marshal(payload)
		if err != nil {
			return e.discard(err, final)
		}
	}
	event, ref, err := e.boundEvent(store.Event{Attempt: a.ID, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "result", Payload: line})
	if err != nil {
		return e.discard(err, final)
	}
	err = e.owned(a.ID, func(tx *store.Tx) error {
		if final.Hash != "" {
			if err := tx.ReferenceArtifact(context.Background(), final.Hash, final.Size, "attempt:"+a.ID+":final"); err != nil {
				return err
			}
		}
		if err := tx.QueryRowContext(context.Background(), "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE attempt=?", a.ID).Scan(&event.Seq); err != nil {
			return err
		}
		return e.appendBoundEvent(tx, event, ref)
	})
	if err != nil {
		return e.discard(err, final, ref)
	}
	return nil
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

func (e *Executor) recordCause(a store.Attempt, cause error) bool {
	payload, _ := json.Marshal(map[string]any{"error": cause.Error()})
	return e.append(a, "executor_error", payload, "") == nil
}

func (e *Executor) recoverAfterResult(a store.Attempt) {
	err := e.owned(a.ID, func(tx *store.Tx) error {
		var state store.State
		var rev int64
		if err := tx.QueryRowContext(context.Background(), "SELECT state,revision FROM tasks WHERE id=?", a.Task).Scan(&state, &rev); err != nil {
			return err
		}
		if state != store.Running && state != store.CancelRequested {
			return fmt.Errorf("result recovery: task is %s", state)
		}
		if err := tx.TransitionOwned(context.Background(), a.Task, state, store.Interrupted, rev); err != nil {
			return err
		}
		if err := tx.TransitionOwned(context.Background(), a.Task, store.Interrupted, store.RecoveryRequired, rev+1); err != nil {
			return err
		}
		if _, err := tx.ExecContext(context.Background(), "UPDATE attempts SET state='recovery_required',ended=? WHERE id=?", time.Now().UTC().Format(time.RFC3339Nano), a.ID); err != nil {
			return err
		}
		var seq int64
		if err := tx.QueryRowContext(context.Background(), "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE attempt=?", a.ID).Scan(&seq); err != nil {
			return err
		}
		return e.appendBoundedEvent(tx, store.Event{Attempt: a.ID, Seq: seq, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "recovery", Payload: []byte(`{"reason":"engine had to be killed after its result"}`)})
	})
	if err != nil {
		e.failFinal(a, fmt.Errorf("result recovery: %w", err), false, true)
		return
	}
	if e.cfg.Hub != nil {
		e.cfg.Hub.Notify(a.ID)
	}
}

func (e *Executor) failFinal(a store.Attempt, cause error, causeRecorded, killed bool) {
	if errors.Is(cause, store.ErrStaleOwnership) {
		return
	}
	payload, _ := json.Marshal(map[string]any{"error": cause.Error()})
	record := func(tx *store.Tx) error {
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
			if err := e.appendBoundedEvent(tx, store.Event{Attempt: a.ID, Seq: seq, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "recovery", Payload: []byte(`{"reason":"engine exit not confirmed"}`)}); err != nil {
				return err
			}
			seq++
		}
		if !causeRecorded {
			if err := e.appendBoundedEvent(tx, store.Event{Attempt: a.ID, Seq: seq, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "executor_error", Payload: payload}); err != nil {
				return err
			}
			seq++
		}
		if killed {
			return e.appendBoundedEvent(tx, store.Event{Attempt: a.ID, Seq: seq, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "executor_error", Payload: []byte(`{"error":"engine was killed after the grace period"}`)})
		}
		return nil
	}
	// The cause is not bounded, so the transaction that references its payload
	// is an artifact lifecycle: a concurrent discard cannot remove content it
	// reuses. The lock is taken before the transaction opens and released
	// before the notification.
	_ = e.artifactLifecycle(func() error { return e.owned(a.ID, record) })
	if e.cfg.Hub != nil {
		e.cfg.Hub.Notify(a.ID)
	}
}
