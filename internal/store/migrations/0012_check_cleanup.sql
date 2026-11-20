-- #143: why removing a check's worktree was last refused, empty until it is, so a refusal is
-- recorded in its attempt's activity once per reason and not at every sweep.
ALTER TABLE check_worktrees ADD COLUMN refused TEXT NOT NULL DEFAULT '';
