package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

var (
	agentCommit = strings.Repeat("a", 40)
	otherCommit = strings.Repeat("b", 40)
)

// reviewTask is task rt of project rp, created by creator, whose attempt ra
// by actor ended in state with a workspace whose tested commit is agentCommit;
// owner is the project's owner, reviewer a member who is none of these.
func reviewTask(t *testing.T, s *Store, state State, attemptState string) {
	t.Helper()
	for _, u := range []string{"owner", "actor", "creator", "reviewer", "outsider"} {
		execute(t, s, "INSERT INTO users VALUES (?, ?, 'created')", u, u)
	}
	execute(t, s, `INSERT INTO projects VALUES ('rp', 'project', '/source', 'created')`)
	execute(t, s, `INSERT INTO project_members VALUES ('rp', 'owner', 'owner', 'added')`)
	for _, u := range []string{"actor", "creator", "reviewer"} {
		execute(t, s, "INSERT INTO project_members VALUES ('rp', ?, 'member', 'added')", u)
	}
	execute(t, s, `INSERT INTO project_sources VALUES ('rp','/source','/source/.git','f','now')`)
	execute(t, s, `INSERT INTO artifacts VALUES (?,0,'test','now')`, hashOf('e'))
	execute(t, s, `INSERT INTO source_snapshots(id,project,base_commit,fingerprint,staged_patch,unstaged_patch,created_by,created) VALUES ('rs','rp',?,'f',?,?,'owner','now')`, strings.Repeat("c", 40), hashOf('e'), hashOf('e'))
	execute(t, s, `INSERT INTO tasks(id,project,created_by,objective,acceptance_criteria,state,revision,created,updated,snapshot) VALUES ('rt','rp','creator','objective','criteria',?,4,'now','now','rs')`, state)
	execute(t, s, `INSERT INTO attempts(id,task,actor,backend,model,effort,mode,executor,ownership_generation,engine_sha256,state,started,ended) VALUES ('ra','rt','actor','codex','','','auto','local',1,'hash',?,'now','now')`, attemptState)
	execute(t, s, `INSERT INTO workspaces(attempt,project,task,snapshot,base_commit,branch,path,manifest_version,manifest_hash,executor,policy,created,tested_commit,imported_commit)
		VALUES ('ra','rp','rt','rs',?,'ada/rt','/w','2',?,'local','auto','now',?,?)`, strings.Repeat("c", 40), hashOf('f'), agentCommit, strings.Repeat("d", 40))
	execute(t, s, `UPDATE attempts SET provider_changes='{"changes":[],"truncated":false}' WHERE id='ra'`)
	execute(t, s, `UPDATE coordinator SET generation=3`)
}

func accept(s *Store, user, commit string, generation int64, tip string) error {
	return s.write(ctx, user, func(tx *Tx) error {
		_, err := tx.Accept(ctx, "rt", 4, commit, generation, tip)
		return err
	})
}

func refine(s *Store, user string, acknowledged bool) error {
	return s.write(ctx, user, func(tx *Tx) error {
		_, err := tx.Refine(ctx, "rt", 4, Attempt{ID: "r2", Backend: "codex", Mode: "auto", Executor: "local", OwnershipGeneration: 3, EngineSHA256: "hash"}, acknowledged)
		return err
	})
}

func TestFindingsFromANonMemberAreRefused(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	err := s.write(ctx, "outsider", func(tx *Tx) error { _, err := tx.RecordFinding(ctx, "rt", "looks wrong"); return err })
	if !errors.Is(err, ErrNotMember) {
		t.Fatalf("a non-member's finding: %v", err)
	}
	if _, err := s.Findings(ctx, "outsider", "rt"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("a non-member read the findings: %v", err)
	}
	// Any member records findings, on the attempt the task shows.
	err = s.write(ctx, "reviewer", func(tx *Tx) error { _, err := tx.RecordFinding(ctx, "rt", "add a test"); return err })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, "reviewer", func(tx *Tx) error { _, err := tx.RecordFinding(ctx, "rt", ""); return err }); !errors.Is(err, ErrInvalidFinding) {
		t.Fatalf("an empty finding: %v", err)
	}
	got, err := s.Findings(ctx, "creator", "rt")
	if err != nil || len(got) != 1 || got[0].Attempt != "ra" || got[0].Author != "reviewer" || got[0].Body != "add a test" {
		t.Fatalf("findings = %+v, %v", got, err)
	}
}

func TestAnAcceptByTheCreatorOrAReviewerWhoIsNeitherTheActorNorAnOwnerIsRefused(t *testing.T) {
	for _, user := range []string{"creator", "reviewer"} {
		t.Run(user, func(t *testing.T) {
			s := openTestStore(t)
			reviewTask(t, s, AwaitingReview, "ended_ok")
			if err := accept(s, user, agentCommit, 3, agentCommit); !errors.Is(err, ErrNotAuthorized) {
				t.Fatalf("accept by the %s: %v", user, err)
			}
		})
	}
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	if err := accept(s, "outsider", agentCommit, 3, agentCommit); !errors.Is(err, ErrNotMember) {
		t.Fatalf("accept by a non-member: %v", err)
	}
}

func TestARefineByTheCreatorOrAReviewerWhoIsNeitherTheActorNorAnOwnerIsRefused(t *testing.T) {
	for _, user := range []string{"creator", "reviewer"} {
		t.Run(user, func(t *testing.T) {
			s := openTestStore(t)
			reviewTask(t, s, AwaitingReview, "ended_ok")
			execute(t, s, "UPDATE workspaces SET removed='now'")
			if err := refine(s, user, false); !errors.Is(err, ErrNotAuthorized) {
				t.Fatalf("refine by the %s: %v", user, err)
			}
		})
	}
}

func TestAnAcceptAgainstAnOlderCommitOrGenerationIsRefused(t *testing.T) {
	for name, c := range map[string]struct {
		commit, tip string
		generation  int64
	}{
		"an older commit":          {otherCommit, agentCommit, 3},
		"a branch moved since":     {agentCommit, otherCommit, 3},
		"an unknown tip":           {agentCommit, "", 3},
		"an older generation":      {agentCommit, agentCommit, 2},
		"a generation not reached": {agentCommit, agentCommit, 4},
	} {
		t.Run(name, func(t *testing.T) {
			s := openTestStore(t)
			reviewTask(t, s, AwaitingReview, "ended_ok")
			if err := accept(s, "actor", c.commit, c.generation, c.tip); !errors.Is(err, ErrStaleReview) {
				t.Fatalf("accept: %v", err)
			}
			if task, _ := s.Task(ctx, "owner", "rt"); task.State != AwaitingReview {
				t.Fatalf("task = %s", task.State)
			}
		})
	}
}

func TestAnAcceptRecordsWhatWasReviewedAndMovesTheTaskToAccepted(t *testing.T) {
	for _, user := range []string{"actor", "owner"} {
		t.Run(user, func(t *testing.T) {
			s := openTestStore(t)
			reviewTask(t, s, AwaitingReview, "ended_ok")
			code := 0
			if err := s.write(ctx, "owner", func(tx *Tx) error {
				return tx.RecordCheckResult(ctx, CheckResult{ID: "chk", Task: "rt", Attempt: "ra", Definition: CheckDefinition{Argv: []string{"x"}}, State: CheckPassed, ExitCode: &code, TestedCommit: agentCommit, BoundCommit: agentCommit})
			}); err != nil {
				t.Fatal(err)
			}
			var review Review
			if err := s.write(ctx, user, func(tx *Tx) error {
				var err error
				review, err = tx.Accept(ctx, "rt", 4, agentCommit, 3, agentCommit)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if review.Reviewer != user || review.Commit != agentCommit || review.Generation != 3 || len(review.Checks) != 1 || review.Checks[0].ID != "chk" || review.Checks[0].Invalidated {
				t.Fatalf("review = %+v", review)
			}
			var stored, checks string
			var generation int64
			if err := s.db.QueryRow("SELECT reviewed_commit,ownership_generation,checks_json FROM reviews WHERE task='rt'").Scan(&stored, &generation, &checks); err != nil {
				t.Fatal(err)
			}
			var recorded []CheckResult
			if err := json.Unmarshal([]byte(checks), &recorded); err != nil || stored != agentCommit || generation != 3 || len(recorded) != 1 || recorded[0].BoundCommit != agentCommit {
				t.Fatalf("stored review %s %d %s %v", stored, generation, checks, err)
			}
			task, err := s.Task(ctx, "owner", "rt")
			if err != nil || task.State != Accepted || task.Revision != 5 {
				t.Fatalf("task = %+v %v", task, err)
			}
			// An accepted task is neither accepted again nor refined.
			if err := s.write(ctx, user, func(tx *Tx) error { _, err := tx.Accept(ctx, "rt", 5, agentCommit, 3, agentCommit); return err }); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("a second accept: %v", err)
			}
			execute(t, s, "UPDATE workspaces SET removed='now'")
			if err := s.write(ctx, user, func(tx *Tx) error {
				_, err := tx.Refine(ctx, "rt", 5, Attempt{ID: "r2", Backend: "codex", Mode: "auto", Executor: "local", OwnershipGeneration: 3, EngineSHA256: "hash"}, true)
				return err
			}); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("refine of an accepted task: %v", err)
			}
		})
	}
}

func TestRefineIsRefusedWhileAnAttemptIsNotEndedOrNeedsRecovery(t *testing.T) {
	for attemptState, want := range map[string]error{"pending": ErrAttemptActive, "launching": ErrAttemptActive, "running": ErrAttemptActive, "recovery_required": ErrAttemptRecovery} {
		t.Run(attemptState, func(t *testing.T) {
			s := openTestStore(t)
			reviewTask(t, s, Failed, "ended_failed")
			execute(t, s, "UPDATE workspaces SET removed='now'")
			// An older attempt of the same task in that state.
			execute(t, s, `INSERT INTO attempts(id,task,actor,backend,model,effort,mode,executor,ownership_generation,engine_sha256,state,started) VALUES ('old','rt','actor','codex','','','auto','local',1,'hash',?,'now')`, attemptState)
			if err := refine(s, "actor", true); !errors.Is(err, want) {
				t.Fatalf("refine: %v, want %v", err, want)
			}
		})
	}
	s := openTestStore(t)
	reviewTask(t, s, Failed, "ended_failed")
	if err := refine(s, "actor", true); !errors.Is(err, ErrWorktreeNotRemoved) {
		t.Fatalf("refine before the earlier worktree's removal: %v", err)
	}
}

func TestRefineFromAwaitingReviewFailedAndCanceledStartsAnAttemptOfTheRequester(t *testing.T) {
	for _, c := range []struct {
		state        State
		attemptState string
	}{{AwaitingReview, "ended_ok"}, {Failed, "ended_failed"}, {Canceled, "canceled"}} {
		t.Run(string(c.state), func(t *testing.T) {
			s := openTestStore(t)
			reviewTask(t, s, c.state, c.attemptState)
			execute(t, s, "UPDATE workspaces SET removed='now'")
			// The owner refines the actor's attempt: the new attempt is the owner's.
			if err := refine(s, "owner", false); err != nil {
				t.Fatal(err)
			}
			a, err := s.AttemptByID(ctx, "r2")
			if err != nil || a.Actor != "owner" || a.State != "pending" {
				t.Fatalf("attempt = %+v %v", a, err)
			}
			var refines string
			if err := s.db.QueryRow("SELECT refines FROM attempts WHERE id='r2'").Scan(&refines); err != nil || refines != "ra" {
				t.Fatalf("refines %q %v", refines, err)
			}
			task, _ := s.Task(ctx, "owner", "rt")
			if task.State != Preparing || task.Revision != 5 || task.Attempt.ID != "r2" {
				t.Fatalf("task = %+v", task)
			}
			cont, ok, err := s.Continuation(ctx, "r2")
			if err != nil || !ok || cont.Tip != agentCommit || cont.ImportedCommit != strings.Repeat("d", 40) || cont.From != "ra" {
				t.Fatalf("continuation = %+v %v %v", cont, ok, err)
			}
		})
	}
}

func TestRefineOfAnAttemptWithProviderChangesNeedsThemAcknowledged(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	execute(t, s, "UPDATE workspaces SET removed='now'")
	execute(t, s, `UPDATE attempts SET provider_changes='{"changes":[{"path":".claude/settings.json","change":"added"}],"truncated":false}' WHERE id='ra'`)
	err := refine(s, "actor", false)
	var flagged *ProviderUnacknowledgedError
	if !errors.As(err, &flagged) || len(flagged.Changes.Changes) != 1 || flagged.Changes.Changes[0].Path != ".claude/settings.json" {
		t.Fatalf("unacknowledged refine: %v", err)
	}
	if err := refine(s, "actor", true); err != nil {
		t.Fatal(err)
	}
}

func TestARefinedAttemptsPromptCarriesTheFindings(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	for _, body := range []string{"rename the flag", "add a test\nfor the edge"} {
		if err := s.write(ctx, "reviewer", func(tx *Tx) error { _, err := tx.RecordFinding(ctx, "rt", body); return err }); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, s, "UPDATE workspaces SET removed='now'")
	if err := refine(s, "actor", false); err != nil {
		t.Fatal(err)
	}
	first, err := s.AttemptPrompt(ctx, "ra")
	if err != nil || first != "objective\n\nAcceptance criteria:\ncriteria" {
		t.Fatalf("first prompt %q %v", first, err)
	}
	prompt, err := s.AttemptPrompt(ctx, "r2")
	if err != nil || !strings.HasPrefix(prompt, first+"\n\n") || !strings.Contains(prompt, "- rename the flag\n") || !strings.Contains(prompt, "- add a test\n  for the edge") {
		t.Fatalf("refined prompt %q %v", prompt, err)
	}
}

func TestAnEndedAttemptWhoseChangesCannotBeCommittedMovesToRecovery(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, Failed, "ended_failed")
	if err := s.RecoverEndedAttempt(ctx, "ra", "workspace verification failed", "the clone config differs"); err != nil {
		t.Fatal(err)
	}
	a, _ := s.AttemptByID(ctx, "ra")
	events, _ := s.Events(ctx, "owner", "ra", 0)
	if a.State != "recovery_required" || len(events) != 1 || events[0].Kind != "recovery" || !strings.Contains(string(events[0].Payload), "clone config") {
		t.Fatalf("attempt %+v events %+v", a, events)
	}
	if err := refine(s, "actor", true); !errors.Is(err, ErrAttemptRecovery) {
		t.Fatalf("refine: %v", err)
	}
	// An attempt that is not ended is left alone.
	execute(t, s, "UPDATE attempts SET state='running' WHERE id='ra'")
	if err := s.RecoverEndedAttempt(ctx, "ra", "r", "m"); !errors.Is(err, ErrAttemptActive) {
		t.Fatalf("recover a running attempt: %v", err)
	}
}

func TestAnAgentCommitIsRecordedOnceAndARemovalIsRecorded(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, Failed, "ended_failed")
	execute(t, s, "UPDATE workspaces SET tested_commit=''")
	if err := s.RecordAgentCommit(ctx, "ra", otherCommit); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAgentCommit(ctx, "ra", agentCommit); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWorktreeRemoved(ctx, "ra"); err != nil {
		t.Fatal(err)
	}
	rs, err := s.ReviewState(ctx, "reviewer", "rt")
	if err != nil || len(rs.Attempts) != 1 || rs.Attempts[0].TestedCommit != otherCommit || !rs.Attempts[0].Removed || rs.Owner {
		t.Fatalf("review state %+v %v", rs, err)
	}
	if _, err := s.ReviewState(ctx, "outsider", "rt"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("a non-member's review state: %v", err)
	}
}

func TestAFindingPastItsSizeOrTheTasksCountIsRefused(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	add := func(body string) error {
		return s.write(ctx, "reviewer", func(tx *Tx) error { _, err := tx.RecordFinding(ctx, "rt", body); return err })
	}
	if err := add(strings.Repeat("x", MaxFindingBytes+1)); !errors.Is(err, ErrInvalidFinding) {
		t.Fatalf("a finding of %d bytes: %v", MaxFindingBytes+1, err)
	}
	if err := add(strings.Repeat("x", MaxFindingBytes)); err != nil {
		t.Fatalf("a finding of %d bytes: %v", MaxFindingBytes, err)
	}
	for i := 1; i < MaxFindingsPerTask; i++ {
		if err := add("note"); err != nil {
			t.Fatal(err)
		}
	}
	if err := add("one more"); !errors.Is(err, ErrTooManyFindings) {
		t.Fatalf("finding %d: %v", MaxFindingsPerTask+1, err)
	}
}

func TestRefineReturnsTheFindingsItCarries(t *testing.T) {
	s := openTestStore(t)
	reviewTask(t, s, AwaitingReview, "ended_ok")
	var ids []string
	for _, body := range []string{"one", "two"} {
		if err := s.write(ctx, "reviewer", func(tx *Tx) error {
			f, err := tx.RecordFinding(ctx, "rt", body)
			ids = append(ids, f.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, s, "UPDATE workspaces SET removed='now'")
	var carried []string
	if err := s.write(ctx, "actor", func(tx *Tx) error {
		var err error
		carried, err = tx.Refine(ctx, "rt", 4, Attempt{ID: "r2", Backend: "codex", Mode: "auto", Executor: "local", OwnershipGeneration: 3, EngineSHA256: "hash"}, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(carried, ",") != strings.Join(ids, ",") {
		t.Fatalf("carried %v, recorded %v", carried, ids)
	}
}
