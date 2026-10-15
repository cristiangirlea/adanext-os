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
	"sort"
	"strings"
	"testing"

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
