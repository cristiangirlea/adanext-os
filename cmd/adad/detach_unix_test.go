//go:build !windows

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/ipc"
)

func init() {
	if log := os.Getenv("ADA_DETACH_HELPER_LOG"); log != "" {
		output, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(3)
		}
		fmt.Fprintf(output, "helper pid=%d args=%v\n", os.Getpid(), os.Args[1:])
		os.Exit(run(context.Background(), os.Args[1:], output, output))
	}
}

// Electron hands adad every descriptor it holds without close-on-exec, beyond the stdio
// that "ignore" replaces: Chromium's own and those of whoever launched Electron, such as
// the extra channels Playwright reads. The coordinator must not keep them: the
// launcher's channel closes while the coordinator serves.
func TestCoordinatorKeepsNothingTheLauncherInherited(t *testing.T) {
	// Short enough for sun_path; the odd characters prove the re-exec's argv.
	root, err := os.MkdirTemp("", "ada")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	dir := filepath.Join(root, "s & $() ' λ")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "detach.log")
	name := testEndpointName(t)
	channel, inherited, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	cmd := exec.Command(os.Args[0], "-detach", "-state-dir", dir, "-name", name)
	cmd.Env = append(os.Environ(), "ADA_DETACH_HELPER_LOG="+log)
	// fd 3 in the launcher, without close-on-exec, as Electron's are.
	cmd.ExtraFiles = []*os.File{inherited}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	inherited.Close()
	cfg := ipc.Config{StateDir: dir, Name: name}
	defer func() {
		if lock, err := ipc.Acquire(cfg); err == nil {
			lock.Release()
		} else {
			stopCoordinator(t, cfg)
		}
	}()
	launched := make(chan error, 1)
	go func() { launched <- cmd.Wait() }()
	select {
	case err := <-launched:
		if err != nil {
			content, _ := os.ReadFile(log)
			t.Fatalf("launcher: %v\n%s", err, content)
		}
	case <-time.After(15 * time.Second):
		content, _ := os.ReadFile(log)
		t.Fatalf("the launcher kept serving instead of handing over to a clean coordinator\n%s", content)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		conn, err := ipc.Dial(ctx, cfg)
		if err == nil {
			conn.Close()
			break
		}
		if ctx.Err() != nil {
			content, _ := os.ReadFile(log)
			t.Fatalf("detached coordinator did not serve: %v\n%s", err, content)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The channel reaches EOF only when no process holds its write end.
	eof := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, channel)
		eof <- err
	}()
	select {
	case err := <-eof:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		content, _ := os.ReadFile(log)
		t.Fatalf("the launcher's channel stayed open while the coordinator served\n%s", content)
	}
}

func TestDetachRunsTheCoordinatorInANewSessionWithNullStdio(t *testing.T) {
	calls := 0
	launched, err := detachWith([]string{"-detach", "-name", "test"}, func(cmd *exec.Cmd) error {
		calls++
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
			t.Fatal("not in a new session", cmd.SysProcAttr)
		}
		if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil || len(cmd.ExtraFiles) != 0 {
			t.Fatal("inherited stdio")
		}
		if !slices.Contains(cmd.Args, "-detach=false") {
			t.Fatal("the re-exec would detach again", cmd.Args)
		}
		return nil
	})
	if err != nil || !launched || calls != 1 {
		t.Fatalf("launched=%v calls=%d err=%v", launched, calls, err)
	}
}
