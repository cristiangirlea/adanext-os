package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckDefinitionsValidateLimitsAndEnvironment(t *testing.T) {
	var seventeen []CheckDefinition
	for range 17 {
		seventeen = append(seventeen, CheckDefinition{Argv: []string{"true"}})
	}
	if _, err := NormalizeCheckDefinitions(seventeen); !errors.Is(err, ErrInvalidCheck) {
		t.Fatalf("17 checks = %v", err)
	}
	var sixteen []CheckDefinition
	for range 16 {
		sixteen = append(sixteen, CheckDefinition{Argv: []string{"true"}})
	}
	if got, err := NormalizeCheckDefinitions(sixteen); err != nil || len(got) != 16 {
		t.Fatalf("16 checks = %d, %v", len(got), err)
	}

	invalid := []CheckDefinition{
		{Argv: nil},
		{Argv: []string{""}},
		{Argv: []string{"true"}, Timeout: "30m1s"},
		{Argv: []string{"true"}, Timeout: "0s"},
		{Argv: []string{"true"}, Timeout: "-1s"},
		{Argv: []string{"true"}, MaxOutputBytes: (16 << 20) + 1},
		{Argv: []string{"true"}, MaxOutputBytes: -1},
		{Argv: []string{"true"}, Workdir: "../outside"},
		{Argv: []string{"true"}, Workdir: "/abs"},
		{Argv: []string{"true"}, Workdir: ".git/hooks"},
		{Argv: []string{"true"}, Env: []string{"ANTHROPIC_API_KEY"}},
		{Argv: []string{"true"}, Env: []string{"FOO_TOKEN"}},
		{Argv: []string{"true"}, Env: []string{"ADANEXT_STATE_DIR"}},
		{Argv: []string{"true"}, Env: []string{"ADA_SECRET"}},
		{Argv: []string{"true"}, Env: []string{"CLAUDE_CONFIG_DIR"}},
		{Argv: []string{"true"}, Env: []string{"CODEX_HOME"}},
		{Argv: []string{"true"}, Env: []string{"BAD=NAME"}},
	}
	for _, c := range invalid {
		if _, err := NormalizeCheckDefinitions([]CheckDefinition{c}); !errors.Is(err, ErrInvalidCheck) {
			t.Errorf("invalid check %+v accepted: %v", c, err)
		}
	}

	got, err := NormalizeCheckDefinitions([]CheckDefinition{{
		Name: "unit",
		Argv: []string{"go", "test"},
		Env:  []string{"CUSTOM_CHECK_VAR", "CUSTOM_CHECK_VAR"},
	}})
	if err != nil || len(got) != 1 || got[0].Workdir != "." || got[0].Timeout != "30m0s" || got[0].MaxOutputBytes != 16<<20 || len(got[0].Env) != 1 || got[0].Env[0] != "CUSTOM_CHECK_VAR" {
		t.Fatalf("normalized = %+v, %v", got, err)
	}
}

func TestCheckResultsTrackBaselineInvalidationAndLogMembership(t *testing.T) {
	s := openTestStore(t)
	boundTask(t, s)
	ctx := context.Background()
	if err := s.AddMember(ctx, "p1", "bob", "member"); err != nil {
		t.Fatal(err)
	}
	defs := []CheckDefinition{
		{Name: "existing-fail", Argv: []string{"check", "1"}},
		{Name: "new-fail", Argv: []string{"check", "2"}},
		{Name: "mutator", Argv: []string{"check", "3"}},
	}
	execute(t, s, "UPDATE tasks SET state='draft' WHERE id='t1'")
	if err := s.write(ctx, "alice", func(tx *Tx) error {
		return tx.SetTaskChecks(ctx, "t1", 0, defs)
	}); err != nil {
		t.Fatal(err)
	}
	normalized, err := s.TaskChecks(ctx, "t1")
	if err != nil || len(normalized) != 3 {
		t.Fatalf("task checks = %+v, %v", normalized, err)
	}
	ws := testWorkspace()
	ws.TaskRevision = 1
	if err := s.write(ctx, "alice", func(tx *Tx) error {
		if _, err := tx.RecordWorkspace(ctx, ws); err != nil {
			return err
		}
		return tx.RecordTestedCommit(ctx, "a1", strings.Repeat("b", 40))
	}); err != nil {
		t.Fatal(err)
	}
	code0, code1 := 0, 1
	logHash := strings.Repeat("c", 64)
	if err := s.write(ctx, "alice", func(tx *Tx) error {
		if err := tx.ReferenceArtifact(ctx, logHash, 5, "check:r1"); err != nil {
			return err
		}
		if err := tx.RecordCheckBaseline(ctx, CheckBaseline{
			ID:             "b1",
			Project:        "p1",
			Snapshot:       "s1",
			DefinitionHash: CheckDefinitionHash(normalized[0]),
			State:          CheckFailed,
			ExitCode:       &code1,
			TestedCommit:   strings.Repeat("a", 40),
			BoundCommit:    strings.Repeat("a", 40),
			OutputArtifact: logHash,
			OutputSize:     5,
		}); err != nil {
			return err
		}
		if err := tx.RecordCheckBaseline(ctx, CheckBaseline{
			ID:             "b2",
			Project:        "p1",
			Snapshot:       "s1",
			DefinitionHash: CheckDefinitionHash(normalized[1]),
			State:          CheckPassed,
			ExitCode:       &code0,
			TestedCommit:   strings.Repeat("a", 40),
			BoundCommit:    strings.Repeat("a", 40),
		}); err != nil {
			return err
		}
		if err := tx.RecordCheckResult(ctx, CheckResult{
			ID:             "r1",
			Task:           "t1",
			Attempt:        "a1",
			Ord:            0,
			Name:           normalized[0].Name,
			DefinitionHash: CheckDefinitionHash(normalized[0]),
			Definition:     normalized[0],
			State:          CheckFailed,
			ExitCode:       &code1,
			TestedCommit:   strings.Repeat("b", 40),
			BoundCommit:    strings.Repeat("b", 40),
			OutputArtifact: logHash,
			OutputSize:     5,
			Truncated:      true,
			BaselineID:     "b1",
			EnvKeys:        []string{"PATH", "CUSTOM"},
			DurationMS:     42,
		}); err != nil {
			return err
		}
		if err := tx.RecordCheckResult(ctx, CheckResult{
			ID:             "r2",
			Task:           "t1",
			Attempt:        "a1",
			Ord:            1,
			Name:           normalized[1].Name,
			DefinitionHash: CheckDefinitionHash(normalized[1]),
			Definition:     normalized[1],
			State:          CheckFailed,
			ExitCode:       &code1,
			TestedCommit:   strings.Repeat("b", 40),
			BoundCommit:    strings.Repeat("b", 40),
			BaselineID:     "b2",
		}); err != nil {
			return err
		}
		return tx.RecordCheckResult(ctx, CheckResult{
			ID:             "r3",
			Task:           "t1",
			Attempt:        "a1",
			Ord:            2,
			Name:           normalized[2].Name,
			DefinitionHash: CheckDefinitionHash(normalized[2]),
			Definition:     normalized[2],
			State:          CheckChangedTree,
			ExitCode:       &code0,
			TestedCommit:   strings.Repeat("b", 40),
			BoundCommit:    "",
		})
	}); err != nil {
		t.Fatal(err)
	}

	tip := strings.Repeat("b", 40)
	tips := func(project, task string) string {
		if project != "p1" || task != "t1" {
			t.Errorf("tip asked for %s/%s", project, task)
		}
		return tip
	}
	results, err := s.CheckResults(ctx, "alice", "t1", tips)
	if err != nil || len(results) != 3 {
		t.Fatalf("results = %+v, %v", results, err)
	}
	if !results[0].BaselineFailure || results[0].NewFailure || results[0].Invalidated || !results[0].Truncated || strings.Join(results[0].EnvKeys, ",") != "PATH,CUSTOM" || results[0].DurationMS != 42 {
		t.Fatalf("result 0 = %+v", results[0])
	}
	if !results[1].NewFailure || results[1].BaselineFailure || results[1].Invalidated {
		t.Fatalf("result 1 = %+v", results[1])
	}
	if results[2].BoundCommit != "" || results[2].State != CheckChangedTree || !results[2].Invalidated {
		t.Fatalf("result 2 = %+v", results[2])
	}

	// A tip that moved, or one that cannot be read, invalidates every result;
	// the store reads no ref itself.
	for _, tip = range []string{strings.Repeat("d", 40), ""} {
		results, err = s.CheckResults(ctx, "alice", "t1", tips)
		if err != nil || !results[0].Invalidated || !results[1].Invalidated || !results[2].Invalidated {
			t.Fatalf("at tip %q = %+v, %v", tip, results, err)
		}
	}

	// Member can read check log; after membership removal, gets not member/not found.
	if hash, size, trunc, err := s.CheckLog(ctx, "bob", "", "r1"); err != nil || hash != logHash || size != 5 || !trunc {
		t.Fatalf("bob log = %s %d %v %v", hash, size, trunc, err)
	}
	// Through a task, only that task's runs and the baselines they were compared with.
	if _, _, _, err := s.CheckLog(ctx, "bob", "t1", "r1"); err != nil {
		t.Fatalf("bob log through t1 = %v", err)
	}
	if _, _, _, err := s.CheckLog(ctx, "bob", "t1", "b1"); err != nil {
		t.Fatalf("bob baseline log through t1 = %v", err)
	}
	for _, run := range []string{"r1", "b1"} {
		if _, _, _, err := s.CheckLog(ctx, "bob", "t2", run); !errors.Is(err, ErrNotFound) {
			t.Fatalf("bob log of %s through another task = %v", run, err)
		}
	}
	if err := s.RemoveMember(ctx, "p1", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.CheckLog(ctx, "bob", "", "r1"); !errors.Is(err, ErrNotMember) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed bob log = %v", err)
	}
}

func TestCheckRestartDuringVerifyingMarksTheRunningCheckInterruptedAndTheRestNotRun(t *testing.T) {
	s := openTestStore(t)
	boundTask(t, s)
	ctx := context.Background()
	defs := []CheckDefinition{
		{Name: "first", Argv: []string{"check", "1"}},
		{Name: "second", Argv: []string{"check", "2"}},
		{Name: "third", Argv: []string{"check", "3"}},
	}
	execute(t, s, "UPDATE tasks SET state='draft' WHERE id='t1'")
	if err := s.write(ctx, "alice", func(tx *Tx) error {
		return tx.SetTaskChecks(ctx, "t1", 0, defs)
	}); err != nil {
		t.Fatal(err)
	}
	normalized, _ := s.TaskChecks(ctx, "t1")
	code0 := 0
	if err := s.write(ctx, "alice", func(tx *Tx) error {
		if err := tx.RecordCheckResult(ctx, CheckResult{
			ID:             "r1",
			Task:           "t1",
			Attempt:        "a1",
			Ord:            0,
			Name:           "first",
			DefinitionHash: CheckDefinitionHash(normalized[0]),
			Definition:     normalized[0],
			State:          CheckPassed,
			ExitCode:       &code0,
			TestedCommit:   strings.Repeat("b", 40),
			BoundCommit:    strings.Repeat("b", 40),
		}); err != nil {
			return err
		}
		return tx.RecordCheckResult(ctx, CheckResult{
			ID:             "r2",
			Task:           "t1",
			Attempt:        "a1",
			Ord:            1,
			Name:           "second",
			DefinitionHash: CheckDefinitionHash(normalized[1]),
			Definition:     normalized[1],
			State:          CheckRunning,
			TestedCommit:   strings.Repeat("b", 40),
		})
	}); err != nil {
		t.Fatal(err)
	}
	execute(t, s, "UPDATE attempts SET state='ended_ok',ended='now',ownership_generation=0 WHERE id='a1'")
	execute(t, s, "UPDATE tasks SET state='verifying',revision=4 WHERE id='t1'")

	if _, _, err := s.BumpAndRecover(ctx); err != nil {
		t.Fatal(err)
	}
	task, err := s.Task(ctx, "alice", "t1")
	if err != nil || task.State != AwaitingReview {
		t.Fatalf("task after restart = %+v, %v", task, err)
	}
	results, err := s.CheckResults(ctx, "alice", "t1", nil)
	if err != nil || len(results) != 3 {
		t.Fatalf("results = %+v, %v", results, err)
	}
	if results[0].State != CheckPassed || results[1].State != CheckInterrupted || results[2].State != CheckNotRun {
		t.Fatalf("states = %s, %s, %s", results[0].State, results[1].State, results[2].State)
	}
}

func TestTaskChecksCanBeSetOnlyBeforeTheTaskStarts(t *testing.T) {
	s := openTestStore(t)
	boundTask(t, s)
	ctx := context.Background()
	defs := []CheckDefinition{{Name: "unit", Argv: []string{"check"}}}
	for _, state := range []State{Draft, Ready} {
		execute(t, s, "UPDATE tasks SET state='"+string(state)+"',revision=0 WHERE id='t1'")
		if err := s.write(ctx, "alice", func(tx *Tx) error { return tx.SetTaskChecks(ctx, "t1", 0, defs) }); err != nil {
			t.Errorf("checks refused in %s: %v", state, err)
		}
	}
	for _, state := range []State{Preparing, Running, Verifying, AwaitingReview, Accepted, Refinement, Failed, CancelRequested, Canceled, Interrupted, RecoveryRequired} {
		execute(t, s, "UPDATE tasks SET state='"+string(state)+"',revision=0 WHERE id='t1'")
		if err := s.write(ctx, "alice", func(tx *Tx) error { return tx.SetTaskChecks(ctx, "t1", 0, defs) }); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("checks set in %s: %v", state, err)
		}
	}
}
