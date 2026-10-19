package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/store"
)

func TestAFirstReservationAgainstAnExhaustedBudgetEndsAtTheWriteDeadline(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			f := newFixture(t)
			attemptForLimits(t, f)
			f.hub.BudgetBytes = pageBytes
			f.hub.WriteDeadline = 100 * time.Millisecond
			if !f.hub.reserveBytes(context.Background(), pageBytes) {
				t.Fatal("initial reservation")
			}
			defer f.hub.releaseBytes(pageBytes)
			h := &handler{store: f.s, hub: f.hub, auth: func(*http.Request) (string, error) { return "alice", nil }}
			h.readLast = func(context.Context, string, string) (int64, error) {
				t.Error("last sequence read without a reservation")
				return 0, errors.New("unreserved read")
			}
			req := httptest.NewRequest("GET", "/v1/attempts/limits-attempt/events", nil)
			req.SetPathValue("id", "limits-attempt")
			if stream {
				req.Header.Set("Accept", "text/event-stream")
			}
			w := httptest.NewRecorder()
			done := make(chan struct{})
			start := time.Now()
			go func() { h.events(w, req); close(done) }()
			select {
			case <-done:
			case <-time.After(f.hub.WriteDeadline + 2*time.Second):
				t.Fatal("the reservation outlived the write deadline")
			}
			if waited := time.Since(start); waited < f.hub.WriteDeadline {
				t.Fatalf("refused after %v, before the %v deadline", waited, f.hub.WriteDeadline)
			}
			if w.Code != 503 || !bytes.Contains(w.Body.Bytes(), []byte(`"error":"busy"`)) || w.Header().Get("Retry-After") != "2" {
				t.Fatalf("busy response: %d %s, Retry-After=%q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
			}
			hubAccounting(t, f.hub, pageBytes, 0)
			f.hub.mu.Lock()
			streams := f.hub.streamTotal
			f.hub.mu.Unlock()
			if streams != 0 {
				t.Fatalf("stream slots held: %d", streams)
			}
		})
	}
}

// buildRefused reports whether New refused a hub with this budget.
func buildRefused(budget int) (refused bool) {
	defer func() { refused = recover() != nil }()
	hub := NewHub()
	hub.BudgetBytes = budget
	New(nil, hub, func(*http.Request) (string, error) { return "alice", nil })
	return false
}

func TestABudgetBelowOnePageIsRefusedWhenTheServerIsBuilt(t *testing.T) {
	for _, tc := range []struct {
		budget  int
		refused bool
	}{{0, false}, {1, true}, {pageBytes - 1, true}, {pageBytes, false}} {
		if got := buildRefused(tc.budget); got != tc.refused {
			t.Errorf("budget %d: refused=%v, want %v", tc.budget, got, tc.refused)
		}
	}
	if got := (&Hub{}).capacity(); got != 32<<20 {
		t.Fatalf("a zero budget means %d bytes, want the 32 MiB default", got)
	}
}

func serveArtifact(t *testing.T, f *fixture, opener ArtifactOpener, refs ArtifactReferences, hash string) (*http.Response, []byte, error) {
	t.Helper()
	server := httptest.NewServer(New(f.s, f.hub, func(*http.Request) (string, error) { return "alice", nil }, Execution{Artifacts: opener, References: refs}))
	defer server.Close()
	return getBody(t, server.URL+"/v1/attempts/limits-attempt/artifacts/"+hash)
}

func chunks(raw []byte) []readStep {
	var steps []readStep
	for len(raw) != 0 {
		n := min(len(raw), 32<<10)
		steps = append(steps, readStep{raw[:n], nil})
		raw = raw[n:]
	}
	return steps
}

func TestAnArtifactOfAnExactMultipleOfTheReadBufferIsServedWhole(t *testing.T) {
	for _, count := range []int{1, 3} {
		for _, form := range []string{"the store's reader", "EOF on a read of its own", "EOF with the last chunk"} {
			t.Run(fmt.Sprintf("%d chunks, %s", count, form), func(t *testing.T) {
				f := newFixture(t)
				attemptForLimits(t, f)
				raw := bytes.Repeat([]byte("0123456789abcdef"), count*(32<<10)/16)
				stored, refs, ref := storedPayload(t, f, raw)
				var opener ArtifactOpener = stored
				switch steps := chunks(raw); form {
				case "EOF on a read of its own":
					opener = scriptedOpener{append(steps, readStep{nil, io.EOF})}
				case "EOF with the last chunk":
					steps[len(steps)-1].err = io.EOF
					opener = scriptedOpener{steps}
				}
				resp, body, err := serveArtifact(t, f, opener, refs, ref.Hash)
				if resp.StatusCode != 200 || err != nil || resp.ContentLength != int64(len(raw)) || !bytes.Equal(body, raw) {
					t.Fatalf("%d, length %d: %d of %d bytes: %v", resp.StatusCode, resp.ContentLength, len(body), len(raw), err)
				}
			})
		}
	}
}

// stallingReader answers its steps and then (0, nil) for ever, counting reads.
type stallingReader struct {
	steps []readStep
	reads *atomic.Int32
}

func (r *stallingReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	if len(r.steps) == 0 {
		return 0, nil
	}
	step := r.steps[0]
	r.steps = r.steps[1:]
	return copy(p, step.data), step.err
}

func (r *stallingReader) Close() error { return nil }

type stallingOpener struct {
	steps []readStep
	reads *atomic.Int32
}

func (o stallingOpener) Open(string) (io.ReadCloser, error) {
	return &stallingReader{steps: append([]readStep(nil), o.steps...), reads: o.reads}, nil
}

func TestAnArtifactReaderThatMakesNoProgressStopsAfterAHundredEmptyReads(t *testing.T) {
	raw := bytes.Repeat([]byte("abcdefgh"), 8<<10) // 64 KiB, two read buffers
	empties := func(n int) []readStep { return make([]readStep, n) }
	cases := []struct {
		name   string
		steps  []readStep
		status int
		reads  int32
		sent   int
	}{
		{"before any byte", nil, 500, 100, 0},
		{"after the response started", chunks(raw), 200, 2 + 100, 32 << 10},
		{"progress resets the count", append(append(append(empties(99), readStep{raw[:32<<10], nil}), empties(99)...), readStep{raw[32<<10:], io.EOF}), 200, 99 + 1 + 99 + 1, len(raw)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			attemptForLimits(t, f)
			_, refs, ref := storedPayload(t, f, raw)
			reads := &atomic.Int32{}
			resp, body, err := serveArtifact(t, f, stallingOpener{tc.steps, reads}, refs, ref.Hash)
			if resp.StatusCode != tc.status || reads.Load() != tc.reads {
				t.Fatalf("status %d after %d reads, want %d after %d", resp.StatusCode, reads.Load(), tc.status, tc.reads)
			}
			if tc.status == 200 && (len(body) != tc.sent || (tc.sent < len(raw)) != errors.Is(err, io.ErrUnexpectedEOF) || !bytes.Equal(body, raw[:len(body)])) {
				t.Fatalf("%d of %d bytes, want %d: %v", len(body), len(raw), tc.sent, err)
			}
		})
	}
}

func TestABlobLongerThanItsReferenceIsNeverServedWhole(t *testing.T) {
	raw := bytes.Repeat([]byte("abcdefgh"), 8<<10) // 64 KiB, two read buffers
	small := raw[:24<<10]
	cases := []struct {
		name      string
		ref       []byte
		steps     []readStep
		status    int
		committed int // bytes the client holds when the 200 was committed
	}{
		{"excess in the first read", small, []readStep{{append(bytes.Clone(small), 'x'), nil}}, 500, 0},
		{"excess with EOF before any write", small, []readStep{{small, nil}, {[]byte("x"), io.EOF}}, 500, 0},
		{"excess after the 200 was committed", raw, append(chunks(raw), readStep{[]byte("x"), nil}), 200, 32 << 10},
		{"excess with EOF after the 200 was committed", raw, append(chunks(raw), readStep{[]byte("x"), io.EOF}), 200, 32 << 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			attemptForLimits(t, f)
			_, refs, ref := storedPayload(t, f, tc.ref)
			resp, body, err := serveArtifact(t, f, scriptedOpener{tc.steps}, refs, ref.Hash)
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d: %d bytes", resp.StatusCode, tc.status, len(body))
			}
			if tc.status == 500 && !bytes.Contains(body, []byte(`"error":"internal"`)) {
				t.Fatalf("refusal body %q", body)
			}
			if tc.status == 200 && (len(body) != tc.committed || !errors.Is(err, io.ErrUnexpectedEOF) || resp.ContentLength != int64(len(tc.ref))) {
				t.Fatalf("%d bytes of Content-Length %d, want %d short: %v", len(body), resp.ContentLength, tc.committed, err)
			}
		})
	}
	t.Run("the store's own blob grown behind it", func(t *testing.T) {
		f := newFixture(t)
		attemptForLimits(t, f)
		opener, refs, ref := storedPayload(t, f, raw)
		opener.overwriteBlob(t, ref.Hash, append(bytes.Clone(raw), 'x'))
		resp, body, err := serveArtifact(t, f, opener, refs, ref.Hash)
		if resp.StatusCode == 200 && (err == nil || len(body) >= len(raw)) {
			t.Fatalf("grown blob served whole: %d bytes: %v", len(body), err)
		}
	})
}

// drainWriter is one stream's response: a healthy one records frames, a
// stalled one blocks every event frame until its write deadline passes.
type drainWriter struct {
	header   http.Header
	stalled  bool
	opened   chan struct{} // closed once the retry line is written
	reason   chan struct{} // closed once the stop's frame is written
	stalling chan struct{} // closed when a stalled write begins
	timedOut chan struct{} // closed when a stalled write hits its deadline
	mu       sync.Mutex
	body     bytes.Buffer
	deadline time.Time
}

var stopFrame = []byte("id: 1\nevent: cancel\ndata: {\"reason\":\"owner stop\"}\n\n")

func newDrainWriter(stalled bool) *drainWriter {
	return &drainWriter{header: http.Header{}, stalled: stalled, opened: make(chan struct{}), reason: make(chan struct{}), stalling: make(chan struct{}), timedOut: make(chan struct{})}
}

func (w *drainWriter) Header() http.Header { return w.header }
func (w *drainWriter) WriteHeader(int)     {}
func (w *drainWriter) Flush()              {}

func (w *drainWriter) SetWriteDeadline(d time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = d
	return nil
}

func (w *drainWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	deadline := w.deadline
	w.mu.Unlock()
	if w.stalled && bytes.HasPrefix(p, []byte("id: ")) {
		close(w.stalling)
		<-time.After(time.Until(deadline)) // a client that never reads: the connection's deadline ends the write
		close(w.timedOut)
		return 0, os.ErrDeadlineExceeded
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.body.Write(p)
	if bytes.HasPrefix(p, []byte("retry: ")) {
		close(w.opened)
	}
	if bytes.Equal(p, stopFrame) {
		close(w.reason)
	}
	return len(p), nil
}

func TestADrainUnderBudgetContentionReachesEveryHealthyStreamDespiteAStalledOne(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	hub := f.hub
	hub.PollInterval, hub.HeartbeatInterval = time.Hour, time.Hour // only the drain sends
	hub.BudgetBytes = pageBytes + 4096                             // one reader's page at a time for sixteen streams
	hub.WriteDeadline = 500 * time.Millisecond
	h := &handler{store: f.s, hub: hub, auth: func(*http.Request) (string, error) { return "alice", nil }}
	writers := make([]*drainWriter, 16) // a user's whole allowance; the last never reads
	ended := make([]chan struct{}, len(writers))
	open := func(i int) {
		writers[i], ended[i] = newDrainWriter(i == len(writers)-1), make(chan struct{})
		req := httptest.NewRequest("GET", "/v1/attempts/limits-attempt/events", nil)
		req.SetPathValue("id", "limits-attempt")
		req.Header.Set("Accept", "text/event-stream")
		go func() { h.events(writers[i], req); close(ended[i]) }()
	}
	for i := range writers[:len(writers)-1] {
		open(i)
	}
	for i, w := range writers[:len(writers)-1] {
		select {
		case <-w.opened:
		case <-time.After(5 * time.Second):
			t.Fatalf("stream %d did not open", i)
		}
	}
	// Committed without a notification, as the stop's transaction is.
	if err := f.s.AppendEvent(context.Background(), "alice", store.Event{Attempt: "limits-attempt", Seq: 1, Kind: "cancel", Payload: json.RawMessage(`{"reason":"owner stop"}`)}); err != nil {
		t.Fatal(err)
	}
	// The stalled stream opens last, so its first send reads the stop and
	// holds its page reservation through a stalled write before the drain
	// wakes any healthy stream: it is never the one served last.
	open(len(writers) - 1)
	stalled := writers[len(writers)-1]
	select {
	case <-stalled.stalling:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled stream never began its write")
	}
	stalled.mu.Lock()
	stalledUntil := stalled.deadline
	stalled.mu.Unlock()
	type drain struct {
		took  time.Duration
		ended int // healthy streams already ended when Drain returned
	}
	drained := make(chan drain, 1)
	start := time.Now()
	go func() {
		hub.Drain()
		d := drain{took: time.Since(start)}
		for _, e := range ended[:len(ended)-1] {
			select {
			case <-e:
				d.ended++
			default:
			}
		}
		drained <- d
	}()
	// Every healthy stream is served while the stalled write still holds its
	// reservation: a full page held there would leave them no budget.
	within := time.After(time.Until(stalledUntil))
	for i, w := range writers[:len(writers)-1] {
		select {
		case <-w.reason:
		case <-within:
			t.Fatalf("stream %d had no stop reason while the stalled write held its reservation", i)
		}
		select {
		case <-ended[i]:
		case <-within:
			t.Fatalf("stream %d did not end while the stalled write held its reservation", i)
		}
	}
	select {
	case <-ended[len(writers)-1]:
	case <-time.After(2*hub.WriteDeadline + time.Second):
		t.Fatal("the stalled stream was never disconnected")
	}
	select {
	case <-stalled.timedOut:
	default:
		t.Fatal("the stalled stream ended without its write reaching the deadline")
	}
	select {
	case d := <-drained:
		if d.took > hub.WriteDeadline+250*time.Millisecond || d.ended != len(writers)-1 {
			t.Fatalf("drain took %v (deadline %v) and returned with %d of %d healthy streams ended", d.took, hub.WriteDeadline, d.ended, len(writers)-1)
		}
	case <-time.After(hub.WriteDeadline + time.Second):
		t.Fatal("drain did not return")
	}
	waitHubReleased(t, hub, "alice", time.Now().Add(time.Second), pageBytes+4096)
}
