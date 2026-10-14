package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/store"
)

func attemptForLimits(t *testing.T, f *fixture) {
	t.Helper()
	id := create(t, f, "limits-create", "work")["id"].(string)
	if err := f.s.CreateAttempt(context.Background(), store.Attempt{ID: "limits-attempt", Task: id, Actor: "alice", Backend: "codex", Executor: "local", State: "running"}); err != nil {
		t.Fatal(err)
	}
}

func TestEventsRejectMalformedAndAheadCursorsInBothForms(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	for _, cursor := range []string{"not-a-number", "9223372036854775808", "-1"} {
		code, _ := f.request(t, "GET", "/v1/attempts/limits-attempt/events?after="+cursor, "", "", nil)
		if code != 400 {
			t.Fatalf("cursor %q: %d", cursor, code)
		}
	}
	code, body := f.request(t, "GET", "/v1/attempts/limits-attempt/events?after=1", "", "", nil)
	if code != 409 || !bytes.Contains(body, []byte(`"error":"cursor_ahead"`)) || !bytes.Contains(body, []byte(`"last_sequence":0`)) {
		t.Fatalf("JSON: %d %s", code, body)
	}
	req, _ := http.NewRequest("GET", "http://ipc/v1/attempts/limits-attempt/events?after=1", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	stream, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !bytes.HasPrefix(stream, []byte("retry: 2000\n\n")) || bytes.Count(stream, []byte("event: cursor_ahead\n")) != 1 || !bytes.Contains(stream, []byte(`"last_sequence":0`)) || bytes.Contains(stream, []byte("id:")) {
		t.Fatalf("stream: %d %s", resp.StatusCode, stream)
	}
}

func TestSeveralLastEventIDsAreRejected(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	req, _ := http.NewRequest("GET", "http://ipc/v1/attempts/limits-attempt/events", nil)
	req.Header["Last-Event-Id"] = []string{"1", "2"}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatal(resp.StatusCode)
	}
}

func TestSeventeenthStreamForOneUserIsRefused(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	var responses []*http.Response
	defer func() {
		for _, r := range responses {
			r.Body.Close()
		}
	}()
	for i := 0; i < 17; i++ {
		req, _ := http.NewRequest("GET", "http://ipc/v1/attempts/limits-attempt/events", nil)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, resp)
		if i == 16 && resp.StatusCode != 429 {
			t.Fatalf("stream %d: %d", i+1, resp.StatusCode)
		}
	}
}

func TestTransitionAcceptsQuotedRevisionAndGetReturnsETag(t *testing.T) {
	f := newFixture(t)
	id := create(t, f, "etag-create", "work")["id"].(string)
	req, _ := http.NewRequest("GET", "http://ipc/v1/tasks/"+id, nil)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("ETag"); got != `"0"` {
		t.Fatalf("etag %q", got)
	}
	code, body := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "etag-ready", `"0"`, nil)
	if code != 200 {
		t.Fatalf("quoted match: %d %s", code, body)
	}
	code, _ = f.request(t, "POST", "/v1/tasks/"+id+"/ready", "etag-star", "*", nil)
	if code != 400 {
		t.Fatal(code)
	}
}

func TestConcurrentTransitionsWithDifferentKeysHaveOneWinner(t *testing.T) {
	f := newFixture(t)
	id := create(t, f, "race-create", "work")["id"].(string)
	var wg sync.WaitGroup
	results := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := f.request(t, "POST", "/v1/tasks/"+id+"/ready", fmt.Sprintf("race-%d", i), "0", nil)
			results <- code
		}(i)
	}
	wg.Wait()
	close(results)
	winners := 0
	for code := range results {
		if code == 200 {
			winners++
		} else if code != 409 {
			t.Errorf("transition: %d", code)
		}
	}
	if winners != 1 {
		t.Fatalf("winners: %d", winners)
	}
}

func TestInternalErrorsHaveARequestIDWithoutInternalDetail(t *testing.T) {
	f := newFixture(t)
	h := New(f.s, f.hub, func(*http.Request) (string, error) { return "", fmt.Errorf("secret detail") })
	readLog, writeLog, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = writeLog
	defer func() { os.Stderr = oldStderr; readLog.Close(); writeLog.Close() }()
	req, _ := http.NewRequest("GET", "/v1/tasks", nil)
	w := &captureResponse{header: make(http.Header)}
	h.ServeHTTP(w, req)
	var body map[string]any
	if err := json.Unmarshal(w.body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	id, valid := body["request_id"].(string)
	if w.status != 500 || !valid || id == "" || strings.Contains(w.body.String(), "secret detail") {
		t.Fatalf("%d %s", w.status, w.body.String())
	}
	writeLog.Close()
	log, err := io.ReadAll(readLog)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(log, []byte(id)) || !bytes.Contains(log, []byte("secret detail")) {
		t.Fatalf("log: %s", log)
	}
}

type captureResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *captureResponse) Header() http.Header         { return w.header }
func (w *captureResponse) WriteHeader(n int)           { w.status = n }
func (w *captureResponse) Write(b []byte) (int, error) { return w.body.Write(b) }

func TestZeroValueHubClosesWithoutPanic(t *testing.T) {
	var h Hub
	h.Close()
	h.Close()
	select {
	case <-h.done:
	default:
		t.Fatal("not closed")
	}
}

func TestHundredTwentyNinthStreamAcrossUsersIsRefused(t *testing.T) {
	h := NewHub()
	var release []func()
	for i := 0; i < 128; i++ {
		fn, ok := h.reserveStream(fmt.Sprintf("user-%d", i/16))
		if !ok {
			t.Fatalf("stream %d refused", i+1)
		}
		release = append(release, fn)
	}
	if _, ok := h.reserveStream("extra-user"); ok {
		t.Fatal("129th stream accepted")
	}
	for _, fn := range release {
		fn()
	}
}

func TestRemovedMemberStreamEnds(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	req, _ := http.NewRequest("GET", "http://ipc/v1/attempts/limits-attempt/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := f.s.RemoveMember(context.Background(), "a", "alice"); err != nil {
		t.Fatal(err)
	}
	f.hub.Notify("limits-attempt")
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("removed member stream remained open")
	}
}

func TestEncodedPageStopsBeforeOneMiBIncludingFraming(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	payload, _ := json.Marshal(strings.Repeat("x", 4090))
	for i := int64(1); i <= 256; i++ {
		if err := f.s.AppendEvent(context.Background(), "alice", store.Event{Attempt: "limits-attempt", Seq: i, Time: "2026-10-14T00:00:00Z", Kind: "update", Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	if len(payload)*256 >= pageBytes {
		t.Fatal("payloads should fit without framing")
	}
	h := &handler{store: f.s}
	for _, stream := range []bool{false, true} {
		events, page, err := h.page(context.Background(), "alice", "limits-attempt", 0, stream)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) >= 256 || len(page) > pageBytes {
			t.Fatalf("stream=%v: %d events, %d bytes", stream, len(events), len(page))
		}
		if len(events) == 0 {
			t.Fatal("empty first page")
		}
		next, _, err := h.page(context.Background(), "alice", "limits-attempt", events[len(events)-1].Seq, stream)
		if err != nil || len(events)+len(next) != 256 {
			t.Fatalf("stream=%v: %d+%d: %v", stream, len(events), len(next), err)
		}
	}
	code, body := f.request(t, "GET", "/v1/attempts/limits-attempt/events", "", "", nil)
	var first []eventJSON
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	if code != 200 || len(body) > pageBytes || len(first) >= 256 {
		t.Fatalf("HTTP first page: %d, %d bytes, %d events", code, len(body), len(first))
	}
}

func TestBudgetReservationPrecedesStoreRead(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	f.hub.BudgetBytes = pageBytes
	if !f.hub.reserveBytes(context.Background(), pageBytes) {
		t.Fatal("initial reservation")
	}
	started := make(chan struct{})
	h := &handler{store: f.s, hub: f.hub, auth: func(*http.Request) (string, error) { return "alice", nil }}
	h.readPage = func(ctx context.Context, user, attempt string, after int64, limit, max int, size func(store.Event) int) ([]store.Event, error) {
		close(started)
		return f.s.EventsPage(ctx, user, attempt, after, limit, max, size)
	}
	req := httptest.NewRequest("GET", "/v1/attempts/limits-attempt/events", nil)
	req.SetPathValue("id", "limits-attempt")
	done := make(chan struct{})
	go func() { h.events(httptest.NewRecorder(), req); close(done) }()
	select {
	case <-started:
		t.Fatal("store read began without reservation")
	case <-time.After(50 * time.Millisecond):
	}
	f.hub.releaseBytes(pageBytes)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("store read did not start")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader did not finish")
	}
}

func TestSlowStreamReleasesItsSubscriberAndBudgetWithinDeadline(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	f.hub.WriteDeadline = 100 * time.Millisecond
	f.hub.BudgetBytes = pageBytes
	payload, _ := json.Marshal(strings.Repeat("x", 200<<10))
	for i := int64(1); i <= 12; i++ {
		if err := f.s.AppendEvent(context.Background(), "alice", store.Event{Attempt: "limits-attempt", Seq: i, Kind: "update", Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	var responses []*http.Response
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("GET", "http://ipc/v1/attempts/limits-attempt/events", nil)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, resp)
	}
	defer func() {
		for _, resp := range responses {
			resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.hub.mu.Lock()
		used := f.hub.budgetUsed
		f.hub.mu.Unlock()
		if used > pageBytes {
			t.Fatalf("budget exceeded: %d", used)
		}
		if f.hub.SubscriberCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscribers remained: %d", f.hub.SubscriberCount())
}

func TestEveryStreamBeginsWithTwoSecondRetry(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	req, _ := http.NewRequest("GET", "http://ipc/v1/attempts/limits-attempt/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "retry: 2000\n" {
		t.Fatalf("first line %q: %v", line, err)
	}
}

type readDeadlineWriter struct {
	captureResponse
	deadline time.Time
}

func (w *readDeadlineWriter) SetReadDeadline(d time.Time) error { w.deadline = d; return nil }

func TestMutatingBodyGetsAReadDeadline(t *testing.T) {
	w := &readDeadlineWriter{captureResponse: captureResponse{header: make(http.Header)}}
	r := httptest.NewRequest("POST", "/v1/tasks", strings.NewReader(`{}`))
	before := time.Now()
	if _, ok := readBody(w, r); !ok {
		t.Fatal("body read failed")
	}
	if w.deadline.Before(before.Add(29*time.Second)) || w.deadline.After(before.Add(31*time.Second)) {
		t.Fatalf("deadline %s", w.deadline)
	}
}

func TestEventsAppendedDuringReplayArriveOnce(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	ctx := context.Background()
	if err := f.s.AppendEvent(ctx, "alice", store.Event{Attempt: "limits-attempt", Seq: 1, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: f.s, hub: f.hub, auth: func(*http.Request) (string, error) { return "alice", nil }}
	var once sync.Once
	h.readPage = func(ctx context.Context, user, attempt string, after int64, limit, max int, size func(store.Event) int) ([]store.Event, error) {
		events, err := f.s.EventsPage(ctx, user, attempt, after, limit, max, size)
		if err == nil {
			once.Do(func() {
				if appendErr := f.s.AppendEvent(ctx, "alice", store.Event{Attempt: "limits-attempt", Seq: 2, Payload: json.RawMessage(`{}`)}); appendErr != nil {
					t.Error(appendErr)
				}
				f.hub.Notify("limits-attempt")
			})
		}
		return events, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/attempts/{id}/events", h.events)
	server := httptest.NewServer(mux)
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL+"/v1/attempts/limits-attempt/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	var ids []string
	for len(ids) < 2 {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "id: ") {
			ids = append(ids, strings.TrimSpace(strings.TrimPrefix(line, "id: ")))
		}
	}
	if strings.Join(ids, ",") != "1,2" {
		t.Fatalf("ids %v", ids)
	}
	third := make(chan string, 1)
	go func() {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "id: ") {
				third <- strings.TrimSpace(strings.TrimPrefix(line, "id: "))
				return
			}
		}
	}()
	select {
	case id := <-third:
		t.Fatalf("duplicate replay id %s", id)
	case <-time.After(100 * time.Millisecond):
	}
}
