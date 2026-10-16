//go:build fakeengine

// Test-only project/membership seeding and lifecycle observation. No production API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cristiangirlea/ada-os/internal/ipc"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 3 {
		return errors.New("usage: testseed seed|replay|holder|job state-dir [name|pid]")
	}
	if os.Args[1] == "job" {
		return job(os.Args[2])
	}
	if os.Args[1] == "holder" {
		if len(os.Args) != 4 {
			return errors.New("holder needs endpoint name")
		}
		lock, err := ipc.Acquire(ipc.Config{StateDir: os.Args[2], Name: os.Args[3]})
		var running *ipc.AlreadyRunningError
		if errors.As(err, &running) {
			return json.NewEncoder(os.Stdout).Encode(running)
		}
		if err != nil {
			return err
		}
		defer lock.Release()
		return json.NewEncoder(os.Stdout).Encode(nil)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(os.Args[2], "adanext.db"))
	if err != nil {
		return err
	}
	defer s.Close()
	if os.Args[1] == "seed" {
		_, err = s.Do(ctx, "local", "e2e-seed", "e2e-seed", nil, func(tx *store.Tx) ([]byte, error) {
			at := time.Now().UTC().Format(time.RFC3339Nano)
			for _, q := range []string{
				"INSERT OR IGNORE INTO users(id,name,created) VALUES('local','Local',?)",
				"INSERT INTO projects(id,name,source_path,created) VALUES('project','Test project','never-executed',?)",
				"INSERT INTO project_members(project,user,role,added) VALUES('project','local','owner',?)",
			} {
				if _, err := tx.ExecContext(ctx, q, at); err != nil {
					return nil, err
				}
			}
			return []byte("ok"), nil
		})
		return err
	}
	if os.Args[1] != "replay" {
		return errors.New("unknown testseed command")
	}
	if err := s.CreateTask(ctx, "local", "replay", "project", "Long replay", ""); err != nil {
		return err
	}
	if err := s.CreateAttempt(ctx, store.Attempt{ID: "replay-attempt", Task: "replay", Actor: "local", Backend: "codex", Mode: "readonly", Executor: "local", State: "ended_ok"}); err != nil {
		return err
	}
	_, err = s.Do(ctx, "local", "replay-events", "replay-events", nil, func(tx *store.Tx) ([]byte, error) {
		payload, _ := json.Marshal(strings.Repeat("replay ", 2048))
		for n := 1; n <= 900; n++ {
			if err := tx.AppendEvent(ctx, store.Event{Attempt: "replay-attempt", Seq: int64(n), Time: time.Now().UTC().Format(time.RFC3339Nano), Kind: "text", Payload: payload}); err != nil {
				return nil, err
			}
		}
		return []byte("ok"), nil
	})
	return err
}
