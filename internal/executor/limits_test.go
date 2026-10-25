package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/ada-os/internal/source"
	"github.com/cristiangirlea/ada-os/internal/store"
)

func setLimits(t *testing.T, s *store.Store, turns int, timeout string) {
	t.Helper()
	if _, err := s.Do(context.Background(), "u", "limits", "limits", nil, func(tx *store.Tx) ([]byte, error) {
		_, err := tx.ExecContext(context.Background(), "UPDATE attempts SET max_turns=?,timeout=? WHERE id='attempt'", turns, timeout)
		return []byte("ok"), err
	}); err != nil {
		t.Fatal(err)
	}
}

func startTask(t *testing.T, marker string) map[string]any {
	t.Helper()
	for _, line := range markerLines(t, marker) {
		if raw, ok := strings.CutPrefix(line, "start:"); ok {
			var start struct {
				Task map[string]any `json:"task"`
			}
			if err := json.Unmarshal([]byte(raw), &start); err != nil {
				t.Fatal(err)
			}
			return start.Task
		}
	}
	t.Fatal("the engine received no start")
	return nil
}

func TestTheStartCarriesTheAttemptsTurnAndTimeLimits(t *testing.T) {
	e, s, _, marker := testExecutor(t, "with_limits")
	setLimits(t, s, 3, "3m0s")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	task := startTask(t, marker)
	if task["max_turns"] != float64(3) || task["timeout"] != "3m0s" {
		t.Fatalf("task = %v", task)
	}
}

func TestAnAttemptWithoutLimitsSendsNone(t *testing.T) {
	e, s, _, marker := testExecutor(t, "with_limits")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_ok")
	task := startTask(t, marker)
	if _, set := task["max_turns"]; set {
		t.Fatalf("max_turns was set: %v", task)
	}
	if _, set := task["timeout"]; set {
		t.Fatalf("timeout was set: %v", task)
	}
}

func TestALimitTheEngineDoesNotOfferFailsTheAttemptBeforeTheStart(t *testing.T) {
	e, s, _, marker := testExecutor(t, "happy")
	setLimits(t, s, 3, "")
	e.LaunchAttempt("attempt")
	waitAttempt(t, s, "ended_failed")
	for _, line := range markerLines(t, marker) {
		if strings.HasPrefix(line, "start:") {
			t.Fatalf("the engine was started with a limit it does not offer: %s", line)
		}
	}
}

func waitProviderChanges(t *testing.T, s *store.Store) *store.ProviderChanges {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		task, err := s.Task(context.Background(), "u", "task")
		if err != nil {
			t.Fatal(err)
		}
		if task.Attempt.ProviderChanges != nil {
			return task.Attempt.ProviderChanges
		}
	}
	t.Fatal("the attempt's provider changes were never recorded")
	return nil
}

func TestAnAttemptThatAddsAProjectHookIsFlagged(t *testing.T) {
	e, s, _, _, _ := workspaceExecutor(t, "write_provider")
	e.cfg.CancelGrace = naturalExitGrace
	e.LaunchAttempt("attempt")
	waitAttemptPrepared(t, s, "ended_ok")
	changes := waitProviderChanges(t, s)
	if !changes.Flagged() || len(changes.Changes) != 1 || changes.Changes[0] != (store.ProviderChange{Path: ".claude/settings.json", Change: "added"}) {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestAnAttemptThatLeavesProviderConfigurationAloneIsNotFlagged(t *testing.T) {
	e, s, _, _, _ := workspaceExecutor(t, "happy")
	e.cfg.CancelGrace = naturalExitGrace
	e.LaunchAttempt("attempt")
	waitAttemptPrepared(t, s, "ended_ok")
	if changes := waitProviderChanges(t, s); changes.Flagged() {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestAnAttemptWhoseProviderWalkFailsOrIsTruncatedIsFlagged(t *testing.T) {
	for _, c := range []struct {
		name string
		walk func(root string, limit int, after bool) (map[string]string, bool, error)
	}{
		{"truncated before", func(root string, _ int, after bool) (map[string]string, bool, error) {
			if after {
				return source.ProviderState(root, providerWalkLimit)
			}
			return source.ProviderState(root, 1)
		}},
		{"truncated after", func(root string, _ int, after bool) (map[string]string, bool, error) {
			if after {
				return source.ProviderState(root, 1)
			}
			return source.ProviderState(root, providerWalkLimit)
		}},
		{"failed before", func(root string, limit int, after bool) (map[string]string, bool, error) {
			if after {
				return source.ProviderState(root, limit)
			}
			return nil, false, errors.New("unreadable")
		}},
		{"failed after", func(root string, limit int, after bool) (map[string]string, bool, error) {
			if after {
				return nil, false, errors.New("unreadable")
			}
			return source.ProviderState(root, limit)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, s, _, _, _ := workspaceExecutor(t, "happy")
			e.cfg.CancelGrace = naturalExitGrace
			var walks atomic.Int32
			e.providerState = func(root string, limit int) (map[string]string, bool, error) {
				return c.walk(root, limit, walks.Add(1) > 1)
			}
			e.LaunchAttempt("attempt")
			waitAttemptPrepared(t, s, "ended_ok")
			if changes := waitProviderChanges(t, s); !changes.Flagged() || !changes.Truncated || len(changes.Changes) != 0 {
				t.Fatalf("changes = %+v", changes)
			}
		})
	}
}
