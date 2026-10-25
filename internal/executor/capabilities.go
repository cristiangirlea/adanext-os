package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cristiangirlea/ada-os/internal/engine"
)

// probeLifetime is how long a probe of the installed CLIs is reused.
const probeLifetime = 5 * time.Minute

// Installed reports, per backend, whether the pinned engine finds its CLI, the engine's own modes
// and options for it, and the version the CLI's own version command prints. The engine is
// verified first and run only to say hello; each CLI is run directly, never through a shell,
// with the engine's environment (decisions log, 2026-10-19) and a timeout. A result is reused for
// a few minutes, and concurrent callers share one probe.
func (e *Executor) Installed(ctx context.Context) map[string]engine.Installed {
	e.probeMu.Lock()
	defer e.probeMu.Unlock()
	if e.probed != nil && time.Since(e.probedAt) < probeLifetime {
		return e.probed
	}
	// The result is shared, so one caller leaving must not cache a failure; the probe has its own timeouts.
	e.probed, e.probedAt = e.probe(context.WithoutCancel(ctx)), time.Now()
	return e.probed
}

func (e *Executor) probe(ctx context.Context) map[string]engine.Installed {
	out := map[string]engine.Installed{}
	unavailable := func(reason string) map[string]engine.Installed {
		for _, name := range []string{"claude", "codex"} {
			out[name] = engine.Installed{Reason: reason}
		}
		return out
	}
	enginePath, err := filepath.Abs(e.cfg.EnginePath)
	if err != nil {
		return unavailable("the engine path cannot be resolved")
	}
	if err := engine.Verify(enginePath, e.cfg.EngineSHA256); err != nil {
		return unavailable("the engine failed its verification")
	}
	dir, err := os.MkdirTemp(e.cfg.StateDir, "probe-")
	if err != nil {
		return unavailable("no probe directory")
	}
	defer os.RemoveAll(dir)
	hello, err := e.hello(ctx, enginePath, dir)
	if err != nil {
		return unavailable("the engine did not say hello: " + err.Error())
	}
	for _, b := range hello.Backends {
		if b.Name != "claude" && b.Name != "codex" {
			continue
		}
		in := engine.Installed{Available: b.Available, Modes: b.Modes, Options: b.Options}
		if !b.Available {
			// The engine's detail can name directories; only the fact reaches the API.
			in.Reason = "the engine did not find the CLI or its sign-in"
		} else if reason := runnableCLI(b.Detail, runtime.GOOS); reason != "" {
			in.Reason = reason
		} else {
			in.Version, in.Reason = e.cliVersion(ctx, b.Detail, dir)
		}
		out[b.Name] = in
	}
	for _, name := range []string{"claude", "codex"} {
		if _, ok := out[name]; !ok {
			out[name] = engine.Installed{Reason: "the engine offers no such backend"}
		}
	}
	return out
}

// hello runs the engine until its first line, then ends its input; the engine exits without a
// start. The deadline bounds both.
func (e *Executor) hello(ctx context.Context, enginePath, dir string) (message, error) {
	ctx, cancel := context.WithTimeout(ctx, e.cfg.PreparingDeadline)
	defer cancel()
	args := e.cfg.EngineArgs
	if args == nil {
		args = []string{"exec", "--protocol", "v1"}
	}
	cmd := exec.CommandContext(ctx, enginePath, args...)
	cmd.Dir = dir
	cmd.Env = e.environment(dir)
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return message{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return message{}, err
	}
	if err := cmd.Start(); err != nil {
		return message{}, errors.New("the engine did not start")
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()
	line, err := bufio.NewReader(io.LimitReader(stdout, 4<<20)).ReadBytes('\n')
	if err != nil {
		return message{}, errors.New("no hello line")
	}
	var m message
	if json.Unmarshal(line, &m) != nil || m.Type != "hello" || m.Protocol != "v1" {
		return message{}, errors.New("not a v1 hello")
	}
	return m, nil
}

// runnableCLI says why a CLI path is not run, or "" when it is: an absolute regular file and, on
// Windows, an .exe, since a .cmd, .bat or .ps1 shim would need a shell. The reason names no path.
func runnableCLI(path, goos string) string {
	if !filepath.IsAbs(path) {
		return "the engine named no absolute CLI path"
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "the CLI the engine named is not a regular file"
	}
	if goos == "windows" && !strings.EqualFold(filepath.Ext(path), ".exe") {
		return "the CLI is a script shim (" + strings.ToLower(filepath.Ext(path)) + "), which Ada does not run through a shell"
	}
	return ""
}

// cliVersion runs the CLI's own version command and keeps only the version it prints.
func (e *Executor) cliVersion(ctx context.Context, path, dir string) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, e.versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, e.versionArgs...)
	cmd.Dir = dir
	cmd.Env = e.cliEnvironment()
	cmd.WaitDelay = time.Second
	var output bytes.Buffer
	cmd.Stdout = &capBuffer{buf: &output, left: 64 << 10}
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", "the version command timed out"
	}
	if err != nil {
		return "", "the version command failed"
	}
	version := engine.ParseCLIVersion(output.String())
	if version == "" {
		return "", "the version command printed no version"
	}
	return version, ""
}

// cliEnvironment is the engine's environment without its HRN_HOME.
func (e *Executor) cliEnvironment() []string {
	var env []string
	for _, entry := range e.environment("") {
		if !strings.HasPrefix(entry, "HRN_HOME=") {
			env = append(env, entry)
		}
	}
	return env
}

// capBuffer keeps the first left bytes and drops the rest.
type capBuffer struct {
	buf  *bytes.Buffer
	left int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if n := min(len(p), c.left); n > 0 {
		c.buf.Write(p[:n])
		c.left -= n
	}
	return len(p), nil
}
