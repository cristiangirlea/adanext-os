package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/api"
	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/ipc"
	"github.com/cristiangirlea/ada-os/internal/store"
)

const testHost = "home.adanext.si"

var testNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testCA is an ECDSA P-256 authority the tests' clients trust.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"}, NotBefore: testNow.Add(-24 * time.Hour), NotAfter: testNow.Add(10 * 365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// pair issues a leaf for names valid in [from, until): its chain (leaf, then the CA) and its key.
func (ca *testCA) pair(t *testing.T, from, until time.Time, names ...string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "leaf"}, DNSNames: names, NotBefore: from, NotAfter: until, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})...)
	return chain, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// valid issues a pair for the test host valid around testNow.
func (ca *testCA) valid(t *testing.T) ([]byte, []byte) {
	return ca.pair(t, testNow.Add(-time.Hour), testNow.Add(90*24*time.Hour), testHost)
}

// leafSHA256 is the hex SHA-256 of a chain's first certificate.
func leafSHA256(t *testing.T, chain []byte) string {
	t.Helper()
	block, _ := pem.Decode(chain)
	if block == nil {
		t.Fatal("no PEM block")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

// chosen writes a file the owner would choose and returns its path.
func chosen(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.pem")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	ln.Close()
	return address
}

func echoHandler(_, origin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "served for "+origin) })
}

// openListener starts a web listener over the configuration directory dir, as adad does.
func openListener(t *testing.T, dir string, clock func() time.Time) *webListener {
	t.Helper()
	l := newWebListener(dir, clock, echoHandler, io.Discard)
	l.start()
	t.Cleanup(l.Close)
	return l
}

// handshake connects to address as a client trusting ca and returns the served leaf's SHA-256.
func handshake(t *testing.T, address string, ca *testCA) (string, error) {
	t.Helper()
	conn, err := dialListener(address, ca)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	sum := sha256.Sum256(conn.ConnectionState().PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}

func dialListener(address string, ca *testCA) (*tls.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", address, &tls.Config{RootCAs: ca.pool, ServerName: testHost, Time: func() time.Time { return testNow }})
}

// serving configures l on a free loopback address with a valid pair installed and returns the address.
func serving(t *testing.T, l *webListener, ca *testCA) (string, string) {
	t.Helper()
	cert, key := ca.valid(t)
	address := freeAddress(t)
	if _, err := l.Configure(context.Background(), api.ListenerSetting{Enabled: true, Address: address, Name: testHost}); err != nil {
		t.Fatal(err)
	}
	status, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key))
	if err != nil || status.State != "serving" || status.Reason != "" {
		t.Fatalf("install: %+v, %v", status, err)
	}
	return address, leafSHA256(t, cert)
}

// placePair writes a pair into the configuration directory as an install would, without its checks.
func placePair(t *testing.T, dir string, cert, key []byte) string {
	t.Helper()
	pairs := filepath.Join(dir, "tls", "pairs")
	if err := artifact.PrivateDirectory(pairs); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("ab", 16)
	if err := os.Mkdir(filepath.Join(pairs, id), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"cert.pem": cert, "key.pem": key} {
		if err := os.WriteFile(filepath.Join(pairs, id, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "tls", "current"), []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTheListenerIsOffAndUnsetByDefault(t *testing.T) {
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	got := l.Status()
	if got.Enabled || got.Address != "" || got.Name != "" || got.Origin != "" || got.State != "off" || got.Reason != "disabled" || got.Message == "" || got.Certificate != nil {
		t.Fatalf("status = %+v", got)
	}
}

func TestAMissingMismatchedUnparsableOrExpiredPairLeavesTheListenerOff(t *testing.T) {
	ca := newTestCA(t)
	cert, key := ca.valid(t)
	_, otherKey := ca.valid(t)
	expired, expiredKey := ca.pair(t, testNow.Add(-90*24*time.Hour), testNow.Add(-time.Hour), testHost)
	early, earlyKey := ca.pair(t, testNow.Add(time.Hour), testNow.Add(90*24*time.Hour), testHost)
	other, otherNameKey := ca.pair(t, testNow.Add(-time.Hour), testNow.Add(90*24*time.Hour), "other.adanext.si")
	for _, c := range []struct {
		name      string
		cert, key []byte
		reason    string
		described bool
	}{
		{"missing", nil, nil, "certificate_missing", false},
		{"mismatched", cert, otherKey, "certificate_invalid", true},
		{"unparsable", []byte("-----BEGIN CERTIFICATE-----\nbm90IGEgY2VydGlmaWNhdGU=\n-----END CERTIFICATE-----\n"), key, "certificate_invalid", false},
		{"not PEM", []byte("not a certificate"), key, "certificate_invalid", false},
		{"expired", expired, expiredKey, "certificate_expired", true},
		{"not yet valid", early, earlyKey, "certificate_expired", true},
		{"another name", other, otherNameKey, "certificate_name", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "config")
			address := freeAddress(t)
			l := openListener(t, dir, (&testClock{now: testNow}).Now)
			if _, err := l.Configure(context.Background(), api.ListenerSetting{Enabled: true, Address: address, Name: testHost}); err != nil {
				t.Fatal(err)
			}
			l.Close()
			if c.cert != nil {
				placePair(t, dir, c.cert, c.key)
			}
			l = openListener(t, dir, (&testClock{now: testNow}).Now)
			got := l.Status()
			if got.State != "off" || got.Reason != c.reason || got.Message == "" || strings.Contains(got.Message, "PRIVATE KEY") {
				t.Fatalf("status = %+v", got)
			}
			if c.described != (got.Certificate != nil) || (c.described && got.Certificate.SHA256 != leafSHA256(t, c.cert)) {
				t.Fatalf("certificate = %+v", got.Certificate)
			}
			if _, err := handshake(t, address, ca); err == nil {
				t.Fatal("a handshake succeeded")
			}
		})
	}
}

func TestAnInstallIsRefusedWithTheCodeOfItsFailedCheck(t *testing.T) {
	ca := newTestCA(t)
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	if _, err := l.Configure(context.Background(), api.ListenerSetting{Enabled: false, Address: "127.0.0.1:8443", Name: testHost}); err != nil {
		t.Fatal(err)
	}
	cert, key := ca.valid(t)
	_, otherKey := ca.valid(t)
	expired, expiredKey := ca.pair(t, testNow.Add(-90*24*time.Hour), testNow.Add(-time.Hour), testHost)
	other, otherNameKey := ca.pair(t, testNow.Add(-time.Hour), testNow.Add(90*24*time.Hour), "other.adanext.si")
	large := bytes.Repeat([]byte("a"), 1<<20+1)
	missing := filepath.Join(t.TempDir(), "missing.pem")
	for _, c := range []struct {
		name      string
		cert, key string
		code      string
	}{
		{"a missing certificate", missing, chosen(t, key), "certificate_invalid"},
		{"a missing key", chosen(t, cert), missing, "certificate_invalid"},
		{"a key of another pair", chosen(t, cert), chosen(t, otherKey), "certificate_invalid"},
		{"a file that is not PEM", chosen(t, []byte("not PEM")), chosen(t, key), "certificate_invalid"},
		{"a key file that is not PEM", chosen(t, cert), chosen(t, []byte("not PEM")), "certificate_invalid"},
		{"an oversized file", chosen(t, append(append([]byte{}, cert...), large...)), chosen(t, key), "certificate_invalid"},
		{"a directory", t.TempDir(), chosen(t, key), "certificate_invalid"},
		{"an expired certificate", chosen(t, expired), chosen(t, expiredKey), "certificate_expired"},
		{"another name", chosen(t, other), chosen(t, otherNameKey), "certificate_name"},
	} {
		_, err := l.InstallCertificate(context.Background(), c.cert, c.key)
		var refusal *api.ListenerError
		if !errors.As(err, &refusal) || refusal.Code != c.code || refusal.Message == "" {
			t.Fatalf("%s: %v", c.name, err)
		}
		if strings.Contains(refusal.Message, filepath.Dir(c.cert)) || strings.Contains(refusal.Message, filepath.Dir(c.key)) || strings.Contains(refusal.Message, "PRIVATE KEY") {
			t.Fatalf("%s: the message names a path or key material: %s", c.name, refusal.Message)
		}
	}
	if got := l.Status(); got.Certificate != nil {
		t.Fatalf("a refused pair was installed: %+v", got)
	}
	entries, _ := os.ReadDir(filepath.Join(l.dir, "tls", "pairs"))
	if len(entries) != 0 {
		t.Fatalf("%d pair directories left", len(entries))
	}
}

func TestAnInstallRefusesASourceThatIsALink(t *testing.T) {
	ca := newTestCA(t)
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	cert, key := ca.valid(t)
	link := filepath.Join(t.TempDir(), "link.pem")
	symlinkOrSkip(t, chosen(t, cert), link)
	var refusal *api.ListenerError
	if _, err := l.InstallCertificate(context.Background(), link, chosen(t, key)); !errors.As(err, &refusal) || refusal.Code != "certificate_invalid" {
		t.Fatalf("a linked certificate: %v", err)
	}
}

func TestAnInstalledPairIsServedAndItsSourceFilesAreLeftAlone(t *testing.T) {
	ca := newTestCA(t)
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	cert, key := ca.valid(t)
	certPath, keyPath := chosen(t, cert), chosen(t, key)
	address := freeAddress(t)
	status, err := l.Configure(context.Background(), api.ListenerSetting{Enabled: true, Address: address, Name: testHost})
	if err != nil || status.State != "off" || status.Reason != "certificate_missing" || status.Origin != "https://"+testHost+":"+address[strings.LastIndex(address, ":")+1:] {
		t.Fatalf("before a pair: %+v, %v", status, err)
	}
	status, err = l.InstallCertificate(context.Background(), certPath, keyPath)
	if err != nil || status.State != "serving" || status.Certificate == nil || status.Certificate.SHA256 != leafSHA256(t, cert) || len(status.Certificate.Names) != 1 || status.Certificate.Names[0] != testHost || status.Certificate.NotAfter != testNow.Add(90*24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("after the install: %+v, %v", status, err)
	}
	if served, err := handshake(t, address, ca); err != nil || served != leafSHA256(t, cert) {
		t.Fatalf("served %s, %v", served, err)
	}
	for path, want := range map[string][]byte{certPath: cert, keyPath: key} {
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("the source %s changed: %v", path, err)
		}
	}
}

func TestAHandshakeIsRefusedOnceTheCertificateExpires(t *testing.T) {
	ca := newTestCA(t)
	clock := &testClock{now: testNow}
	l := openListener(t, filepath.Join(t.TempDir(), "config"), clock.Now)
	address, sha := serving(t, l, ca)
	if served, err := handshake(t, address, ca); err != nil || served != sha {
		t.Fatalf("before expiry: %s, %v", served, err)
	}
	clock.Advance(90 * 24 * time.Hour)
	if _, err := handshake(t, address, ca); err == nil {
		t.Fatal("an expired certificate was served")
	}
	if got := l.Status(); got.State != "off" || got.Reason != "certificate_expired" {
		t.Fatalf("status = %+v", got)
	}
}

func TestAConnectionOpenAcrossASwapKeepsItsCertificateAndNewOnesGetTheNewPair(t *testing.T) {
	ca := newTestCA(t)
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	address, first := serving(t, l, ca)
	open, err := dialListener(address, ca)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	cert, key := ca.valid(t)
	if status, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err != nil || status.Certificate.SHA256 != leafSHA256(t, cert) {
		t.Fatalf("the swap: %+v, %v", status, err)
	}
	sum := sha256.Sum256(open.ConnectionState().PeerCertificates[0].Raw)
	if hex.EncodeToString(sum[:]) != first {
		t.Fatal("the open connection's certificate changed")
	}
	req, _ := http.NewRequest("GET", "https://"+testHost+"/", nil)
	if err := req.Write(open); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(open), req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("the open connection after the swap: %v", err)
	}
	resp.Body.Close()
	if served, err := handshake(t, address, ca); err != nil || served != leafSHA256(t, cert) {
		t.Fatalf("a new connection got %s, %v", served, err)
	}
}

func TestARestartAfterAnInstallStoppedAtEachStepEndsOnACompletePair(t *testing.T) {
	ca := newTestCA(t)
	for _, c := range []struct {
		step     string
		finished bool
	}{{"cert-written", false}, {"key-written", false}, {"checked", false}, {"previous-written", false}, {"current-written", true}} {
		t.Run(c.step, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "config")
			l := openListener(t, dir, (&testClock{now: testNow}).Now)
			address, old := serving(t, l, ca)
			cert, key := ca.valid(t)
			crash := errors.New("stopped")
			l.pairs.step = func(step string) error {
				if step == c.step {
					return crash
				}
				return nil
			}
			if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); !errors.Is(err, crash) {
				t.Fatalf("the install: %v", err)
			}
			l.Close()
			want := old
			if c.finished {
				want = leafSHA256(t, cert)
			}
			restarted := openListener(t, dir, (&testClock{now: testNow}).Now)
			if got := restarted.Status(); got.State != "serving" || got.Certificate.SHA256 != want {
				t.Fatalf("after the restart: %+v", got)
			}
			if served, err := handshake(t, address, ca); err != nil || served != want {
				t.Fatalf("served %s, %v; want %s", served, err, want)
			}
			// The next install leaves the current and previous pairs alone, the crashed one removed.
			next, nextKey := ca.valid(t)
			if _, err := restarted.InstallCertificate(context.Background(), chosen(t, next), chosen(t, nextKey)); err != nil {
				t.Fatal(err)
			}
			if entries, _ := os.ReadDir(filepath.Join(dir, "tls", "pairs")); len(entries) != 2 {
				t.Fatalf("%d pair directories", len(entries))
			}
		})
	}
}

func TestAPairFileThatIsNotOwnerOnlyIsRefused(t *testing.T) {
	ca := newTestCA(t)
	dir := filepath.Join(t.TempDir(), "config")
	l := openListener(t, dir, (&testClock{now: testNow}).Now)
	address, _ := serving(t, l, ca)
	l.Close()
	current, err := os.ReadFile(filepath.Join(dir, "tls", "current"))
	if err != nil {
		t.Fatal(err)
	}
	loosen(t, filepath.Join(dir, "tls", "pairs", strings.TrimSpace(string(current)), "key.pem"))
	restarted := openListener(t, dir, (&testClock{now: testNow}).Now)
	if got := restarted.Status(); got.State != "off" || got.Reason != "certificate_invalid" {
		t.Fatalf("status = %+v", got)
	}
	if _, err := handshake(t, address, ca); err == nil {
		t.Fatal("a handshake succeeded")
	}
}

func TestInvalidSettingsAreRefusedAndLeaveTheSavedOneAlone(t *testing.T) {
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	if _, err := l.Configure(context.Background(), api.ListenerSetting{Address: "127.0.0.1:8443", Name: testHost}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		setting api.ListenerSetting
		code    string
	}{
		{api.ListenerSetting{Enabled: true, Address: "0.0.0.0:8443", Name: testHost}, "address_invalid"},
		{api.ListenerSetting{Enabled: true, Address: "127.0.0.1:0", Name: testHost}, "address_invalid"},
		{api.ListenerSetting{Enabled: true, Address: "127.0.0.1:8443", Name: "Home"}, "name_invalid"},
		{api.ListenerSetting{Enabled: true, Address: "127.0.0.1:8443", Name: "127.0.0.1"}, "name_invalid"},
	} {
		var refusal *api.ListenerError
		if _, err := l.Configure(context.Background(), c.setting); !errors.As(err, &refusal) || refusal.Code != c.code {
			t.Fatalf("%+v: %v", c.setting, err)
		}
	}
	if got := l.Status(); got.Enabled || got.Address != "127.0.0.1:8443" || got.Name != testHost {
		t.Fatalf("status = %+v", got)
	}
}

func TestASavedSettingIsReloadedByANewListenerAtStart(t *testing.T) {
	ca := newTestCA(t)
	dir := filepath.Join(t.TempDir(), "config")
	l := openListener(t, dir, (&testClock{now: testNow}).Now)
	address, sha := serving(t, l, ca)
	l.Close()
	var saved map[string]any
	data, err := os.ReadFile(filepath.Join(dir, "listener.json"))
	if err != nil || json.Unmarshal(data, &saved) != nil || saved["enabled"] != true || saved["address"] != address || saved["name"] != testHost || len(saved) != 3 {
		t.Fatalf("listener.json = %s, %v", data, err)
	}
	restarted := openListener(t, dir, (&testClock{now: testNow}).Now)
	if got := restarted.Status(); !got.Enabled || got.Address != address || got.Name != testHost || got.State != "serving" {
		t.Fatalf("status = %+v", got)
	}
	if served, err := handshake(t, address, ca); err != nil || served != sha {
		t.Fatalf("served %s, %v", served, err)
	}
}

func TestTurningTheListenerOffRefusesNewConnections(t *testing.T) {
	ca := newTestCA(t)
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	address, _ := serving(t, l, ca)
	status, err := l.Configure(context.Background(), api.ListenerSetting{Enabled: false, Address: address, Name: testHost})
	if err != nil || status.State != "off" || status.Reason != "disabled" || status.Certificate == nil {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if conn, err := net.DialTimeout("tcp", address, 2*time.Second); err == nil {
		conn.Close()
		t.Fatal("a connection was accepted")
	}
}

func TestAnAddressInUseLeavesTheListenerOffWithListenFailed(t *testing.T) {
	ca := newTestCA(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	cert, key := ca.valid(t)
	if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err != nil {
		t.Fatal(err)
	}
	status, err := l.Configure(context.Background(), api.ListenerSetting{Enabled: true, Address: held.Addr().String(), Name: testHost})
	if err != nil || status.State != "off" || status.Reason != "listen_failed" || status.Message == "" {
		t.Fatalf("status = %+v, %v", status, err)
	}
}

func TestAPasswordAccountSignsInOverTLSAndReadsItsProjects(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateUser(ctx, ownerAccount, "Local"); err != nil {
		t.Fatal(err)
	}
	hub := api.NewHub()
	t.Cleanup(hub.Close)
	execution := api.Execution{Owner: ownerAccount}
	local := api.New(s, hub, func(*http.Request) (string, error) { return ownerAccount, nil }, execution)
	req := httptest.NewRequest("POST", "/v1/accounts", strings.NewReader(`{"name":"Vera","password_allowed":true}`))
	req.Header.Set("Idempotency-Key", "create-vera")
	created := callHandler(t, local, req)
	var account struct {
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(created, &account); err != nil || account.Code == "" {
		t.Fatalf("create an account: %s", created)
	}
	if _, err := s.Do(ctx, ownerAccount, "seed", "seed", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "INSERT INTO projects(id,name,source_path,created) VALUES('q','Quiet','/host/repo','2026-11-24T00:00:00Z'),('p','Owners','/host/other','2026-11-24T00:00:00Z')")
		return []byte("ok"), err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "q", account.Account.ID, "member"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, "p", ownerAccount, "owner"); err != nil {
		t.Fatal(err)
	}
	ca := newTestCA(t)
	cert, key := ca.pair(t, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), testHost)
	l := newWebListener(filepath.Join(t.TempDir(), "config"), time.Now, func(name, origin string) http.Handler {
		return api.NewWeb(s, hub, api.WebConfig{Name: name, Origin: origin, Owner: ownerAccount, Clock: time.Now}, execution)
	}, io.Discard)
	l.start()
	t.Cleanup(l.Close)
	address := freeAddress(t)
	if _, err := l.InstallCertificate(ctx, chosen(t, cert), chosen(t, key)); err != nil {
		t.Fatal(err)
	}
	status, err := l.Configure(ctx, api.ListenerSetting{Enabled: true, Address: address, Name: testHost})
	if err != nil || status.State != "serving" {
		t.Fatalf("status = %+v, %v", status, err)
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: ca.pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}}
	defer client.CloseIdleConnections()
	send := func(method, path, cookie, body string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, status.Origin+path, strings.NewReader(body))
		if method != "GET" {
			req.Header.Set("Origin", status.Origin)
			req.Header.Set("Content-Type", "application/json")
		}
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, data
	}
	sessionCookie := func(resp *http.Response) string {
		for _, c := range resp.Cookies() {
			if c.Value != "" {
				return c.Name + "=" + c.Value
			}
		}
		t.Fatal("no session cookie")
		return ""
	}
	resp, body := send("POST", "/v1/enrollment/password", "", `{"code":"`+account.Code+`","password":"a long enough password"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("enrollment: %d %s", resp.StatusCode, body)
	}
	resp, body = send("POST", "/v1/session/password", "", `{"account":"Vera","password":"a long enough password"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	cookie := sessionCookie(resp)
	resp, body = send("GET", "/v1/projects", cookie, "")
	var projects []map[string]any
	if err := json.Unmarshal(body, &projects); err != nil || resp.StatusCode != 200 || len(projects) != 1 || projects[0]["id"] != "q" || projects[0]["role"] != "member" {
		t.Fatalf("projects: %d %s", resp.StatusCode, body)
	}
	if resp, _ := send("GET", "/v1/projects", "", ""); resp.StatusCode != 401 {
		t.Fatalf("without the session: %d", resp.StatusCode)
	}
}

func callHandler(t *testing.T, h http.Handler, req *http.Request) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.Bytes()
}

func TestTheUIFlagDefaultsToTheApplicationDirectoryAndAMissingOneServesNothing(t *testing.T) {
	app := t.TempDir()
	saved := appDirectory
	appDirectory = func() (string, error) { return app, nil }
	t.Cleanup(func() { appDirectory = saved })
	if ui := resolveUI(""); ui != nil {
		t.Fatal("a missing default directory served files")
	}
	if err := os.MkdirAll(filepath.Join(app, "ui"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "ui", "index.html"), []byte("page"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func(dir string) (string, error) {
		root := resolveUI(dir)
		if root == nil {
			return "", fs.ErrNotExist
		}
		defer root.Close()
		data, err := fs.ReadFile(root.FS(), "index.html")
		return string(data), err
	}
	if data, err := read(""); err != nil || data != "page" {
		t.Fatalf("the default: %q, %v", data, err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "index.html"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := read(other); err != nil || data != "other" {
		t.Fatalf("-ui: %q, %v", data, err)
	}
	if resolveUI(filepath.Join(other, "missing")) != nil || resolveUI(filepath.Join(other, "index.html")) != nil {
		t.Fatal("a missing or non-directory -ui served files")
	}
}

func TestADetachedCoordinatorGetsAnAbsoluteUIDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-detach", "-state-dir", "state", "-ui", "dist"}
	opts, _, err := parseFlags(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	got, err := detachedArgs(args, opts)
	if err != nil || got[len(got)-1] != "-ui="+filepath.Join(wd, "dist") || opts.uiDir != filepath.Join(wd, "dist") {
		t.Fatalf("detachedArgs = %q, %v", got, err)
	}
}

func TestTheCoordinatorSetsUpAndServesItsNetworkListenerFromTheOwnerChannel(t *testing.T) {
	dir := ownerOnlyDir(t)
	name := testEndpointName(t)
	helper := startHelper(t, dir, name)
	defer func() {
		helper.Process.Kill()
		helper.Wait()
	}()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return ipc.Dial(ctx, ipc.Config{StateDir: dir, Name: name})
	}}}
	defer client.CloseIdleConnections()
	owner := func(method, path, body string) api.ListenerStatus {
		t.Helper()
		req, _ := http.NewRequest(method, "http://ipc"+path, strings.NewReader(body))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var status api.ListenerStatus
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s %s: %d %v", method, path, resp.StatusCode, err)
		}
		return status
	}
	if got := owner("GET", "/v1/listener", ""); got.State != "off" || got.Reason != "disabled" {
		t.Fatalf("at start: %+v", got)
	}
	address := freeAddress(t)
	if got := owner("PUT", "/v1/listener", `{"enabled":true,"address":"`+address+`","name":"`+testHost+`"}`); got.State != "off" || got.Reason != "certificate_missing" {
		t.Fatalf("without a pair: %+v", got)
	}
	ca := newTestCA(t)
	cert, key := ca.pair(t, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), testHost)
	body, _ := json.Marshal(map[string]string{"certificate": chosen(t, cert), "key": chosen(t, key)})
	status := owner("PUT", "/v1/listener/certificate", string(body))
	if status.State != "serving" {
		t.Fatalf("after the install: %+v", status)
	}
	web := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: ca.pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}}
	defer web.CloseIdleConnections()
	for path, want := range map[string]int{"/v1/projects": 401, "/": 404} {
		resp, err := web.Get(status.Origin + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want || resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("GET %s over TLS: %d %v", path, resp.StatusCode, resp.Header)
		}
	}
	conn, err := tls.Dial("tcp", address, &tls.Config{RootCAs: ca.pool, ServerName: testHost})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "OPTIONS * HTTP/1.1\r\nHost: "+strings.TrimPrefix(status.Origin, "https://")+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("OPTIONS * over TLS: %d %v", resp.StatusCode, resp.Header)
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "listener.json")); err != nil {
		t.Fatal(err)
	}
}

func TestAChannelThatCannotBeBuiltLeavesTheListenerOffInsteadOfStoppingAdad(t *testing.T) {
	ca := newTestCA(t)
	dir := filepath.Join(t.TempDir(), "config")
	l := openListener(t, dir, (&testClock{now: testNow}).Now)
	address, _ := serving(t, l, ca)
	l.Close()
	broken := newWebListener(dir, (&testClock{now: testNow}).Now, func(string, string) http.Handler { panic("refused") }, io.Discard)
	broken.start()
	t.Cleanup(broken.Close)
	if got := broken.Status(); got.State != "off" || got.Reason != "listen_failed" {
		t.Fatalf("status = %+v", got)
	}
	if _, err := handshake(t, address, ca); err == nil {
		t.Fatal("a handshake succeeded")
	}
}
