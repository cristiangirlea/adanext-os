package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// sessionCookie is host-only, Secure and path / by its prefix.
const sessionCookie = "__Host-ada-session"

// newToken returns a session token and the SHA-256 of its 32 random bytes, all the store keeps of it.
func newToken() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(b), sum[:], nil
}

// tokenHash hashes the bytes of a presented token, refusing anything that does not decode to 32.
func tokenHash(token string) ([]byte, bool) {
	if len(token) != 43 {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) != 32 {
		return nil, false
	}
	sum := sha256.Sum256(b)
	return sum[:], true
}

// Enrollment codes are 20 Crockford base32 symbols, 100 random bits.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newCode returns a code in its stored form; groupCode is its display form.
func newCode() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	code := make([]byte, 20)
	for i, v := range b {
		code[i] = crockford[v&31] // 256 is a multiple of 32: uniform
	}
	return string(code), nil
}

func groupCode(code string) string {
	return code[0:5] + "-" + code[5:10] + "-" + code[10:15] + "-" + code[15:20]
}

// normalizeCode reads a code as people type it: any case, hyphens and spaces, I and L for 1, O for 0.
func normalizeCode(input string) (string, bool) {
	if len(input) > 64 {
		return "", false
	}
	var b strings.Builder
	for _, r := range strings.ToUpper(input) {
		switch r {
		case '-', ' ':
			continue
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		}
		if r > unicode.MaxASCII || !strings.ContainsRune(crockford, r) {
			return "", false
		}
		b.WriteRune(r)
	}
	if b.Len() != 20 {
		return "", false
	}
	return b.String(), true
}

func codeHash(code string) []byte {
	sum := sha256.Sum256([]byte(code))
	return sum[:]
}

// Password bounds: at least 12 characters, at most 1,024 bytes.
const (
	minPasswordRunes = 12
	maxPasswordBytes = 1024
)

func validPassword(p string) bool {
	return len(p) <= maxPasswordBytes && utf8.ValidString(p) && utf8.RuneCountInString(p) >= minPasswordRunes
}

type argonParams struct {
	memory  uint32 // KiB
	time    uint32
	threads uint8
	saltLen int
	keyLen  int
}

var currentArgon = argonParams{memory: 64 * 1024, time: 3, threads: 1, saltLen: 16, keyLen: 32}

func (p argonParams) below(q argonParams) bool {
	return p.memory < q.memory || p.time < q.time || p.threads < q.threads || p.saltLen < q.saltLen || p.keyLen < q.keyLen
}

// hasher computes Argon2id at most two at a time, for login and enrollment together.
type hasher struct {
	sem    chan struct{}
	key    func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte
	queued atomic.Int32 // requests waiting for a slot
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error

	calibration sync.Once
	calibrated  chan struct{} // closed once cost is measured
	cost        time.Duration // one hash at the current parameters, measured once and frozen
}

func newHasher() *hasher {
	return &hasher{sem: make(chan struct{}, 2), key: argon2.IDKey, now: time.Now, sleep: sleepContext}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// slot waits for one of the two hashing slots; the caller hashes with the Held methods, then releases.
func (h *hasher) slot(ctx context.Context) (func(), error) {
	h.queued.Add(1)
	defer h.queued.Add(-1)
	select {
	case h.sem <- struct{}{}:
		return func() { <-h.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *hasher) compute(password, salt []byte, p argonParams) []byte {
	return h.key(password, salt, p.time, p.memory, p.threads, uint32(p.keyLen))
}

// calibrate waits for the one hash that measures cost, started by the first caller in a slot like
// any hash; every password login waits on it first, and cost never changes after.
func (h *hasher) calibrate(ctx context.Context) error {
	h.calibration.Do(func() {
		h.calibrated = make(chan struct{})
		go func() {
			defer close(h.calibrated)
			release, err := h.slot(context.Background())
			if err != nil {
				return
			}
			defer release()
			began := h.now()
			_, _ = h.hashHeld(calibrationPassword)
			h.cost = h.now().Sub(began)
		}()
	})
	select {
	case <-h.calibrated:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

const calibrationPassword = "a hash to measure"

// deadline is when a check admitted now ends: one calibrated hash later, so a wrong password, an
// unknown account and one without a usable password cost the same (parameters only rise, so a
// stored hash never costs more).
func (h *hasher) deadline() time.Time { return h.now().Add(h.cost) }

// pad waits, holding the caller's slot, until the deadline or until ctx ends.
func (h *hasher) pad(ctx context.Context, deadline time.Time) error {
	if remaining := deadline.Sub(h.now()); remaining > 0 {
		return h.sleep(ctx, remaining)
	}
	return nil
}

// hash returns the password's PHC string under the current parameters.
func (h *hasher) hash(ctx context.Context, password string) (string, error) {
	release, err := h.slot(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return h.hashHeld(password)
}

func (h *hasher) hashHeld(password string) (string, error) {
	salt := make([]byte, currentArgon.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return encodePHC(currentArgon, salt, h.compute([]byte(password), salt, currentArgon)), nil
}

// dummyPHC is what a login without a usable password is checked against, so it costs one hash too.
var dummyPHC = encodePHC(currentArgon, make([]byte, currentArgon.saltLen), make([]byte, currentArgon.keyLen))

// verify compares in constant time; rehash is set when the stored parameters are below the current ones.
func (h *hasher) verify(ctx context.Context, password, phc string) (ok, rehash bool, err error) {
	release, err := h.slot(ctx)
	if err != nil {
		return false, false, err
	}
	defer release()
	return h.verifyHeld(password, phc)
}

// verifyHeld is verify in a slot already held.
func (h *hasher) verifyHeld(password, phc string) (ok, rehash bool, err error) {
	p, salt, want, err := decodePHC(phc)
	if err != nil {
		return false, false, err
	}
	ok = subtle.ConstantTimeCompare(h.compute([]byte(password), salt, p), want) == 1
	return ok, p.below(currentArgon), nil
}

var b64 = base64.RawStdEncoding

func encodePHC(p argonParams, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.memory, p.time, p.threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

func decodePHC(phc string) (argonParams, []byte, []byte, error) {
	var p argonParams
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return p, nil, nil, errors.New("not an argon2id PHC string")
	}
	for _, field := range strings.Split(parts[3], ",") {
		name, value, _ := strings.Cut(field, "=")
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return p, nil, nil, fmt.Errorf("argon2id parameter %q: %w", name, err)
		}
		switch name {
		case "m":
			p.memory = uint32(n)
		case "t":
			p.time = uint32(n)
		case "p":
			if n > 255 {
				return p, nil, nil, errors.New("argon2id lanes out of range")
			}
			p.threads = uint8(n)
		default:
			return p, nil, nil, fmt.Errorf("argon2id parameter %q", name)
		}
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, err
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return p, nil, nil, err
	}
	if p.memory == 0 || p.time == 0 || p.threads == 0 || len(key) == 0 || p.memory > 4<<20 || p.time > 64 {
		return p, nil, nil, errors.New("argon2id parameters out of range")
	}
	p.saltLen, p.keyLen = len(salt), len(key)
	return p, salt, key, nil
}

// validAccountName is 1-64 printable characters with no space at either end.
func validAccountName(name string) bool {
	if !utf8.ValidString(name) || name != strings.TrimSpace(name) {
		return false
	}
	n := utf8.RuneCountInString(name)
	if n < 1 || n > 64 {
		return false
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
