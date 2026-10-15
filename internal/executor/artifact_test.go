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
