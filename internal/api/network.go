package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/cristiangirlea/ada-os/internal/store"
)

// NetworkConfig is the network channel's relying party: RPID such as home.adanext.si and
// Origin such as https://home.adanext.si; Owner, the coordinator's owner account, which a
// session reads as owner. Clock nil means time.Now.
type NetworkConfig struct {
	RPID   string
	Origin string
	Owner  string
	Clock  func() time.Time
}

// Session lifetimes: idle from the last use, absolute from creation; last use is written at most
// once a minute.
const (
	sessionIdle     = 8 * time.Hour
	sessionAbsolute = 7 * 24 * time.Hour
	sessionTouch    = time.Minute
)

type sessionKey struct{}

// sessionFrom is the network session a request was authenticated with.
func sessionFrom(ctx context.Context) (store.Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(store.Session)
	return s, ok
}

type network struct {
	h          *handler
	rp         *webauthn.WebAuthn
	ceremonies *ceremonies
	hasher     *hasher
	clock      func() time.Time
	open       *http.ServeMux                // the routes that need no session
	session    *http.ServeMux                // the routes of a signed-in account
	owner      *http.ServeMux                // the owner channel's routes, refused here with local_only
	pause      func(context.Context, string) // test seam: called at the named point of a request; nil in service
	shutdown   context.Context               // ends when the hub drains or closes
}

// at calls the test seam at a named point of a request.
func (n *network) at(ctx context.Context, point string) {
	if n.pause != nil {
		n.pause(ctx, point)
	}
}

// NewNetwork serves the network channel: sign-in, enrollment with a code, and the signed-in
// account's own sessions and credentials. The actor of every other route is the session's
// account. hub must be the local channel's, so the owner's revocations reach open streams and
// recovery clears the sign-in throttles. It panics without an owner account or on a
// configuration WebAuthn refuses.
func NewNetwork(s *store.Store, hub *Hub, cfg NetworkConfig, execution ...Execution) http.Handler {
	n, err := newNetwork(s, hub, cfg, execution...)
	if err != nil {
		panic(fmt.Sprintf("api: network channel: %v", err))
	}
	return withRequestID(n)
}

func newNetwork(s *store.Store, hub *Hub, cfg NetworkConfig, execution ...Execution) (*network, error) {
	if hub == nil {
		return nil, errors.New("a hub is required")
	}
	if cfg.Owner == "" {
		return nil, errors.New("the owner account is required")
	}
	rp, err := newRelyingParty(cfg)
	if err != nil {
		return nil, err
	}
	n := &network{rp: rp, ceremonies: newCeremonies(), hasher: newHasher(), clock: cfg.Clock, open: http.NewServeMux(), session: http.NewServeMux(), owner: http.NewServeMux()}
	if n.clock == nil {
		n.clock = time.Now
	}
	n.h = newHandler(s, hub, func(r *http.Request) (string, error) {
		if sess, ok := sessionFrom(r.Context()); ok {
			return sess.User, nil
		}
		return "", errors.New("no network session")
	}, execution...)
	n.h.exec.Owner = cfg.Owner
	n.h.net = n
	hub.mu.Lock()
	hub.channelsLocked()
	done, draining := hub.done, hub.draining
	hub.mu.Unlock()
	var stop context.CancelFunc
	n.shutdown, stop = context.WithCancel(context.Background())
	go func() {
		defer stop()
		select {
		case <-done:
		case <-draining:
		}
	}()
	n.open.HandleFunc("POST /v1/session/passkey/options", n.loginOptions)
	n.open.HandleFunc("POST /v1/session/passkey", n.loginPasskey)
	n.open.HandleFunc("POST /v1/session/password", n.loginPassword)
	n.open.HandleFunc("POST /v1/enrollment/options", n.enrollmentOptions)
	n.open.HandleFunc("POST /v1/enrollment/passkey", n.enrollPasskey)
	n.open.HandleFunc("POST /v1/enrollment/password", n.enrollPassword)
	n.session.HandleFunc("POST /v1/session/logout", n.logout)
	n.session.HandleFunc("GET /v1/account", n.account)
	n.session.HandleFunc("GET /v1/account/sessions", n.accountSessions)
	n.session.HandleFunc("DELETE /v1/account/sessions/{id}", n.revokeOwnSession)
	n.session.HandleFunc("GET /v1/account/credentials", n.accountCredentials)
	n.session.HandleFunc("DELETE /v1/account/credentials/{id}", n.removeOwnCredential)
	for pattern := range n.h.ownerRoutes() {
		n.owner.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) { writeError(w, 403, "local_only") })
	}
	return n, nil
}

func (n *network) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, pattern := n.open.Handler(r); pattern != "" {
		n.open.ServeHTTP(w, r)
		return
	}
	sess, ok, err := n.authenticate(r)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !ok {
		writeError(w, 401, "unauthenticated")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess))
	if _, pattern := n.session.Handler(r); pattern != "" {
		n.session.ServeHTTP(w, r)
		return
	}
	if _, pattern := n.owner.Handler(r); pattern != "" {
		n.owner.ServeHTTP(w, r)
		return
	}
	writeError(w, 404, "not_found")
}

// authenticate finds the cookie's live session and records its use at most once a minute. A
// missing, malformed, unknown or expired session is false; a store failure is the error.
func (n *network) authenticate(r *http.Request) (store.Session, bool, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.Session{}, false, nil
	}
	hash, ok := tokenHash(cookie.Value)
	if !ok {
		return store.Session{}, false, nil
	}
	sess, err := n.h.store.SessionByToken(r.Context(), hash)
	if errors.Is(err, store.ErrNotFound) {
		return store.Session{}, false, nil
	}
	if err != nil {
		return store.Session{}, false, err
	}
	now := n.clock()
	if sess, ok, err = n.live(r.Context(), sess, now); !ok || err != nil {
		return store.Session{}, false, err
	}
	if last, err := time.Parse(time.RFC3339Nano, sess.LastUsed); err == nil && now.Sub(last) >= sessionTouch {
		// A session revoked since it was read is gone; a newer use another request wrote stays.
		err := n.h.store.TouchSession(r.Context(), sess.ID, sess.LastUsed, now)
		if errors.Is(err, store.ErrNotFound) {
			return store.Session{}, false, nil
		}
		if err != nil {
			return store.Session{}, false, err
		}
		sess.LastUsed = store.SessionStamp(now)
	}
	return sess, true, nil
}

// sessionLive reports whether the session is within both expiries at now, judged in whole
// milliseconds as lists and counts judge it in SQL: last use after now less the idle lifetime,
// creation after now less the absolute one.
func sessionLive(sess store.Session, now time.Time) bool {
	created, err1 := time.Parse(time.RFC3339Nano, sess.Created)
	last, err2 := time.Parse(time.RFC3339Nano, sess.LastUsed)
	now = now.Truncate(time.Millisecond)
	return err1 == nil && err2 == nil && now.Before(last.Truncate(time.Millisecond).Add(sessionIdle)) && now.Before(created.Truncate(time.Millisecond).Add(sessionAbsolute))
}

// live returns the session's row while it is within both expiries. An expired row is deleted;
// when another request used it first, the row it wrote is judged instead.
func (n *network) live(ctx context.Context, sess store.Session, now time.Time) (store.Session, bool, error) {
	for range 2 {
		if sessionLive(sess, now) {
			return sess, true, nil
		}
		gone, err := n.h.expire(ctx, sess)
		if err != nil || gone {
			return store.Session{}, false, err
		}
		sess, err = n.h.store.Session(ctx, sess.ID)
		if errors.Is(err, store.ErrNotFound) {
			return store.Session{}, false, nil
		}
		if err != nil {
			return store.Session{}, false, err
		}
	}
	return store.Session{}, false, nil
}

// sessionAlive is an open request's recheck: the row is still there and not expired, or the
// store's error.
func (n *network) sessionAlive(ctx context.Context, id string) (bool, error) {
	n.at(ctx, "recheck")
	sess, err := n.h.store.Session(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, ok, err := n.live(ctx, sess, n.clock())
	return ok, err
}

// peer is the TCP peer's address; forwarded headers are not trusted.
func peer(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// throttled answers 429 while the account (when named) or the address must wait.
func (n *network) throttled(w http.ResponseWriter, account, address string) bool {
	wait := n.h.hub.limits.wait(account, address, n.clock())
	if wait <= 0 {
		return false
	}
	retryAfter(w, wait)
	return true
}

func retryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	writeError(w, 429, "throttled")
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

type selfJSON struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Owner           bool   `json:"owner"`
	PasswordAllowed bool   `json:"password_allowed"`
}

func (n *network) self(a store.Account) selfJSON {
	return selfJSON{a.ID, a.Name, n.h.isOwner(a.ID), a.PasswordAllowed}
}

type signedInJSON struct {
	Account selfJSON `json:"account"`
}

// newSession is a fresh session and its token; a cookie sent with the request is never reused.
func newSession(r *http.Request, user, credential string) (store.Session, string, error) {
	token, hash, err := newToken()
	if err != nil {
		return store.Session{}, "", err
	}
	id, err := newID()
	if err != nil {
		return store.Session{}, "", err
	}
	return store.Session{ID: id, TokenHash: hash, User: user, Credential: credential, Address: peer(r)}, token, nil
}

// signIn commits a verified credential's use with a new session, the account rechecked in that
// transaction, and sets the session's cookie.
func (n *network) signIn(w http.ResponseWriter, r *http.Request, a store.Account, credential string, check *checking, commit func(store.Session) error) {
	sess, token, err := newSession(r, a.ID, credential)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if err := commit(sess); err != nil {
		switch {
		case errors.Is(err, store.ErrSignCount): // passkeys only, which pass their check
			check.failed(n.clock())
			writeError(w, 401, "credential_invalid")
		case errors.Is(err, store.ErrAccountDisabled), errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrPasswordNotAllowed):
			writeError(w, 401, "login_failed")
		default:
			writeInternalError(w, err)
		}
		return
	}
	n.h.hub.limits.clear(a.ID)
	setSessionCookie(w, token)
	writeJSON(w, 200, signedInJSON{n.self(a)})
}

func (n *network) loginOptions(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if !emptyBody(body) {
		writeError(w, 400, "bad_request")
		return
	}
	if n.throttled(w, "", peer(r)) {
		return
	}
	assertion, session, err := n.rp.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	cer := &ceremony{kind: loginCeremony, session: *session, expires: n.clock().Add(ceremonyLifetime)}
	if err := n.ceremonies.put(cer); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, 200, struct {
		Ceremony string                                     `json:"ceremony"`
		Options  protocol.PublicKeyCredentialRequestOptions `json:"options"`
	}{cer.id, assertion.Response})
}

func (n *network) loginPasskey(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Ceremony   string          `json:"ceremony"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := decode(body, &input); err != nil || len(input.Credential) == 0 {
		writeError(w, 400, "bad_request")
		return
	}
	address := peer(r)
	if n.throttled(w, "", address) {
		return
	}
	cer, ok := n.ceremonies.take(input.Ceremony, loginCeremony, n.clock())
	if !ok {
		writeError(w, 400, "ceremony_invalid")
		return
	}
	parsed, parseErr := protocol.ParseCredentialRequestResponseBytes(input.Credential)
	var a store.Account
	known := false
	if parseErr == nil {
		var err error
		a, err = n.h.store.AccountByHandle(r.Context(), parsed.Response.UserHandle)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeInternalError(w, err)
			return
		}
		known = err == nil
	}
	account := ""
	if known {
		account = a.ID
	}
	// Admitted before the assertion is verified, and counted as a failure until it is settled.
	check, wait := n.h.hub.limits.admit(account, address, n.clock())
	if wait > 0 {
		retryAfter(w, wait)
		return
	}
	defer check.done()
	if !known {
		check.failed(n.clock())
		writeError(w, 401, "login_failed")
		return
	}
	n.at(r.Context(), "passkey")
	stored, err := n.h.store.Credentials(r.Context(), a.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	records, ids := passkeysOf(stored)
	user := &passkeyUser{account: a, handle: a.Handle, credentials: records, ids: ids}
	_, credential, err := n.rp.ValidatePasskeyLogin(func(_, _ []byte) (webauthn.User, error) { return user, nil }, cer.session, parsed)
	if err != nil {
		check.failed(n.clock())
		writeError(w, 401, "login_failed")
		return
	}
	// A sign count that does not advance means a copy of the key may exist.
	if credential.Authenticator.CloneWarning {
		check.failed(n.clock())
		writeError(w, 401, "credential_invalid")
		return
	}
	if a.Disabled {
		writeError(w, 401, "login_failed")
		return
	}
	i, ok := user.storedID(credential.ID)
	if !ok {
		writeInternalError(w, errors.New("verified passkey is not among the account's"))
		return
	}
	n.signIn(w, r, a, ids[i], check, func(sess store.Session) error {
		return n.h.store.PasskeySignIn(r.Context(), credential.Authenticator.SignCount, byte(credential.Flags.ProtocolValue()), sess, n.clock())
	})
}

func (n *network) loginPassword(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Account  string `json:"account"`
		Password string `json:"password"`
	}
	if err := decode(body, &input); err != nil || len(input.Password) > maxPasswordBytes || len(input.Account) > 256 {
		writeError(w, 400, "bad_request")
		return
	}
	if err := n.hasher.calibrate(r.Context()); err != nil {
		return
	}
	address := peer(r)
	if n.throttled(w, "", address) {
		return
	}
	a, err := n.h.store.AccountByName(r.Context(), input.Account)
	known := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeInternalError(w, err)
		return
	}
	// An unknown name is throttled under the name, so a refusal does not tell it apart.
	account := "name:" + input.Account
	if known {
		account = a.ID
	}
	if n.throttled(w, account, address) {
		return
	}
	phc, credential := dummyPHC, ""
	if known && !a.Disabled && a.PasswordAllowed {
		c, err := n.h.store.PasswordCredential(r.Context(), a.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeInternalError(w, err)
			return
		}
		if err == nil {
			phc, credential = c.PasswordHash, c.ID
		}
	}
	free, ok := n.hashSlot(w, r)
	if !ok {
		return
	}
	defer free()
	// Admitted only once this request holds a slot: failures recorded while it waited, and checks
	// still running, count against it. From here it counts as a failure until it ends.
	check, wait := n.h.hub.limits.admit(account, address, n.clock())
	if wait > 0 {
		retryAfter(w, wait)
		return
	}
	defer check.done()
	// Every check holds its slot until a deadline fixed at admission, one calibrated hash later,
	// and a failure counts at that deadline, a disconnected client's included: neither the stored
	// hash's cost nor a disconnect shows in when the slot frees or the failure counts. Only the
	// network channel's shutdown cuts the wait short.
	deadline, failsAt := n.hasher.deadline(), n.clock().Add(n.hasher.cost)
	untilDeadline := func() { _ = n.hasher.pad(n.shutdown, deadline) }
	match, rehash, err := n.hasher.verifyHeld(input.Password, phc)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !match || credential == "" {
		untilDeadline()
		check.failed(failsAt) // at the deadline, however late this check ended
		free()
		if r.Context().Err() != nil {
			return
		}
		writeError(w, 401, "login_failed")
		return
	}
	updated := ""
	if rehash {
		if updated, err = n.hasher.hashHeld(input.Password); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	untilDeadline()
	check.done()
	free()
	n.signIn(w, r, a, credential, nil, func(sess store.Session) error {
		return n.h.store.PasswordSignIn(r.Context(), updated, sess, n.clock())
	})
}

// hashSlot waits for a hashing slot; free gives it back and may be called more than once.
func (n *network) hashSlot(w http.ResponseWriter, r *http.Request) (func(), bool) {
	release, err := n.hasher.slot(r.Context())
	if err != nil {
		if r.Context().Err() == nil {
			writeInternalError(w, err)
		}
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(release) }, true
}

// enrollmentCode reads and checks a code without using it; a bad one counts as a failure. A good
// one returns its reservation, which the caller settles once with the code's final outcome.
func (n *network) enrollmentCode(w http.ResponseWriter, r *http.Request, input string) (store.Account, []byte, *checking, bool) {
	// Admitted before the code is looked up, and counted as a failure until it is settled: here
	// unless it passes to the caller, a panic included.
	check, wait := n.h.hub.limits.admit("", peer(r), n.clock())
	if wait > 0 {
		retryAfter(w, wait)
		return store.Account{}, nil, nil, false
	}
	owned := false
	defer func() {
		if !owned {
			check.done()
		}
	}()
	code, ok := normalizeCode(input)
	if !ok {
		check.failed(n.clock())
		writeError(w, 400, "code_invalid")
		return store.Account{}, nil, nil, false
	}
	n.at(r.Context(), "code")
	hash := codeHash(code)
	a, err := n.h.store.CheckEnrollmentCode(r.Context(), hash, n.clock())
	if errors.Is(err, store.ErrCodeInvalid) {
		check.failed(n.clock())
		writeError(w, 400, "code_invalid")
		return store.Account{}, nil, nil, false
	}
	if err != nil {
		writeInternalError(w, err)
		return store.Account{}, nil, nil, false
	}
	owned = true
	return a, hash, check, true
}

func (n *network) enrollmentOptions(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Code string `json:"code"`
	}
	if err := decode(body, &input); err != nil {
		writeError(w, 400, "bad_request")
		return
	}
	if n.throttled(w, "", peer(r)) {
		return
	}
	a, hash, check, ok := n.enrollmentCode(w, r, input.Code)
	if !ok {
		return
	}
	defer check.done()
	handle := a.Handle
	if handle == nil {
		handle = make([]byte, 64)
		if _, err := rand.Read(handle); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	stored, err := n.h.store.Credentials(r.Context(), a.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	records, ids := passkeysOf(stored)
	user := &passkeyUser{account: a, handle: handle, credentials: records, ids: ids}
	creation, session, err := n.rp.BeginRegistration(user, webauthn.WithExclusions(webauthn.Credentials(records).CredentialDescriptors()))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	cer := &ceremony{kind: registrationCeremony, session: *session, user: a.ID, handle: handle, codeHash: hash, expires: n.clock().Add(ceremonyLifetime)}
	if err := n.ceremonies.put(cer); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, 200, struct {
		Ceremony string                                      `json:"ceremony"`
		Options  protocol.PublicKeyCredentialCreationOptions `json:"options"`
		Account  selfJSON                                    `json:"account"`
	}{cer.id, creation.Response, n.self(a)})
}

func (n *network) enrollPasskey(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Code       string          `json:"code"`
		Ceremony   string          `json:"ceremony"`
		Credential json.RawMessage `json:"credential"`
		Name       string          `json:"name"`
	}
	if err := decode(body, &input); err != nil || len(input.Credential) == 0 || !validAccountName(input.Name) {
		writeError(w, 400, "bad_request")
		return
	}
	if n.throttled(w, "", peer(r)) {
		return
	}
	a, hash, check, ok := n.enrollmentCode(w, r, input.Code)
	if !ok {
		return
	}
	defer check.done()
	cer, ok := n.ceremonies.take(input.Ceremony, registrationCeremony, n.clock())
	if !ok || cer.user != a.ID || !bytes.Equal(cer.codeHash, hash) {
		writeError(w, 400, "ceremony_invalid")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(input.Credential)
	if err != nil {
		writeError(w, 400, "credential_invalid")
		return
	}
	created, err := n.rp.CreateCredential(&passkeyUser{account: a, handle: cer.handle}, cer.session, parsed)
	if err != nil {
		writeError(w, 400, "credential_invalid")
		return
	}
	if _, err := n.h.store.PasskeyByCredentialID(r.Context(), created.ID); err == nil {
		writeError(w, 400, "credential_invalid")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeInternalError(w, err)
		return
	}
	id, err := newID()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	transports := make([]string, len(created.Transport))
	for i, t := range created.Transport {
		transports[i] = string(t)
	}
	credential := store.Credential{ID: id, User: a.ID, Kind: store.Passkey, Name: input.Name, CredentialID: created.ID, PublicKey: created.PublicKey, AAGUID: created.Authenticator.AAGUID, SignCount: created.Authenticator.SignCount, Flags: byte(created.Flags.ProtocolValue()), Transports: transports}
	n.enroll(w, r, a, credential, check, func(sess store.Session) ([]string, error) {
		return n.h.store.EnrollPasskey(r.Context(), hash, n.clock, cer.handle, credential, sess)
	})
}

func (n *network) enrollPassword(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var input struct {
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if err := decode(body, &input); err != nil {
		writeError(w, 400, "bad_request")
		return
	}
	if n.throttled(w, "", peer(r)) {
		return
	}
	if !validPassword(input.Password) {
		writeError(w, 400, "password_invalid")
		return
	}
	a, hash, check, ok := n.enrollmentCode(w, r, input.Code)
	if !ok {
		return
	}
	defer check.done()
	if !a.PasswordAllowed {
		writeError(w, 403, "password_not_allowed")
		return
	}
	free, ok := n.hashSlot(w, r)
	if !ok {
		return
	}
	defer free()
	// An address blocked while this request waited for a slot is refused before the hash; the
	// request's own reservation aside.
	if wait := check.wait(n.clock()); wait > 0 {
		retryAfter(w, wait)
		return
	}
	phc, err := n.hasher.hashHeld(input.Password)
	free()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	id, err := newID()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	credential := store.Credential{ID: id, User: a.ID, Kind: store.Password, Name: "Password", PasswordHash: phc}
	n.enroll(w, r, a, credential, check, func(sess store.Session) ([]string, error) {
		return n.h.store.EnrollPassword(r.Context(), hash, n.clock, credential, sess)
	})
}

// enroll commits a credential with its new session, ending the account's others and their streams.
func (n *network) enroll(w http.ResponseWriter, r *http.Request, a store.Account, credential store.Credential, check *checking, commit func(store.Session) ([]string, error)) {
	sess, token, err := newSession(r, a.ID, credential.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	n.at(r.Context(), "enroll-commit")
	ended, err := commit(sess)
	if errors.Is(err, store.ErrCodeInvalid) {
		check.failed(n.clock())
	}
	if err != nil {
		storeError(w, err)
		return
	}
	n.h.closeSessions(ended)
	setSessionCookie(w, token)
	writeJSON(w, 201, signedInJSON{n.self(a)})
}

func (n *network) logout(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	if err := n.h.store.DeleteSession(r.Context(), sess.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		writeInternalError(w, err)
		return
	}
	n.h.closeSessions([]string{sess.ID})
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (n *network) account(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	a, err := n.h.store.Account(r.Context(), sess.User)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, n.self(a))
}

func (n *network) accountSessions(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	sessions, err := n.h.store.Sessions(r.Context(), sess.User, n.h.live())
	if err != nil {
		storeError(w, err)
		return
	}
	type ownSessionJSON struct {
		ID       string `json:"id"`
		Created  string `json:"created"`
		LastUsed string `json:"last_used"`
		Address  string `json:"address"`
		Current  bool   `json:"current"`
	}
	out := make([]ownSessionJSON, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, ownSessionJSON{s.ID, s.Created, s.LastUsed, s.Address, s.ID == sess.ID})
	}
	writeJSON(w, 200, out)
}

func (n *network) revokeOwnSession(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	n.at(r.Context(), "revoke-session")
	if err := n.h.store.DeleteUserSession(r.Context(), sess, id, n.h.live); err != nil {
		storeError(w, err)
		return
	}
	n.h.closeSessions([]string{id})
	if id == sess.ID {
		clearSessionCookie(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (n *network) accountCredentials(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	credentials, err := n.h.store.Credentials(r.Context(), sess.User)
	if err != nil {
		storeError(w, err)
		return
	}
	type credentialJSON struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		Name     string `json:"name"`
		Created  string `json:"created"`
		LastUsed string `json:"last_used"`
	}
	out := make([]credentialJSON, 0, len(credentials))
	for _, c := range credentials {
		out = append(out, credentialJSON{c.ID, c.Kind, c.Name, c.Created, c.LastUsed})
	}
	writeJSON(w, 200, out)
}

func (n *network) removeOwnCredential(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	n.at(r.Context(), "remove-credential")
	ended, err := n.h.store.RemoveCredential(r.Context(), sess, r.PathValue("id"), n.h.live)
	if err != nil {
		storeError(w, err)
		return
	}
	n.h.closeSessions(ended)
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}
