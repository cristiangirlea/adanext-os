package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const detachDepthVar = "ADA_DETACH_DEPTH"

// openDetachHelper opens the detach helper's log and refuses a second re-exec, so a
// reverted -detach=false fails the test instead of leaving a fork chain running.
func openDetachHelper(log string) *os.File {
	output, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(3)
	}
	depth, _ := strconv.Atoi(os.Getenv(detachDepthVar))
	if depth >= 2 {
		fmt.Fprintln(output, "helper refused a second re-exec")
		os.Exit(4)
	}
	os.Setenv(detachDepthVar, strconv.Itoa(depth+1))
	return output
}

func logDetachHelper(output *os.File) {
	cwd, _ := os.Getwd()
	fmt.Fprintf(output, "helper pid=%d cwd=%q args=%v\n", os.Getpid(), cwd, os.Args[1:])
}

// servingHelperLine is the log line of the re-executed coordinator.
func servingHelperLine(t *testing.T, log string) string {
	t.Helper()
	content, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "helper ") && strings.Contains(line, "-detach=false") {
			return line
		}
	}
	t.Fatalf("no re-executed coordinator in the log\n%s", content)
	return ""
}

// assertServedFromTheRoot checks the re-executed coordinator's working directory.
func assertServedFromTheRoot(t *testing.T, log string) {
	t.Helper()
	line := servingHelperLine(t, log)
	if want := fmt.Sprintf("cwd=%q", detachDir()); !strings.Contains(line, want) {
		t.Fatalf("detached coordinator %s, want %s", line, want)
	}
}

func TestDetachHelperRefusesASecondReexec(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "detach.log")
	cmd := exec.Command(os.Args[0], "-detach", "-concurrency", "0", "-state-dir", dir, "-name", testEndpointName(t))
	cmd.Env = append(os.Environ(), "ADA_DETACH_HELPER_LOG="+log, detachDepthVar+"=2")
	err := cmd.Run()
	content, _ := os.ReadFile(log)
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 4 || !bytes.Contains(content, []byte("refused a second re-exec")) {
		t.Fatalf("helper = %v\n%s", err, content)
	}
}

func TestDetachDropsATrailingTerminatorButNotAFlagValue(t *testing.T) {
	for _, tc := range []struct{ args, want []string }{
		{[]string{"-detach", "--"}, []string{"-detach"}},
		{[]string{"-detach", "-name", "a", "--"}, []string{"-detach", "-name", "a"}},
		{[]string{"-detach", "-state-dir", "--"}, []string{"-detach", "-state-dir", "--"}},
		{[]string{"-detach", "-state-dir", "--", "--"}, []string{"-detach", "-state-dir", "--"}},
		{[]string{"-detach"}, []string{"-detach"}},
	} {
		if got := detachArgs(tc.args); !slices.Equal(got, tc.want) {
			t.Errorf("detachArgs(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}
