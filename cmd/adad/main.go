package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/cristiangirlea/ada-os/internal/api"
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
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "adanext")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "adanext")
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	_ = stdout
	flags := flag.NewFlagSet("adad", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateDir := flags.String("state-dir", defaultStateDir(), "coordinator state directory")
	name := flags.String("name", "adad", "coordinator endpoint name")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *stateDir == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "adad: invalid state directory or arguments")
		return 2
	}
	if err := os.MkdirAll(*stateDir, 0700); err != nil {
		fmt.Fprintln(stderr, "adad: create state directory:", err)
		return 1
	}
	lock, err := ipc.Acquire(ipc.Config{StateDir: *stateDir, Name: *name})
	if err != nil {
		var running *ipc.AlreadyRunningError
		if errors.As(err, &running) {
			fmt.Fprintf(stderr, "adad already running as PID %d\n", running.PID)
			return 0
		}
		fmt.Fprintln(stderr, "adad: acquire lock:", err)
		return 1
	}
	defer lock.Release()
	s, err := store.Open(ctx, filepath.Join(*stateDir, "adanext.db"))
	if err != nil {
		fmt.Fprintln(stderr, "adad: open store:", err)
		return 1
	}
	defer s.Close()
	_, err = s.Do(ctx, "local", "initial-local-user", "initial-local-user-v1", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO users(id,name,created) VALUES('local','Local',?)", time.Now().UTC().Format(time.RFC3339Nano))
		return []byte("ok"), err
	})
	if err != nil {
		fmt.Fprintln(stderr, "adad: create local user:", err)
		return 1
	}
	listener, err := lock.Listen()
	if err != nil {
		fmt.Fprintln(stderr, "adad: listen:", err)
		return 1
	}
	defer listener.Close()
	hub := api.NewHub()
	server := &http.Server{Handler: api.New(s, hub, func(*http.Request) (string, error) { return "local", nil }), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 1 << 20, IdleTimeout: 60 * time.Second}
	server.RegisterOnShutdown(hub.Close)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "adad: serve:", err)
			return 1
		}
	case <-ctx.Done():
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
	return 0
}
