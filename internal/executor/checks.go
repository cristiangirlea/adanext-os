package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func checkEnvironmentKeys(goos string) []string {
	var out []string
	for _, k := range engineEnvironmentKeys(goos) {
		if !store.CredentialProfileEnvKey(k) && !refusedKey(k) {
			out = append(out, k)
		}
	}
	return out
}

func (e *Executor) lookupCheckEnv(key string) (string, bool) {
	for i := len(e.cfg.Env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(e.cfg.Env[i], "=")
		if !ok {
			continue
		}
		if runtime.GOOS == "windows" && strings.EqualFold(k, key) {
			return v, true
		}
		if k == key {
			return v, true
		}
	}
	return os.LookupEnv(key)
}

func (e *Executor) checkEnvironment(extra []string) []string {
	var env []string
	seen := map[string]bool{}
	add := func(key string) {
		if refusedKey(key) || store.CredentialProfileEnvKey(key) {
			return
		}
		norm := key
		if runtime.GOOS == "windows" {
			norm = strings.ToUpper(key)
		}
		if seen[norm] {
			return
		}
		if val, ok := e.lookupCheckEnv(key); ok && val != "" {
			seen[norm] = true
			env = append(env, key+"="+val)
		}
	}
	for _, k := range checkEnvironmentKeys(runtime.GOOS) {
		add(k)
	}
	for _, k := range extra {
		add(k)
	}
	return env
}

func newRunID(prefix string) string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func (e *Executor) commitAndVerify(a store.Attempt, active *active) {
	ctx := context.Background()
	if e.cfg.Workspaces == nil {
		if err := e.finish(a, "ok"); err != nil {
			e.fail(a, fmt.Errorf("finish storage: %w", err))
		}
		return
	}
	row, err := e.cfg.Store.Workspace(ctx, a.ID)
	if err != nil {
		e.recoverWorkspaceVerify(a, "workspace verification failed", err.Error())
		return
	}
	configSHA256, err := e.cfg.Store.CloneConfig(ctx, row.Project)
	if err != nil {
		e.recoverWorkspaceVerify(a, "workspace verification failed", err.Error())
		return
	}
	m := source.Manifest{
		Version:      row.ManifestVersion,
		Project:      row.Project,
		Task:         row.Task,
		TaskRevision: row.TaskRevision,
		Attempt:      row.Attempt,
		Generation:   row.Generation,
		Snapshot:     row.Snapshot,
		BaseCommit:   row.BaseCommit,
		Branch:       row.Branch,
		Path:         row.Path,
		Executor:     row.Executor,
		Policy:       row.Policy,
		ConfigSHA256: configSHA256,
	}
	testedCommit, importedCommit, err := e.cfg.Workspaces.CommitAgentChanges(ctx, m, row.ManifestHash)
	if err != nil {
		e.recoverWorkspaceVerify(a, "workspace verification failed", err.Error())
		return
	}

	var verifying bool
	err = e.owned(a.ID, func(tx *store.Tx) error {
		var state store.State
		var rev int64
		if err := tx.QueryRowContext(ctx, "SELECT state,revision FROM tasks WHERE id=?", a.Task).Scan(&state, &rev); err != nil {
			return err
		}
		to := store.Verifying
		attemptState := "ended_ok"
		if state == store.CancelRequested {
			to = store.Canceled
			attemptState = "canceled"
		} else {
			verifying = true
		}
		if err := tx.TransitionOwned(ctx, a.Task, state, to, rev); err != nil {
			return err
		}
		if err := tx.RecordTestedCommit(ctx, a.ID, testedCommit); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE attempts SET state=?,ended=? WHERE id=?", attemptState, time.Now().UTC().Format(time.RFC3339Nano), a.ID)
		return err
	})
	if err != nil {
		e.fail(a, fmt.Errorf("finish storage: %w", err))
		return
	}
	if e.cfg.Hub != nil {
		e.cfg.Hub.Notify(a.ID)
	}
	if !verifying {
		return
	}
	e.verifyAttempt(ctx, a, active, row, importedCommit, testedCommit, configSHA256)
}

func (e *Executor) recoverWorkspaceVerify(a store.Attempt, reason, detail string) {
	payload, _ := json.Marshal(map[string]any{"reason": reason, "mismatch": detail})
	err := e.artifactLifecycle(func() error {
		return e.owned(a.ID, func(tx *store.Tx) error {
			var state store.State
			var rev int64
			if err := tx.QueryRowContext(context.Background(), "SELECT state,revision FROM tasks WHERE id=?", a.Task).Scan(&state, &rev); err != nil {
				return err
			}
			if state != store.Running && state != store.CancelRequested {
				return fmt.Errorf("verify recovery: task is %s", state)
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
			return e.appendBoundedEvent(tx, store.Event{Attempt: a.ID, Seq: seq, Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "recovery", Payload: payload})
		})
	})
	if err != nil {
		e.failFinal(a, fmt.Errorf("verify recovery: %w", err), false, false)
		return
	}
	if e.cfg.Hub != nil {
		e.cfg.Hub.Notify(a.ID)
	}
}

func (e *Executor) sweepCheckWorktrees(ctx context.Context, project string) {
	if e.cfg.Workspaces == nil || e.cfg.Store == nil {
		return
	}
	leases, err := e.cfg.Store.ListCheckWorktrees(ctx, project)
	if err != nil {
		return
	}
	for _, l := range leases {
		if err := e.cfg.Workspaces.RemoveCheckWorktree(l.Project, l.Attempt, l.RunID, l.AdminName); err == nil {
			_ = e.cfg.Store.DeleteCheckWorktree(ctx, l.Project, l.Attempt, l.RunID)
		}
	}
}

// verifyAttempt runs the task's checks and moves the task to awaiting_review.
// A storage error ends verification there too: it is recorded, and the checks
// without a result are recorded as not run.
func (e *Executor) verifyAttempt(ctx context.Context, a store.Attempt, active *active, ws store.Workspace, importedCommit, testedCommit, configSHA256 string) {
	e.sweepCheckWorktrees(ctx, ws.Project)
	defs, err := e.cfg.Store.TaskChecks(ctx, a.Task)
	next := 0
	if err == nil {
		next, err = e.runChecks(ctx, a, active, ws, defs, importedCommit, testedCommit, configSHA256)
	}
	if err != nil {
		e.recordCause(a, fmt.Errorf("verify: %w", err))
	}
	_ = e.owned(a.ID, func(tx *store.Tx) error {
		for ord := next; ord < len(defs); ord++ {
			if err := tx.RecordCheckResult(ctx, store.CheckResult{
				ID:             newRunID("chk_"),
				Task:           a.Task,
				Attempt:        a.ID,
				Ord:            ord,
				Name:           defs[ord].Name,
				DefinitionHash: store.CheckDefinitionHash(defs[ord]),
				Definition:     defs[ord],
				State:          store.CheckNotRun,
				TestedCommit:   testedCommit,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	_ = e.owned(a.ID, func(tx *store.Tx) error {
		var state store.State
		var rev int64
		if err := tx.QueryRowContext(ctx, "SELECT state,revision FROM tasks WHERE id=?", a.Task).Scan(&state, &rev); err != nil {
			return err
		}
		if state != store.Verifying {
			return nil
		}
		return tx.TransitionOwned(ctx, a.Task, store.Verifying, store.AwaitingReview, rev)
	})
	if e.cfg.Hub != nil {
		e.cfg.Hub.Notify(a.ID)
	}
}

// runChecks runs each check after its baseline and returns the ord of the
// first check left without a result.
func (e *Executor) runChecks(ctx context.Context, a store.Attempt, active *active, ws store.Workspace, defs []store.CheckDefinition, importedCommit, testedCommit, configSHA256 string) (int, error) {
	isStopped := func() bool {
		select {
		case <-active.cancel:
			return true
		default:
			return false
		}
	}
	for ord, def := range defs {
		if isStopped() {
			return ord, nil
		}
		defHash := store.CheckDefinitionHash(def)
		result := store.CheckResult{
			ID:             newRunID("chk_"),
			Task:           a.Task,
			Attempt:        a.ID,
			Ord:            ord,
			Name:           def.Name,
			DefinitionHash: defHash,
			Definition:     def,
			State:          store.CheckRunning,
			TestedCommit:   testedCommit,
			StartedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := e.owned(a.ID, func(tx *store.Tx) error { return tx.RecordCheckResult(ctx, result) }); err != nil {
			return ord, err
		}
		killed := func() (int, error) {
			result.State = store.CheckKilled
			if err := e.saveCheckResult(ctx, a.ID, result, nil); err != nil {
				return ord, err
			}
			return ord + 1, nil
		}

		baseline, err := e.cfg.Store.BaselineForCheck(ctx, ws.Snapshot, defHash)
		if err != nil {
			return ord, err
		}
		if baseline == nil {
			bID := newRunID("base_")
			bOut := e.runSingleCheck(ctx, a, active, ws.Project, bID, def, importedCommit, configSHA256)
			if bOut.state == store.CheckKilled {
				return killed()
			}
			// A refused run says nothing about the snapshot: it is not kept, so
			// the next attempt runs the baseline again.
			if !bOut.refused {
				defBytes, _ := json.Marshal(def)
				b := store.CheckBaseline{
					ID:             bID,
					Project:        ws.Project,
					Snapshot:       ws.Snapshot,
					DefinitionHash: defHash,
					DefinitionJSON: string(defBytes),
					State:          bOut.state,
					ExitCode:       bOut.exitCode,
					TestedCommit:   importedCommit,
					BoundCommit:    bOut.boundCommit,
					OutputSize:     bOut.outputSize,
					Truncated:      bOut.truncated,
					EnvKeys:        bOut.envKeys,
					DurationMS:     bOut.durationMS,
					StartedAt:      bOut.startedAt,
					EndedAt:        bOut.endedAt,
				}
				if err := e.saveCheckBaseline(ctx, a.ID, b, bOut.outputBytes); err != nil {
					return ord, err
				}
				baseline = &b
			}
		}
		if baseline != nil {
			result.BaselineID = baseline.ID
		}
		if isStopped() {
			return killed()
		}

		out := e.runSingleCheck(ctx, a, active, ws.Project, result.ID, def, testedCommit, configSHA256)
		result.State, result.ExitCode, result.BoundCommit = out.state, out.exitCode, out.boundCommit
		result.OutputSize, result.Truncated, result.EnvKeys, result.DurationMS, result.EndedAt = out.outputSize, out.truncated, out.envKeys, out.durationMS, out.endedAt
		if err := e.saveCheckResult(ctx, a.ID, result, out.outputBytes); err != nil {
			return ord, err
		}
		if out.state == store.CheckKilled {
			return ord + 1, nil
		}
	}
	return len(defs), nil
}

type singleCheckOutcome struct {
	state       store.CheckState
	refused     bool // Ada could not run it: nothing is said about the commit.
	exitCode    *int
	boundCommit string
	outputBytes []byte
	outputSize  int64
	truncated   bool
	envKeys     []string
	durationMS  int64
	startedAt   string
	endedAt     string
}

func (e *Executor) runSingleCheck(ctx context.Context, a store.Attempt, active *active, project, runID string, def store.CheckDefinition, commit, configSHA256 string) singleCheckOutcome {
	startedAt := time.Now().UTC().Format(time.RFC3339Nano)
	// refuse is a check Ada did not run: not run, binding nothing.
	refuse := func(format string, args ...any) singleCheckOutcome {
		msg := []byte(fmt.Sprintf(format+"\n", args...))
		return singleCheckOutcome{
			state:       store.CheckNotRun,
			refused:     true,
			outputBytes: msg,
			outputSize:  int64(len(msg)),
			envKeys:     []string{},
			startedAt:   startedAt,
			endedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		}
	}
	adminName := runID
	_ = e.cfg.Store.RecordCheckWorktree(ctx, store.CheckWorktreeLease{
		Project:   project,
		Attempt:   a.ID,
		RunID:     runID,
		AdminName: adminName,
	})
	wt, prepAdmin, err := e.cfg.Workspaces.PrepareCheckWorktree(ctx, project, a.ID, runID, commit, configSHA256)
	if prepAdmin != "" {
		adminName = prepAdmin
	}
	defer func() {
		if err := e.cfg.Workspaces.RemoveCheckWorktree(project, a.ID, runID, adminName); err == nil {
			_ = e.cfg.Store.DeleteCheckWorktree(ctx, project, a.ID, runID)
		}
	}()
	if err != nil {
		return refuse("prepare check worktree: %v", err)
	}

	resolvedDir, err := source.ResolveWorkdirNoFollow(wt, def.Workdir)
	if err != nil {
		return refuse("resolve check workdir: %v", err)
	}

	maxOut := def.MaxOutputBytes
	if maxOut <= 0 || maxOut > store.MaxCheckOutputBytes {
		maxOut = store.MaxCheckOutputBytes
	}
	timeout := store.MaxCheckTimeout
	if def.Timeout != "" {
		if d, err := time.ParseDuration(def.Timeout); err == nil && d > 0 && d <= store.MaxCheckTimeout {
			timeout = d
		}
	}

	cmd := exec.Command(def.Argv[0], def.Argv[1:]...)
	cmd.Dir = resolvedDir
	cmd.Env = e.checkEnvironment(def.Env)
	cmd.WaitDelay = e.cfg.CancelGrace
	keys := make([]string, 0, len(cmd.Env))
	for _, kv := range cmd.Env {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	supervisor, err := newProcessSupervisor(cmd)
	if err != nil {
		return refuse("process supervision: %v", err)
	}
	defer supervisor.close()

	outReader, outWriter, err := os.Pipe()
	if err != nil {
		return refuse("pipe: %v", err)
	}
	cmd.Stdout = outWriter
	cmd.Stderr = outWriter

	start := time.Now()
	if err := cmd.Start(); err != nil {
		_ = outWriter.Close()
		_ = outReader.Close()
		return refuse("start check: %v", err)
	}
	if err := supervisor.started(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = outWriter.Close()
		_ = outReader.Close()
		return refuse("supervise check: %v", err)
	}
	_ = outWriter.Close()

	var captured bytes.Buffer
	var truncated bool
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := outReader.Read(buf)
			if n > 0 {
				rem := maxOut - int64(captured.Len())
				if rem > 0 {
					take := min(int64(n), rem)
					_, _ = captured.Write(buf[:take])
					if int64(n) > rem {
						truncated = true
					}
				} else {
					truncated = true
				}
			}
			if err != nil {
				return
			}
		}
	}()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var timedOut, stopped bool
	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-timer.C:
		timedOut = true
		_ = supervisor.kill(cmd)
		_ = outReader.Close()
		waitErr = <-waitCh
	case <-active.cancel:
		stopped = true
		_ = supervisor.kill(cmd)
		_ = outReader.Close()
		waitErr = <-waitCh
	}
	duration := time.Since(start).Milliseconds()
	// Terminate and wait for any remaining descendants before closing outReader
	// and before inspecting or removing the check's worktree.
	supervisor.close()
	_ = outReader.Close()
	<-readDone

	out := singleCheckOutcome{
		outputBytes: captured.Bytes(),
		outputSize:  int64(captured.Len()),
		truncated:   truncated,
		envKeys:     keys,
		durationMS:  duration,
		startedAt:   startedAt,
		endedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	if cmd.ProcessState != nil {
		c := cmd.ProcessState.ExitCode()
		out.exitCode = &c
	}
	if stopped {
		out.state = store.CheckKilled
		return out
	}

	unchanged, err := e.cfg.Workspaces.CheckTreeUnchanged(ctx, project, a.ID, runID, adminName, commit, configSHA256)
	if err != nil {
		return refuse("inspect check worktree: %v", err)
	}
	if !unchanged {
		out.state = store.CheckChangedTree
		return out
	}

	out.state, out.boundCommit = store.CheckPassed, commit
	switch {
	case timedOut:
		out.state = store.CheckTimedOut
	case waitErr != nil && (!errors.Is(waitErr, exec.ErrWaitDelay) || !cmd.ProcessState.Success()):
		out.state = store.CheckFailed
	}
	return out
}

func (e *Executor) saveCheckBaseline(ctx context.Context, attemptID string, b store.CheckBaseline, output []byte) error {
	return e.artifactLifecycle(func() error {
		var ref artifact.Ref
		if len(output) > 0 && e.cfg.Artifacts != nil {
			var err error
			ref, err = e.cfg.Artifacts.Write(ctx, bytes.NewReader(output), int64(len(output)))
			if err != nil {
				return err
			}
			b.OutputArtifact = ref.Hash
			b.OutputSize = ref.Size
		}
		err := e.owned(attemptID, func(tx *store.Tx) error {
			if ref.Hash != "" {
				if err := tx.ReferenceArtifact(ctx, ref.Hash, ref.Size, "baseline:"+b.ID); err != nil {
					return err
				}
			}
			return tx.RecordCheckBaseline(ctx, b)
		})
		if err != nil {
			return e.discard(err, ref)
		}
		return nil
	})
}

func (e *Executor) saveCheckResult(ctx context.Context, attemptID string, r store.CheckResult, output []byte) error {
	return e.artifactLifecycle(func() error {
		var ref artifact.Ref
		if len(output) > 0 && e.cfg.Artifacts != nil {
			var err error
			ref, err = e.cfg.Artifacts.Write(ctx, bytes.NewReader(output), int64(len(output)))
			if err != nil {
				return err
			}
			r.OutputArtifact = ref.Hash
			r.OutputSize = ref.Size
		}
		err := e.owned(attemptID, func(tx *store.Tx) error {
			if ref.Hash != "" {
				if err := tx.ReferenceArtifact(ctx, ref.Hash, ref.Size, "check:"+r.ID); err != nil {
					return err
				}
			}
			return tx.RecordCheckResult(ctx, r)
		})
		if err != nil {
			return e.discard(err, ref)
		}
		return nil
	})
}
