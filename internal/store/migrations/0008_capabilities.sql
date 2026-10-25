-- M4.1: a start's limits, the provider configuration a snapshot holds, and what an attempt changed of it.
ALTER TABLE attempts ADD COLUMN max_turns INTEGER NOT NULL DEFAULT 0 CHECK (max_turns >= 0);
ALTER TABLE attempts ADD COLUMN timeout TEXT NOT NULL DEFAULT '';
-- JSON {"paths":[...],"count":n}; NULL for a snapshot taken before 0008.
ALTER TABLE source_snapshots ADD COLUMN provider_configuration TEXT;
-- JSON {"changes":[{"path":...,"change":...}],"truncated":false}; NULL until assessed, and for an attempt with no workspace.
ALTER TABLE attempts ADD COLUMN provider_changes TEXT;
