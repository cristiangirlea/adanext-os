package api

import (
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
