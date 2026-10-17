package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/api"
	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func oversizePayload(fill string) []byte {
	return []byte(`{"text":"` + strings.Repeat(fill, 256<<10) + `"}`)
}

func TestReferencedEventPayloadCarriesTheArtifactMarker(t *testing.T) {
	e, s, _, _ := testExecutor(t, "happy")
	a, err := s.AttemptByID(context.Background(), "attempt")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.append(a, "event", oversizePayload("m"), ""); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(context.Background(), "u", "attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(events[0].Payload, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["payload_ref"] != "artifact" || len(stored) != 3 {
		t.Fatalf("stored payload %s lacks the payload_ref marker", events[0].Payload)
	}
}

func blobPath(dir string, content []byte) string {
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	return filepath.Join(dir, "artifacts", hash[:2], hash)
}

func TestAFailedEventTransactionLeavesNoUnreferencedArtifact(t *testing.T) {
	e, s, dir, _ := testExecutor(t, "happy")
	ctx := context.Background()
	a, err := s.AttemptByID(ctx, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	shared := oversizePayload("s")
	if err := e.append(a, "event", shared, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BumpAndRecover(ctx); err != nil {
		t.Fatal(err)
	}
	orphan := oversizePayload("o")
	if err := e.append(a, "event", orphan, ""); !errors.Is(err, store.ErrStaleOwnership) {
		t.Fatalf("append after the generation moved on: %v", err)
	}
	if _, err := os.Stat(blobPath(dir, orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the unreferenced blob remains: %v", err)
	}
	if err := e.append(a, "event", shared, ""); !errors.Is(err, store.ErrStaleOwnership) {
		t.Fatalf("append of shared content: %v", err)
	}
	if _, err := os.Stat(blobPath(dir, shared)); err != nil {
		t.Fatalf("the blob a committed record references was removed: %v", err)
	}
	events, err := s.Events(ctx, "u", "attempt", 0)
	if err != nil || len(events) != 2 {
		t.Fatalf("events %d: %v", len(events), err)
	}
}

func TestAFailedResultTransactionRemovesTheFinalArtifact(t *testing.T) {
	e, s, dir, _ := testExecutor(t, "happy")
	ctx := context.Background()
	a, err := s.AttemptByID(ctx, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BumpAndRecover(ctx); err != nil {
		t.Fatal(err)
	}
	final := strings.Repeat("f", 100<<10)
	var m message
	m.Result.Status, m.Result.Final = "ok", final
	line, _ := json.Marshal(map[string]any{"v": 1, "seq": 6, "type": "result", "result": map[string]any{"status": "ok", "final": final}})
	if err := e.result(a, m, line); !errors.Is(err, store.ErrStaleOwnership) {
		t.Fatalf("result after the generation moved on: %v", err)
	}
	if _, err := os.Stat(blobPath(dir, []byte(final))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the final blob remains: %v", err)
	}
}

func TestTheExecutorReferencesEveryEventTheAPIWouldRefuseToEncode(t *testing.T) {
	e, s, _, _ := testExecutor(t, "happy")
	ctx := context.Background()
	a, err := s.AttemptByID(ctx, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	const when = "2026-10-15T12:00:00.123456789Z"
	event := func(seq int64, n int) store.Event {
		return store.Event{Attempt: a.ID, Seq: seq, Time: when, Kind: "event", Payload: []byte(`{"text":"` + strings.Repeat("x", n) + `"}`)}
	}
	// The sequence number is assigned inside the transaction, so the executor
	// decides with the widest one an event can carry.
	limit := sort.Search(1<<20, func(n int) bool { return api.EventExceedsLimit(event(math.MaxInt64, n)) })
	inline, ref, err := e.boundEvent(event(0, limit-1))
	if err != nil || ref.Hash != "" || !bytes.Equal(inline.Payload, event(0, limit-1).Payload) {
		t.Fatalf("one byte under the limit: ref %+v, %v", ref, err)
	}
	referenced, ref, err := e.boundEvent(event(0, limit))
	if err != nil || ref.Hash == "" || !bytes.Equal(referenced.Payload, api.ReferencedPayload(ref.Hash, ref.Size)) {
		t.Fatalf("at the limit: ref %+v, payload %.80s, %v", ref, referenced.Payload, err)
	}
	if ref.Size != int64(len(event(0, limit).Payload)) {
		t.Fatalf("referenced size %d", ref.Size)
	}
	refs := []artifact.Ref{{}, ref}
	for i, stored := range []store.Event{inline, referenced} {
		stored.Seq = int64(i + 1)
		if err := e.owned(a.ID, func(tx *store.Tx) error { return e.appendBoundEvent(tx, stored, refs[i]) }); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(api.New(s, api.NewHub(), func(*http.Request) (string, error) { return "u", nil }))
	defer server.Close()
	resp, err := http.Get(server.URL + "/v1/attempts/attempt/events")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(body, inline.Payload) || !bytes.Contains(body, referenced.Payload) {
		t.Fatalf("page: %d %.80s", resp.StatusCode, body)
	}
}

// storedBlobs lists the published blobs under the executor's artifact store,
// leaving out the temporary directory.
func storedBlobs(t *testing.T, dir string) []string {
	t.Helper()
	var blobs []string
	root := filepath.Join(dir, "artifacts")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "tmp" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			blobs = append(blobs, filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(blobs)
	return blobs
}

func TestADiscardNeverRemovesContentAnotherWriterReusesMeanwhile(t *testing.T) {
	e, s, dir, _ := testExecutor(t, "happy")
	ctx := context.Background()
	a, err := s.AttemptByID(ctx, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	shared := oversizePayload("r")
	var otherErr error
	done := make(chan struct{})
	e.beforeRemove = func(string) {
		// The other writer appends the same content to a live attempt while
		// this one stands between its reference check and its removal. Under
		// the lifecycle lock it cannot finish before the removal, so the wait
		// runs out; without the lock it commits a reference to content the
		// removal then deletes.
		go func() { otherErr = e.append(a, "event", shared, ""); close(done) }()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
		}
	}
	if err := e.append(store.Attempt{ID: "missing"}, "event", shared, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("append to a missing attempt: %v", err)
	}
	<-done
	if otherErr != nil {
		t.Fatalf("the other writer: %v", otherErr)
	}
	sum := sha256.Sum256(shared)
	hash := hex.EncodeToString(sum[:])
	if referenced, err := s.ArtifactReferenced(ctx, hash); err != nil || !referenced {
		t.Fatalf("referenced=%v: %v", referenced, err)
	}
	if content, err := os.ReadFile(blobPath(dir, shared)); err != nil || !bytes.Equal(content, shared) {
		t.Fatalf("the blob the other writer references: %d bytes, %v", len(content), err)
	}
	events, err := s.Events(ctx, "u", "attempt", 0)
	if err != nil || len(events) != 1 || !bytes.Equal(events[0].Payload, api.ReferencedPayload(hash, int64(len(shared)))) {
		t.Fatalf("events %d: %v", len(events), err)
	}
}

func TestADiscardNeverRemovesContentAFailingAttemptRecordsMeanwhile(t *testing.T) {
	e, s, dir, _ := testExecutor(t, "happy")
	ctx := context.Background()
	a, err := s.AttemptByID(ctx, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	// The failing attempt's cause is not bounded: an oversized line the engine
	// sent, or an error enlarged by a later exit status, is recorded through
	// failFinal's in-transaction append, which references its payload exactly
	// as the first writer's content was written.
	cause := errors.New(strings.Repeat("r", 256<<10))
	shared := []byte(`{"error":"` + cause.Error() + `"}`)
	done := make(chan struct{})
	e.beforeRemove = func(string) {
		go func() { e.failFinal(a, cause, false, false); close(done) }()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
		}
	}
	if err := e.append(store.Attempt{ID: "missing"}, "event", shared, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("append to a missing attempt: %v", err)
	}
	<-done
	sum := sha256.Sum256(shared)
	hash := hex.EncodeToString(sum[:])
	if referenced, err := s.ArtifactReferenced(ctx, hash); err != nil || !referenced {
		t.Fatalf("referenced=%v: %v", referenced, err)
	}
	if content, err := os.ReadFile(blobPath(dir, shared)); err != nil || !bytes.Equal(content, shared) {
		t.Fatalf("the blob the failing attempt references: %d bytes, %v", len(content), err)
	}
	events, err := s.Events(ctx, "u", "attempt", 0)
	if err != nil || len(events) != 1 || events[0].Kind != "executor_error" || !bytes.Equal(events[0].Payload, api.ReferencedPayload(hash, int64(len(shared)))) {
		t.Fatalf("events %d: %v", len(events), err)
	}
	if got, err := s.AttemptByID(ctx, a.ID); err != nil || got.State != "ended_failed" {
		t.Fatalf("attempt state %q: %v", got.State, err)
	}
}

func TestAFailureAfterTheReferenceIsRecordedRemovesTheRolledBackBlob(t *testing.T) {
	e, s, dir, _ := testExecutor(t, "happy")
	ctx := context.Background()
	a, err := s.AttemptByID(ctx, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(ctx, "u", "fault", "fault", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "CREATE TRIGGER fault BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'injected event fault'); END")
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := oversizePayload("t")
	if err := e.append(a, "event", payload, ""); err == nil || !strings.Contains(err.Error(), "injected event fault") {
		t.Fatalf("append with the event insert failing: %v", err)
	}
	sum := sha256.Sum256(payload)
	if referenced, err := s.ArtifactReferenced(ctx, hex.EncodeToString(sum[:])); err != nil || referenced {
		t.Fatalf("the rolled-back reference remains: referenced=%v, %v", referenced, err)
	}
	if blobs := storedBlobs(t, dir); len(blobs) != 0 {
		t.Fatalf("blobs remain: %v", blobs)
	}
}

func TestAFailedResultRemovesItsFinalAndEventBlobsAndKeepsSharedContent(t *testing.T) {
	e, s, dir, _ := testExecutor(t, "happy")
	ctx := context.Background()
	a, err := s.AttemptByID(ctx, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	shared := oversizePayload("s")
	if err := e.append(a, "event", shared, ""); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(shared)
	want := []string{hex.EncodeToString(sum[:])}
	if _, _, err := s.BumpAndRecover(ctx); err != nil {
		t.Fatal(err)
	}
	// Each line is over the event limit on its own, so a failed result leaves
	// a final blob and an event blob to discard.
	notes := strings.Repeat("n", 256<<10)
	for _, final := range []string{strings.Repeat("f", 100<<10), string(shared)} {
		var m message
		m.Result.Status, m.Result.Final = "ok", final
		line, _ := json.Marshal(map[string]any{"v": 1, "seq": 6, "type": "result", "result": map[string]any{"status": "ok", "final": final, "notes": notes}})
		if err := e.result(a, m, line); !errors.Is(err, store.ErrStaleOwnership) {
			t.Fatalf("result after the generation moved on: %v", err)
		}
		if blobs := storedBlobs(t, dir); !reflect.DeepEqual(blobs, want) {
			t.Fatalf("blobs after a failed result with a %d-byte final: %v, want %v", len(final), blobs, want)
		}
	}
}

func TestTheSweepRemovesAnOrphanFromACrashAndKeepsAReferencedBlob(t *testing.T) {
	e, s, dir, _ := testExecutor(t, "happy")
	ctx := context.Background()
	a, err := s.AttemptByID(ctx, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	kept := oversizePayload("k")
	if err := e.append(a, "event", kept, ""); err != nil {
		t.Fatal(err)
	}
	// A crash between the publish and the commit leaves the blob and no row.
	orphan := oversizePayload("c")
	if _, err := e.cfg.Artifacts.Write(ctx, bytes.NewReader(orphan), int64(len(orphan))); err != nil {
		t.Fatal(err)
	}
	removed, err := e.RemoveUnreferencedArtifacts(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("removed %d: %v", removed, err)
	}
	if _, err := os.Stat(blobPath(dir, orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the orphan remains: %v", err)
	}
	if content, err := os.ReadFile(blobPath(dir, kept)); err != nil || !bytes.Equal(content, kept) {
		t.Fatalf("the referenced blob: %d bytes, %v", len(content), err)
	}
}

// pausedReader yields its first chunk, then closes paused and waits for
// release before it yields the rest. Only the reading goroutine touches chunks
// and waited.
type pausedReader struct {
	chunks  [][]byte
	paused  chan struct{}
	release chan struct{}
	waited  bool
}

func (r *pausedReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	if len(r.chunks) == 1 && !r.waited {
		r.waited = true
		close(r.paused)
		<-r.release
	}
	n := copy(p, r.chunks[0])
	if r.chunks[0] = r.chunks[0][n:]; len(r.chunks[0]) == 0 {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

func TestTheSweepNeverTouchesABlobBeingWritten(t *testing.T) {
	e, _, dir, _ := testExecutor(t, "happy")
	ctx := context.Background()
	content := oversizePayload("w")
	reader := &pausedReader{chunks: [][]byte{content[:1024], content[1024:]}, paused: make(chan struct{}), release: make(chan struct{})}
	type written struct {
		ref artifact.Ref
		err error
	}
	done := make(chan written, 1)
	go func() {
		ref, err := e.cfg.Artifacts.Write(ctx, reader, int64(len(content)))
		done <- written{ref, err}
	}()
	<-reader.paused
	removed, err := e.RemoveUnreferencedArtifacts(ctx)
	if err != nil || removed != 0 {
		t.Fatalf("removed %d: %v", removed, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "artifacts", "tmp"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("the temporary being written: %v, %v", entries, err)
	}
	close(reader.release)
	result := <-done
	if result.err != nil {
		t.Fatalf("the write after the sweep: %v", result.err)
	}
	if got, err := os.ReadFile(blobPath(dir, content)); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("the blob written across the sweep: %d bytes, %v", len(got), err)
	}
}

func TestTheSweepWaitsForAWriterHoldingTheLifecycleLock(t *testing.T) {
	e, s, dir, _ := testExecutor(t, "happy")
	ctx := context.Background()
	a, err := s.AttemptByID(ctx, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	content := oversizePayload("l")
	type swept struct {
		removed int
		err     error
	}
	sweepDone := make(chan swept, 1)
	err = e.artifactLifecycle(func() error {
		ref, err := e.cfg.Artifacts.Write(ctx, bytes.NewReader(content), int64(len(content)))
		if err != nil {
			return err
		}
		go func() {
			removed, err := e.RemoveUnreferencedArtifacts(ctx)
			sweepDone <- swept{removed, err}
		}()
		// Under the lock the sweep cannot finish before the commit, so the
		// wait runs out; without it the sweep removes the uncommitted blob.
		select {
		case result := <-sweepDone:
			t.Errorf("the sweep finished while a writer held the lock: %+v", result)
			sweepDone <- result
		case <-time.After(300 * time.Millisecond):
		}
		return e.owned(a.ID, func(tx *store.Tx) error {
			return tx.ReferenceArtifact(ctx, ref.Hash, ref.Size, "attempt:attempt:event:1")
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	result := <-sweepDone
	if result.err != nil || result.removed != 0 {
		t.Fatalf("removed %d: %v", result.removed, result.err)
	}
	if got, err := os.ReadFile(blobPath(dir, content)); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("the committed blob: %d bytes, %v", len(got), err)
	}
}

// signalLock tells the test when the snapshot handler takes the lifecycle lock.
type signalLock struct {
	*artifact.Store
	locking chan struct{}
}

func (l signalLock) Lock() {
	close(l.locking)
	l.Store.Lock()
}

// sameContent captures one untracked file holding the given content.
type sameContent struct {
	src     store.ProjectSource
	content []byte
}

func (c sameContent) Capture(context.Context, string, source.Selection) (source.Capture, error) {
	identity := source.Identity{Path: c.src.Path, CommonDir: c.src.CommonDir}
	return source.Capture{Inspection: source.Inspection{Identity: identity, Fingerprint: c.src.Fingerprint}, Untracked: []source.File{{Path: "same.json", Mode: "100644", Content: c.content}}}, nil
}

func TestADiscardNeverRemovesContentASnapshotCommitsMeanwhile(t *testing.T) {
	e, s, dir, _ := testExecutor(t, "happy")
	ctx := context.Background()
	shared := oversizePayload("s")
	var src store.ProjectSource
	_, err := s.Do(ctx, "u", "import", "import", nil, func(tx *store.Tx) ([]byte, error) {
		var err error
		src, err = tx.ImportProject(ctx, "imported", "imported", store.ProjectSource{Path: "/repo", CommonDir: "/repo/.git", Fingerprint: "f"})
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	lock := signalLock{e.cfg.Artifacts.(*artifact.Store), make(chan struct{})}
	server := httptest.NewServer(api.New(s, api.NewHub(), func(*http.Request) (string, error) { return "u", nil }, api.Execution{Capturer: sameContent{src, shared}, Writer: lock}))
	defer server.Close()
	type response struct {
		status int
		body   []byte
		err    error
	}
	snapshot := make(chan response, 1)
	e.beforeRemove = func(string) {
		// A snapshot stores the same content while this discard stands
		// between its reference check and its removal: it waits at the
		// shared lock, or without one commits before the removal.
		go func() {
			req, _ := http.NewRequest("POST", server.URL+"/v1/projects/imported/snapshots", strings.NewReader(`{"untracked":["same.json"]}`))
			req.Header.Set("Idempotency-Key", "snap")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				snapshot <- response{err: err}
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			snapshot <- response{resp.StatusCode, body, err}
		}()
		select {
		case <-lock.locking:
			// The handler now waits at the lock this discard holds, so
			// the wait runs out; a private executor lock lets it commit.
			select {
			case r := <-snapshot:
				t.Errorf("the snapshot committed while the discard held the lock: %d %s", r.status, r.body)
				snapshot <- r
			case <-time.After(300 * time.Millisecond):
			}
		case r := <-snapshot:
			snapshot <- r
		}
	}
	if err := e.append(store.Attempt{ID: "missing"}, "event", shared, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("append to a missing attempt: %v", err)
	}
	r := <-snapshot
	if r.err != nil || r.status != 201 {
		t.Fatalf("snapshot = %d %s: %v", r.status, r.body, r.err)
	}
	if content, err := os.ReadFile(blobPath(dir, shared)); err != nil || !bytes.Equal(content, shared) {
		t.Fatalf("the blob the snapshot references: %d bytes, %v", len(content), err)
	}
}
