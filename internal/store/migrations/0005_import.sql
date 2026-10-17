CREATE TABLE project_sources (
    project TEXT PRIMARY KEY NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    canonical_path TEXT NOT NULL UNIQUE CHECK (canonical_path <> ''),
    common_dir TEXT NOT NULL CHECK (common_dir <> ''),
    fingerprint TEXT NOT NULL CHECK (fingerprint <> ''),
    imported TEXT NOT NULL
);

CREATE TABLE source_snapshots (
    id TEXT PRIMARY KEY NOT NULL,
    project TEXT NOT NULL REFERENCES project_sources(project) ON DELETE RESTRICT,
    base_commit TEXT NOT NULL,
    fingerprint TEXT NOT NULL CHECK (fingerprint <> ''),
    staged_patch TEXT NOT NULL REFERENCES artifacts(hash) ON DELETE RESTRICT,
    unstaged_patch TEXT NOT NULL REFERENCES artifacts(hash) ON DELETE RESTRICT,
    created_by TEXT NOT NULL REFERENCES users(id),
    created TEXT NOT NULL
);
CREATE INDEX source_snapshots_project ON source_snapshots(project);

CREATE TABLE snapshot_untracked (
    snapshot TEXT NOT NULL REFERENCES source_snapshots(id) ON DELETE RESTRICT,
    path TEXT NOT NULL CHECK (path <> ''),
    mode TEXT NOT NULL CHECK (mode IN ('100644', '100755')),
    artifact TEXT NOT NULL REFERENCES artifacts(hash) ON DELETE RESTRICT,
    PRIMARY KEY (snapshot, path)
);
