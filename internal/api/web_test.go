package api

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/cristiangirlea/ada-os/internal/store"
)

const portOrigin = "https://home.adanext.si:8443"

var testUI = fstest.MapFS{
	"index.html":     {Data: []byte("<!doctype html><title>Ada</title>")},
	"assets/app.js":  {Data: []byte("console.log('ada')")},
	"assets/app.css": {Data: []byte("body{}")},
}

// newWebFixture is newNetFixture with its network channel served through the web middleware.
func newWebFixture(t *testing.T, ui fs.FS, change ...func(*Execution)) *netFixture {
	t.Helper()
	f := newNetFixture(t, change...)
	f.server.Close()
	f.server = httptest.NewUnstartedServer(withRequestID(newWeb(f.n, WebConfig{Name: testRPID, Origin: testOrigin, Owner: "local", UI: ui, Clock: f.clock.Now})))
	// As adad's listener: OPTIONS * reaches the middleware.
	f.server.Config.DisableGeneralOptionsHandler = true
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}

// webHandler is the fixture's network channel behind the web middleware for origin.
func webHandler(f *netFixture, origin string, ui fs.FS) http.Handler {
	return withRequestID(newWeb(f.n, WebConfig{Name: testRPID, Origin: origin, Owner: "local", UI: ui, Clock: f.clock.Now}))
}

// webRequest sends one request straight to h with the given Host and headers.
func webRequest(t *testing.T, h http.Handler, method, host, path string, header map[string]string, body string) reply {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = host
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return readReply(t, rec.Result())
}

// mutation is the header of a browser's mutation from the configured origin.
func mutation(origin string) map[string]string {
	return map[string]string{"Origin": origin, "Content-Type": "application/json"}
}

func TestTheWebChannelServesOnlyItsConfiguredHost(t *testing.T) {
	f := newNetFixture(t)
	for origin, cases := range map[string]map[string]int{
		portOrigin: {"home.adanext.si:8443": 200, "HOME.AdaNext.si:8443": 200, "home.adanext.si": 421, "evil.example:8443": 421, "192.168.1.20:8443": 421, "home.adanext.si:9443": 421, "": 421},
		testOrigin: {"home.adanext.si": 200, "home.adanext.si:443": 421, "preview.home.adanext.si": 421},
	} {
		h := webHandler(f, origin, testUI)
		for host, want := range cases {
			r := webRequest(t, h, "GET", host, "/", nil, "")
			if r.status != want {
				t.Fatalf("%s, Host %q: %d %s", origin, host, r.status, r.body)
			}
			if want == 421 && r.code(t) != "host_invalid" {
				t.Fatalf("%s, Host %q: %s", origin, host, r.body)
			}
		}
	}
}

func TestAMutationNeedsTheExactConfiguredOrigin(t *testing.T) {
	f := newNetFixture(t)
	h := webHandler(f, portOrigin, testUI)
	routes := []struct{ method, path string }{{"POST", "/v1/session/passkey/options"}, {"POST", "/v1/session/logout"}, {"DELETE", "/v1/account/sessions/x"}, {"POST", "/"}, {"PUT", "/assets/app.js"}}
	for _, origin := range []string{"", "null", "https://evil.example", "https://preview.home.adanext.si:8443", "https://home.adanext.si:9443", "https://home.adanext.si", "http://home.adanext.si:8443", portOrigin + "/"} {
		for _, route := range routes {
			header := map[string]string{"Content-Type": "application/json"}
			if origin != "" {
				header["Origin"] = origin
			}
			if r := webRequest(t, h, route.method, "home.adanext.si:8443", route.path, header, ""); r.status != 403 || r.code(t) != "origin_refused" {
				t.Fatalf("%s %s from %q: %d %s", route.method, route.path, origin, r.status, r.body)
			}
		}
	}
	req := httptest.NewRequest("POST", "/v1/session/passkey/options", nil)
	req.Host = "home.adanext.si:8443"
	req.Header["Origin"] = []string{portOrigin, "https://evil.example"}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("two Origin headers: %d", rec.Code)
	}
	for route, want := range map[[2]string]int{{"POST", "/v1/session/passkey/options"}: 200, {"POST", "/v1/session/logout"}: 401, {"DELETE", "/v1/account/sessions/x"}: 401, {"POST", "/"}: 405} {
		if r := webRequest(t, h, route[0], "home.adanext.si:8443", route[1], mutation(portOrigin), ""); r.status != want {
			t.Fatalf("%s %s from the exact origin: %d %s", route[0], route[1], r.status, r.body)
		}
	}
}

func TestAMutationNeedsAJSONContentType(t *testing.T) {
	f := newNetFixture(t)
	h := webHandler(f, portOrigin, testUI)
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "application/jsonx", "text/json"} {
		header := map[string]string{"Origin": portOrigin}
		if contentType != "" {
			header["Content-Type"] = contentType
		}
		for _, path := range []string{"/v1/session/passkey/options", "/v1/session/logout", "/"} {
			if r := webRequest(t, h, "POST", "home.adanext.si:8443", path, header, ""); r.status != 415 || r.code(t) != "content_type_refused" {
				t.Fatalf("POST %s as %q: %d %s", path, contentType, r.status, r.body)
			}
		}
	}
	req := httptest.NewRequest("POST", "/v1/session/passkey/options", nil)
	req.Host = "home.adanext.si:8443"
	req.Header.Set("Origin", portOrigin)
	req.Header["Content-Type"] = []string{"application/json", "text/plain"}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 415 {
		t.Fatalf("two Content-Type fields: %d", rec.Code)
	}
	for _, contentType := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
		if r := webRequest(t, h, "POST", "home.adanext.si:8443", "/v1/session/passkey/options", map[string]string{"Origin": portOrigin, "Content-Type": contentType}, ""); r.status != 200 {
			t.Fatalf("as %q: %d %s", contentType, r.status, r.body)
		}
	}
}

func TestNoResponseCarriesACrossOriginHeader(t *testing.T) {
	f := newNetFixture(t)
	h := webHandler(f, portOrigin, testUI)
	preflight := map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type"}
	own := map[string]string{"Origin": portOrigin, "Access-Control-Request-Method": "POST"}
	for _, c := range []struct {
		method, host, path string
		header             map[string]string
	}{
		{"OPTIONS", "home.adanext.si:8443", "/v1/tasks", preflight},
		{"OPTIONS", "home.adanext.si:8443", "/v1/tasks", own},
		{"OPTIONS", "home.adanext.si:8443", "/", preflight},
		{"GET", "home.adanext.si:8443", "/", map[string]string{"Origin": "https://evil.example"}},
		{"GET", "home.adanext.si:8443", "/v1/account", map[string]string{"Origin": portOrigin}},
		{"POST", "home.adanext.si:8443", "/v1/session/passkey/options", mutation(portOrigin)},
		{"GET", "evil.example", "/", nil},
	} {
		r := webRequest(t, h, c.method, c.host, c.path, c.header, "")
		for name := range r.header {
			if strings.HasPrefix(strings.ToLower(name), "access-control-") {
				t.Fatalf("%s %s answered %d with %s", c.method, c.path, r.status, name)
			}
		}
	}
}

// indexPolicy is the policy of the shared UI's page, which the web channel sends as a header.
func indexPolicy(t *testing.T) string {
	t.Helper()
	page, err := os.ReadFile("../../ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`http-equiv="Content-Security-Policy"\s+content="([^"]+)"`).FindSubmatch(page)
	if m == nil {
		t.Fatal("ui/index.html has no policy")
	}
	return string(m[1])
}

func TestEveryResponseCarriesTheContentSecurityPolicy(t *testing.T) {
	f := newNetFixture(t)
	h := webHandler(f, portOrigin, testUI)
	policy := indexPolicy(t)
	if policy != contentSecurityPolicy || !strings.Contains(policy, "frame-ancestors 'none'") {
		t.Fatalf("the header's policy %q is not the page's %q", contentSecurityPolicy, policy)
	}
	for _, c := range []struct{ host, path, cache string }{
		{"home.adanext.si:8443", "/", ""},
		{"home.adanext.si:8443", "/assets/app.js", ""},
		{"home.adanext.si:8443", "/v1", "no-store"},
		{"home.adanext.si:8443", "/v1/tasks", "no-store"},
		{"home.adanext.si:8443", "/v1/account", "no-store"},
		{"home.adanext.si:8443", "/v10", ""},
		{"home.adanext.si:8443", "/missing.js", ""},
		{"evil.example", "/", ""},
	} {
		r := webRequest(t, h, "GET", c.host, c.path, nil, "")
		if got := r.header.Get("Content-Security-Policy"); got != policy {
			t.Fatalf("%s%s: policy %q", c.host, c.path, got)
		}
		if r.header.Get("X-Content-Type-Options") != "nosniff" || r.header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s%s: %v", c.host, c.path, r.header)
		}
		if r.header.Get("Cache-Control") != c.cache {
			t.Fatalf("%s%s: Cache-Control %q", c.host, c.path, r.header.Get("Cache-Control"))
		}
	}
}

func TestUIFilesHaveTheSameContentTypeOnEveryMachine(t *testing.T) {
	want := map[string]string{
		"index.html":          "text/html; charset=utf-8",
		"assets/app.js":       "text/javascript; charset=utf-8",
		"assets/app.mjs":      "text/javascript; charset=utf-8",
		"assets/app.css":      "text/css; charset=utf-8",
		"assets/app.js.map":   "application/json",
		"assets/data.json":    "application/json",
		"assets/logo.svg":     "image/svg+xml",
		"assets/logo.png":     "image/png",
		"favicon.ico":         "image/x-icon",
		"assets/font.woff":    "font/woff",
		"assets/font.woff2":   "font/woff2",
		"robots.txt":          "text/plain; charset=utf-8",
		"assets/APP.JS":       "text/javascript; charset=utf-8",
		"assets/unknown.blob": "application/octet-stream",
	}
	ui := fstest.MapFS{}
	for name := range want {
		ui[name] = &fstest.MapFile{Data: []byte{0, 1, 2}}
	}
	h := webHandler(newNetFixture(t), portOrigin, ui)
	for name, ct := range want {
		if r := webRequest(t, h, "GET", "home.adanext.si:8443", "/"+name, nil, ""); r.status != 200 || r.header.Get("Content-Type") != ct {
			t.Errorf("%s: %d %q, want %q", name, r.status, r.header.Get("Content-Type"), ct)
		}
	}
}

func TestStaticAssetsAreServedWithoutASessionAndNeverOutsideTheUI(t *testing.T) {
	f := newNetFixture(t)
	h := webHandler(f, portOrigin, testUI)
	index := string(testUI["index.html"].Data)
	for path, want := range map[string]string{
		"/":              index,
		"/index.html":    index,
		"/assets/app.js": "console.log('ada')",
	} {
		r := webRequest(t, h, "GET", "home.adanext.si:8443", path, nil, "")
		if r.status != 200 || string(r.body) != want {
			t.Fatalf("GET %s: %d %q", path, r.status, r.body)
		}
	}
	if r := webRequest(t, h, "GET", "home.adanext.si:8443", "/assets/app.js", nil, ""); !strings.HasPrefix(r.header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("app.js as %q", r.header.Get("Content-Type"))
	}
	if r := webRequest(t, h, "HEAD", "home.adanext.si:8443", "/assets/app.css", nil, ""); r.status != 200 || len(r.body) != 0 {
		t.Fatalf("HEAD: %d %q", r.status, r.body)
	}
	for _, path := range []string{"/projects/p", "/assets", "/missing", "/missing.js", "/assets/missing.css", "/../web_test.go", "/assets/../../go.mod", "/assets/", "//index.html", "/./index.html"} {
		if r := webRequest(t, h, "GET", "home.adanext.si:8443", path, nil, ""); r.status != 404 {
			t.Fatalf("GET %s: %d %q", path, r.status, r.body)
		}
	}
	if r := webRequest(t, h, "DELETE", "home.adanext.si:8443", "/index.html", mutation(portOrigin), ""); r.status != 405 || r.header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("DELETE a static path: %d %v", r.status, r.header)
	}
	none := webHandler(f, portOrigin, nil)
	for _, path := range []string{"/", "/index.html", "/projects/p"} {
		if r := webRequest(t, none, "GET", "home.adanext.si:8443", path, nil, ""); r.status != 404 {
			t.Fatalf("without a UI, GET %s: %d", path, r.status)
		}
	}
}

// concrete turns a route pattern into a method and a path that matches it.
func concrete(pattern string) (string, string) {
	method, path, _ := strings.Cut(pattern, " ")
	return method, regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(path, "x")
}

func TestWithoutASessionOnlyLoginEnrollmentAndStaticAssetsAreAnswered(t *testing.T) {
	f := newWebFixture(t, testUI)
	patterns := []string{}
	for _, routes := range []map[string]http.HandlerFunc{f.n.h.localRoutes(), f.n.h.ownerRoutes(), f.n.sessionRoutes()} {
		for pattern := range routes {
			patterns = append(patterns, pattern)
		}
	}
	slices.Sort(patterns)
	for _, pattern := range patterns {
		method, path := concrete(pattern)
		if r := f.call(t, method, path, "", nil); r.status != 401 || r.code(t) != "unauthenticated" {
			t.Fatalf("%s without a session: %d %s", pattern, r.status, r.body)
		}
	}
	if r := f.call(t, "GET", "/", "", nil); r.status != 200 {
		t.Fatalf("the page without a session: %d", r.status)
	}
	if r := f.call(t, "POST", "/v1/session/passkey/options", "", nil); r.status != 200 {
		t.Fatalf("login options without a session: %d %s", r.status, r.body)
	}
}

func TestEveryLocalRouteIsInExactlyOneNetworkList(t *testing.T) {
	f := newNetFixture(t)
	all := map[string]bool{}
	for _, routes := range []map[string]http.HandlerFunc{f.n.h.localRoutes(), f.n.h.ownerRoutes()} {
		for pattern := range routes {
			all[pattern] = true
		}
	}
	count := map[string]int{}
	for _, list := range [][]string{networkReads, networkLocalOnly, networkOwner, networkLater} {
		for _, pattern := range list {
			count[pattern]++
		}
	}
	for pattern := range f.n.h.ownerRoutes() {
		count[pattern]++
	}
	for pattern := range all {
		if count[pattern] != 1 {
			t.Errorf("%s is in %d network lists", pattern, count[pattern])
		}
	}
	for pattern := range count {
		if !all[pattern] {
			t.Errorf("%s is listed but is no local route", pattern)
		}
	}
	want := []string{"GET /v1/projects", "GET /v1/capabilities", "GET /v1/tasks", "GET /v1/tasks/{id}", "GET /v1/tasks/{id}/checks", "GET /v1/tasks/{id}/changes", "GET /v1/tasks/{id}/files", "GET /v1/tasks/{id}/findings", "GET /v1/projects/{id}/snapshots/{sid}", "GET /v1/attempts/{id}/events", "GET /v1/attempts/{id}/artifacts/{hash}", "GET /v1/checks/{id}/log", "GET /v1/tasks/{task}/checks/{id}/log"}
	reads := slices.Clone(networkReads)
	slices.Sort(reads)
	slices.Sort(want)
	if !slices.Equal(reads, want) {
		t.Fatalf("the network reads are %v", reads)
	}
}

func TestWithASessionTheNetworkRefusesStartsRefinementsHostPathsAndTheExperienceCard(t *testing.T) {
	f := newWebFixture(t, testUI)
	owner := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	// The task writes are the owner account's alone; another account's session is refused them.
	_, code := f.account(t, "Vera", false)
	cookie := f.mustEnroll(t, code, newSoftKey(t))
	for _, c := range []struct {
		method, path string
		body         any
		status       int
		code         string
	}{
		{"POST", "/v1/tasks/t/start", map[string]any{"backend": "codex"}, 403, "not_on_network"},
		{"POST", "/v1/tasks/t/refine", map[string]any{"objective": "again"}, 403, "not_on_network"},
		{"POST", "/v1/projects", map[string]any{"path": t.TempDir()}, 403, "local_only"},
		{"POST", "/v1/projects/inspect", map[string]any{"path": t.TempDir()}, 403, "local_only"},
		{"GET", "/v1/experience", nil, 403, "local_only"},
		{"GET", "/v1/listener", nil, 403, "local_only"},
		{"PUT", "/v1/listener", map[string]any{"enabled": true, "address": "127.0.0.1:8443", "name": "home.adanext.si"}, 403, "local_only"},
		{"PUT", "/v1/listener/certificate", map[string]any{"certificate": "/etc/passwd", "key": "/etc/passwd"}, 403, "local_only"},
	} {
		r := f.call(t, c.method, c.path, cookie, c.body)
		if r.status != c.status || r.code(t) != c.code {
			t.Fatalf("%s %s: %d %s", c.method, c.path, r.status, r.body)
		}
		if c.code == "not_on_network" && r.json(t)["message"] != "not available on the network yet" {
			t.Fatalf("%s %s: %s", c.method, c.path, r.body)
		}
	}
	for _, list := range []struct {
		patterns []string
		code     string
		owner    bool // the owner account's session is refused too
	}{{networkLater, "not_on_network", true}, {networkLocalOnly, "local_only", true}, {networkOwner, "not_on_network", false}} {
		for _, pattern := range list.patterns {
			method, path := concrete(pattern)
			if r := f.call(t, method, path, cookie, nil); r.status != 403 || r.code(t) != list.code {
				t.Fatalf("%s with a session: %d %s", pattern, r.status, r.body)
			}
			if r := f.call(t, method, path, owner, nil); list.owner && (r.status != 403 || r.code(t) != list.code) {
				t.Fatalf("%s with the owner's session: %d %s", pattern, r.status, r.body)
			}
		}
	}
	if got := f.column(t, "SELECT state FROM tasks WHERE id='t'"); len(got) != 1 || string(got[0]) != "draft" {
		t.Fatalf("the task moved to %s", got)
	}
}

func TestTheNetworkProjectListShowsAMembersProjectWithoutATaskAndNoOtherUsers(t *testing.T) {
	f := newWebFixture(t, testUI)
	id, code := f.account(t, "Vera", false)
	cookie := f.mustEnroll(t, code, newSoftKey(t))
	f.exec(t, "INSERT INTO projects(id,name,source_path,created) VALUES('q','Quiet','/host/secret/repo','2026-11-24T10:00:00Z')")
	f.exec(t, "INSERT INTO project_members(project,user,role,added) VALUES('q',?,'member','now')", id)
	r := f.call(t, "GET", "/v1/projects", cookie, nil)
	var projects []map[string]any
	if err := json.Unmarshal(r.body, &projects); err != nil || r.status != 200 {
		t.Fatalf("%d %s: %v", r.status, r.body, err)
	}
	want := map[string]any{"id": "q", "name": "Quiet", "role": "member", "created": "2026-11-24T10:00:00Z", "default_mode": "readonly"}
	if len(projects) != 1 || !maps.Equal(projects[0], want) {
		t.Fatalf("Vera sees %s", r.body)
	}
	if strings.Contains(string(r.body), "/host/secret") {
		t.Fatalf("a host path: %s", r.body)
	}
}

func TestTheLocalProjectListShowsTheOwnersProjects(t *testing.T) {
	f := newNetFixture(t)
	f.exec(t, "INSERT INTO projects(id,name,source_path,created) VALUES('other','Other','/x','now')")
	f.exec(t, "INSERT INTO users(id,name,created) VALUES('vera','Vera','now')")
	f.exec(t, "INSERT INTO project_members(project,user,role,added) VALUES('other','vera','owner','now')")
	r := f.owner(t, "GET", "/v1/projects", nil)
	var projects []map[string]any
	if err := json.Unmarshal(r.body, &projects); err != nil || r.status != 200 {
		t.Fatalf("%d %s: %v", r.status, r.body, err)
	}
	if len(projects) != 1 || !maps.Equal(projects[0], map[string]any{"id": "p", "name": "p", "role": "owner", "created": "now", "default_mode": "readonly"}) {
		t.Fatalf("the owner sees %s", r.body)
	}
}

// symlinkOrSkip makes link name target; only Windows without the symlink privilege skips.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
			t.Skipf("creating a symlink needs a privilege this account lacks: %v", err)
		}
		t.Fatal(err)
	}
}

func TestALinkInTheUIDirectoryNeverReachesOutsideIt(t *testing.T) {
	f := newNetFixture(t)
	outside := filepath.Join(t.TempDir(), "secret.js")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("page"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(dir, "linked.js"))
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	h := webHandler(f, portOrigin, root.FS())
	if r := webRequest(t, h, "GET", "home.adanext.si:8443", "/", nil, ""); r.status != 200 || string(r.body) != "page" {
		t.Fatalf("GET /: %d %q", r.status, r.body)
	}
	if r := webRequest(t, h, "GET", "home.adanext.si:8443", "/linked.js", nil, ""); r.status != 404 || strings.Contains(string(r.body), "secret") {
		t.Fatalf("GET /linked.js: %d %q", r.status, r.body)
	}
}

// rawExchange sends a raw request over TLS to the fixture's server and reads its answer.
func rawExchange(t *testing.T, f *netFixture, request string) *http.Response {
	t.Helper()
	conn, err := tls.Dial("tcp", f.server.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestARequestTargetThatIsNotOriginFormIsRefused(t *testing.T) {
	f := newWebFixture(t, testUI)
	for _, request := range []string{
		"GET https://home.adanext.si/ HTTP/1.1\r\nHost: home.adanext.si\r\nConnection: close\r\n\r\n",
		"GET https://home.adanext.si/v1/projects HTTP/1.1\r\nHost: evil.example\r\nConnection: close\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: home.adanext.si\r\nConnection: close\r\n\r\n",
	} {
		resp := rawExchange(t, f, request)
		var body struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode != 400 || body.Error != "bad_request" || resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("%q: %d %+v", request, resp.StatusCode, body)
		}
	}
	if resp := rawExchange(t, f, "GET / HTTP/1.1\r\nHost: home.adanext.si\r\nConnection: close\r\n\r\n"); resp.StatusCode != 200 {
		t.Fatalf("origin-form: %d", resp.StatusCode)
	}
}

func TestEveryNetworkAPIResponseIsNoStoreStreamsAndBlobsIncluded(t *testing.T) {
	f := newWebFixture(t, testUI)
	content := "artifact bytes"
	hash := strings.Repeat("ab", 32)
	f.n.h.exec.Artifacts = blobArtifacts{hash: content}
	f.n.h.exec.References = blobReferences{hash: int64(len(content))}
	f.exec(t, "INSERT INTO check_results(id,task,attempt,ord,definition_hash,definition_json,state,started_at) VALUES('c','t','a',0,'h','{}','passed','now')")
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	stream := f.openStream(t, cookie)
	stream.Body.Close()
	if got := stream.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("the event stream: Cache-Control %q", got)
	}
	for path, want := range map[string]int{"/v1/attempts/a/artifacts/" + hash: 200, "/v1/tasks/t/checks/c/log": 200, "/v1/checks/c/log": 200, "/v1/tasks": 200, "/v1/nothing": 404} {
		r := f.call(t, "GET", path, cookie, nil)
		if r.status != want || r.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("GET %s: %d Cache-Control %q", path, r.status, r.header.Get("Cache-Control"))
		}
	}
	h := webHandler(f, portOrigin, testUI)
	for _, c := range []struct{ host, path string }{{"evil.example", "/v1/tasks"}, {"home.adanext.si:8443", "/v1/tasks"}} {
		if r := webRequest(t, h, "POST", c.host, c.path, nil, ""); r.status/100 != 4 || r.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("a refusal of %s%s: %d Cache-Control %q", c.host, c.path, r.status, r.header.Get("Cache-Control"))
		}
	}
}

type blobArtifacts map[string]string

func (a blobArtifacts) Open(hash string) (io.ReadCloser, error) {
	content, ok := a[hash]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

type blobReferences map[string]int64

func (r blobReferences) ArtifactReference(_ context.Context, _, hash string) (int64, error) {
	size, ok := r[hash]
	if !ok {
		return 0, store.ErrNotFound
	}
	return size, nil
}

func TestASessionsDriftRequestIsNotOnTheNetwork(t *testing.T) {
	f := newWebFixture(t, testUI)
	cookie := f.mustEnroll(t, f.newCode(t, "local"), newSoftKey(t))
	if r := f.call(t, "GET", "/v1/projects/p/drift", cookie, nil); r.status != 403 || r.code(t) != "not_on_network" {
		t.Fatalf("drift: %d %s", r.status, r.body)
	}
}
