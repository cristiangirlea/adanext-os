package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/ada-os/internal/api"
	"github.com/cristiangirlea/ada-os/internal/artifact"
)

// maxPEMBytes bounds every certificate and key file adad reads; maxSmallBytes the setting and
// the pointer files.
const (
	maxPEMBytes   = 1 << 20
	maxSmallBytes = 4 << 10
)

var errNotOwnerOnly = errors.New("the file is not owner-only")

// syncDir makes a directory's entries durable; a test seam over syncDirectory.
var syncDir = syncDirectory

// tlsPair is a checked certificate and key, ready for GetCertificate.
type tlsPair struct {
	cert tls.Certificate
	leaf *x509.Certificate
	info api.ListenerCertificate
}

// validAt reports whether now is within the leaf's validity.
func (p *tlsPair) validAt(now time.Time) bool {
	return !now.Before(p.leaf.NotBefore) && now.Before(p.leaf.NotAfter)
}

// check is the pair's time and name checks at now, nil when both pass.
func (p *tlsPair) check(name string, now time.Time) *api.ListenerError {
	if now.Before(p.leaf.NotBefore) {
		return &api.ListenerError{Code: "certificate_expired", Message: fmt.Sprintf("The certificate is valid only from %s; install one that is valid now.", p.info.NotBefore)}
	}
	if !now.Before(p.leaf.NotAfter) {
		return &api.ListenerError{Code: "certificate_expired", Message: fmt.Sprintf("The certificate expired on %s; install a current one.", p.info.NotAfter)}
	}
	if name != "" && p.leaf.VerifyHostname(name) != nil {
		return &api.ListenerError{Code: "certificate_name", Message: fmt.Sprintf("The certificate does not name %s; install one for that name, or change the name.", name)}
	}
	return nil
}

func invalidPair(detail string) *api.ListenerError {
	return &api.ListenerError{Code: "certificate_invalid", Message: "The certificate was refused: " + detail + "."}
}

// describe is what the owner sees of a leaf.
func describe(leaf *x509.Certificate) api.ListenerCertificate {
	sum := sha256.Sum256(leaf.Raw)
	return api.ListenerCertificate{Names: append([]string{}, leaf.DNSNames...), NotBefore: leaf.NotBefore.UTC().Format(time.RFC3339), NotAfter: leaf.NotAfter.UTC().Format(time.RFC3339), SHA256: hex.EncodeToString(sum[:])}
}

// checkPair runs every check of a pair at now: the chain parses, the key matches, the leaf is
// valid and names name when one is set. The description is returned whenever the leaf parses.
func checkPair(certPEM, keyPEM []byte, name string, now time.Time) (*tlsPair, *api.ListenerCertificate, *api.ListenerError) {
	var leaf *x509.Certificate
	rest := certPEM
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		// pem.Decode skips a malformed block silently; the bytes it took hold one boundary line only when none was skipped.
		if beginLines(rest[:len(rest)-len(next)]) != 1 {
			return nil, nil, invalidPair("the certificate file holds a malformed PEM block")
		}
		if block.Type != "CERTIFICATE" {
			return nil, nil, invalidPair("the certificate file holds a PEM block that is not a certificate")
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, invalidPair("a certificate of the chain does not parse")
		}
		if leaf == nil {
			leaf = c
		}
		rest = next
	}
	if leaf == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, invalidPair("the certificate file is not a PEM certificate chain")
	}
	info := describe(leaf)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, &info, invalidPair("the key does not match the certificate, or is not a PEM private key")
	}
	cert.Leaf = leaf
	p := &tlsPair{cert: cert, leaf: leaf, info: info}
	if fail := p.check(name, now); fail != nil {
		return nil, &info, fail
	}
	return p, &info, nil
}

// beginLines counts the PEM boundary lines that open a block: lines starting "-----BEGIN ".
func beginLines(data []byte) int {
	n := 0
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("-----BEGIN ")) {
			n++
		}
	}
	return n
}

// leafOf describes the chain's first certificate, nil when it does not parse.
func leafOf(certPEM []byte) *api.ListenerCertificate {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	info := describe(leaf)
	return &info
}

// readLimited reads an opened file of at most limit bytes and closes it.
func readLimited(f *os.File, limit int64) ([]byte, error) {
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("the file is larger than %d bytes", limit)
	}
	return data, nil
}

// readPrivate reads an owner-only regular file of at most limit bytes, never through a link.
func readPrivate(path string, limit int64) ([]byte, error) {
	f, err := openPrivate(path)
	if err != nil {
		return nil, err
	}
	return readLimited(f, limit)
}

// readSource reads a file the owner chose: a regular file, not a link, of at most 1 MiB.
func readSource(path, what string) ([]byte, error) {
	f, err := openSource(path)
	if err == nil {
		var data []byte
		if data, err = readLimited(f, maxPEMBytes); err == nil {
			return data, nil
		}
	}
	return nil, invalidPair("the " + what + " file could not be read; it must be an existing regular file, not a link, of at most 1 MiB")
}

// pairStore keeps the installed pairs under the configuration directory's tls directory: each
// in pairs/<id>/, the one served named by current, the one before it by previous.
type pairStore struct {
	dir  string             // <configuration directory>/tls
	log  io.Writer          // what could not be made durable after the fact
	step func(string) error // test seam: called at each named point of an install; an error stops it there
	good string             // the pair last loaded and checked, or installed; "" when none
}

func (ps *pairStore) at(step string) error {
	if ps.step != nil {
		return ps.step(step)
	}
	return nil
}

var noPair = &api.ListenerError{Code: "certificate_missing", Message: "No certificate is installed; choose a certificate and its key."}

// load is the pair current names, or previous while current fails and it passes. With neither,
// the failure is current's, and the description current's leaf when it parses. It remembers the
// id it returned as the good pair.
func (ps *pairStore) load(name string, now time.Time) (*tlsPair, *api.ListenerCertificate, *api.ListenerError) {
	ps.good = ""
	var shown *api.ListenerCertificate
	fail := noPair
	current, err := ps.pointer("current")
	if err == nil {
		p, info, f := ps.loadPair(current, name, now)
		if f == nil {
			ps.good = current
			return p, &p.info, nil
		}
		shown, fail = info, f
	}
	if previous, err := ps.pointer("previous"); err == nil && previous != current {
		if p, _, f := ps.loadPair(previous, name, now); f == nil {
			ps.good = previous
			return p, &p.info, nil
		}
	}
	return nil, shown, fail
}

// loadPair reads and checks pairs/<id>, whose files must be owner-only regular files. A refused
// key still leaves the certificate described.
func (ps *pairStore) loadPair(id, name string, now time.Time) (*tlsPair, *api.ListenerCertificate, *api.ListenerError) {
	unreadable := invalidPair("a file of the installed pair is a link, not a regular file, larger than 1 MiB or readable by another account; install the pair again")
	certPEM, err := readPrivate(filepath.Join(ps.dir, "pairs", id, "cert.pem"), maxPEMBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, noPair
	}
	if err != nil {
		return nil, nil, unreadable
	}
	keyPEM, err := readPrivate(filepath.Join(ps.dir, "pairs", id, "key.pem"), maxPEMBytes)
	if err != nil {
		return nil, leafOf(certPEM), unreadable
	}
	return checkPair(certPEM, keyPEM, name, now)
}

// pointer reads a pointer file's pair id.
func (ps *pairStore) pointer(file string) (string, error) {
	data, err := readPrivate(filepath.Join(ps.dir, file), maxSmallBytes)
	if err != nil {
		return "", err
	}
	id := strings.TrimSuffix(string(data), "\n")
	if len(id) != 32 || !isLowerHex(id) {
		return "", fmt.Errorf("%s does not name a pair", file)
	}
	return id, nil
}

// install writes a checked pair into its own directory, then names it current by renaming a
// pointer into place, previous naming the good pair the store last loaded, when there is one.
// Each named step is a point a crash may stop at; a later load still finds a complete pair, the
// old or the new.
func (ps *pairStore) install(certPEM, keyPEM []byte, name string, now time.Time) (p *tlsPair, err error) {
	pairs := filepath.Join(ps.dir, "pairs")
	config := filepath.Dir(ps.dir)
	// Each level made private on its own, so a link at any of them is refused.
	for _, dir := range []string{config, ps.dir, pairs} {
		if err := artifact.PrivateDirectory(dir); err != nil {
			return nil, err
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(raw)
	dir := filepath.Join(pairs, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	// A failure before current names the new pair removes its directory; a stop at a step is a
	// crash in tests, which leaves it as a crash would.
	published, crashed := false, false
	defer func() {
		if err != nil && !published && !crashed {
			_ = os.RemoveAll(dir)
		}
	}()
	at := func(step string) error {
		err := ps.at(step)
		crashed = err != nil
		return err
	}
	// Every level's entry durable before a pointer can name the pair, a retried first install's included.
	for _, d := range []string{pairs, ps.dir, config} {
		if err := syncDir(d); err != nil {
			return nil, err
		}
	}
	if err := writeSynced(filepath.Join(dir, "cert.pem"), certPEM); err != nil {
		return nil, err
	}
	if err := at("cert-written"); err != nil {
		return nil, err
	}
	if err := writeSynced(filepath.Join(dir, "key.pem"), keyPEM); err != nil {
		return nil, err
	}
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	if err := at("key-written"); err != nil {
		return nil, err
	}
	p, _, fail := ps.loadPair(id, name, now)
	if fail != nil {
		return nil, fail
	}
	if err := at("checked"); err != nil {
		return nil, err
	}
	// previous is the pair the store last loaded and checked; with none known it stays as it is.
	if ps.good != "" && ps.good != id {
		if err := writeAtomic(ps.dir, "previous", []byte(ps.good+"\n")); err != nil {
			return nil, err
		}
	}
	if err := at("previous-written"); err != nil {
		return nil, err
	}
	if err := replaceFile(ps.dir, "current", []byte(id+"\n")); err != nil {
		return nil, err
	}
	published, ps.good = true, id
	// Renamed: a restart serves the new pair, so it is served now too, whatever the sync says.
	if err := syncDir(ps.dir); err != nil && ps.log != nil {
		fmt.Fprintln(ps.log, "adad: network listener: sync after naming the new pair:", err)
	}
	if err := at("current-written"); err != nil {
		return nil, err
	}
	return p, nil
}

// removeStale removes, best effort, every pair directory neither pointer names, those a stopped
// install left included. Only pair directories are touched, and nothing while a pointer exists
// that cannot be read.
func (ps *pairStore) removeStale() {
	keep := []string{}
	for _, file := range []string{"current", "previous"} {
		id, err := ps.pointer(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return
		}
		keep = append(keep, id)
	}
	entries, err := os.ReadDir(filepath.Join(ps.dir, "pairs"))
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if len(name) != 32 || !isLowerHex(name) || slices.Contains(keep, name) {
			continue
		}
		path := filepath.Join(ps.dir, "pairs", name)
		if info, err := os.Lstat(path); err != nil || info.Mode().Type() != fs.ModeDir {
			continue
		}
		_ = os.RemoveAll(path)
	}
}

// writeSynced creates path, owner-only, with data on disk before it returns.
func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// writeAtomic replaces dir/name with data through a synced temporary file renamed over it, and
// syncs dir.
func writeAtomic(dir, name string, data []byte) error {
	if err := replaceFile(dir, name, data); err != nil {
		return err
	}
	return syncDir(dir)
}

// replaceFile renames a synced temporary file with data over dir/name.
func replaceFile(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err == nil {
		target := filepath.Join(dir, name)
		err = os.Rename(tmp, target)
		// An empty directory where the file belongs holds nothing: it is replaced too.
		if info, lerr := os.Lstat(target); err != nil && lerr == nil && info.IsDir() && os.Remove(target) == nil {
			err = os.Rename(tmp, target)
		}
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
