package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/api"
)

func TestAnIncompleteOrTrailingSettingFileAsksForTheSettingAgain(t *testing.T) {
	address := freeAddress(t)
	for name, content := range map[string]string{
		"trailing garbage": `{"enabled":true,"address":"` + address + `","name":"` + testHost + `"} trailing`,
		"an empty object":  `{}`,
		"null":             `null`,
		"a missing field":  `{"enabled":true,"address":"` + address + `"}`,
		"a null field":     `{"enabled":true,"address":"` + address + `","name":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			ca := newTestCA(t)
			dir := filepath.Join(t.TempDir(), "config")
			l := openListener(t, dir, (&testClock{now: testNow}).Now)
			cert, key := ca.valid(t)
			if _, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key)); err != nil {
				t.Fatal(err)
			}
			l.Close()
			if err := os.WriteFile(filepath.Join(dir, "listener.json"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			restarted := openListener(t, dir, (&testClock{now: testNow}).Now)
			if got := restarted.Status(); got.State != "off" || got.Reason != "not_configured" || !strings.Contains(got.Message, "setting could not be read") {
				t.Fatalf("status = %+v", got)
			}
			if _, err := handshake(t, address, ca); err == nil {
				t.Fatal("the listener started")
			}
		})
	}
}

func TestConfigTLSAndPairsAreSyncedBeforeTheCertificateOnAFirstInstallAndItsRetry(t *testing.T) {
	ca := newTestCA(t)
	dir := filepath.Join(t.TempDir(), "config")
	l := openListener(t, dir, (&testClock{now: testNow}).Now)
	saved := syncDir
	t.Cleanup(func() { syncDir = saved })
	var order []string
	syncDir = func(path string) error {
		order = append(order, "sync "+path)
		return saved(path)
	}
	crash := errors.New("stopped")
	stop := false
	l.pairs.step = func(step string) error {
		order = append(order, step)
		if step == "cert-written" && stop {
			return crash
		}
		return nil
	}
	cert, key := ca.valid(t)
	attempt := func(stopped bool) []string {
		order, stop = nil, stopped
		_, err := l.InstallCertificate(context.Background(), chosen(t, cert), chosen(t, key))
		if stopped != errors.Is(err, crash) || (!stopped && err != nil) {
			t.Fatalf("the install: %v", err)
		}
		return order
	}
	for name, got := range map[string][]string{"the first attempt": attempt(true), "its retry": attempt(false)} {
		at := slices.Index(got, "cert-written")
		if at < 0 {
			t.Fatalf("%s: no cert-written in %q", name, got)
		}
		for _, want := range []string{dir, filepath.Join(dir, "tls"), filepath.Join(dir, "tls", "pairs")} {
			if !slices.Contains(got[:at], "sync "+want) {
				t.Fatalf("%s: %s was not synced before the certificate: %q", name, want, got)
			}
		}
	}
}

func TestAStoppedListenerRefusesBeforeCheckingWhatItIsGiven(t *testing.T) {
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	l.Close()
	if _, err := l.Configure(context.Background(), api.ListenerSetting{Enabled: true, Address: "0.0.0.0:1", Name: "Home"}); !errors.Is(err, api.ErrListenerStopped) {
		t.Fatalf("an invalid setting: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.pem")
	if _, err := l.InstallCertificate(context.Background(), missing, missing); !errors.Is(err, api.ErrListenerStopped) {
		t.Fatalf("a missing certificate: %v", err)
	}
}

// reencode writes a chain with CRLF line endings, a comment before and between its blocks, and a
// header on each block.
func reencode(t *testing.T, chain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString("Issued for the test.\n-----BEGINNING of the chain-----\n")
	for rest := chain; ; {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		block.Headers = map[string]string{"Comment": "see -----BEGINNING above"}
		out.Write(pem.EncodeToMemory(block))
		out.WriteString("a note between blocks\n")
		rest = next
	}
	text := strings.TrimSuffix(out.String(), "a note between blocks\n")
	return []byte(strings.ReplaceAll(text, "\n", "\r\n"))
}

func TestAChainWithCRLFCommentsAndHeadersIsAccepted(t *testing.T) {
	ca := newTestCA(t)
	l := openListener(t, filepath.Join(t.TempDir(), "config"), (&testClock{now: testNow}).Now)
	cert, key := ca.valid(t)
	status, err := l.InstallCertificate(context.Background(), chosen(t, reencode(t, cert)), chosen(t, key))
	if err != nil || status.Certificate == nil || status.Certificate.SHA256 != leafSHA256(t, cert) {
		t.Fatalf("status = %+v, %v", status, err)
	}
}
