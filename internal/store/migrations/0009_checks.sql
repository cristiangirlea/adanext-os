ALTER TABLE tasks ADD COLUMN checks_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE workspaces ADD COLUMN tested_commit TEXT NOT NULL DEFAULT '';

-- The SHA-256 of the clone config Ada last wrote for a project.
CREATE TABLE clone_configs (
    project TEXT PRIMARY KEY REFERENCES projects(id),
    sha256 TEXT NOT NULL,
    updated TEXT NOT NULL
);

CREATE TABLE check_worktrees (
    project TEXT NOT NULL,
    attempt TEXT NOT NULL,
    run_id TEXT NOT NULL,
    admin_name TEXT NOT NULL,
    created TEXT NOT NULL,
    PRIMARY KEY (project, attempt, run_id)
);

CREATE TABLE check_baselines (
    id TEXT PRIMARY KEY,
    project TEXT NOT NULL REFERENCES projects(id),
    snapshot TEXT NOT NULL REFERENCES source_snapshots(id),
    definition_hash TEXT NOT NULL,
    definition_json TEXT NOT NULL,
    tested_commit TEXT NOT NULL,
    bound_commit TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('passed', 'failed', 'timed_out', 'killed', 'not_run', 'interrupted', 'changed_tree')),
    exit_code INTEGER,
    output_artifact TEXT NOT NULL DEFAULT '',
    output_size INTEGER NOT NULL DEFAULT 0 CHECK (output_size >= 0),
    output_truncated INTEGER NOT NULL DEFAULT 0 CHECK (output_truncated IN (0, 1)),
    env_keys TEXT NOT NULL DEFAULT '[]',
    duration_ms INTEGER NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    started_at TEXT NOT NULL,
    ended_at TEXT NOT NULL,
    UNIQUE (snapshot, definition_hash)
);

CREATE TABLE check_results (
    id TEXT PRIMARY KEY,
    task TEXT NOT NULL REFERENCES tasks(id),
    attempt TEXT NOT NULL REFERENCES attempts(id),
    ord INTEGER NOT NULL CHECK (ord >= 0),
    name TEXT NOT NULL DEFAULT '',
    definition_hash TEXT NOT NULL,
    definition_json TEXT NOT NULL,
    tested_commit TEXT NOT NULL DEFAULT '',
    bound_commit TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK (state IN ('running', 'passed', 'failed', 'timed_out', 'killed', 'not_run', 'interrupted', 'changed_tree')),
    exit_code INTEGER,
    output_artifact TEXT NOT NULL DEFAULT '',
    output_size INTEGER NOT NULL DEFAULT 0 CHECK (output_size >= 0),
    output_truncated INTEGER NOT NULL DEFAULT 0 CHECK (output_truncated IN (0, 1)),
    baseline_id TEXT NOT NULL DEFAULT '',
    env_keys TEXT NOT NULL DEFAULT '[]',
    duration_ms INTEGER NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    started_at TEXT NOT NULL,
    ended_at TEXT NOT NULL DEFAULT '',
    UNIQUE (attempt, ord)
);

CREATE INDEX idx_check_results_task ON check_results(task);
CREATE INDEX idx_check_results_attempt ON check_results(attempt, ord);
