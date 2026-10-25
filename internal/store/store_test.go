package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

var ctx = context.Background()

func mustMigration(t *testing.T, name string) []byte {
	t.Helper()
	data, err := migrationFiles.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func execute(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func seed(t *testing.T, s *Store) {
	t.Helper()
	execute(t, s, `INSERT INTO users VALUES ('u', 'user', 'created')`)
	execute(t, s, `INSERT INTO projects VALUES ('p', 'project', '/source', '2026-10-11T00:00:00Z')`)
	execute(t, s, `INSERT INTO project_members VALUES ('p', 'u', 'owner', 'added')`)
	execute(t, s, `INSERT INTO tasks(id,project,created_by,objective,acceptance_criteria,state,revision,created,updated) VALUES ('t', 'p', 'u', 'objective', 'criteria', 'draft', 0, 'created', 'updated')`)
	execute(t, s, `INSERT INTO attempts(id,task,actor,backend,model,effort,executor,ownership_generation,engine_sha256,run_id,native_session,state,started,ended) VALUES ('a', 't', 'u', 'codex', 'model', 'low', 'local', 1, 'hash', 'run', 'session', 'running', 'started', NULL)`)
}

func TestOpenAppliesAndReadsBackPragmasOnEveryConnection(t *testing.T) {
	s := openTestStore(t)
	var conns []*sql.Conn
	for range 4 {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		for name, want := range map[string]string{"journal_mode": "wal", "foreign_keys": "1", "synchronous": "2", "busy_timeout": "5000"} {
			var got string
			if err := conn.QueryRowContext(ctx, "PRAGMA "+name).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("%s = %s, want %s", name, got, want)
			}
		}
	}
	for _, conn := range conns {
		conn.Close()
	}
	// Evict idle connections: replacements must also be initialized.
	s.db.SetMaxIdleConns(0)
	var foreignKeys int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("replacement: %d, %v", foreignKeys, err)
	}
}

func TestOpenRefusesADatabaseThatCannotUseWAL(t *testing.T) {
	s, err := Open(ctx, ":memory:")
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Fatal("opened a database without WAL")
	}
}

func TestOpenRefusesAMissingParentDirectory(t *testing.T) {
	s, err := Open(ctx, filepath.Join(t.TempDir(), "missing", "state.sqlite"))
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Fatal("opened a missing parent")
	}
}

// This driver double accepts every PRAGMA write but can lie on read-back.
// It pins refusal for each setting, independent of SQLite's normal defaults.
type pragmaConnection struct{ bad, fail string }

func (p pragmaConnection) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

func (p pragmaConnection) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	name := strings.TrimPrefix(query, "PRAGMA ")
	if name == p.fail {
		return nil, errors.New("read failed")
	}
	values := map[string]driver.Value{"journal_mode": "wal", "foreign_keys": int64(1), "synchronous": int64(2), "busy_timeout": int64(5000)}
	if name == p.bad {
		return &pragmaRows{value: "wrong"}, nil
	}
	return &pragmaRows{value: values[name]}, nil
}

type pragmaRows struct {
	value driver.Value
	read  bool
}

func (r *pragmaRows) Columns() []string { return []string{"pragma"} }
func (r *pragmaRows) Close() error      { return nil }
func (r *pragmaRows) Next(values []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	values[0] = r.value
	return nil
}

func TestPragmaReadBackRefusesEveryMismatchOrReadFailure(t *testing.T) {
	for _, name := range []string{"busy_timeout", "journal_mode", "foreign_keys", "synchronous"} {
		for _, conn := range []pragmaConnection{{bad: name}, {fail: name}} {
			if err := configure(ctx, conn); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
}

func TestConcurrentOpenersApplyTheInitialMigrationOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s, err := Open(ctx, path)
			if err != nil {
				t.Error(err)
				return
			}
			defer s.Close()
			var version int
			if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 8 {
				t.Errorf("version: %d %v", version, err)
			}
		}()
	}
	close(start)
	wg.Wait()
}

func TestMigrationsCreateSchemaAndRefuseANewerVersion(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 8 {
		t.Fatalf("version: %d, %v", version, err)
	}
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	execute(t, s, "PRAGMA user_version=99")
	s.Close()
	s, err = Open(ctx, path)
	if s != nil {
		s.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "99") || !strings.Contains(err.Error(), "8") {
		t.Fatalf("future schema: %v", err)
	}
}

func TestEachMigrationAndItsVersionCommitTogether(t *testing.T) {
	s := openTestStore(t)
	initial, err := migrationFiles.ReadFile("migrations/0001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	second, err := migrationFiles.ReadFile("migrations/0002_idempotency_project.sql")
	if err != nil {
		t.Fatal(err)
	}
	third, err := migrationFiles.ReadFile("migrations/0003_artifacts.sql")
	if err != nil {
		t.Fatal(err)
	}
	migrations := fstest.MapFS{
		"migrations/0001_initial.sql":             &fstest.MapFile{Data: initial},
		"migrations/0002_idempotency_project.sql": &fstest.MapFile{Data: second},
		"migrations/0003_artifacts.sql":           &fstest.MapFile{Data: third},
		"migrations/0004_execution.sql":           &fstest.MapFile{Data: mustMigration(t, "migrations/0004_execution.sql")},
		"migrations/0005_import.sql":              &fstest.MapFile{Data: mustMigration(t, "migrations/0005_import.sql")},
		"migrations/0006_workspaces.sql":          &fstest.MapFile{Data: mustMigration(t, "migrations/0006_workspaces.sql")},
		"migrations/0007_drift.sql":               &fstest.MapFile{Data: mustMigration(t, "migrations/0007_drift.sql")},
		"migrations/0008_capabilities.sql":        &fstest.MapFile{Data: mustMigration(t, "migrations/0008_capabilities.sql")},
		"migrations/0009_good.sql":                &fstest.MapFile{Data: []byte("CREATE TABLE committed (id INTEGER);")},
		"migrations/0010_bad.sql":                 &fstest.MapFile{Data: []byte("CREATE TABLE rolled_back (id INTEGER); INVALID SQL;")},
	}
	if err := s.migrate(ctx, migrations); err == nil {
		t.Fatal("bad migration succeeded")
	}
	var version, count int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 9 {
		t.Fatalf("version: %d, %v", version, err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='rolled_back'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial migration: %d, %v", count, err)
	}
	execute(t, s, "INSERT INTO committed VALUES(1)")
	var recorded int
	if err := s.db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&recorded); err != nil || recorded != 9 {
		t.Fatalf("migration records: %d %v", recorded, err)
	}
}

func TestAppliedMigrationChecksumsNeverChange(t *testing.T) {
	want := map[string]string{"migrations/0001_initial.sql": "8b5adf6848a04663e52bceea5dde5c31036ea5db07c6c11033e825b2ef61ba82", "migrations/0002_idempotency_project.sql": "9359f995f73c3c769c9023ace84cabde6b9919bf9a331a1ba051bf349f35c8d7", "migrations/0003_artifacts.sql": "311cf37d45794fb5659f5d66dd5dffff4579d1bf93b022f176c93b725f964fa5", "migrations/0004_execution.sql": "74a497b8e4f143e143e0d32f99cd763a157ba1742c2b5929d363e50e17f140be", "migrations/0005_import.sql": "8d41f95ae741b9af1e052297892d0a785abae629d7959f5882002f650b5ad49c", "migrations/0006_workspaces.sql": "e64a38b34b8af8311a7859f632641a94185f4c34a4d3540a844bc378a0e6c2a1", "migrations/0007_drift.sql": "cf35501a79cf73c3c734802ed34be4b00b72ba6825a287fb4aa5bb4033846171", "migrations/0008_capabilities.sql": "2f999a7e21581bb2f6eabc588652a94445d88f5489cdd1bf78150f80ab050807"}
	files, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(want) {
		t.Fatal("pin every new migration checksum")
	}
	for _, file := range files {
		name := "migrations/" + file.Name()
		data, err := migrationFiles.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want[name] {
			t.Fatalf("%s checksum = %s, want %s", name, got, want[name])
		}
	}
}

func TestForeignKeysPreventOrphansAndDeletingEvidence(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	if err := s.AppendEvent(ctx, "u", Event{Attempt: "a", Seq: 1, Time: "now", Kind: "output", Payload: json.RawMessage(`{"text":"evidence"}`)}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"DELETE FROM projects WHERE id='p'", "DELETE FROM tasks WHERE id='t'", "DELETE FROM attempts WHERE id='a'",
		"UPDATE tasks SET project='missing'", "UPDATE attempts SET task='missing'", "UPDATE events SET attempt='missing'",
	} {
		if _, err := s.db.Exec(query); err == nil {
			t.Errorf("allowed %s", query)
		}
	}
	if _, err := s.db.Exec("UPDATE tasks SET state='unknown'"); err == nil {
		t.Fatal("unknown task state allowed")
	}
}

func TestTransitionsAllowExactlyThePlanEdgesAndBumpRevision(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	states := []State{Draft, Ready, Preparing, Running, Verifying, AwaitingReview, Accepted, Refinement, Failed, CancelRequested, Canceled, Interrupted, RecoveryRequired, "unknown"}
	allowed := map[[2]State]bool{
		{Draft, Ready}: true, {Ready, Preparing}: true, {Preparing, Running}: true, {Preparing, Failed}: true, {Preparing, Interrupted}: true, {Running, Verifying}: true,
		{Verifying, AwaitingReview}: true, {AwaitingReview, Accepted}: true, {Running, Failed}: true,
		{Verifying, Failed}: true, {AwaitingReview, Refinement}: true, {Refinement, Ready}: true,
		{Running, CancelRequested}: true, {CancelRequested, Canceled}: true, {CancelRequested, Interrupted}: true, {Running, Interrupted}: true,
		{Interrupted, RecoveryRequired}: true,
	}
	for _, from := range states {
		for _, to := range states {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				if from != "unknown" {
					execute(t, s, "UPDATE tasks SET state=?, revision=7, updated='before' WHERE id='t'", from)
				} else {
					execute(t, s, "UPDATE tasks SET state='draft', revision=7, updated='before' WHERE id='t'")
				}
				err := s.Transition(ctx, "u", "t", from, to, 7)
				var state, updated string
				var revision int64
				if scanErr := s.db.QueryRow("SELECT state,revision,updated FROM tasks WHERE id='t'").Scan(&state, &revision, &updated); scanErr != nil {
					t.Fatal(scanErr)
				}
				if allowed[[2]State{from, to}] {
					if err != nil || state != string(to) || revision != 8 || updated == "before" {
						t.Fatalf("transition: %s %d %s %v", state, revision, updated, err)
					}
				} else if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("error = %v", err)
				} else if revision != 7 || updated != "before" || (from != "unknown" && state != string(from)) {
					t.Fatalf("invalid transition changed task: %s %d %s", state, revision, updated)
				}
			})
		}
	}
}

func TestTransitionDistinguishesMissingAndStaleTasks(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	if err := s.Transition(ctx, "u", "missing", Draft, Ready, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		from, to State
		revision int64
	}{{Draft, Ready, 1}, {Ready, Preparing, 0}} {
		if err := s.Transition(ctx, "u", "t", tc.from, tc.to, tc.revision); !errors.Is(err, ErrStaleRevision) {
			t.Fatal(err)
		}
	}
}

func TestConcurrentTransitionsAcrossStoresHaveExactlyOneWinner(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	other, err := Open(ctx, s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store := s
			if i%2 == 0 {
				store = other
			}
			err := store.Transition(ctx, "u", "t", Draft, Ready, 0)
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrStaleRevision) {
				t.Errorf("loser: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("winners = %d", wins.Load())
	}
}

func TestIdempotencyStoresResponsesAndRollsBackFailedCallbacks(t *testing.T) {
	s := openTestStore(t)
	execute(t, s, "INSERT INTO users VALUES('u','user','created')")
	var calls int
	fn := func(tx *Tx) ([]byte, error) {
		calls++
		_, err := tx.ExecContext(ctx, "INSERT INTO projects VALUES('p','name','source','created')")
		return []byte("response"), err
	}
	for range 2 {
		response, err := s.Do(ctx, "u", "key", "hash", nil, fn)
		if err != nil || string(response) != "response" {
			t.Fatalf("%s %v", response, err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
	other, err := Open(ctx, s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if response, err := other.Do(ctx, "u", "key", "hash", nil, func(*Tx) ([]byte, error) { t.Fatal("persisted key ran callback"); return nil, nil }); err != nil || string(response) != "response" {
		t.Fatalf("persisted response: %s %v", response, err)
	}
	if _, err := s.Do(ctx, "u", "key", "other", nil, fn); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatal(err)
	}
	failure := errors.New("callback failed")
	if _, err := s.Do(ctx, "u", "retry", "hash", nil, func(tx *Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "INSERT INTO projects VALUES('q','name','source','created')")
		if err != nil {
			return nil, err
		}
		return nil, failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := s.Do(ctx, "u", "retry", "hash", nil, func(tx *Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "INSERT INTO projects VALUES('q','name','source','created')")
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentIdempotencyAcrossStoresExecutesOnce(t *testing.T) {
	s := openTestStore(t)
	execute(t, s, "INSERT INTO users VALUES('u','user','created')")
	other, err := Open(ctx, s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var calls atomic.Int32
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store := s
			if i%2 == 0 {
				store = other
			}
			response, err := store.Do(ctx, "u", "key", "hash", nil, func(tx *Tx) ([]byte, error) { calls.Add(1); return []byte("ok"), nil })
			if err != nil || string(response) != "ok" {
				t.Errorf("%s %v", response, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestEventsRequireContiguousSequencesAndReadInOrder(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	for _, tc := range []struct {
		seq   int64
		valid bool
	}{{0, false}, {2, false}, {1, true}, {1, false}, {3, false}, {2, true}} {
		err := s.AppendEvent(ctx, "u", Event{Attempt: "a", Seq: tc.seq, Time: "now", Kind: "output", Payload: json.RawMessage(`{"ok":true}`)})
		if tc.valid && err != nil {
			t.Fatal(err)
		}
		if !tc.valid && !errors.Is(err, ErrEventSequence) {
			t.Fatalf("seq %d: %v", tc.seq, err)
		}
	}
	events, err := s.Events(ctx, "u", "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("events: %+v", events)
	}
	tail, err := s.Events(ctx, "u", "a", 1)
	if err != nil || !reflect.DeepEqual(tail, events[1:]) {
		t.Fatalf("tail: %+v %v", tail, err)
	}
	if err := s.AppendEvent(ctx, "u", Event{Attempt: "missing", Seq: 1, Payload: json.RawMessage(`{}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, "u", Event{Attempt: "a", Seq: 3, Payload: json.RawMessage(`invalid`)}); err == nil {
		t.Fatal("invalid JSON stored")
	}
	execute(t, s, `INSERT INTO attempts(id,task,actor,backend,model,effort,executor,ownership_generation,engine_sha256,run_id,native_session,state,started,ended,mode) SELECT 'b',task,actor,backend,model,effort,executor,ownership_generation,engine_sha256,run_id,native_session,state,started,ended,mode FROM attempts WHERE id='a'`)
	if err := s.AppendEvent(ctx, "u", Event{Attempt: "b", Seq: 1, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentEventAppendsHaveExactlyOneWinner(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	other, err := Open(ctx, s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store := s
			if i%2 == 0 {
				store = other
			}
			err := store.AppendEvent(ctx, "u", Event{Attempt: "a", Seq: 1, Payload: json.RawMessage(`{}`)})
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrEventSequence) {
				t.Errorf("loser: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("winners = %d", wins.Load())
	}
}

func TestBackupRestoresAllCommittedRowsFromALiveWALDatabase(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	if err := s.AppendEvent(ctx, "u", Event{Attempt: "a", Seq: 1, Time: "now", Kind: "output", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Do(ctx, "u", "key", "hash", nil, func(tx *Tx) ([]byte, error) { return []byte("response"), nil }); err != nil {
		t.Fatal(err)
	}
	// Keep a write transaction open during the snapshot. Its uncommitted row
	// must be excluded while every committed fixture row remains visible.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO projects VALUES('pending','name','source','created')"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "backup's %#.sqlite")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for _, table := range []string{"projects", "tasks", "attempts", "events", "idempotency"} {
		var original, copy string
		// Each fixture has one row. Compare every column, including NULL and blobs.
		read := func(db *sql.DB) string {
			rows, err := db.Query("SELECT * FROM " + table)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			columns, _ := rows.Columns()
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if !rows.Next() {
				t.Fatal("missing " + table)
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("%#v", values)
		}
		original = read(s.db)
		copy = read(restored.db)
		if original != copy {
			t.Fatalf("%s: %s != %s", table, original, copy)
		}
	}
	if err := s.Backup(ctx, dest); err == nil {
		t.Fatal("overwrote backup")
	}
	var pending int
	if err := restored.db.QueryRow("SELECT count(*) FROM projects WHERE id='pending'").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("uncommitted backup row: %d %v", pending, err)
	}
}

func TestKilledWriterLeavesAnAtomicAndIntegralDatabase(t *testing.T) {
	s := openTestStore(t)
	path := s.path
	seed(t, s)
	s.Close()
	signal := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperInterruptedWriter$")
	cmd.Env = append(os.Environ(), "ADA_STORE_HELPER=1", "ADA_STORE_PATH="+path, "ADA_STORE_SIGNAL="+signal)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	t.Cleanup(func() {
		if !waited {
			cmd.Process.Kill()
			<-done
		}
	})
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			waited = true
			t.Fatalf("helper exited before signal: %v", err)
		case <-deadline.C:
			t.Fatal("helper never signaled")
		case <-ticker.C:
			if _, err := os.Stat(signal); err == nil {
				goto kill
			}
		}
	}
kill:
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	waited = true
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.db.QueryRow("SELECT count(*) FROM projects WHERE id LIKE 'crash-%'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial write: %d %v", count, err)
	}
	var integrity string
	if err := reopened.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity: %s %v", integrity, err)
	}
}

func TestHelperInterruptedWriter(t *testing.T) {
	if os.Getenv("ADA_STORE_HELPER") != "1" {
		return
	}
	s, err := Open(ctx, os.Getenv("ADA_STORE_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA cache_size=1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"crash-1", "crash-2"} {
		if _, err := tx.ExecContext(ctx, "INSERT INTO projects VALUES(?,?,'source','created')", id, strings.Repeat("x", 128*1024)); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(os.Getenv("ADA_STORE_PATH") + "-wal"); err != nil || info.Size() <= 32 {
		t.Fatalf("uncommitted pages did not reach WAL: %v", err)
	}
	if err := os.WriteFile(os.Getenv("ADA_STORE_SIGNAL"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}
