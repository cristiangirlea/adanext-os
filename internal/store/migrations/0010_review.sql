-- M4.3: the attempt a refine continues, the task's imported-changes commit and the removal of a worktree.
ALTER TABLE attempts ADD COLUMN refines TEXT;
ALTER TABLE workspaces ADD COLUMN imported_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN removed TEXT;

-- Findings any member records on the attempt a task shows.
CREATE TABLE findings (
    id TEXT PRIMARY KEY NOT NULL,
    task TEXT NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
    attempt TEXT NOT NULL REFERENCES attempts(id) ON DELETE RESTRICT,
    author TEXT NOT NULL REFERENCES users(id),
    body TEXT NOT NULL CHECK (length(body) > 0),
    created TEXT NOT NULL
);
CREATE INDEX findings_task ON findings(task);

-- An acceptance, bound to the commit, the check results and the ownership generation reviewed.
CREATE TABLE reviews (
    id TEXT PRIMARY KEY NOT NULL,
    task TEXT NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
    attempt TEXT NOT NULL REFERENCES attempts(id) ON DELETE RESTRICT,
    reviewer TEXT NOT NULL REFERENCES users(id),
    decision TEXT NOT NULL CHECK (decision IN ('accepted')),
    reviewed_commit TEXT NOT NULL CHECK (length(reviewed_commit) IN (40, 64)),
    ownership_generation INTEGER NOT NULL CHECK (ownership_generation >= 0),
    checks_json TEXT NOT NULL CHECK (json_valid(checks_json)),
    created TEXT NOT NULL
);
CREATE INDEX reviews_task ON reviews(task);
