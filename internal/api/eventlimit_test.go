package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/store"
)

func TestEventExceedsLimitIsExactlyTheLimitAPageRefuses(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	const when = "2026-10-15T12:00:00.123456789Z"
	event := func(seq int64, n int) store.Event {
		return store.Event{Attempt: "limits-attempt", Seq: seq, Time: when, Kind: "update", Payload: json.RawMessage(`{"text":"` + strings.Repeat("x", n) + `"}`)}
	}
	n := eventBytes - len(jsonEvent(event(1, 0)))
	fits, over := event(1, n), event(2, n+1)
	if len(jsonEvent(fits)) != eventBytes || len(jsonEvent(over)) != eventBytes+1 {
		t.Fatalf("encoded sizes %d and %d", len(jsonEvent(fits)), len(jsonEvent(over)))
	}
	if EventExceedsLimit(fits) || !EventExceedsLimit(over) {
		t.Fatalf("exceeds: fits=%v over=%v", EventExceedsLimit(fits), EventExceedsLimit(over))
	}
	server := httptest.NewServer(New(f.s, f.hub, func(*http.Request) (string, error) { return "alice", nil }))
	defer server.Close()
	if err := f.s.AppendEvent(context.Background(), "alice", fits); err != nil {
		t.Fatal(err)
	}
	resp, body := get(t, server.URL+"/v1/attempts/limits-attempt/events", "")
	if resp.StatusCode != 200 || !bytes.Contains(body, fits.Payload) {
		t.Fatalf("event at the limit: %d %.80s", resp.StatusCode, body)
	}
	if err := f.s.AppendEvent(context.Background(), "alice", over); err != nil {
		t.Fatal(err)
	}
	resp, body = get(t, server.URL+"/v1/attempts/limits-attempt/events?after=1", "")
	if resp.StatusCode != 500 {
		t.Fatalf("event over the limit: %d %.80s", resp.StatusCode, body)
	}
}

func TestLegacyReferencePayloadsWithoutTheMarkerReplayThroughJSONAndSSE(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	// Events stored before payload_ref existed carry only the artifact and
	// its size; both forms replay them as stored.
	legacy := json.RawMessage(`{"payload_artifact":"` + strings.Repeat("ab", 32) + `","payload_size":307200}`)
	if err := f.s.AppendEvent(context.Background(), "alice", store.Event{Attempt: "limits-attempt", Seq: 1, Time: "2026-10-15T12:00:00Z", Kind: "update", Payload: legacy}); err != nil {
		t.Fatal(err)
	}
	code, body := f.request(t, "GET", "/v1/attempts/limits-attempt/events", "", "", nil)
	var events []eventJSON
	if err := json.Unmarshal(body, &events); err != nil || code != 200 || len(events) != 1 || !bytes.Equal(events[0].Payload, legacy) {
		t.Fatalf("JSON: %d %s: %v", code, body, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://ipc/v1/attempts/limits-attempt/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	var frame []string
	for len(frame) < 3 {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream after %q: %v", frame, err)
		}
		if strings.HasPrefix(line, "id: ") || strings.HasPrefix(line, "event: ") || strings.HasPrefix(line, "data: ") {
			frame = append(frame, strings.TrimSuffix(line, "\n"))
		}
	}
	if resp.StatusCode != 200 || frame[0] != "id: 1" || frame[1] != "event: update" || frame[2] != "data: "+string(legacy) {
		t.Fatalf("SSE: %d %q", resp.StatusCode, frame)
	}
}
