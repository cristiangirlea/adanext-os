CREATE TABLE projects (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    source_path TEXT NOT NULL,
    created TEXT NOT NULL
);

CREATE TABLE tasks (
    id TEXT PRIMARY KEY NOT NULL,
    project TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    objective TEXT NOT NULL,
    acceptance_criteria TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN (
        'draft', 'ready', 'preparing', 'running', 'verifying', 'awaiting_review',
        'accepted', 'refinement', 'failed', 'cancel_requested', 'canceled',
        'interrupted', 'recovery_required'
    )),
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    created TEXT NOT NULL,
    updated TEXT NOT NULL
);
CREATE INDEX tasks_project ON tasks(project);

CREATE TABLE attempts (
    id TEXT PRIMARY KEY NOT NULL,
    task TEXT NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
    backend TEXT NOT NULL,
    model TEXT NOT NULL,
    effort TEXT NOT NULL,
    executor TEXT NOT NULL,
    ownership_generation INTEGER NOT NULL CHECK (ownership_generation >= 0),
    engine_sha256 TEXT NOT NULL,
    run_id TEXT,
    native_session TEXT,
    state TEXT NOT NULL,
    started TEXT NOT NULL,
    ended TEXT
);
CREATE INDEX attempts_task ON attempts(task);

CREATE TABLE events (
    attempt TEXT NOT NULL REFERENCES attempts(id) ON DELETE RESTRICT,
    seq INTEGER NOT NULL CHECK (seq > 0),
    time TEXT NOT NULL,
    kind TEXT NOT NULL,
    payload TEXT NOT NULL CHECK (json_valid(payload)),
    PRIMARY KEY (attempt, seq)
);

CREATE TABLE idempotency (
    key TEXT PRIMARY KEY NOT NULL,
    request_hash TEXT NOT NULL,
    response BLOB NOT NULL,
    created TEXT NOT NULL
);
