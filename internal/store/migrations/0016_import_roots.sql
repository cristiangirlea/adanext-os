CREATE TABLE import_roots (
    id TEXT PRIMARY KEY NOT NULL,
    path TEXT NOT NULL UNIQUE,
    label TEXT NOT NULL,
    created TEXT NOT NULL
);

CREATE TABLE import_root_grants (
    root TEXT NOT NULL REFERENCES import_roots(id) ON DELETE CASCADE,
    user TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    granted TEXT NOT NULL,
    PRIMARY KEY(root, user)
);
CREATE INDEX import_root_grants_user ON import_root_grants(user);

-- Keep the id after root removal so a chooser project cannot become a direct import.
ALTER TABLE project_sources ADD COLUMN import_root TEXT NOT NULL DEFAULT '';
