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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func TestReleasedStreamSlotLeavesNoUserEntry(t *testing.T) {
	h := NewHub()
	first, ok := h.reserveStream("alice")
	if !ok {
		t.Fatal("first stream refused")
	}
	second, ok := h.reserveStream("alice")
	if !ok {
		t.Fatal("second stream refused")
	}
	first()
	h.mu.Lock()
	count, present := h.streamUsers["alice"]
	h.mu.Unlock()
	if !present || count != 1 {
		t.Fatalf("after one release: present=%v count=%d", present, count)
	}
	second()
	h.mu.Lock()
	_, present = h.streamUsers["alice"]
	h.mu.Unlock()
	if present {
		t.Fatal("entry remains after the user's last stream closed")
	}
}

func TestNonMemberIsRefusedBeforeAnyStreamSlotOrBudgetIsReserved(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	_, unknown := f.request(t, "GET", "/v1/attempts/missing/events", "", "", nil)
	f.hub.BudgetBytes = pageBytes
	if !f.hub.reserveBytes(context.Background(), pageBytes) {
		t.Fatal("initial reservation")
	}
	defer f.hub.releaseBytes(pageBytes)
	for i := 0; i < 16; i++ {
		release, ok := f.hub.reserveStream("bob")
		if !ok {
			t.Fatalf("stream %d refused", i+1)
		}
		defer release()
	}
	server := httptest.NewServer(New(f.s, f.hub, func(*http.Request) (string, error) { return "bob", nil }))
	defer server.Close()
	for _, stream := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/v1/attempts/limits-attempt/events", nil)
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatalf("stream=%v: %v", stream, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if resp.StatusCode != 404 || !bytes.Equal(body, unknown) {
			t.Fatalf("stream=%v: %d %s", stream, resp.StatusCode, body)
		}
	}
	f.hub.mu.Lock()
	defer f.hub.mu.Unlock()
	if f.hub.budgetUsed != pageBytes || f.hub.streamTotal != 16 || f.hub.streamUsers["bob"] != 16 {
		t.Fatalf("non-member touched the limits: bytes=%d streams=%d user=%d", f.hub.budgetUsed, f.hub.streamTotal, f.hub.streamUsers["bob"])
	}
}

func TestStreamPageReadsUnderLoadNeverExceedTheBudget(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	for seq := int64(1); seq <= 3; seq++ {
		if err := f.s.AppendEvent(context.Background(), "alice", store.Event{Attempt: "limits-attempt", Seq: seq, Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	f.hub.BudgetBytes = 2 * pageBytes
	var inflight, peak, reads atomic.Int32
	h := &handler{store: f.s, hub: f.hub, auth: func(*http.Request) (string, error) { return "alice", nil }}
	h.readPage = func(ctx context.Context, user, attempt string, after int64, limit, max int, size func(store.Event) int) ([]store.Event, error) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			seen := peak.Load()
			if n <= seen || peak.CompareAndSwap(seen, n) {
				break
			}
		}
		f.hub.mu.Lock()
		used := f.hub.budgetUsed
		f.hub.mu.Unlock()
		if used < int(n)*pageBytes {
			t.Errorf("%d reads in flight with %d bytes reserved", n, used)
		}
		time.Sleep(2 * time.Millisecond)
		reads.Add(1)
		return f.s.EventsPage(ctx, user, attempt, after, limit, max, size)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/attempts/{id}/events", h.events)
	server := httptest.NewServer(mux)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/v1/attempts/limits-attempt/events", nil)
			req.Header.Set("Accept", "text/event-stream")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			r := bufio.NewReader(resp.Body)
			for {
				line, err := r.ReadString('\n')
				if err != nil || line == "id: 3\n" {
					return
				}
			}
		}()
	}
	wg.Wait()
	cancel()
	waitHubReleased(t, f.hub, "alice", time.Now().Add(2*time.Second), 2*pageBytes)
	if reads.Load() < 8 || peak.Load() > 2 {
		t.Fatalf("reads=%d, peak in flight=%d", reads.Load(), peak.Load())
	}
}

func TestMultiPageReplayWithAnAppendMidReplayDeliversEverySequenceOnce(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	ctx := context.Background()
	for seq := int64(1); seq <= 300; seq++ {
		if err := f.s.AppendEvent(ctx, "alice", store.Event{Attempt: "limits-attempt", Seq: seq, Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	h := &handler{store: f.s, hub: f.hub, auth: func(*http.Request) (string, error) { return "alice", nil }}
	var once sync.Once
	h.readPage = func(ctx context.Context, user, attempt string, after int64, limit, max int, size func(store.Event) int) ([]store.Event, error) {
		events, err := f.s.EventsPage(ctx, user, attempt, after, limit, max, size)
		if err == nil && len(events) == 256 {
			once.Do(func() {
				if appendErr := f.s.AppendEvent(ctx, "alice", store.Event{Attempt: "limits-attempt", Seq: 301, Payload: json.RawMessage(`{}`)}); appendErr != nil {
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
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	ids := make(chan string, 400)
	go func() {
		r := bufio.NewReader(resp.Body)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				close(ids)
				return
			}
			if strings.HasPrefix(line, "id: ") {
				ids <- strings.TrimSpace(strings.TrimPrefix(line, "id: "))
			}
		}
	}()
	var got []string
	for len(got) < 301 {
		select {
		case id := <-ids:
			got = append(got, id)
		case <-time.After(2 * time.Second):
			t.Fatalf("stalled after %d ids", len(got))
		}
	}
	for i, id := range got {
		if id != fmt.Sprint(i+1) {
			t.Fatalf("id %d is %s", i+1, id)
		}
	}
	select {
	case id := <-ids:
		t.Fatalf("extra id %s", id)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestIfMatchRefusesWeakUnbalancedAndListValidators(t *testing.T) {
	f := newFixture(t)
	id := create(t, f, "validators-create", "work")["id"].(string)
	for _, match := range []string{`W/"0"`, `"0`, `0"`, `"0", "0"`, `"0","1"`, `0,0`} {
		code, body := f.request(t, "POST", "/v1/tasks/"+id+"/ready", "validators-"+match, match, nil)
		if code != 400 || !bytes.Contains(body, []byte(`"error":"bad_request"`)) {
			t.Fatalf("If-Match %q: %d %s", match, code, body)
		}
	}
	task, err := f.s.Task(context.Background(), "alice", id)
	if err != nil || task.State != store.Draft || task.Revision != 0 {
		t.Fatalf("task changed: %+v: %v", task, err)
	}
}

func TestLastEventIDWithACommaIsRefusedInBothForms(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	for _, stream := range []bool{false, true} {
		req, _ := http.NewRequest("GET", "http://ipc/v1/attempts/limits-attempt/events", nil)
		req.Header.Set("Last-Event-ID", "1,2")
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		}
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 || !bytes.Contains(body, []byte(`"error":"bad_request"`)) {
			t.Fatalf("stream=%v: %d %s", stream, resp.StatusCode, body)
		}
	}
	assertHubReleased(t, f.hub, "alice")
}

func TestHundredTwentyNinthStreamOverHTTPIsRefused(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	f.hub.PollInterval = time.Second
	ctx := context.Background()
	for i := 0; i < 9; i++ {
		user := fmt.Sprintf("user-%d", i)
		if err := f.s.CreateUser(ctx, user, user); err != nil {
			t.Fatal(err)
		}
		if err := f.s.AddMember(ctx, "a", user, "member"); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(New(f.s, f.hub, func(r *http.Request) (string, error) { return r.Header.Get("X-Test-User"), nil }))
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 0}}
	defer client.CloseIdleConnections()
	var responses []*http.Response
	defer func() {
		for _, resp := range responses {
			resp.Body.Close()
		}
	}()
	open := func(user string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", server.URL+"/v1/attempts/limits-attempt/events", nil)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("X-Test-User", user)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, resp)
		return resp
	}
	for i := 0; i < 128; i++ {
		if resp := open(fmt.Sprintf("user-%d", i/16)); resp.StatusCode != 200 {
			t.Fatalf("stream %d: %d", i+1, resp.StatusCode)
		}
	}
	resp := open("user-8")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || !bytes.Contains(body, []byte(`"error":"too_many_streams"`)) {
		t.Fatalf("129th stream: %d %s", resp.StatusCode, body)
	}
}

func TestJSONPageBufferIsExactlyItsSize(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	h := &handler{store: f.s}
	check := func(want int) {
		t.Helper()
		events, page, err := h.page(context.Background(), "alice", "limits-attempt", 0, false)
		if err != nil || len(events) != want || cap(page) != len(page) {
			t.Fatalf("%d events: got %d, %d bytes in %d-byte buffer: %v", want, len(events), len(page), cap(page), err)
		}
		if !json.Valid(page) {
			t.Fatalf("page is not JSON: %q", page)
		}
	}
	check(0)
	for seq := int64(1); seq <= 3; seq++ {
		if err := f.s.AppendEvent(context.Background(), "alice", store.Event{Attempt: "limits-attempt", Seq: seq, Payload: json.RawMessage(`{"n":1}`)}); err != nil {
			t.Fatal(err)
		}
	}
	check(3)
}

func waitBudgetWaiters(t *testing.T, h *Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.Lock()
		got := len(h.budgetQueue)
		h.mu.Unlock()
		if got == n {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%d budget waiters queued, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func reserveInBackground(h *Hub, ctx context.Context, n int) <-chan bool {
	got := make(chan bool, 1)
	go func() { got <- h.reserveBytes(ctx, n) }()
	return got
}

func expectGranted(t *testing.T, name string, got <-chan bool) {
	t.Helper()
	select {
	case ok := <-got:
		if !ok {
			t.Fatalf("%s refused", name)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s not granted", name)
	}
}

func expectWaiting(t *testing.T, name string, got <-chan bool) {
	t.Helper()
	select {
	case ok := <-got:
		t.Fatalf("%s returned %v while it should wait", name, ok)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLateBudgetWaiterCannotOvertakeAnEarlierOne(t *testing.T) {
	h := NewHub()
	h.BudgetBytes = pageBytes
	if !h.reserveBytes(context.Background(), pageBytes) {
		t.Fatal("initial reservation")
	}
	first := reserveInBackground(h, context.Background(), pageBytes)
	waitBudgetWaiters(t, h, 1)
	second := reserveInBackground(h, context.Background(), 1)
	waitBudgetWaiters(t, h, 2)
	h.releaseBytes(1)
	expectWaiting(t, "first waiter", first)
	expectWaiting(t, "late one-byte waiter", second)
	h.releaseBytes(pageBytes - 1)
	expectGranted(t, "first waiter", first)
	expectWaiting(t, "late one-byte waiter", second)
	h.releaseBytes(pageBytes)
	expectGranted(t, "late one-byte waiter", second)
	h.releaseBytes(1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.budgetUsed != 0 || len(h.budgetQueue) != 0 {
		t.Fatalf("used=%d queued=%d", h.budgetUsed, len(h.budgetQueue))
	}
}

func TestCanceledBudgetWaiterLeavesTheQueueToTheNext(t *testing.T) {
	h := NewHub()
	h.BudgetBytes = pageBytes
	if !h.reserveBytes(context.Background(), pageBytes) {
		t.Fatal("initial reservation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := reserveInBackground(h, ctx, pageBytes)
	waitBudgetWaiters(t, h, 1)
	second := reserveInBackground(h, context.Background(), pageBytes)
	waitBudgetWaiters(t, h, 2)
	cancel()
	select {
	case ok := <-first:
		if ok {
			t.Fatal("canceled waiter was granted")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not return")
	}
	waitBudgetWaiters(t, h, 1)
	h.releaseBytes(pageBytes)
	expectGranted(t, "second waiter", second)
	h.releaseBytes(pageBytes)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.budgetUsed != 0 || len(h.budgetQueue) != 0 {
		t.Fatalf("used=%d queued=%d", h.budgetUsed, len(h.budgetQueue))
	}
}

type fakeReferences struct {
	mu    sync.Mutex
	refs  map[string]int64
	calls int
}

func (r *fakeReferences) ArtifactReference(ctx context.Context, attemptID, hash string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if size, ok := r.refs[attemptID+"/"+hash]; ok {
		return size, nil
	}
	return 0, store.ErrNotFound
}

func (r *fakeReferences) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type countingOpener struct {
	*artifact.Store
	calls atomic.Int32
}

func (o *countingOpener) Open(hash string) (io.ReadCloser, error) {
	o.calls.Add(1)
	return o.Store.Open(hash)
}

func storedPayload(t *testing.T, f *fixture, raw []byte) (*countingOpener, *fakeReferences, artifact.Ref) {
	t.Helper()
	artifacts, err := artifact.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Write(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	refs := &fakeReferences{refs: map[string]int64{"limits-attempt/" + ref.Hash: ref.Size}}
	if err := f.s.AppendEvent(context.Background(), "alice", store.Event{Attempt: "limits-attempt", Seq: 1, Kind: "update", Payload: ReferencedPayload(ref.Hash, ref.Size)}); err != nil {
		t.Fatal(err)
	}
	return &countingOpener{Store: artifacts}, refs, ref
}

func get(t *testing.T, url, user string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestArtifactBytesEqualTheRawPayloadAndTheEventCarriesItsReference(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	raw, _ := json.Marshal(strings.Repeat("x", 300<<10))
	opener, refs, ref := storedPayload(t, f, raw)
	server := httptest.NewServer(New(f.s, f.hub, func(*http.Request) (string, error) { return "alice", nil }, Execution{Artifacts: opener, References: refs}))
	defer server.Close()
	resp, body := get(t, server.URL+"/v1/attempts/limits-attempt/events", "")
	var events []struct {
		Payload struct {
			Ref  string `json:"payload_ref"`
			Hash string `json:"payload_artifact"`
			Size int64  `json:"payload_size"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &events); err != nil || resp.StatusCode != 200 || len(events) != 1 {
		t.Fatalf("events: %d %s: %v", resp.StatusCode, body, err)
	}
	if p := events[0].Payload; p.Ref != "artifact" || p.Hash != ref.Hash || p.Size != int64(len(raw)) {
		t.Fatalf("reference %+v", p)
	}
	resp, body = get(t, server.URL+"/v1/attempts/limits-attempt/artifacts/"+ref.Hash, "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/octet-stream" || resp.ContentLength != int64(len(raw)) || resp.Header.Get("ETag") != `"`+ref.Hash+`"` {
		t.Fatalf("artifact: %d %v length=%d", resp.StatusCode, resp.Header, resp.ContentLength)
	}
	if !bytes.Equal(body, raw) {
		t.Fatalf("artifact bytes differ: %d bytes, want %d", len(body), len(raw))
	}
	if opener.calls.Load() != 1 || refs.count() != 1 {
		t.Fatalf("opens=%d references=%d", opener.calls.Load(), refs.count())
	}
}

func TestArtifactReadNeedsCurrentMembershipOnEveryRead(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	opener, refs, ref := storedPayload(t, f, []byte(`{"small":true}`))
	server := httptest.NewServer(New(f.s, f.hub, func(r *http.Request) (string, error) { return r.Header.Get("X-Test-User"), nil }, Execution{Artifacts: opener, References: refs}))
	defer server.Close()
	url := server.URL + "/v1/attempts/limits-attempt/artifacts/" + ref.Hash
	if resp, _ := get(t, url, "alice"); resp.StatusCode != 200 {
		t.Fatalf("member: %d", resp.StatusCode)
	}
	_, unknown := get(t, server.URL+"/v1/attempts/missing/artifacts/"+ref.Hash, "alice")
	opens, references := opener.calls.Load(), refs.count()
	resp, body := get(t, url, "bob")
	if resp.StatusCode != 404 || !bytes.Equal(body, unknown) {
		t.Fatalf("non-member: %d %s", resp.StatusCode, body)
	}
	if opener.calls.Load() != opens || refs.count() != references {
		t.Fatal("a non-member reached the reference or the artifact store")
	}
	other := strings.Repeat("ab", 32)
	if resp, body := get(t, url[:len(url)-len(ref.Hash)]+other, "alice"); resp.StatusCode != 404 || !bytes.Equal(body, unknown) || opener.calls.Load() != opens {
		t.Fatalf("unreferenced hash: %d %s, opens=%d", resp.StatusCode, body, opener.calls.Load())
	}
	if err := f.s.RemoveMember(context.Background(), "a", "alice"); err != nil {
		t.Fatal(err)
	}
	resp, body = get(t, url, "alice")
	if resp.StatusCode != 404 || !bytes.Equal(body, unknown) {
		t.Fatalf("removed member: %d %s", resp.StatusCode, body)
	}
}

func TestArtifactRouteWithoutAnArtifactStoreAnswersMembershipFirst(t *testing.T) {
	f := newFixture(t)
	attemptForLimits(t, f)
	server := httptest.NewServer(New(f.s, f.hub, func(r *http.Request) (string, error) { return r.Header.Get("X-Test-User"), nil }))
	defer server.Close()
	url := server.URL + "/v1/attempts/limits-attempt/artifacts/" + strings.Repeat("ab", 32)
	resp, body := get(t, url, "alice")
	if resp.StatusCode != 503 || !bytes.Contains(body, []byte(`"error":"engine_unavailable"`)) {
		t.Fatalf("member without a store: %d %s", resp.StatusCode, body)
	}
	resp, body = get(t, url, "bob")
	if resp.StatusCode != 404 || !bytes.Contains(body, []byte(`"error":"not_found"`)) {
		t.Fatalf("non-member without a store: %d %s", resp.StatusCode, body)
	}
}
