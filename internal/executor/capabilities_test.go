package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHelperProbeEngine says hello as the pinned engine does, with claude found at the test
// binary and codex not found, then exits when its input ends.
func TestHelperProbeEngine(t *testing.T) {
	if os.Getenv("ADA_PROBE_ENGINE") == "" {
		return
	}
	claude := map[string]any{"name": "claude", "available": true, "detail": os.Args[0], "modes": []string{"readonly", "auto", "yolo"}, "options": []string{"model", "mode", "effort", "timeout", "max_turns", "mcp"}, "approvals": false}
	codex := map[string]any{"name": "codex", "available": false, "detail": `codex not found in C:\Users\someone\AppData`, "modes": []string{"readonly", "auto", "yolo"}, "options": []string{"model", "mode", "effort", "timeout"}, "approvals": false}
	b, _ := json.Marshal(map[string]any{"v": 1, "seq": 1, "type": "hello", "protocol": "v1", "backends": []any{claude, codex}})
	fmt.Println(string(b))
	_, _ = os.Stdin.Read(make([]byte, 1))
	os.Exit(2)
}

// TestHelperFakeCLI answers a version command, recording the environment it was given.
func TestHelperFakeCLI(t *testing.T) {
	behaviour := os.Getenv("ADA_FAKE_CLI")
	if behaviour == "" {
		return
	}
	var names []string
	for _, entry := range os.Environ() {
		names = append(names, strings.SplitN(entry, "=", 2)[0])
	}
	_ = os.WriteFile(os.Getenv("ADA_FAKE_CLI_ENV"), []byte(strings.Join(names, "\n")), 0o600)
	if behaviour == "hang" {
		time.Sleep(time.Hour)
	}
	fmt.Println("2.1.284 (Claude Code)")
	os.Exit(0)
}

func probeExecutor(t *testing.T, behaviour string) (*Executor, string) {
	t.Helper()
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	dir := t.TempDir()
	envFile := filepath.Join(dir, "cli-env")
	e := New(Config{EnginePath: os.Args[0], EngineSHA256: hex.EncodeToString(sum[:]), EngineArgs: []string{"-test.run=^TestHelperProbeEngine$"}, StateDir: dir,
		Env: []string{"ADA_PROBE_ENGINE=1", "ADA_FAKE_CLI=" + behaviour, "ADA_FAKE_CLI_ENV=" + envFile}})
	e.versionArgs = []string{"-test.run=^TestHelperFakeCLI$"}
	return e, envFile
}

func TestTheProbeTakesEachCLIsVersionFromItsOwnVersionCommand(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "probe-secret")
	t.Setenv("ADANEXT_SOMETHING", "adad's own")
	e, envFile := probeExecutor(t, "answer")
	installed := e.Installed(context.Background())
	claude := installed["claude"]
	if !claude.Available || claude.Version != "2.1.284" || claude.Reason != "" {
		t.Fatalf("claude = %+v", claude)
	}
	if strings.Join(claude.Modes, ",") != "readonly,auto,yolo" || len(claude.Options) != 6 {
		t.Fatalf("claude's engine answers = %+v", claude)
	}
	codex := installed["codex"]
	if codex.Available || codex.Version != "" || codex.Reason == "" || strings.Contains(codex.Reason, `C:\`) {
		t.Fatalf("codex = %+v: a missing CLI is unavailable, with a reason that holds no path", codex)
	}
	names, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, refused := range []string{"ANTHROPIC_API_KEY", "ADANEXT_SOMETHING", "HRN_HOME"} {
		if strings.Contains("\n"+string(names)+"\n", "\n"+refused+"\n") {
			t.Errorf("the version command saw %s", refused)
		}
	}
	if !strings.Contains(string(names), "PATH") {
		t.Errorf("the version command lacks the #58 environment: %s", names)
	}
	// A second call within the cache's life does not run the commands again.
	if err := os.Remove(envFile); err != nil {
		t.Fatal(err)
	}
	e.Installed(context.Background())
	if _, err := os.Stat(envFile); err == nil {
		t.Fatal("the probe ran again inside its cache lifetime")
	}
}

func TestAVersionCommandThatHangsIsStoppedAtItsTimeout(t *testing.T) {
	e, _ := probeExecutor(t, "hang")
	e.versionTimeout = 500 * time.Millisecond
	start := time.Now()
	claude := e.Installed(context.Background())["claude"]
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("the probe took %s", elapsed)
	}
	if claude.Version != "" || !strings.Contains(claude.Reason, "version") {
		t.Fatalf("claude = %+v", claude)
	}
}

func TestTheProbeRefusesAnEngineWithAnotherHash(t *testing.T) {
	e, envFile := probeExecutor(t, "answer")
	e.cfg.EngineSHA256 = strings.Repeat("0", 64)
	installed := e.Installed(context.Background())
	if claude := installed["claude"]; claude.Available || !strings.Contains(claude.Reason, "engine") {
		t.Fatalf("claude = %+v", claude)
	}
	if _, err := os.Stat(envFile); err == nil {
		t.Fatal("a CLI ran behind an unverified engine")
	}
}

func TestAScriptShimIsNotRunThroughAShell(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"claude.cmd", "claude.bat", "claude.ps1", "claude"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("@echo off"), 0o755); err != nil {
			t.Fatal(err)
		}
		if reason := runnableCLI(p, "windows"); reason == "" || strings.Contains(reason, dir) {
			t.Errorf("windows %s: reason %q", name, reason)
		}
	}
	exe := filepath.Join(dir, "claude.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	if reason := runnableCLI(exe, "windows"); reason != "" {
		t.Errorf("windows .exe refused: %s", reason)
	}
	if reason := runnableCLI("relative/claude", "linux"); reason == "" {
		t.Error("a relative path was accepted")
	}
	if reason := runnableCLI(dir, "linux"); reason == "" {
		t.Error("a directory was accepted")
	}
}
