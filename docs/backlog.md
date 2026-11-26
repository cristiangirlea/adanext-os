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
- [x] **M2.4 The window can close.** Done as M2.4a queue (#64: #70, 2026-10-15, merge
  `5e03a28`), M2.4b UI and shell (done: #68, 2026-10-15, merge `a73fae4`) and M2.4c window
  lifetime (#75 with #69: #76, 2026-10-16, merge `4ef4e5c`). The browser adapter's share of #4
  and #5 (`EventSource`, the tests against both adapters) stays with M4b, as the decisions log,
  2026-10-16, says.
  A minimal Electron shell (main and preload in `desktop/`, one
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

- [x] **M3.1 Safe git and inspection** (#80: #81, 2026-10-17). A new package, `internal/source`.
  The git runner: git resolved once from PATH at start-up as an absolute path (Go's `LookPath`
  already refuses the current directory), and git without the May 2024 clone fixes refused at
  start-up (`git version`); an environment built from an allow-list (PATH, SystemRoot, TEMP and
  TMP, LANG) plus `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL` set to `os.DevNull`, HOME on an Ada
  scratch directory, `GIT_TERMINAL_PROMPT=0`, `GIT_OPTIONAL_LOCKS=0`, `GIT_NO_LAZY_FETCH=1`,
  `GIT_NO_REPLACE_OBJECTS=1` and `GIT_ATTR_NOSYSTEM=1`; on every call `--no-pager
  -c core.hooksPath=<empty Ada dir> -c core.fsmonitor=false -c core.untrackedCache=false
  -c credential.helper= -c protocol.allow=never` (the M3.3 clone and fetch alone add `-c protocol.file.allow=always`);
  diffs always with `--no-ext-diff --no-textconv`; status and diff always with
  `--ignore-submodules=all` and `-c diff.ignoreSubmodules=all`. No git process ever uses the
  original's git directory as its repository, because its config can name commands (filters,
  fsmonitor, textconv, ssh).
  The layout is read in Go: a `.git` directory or a `gitdir:` file, `commondir`, the alternates
  chain, `core.bare` and `core.worktree`, and the index, where gitlinks, unmerged entries and
  skip-worktree entries are found before any git runs. A UNC or device path (`\\host\`, `\\?\`,
  `\\.\`) in `gitdir:`, `commondir` or the alternates chain is refused without being opened,
  since reading one sends the user's credentials over SMB. On Windows the user's files are opened
  with `FILE_SHARE_DELETE`, so a handle Ada holds never fails the user's `index.lock` rename.
  The mirror git directory under Ada's state: a config Ada writes, with the format version, the
  object-format extension, and an allow-list of data-only keys copied from the repository, global
  and system config as the Go reader resolves them (`core.filemode`, `core.symlinks`,
  `core.ignorecase`, `core.precomposeunicode`, `core.autocrlf`, `core.eol`, `core.safecrlf`,
  `core.checkRoundtripEncoding`, `core.checkStat`, `core.trustctime`); `info/exclude`,
  `info/attributes` and the global excludes file (`core.excludesFile`) copied, so ignored and
  untracked match the user's git; HEAD, refs, packed-refs and `shallow` copied; a copy of the
  index that keeps the original's mtime (a newer one would hide racily clean edits); and the
  original's objects reached read-only through `objects/info/alternates`. Git then runs with
  `--git-dir=<mirror> --work-tree=<original>`. Filter and diff drivers named in `.gitattributes`
  are undefined there, so nothing runs.
  `Inspect(path)` returns the identity (canonical path, HEAD commit, branch or detached, remote
  URLs read by a minimal Go config reader, which never follows `include` or `includeIf`); the status from `git status --porcelain=v2 -z
  --untracked-files=all`, split into staged, unstaged and untracked; recent history
  (`git log -n 20`); the instruction and manifest files present (AGENTS.md, CLAUDE.md, README*,
  go.mod, package.json and similar); and a fingerprint, SHA-256 over HEAD, the parsed index
  entries (path, mode, object ID, stage, flags; not the stat data, which any `git status` an
  editor runs rewrites) and, for each changed or untracked path, its mode, size and content hash
  (by Lstat: regular files, and link targets as text).
  Import blockers, each a code and its paths: not a git repository; a bare repository or
  `core.bare` set; `core.worktree` set (the work tree is then not the selected directory); a
  partial clone (`extensions.partialClone`: objects are missing); reftable or an unknown
  repository extension; a split index; sparse checkout (in config or as skip-worktree entries);
  a merge, rebase, cherry-pick, revert or bisect in progress, or unmerged entries (they cannot be
  shown as a staged and an unstaged patch); assume-unchanged entries (git skips those files, so
  their edits would not show); submodules (any gitlink); Git LFS (a `filter=lfs`
  attribute in any `.gitattributes` in the tree or in `info/attributes`); and symlinks or
  redirecting reparse points (symlinks or junctions) that escape the work tree.
  `POST /v1/projects/inspect` returns an inspection and stores nothing.
  Acceptance, on repositories built in `t.TempDir`: a repository whose config defines a clean
  filter, fsmonitor and textconv, with a hook that writes a marker file, is inspected and no
  marker is written; a populated submodule whose own config defines fsmonitor and a clean filter
  writes no marker; the mirror config holds only allow-listed keys (a filter or fsmonitor set in
  the repository, global or system config is not copied); git without the May 2024 clone fixes is
  refused at start-up; a UNC or device path in `gitdir:`, `commondir` or alternates is refused;
  each blocker, including each kind of operation in progress and an unmerged entry; a shallow
  repository inspects with its history; a path ignored only by the global excludes file is
  ignored; staged, unstaged and untracked are told apart; the original's `.git` and work tree
  are byte-identical before and after (every file hashed); the index copy keeps the original's
  mtime; the fingerprint changes when HEAD, the index entries or a changed file changes, and not
  when a `git status` in the original rewrites only stat data. On Windows CI: with
  `core.autocrlf=true` and `core.filemode=false`, a clean checkout inspects clean and a one-line
  edit gives a one-line patch; the user's git can replace its index while Ada holds it open.
- [x] **M3.2 Import and snapshots** (#84: #85, 2026-10-17). Migration 0005: the project's source identity (canonical
  path, git common directory, the identity's fingerprint at import) and `source_snapshots`
  (project, base commit, fingerprint, staged patch artifact, unstaged patch artifact, untracked
  files as a list of path, mode and artifact, `created_by`, created). `POST /v1/projects`
  imports: it inspects, refuses on blockers with `import_blocked` and their list, and creates the
  project with its creator as owner. The import result lists the Ada tasks already recorded for
  the same source (canonical path and common directory); verifiable Harness history waits for M6
  (decisions log, 2026-10-16). `POST /v1/projects/{id}/snapshots` with `{untracked: [paths]}`
  captures `git diff --cached --binary` and `git diff --binary` through the mirror, and the
  selected untracked files, into artifacts. The fingerprint is taken before and after; a
  difference refuses with `source_changed` and stores nothing. Untracked selection is explicit:
  each requested path must be an exact member of the untracked list of the inspection the same
  snapshot takes, is opened relative to the work tree with no symlink or redirecting reparse
  point (symlink or junction) anywhere on its path, and must be a regular file; anything else is
  refused with `invalid_path`.
  Ignored files (judged with the mirror's excludes, M3.1), and paths that look like credentials
  (`.env*`, `*.pem`, `*.key`, `id_rsa*`, `.npmrc`, `.netrc`, `.pypirc`, `*.p12`, `*.pfx`,
  `*.kdbx`, anything under `.aws/` or `.ssh/`, credentials files), are refused unless listed in
  `include_sensitive`.
  Acceptance: import creates the project and the owner's membership; blockers refuse; a second
  import of the same source lists the first one's tasks; a snapshot reproduces the staged,
  unstaged and selected untracked content exactly (applied to a clone of the base, it gives the
  original's work tree for those paths); a change during capture is refused; each of these is
  refused with `invalid_path`: a path not in the untracked list, `..`, an absolute or drive path,
  `file:stream` (an NTFS alternate data stream), `CON`, a trailing dot, a directory, a symlink,
  and a junction swapped in after inspection; each credential pattern is refused without
  `include_sensitive`; the original is byte-identical before and after; idempotency and
  membership as for the other mutations.
- [x] **M3.3 Managed clone and task workspaces** (#95: #97, 2026-10-19). `git clone --bare --no-local --no-hardlinks
  --template=` from the mirror, never from the original, into `<state>/projects/<id>/repo.git`,
  with `GIT_NO_LAZY_FETCH=1` and `-c transfer.bundleURI=false`, and its config replaced by Ada's;
  later imports rebuild the mirror and fetch from it the same way. Upload-pack therefore reads
  only Ada's config, and `--no-local` makes pack-objects copy the objects the mirror reaches
  through alternates into the clone's own pack. A task bound to a snapshot gets a feature branch
  `ada/<task id>` from the snapshot's base commit. The attempt's worktree,
  `<state>/projects/<id>/worktrees/<attempt>`, has the snapshot applied and committed as one
  commit, "Imported working changes" (staged and unstaged as recorded), so the task's own changes
  stay apart. A versioned workspace manifest (a stored `workspaces` row, and a copy in the
  worktree's git admin directory) binds project, task, attempt, snapshot, base commit, executor
  and policy. The executor runs the engine in that worktree instead of its scratch directory; a
  missing or mismatched manifest moves the attempt to `recovery_required` and is never rebuilt
  silently.
  Acceptance: the clone shares no files or object storage with the original (no alternates, no
  hardlinks: file IDs compared); a repository whose config names upload-pack and pack-objects
  commands and a bundle URI is cloned and fetched, and no marker is written; the imported-changes
  commit reproduces the snapshot; the engine's working directory is the worktree; a tampered
  manifest goes to recovery.
- [x] **M3.4 Drift and launch revalidation** (#99: #100, 2026-10-19). `GET /v1/projects/{id}/drift` inspects the original
  again and compares it with the newest snapshot: new commits, a moved branch, changed or new
  paths, each reported. Immediately before launch the scheduler checks that the snapshot's
  artifacts still hash to their recorded values and that the workspace manifest matches the
  attempt (task revision, snapshot, ownership generation); a mismatch refuses the launch with a
  reason.
  Acceptance: each kind of drift is reported, and an unchanged original reports none; a
  corrupted snapshot artifact or a stale manifest refuses the launch.

## M4 First local workflow

Five slices (decisions log, 2026-10-20). M4.1 first. M4.2 and M4.3 then one at a time, since
each touches the schema or the API. M4.4 is split in two (decisions log, 2026-10-26): M4.4a can
run beside M4.3, since it touches only `ui/`; M4.4b comes after both. M4.5 comes last.

- [x] **M4.1 The real engine, end to end** (#112: #113, #116; live runs 2026-10-25). `adad` finds the pinned engine, built by
  `scripts/build-engine.sh` at `engine/engine.json`'s revision, in the app's directory or through
  a flag. It checks the binary's SHA-256 against the expected one and refuses any other. The
  expected SHA-256 is built into `adad` from `build-engine.sh`'s output, never read from beside
  the binary. The engine runs with the environment of #58 (decisions log, 2026-10-19).
  The capability matrix: per backend (claude, codex) and mode (readonly, auto, yolo; `ask`
  refused until approvals exist, #7), what each enforces: read-only, workspace writes and shell,
  network, approvals, continuation, cancellation, usage reporting, model discovery (M4.4b takes
  models from it) and enforceable limits. Checks (M4.2) are listed as unsandboxed. Each cell is
  `enforced` (by whom, and the live test that showed it), `requested` (passed, not verified) or
  `unsupported`, keyed by the CLI version observed and dated; a different installed version
  shows the cells as unverified. It is built from the pinned `hrn`'s accept and reject answers,
  a static table recorded in the decisions log, and, per plan section 4, an assessment of what
  the CLI loads: repository configuration and hooks in the worktree (`.claude/`, `.codex/`,
  `.mcp.json`), user-level configuration, plugins and MCP servers under the profile. A worktree
  holding provider configuration or hooks is reported at inspection and shown on the task, and
  again when an attempt's changes add or alter any of them: marked in Changes, and shown before
  a refine starts. A start whose options the matrix refuses answers 409 `unsupported_options`
  before any preparation. The API exposes the matrix for the UI.
  Evidence, on the owner's plans, small, never an API key, the backend always named: one real
  claude run and one real codex run in readonly mode through `adad` on a tiny fixture repository
  holding a project hook that writes a marker, each run asked to write a file, on Windows,
  recorded with run ids, durations and the events received. This is also the live
  check of the #58 allow-list. Before each live run, each CLI's own status command shows a plan
  sign-in (not an API key), and its settings hold no `apiKeyHelper` or key in `env`; the output
  is recorded with identifiers masked. Live runs are behind an explicit opt-in that CI never
  sets, with a turn and time cap and a fixed tiny prompt.
  Acceptance: the matrix refuses each unsupported combination; a cell for another CLI version
  shows as unverified; a wrong engine hash is refused, and so is a hash file placed beside the
  binary; a live readonly run asked to write a file leaves it absent, or the cell reads
  `requested`; a fixture repository with a project hook writing a marker is reported, and the
  result is recorded; without the opt-in no live run starts; each CLI's sign-in status is
  recorded before its run; the live runs are recorded.
- [x] **M4.2 Checks** (#114, #108: #115, 2026-10-26).
  A task declares its check commands: argv lists, a working directory
  inside the worktree, a timeout of at most 30 minutes, and output capped at 16 MiB per check;
  output beyond the cap is read and discarded, the log marked truncated, and the check keeps
  running. There are at most 16 checks per task. They are part of the task's revision and are never discovered from the repository. A check
  still runs the repository's code and the agent's changes with the user's rights, on the host,
  outside any backend sandbox and with network. The UI says so where checks are declared and
  where their results are shown, and the capability matrix lists checks as unsandboxed.
  The commit. After an attempt ends ok, Ada commits the agent's changes as one commit on
  `ada/<task>` ("Agent changes", Ada's author). The commit runs through M3's git runner with the
  git directory named explicitly (never read from the worktree's `.git` file), system and global
  config off, Ada's clone config checked against the copy Ada wrote (a difference moves the
  attempt to `recovery_required`), hooks pointed at an empty directory, `core.fsmonitor=false`,
  `commit.gpgsign=false`, and author and committer set through `GIT_AUTHOR_*` and
  `GIT_COMMITTER_*`. Before committing, Ada checks that the branch still descends from the
  imported-changes commit. Commits the agent made itself are kept beneath Ada's commit; a
  rewritten or moved history moves the attempt to `recovery_required`.
  Running. Each check runs as a managed process in a fresh detached worktree at the tested
  commit, as the baseline does, at `<state>/projects/<id>/checks/<attempt>/<run>`, a path built from ids, its admin directory recorded when it is created, and both removed afterwards by the rules of M4.3's removal; nothing the
  agent left outside the commit reaches a check, and the attempt's worktree is never changed by
  one. It runs under the executor's process-tree supervision, output capped into artifacts,
  with the #58 list minus `CLAUDE_CONFIG_DIR` and `CODEX_HOME` and under the same never-passed
  rule. A check may name further variables taken from `adad`'s environment, recorded with the
  check; a never-passed one is refused. The working directory is resolved at run
  time through no-follow handles (M3.3's walk); a link or reparse point on its path refuses the
  check. After each check, the tracked files of the check's worktree must still match the tested commit; a check that changes them is recorded as such and its result does not bind the commit. Later checks start from their own fresh worktree and are unaffected.
  Ada records the command, the environment's keys, the tested commit, the exit code, the
  duration and the log artifact. The first `/v1` route that serves a check log checks
  membership on each read (#3).
  The baseline. The same checks run on the snapshot's imported-changes commit, in a separate
  detached worktree removed afterwards, once per snapshot and check definition (again when a
  check's definition changes). So failures that already existed are told apart from new ones.
  States. The task moves `running` -> `verifying` -> `awaiting_review`; failed checks are
  recorded, the task still goes to `awaiting_review`, and is shown as failing. Stop during
  `verifying` kills the running check's tree and records the remaining checks as not run. A
  restart during `verifying` moves the task to `awaiting_review` with the interrupted checks
  marked. Any change to the branch after a check invalidates that result. A forward migration
  adds checks and their results.
  Acceptance: a check that passes, one that fails, one that times out, and one killed with its
  process tree; a timeout or output cap above the ceiling refused, and so is a seventeenth check;
  a check writing past the cap ends with its own exit code, its log marked truncated; a baseline
  failure told apart
  from a new one, and the baseline run again after a check's definition changes; a result
  invalidated by a later edit; a check that edits a tracked file recorded as such, binding
  nothing; no check runs from a repository script unless the task declares it; the check's
  environment holds no credential-profile variable and no never-passed variable; a further variable the check names reaches it from `adad`'s environment, and naming a never-passed one is refused; a check's worktree and its admin directory are gone after the check, and a crash in between finishes on the next try; a working
  directory that the tested commit holds as a symlink to outside is refused; an ignored file the
  agent left in its worktree is absent from the check's worktree; a check that edits a tracked
  file leaves the attempt's worktree unchanged; a check
  that daemonizes (double fork or `setsid` on Linux; a detached child on Windows) leaves nothing
  running when it ends; an agent that sets `core.hooksPath`, `core.fsmonitor` or `gpg.program`
  in the clone, or repoints the worktree's `.git` file, causes no marker and goes to recovery;
  an agent commit is kept; an agent reset below the imported-changes commit goes to recovery;
  Stop and a restart during `verifying` end as stated; a non-member reading a check log gets
  404.
- [x] **M4.3 Changes and review** (#118: #122, 2026-10-28, into `main` with #126).
  `GET /v1/tasks/{id}/changes` returns the task's own changes,
  the diff from the imported-changes commit to the agent-changes commit: the file list with
  each file's status and patch, binary files marked. It is capped at stated numbers (files,
  bytes per file, bytes in all), with truncation marked; members only (404 otherwise). It runs
  through Ada's git runner with `--no-ext-diff --no-textconv -z` and a fixed rename threshold,
  and returns paths as the bytes git reported. `GET /v1/tasks/{id}/files` lists and reads files
  from the agent-changes commit through git (`ls-tree`, `cat-file`), never by walking the
  worktree, so no link is followed; symlinks are shown as links; size-capped; members only.
  Review actions as #3 says: findings (any member); accept (the attempt's actor or a project
  owner), which records the reviewed commit, its check results and the coordinator's ownership
  generation (#3), so a later change invalidates it, and moves the task to `accepted`.
  Refine (the attempt's actor or a project owner, #3) is a new attempt on the same task, run as
  a fresh engine session (native continuation stays in M6) whose actor is the requester and
  which runs on the requester's profile. It continues on `ada/<task>` from its tip, with the
  findings in its prompt. Refine is allowed from `awaiting_review`, `failed` and `canceled`, and
  moves the task to `preparing` with the new attempt; it is refused for an accepted task. It is
  also refused while any attempt of the task is not ended or is `recovery_required`. An ended
  attempt's remaining changes are committed as in M4.2 before its worktree is removed. Removal
  deletes only `<state>/projects/<id>/worktrees/<attempt>`, a path built from ids, never from
  stored text. It never follows a link or reparse point (the directory itself is Lstat-checked
  as a real directory under the state root), and then removes only that attempt's admin
  directory under the clone's `worktrees/`, as M3.3 recorded it, never through `git worktree
  prune`. Each step is idempotent after a crash. This settles #98. Export stays in M7.
  Acceptance: the diff matches git's for a text change, a binary file, a rename, a mode change
  and a deletion; each cap marks its truncation; a non-member gets 404 from changes and files;
  the files route shows a symlink as a link and reads nothing through it; findings from a
  non-member refused; an accept or refine by the creator or a reviewer who is neither the actor
  nor an owner refused; an accept against an older commit or generation refused; refine starts
  a second attempt on the requester's profile that sees the first one's commits; refine refused
  for a running or `recovery_required` attempt and for an accepted task; refine of a failed and
  of a canceled task; a failed attempt's changes reach the second attempt; an attempt that adds
  a project hook is marked in Changes and before refine; symlinks and junctions inside the
  worktree, and the worktree itself replaced by a junction, all pointing at a directory with a
  marker: removal leaves the marker; another attempt's missing worktree keeps its admin
  directory; a crash between the removal steps finishes on the next try.
- [x] **M4.4a UI foundation** (#119: #121, 2026-10-28, into `main` with #126).
  The base the workspace UI is built on, in `ui/`, with no new
  dependency in `package.json`.
  Tokens. CSS custom properties in tiers: five background levels (bg, sidebar, surface, elevated,
  overlay); border and text in three strengths each; an accent with `-text`, `-dim` and `-fg`;
  status colours (success, warning, danger, info), each with a `-dim`; `chart-1` to `chart-8`; a
  4-point space scale; a z-index scale; named durations of 90, 120, 140, 180, 280 and 350 ms with
  one easing, `cubic-bezier(.22,1,.36,1)`. The CSS is split by concern into small files, never one
  large global file mixed with inline styles.
  Themes. Ada's light slate default and a dark "workshop" preset as the second theme. They switch
  through data attributes on `<html>`, set before first paint by a small classic script file
  (`ui/public/theme-boot.js`, loaded without `type="module"`, `defer` or `async` in `<head>`
  before any stylesheet), so the CSP stays `script-src 'self'` with no inline script,
  `'unsafe-inline'` or hash exception. It reads the saved choice from `localStorage` (the theme is
  a per-device preference, not an `adad` setting) and falls back to the light default, so the
  wrong palette never flashes. No theme switches itself on.
  Contrast. A script, run by `scripts/check.sh` (so CI runs it), reads the theme token files and a
  declared list of foreground/background token pairs kept beside them (text strengths, accent
  `-text` and `-fg`, each status colour on its backgrounds, focus ring and borders as UI parts),
  and checks WCAG AA for each pair in every theme, plain Node with no new dependency: 4.5:1 for
  text, 3:1 for large text and UI parts.
  Motion. Only the named durations (90 to 140 ms for hover and press feedback, 180 to 350 ms for
  movement and entrances), transform and opacity only, and nothing loops while the system is
  healthy. A skeleton shaped like the content while it loads; a spinner only on a button just
  pressed (`aria-busy`). One kill switch, `body[data-motion="off"]`, beside
  `prefers-reduced-motion`.
  Effects. One registry: every effect declares what it does under reduced motion (`static`, `off`
  or `unchanged`), so later effects obey the same switch. No effect switches itself on.
  Command palette. Ctrl+P opens it (the browser's print default prevented): scope chips (all,
  project, task, action), Recent, Quick actions, and key hints in the footer. A combobox and
  listbox with `aria-activedescendant` and a live status line. Data is loaded lazily per scope
  through the `AdaClient` methods that exist today, not all at once when it opens: tasks from
  `listTasks` when the task scope (or "all") is first shown, projects as the distinct `project`
  ids of those tasks, and actions defined locally. No `AdaClient`, bridge or `/v1` change in this
  slice; a project list endpoint comes with M4.4b if it needs one. A native `<dialog>`, written by
  hand.
  Resize handle. One component, shared by the work-area panes and the drawers: 4 edges at 6 px and
  4 corners at 12 px, ARIA separator semantics, and resizable from the keyboard too (arrow keys by
  a step, Home and End to the limits).
  Avoid: very large components, parallel effect systems, a minimum boot-screen time.
  Read the frontend-design skill first.
  Acceptance: Vitest for the theme switch (`index.html` loads the boot script as a blocking
  classic script in `<head>` before any stylesheet or module, and its CSP is unchanged; the script
  sets the attributes from the saved choice, falls back to light when nothing or something invalid
  is saved, and the in-app switch saves the choice), the contrast script (fails on a pair below
  AA, passes the shipped themes, wired into `check.sh`), the motion switch (`data-motion="off"`
  and reduced motion stop every registered effect as it declares), the palette (keyboard
  navigation, `aria-activedescendant`, scope chips, Ctrl+P's default prevented, lazy loading (no
  AdaClient call until a scope needs it; nothing outside the existing methods), the live status),
  and the resize handle (mouse and keyboard, limits); an accessibility pass; no new dependency in
  `package.json`.
- [x] **M4.4b The workspace UI** (#120: #123, 2026-10-29, into `main` with #126).
  Builds on M4.4a: its tokens, themes, motion rules, effect
  registry, palette and resize handle. The plan's practical layout (`docs/plan.md` section 3) in `ui/`,
  through `AdaClient` only. Left navigation: projects and active tasks. Main area: the task and
  its activity, events rendered by kind instead of raw JSON. A resizable work area: Changes (a
  diff viewer), Checks (results and logs, marked as unsandboxed), and Files (a read-only tree of
  the agent-changes commit, from M4.3's files route); Preview waits for M5. Patches and paths
  render as text, never HTML; control and bidirectional characters in paths and lines are
  marked. A status bar: branch, executor, backend and model, task state, Stop. The import flow:
  a folder chosen through a typed Electron dialog in main, the only way a path enters (#5). The
  bridge method takes no path; main sends the chosen path to inspect and import and keeps a
  short-lived handle for the snapshot and create calls; the generic bridge refuses a `path`
  field on the inspect and import routes from the renderer. Then inspect; blockers shown with
  their paths; provider configuration and hooks found in the repository shown; the uncommitted
  changes offered ("Include these changes in the new feature branch"), untracked files
  selectable and sensitive ones marked; the snapshot; then task creation with objective,
  acceptance criteria, check commands (with the unsandboxed notice), and backend, model and mode
  from the capability matrix. Readonly is the default mode; yolo is never preselected and is
  shown with what it disables (plan section 4). The themes of M4.4a; Source
  Sans 3 and JetBrains Mono bundled, no remote fonts; keyboard navigation, visible focus, reduced
  motion. Read the frontend-design skill first. The resume summary and Overview wait for M6.
  Acceptance: Vitest for the components; a renderer call with a typed path is refused by the
  bridge; readonly preselected and yolo never; a path holding control or bidirectional
  characters is marked and a patch holding HTML shows as text; Playwright for the flow import ->
  task -> fake engine -> checks -> changes -> accept; an accessibility pass.
- [ ] **M4.5 Demonstrations.** The full flow with a real backend on Windows (the owner's PC)
  and on Ubuntu, recorded in the status file as evidence with screenshots, on a fixture
  repository and a scratch state directory; screenshots show no account, e-mail or sign-in
  details. Ubuntu needs a desktop session and a signed-in claude or codex CLI there, an owner
  action ("Owner actions pending"). As in M4.1: before each live run, each CLI's own status
  command shows a plan sign-in (not an API key), and its settings hold no `apiKeyHelper` or key
  in `env`; the output is recorded with identifiers masked. Live runs are behind an explicit
  opt-in that CI never sets, with a turn and time cap.
  Acceptance: both demonstrations recorded with the sign-in status before each; the Playwright
  flow green on the pull request's ubuntu-latest job (under xvfb) and on the windows-latest job
  of the push to main after M4.4b merges.
  Not ticked: the opt-in demonstration through the app is merged (#124: #125, 2026-10-29, into
  `main` with #126); the demonstrations are not recorded yet (#124 open). The Windows CI
  job on `main` passes again since #139 (#127, 2026-11-18, `763a797`).

- [x] **M4.6 Secret scan of the agent's changes** (#130: #132, 2026-11-06).
  The decisions log, 2026-11-06. After
  the agent-changes commit, `adad` scans the lines the task's changes add (base to commit) for
  credentials and records findings without their values (migration 0011); changes carries the
  scan; accept answers each open finding as a test value (allow-listed with a reason) or a real
  secret (recorded with the review). Owned: `internal/source/secrets.go`, `internal/store`
  (`secrets.go`, migration 0011, `Accept`), `internal/executor` (`scanSecrets`), `internal/api`
  (changes, accept), the bridge's accept, and `ui/src/workspace/Secrets.tsx`.
  Acceptance: each rule's sample found with file, line and rule, and no part of the value in the
  stored scan or the answer; removed lines and lines already in the base not reported; an
  allow-listed path and rule not reported; an allow-list entry without a reason refused; accept
  refused until each open finding is answered, each answer stored with the review and a test
  value written to the allow-list; binary and oversized files reported as not scanned; the
  Playwright flow answers a fake key before it accepts.

## M4b Web access

Nine slices (decisions log, 2026-11-23, M4b in nine slices). M4b.1 first: it is a spike that ends
in an owner decision, and only M4b.6 and M4b.9 wait for that decision. The slices that change the
schema (M4b.2, M4b.5, M4b.6, M4b.7, M4b.8) run one at a time. M4b.3 follows M4b.2; M4b.4 may start
beside M4b.3, since it touches only `ui/`, and finishes after it; M4b.5 follows M4b.3; M4b.6
follows M4b.5 and the owner's choice; M4b.7 follows M4b.5; M4b.8 follows M4b.3; M4b.9 is last. The
TLS listener serves only the routes on its list: M4b.3 starts it with sign-in to Ada, the user's
own account, reading what the user's memberships allow (with a new membership-filtered project
list), and the capability matrix (`GET /v1/capabilities`); the experience card's route stays
local, since it reads the coordinator's own ledger. M4b.5 adds, for the owner account, the task
writes (creating a task, setting its checks, marking it ready, starting, cancelling, refining), a
project's snapshot (`POST /v1/projects/{id}/snapshots`), findings and acceptance under #3's rules
(any member records a finding; the result's actor or a project owner accepts), and permission
decisions; M4b.6 adds the same for other users once the chosen boundary is active, and the
provider sign-in relay's routes; M4b.7 adds the chooser. Routes that take a host path stay on the
local pipe or socket for good. Each slice that adds a route to the network list, or an
owner-channel route, tests it through an HTTPS session: allowed where its slice says, refused
otherwise, the owner-channel routes refused even to the owner's session. Every operation the
decisions give to the owner's desktop channel is a `/v1` route served only on the local pipe or
socket, so the desktop's settings and M4d's `adad` subcommand are both its clients and no slice
assumes a desktop on the coordinator's host (decisions log, 2026-11-23, #147).

- [ ] **M4b.1 The execution boundary, a spike.** No product code. On the owner's Windows PC and on
  Ubuntu (macOS when a Mac is available), find how `adad`, running as the owner without
  administrator rights, can start a task (the engine, its CLIs, checks and their descendants)
  so that nothing it starts can read another credential profile, `adad`'s state, its pipe or
  socket and token, or the TLS and DNS keys, while the provider process alone holds its own
  profile's login. Candidates: a separate OS account per credential profile (what starting a
  process as it needs on each OS: a stored credential, a helper, a service or administrator
  rights); an OS sandbox (Windows AppContainer or restricted tokens, Linux namespaces, the
  macOS sandbox); and M5's VM executor. Probes run under scratch accounts the owner creates;
  creating OS accounts and changing security settings are the owner's steps, never Ada's or an
  agent's. Done when the decisions log records, per OS and per candidate, what it needs, what it
  blocks (checks and descendants included, a check reading its own actor's login among them)
  and what it costs to install, with a recommendation and whether M4b.6 should be split in two
  (the boundary, then the sign-in relay), and the owner has chosen (owner action 15).
  Progress (#156): the owner chose on 2026-11-24 a separate OS account per user, the owner's own
  tasks included, with a guarantee narrowed to what any candidate can show (decisions log,
  2026-11-24, M4b.1: the owner chose the execution boundary); the Windows probes that need no
  setup ran. The account probes and the Linux probes wait for owner action 16.
- [x] **M4b.2 Accounts, credentials and sessions** (#154: #155, 2026-11-24). A forward migration for credentials (passkeys,
  and an Argon2id password where the owner enables it for an account), sessions stored as
  SHA-256 hashes, enrollment codes, and a disabled flag on accounts, all as #6 specifies; a
  pinned WebAuthn library chosen by the dependency rule and recorded. The API's actor comes from
  the session cookie on a network listener and from the pipe or socket's ownership locally, as
  now. Owner-channel routes, local listener only: create, disable and remove an account, issue an
  enrollment code, recover an account (a new credential, throttling cleared), list and revoke any
  session. Network routes: login, enrollment with a code, logout, the user's own sessions and
  credentials. Throttling in memory, bounded, as #6 says. Owns `internal/store` (the migration,
  accounts and sessions), `internal/api` (authentication and these routes) and the desktop's
  account settings. Tests: #6's, except those that need the TLS listener (M4b.3): rotation,
  every session of an account ended by a credential change or a disable, with their open
  streams closed; idle and absolute expiry and both throttles on a fake clock;
  enrollment codes single use and expiring; the stored hash and token never equal to their
  inputs; the owner's first credential added to the existing owner account, whose projects and
  tasks stay visible; this slice's owner-channel routes refused through a network session, a
  freshly authenticated owner's included.
- [x] **M4b.3 The TLS listener, from files** (#158: #159, 2026-11-24). The network listener as a setting, off by default,
  set through the owner's channel: one interface address, and a certificate and key from files in
  `adad`'s configuration directory, owner-only, checked (the key matches, the chain parses, not
  expired). Missing or mismatched files leave the listener off with the reason shown to the
  owner; never plain HTTP, never a self-signed fallback. `GetCertificate` reads an atomic pointer
  swapped after the same checks, and the pair on disk is replaced through a pointer file renamed
  into place, as #8 specifies, so M4b.8 only adds issuance. On this listener: the session
  authentication of M4b.2; `Host` equal to the configured name; mutations only with the exact
  configured `Origin` and a JSON content type; no CORS headers; without a session, only login,
  enrollment with a code and static assets; the shared UI served with a content security policy
  and `frame-ancestors 'none'`. The network list as above; host-path routes refused on it. Owns
  `cmd/adad` (the listener and its setting), `internal/api` (the middleware and the list) and the
  desktop's listener settings, where the owner sets it up and sees why it is off. Tests: #6's
  `Host`, `Origin` (missing, `null`, foreign, the preview host's) and content type checks; every
  other route refused without a session; with a session, a start, a refinement and a host-path
  import refused; #8's missing, mismatched and expired files and handshakes refused after
  expiry; a connection open across a swap keeping its certificate while new ones get the new
  pair; a restart at each step of a replacement ending on a complete pair; the project list
  showing a member's project that has no task yet, and no other user's; `GET /v1/experience`
  refused on the network.
- [x] **M4b.4 The browser UI** (#160: #162, 2026-11-25). The HTTPS `AdaClient` adapter (same-origin `fetch` with the session
  cookie, `EventSource` for events with #4's `cursor_ahead` handling), and the pages the browser
  needs: login with a passkey (or a password where enabled), enrollment with a code, the user's
  sessions and credentials, and "Leave" (#9), which signs out and stops nothing; the project
  navigation reads M4b.3's project list, and the browser shows no experience card. Owns `ui/`.
  Tests: no module outside the adapters touches `window.ada` or `fetch`; the component tests run
  against both adapters with one fake `adad`; `cursor_ahead` reopening from 0 without
  `Last-Event-ID`; Playwright from two browser contexts against a real `adad` on M4b.3's TLS
  listener with a test certificate and two seeded users with their own projects, each signed in
  as its own account and seeing only its own projects, an empty one included; opening a browser
  workspace making no experience request.
- [x] **M4b.5 Membership, permission decisions and the owner's stop** (#163: #165, 2026-11-25). The `permission_decisions`
  migration and the scheduler gate in the claim, `POST /v1/attempts/{id}/decision` with its
  idempotency key, all as #7 specifies; membership added and removed through the owner's channel,
  a removal denying that user's pending decisions and cancelling their attempts in one
  transaction, and the last owner's removal refused (#3); a role change ending every session of
  that account (#6); the stop refused to everyone but the owner account, which first sees the
  count of other users' running and queued work and confirms, with the reason on their tasks
  (#9); the task writes and decisions added to the network list for the owner account. In
  `ui/`: the attempt actor's pending decisions with allow and deny (deny for project owners), the
  stop's count and confirmation before anything stops (today the desktop stops first and shows
  counts after), and the desktop's membership administration. Owns `internal/store`,
  `internal/executor` (the claim), `internal/api` and those `ui/` and desktop files. Tests: #7's
  decision tests; #3's removal tests, the existing artifact read check (M4.2) exercised through
  the new removal route; a role change ending the account's sessions on two browsers with their
  streams closed; #9's stop tests; a decision seen and answered by its actor in one session and
  invisible in another user's; through an HTTPS session, the owner account taking a snapshot,
  creating a task on it, marking it ready, starting it and recording a finding, and another user
  refused each.
- [ ] **M4b.6 Credential profiles and the relayed sign-in.** After M4b.1's decision and M4b.5.
  Each user's credential profile under `adad`'s state, selected by the attempt's actor and failing
  closed (no profile, or a sign-in the CLI reports invalid, refuses the start with no fallback);
  the launcher's environment built per actor from #58's list with that profile's
  `CLAUDE_CONFIG_DIR` and `CODEX_HOME`; the boundary the owner chose in M4b.1 applied to the
  engine, its CLIs, checks and descendants; the sign-in relay for the providers owner action 10
  names, as #7 specifies, loopback-only providers not offered in the browser, with its page in
  `ui/` (the verification link and code shown to the session that started the job, and code entry
  where the provider gives one); the task writes, decisions and the relay's routes added to the
  network list for other users. If the owner chose M5's VM executor, a working M5 executor comes
  before this slice and before M4b.9. Owns `internal/executor`, `internal/store` (profiles and
  sign-in jobs), `internal/api` and the sign-in page. Tests: #7's relay tests; a missing or
  invalid profile refusing the start; a second user's start refused on a host where the chosen
  boundary is not active, and through an HTTPS session a second user creating, readying and
  starting a task once it is; and, with scratch credentials and keys under the chosen boundary,
  the engine, a check and the descendants of each reading no other user's profile, `adad`'s
  state, its pipe or socket and token, the TLS key or the DNS token (a user's own tasks may read
  that user's own login; decisions log, 2026-11-24, M4b.1: the owner chose the execution
  boundary).
- [x] **M4b.7 Import roots and the server-side chooser** (#166: #168, 2026-11-25). Roots granted per user through the
  owner's channel and kept in the store (a forward migration), so they survive a restart;
  browsing below a root, inspecting the chosen directory by root id and relative path before
  import (blockers, provider configuration, the untracked files the snapshot may take), and
  importing it, with M3's import blockers;
  no git-URL import, in the browser or through `/v1`; the browser's import page and the desktop's
  page for granting roots. Owns `internal/store`, `internal/api`, `internal/source` (the walk
  below a root), `ui/` and the desktop settings. Tests: #5's chooser tests; grants surviving a
  restart; through the owner's HTTPS session, a directory with an untracked file inspected and
  imported through the chooser, then its snapshot taken with that file, a task created on it,
  readied and started.
- [ ] **M4b.8 `home.adanext.si` and the DNS-01 client.** The Cloudflare token file (owner-only,
  refused when others can read it, never logged), the ACME client against a fake ACME server in
  tests, renewal on #8's schedule with its notices to the owner, the durable challenge intents and
  their cleanup and reconciliation (a forward migration), the desktop's page for replacing the
  token, the notices on the desktop and in the owner's web session (carried on the owner's
  own-account response, which an open page reads again every minute without refreshing the
  session's idle time), and the setup page's note on local DNS for resolvers that drop private
  answers. Owns `cmd/adad`, `internal/store`, `internal/api` (the own-account response), those
  desktop pages and the notice in `ui/`. Tests: #8's issuance, renewal, notification, token-file
  and challenge cleanup and reconciliation tests (its file, swap and expiry tests are M4b.3's),
  with a failure's notice reaching an owner HTTPS session with no attempt open, as well as the
  desktop, and an unattended owner page still expiring after 8 idle hours on a fake clock.
- [ ] **M4b.9 The fifth lane.** The M4b evidence of the decisions log, 2026-10-13, against a
  Windows coordinator host (an Ubuntu host joins when lane 4's host exists): a browser on a
  second computer reaching `https://home.adanext.si`, a separate account, the provider sign-in
  completed from that computer for each relayed provider (#7), that user's own project and task
  on their own sign-in, a decision answered by that user and invisible to the other, and
  reconnection after a dropped connection; recorded in the status file with the revision, the
  commands and their results. The second computer and its user's provider sign-ins are the
  owner's to provide.

## Later milestones

Broken into slices when M2 is done, from `docs/plan.md` sections 3 to 6.

- **M3 Project workspaces:** broken into slices; see "M3 Project workspaces" above.
- **M4 First local workflow:** broken into slices; see "M4 First local workflow" above.
- **M4c Editor integration, VS Code first:** an extension that is one more client of `adad`.
  The desktop's Node modules (find-or-start, handshake, event-stream client) move to a package
  both use; the extension finds `adad` from the installed desktop app or a setting, starts it if
  needed and never stops it. Its Node side alone holds the endpoint token and checks every
  message's shape before forwarding, as the bridge does; its webview loads the shared UI through
  a third `AdaClient` adapter, with a content security policy, `localResourceRoots` limited to
  the UI bundle and no remote code; authorization stays `adad`'s. Scope: import the open folder
  (in a multi-root workspace, the one chosen), start, follow and cancel tasks, the task state in
  the status bar, and a task's changes in the editor's own diff view, read from Ada's artifacts,
  not the checkout. Taking the result as a branch arrives with M7's branch export. It never
  edits the checkout itself. Tests: the adapter with Vitest, and the extension against a real
  `adad` and the fake engine in VS Code's extension test host (xvfb on Linux CI, one worker).
  JetBrains follows later as its own slice (#92), a Kotlin client of the same API. Reaching a
  server from an editor is an editor slice after M4d (decisions log, 2026-11-23).
- **M4b Web access:** broken into slices; see "M4b Web access" above.
- **M4d Headless server (#147):** after M4b, on its listener, accounts and sessions (decisions
  log, 2026-11-23). Slices: the execution boundary first (engine, CLIs, checks and descendants;
  nothing a task starts reads another user's login or `adad`'s secrets, as amended 2026-11-24;
  `adad` refuses tasks where the host cannot provide it); the owner's channel as an `adad` subcommand over the socket, run by SSH as the service
  account, taking every desktop-channel operation of M4b; the Linux package and systemd unit
  (brought forward from M9); public mode behind an access proxy the owner controls (forwarded
  headers trusted only from the proxy, passkeys only, rate limits, the external hostname as the
  relying party ID); remote imports from server-side roots, no public previews; the desktop's
  server mode (a window on the server's origin with no preload bridge, "Leave" instead of Quit).
  Direct public mode waits for its own decision. Evidence, a gate of its own: on a local Linux
  VM or a Linux machine the owner runs, install and start at boot, an account created over SSH,
  passkey login and a task from a browser on another network and from a phone's browser, the
  desktop in server mode on Windows and Linux (macOS at the real-Mac gate), a second user with
  their own provider sign-in, account creation refused through the owner's HTTPS session,
  reconnect, and a reboot without replaying an abandoned attempt.
- **M5 VMware execution:** Packer template, guest service, isolated network, guest login, previews,
  recovery. Images carry the compatibility manifest of #10, naming the source commit and the
  built artifact's SHA-256; the `adanext-images` build inspects the binaries inside the artifact
  it hashed and publishes those same bytes, and `adad` hashes the artifact it is given and
  refuses one the manifest does not match, with the tests the decisions log, 2026-10-14, lists. The
  browser's preview host and its per-request check (#5) come here with previews, from M4b
  (decisions log, 2026-11-23, M4b in nine slices), with #5's preview tests, the
  `preview.home.adanext.si` record and the certificate naming it.
- **M6 Resume intelligence:** evidence extraction, cited summaries, next-task proposals,
  continuation. Citations are checked in two steps (#131): the evidence id exists, and the evidence
  supports the claim. Typed claims are compared with structured evidence in code (a "checks passed"
  claim against a failed run is a contradiction). Each free-text claim quotes a short span of the
  evidence it cites, and code checks that the span appears in that evidence after normalizing
  whitespace and case in both. A quote that is missing, empty after normalizing, or not found reads
  "quote not found", never "supported", and goes no further. A quote that is found shows only that
  the text is there, not that it supports the claim, so only those claims go to an optional local
  classifier (supports, contradicts, does not address), never a service billed per call. A
  contradicted claim is not shown as a fact, and its evidence is shown with a note that the summary
  disagreed; one the evidence does not address is shown as unsupported, beside its evidence. Without
  the classifier, or when it fails, a free-text claim whose quote was found reads "support not
  checked", never "supported". The classifier's thresholds and model are versioned and stored with
  each verdict, and a labelled set of claim and evidence pairs in the repository measures it before
  it is turned on and again whenever the model or the thresholds change.
- **M7 Review and integration:** review package, branch export, destination checks. Export scans
  every commit of the exported range for secrets (#130, the decisions log 2026-11-06), not only
  the tip: a finding that is not allow-listed refuses the export, a real secret that a later
  commit removed included. Ada may offer to rebuild its own `ada/<task>` branch without the
  secret before an export, never silently, and never once the branch has been exported. Tests:
  an export refused when an earlier commit holds the secret and the tip does not; the rebuilt
  branch exports; a rebuild of an exported branch refused.
- **M8 Recovery and routing:** the fault matrix, model policies, quota handling.
- **M9 Packaging and dogfood:** installers, the web form as a service (its Linux package and
  systemd unit come with M4d), a download page on
  adanext.si, diagnostics, accessibility, one improvement to Ada made through Ada.
