package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestOpenRefusesUnrecordedOrTamperedMigrations(t *testing.T) {
	for _, query := range []string{
		"DROP TABLE events",
		"UPDATE schema_migrations SET sha256='tampered' WHERE version=1",
		"DELETE FROM schema_migrations WHERE version=1",
		"INSERT INTO schema_migrations VALUES(2,'unexpected')",
		"PRAGMA user_version=0",
	} {
		t.Run(query, func(t *testing.T) {
			s := openTestStore(t)
			execute(t, s, query)
			path := s.path
			s.Close()
			reopened, err := Open(ctx, path)
			if reopened != nil {
				reopened.Close()
			}
			if err == nil {
				t.Fatal("opened a tampered database")
			}
		})
	}
	t.Run("user version without migration records", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unrecorded.sqlite")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("PRAGMA user_version=1"); err != nil {
			t.Fatal(err)
		}
		db.Close()
		s, err := Open(ctx, path)
		if s != nil {
			s.Close()
		}
		if err == nil {
			t.Fatal("opened user_version=1 without recorded migrations")
		}
	})
}

func TestDoRefusesAnEmptyKeyOrRequestHashBeforeCallingTheCallback(t *testing.T) {
	s := openTestStore(t)
	for _, pair := range [][2]string{{"", "hash"}, {"key", ""}} {
		called := false
		_, err := s.Do(ctx, pair[0], pair[1], func(*Tx) ([]byte, error) { called = true; return nil, nil })
		if err == nil || called {
			t.Fatalf("empty idempotency input accepted: %v called=%v", err, called)
		}
	}
}

func TestBackupRefusesRelativePathsAndFileURIs(t *testing.T) {
	s := openTestStore(t)
	for _, dest := range []string{"", ":memory:", filepath.Join(".hrn", "sandbox", "tmp", "relative-backup.sqlite"), "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "backup.sqlite"))} {
		if err := s.Backup(ctx, dest); err == nil {
			t.Fatalf("accepted backup destination %q", dest)
		}
	}
}

func TestAnImmediateTransactionBlocksWritesBeforeWritingAnyRows(t *testing.T) {
	s := openTestStore(t)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=1"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := conn.ExecContext(ctx, "INSERT INTO projects VALUES('locked','name','source','created')"); !isBusy(err) {
		t.Fatalf("BeginTx did not acquire the write lock: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO projects VALUES('unlocked','name','source','created')"); err != nil {
		t.Fatal(err)
	}
}

func TestDoComposesTransitionsAndEventsInTheSameTransaction(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	fn := func(tx *Tx) ([]byte, error) {
		if err := tx.Transition(ctx, "t", Draft, Ready, 0); err != nil {
			return nil, err
		}
		if err := tx.AppendEvent(ctx, Event{Attempt: "a", Seq: 1, Payload: json.RawMessage(`{"result":"ready"}`)}); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO projects VALUES('q','name','source','created')"); err != nil {
			return nil, err
		}
		return []byte("ready"), nil
	}
	for range 2 {
		response, err := s.Do(ctx, "change", "hash", fn)
		if err != nil || string(response) != "ready" {
			t.Fatalf("response: %s %v", response, err)
		}
	}
	var state string
	var revision int
	if err := s.db.QueryRow("SELECT state,revision FROM tasks WHERE id='t'").Scan(&state, &revision); err != nil || state != "ready" || revision != 1 {
		t.Fatalf("task: %s %d %v", state, revision, err)
	}
	events, err := s.Events(ctx, "a", 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("events: %+v %v", events, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM projects WHERE id='q'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("insert: %d %v", count, err)
	}
	failure := errors.New("rollback")
	if _, err := s.Do(ctx, "rollback", "hash", func(tx *Tx) ([]byte, error) {
		if err := tx.Transition(ctx, "t", Ready, Preparing, 1); err != nil {
			return nil, err
		}
		if err := tx.AppendEvent(ctx, Event{Attempt: "a", Seq: 2, Payload: json.RawMessage(`{}`)}); err != nil {
			return nil, err
		}
		return nil, failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT revision FROM tasks WHERE id='t'").Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("rollback revision: %d %v", revision, err)
	}
	events, err = s.Events(ctx, "a", 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("rollback events: %+v %v", events, err)
	}
	for _, tc := range []struct {
		key  string
		fn   func(*Tx) error
		want error
	}{
		{"stale", func(tx *Tx) error { return tx.Transition(ctx, "t", Ready, Preparing, 0) }, ErrStaleRevision},
		{"invalid", func(tx *Tx) error { return tx.Transition(ctx, "t", Ready, Accepted, 1) }, ErrInvalidTransition},
		{"sequence", func(tx *Tx) error {
			return tx.AppendEvent(ctx, Event{Attempt: "a", Seq: 3, Payload: json.RawMessage(`{}`)})
		}, ErrEventSequence},
		{"missing", func(tx *Tx) error { return tx.Transition(ctx, "missing", Ready, Preparing, 0) }, ErrNotFound},
	} {
		if _, err := s.Do(ctx, tc.key, "hash", func(tx *Tx) ([]byte, error) { return nil, tc.fn(tx) }); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", tc.key, err)
		}
	}
}

type busyError struct{}

func (busyError) Error() string { return "busy" }

func TestBusyRetryRetriesOnlyBusyAndStopsAtItsDeadline(t *testing.T) {
	busy := busyError{}
	isBusy := func(err error) bool { var target busyError; return errors.As(err, &target) }
	calls := 0
	err := retryBusy(ctx, 100*time.Millisecond, isBusy, func(context.Context) error {
		calls++
		if calls == 1 {
			return busy
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("retry: %d %v", calls, err)
	}
	other := errors.New("other")
	calls = 0
	err = retryBusy(ctx, time.Second, isBusy, func(context.Context) error { calls++; return other })
	if !errors.Is(err, other) || calls != 1 {
		t.Fatalf("non-busy: %d %v", calls, err)
	}
	started := time.Now()
	err = retryBusy(ctx, 25*time.Millisecond, isBusy, func(context.Context) error { return busy })
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("deadline: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err = retryBusy(canceled, time.Second, isBusy, func(context.Context) error { t.Fatal("ran after cancellation"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestOpenAndEventsHonorCanceledContexts(t *testing.T) {
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if s, err := Open(canceled, filepath.Join(t.TempDir(), "state.sqlite")); !errors.Is(err, context.Canceled) {
		if s != nil {
			s.Close()
		}
		t.Fatal(err)
	}
	s := openTestStore(t)
	if _, err := s.Events(canceled, "a", 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestMigrationRefusesForeignKeyViolationsBeforeCommit(t *testing.T) {
	s := openTestStore(t)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO tasks VALUES('orphan','missing','objective','criteria','draft',0,'created','updated')"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	initial, err := migrationFiles.ReadFile("migrations/0001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{"migrations/0001_initial.sql": &fstest.MapFile{Data: initial}, "migrations/0002_good.sql": &fstest.MapFile{Data: []byte("CREATE TABLE should_not_commit(id INTEGER);")}}
	if err := s.migrate(ctx, files); err == nil || !strings.Contains(err.Error(), "foreign key check") {
		t.Fatalf("migration: %v", err)
	}
	var version, count int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("version: %d %v", version, err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='should_not_commit'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial migration: %d %v", count, err)
	}
}

func TestBackupUnderConcurrentWritesRestoresAConsistentCommitPrefix(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	writerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	first := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for seq := int64(1); ; seq++ {
			if writerCtx.Err() != nil {
				done <- nil
				return
			}
			_, err := s.Do(writerCtx, fmt.Sprintf("work-%d", seq), "hash", func(tx *Tx) ([]byte, error) {
				if err := tx.AppendEvent(writerCtx, Event{Attempt: "a", Seq: seq, Payload: json.RawMessage(`{}`)}); err != nil {
					return nil, err
				}
				return []byte(fmt.Sprintf("%d", seq)), nil
			})
			if err != nil {
				if writerCtx.Err() != nil {
					err = nil
				}
				done <- err
				return
			}
			if seq == 1 {
				close(first)
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		if done != nil {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	})
	select {
	case <-first:
	case err := <-done:
		done = nil
		t.Fatalf("writer: %v", err)
	case <-writerCtx.Done():
		t.Fatal("writer did not start")
	}
	dest := filepath.Join(t.TempDir(), "concurrent.sqlite")
	if err := s.Backup(writerCtx, dest); err != nil {
		t.Fatal(err)
	}
	cancel()
	writerErr := <-done
	done = nil
	if writerErr != nil {
		t.Fatal(writerErr)
	}
	restored, err := Open(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var integrity string
	if err := restored.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity: %s %v", integrity, err)
	}
	events, err := restored.Events(ctx, "a", 0)
	if err != nil || len(events) == 0 {
		t.Fatalf("events: %d %v", len(events), err)
	}
	for i, event := range events {
		seq := int64(i + 1)
		if event.Seq != seq {
			t.Fatalf("event gap: %d != %d", event.Seq, seq)
		}
		var response []byte
		if err := restored.db.QueryRow("SELECT response FROM idempotency WHERE key=?", fmt.Sprintf("work-%d", seq)).Scan(&response); err != nil || string(response) != fmt.Sprintf("%d", seq) {
			t.Fatalf("committed work: %s %v", response, err)
		}
	}
	var count int
	if err := restored.db.QueryRow("SELECT count(*) FROM idempotency").Scan(&count); err != nil || count != len(events) {
		t.Fatalf("rows: %d events: %d %v", count, len(events), err)
	}
}
