CREATE TABLE artifacts (
    hash TEXT PRIMARY KEY CHECK(length(hash)=64),
    size INTEGER NOT NULL CHECK(size>=0),
    origin TEXT NOT NULL,
    created TEXT NOT NULL
);
