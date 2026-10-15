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
	"github.com/cristiangirlea/ada-os/internal/executor"
	"github.com/cristiangirlea/ada-os/internal/ipc"
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

func isLowerHex(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	_ = stdout
	flags := flag.NewFlagSet("adad", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateDir := flags.String("state-dir", defaultStateDir(), "coordinator state directory")
	name := flags.String("name", "adad", "coordinator endpoint name")
	detach := flags.Bool("detach", false, "refuse to inherit the desktop's Windows job; re-exec with breakaway before serving")
	enginePath := flags.String("engine", "", "pinned Harness engine path")
	engineHash := flags.String("engine-sha256", "", "pinned Harness engine SHA-256")
	concurrency := flags.Int("concurrency", 1, "attempts launched at once across all users; one writer per workspace holds regardless")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *concurrency < 1 {
		fmt.Fprintln(stderr, "adad: -concurrency must be at least 1")
		return 2
	}
	if *stateDir == "" {
		fmt.Fprintln(stderr, "adad: LOCALAPPDATA is not set; pass -state-dir")
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "adad: unexpected arguments")
		return 2
	}
	if (*enginePath == "") != (*engineHash == "") || (*engineHash != "" && (len(*engineHash) != 64 || !isLowerHex(*engineHash))) {
		fmt.Fprintln(stderr, "adad: -engine and -engine-sha256 must be paired; SHA-256 must be 64 lowercase hex characters")
		return 2
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "adad: stopped during start-up")
		return 0
	}
	if *detach {
		launched, err := detachFromJob(args)
		if err != nil {
			fmt.Fprintln(stderr, "adad:", err)
			return 1
		}
		if launched {
			return 0
		}
	}
	if err := os.MkdirAll(*stateDir, 0700); err != nil {
		fmt.Fprintln(stderr, "adad: create state directory:", err)
		return 1
	}
	lock, err := ipc.Acquire(ipc.Config{StateDir: *stateDir, Name: *name})
	if err != nil {
		var running *ipc.AlreadyRunningError
		if errors.As(err, &running) {
			fmt.Fprintln(stderr, "adad:", running)
			return 0
		}
		fmt.Fprintln(stderr, "adad: acquire lock:", err)
		return 1
	}
	defer lock.Release()
	s, err := store.Open(ctx, filepath.Join(*stateDir, "adanext.db"))
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
	artifacts, err := artifact.Open(filepath.Join(*stateDir, "artifacts"))
	if err != nil {
		fmt.Fprintln(stderr, "adad: open artifacts:", err)
		return 1
	}
	stop := &coordinatorStop{requested: make(chan struct{})}
	execution := api.Execution{Generation: generation, Artifacts: artifacts, References: s, Owner: ownerAccount, Stopper: stop}
	var local *executor.Executor
	if *enginePath != "" && *engineHash != "" {
		local = executor.New(executor.Config{EnginePath: *enginePath, EngineSHA256: *engineHash, StateDir: *stateDir, Artifacts: artifacts, Store: s, Hub: hub, Generation: generation, Concurrency: *concurrency})
		stop.local = local
		execution.EngineSHA256, execution.Runner = *engineHash, local
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), local.StopBudget())
			defer cancel()
			if err := local.Stop(stopCtx); err != nil {
				fmt.Fprintln(stderr, "adad: stop executor:", err)
			}
		}()
	}
	server := &http.Server{Handler: api.New(s, hub, func(*http.Request) (string, error) { return ownerAccount, nil }, execution), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 1 << 20, IdleTimeout: 60 * time.Second}
	server.RegisterOnShutdown(hub.Close)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	// A signal and the owner's stop request both end the serve loop the same
	// way; the stop request has already recorded its reason on every attempt
	// and asked the running ones to cancel, the signal path leaves that to
	// the executor's orderly stop below.
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
	if local != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), local.StopBudget())
		defer cancel()
		if err := local.Stop(stopCtx); err != nil {
			fmt.Fprintln(stderr, "adad: stop executor:", err)
			return 1
		}
	}
	return 0
}
