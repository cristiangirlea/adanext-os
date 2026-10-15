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
  request carries an idempotency key and the expected task revision. Acceptance: duplicate start, concurrent clients, a client
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

## Later milestones

Broken into slices when M2 is done, from `docs/plan.md` sections 3 to 6.

- **M3 Project workspaces:** import without running repository code, snapshots of staged,
  unstaged and selected untracked files, an Ada-owned clone, task worktrees, drift detection,
  import blockers.
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
