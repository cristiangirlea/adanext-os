package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodRevision = "1ca56193998ca0dd1f56e64ef86c67953771f74a"

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadPinReadsTheRepositoryRevisionAndGoVersion(t *testing.T) {
	p := writeFile(t, "engine.json", `{"repository": "github.com/cristiangirlea/harness", "revision": "`+goodRevision+`", "go": "go1.26.4"}`)
	pin, err := LoadPin(p)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Repository != "github.com/cristiangirlea/harness" || pin.Revision != goodRevision || pin.Go != "go1.26.4" {
		t.Fatalf("pin = %+v", pin)
	}
}

func TestTheRepositoryPinLoads(t *testing.T) {
	pin, err := LoadPin(filepath.Join("..", "..", "engine", "engine.json"))
	if err != nil {
		t.Fatalf("engine/engine.json: %v", err)
	}
	if pin.Repository != "github.com/cristiangirlea/harness" {
		t.Fatalf("engine/engine.json pins %s, want the Harness repository", pin.Repository)
	}
}

func TestLoadPinRefusesWhatCannotNameOneBuild(t *testing.T) {
	cases := map[string]string{
		"short revision":     `{"repository": "r", "revision": "1ca5619", "go": "go1.26.4"}`,
		"uppercase revision": `{"repository": "r", "revision": "` + strings.ToUpper(goodRevision) + `", "go": "go1.26.4"}`,
		"no repository":      `{"revision": "` + goodRevision + `", "go": "go1.26.4"}`,
		"no go version":      `{"repository": "r", "revision": "` + goodRevision + `"}`,
		"bad go version":     `{"repository": "r", "revision": "` + goodRevision + `", "go": "1.26.4"}`,
		"unknown field":      `{"repository": "r", "revision": "` + goodRevision + `", "go": "go1.26.4", "branch": "main"}`,
		"trailing data":      `{"repository": "r", "revision": "` + goodRevision + `", "go": "go1.26.4"} {}`,
		"not json":           `revision=` + goodRevision,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadPin(writeFile(t, "engine.json", content)); err == nil {
				t.Fatalf("LoadPin accepted %s", content)
			}
		})
	}
}

func TestIdentifyHashesTheBinaryBytes(t *testing.T) {
	content := "not really an executable, but bytes all the same"
	p := writeFile(t, "hrn", content)
	id, err := Identify(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	if id.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 = %s, want %s", id.SHA256, hex.EncodeToString(sum[:]))
	}
	if id.Size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", id.Size, len(content))
	}
	if !filepath.IsAbs(id.Path) {
		t.Fatalf("path %q is not absolute", id.Path)
	}
}

func TestIdentifyRefusesADirectoryAndAMissingFile(t *testing.T) {
	if _, err := Identify(t.TempDir()); err == nil {
		t.Fatal("Identify accepted a directory")
	}
	if _, err := Identify(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("Identify accepted a missing file")
	}
}

func TestIdentifyRefusesALinkEvenToARegularFile(t *testing.T) {
	target := writeFile(t, "hrn", "engine bytes")
	link := filepath.Join(t.TempDir(), "hrn-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	if _, err := Identify(link); err == nil {
		t.Fatal("Identify followed a symbolic link to the engine")
	}
}

func TestVerifyComparesTheHashCaseInsensitively(t *testing.T) {
	p := writeFile(t, "hrn", "engine bytes")
	id, err := Identify(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(p, strings.ToUpper(id.SHA256)); err != nil {
		t.Fatalf("Verify with the right hash: %v", err)
	}
	if err := Verify(p, strings.Repeat("0", 64)); err == nil {
		t.Fatal("Verify accepted the wrong hash")
	}
	if err := Verify(p, "abc"); err == nil {
		t.Fatal("Verify accepted a hash that is not 64 hex characters")
	}
}
