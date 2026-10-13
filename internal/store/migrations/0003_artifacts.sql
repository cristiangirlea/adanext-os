CREATE TABLE artifacts (
    hash TEXT PRIMARY KEY CHECK(length(hash)=64 AND hash NOT GLOB '*[^0-9a-f]*'),
    size INTEGER NOT NULL CHECK(size>=0),
    origin TEXT NOT NULL CHECK(origin <> ''),
    created TEXT NOT NULL
);
