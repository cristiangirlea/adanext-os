package ipc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	request := "GET / HTTP/1.1\r\nHost: adad\r\n\r\n"
	for name, input := range map[string]string{"raw request": request, "garbage": "garbage\n" + request, "silent": ""} {
		t.Run(name, func(t *testing.T) {
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
			// A real server: only the listener's handshake stands between a raw request and an answer.
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "answered") })}
			defer server.Close()
			go server.Serve(l)
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
			got, err := io.ReadAll(c)
			if len(got) != 0 {
				t.Fatalf("a client without the handshake got %q", got)
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

// slowCloseListener's Accept returns some time after Close, like a real
// listener whose blocked accept is still unwinding.
type slowCloseListener struct {
	net.Listener
	once   sync.Once
	closed chan struct{}
}

func (l *slowCloseListener) Accept() (net.Conn, error) {
	<-l.closed
	time.Sleep(50 * time.Millisecond)
	return nil, net.ErrClosed
}

func (l *slowCloseListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func TestCloseReturnsOnlyAfterTheAcceptLoopHasExited(t *testing.T) {
	a := authenticateListener(&slowCloseListener{closed: make(chan struct{})}, make([]byte, 32))
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.loopEnd:
	default:
		t.Fatal("Close returned while the accept loop was still running")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

// flakyListener fails its first accepts the way a listener out of descriptors
// does, then hands over the connections it is given.
type flakyListener struct {
	net.Listener
	failures int
	conns    chan net.Conn
	closed   chan struct{}
	once     sync.Once
	mu       sync.Mutex
	calls    []time.Time
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.calls = append(l.calls, time.Now())
	fail := len(l.calls) <= l.failures
	l.mu.Unlock()
	if fail {
		return nil, errors.New("accept: too many open files")
	}
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *flakyListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func TestListenerRetriesAFailedAcceptWithBackoffAndKeepsServing(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	flaky := &flakyListener{failures: 3, conns: make(chan net.Conn, 1), closed: make(chan struct{})}
	a := authenticateListener(flaky, key)
	defer a.Close()
	server, client := net.Pipe()
	defer client.Close()
	flaky.conns <- server
	proved := make(chan error, 1)
	go func() {
		nonce := make([]byte, 32)
		rand.Read(nonce)
		if _, err := io.WriteString(client, "ADANEXT1 "+hex.EncodeToString(nonce)+"\n"); err != nil {
			proved <- err
			return
		}
		line := make([]byte, 65)
		if _, err := io.ReadFull(client, line); err != nil {
			proved <- err
			return
		}
		if string(line[:64]) != hex.EncodeToString(endpointMAC(key, nonce)) {
			proved <- errors.New("wrong proof")
			return
		}
		proved <- nil
	}()
	accepted := make(chan error, 1)
	go func() {
		c, err := a.Accept()
		if c != nil {
			c.Close()
		}
		accepted <- err
	}()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("a failed accept ended the listener: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the listener did not hand over the connection after failed accepts")
	}
	if err := <-proved; err != nil {
		t.Fatal(err)
	}
	flaky.mu.Lock()
	calls := append([]time.Time(nil), flaky.calls...)
	flaky.mu.Unlock()
	for i := 1; i < 4; i++ {
		if gap := calls[i].Sub(calls[i-1]); gap < 4*time.Millisecond {
			t.Fatalf("accept %d retried after %v, without backing off", i, gap)
		}
	}
}

func TestCloseDuringAcceptBackoffReturnsPromptlyAsClosed(t *testing.T) {
	flaky := &flakyListener{failures: 1 << 30, conns: make(chan net.Conn), closed: make(chan struct{})}
	a := authenticateListener(flaky, make([]byte, 32))
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Close waited for the accept backoff")
	}
	if _, err := a.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close = %v", err)
	}
}
