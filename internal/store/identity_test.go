package store

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

func TestIdempotentTaskReplayIsRefusedAfterMembershipRemoval(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	calls := 0
	fn := func(tx *Tx) ([]byte, error) {
		calls++
		if err := tx.Transition(ctx, "t", Draft, Ready, 0); err != nil {
			return nil, err
		}
		task, err := scanTask(tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id='t'"))
		if err != nil {
			return nil, err
		}
		return json.Marshal(task)
	}
	response, err := s.Do(ctx, "u", "change", "hash", &Scope{ProjectID: "p"}, fn)
	if err != nil {
		t.Fatal(err)
	}
	var task Task
	if err := json.Unmarshal(response, &task); err != nil || task.ID != "t" || task.State != Ready || task.Revision != 1 {
		t.Fatalf("task response: %+v %v", task, err)
	}
	if err := s.RemoveMember(ctx, "p", "u"); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"hash", "different hash"} {
		response, err = s.Do(ctx, "u", "change", hash, &Scope{ProjectID: "p"}, fn)
		if !errors.Is(err, ErrNotMember) || response != nil || calls != 1 {
			t.Fatalf("revoked replay: response=%s err=%v callbacks=%d", response, err, calls)
		}
	}
}

func TestProjectScopedRequestsRefuseNonmembersBeforeCallingTheCallback(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	if err := s.CreateUser(ctx, "other", "other"); err != nil {
		t.Fatal(err)
	}
	called := false
	response, err := s.Do(ctx, "other", "new", "hash", &Scope{ProjectID: "p"}, func(tx *Tx) ([]byte, error) {
		called = true
		_, err := tx.ExecContext(ctx, "UPDATE tasks SET objective='forbidden' WHERE id='t'")
		return []byte("forbidden"), err
	})
	if !errors.Is(err, ErrNotMember) || response != nil || called {
		t.Fatalf("nonmember request: response=%s err=%v callback=%v", response, err, called)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM idempotency").Scan(&count); err != nil || count != 0 {
		t.Fatalf("denied request cached: %d %v", count, err)
	}
}

func TestMembershipInAnotherProjectDoesNotGrantTaskAccess(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	if err := s.CreateUser(ctx, "other", "other"); err != nil {
		t.Fatal(err)
	}
	execute(t, s, "INSERT INTO projects VALUES('q','other project','source','created')")
	if err := s.AddMember(ctx, "q", "other", "member"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, "other", "own", "q", "own objective", "criteria"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, "u", Event{Attempt: "a", Seq: 1, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	t.Run("list", func(t *testing.T) {
		tasks, err := s.ListTasks(ctx, "other")
		if err != nil || len(tasks) != 1 || tasks[0].ID != "own" {
			t.Fatalf("listing: %+v %v", tasks, err)
		}
	})
	for _, tc := range []struct {
		name string
		fn   func() error
	}{
		{"read", func() error { _, err := s.Task(ctx, "other", "t"); return err }},
		{"events", func() error { _, err := s.Events(ctx, "other", "a", 0); return err }},
		{"mutation", func() error { return s.Transition(ctx, "other", "t", Draft, Ready, 0) }},
		{"append", func() error {
			return s.AppendEvent(ctx, "other", Event{Attempt: "a", Seq: 2, Payload: json.RawMessage(`{}`)})
		}},
		{"create task", func() error { return s.CreateTask(ctx, "other", "forbidden", "p", "objective", "criteria") }},
		{"create attempt", func() error {
			return s.CreateAttempt(ctx, Attempt{ID: "forbidden", Task: "t", Actor: "other", Backend: "codex"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); !errors.Is(err, ErrNotMember) {
				t.Fatalf("access: %v", err)
			}
		})
	}
}

func TestConcurrentIdenticalKeysAndHashesExecuteOncePerUser(t *testing.T) {
	s := openTestStore(t)
	for _, user := range []string{"one", "two"} {
		if err := s.CreateUser(ctx, user, user); err != nil {
			t.Fatal(err)
		}
	}
	other, err := Open(ctx, s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var calls [2]atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for index, user := range []string{"one", "two"} {
		for i := range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				store := s
				if i%2 == 0 {
					store = other
				}
				response, err := store.Do(ctx, user, "key", "hash", nil, func(*Tx) ([]byte, error) {
					calls[index].Add(1)
					return []byte(user), nil
				})
				if err != nil || string(response) != user {
					t.Errorf("user %s response: %s %v", user, response, err)
				}
			}()
		}
	}
	close(start)
	wg.Wait()
	for index := range calls {
		if calls[index].Load() != 1 {
			t.Errorf("user %d callbacks: %d", index, calls[index].Load())
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM idempotency WHERE key='key' AND request_hash='hash'").Scan(&count); err != nil || count != 2 {
		t.Fatalf("responses: %d %v", count, err)
	}
}

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
			response, err := s.Do(ctx, user, "key", user, nil, func(*Tx) ([]byte, error) { calls++; return []byte(user), nil })
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
