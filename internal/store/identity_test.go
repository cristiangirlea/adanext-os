package store

import (
	"encoding/json"
	"errors"
	"testing"

	"modernc.org/sqlite"
)

func TestNonmembersCannotListReadOrChangeTasksOrEvents(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	if err := s.CreateUser(ctx, "outsider", "outsider"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, "u", Event{Attempt: "a", Seq: 1, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"outsider", "unknown"} {
		tasks, err := s.ListTasks(ctx, user)
		if err != nil || len(tasks) != 0 {
			t.Fatalf("listing: %+v %v", tasks, err)
		}
		if _, err := s.Task(ctx, user, "t"); !errors.Is(err, ErrNotMember) {
			t.Fatalf("task: %v", err)
		}
		if _, err := s.Events(ctx, user, "a", 0); !errors.Is(err, ErrNotMember) {
			t.Fatalf("events: %v", err)
		}
		if err := s.Transition(ctx, user, "t", Draft, Ready, 0); !errors.Is(err, ErrNotMember) {
			t.Fatalf("transition: %v", err)
		}
		if err := s.AppendEvent(ctx, user, Event{Attempt: "a", Seq: 2, Payload: json.RawMessage(`{}`)}); !errors.Is(err, ErrNotMember) {
			t.Fatalf("append: %v", err)
		}
	}
	task, err := s.Task(ctx, "u", "t")
	if err != nil || task.CreatedBy != "u" {
		t.Fatalf("member read: %+v %v", task, err)
	}
	tasks, err := s.ListTasks(ctx, "u")
	if err != nil || len(tasks) != 1 || tasks[0] != task {
		t.Fatalf("member list: %+v %v", tasks, err)
	}
}

func TestUnknownActorsAreRefusedByForeignKeys(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	for _, query := range []string{
		"UPDATE tasks SET created_by='forged'",
		"UPDATE attempts SET actor='forged'",
	} {
		_, err := s.db.ExecContext(ctx, query)
		var cause *sqlite.Error
		if !errors.As(err, &cause) || cause.Code() != 787 {
			t.Fatalf("foreign key: %v", err)
		}
	}
}

func TestRemovingMembershipPreservesEvidenceForRemainingMembers(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	if err := s.CreateUser(ctx, "remaining", "remaining"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "p", "remaining", "member"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, "u", Event{Attempt: "a", Seq: 1, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, "p", "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, "u", "t"); !errors.Is(err, ErrNotMember) {
		t.Fatal(err)
	}
	if _, err := s.Events(ctx, "u", "a", 0); !errors.Is(err, ErrNotMember) {
		t.Fatal(err)
	}
	if tasks, err := s.ListTasks(ctx, "u"); err != nil || len(tasks) != 0 {
		t.Fatalf("removed list: %+v %v", tasks, err)
	}
	if task, err := s.Task(ctx, "remaining", "t"); err != nil || task.CreatedBy != "u" {
		t.Fatalf("remaining read: %+v %v", task, err)
	}
	if events, err := s.Events(ctx, "remaining", "a", 0); err != nil || len(events) != 1 {
		t.Fatalf("remaining events: %+v %v", events, err)
	}
}

func TestTheSameIdempotencyKeyFromTwoUsersIsTwoRequests(t *testing.T) {
	s := openTestStore(t)
	for _, user := range []string{"one", "two"} {
		if err := s.CreateUser(ctx, user, user); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	for range 2 {
		for _, user := range []string{"one", "two"} {
			response, err := s.Do(ctx, user, "key", user, func(*Tx) ([]byte, error) { calls++; return []byte(user), nil })
			if err != nil || string(response) != user {
				t.Fatalf("response: %s %v", response, err)
			}
		}
	}
	if calls != 2 {
		t.Fatalf("callbacks: %d", calls)
	}
}

func TestAnAttemptActorMustBeAMemberAtCreation(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	if err := s.CreateUser(ctx, "other", "other"); err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{ID: "new", Task: "t", Backend: "codex", Actor: "other"}
	if err := s.CreateAttempt(ctx, attempt); !errors.Is(err, ErrNotMember) {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "p", "other", "member"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, "p", "other"); err != nil {
		t.Fatal(err)
	}
	attempt.ID = "after-revocation"
	if err := s.CreateAttempt(ctx, attempt); !errors.Is(err, ErrNotMember) {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM attempts WHERE actor='other'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("attempts: %d %v", count, err)
	}
}

func TestTaskCreationRequiresMembershipAndRecordsItsActor(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	if err := s.CreateUser(ctx, "other", "other"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, "other", "new", "p", "objective", "criteria"); !errors.Is(err, ErrNotMember) {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, "u", "new", "p", "objective", "criteria"); err != nil {
		t.Fatal(err)
	}
	task, err := s.Task(ctx, "u", "new")
	if err != nil || task.CreatedBy != "u" || task.State != Draft || task.Revision != 0 {
		t.Fatalf("created task: %+v %v", task, err)
	}
}
