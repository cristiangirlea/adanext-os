ALTER TABLE attempts ADD COLUMN mode TEXT NOT NULL DEFAULT '';
CREATE TABLE coordinator (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    generation INTEGER NOT NULL CHECK (generation >= 0)
);
INSERT INTO coordinator(id, generation) VALUES(1, 0);
