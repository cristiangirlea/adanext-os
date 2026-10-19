package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/ipc"
	"github.com/cristiangirlea/ada-os/internal/source"
)

// The test binary copied as git is a fake reporting the version in
// version.txt beside it.
func init() {
	exe, err := os.Executable()
	if err != nil || strings.TrimSuffix(filepath.Base(exe), ".exe") != "git" {
		return
	}
	version, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "version.txt"))
	if err != nil {
		os.Exit(2)
	}
	os.Stdout.Write(version)
	os.Exit(0)
}

func inspectThroughCoordinator(t *testing.T, path string) (int, []byte, string) {
	t.Helper()
	dir := ownerOnlyDir(t)
	name := testEndpointName(t)
	process := startHelper(t, dir, name)
	t.Cleanup(func() { process.Process.Kill(); process.Wait() })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return ipc.Dial(ctx, ipc.Config{StateDir: dir, Name: name})
	}}}
	t.Cleanup(client.CloseIdleConnections)
	body, _ := json.Marshal(map[string]string{"path": path})
	resp, err := client.Post("http://ipc/v1/projects/inspect", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data, dir
}

func gitUnavailable(t *testing.T, status int, data []byte, reason string) {
	t.Helper()
	var got struct{ Error, Message string }
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if status != 503 || got.Error != "git_unavailable" || !strings.Contains(got.Message, reason) {
		t.Fatalf("inspect = %d %s", status, data)
	}
}

func TestCoordinatorWithoutGitRunsAndAnswersGitUnavailable(t *testing.T) {
	t.Setenv("PATH", ownerOnlyDir(t))
	status, data, _ := inspectThroughCoordinator(t, ownerOnlyDir(t))
	gitUnavailable(t, status, data, "not found")
}

func TestCoordinatorRefusesAnOldGitAtStartUp(t *testing.T) {
	dir := ownerOnlyDir(t)
	name := "git"
	if runtime.GOOS == "windows" {
		name = "git.exe"
	}
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), self, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte("git version 2.44.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	status, data, _ := inspectThroughCoordinator(t, ownerOnlyDir(t))
	gitUnavailable(t, status, data, "2.44.0")
}

func TestCoordinatorInspectsARepositoryAndKeepsNoMirror(t *testing.T) {
	repo, err := filepath.EvalSymlinks(ownerOnlyDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "first"}} {
		cmd := exec.Command("git", append([]string{"-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	status, data, state := inspectThroughCoordinator(t, repo)
	if status != 200 {
		t.Fatalf("inspect = %d %s", status, data)
	}
	var got source.Inspection
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Identity.Path != repo || got.Identity.Branch != "main" || len(got.Blockers) != 0 || len(got.Files) != 1 {
		t.Fatalf("inspection = %s", data)
	}
	left, err := os.ReadDir(filepath.Join(state, "inspect"))
	if err != nil || len(left) != 0 {
		t.Fatalf("mirrors left behind: %v %v", left, err)
	}
}
