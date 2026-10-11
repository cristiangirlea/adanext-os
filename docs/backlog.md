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

- [ ] **M0.1 Bootstrap.** Agent setup, CI on the owner's runner, `scripts/check.sh`, the engine
  pin and `internal/engine`, desktop version pins and lockfile, plan, status and backlog.
  Acceptance: `bash scripts/check.sh` passes; the engine tests reject short revisions, unknown
  fields and wrong hashes; CI green on `lab-ada-os`.
- [ ] **M0.2 Runner.** Infra pull request adding the `lab-ada-os` scale set and the repository
  variable (owner merges; the GitHub App step is the owner's).

## M1 Execution protocol (Harness repository)

Each slice branches from Harness `main` and follows Harness's AGENTS.md: standard library only,
records stay loadable, README and PLAN.md updated, tests first.

- [ ] **M1.1 Reasoning effort for Codex and Claude.** Codex gets `-c model_reasoning_effort=<level>`,
  Claude gets `--effort <level>`. Levels are validated before the CLI starts. Backends that cannot
  apply an effort say so in the run, as unenforced limits already do. Acceptance: argument tests
  for both adapters through the helper process; an unknown level fails before start; old records
  load; a bounded live smoke on each CLI recorded in this file.
- [ ] **M1.2 Run records surface write failures.** `runlog.Writer` keeps the first event write
  error and counts lost events; `Close` returns them; the result record carries them in an
  additive field; `hrn run` and the MCP run tool report them. Acceptance: a writer whose events file
  fails mid-run reports the error and the count; records without the field still load.
- [ ] **M1.3 Agent CLIs run in a process tree.** CLI backends start their child in the Job Object
  on Windows and a process group on Unix that `internal/tools` already uses for commands, so a
  cancel or timeout also ends grandchildren, and the run reports canceled only after the tree is
  gone. Acceptance: a helper process that starts a grandchild; after cancel the grandchild no longer
  runs, checked by PID and creation time; Windows and Linux.
- [ ] **M1.4 `hrn exec --protocol v1`.** A versioned NDJSON protocol on stdin and stdout for a
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

- [ ] **M2.1 Store.** `internal/store` on modernc.org/sqlite v1.59.0: WAL, foreign keys,
  `synchronous=FULL`, numbered migrations each in a transaction, a refusal to open a newer schema.
  Tables for projects, tasks, attempts, events and idempotency keys; the task state machine of
  `docs/plan.md` section 2 with a revision per task; consistent backups through the SQLite backup
  path. Acceptance: migrations from empty; refusing a future schema; every allowed and refused
  transition; duplicate requests with one idempotency key; a stale expected revision refused; a
  helper process killed mid-transaction leaves a consistent database; restore from a backup;
  disk-full on the artifact writer surfaces and stops the task.
- [ ] **M2.2 `adad`, singleton and IPC.** `cmd/adad` listens, as `docs/plan.md` section 2 says,
  on a per-user Windows named pipe whose security descriptor grants only the current user, or an
  owner-only Unix socket (mode 0600 in a 0700 directory) on Linux. How the pipe is created
  (direct Windows calls or a vetted library) is decided in the slice; a test reads the
  descriptor back and finds no other principal.
  A second `adad` finds the first and exits; a stale lock from a dead process (PID and creation
  time) is recovered. Versioned NDJSON requests: create, start, cancel, get and list tasks, and
  subscribe to events from a sequence number. Every mutating request carries an idempotency key
  and the expected task revision. Acceptance: duplicate start, concurrent clients, a client
  disconnecting mid-stream, reconnect from the last sequence, a malformed request, and the pipe's
  or socket's permissions.
- [ ] **M2.3 Local executor.** `internal/executor` runs the pinned engine (`hrn exec --protocol v1`)
  per attempt, verified by hash, records the attempt intent before launch, ingests events in
  order, and holds an ownership generation. Cancel reports canceled only after the engine exits.
  After an `adad` crash, an attempt that was running becomes `interrupted` and then
  `recovery_required`; it is never replayed. Acceptance: a fake engine speaking the protocol;
  cancel, engine crash, `adad` crash and restart, truncated stream, duplicate start.
- [ ] **M2.4 The window can close.** A minimal Electron shell (main, preload, one React view of
  tasks and events) that connects to `adad`, or starts it independently of its own process tree.
  Acceptance, with Playwright against a real `adad` and a fake engine: start a task, close the
  window, `adad` and the task keep running, reopen, the task and its events are there; the explicit
  "Quit Ada and stop work" cancels and stops.

## Later milestones

Broken into slices when M2 is done, from `docs/plan.md` sections 3 to 6.

- **M3 Project workspaces:** import without running repository code, snapshots of staged,
  unstaged and selected untracked files, an Ada-owned clone, task worktrees, drift detection,
  import blockers.
- **M4 First local workflow:** desktop import, task, edit, checks, diff and review on Windows and
  Ubuntu with a real backend.
- **M5 VMware execution:** Packer template, guest service, isolated network, guest login, previews,
  recovery.
- **M6 Resume intelligence:** evidence extraction, cited summaries, next-task proposals,
  continuation.
- **M7 Review and integration:** review package, branch export, destination checks.
- **M8 Recovery and routing:** the fault matrix, model policies, quota handling.
- **M9 Packaging and dogfood:** installers, diagnostics, accessibility, one improvement to Ada made
  through Ada.
