package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

// softKey is a software authenticator: one ES256 discoverable credential, attestation "none",
// its client data and authenticator data built by hand. The fields bend one rule each.
type softKey struct {
	key    *ecdsa.PrivateKey
	id     []byte
	handle []byte // the user handle the registration options carried
	count  uint32 // the sign count the next ceremony reports, then advanced

	origin string // the client's origin; the fixture's when empty
	rpID   string // the RP ID hashed into authenticator data; the options' when empty
	noUV   bool   // leave user verification unset
	frozen bool   // report the same count without advancing it
}

func newSoftKey(t *testing.T) *softKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &softKey{key: key, id: id, count: 1}
}

var b64u = base64.RawURLEncoding

func (k *softKey) flags(attested bool) byte {
	f := byte(0x01) // user present
	if !k.noUV {
		f |= 0x04
	}
	if attested {
		f |= 0x40
	}
	return f
}

func (k *softKey) authData(rpID string, attested bool) []byte {
	if k.rpID != "" {
		rpID = k.rpID
	}
	hash := sha256.Sum256([]byte(rpID))
	data := append(hash[:], k.flags(attested))
	data = binary.BigEndian.AppendUint32(data, k.count)
	if !k.frozen {
		k.count++
	}
	return data
}

func (k *softKey) clientData(t *testing.T, kind, challenge, origin string) []byte {
	t.Helper()
	if k.origin != "" {
		origin = k.origin
	}
	data, err := json.Marshal(map[string]any{"type": kind, "challenge": challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// create answers navigator.credentials.create for the creation options.
func (k *softKey) create(t *testing.T, options map[string]any, origin string) json.RawMessage {
	t.Helper()
	rp := options["rp"].(map[string]any)
	user := options["user"].(map[string]any)
	handle, err := b64u.DecodeString(user["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	k.handle = handle
	cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: k.key.PublicKey.X.FillBytes(make([]byte, 32)), -3: k.key.PublicKey.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	authData := k.authData(rp["id"].(string), true)
	authData = append(authData, make([]byte, 16)...) // AAGUID
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(k.id)))
	authData = append(authData, k.id...)
	authData = append(authData, cose...)
	attestation, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		t.Fatal(err)
	}
	clientData := k.clientData(t, "webauthn.create", options["challenge"].(string), origin)
	return k.marshal(t, map[string]any{"clientDataJSON": b64u.EncodeToString(clientData), "attestationObject": b64u.EncodeToString(attestation), "transports": []string{"internal"}})
}

// get answers navigator.credentials.get for the request options.
func (k *softKey) get(t *testing.T, options map[string]any, origin string) json.RawMessage {
	t.Helper()
	authData := k.authData(options["rpId"].(string), false)
	clientData := k.clientData(t, "webauthn.get", options["challenge"].(string), origin)
	clientHash := sha256.Sum256(clientData)
	signed := sha256.Sum256(append(append([]byte{}, authData...), clientHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, k.key, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	return k.marshal(t, map[string]any{"clientDataJSON": b64u.EncodeToString(clientData), "authenticatorData": b64u.EncodeToString(authData), "signature": b64u.EncodeToString(signature), "userHandle": b64u.EncodeToString(k.handle)})
}

func (k *softKey) marshal(t *testing.T, response map[string]any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]any{"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key", "response": response, "clientExtensionResults": map[string]any{}, "authenticatorAttachment": "platform"})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
