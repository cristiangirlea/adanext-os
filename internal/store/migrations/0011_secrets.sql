-- #130: a scan of an attempt's changes for credentials. Findings hold where and which rule,
-- never the matched value.
CREATE TABLE secret_scans (
    attempt TEXT PRIMARY KEY NOT NULL REFERENCES attempts(id) ON DELETE RESTRICT,
    scanned_commit TEXT NOT NULL CHECK (length(scanned_commit) IN (40, 64)),
    rules_version TEXT NOT NULL CHECK (length(rules_version) > 0),
    scan_json TEXT NOT NULL CHECK (json_valid(scan_json)),
    created TEXT NOT NULL
);

-- Paths and rules a project's reviewer said hold test values, each with the reason given. A path
-- that is not UTF-8 is matched by its bytes in base64 (path_raw); otherwise path_raw is empty.
CREATE TABLE secret_allowlist (
    project TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    path TEXT NOT NULL,
    path_raw TEXT NOT NULL DEFAULT '',
    rule TEXT NOT NULL,
    reason TEXT NOT NULL CHECK (length(trim(reason)) > 0),
    author TEXT NOT NULL REFERENCES users(id),
    created TEXT NOT NULL,
    PRIMARY KEY (project, path, path_raw, rule)
);

-- The acknowledgement of each open finding an acceptance answered.
ALTER TABLE reviews ADD COLUMN secrets_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(secrets_json));
