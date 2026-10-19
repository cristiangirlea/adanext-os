//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/cristiangirlea/ada-os/internal/ipc"
	"golang.org/x/sys/windows"
)

func init() {
	if log := os.Getenv("ADA_DETACH_HELPER_LOG"); log != "" {
		output := openDetachHelper(log)
		if os.Getenv("ADA_DETACH_BROKER_HOLD") != "" {
			beforeBroker = func() { time.Sleep(2 * time.Second) }
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
		logDetachHelper(output)
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
// them: the launcher's output closes while the coordinator serves. The launcher breaks away
// from the test's own job, so a probe that skipped the re-exec outside a job would be caught.
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
	// A trailing "--" must not turn the re-exec's -detach=false into an argument.
	cmd := exec.Command(os.Args[0], "-detach", "-state-dir", dir, "-name", name, "--")
	cmd.Env = append(os.Environ(), "ADA_DETACH_HELPER_LOG="+log)
	switch launcher {
	case jobDeniesBreakaway:
		cmd.Env = append(cmd.Env, "ADA_DETACH_DENY=1")
	case noJob:
		cmd.Env = append(cmd.Env, "ADA_DETACH_NO_JOB=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB}
	}
	cfg := ipc.Config{StateDir: dir, Name: name}
	// The launcher's output reaches EOF only when no process holds its pipe.
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		if launcher == noJob && errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Skip("this test's job forbids breakaway, so the launcher cannot start outside a job")
		}
		t.Fatal(err)
	}
	launched := make(chan error, 1)
	go func() { launched <- cmd.Wait() }()
	select {
	case err := <-launched:
		if err != nil {
			t.Fatalf("launcher: %v %s", err, out.Bytes())
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
	var running *ipc.AlreadyRunningError
	if _, err := ipc.Acquire(cfg); !errors.As(err, &running) || running.PID == cmd.Process.Pid {
		stopCoordinator(t, cfg)
		t.Fatalf("the launcher itself serves: %v", err)
	}
	assertServedFromTheRoot(t, log)
	stopDetachedCoordinator(t, cfg)
}

// stopDetachedCoordinator stops the coordinator through its API and waits, by pid and
// creation time, for its process to exit: it frees its lock before it exits, and until
// then it still holds files in the state directory open.
func stopDetachedCoordinator(t *testing.T, cfg ipc.Config) {
	t.Helper()
	var running *ipc.AlreadyRunningError
	lock, err := ipc.Acquire(cfg)
	if err == nil {
		lock.Release()
	}
	if !errors.As(err, &running) {
		t.Fatalf("no coordinator holds the lock: %v", err)
	}
	stopCoordinator(t, cfg)
	if err := waitProcessGone(running.PID, running.Created, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

// waitProcessGone waits until the process with this pid and creation time (as the
// lock records it) has exited. A pid that is gone or now names another process has
// exited; any other open error is reported.
func waitProcessGone(pid int, created string, timeout time.Duration) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var creation, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exited, &kernel, &user); err != nil {
		return err
	}
	if fmt.Sprintf("%d", uint64(creation.HighDateTime)<<32|uint64(creation.LowDateTime)) != created {
		return nil
	}
	event, err := windows.WaitForSingleObject(handle, uint32(timeout/time.Millisecond))
	if err != nil {
		return err
	}
	if event != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("process %d still running after %s", pid, timeout)
	}
	return nil
}

// The broker can take seconds; the launcher's output must not wait for it.
func TestBrokerPathReleasesTheLaunchersOutputBeforeTheBroker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "detach.log")
	cfg := ipc.Config{StateDir: dir, Name: testEndpointName(t)}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	cmd := exec.Command(os.Args[0], "-detach", "-state-dir", dir, "-name", cfg.Name)
	cmd.Env = append(os.Environ(), "ADA_DETACH_HELPER_LOG="+log, "ADA_DETACH_DENY=1", "ADA_DETACH_BROKER_HOLD=1")
	cmd.Stdout, cmd.Stderr = write, write
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	write.Close()
	eof := make(chan time.Time, 1)
	go func() { io.Copy(io.Discard, read); eof <- time.Now() }()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var closed time.Time
	select {
	case closed = <-eof:
	case <-time.After(15 * time.Second):
		t.Fatal("the launcher's output stayed open")
	}
	if err := <-exited; err != nil {
		content, _ := os.ReadFile(log)
		t.Fatalf("launcher: %v\n%s", err, content)
	}
	held := time.Since(closed)
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
	stopDetachedCoordinator(t, cfg)
	// The broker is held for 2 s: output closed before it, not at the launcher's exit.
	if held < time.Second {
		content, _ := os.ReadFile(log)
		t.Fatalf("the launcher held its output until the broker returned (closed %v before exit)\n%s", held, content)
	}
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
		if cmd.Dir == "" || cmd.Dir != detachDir() || len(cmd.Dir) != 3 {
			t.Fatalf("the coordinator would keep the launcher's directory, not the system drive's root: %q", cmd.Dir)
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
