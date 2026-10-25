package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/engine"
)

// TestMain makes the test binary the expected engine, since the tests run it as their fake
// engine, and keeps the test binary's own directory from being searched for one.
func TestMain(m *testing.M) {
	data, err := os.ReadFile(os.Args[0])
	if err == nil {
		sum := sha256.Sum256(data)
		expectedEngineSHA256 = func() (string, error) { return hex.EncodeToString(sum[:]), nil }
	}
	appDirectory = func() (string, error) { return filepath.Join(os.TempDir(), "adad-test-no-app-directory"), nil }
	os.Exit(m.Run())
}

func engineName() string {
	if runtime.GOOS == "windows" {
		return "hrn.exe"
	}
	return "hrn"
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func withExpected(t *testing.T, want string, app string) {
	t.Helper()
	oldExpected, oldApp := expectedEngineSHA256, appDirectory
	t.Cleanup(func() { expectedEngineSHA256, appDirectory = oldExpected, oldApp })
	expectedEngineSHA256 = func() (string, error) { return want, nil }
	appDirectory = func() (string, error) { return app, nil }
}

func TestTheExpectedEngineHashIsTheEmbeddedPinsForThisPlatform(t *testing.T) {
	pin, err := engine.Pinned()
	if err != nil {
		t.Fatal(err)
	}
	want, err := pin.Expected(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := pinnedEngineSHA256(); err != nil || got != want {
		t.Fatalf("pinnedEngineSHA256 = %q, %v; want %q", got, err, want)
	}
}

func TestAnEngineInTheAppDirectoryIsFoundAndChecked(t *testing.T) {
	app := t.TempDir()
	content := []byte("the pinned engine's bytes")
	if err := os.WriteFile(filepath.Join(app, engineName()), content, 0o755); err != nil {
		t.Fatal(err)
	}
	withExpected(t, sha(content), app)
	o := &options{}
	path, hash, err := resolveEngine(o)
	if err != nil || path != filepath.Join(app, engineName()) || hash != sha(content) {
		t.Fatalf("resolveEngine = %q %q %v", path, hash, err)
	}
	empty := t.TempDir()
	withExpected(t, sha(content), empty)
	if path, hash, err := resolveEngine(&options{}); err != nil || path != "" || hash != "" {
		t.Fatalf("no engine anywhere: %q %q %v", path, hash, err)
	}
}

func TestAWrongEngineHashIsRefused(t *testing.T) {
	app := t.TempDir()
	if err := os.WriteFile(filepath.Join(app, engineName()), []byte("some other engine"), 0o755); err != nil {
		t.Fatal(err)
	}
	withExpected(t, sha([]byte("the pinned engine's bytes")), app)
	if _, _, err := resolveEngine(&options{}); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("app directory engine with another hash: %v", err)
	}
	flagged := filepath.Join(t.TempDir(), engineName())
	if err := os.WriteFile(flagged, []byte("yet another engine"), 0o755); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	code := run(context.Background(), []string{"-state-dir", ownerOnlyDir(t), "-name", testEndpointName(t), "-engine", flagged}, &output, &output)
	if code != 1 || !strings.Contains(output.String(), "sha256") {
		t.Fatalf("run with a wrong engine = %d: %s", code, output.String())
	}
}

func TestAHashFileBesideTheEngineIsIgnored(t *testing.T) {
	app := t.TempDir()
	other := []byte("an engine someone swapped in")
	beside := filepath.Join(app, engineName())
	if err := os.WriteFile(beside, other, 0o755); err != nil {
		t.Fatal(err)
	}
	// Every name a hash beside the binary could take, each naming the swapped engine's own hash.
	for _, name := range []string{engineName() + ".sha256", "hrn.sha256", "engine.json", "SHA256SUMS"} {
		if err := os.WriteFile(filepath.Join(app, name), []byte(sha(other)+"  "+engineName()+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	withExpected(t, sha([]byte("the pinned engine's bytes")), app)
	if _, _, err := resolveEngine(&options{}); err == nil {
		t.Fatal("the hash files beside the engine were believed")
	}
	if _, _, err := resolveEngine(&options{enginePath: beside}); err == nil {
		t.Fatal("the hash files beside a flagged engine were believed")
	}
}

func TestAnEngineSHA256FlagMustBeTheEmbeddedOne(t *testing.T) {
	content := []byte("engine bytes")
	app := t.TempDir()
	if err := os.WriteFile(filepath.Join(app, engineName()), content, 0o755); err != nil {
		t.Fatal(err)
	}
	withExpected(t, sha(content), app)
	if _, hash, err := resolveEngine(&options{engineHash: sha(content)}); err != nil || hash != sha(content) {
		t.Fatalf("the embedded hash named again: %q %v", hash, err)
	}
	if _, _, err := resolveEngine(&options{engineHash: sha([]byte("other"))}); err == nil {
		t.Fatal("a flag naming another hash replaced the embedded one")
	}
}

func TestAnEngineFlagNamingNothingOrAnotherHashRefusesTheStart(t *testing.T) {
	for name, args := range map[string][]string{
		"a missing engine": {"-engine", filepath.Join(t.TempDir(), engineName())},
		"another hash":     {"-engine-sha256", strings.Repeat("a", 64)},
	} {
		var output bytes.Buffer
		code := run(context.Background(), append([]string{"-state-dir", ownerOnlyDir(t), "-name", testEndpointName(t)}, args...), &output, &output)
		if code != 1 || !strings.Contains(output.String(), "adad: engine:") {
			t.Errorf("%s: run = %d: %s", name, code, output.String())
		}
	}
}

func TestAReleaseBuildNeverTakesTheExpectedHashFromAFlag(t *testing.T) {
	if testEngineFlag {
		t.Fatal("this build takes the expected engine hash from -engine-sha256")
	}
}
