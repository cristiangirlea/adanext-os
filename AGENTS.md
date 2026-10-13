# Ada OS (private)

A desktop workspace that remembers a project's state, proposes the next bounded task, runs the
chosen agent on a feature branch, and brings back verified changes for review. The accepted plan is
`docs/plan.md`; progress, evidence and decisions are in `docs/implementation-status.md`; the work
queue is `docs/backlog.md`. Read all three before taking a slice, and don't reopen a recorded
decision.

## Layout

| Path | Holds |
|---|---|
| `internal/engine/` | The pinned Harness engine: its source revision and the identity of a built binary. |
| `engine/engine.json` | The Harness revision Ada runs. Change it only in a slice that says why. |
| `desktop/` | The Electron, React and TypeScript shell. Versions are pinned; code arrives in M2 and M4. |
| `scripts/check.sh` | Everything CI runs, in order. |
| `scripts/test-hooks.sh` | Tests for the two Claude hooks, run by `check.sh`. |
| `scripts/gen-settings.py` | Writes `.claude/settings.json`, giving every Bash rule its PowerShell twin. Edit the lists there, not the JSON. |
| `scripts/build-engine.sh` | Builds `hrn` from a clean Harness checkout at the pinned revision into `.engine/`. |
| `docs/` | Plan, status and backlog. |

Planned, not yet present: `cmd/adad` (the coordinator), `internal/store` (SQLite),
`internal/ipc`, `internal/executor`, and `cmd/ada-guest` (M5).

## Build and test

Go as pinned in `go.mod`; Node as pinned in `desktop/.nvmrc`. CI runs `bash scripts/check.sh`;
run the same before a pull request (`/check` does):

```bash
bash scripts/check.sh
```

It runs `go vet`, `go build`, `go test -count=1`, `go test -race` where cgo and a C compiler exist,
the repository rules (`.claude/hooks/repo-rules.sh`: gofmt, the Harness-internal import ban,
Electron isolation), a syntax check of every script, the hook tests, and `npm ci --ignore-scripts`
in `desktop/`. The owner's Windows PC has no cgo: there, `-race` is
left to CI.

The repository is public (since 2026-10-13, MIT), so CI runs on GitHub's runners, free for public
repositories: pull requests on `ubuntu-latest`, pushes to `main` on `ubuntu-latest` and
`windows-latest`. The owner's rule keeps their lab runners for private repositories; never add
one here. Keep `GOFLAGS=-p=4`, and cap Vitest and Playwright workers when they arrive.

## Rules of this repository

- **Harness stays separate.** Ada runs the pinned `hrn` binary through its versioned `exec`
  protocol. Never import `github.com/cristiangirlea/harness/internal/...`, and never use whichever
  `hrn` is on PATH: build it with `scripts/build-engine.sh` and record its hash.
- **No paid model calls in tests.** Tests use fake backends and helper processes. Live model runs
  go through `hrn` on the owner's Claude or Codex plans only, always with `-b claude` or
  `-b codex` named, small and gated; never an API key.
- **Evidence over claims.** An agent saying "done" is not acceptance. Status moves to
  `verified` only with the revision, the commands, and their results in
  `docs/implementation-status.md`. Keep `implemented` and `verified` apart.
- **The plan is preserved.** Record material changes and their reasons in the status file's
  decisions log; don't silently rewrite `docs/plan.md`.
- **Records stay loadable.** Once a SQLite schema ships, changes are forward migrations in a
  transaction; never edit an applied migration.
- **Desktop security.** Context isolation and renderer sandboxing on, Node integration off, IPC
  senders validated, a narrow typed preload bridge, previews in separate sandboxed content with no
  bridge. See `.claude/rules/desktop.md`.
- **The user's checkout is never touched.** Ada never stashes, resets, cleans or commits a
  repository it imports; it works in its own clone.

## Working rules

- One branch per slice (`m<N>/<short-name>`), one pull request, linked to its issue. Work in a
  worktree when the main checkout is busy.
- **Everything is tested before a merge**: `scripts/check.sh`, tests for every behaviour the
  change adds or alters, and CI green on GitHub's runners. Only then may an agent merge its pull
  request, and only through it (`gh pr merge <n> --merge --delete-branch --match-head-commit
  <sha>`), when an independent review ran and its findings are fixed, a security read of the diff
  has nothing open, and the head being merged is the commit all of that ran on. Never `--auto`
  or `--admin`, never push to `main`, never tag or release.
- Delegated work goes through `hrn` with a bounded task packet: base revision, objective,
  acceptance criteria, owned files, exclusions, backend and model, timeout, required tests. One
  writer per worktree. Independent slices may run two at a time from M3 (`docs/plan.md`,
  section 8).
- Commit messages: the area first, lower case, then a plain sentence
  (`engine: refuse a pin whose revision is not a full commit`). No emoji and no AI attribution
  anywhere: no `Co-Authored-By`, `Assisted-by` or "Generated with" in commits, pull requests,
  comments or docs. Use the configured git identity; check `git config user.email` first.
- Tests first, seen failing. Files are LF (`.gitattributes`). Secrets are never read, printed or
  committed. Small diffs; don't restyle what you didn't change.
