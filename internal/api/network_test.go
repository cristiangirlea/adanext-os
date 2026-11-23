package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/store"
)

const (
	testRPID   = "home.adanext.si"
	testOrigin = "https://home.adanext.si"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type netFixture struct {
	s      *store.Store
	hub    *Hub
	n      *network
	local  http.Handler
	clock  *fakeClock
	server *httptest.Server
	keys   int
	events sync.Once
}

// fastKey stands in for Argon2id where a test is about something else.
func fastKey(password, salt []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
	sum := sha256.Sum256(append(append([]byte{}, salt...), password...))
	return sum[:keyLen]
}

// newNetFixture serves both channels over one store and hub: the owner account "local" with a
// project, a task and an attempt, the network channel on a TLS test server.
func newNetFixture(t *testing.T) *netFixture {
	t.Helper()
	f := newColdNetFixture(t)
	if err := f.n.hasher.calibrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

// newColdNetFixture is newNetFixture as a coordinator first starts: nothing hashed yet.
func newColdNetFixture(t *testing.T) *netFixture {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateUser(ctx, "local", "Local"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(ctx, "local", "seed", "seed", nil, func(tx *store.Tx) ([]byte, error) {
		for _, q := range []string{
			"INSERT INTO projects(id,name,source_path,created) VALUES('p','p','p','now')",
			"INSERT INTO project_members(project,user,role,added) VALUES('p','local','owner','now')",
			"INSERT INTO tasks(id,project,created_by,objective,acceptance_criteria,state,created,updated) VALUES('t','p','local','objective','criteria','draft','now','now')",
			"INSERT INTO attempts(id,task,actor,backend,model,effort,executor,ownership_generation,engine_sha256,state,started) VALUES('a','t','local','codex','','','local',0,'hash','running','now')",
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return nil, err
			}
		}
		return []byte("ok"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	hub.PollInterval = 5 * time.Millisecond
	hub.HeartbeatInterval = 5 * time.Millisecond
	clock := &fakeClock{now: time.Now().UTC()}
	exec := Execution{Owner: "local", Clock: clock.Now}
	n, err := newNetwork(s, hub, NetworkConfig{RPID: testRPID, Origin: testOrigin, Owner: "local", Clock: clock.Now}, exec)
	if err != nil {
		t.Fatal(err)
	}
	n.hasher.key = fastKey
	server := httptest.NewTLSServer(withRequestID(n))
	t.Cleanup(server.Close)
	t.Cleanup(hub.Close)
	local := New(s, hub, func(*http.Request) (string, error) { return "local", nil }, exec)
	return &netFixture{s: s, hub: hub, n: n, local: local, clock: clock, server: server}
}

type reply struct {
	status int
	body   []byte
	header http.Header
	cookie string // the session cookie set, "" when none
	unset  bool   // the session cookie was cleared
}

func (r reply) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("%d %s: %v", r.status, r.body, err)
	}
	return v
}

func (r reply) code(t *testing.T) string {
	t.Helper()
	var v struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(r.body, &v)
	return v.Error
}

func marshal(t *testing.T, body any) []byte {
	t.Helper()
	if body == nil {
		return nil
	}
	if raw, ok := body.([]byte); ok {
		return raw
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readReply(t *testing.T, resp *http.Response) reply {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	out := reply{status: resp.StatusCode, body: data, header: resp.Header}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			if c.MaxAge < 0 || c.Value == "" {
				out.unset = true
			} else {
				out.cookie = c.Value
			}
		}
	}
	return out
}

// call sends a network request over TLS with the session cookie, when given.
func (f *netFixture) call(t *testing.T, method, path, cookie string, body any) reply {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, bytes.NewReader(marshal(t, body)))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return readReply(t, resp)
}

// from sends a network request from a given TCP peer, straight to the handler.
func (f *netFixture) from(t *testing.T, address, method, path string, body any) reply {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(marshal(t, body)))
	req.RemoteAddr = address + ":40000"
	rec := httptest.NewRecorder()
	withRequestID(f.n).ServeHTTP(rec, req)
	return readReply(t, rec.Result())
}

// owner sends a request on the local channel, with a fresh idempotency key for a mutation.
func (f *netFixture) owner(t *testing.T, method, path string, body any) reply {
	t.Helper()
	f.keys++
	return f.ownerWithKey(t, method, path, "key-"+strings.Repeat("k", f.keys), body)
}

func (f *netFixture) ownerWithKey(t *testing.T, method, path, key string, body any) reply {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(marshal(t, body)))
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	f.local.ServeHTTP(rec, req)
	return readReply(t, rec.Result())
}

// account creates an account through the owner's channel and returns its id and code.
func (f *netFixture) account(t *testing.T, name string, passwordAllowed bool) (string, string) {
	t.Helper()
	r := f.owner(t, "POST", "/v1/accounts", map[string]any{"name": name, "password_allowed": passwordAllowed})
	if r.status != 201 {
		t.Fatalf("create account: %d %s", r.status, r.body)
	}
	v := r.json(t)
	return v["account"].(map[string]any)["id"].(string), v["code"].(string)
}

func (f *netFixture) newCode(t *testing.T, id string) string {
	t.Helper()
	r := f.owner(t, "POST", "/v1/accounts/"+id+"/enrollment-codes", nil)
	if r.status != 201 {
		t.Fatalf("issue code: %d %s", r.status, r.body)
	}
	return r.json(t)["code"].(string)
}

// enroll registers key with the code and returns the reply of the registration.
func (f *netFixture) enroll(t *testing.T, code string, key *softKey) reply {
	t.Helper()
	r := f.call(t, "POST", "/v1/enrollment/options", "", map[string]any{"code": code})
	if r.status != 200 {
		t.Fatalf("enrollment options: %d %s", r.status, r.body)
	}
	v := r.json(t)
	credential := key.create(t, v["options"].(map[string]any), testOrigin)
	return f.call(t, "POST", "/v1/enrollment/passkey", "", map[string]any{"code": code, "ceremony": v["ceremony"], "credential": credential, "name": "laptop"})
}

func (f *netFixture) mustEnroll(t *testing.T, code string, key *softKey) string {
	t.Helper()
	r := f.enroll(t, code, key)
	if r.status != 201 || r.cookie == "" {
		t.Fatalf("enroll: %d %s", r.status, r.body)
	}
	return r.cookie
}

// login signs in with the key and returns the reply of the assertion.
func (f *netFixture) login(t *testing.T, key *softKey, cookie string) reply {
	t.Helper()
	r := f.call(t, "POST", "/v1/session/passkey/options", "", nil)
	if r.status != 200 {
		t.Fatalf("login options: %d %s", r.status, r.body)
	}
	v := r.json(t)
	credential := key.get(t, v["options"].(map[string]any), testOrigin)
	return f.call(t, "POST", "/v1/session/passkey", cookie, map[string]any{"ceremony": v["ceremony"], "credential": credential})
}

func (f *netFixture) mustLogin(t *testing.T, key *softKey) string {
	t.Helper()
	r := f.login(t, key, "")
	if r.status != 200 || r.cookie == "" {
		t.Fatalf("login: %d %s", r.status, r.body)
	}
	return r.cookie
}

func (f *netFixture) signedIn(t *testing.T, cookie string) bool {
	t.Helper()
	r := f.call(t, "GET", "/v1/account", cookie, nil)
	switch r.status {
	case 200:
		return true
	case 401:
		return false
	}
	t.Fatalf("account: %d %s", r.status, r.body)
	return false
}

func TestPasskeyRegistrationAndDiscoverableLoginWithASoftwareAuthenticator(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", false)
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{5}(-[0-9A-HJKMNP-TV-Z]{5}){3}$`).MatchString(code) {
		t.Fatalf("code %q is not grouped Crockford base32", code)
	}
	key := newSoftKey(t)
	options := f.call(t, "POST", "/v1/enrollment/options", "", map[string]any{"code": strings.ToLower(code)}).json(t)
	creation := options["options"].(map[string]any)
	selection := creation["authenticatorSelection"].(map[string]any)
	if selection["residentKey"] != "required" || selection["userVerification"] != "required" || creation["attestation"] != "none" || creation["rp"].(map[string]any)["id"] != testRPID {
		t.Fatalf("creation options: %v", creation)
	}
	if options["account"].(map[string]any)["name"] != "Vera" {
		t.Fatalf("account in the options: %v", options["account"])
	}
	enrolled := f.call(t, "POST", "/v1/enrollment/passkey", "", map[string]any{"code": code, "ceremony": options["ceremony"], "credential": key.create(t, creation, testOrigin), "name": "laptop"})
	if enrolled.status != 201 || enrolled.cookie == "" || enrolled.json(t)["account"].(map[string]any)["id"] != id {
		t.Fatalf("enrolled: %d %s", enrolled.status, enrolled.body)
	}
	if len(key.handle) != 64 {
		t.Fatalf("user handle of %d bytes", len(key.handle))
	}
	request := f.call(t, "POST", "/v1/session/passkey/options", "", nil).json(t)["options"].(map[string]any)
	if request["userVerification"] != "required" || request["allowCredentials"] != nil {
		t.Fatalf("request options are not discoverable with verification: %v", request)
	}
	cookie := f.mustLogin(t, key)
	if cookie == enrolled.cookie {
		t.Fatal("a login reused the enrollment's token")
	}
	account := f.call(t, "GET", "/v1/account", cookie, nil).json(t)
	if account["id"] != id || account["name"] != "Vera" || account["owner"] != false || account["password_allowed"] != false {
		t.Fatalf("account = %v", account)
	}
	var credentials []map[string]any
	if err := json.Unmarshal(f.call(t, "GET", "/v1/account/credentials", cookie, nil).body, &credentials); err != nil || len(credentials) != 1 || credentials[0]["kind"] != "passkey" || credentials[0]["name"] != "laptop" || credentials[0]["last_used"] == "" {
		t.Fatalf("credentials = %v, %v", credentials, err)
	}
}

func TestARegistrationIsRefusedForAWrongOriginRPIDOrMissingVerification(t *testing.T) {
	f := newNetFixture(t)
	id, _ := f.account(t, "Vera", false)
	for name, bend := range map[string]func(*softKey){
		"origin":       func(k *softKey) { k.origin = "https://evil.example" },
		"rp id":        func(k *softKey) { k.rpID = "evil.example" },
		"verification": func(k *softKey) { k.noUV = true },
	} {
		key := newSoftKey(t)
		bend(key)
		code := f.newCode(t, id)
		if r := f.enroll(t, code, key); r.status != 400 || r.code(t) != "credential_invalid" || r.cookie != "" {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	if a, err := f.s.Account(context.Background(), id); err != nil || a.Credentials != 0 || a.Sessions != 0 {
		t.Fatalf("after refused registrations: %+v, %v", a, err)
	}
}

func TestALoginIsRefusedForAWrongOriginRPIDMissingVerificationOrABackwardSignCount(t *testing.T) {
	f := newNetFixture(t)
	_, code := f.account(t, "Vera", false)
	key := newSoftKey(t)
	f.mustEnroll(t, code, key)
	for name, bend := range map[string]func(*softKey){
		"origin":       func(k *softKey) { k.origin = "https://evil.example" },
		"rp id":        func(k *softKey) { k.rpID = "evil.example" },
		"verification": func(k *softKey) { k.noUV = true },
	} {
		bent := *key
		bend(&bent)
		if r := f.login(t, &bent, ""); r.status != 401 || r.code(t) != "login_failed" || r.cookie != "" {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	f.mustLogin(t, key)
	key.count -= 2 // the stored count is now ahead of what the key reports
	if r := f.login(t, key, ""); r.status != 401 || r.code(t) != "credential_invalid" || r.cookie != "" {
		t.Fatalf("a backward sign count: %d %s", r.status, r.body)
	}
	key.count = 1000
	key.frozen = true
	f.mustLogin(t, key)
	if r := f.login(t, key, ""); r.status != 401 || r.code(t) != "credential_invalid" {
		t.Fatalf("a repeated sign count: %d %s", r.status, r.body)
	}
}

func TestACeremonyIsAnsweredOnceAndExpiresAfterFiveMinutes(t *testing.T) {
	f := newNetFixture(t)
	_, code := f.account(t, "Vera", false)
	key := newSoftKey(t)
	f.mustEnroll(t, code, key)
	v := f.call(t, "POST", "/v1/session/passkey/options", "", nil).json(t)
	credential := key.get(t, v["options"].(map[string]any), testOrigin)
	f.clock.Advance(ceremonyLifetime)
	if r := f.call(t, "POST", "/v1/session/passkey", "", map[string]any{"ceremony": v["ceremony"], "credential": credential}); r.status != 400 || r.code(t) != "ceremony_invalid" {
		t.Fatalf("an expired ceremony: %d %s", r.status, r.body)
	}
	v = f.call(t, "POST", "/v1/session/passkey/options", "", nil).json(t)
	credential = key.get(t, v["options"].(map[string]any), testOrigin)
	if r := f.call(t, "POST", "/v1/session/passkey", "", map[string]any{"ceremony": v["ceremony"], "credential": credential}); r.status != 200 {
		t.Fatalf("login: %d %s", r.status, r.body)
	}
	if r := f.call(t, "POST", "/v1/session/passkey", "", map[string]any{"ceremony": v["ceremony"], "credential": credential}); r.status != 400 || r.code(t) != "ceremony_invalid" {
		t.Fatalf("a replayed assertion: %d %s", r.status, r.body)
	}
}

func TestPendingCeremoniesAreBoundedDroppingTheOldest(t *testing.T) {
	c := newCeremonies()
	now := time.Now()
	first := &ceremony{kind: loginCeremony, expires: now.Add(time.Minute)}
	if err := c.put(first); err != nil {
		t.Fatal(err)
	}
	for range ceremonyLimit {
		if err := c.put(&ceremony{kind: loginCeremony, expires: now.Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if c.len() != ceremonyLimit {
		t.Fatalf("%d pending", c.len())
	}
	if _, ok := c.take(first.id, loginCeremony, now); ok {
		t.Fatal("the oldest ceremony was kept past the limit")
	}
}

func TestALoginNeverPromotesACookieSentBeforeIt(t *testing.T) {
	f := newNetFixture(t)
	_, code := f.account(t, "Vera", false)
	key := newSoftKey(t)
	first := f.mustEnroll(t, code, key)
	planted, _, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, before := range []string{planted, first} {
		r := f.login(t, key, before)
		if r.status != 200 || r.cookie == "" || r.cookie == before {
			t.Fatalf("login with a cookie: %d %s, cookie %q", r.status, r.body, r.cookie)
		}
		if !f.signedIn(t, r.cookie) {
			t.Fatal("the new session is not signed in")
		}
	}
	if f.signedIn(t, planted) {
		t.Fatal("a cookie from before the login was promoted")
	}
	if !f.signedIn(t, first) {
		t.Fatal("a login ended an existing session of its own account")
	}
}

func TestTheSessionCookieIsHostOnlySecureHttpOnlyStrictAndForTheBrowserSession(t *testing.T) {
	f := newNetFixture(t)
	_, code := f.account(t, "Vera", false)
	r := f.enroll(t, code, newSoftKey(t))
	header := r.header.Get("Set-Cookie")
	for _, want := range []string{sessionCookie + "=", "Path=/", "HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(header, want) {
			t.Fatalf("Set-Cookie %q lacks %q", header, want)
		}
	}
	for _, unwanted := range []string{"Domain", "Max-Age", "Expires"} {
		if strings.Contains(header, unwanted) {
			t.Fatalf("Set-Cookie %q has %q", header, unwanted)
		}
	}
}

func TestStoredSecretsNeverEqualTheirInputs(t *testing.T) {
	f := newNetFixture(t)
	f.n.hasher.key = newHasher().key
	id, code := f.account(t, "Vera", true)
	// The code's digest, read before the code is used.
	normalized, _ := normalizeCode(code)
	digests := f.column(t, "SELECT code_hash FROM enrollment_codes WHERE user=?", id)
	if want := sha256.Sum256([]byte(normalized)); len(digests) != 1 || !bytes.Equal(digests[0], want[:]) {
		t.Fatalf("stored code digests %x", digests)
	}
	for _, form := range []string{normalized, code} {
		if bytes.Contains(digests[0], []byte(form)) {
			t.Fatalf("the stored code holds %q", form)
		}
	}
	password := "correct horse battery staple"
	r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": password})
	if r.status != 201 || r.cookie == "" {
		t.Fatalf("password enrollment: %d %s", r.status, r.body)
	}
	ctx := context.Background()
	sessions, err := f.s.Sessions(ctx, id, store.Live{})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions = %v, %v", sessions, err)
	}
	if bytes.Equal(sessions[0].TokenHash, []byte(r.cookie)) || bytes.Contains(sessions[0].TokenHash, []byte(r.cookie)) {
		t.Fatal("the stored session hash is the token")
	}
	raw, err := b64u.DecodeString(r.cookie)
	if want := sha256.Sum256(raw); err != nil || len(raw) != 32 || !bytes.Equal(sessions[0].TokenHash, want[:]) {
		t.Fatal("the stored session hash is not the SHA-256 of the token's 32 bytes")
	}
	pw, err := f.s.PasswordCredential(ctx, id)
	if err != nil || strings.Contains(pw.PasswordHash, password) || !strings.HasPrefix(pw.PasswordHash, "$argon2id$v=19$m=65536,t=3,p=1$") {
		t.Fatalf("password hash %q, %v", pw.PasswordHash, err)
	}
	if r := f.call(t, "POST", "/v1/session/password", "", map[string]any{"account": "Vera", "password": password}); r.status != 200 || r.cookie == "" {
		t.Fatalf("password login with Argon2id: %d %s", r.status, r.body)
	}
	if r := f.call(t, "POST", "/v1/session/password", "", map[string]any{"account": "Vera", "password": password + "!"}); r.status != 401 || r.code(t) != "login_failed" {
		t.Fatalf("a wrong password: %d %s", r.status, r.body)
	}
}

func TestPasswordsAreRefusedWhereNotAllowedOrTooShort(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", false)
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": "long enough password"}); r.status != 403 || r.code(t) != "password_not_allowed" {
		t.Fatalf("not allowed: %d %s", r.status, r.body)
	}
	if r := f.owner(t, "PATCH", "/v1/accounts/"+id, map[string]any{"password_allowed": true}); r.status != 200 || r.json(t)["password_allowed"] != true {
		t.Fatalf("allow a password: %d %s", r.status, r.body)
	}
	for _, short := range []string{"elevenchars", strings.Repeat("é", 11), strings.Repeat("a", maxPasswordBytes+1)} {
		if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": short}); r.status != 400 || r.code(t) != "password_invalid" {
			t.Fatalf("password %d bytes: %d %s", len(short), r.status, r.body)
		}
	}
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": strings.Repeat("é", 12)}); r.status != 201 {
		t.Fatalf("twelve characters: %d %s", r.status, r.body)
	}
	if r := f.owner(t, "PATCH", "/v1/accounts/"+id, map[string]any{"password_allowed": false}); r.status != 200 {
		t.Fatalf("disallow: %d %s", r.status, r.body)
	}
	if r := f.call(t, "POST", "/v1/session/password", "", map[string]any{"account": "Vera", "password": strings.Repeat("é", 12)}); r.status != 401 {
		t.Fatalf("a password login where none is allowed: %d %s", r.status, r.body)
	}
}

func TestAStoredHashBelowTheCurrentParametersIsReplacedAtLogin(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", true)
	password := "a password of some length"
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": password}); r.status != 201 {
		t.Fatalf("enroll: %d %s", r.status, r.body)
	}
	ctx := context.Background()
	pw, _ := f.s.PasswordCredential(ctx, id)
	weak := argonParams{memory: 1024, time: 1, threads: 1, saltLen: 16, keyLen: 32}
	salt := bytes.Repeat([]byte{7}, 16)
	f.exec(t, "UPDATE credentials SET password_hash=? WHERE id=?", encodePHC(weak, salt, fastKey([]byte(password), salt, 0, 0, 0, 32)), pw.ID)
	if r := f.call(t, "POST", "/v1/session/password", "", map[string]any{"account": "Vera", "password": password}); r.status != 200 {
		t.Fatalf("login: %d %s", r.status, r.body)
	}
	pw, _ = f.s.PasswordCredential(ctx, id)
	if !strings.Contains(pw.PasswordHash, "$m=65536,t=3,p=1$") {
		t.Fatalf("not rehashed: %s", pw.PasswordHash)
	}
}

func TestAtMostTwoArgon2idHashesRunAtOnce(t *testing.T) {
	h := newHasher()
	var running, peak atomic.Int32
	release := make(chan struct{})
	h.key = func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return make([]byte, keyLen)
	}
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			if i%2 == 0 {
				_, _ = h.hash(context.Background(), "a password to hash")
			} else {
				_, _, _ = h.verify(context.Background(), "a password to check", dummyPHC)
			}
		})
	}
	// Every worker has reached the slots: two hold one, four wait for one.
	deadline := time.Now().Add(10 * time.Second)
	for (running.Load() < 2 || h.queued.Load() < 4) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got, waiting := running.Load(), h.queued.Load(); got != 2 || waiting != 4 {
		t.Fatalf("%d hashes running and %d waiting of six", got, waiting)
	}
	close(release)
	wg.Wait()
	if peak.Load() != 2 {
		t.Fatalf("peak of %d hashes at once", peak.Load())
	}
}

func TestALoginWithoutAUsablePasswordStillComputesAHash(t *testing.T) {
	f := newNetFixture(t)
	var computed atomic.Int32
	f.n.hasher.key = func(password, salt []byte, t, m uint32, th uint8, kl uint32) []byte {
		computed.Add(1)
		return fastKey(password, salt, t, m, th, kl)
	}
	disabledID, _ := f.account(t, "Disabled", true)
	f.owner(t, "PATCH", "/v1/accounts/"+disabledID, map[string]any{"disabled": true})
	f.account(t, "NoPassword", true)
	f.account(t, "NotAllowed", false)
	for _, name := range []string{"nobody", "Disabled", "NoPassword", "NotAllowed"} {
		before := computed.Load()
		r := f.from(t, "198.51.100."+string(rune('1'+len(name)%8)), "POST", "/v1/session/password", map[string]any{"account": name, "password": "some password here"})
		if r.status != 401 || r.code(t) != "login_failed" {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
		if computed.Load() != before+1 {
			t.Fatalf("%s: %d hashes", name, computed.Load()-before)
		}
	}
}

func TestEnrollmentCodesAreSingleUseAndExpireAfterADay(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", false)
	f.clock.Advance(store.CodeLifetime)
	if r := f.call(t, "POST", "/v1/enrollment/options", "", map[string]any{"code": code}); r.status != 400 || r.code(t) != "code_invalid" {
		t.Fatalf("an expired code: %d %s", r.status, r.body)
	}
	code = f.newCode(t, id)
	f.mustEnroll(t, code, newSoftKey(t))
	if r := f.call(t, "POST", "/v1/enrollment/options", "", map[string]any{"code": code}); r.status != 400 || r.code(t) != "code_invalid" {
		t.Fatalf("a used code: %d %s", r.status, r.body)
	}
	// People type codes loosely.
	if got, ok := normalizeCode(" abcde-fghjk mnpqr-stvwi "); !ok || got != "ABCDEFGHJKMNPQRSTVW1" {
		t.Fatalf("normalized %q, %v", got, ok)
	}
	if got, ok := normalizeCode("OoLl1-00000-00000-00000"); !ok || got != "00111000000000000000" {
		t.Fatalf("normalized %q, %v", got, ok)
	}
	if _, ok := normalizeCode("UUUUU-00000-00000-00000"); ok {
		t.Fatal("U is not a Crockford symbol")
	}
}

func TestTheAccountThrottleDoublesFromOneSecondAndASuccessResetsIt(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", true)
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": "the right password"}); r.status != 201 {
		t.Fatalf("enroll: %d %s", r.status, r.body)
	}
	attempt := func(address, password string) reply {
		return f.from(t, address, "POST", "/v1/session/password", map[string]any{"account": "Vera", "password": password})
	}
	// Each failure from its own address, so only the account's throttle is under test.
	for i := range 5 {
		if r := attempt("203.0.113."+string(rune('0'+i)), "wrong password!!"); r.status != 401 {
			t.Fatalf("failure %d: %d %s", i, r.status, r.body)
		}
	}
	r := attempt("203.0.113.9", "the right password")
	if r.status != 429 || r.code(t) != "throttled" || r.header.Get("Retry-After") != "1" {
		t.Fatalf("after five failures: %d %s, Retry-After %q", r.status, r.body, r.header.Get("Retry-After"))
	}
	f.clock.Advance(time.Second)
	if r := attempt("203.0.113.9", "wrong password!!"); r.status != 401 {
		t.Fatalf("the sixth failure: %d %s", r.status, r.body)
	}
	if r := attempt("203.0.113.9", "the right password"); r.status != 429 || r.header.Get("Retry-After") != "2" {
		t.Fatalf("after six failures: %d, Retry-After %q", r.status, r.header.Get("Retry-After"))
	}
	for range 20 {
		f.clock.Advance(accountMaxDelay)
		attempt("203.0.113.10", "wrong password!!")
	}
	if r := attempt("203.0.113.11", "the right password"); r.status != 429 || r.header.Get("Retry-After") != "900" {
		t.Fatalf("the delay is capped at 15 minutes: %d, Retry-After %q", r.status, r.header.Get("Retry-After"))
	}
	f.clock.Advance(accountMaxDelay)
	if r := attempt("203.0.113.11", "the right password"); r.status != 200 {
		t.Fatalf("no permanent lockout: %d %s", r.status, r.body)
	}
	if r := attempt("203.0.113.12", "wrong password!!"); r.status != 401 {
		t.Fatalf("a failure after the success is not delayed: %d %s", r.status, r.body)
	}
	if r := attempt("203.0.113.12", "the right password"); r.status != 200 {
		t.Fatalf("a success reset the count: %d %s", r.status, r.body)
	}
	// The owner's recovery clears a running delay.
	for range 6 {
		attempt("203.0.113.13", "wrong password!!")
	}
	if r := attempt("203.0.113.14", "the right password"); r.status != 429 {
		t.Fatalf("throttled again: %d", r.status)
	}
	if r := f.owner(t, "POST", "/v1/accounts/"+id+"/recover", nil); r.status != 201 {
		t.Fatalf("recover: %d %s", r.status, r.body)
	}
	if r := attempt("203.0.113.14", "the right password"); r.status != 200 {
		t.Fatalf("after recovery: %d %s", r.status, r.body)
	}
}

func TestTheAddressThrottleBlocksTwentyFailuresInTenMinutesForFifteen(t *testing.T) {
	f := newNetFixture(t)
	_, code := f.account(t, "Vera", false)
	bad := func(address string) reply {
		return f.from(t, address, "POST", "/v1/enrollment/options", map[string]any{"code": "00000-00000-00000-00000"})
	}
	for i := range 19 {
		if r := bad("192.0.2.7"); r.status != 400 {
			t.Fatalf("failure %d: %d", i, r.status)
		}
		f.clock.Advance(30 * time.Second)
	}
	f.clock.Advance(30 * time.Second) // the first failure leaves the window
	if r := bad("192.0.2.7"); r.status != 400 {
		t.Fatalf("nineteen in the window: %d", r.status)
	}
	if r := bad("192.0.2.7"); r.status != 400 {
		t.Fatalf("the twentieth failure is answered: %d", r.status)
	}
	r := f.from(t, "192.0.2.7", "POST", "/v1/enrollment/options", map[string]any{"code": code})
	if r.status != 429 || r.header.Get("Retry-After") != "900" {
		t.Fatalf("a good code from a blocked address: %d %s, Retry-After %q", r.status, r.body, r.header.Get("Retry-After"))
	}
	if r := f.from(t, "192.0.2.8", "POST", "/v1/enrollment/options", map[string]any{"code": code}); r.status != 200 {
		t.Fatalf("another address: %d %s", r.status, r.body)
	}
	f.clock.Advance(addressBlock)
	if r := f.from(t, "192.0.2.7", "POST", "/v1/enrollment/options", map[string]any{"code": code}); r.status != 200 {
		t.Fatalf("after the block: %d %s", r.status, r.body)
	}
}

func TestTheThrottleTracksAtMostItsLimitDroppingTheLeastRecentlySeen(t *testing.T) {
	l := lru[*addressThrottle]{limit: 3}
	for _, k := range []string{"a", "b", "c"} {
		l.getOrAdd(k, func() *addressThrottle { return &addressThrottle{} })
	}
	l.get("a")
	l.getOrAdd("d", func() *addressThrottle { return &addressThrottle{} })
	if _, ok := l.get("b"); ok || l.len() != 3 {
		t.Fatalf("b kept, %d tracked", l.len())
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := l.get(k); !ok {
			t.Fatalf("%s dropped", k)
		}
	}
	var unbounded throttle
	now := time.Now()
	for i := range throttleTracked + 10 {
		unbounded.fail("", strings.Repeat("x", i%7)+string(rune(i)), now)
	}
	if unbounded.addresses.len() != throttleTracked {
		t.Fatalf("%d addresses tracked", unbounded.addresses.len())
	}
}

func TestSessionsExpireAfterEightIdleHoursAndSevenDays(t *testing.T) {
	f := newNetFixture(t)
	_, code := f.account(t, "Vera", false)
	key := newSoftKey(t)
	idle := f.mustEnroll(t, code, key)
	f.clock.Advance(sessionIdle - time.Minute)
	if !f.signedIn(t, idle) {
		t.Fatal("expired before eight idle hours")
	}
	f.clock.Advance(sessionIdle - time.Second)
	if !f.signedIn(t, idle) {
		t.Fatal("the last use did not move the idle expiry")
	}
	f.clock.Advance(sessionIdle)
	if f.signedIn(t, idle) {
		t.Fatal("alive after eight idle hours")
	}
	if n := len(mustSessions(t, f, "")); n != 0 {
		t.Fatalf("%d sessions left after the idle expiry", n)
	}
	busy := f.mustLogin(t, key)
	start := f.clock.Now()
	used := start.Add(sessionAbsolute - time.Hour)
	for f.clock.Now().Add(sessionIdle / 2).Before(used) {
		f.clock.Advance(sessionIdle / 2)
		if !f.signedIn(t, busy) {
			t.Fatalf("expired after %s of use", f.clock.Now().Sub(start))
		}
	}
	f.clock.Advance(used.Sub(f.clock.Now()))
	if !f.signedIn(t, busy) {
		t.Fatal("expired an hour before seven days")
	}
	id := mustSessions(t, f, "")[0].ID
	// Two hours after the last use: well inside the idle window, past the absolute one.
	f.clock.Advance(2 * time.Hour)
	if f.signedIn(t, busy) {
		t.Fatal("alive after seven days")
	}
	if _, err := f.s.Session(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the expired row stayed: %v", err)
	}
}

func TestLastUseIsWrittenAtMostOnceAMinute(t *testing.T) {
	f := newNetFixture(t)
	_, code := f.account(t, "Vera", false)
	cookie := f.mustEnroll(t, code, newSoftKey(t))
	first := mustSessions(t, f, "")[0].LastUsed
	f.clock.Advance(59 * time.Second)
	f.signedIn(t, cookie)
	if got := mustSessions(t, f, "")[0].LastUsed; got != first {
		t.Fatalf("written within the minute: %s, %s", first, got)
	}
	f.clock.Advance(time.Second)
	f.signedIn(t, cookie)
	if got := mustSessions(t, f, "")[0].LastUsed; got != store.SessionStamp(f.clock.Now()) {
		t.Fatalf("not written after a minute: %s", got)
	}
}

func mustSessions(t *testing.T, f *netFixture, user string) []store.Session {
	t.Helper()
	sessions, err := f.s.Sessions(context.Background(), user, store.Live{})
	if err != nil {
		t.Fatal(err)
	}
	return sessions
}

// openStream opens the attempt's event stream on the network session; the events route is
// not on the network list yet, so the test adds it to this handler alone.
func (f *netFixture) openStream(t *testing.T, cookie string) *http.Response {
	t.Helper()
	f.eventsRoute()
	req, err := http.NewRequest("GET", f.server.URL+"/v1/attempts/a/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "retry:") {
		t.Fatalf("stream start %q, %v", line, err)
	}
	return resp
}

// streamEnds waits for the stream's body to end.
func streamEnds(t *testing.T, resp *http.Response) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		resp.Body.Close()
		t.Fatal("the stream stayed open")
	}
	resp.Body.Close()
}

// ownerSessions enrolls the owner's passkey and opens two sessions with a stream each.
func ownerSessions(t *testing.T, f *netFixture) (*softKey, [2]string, [2]*http.Response) {
	t.Helper()
	key := newSoftKey(t)
	first := f.mustEnroll(t, f.newCode(t, "local"), key)
	second := f.mustLogin(t, key)
	cookies := [2]string{first, second}
	return key, cookies, [2]*http.Response{f.openStream(t, first), f.openStream(t, second)}
}

func TestACredentialAddedEndsEverySessionOfTheAccountAndClosesTheirStreams(t *testing.T) {
	f := newNetFixture(t)
	_, cookies, streams := ownerSessions(t, f)
	f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	for i := range 2 {
		streamEnds(t, streams[i])
		if f.signedIn(t, cookies[i]) {
			t.Fatalf("session %d survived a new credential", i)
		}
	}
}

func TestACredentialRemovedEndsEverySessionOfTheAccountAndClosesTheirStreams(t *testing.T) {
	f := newNetFixture(t)
	key, _, _ := ownerSessions(t, f)
	f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	cookies := [2]string{f.mustLogin(t, key), f.mustLogin(t, key)}
	streams := [2]*http.Response{f.openStream(t, cookies[0]), f.openStream(t, cookies[1])}
	var credentials []map[string]any
	if err := json.Unmarshal(f.call(t, "GET", "/v1/account/credentials", cookies[0], nil).body, &credentials); err != nil || len(credentials) != 2 {
		t.Fatalf("credentials %v, %v", credentials, err)
	}
	kept, err := f.s.PasskeyByCredentialID(context.Background(), key.id)
	if err != nil {
		t.Fatal(err)
	}
	other := credentials[0]["id"].(string)
	if other == kept.ID {
		other = credentials[1]["id"].(string)
	}
	r := f.call(t, "DELETE", "/v1/account/credentials/"+other, cookies[0], nil)
	if r.status != 204 || !r.unset {
		t.Fatalf("remove: %d %s", r.status, r.body)
	}
	for i := range 2 {
		streamEnds(t, streams[i])
		if f.signedIn(t, cookies[i]) {
			t.Fatalf("session %d survived a removed credential", i)
		}
	}
	last := f.mustLogin(t, key)
	if r := f.call(t, "DELETE", "/v1/account/credentials/"+kept.ID, last, nil); r.status != 409 || r.code(t) != "last_credential" {
		t.Fatalf("the last credential: %d %s", r.status, r.body)
	}
}

func TestDisablingAnAccountEndsEverySessionAndClosesTheirStreamsAndLoginIsRefused(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", false)
	if err := f.s.AddMember(context.Background(), "p", id, "member"); err != nil {
		t.Fatal(err)
	}
	key := newSoftKey(t)
	cookies := [2]string{f.mustEnroll(t, code, key), f.mustLogin(t, key)}
	// No poll or heartbeat comes in time: the deletion itself must close the streams.
	f.hub.PollInterval, f.hub.HeartbeatInterval = time.Hour, time.Hour
	streams := [2]*http.Response{f.openStream(t, cookies[0]), f.openStream(t, cookies[1])}
	if r := f.owner(t, "PATCH", "/v1/accounts/"+id, map[string]any{"disabled": true}); r.status != 200 || r.json(t)["disabled"] != true {
		t.Fatalf("disable: %d %s", r.status, r.body)
	}
	for i := range 2 {
		streamEnds(t, streams[i])
		if f.signedIn(t, cookies[i]) {
			t.Fatalf("session %d survived the disable", i)
		}
	}
	if r := f.login(t, key, ""); r.status != 401 || r.cookie != "" {
		t.Fatalf("a disabled account's login: %d %s", r.status, r.body)
	}
	if r := f.owner(t, "PATCH", "/v1/accounts/"+id, map[string]any{"disabled": false}); r.status != 200 {
		t.Fatalf("enable: %d %s", r.status, r.body)
	}
	f.mustLogin(t, key)
}

func TestAnOpenStreamEndsWhenItsSessionExpires(t *testing.T) {
	f := newNetFixture(t)
	key := newSoftKey(t)
	f.mustEnroll(t, f.newCode(t, "local"), key)
	cookie := f.mustLogin(t, key)
	stream := f.openStream(t, cookie)
	f.clock.Advance(sessionIdle)
	streamEnds(t, stream)
}

func TestTheOwnersRevocationsEndNetworkSessionsAndTheirStreams(t *testing.T) {
	f := newNetFixture(t)
	key, cookies, streams := ownerSessions(t, f)
	var sessions []map[string]any
	if err := json.Unmarshal(f.owner(t, "GET", "/v1/sessions", nil).body, &sessions); err != nil || len(sessions) != 2 {
		t.Fatalf("sessions %v, %v", sessions, err)
	}
	if sessions[0]["account"] != "local" || sessions[0]["account_name"] != "Local" || sessions[0]["address"] != "127.0.0.1" || sessions[0]["created"] == "" || sessions[0]["last_used"] == "" {
		t.Fatalf("session = %v", sessions[0])
	}
	for i := range sessions {
		r := f.owner(t, "DELETE", "/v1/sessions/"+sessions[i]["id"].(string), nil)
		if r.status != 204 || len(r.body) != 0 {
			t.Fatalf("revoke: %d %q", r.status, r.body)
		}
	}
	for i := range 2 {
		streamEnds(t, streams[i])
		if f.signedIn(t, cookies[i]) {
			t.Fatalf("session %d survived its revocation", i)
		}
	}
	if r := f.owner(t, "DELETE", "/v1/sessions/unknown", nil); r.status != 404 {
		t.Fatalf("an unknown session: %d", r.status)
	}
	// A user revokes their own sessions only.
	mine := f.mustLogin(t, key)
	other := f.mustLogin(t, key)
	_, vCode := f.account(t, "Vera", false)
	vera := f.mustEnroll(t, vCode, newSoftKey(t))
	var own []map[string]any
	if err := json.Unmarshal(f.call(t, "GET", "/v1/account/sessions", mine, nil).body, &own); err != nil || len(own) != 2 {
		t.Fatalf("own sessions %v, %v", own, err)
	}
	var current, otherID string
	for _, s := range own {
		if s["current"] == true {
			current = s["id"].(string)
		} else {
			otherID = s["id"].(string)
		}
	}
	if current == "" || otherID == "" {
		t.Fatalf("own sessions %v", own)
	}
	if r := f.call(t, "DELETE", "/v1/account/sessions/"+otherID, vera, nil); r.status != 404 {
		t.Fatalf("another account's session: %d %s", r.status, r.body)
	}
	if r := f.call(t, "DELETE", "/v1/account/sessions/"+otherID, mine, nil); r.status != 204 {
		t.Fatalf("own session: %d %s", r.status, r.body)
	}
	if f.signedIn(t, other) || !f.signedIn(t, mine) {
		t.Fatal("the wrong session ended")
	}
	if r := f.call(t, "POST", "/v1/session/logout", mine, nil); r.status != 204 || !r.unset {
		t.Fatalf("logout: %d %s", r.status, r.body)
	}
	if f.signedIn(t, mine) {
		t.Fatal("signed in after logout")
	}
}

func TestTheOwnersFirstPasskeyGoesOnTheLocalAccountWithItsProjectsAndTasks(t *testing.T) {
	f := newNetFixture(t)
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	account := f.call(t, "GET", "/v1/account", cookie, nil).json(t)
	if account["id"] != "local" || account["owner"] != true {
		t.Fatalf("account = %v", account)
	}
	var accounts []map[string]any
	if err := json.Unmarshal(f.owner(t, "GET", "/v1/accounts", nil).body, &accounts); err != nil || len(accounts) != 1 || accounts[0]["credentials"] != 1.0 {
		t.Fatalf("a second account appeared: %v, %v", accounts, err)
	}
	hash, _ := tokenHash(cookie)
	sess, err := f.s.SessionByToken(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := f.s.ListTasks(context.Background(), sess.User)
	if err != nil || len(tasks) != 1 || tasks[0].ID != "t" || tasks[0].Project != "p" {
		t.Fatalf("the session's account sees %v, %v", tasks, err)
	}
}

// ownerChannelRoutes is every owner-channel route with a body it would take.
var ownerChannelRoutes = []struct{ method, path string }{
	{"GET", "/v1/accounts"},
	{"POST", "/v1/accounts"},
	{"PATCH", "/v1/accounts/local"},
	{"DELETE", "/v1/accounts/local"},
	{"POST", "/v1/accounts/local/enrollment-codes"},
	{"POST", "/v1/accounts/local/recover"},
	{"GET", "/v1/sessions"},
	{"DELETE", "/v1/sessions/any"},
}

func TestEveryOwnerChannelRouteIsRefusedOnTheNetworkToTheOwnersFreshSessionToo(t *testing.T) {
	f := newNetFixture(t)
	if len(ownerChannelRoutes) != len(f.n.h.ownerRoutes()) {
		t.Fatalf("the test lists %d routes, the channel %d", len(ownerChannelRoutes), len(f.n.h.ownerRoutes()))
	}
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	_, code := f.account(t, "Vera", false)
	ordinary := f.mustEnroll(t, code, newSoftKey(t))
	for _, route := range ownerChannelRoutes {
		body := map[string]any{"name": "Mallory"}
		if r := f.call(t, route.method, route.path, "", body); r.status != 401 || r.code(t) != "unauthenticated" {
			t.Fatalf("%s %s without a session: %d %s", route.method, route.path, r.status, r.body)
		}
		for who, c := range map[string]string{"the owner's session": cookie, "an ordinary account's session": ordinary} {
			if r := f.call(t, route.method, route.path, c, body); r.status != 403 || r.code(t) != "local_only" {
				t.Fatalf("%s %s with %s: %d %s", route.method, route.path, who, r.status, r.body)
			}
		}
	}
	if accounts, _ := f.s.Accounts(context.Background(), store.Live{}); len(accounts) != 2 {
		t.Fatalf("%d accounts", len(accounts))
	}
	for _, path := range []string{"/v1/tasks", "/v1/projects", "/v1/experience", "/v1/attempts/a/events"} {
		if r := f.call(t, "GET", path, cookie, nil); r.status != 404 || r.code(t) != "not_found" {
			t.Fatalf("%s with a session: %d %s", path, r.status, r.body)
		}
		if r := f.call(t, "GET", path, "", nil); r.status != 401 {
			t.Fatalf("%s without a session: %d %s", path, r.status, r.body)
		}
	}
}

func TestTheOwnerChannelManagesAccounts(t *testing.T) {
	f := newNetFixture(t)
	id, _ := f.account(t, "Vera", false)
	if r := f.owner(t, "POST", "/v1/accounts", map[string]any{"name": "Vera"}); r.status != 409 || r.code(t) != "name_taken" {
		t.Fatalf("a taken name: %d %s", r.status, r.body)
	}
	for _, name := range []string{"", " Vera", strings.Repeat("v", 65), "tab\there"} {
		if r := f.owner(t, "POST", "/v1/accounts", map[string]any{"name": name}); r.status != 400 {
			t.Fatalf("name %q: %d %s", name, r.status, r.body)
		}
	}
	if r := f.owner(t, "PATCH", "/v1/accounts/local", map[string]any{"disabled": true}); r.status != 409 || r.code(t) != "owner_account" {
		t.Fatalf("disable the owner: %d %s", r.status, r.body)
	}
	if r := f.owner(t, "DELETE", "/v1/accounts/local", nil); r.status != 409 || r.code(t) != "owner_account" {
		t.Fatalf("remove the owner: %d %s", r.status, r.body)
	}
	if err := f.s.AddMember(context.Background(), "p", id, "member"); err != nil {
		t.Fatal(err)
	}
	if r := f.owner(t, "DELETE", "/v1/accounts/"+id, nil); r.status != 409 || r.code(t) != "account_in_use" {
		t.Fatalf("remove a member: %d %s", r.status, r.body)
	}
	if err := f.s.RemoveMember(context.Background(), "p", id); err != nil {
		t.Fatal(err)
	}
	cookie := f.mustEnroll(t, f.newCode(t, id), newSoftKey(t))
	if !f.signedIn(t, cookie) {
		t.Fatal("not signed in")
	}
	r := f.owner(t, "DELETE", "/v1/accounts/"+id, nil)
	if r.status != 204 || len(r.body) != 0 {
		t.Fatalf("remove: %d %q", r.status, r.body)
	}
	if f.signedIn(t, cookie) {
		t.Fatal("a removed account's session survived")
	}
	if r := f.owner(t, "DELETE", "/v1/accounts/"+id, nil); r.status != 404 {
		t.Fatalf("remove again: %d %s", r.status, r.body)
	}
	if r := f.owner(t, "POST", "/v1/accounts/nobody/enrollment-codes", nil); r.status != 404 {
		t.Fatalf("a code for nobody: %d %s", r.status, r.body)
	}
}

func TestACodeIsShownOnceEvenToAReplayOfItsRequest(t *testing.T) {
	f := newNetFixture(t)
	id, accountCode := f.account(t, "Vera", false)
	first := f.ownerWithKey(t, "POST", "/v1/accounts/"+id+"/enrollment-codes", "same-key", nil)
	if first.status != 201 || first.json(t)["code"] == nil || first.json(t)["expires"] == "" {
		t.Fatalf("first: %d %s", first.status, first.body)
	}
	replay := f.ownerWithKey(t, "POST", "/v1/accounts/"+id+"/enrollment-codes", "same-key", nil)
	if replay.status != 409 || replay.code(t) != "code_shown" {
		t.Fatalf("replay: %d %s", replay.status, replay.body)
	}
	created := f.ownerWithKey(t, "POST", "/v1/accounts", "create-key", map[string]any{"name": "Ida"})
	again := f.ownerWithKey(t, "POST", "/v1/accounts", "create-key", map[string]any{"name": "Ida"})
	if created.status != 201 || again.status != 409 || again.code(t) != "code_shown" {
		t.Fatalf("create: %d, replay %d %s", created.status, again.status, again.body)
	}
	recovered := f.ownerWithKey(t, "POST", "/v1/accounts/"+id+"/recover", "recover-key", nil)
	if replay := f.ownerWithKey(t, "POST", "/v1/accounts/"+id+"/recover", "recover-key", nil); recovered.status != 201 || replay.status != 409 || replay.code(t) != "code_shown" {
		t.Fatalf("recover: %d, replay %d %s", recovered.status, replay.status, replay.body)
	}
	// The idempotency cache never holds a code, in either form.
	accountNormalized, _ := normalizeCode(accountCode)
	forms := []string{accountCode, accountNormalized}
	for _, r := range []reply{first, created, recovered} {
		code := r.json(t)["code"].(string)
		normalized, _ := normalizeCode(code)
		forms = append(forms, code, normalized)
	}
	responses := f.column(t, "SELECT response FROM idempotency")
	if len(responses) < 3 {
		t.Fatalf("%d cached responses", len(responses))
	}
	for _, response := range responses {
		for _, form := range forms {
			if bytes.Contains(response, []byte(form)) {
				t.Fatalf("the cache holds code %s: %s", form, response)
			}
		}
	}
}

// column reads the first column of a query through a transaction of the owner's.
func (f *netFixture) column(t *testing.T, query string, args ...any) [][]byte {
	t.Helper()
	out, err := f.probe(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// probe reads the first column of a query in a transaction it rolls back; a row error is returned.
func (f *netFixture) probe(query string, args ...any) ([][]byte, error) {
	f.keys++
	var out [][]byte
	_, err := f.s.Do(context.Background(), "local", "probe-"+strings.Repeat("p", f.keys), "probe", nil, func(tx *store.Tx) ([]byte, error) {
		rows, err := tx.QueryContext(context.Background(), query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var v []byte
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, errProbe
	})
	if !errors.Is(err, errProbe) {
		if err == nil {
			err = errors.New("the probe committed")
		}
		return nil, err
	}
	return out, nil
}

// errProbe rolls a probe back, so it caches nothing.
var errProbe = errors.New("probe")

// exec runs trusted SQL as the owner, for a state no route makes.
func (f *netFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	f.keys++
	if _, err := f.s.Do(context.Background(), "local", "exec-"+strings.Repeat("e", f.keys), "exec", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(context.Background(), query, args...)
		return []byte("ok"), err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerChannelRoutesAreTheOwnersAlone(t *testing.T) {
	f := newNetFixture(t)
	bob := New(f.s, f.hub, func(*http.Request) (string, error) { return "bob", nil }, Execution{Owner: "local"})
	for _, route := range ownerChannelRoutes {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
		req.Header.Set("Idempotency-Key", "k")
		rec := httptest.NewRecorder()
		bob.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatalf("%s %s for another account: %d %s", route.method, route.path, rec.Code, rec.Body)
		}
	}
}

// pausedKey holds every hash until released, saying when one starts.
type pausedKey struct {
	entered chan argonParams
	release chan struct{}
}

func pauseHasher(f *netFixture) *pausedKey {
	p := &pausedKey{entered: make(chan argonParams, 16), release: make(chan struct{})}
	f.n.hasher.key = func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		p.entered <- argonParams{memory: memory, time: time, threads: threads}
		<-p.release
		return fastKey(password, salt, time, memory, threads, keyLen)
	}
	return p
}

func (p *pausedKey) wait(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no hash started")
	}
}

func TestAPasswordLoginIsRefusedWhenTheOwnerDisallowsPasswordsDuringItsHash(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", true)
	password := "the right password"
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": password}); r.status != 201 {
		t.Fatalf("enroll: %d %s", r.status, r.body)
	}
	before := len(mustSessions(t, f, id))
	paused := pauseHasher(f)
	done := make(chan reply, 1)
	go func() {
		done <- f.from(t, "192.0.2.50", "POST", "/v1/session/password", map[string]any{"account": "Vera", "password": password})
	}()
	paused.wait(t)
	if r := f.owner(t, "PATCH", "/v1/accounts/"+id, map[string]any{"password_allowed": false}); r.status != 200 {
		t.Fatalf("disallow: %d %s", r.status, r.body)
	}
	close(paused.release)
	if r := <-done; r.status != 401 || r.cookie != "" {
		t.Fatalf("a login verified across the change: %d %s", r.status, r.body)
	}
	if after := len(mustSessions(t, f, id)); after != before {
		t.Fatalf("sessions %d -> %d", before, after)
	}
}

func TestAPasswordAttemptQueuedForAHashSlotMeetsTheThrottleOfFailuresBeforeIt(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", true)
	password := "the right password"
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": password}); r.status != 201 {
		t.Fatalf("enroll: %d %s", r.status, r.body)
	}
	key := newSoftKey(t)
	f.mustEnroll(t, f.newCode(t, id), key)
	_, otherCode := f.account(t, "Other", true)
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": otherCode, "password": "another password"}); r.status != 201 {
		t.Fatalf("enroll: %d %s", r.status, r.body)
	}
	paused := pauseHasher(f)
	held := make(chan reply, 2)
	// Another account's checks hold both slots; Vera's right password waits for one.
	for _, address := range []string{"198.51.100.21", "198.51.100.22"} {
		go func() {
			held <- f.from(t, address, "POST", "/v1/session/password", map[string]any{"account": "Other", "password": "wrong password!!"})
		}()
	}
	paused.wait(t)
	paused.wait(t)
	queued := make(chan reply, 1)
	go func() {
		queued <- f.from(t, "198.51.100.23", "POST", "/v1/session/password", map[string]any{"account": "Vera", "password": password})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for f.n.hasher.queued.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.n.hasher.queued.Load() != 1 {
		t.Fatal("Vera's attempt never queued")
	}
	// While it waits, five failed passkey sign-ins of Vera's reach the threshold.
	bent := *key
	bent.noUV = true
	for range accountFreeFailures {
		if r := f.login(t, &bent, ""); r.status != 401 {
			t.Fatalf("a failing passkey: %d %s", r.status, r.body)
		}
	}
	close(paused.release)
	for range 2 {
		<-held
	}
	if r := <-queued; r.status != 429 || r.cookie != "" {
		t.Fatalf("the queued attempt ran after the threshold: %d %s", r.status, r.body)
	}
}

func TestARevokedStreamWaitingForBudgetEndsWithoutSendingAPage(t *testing.T) {
	f := newNetFixture(t)
	f.hub.BudgetBytes = pageBytes
	f.hub.PollInterval, f.hub.HeartbeatInterval, f.hub.WriteDeadline = time.Hour, time.Hour, time.Minute
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	stream := f.openStream(t, cookie)
	if !f.hub.reserveBytes(context.Background(), pageBytes) {
		t.Fatal("the budget was not free")
	}
	released := false
	defer func() {
		if !released {
			f.hub.releaseBytes(pageBytes)
		}
	}()
	if err := f.s.AppendEvent(context.Background(), "local", store.Event{Attempt: "a", Seq: 1, Time: "now", Kind: "text", Payload: json.RawMessage(`{"secret":"after revocation"}`)}); err != nil {
		t.Fatal(err)
	}
	f.hub.Notify("a")
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.hub.mu.Lock()
		waiting := len(f.hub.budgetQueue)
		f.hub.mu.Unlock()
		if waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream never waited for budget")
		}
		time.Sleep(time.Millisecond)
	}
	id := mustSessions(t, f, "local")[0].ID
	if r := f.owner(t, "DELETE", "/v1/sessions/"+id, nil); r.status != 204 {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	body := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(stream.Body)
		body <- data
	}()
	select {
	case data := <-body:
		if bytes.Contains(data, []byte("after revocation")) {
			t.Fatalf("a revoked stream sent %s", data)
		}
	case <-time.After(10 * time.Second):
		stream.Body.Close()
		t.Fatal("the revoked stream kept waiting for budget")
	}
	stream.Body.Close()
	f.hub.releaseBytes(pageBytes)
	released = true
}

// quietStreams opens a stream per cookie with no poll or heartbeat due, so only a deletion closes them.
func quietStreams(t *testing.T, f *netFixture, cookies ...string) []*http.Response {
	t.Helper()
	f.hub.PollInterval, f.hub.HeartbeatInterval = time.Hour, time.Hour
	streams := make([]*http.Response, len(cookies))
	for i, c := range cookies {
		streams[i] = f.openStream(t, c)
	}
	return streams
}

func endedEverywhere(t *testing.T, f *netFixture, cookies []string, streams []*http.Response) {
	t.Helper()
	for i := range cookies {
		streamEnds(t, streams[i])
		if f.signedIn(t, cookies[i]) {
			t.Fatalf("session %d survived", i)
		}
	}
}

func TestRecoveryEndsEverySessionOfTheAccountAndClosesTheirStreams(t *testing.T) {
	f := newNetFixture(t)
	key := newSoftKey(t)
	cookies := []string{f.mustEnroll(t, f.newCode(t, "local"), key), f.mustLogin(t, key)}
	streams := quietStreams(t, f, cookies...)
	if r := f.owner(t, "POST", "/v1/accounts/local/recover", nil); r.status != 201 {
		t.Fatalf("recover: %d %s", r.status, r.body)
	}
	endedEverywhere(t, f, cookies, streams)
}

func TestAReplacedPasswordEndsEverySessionOfTheAccountAndClosesTheirStreams(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", true)
	if err := f.s.AddMember(context.Background(), "p", id, "member"); err != nil {
		t.Fatal(err)
	}
	first := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": "the first password"})
	second := f.call(t, "POST", "/v1/session/password", "", map[string]any{"account": "Vera", "password": "the first password"})
	if first.status != 201 || second.status != 200 {
		t.Fatalf("sign in: %d, %d", first.status, second.status)
	}
	cookies := []string{first.cookie, second.cookie}
	streams := quietStreams(t, f, cookies...)
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": f.newCode(t, id), "password": "the second password"}); r.status != 201 {
		t.Fatalf("replace: %d %s", r.status, r.body)
	}
	endedEverywhere(t, f, cookies, streams)
	if r := f.call(t, "POST", "/v1/session/password", "", map[string]any{"account": "Vera", "password": "the first password"}); r.status != 401 {
		t.Fatalf("the replaced password: %d", r.status)
	}
}

// eventsRoute adds the events route to this fixture's network handler alone.
func (f *netFixture) eventsRoute() {
	f.events.Do(func() { f.n.session.HandleFunc("GET /v1/attempts/{id}/events", f.n.h.events) })
}

// hashClock is the hasher's clock in a test: hashes and waits move it, nothing else does.
type hashClock struct {
	mu      sync.Mutex
	now     time.Time
	advance func(time.Time) // called after each move, outside the lock
}

func (c *hashClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *hashClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now, hook := c.now, c.advance
	c.mu.Unlock()
	if hook != nil {
		hook(now)
	}
}

// timedHasher gives the hasher a fake clock that only its hashes and its padding move: a hash
// costs a microsecond per KiB, the current parameters 65.536 ms, the weak ones of the tests 1.024 ms.
func timedHasher(h *hasher) (*hashClock, *[]int) {
	clock := &hashClock{now: time.Unix(0, 0)}
	h.now = clock.Now
	h.key = func(password, salt []byte, passes, memory uint32, threads uint8, keyLen uint32) []byte {
		clock.Advance(time.Duration(memory) * time.Microsecond)
		return fastKey(password, salt, passes, memory, threads, keyLen)
	}
	var heldWhilePadding []int
	h.sleep = func(ctx context.Context, d time.Duration) error {
		heldWhilePadding = append(heldWhilePadding, len(h.sem))
		if err := ctx.Err(); err != nil {
			return err
		}
		clock.Advance(d)
		return nil
	}
	return clock, &heldWhilePadding
}

func TestEveryFailedPasswordCheckEndsOneCurrentHashAfterItBeganHoldingItsSlot(t *testing.T) {
	f := newColdNetFixture(t)
	h := f.n.hasher
	clock, padded := timedHasher(h)
	if err := h.calibrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	id, code := f.account(t, "Weak", true)
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": "the right password"}); r.status != 201 {
		t.Fatalf("enroll: %d %s", r.status, r.body)
	}
	pw, _ := f.s.PasswordCredential(context.Background(), id)
	weak := argonParams{memory: 1024, time: 1, threads: 1, saltLen: 8, keyLen: 32}
	salt := bytes.Repeat([]byte{5}, 8)
	f.exec(t, "UPDATE credentials SET password_hash=? WHERE id=?", encodePHC(weak, salt, fastKey([]byte("the right password"), salt, 0, 0, 0, 32)), pw.ID)
	_, currentCode := f.account(t, "Current", true)
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": currentCode, "password": "the right password"}); r.status != 201 {
		t.Fatalf("enroll: %d %s", r.status, r.body)
	}
	disabledID, disabledCode := f.account(t, "Disabled", true)
	f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": disabledCode, "password": "the right password"})
	f.owner(t, "PATCH", "/v1/accounts/"+disabledID, map[string]any{"disabled": true})
	f.account(t, "NoPassword", true)
	cost := time.Duration(currentArgon.memory) * time.Microsecond
	for i, name := range []string{"Weak", "Current", "nobody", "Disabled", "NoPassword"} {
		began := clock.Now()
		r := f.from(t, "198.51.100."+strings.Repeat("7", i+1), "POST", "/v1/session/password", map[string]any{"account": name, "password": "a wrong password"})
		if r.status != 401 {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
		if took := clock.Now().Sub(began); took != cost {
			t.Fatalf("%s: a failure took %s, one current hash %s", name, took, cost)
		}
	}
	// The check keeps its slot to its deadline, so the stored hash's cost shows in nothing.
	if len(*padded) != 1 || (*padded)[0] != 1 {
		t.Fatalf("padding ran %d times, holding %v slots", len(*padded), *padded)
	}
}

func TestAPasswordEnrollmentQueuedForAHashSlotMeetsAnAddressBlockStartedDuringItsWait(t *testing.T) {
	f := newNetFixture(t)
	_, code := f.account(t, "Vera", true)
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": "the right password"}); r.status != 201 {
		t.Fatalf("enroll: %d %s", r.status, r.body)
	}
	_, queuedCode := f.account(t, "Ida", true)
	const address = "192.0.2.77"
	paused := pauseHasher(f)
	held := make(chan reply, 2)
	// Two checks from elsewhere hold both slots; the enrollment from the address waits for one.
	for _, other := range []string{"192.0.2.78", "192.0.2.79"} {
		go func() {
			held <- f.from(t, other, "POST", "/v1/session/password", map[string]any{"account": "Vera", "password": "wrong password!!"})
		}()
	}
	paused.wait(t)
	paused.wait(t)
	enrolled := make(chan reply, 1)
	go func() {
		enrolled <- f.from(t, address, "POST", "/v1/enrollment/password", map[string]any{"code": queuedCode, "password": "a password for Ida"})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for f.n.hasher.queued.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.n.hasher.queued.Load() != 1 {
		t.Fatal("the enrollment never queued")
	}
	// The address is blocked while the enrollment waits. New attempts from it cannot do that while
	// the enrollment's own reservation is counted, so the failures are recorded straight away, as
	// checks admitted earlier would record them.
	for range addressFailures {
		f.hub.limits.fail("", address, f.clock.Now())
	}
	close(paused.release)
	for range 2 {
		if r := <-held; r.status != 401 {
			t.Fatalf("a held login: %d %s", r.status, r.body)
		}
	}
	if r := <-enrolled; r.status != 429 || r.cookie != "" {
		t.Fatalf("the queued enrollment: %d %s", r.status, r.body)
	}
	select {
	case <-paused.entered:
		t.Fatal("the refused enrollment hashed")
	default:
	}
	normalized, _ := normalizeCode(queuedCode)
	if _, err := f.s.CheckEnrollmentCode(context.Background(), codeHash(normalized), f.clock.Now()); err != nil {
		t.Fatalf("the refused enrollment used its code: %v", err)
	}
}

func TestAStreamRevokedWhileItStartsEndsWithNothingSent(t *testing.T) {
	f := newNetFixture(t)
	f.hub.BudgetBytes = pageBytes
	f.hub.PollInterval, f.hub.HeartbeatInterval, f.hub.WriteDeadline = time.Hour, time.Hour, time.Minute
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	f.eventsRoute()
	if err := f.s.AppendEvent(context.Background(), "local", store.Event{Attempt: "a", Seq: 1, Time: "now", Kind: "text", Payload: json.RawMessage(`{"secret":"after revocation"}`)}); err != nil {
		t.Fatal(err)
	}
	if !f.hub.reserveBytes(context.Background(), pageBytes) {
		t.Fatal("the budget was not free")
	}
	defer f.hub.releaseBytes(pageBytes)
	result := make(chan reply, 1)
	go func() {
		req, _ := http.NewRequest("GET", f.server.URL+"/v1/attempts/a/events?after=5", nil)
		req.Header.Set("Accept", "text/event-stream")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		resp, err := f.server.Client().Do(req)
		if err != nil {
			result <- reply{status: -1, body: []byte(err.Error())}
			return
		}
		result <- readReply(t, resp)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.hub.mu.Lock()
		waiting := len(f.hub.budgetQueue)
		f.hub.mu.Unlock()
		if waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream never waited for budget")
		}
		time.Sleep(time.Millisecond)
	}
	id := mustSessions(t, f, "local")[0].ID
	if r := f.owner(t, "DELETE", "/v1/sessions/"+id, nil); r.status != 204 {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	select {
	case r := <-result:
		if r.status != 401 || bytes.Contains(r.body, []byte("last_sequence")) || bytes.Contains(r.body, []byte("after revocation")) {
			t.Fatalf("a stream revoked at start: %d %s", r.status, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked stream kept waiting for budget")
	}
	if n := f.hub.Sessions.Open(id); n != 0 {
		t.Fatalf("%d streams still registered", n)
	}
}

func TestASessionAnotherRequestRefreshedStaysSignedIn(t *testing.T) {
	f := newNetFixture(t)
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	stale := mustSessions(t, f, "local")[0]
	f.clock.Advance(sessionIdle - time.Minute)
	if !f.signedIn(t, cookie) {
		t.Fatal("expired early")
	}
	// The stale copy is past its idle expiry; the row the other request wrote is not.
	f.clock.Advance(2 * time.Minute)
	sess, ok, err := f.n.live(context.Background(), stale, f.clock.Now())
	if err != nil || !ok || sess.LastUsed == stale.LastUsed {
		t.Fatalf("judged by the stale copy: %v, %+v, %v", ok, sess, err)
	}
	if !f.signedIn(t, cookie) {
		t.Fatal("the refreshed session was deleted")
	}
}

// staleAndFresh gives the owner two sessions: one past its idle expiry, still stored, and one live.
func staleAndFresh(t *testing.T) (*netFixture, string) {
	t.Helper()
	f := newNetFixture(t)
	key := newSoftKey(t)
	f.mustEnroll(t, f.newCode(t, "local"), key)
	fresh := f.mustLogin(t, key)
	f.clock.Advance(sessionIdle - time.Hour)
	if !f.signedIn(t, fresh) {
		t.Fatal("fresh expired")
	}
	f.clock.Advance(2 * time.Hour)
	if n := len(mustSessions(t, f, "")); n != 2 {
		t.Fatalf("%d sessions stored", n)
	}
	return f, fresh
}

func TestListsAndCountsShowOnlyLiveSessions(t *testing.T) {
	for name, count := range map[string]func(*testing.T, *netFixture, string) int{
		"the owner's session list": func(t *testing.T, f *netFixture, _ string) int {
			var v []map[string]any
			_ = json.Unmarshal(f.owner(t, "GET", "/v1/sessions", nil).body, &v)
			return len(v)
		},
		"the user's own session list": func(t *testing.T, f *netFixture, fresh string) int {
			var v []map[string]any
			_ = json.Unmarshal(f.call(t, "GET", "/v1/account/sessions", fresh, nil).body, &v)
			return len(v)
		},
		"the account list's count": func(t *testing.T, f *netFixture, _ string) int {
			var v []map[string]any
			_ = json.Unmarshal(f.owner(t, "GET", "/v1/accounts", nil).body, &v)
			return int(v[0]["sessions"].(float64))
		},
		"an account change's answer": func(t *testing.T, f *netFixture, _ string) int {
			r := f.owner(t, "PATCH", "/v1/accounts/local", map[string]any{"password_allowed": true})
			return int(r.json(t)["sessions"].(float64))
		},
	} {
		f, fresh := staleAndFresh(t)
		if got := count(t, f, fresh); got != 1 {
			t.Fatalf("%s shows %d sessions", name, got)
		}
	}
}

func TestTheCacheProbeReturnsAnErrorMetMidScan(t *testing.T) {
	f := newNetFixture(t)
	// The second row overflows an integer: the scan reads one row, then fails.
	_, err := f.probe("SELECT CASE WHEN column1 = 1 THEN x'61' ELSE abs(-9223372036854775807 - 1) END FROM (VALUES (1), (2))")
	if err == nil || errors.Is(err, errProbe) {
		t.Fatalf("a failing scan passed: %v", err)
	}
	if rows, err := f.probe("SELECT x'61'"); err != nil || len(rows) != 1 {
		t.Fatalf("a clean scan: %q, %v", rows, err)
	}
}

func TestAStreamRevokedDuringItsMembershipLookupAnswersUnauthenticated(t *testing.T) {
	f := newNetFixture(t)
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	f.eventsRoute()
	entered := make(chan struct{})
	f.n.h.attemptProject = func(ctx context.Context, _, _ string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", store.ErrNotFound
	}
	result := f.streamOnce(t, "/v1/attempts/a/events", cookie)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the lookup never ran")
	}
	id := mustSessions(t, f, "local")[0].ID
	if r := f.owner(t, "DELETE", "/v1/sessions/"+id, nil); r.status != 204 {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	select {
	case r := <-result:
		if r.status != 401 || r.code(t) != "unauthenticated" {
			t.Fatalf("revoked during the lookup: %d %s", r.status, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lookup outlived the revocation")
	}
}

func TestListsCountsAndAuthenticationAgreeInsideTheCutoffsMillisecond(t *testing.T) {
	// Last use 400 or 600 microseconds into a millisecond, the cutoff 200 microseconds before it.
	for _, into := range []time.Duration{400 * time.Microsecond, 600 * time.Microsecond} {
		f := newNetFixture(t)
		f.clock.mu.Lock()
		f.clock.now = time.Now().UTC().Truncate(time.Millisecond).Add(into)
		f.clock.mu.Unlock()
		cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
		f.clock.Advance(sessionIdle - 200*time.Microsecond)
		var sessions []map[string]any
		if err := json.Unmarshal(f.owner(t, "GET", "/v1/sessions", nil).body, &sessions); err != nil {
			t.Fatal(err)
		}
		var accounts []map[string]any
		if err := json.Unmarshal(f.owner(t, "GET", "/v1/accounts", nil).body, &accounts); err != nil {
			t.Fatal(err)
		}
		listed, counted, signedIn := len(sessions) == 1, accounts[0]["sessions"] == 1.0, f.signedIn(t, cookie)
		if listed != signedIn || counted != signedIn {
			t.Fatalf("%s in: listed %v, counted %v, signed in %v", into, listed, counted, signedIn)
		}
	}
}

func TestTheFirstLoginsAfterAStartCostTheSameWhateverTheyMeet(t *testing.T) {
	cost := time.Duration(currentArgon.memory) * time.Microsecond
	for name, setup := range map[string]func(*testing.T, *netFixture) string{
		"a stored hash below the current parameters": func(t *testing.T, f *netFixture) string {
			id, code := f.account(t, "Weak", true)
			weak := argonParams{memory: 1024, time: 1, threads: 1, saltLen: 8, keyLen: 32}
			salt := bytes.Repeat([]byte{5}, 8)
			normalized, _ := normalizeCode(code)
			credential := store.Credential{ID: "pw", User: id, Kind: store.Password, Name: "Password", PasswordHash: encodePHC(weak, salt, fastKey([]byte("the right password"), salt, 0, 0, 0, 32))}
			if _, err := f.s.EnrollPassword(context.Background(), codeHash(normalized), f.clock.Now, credential, store.Session{ID: "s", TokenHash: bytes.Repeat([]byte{1}, 32), User: id, Credential: "pw", Address: "192.0.2.1"}); err != nil {
				t.Fatal(err)
			}
			return "Weak"
		},
		"an unknown account": func(*testing.T, *netFixture) string { return "nobody" },
	} {
		f := newColdNetFixture(t)
		clock, _ := timedHasher(f.n.hasher)
		account := setup(t, f)
		began := clock.Now()
		if r := f.from(t, "192.0.2.9", "POST", "/v1/session/password", map[string]any{"account": account, "password": "a wrong password"}); r.status != 401 {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
		if took := clock.Now().Sub(began); took != 2*cost {
			t.Fatalf("%s: the first failure took %s, want a calibration and one hash, %s", name, took, 2*cost)
		}
	}
}

func TestConcurrentFirstLoginsCalibrateOnce(t *testing.T) {
	f := newColdNetFixture(t)
	var calibrations atomic.Int32
	f.n.hasher.key = func(password, salt []byte, passes, memory uint32, threads uint8, keyLen uint32) []byte {
		if string(password) == calibrationPassword {
			calibrations.Add(1)
		}
		return fastKey(password, salt, passes, memory, threads, keyLen)
	}
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			f.from(t, "192.0.2."+strings.Repeat("3", i+1), "POST", "/v1/session/password", map[string]any{"account": "nobody", "password": "a wrong password"})
		})
	}
	wg.Wait()
	if n := calibrations.Load(); n != 1 {
		t.Fatalf("%d calibrations", n)
	}
}

func TestPaddingEndsWithItsRequestAndTheFailureIsStillRecorded(t *testing.T) {
	h := newHasher()
	h.cost = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	done := make(chan error, 1)
	go func() { done <- h.pad(ctx, h.deadline()) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("padding ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the padding outlived its request")
	}

	f := newNetFixture(t)
	id, code := f.account(t, "Vera", true)
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": "the right password"}); r.status != 201 {
		t.Fatalf("enroll: %d %s", r.status, r.body)
	}
	f.n.hasher.cost = time.Hour
	request, stop := context.WithCancel(context.Background())
	f.n.hasher.sleep = func(ctx context.Context, d time.Duration) error {
		stop() // the client goes away during the padding, which still runs to its deadline
		return ctx.Err()
	}
	req := httptest.NewRequest("POST", "/v1/session/password", bytes.NewReader(marshal(t, map[string]any{"account": "Vera", "password": "a wrong password"}))).WithContext(request)
	req.RemoteAddr = "192.0.2.60:40000"
	withRequestID(f.n).ServeHTTP(httptest.NewRecorder(), req)
	f.hub.limits.mu.Lock()
	account, _ := f.hub.limits.accounts.get(id)
	address, _ := f.hub.limits.addresses.get("192.0.2.60")
	f.hub.limits.mu.Unlock()
	if account == nil || account.failures != 1 || account.inFlight != 0 || address == nil || len(address.failures) != 1 || address.inFlight != 0 || len(address.starts) != 0 {
		t.Fatalf("after a cancelled padding: account %+v, address %+v", account, address)
	}
}

func TestOverlappingAttemptsAtTheThresholdMeetThe429OnTheSameScheduleWhateverTheStoredHash(t *testing.T) {
	schedule := map[string][]string{}
	for _, weak := range []bool{true, false} {
		f := newColdNetFixture(t)
		clock, _ := timedHasher(f.n.hasher)
		f.n.clock = clock.Now // the throttle runs on the hashes' clock here
		if err := f.n.hasher.calibrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, code := f.account(t, "Vera", true)
		if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": "the right password"}); r.status != 201 {
			t.Fatalf("enroll: %d %s", r.status, r.body)
		}
		if weak {
			params := argonParams{memory: 1024, time: 1, threads: 1, saltLen: 16, keyLen: 32}
			salt := bytes.Repeat([]byte{9}, 16)
			f.exec(t, "UPDATE credentials SET password_hash=? WHERE kind='password'", encodePHC(params, salt, fastKey([]byte("the right password"), salt, 0, 0, 0, 32)))
		}
		attempt := func(address string) reply {
			return f.from(t, address, "POST", "/v1/session/password", map[string]any{"account": "Vera", "password": "a wrong password"})
		}
		for i := range accountFreeFailures - 1 {
			if r := attempt("203.0.113." + strings.Repeat("1", i+1)); r.status != 401 {
				t.Fatalf("failure %d: %d", i, r.status)
			}
		}
		var seen []string
		note := func(r reply) { seen = append(seen, strconv.Itoa(r.status)+" "+r.header.Get("Retry-After")) }
		// While the fifth failure is checked, 30 ms in, another attempt arrives.
		began := clock.Now()
		fired := false
		clock.mu.Lock()
		clock.advance = func(now time.Time) {
			if !fired && !now.Before(began.Add(30*time.Millisecond)) {
				fired = true
				note(attempt("203.0.113.30"))
			}
		}
		clock.mu.Unlock()
		note(attempt("203.0.113.20"))
		if !fired {
			t.Fatal("no attempt overlapped")
		}
		// The delay runs a second from the failure's end, a current hash's time after it began.
		end := began.Add(time.Duration(currentArgon.memory) * time.Microsecond)
		clock.Advance(end.Add(accountFirstDelay - time.Millisecond).Sub(clock.Now()))
		note(attempt("203.0.113.31"))
		clock.Advance(time.Millisecond)
		note(attempt("203.0.113.32"))
		schedule[strconv.FormatBool(weak)] = seen
	}
	want := []string{"429 1", "401 ", "429 1", "401 "}
	for kind, seen := range schedule {
		if strings.Join(seen, ",") != strings.Join(want, ",") {
			t.Fatalf("weak stored hash %s: %v, want %v (other: %v)", kind, seen, want, schedule)
		}
	}
}

// pauseAt holds the first request that reaches the named point until released.
func pauseAt(f *netFixture, point string) (entered <-chan struct{}, release func()) {
	in, out := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.n.pause = func(_ context.Context, at string) {
		if at != point {
			return
		}
		first := false
		once.Do(func() { first = true })
		if first {
			close(in)
			<-out
		}
	}
	var closing sync.Once
	return in, func() { closing.Do(func() { close(out) }) }
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

// legacyHash stores a password hash below the current parameters for every password of the fixture.
func legacyHash(t *testing.T, f *netFixture, password string) {
	t.Helper()
	params := argonParams{memory: 1024, time: 1, threads: 1, saltLen: 16, keyLen: 32}
	salt := bytes.Repeat([]byte{9}, 16)
	f.exec(t, "UPDATE credentials SET password_hash=? WHERE kind='password'", encodePHC(params, salt, fastKey([]byte(password), salt, 0, 0, 0, 32)))
}

func TestACancelledCheckAtTheThresholdCountsAtItsDeadlineWhateverTheStoredHash(t *testing.T) {
	schedules := map[bool][]string{}
	for _, weak := range []bool{true, false} {
		f := newColdNetFixture(t)
		clock, _ := timedHasher(f.n.hasher)
		f.n.clock = clock.Now
		h := f.n.hasher
		if err := h.calibrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, code := f.account(t, "Vera", true)
		if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": "the right password"}); r.status != 201 {
			t.Fatalf("enroll: %d %s", r.status, r.body)
		}
		if weak {
			legacyHash(t, f, "the right password")
		}
		attempt := func(address string) reply {
			return f.from(t, address, "POST", "/v1/session/password", map[string]any{"account": "Vera", "password": "a wrong password"})
		}
		for i := range accountFreeFailures - 1 {
			attempt("203.0.113." + strings.Repeat("1", i+1))
		}
		// The fifth check's client goes away while it is checked.
		request, stop := context.WithCancel(context.Background())
		timed := h.key
		h.key = func(password, salt []byte, passes, memory uint32, threads uint8, keyLen uint32) []byte {
			stop()
			return timed(password, salt, passes, memory, threads, keyLen)
		}
		began := clock.Now()
		req := httptest.NewRequest("POST", "/v1/session/password", bytes.NewReader(marshal(t, map[string]any{"account": "Vera", "password": "a wrong password"}))).WithContext(request)
		req.RemoteAddr = "203.0.113.20:40000"
		withRequestID(f.n).ServeHTTP(httptest.NewRecorder(), req)
		h.key = timed
		end := began.Add(time.Duration(currentArgon.memory) * time.Microsecond)
		if clock.Now() != end {
			t.Fatalf("the cancelled check ended %s after it began", clock.Now().Sub(began))
		}
		clock.Advance(accountFirstDelay - time.Millisecond)
		r := attempt("203.0.113.31")
		clock.Advance(time.Millisecond)
		after := attempt("203.0.113.32")
		schedules[weak] = []string{strconv.Itoa(r.status) + " " + r.header.Get("Retry-After"), strconv.Itoa(after.status)}
	}
	want := []string{"429 1", "401"}
	for weak, got := range schedules {
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("weak %v: %v, want %v (all %v)", weak, got, want, schedules)
		}
	}
}

func TestOverlappingPasskeyAssertionsAtTheAccountThresholdRunOneAtATime(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", false)
	key := newSoftKey(t)
	f.mustEnroll(t, code, key)
	bent := *key
	bent.noUV = true
	for range accountFreeFailures - 1 {
		if r := f.login(t, &bent, ""); r.status != 401 {
			t.Fatalf("a failing assertion: %d %s", r.status, r.body)
		}
	}
	entered, release := pauseAt(f, "passkey")
	first := make(chan reply, 1)
	go func() { first <- f.login(t, &bent, "") }()
	waitFor(t, entered, "the first assertion's check")
	second := f.login(t, &bent, "")
	release()
	if r := <-first; r.status != 401 {
		t.Fatalf("the first: %d %s", r.status, r.body)
	}
	if second.status != 429 {
		t.Fatalf("the overlapping assertion of %s ran: %d %s", id, second.status, second.body)
	}
}

func TestOverlappingCodeChecksAtTheAddressThresholdRunOneAtATime(t *testing.T) {
	f := newNetFixture(t)
	const address = "192.0.2.90"
	bad := map[string]any{"code": "00000-00000-00000-00000"}
	for range addressFailures - 1 {
		if r := f.from(t, address, "POST", "/v1/enrollment/options", bad); r.status != 400 {
			t.Fatalf("a bad code: %d", r.status)
		}
	}
	entered, release := pauseAt(f, "code")
	first := make(chan reply, 1)
	go func() { first <- f.from(t, address, "POST", "/v1/enrollment/options", bad) }()
	waitFor(t, entered, "the first code's lookup")
	second := f.from(t, address, "POST", "/v1/enrollment/options", bad)
	release()
	if r := <-first; r.status != 400 {
		t.Fatalf("the first: %d %s", r.status, r.body)
	}
	if second.status != 429 {
		t.Fatalf("the overlapping lookup ran: %d %s", second.status, second.body)
	}
}

func TestARemovalForASessionEndedWhileItWaitedChangesNothing(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", true)
	key := newSoftKey(t)
	cookie := f.mustEnroll(t, code, key)
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": f.newCode(t, id), "password": "the first password"}); r.status != 201 {
		t.Fatalf("password: %d %s", r.status, r.body)
	}
	cookie = f.mustLogin(t, key)
	passkey, err := f.s.PasskeyByCredentialID(context.Background(), key.id)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := pauseAt(f, "remove-credential")
	removed := make(chan reply, 1)
	go func() { removed <- f.call(t, "DELETE", "/v1/account/credentials/"+passkey.ID, cookie, nil) }()
	waitFor(t, entered, "the removal")
	replaced := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": f.newCode(t, id), "password": "the second password"})
	if replaced.status != 201 {
		t.Fatalf("replace: %d %s", replaced.status, replaced.body)
	}
	release()
	if r := <-removed; r.status != 401 {
		t.Fatalf("a removal for an ended session: %d %s", r.status, r.body)
	}
	if _, err := f.s.PasskeyByCredentialID(context.Background(), key.id); err != nil {
		t.Fatalf("the passkey: %v", err)
	}
	if !f.signedIn(t, replaced.cookie) {
		t.Fatal("the replacement's session ended")
	}

	// The same for revoking another of one's own sessions.
	other := f.mustLogin(t, key)
	sessions := mustSessions(t, f, id)
	var otherID string
	for _, s := range sessions {
		if hash, _ := tokenHash(other); bytes.Equal(s.TokenHash, hash) {
			otherID = s.ID
		}
	}
	entered, release = pauseAt(f, "revoke-session")
	revoked := make(chan reply, 1)
	go func() { revoked <- f.call(t, "DELETE", "/v1/account/sessions/"+otherID, replaced.cookie, nil) }()
	waitFor(t, entered, "the revocation")
	if r := f.call(t, "POST", "/v1/session/logout", replaced.cookie, nil); r.status != 204 {
		t.Fatalf("logout: %d", r.status)
	}
	release()
	if r := <-revoked; r.status != 401 {
		t.Fatalf("a revocation for an ended session: %d %s", r.status, r.body)
	}
	if !f.signedIn(t, other) {
		t.Fatal("the other session was revoked")
	}
}

// stuckWriter is a response whose writes, once blocked, return only when a deadline at or before
// now is set on it, as a connection's do.
type stuckWriter struct {
	header  http.Header
	mu      sync.Mutex
	block   bool
	body    bytes.Buffer
	inside  chan struct{}
	freed   chan struct{}
	inOnce  sync.Once
	outOnce sync.Once
}

func (w *stuckWriter) Header() http.Header { return w.header }
func (w *stuckWriter) WriteHeader(int)     {}
func (w *stuckWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	block := w.block
	if !block {
		w.body.Write(p)
	}
	w.mu.Unlock()
	if !block {
		return len(p), nil
	}
	w.inOnce.Do(func() { close(w.inside) })
	<-w.freed
	return 0, os.ErrDeadlineExceeded
}
func (w *stuckWriter) FlushError() error { return nil }
func (w *stuckWriter) SetWriteDeadline(t time.Time) error {
	if !t.IsZero() && !t.After(time.Now()) {
		w.outOnce.Do(func() { close(w.freed) })
	}
	return nil
}
func (w *stuckWriter) started() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Contains(w.body.String(), "retry:")
}

func TestARevocationInterruptsAStreamBlockedInAWriteAndFreesItsBudgetAndSlot(t *testing.T) {
	f := newNetFixture(t)
	f.hub.PollInterval, f.hub.HeartbeatInterval = time.Hour, time.Hour
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	f.eventsRoute()
	w := &stuckWriter{header: http.Header{}, inside: make(chan struct{}), freed: make(chan struct{})}
	req := httptest.NewRequest("GET", "/v1/attempts/a/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	done := make(chan struct{})
	go func() {
		withRequestID(f.n).ServeHTTP(w, req)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !w.started() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	w.mu.Lock()
	w.block = true
	w.mu.Unlock()
	if err := f.s.AppendEvent(context.Background(), "local", store.Event{Attempt: "a", Seq: 1, Time: "now", Kind: "text", Payload: json.RawMessage(`{"x":1}`)}); err != nil {
		t.Fatal(err)
	}
	f.hub.Notify("a")
	waitFor(t, w.inside, "a blocked write")
	id := mustSessions(t, f, "local")[0].ID
	if r := f.owner(t, "DELETE", "/v1/sessions/"+id, nil); r.status != 204 {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	waitFor(t, done, "the stream's end")
	f.hub.mu.Lock()
	used, streams := f.hub.budgetUsed, f.hub.streamTotal
	f.hub.mu.Unlock()
	if used != 0 || streams != 0 {
		t.Fatalf("after the stream: %d bytes of budget and %d stream slots held", used, streams)
	}
}

func TestAStoreFailureDuringAuthenticationIsAnInternalErrorNotASignIn(t *testing.T) {
	f := newNetFixture(t)
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	if r := f.call(t, "GET", "/v1/account", "not-a-token", nil); r.status != 401 {
		t.Fatalf("a malformed cookie: %d", r.status)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	r := f.call(t, "GET", "/v1/account", cookie, nil)
	if r.status != 500 || r.code(t) != "internal" {
		t.Fatalf("a store failure: %d %s", r.status, r.body)
	}
}

func TestNewNetworkNeedsTheOwnerAndReadsItWithoutAnExecution(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateUser(ctx, "local", "Local"); err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	t.Cleanup(hub.Close)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("a network channel without an owner was made")
			}
		}()
		NewNetwork(s, hub, NetworkConfig{RPID: testRPID, Origin: testOrigin})
	}()
	server := httptest.NewTLSServer(NewNetwork(s, hub, NetworkConfig{RPID: testRPID, Origin: testOrigin, Owner: "local"}))
	t.Cleanup(server.Close)
	f := &netFixture{s: s, hub: hub, server: server, local: New(s, hub, func(*http.Request) (string, error) { return "local", nil }, Execution{Owner: "local"})}
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	if account := f.call(t, "GET", "/v1/account", cookie, nil).json(t); account["id"] != "local" || account["owner"] != true {
		t.Fatalf("the owner reads %v", account)
	}
}

// streamOnce opens an events request on a fresh connection, which a failed answer cannot retry.
func (f *netFixture) streamOnce(t *testing.T, path, cookie string) <-chan reply {
	t.Helper()
	transport := f.server.Client().Transport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	client := &http.Client{Transport: transport}
	result := make(chan reply, 1)
	go func() {
		defer transport.CloseIdleConnections()
		req, _ := http.NewRequest("GET", f.server.URL+path, nil)
		req.Header.Set("Accept", "text/event-stream")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		resp, err := client.Do(req)
		if err != nil {
			result <- reply{status: -1, body: []byte(err.Error())}
			return
		}
		result <- readReply(t, resp)
	}()
	return result
}

func revokeOwner(t *testing.T, f *netFixture) {
	t.Helper()
	id := mustSessions(t, f, "local")[0].ID
	if r := f.owner(t, "DELETE", "/v1/sessions/"+id, nil); r.status != 204 {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
}

func TestARevocationRacingTheCursorAheadAnswerSendsNothingAfterIt(t *testing.T) {
	f := newNetFixture(t)
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	f.eventsRoute()
	entered, release := pauseAt(f, "cursor-ahead")
	result := f.streamOnce(t, "/v1/attempts/a/events?after=5", cookie)
	waitFor(t, entered, "the cursor-ahead answer")
	revokeOwner(t, f)
	release()
	select {
	case r := <-result:
		if r.status != 401 || r.code(t) != "unauthenticated" || bytes.Contains(r.body, []byte("last_sequence")) {
			t.Fatalf("after the revocation: %d %s", r.status, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no answer")
	}
}

func TestAStoreFailureAtTheRecheckIsAnInternalError(t *testing.T) {
	f := newNetFixture(t)
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	f.eventsRoute()
	// Authentication passed; the store fails before the session is read again.
	f.n.h.attemptProject = func(ctx context.Context, _, _ string) (string, error) {
		f.s.Close()
		return "p", nil
	}
	select {
	case r := <-f.streamOnce(t, "/v1/attempts/a/events", cookie):
		if r.status != 500 || r.code(t) != "internal" {
			t.Fatalf("a store failure at the recheck: %d %s", r.status, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no answer")
	}
}

func TestAChangeWaitingBehindAnotherWriterIsJudgedAtItsTransaction(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Vera", true)
	key := newSoftKey(t)
	f.mustEnroll(t, code, key)
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": f.newCode(t, id), "password": "a password to keep"}); r.status != 201 {
		t.Fatalf("password: %d", r.status)
	}
	cookie, other := f.mustLogin(t, key), f.mustLogin(t, key)
	passkey, _ := f.s.PasskeyByCredentialID(context.Background(), key.id)
	var otherID string
	hash, _ := tokenHash(other)
	for _, s := range mustSessions(t, f, id) {
		if bytes.Equal(s.TokenHash, hash) {
			otherID = s.ID
		}
	}
	// Both sessions are a minute and a half short of seven days, used half a minute ago, so
	// authenticating them writes nothing.
	f.clock.Advance(sessionAbsolute - 2*time.Minute)
	f.exec(t, "UPDATE sessions SET last_used=? WHERE user=?", store.SessionStamp(f.clock.Now()), id)
	f.clock.Advance(30 * time.Second)
	held, free := make(chan struct{}), make(chan struct{})
	go f.s.Do(context.Background(), "local", "hold-the-writer", "hold", nil, func(*store.Tx) ([]byte, error) {
		close(held)
		<-free
		return nil, errProbe
	})
	waitFor(t, held, "the other writer")
	queued := queuedWrites(f, 2)
	removed := make(chan reply, 1)
	revoked := make(chan reply, 1)
	go func() { removed <- f.call(t, "DELETE", "/v1/account/credentials/"+passkey.ID, cookie, nil) }()
	go func() { revoked <- f.call(t, "DELETE", "/v1/account/sessions/"+otherID, cookie, nil) }()
	waitFor(t, queued, "both changes reaching the write lock")
	f.clock.Advance(2 * time.Minute)
	close(free)
	for name, ch := range map[string]chan reply{"removal": removed, "revocation": revoked} {
		if r := <-ch; r.status != 401 {
			t.Fatalf("the %s past absolute expiry: %d %s", name, r.status, r.body)
		}
	}
	if _, err := f.s.PasskeyByCredentialID(context.Background(), key.id); err != nil {
		t.Fatalf("the passkey: %v", err)
	}
	if _, err := f.s.Session(context.Background(), otherID); err != nil {
		t.Fatalf("the other session: %v", err)
	}
}

func TestASuccessKeepsTheReservationsOfChecksStillRunning(t *testing.T) {
	f := newNetFixture(t)
	_, code := f.account(t, "Vera", false)
	key := newSoftKey(t)
	f.mustEnroll(t, code, key)
	bent := *key
	bent.noUV = true
	entered, release := pauseAt(f, "passkey")
	defer release()
	paused := make(chan reply, 1)
	go func() { paused <- f.login(t, &bent, "") }()
	waitFor(t, entered, "the paused assertion")
	f.mustLogin(t, key) // clears the account's failures, not the paused check
	for range accountFreeFailures - 1 {
		if r := f.login(t, &bent, ""); r.status != 401 {
			t.Fatalf("a failing assertion: %d %s", r.status, r.body)
		}
	}
	// Four failures and the paused check reach the threshold.
	if r := f.login(t, &bent, ""); r.status != 429 {
		t.Fatalf("at the threshold with a check still running: %d %s", r.status, r.body)
	}
	release()
	<-paused
}

func TestACodeReplacedBeforeItsCommitCountsOnceUnderItsReservation(t *testing.T) {
	f := newNetFixture(t)
	id, code := f.account(t, "Ida", true)
	const address = "192.0.2.95"
	bad := map[string]any{"code": "00000-00000-00000-00000"}
	for range addressFailures - 2 {
		f.from(t, address, "POST", "/v1/enrollment/options", bad)
	}
	var waiting sync.WaitGroup
	waiting.Add(2)
	release := make(chan struct{})
	f.n.pause = func(_ context.Context, at string) {
		if at == "enroll-commit" {
			waiting.Done()
			<-release
		}
	}
	results := make(chan reply, 2)
	for range 2 {
		go func() {
			results <- f.from(t, address, "POST", "/v1/enrollment/password", map[string]any{"code": code, "password": "a password for Ida"})
		}()
	}
	waiting.Wait()
	// Eighteen failures and two enrollments still running: the next attempt waits.
	if r := f.from(t, address, "POST", "/v1/enrollment/options", bad); r.status != 429 {
		t.Fatalf("over the threshold: %d %s", r.status, r.body)
	}
	f.newCode(t, id) // replaces the code both enrollments read
	close(release)
	for range 2 {
		if r := <-results; r.status != 400 || r.code(t) != "code_invalid" {
			t.Fatalf("an enrollment with a replaced code: %d %s", r.status, r.body)
		}
	}
	f.hub.limits.mu.Lock()
	a, _ := f.hub.limits.addresses.get(address)
	f.hub.limits.mu.Unlock()
	// Exactly twenty failures, the block on, nothing still in flight.
	if a == nil || a.until.IsZero() || len(a.failures) != addressFailures || a.inFlight != 0 || len(a.starts) != 0 {
		t.Fatalf("the address after the commits: %+v", a)
	}
}

func TestTheCostIsFrozenAtCalibration(t *testing.T) {
	h := newHasher()
	clock, _ := timedHasher(h)
	if err := h.calibrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cost := time.Duration(currentArgon.memory) * time.Microsecond
	admitted, deadline := clock.Now(), h.deadline()
	timed := h.key
	h.key = func(password, salt []byte, passes, memory uint32, threads uint8, keyLen uint32) []byte {
		clock.Advance(10 * cost) // a hash far slower than calibrated
		return timed(password, salt, passes, memory, threads, keyLen)
	}
	if _, err := h.hash(context.Background(), "a slow hash"); err != nil {
		t.Fatal(err)
	}
	if h.cost != cost || !h.deadline().Equal(clock.Now().Add(cost)) || !deadline.Equal(admitted.Add(cost)) {
		t.Fatalf("cost %s after a slow hash, want %s", h.cost, cost)
	}
}

func TestAShutdownEndsThePaddingAndFreesTheSlot(t *testing.T) {
	f := newNetFixture(t)
	f.n.hasher.cost = time.Hour
	padding := make(chan struct{})
	f.n.hasher.sleep = func(ctx context.Context, d time.Duration) error {
		close(padding)
		return sleepContext(ctx, d)
	}
	done := make(chan reply, 1)
	go func() {
		done <- f.from(t, "192.0.2.70", "POST", "/v1/session/password", map[string]any{"account": "nobody", "password": "a wrong password"})
	}()
	waitFor(t, padding, "the padding")
	if len(f.n.hasher.sem) != 1 {
		t.Fatalf("%d slots held while padding", len(f.n.hasher.sem))
	}
	f.hub.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the padding outlived the shutdown")
	}
	if len(f.n.hasher.sem) != 0 {
		t.Fatalf("%d slots held after the shutdown", len(f.n.hasher.sem))
	}
}

// queuedWrites closes its channel once n writes have come to the store's write lock.
func queuedWrites(f *netFixture, n int32) <-chan struct{} {
	reached := make(chan struct{})
	var count atomic.Int32
	f.s.OnWrite(func() {
		if count.Add(1) == n {
			close(reached)
		}
	})
	return reached
}

// holdWriter keeps the store's write lock until the returned function is called.
func holdWriter(t *testing.T, f *netFixture) func() {
	t.Helper()
	held, free := make(chan struct{}), make(chan struct{})
	go f.s.Do(context.Background(), "local", "hold-the-writer-"+strconv.Itoa(f.keys), "hold", nil, func(*store.Tx) ([]byte, error) {
		close(held)
		<-free
		return nil, errProbe
	})
	f.keys++
	waitFor(t, held, "the other writer")
	return func() { close(free) }
}

func TestAFinitePageReadAsTheSessionReachesSevenDaysIsNotSent(t *testing.T) {
	f := newNetFixture(t)
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	f.eventsRoute()
	if err := f.s.AppendEvent(context.Background(), "local", store.Event{Attempt: "a", Seq: 1, Time: "now", Kind: "text", Payload: json.RawMessage(`{"secret":"past the limit"}`)}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(sessionAbsolute - time.Minute)
	f.exec(t, "UPDATE sessions SET last_used=?", store.SessionStamp(f.clock.Now()))
	f.n.h.readPage = func(ctx context.Context, user, id string, after int64, limit, max int, size func(store.Event) int) ([]store.Event, error) {
		f.clock.Advance(2 * time.Minute) // the session passes seven days while its page is read
		return f.s.EventsPage(ctx, user, id, after, limit, max, size)
	}
	r := f.call(t, "GET", "/v1/attempts/a/events", cookie, nil)
	if r.status != 401 || bytes.Contains(r.body, []byte("past the limit")) {
		t.Fatalf("a page read past the limit: %d %s", r.status, r.body)
	}
}

func TestAnEnrollmentWaitingBehindAnotherWriterMeetsItsCodesExpiry(t *testing.T) {
	for _, kind := range []string{"passkey", "password"} {
		f := newNetFixture(t)
		_, code := f.account(t, "Ida", true)
		address := "192.0.2.97"
		key := newSoftKey(t)
		// The code has a minute left when the enrollment reads it.
		f.clock.Advance(store.CodeLifetime - time.Minute)
		var body map[string]any
		path := "/v1/enrollment/password"
		if kind == "passkey" {
			options := f.from(t, address, "POST", "/v1/enrollment/options", map[string]any{"code": code}).json(t)
			body = map[string]any{"code": code, "ceremony": options["ceremony"], "credential": key.create(t, options["options"].(map[string]any), testOrigin), "name": "laptop"}
			path = "/v1/enrollment/passkey"
		} else {
			body = map[string]any{"code": code, "password": "a password for Ida"}
		}
		free := holdWriter(t, f)
		queued := queuedWrites(f, 1)
		done := make(chan reply, 1)
		go func() { done <- f.from(t, address, "POST", path, body) }()
		waitFor(t, queued, "the enrollment reaching the write lock")
		f.clock.Advance(2 * time.Minute)
		free()
		r := <-done
		if r.status != 400 || r.code(t) != "code_invalid" || r.cookie != "" {
			t.Fatalf("%s enrolled past its code's expiry: %d %s", kind, r.status, r.body)
		}
		f.hub.limits.mu.Lock()
		a, _ := f.hub.limits.addresses.get(address)
		f.hub.limits.mu.Unlock()
		if a == nil || len(a.failures) != 1 || a.inFlight != 0 || len(a.starts) != 0 {
			t.Fatalf("%s: the address after it: %+v", kind, a)
		}
		if n := len(mustSessions(t, f, "")); n != 0 {
			t.Fatalf("%s: %d sessions", kind, n)
		}
	}
}

func TestAFailureCountsAtItsDeadlineEvenWhenItsCheckEndsLate(t *testing.T) {
	f := newColdNetFixture(t)
	clock, _ := timedHasher(f.n.hasher)
	f.n.clock = clock.Now
	h := f.n.hasher
	if err := h.calibrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, code := f.account(t, "Vera", true)
	if r := f.call(t, "POST", "/v1/enrollment/password", "", map[string]any{"code": code, "password": "the right password"}); r.status != 201 {
		t.Fatalf("enroll: %d", r.status)
	}
	attempt := func(address string) reply {
		return f.from(t, address, "POST", "/v1/session/password", map[string]any{"account": "Vera", "password": "a wrong password"})
	}
	for i := range accountFreeFailures - 1 {
		attempt("203.0.113." + strings.Repeat("1", i+1))
	}
	timed := h.key
	h.key = func(password, salt []byte, passes, memory uint32, threads uint8, keyLen uint32) []byte {
		clock.Advance(10 * h.cost) // under load: far slower than calibrated
		return timed(password, salt, passes, memory, threads, keyLen)
	}
	began := clock.Now()
	attempt("203.0.113.20")
	h.key = timed
	deadline := began.Add(h.cost)
	clock.Advance(deadline.Add(accountFirstDelay - time.Millisecond).Sub(clock.Now()))
	if r := attempt("203.0.113.21"); r.status != 429 || r.header.Get("Retry-After") != "1" {
		t.Fatalf("a millisecond before the delay from the deadline ends: %d %s", r.status, r.header.Get("Retry-After"))
	}
	clock.Advance(time.Millisecond)
	if r := attempt("203.0.113.22"); r.status != 401 {
		t.Fatalf("one delay after the deadline: %d", r.status)
	}
}

func TestARevocationThatCancelsTheRecheckAnswersUnauthenticated(t *testing.T) {
	f := newNetFixture(t)
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	f.eventsRoute()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.n.pause = func(ctx context.Context, at string) {
		if at != "recheck" {
			return
		}
		first := false
		once.Do(func() { first = true })
		if first {
			close(entered)
			<-release
			select { // the revocation's cancel reaches the recheck before its read
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
		}
	}
	result := f.streamOnce(t, "/v1/attempts/a/events", cookie)
	waitFor(t, entered, "the recheck")
	revokeOwner(t, f)
	close(release)
	select {
	case r := <-result:
		if r.status != 401 || r.code(t) != "unauthenticated" {
			t.Fatalf("a revocation during the recheck: %d %s", r.status, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no answer")
	}
}

func TestAPanicInACodeCheckSettlesItsReservation(t *testing.T) {
	f := newNetFixture(t)
	f.n.pause = func(_ context.Context, at string) {
		if at == "code" {
			panic("a failure inside the code check")
		}
	}
	func() {
		defer func() { _ = recover() }()
		f.from(t, "192.0.2.98", "POST", "/v1/enrollment/options", map[string]any{"code": "00000-00000-00000-00000"})
	}()
	f.hub.limits.mu.Lock()
	a, _ := f.hub.limits.addresses.get("192.0.2.98")
	f.hub.limits.mu.Unlock()
	if a == nil || a.inFlight != 0 || len(a.starts) != 0 {
		t.Fatalf("the reservation after a panic: %+v", a)
	}
}

func TestAFailureRecordedLateKeepsTheNewerDelayAndBlock(t *testing.T) {
	var th throttle
	now := time.Now()
	for i := range accountFreeFailures - 1 {
		th.fail("v", "192.0.2.1", now.Add(time.Duration(i)*time.Millisecond))
	}
	th.fail("v", "192.0.2.1", now.Add(time.Minute)) // the fifth, ended at a later deadline
	th.fail("v", "192.0.2.1", now)                  // the sixth, whose deadline came first
	// The sixth failure's delay runs from the streak's latest failure, a minute on, not from now.
	if wait := th.wait("v", "", now.Add(time.Minute)); wait != 2*accountFirstDelay {
		t.Fatalf("an earlier deadline moved the delay to %s", wait)
	}
	var block throttle
	for range addressFailures - 1 {
		block.fail("", "192.0.2.2", now.Add(time.Minute))
	}
	block.fail("", "192.0.2.2", now.Add(time.Minute)) // the block starts a minute on
	for range addressFailures {
		block.fail("", "192.0.2.2", now) // a later block from earlier deadlines
	}
	if wait := block.wait("", "192.0.2.2", now.Add(time.Minute)); wait != addressBlock {
		t.Fatalf("an earlier block replaced the newer one: %s", wait)
	}
}

func TestOverlappingFailuresCrossingTheAccountThresholdInReverseStartTheDelayAtTheLatest(t *testing.T) {
	var th throttle
	now := time.Now()
	for i := range accountFreeFailures - 2 {
		th.fail("v", "192.0.2."+strconv.Itoa(i+1), now)
	}
	later := now.Add(300 * time.Millisecond)
	th.fail("v", "192.0.2.10", later) // the later deadline is recorded first, as failure four
	th.fail("v", "192.0.2.11", now)   // the earlier one crosses the threshold
	if wait := th.wait("v", "", later.Add(accountFirstDelay-time.Millisecond)); wait != time.Millisecond {
		t.Fatalf("a millisecond before the delay from the latest failure ends: %s left", wait)
	}
	if wait := th.wait("v", "", later.Add(accountFirstDelay)); wait != 0 {
		t.Fatalf("at the delay's end: %s left", wait)
	}
}

func TestOverlappingFailuresCrossingTheAddressThresholdInReverseStartTheBlockAtTheLatest(t *testing.T) {
	var th throttle
	now := time.Now()
	for range addressFailures - 2 {
		th.fail("", "192.0.2.1", now)
	}
	later := now.Add(300 * time.Millisecond)
	th.fail("", "192.0.2.1", later) // the later deadline is recorded first
	th.fail("", "192.0.2.1", now)   // the earlier one crosses the threshold
	if wait := th.wait("", "192.0.2.1", later.Add(addressBlock-time.Millisecond)); wait != time.Millisecond {
		t.Fatalf("a millisecond before the block from the latest failure ends: %s left", wait)
	}
	if wait := th.wait("", "192.0.2.1", later.Add(addressBlock)); wait != 0 {
		t.Fatalf("at the block's end: %s left", wait)
	}
}

func TestAFailureRecordedAfterALaterOneStillMeetsTheWindowItBelongsTo(t *testing.T) {
	var th throttle
	const address = "192.0.2.50"
	t0 := time.Now()
	th.fail("", address, t0)
	th.fail("", address, t0)
	for range addressFailures - 3 {
		th.fail("", address, t0.Add(9*time.Minute))
	}
	deadline := t0.Add(addressWindow - time.Millisecond) // the password check's: twentieth in ten minutes
	check, wait := th.admit("", address, deadline.Add(-65*time.Millisecond))
	if wait != 0 {
		t.Fatalf("the password check was not admitted: %s", wait)
	}
	// A faster request fails just after the two oldest leave the window, and is recorded first.
	th.fail("", address, t0.Add(addressWindow+time.Millisecond))
	check.failed(deadline)
	if wait := th.wait("", address, deadline.Add(addressBlock-time.Millisecond)); wait != time.Millisecond {
		t.Fatalf("a millisecond before the block from the password deadline ends: %s left", wait)
	}
	if wait := th.wait("", address, deadline.Add(addressBlock)); wait != 0 {
		t.Fatalf("at the block's end: %s left", wait)
	}
}

func TestAnAddressKeepsABoundedHistory(t *testing.T) {
	var th throttle
	now := time.Now()
	for i := range 3 * addressKept {
		th.fail("", "192.0.2.51", now.Add(time.Duration(i)*time.Second))
	}
	a, _ := th.addresses.get("192.0.2.51")
	if len(a.failures) > addressKept {
		t.Fatalf("%d failure times kept", len(a.failures))
	}
}
