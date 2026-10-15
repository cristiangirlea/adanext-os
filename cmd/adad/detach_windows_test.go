//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
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
		if !slices.Contains(os.Args, "-detach=false") {
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

func TestDetachedCoordinatorServesOutsideTheLauncherJob(t *testing.T) {
	testDetachedCoordinator(t, false)
}

func testDetachedCoordinator(t *testing.T, denyBreakaway bool) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state & $() ' unicode-λ")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "detach.log")
	name := testEndpointName(t)
	cmd := exec.Command(os.Args[0], "-detach", "-state-dir", dir, "-name", name)
	cmd.Env = append(os.Environ(), "ADA_DETACH_HELPER_LOG="+log)
	if denyBreakaway {
		cmd.Env = append(cmd.Env, "ADA_DETACH_DENY=1")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launcher: %v %s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := ipc.Config{StateDir: dir, Name: name}
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
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return ipc.Dial(ctx, cfg) }}}
	defer client.CloseIdleConnections()
	req, _ := http.NewRequest("POST", "http://ipc/v1/coordinator/stop", nil)
	req.Header.Set("Idempotency-Key", "stop-detached")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatal(resp.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		lock, err := ipc.Acquire(cfg)
		if err == nil {
			lock.Release()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("detached coordinator did not exit")
}

func TestDesktopJobWithoutBreakawayUsesTheOwnerProcessBroker(t *testing.T) {
	testDetachedCoordinator(t, true)
}

func TestDesktopDetachmentRequiresBreakawayAndNeverFallsBackIntoTheJob(t *testing.T) {
	for _, inJob := range []bool{false, true} {
		calls := 0
		launched, err := detachWith([]string{"-detach", "-name", "test"}, func() (bool, error) { return inJob, nil }, func(cmd *exec.Cmd) error {
			calls++
			if cmd.SysProcAttr.CreationFlags&windows.CREATE_BREAKAWAY_FROM_JOB == 0 || !cmd.SysProcAttr.HideWindow {
				t.Fatal("not detached", cmd.SysProcAttr)
			}
			if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
				t.Fatal("inherited stdio")
			}
			return nil
		})
		if err != nil || launched != inJob || (calls == 1) != inJob {
			t.Fatalf("job=%v launched=%v calls=%d err=%v", inJob, launched, calls, err)
		}
	}
	denied := errors.New("job refuses breakaway")
	launched, err := detachWith(nil, func() (bool, error) { return true, nil }, func(*exec.Cmd) error { return denied })
	if launched || !errors.Is(err, denied) {
		t.Fatalf("must refuse instead of serving inside the job: %v %v", launched, err)
	}
}
