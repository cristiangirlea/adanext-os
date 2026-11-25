package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

// ownerChannel is the local channel of the coordinator's owner account "local".
func ownerChannel(t *testing.T, f *fixture, execution ...Execution) http.Handler {
	t.Helper()
	if exists, err := f.s.UserExists(context.Background(), "local"); err != nil {
		t.Fatal(err)
	} else if !exists {
		if err := f.s.CreateUser(context.Background(), "local", "Local"); err != nil {
			t.Fatal(err)
		}
	}
	e := Execution{}
	if len(execution) != 0 {
		e = execution[0]
	}
	e.Owner = "local"
	return asUser(f, "local", e)
}

// attemptOf decodes a task answer's current attempt.
func attemptOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var task struct {
		Attempt map[string]any `json:"attempt"`
	}
	if err := json.Unmarshal(body, &task); err != nil || task.Attempt == nil {
		t.Fatalf("task %s: %v", body, err)
	}
	return task.Attempt
}

func decisionBody(decision string) []byte { return []byte(`{"decision":"` + decision + `"}`) }

func TestADecisionIsSeenAndAnsweredByItsActorAndInvisibleToAnotherUser(t *testing.T) {
	runner := &recordingRunner{}
	execution := Execution{EngineSHA256: strings.Repeat("a", 64), Runner: runner}
	f := newFixture(t, execution)
	ctx := context.Background()
	if err := f.s.AddMember(ctx, "a", "bob", "member"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateUser(ctx, "carol", "Carol"); err != nil {
		t.Fatal(err)
	}
	id, attempt := startQueued(t, f, "decide")
	launches := len(runner.launches())
	code, body := f.request(t, "GET", "/v1/tasks/"+id, "", "", nil)
	decision, _ := attemptOf(t, body)["decision"].(map[string]any)
	if code != 200 || decision == nil || decision["required"] != true || decision["actor"] != "alice" || decision["allowed"] != false || decision["denied"] != false || decision["project_default"] != "readonly" {
		t.Fatalf("the actor sees %d %s", code, body)
	}
	if scope := decision["scope"].(map[string]any); scope["backend"] != "codex" || scope["mode"] != "auto" || scope["workspace"] != "local" {
		t.Fatalf("scope = %v", scope)
	}
	bob := asUser(f, "bob", execution)
	code, body = do(t, bob, "GET", "/v1/tasks/"+id, "", "", nil)
	if code != 200 || attemptOf(t, body)["decision"] != nil {
		t.Fatalf("another member sees %d %s", code, body)
	}
	if code, body = do(t, asUser(f, "carol", execution), "GET", "/v1/tasks/"+id, "", "", nil); code != 404 {
		t.Fatalf("a non-member sees %d %s", code, body)
	}
	if code, body = do(t, bob, "POST", "/v1/attempts/"+attempt+"/decision", "bob-allow", "", decisionBody("allow")); code != 403 || !bytes.Contains(body, []byte(`"error":"forbidden"`)) {
		t.Fatalf("another member's allow = %d %s", code, body)
	}
	if code, body = do(t, bob, "POST", "/v1/attempts/"+attempt+"/decision", "bob-deny", "", decisionBody("deny")); code != 403 {
		t.Fatalf("another member's deny = %d %s", code, body)
	}
	if code, body = do(t, asUser(f, "carol", execution), "POST", "/v1/attempts/"+attempt+"/decision", "carol", "", decisionBody("deny")); code != 404 {
		t.Fatalf("a non-member's deny = %d %s", code, body)
	}
	if code, _ = f.request(t, "POST", "/v1/attempts/"+attempt+"/decision", "", "", decisionBody("allow")); code != 400 {
		t.Fatalf("no key = %d", code)
	}
	if code, _ = f.request(t, "POST", "/v1/attempts/"+attempt+"/decision", "maybe", "", decisionBody("maybe")); code != 400 {
		t.Fatalf("an unknown decision = %d", code)
	}
	if len(runner.launches()) != launches {
		t.Fatal("a refused decision woke the scheduler")
	}
	code, first := f.request(t, "POST", "/v1/attempts/"+attempt+"/decision", "allow", "", decisionBody("allow"))
	allowed, _ := attemptOf(t, first)["decision"].(map[string]any)
	if code != 200 || allowed == nil || allowed["allowed"] != true || attemptOf(t, first)["state"] != "pending" {
		t.Fatalf("the actor's allow = %d %s", code, first)
	}
	if got := runner.launches(); len(got) != launches+1 || got[len(got)-1] != attempt {
		t.Fatalf("launches = %v", got)
	}
	code, replay := f.request(t, "POST", "/v1/attempts/"+attempt+"/decision", "allow", "", decisionBody("allow"))
	if code != 200 || !bytes.Equal(first, replay) || len(runner.launches()) != launches+1 {
		t.Fatalf("replay = %d %s", code, replay)
	}
	if code, body = f.request(t, "POST", "/v1/attempts/"+attempt+"/decision", "allow", "", decisionBody("deny")); code != 422 {
		t.Fatalf("another body under the key = %d %s", code, body)
	}
	if code, body = do(t, bob, "GET", "/v1/tasks/"+id, "", "", nil); attemptOf(t, body)["decision"] != nil {
		t.Fatalf("another member sees the answered decision: %d %s", code, body)
	}
}

func TestAnOwnersAllowIsRefusedItsDenyAcceptedAndAClosedAttemptRefusesDecisions(t *testing.T) {
	execution := Execution{EngineSHA256: strings.Repeat("a", 64), Runner: &recordingRunner{}}
	f := newFixture(t, execution)
	if err := f.s.AddMember(context.Background(), "a", "bob", "owner"); err != nil {
		t.Fatal(err)
	}
	id, attempt := startQueued(t, f, "owner")
	bob := asUser(f, "bob", execution)
	if code, body := do(t, bob, "POST", "/v1/attempts/"+attempt+"/decision", "bob-allow", "", decisionBody("allow")); code != 403 {
		t.Fatalf("an owner's allow = %d %s", code, body)
	}
	if code, body := do(t, bob, "GET", "/v1/tasks/"+id, "", "", nil); attemptOf(t, body)["decision"] == nil {
		t.Fatalf("an owner does not see the decision: %d %s", code, body)
	}
	code, body := do(t, bob, "POST", "/v1/attempts/"+attempt+"/decision", "bob-deny", "", decisionBody("deny"))
	denied, _ := attemptOf(t, body)["decision"].(map[string]any)
	if code != 200 || !bytes.Contains(body, []byte(`"state":"canceled"`)) || denied == nil || denied["denied"] != true {
		t.Fatalf("an owner's deny = %d %s", code, body)
	}
	events, err := f.s.Events(context.Background(), "alice", attempt, 0)
	if err != nil || len(events) != 1 || !bytes.Contains(events[0].Payload, []byte(`"reason":"permission denied"`)) {
		t.Fatalf("events %+v: %v", events, err)
	}
	if code, body := f.request(t, "POST", "/v1/attempts/"+attempt+"/decision", "late", "", decisionBody("allow")); code != 409 || !bytes.Contains(body, []byte(`"error":"invalid_transition"`)) {
		t.Fatalf("a decision on a closed attempt = %d %s", code, body)
	}
	_, launched := startQueued(t, f, "launched")
	_, err = f.s.Do(context.Background(), "alice", "launch", "launch", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(context.Background(), "UPDATE attempts SET state='launching' WHERE id=?", launched)
		return []byte("ok"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := f.request(t, "POST", "/v1/attempts/"+launched+"/decision", "launched", "", decisionBody("allow")); code != 409 {
		t.Fatalf("a decision on a launched attempt = %d %s", code, body)
	}
	readonly := create(t, f, "ro-create", "work")["id"].(string)
	if code, _ := f.request(t, "POST", "/v1/tasks/"+readonly+"/ready", "ro-ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	code, body = f.request(t, "POST", "/v1/tasks/"+readonly+"/start", "ro-start", "1", []byte(`{"backend":"codex","mode":"readonly"}`))
	if code != 200 || attemptOf(t, mustTask(t, f, readonly))["decision"] != nil {
		t.Fatalf("a readonly start = %d %s", code, body)
	}
	var started struct {
		AttemptID string `json:"attempt_id"`
	}
	_ = json.Unmarshal(body, &started)
	if code, body := f.request(t, "POST", "/v1/attempts/"+started.AttemptID+"/decision", "ro", "", decisionBody("allow")); code != 409 {
		t.Fatalf("a decision nothing needs = %d %s", code, body)
	}
}

func mustTask(t *testing.T, f *fixture, id string) []byte {
	t.Helper()
	code, body := f.request(t, "GET", "/v1/tasks/"+id, "", "", nil)
	if code != 200 {
		t.Fatalf("task %s: %d %s", id, code, body)
	}
	return body
}

func TestTheOwnerChannelManagesMembersAndTheDefaultMode(t *testing.T) {
	runner := &recordingRunner{}
	f := newFixture(t)
	owner := ownerChannel(t, f, Execution{Runner: runner})
	members := func(body []byte) []map[string]any {
		var out []map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		return out
	}
	code, body := do(t, owner, "GET", "/v1/projects/a/members", "", "", nil)
	if list := members(body); code != 200 || len(list) != 1 || list[0]["user"] != "alice" || list[0]["name"] != "Alice" || list[0]["role"] != "owner" || list[0]["added"] == "" {
		t.Fatalf("members = %d %s", code, body)
	}
	code, body = do(t, owner, "PUT", "/v1/projects/a/members/bob", "add-bob", "", []byte(`{"role":"member"}`))
	if list := members(body); code != 200 || len(list) != 2 || list[1]["user"] != "bob" || list[1]["role"] != "member" {
		t.Fatalf("add = %d %s", code, body)
	}
	for _, c := range []struct {
		method, path, key string
		body              string
		status            int
		code              string
	}{
		{"PUT", "/v1/projects/a/members/bob", "", `{"role":"owner"}`, 400, "bad_request"},
		{"PUT", "/v1/projects/a/members/bob", "bad-role", `{"role":"admin"}`, 400, "bad_request"},
		{"PUT", "/v1/projects/a/members/nobody", "nobody", `{"role":"member"}`, 404, "not_found"},
		{"PUT", "/v1/projects/missing/members/bob", "missing", `{"role":"member"}`, 404, "not_found"},
		{"DELETE", "/v1/projects/a/members/alice", "last", "", 409, "last_owner"},
		{"DELETE", "/v1/projects/a/members/carol", "carol", "", 404, "not_found"},
		{"PUT", "/v1/projects/a/default-mode", "bad-mode", `{"mode":"ask"}`, 400, "bad_request"},
		{"GET", "/v1/projects/missing/members", "", "", 404, "not_found"},
	} {
		code, body := do(t, owner, c.method, c.path, c.key, "", []byte(c.body))
		if code != c.status || !bytes.Contains(body, []byte(`"error":"`+c.code+`"`)) {
			t.Fatalf("%s %s = %d %s", c.method, c.path, code, body)
		}
	}
	if code, body := do(t, owner, "GET", "/v1/projects/a/members", "", "", nil); len(members(body)) != 2 {
		t.Fatalf("after the refusals = %d %s", code, body)
	}
	alice := asUser(f, "alice", Execution{Owner: "local"})
	for _, c := range []struct{ method, path, body string }{{"GET", "/v1/projects/a/members", ""}, {"PUT", "/v1/projects/a/members/bob", `{"role":"owner"}`}, {"DELETE", "/v1/projects/a/members/bob", ""}, {"PUT", "/v1/projects/a/default-mode", `{"mode":"yolo"}`}} {
		if code, body := do(t, alice, c.method, c.path, "alice-"+c.method, "", []byte(c.body)); code != 403 || !bytes.Contains(body, []byte(`"error":"forbidden"`)) {
			t.Fatalf("a project owner who is not the owner account: %s %s = %d %s", c.method, c.path, code, body)
		}
	}
	// A key is optional here; setting a mode twice is harmless.
	code, body = do(t, owner, "PUT", "/v1/projects/a/default-mode", "", "", []byte(`{"mode":"yolo"}`))
	if code != 200 || string(bytes.TrimSpace(body)) != `{"id":"a","default_mode":"yolo"}` {
		t.Fatalf("default mode = %d %s", code, body)
	}
	if code, body := do(t, owner, "PUT", "/v1/projects/a/default-mode", "mode-key", "", []byte(`{"mode":"auto"}`)); code != 200 || !bytes.Contains(body, []byte(`"default_mode":"auto"`)) {
		t.Fatalf("default mode with a key = %d %s", code, body)
	}
	if len(runner.launches()) != 2 {
		t.Fatalf("the scheduler was woken %d times", len(runner.launches()))
	}
	code, body = f.request(t, "GET", "/v1/projects", "", "", nil)
	if code != 200 || !bytes.Contains(body, []byte(`"id":"a","name":"a","role":"owner"`)) || !bytes.Contains(body, []byte(`"default_mode":"auto"`)) {
		t.Fatalf("projects = %d %s", code, body)
	}
	if code, body := do(t, owner, "DELETE", "/v1/projects/a/members/bob", "remove-bob", "", nil); code != 200 || len(members(body)) != 1 {
		t.Fatalf("remove = %d %s", code, body)
	}
}

func TestRemovingAMemberThroughTheOwnerChannelDeniesTheirDecisionsAndCancelsTheirWork(t *testing.T) {
	runner := &recordingRunner{}
	execution := Execution{EngineSHA256: strings.Repeat("a", 64), Runner: runner}
	f := newFixture(t, execution)
	ctx := context.Background()
	if err := f.s.AddMember(ctx, "a", "bob", "member"); err != nil {
		t.Fatal(err)
	}
	bob := asUser(f, "bob", execution)
	task := create(t, f, "bob-task", "work")["id"].(string)
	if code, _ := do(t, bob, "POST", "/v1/tasks/"+task+"/ready", "ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	code, body := do(t, bob, "POST", "/v1/tasks/"+task+"/start", "start", "1", []byte(policy))
	if code != 200 {
		t.Fatalf("start = %d %s", code, body)
	}
	var started struct {
		AttemptID string `json:"attempt_id"`
	}
	_ = json.Unmarshal(body, &started)
	running := create(t, f, "running-task", "work")["id"].(string)
	if err := f.s.CreateAttempt(ctx, store.Attempt{ID: "bob-running", Task: running, Actor: "bob", Backend: "codex", Mode: "auto", Executor: "local", State: "running"}); err != nil {
		t.Fatal(err)
	}
	owner := ownerChannel(t, f, execution)
	if code, body := do(t, owner, "DELETE", "/v1/projects/a/members/bob", "remove-bob", "", nil); code != 200 || bytes.Contains(body, []byte(`"bob"`)) {
		t.Fatalf("remove = %d %s", code, body)
	}
	if !slices.Contains(runner.cancels(), running) {
		t.Fatalf("cancels = %v", runner.cancels())
	}
	code, body = f.request(t, "GET", "/v1/tasks/"+task, "", "", nil)
	decision, _ := attemptOf(t, body)["decision"].(map[string]any)
	if code != 200 || !bytes.Contains(body, []byte(`"state":"canceled"`)) || decision == nil || decision["denied"] != true {
		t.Fatalf("the removed member's task, as the owner sees it = %d %s", code, body)
	}
	for _, path := range []string{"/v1/tasks/" + task, "/v1/attempts/" + started.AttemptID + "/events", "/v1/attempts/bob-running/events"} {
		if code, body := do(t, bob, "GET", path, "", "", nil); code != 404 {
			t.Fatalf("the removed member reads %s: %d %s", path, code, body)
		}
	}
	if code, body := do(t, bob, "GET", "/v1/projects", "", "", nil); code != 200 || bytes.Contains(body, []byte(`"id":"a"`)) {
		t.Fatalf("the removed member lists %s", body)
	}
}

func TestTheStopShowsItsCountsToTheOwnerAndRefusesAStaleCount(t *testing.T) {
	stopper := &recordingStopper{}
	execution := Execution{EngineSHA256: strings.Repeat("a", 64), Runner: noopRunner{}, Owner: "local", Stopper: stopper}
	f := newFixture(t, execution)
	owner := ownerChannel(t, f, execution)
	if err := f.s.AddMember(context.Background(), "a", "local", "member"); err != nil {
		t.Fatal(err)
	}
	queued, attempt := startQueued(t, f, "queued")
	// One queued attempt of the owner account's own.
	mine := create(t, f, "mine-create", "work")["id"].(string)
	if code, _ := f.request(t, "POST", "/v1/tasks/"+mine+"/ready", "mine-ready", "0", nil); code != 200 {
		t.Fatal(code)
	}
	if code, body := do(t, owner, "POST", "/v1/tasks/"+mine+"/start", "mine-start", "1", []byte(policy)); code != 200 {
		t.Fatalf("owner's start = %d %s", code, body)
	}
	for _, user := range []string{"alice", "bob"} {
		other := asUser(f, user, execution)
		if code, body := do(t, other, "GET", "/v1/coordinator/stop", "", "", nil); code != 403 || !bytes.Contains(body, []byte(`"error":"forbidden"`)) {
			t.Fatalf("%s's counts = %d %s", user, code, body)
		}
		if code, body := do(t, other, "POST", "/v1/coordinator/stop", user+"-stop", "", []byte(`{"others":1}`)); code != 403 {
			t.Fatalf("%s's stop = %d %s", user, code, body)
		}
	}
	code, body := do(t, owner, "GET", "/v1/coordinator/stop", "", "", nil)
	if code != 200 || string(bytes.TrimSpace(body)) != `{"running":0,"queued":2,"others_running":0,"others_queued":1}` {
		t.Fatalf("counts = %d %s", code, body)
	}
	for _, bad := range []string{"", `{}`, `{"others":-1}`, `{"others":"1"}`} {
		if code, body := do(t, owner, "POST", "/v1/coordinator/stop", "bad", "", []byte(bad)); code != 400 {
			t.Fatalf("stop with %q = %d %s", bad, code, body)
		}
	}
	code, body = do(t, owner, "POST", "/v1/coordinator/stop", "stale", "", []byte(`{"others":0}`))
	want := `{"error":"stop_count_changed","message":"` + errorMessage("stop_count_changed") + `","running":0,"queued":2,"others_running":0,"others_queued":1}`
	if code != 409 || string(bytes.TrimSpace(body)) != want {
		t.Fatalf("a stale count = %d %s", code, body)
	}
	if task, err := f.s.Task(context.Background(), "alice", queued); err != nil || task.State != store.Preparing || stopper.calls.get() != 0 {
		t.Fatalf("a stale stop stopped %+v, %d calls: %v", task, stopper.calls.get(), err)
	}
	// The key of the refused stop holds nothing: it confirms again with the count shown.
	code, body = do(t, owner, "POST", "/v1/coordinator/stop", "stale", "", []byte(`{"others":1}`))
	if code != 202 || !bytes.Contains(body, []byte(`"queued":2`)) || stopper.calls.get() != 1 {
		t.Fatalf("stop = %d %s", code, body)
	}
	events, err := f.s.Events(context.Background(), "alice", attempt, 0)
	if err != nil || len(events) != 1 || !bytes.Contains(events[0].Payload, []byte(`"reason":"coordinator stopped by the owner"`)) {
		t.Fatalf("the other user's events %+v: %v", events, err)
	}
}

// keyed sends a network request with an idempotency key and an If-Match revision, when given.
func (f *netFixture) keyed(t *testing.T, method, path, cookie, key, match string, body any) reply {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, bytes.NewReader(marshal(t, body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testRPID
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Idempotency-Key", key)
	if match != "" {
		req.Header.Set("If-Match", match)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return readReply(t, resp)
}

func TestARoleChangeEndsEverySessionOfTheAccountAndClosesTheirStreams(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", false)
	if r := f.owner(t, "PUT", "/v1/projects/p/members/"+id, map[string]any{"role": "member"}); r.status != 200 {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	key := newSoftKey(t)
	cookies := [2]string{f.mustEnroll(t, code, key), f.mustLogin(t, key)}
	f.hub.PollInterval, f.hub.HeartbeatInterval = 1<<62, 1<<62
	streams := [2]*http.Response{f.openStream(t, cookies[0]), f.openStream(t, cookies[1])}
	if r := f.owner(t, "PUT", "/v1/projects/p/members/"+id, map[string]any{"role": "owner"}); r.status != 200 || !strings.Contains(string(r.body), `"role":"owner"`) {
		t.Fatalf("role change: %d %s", r.status, r.body)
	}
	for i := range 2 {
		streamEnds(t, streams[i])
		if f.signedIn(t, cookies[i]) {
			t.Fatalf("session %d survived the role change", i)
		}
	}
	// The same role again changes nothing and ends no session.
	cookie := f.mustLogin(t, key)
	if r := f.owner(t, "PUT", "/v1/projects/p/members/"+id, map[string]any{"role": "owner"}); r.status != 200 || !f.signedIn(t, cookie) {
		t.Fatalf("an unchanged role: %d %s", r.status, r.body)
	}
}

func TestADecisionIsSeenAndAnsweredInTheActorsSessionAndInvisibleInAnotherUsers(t *testing.T) {
	runner := &recordingRunner{}
	f := newWebFixture(t, testUI, func(e *Execution) { e.EngineSHA256, e.Runner = strings.Repeat("a", 64), runner })
	owner := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	id, code := f.account(t, "Vera", false)
	vera := f.mustEnroll(t, code, newSoftKey(t))
	f.exec(t, "INSERT INTO project_members(project,user,role,added) VALUES('p',?,'member','now')", id)
	f.exec(t, "UPDATE attempts SET state='ended_ok' WHERE id='a'")
	if r := f.keyed(t, "POST", "/v1/tasks/t/ready", owner, "ready", "0", nil); r.status != 200 {
		t.Fatalf("ready: %d %s", r.status, r.body)
	}
	r := f.keyed(t, "POST", "/v1/tasks/t/start", owner, "start", "1", map[string]any{"backend": "codex", "mode": "yolo"})
	if r.status != 200 {
		t.Fatalf("start: %d %s", r.status, r.body)
	}
	attempt := r.json(t)["attempt_id"].(string)
	seen := f.call(t, "GET", "/v1/tasks/t", owner, nil)
	if decision, _ := attemptOf(t, seen.body)["decision"].(map[string]any); decision == nil || decision["required"] != true || decision["actor"] != "local" {
		t.Fatalf("the actor's session sees %s", seen.body)
	}
	other := f.call(t, "GET", "/v1/tasks/t", vera, nil)
	if other.status != 200 || attemptOf(t, other.body)["decision"] != nil {
		t.Fatalf("another user's session sees %d %s", other.status, other.body)
	}
	if r := f.keyed(t, "POST", "/v1/attempts/"+attempt+"/decision", vera, "vera", "", map[string]any{"decision": "deny"}); r.status != 403 || r.code(t) != "not_on_network" {
		t.Fatalf("another user's decision: %d %s", r.status, r.body)
	}
	r = f.keyed(t, "POST", "/v1/attempts/"+attempt+"/decision", owner, "allow", "", map[string]any{"decision": "allow"})
	if decision, _ := attemptOf(t, r.body)["decision"].(map[string]any); r.status != 200 || decision["allowed"] != true {
		t.Fatalf("the actor's allow: %d %s", r.status, r.body)
	}
	if !slices.Contains(runner.launches(), attempt) {
		t.Fatalf("launches = %v", runner.launches())
	}
}

func TestThroughAnHTTPSSessionTheOwnerAccountRunsATaskAndAnotherUserIsRefusedEach(t *testing.T) {
	runner := &recordingRunner{}
	g, err := source.Resolve(context.Background(), canonicalTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	inspector := &source.Inspector{Git: g, MirrorRoot: canonicalTemp(t)}
	artifacts, err := artifact.Open(filepath.Join(canonicalTemp(t), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	f := newWebFixture(t, testUI, func(e *Execution) {
		e.EngineSHA256, e.Runner = strings.Repeat("a", 64), runner
		e.Inspector, e.Capturer, e.Writer, e.Artifacts = inspector, inspector, artifacts, artifacts
	})
	repo := newRepo(t)
	imported := f.owner(t, "POST", "/v1/projects", map[string]any{"path": repo, "name": "repo"})
	if imported.status != 201 {
		t.Fatalf("import: %d %s", imported.status, imported.body)
	}
	project := imported.json(t)["id"].(string)
	owner := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	id, code := f.account(t, "Vera", false)
	vera := f.mustEnroll(t, code, newSoftKey(t))
	f.exec(t, "INSERT INTO project_members(project,user,role,added) VALUES(?,?,'member','now')", project, id)
	refused := func(method, path, match string, body any) {
		t.Helper()
		if r := f.keyed(t, method, path, vera, "vera-"+path, match, body); r.status != 403 || r.code(t) != "not_on_network" {
			t.Fatalf("another user's %s %s: %d %s", method, path, r.status, r.body)
		}
	}
	snapshotBody := map[string]any{"include_changes": false}
	refused("POST", "/v1/projects/"+project+"/snapshots", "", snapshotBody)
	r := f.keyed(t, "POST", "/v1/projects/"+project+"/snapshots", owner, "snapshot", "", snapshotBody)
	if r.status != 201 {
		t.Fatalf("snapshot: %d %s", r.status, r.body)
	}
	snapshot := r.json(t)["id"].(string)
	taskBody := map[string]any{"project": project, "objective": "work", "criteria": "done", "snapshot": snapshot}
	refused("POST", "/v1/tasks", "", taskBody)
	r = f.keyed(t, "POST", "/v1/tasks", owner, "create", "", taskBody)
	if r.status != 201 {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	task := r.json(t)["id"].(string)
	refused("POST", "/v1/tasks/"+task+"/ready", "0", nil)
	if r := f.keyed(t, "POST", "/v1/tasks/"+task+"/ready", owner, "ready", "0", nil); r.status != 200 {
		t.Fatalf("ready: %d %s", r.status, r.body)
	}
	start := map[string]any{"backend": "codex", "mode": "auto"}
	refused("POST", "/v1/tasks/"+task+"/start", "1", start)
	r = f.keyed(t, "POST", "/v1/tasks/"+task+"/start", owner, "start", "1", start)
	if r.status != 200 {
		t.Fatalf("start: %d %s", r.status, r.body)
	}
	attempt := r.json(t)["attempt_id"].(string)
	refused("POST", "/v1/attempts/"+attempt+"/decision", "", map[string]any{"decision": "deny"})
	if r := f.keyed(t, "POST", "/v1/attempts/"+attempt+"/decision", owner, "allow", "", map[string]any{"decision": "allow"}); r.status != 200 {
		t.Fatalf("allow: %d %s", r.status, r.body)
	}
	finding := map[string]any{"body": "looks fine"}
	refused("POST", "/v1/tasks/"+task+"/findings", "", finding)
	if r := f.keyed(t, "POST", "/v1/tasks/"+task+"/findings", owner, "finding", "", finding); r.status != 201 {
		t.Fatalf("finding: %d %s", r.status, r.body)
	}
	refused("GET", "/v1/coordinator/stop", "", nil)
	if r := f.call(t, "GET", "/v1/coordinator/stop", owner, nil); r.status != 200 {
		t.Fatalf("the owner's counts: %d %s", r.status, r.body)
	}
	if !slices.Contains(runner.launches(), attempt) {
		t.Fatalf("launches = %v", runner.launches())
	}
}

// ownerSessionID is the id of the owner account's only session.
func ownerSessionID(t *testing.T, f *netFixture) string {
	t.Helper()
	sessions := mustSessions(t, f, "local")
	if len(sessions) != 1 {
		t.Fatalf("%d sessions", len(sessions))
	}
	return sessions[0].ID
}

func TestANetworkWriteWhoseSessionIsRevokedBeforeItsCommitIsRefusedAndChangesNothing(t *testing.T) {
	f := newWebFixture(t, testUI)
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	entered, release := pauseAt(f, "commit")
	created := make(chan reply, 1)
	go func() {
		created <- f.keyed(t, "POST", "/v1/tasks", cookie, "create", "", map[string]any{"project": "p", "objective": "after revocation", "criteria": "c"})
	}()
	waitFor(t, entered, "the commit")
	if r := f.owner(t, "DELETE", "/v1/sessions/"+ownerSessionID(t, f), nil); r.status != 204 {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	release()
	if r := <-created; r.status != 401 || r.code(t) != "unauthenticated" {
		t.Fatalf("the write: %d %s", r.status, r.body)
	}
	if got := f.column(t, "SELECT id FROM tasks WHERE objective='after revocation'"); len(got) != 0 {
		t.Fatalf("a task was created: %s", got)
	}
}

func TestAStopWhoseSessionIsRevokedAfterItsCommitStillStopsAndItsReplayAnswersWhatWasDone(t *testing.T) {
	stopper := &recordingStopper{}
	f := newWebFixture(t, testUI, func(e *Execution) {
		e.Stopper, e.Runner, e.EngineSHA256 = stopper, &recordingRunner{}, strings.Repeat("a", 64)
	})
	key := newSoftKey(t)
	cookie := f.mustEnroll(t, f.newCode(t, "local"), key)
	// Revoked as soon as the stop's transaction has committed.
	f.n.h.afterStopCommit = func() {
		if r := f.owner(t, "DELETE", "/v1/sessions/"+ownerSessionID(t, f), nil); r.status != 204 {
			t.Errorf("revoke: %d %s", r.status, r.body)
		}
	}
	first := f.keyed(t, "POST", "/v1/coordinator/stop", cookie, "stop", "", map[string]any{"others": 0})
	if first.status != 202 || stopper.calls.get() != 1 {
		t.Fatalf("the stop: %d %s, %d handoffs", first.status, first.body, stopper.calls.get())
	}
	f.n.h.afterStopCommit = nil
	replay := f.keyed(t, "POST", "/v1/coordinator/stop", f.mustLogin(t, key), "stop", "", map[string]any{"others": 0})
	if replay.status != 202 || !bytes.Equal(replay.body, first.body) || stopper.calls.get() != 1 {
		t.Fatalf("the replay: %d %s, %d handoffs", replay.status, replay.body, stopper.calls.get())
	}
}

// refineSeed makes the network fixture's task a failed one whose attempt left a worktree with
// changes to commit, so a refine has irreversible work to do.
func refineSeed(t *testing.T, f *netFixture) {
	t.Helper()
	hash := strings.Repeat("a", 64)
	for _, q := range []string{
		"INSERT INTO project_sources(project,canonical_path,common_dir,fingerprint,imported) VALUES('p','/repo','/repo/.git','f','now')",
		"INSERT INTO artifacts(hash,size,origin,created) VALUES('" + hash + "',0,'test','now')",
		"INSERT INTO source_snapshots(id,project,base_commit,fingerprint,staged_patch,unstaged_patch,created_by,created) VALUES('s','p','" + strings.Repeat("c", 40) + "','f','" + hash + "','" + hash + "','local','now')",
		"UPDATE tasks SET state='failed',revision=4,snapshot='s' WHERE id='t'",
		`UPDATE attempts SET state='ended_failed',mode='auto',provider_changes='{"changes":[],"truncated":false}' WHERE id='a'`,
		"INSERT INTO workspaces(attempt,project,task,snapshot,base_commit,branch,path,manifest_version,manifest_hash,executor,policy,created,tested_commit,imported_commit) VALUES('a','p','t','s','" + strings.Repeat("c", 40) + "','ada/t','/w',2,'" + strings.Repeat("f", 64) + "','local','auto','now','','" + importedCommit + "')",
	} {
		f.exec(t, q)
	}
}

func TestARefineWhoseSessionIsRevokedAfterItsAuthorizationCompletes(t *testing.T) {
	reviewer, runner := &fakeReviewer{}, &recordingRunner{}
	f := newWebFixture(t, testUI, func(e *Execution) { e.Review, e.Runner, e.EngineSHA256 = reviewer, runner, strings.Repeat("a", 64) })
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	refineSeed(t, f)
	entered, release := pauseAt(f, "authorized")
	refined := make(chan reply, 1)
	go func() { refined <- f.keyed(t, "POST", "/v1/tasks/t/refine", cookie, "refine", "4", refineBody) }()
	waitFor(t, entered, "the authorization")
	if r := f.owner(t, "DELETE", "/v1/sessions/"+ownerSessionID(t, f), nil); r.status != 204 {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	release()
	if r := <-refined; r.status != 200 {
		t.Fatalf("the refine: %d %s", r.status, r.body)
	}
	if calls := reviewer.recorded(); strings.Join(calls, ",") != "commit a,remove a" || len(runner.launches()) != 1 {
		t.Fatalf("calls %v, launches %v", calls, runner.launches())
	}
}

func TestARefineWhoseSessionIsRevokedBeforeItsAuthorizationIsRefusedWithNothingDone(t *testing.T) {
	reviewer, runner := &blockingReviewer{entered: make(chan struct{}), release: make(chan struct{})}, &recordingRunner{}
	f := newWebFixture(t, testUI, func(e *Execution) { e.Review, e.Runner, e.EngineSHA256 = reviewer, runner, strings.Repeat("a", 64) })
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	refineSeed(t, f)
	refined := make(chan reply, 1)
	go func() { refined <- f.keyed(t, "POST", "/v1/tasks/t/refine", cookie, "refine", "4", refineBody) }()
	waitFor(t, reviewer.entered, "the clone check")
	if r := f.owner(t, "DELETE", "/v1/sessions/"+ownerSessionID(t, f), nil); r.status != 204 {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	close(reviewer.release)
	if r := <-refined; r.status != 401 {
		t.Fatalf("the refine: %d %s", r.status, r.body)
	}
	if calls := reviewer.recorded(); len(calls) != 0 || len(runner.launches()) != 0 {
		t.Fatalf("calls %v, launches %v", calls, runner.launches())
	}
	if got := f.column(t, "SELECT state FROM tasks WHERE id='t'"); len(got) != 1 || string(got[0]) != "failed" {
		t.Fatalf("the task is %s", got)
	}
}

func TestTwoIdenticalRefinesRacingOnBothChannelsAnswerTheFirstResult(t *testing.T) {
	reviewer := &blockingReviewer{entered: make(chan struct{}), release: make(chan struct{})}
	f := newWebFixture(t, testUI, func(e *Execution) {
		e.Review, e.Runner, e.EngineSHA256, e.Guards = reviewer, &recordingRunner{}, strings.Repeat("a", 64), &Guards{}
	})
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	f.exec(t, "UPDATE tasks SET state='failed',revision=4 WHERE id='t'")
	f.exec(t, "UPDATE attempts SET state='ended_failed' WHERE id='a'")
	local := make(chan reply, 1)
	go func() {
		req := httptest.NewRequest("POST", "/v1/tasks/t/refine", bytes.NewReader(marshal(t, refineBody)))
		req.Header.Set("Idempotency-Key", "same")
		req.Header.Set("If-Match", "4")
		rec := httptest.NewRecorder()
		f.local.ServeHTTP(rec, req)
		local <- readReply(t, rec.Result())
	}()
	waitFor(t, reviewer.entered, "the local refine")
	reached := make(chan struct{})
	f.n.h.atRefineLock = func() { close(reached) }
	network := make(chan reply, 1)
	go func() { network <- f.keyed(t, "POST", "/v1/tasks/t/refine", cookie, "same", "4", refineBody) }()
	waitFor(t, reached, "the network refine at the lock")
	close(reviewer.release)
	first, second := <-local, <-network
	if first.status != 200 || second.status != 200 || !bytes.Equal(first.body, second.body) {
		t.Fatalf("local %d %s, network %d %s", first.status, first.body, second.status, second.body)
	}
}

// blockingReviewer holds its first clone check until released and counts every check.
type blockingReviewer struct {
	fakeReviewer
	checks           atomic.Int32
	entered, release chan struct{}
}

func (b *blockingReviewer) VerifyClone(string, string) error {
	if b.checks.Add(1) == 1 {
		close(b.entered)
		<-b.release
	}
	return nil
}

func TestARefineOnEachChannelIsSerialised(t *testing.T) {
	reviewer := &blockingReviewer{entered: make(chan struct{}), release: make(chan struct{})}
	f := newWebFixture(t, testUI, func(e *Execution) {
		e.Review, e.Runner, e.EngineSHA256, e.Guards = reviewer, &recordingRunner{}, strings.Repeat("a", 64), &Guards{}
	})
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	f.exec(t, "UPDATE tasks SET state='failed',revision=4 WHERE id='t'")
	f.exec(t, "UPDATE attempts SET state='ended_failed' WHERE id='a'")
	local := make(chan reply, 1)
	go func() {
		req := httptest.NewRequest("POST", "/v1/tasks/t/refine", bytes.NewReader(marshal(t, refineBody)))
		req.Header.Set("Idempotency-Key", "local-refine")
		req.Header.Set("If-Match", "4")
		rec := httptest.NewRecorder()
		f.local.ServeHTTP(rec, req)
		local <- readReply(t, rec.Result())
	}()
	waitFor(t, reviewer.entered, "the local refine")
	network := make(chan reply, 1)
	go func() { network <- f.keyed(t, "POST", "/v1/tasks/t/refine", cookie, "network-refine", "4", refineBody) }()
	time.Sleep(150 * time.Millisecond)
	if n := reviewer.checks.Load(); n != 1 {
		t.Fatalf("%d refines released earlier work at once", n)
	}
	close(reviewer.release)
	if r := <-local; r.status != 200 {
		t.Fatalf("the local refine: %d %s", r.status, r.body)
	}
	if r := <-network; r.status != 409 || reviewer.checks.Load() != 1 {
		t.Fatalf("the network refine after it: %d %s, %d checks", r.status, r.body, reviewer.checks.Load())
	}
}

func TestACaptureOnEachChannelIsSerialised(t *testing.T) {
	held := &heldCapturer{entered: make(chan struct{}), release: make(chan struct{})}
	artifacts, err := artifact.Open(canonicalTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	f := newWebFixture(t, testUI, func(e *Execution) { e.Capturer, e.Writer, e.Guards = held, artifacts, &Guards{} })
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	f.exec(t, "INSERT INTO project_sources(project,canonical_path,common_dir,fingerprint,imported) VALUES('p','/repo','/repo/.git','f','now')")
	first := make(chan reply, 1)
	go func() { first <- f.owner(t, "POST", "/v1/projects/p/snapshots", map[string]any{}) }()
	waitFor(t, held.entered, "the local capture")
	if r := f.keyed(t, "POST", "/v1/projects/p/snapshots", cookie, "network-snapshot", "", map[string]any{}); r.status != 429 || r.code(t) != "capture_busy" {
		t.Fatalf("a network capture during the local one: %d %s", r.status, r.body)
	}
	close(held.release)
	if r := <-first; r.status != 409 {
		t.Fatalf("the local capture: %d %s", r.status, r.body)
	}
}

func TestAFailedSnapshotOverHTTPSCarriesItsBlockersWithoutHostPaths(t *testing.T) {
	g, err := source.Resolve(context.Background(), canonicalTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	inspector := &source.Inspector{Git: g, MirrorRoot: canonicalTemp(t)}
	artifacts, err := artifact.Open(canonicalTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	f := newWebFixture(t, testUI, func(e *Execution) { e.Inspector, e.Capturer, e.Writer = inspector, inspector, artifacts })
	repo := newRepo(t)
	imported := f.owner(t, "POST", "/v1/projects", map[string]any{"path": repo, "name": "repo"})
	if imported.status != 201 {
		t.Fatalf("import: %d %s", imported.status, imported.body)
	}
	project := imported.json(t)["id"].(string)
	if err := os.Rename(repo, repo+"-moved"); err != nil {
		t.Fatal(err)
	}
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	r := f.keyed(t, "POST", "/v1/projects/"+project+"/snapshots", cookie, "snapshot", "", map[string]any{})
	var refused struct {
		Error    string           `json:"error"`
		Blockers []source.Blocker `json:"blockers"`
	}
	if err := json.Unmarshal(r.body, &refused); err != nil || r.status != 409 || refused.Error != "import_blocked" || len(refused.Blockers) == 0 {
		t.Fatalf("network snapshot: %d %s", r.status, r.body)
	}
	for _, b := range refused.Blockers {
		if b.Code == "" || b.Count == 0 || len(b.Paths) != 0 {
			t.Fatalf("blocker %+v", b)
		}
	}
	if strings.Contains(string(r.body), filepath.Base(repo)) || strings.Contains(string(r.body), filepath.ToSlash(repo)) {
		t.Fatalf("a host path: %s", r.body)
	}
	// The owner's desktop still sees where it looked.
	local := f.owner(t, "POST", "/v1/projects/"+project+"/snapshots", map[string]any{})
	if local.status != 409 || !strings.Contains(string(local.body), filepath.Base(repo)) {
		t.Fatalf("local snapshot: %d %s", local.status, local.body)
	}
}

func TestAnAllowExpiredByTheInjectedClockReadsExpiredAndTheClaimRefusesIt(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	f := newFixture(t, Execution{EngineSHA256: strings.Repeat("a", 64), Runner: &recordingRunner{}, Clock: clock.Now})
	id, attempt := startQueued(t, f, "clock")
	if code, body := f.request(t, "POST", "/v1/attempts/"+attempt+"/decision", "allow", "", decisionBody("allow")); code != 200 {
		t.Fatalf("allow: %d %s", code, body)
	}
	// Allowed on the fake clock first, then expired by it: a wall-clock read cannot pass both.
	clock.Advance(store.DecisionLifetime - time.Second)
	for _, path := range []string{"/v1/tasks/" + id, "/v1/tasks"} {
		if _, body := f.request(t, "GET", path, "", "", nil); !bytes.Contains(body, []byte(`"allowed":true`)) {
			t.Fatalf("%s before the expiry reads %s", path, body)
		}
	}
	clock.Advance(time.Second)
	for _, path := range []string{"/v1/tasks/" + id, "/v1/tasks"} {
		_, body := f.request(t, "GET", path, "", "", nil)
		if !bytes.Contains(body, []byte(`"allowed":false`)) || bytes.Contains(body, []byte(`"allowed":true`)) {
			t.Fatalf("%s reads %s", path, body)
		}
	}
	err := f.s.DoOwned(context.Background(), attempt, 0, func(tx *store.Tx) error {
		_, err := tx.ClaimLaunch(context.Background(), attempt, clock.Now())
		return err
	})
	if !errors.Is(err, store.ErrDecisionRequired) {
		t.Fatalf("claim: %v", err)
	}
}

func TestARefineWithNoWorktreeWhoseSessionIsRevokedWhileItWaitsForAdmissionIsRefusedAtCommit(t *testing.T) {
	reviewer, runner := &blockingReviewer{entered: make(chan struct{}), release: make(chan struct{})}, &recordingRunner{}
	f := newWebFixture(t, testUI, func(e *Execution) { e.Review, e.Runner, e.EngineSHA256 = reviewer, runner, strings.Repeat("a", 64) })
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	f.exec(t, "UPDATE tasks SET state='failed',revision=4 WHERE id='t'")
	f.exec(t, "UPDATE attempts SET state='ended_failed' WHERE id='a'")
	refined := make(chan reply, 1)
	go func() { refined <- f.keyed(t, "POST", "/v1/tasks/t/refine", cookie, "refine", "4", refineBody) }()
	waitFor(t, reviewer.entered, "the clone check")
	// A stop holds admission; the refine, with nothing to release, waits for it after the clone check.
	f.n.h.admission.mu.Lock()
	close(reviewer.release)
	if r := f.owner(t, "DELETE", "/v1/sessions/"+ownerSessionID(t, f), nil); r.status != 204 {
		f.n.h.admission.mu.Unlock()
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	f.n.h.admission.mu.Unlock()
	if r := <-refined; r.status != 401 {
		t.Fatalf("the refine: %d %s", r.status, r.body)
	}
	if len(runner.launches()) != 0 {
		t.Fatalf("launches %v", runner.launches())
	}
	if got := f.column(t, "SELECT state FROM tasks WHERE id='t'"); len(got) != 1 || string(got[0]) != "failed" {
		t.Fatalf("the task is %s", got)
	}
}

func TestARefineWhoseRequesterIsRemovedDuringTheCloneCheckIsRefusedWithNoWorktreeTouched(t *testing.T) {
	reviewer, runner := &blockingReviewer{entered: make(chan struct{}), release: make(chan struct{})}, &recordingRunner{}
	f := newWebFixture(t, testUI, func(e *Execution) { e.Review, e.Runner, e.EngineSHA256 = reviewer, runner, strings.Repeat("a", 64) })
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	refineSeed(t, f)
	f.exec(t, "INSERT INTO users(id,name,created) VALUES('w','Walt','now')")
	f.exec(t, "INSERT INTO project_members(project,user,role,added) VALUES('p','w','owner','now')")
	refined := make(chan reply, 1)
	go func() { refined <- f.keyed(t, "POST", "/v1/tasks/t/refine", cookie, "refine", "4", refineBody) }()
	waitFor(t, reviewer.entered, "the clone check")
	if r := f.owner(t, "DELETE", "/v1/projects/p/members/local", nil); r.status != 200 {
		t.Fatalf("remove: %d %s", r.status, r.body)
	}
	close(reviewer.release)
	if r := <-refined; r.status != 404 {
		t.Fatalf("the refine: %d %s", r.status, r.body)
	}
	if calls := reviewer.recorded(); len(calls) != 0 || len(runner.launches()) != 0 {
		t.Fatalf("calls %v, launches %v", calls, runner.launches())
	}
	if got := f.column(t, "SELECT COALESCE(removed,'')||'|'||tested_commit FROM workspaces WHERE attempt='a'"); len(got) != 1 || string(got[0]) != "|" {
		t.Fatalf("the worktree's record changed: %q", got)
	}
}
