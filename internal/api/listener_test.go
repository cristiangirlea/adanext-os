package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeListener struct {
	mu        sync.Mutex
	status    ListenerStatus
	err       error
	settings  []ListenerSetting
	installed [][2]string
}

func (l *fakeListener) Status() ListenerStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

func (l *fakeListener) Configure(_ context.Context, s ListenerSetting) (ListenerStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.settings = append(l.settings, s)
	return l.status, l.err
}

func (l *fakeListener) InstallCertificate(_ context.Context, cert, key string) (ListenerStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.installed = append(l.installed, [2]string{cert, key})
	return l.status, l.err
}

// listenerChannel is a local channel over the fixture's store whose actor is user.
func listenerChannel(f *netFixture, user string, l ListenerControl) http.Handler {
	return New(f.s, f.hub, func(*http.Request) (string, error) { return user, nil }, Execution{Owner: "local", Listener: l})
}

func localCall(t *testing.T, h http.Handler, method, path, body string) reply {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return readReply(t, rec.Result())
}

func TestTheListenerRoutesAnswerUnavailableWithoutAControl(t *testing.T) {
	f := newNetFixture(t)
	h := listenerChannel(f, "local", nil)
	for _, c := range []struct{ method, path, body string }{{"GET", "/v1/listener", ""}, {"PUT", "/v1/listener", `{"enabled":false,"address":"","name":""}`}, {"PUT", "/v1/listener/certificate", `{"certificate":"/a","key":"/b"}`}} {
		if r := localCall(t, h, c.method, c.path, c.body); r.status != 503 || r.code(t) != "listener_unavailable" {
			t.Fatalf("%s %s: %d %s", c.method, c.path, r.status, r.body)
		}
	}
}

func TestTheOwnerReadsAndSetsTheListener(t *testing.T) {
	f := newNetFixture(t)
	l := &fakeListener{status: ListenerStatus{Enabled: true, Address: "192.168.1.20:8443", Name: "home.adanext.si", Origin: "https://home.adanext.si:8443", State: "off", Reason: "certificate_missing", Message: "No certificate is installed."}}
	h := listenerChannel(f, "local", l)
	r := localCall(t, h, "GET", "/v1/listener", "")
	var got map[string]any
	if err := json.Unmarshal(r.body, &got); err != nil || r.status != 200 {
		t.Fatalf("%d %s: %v", r.status, r.body, err)
	}
	for k, v := range map[string]any{"enabled": true, "address": "192.168.1.20:8443", "name": "home.adanext.si", "origin": "https://home.adanext.si:8443", "state": "off", "reason": "certificate_missing", "message": "No certificate is installed.", "certificate": nil} {
		if got[k] != v {
			t.Fatalf("%s = %v in %s", k, got[k], r.body)
		}
	}
	if len(got) != 8 {
		t.Fatalf("fields: %s", r.body)
	}
	l.status.Certificate = &ListenerCertificate{Names: []string{"home.adanext.si"}, NotBefore: "2026-11-01T00:00:00Z", NotAfter: "2027-01-30T00:00:00Z", SHA256: "ab"}
	r = localCall(t, h, "PUT", "/v1/listener", `{"enabled":true,"address":"192.168.1.20:8443","name":"home.adanext.si"}`)
	if r.status != 200 || !strings.Contains(string(r.body), `"certificate":{"names":["home.adanext.si"],"not_before":"2026-11-01T00:00:00Z","not_after":"2027-01-30T00:00:00Z","sha256":"ab"}`) {
		t.Fatalf("PUT: %d %s", r.status, r.body)
	}
	if len(l.settings) != 1 || l.settings[0] != (ListenerSetting{Enabled: true, Address: "192.168.1.20:8443", Name: "home.adanext.si"}) {
		t.Fatalf("configured %+v", l.settings)
	}
}

func TestAListenerSettingNeedsAllThreeFieldsAndNoOthers(t *testing.T) {
	f := newNetFixture(t)
	l := &fakeListener{}
	h := listenerChannel(f, "local", l)
	for _, body := range []string{``, `{}`, `{"enabled":true}`, `{"enabled":true,"address":"127.0.0.1:1"}`, `{"address":"127.0.0.1:1","name":"a.b"}`, `{"enabled":true,"address":"127.0.0.1:1","name":"a.b","port":1}`, `{"enabled":"yes","address":"127.0.0.1:1","name":"a.b"}`, `[]`} {
		if r := localCall(t, h, "PUT", "/v1/listener", body); r.status != 400 || r.code(t) != "bad_request" {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	if len(l.settings) != 0 {
		t.Fatalf("configured %+v", l.settings)
	}
}

func TestAListenerRefusalCarriesItsOwnCodeAndMessage(t *testing.T) {
	f := newNetFixture(t)
	for _, code := range []string{"address_invalid", "name_invalid", "certificate_invalid", "certificate_expired", "certificate_name"} {
		l := &fakeListener{err: &ListenerError{Code: code, Message: "The check named " + code + " failed."}}
		h := listenerChannel(f, "local", l)
		for _, c := range []struct{ path, body string }{{"/v1/listener", `{"enabled":true,"address":"x","name":"y"}`}, {"/v1/listener/certificate", `{"certificate":"` + jsonPath(filepath.Join(t.TempDir(), "c.pem")) + `","key":"` + jsonPath(filepath.Join(t.TempDir(), "k.pem")) + `"}`}} {
			r := localCall(t, h, "PUT", c.path, c.body)
			if r.status != 400 || r.code(t) != code || r.json(t)["message"] != "The check named "+code+" failed." {
				t.Fatalf("%s %s: %d %s", code, c.path, r.status, r.body)
			}
		}
	}
	l := &fakeListener{err: errors.New("disk full")}
	if r := localCall(t, listenerChannel(f, "local", l), "PUT", "/v1/listener", `{"enabled":true,"address":"x","name":"y"}`); r.status != 500 || strings.Contains(string(r.body), "disk full") {
		t.Fatalf("another failure: %d %s", r.status, r.body)
	}
}

func jsonPath(p string) string { return strings.ReplaceAll(p, `\`, `\\`) }

func TestACertificateInstallNeedsTwoAbsolutePaths(t *testing.T) {
	f := newNetFixture(t)
	l := &fakeListener{status: ListenerStatus{State: "serving"}}
	h := listenerChannel(f, "local", l)
	cert, key := filepath.Join(t.TempDir(), "cert.pem"), filepath.Join(t.TempDir(), "key.pem")
	for _, body := range []string{`{}`, `{"certificate":"cert.pem","key":"` + jsonPath(key) + `"}`, `{"certificate":"` + jsonPath(cert) + `","key":""}`, `{"certificate":"` + jsonPath(cert) + `"}`, `{"certificate":"` + jsonPath(cert) + `","key":"` + jsonPath(key) + `","x":1}`} {
		if r := localCall(t, h, "PUT", "/v1/listener/certificate", body); r.status != 400 || r.code(t) != "bad_request" {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	if len(l.installed) != 0 {
		t.Fatalf("installed %v", l.installed)
	}
	r := localCall(t, h, "PUT", "/v1/listener/certificate", `{"certificate":"`+jsonPath(cert)+`","key":"`+jsonPath(key)+`"}`)
	if r.status != 200 || r.json(t)["state"] != "serving" || len(l.installed) != 1 || l.installed[0] != [2]string{cert, key} {
		t.Fatalf("%d %s, installed %v", r.status, r.body, l.installed)
	}
}

func TestAnotherAccountIsRefusedTheListenerRoutes(t *testing.T) {
	f := newNetFixture(t)
	f.exec(t, "INSERT INTO users(id,name,created) VALUES('vera','Vera','now')")
	l := &fakeListener{}
	h := listenerChannel(f, "vera", l)
	for _, c := range []struct{ method, path, body string }{{"GET", "/v1/listener", ""}, {"PUT", "/v1/listener", `{"enabled":false,"address":"","name":""}`}, {"PUT", "/v1/listener/certificate", `{"certificate":"/a","key":"/b"}`}} {
		if r := localCall(t, h, c.method, c.path, c.body); r.status != 403 || r.code(t) != "forbidden" {
			t.Fatalf("%s %s: %d %s", c.method, c.path, r.status, r.body)
		}
	}
	if len(l.settings)+len(l.installed) != 0 {
		t.Fatal("the control was reached")
	}
}

func TestListenerSettingsAreValidated(t *testing.T) {
	long := strings.Repeat("a", 63)
	for _, address := range []string{"192.168.1.20", "0.0.0.0:8443", "[::]:8443", "224.0.0.1:8443", "[ff02::1]:8443", "[::ffff:0.0.0.0]:8443", "home.adanext.si:8443", "localhost:8443", "192.168.1.20:0", "192.168.1.20:65536", "192.168.1.20:-1", "192.168.1.20:+80", "192.168.1.20:http", ":8443", "192.168.1.20:", "192.168.1.020:8443", "[::%eth0]:443", "[fe80::1%eth0]:8443", "127.0.0.1:0443", "127.0.0.1:08443"} {
		var e *ListenerError
		if err := ValidateListenerSetting(ListenerSetting{Address: address}); !errors.As(err, &e) || e.Code != "address_invalid" || e.Message == "" {
			t.Fatalf("address %q: %v", address, err)
		}
	}
	for _, address := range []string{"", "127.0.0.1:8443", "192.168.1.20:443", "[::1]:8443", "[fe80::1]:1", "10.0.0.1:65535", "127.0.0.1:10"} {
		if err := ValidateListenerSetting(ListenerSetting{Address: address}); err != nil {
			t.Fatalf("address %q: %v", address, err)
		}
	}
	for _, name := range []string{"Home.adanext.si", "home", "-home.adanext.si", "home-.adanext.si", "home..adanext.si", "home.adanext.si.", ".home.adanext.si", "192.168.1.20", "home_x.adanext.si", long + "a.si", strings.Repeat(long+".", 4) + "si", "home.adanext.si:8443", "::1", "home adanext.si"} {
		var e *ListenerError
		if err := ValidateListenerSetting(ListenerSetting{Name: name}); !errors.As(err, &e) || e.Code != "name_invalid" || e.Message == "" {
			t.Fatalf("name %q: %v", name, err)
		}
	}
	for _, name := range []string{"", "home.adanext.si", "localhost", "a.b", "x-1.y-2", long + ".si"} {
		if err := ValidateListenerSetting(ListenerSetting{Name: name}); err != nil {
			t.Fatalf("name %q: %v", name, err)
		}
	}
}

func TestTheListenerOriginNamesThePortUnlessItIs443(t *testing.T) {
	for _, c := range []struct{ name, address, want string }{
		{"home.adanext.si", "192.168.1.20:443", "https://home.adanext.si"},
		{"home.adanext.si", "192.168.1.20:8443", "https://home.adanext.si:8443"},
		{"home.adanext.si", "[::1]:8443", "https://home.adanext.si:8443"},
		{"home.adanext.si", "", ""},
		{"", "192.168.1.20:8443", ""},
	} {
		if got := ListenerOrigin(c.name, c.address); got != c.want {
			t.Fatalf("%q %q: %q", c.name, c.address, got)
		}
	}
}

func TestACertificateInstallRefusesARemoteOrDevicePathBeforeTheControl(t *testing.T) {
	f := newNetFixture(t)
	l := &fakeListener{}
	h := listenerChannel(f, "local", l)
	key := filepath.Join(t.TempDir(), "key.pem")
	for _, path := range []string{`\\host\share\c.pem`, "//host/share/c.pem", `\\?\C:\c.pem`, `\\.\pipe\c`, filepath.Join(t.TempDir(), "c\x00.pem")} {
		body, _ := json.Marshal(map[string]string{"certificate": path, "key": key})
		if r := localCall(t, h, "PUT", "/v1/listener/certificate", string(body)); r.status != 400 || r.code(t) != "bad_request" {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	if len(l.installed) != 0 {
		t.Fatalf("the control was reached: %v", l.installed)
	}
}

func TestAStoppedListenerAnswersShuttingDown(t *testing.T) {
	f := newNetFixture(t)
	l := &fakeListener{err: fmt.Errorf("configure: %w", ErrListenerStopped)}
	h := listenerChannel(f, "local", l)
	for _, c := range []struct{ path, body string }{{"/v1/listener", `{"enabled":true,"address":"","name":""}`}, {"/v1/listener/certificate", `{"certificate":"` + jsonPath(filepath.Join(t.TempDir(), "c.pem")) + `","key":"` + jsonPath(filepath.Join(t.TempDir(), "k.pem")) + `"}`}} {
		if r := localCall(t, h, "PUT", c.path, c.body); r.status != 503 || r.code(t) != "shutting_down" {
			t.Fatalf("%s: %d %s", c.path, r.status, r.body)
		}
	}
}
