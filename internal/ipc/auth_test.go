package ipc

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEndpointTokenIsPrivateAndReplacedOnRestart(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "token-rotation"}
	var previous []byte
	for range 2 {
		lock, err := Acquire(cfg)
		if err != nil {
			t.Fatal(err)
		}
		l, err := lock.Listen()
		if err != nil {
			lock.Release()
			t.Fatal(err)
		}
		p := filepath.Join(cfg.StateDir, "adad.token")
		key, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(key) != 32 {
			t.Fatal("token must be 32 bytes")
		}
		if previous != nil && string(key) == string(previous) {
			t.Fatal("token was reused")
		}
		previous = key
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatal("token is not owner-only")
		}
		l.Close()
		lock.Release()
	}
}

func TestDialRefusesAnEndpointWithTheWrongToken(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "wrong-token"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	l, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, e := l.Accept()
		if e == nil {
			c.Close()
		}
	}()
	// Replace the client's copy; the listener must retain its original key.
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "adad.token"), make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Dial(ctx, cfg)
	if c != nil {
		c.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "endpoint authentication failed") {
		t.Fatalf("got %v", err)
	}
}

func TestListenerClosesMalformedAndSilentClientsBeforeHTTP(t *testing.T) {
	for _, input := range []string{"garbage\n", ""} {
		t.Run(input, func(t *testing.T) {
			cfg := Config{StateDir: t.TempDir(), Name: "bad-client"}
			lock, err := Acquire(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Release()
			l, err := lock.Listen()
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			go func() {
				c, e := l.Accept()
				if e == nil {
					c.Close()
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c, err := dial(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			start := time.Now()
			c.SetDeadline(start.Add(6 * time.Second))
			if input != "" {
				io.WriteString(c, input)
			}
			var b [1]byte
			_, err = c.Read(b[:])
			if err == nil {
				t.Fatal("client was not closed")
			}
			if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatalf("connection was not closed: %v", err)
			}
			if time.Since(start) > 5500*time.Millisecond {
				t.Fatal("handshake exceeded deadline")
			}
		})
	}
}

func TestHTTPWorksAfterEndpointAuthentication(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), Name: "http-auth"}
	lock, err := Acquire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	l, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "authenticated") })}
	defer server.Close()
	go server.Serve(l)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return Dial(ctx, cfg) }}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	res, err := client.Get("http://adad/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil || string(b) != "authenticated" {
		t.Fatalf("HTTP: %q %v", b, err)
	}
}
