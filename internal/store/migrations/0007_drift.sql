-- For drift: a snapshot's branch ('' when detached) and per-path entries; NULL branch before 0007.
ALTER TABLE source_snapshots ADD COLUMN branch TEXT;

CREATE TABLE snapshot_paths (
    snapshot TEXT NOT NULL REFERENCES source_snapshots(id) ON DELETE RESTRICT,
    path TEXT NOT NULL CHECK (path <> ''),
    untracked INTEGER NOT NULL CHECK (untracked IN (0, 1)),
    entry TEXT NOT NULL CHECK (length(entry) = 64),
    PRIMARY KEY (snapshot, path)
);

-- Manifest version 2 binds these; a version 1 row has neither.
ALTER TABLE workspaces ADD COLUMN task_revision INTEGER;
ALTER TABLE workspaces ADD COLUMN ownership_generation INTEGER;
