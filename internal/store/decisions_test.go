package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// seedDecision leaves attempt a pending in mode auto under project p's readonly default, so it
// waits for a decision, with v a member and w an owner of p beside its actor u.
func seedDecision(t *testing.T, s *Store) {
	t.Helper()
	seedQueue(t, s)
	execute(t, s, "UPDATE attempts SET mode='auto' WHERE id='a'")
	execute(t, s, "INSERT INTO users(id,name,created) VALUES('v','vera','created'),('w','walt','created')")
	execute(t, s, "INSERT INTO project_members(project,user,role,added) VALUES('p','v','member','added'),('p','w','owner','added')")
}

func decideAs(s *Store, user, attempt, decision, key string, at time.Time) (DecisionResult, error) {
	var result DecisionResult
	err := s.write(ctx, user, func(tx *Tx) error {
		var err error
		result, err = tx.Decide(ctx, attempt, decision, key, at)
		return err
	})
	return result, err
}

func attemptState(t *testing.T, s *Store, id string) string {
	t.Helper()
	a, err := s.AttemptByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return a.State
}

func lastCancelReason(t *testing.T, s *Store, user, attempt string) string {
	t.Helper()
	events, err := s.Events(ctx, user, attempt, 0)
	if err != nil {
		t.Fatal(err)
	}
	reason := ""
	for _, e := range events {
		if e.Kind == "cancel" {
			reason = string(e.Payload)
		}
	}
	return reason
}

func TestTwoClaimsRacingOnOneAttemptHaveOneWinner(t *testing.T) {
	s := openTestStore(t)
	seedDecision(t, s)
	if _, err := decideAs(s, "u", "a", "allow", "allow", time.Now()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- s.DoOwned(ctx, "a", 0, func(tx *Tx) error { _, err := tx.ClaimLaunch(ctx, "a", time.Now()); return err })
		}()
	}
	wg.Wait()
	close(results)
	won, lost := 0, 0
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrAttemptLaunched):
			lost++
		default:
			t.Fatal(err)
		}
	}
	if won != 1 || lost != 1 || attemptState(t, s, "a") != "launching" {
		t.Fatalf("won %d, lost %d", won, lost)
	}
}

func TestTheClaimRefusesAnAttemptWaitingForADecision(t *testing.T) {
	s := openTestStore(t)
	seedDecision(t, s)
	err := s.DoOwned(ctx, "a", 0, func(tx *Tx) error { _, err := tx.ClaimLaunch(ctx, "a", time.Now()); return err })
	if !errors.Is(err, ErrDecisionRequired) || attemptState(t, s, "a") != "pending" {
		t.Fatalf("claim: %v", err)
	}
	if _, ok, err := s.NextPendingAttempt(ctx, 0, time.Now(), nil); err != nil || ok {
		t.Fatalf("offered for launch: %v %v", ok, err)
	}
	// A default raised to the attempt's mode admits it at the claim, with no decision.
	execute(t, s, "UPDATE projects SET default_mode='auto' WHERE id='p'")
	if err := s.DoOwned(ctx, "a", 0, func(tx *Tx) error { _, err := tx.ClaimLaunch(ctx, "a", time.Now()); return err }); err != nil {
		t.Fatal(err)
	}
}

func TestAnOtherMembersDecisionAndAnOwnersAllowAreRefusedAndAnOwnersDenyIsAccepted(t *testing.T) {
	s := openTestStore(t)
	seedDecision(t, s)
	for _, c := range []struct{ user, decision string }{{"v", "allow"}, {"v", "deny"}, {"w", "allow"}} {
		if _, err := decideAs(s, c.user, "a", c.decision, c.user+c.decision, time.Now()); !errors.Is(err, ErrDecisionRefused) {
			t.Fatalf("%s %s: %v", c.user, c.decision, err)
		}
	}
	execute(t, s, "INSERT INTO users(id,name,created) VALUES('x','xavier','created')")
	if _, err := decideAs(s, "x", "a", "deny", "x", time.Now()); !errors.Is(err, ErrNotMember) {
		t.Fatalf("a non-member: %v", err)
	}
	result, err := decideAs(s, "w", "a", "deny", "w-deny", time.Now())
	if err != nil || !result.Denied || result.Task != "t" {
		t.Fatalf("owner's deny = %+v: %v", result, err)
	}
	if attemptState(t, s, "a") != "canceled" || !strings.Contains(lastCancelReason(t, s, "u", "a"), DeniedReason) {
		t.Fatalf("attempt %s, reason %s", attemptState(t, s, "a"), lastCancelReason(t, s, "u", "a"))
	}
	task, err := s.Task(ctx, "u", "t")
	if err != nil || task.State != Canceled || task.Attempt.Decision == nil || !task.Attempt.Decision.Denied {
		t.Fatalf("task = %+v: %v", task, err)
	}
}

func TestARepeatedDecisionWithTheSameKeyReturnsTheFirstResultAndAnotherBodyIsRefused(t *testing.T) {
	s := openTestStore(t)
	seedDecision(t, s)
	first, err := decideAs(s, "u", "a", "allow", "k", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	again, err := decideAs(s, "u", "a", "allow", "k", time.Now())
	if err != nil || again != first {
		t.Fatalf("replay = %+v: %v", again, err)
	}
	if _, err := decideAs(s, "u", "a", "deny", "k", time.Now()); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("another body: %v", err)
	}
	var rows int
	if err := s.db.QueryRow("SELECT count(*) FROM permission_decisions").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("%d decisions: %v", rows, err)
	}
	if attemptState(t, s, "a") != "pending" {
		t.Fatal("the refused deny closed the attempt")
	}
}

func TestADecisionOnALaunchedClosedOrUnneedingAttemptIsRefused(t *testing.T) {
	for _, state := range []string{"launching", "running", "canceled", "ended_ok"} {
		s := openTestStore(t)
		seedDecision(t, s)
		execute(t, s, "UPDATE attempts SET state=? WHERE id='a'", state)
		if _, err := decideAs(s, "u", "a", "allow", "k", time.Now()); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%s: %v", state, err)
		}
	}
	s := openTestStore(t)
	seedDecision(t, s)
	execute(t, s, "UPDATE projects SET default_mode='yolo' WHERE id='p'")
	if _, err := decideAs(s, "u", "a", "allow", "k", time.Now()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("no decision needed: %v", err)
	}
}

func TestADecisionIsShownOnlyToItsActorAndTheProjectsOwners(t *testing.T) {
	s := openTestStore(t)
	seedDecision(t, s)
	for user, visible := range map[string]bool{"u": true, "w": true, "v": false} {
		task, err := s.Task(ctx, user, "t")
		if err != nil {
			t.Fatal(err)
		}
		if (task.Attempt.Decision != nil) != visible {
			t.Fatalf("%s sees %+v", user, task.Attempt.Decision)
		}
	}
	task, _ := s.Task(ctx, "u", "t")
	d := task.Attempt.Decision
	if d.Actor != "u" || !d.Required || d.Allowed || d.Denied || d.ProjectDefault != "readonly" || d.Scope != (DecisionScope{"codex", "auto", "local"}) {
		t.Fatalf("decision = %+v", d)
	}
}

// seedRemoval gives member v a pending attempt b waiting for a decision, a pending attempt c
// needing none (task t3, mode readonly) and a running attempt d.
func seedRemoval(t *testing.T, s *Store) {
	t.Helper()
	seedDecision(t, s)
	for _, row := range [][2]string{{"t2", "preparing"}, {"t3", "preparing"}, {"t4", "running"}} {
		execute(t, s, `INSERT INTO tasks(id,project,created_by,objective,acceptance_criteria,state,revision,created,updated) VALUES (?, 'p', 'v', 'objective', 'criteria', ?, 2, 'created', 'updated')`, row[0], row[1])
	}
	for _, row := range [][4]string{{"b", "t2", "auto", "pending"}, {"c", "t3", "readonly", "pending"}, {"d", "t4", "auto", "running"}} {
		execute(t, s, `INSERT INTO attempts(id,task,actor,backend,model,effort,mode,executor,ownership_generation,engine_sha256,state,started) VALUES (?, ?, 'v', 'codex', '', '', ?, 'local', 0, 'hash', ?, 'started')`, row[0], row[1], row[2], row[3])
	}
}

func TestRemovingAMemberDeniesTheirPendingDecisionsAndCancelsTheirAttemptsInOneTransaction(t *testing.T) {
	s := openTestStore(t)
	seedRemoval(t, s)
	// A transaction that fails after the removal leaves everything as it was.
	failed := errors.New("later step failed")
	err := s.write(ctx, "w", func(tx *Tx) error {
		if _, err := tx.RemoveMembership(ctx, "p", "v", "remove", time.Now()); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) || attemptState(t, s, "b") != "pending" || attemptState(t, s, "d") != "running" {
		t.Fatalf("after a rollback: %v, b %s, d %s", err, attemptState(t, s, "b"), attemptState(t, s, "d"))
	}
	var removal Removal
	if err := s.write(ctx, "w", func(tx *Tx) error {
		var err error
		removal, err = tx.RemoveMembership(ctx, "p", "v", "remove", time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(removal.Attempts, ",") != "b,c,d" || strings.Join(removal.Running, ",") != "t4" {
		t.Fatalf("removal = %+v", removal)
	}
	var decision, by string
	if err := s.db.QueryRow("SELECT decision,decided_by FROM permission_decisions WHERE attempt='b'").Scan(&decision, &by); err != nil || decision != "deny" || by != "w" {
		t.Fatalf("b's decision %s by %s: %v", decision, by, err)
	}
	var others int
	if err := s.db.QueryRow("SELECT count(*) FROM permission_decisions WHERE attempt<>'b'").Scan(&others); err != nil || others != 0 {
		t.Fatalf("%d other decisions: %v", others, err)
	}
	for attempt, want := range map[string]struct{ state, reason string }{"b": {"canceled", RevokedReason}, "c": {"canceled", RemovedReason}, "d": {"running", RemovedReason}} {
		if got := attemptState(t, s, attempt); got != want.state {
			t.Fatalf("%s is %s", attempt, got)
		}
		if reason := lastCancelReason(t, s, "w", attempt); !strings.Contains(reason, want.reason) {
			t.Fatalf("%s's reason %s", attempt, reason)
		}
	}
	for task, want := range map[string]State{"t2": Canceled, "t3": Canceled, "t4": CancelRequested, "t": Preparing} {
		got, err := s.Task(ctx, "w", task)
		if err != nil || got.State != want {
			t.Fatalf("%s = %s: %v", task, got.State, err)
		}
	}
	if _, err := s.Task(ctx, "v", "t2"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("the removed member reads a task: %v", err)
	}
	if tasks, err := s.ListTasks(ctx, "v"); err != nil || len(tasks) != 0 {
		t.Fatalf("the removed member lists %v: %v", tasks, err)
	}
	if _, err := s.Events(ctx, "v", "b", 0); !errors.Is(err, ErrNotMember) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("the removed member reads events: %v", err)
	}
}

func TestTheLastOwnersRemovalAndDemotionAreRefused(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	err := s.write(ctx, "u", func(tx *Tx) error {
		_, err := tx.RemoveMembership(ctx, "p", "u", "k", time.Now())
		return err
	})
	if !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removal: %v", err)
	}
	err = s.write(ctx, "u", func(tx *Tx) error {
		_, err := tx.SetMember(ctx, "p", "u", "member", time.Now())
		return err
	})
	if !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demotion: %v", err)
	}
	if members, err := s.Members(ctx, "p"); err != nil || len(members) != 1 || members[0].Role != "owner" {
		t.Fatalf("members %+v: %v", members, err)
	}
}

func TestStopCountsSeparateOtherUsersWork(t *testing.T) {
	s := openTestStore(t)
	seedRemoval(t, s)
	counts, err := s.StopCounts(ctx, 0, "u")
	if err != nil || counts != (StopCounts{Running: 1, Queued: 3, OthersRunning: 1, OthersQueued: 2}) {
		t.Fatalf("counts %+v: %v", counts, err)
	}
}

func TestARemovalCancelsAnAllowedAttemptWaitingForASlotWithoutADeny(t *testing.T) {
	s := openTestStore(t)
	seedRemoval(t, s)
	if _, err := decideAs(s, "v", "b", "allow", "allow-b", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, "w", func(tx *Tx) error {
		_, err := tx.RemoveMembership(ctx, "p", "v", "remove", time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var denies int
	if err := s.db.QueryRow("SELECT count(*) FROM permission_decisions WHERE attempt='b' AND decision='deny'").Scan(&denies); err != nil || denies != 0 {
		t.Fatalf("%d denies: %v", denies, err)
	}
	if attemptState(t, s, "b") != "canceled" || !strings.Contains(lastCancelReason(t, s, "w", "b"), RemovedReason) {
		t.Fatalf("b is %s with %s", attemptState(t, s, "b"), lastCancelReason(t, s, "w", "b"))
	}
}
