package ipc

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const handshakeTimeout = 5 * time.Second
const endpointDomain = "adanext-endpoint-v1"

// A new key is published atomically under the singleton lock, before listening.
func newEndpointToken(dir string) ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := createTokenFile(dir)
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(key)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, "adad.token")); err != nil {
		return nil, err
	}
	return key, nil
}

func endpointMAC(key, nonce []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(endpointDomain))
	h.Write(nonce)
	return h.Sum(nil)
}

// Read exactly a bounded line, without consuming any subsequent HTTP bytes.
func authLine(c net.Conn, limit int) (string, error) {
	b := make([]byte, 0, limit)
	for len(b) < limit {
		var one [1]byte
		if _, err := io.ReadFull(c, one[:]); err != nil {
			return "", err
		}
		b = append(b, one[0])
		if one[0] == '\n' {
			return string(b), nil
		}
	}
	return "", errors.New("invalid handshake")
}

// Dial retains the platform's peer checks, then authenticates the endpoint on
// every connection. A failed proof never permits HTTP or a fallback endpoint.
func Dial(ctx context.Context, cfg Config) (net.Conn, error) {
	c, err := dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	fail := func() (net.Conn, error) { c.Close(); return nil, fmt.Errorf("ipc: endpoint authentication failed") }
	key, err := os.ReadFile(filepath.Join(cfg.StateDir, "adad.token"))
	if err != nil || len(key) != 32 {
		return fail()
	}
	deadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = c.SetDeadline(deadline); err != nil {
		return fail()
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return fail()
	}
	if _, err = io.WriteString(c, "ADANEXT1 "+hex.EncodeToString(nonce)+"\n"); err != nil {
		return fail()
	}
	line, err := authLine(c, 65)
	if err != nil {
		return fail()
	}
	proof, err := hex.DecodeString(strings.TrimSuffix(line, "\n"))
	if err != nil || !hmac.Equal(proof, endpointMAC(key, nonce)) {
		return fail()
	}
	if err = c.SetDeadline(time.Time{}); err != nil {
		return fail()
	}
	return c, nil
}

type authenticatedListener struct {
	net.Listener
	key     []byte
	ready   chan net.Conn
	done    chan struct{}
	loopEnd chan struct{}
	once    sync.Once
	mu      sync.Mutex
	pending map[net.Conn]bool
	err     error
}

func authenticateListener(l net.Listener, key []byte) *authenticatedListener {
	a := &authenticatedListener{Listener: l, key: key, ready: make(chan net.Conn), done: make(chan struct{}), loopEnd: make(chan struct{}), pending: make(map[net.Conn]bool)}
	go a.acceptLoop()
	return a
}
func (a *authenticatedListener) acceptLoop() {
	defer close(a.loopEnd)
	for {
		c, err := a.Listener.Accept()
		if err != nil {
			a.mu.Lock()
			a.err = err
			a.mu.Unlock()
			a.shutdown()
			return
		}
		a.mu.Lock()
		select {
		case <-a.done:
			a.mu.Unlock()
			c.Close()
			return
		default:
		}
		// Bound both handshake goroutines and sockets waiting for HTTP to accept.
		if len(a.pending) >= 64 {
			a.mu.Unlock()
			c.Close()
			continue
		}
		a.pending[c] = true
		a.mu.Unlock()
		go a.handshake(c)
	}
}
func (a *authenticatedListener) handshake(c net.Conn) {
	handed := false
	defer func() {
		a.mu.Lock()
		delete(a.pending, c)
		a.mu.Unlock()
		if !handed {
			c.Close()
		}
	}()
	if err := c.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return
	}
	line, err := authLine(c, 74)
	if err != nil || !strings.HasPrefix(line, "ADANEXT1 ") || len(line) != 74 {
		return
	}
	nonce, err := hex.DecodeString(line[9:73])
	if err != nil || len(nonce) != 32 {
		return
	}
	if _, err = io.WriteString(c, hex.EncodeToString(endpointMAC(a.key, nonce))+"\n"); err != nil {
		return
	}
	if err = c.SetDeadline(time.Time{}); err != nil {
		return
	}
	select {
	case a.ready <- c:
		handed = true
	case <-a.done:
	}
}
func (a *authenticatedListener) Accept() (net.Conn, error) {
	select {
	case c := <-a.ready:
		return c, nil
	case <-a.done:
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.err != nil {
			return nil, a.err
		}
		return nil, net.ErrClosed
	}
}

// Close returns only after the accept loop has exited, so no listener
// goroutine outlives it.
func (a *authenticatedListener) Close() error {
	err := a.shutdown()
	<-a.loopEnd
	return err
}

// shutdown stops accepting without waiting; the accept loop calls it itself.
func (a *authenticatedListener) shutdown() error {
	var err error
	a.once.Do(func() {
		close(a.done)
		err = a.Listener.Close()
		a.mu.Lock()
		defer a.mu.Unlock()
		for c := range a.pending {
			c.Close()
		}
	})
	return err
}
