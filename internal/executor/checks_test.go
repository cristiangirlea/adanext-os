package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func TestHelperCheck(t *testing.T) {
	idx := -1
	for i, arg := range os.Args {
		if arg == "--" {
			idx = i + 1
			break
		}
	}
	if idx < 0 || idx >= len(os.Args) {
		return
	}
	args := os.Args[idx:]
	mode := args[0]
	switch mode {
	case "pass":
		fmt.Println("check passed")
		os.Exit(0)
	case "fail":
		fmt.Println("check failed")
		os.Exit(7)
	case "timeout_tree":
		marker := args[1]
		child := exec.Command(os.Args[0], "-test.run=^TestHelperCheck$", "--", "hold_child", marker)
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "hold_child":
		marker := args[1]
		id, _ := processIdentity(os.Getpid())
		f, _ := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		fmt.Fprintf(f, "child:%d:%s\n", os.Getpid(), id)
		f.Close()
		for {
			time.Sleep(time.Hour)
		}
	case "over_cap":
		fmt.Print(strings.Repeat("A", 8192))
		os.Exit(5)
	case "check_file":
		file, want, countFile := args[1], args[2], args[3]
		if countFile != "" {
			f, _ := os.OpenFile(countFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			fmt.Fprintln(f, "run")
			f.Close()
		}
		data, err := os.ReadFile(file)
		if err != nil || !strings.Contains(string(data), want) {
			os.Exit(1)
		}
		os.Exit(0)
	case "edit_tracked":
		file := args[1]
		_ = os.WriteFile(file, []byte("mutated by check\n"), 0o644)
		os.Exit(0)
	case "assert_content":
		file, want := args[1], args[2]
		data, err := os.ReadFile(file)
		if err != nil || string(data) != want {
			fmt.Fprintf(os.Stderr, "got %q, want %q (%v)\n", data, want, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "assert_absent":
		if _, err := os.Lstat(args[1]); !errors.Is(err, fs.ErrNotExist) {
			os.Exit(1)
		}
		os.Exit(0)
	case "dump_env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
		os.Exit(0)
	case "daemonize":
		marker := args[1]
		if runtime.GOOS == "windows" {
			child := exec.Command(os.Args[0], "-test.run=^TestHelperCheck$", "--", "hold_child", marker)
			detachProcess(child)
			if err := child.Start(); err != nil {
				os.Exit(2)
			}
		} else {
			// Both setsid and double-fork on Unix.
			setsidChild := exec.Command(os.Args[0], "-test.run=^TestHelperCheck$", "--", "hold_child", marker)
			detachProcess(setsidChild)
			if err := setsidChild.Start(); err != nil {
				os.Exit(2)
			}
			forker := exec.Command(os.Args[0], "-test.run=^TestHelperCheck$", "--", "double_fork_step", marker)
			if err := forker.Run(); err != nil {
				os.Exit(2)
			}
		}
		for deadline := time.Now().Add(markerBound); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			data, _ := os.ReadFile(marker)
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			want := 1
			if runtime.GOOS != "windows" {
				want = 2
			}
			if len(lines) >= want && strings.HasPrefix(lines[0], "child:") {
				os.Exit(0)
			}
		}
		os.Exit(3)
	case "double_fork_step":
		marker := args[1]
		grand := exec.Command(os.Args[0], "-test.run=^TestHelperCheck$", "--", "hold_child", marker)
		if err := grand.Start(); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
}

func checkHelperArgv(args ...string) []string {
	return append([]string{os.Args[0], "-test.run=^TestHelperCheck$", "--"}, args...)
}

func runFakeAgentScript(script string) {
	cwd, _ := os.Getwd()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_AUTHOR_NAME=Agent", "GIT_AUTHOR_EMAIL=agent@example.com",
			"GIT_COMMITTER_NAME=Agent", "GIT_COMMITTER_EMAIL=agent@example.com")
		_ = cmd.Run()
	}
	switch script {
	case "agent_edit":
		_ = os.WriteFile(os.Getenv("ADA_FAKE_EDIT_FILE"), []byte(os.Getenv("ADA_FAKE_EDIT_CONTENT")), 0o644)
	case "agent_edit_and_ignored":
		_ = os.WriteFile(filepath.Join(cwd, ".gitignore"), []byte("ignored.secret\n"), 0o644)
		_ = os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("agent-clean\n"), 0o644)
		_ = os.WriteFile(filepath.Join(cwd, "ignored.secret"), []byte("do-not-leak\n"), 0o644)
	case "agent_repo_scripts":
		m := os.Getenv("ADA_FAKE_SCRIPT_MARKER")
		_ = os.WriteFile(filepath.Join(cwd, "package.json"), []byte(`{"scripts":{"test":"echo ran > `+filepath.ToSlash(m)+`"}}`), 0o644)
		_ = os.WriteFile(filepath.Join(cwd, "Makefile"), []byte("test:\n\techo ran > "+filepath.ToSlash(m)+"\n"), 0o644)
	case "agent_own_commit":
		_ = os.WriteFile(filepath.Join(cwd, "agent.txt"), []byte("from agent commit\n"), 0o644)
		runGit("add", "agent.txt")
		runGit("commit", "-q", "-m", "agent commit")
		_ = os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("uncommitted after agent commit\n"), 0o644)
	case "tamper_hookspath":
		hooks := filepath.Join(cwd, ".evil-hooks")
		_ = os.MkdirAll(hooks, 0o755)
		_ = os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\necho pwn > "+filepath.ToSlash(os.Getenv("ADA_FAKE_HOOK_MARKER"))+"\n"), 0o755)
		runGit("config", "core.hooksPath", filepath.ToSlash(hooks))
	case "tamper_fsmonitor":
		runGit("config", "core.fsmonitor", "true")
	case "tamper_gpg":
		runGit("config", "gpg.program", "evil-gpg")
	case "tamper_gitfile":
		_ = os.WriteFile(filepath.Join(cwd, ".git"), []byte("gitdir: "+filepath.ToSlash(cwd)+"/.fake-git\n"), 0o600)
	case "tamper_reset_below":
		runGit("reset", "-q", "--hard", "HEAD~1")
	case "agent_link_workdir":
		target := filepath.ToSlash(os.Getenv("ADA_FAKE_OUTSIDE_DIR"))
		cmd := exec.Command("git", "hash-object", "-w", "--stdin")
		cmd.Dir = cwd
		cmd.Stdin = strings.NewReader(target)
		out, _ := cmd.Output()
		blob := strings.TrimSpace(string(out))
		runGit("update-index", "--add", "--cacheinfo", "120000,"+blob+",linked")
		runGit("commit", "-q", "-m", "linked workdir")
	}
}

func setTaskChecksForTest(t *testing.T, s *store.Store, defs []store.CheckDefinition) {
	t.Helper()
	ctx := context.Background()
	_, err := s.Do(ctx, "u", "set-checks-"+fmt.Sprint(time.Now().UnixNano()), "set-checks", nil, func(tx *store.Tx) ([]byte, error) {
		raw, err := s.TaskChecks(ctx, "task")
		_ = raw
		norm, err := store.NormalizeCheckDefinitions(defs)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(bytes.NewReader(mustJSON(t, norm)))
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, "UPDATE tasks SET checks_json=? WHERE id='task'", string(data))
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func waitTaskState(t *testing.T, s *store.Store, want store.State) store.Task {
	t.Helper()
	deadline := time.Now().Add(preparationBound)
	for time.Now().Before(deadline) {
		task, err := s.Task(context.Background(), "u", "task")
		if err == nil && task.State == want {
			return task
		}
		time.Sleep(15 * time.Millisecond)
	}
	task, _ := s.Task(context.Background(), "u", "task")
	t.Fatalf("task state = %s, want %s (attempt = %+v)", task.State, want, task.Attempt)
	return store.Task{}
}

func TestChecksPassFailTimeOutAndKillProcessTree(t *testing.T) {
	e, s, _, _, _ := workspaceExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	childMarker := filepath.Join(t.TempDir(), "timeout-child")
	cleanupGrandchild(t, childMarker)
	setTaskChecksForTest(t, s, []store.CheckDefinition{
		{Name: "pass", Argv: checkHelperArgv("pass")},
		{Name: "fail", Argv: checkHelperArgv("fail")},
		{Name: "timeout", Argv: checkHelperArgv("timeout_tree", childMarker), Timeout: "200ms"},
	})
	e.LaunchAttempt("attempt")
	task := waitTaskState(t, s, store.AwaitingReview)
	if !task.Attempt.ChecksFailed {
		t.Fatalf("expected ChecksFailed on task attempt: %+v", task.Attempt)
	}
	results := task.Attempt.CheckResults()
	if len(results) != 3 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].State != store.CheckPassed || results[0].BoundCommit == "" || *results[0].ExitCode != 0 {
		t.Fatalf("result 0 = %+v", results[0])
	}
	if results[1].State != store.CheckFailed || results[1].BoundCommit == "" || *results[1].ExitCode != 7 {
		t.Fatalf("result 1 = %+v", results[1])
	}
	if results[2].State != store.CheckTimedOut || results[2].BoundCommit == "" {
		t.Fatalf("result 2 = %+v", results[2])
	}
	waitGrandchildMarker(t, childMarker)
	for _, line := range markerLines(t, childMarker) {
		var pid int
		var id string
		if _, err := fmt.Sscanf(line, "child:%d:%s", &pid, &id); err == nil && pid > 0 {
			if err := waitProcessGone(pid, id, 0); err != nil {
				t.Fatalf("timed-out check descendant still running: %v", err)
			}
		}
	}
}

func TestCheckOutputPastCapEndsWithItsOwnExitCodeAndMarksTruncated(t *testing.T) {
	e, s, _, _, _ := workspaceExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	setTaskChecksForTest(t, s, []store.CheckDefinition{
		{Name: "capped", Argv: checkHelperArgv("over_cap"), MaxOutputBytes: 1024},
	})
	e.LaunchAttempt("attempt")
	task := waitTaskState(t, s, store.AwaitingReview)
	results := task.Attempt.CheckResults()
	if len(results) != 1 || results[0].State != store.CheckFailed || *results[0].ExitCode != 5 || !results[0].Truncated || results[0].OutputSize != 1024 {
		t.Fatalf("result = %+v", results)
	}
	hash, size, trunc, err := s.CheckLog(context.Background(), "u", results[0].ID)
	if err != nil || size != 1024 || !trunc {
		t.Fatalf("log = %s %d %v %v", hash, size, trunc, err)
	}
	rc, err := e.cfg.Artifacts.Open(hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil || len(data) != 1024 || string(data) != strings.Repeat("A", 1024) {
		t.Fatalf("artifact len = %d, err = %v", len(data), err)
	}
}

func TestCheckBaselineTellsExistingFailureFromNewAndRerunsWhenDefinitionChanges(t *testing.T) {
	e, s, _, _, worktree := workspaceExecutor(t, "agent_edit")
	e.cfg.CancelGrace = naturalExitGrace
	countFile := filepath.Join(t.TempDir(), "baseline-count")
	setTaskChecksForTest(t, s, []store.CheckDefinition{
		{Name: "already-broken", Argv: checkHelperArgv("check_file", "a.txt", "never-present", countFile)},
		{Name: "broken-by-agent", Argv: checkHelperArgv("check_file", "a.txt", "unstaged", "")},
	})
	// Fake engine replaces a.txt so "unstaged" is removed (breaking check 1).
	e.cfg.Env = append(e.cfg.Env, "ADA_FAKE_EDIT_FILE="+filepath.Join(worktree, "a.txt"), "ADA_FAKE_EDIT_CONTENT=agent-replaced\n")
	e.LaunchAttempt("attempt")
	task := waitTaskState(t, s, store.AwaitingReview)
	results := task.Attempt.CheckResults()
	if len(results) != 2 {
		t.Fatalf("results = %+v", results)
	}
	if !results[0].BaselineFailure || results[0].NewFailure || results[0].Baseline == nil || results[0].Baseline.State != store.CheckFailed {
		t.Fatalf("check 0 = %+v, baseline = %+v", results[0], results[0].Baseline)
	}
	if !results[1].NewFailure || results[1].BaselineFailure || results[1].Baseline == nil || results[1].Baseline.State != store.CheckPassed {
		t.Fatalf("check 1 = %+v, baseline = %+v", results[1], results[1].Baseline)
	}
	// Baseline + attempt run = 2 invocations of check 0.
	if got := len(markerLines(t, countFile)); got != 2 {
		t.Fatalf("countFile lines = %d, want 2", got)
	}
}

func TestCheckEditingTrackedFileAndIgnoredFileIsolation(t *testing.T) {
	e, s, _, _, worktree := workspaceExecutor(t, "agent_edit_and_ignored")
	e.cfg.CancelGrace = naturalExitGrace
	setTaskChecksForTest(t, s, []store.CheckDefinition{
		{Name: "mutator", Argv: checkHelperArgv("edit_tracked", "a.txt")},
		{Name: "verify-clean", Argv: checkHelperArgv("assert_content", "a.txt", "agent-clean\n")},
		{Name: "verify-no-ignored", Argv: checkHelperArgv("assert_absent", "ignored.secret")},
	})
	e.LaunchAttempt("attempt")
	task := waitTaskState(t, s, store.AwaitingReview)
	results := task.Attempt.CheckResults()
	if len(results) != 3 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].State != store.CheckChangedTree || results[0].BoundCommit != "" {
		t.Fatalf("mutator result = %+v", results[0])
	}
	if results[1].State != store.CheckPassed || results[1].BoundCommit == "" {
		t.Fatalf("verify-clean result = %+v", results[1])
	}
	if results[2].State != store.CheckPassed || results[2].BoundCommit == "" {
		t.Fatalf("verify-no-ignored result = %+v", results[2])
	}
	// Attempt's own worktree still has "agent-clean\n" and the ignored file.
	if got, _ := os.ReadFile(filepath.Join(worktree, "a.txt")); string(got) != "agent-clean\n" {
		t.Fatalf("attempt worktree a.txt = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(worktree, "ignored.secret")); err != nil {
		t.Fatalf("attempt worktree ignored.secret missing: %v", err)
	}
}

func TestCheckEnvironmentAndNoRepoScriptAutoDiscovery(t *testing.T) {
	e, s, _, _, worktree := workspaceExecutor(t, "agent_repo_scripts")
	e.cfg.CancelGrace = naturalExitGrace
	scriptMarker := filepath.Join(t.TempDir(), "repo-script-ran")
	e.cfg.Env = append(e.cfg.Env, "ADA_FAKE_SCRIPT_MARKER="+scriptMarker)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	t.Setenv("ANTHROPIC_API_KEY", "secret-key")
	t.Setenv("FOO_TOKEN", "secret-token")
	t.Setenv("ADANEXT_SECRET", "secret-ada")
	t.Setenv("ADA_SECRET", "secret-ada2")
	t.Setenv("UNLISTED_CHECK_VAR", "unlisted")
	t.Setenv("NAMED_CHECK_VAR", "named-ok")
	setTaskChecksForTest(t, s, []store.CheckDefinition{
		{Name: "env", Argv: checkHelperArgv("dump_env"), Env: []string{"NAMED_CHECK_VAR"}},
	})
	e.LaunchAttempt("attempt")
	task := waitTaskState(t, s, store.AwaitingReview)
	if _, err := os.Stat(scriptMarker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("repository script ran without declaration: %v", err)
	}
	_ = worktree
	results := task.Attempt.CheckResults()
	hash, _, _, err := s.CheckLog(context.Background(), "u", results[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := e.cfg.Artifacts.Open(hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	out, _ := io.ReadAll(rc)
	logStr := string(out)
	if !strings.Contains(logStr, "NAMED_CHECK_VAR=named-ok") {
		t.Fatalf("named variable missing from check env:\n%s", logStr)
	}
	for _, forbidden := range []string{"CLAUDE_CONFIG_DIR=", "CODEX_HOME=", "ANTHROPIC_API_KEY=", "FOO_TOKEN=", "ADANEXT_", "ADA_", "UNLISTED_CHECK_VAR="} {
		for _, line := range strings.Split(logStr, "\n") {
			if strings.HasPrefix(line, forbidden) {
				t.Fatalf("forbidden variable in check env: %s", line)
			}
		}
	}
}

func TestDaemonizingCheckLeavesNothingRunningWhenItEnds(t *testing.T) {
	e, s, _, _, _ := workspaceExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	daemonMarker := filepath.Join(t.TempDir(), "daemon-marker")
	cleanupGrandchild(t, daemonMarker)
	setTaskChecksForTest(t, s, []store.CheckDefinition{
		{Name: "daemon", Argv: checkHelperArgv("daemonize", daemonMarker)},
	})
	e.LaunchAttempt("attempt")
	task := waitTaskState(t, s, store.AwaitingReview)
	results := task.Attempt.CheckResults()
	if len(results) != 1 || results[0].State != store.CheckPassed {
		t.Fatalf("results = %+v", results)
	}
	lines := markerLines(t, daemonMarker)
	count := 0
	for _, line := range lines {
		var pid int
		var id string
		if _, err := fmt.Sscanf(line, "child:%d:%s", &pid, &id); err == nil && pid > 0 {
			count++
			if err := waitProcessGone(pid, id, 0); err != nil {
				t.Fatalf("daemonized process %d still running after check ended: %v", pid, err)
			}
		}
	}
	if count == 0 {
		t.Fatalf("no daemon child recorded in %v", lines)
	}
}

func TestAgentTamperingGoesToRecoveryAndCreatesNoMarker(t *testing.T) {
	for _, tc := range []string{"tamper_hookspath", "tamper_fsmonitor", "tamper_gpg", "tamper_gitfile", "tamper_reset_below"} {
		t.Run(tc, func(t *testing.T) {
			e, s, _, _, _ := workspaceExecutor(t, tc)
			e.cfg.CancelGrace = naturalExitGrace
			hookMarker := filepath.Join(t.TempDir(), "hook-ran")
			e.cfg.Env = append(e.cfg.Env, "ADA_FAKE_HOOK_MARKER="+hookMarker)
			setTaskChecksForTest(t, s, []store.CheckDefinition{
				{Name: "should-not-run", Argv: checkHelperArgv("pass")},
			})
			e.LaunchAttempt("attempt")
			waitAttemptPrepared(t, s, "recovery_required")
			task, err := s.Task(context.Background(), "u", "task")
			if err != nil || task.State != store.RecoveryRequired {
				t.Fatalf("task = %+v, %v", task, err)
			}
			if _, err := os.Stat(hookMarker); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("tampered git config/hook executed: %v", err)
			}
			if results := task.Attempt.CheckResults(); len(results) != 0 {
				t.Fatalf("checks ran on tampered workspace: %+v", results)
			}
		})
	}
}

func TestStopDuringVerifyingKillsRunningCheckTreeAndMarksRemainingNotRun(t *testing.T) {
	e, s, _, _, _ := workspaceExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	childMarker := filepath.Join(t.TempDir(), "stop-verifying-child")
	cleanupGrandchild(t, childMarker)
	setTaskChecksForTest(t, s, []store.CheckDefinition{
		{Name: "running-check", Argv: checkHelperArgv("timeout_tree", childMarker)},
		{Name: "later-check", Argv: checkHelperArgv("pass")},
	})
	// Wait until the attempt check (not baseline) has spawned its child, or stop once in Verifying.
	e.LaunchAttempt("attempt")
	waitTaskState(t, s, store.Verifying)
	waitGrandchildMarker(t, childMarker)
	ctx, cancel := context.WithTimeout(context.Background(), preparationBound+e.StopBudget())
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	for _, line := range markerLines(t, childMarker) {
		var pid int
		var id string
		if _, err := fmt.Sscanf(line, "child:%d:%s", &pid, &id); err == nil && pid > 0 {
			if err := waitProcessGone(pid, id, 0); err != nil {
				t.Fatalf("check child still running after Stop: %v", err)
			}
		}
	}
	task, err := s.Task(context.Background(), "u", "task")
	if err != nil || task.State != store.AwaitingReview {
		t.Fatalf("task = %+v, %v", task, err)
	}
	results := task.Attempt.CheckResults()
	if len(results) != 2 || results[0].State != store.CheckKilled || results[1].State != store.CheckNotRun {
		t.Fatalf("results = %+v", results)
	}
}

func TestAgentCommitIsKeptBeneathAdasCommitAndBranchMoveInvalidatesResult(t *testing.T) {
	e, s, _, _, worktree := workspaceExecutor(t, "agent_own_commit")
	e.cfg.CancelGrace = naturalExitGrace
	setTaskChecksForTest(t, s, []store.CheckDefinition{
		{Name: "verify-both", Argv: checkHelperArgv("assert_content", "agent.txt", "from agent commit\n")},
	})
	e.LaunchAttempt("attempt")
	task := waitTaskState(t, s, store.AwaitingReview)
	if got := fixtureGit(t, worktree, "log", "-2", "--format=%s <%ae>"); got != "Agent changes <ada@adanext.invalid>\nagent commit <agent@example.com>" {
		t.Fatalf("commit log =\n%s", got)
	}
	results := task.Attempt.CheckResults()
	if len(results) != 1 || results[0].State != store.CheckPassed || results[0].Invalidated {
		t.Fatalf("results before edit = %+v", results)
	}
	// A later commit on ada/task invalidates the bound check result.
	writeFile(t, filepath.Join(worktree, "later.txt"), "edit\n")
	fixtureGit(t, worktree, "add", "later.txt")
	fixtureGit(t, worktree, "commit", "-q", "-m", "later edit")
	task, err := s.Task(context.Background(), "u", "task")
	if err != nil {
		t.Fatal(err)
	}
	results = task.Attempt.CheckResults()
	if len(results) != 1 || !results[0].Invalidated {
		t.Fatalf("results after edit = %+v", results)
	}
}

func TestCheckWorktreeAndAdminDirAreRemovedAndCrashFinishesOnNextTry(t *testing.T) {
	e, s, _, _, _ := workspaceExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	leftoverWT := e.cfg.Workspaces.CheckWorktreePath("p", "oldattempt", "crashedrun")
	leftoverAdmin := filepath.Join(e.cfg.Workspaces.Root, "p", "repo.git", "worktrees", "crashedrun")
	e.beforeCheck = func(string) {
		// Seed a leftover check worktree and admin directory from a prior crash once the clone exists.
		if err := os.MkdirAll(leftoverWT, 0o700); err != nil {
			t.Error(err)
		}
		if err := os.MkdirAll(leftoverAdmin, 0o700); err != nil {
			t.Error(err)
		}
		if err := s.RecordCheckWorktree(context.Background(), store.CheckWorktreeLease{
			Project: "p", Attempt: "oldattempt", RunID: "crashedrun", AdminName: "crashedrun",
		}); err != nil {
			t.Error(err)
		}
	}
	setTaskChecksForTest(t, s, []store.CheckDefinition{
		{Name: "pass", Argv: checkHelperArgv("pass")},
	})
	e.LaunchAttempt("attempt")
	task := waitTaskState(t, s, store.AwaitingReview)
	results := task.Attempt.CheckResults()
	if len(results) != 1 || results[0].State != store.CheckPassed {
		t.Fatalf("results = %+v", results)
	}
	runID := results[0].ID
	for _, p := range []string{
		e.cfg.Workspaces.CheckWorktreePath("p", "attempt", runID),
		filepath.Join(e.cfg.Workspaces.Root, "p", "repo.git", "worktrees", runID),
		leftoverWT,
		leftoverAdmin,
	} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s still exists after check: %v", p, err)
		}
	}
}

func TestCheckWorkdirLinkToOutsideIsRefused(t *testing.T) {
	outside := t.TempDir()
	base := t.TempDir()
	link := filepath.Join(base, "linked")
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v: %s", err, out)
		}
	} else {
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := source.ResolveWorkdirNoFollow(base, "linked"); err == nil {
		t.Fatal("ResolveWorkdirNoFollow accepted a link/junction to outside")
	}

	e, s, _, _, worktree := workspaceExecutor(t, "agent_link_workdir")
	e.cfg.CancelGrace = naturalExitGrace
	e.cfg.Env = append(e.cfg.Env, "ADA_FAKE_OUTSIDE_DIR="+outside)
	_ = worktree
	setTaskChecksForTest(t, s, []store.CheckDefinition{
		{Name: "linked-dir", Workdir: "linked", Argv: checkHelperArgv("pass")},
	})
	e.LaunchAttempt("attempt")
	task := waitTaskState(t, s, store.AwaitingReview)
	results := task.Attempt.CheckResults()
	if len(results) != 1 || results[0].State != store.CheckFailed {
		t.Fatalf("expected linked workdir check to fail, got %+v", results)
	}
}
