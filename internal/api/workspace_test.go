package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
)

// startAs starts a task as user at the given revision.
func (p *projectsFixture) startAs(t *testing.T, user, key, task string, revision int) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", p.url+"/v1/tasks/"+task+"/start", bytes.NewReader([]byte(policy)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-User", user)
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("If-Match", fmt.Sprint(revision))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

type createdTask struct {
	ID       string `json:"id"`
	Snapshot string `json:"snapshot"`
}

// readyTask creates a task in project, bound to snapshot unless it is empty,
// and readies it.
func (p *projectsFixture) readyTask(t *testing.T, key, project, snapshot string) createdTask {
	t.Helper()
	body := map[string]string{"project": project, "objective": "work", "criteria": "done"}
	if snapshot != "" {
		body["snapshot"] = snapshot
	}
	status, data := p.call(t, "alice", "POST", "/v1/tasks", key+"-create", body)
	if status != 201 {
		t.Fatalf("create = %d %s", status, data)
	}
	task := decodeJSON[createdTask](t, data)
	req, err := http.NewRequest("POST", p.url+"/v1/tasks/"+task.ID+"/ready", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-User", "alice")
	req.Header.Set("Idempotency-Key", key+"-ready")
	req.Header.Set("If-Match", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ready = %d", resp.StatusCode)
	}
	return task
}

func (p *projectsFixture) snapshotOf(t *testing.T, key, project string) string {
	t.Helper()
	status, data := p.call(t, "alice", "POST", "/v1/projects/"+project+"/snapshots", key, map[string]any{"untracked": []string{}})
	if status != 201 {
		t.Fatalf("snapshot = %d %s", status, data)
	}
	return decodeJSON[struct {
		ID string `json:"id"`
	}](t, data).ID
}

func withEngine(runner *recordingRunner) func(*Execution) {
	return func(e *Execution) { e.EngineSHA256, e.Runner = "engine", runner }
}

func TestATaskIsBoundToASnapshotOfItsOwnProject(t *testing.T) {
	p := newProjectsFixture(t)
	project := p.importRepo(t, "alice", "import", newRepo(t))
	snapshot := p.snapshotOf(t, "snap", project.ID)
	task := p.readyTask(t, "bound", project.ID, snapshot)
	if task.Snapshot != snapshot {
		t.Fatalf("created = %+v", task)
	}
	status, data := p.call(t, "alice", "GET", "/v1/tasks/"+task.ID, "", nil)
	if status != 200 || decodeJSON[createdTask](t, data).Snapshot != snapshot {
		t.Fatalf("get = %d %s", status, data)
	}
	// Another project's snapshot, an unknown one, and a project the caller is not in.
	for _, c := range []struct{ user, project, snapshot string }{{"alice", "a", snapshot}, {"alice", project.ID, "missing"}, {"bob", project.ID, snapshot}} {
		status, data := p.call(t, c.user, "POST", "/v1/tasks", "refused-"+c.user+c.project+c.snapshot, map[string]string{"project": c.project, "objective": "work", "snapshot": c.snapshot})
		if status != 404 {
			t.Errorf("%+v: %d %s", c, status, data)
		}
	}
	if n := p.count(t, "SELECT count(*) FROM tasks WHERE snapshot IS NOT NULL"); n != 1 {
		t.Fatalf("%d bound tasks", n)
	}
}

func TestStartingAnImportedProjectsTaskNeedsASnapshotWithACommit(t *testing.T) {
	runner := &recordingRunner{}
	p := newProjectsFixture(t, withEngine(runner))
	project := p.importRepo(t, "alice", "import", newRepo(t))
	unbound := p.readyTask(t, "unbound", project.ID, "")
	if status, data := p.startAs(t, "alice", "start-unbound", unbound.ID, 1); status != 409 || !bytes.Contains(data, []byte(`"snapshot_required"`)) {
		t.Fatalf("unbound start = %d %s", status, data)
	}

	empty := canonicalTemp(t)
	runGit(t, empty, "init", "-q")
	writeFile(t, filepath.Join(empty, "a.txt"), "a\n")
	runGit(t, empty, "add", "a.txt")
	unborn := p.importRepo(t, "alice", "import-unborn", empty)
	task := p.readyTask(t, "unborn", unborn.ID, p.snapshotOf(t, "snap-unborn", unborn.ID))
	if status, data := p.startAs(t, "alice", "start-unborn", task.ID, 1); status != 409 || !bytes.Contains(data, []byte(`"unborn_base"`)) {
		t.Fatalf("unborn start = %d %s", status, data)
	}
	if n := p.count(t, "SELECT count(*) FROM attempts"); n != 0 || len(runner.launches()) != 0 {
		t.Fatalf("%d attempts, launches %v", n, runner.launches())
	}

	bound := p.readyTask(t, "bound", project.ID, p.snapshotOf(t, "snap", project.ID))
	if status, data := p.startAs(t, "alice", "start-bound", bound.ID, 1); status != 200 {
		t.Fatalf("bound start = %d %s", status, data)
	}
	// A seeded project with no imported source keeps the scratch directory.
	seeded := p.readyTask(t, "seeded", "a", "")
	if status, data := p.startAs(t, "alice", "start-seeded", seeded.ID, 1); status != 200 {
		t.Fatalf("seeded start = %d %s", status, data)
	}
	if len(runner.launches()) != 2 {
		t.Fatalf("launches %v", runner.launches())
	}
}
