package api

import (
	"container/list"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/cristiangirlea/ada-os/internal/store"
)

// Ceremony state lives in memory: 5 minutes, at most 1,000 pending, the oldest dropped first.
const (
	ceremonyLifetime = 5 * time.Minute
	ceremonyLimit    = 1000
)

type ceremonyKind int

const (
	loginCeremony ceremonyKind = iota + 1
	registrationCeremony
)

type ceremony struct {
	id       string
	kind     ceremonyKind
	session  webauthn.SessionData
	user     string // registration: the account the code was issued for
	handle   []byte // registration: the user handle the options carried
	codeHash []byte // registration: the code the options were issued for
	expires  time.Time
}

type ceremonies struct {
	mu    sync.Mutex
	order *list.List // oldest at the back
	byID  map[string]*list.Element
}

func newCeremonies() *ceremonies {
	return &ceremonies{order: list.New(), byID: map[string]*list.Element{}}
}

func (c *ceremonies) put(cer *ceremony) error {
	id, err := newID()
	if err != nil {
		return err
	}
	cer.id = id
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.order.Len() >= ceremonyLimit {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.byID, oldest.Value.(*ceremony).id)
	}
	c.byID[id] = c.order.PushFront(cer)
	return nil
}

// take removes the ceremony: each is answered once, whatever the answer.
func (c *ceremonies) take(id string, kind ceremonyKind, now time.Time) (*ceremony, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byID[id]
	if !ok {
		return nil, false
	}
	c.order.Remove(e)
	delete(c.byID, id)
	cer := e.Value.(*ceremony)
	if cer.kind != kind || !now.Before(cer.expires) {
		return nil, false
	}
	return cer, true
}

func (c *ceremonies) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// passkeyUser presents an account to the WebAuthn library.
type passkeyUser struct {
	account     store.Account
	handle      []byte
	credentials []webauthn.Credential
	ids         []string // credentials[i] is stored as ids[i]
}

func (u *passkeyUser) WebAuthnID() []byte                         { return u.handle }
func (u *passkeyUser) WebAuthnName() string                       { return u.account.Name }
func (u *passkeyUser) WebAuthnDisplayName() string                { return u.account.Name }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func (u *passkeyUser) storedID(raw []byte) (int, bool) {
	for i, c := range u.credentials {
		if string(c.ID) == string(raw) {
			return i, true
		}
	}
	return 0, false
}

// passkeysOf lists the account's passkeys as WebAuthn records.
func passkeysOf(credentials []store.Credential) ([]webauthn.Credential, []string) {
	var out []webauthn.Credential
	var ids []string
	for _, c := range credentials {
		if c.Kind != store.Passkey {
			continue
		}
		transports := make([]protocol.AuthenticatorTransport, len(c.Transports))
		for i, t := range c.Transports {
			transports[i] = protocol.AuthenticatorTransport(t)
		}
		out = append(out, webauthn.Credential{
			ID:            c.CredentialID,
			PublicKey:     c.PublicKey,
			Transport:     transports,
			Flags:         webauthn.NewCredentialFlags(protocol.AuthenticatorFlags(c.Flags)),
			Authenticator: webauthn.Authenticator{AAGUID: c.AAGUID, SignCount: c.SignCount},
		})
		ids = append(ids, c.ID)
	}
	return out, ids
}

func newRelyingParty(cfg NetworkConfig) (*webauthn.WebAuthn, error) {
	timeout := webauthn.TimeoutConfig{Timeout: ceremonyLifetime, TimeoutUVD: ceremonyLifetime}
	return webauthn.New(&webauthn.Config{
		RPID:                  cfg.RPID,
		RPDisplayName:         "Ada",
		RPOrigins:             []string{cfg.Origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		// The ceremony store enforces expiry on the configured clock.
		Timeouts: webauthn.TimeoutsConfig{Login: timeout, Registration: timeout},
	})
}
