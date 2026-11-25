package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cristiangirlea/ada-os/internal/api"
	"github.com/cristiangirlea/ada-os/internal/artifact"
)

// webListener is the TLS listener of the network channel: off by default, set from the owner's
// channel, serving only with a checked pair from adad's configuration directory.
type webListener struct {
	dir     string                                 // the configuration directory
	clock   func() time.Time                       // the coordinator's clock
	handler func(name, origin string) http.Handler // the network channel for a name and origin
	log     io.Writer                              // the serve loop's errors
	pairs   *pairStore                             // the installed pairs
	pair    atomic.Pointer[tlsPair]                // what GetCertificate serves
	mu      sync.Mutex                             // serialises the owner's changes and the stop
	setting api.ListenerSetting                    // the saved setting
	shown   *api.ListenerCertificate               // the installed pair as the owner sees it
	failure *api.ListenerError                     // why the listener is off despite its setting
	server  *http.Server                           // the running listener's server; nil when off
	ln      net.Listener                           // the running listener's socket
	built   struct {
		origin string
		h      http.Handler
	} // the handler of the last origin served
	closed     bool // the coordinator is stopping
	settingBad bool // the saved setting exists but could not be read
	// startFailed is why the start could not even read the setting; nil once a save succeeds.
	startFailed *api.ListenerError
}

func newWebListener(dir string, clock func() time.Time, handler func(name, origin string) http.Handler, log io.Writer) *webListener {
	return &webListener{dir: dir, clock: clock, handler: handler, log: log, pairs: &pairStore{dir: filepath.Join(dir, "tls"), log: log}}
}

// start makes the configuration directory private, reads the setting and starts the listener
// when the setting and a pair allow it. A failure is recorded in the status, never returned.
func (l *webListener) start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := artifact.PrivateDirectory(l.dir); err != nil {
		l.startFailed = &api.ListenerError{Code: "certificate_missing", Message: "adad's configuration directory could not be made owner-only: " + err.Error()}
		return
	}
	setting, err := l.loadSetting()
	if err != nil {
		fmt.Fprintln(l.log, "adad: network listener setting:", err)
		l.settingBad = true
	}
	l.setting = setting
	l.evaluateLocked()
}

func (l *webListener) settingPath() string { return filepath.Join(l.dir, "listener.json") }

type settingJSON struct {
	Enabled bool   `json:"enabled"`
	Address string `json:"address"`
	Name    string `json:"name"`
}

// loadSetting reads the saved setting; a missing file is the default, off and unset.
func (l *webListener) loadSetting() (api.ListenerSetting, error) {
	data, err := readPrivate(l.settingPath(), maxSmallBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return api.ListenerSetting{}, nil
	}
	if err != nil {
		return api.ListenerSetting{}, err
	}
	// Every field present and nothing after the object: anything less is not a saved setting.
	var saved struct {
		Enabled *bool   `json:"enabled"`
		Address *string `json:"address"`
		Name    *string `json:"name"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&saved); err != nil {
		return api.ListenerSetting{}, fmt.Errorf("listener.json: %w", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return api.ListenerSetting{}, errors.New("listener.json: data after the setting")
	}
	if saved.Enabled == nil || saved.Address == nil || saved.Name == nil {
		return api.ListenerSetting{}, errors.New("listener.json: a field is missing")
	}
	setting := api.ListenerSetting{Enabled: *saved.Enabled, Address: *saved.Address, Name: *saved.Name}
	if err := api.ValidateListenerSetting(setting); err != nil {
		return api.ListenerSetting{}, fmt.Errorf("listener.json: %w", err)
	}
	return setting, nil
}

func (l *webListener) Status() api.ListenerStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.statusLocked()
}

func (l *webListener) statusLocked() api.ListenerStatus {
	s := l.setting
	status := api.ListenerStatus{Enabled: s.Enabled, Address: s.Address, Name: s.Name, Origin: api.ListenerOrigin(s.Name, s.Address), State: "off", Certificate: l.shown}
	off := func(reason, message string) api.ListenerStatus {
		status.Reason, status.Message = reason, message
		return status
	}
	switch {
	case l.startFailed != nil:
		return off(l.startFailed.Code, l.startFailed.Message)
	case l.settingBad:
		return off("not_configured", "The saved listener setting could not be read; set the address and name and save it again.")
	case !s.Enabled:
		return off("disabled", "The network listener is turned off.")
	case s.Address == "" || s.Name == "":
		return off("not_configured", "Set the listener's address and name; it starts once both are set and a certificate is installed.")
	case l.failure != nil:
		return off(l.failure.Code, l.failure.Message)
	case l.server == nil:
		return off("listen_failed", "The network listener stopped with the coordinator.")
	}
	p := l.pair.Load()
	if p == nil {
		return off(noPair.Code, noPair.Message)
	}
	// Judged now: a certificate that expired since the start has its handshakes refused.
	if fail := p.check(s.Name, l.clock()); fail != nil {
		return off(fail.Code, fail.Message)
	}
	status.State, status.Message = "serving", "The network listener is serving at "+status.Origin+"."
	return status
}

func (l *webListener) Configure(_ context.Context, s api.ListenerSetting) (api.ListenerStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A stopped listener refuses first, whatever it is given.
	if l.closed {
		return api.ListenerStatus{}, api.ErrListenerStopped
	}
	if err := api.ValidateListenerSetting(s); err != nil {
		return api.ListenerStatus{}, err
	}
	data, err := json.Marshal(settingJSON{s.Enabled, s.Address, s.Name})
	if err != nil {
		return api.ListenerStatus{}, err
	}
	if err := artifact.PrivateDirectory(l.dir); err != nil {
		return api.ListenerStatus{}, err
	}
	if err := writeAtomic(l.dir, "listener.json", data); err != nil {
		return api.ListenerStatus{}, err
	}
	l.setting, l.settingBad, l.startFailed = s, false, nil
	l.evaluateLocked()
	return l.statusLocked(), nil
}

func (l *webListener) InstallCertificate(_ context.Context, certPath, keyPath string) (api.ListenerStatus, error) {
	// Refused before any source is read; checked again under the lock before anything is written.
	if l.stopped() {
		return api.ListenerStatus{}, api.ErrListenerStopped
	}
	certPEM, err := readSource(certPath, "certificate")
	if err != nil {
		return api.ListenerStatus{}, err
	}
	keyPEM, err := readSource(keyPath, "key")
	if err != nil {
		return api.ListenerStatus{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return api.ListenerStatus{}, api.ErrListenerStopped
	}
	if _, _, fail := checkPair(certPEM, keyPEM, l.setting.Name, l.clock()); fail != nil {
		return api.ListenerStatus{}, fail
	}
	p, err := l.pairs.install(certPEM, keyPEM, l.setting.Name, l.clock())
	if err != nil {
		return api.ListenerStatus{}, err
	}
	if l.server != nil && l.failure == nil && p.check(l.setting.Name, l.clock()) == nil {
		// Only the pointer changes: open connections keep their pair, new handshakes get this one.
		l.pair.Store(p)
		l.shown = &p.info
	} else {
		l.evaluateLocked()
	}
	l.pairs.removeStale()
	return l.statusLocked(), nil
}

// evaluateLocked stops the running listener, loads the pair again and starts a listener when the
// setting and the pair allow it.
func (l *webListener) evaluateLocked() {
	l.stopLocked()
	p, shown, fail := l.pairs.load(l.setting.Name, l.clock())
	l.pair.Store(p)
	l.shown, l.failure = shown, fail
	s := l.setting
	if l.closed || !s.Enabled || s.Address == "" || s.Name == "" || fail != nil {
		return
	}
	origin := api.ListenerOrigin(s.Name, s.Address)
	if l.built.origin != origin || l.built.h == nil {
		h, err := buildHandler(l.handler, s.Name, origin)
		if err != nil {
			l.failure = &api.ListenerError{Code: "listen_failed", Message: fmt.Sprintf("The network channel could not be set up for %s (%v).", origin, err)}
			return
		}
		l.built.origin, l.built.h = origin, h
	}
	ln, err := net.Listen("tcp", s.Address)
	if err != nil {
		l.failure = &api.ListenerError{Code: "listen_failed", Message: fmt.Sprintf("The listener cannot use %s (%v); check that the address is this computer's and not in use.", s.Address, err)}
		return
	}
	// OPTIONS * goes to the channel, which refuses it, rather than the server's own answer.
	server := &http.Server{Handler: l.built.h, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 1 << 20, IdleTimeout: 60 * time.Second, ErrorLog: log.New(l.log, "adad: network listener: ", 0), DisableGeneralOptionsHandler: true}
	l.server, l.ln = server, ln
	// No tickets: every new connection meets GetCertificate, so a swap and expiry apply to it.
	config := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: l.getCertificate, SessionTicketsDisabled: true}
	go func() {
		err := server.Serve(tls.NewListener(ln, config))
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		// Only this listener's own end is recorded; a newer one is not affected.
		if l.server == server {
			l.failure = &api.ListenerError{Code: "listen_failed", Message: fmt.Sprintf("The network listener stopped (%v); save the setting again to restart it.", err)}
		}
	}()
}

// buildHandler is build's handler, or its panic as an error: a refused configuration must not
// stop adad's start.
func buildHandler(build func(name, origin string) http.Handler, name, origin string) (h http.Handler, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return build(name, origin), nil
}

// getCertificate serves the current pair, and refuses the handshake outside its validity.
func (l *webListener) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	p := l.pair.Load()
	if p == nil {
		return nil, errors.New("no certificate is installed")
	}
	if !p.validAt(l.clock()) {
		return nil, errors.New("the certificate is not valid now")
	}
	return &p.cert, nil
}

// stopLocked ends the running listener: its requests get two seconds, then every connection,
// an open event stream's included, is closed.
func (l *webListener) stopLocked() {
	if l.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = l.server.Shutdown(ctx)
	_ = l.server.Close()
	// Closed here too, in case Serve has not taken it yet, so the address can be bound again at once.
	_ = l.ln.Close()
	l.server, l.ln = nil, nil
}

func (l *webListener) stopped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// Close stops the listener for good, as the coordinator stops.
func (l *webListener) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	l.stopLocked()
}
