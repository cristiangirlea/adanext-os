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
	p := writeFile(t, "engine.json", pinJSON(`"linux/amd64": "`+strings.Repeat("0", 64)+`"`))
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

func pinJSON(sha string) string {
	return `{"repository": "github.com/cristiangirlea/harness", "revision": "` + goodRevision + `", "go": "go1.26.4", "sha256": {` + sha + `}}`
}

func TestLoadPinReadsTheHashPerPlatform(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	pin, err := LoadPin(writeFile(t, "engine.json", pinJSON(`"windows/amd64": "`+hash+`"`)))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := pin.Expected("windows", "amd64"); err != nil || got != hash {
		t.Fatalf("Expected(windows, amd64) = %q, %v", got, err)
	}
	if _, err := pin.Expected("plan9", "amd64"); err == nil {
		t.Fatal("a platform with no recorded hash was given one")
	}
}

func TestLoadPinRefusesABadHashTable(t *testing.T) {
	cases := map[string]string{
		"short hash":     `"linux/amd64": "abc"`,
		"uppercase hash": `"linux/amd64": "` + strings.Repeat("AB", 32) + `"`,
		"bad platform":   `"linux": "` + strings.Repeat("ab", 32) + `"`,
		"spaced key":     `"linux /amd64": "` + strings.Repeat("ab", 32) + `"`,
		"no hashes":      ``,
	}
	for name, sha := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadPin(writeFile(t, "engine.json", pinJSON(sha))); err == nil {
				t.Fatalf("LoadPin accepted %s", sha)
			}
		})
	}
	if _, err := LoadPin(writeFile(t, "engine.json", `{"repository": "r", "revision": "`+goodRevision+`", "go": "go1.26.4"}`)); err == nil {
		t.Fatal("LoadPin accepted a pin without hashes")
	}
}

func TestTheEmbeddedPinIsTheRepositoryPin(t *testing.T) {
	embedded, err := Pinned()
	if err != nil {
		t.Fatal(err)
	}
	file, err := LoadPin(filepath.Join("..", "..", "engine", "engine.json"))
	if err != nil {
		t.Fatal(err)
	}
	if embedded.Revision != file.Revision || len(embedded.SHA256) != len(file.SHA256) {
		t.Fatalf("embedded %+v, file %+v", embedded, file)
	}
	for _, platform := range []string{"windows/amd64", "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		if embedded.SHA256[platform] == "" || embedded.SHA256[platform] != file.SHA256[platform] {
			t.Errorf("%s: embedded %q, file %q", platform, embedded.SHA256[platform], file.SHA256[platform])
		}
	}
}

func TestBuildEngineScriptBuildsReproducibly(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "build-engine.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CGO_ENABLED=0", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "GOENV=off", "GOFLAGS=", `GOTOOLCHAIN="$want_go"`, "GOFIPS140"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("build-engine.sh lacks %s", want)
		}
	}
}

func TestLocateFindsTheEngineInTheAppDirectoryOrThroughTheFlag(t *testing.T) {
	app := t.TempDir()
	if _, found, err := Locate("", app, "linux"); err != nil || found {
		t.Fatalf("empty app directory: found=%v err=%v", found, err)
	}
	beside := filepath.Join(app, "hrn")
	if err := os.WriteFile(beside, []byte("engine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if path, found, err := Locate("", app, "linux"); err != nil || !found || path != beside {
		t.Fatalf("app directory: %q %v %v", path, found, err)
	}
	if path, found, err := Locate("", app, "windows"); err != nil || found {
		t.Fatalf("windows looks for hrn.exe: %q %v %v", path, found, err)
	}
	flag := writeFile(t, "elsewhere", "engine")
	if path, found, err := Locate(flag, app, "linux"); err != nil || !found || path != flag {
		t.Fatalf("flag: %q %v %v", path, found, err)
	}
	if _, _, err := Locate(filepath.Join(app, "absent"), app, "linux"); err == nil {
		t.Fatal("a flag naming a missing engine was accepted")
	}
}

func TestAWindowsEngineFlagWithoutExeIsRefusedAtStartup(t *testing.T) {
	plain := writeFile(t, "hrn", "engine")
	if _, _, err := Locate(plain, t.TempDir(), "windows"); err == nil || !strings.Contains(err.Error(), ".exe") {
		t.Fatalf("a Windows engine without .exe: %v", err)
	}
	upper := writeFile(t, "hrn.EXE", "engine")
	if path, found, err := Locate(upper, t.TempDir(), "windows"); err != nil || !found || path != upper {
		t.Fatalf("hrn.EXE: %q %v %v", path, found, err)
	}
}
