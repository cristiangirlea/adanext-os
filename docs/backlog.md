# Backlog

Slices in delivery order. Each is one issue, one branch and one pull request; a slice is ticked
with its pull request and date when it merges. Acceptance tests are the ones that must exist and
pass before review. M0 to M2 run one slice at a time; from M3, independent slices may run two at a
time through `hrn batch` (`docs/plan.md`, section 8).

## Delegating a slice

Every delegated run gets a task packet with: the repository and base commit, the objective, the
acceptance criteria and tests below, the files it owns, what it must not touch, the backend and
model, the timeout, and the stop conditions. The supervisor reads the diff, runs the full checks
outside the sandbox, and obtains an independent review before any pull request.

Codex's workspace-write sandbox on Windows cannot write the Go cache or temp directory or read the
user's profile. Each Go or git command a worker runs starts with this PowerShell preamble, which
keeps everything under the ignored `.hrn/` directory:

```powershell
$root=(Get-Location).Path; $h="$root\.hrn\sandbox"; New-Item -ItemType Directory -Force "$h\gocache","$h\gotmp","$h\tmp","$h\home\.config\git","$h\hrnhome" | Out-Null; $env:GOCACHE="$h\gocache"; $env:GOTMPDIR="$h\gotmp"; $env:TEMP="$h\tmp"; $env:TMP="$h\tmp"; $env:HOME="$h\home"; $env:USERPROFILE="$h\home"; $env:XDG_CONFIG_HOME="$h\home\.config"; $env:GIT_CONFIG_NOSYSTEM="1"; $env:HRN_HOME="$h\hrnhome"
```

## M0 Baseline

- [x] **M0.1 Bootstrap.** Agent setup, CI on the owner's runner, `scripts/check.sh`, the engine
  pin and `internal/engine`, desktop version pins and lockfile, plan, status and backlog.
  Acceptance: `bash scripts/check.sh` passes; the engine tests reject short revisions, unknown
  fields and wrong hashes; CI green. Done: #1, 2026-10-13, before CI could run (no runner then);
  CI green on GitHub's runners, Linux and Windows, on `main` from `e4a2c14` (#14).
- [x] **M0.2 Runner.** Superseded: infra #55 added a `lab-ada-os` scale set on 2026-10-11, and
  infra #61 removes it, since the repository is public and runs on GitHub's runners (decisions
  log, 2026-10-13).
- [x] **M0.3 AdaNext OS.** The product's name through README, AGENTS.md, the check skill and the
  desktop package; the amendments of 2026-10-13 in the status file's decisions log and in this
  backlog; a pointer at the top of `docs/plan.md`, whose text stays. The owner renamed the
  repository to `adanext-os` the same day. Acceptance: `bash scripts/check.sh` passes; the old
  name appears only in `docs/plan.md` and in the decisions log entries that record the change. Done:
  #13, 2026-10-13.
- [x] **M0.4 Public repository.** A MIT `LICENSE` and AGENTS.md saying the repository is public
  (#12, 2026-10-13); the workflow on GitHub's runners (#14). The owner flipped the visibility on
  2026-10-13; branch protection and the call on which machine details and local paths stay in
  `docs/plan.md` and the status file are owner actions 6 and 8.

## M1 Execution protocol (Harness repository)

Each slice branches from Harness `main` and follows Harness's AGENTS.md: standard library only,
records stay loadable, README and PLAN.md updated, tests first.

- [x] **M1.1 Reasoning effort for Codex and Claude.** Done: Harness #50, 2026-10-11, merge `0c96142`. Codex gets `-c model_reasoning_effort=<level>`,
  Claude gets `--effort <level>`. Levels are validated before the CLI starts. Backends that cannot
  apply an effort say so in the run, as unenforced limits already do. Acceptance: argument tests
  for both adapters through the helper process; an unknown level fails before start; old records
  load; a bounded live smoke on each CLI recorded in this file.
- [x] **M1.2 Run records surface write failures.** Done: Harness #51, 2026-10-11, merge `b9f1733`. `runlog.Writer` keeps the first event write
  error and counts lost events; `Close` returns them; the result record carries them in an
  additive field; `hrn run` and the MCP run tool report them. Acceptance: a writer whose events file
  fails mid-run reports the error and the count; records without the field still load.
- [x] **M1.3 Agent CLIs run in a process tree.** Done: Harness #52, 2026-10-11, merge `75ba625`. CLI backends start their child in the Job Object
  on Windows and a process group on Unix that `internal/tools` already uses for commands, so a
  cancel or timeout also ends grandchildren, and the run reports canceled only after the tree is
  gone. Acceptance: a helper process that starts a grandchild; after cancel the grandchild no longer
  runs, checked by PID and creation time; Windows and Linux.
- [x] **M1.4 `hrn exec --protocol v1`.** Done: Harness #53, 2026-10-11, merge `8dd2659`. A versioned NDJSON protocol on stdin and stdout for a
  supervising program. Outline:
  - hrn speaks first: `hello` with the protocol version, hrn version and VCS revision, and per
    backend its availability, modes, effort levels, session support, enforced limits, and
    `approvals: false`.
  - The caller sends one `start` with its own `attempt_id` and the task: prompt, dir, backend,
    model, mode, effort, timeout, max turns, MCP files, optional `continue_run_id`.
  - hrn answers `accepted` (run id, requested backend, model, effort and mode) or `rejected` with
    a code (`bad_request`, `unsupported`, `storage`) and exits non-zero. Anything a backend cannot
    honour is rejected, never silently ignored: effort on a backend without a mapping, mode `ask`
    with no approvals, limits the backend does not enforce.
  - Every line hrn writes carries `seq`, increasing by one from `hello`. Events wrap
    `agent.Event`; a `session` event appears as soon as a CLI reports its session id.
  - The caller may send `cancel` at any time. End of stdin also cancels: the caller lost
    ownership.
  - A storage failure while recording is reported as an `error` line with code `storage`, and the
    run is cancelled rather than continuing unrecorded.
  - The last line is `result` with the `agent.Result`, the requested and resolved model, and any
    record error; exit status 0 when the protocol completed, whatever the task's own status.
  - Lines are bounded (input 4 MiB; event text over 256 KiB is cut and marked `truncated`); the
    run record keeps the full text.
  Acceptance: protocol tests against a fake backend covering the handshake, accept and reject,
  ordering, cancel, end of stdin, storage failure, oversized lines, unsupported options,
  continuation, and old records loading; a protocol document in the Harness repository.

## M2 Durable coordinator

- [x] **M2.1 Store.** `internal/store` on modernc.org/sqlite v1.59.0: WAL, foreign keys,
  `synchronous=FULL`, numbered migrations each in a transaction, a refusal to open a newer schema.
  Tables for users, projects and their members, tasks, attempts, events and idempotency keys, with
  an actor on tasks and attempts (decisions log, 2026-10-13; reviews arrive in M7 and permission
  decisions in M2.3); the task state machine of `docs/plan.md` section 2 with a revision per task;
  consistent backups
  through the SQLite backup path. Acceptance: migrations from empty; refusing a future schema;
  every allowed and refused transition; duplicate requests with one idempotency key per user;
  the same key from two users is two requests; a stale expected revision refused; tasks and events
  listed or read only by the task project's members, including after membership removal; unknown
  actors refused by foreign keys; an attempt's actor is a member at creation. A permission prompt
  addressed to the task's actor is M2.3's acceptance, where permission decisions arrive. A helper
  process killed mid-transaction leaves a consistent database; restore from a backup.
  Done: #18, 2026-10-13, merge `36174d9`; the files private to the user on Unix: #24,
  2026-10-13, merge `09ff3bf`.
- [x] **M2.2 `adad`, singleton and IPC.** Split in two slices (decisions log, 2026-10-13): M2.2a,
  `internal/ipc` and the single-coordinator lock (#25; done: #28, 2026-10-13, merge `7e117ba`),
  and M2.2b, `cmd/adad` and the `/v1` API
  (#26; done: #31, 2026-10-13, merge `2b0bf39`). Both halves hardened by #38, #45 and #47 on
  2026-10-14.
  `cmd/adad` listens, as `docs/plan.md` section 2 says,
  on a per-user Windows named pipe whose security descriptor grants only the current user, or an
  owner-only Unix socket (mode 0600 in a 0700 directory) on Linux. How the pipe is created
  (direct Windows calls or a vetted library) is decided in the slice; a test reads the
  descriptor back and finds no other principal.
  A second `adad` finds the first and exits; a stale lock from a dead process (PID and creation
  time) is recovered. HTTP/1.1 on that pipe or socket, versioned under `/v1`: create, start,
  cancel, get and list tasks, and subscribe to events from a sequence number as server-sent
  events (decisions log, 2026-10-13: the same server takes M4b's TLS listener). Every mutating
  request carries an idempotency key and the expected task revision. M2.4a adds the owner's
  stop, `POST /v1/coordinator/stop` (empty body, idempotency key, no revision; `202` with the
  counts of running and queued work it ended; `403` for any other account), `queue_position` on
  a queued task's `GET`, `queue_full` (`409`) on a user's fourth queued start, and cancel as the
  withdrawal of a queued task (decisions log, 2026-10-15). Acceptance: duplicate start, concurrent clients, a client
  disconnecting mid-stream, reconnect from the last sequence, a malformed request, and the pipe's
  or socket's permissions.
- [x] **M2.3 Local executor.** Done as M2.3a artifacts (#32: #35, 2026-10-13, merge `7cdad00`,
  hardened by #49 on 2026-10-14) and M2.3b executor (#33: #40, 2026-10-14, merge `ff8917c`; its follow-ups are
  #39). `internal/executor` runs the pinned engine (`hrn exec --protocol v1`)
  per attempt, verified by hash, records the attempt intent before launch, ingests events in
  order, and holds an ownership generation. Artifacts are written outside the database by content
  hash, and only then referenced. Cancel reports canceled only after the engine exits.
  After an `adad` crash, an attempt that was running becomes `interrupted` and then
  `recovery_required`; it is never replayed. Acceptance: a fake engine speaking the protocol;
  cancel, engine crash, `adad` crash and restart, truncated stream, duplicate start; disk-full on
  the artifact writer surfaces and stops the task.
- [ ] **M2.4 The window can close.** A minimal Electron shell (main and preload in `desktop/`, one
  React view of tasks and events in `ui/`, the app `adad` serves in M4b) that connects to `adad`,
  or starts it independently of its own process tree.
  Acceptance, with Playwright against a real `adad` and a fake engine: start a task, close the
  window, `adad` and the task keep running, reopen, the task and its events are there; the explicit
  "Quit Ada and stop work" cancels and stops. Also the event-stream tests of #4 (paged reads, the
  payload cap and byte budgets, write deadline, stream limits, `cursor_ahead` in both forms, a
  restart during replay, the Node client's reconnect over the pipe and the socket), the `AdaClient` tests of #5, and the queue tests of #9 (FIFO launch,
  `queue_full`, a queued task cancelled before launch, "Quit" with its reason), as the decisions
  log, 2026-10-14, lists them.

## M3 Project workspaces

Four slices, one at a time in this order: each touches the schema or the API, which
`docs/plan.md` section 8 keeps sequential (decisions log, 2026-10-16). The desktop import screen
is M4.

- [ ] **M3.1 Safe git and inspection.** A new package, `internal/source`. A git runner: git
  resolved once from PATH at start-up as an absolute path (Go's `LookPath` already refuses the
  current directory); an environment built from an allow-list (PATH, SystemRoot, TEMP and TMP,
  LANG) plus `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL` set to `os.DevNull`, HOME on an Ada
  scratch directory, `GIT_TERMINAL_PROMPT=0` and `GIT_OPTIONAL_LOCKS=0`; on every call
  `-c core.hooksPath=<empty Ada dir> -c core.fsmonitor=false -c protocol.allow=never`; diffs
  always with `--no-ext-diff --no-textconv`. Git never runs against the original repository's own
  git directory, because its config can name commands (filters, fsmonitor, textconv, ssh).
  Inspection reads the layout in Go (a `.git` directory or a `gitdir:` file, and `commondir`),
  then builds a mirror git directory under Ada's state: a config Ada writes (the format version
  and the object-format extension only), HEAD, refs and packed-refs copied, a copy of the index,
  and the original's objects reached read-only through `objects/info/alternates`. Git then runs
  with `--git-dir=<mirror> --work-tree=<original>`. Filter and diff drivers named in
  `.gitattributes` are undefined there, so nothing runs. `Inspect(path)` returns the identity
  (canonical path, HEAD commit, branch or detached, remote URLs read by a minimal Go config
  reader); the status from `git status --porcelain=v2 -z --untracked-files=all`, split into
  staged, unstaged and untracked; recent history (`git log -n 20`); the instruction and manifest
  files present (AGENTS.md, CLAUDE.md, README*, go.mod, package.json and similar); and a
  fingerprint, SHA-256 over HEAD, the index bytes and, for each changed or untracked path, its
  mode, size and content hash. Import blockers, each a code and its paths: not a git repository,
  a bare repository, reftable or an unknown repository extension, a split index, sparse checkout
  (in config or as skip-worktree entries), submodules (any gitlink), Git LFS (a `filter=lfs`
  attribute), and symlinks or reparse points (junctions) that escape the work tree.
  `POST /v1/projects/inspect` returns an inspection and stores nothing.
  Acceptance, on repositories built in `t.TempDir`: a repository whose config defines a clean
  filter, fsmonitor and textconv, with a hook that writes a marker file, is inspected and no
  marker is written; each blocker; staged, unstaged and untracked are told apart; the original's
  `.git` and work tree are byte-identical before and after (every file hashed); the fingerprint
  changes when HEAD, the index or a changed file changes.
- [ ] **M3.2 Import and snapshots.** Migration 0005: the project's source identity (canonical
  path, git common directory, the identity's fingerprint at import) and `source_snapshots`
  (project, base commit, fingerprint, staged patch artifact, unstaged patch artifact, untracked
  files as a list of path, mode and artifact, `created_by`, created). `POST /v1/projects`
  imports: it inspects, refuses on blockers with `import_blocked` and their list, and creates the
  project with its creator as owner. `POST /v1/projects/{id}/snapshots` with `{untracked: [paths]}`
  captures `git diff --cached --binary` and `git diff --binary` through the mirror, and the
  selected untracked files, into artifacts. The fingerprint is taken before and after; a
  difference refuses with `source_changed` and stores nothing. Untracked selection is explicit.
  Ignored files, and paths that look like credentials (`.env*`, `*.pem`, `*.key`, `id_rsa*`,
  credentials files), are refused unless listed in `include_sensitive`.
  Acceptance: import creates the project and the owner's membership; blockers refuse; a snapshot
  reproduces the staged, unstaged and selected untracked content exactly (applied to a clone of
  the base, it gives the original's work tree for those paths); a change during capture is
  refused; the original is byte-identical before and after; idempotency and membership as for
  the other mutations.
- [ ] **M3.3 Managed clone and task workspaces.** `git clone --bare --no-local --no-hardlinks`
  from the original into `<state>/projects/<id>/repo.git`, its config replaced by Ada's; later
  imports fetch the same way. A clone through upload-pack is safe on an untrusted repository: git
  ignores `uploadpack.packObjectsHook` in repository config. A task bound to a snapshot gets a
  feature branch `ada/<task id>` from the snapshot's base commit. The attempt's worktree,
  `<state>/projects/<id>/worktrees/<attempt>`, has the snapshot applied and committed as one
  commit, "Imported working changes" (staged and unstaged as recorded), so the task's own changes
  stay apart. A versioned workspace manifest (a stored `workspaces` row, and a copy in the
  worktree's git admin directory) binds project, task, attempt, snapshot, base commit, executor
  and policy. The executor runs the engine in that worktree instead of its scratch directory; a
  missing or mismatched manifest moves the attempt to `recovery_required` and is never rebuilt
  silently.
  Acceptance: the clone shares no files or object storage with the original (no alternates, no
  hardlinks: file IDs compared); the imported-changes commit reproduces the snapshot; the
  engine's working directory is the worktree; a tampered manifest goes to recovery.
- [ ] **M3.4 Drift and launch revalidation.** `GET /v1/projects/{id}/drift` inspects the original
  again and compares it with the newest snapshot: new commits, a moved branch, changed or new
  paths, each reported. Immediately before launch the scheduler checks that the snapshot's
  artifacts still hash to their recorded values and that the workspace manifest matches the
  attempt (task revision, snapshot, ownership generation); a mismatch refuses the launch with a
  reason.
  Acceptance: each kind of drift is reported, and an unchanged original reports none; a
  corrupted snapshot artifact or a stale manifest refuses the launch.

## Later milestones

Broken into slices when M2 is done, from `docs/plan.md` sections 3 to 6.

- **M3 Project workspaces:** broken into slices; see "M3 Project workspaces" above.
- **M4 First local workflow:** desktop import, task, edit, checks, diff and review on Windows and
  Ubuntu with a real backend.
- **M4b Web access:** the TLS listener and its setting, local accounts and sessions, per-user
  project visibility and permission routing, credential profiles whose selection fails closed and
  an execution boundary that keeps other profiles and `adad`'s secrets out of a task's reach, the
  relayed provider sign-in, `home.adanext.si` with a certificate from files, then the DNS-01
  client. Evidence: Playwright from a second browser context, and a real run from a second
  computer over HTTPS (own account, project, task and provider sign-in, a permission prompt
  answered there, reconnect), the fifth lane of the v1 gate (decisions log, 2026-10-13). Also
  the tests the decisions log, 2026-10-14, gives for the browser boundary (#5: the import-root
  chooser as the only browser import, no git-URL import, and the per-request preview check),
  sessions (#6), the sign-in relay and the `permission_decisions` gate on the scheduler's
  claim, the only launch path (#7), the token, renewal and challenge reconciliation (#8), the
  owner account's stop (#9), membership removal and artifact reads (#3), and the `EventSource`
  side of `cursor_ahead` (#4).
- **M5 VMware execution:** Packer template, guest service, isolated network, guest login, previews,
  recovery. Images carry the compatibility manifest of #10, naming the source commit and the
  built artifact's SHA-256; the `adanext-images` build inspects the binaries inside the artifact
  it hashed and publishes those same bytes, and `adad` hashes the artifact it is given and
  refuses one the manifest does not match, with the tests the decisions log, 2026-10-14, lists.
- **M6 Resume intelligence:** evidence extraction, cited summaries, next-task proposals,
  continuation.
- **M7 Review and integration:** review package, branch export, destination checks.
- **M8 Recovery and routing:** the fault matrix, model policies, quota handling.
- **M9 Packaging and dogfood:** installers, the web form as a service, a download page on
  adanext.si, diagnostics, accessibility, one improvement to Ada made through Ada.
