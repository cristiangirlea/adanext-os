-- Every origin that stored an artifact's content, so content several owners stored is read
-- through each; artifacts.origin keeps only the first.
CREATE TABLE artifact_refs (
    hash TEXT NOT NULL REFERENCES artifacts(hash) ON DELETE RESTRICT,
    origin TEXT NOT NULL CHECK(origin <> ''),
    PRIMARY KEY (hash, origin)
) WITHOUT ROWID;
INSERT INTO artifact_refs(hash, origin) SELECT hash, origin FROM artifacts;
