package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/cristiangirlea/ada-os/internal/api"
	"github.com/cristiangirlea/ada-os/internal/artifact"
	"github.com/cristiangirlea/ada-os/internal/engine"
	"github.com/cristiangirlea/ada-os/internal/executor"
	"github.com/cristiangirlea/ada-os/internal/ipc"
	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func defaultStateDir() string {
	if runtime.GOOS == "windows" {
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, "AdaNext")
		}
		return ""
	}
	if base := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(base) {
		return filepath.Join(base, "adanext")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "adanext")
}

// The implicit local user is the coordinator's owner account: the only one
// that may stop it (decisions log, 2026-10-14, #9).
const ownerAccount = "local"

// coordinatorStop is what POST /v1/coordinator/stop drives once the owner's
// request has recorded its reason: no more launches, every running attempt
// asked to cancel, and the serve loop told to shut down and exit.
type coordinatorStop struct {
	local     *executor.Executor
	requested chan struct{}
	once      sync.Once
}

func (c *coordinatorStop) StopWork() {
	if c.local != nil {
		c.local.StopWork()
	}
	c.once.Do(func() { close(c.requested) })
}

// pinnedEngineSHA256 is the hash engine/engine.json, built into this binary, records for this
// platform. Nothing beside the engine is read for it.
func pinnedEngineSHA256() (string, error) {
	pin, err := engine.Pinned()
	if err != nil {
		return "", err
	}
	return pin.Expected(runtime.GOOS, runtime.GOARCH)
}

// The test seams for the expected hash and the application's directory.
var (
	expectedEngineSHA256 = pinnedEngineSHA256
	appDirectory         = func() (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.Dir(exe), nil
	}
)

// resolveEngine finds the engine through -engine or in the application's directory and checks
// its SHA-256 against the embedded one; an -engine-sha256 flag may only repeat that hash. With no
// engine anywhere it returns no path, and adad serves without one.
func resolveEngine(o *options) (path, hash string, err error) {
	want, wantErr := expectedEngineSHA256()
	if testEngineFlag && o.engineHash != "" {
		want, wantErr = o.engineHash, nil
	}
	if o.engineHash != "" && (wantErr != nil || o.engineHash != want) {
		return "", "", fmt.Errorf("-engine-sha256 %s is not the engine this adad was built for", o.engineHash)
	}
	dir := ""
	if o.enginePath == "" {
		if dir, err = appDirectory(); err != nil {
			return "", "", fmt.Errorf("application directory: %w", err)
		}
	}
	path, found, err := engine.Locate(o.enginePath, dir, runtime.GOOS)
	if err != nil || !found {
		return "", "", err
	}
	if wantErr != nil {
		return "", "", wantErr
	}
	if err := engine.Verify(path, want); err != nil {
		return "", "", err
	}
	return path, want, nil
}

func isLowerHex(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

type options struct {
	stateDir, name, enginePath, engineHash string
	detach                                 bool
	concurrency                            int
	preparingDeadline                      time.Duration
}

func parseFlags(args []string, output io.Writer) (*options, *flag.FlagSet, error) {
	o := &options{}
	flags := flag.NewFlagSet("adad", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&o.stateDir, "state-dir", defaultStateDir(), "coordinator state directory")
	flags.StringVar(&o.name, "name", "adad", "coordinator endpoint name")
	flags.BoolVar(&o.detach, "detach", false, "keep neither the desktop's Windows job nor any handle or descriptor it inherited; re-exec before serving")
	flags.StringVar(&o.enginePath, "engine", "", "pinned Harness engine path; default hrn in the application's directory")
	flags.StringVar(&o.engineHash, "engine-sha256", "", "optional: the pinned engine's SHA-256 again, which must equal the one built into adad")
	flags.IntVar(&o.concurrency, "concurrency", 1, "attempts launched at once across all users; one writer per workspace holds regardless")
	flags.DurationVar(&o.preparingDeadline, "preparing-deadline", 2*time.Minute, "how long a launched engine has to say hello and accept the start before the attempt fails")
	return o, flags, flags.Parse(args)
}

// executorConfig is the executor's configuration the flags decide.
func (o *options) executorConfig() executor.Config {
	return executor.Config{EnginePath: o.enginePath, EngineSHA256: o.engineHash, StateDir: o.stateDir, Concurrency: o.concurrency, PreparingDeadline: o.preparingDeadline}
}

// detachArgs drops a trailing "--" that ends the flags, so the re-exec's appended
// -detach=false stays a flag; a "--" that is a flag's value is kept.
func detachArgs(args []string) []string {
	if n := len(args); n > 0 && args[n-1] == "--" {
		if _, _, err := parseFlags(args[:n-1], io.Discard); err == nil {
			return args[:n-1]
		}
	}
	return args
}

// detachedArgs is the coordinator's command line. It runs from the root, so its paths must
// mean what the launcher meant: each one set is made absolute in o and appended last, where it
// wins over the relative value before it.
func detachedArgs(args []string, o *options) ([]string, error) {
	child := append([]string{}, detachArgs(args)...)
	for _, path := range []struct {
		flag  string
		value *string
	}{{"state-dir", &o.stateDir}, {"engine", &o.enginePath}} {
		if *path.value == "" {
			continue
		}
		abs, err := filepath.Abs(*path.value)
		if err != nil {
			return nil, fmt.Errorf("-%s: %w", path.flag, err)
		}
		*path.value = abs
		child = append(child, "-"+path.flag+"="+abs)
	}
	return child, nil
}

// startupLogLimit caps the start-up log; anything beyond it is dropped.
const startupLogLimit = 64 << 10

// startupLog keeps what a start writes to stderr, which is null for a detached
// coordinator, until the coordinator serves; then it closes and drops the rest.
type startupLog struct {
	mu   sync.Mutex
	w    io.Writer
	c    io.Closer
	left int
}

func (l *startupLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := min(len(p), l.left); n > 0 {
		written, _ := l.w.Write(p[:n])
		l.left -= max(written, n)
	}
	return len(p), nil
}

func (l *startupLog) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.left = 0
	if l.c != nil {
		l.c.Close()
		l.c = nil
	}
}

// withStartupLog writes stderr to the state directory's start-up log as well, replaced
// on every start; without a usable state directory it is stderr alone.
func withStartupLog(stateDir string, stderr io.Writer) (io.Writer, *startupLog) {
	if os.MkdirAll(stateDir, 0700) != nil {
		return stderr, nil
	}
	f, err := ipc.OpenStartupLog(stateDir)
	if err != nil {
		return stderr, nil
	}
	log := &startupLog{w: f, c: f, left: startupLogLimit}
	return io.MultiWriter(log, stderr), log
}

// failBeforeLock reports a start that failed before taking the lock. The start-up log gets
// it too, under the lock, unless another coordinator holds it: that log is the other's.
func failBeforeLock(cfg ipc.Config, stderr io.Writer, message ...any) int {
	_ = os.MkdirAll(cfg.StateDir, 0700)
	lock, err := ipc.Acquire(cfg)
	var running *ipc.AlreadyRunningError
	if errors.As(err, &running) {
		fmt.Fprintln(stderr, message...)
		return 1
	}
	if err == nil {
		defer lock.Release()
	}
	stderr, log := withStartupLog(cfg.StateDir, stderr)
	fmt.Fprintln(stderr, message...)
	log.close()
	return 1
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	_ = stdout
	opts, flags, err := parseFlags(args, stderr)
	if err != nil {
		return 2
	}
	if opts.concurrency < 1 {
		fmt.Fprintln(stderr, "adad: -concurrency must be at least 1")
		return 2
	}
	if runtime.GOOS == "linux" && opts.concurrency > 1 {
		// An orphan that left its session can't yet be told to belong to one
		// attempt rather than another (decisions log, 2026-10-26).
		fmt.Fprintln(stderr, "adad: -concurrency above 1 is refused on Linux until adad can tell which attempt a detached process belongs to")
		return 2
	}
	if opts.preparingDeadline <= 0 {
		fmt.Fprintln(stderr, "adad: -preparing-deadline must be positive")
		return 2
	}
	if opts.stateDir == "" {
		fmt.Fprintln(stderr, "adad: LOCALAPPDATA is not set; pass -state-dir")
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "adad: unexpected arguments")
		return 2
	}
	if opts.engineHash != "" && (len(opts.engineHash) != 64 || !isLowerHex(opts.engineHash)) {
		fmt.Fprintln(stderr, "adad: -engine-sha256 must be 64 lowercase hex characters")
		return 2
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "adad: stopped during start-up")
		return 0
	}
	if opts.detach {
		child, err := detachedArgs(args, opts)
		if err != nil {
			fmt.Fprintln(stderr, "adad:", err)
			return 2
		}
		launched, err := detachFromJob(child)
		if err != nil {
			// The launcher's stdio may be null or already dropped.
			return failBeforeLock(ipc.Config{StateDir: opts.stateDir, Name: opts.name}, stderr, "adad:", err)
		}
		if launched {
			return 0
		}
	}
	// Found and checked before serving: a wrong engine refuses the start rather than every launch.
	enginePath, engineHash, err := resolveEngine(opts)
	if err != nil {
		return failBeforeLock(ipc.Config{StateDir: opts.stateDir, Name: opts.name}, stderr, "adad: engine:", err)
	}
	opts.enginePath, opts.engineHash = enginePath, engineHash
	if err := os.MkdirAll(opts.stateDir, 0700); err != nil {
		fmt.Fprintln(stderr, "adad: create state directory:", err)
		return 1
	}
	lock, err := ipc.Acquire(ipc.Config{StateDir: opts.stateDir, Name: opts.name})
	if err != nil {
		var running *ipc.AlreadyRunningError
		if errors.As(err, &running) {
			// The running coordinator's start-up log is its own.
			fmt.Fprintln(stderr, "adad:", running)
			return 0
		}
		stderr, log := withStartupLog(opts.stateDir, stderr)
		fmt.Fprintln(stderr, "adad: acquire lock:", err)
		log.close()
		return 1
	}
	defer lock.Release()
	// Under the lock, so a second start never replaces this one's log.
	stderr, startup := withStartupLog(opts.stateDir, stderr)
	defer startup.close()
	s, err := store.Open(ctx, filepath.Join(opts.stateDir, "adanext.db"))
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "adad: stopped during start-up")
			return 0
		}
		fmt.Fprintln(stderr, "adad: open store:", err)
		return 1
	}
	defer s.Close()
	_, err = s.Do(ctx, ownerAccount, "initial-local-user", "initial-local-user-v1", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO users(id,name,created) VALUES(?,'Local',?)", ownerAccount, time.Now().UTC().Format(time.RFC3339Nano))
		return []byte("ok"), err
	})
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "adad: stopped during start-up")
			return 0
		}
		fmt.Fprintln(stderr, "adad: create local user:", err)
		return 1
	}
	generation, recovered, err := s.BumpAndRecover(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, "adad: recover attempts:", err)
		return 1
	}
	listener, err := lock.Listen()
	if err != nil {
		fmt.Fprintln(stderr, "adad: listen:", err)
		return 1
	}
	defer listener.Close()
	hub := api.NewHub()
	for _, id := range recovered {
		hub.Notify(id)
	}
	// Artifacts already on disk are served whether or not this coordinator
	// runs an engine: a restart without one still answers reads of past results.
	artifacts, err := artifact.Open(filepath.Join(opts.stateDir, "artifacts"))
	if err != nil {
		fmt.Fprintln(stderr, "adad: open artifacts:", err)
		return 1
	}
	stop := &coordinatorStop{requested: make(chan struct{})}
	admission := &api.Admission{}
	execution := api.Execution{Generation: generation, Artifacts: artifacts, References: s, Writer: artifacts, Owner: ownerAccount, Stopper: stop, Admission: admission}
	// Without a usable git adad still serves; only inspection answers
	// git_unavailable, and an imported project's attempt fails.
	mirrors := filepath.Join(opts.stateDir, "inspect")
	var workspaces *source.Workspaces
	_ = os.RemoveAll(mirrors) // mirrors an earlier coordinator did not finish removing
	if git, err := source.Resolve(ctx, filepath.Join(opts.stateDir, "git")); err != nil {
		fmt.Fprintln(stderr, "adad: inspection unavailable:", err)
		execution.GitUnavailable = err.Error()
	} else {
		inspector := &source.Inspector{Git: git, MirrorRoot: mirrors, Config: source.DefaultConfigFiles(git.Path)}
		execution.Inspector, execution.Capturer, execution.Drifter = inspector, inspector, inspector
		workspaces = &source.Workspaces{Inspector: inspector, Root: filepath.Join(opts.stateDir, "projects")}
		execution.Branches, execution.Review = workspaces, workspaces
	}
	var local *executor.Executor
	var stopOnce sync.Once
	var stopErr error
	stopExecutor := func() error {
		stopOnce.Do(func() {
			if local == nil {
				return
			}
			stopCtx, cancel := context.WithTimeout(context.Background(), local.StopBudget())
			defer cancel()
			stopErr = local.Stop(stopCtx)
		})
		return stopErr
	}
	if opts.enginePath != "" && opts.engineHash != "" {
		cfg := opts.executorConfig()
		cfg.Artifacts, cfg.Store, cfg.Hub, cfg.Generation, cfg.Workspaces = artifacts, s, hub, generation, workspaces
		local = executor.New(cfg)
		stop.local = local
		execution.EngineSHA256, execution.Runner, execution.Capabilities, execution.Experience = opts.engineHash, local, local, local
		defer func() {
			if err := stopExecutor(); err != nil {
				fmt.Fprintln(stderr, "adad: stop executor:", err)
			}
		}()
		// Before serving, so no launch can start a writer. A failure only
		// costs disk space, since no API path reaches an unreferenced blob.
		// Without an engine there is no sweep: a snapshot's orphans wait for a start with one.
		if _, err := local.RemoveUnreferencedArtifacts(ctx); err != nil {
			fmt.Fprintln(stderr, "adad: remove unreferenced artifacts:", err)
		}
	}
	server := &http.Server{Handler: api.New(s, hub, func(*http.Request) (string, error) { return ownerAccount, nil }, execution), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 1 << 20, IdleTimeout: 60 * time.Second}
	server.RegisterOnShutdown(hub.Close)
	serveErr := make(chan error, 1)
	startup.close()
	go func() { serveErr <- server.Serve(listener) }()
	// A signal and the owner's stop request take the same path: the stop request
	// has already recorded its reason on every attempt and asked the running ones
	// to cancel; either way the executor stops first and the HTTP server closes
	// after it, so a vanished endpoint means work has stopped.
	stopping := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-stop.requested:
		}
		close(stopping)
	}()
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "adad: serve:", err)
			return 1
		}
	case <-stopping:
		// Either path refuses starts from here: the endpoint stays up while work stops.
		admission.Close()
		// Keep the endpoint alive until cancellation and process-tree shutdown
		// finish. Its disappearance is the desktop's proof that work stopped.
		if err := stopExecutor(); err != nil {
			fmt.Fprintln(stderr, "adad: stop executor:", err)
			return 1
		}
		// Open streams first deliver what the stop committed, within the write
		// deadline, before the shutdown closes the hub under them.
		hub.Drain()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintln(stderr, "adad: shutdown:", err)
			return 1
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "adad: serve:", err)
			return 1
		}
	}
	if err := stopExecutor(); err != nil {
		fmt.Fprintln(stderr, "adad: stop executor:", err)
		return 1
	}
	return 0
}
