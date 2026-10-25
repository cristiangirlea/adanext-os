// Package engine identifies the Harness binary Ada runs: the source revision it is pinned to,
// and the exact bytes of a build of it. Run evidence names the engine by both, so Ada never
// silently runs whichever hrn happens to be on PATH.
package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	enginepin "github.com/cristiangirlea/ada-os/engine"
)

// Pin is the Harness revision Ada is built and tested against, as engine/engine.json records it.
type Pin struct {
	Repository string `json:"repository"`
	// Revision is a full, lower-case 40-character commit id: a short id can become ambiguous,
	// and a branch name moves.
	Revision string `json:"revision"`
	// Go is the toolchain the engine is built with, as `go env GOVERSION` prints it.
	Go string `json:"go"`
	// SHA256 is the reproducible build's hash per "goos/goarch", as scripts/build-engine.sh prints it.
	SHA256 map[string]string `json:"sha256"`
}

var (
	fullRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)
	goVersion    = regexp.MustCompile(`^go[0-9]+\.[0-9]+(\.[0-9]+)?$`)
	sha256Hex    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	platform     = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)
)

// LoadPin reads a pin file and refuses anything that does not name exactly one build: a short or
// upper-case revision, a missing field, an unknown field, a malformed hash table, or trailing data.
func LoadPin(path string) (Pin, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Pin{}, err
	}
	return parsePin(path, data)
}

// Pinned is the pin built into this binary from engine/engine.json.
func Pinned() (Pin, error) {
	return parsePin("embedded engine/engine.json", enginepin.JSON)
}

func parsePin(name string, data []byte) (Pin, error) {
	var pin Pin
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pin); err != nil {
		return Pin{}, fmt.Errorf("%s: %w", name, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Pin{}, fmt.Errorf("%s: unexpected data after the pin object", name)
	}
	switch {
	case pin.Repository == "":
		return Pin{}, fmt.Errorf("%s: repository is empty", name)
	case !fullRevision.MatchString(pin.Revision):
		return Pin{}, fmt.Errorf("%s: revision %q is not a full lower-case commit id", name, pin.Revision)
	case !goVersion.MatchString(pin.Go):
		return Pin{}, fmt.Errorf("%s: go %q is not a toolchain name like go1.26.4", name, pin.Go)
	case len(pin.SHA256) == 0:
		return Pin{}, fmt.Errorf("%s: sha256 records no build", name)
	}
	for target, sum := range pin.SHA256 {
		if !platform.MatchString(target) || !sha256Hex.MatchString(sum) {
			return Pin{}, fmt.Errorf("%s: sha256 entry %q: %q is not goos/goarch with 64 lower-case hex characters", name, target, sum)
		}
	}
	return pin, nil
}

// Expected is the SHA-256 the pinned build has on goos/goarch.
func (p Pin) Expected(goos, goarch string) (string, error) {
	sum, ok := p.SHA256[goos+"/"+goarch]
	if !ok {
		return "", fmt.Errorf("engine/engine.json records no engine build for %s/%s", goos, goarch)
	}
	return sum, nil
}

// Locate finds the engine: the path a flag named, which must exist, or else hrn (hrn.exe on
// Windows) in the application's directory. found is false when neither names an engine. Nothing
// beside the binary says which hash it should have: the caller checks it against Pinned.
func Locate(flagPath, appDir, goos string) (path string, found bool, err error) {
	if flagPath != "" {
		abs, err := filepath.Abs(flagPath)
		if err != nil {
			return "", false, err
		}
		if _, err := os.Lstat(abs); err != nil {
			return "", false, err
		}
		return abs, true, nil
	}
	name := "hrn"
	if goos == "windows" {
		name = "hrn.exe"
	}
	path = filepath.Join(appDir, name)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	} else if err != nil {
		return "", false, err
	}
	return path, true, nil
}

// beforeOpen runs between Identify's check of the path and its open. Tests replace it to swap the
// file in that window; it does nothing otherwise.
var beforeOpen = func(path string) {}

// Identity is what a run's evidence records about the engine binary it used.
type Identity struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Identify hashes the binary at path. It refuses anything that is not a regular file itself: a
// directory, a symbolic link or reparse point (even one leading to a regular file), a FIFO or a
// device. The path is checked without following it, then opened without following a link and
// without blocking (openNoFollow, per platform), and the opened file must be a regular file and
// the very one checked, so a swap in between is refused rather than hashed or waited on.
func Identify(path string) (Identity, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Identity{}, err
	}
	before, err := os.Lstat(abs)
	if err != nil {
		return Identity{}, err
	}
	if !before.Mode().IsRegular() {
		return Identity{}, fmt.Errorf("%s is not a regular file (%s)", abs, before.Mode().Type())
	}
	beforeOpen(abs)
	f, err := openNoFollow(abs)
	if err != nil {
		return Identity{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Identity{}, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return Identity{}, fmt.Errorf("%s changed while it was being opened", abs)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Path: abs, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// Verify checks that the binary at path has the expected SHA-256, given as 64 hex characters in
// either case.
func Verify(path, want string) error {
	want = strings.ToLower(want)
	if !sha256Hex.MatchString(want) {
		return fmt.Errorf("expected hash %q is not 64 hex characters", want)
	}
	id, err := Identify(path)
	if err != nil {
		return err
	}
	if id.SHA256 != want {
		return fmt.Errorf("%s has sha256 %s, want %s", id.Path, id.SHA256, want)
	}
	return nil
}
