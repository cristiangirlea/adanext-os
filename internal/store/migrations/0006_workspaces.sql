ALTER TABLE tasks ADD COLUMN snapshot TEXT REFERENCES source_snapshots(id) ON DELETE RESTRICT;

CREATE TABLE workspaces (
    attempt TEXT PRIMARY KEY NOT NULL REFERENCES attempts(id) ON DELETE RESTRICT,
    project TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    task TEXT NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
    snapshot TEXT NOT NULL REFERENCES source_snapshots(id) ON DELETE RESTRICT,
    base_commit TEXT NOT NULL CHECK (base_commit <> ''),
    branch TEXT NOT NULL CHECK (branch <> ''),
    path TEXT NOT NULL CHECK (path <> ''),
    manifest_version INTEGER NOT NULL CHECK (manifest_version > 0),
    manifest_hash TEXT NOT NULL CHECK (length(manifest_hash) = 64),
    executor TEXT NOT NULL,
    policy TEXT NOT NULL,
    created TEXT NOT NULL
);
CREATE INDEX workspaces_task ON workspaces(task);
