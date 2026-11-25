-- A project's default mode, and the decisions that let an attempt above it launch.
ALTER TABLE projects ADD COLUMN default_mode TEXT NOT NULL DEFAULT 'readonly' CHECK (default_mode IN ('readonly','auto','yolo'));

CREATE TABLE permission_decisions (
    id TEXT PRIMARY KEY NOT NULL,
    attempt TEXT NOT NULL REFERENCES attempts(id) ON DELETE RESTRICT,
    task_revision INTEGER NOT NULL,
    snapshot TEXT NOT NULL,
    backend TEXT NOT NULL,
    mode TEXT NOT NULL,
    workspace TEXT NOT NULL,
    policy_version INTEGER NOT NULL,
    generation INTEGER NOT NULL,
    decision TEXT NOT NULL CHECK (decision IN ('allow','deny')),
    decided_by TEXT NOT NULL REFERENCES users(id),
    decided TEXT NOT NULL,
    expires TEXT NOT NULL,
    idempotency_key TEXT NOT NULL
);
CREATE INDEX permission_decisions_attempt ON permission_decisions(attempt);
