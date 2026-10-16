//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
	"unsafe"

	"github.com/cristiangirlea/ada-os/internal/ipc"
	"golang.org/x/sys/windows"
)

func init() {
	if log := os.Getenv("ADA_DETACH_HELPER_LOG"); log != "" {
		output, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(3)
		}
		if !slices.Contains(os.Args, "-detach=false") && os.Getenv("ADA_DETACH_NO_JOB") == "" {
			job, err := windows.CreateJobObject(nil, nil)
			if err != nil {
				fmt.Fprintln(output, err)
				os.Exit(3)
			}
			var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
			info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
			if os.Getenv("ADA_DETACH_DENY") == "" {
				info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
			}
			if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
				fmt.Fprintln(output, err)
				os.Exit(3)
			}
			if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
				fmt.Fprintln(output, err)
				os.Exit(3)
			}
			// The launcher owns the only handle. Its exit destroys the job and
			// kills anything that failed to break away, just like a desktop job.
		}
		fmt.Fprintf(output, "helper pid=%d args=%v\n", os.Getpid(), os.Args[1:])
		os.Exit(run(context.Background(), os.Args[1:], output, output))
	}
}

type launcherJob int

const (
	jobAllowsBreakaway launcherJob = iota
	jobDeniesBreakaway
	noJob
)

func TestDetachedCoordinatorServesOutsideTheLauncherJob(t *testing.T) {
	testDetachedCoordinator(t, jobAllowsBreakaway)
}

// A desktop outside any job still hands adad its inheritable handles (Node spawns with
// inheritance on), such as the pipes its own launcher reads. The coordinator must not keep
// them: the launcher's output closes while the coordinator serves.
func TestCoordinatorOutsideAnyJobKeepsNothingTheLauncherInherited(t *testing.T) {
	testDetachedCoordinator(t, noJob)
}

func testDetachedCoordinator(t *testing.T, launcher launcherJob) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state & $() ' unicode-λ")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "detach.log")
	name := testEndpointName(t)
	cmd := exec.Command(os.Args[0], "-detach", "-state-dir", dir, "-name", name)
	cmd.Env = append(os.Environ(), "ADA_DETACH_HELPER_LOG="+log)
	switch launcher {
	case jobDeniesBreakaway:
		cmd.Env = append(cmd.Env, "ADA_DETACH_DENY=1")
	case noJob:
		cmd.Env = append(cmd.Env, "ADA_DETACH_NO_JOB=1")
	}
	cfg := ipc.Config{StateDir: dir, Name: name}
	// The launcher's output reaches EOF only when no process holds its pipe.
	type result struct {
		out []byte
		err error
	}
	launched := make(chan result, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		launched <- result{out, err}
	}()
	select {
	case r := <-launched:
		if r.err != nil {
			t.Fatalf("launcher: %v %s", r.err, r.out)
		}
	case <-time.After(15 * time.Second):
		stopCoordinator(t, cfg)
		content, _ := os.ReadFile(log)
		t.Fatalf("the launcher's output stayed open while the coordinator served\n%s", content)
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
	stopCoordinator(t, cfg)
}

func TestDesktopJobWithoutBreakawayUsesTheOwnerProcessBroker(t *testing.T) {
	testDetachedCoordinator(t, jobDeniesBreakaway)
}

func TestDesktopDetachmentRequiresBreakawayAndNeverFallsBackIntoTheJob(t *testing.T) {
	// Out of a job too: the serving process must not be the one the desktop spawned.
	calls := 0
	launched, err := detachWith([]string{"-detach", "-name", "test"}, func(cmd *exec.Cmd) error {
		calls++
		if cmd.SysProcAttr.CreationFlags&windows.CREATE_BREAKAWAY_FROM_JOB == 0 || !cmd.SysProcAttr.HideWindow {
			t.Fatal("not detached", cmd.SysProcAttr)
		}
		if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
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
	denied := errors.New("job refuses breakaway")
	launched, err = detachWith(nil, func(*exec.Cmd) error { return denied })
	if launched || !errors.Is(err, denied) {
		t.Fatalf("must refuse instead of serving inside the job: %v %v", launched, err)
	}
}
