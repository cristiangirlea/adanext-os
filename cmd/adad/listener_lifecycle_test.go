package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/api"
)

// resumable dials with a session cache and reads one answer, so a ticket the server sends is kept.
func resumable(address string, ca *testCA, cache tls.ClientSessionCache) (string, error) {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", address, &tls.Config{RootCAs: ca.pool, ServerName: testHost, Time: func() time.Time { return testNow }, ClientSessionCache: cache})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	req, _ := http.NewRequest("GET", "https://"+testHost+"/", nil)
	if err := req.Write(conn); err != nil {
		return "", err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return "", err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	sum := sha256.Sum256(conn.ConnectionState().PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}

func TestASessionCacheCannotKeepAnOldPairAfterASwapOrExpiry(t *testing.T) {
	ca := newTestCA(t)
	clock := &testClock{now: testNow}
	l := openListener(t, filepath.Join(t.TempDir(), "config"), clock.Now)
	address, first := serving(t, l, ca)
	cache := tls.NewLRUClientSessionCache(8)
	if served, err := resumable(address, ca, cache); err != nil || served != first {
		t.Fatalf("first: %s, %v", served, err)
	}
	cert, key := ca.valid(t)
	if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err != nil {
		t.Fatal(err)
	}
	if served, err := resumable(address, ca, cache); err != nil || served != leafSHA256(t, cert) {
		t.Fatalf("after the swap: %s, %v", served, err)
	}
	clock.Advance(90 * 24 * time.Hour)
	if _, err := resumable(address, ca, cache); err == nil {
		t.Fatal("a cached session outlived the certificate")
	}
}

func TestOptionsStarReachesTheChannelThroughTheListenersServer(t *testing.T) {
	ca := newTestCA(t)
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	address, _ := serving(t, l, ca)
	conn, err := dialListener(address, ca)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "OPTIONS * HTTP/1.1\r\nHost: "+testHost+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(body), "served for ") {
		t.Fatalf("OPTIONS * was answered by the server itself: %d %q", resp.StatusCode, body)
	}
}

func TestACorruptSettingFileLeavesTheListenerOffAndAsksForTheSettingAgain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	openListener(t, dir, (&testClock{now: testNow}).Now).Close()
	if err := os.WriteFile(filepath.Join(dir, "listener.json"), []byte(`{"enabled":tru`), 0o600); err != nil {
		t.Fatal(err)
	}
	l := openListener(t, dir, (&testClock{now: testNow}).Now)
	if got := l.Status(); got.State != "off" || got.Reason != "not_configured" || !strings.Contains(got.Message, "setting could not be read") {
		t.Fatalf("status = %+v", got)
	}
	if _, err := l.Configure(context.Background(), api.ListenerSetting{Address: "127.0.0.1:8443", Name: testHost}); err != nil {
		t.Fatal(err)
	}
	if got := l.Status(); got.Reason != "disabled" {
		t.Fatalf("after a save: %+v", got)
	}
}

func TestChangesAfterCloseAreRefused(t *testing.T) {
	ca := newTestCA(t)
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	l.Close()
	if _, err := l.Configure(context.Background(), api.ListenerSetting{Enabled: true, Address: freeAddress(t), Name: testHost}); !errors.Is(err, api.ErrListenerStopped) {
		t.Fatalf("configure: %v", err)
	}
	cert, key := ca.valid(t)
	if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); !errors.Is(err, api.ErrListenerStopped) {
		t.Fatalf("install: %v", err)
	}
}

func TestReconfiguringToTheSameAddressTwiceServesEachTime(t *testing.T) {
	ca := newTestCA(t)
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	address, sha := serving(t, l, ca)
	for i := range 2 {
		status, err := l.Configure(context.Background(), api.ListenerSetting{Enabled: true, Address: address, Name: testHost})
		if err != nil || status.State != "serving" {
			t.Fatalf("reconfigure %d: %+v, %v", i, status, err)
		}
		if served, err := handshake(t, address, ca); err != nil || served != sha {
			t.Fatalf("reconfigure %d: served %s, %v", i, served, err)
		}
	}
}

func TestAServeLoopThatEndsOnAnErrorIsShownAsListenFailed(t *testing.T) {
	ca := newTestCA(t)
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	serving(t, l, ca)
	l.mu.Lock()
	l.ln.Close()
	l.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for l.Status().Reason != "listen_failed" {
		if time.Now().After(deadline) {
			t.Fatalf("status = %+v", l.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTheUIIsReadThroughARootThatNoLinkLeaves(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.js")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("page"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(dir, "linked.js"))
	root := resolveUI(dir)
	if root == nil {
		t.Fatal("no UI")
	}
	defer root.Close()
	if data, err := fs.ReadFile(root.FS(), "index.html"); err != nil || string(data) != "page" {
		t.Fatalf("index.html: %q, %v", data, err)
	}
	if data, err := fs.ReadFile(root.FS(), "linked.js"); err == nil {
		t.Fatalf("a link out of the UI was read: %q", data)
	}
}
