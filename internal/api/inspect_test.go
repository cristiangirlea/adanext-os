package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cristiangirlea/ada-os/internal/source"
)

type fakeInspector struct {
	mu     sync.Mutex
	paths  []string
	result source.Inspection
	err    error
}

func (f *fakeInspector) Inspect(_ context.Context, path string) (source.Inspection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, path)
	return f.result, f.err
}

func absolute(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("repo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspectReturnsTheInspectionWithoutAnIdempotencyKey(t *testing.T) {
	inspector := &fakeInspector{result: source.Inspection{Fingerprint: "fp", Blockers: []source.Blocker{{Code: source.BlockSubmodule, Paths: []string{"sub"}, Count: 1}}}}
	f := newFixture(t, Execution{Inspector: inspector})
	path := absolute(t)
	body, _ := json.Marshal(map[string]string{"path": path})
	status, data := f.request(t, "POST", "/v1/projects/inspect", "", "", body)
	if status != 200 {
		t.Fatalf("inspect = %d %s", status, data)
	}
	var got source.Inspection
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != "fp" || len(got.Blockers) != 1 || got.Blockers[0].Code != source.BlockSubmodule {
		t.Fatalf("inspection = %+v", got)
	}
	if len(inspector.paths) != 1 || inspector.paths[0] != path {
		t.Fatalf("inspected %v", inspector.paths)
	}
}

func TestInspectRefusesARelativeOrMissingPath(t *testing.T) {
	inspector := &fakeInspector{}
	f := newFixture(t, Execution{Inspector: inspector})
	for _, body := range []string{`{"path":"relative/repo"}`, `{"path":""}`, `{}`, `{"path":"/x","extra":1}`, `not json`} {
		status, data := f.request(t, "POST", "/v1/projects/inspect", "", "", []byte(body))
		if status != 400 || !strings.Contains(string(data), `"bad_request"`) {
			t.Errorf("%s: %d %s", body, status, data)
		}
	}
	if len(inspector.paths) != 0 {
		t.Fatalf("inspected %v", inspector.paths)
	}
}

func TestInspectAnswers503WithTheReasonWhenGitIsUnavailable(t *testing.T) {
	f := newFixture(t, Execution{GitUnavailable: "git 2.45.0 lacks the 2024 clone fixes"})
	body, _ := json.Marshal(map[string]string{"path": absolute(t)})
	status, data := f.request(t, "POST", "/v1/projects/inspect", "", "", body)
	var got struct{ Error, Message string }
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if status != 503 || got.Error != "git_unavailable" || !strings.Contains(got.Message, "2.45.0") {
		t.Fatalf("inspect = %d %s", status, data)
	}
}

func TestInspectFailureIsAnInternalError(t *testing.T) {
	f := newFixture(t, Execution{Inspector: &fakeInspector{err: errors.New("git status failed")}})
	body, _ := json.Marshal(map[string]string{"path": absolute(t)})
	if status, data := f.request(t, "POST", "/v1/projects/inspect", "", "", body); status != 500 || !strings.Contains(string(data), `"internal"`) {
		t.Fatalf("inspect = %d %s", status, data)
	}
}
