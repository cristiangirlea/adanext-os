# Implementation status

Updated 2026-10-19. Statuses: not started, in progress, implemented (code merged), verified
(evidence below names the revision, commands and results). The plan is `docs/plan.md`; the slices
are in `docs/backlog.md`.

| Milestone | Status | Where | Next |
|---|---|---|---|
| M0 Baseline | in progress | this repository: #1, #12, #13, #14 merged | Owner actions 6 and 8 |
| M1 Execution protocol | implemented | Harness issue #49 (closed): #50 to #53, `main` at `8dd2659` | Windows CI leg on Harness `main`, then verified |
| M2 Durable coordinator | implemented | M2.1 to M2.4c merged, the last as #76 (`4ef4e5c`, 2026-10-16); M2.1 verified | The open follow-up issues from the M2 reviews |
| M3 Project workspaces | implemented | Slices in `docs/backlog.md` (#78); M3.1 #81, M3.2 #85, M3.3 #97, M3.4 #100 | M4 first local workflow |
| M4 First local workflow | not started | | |
| M4b Web access | not started | | After M4; evidence from a second computer (decisions log, 2026-10-13) |
| M5 VMware execution | not started | | Packer and VM storage location, see below |
| M6 Resume intelligence | not started | | |
| M7 Review and integration | not started | | |
| M8 Recovery and routing | not started | | |
| M9 Packaging and dogfood | not started | | |

## M0 evidence

### Harness baseline (2026-10-11)

Harness `main` at `1ca56193998ca0dd1f56e64ef86c67953771f74a`, in a fresh worktree, on the
owner's Windows 11 Pro PC (build 26200), go1.26.4:

| Check | Result |
|---|---|
| `go vet ./...` | pass |
| `go build ./...` | pass |
| `go test -count=1 ./...` | pass, 12 packages |
| `go test -run='^$' -fuzz=FuzzParseSSE -fuzztime=10s -parallel=4 ./internal/agent` | pass |
| `go test -run='^$' -fuzz=FuzzCodec -fuzztime=10s -parallel=4 ./internal/mcp` | pass |
| `bash -n` on the release and api-check scripts | pass |
| gofmt on tracked files | clean |
| `go test -race` | not run: no cgo on this PC; Harness CI runs it on Linux |
| release dry run | not run locally; Harness CI runs it |

The candidate the plan names, `8d521f0` on `antigravity-cli`, is two unreviewed commits ahead of
`main` and has never been pushed. See the decisions log.

### Engine build (2026-10-11)

`bash scripts/build-engine.sh` against that clean worktree built `.engine/hrn.exe` for
windows/amd64 with go1.26.4 and `-trimpath`. Two builds gave the same SHA-256,
`0d361f6e171ef7c1f861489b20fb2ecd1a3e5111cf080e2847aadb442b6fc8bb`. The script refused a checkout
at another revision. Go embedded no VCS revision in these builds, so the script's own revision and
cleanliness checks plus the hash are the evidence, and M1.4's handshake must report the revision as
unknown rather than guess when the stamp is absent.

### Ada bootstrap checks (2026-10-11)

At `8fb42c0`, the last code commit after six review rounds; the commits after it change only docs:

| Where | Command | Result |
|---|---|---|
| Windows 11, go1.26.4, Node 22.16.0 | `bash scripts/check.sh` | pass; `-race` skipped (no cgo); the two permission tests skipped (chmod cannot remove access on Windows) |
| Windows | `bash scripts/test-hooks.sh` | 92 pass, 2 skipped as above |
| Linux container (`golang:1.26`, go1.26.8, uid 1000, no network) | `go vet`, `go build`, `go test -count=1`, `go test -race -count=1` | pass |
| Linux container | `sh .claude/hooks/repo-rules.sh` under dash; the 10 rules tests of `test-hooks.sh`, including both permission cases | pass |
| Windows | `bash scripts/build-engine.sh` on a checkout at another revision, and on a clean worktree at the pin | refused; built the same SHA-256 as before |

Tests seen failing before their fix: the engine's link, FIFO and swap tests (Linux); the rules
tests against each earlier hook; and, against each earlier push guard, the cases the next review
named (10, then 11, then 7, then 7, then 3, then 6). Not run: the guard tests and the desktop lockfile
install on Linux (the container has no Node), the engine's link tests on Windows (no symlink
privilege), and CI, which has no runner yet.

### Model qualification (2026-10-11)

All runs went through `hrn` on the owner's subscriptions, recorded under `~/.hrn/runs`.

| Run | Backend and model | Mode | Outcome |
|---|---|---|---|
| `20261011-112105-4ae8ae` | codex, gpt-6-luna | readonly | ok in 17 s: the planning audit of continuation limits |
| `20261011-112845-c19ad3` | claude, claude-opus-5-5 | readonly | error: weekly limit, reset 13:00 Europe/Bucharest; also warned that the workspace's permission entries were ignored because trust was not accepted |
| `20261011-143545-cad2a6` | codex, gpt-6.1-sol | auto | `go build` failed: the workspace-write sandbox cannot create Go's build cache under `%LOCALAPPDATA%\go-build` |
| `20261011-143731-840fed` | codex, gpt-6.1-sol | auto, cache and temp redirected into `.hrn/` | build ok; followup tests "Access is denied"; batch tests could not read `~/.config/git/ignore` |
| `20261011-143933-1927df` | codex, gpt-6.1-sol | auto, cache, temp and home redirected | runlog, batch, agent and cmd/hrn tests ok; nine followup tests "Access is denied" |

Effective reasoning effort for the Codex runs was the owner's Codex default (`high`), because the
Harness revision used does not pass effort to Codex. Usage was reported in tokens only.

Conclusion: Codex Sol is qualified as a sandboxed implementation worker when every Go or git
command runs after the environment preamble in `docs/backlog.md`. Inside the sandbox it cannot run
the followup package's tests, so the supervisor runs the full suite outside it before any review.
Claude Opus is to be rechecked when the first review runs. Antigravity is not qualified: the
installed `hrn` does not expose it, and its adapter is on the unmerged branch.

### VMware host (2026-10-11, read-only checks)

| Item | Observed |
|---|---|
| VMware Workstation | 17.6.4 build 24832109; `vmrun -T ws list` reports 0 running VMs |
| Tools present | `vmrun`, `vmware-vdiskmanager`, OVF Tool |
| CPU | AMD Ryzen 9 9950X, 16 cores, 32 threads, virtualization enabled in firmware |
| Memory | 61.6 GiB |
| Hypervisor and VBS | hypervisor present; virtualization-based security running, so VMware runs on the Windows Hypervisor Platform |
| Networks | VMnet1 (host-only) and VMnet8 (NAT) up; VMware NAT and DHCP services running |
| Free disk | C: 35 GiB, D: 255 GiB, E: 39 GiB, F: 224 GiB, I: 161 GiB |
| Packer | not installed |

Consequences for M5: the image factory needs Packer and its VMware plugin installed at pinned
versions, which is the owner's call. With E: nearly full, Ada's VM storage should default to a
configurable location on D: or F:. VBS stays on; Ada must not disable it, and VM performance is
measured under it. Nested virtualization inside guests is not assumed. The Ubuntu host for the
fourth release lane is still undecided.

### Dependency pins

Chosen on 2026-10-11 as the newest stable release published at least 14 days earlier, unless noted.

| Component | Pin |
|---|---|
| Go | 1.26.4 (`go.mod`) |
| Node for tooling | 22.16.0 (`desktop/.nvmrc`, `engines`) |
| electron | 44.4.5 |
| react, react-dom, @types/react, @types/react-dom | 19.3.0 |
| typescript | 7.0.2 |
| vite | 8.3.1 |
| @vitejs/plugin-react | 6.1.1 |
| vitest | 5.0.3 (11 days old; see the decisions log) |
| @playwright/test | 1.63.0 |
| electron-builder | 26.15.3 (the `latest` tag; 26.17.0 is on a side tag) |
| @types/node | 22.20.4 |
| modernc.org/sqlite | v1.59.0, added to `go.mod` in M2 when first imported (v1.60.x is younger than 14 days) |

### CI

Until 2026-10-13 the workflow was gated on `vars.CI_RUNNER` and skipped, which did not count as
a pass: #1 and #12 merged on local checks alone. Since #14 the repository, being public, runs on
GitHub's runners (decisions log, 2026-10-13); the first hosted run, on #14, passed `scripts/check.sh`
on `ubuntu-latest` with `-race`.

### M1.1 (2026-10-11)

Harness #50 merged as `0c96142`, after CI passed on the `lab-harness` runner at head `4117bed` and
the full local checks ran again at that head. Codex wrote it (run `20261011-144813-af3194`); Claude
Opus reviewed it (run `20261011-150534-0395ed`) and found the eval judge's effort unchecked, which
was fixed with a test that failed first. Live: codex accepted `low`, `xhigh` and `max` and a forked
session with `medium`; claude accepted `low` and `max`. The CLIs do not report the effective level.

### M1.2 and M1.3 (2026-10-11)

Harness #51 (run records surface write failures) merged as `b9f1733` and #52 (contained process
trees) as `75ba625`, each after CI passed on `lab-harness` at its head and the full local checks
ran again there, with `go test -race` on the touched packages in a Linux container. Codex wrote
both; Claude Opus reviewed each twice. #51's review found a short write that corrupted the next
event; #52's found zombies counted as survivors, a missing explicit kill, and a /proc scan that
gave up under `hidepid`. All were fixed with tests that failed first. On Windows a CLI's leftovers
now end with the run; on Unix the CLI has its own process group and hrn stops on SIGTERM and
SIGHUP. Live, a Codex timeout ended a running `ping`, though the older hrn did too (Codex's own
sandbox cleans up), so the fake-CLI tests are what show the change.

### M1.4 and the new engine pin (2026-10-11)

Harness #53 (`hrn exec --protocol v1`) merged as `8dd2659`, closing #49, after CI passed on
`lab-harness` at its head, the full local checks ran again there, and `go test -race` passed in a
Linux container. The protocol document came first; Codex implemented it in three runs; Claude Opus
found case-variant field names slipping past the option gate and an oversized line that stopped
stdin while labelled non-fatal, both fixed. Live through a protocol client on the owner's plans:
codex and claude read-only runs (`ok`, contiguous `seq`, a session event, `resolved_model` empty
for codex and `claude-sonnet-5-5` for claude), a codex start with `max_turns` rejected with no
record, and a codex run cancelled during `ping -n 120` (`canceled`, no ping left).

`engine/engine.json` now pins `8dd2659b953e9e5d9c593fa655254167ea9f8a8c`. `scripts/build-engine.sh`
on a clean worktree at that commit built `hrn.exe` for windows/amd64 with go1.26.4, SHA-256
`82523613251daf7624ca3a16b66826eb5c16a050e423c85f61aeb6d865fe5656`, which answers
`hrn exec --protocol v1` with a v1 `hello` (revision reported as unknown, as expected without a VCS
stamp). M1 stays "implemented" until Harness's Windows CI leg passes on `main`.

### M2.1 store (2026-10-11 to 2026-10-13, #18 and #24, verified at `09ff3bf`)

`internal/store` on modernc.org/sqlite v1.59.0, written in runs `20261011-165945-8a1560`,
`20261011-171916-6142ca` on a branch stacked on the bootstrap, at `6925b33`. Checks:
`bash scripts/check.sh` on Windows passes; offline in a Linux container, `go vet ./...`,
`go test -race -count=3 ./internal/store/` and `go test -race -count=1 ./...` pass. Claude Opus
reviewed it twice (runs `20261011-171544-53b71d`, `20261011-173013-a06dc5`): the first found the
refinement edge undocumented and the disk-full criterion unplaced (both decided below) and asked for
a transaction handle for idempotent requests, migrations recorded in the database, and synced
backups, all done; the second found nothing blocking. Follow-ups before its pull request: wrap the
cause into the busy-retry timeout error; make the tamper tests match their error messages; remove a
backup file whose sync failed; give the concurrent-backup test its own context; restore pragmas a
test changes on pooled connections. Opening its pull request waits for the bootstrap to merge. A
database created at its first commit (`77015e4`) will not open any more; none exists outside tests.

2026-10-13: the branch was rebased onto `main` (`e4a2c14`, after the amendment of #13), and
run `20261013-183524-47c2e9` (mode auto) added users, project membership, task and attempt
actors, membership-scoped task and event calls and per-user idempotency (`88f0a7a`) and the five
review follow-ups (`8dbc89a`); the identity tests failed before the schema and API existed, the
busy-timeout cause and failed-backup removal tests before their fixes. Inside the sandbox
`go test -count=3 ./...` passed and the commits could not be written (the worktree's index lock);
outside it `bash scripts/check.sh` passed on the owner's PC (`-race` skipped, no cgo; the two
chmod hook tests skipped on Windows). The pull request is still to open, after an independent
review; nothing is verified or ticked. Review run `20261013-185055-ec8024` found the replay gap,
fixed with the scope gate and its regression test; its two other findings are #15 and #16.

Later on 2026-10-13, the merges: #18 at `205c128`, CI green on ubuntu-latest (run 38427995495); `bash scripts/check.sh` on the
owner's PC passed at that head (run `20261013-201027-c1982b`); a security read of the whole diff
(run `20261013-200746-cb6e25`) found nothing blocking and three gaps: idempotent responses not
bound to their project (#20), unknown and foreign tasks told apart (#21), and the database, its
sidecars and backups created readable by other users on Unix, fixed in #24 and so not filed as
an issue.
#24 (`3f20c6b`): the database file is created 0600 before SQLite opens it, backups are created
with `O_EXCL` and 0600; `bash scripts/check.sh` on Windows and, in a `golang:1.26` container as uid
1000, `go vet`, `go test` and `go test -race` on Linux passed (run `20261013-202333-1a9696`);
review run `20261013-201921-3d3373`, its missing tests added, the rest #23; the same review was
its security read, #24 being the fix itself; CI green on ubuntu-latest (run 38438816831). On
`main`, CI passed on ubuntu-latest and windows-latest at `36174d9` (run 38436772635) and at
`09ff3bf` (run 38441899352), so M2.1 is verified at `09ff3bf`.

### M2.4c candidate (2026-10-16, #75 with #69)

Commit `16c77bd` on `m2/window` (PR #76), based on
`5e03a281ef5d747472bbaa15876c529ea54892cc`. No commit or push; M2 is not marked verified
and the backlog is not ticked. The supervisor still owns the independent review, one commit,
pull request and hosted CI checks. Local evidence:

| Check | Result |
|---|---|
| `& 'C:/Program Files/Git/bin/bash.exe' .hrn/check.sh`, `GOFLAGS=-p=4` | Pass, exit 0: all Go checks, repository rules/hooks, lockfile installs, UI/desktop typechecks, 24 UI tests, 36 desktop tests, builds and five e2e scenarios; `.hrn/check-direct.log` / `.hrn/check-direct.exit` |
| `go test -count=3 ./...`, Windows, `GOFLAGS=-p=4` | Fail, including a serial repeat: unchanged executor cancellation/exit tests reached `recovery_required`; `.hrn/windows-repeat.log` |
| `& 'C:/Program Files/Git/bin/bash.exe' .hrn/linux-test.sh` | Pass: non-root `golang:1.26` container, read-only checkout; `go vet ./...`, `go test -count=1 ./...`, `go test -race -count=1 ./...`, all seven packages |
| UI clean lockfile install, typecheck, Vitest, build | Pass; 24 tests, including selection of a pre-existing attempt and App through the complete bridge adapter against one fake adad |
| Desktop clean lockfile install, typecheck, Vitest, build | Pass; 36 tests, including bounded startup/exit, detached spawn/unref, SID/name/configuration, repeated and refused Quit, sender checks and stop timeout |
| Windows native job tests | Pass: launcher-owned job destroyed with the coordinator still serving, both native breakaway and the no-breakaway broker path; spaces, shell metacharacters and Unicode in the state path |
| `bash scripts/test-e2e.sh` | Pass in the final full gate: **5 passed (47.7s)**, one worker, real adad and the hashed test-only fake engine |
| `git diff --check` | Pass on the uncommitted changes (the wrapper's committed-range check alone would not cover them) |

Playwright scenarios passed:

1. Closing the window keeps adad and events alive; reopening replays every event, including
   sequences received by a Node subscriber while no window was open, without gaps or duplicates.
2. Quit cancels running and queued work, displays the returned reason/counts, and exits;
   a reopened window shows both tasks canceled with their persisted stop reason.
3. A second app finds the same coordinator (same PID and creation identity, one matching process);
   a fake successful task reaches the existing M2 `verifying`/`ended_ok` states; the menu quits
   while the first window remains open, and that window does not restart the coordinator.
4. The UI displays queue position and `queue_full`; withdrawal cancels a queued task without
   starting an engine and frees its queue place.
5. A Node client reconnects after a forced coordinator restart during a 900-event replay spanning
   multiple pages, and receives each sequence exactly once. The same test uses a pipe on Windows
   and a socket in Linux CI.

Regression tests were seen failing before the attempt field, subscribe-on-selection, lifecycle
helpers, queued withdrawal and Quit wiring were implemented. The first real Windows run exposed
the inherited job; the restrictive-job native regression failed before the broker fallback.
The first full Windows gate stalled in the existing shell rules hook; even a standalone
`xargs --version` probe was delayed. It was stopped and restarted with an output log in a hidden
same-user process. That run passed Go, hooks, UI and desktop checks, then timed out waiting
for the first Electron window and during its cleanup; it was stopped, and the fixture cleanup
was bounded for an absent/exited window. The fresh direct Git Bash full run then passed,
including all five e2e scenarios. The two permission-only hook cases skip on Windows because
`chmod` cannot remove access; hosted Linux CI still owns those cases.
No check was bypassed. No live model calls ran.

The overlapping Windows three-repeat run reported
`TestTheCancelVersusClaimRaceHasOneWinnerWhicheverSideIsFirst` (`ended_failed` instead of
`canceled`, twice) and `TestPartialLineAfterASuccessfulResultFailsTheAttempt`
(`recovery_required` instead of `ended_failed`). These tests and executor code were not changed;
the same packages passed the earlier full single run and Linux race run. The serial repeat
also failed: cancel-versus-claim reached `recovery_required`, and
`TestResultBeforeNaturalExitAlwaysEndsOK` reached `recovery_required` after an `ok` result
with reason "engine had to be killed after its result". All other packages passed three times.
This remains unresolved; no executor behavior or deadlines were changed to conceal it.
These failures need supervisor investigation before acceptance; this slice does not change
executor behavior beyond the test-only protocol helper.

Not verified here: Linux Playwright (runs in CI), hosted Windows/Linux CI, packaged installation,
or an independent review. Windows has no cgo; the Linux race run supplies that evidence.
The self-review found no open security issue in the changed bridge, sender validation,
renderer isolation, detachment argument handling or test-only build boundary. It is not
an independent review, and the unresolved repeated Windows executor failures remain open.

### M2.4c review fix round (2026-10-16, #76)

Commit `58eaffa` over `16c77bd2e6e4970241dc532ad453dc461d48ddb0` on `m2/window` (PR #76).
The supervisor owns the single commit, renewed independent review and hosted CI; the milestone
and backlog remain unchanged. Executor source and tests are unchanged.

The endpoint now proves possession of a fresh 32-byte `<state>/adad.token` on every Go and
Node HTTP/SSE connection, before HTTP bytes are sent. The token is atomically replaced under
the singleton lock, 0600 on Unix and created with the pipe's protected owner-only SDDL on
Windows (descriptor read back in a native test). The bounded `ADANEXT1` nonce/HMAC exchange
retains Go's SID/UID checks and fails closed without spawning. Handshakes have an absolute
five-second deadline and a cap of 64 pending connections; closing the listener closes them.

Owner stop and signals now wait for the executor's `StopBudget` before draining streams and
shutting HTTP down. Executor stop runs once, including error cleanup. Electron waits up to
70 seconds (the default 55-second executor budget plus 15 seconds), showing native
indeterminate progress and a waiting title. Its existing reason/counts remain visible.

New coverage: Go wrong-token refusal, malformed/silent clients, HTTP after authentication,
token rotation and token permissions; real Node wrong-key and malformed-proof peers,
fresh nonces, no HTTP payload/no spawn, silent/trickling handshake and HTTP deadlines;
same-project task/attempt reads, unrelated newer attempts and removal of membership.
Playwright adds synchronized real cold starts and a test-only engine shim that delays
cancellation four seconds, writes its PID/creation-time exit marker, and must be gone
before Electron exits. The shim lives under `desktop/e2e/`, behind `fakeengine`; no executor
file was changed.

Final checks on the uncommitted fixes above, `GOFLAGS=-p=4` on Windows:

| Command | Result |
|---|---|
| `& 'C:/Program Files/Git/bin/bash.exe' .hrn/check.sh` | Pass, exit 0: vet, module tidy/verify, build, all Go tests, rules/hooks, clean lockfile installs, both typechecks/builds, 24 UI tests, 43 desktop tests and **7 Playwright scenarios (40.9s)**. `.hrn/fix-round-check.log` and `.exit`. Windows race detection and two chmod-only hook tests skip as before. |
| `& 'C:/Program Files/Git/bin/bash.exe' .hrn/linux-test.sh` | Pass, exit 0: requested non-root, read-only `golang:1.26` Docker invocation; `go vet ./...`, `go test -count=1 ./...`, `go test -race -count=1 ./...`, all seven packages. `.hrn/fix-round-linux.log` and `.exit`. |
| `go test -count=3 ./internal/ipc ./cmd/adad ./internal/api` | Pass, exit 0; 60.863s / 13.232s / 33.195s respectively. `.hrn/fix-round-repeat.log` and `.exit`. |
| `git diff --check` | Pass on the uncommitted changes, in addition to the wrapper's committed-range check. |

Named new Go regressions, all passing: `TestDialRefusesAnEndpointWithTheWrongToken`,
`TestListenerClosesMalformedAndSilentClientsBeforeHTTP`, `TestHTTPWorksAfterEndpointAuthentication`,
`TestEndpointTokenIsPrivateAndReplacedOnRestart`,
`TestTokenDescriptorGrantsOnlyTheOwnerWithoutInheritedACEs` (Windows), and
`TestTaskAttemptsStayScopedToTheTaskAndCurrentMembership`.
The existing `TestTaskReadsIncludeTheCurrentAttemptAndQueuePosition` also passes.
New Vitest cases in `desktop/src/endpoint.test.ts` cover the real wrong-key server (including SSE),
non-ASCII proof bytes and four absolute deadline cases (`silent`, `trickle`, `http-silent`,
`http-trickle`), all passing. The new lifecycle case verifies the default 70-second bound.
Wrong-key Node behavior failed before the transport change; the non-ASCII proof test first
exposed an uncaught constant-time-comparison length error, then passed after strict UTF-8
validation. The new Go boundary tests first failed to compile before the raw/authenticated dial
split. The delayed engine fixture initially failed to start because it used environment variables
not passed to the engine; its per-fixture sidecar now supplies its test-only configuration.
An absent-process PowerShell assertion was corrected to exit successfully when the process is gone.

Playwright, all passing: window close/reopen with continuing events; running/queued Quit and
persisted reason; a second app/menu Quit without restart; **synchronized cold starts both connect
to exactly one real coordinator**; **Quit keeps Electron and the endpoint alive until the delayed
engine exits** (exit marker read at Electron's process exit, plus OS PID/creation-time comparison);
queue-full/withdrawal; and coordinator restart during a 900-event replay without gaps/duplicates.

The first Linux run was superseded after a generated root Vitest cache was moved while Go was
enumerating the checkout (`pattern ./...: open node_modules: no such file or directory`);
the fresh stable-tree run above passed all gates. No check was skipped to obtain a pass.
Not verified here: Linux Playwright, hosted Windows/Linux CI, packaged installation or a renewed
independent review. The security read of these fixes has no open finding; it is not independent.
No live model calls ran, and no merge, tag or release was made.

Windows executor comparison, both with `GOFLAGS=-p=4`, `go test -count=3 ./internal/executor/`:
`origin/main` at `5e03a281ef5d747472bbaa15876c529ea54892cc` passed (180.092s) in
`.hrn/executor-baseline`, then that detached temporary worktree was removed. This branch at
`16c77bd` plus the uncommitted fixes passed (175.797s). This observation does not establish
that the intermittent failures recorded above have been fixed.

### M3.1 safe git and inspection (2026-10-16 to 2026-10-17, #80, PR #81)

`internal/source` and `POST /v1/projects/inspect`, as the M3.1 entry of `docs/backlog.md` and the
decisions-log entry of 2026-10-16 ("M3 in four slices") describe them. Implementation and fix
rounds ran as Claude Opus runs through hrn (`20261016-172957-7df2c1`, `-204148-fd3076`,
`-221139-e72776`, `-233544-ff2c1e`); three independent read-only reviews, each also the security
read (`20261016-190627-df5d74`, `-214007-c65c38`, `-231407-f5e92b`), are on the pull request.

Evidence at `3ca8e3a`: `.hrn/check.sh` passed, that is `bash scripts/check.sh` on Windows (all
steps; `-race` left to Linux for want of cgo) and `go vet`, `go test` and `go test -race` for
every package in `golang:1.26` in Docker. Earlier rounds met only the executor timing flakes of
#72 on Windows, each passing on a re-run. The marker tests prove that no filter, fsmonitor,
textconv, external diff, ssh command or hook the repository, its submodules or the global and
system config define runs; every original file is hashed before and after.

Not verified: a repository in OneDrive with online-only metadata (#83); macOS.

### M3.2 import and snapshots (2026-10-17, #84, PR #85)

Migration 0005 (`project_sources`, `source_snapshots`, `snapshot_untracked`), `POST /v1/projects`,
`POST /v1/projects/{id}/snapshots` and `GET /v1/projects/{id}/snapshots/{sid}`, as the M3.2
entry of `docs/backlog.md` describes them, with #79's M3.2 decisions: a second import of a path
answers 409 `already_imported` (the project's id and tasks for a member, nothing for others), and
a nested repository is refused as a directory. Snapshot writes share the artifact lifecycle lock
that now lives on `artifact.Store`, so the executor's discard and the start-up sweep cannot remove
a blob a snapshot is committing; a failed snapshot removes the blobs only it wrote. One capture
runs at a time (429 `capture_busy`). A snapshot holds that lock while it writes, so event appends
wait for it. Implementation and fix rounds ran as Claude Opus runs through hrn
(`20261017-003314-18e186`, `-051104-ecce19`, `-061251-c3bcb6`/`-063427-e448c1`); the independent
reviews, each also the security read, are on the pull request.

Evidence at `3ef98fa`: `.hrn/check.sh` passed, that is `bash scripts/check.sh` on Windows (all
steps including the seven Playwright tests; `-race` left to Linux) and `go vet`, `go test` and
`go test -race` for every package in `golang:1.26` in Docker. The core test applies the staged
and unstaged patches to a clone of the base commit and writes the untracked files, and gets the
original's work tree byte for byte; every original file is hashed before and after.

Not verified: Windows file symlinks (junctions cover Windows), macOS, a real patch over 64 MiB.
Later: opening each path component through its parent's handle (M3.3), import authorization for
more users (M4b), artifact origins when content repeats (#89).

### M3.3 managed clone and task workspaces (2026-10-19, #95, PR #97)

Migration 0006 (the nullable `tasks.snapshot` and the `workspaces` table), a task bound to a
snapshot at creation, and Ada's bare clone of each imported project at
`<state>/projects/<id>/repo.git`, cloned from a freshly built mirror on the first start that
needs it and fetched the same way on every later one, under a per-project lock. Each attempt gets
a worktree on `ada/<task>` at the snapshot's base with the imported changes committed as Ada, and
a manifest the executor checks against the workspace row, its hash, the worktree's git directory
and the branch's HEAD just before the engine starts; any difference moves the attempt to
`recovery_required` and nothing is rebuilt. Branches named `ada` or `ada/*` in any case never
reach the clone. The decisions are in the 2026-10-19 entry of the decisions log. Implementation
and fix rounds ran as Claude Opus runs through hrn (`20261018-232512-8755a2`, fixes
`20261019-022108-2c77ab`, `20261019-033845-327876` and the case-insensitive `ada` fix); the
independent reviews, each also the security read, are on the pull request.

Evidence at the commit that adds this section: `.hrn/check.sh` passed (`== windows exit 0, linux
exit 0`), that is `bash scripts/check.sh` on Windows (all steps including the seven Playwright
tests and the Windows-only test of a branch `Ada`; `-race` left to Linux) and `go vet`, `go test`
and `go test -race` for every package in `golang:1.26` in Docker.

Not verified: macOS, a real hrn run in a worktree, removal of worktrees and branches (M7), a
task's second attempt (#98).

## Decisions log

- **2026-10-16, #76 review fixes: endpoint proof and shutdown order.** A predictable per-user
  pipe name plus Node transport is not server authentication: another account can squat on
  the name. Require `ADANEXT1 <32-byte nonce in hex>\n`, answered with hex
  HMAC-SHA256(token, `adanext-endpoint-v1` followed by the nonce bytes), on every local
  connection before HTTP. The token never crosses the bridge or appears in logs. Rotate it
  only under the coordinator lock; Windows uses an explicit owner-only DACL rather than
  relying on inherited state-directory ACLs. Endpoint disappearance must follow executor stop,
  for both owner requests and signals, so Electron's bounded Quit wait observes real work
  termination rather than early listener closure. This amends shutdown ordering described in
  M2.4a below without changing executor policy, the database or the engine pin.

- **2026-10-16, M2.4c window lifetime (#75, folding in #69).** Task reads join the
  newest attempt (insertion order, including terminal attempts) and its global queue position
  in the same SQLite query as the task and membership check. The existing top-level
  `queue_position` stays for compatibility; the new `attempt` is null when absent. Selection
  subscribes to that attempt, including after reopening a window. No schema or engine-pin change.
  Desktop startup uses `ADANEXT_ADAD` or the binary next to the app, the user's state directory,
  and the configured `ADANEXT_ENGINE`/`ADANEXT_ENGINE_SHA256`. `ADANEXT_STATE_DIR` and
  `ADANEXT_NAME` permit isolated instances/tests. The old explicit socket override remains a
  connect-only option. Windows resolves the current SID with the system `whoami /user /fo csv /nh`
  command and a tested parser; Unix uses `<state>/run/<name>.sock`. Startup and stop waits are
  bounded. The menu and button use the same typed bridge flow: show adad's returned counts and
  reason, wait for endpoint disappearance, then close the app. Running cancellation is described
  as requested until the persisted task can be read; reopening shows its actual terminal state.
  Only app launch starts the coordinator; existing windows reconnect without starting it,
  so another open window cannot undo the owner's explicit stop. The two-window Quit test
  first failed with a replacement coordinator, then exercises this boundary with both open.
- **2026-10-16, Windows job detachment observed and tested (#75).** A real Electron run here
  inherited a job, and plain Node `detached: true` left adad in it. The Git Bash launch path had
  job flags `8192` (`KILL_ON_JOB_CLOSE`, no breakaway), whereas the direct PowerShell path had
  `10240` (also `BREAKAWAY_OK`). Native breakaway under Git Bash returned access denied.
  The desktop now passes `-detach`: before opening state, adad requests native breakaway;
  on access denied it uses the same-user `Win32_Process.Create` broker through the system
  PowerShell, hidden, with an eight-second bound, no elevation or installed service. Arguments
  are Windows-escaped data, separately encoded from the script; the environment passes in memory,
  not in argv or files. Broker failure never falls back to serving inside the desktop's job.
  A native test destroys a launcher-owned job and verifies the coordinator still serves, both
  with and without breakaway permission, including paths with spaces, metacharacters and Unicode.
  The final Git Bash Playwright observation was `Electron in_job=true, adad in_job=false`.
  A non-desktop ancestor job may remain on the native path; the launcher-owned job's lifetime is
  what the native test destroys. This follows the documented
  [Windows job inheritance and broker behavior](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects)
  and [process startup properties](https://learn.microsoft.com/en-us/windows/win32/cimwin32prov/win32-processstartup).
- **2026-10-16, adad re-execs on Windows even outside a job (#76).** Run from a PowerShell
  launch, Electron's children were outside any job, so `-detach` served in the process Electron
  spawned. Node spawns with handle inheritance on, so that coordinator held Electron's inheritable
  handles, among them the stdout and stderr pipes Playwright reads: Electron exited 120 ms after
  its last window closed, but its pipes closed only when adad stopped, and the window-close e2e
  timed out. `-detach` now always re-execs before opening state; Go's child gets only its own
  null stdio, and the spawned process exits. `TestCoordinatorOutsideAnyJobKeepsNothingTheLauncherInherited`
  failed before the change when run outside a job (the launcher's output stayed open for 15 s).
  The broker path still holds the inherited handles until the broker returns (at most 8 s
  after launch).
- **2026-10-16, adad re-execs on Unix too, with every inherited descriptor close-on-exec (#76).**
  On ubuntu-latest the window-close e2e still timed out. In the Playwright 1.63.0 noble image,
  Electron exited (code 0) 32 ms after its window closed, but Playwright's `close` came only after
  5 s: Electron's fds 3 and 4 (the extra stdio channels Playwright spawns it with) and Chromium's
  own descriptors without close-on-exec were open in adad and in its engine child. Node's
  `stdio: "ignore"` replaces only fds 0-2, and Go's `os/exec` does not close inherited
  descriptors either, so a plain re-exec still kept them (observed with the new test). `-detach`
  on Unix now marks every descriptor above 2 close-on-exec, then re-execs into a new session
  with null stdio, and the spawned process exits. `TestCoordinatorKeepsNothingTheLauncherInherited`
  hands the launcher a pipe as fd 3 and requires its EOF while the coordinator serves; it failed
  before the change both without a re-exec and with a plain one.
- **2026-10-16, M2.4c test boundary (#75).** The fake engine and project/membership seed helper
  require `-tags=fakeengine` and live in test-only paths. Playwright 1.63.0 uses the real built
  adad and the hash of the fake engine, one worker, unique endpoint names and temporary state.
  The shared App also runs through the full bridge adapter against one fake HTTP adad; the
  HTTPS adapter remains M4b. UI tooling pins `@types/node` to desktop's existing 22.20.4 solely
  for that integration test. `check.sh` invokes `test-e2e.sh` after the builds, installs the pinned
  Electron binary explicitly, and builds the three Go executables into a temporary directory.
  Ubuntu CI installs xvfb and uses it when DISPLAY is absent; Windows runs directly. Only local
  `ADANEXT_SKIP_E2E=1` skips the suite visibly; CI rejects that override.

- **2026-10-11, engine baseline.** Ada pinned Harness `main` (`1ca5619`), not `8d521f0`, and
  moved the pin to `8dd2659` once M1.4 merged. The
  `antigravity-cli` branch is another session's unreviewed work and the owner has not decided
  whether it lands; nothing in M0 to M2 needs the Antigravity backend. M1 branches from `main`,
  and the pin moves to the `main` commit that contains M1.4. Revisit when the owner decides.
- **2026-10-11, M1 as four Harness slices.** Effort mapping, runlog write errors, CLI process
  trees, and the `exec` protocol each get their own Harness pull request from `main`, merged in
  that order. They run one at a time, as `docs/plan.md` section 8 requires for M0 to M2.
- **2026-10-11, delegated workers.** Implementation slices go to `codex / gpt-6.1-sol` through
  `hrn` in mode `auto` (Codex's workspace-write sandbox) with the environment preamble. Never
  `yolo`, never a permission bypass. Reviews go to Claude Opus when available, otherwise a fresh
  Codex Sol run.
- **2026-10-11, vitest 5.0.3.** Versions 5.0.0 to 5.0.2 crash npm 10.9.2's peer resolver
  (`Cannot read properties of null (reading 'edgesOut')`); 5.0.3 is the first that resolves.
- **2026-10-11, one list of checks.** `scripts/check.sh` is run by both CI and `/check`, so the
  two cannot drift.
- **2026-10-11, GitHub repository.** Creating `cristiangirlea/ada-os` was refused by the
  permission check, so the owner created it, private, with a README. The bootstrap, built locally
  meanwhile, was replayed onto that `main` (`git rebase --onto origin/main --root m0/bootstrap`,
  keeping this README) so its pull request shares history with `main`. The content was
  byte-identical afterwards; commit ids changed, and the ids in this file are the new ones.
- **2026-10-11, merging.** The owner started implementation with `/autonomous-delivery`, whose
  process merges through the pull request once CI on the owner's runner, the full local checks,
  an independent review and a security read all hold for the head being merged. That is the
  owner's standing rule for their repositories, and this repository's AGENTS.md follows it; it
  replaces the stricter per-merge authorization `docs/plan.md` section 8 kept "until the owner says
  otherwise". Infra merges still need the owner's yes, because they deploy.
- **2026-10-11, bootstrap review.** The independent review (Codex Sol, read-only, run
  `20261011-144717-71996c`) found five blocking problems, all fixed: push-to-main and force forms
  the deny patterns missed (now also a pre-command hook with tests), scripts that could read a
  failed command as success, a post-edit hook that split file names with spaces and ignored
  inspection errors, an engine identity check that followed links and could block on a FIFO, and
  this merge rule lacking a recorded decision. Its non-blocking points were taken too: the build
  script enforces the pinned toolchain, and the backlog's IPC is back to the plan's named pipe.
- **2026-10-11, bootstrap re-review.** The second review (Codex Sol, run
  `20261011-152631-5f6c59`) found three of the fixes incomplete, all fixed again: the push guard
  became an allowlist (one named feature branch) after a blocklist missed `-uf`, `:` and wildcard
  refspecs; the rules hook reads each file on its own because git grep reports an unreadable file
  as no match; and the engine opens its binary without following links or blocking, so a swap
  after the path check is refused. The build script pins the toolchain where the build runs, and
  check.sh compares settings.json with its generator.
- **2026-10-11, refinement goes back to ready.** The plan's state diagram ends at `refinement`, but
  section 3 says refinement "stays in the task lineage and preserves work", so the store allows
  `refinement` to `ready` and nothing else out of it. The review of M2.1 (Claude Opus, run
  `20261011-171544-53b71d`) asked for this to be recorded.
- **2026-10-11, disk-full on the artifact writer moves to M2.3.** There is no artifact writer in
  M2.1; artifacts arrive with the executor, so the criterion moved with them.
- **2026-10-11, the push guard's design.** Three more review rounds (Codex Sol, runs
  `20261011-154345-0b857e`, `20261011-155721-4523e7`, `20261011-160325-a0e847`) each found new
  ways to spell a push the guard missed: quoting, `-c` and `GIT_CONFIG_*` overrides, tags,
  continuations, `cd`, nested shells, subtree, plumbing, aliases, substitutions inside double
  quotes. Parsing every shell spelling cannot be won, so the guard stopped trying: a command that
  mentions a push must be exactly one plain `git push <remote> <branch>` on its own, run in the
  session's directory, with the branch checked against the effective git configuration; aliases
  that push or run a shell are refused; a single non-pushing git command with literal arguments
  still passes. It is a guard rail for commands Claude runs directly, not a sandbox: a push from a
  script file is out of its reach, which the hook's header says. A sixth review (run
  `20261011-161646-3924a6`) found `--upload-pack` running a shell and gaps in alias resolution;
  fixed, but it confirms the point: a text check is a guard rail against mistakes, not a security
  boundary. The boundary for `main` is branch protection on GitHub (owner action 6). Review rounds
  on the guard stop here; anything further is recorded as a known limit.
- **2026-10-13, the name.** "Ada OS" is another product's name, so this one is AdaNext OS: the
  brand word `adanext`, one word, as in the domain `adanext.si` and the `adanext-map` repository;
  short form AdaNext. The repository becomes `adanext-os`: the owner renamed it on GitHub the same
  day (the old URL redirects). Binaries keep short names,
  as `adamap` does: `adad`, `ada-guest`. `docs/plan.md` keeps its original text and names, as the
  docs rule asks; everything else says AdaNext OS.
- **2026-10-13, three forms of one product.** AdaNext OS ships as an installed application (the
  Electron shell, as planned), as a web interface (`adad` serves the same React UI over HTTPS to
  browsers on the owner's own network, each person signed in as themselves), and later as a
  bootable system (an image that runs `adad` and the web interface: the plan's "bootable Linux
  distribution"). One UI and one coordinator; the shells differ only in how they reach it.
  Reason: the first second user works from another computer on the same network, and the
  bootable form is the web form preinstalled, so designing the web form now makes the later
  system a packaging task rather than a new product.
- **2026-10-13, one API for both shells.** `adad` speaks HTTP/1.1, with server-sent events for the
  event stream, on the per-user named pipe or owner-only Unix socket of M2.2 (Go serves HTTP on
  any listener; Electron's main process reaches a pipe through Node's `socketPath`), and the web
  form is the same server on a TLS listener. M2.2's framing changes from NDJSON to HTTP and SSE;
  the pipe and its permissions, the versioning, idempotency keys, expected revisions and
  reconnection from a sequence number stay. Reason: two framings of one request set would drift,
  and the renderer's narrow typed API (`docs/plan.md` section 2, desktop boundary) was designed
  for an untrusted renderer, which makes it the right surface for a browser too.
- **2026-10-13, people in the store.** M2.1 adds a `User` record and project membership, and an
  actor on `Task`, `Attempt`, `Review` and `PermissionDecision`; a permission prompt goes to the
  task's actor. The desktop form has one implicit local user. Reason: a column now is cheap and a
  migration later is not (`docs/plan.md` section 8 used the same argument for the concurrency
  cap), and M5 to M8 build on this schema.
- **2026-10-13, whose provider account runs a task.** Every AdaNext user has a credential profile:
  the engine runs with `CLAUDE_CONFIG_DIR`, `CODEX_HOME` and the other CLIs' equivalents under
  that user's directory in `adad`'s state, so each person signs the provider CLIs in once (the
  provider's own browser or device flow, relayed through the web interface) and their tasks run
  on their own plan. A task never runs on another user's login. The selection fails closed: an
  attempt whose actor has no profile, or whose sign-in the CLI reports invalid, is refused with no
  fallback to another login, and a continuation stays in the profile of the attempt it continues.
  Because the engine passes its environment on to the CLI (Harness `internal/agent/cli.go` at the
  pin), the launcher builds the environment explicitly, with nothing of `adad`'s own
  configuration or secrets in it, and the execution boundary must keep the other profiles and
  `adad`'s state out of a task's reach: which mechanism (a separate OS account per profile, or
  the VM executor of section 4) is decided and tested before M4b lets a second user run anything,
  since the plan already says a worktree alone is no protection against hostile code (section 4).
  Reason: the rule in Harness's durable-workers specification never to share the owner's account
  implicitly, and the subscriptions' terms; the plan already applies the same rule inside guests
  (section 4).
- **2026-10-13, reaching it from the network.** Loopback only, by default. The network listener is
  a setting: one interface address, TLS required (certificate and key together or neither, as
  Harness's server does), a name under the owner's domain (`home.adanext.si`, a DNS-only record
  on the private address) with a Let's Encrypt certificate from the DNS-01 challenge through a
  Cloudflare API token scoped to that zone's DNS, renewed by `adad`. The first web slice takes
  the certificate from files; the ACME client is the slice after. Accounts are local and created
  by the owner, never from the network; the credential scheme (passkeys, or a password with a
  memory-hard hash) is decided in the slice; sessions are cookies. Reason: the owner's rule that
  servers bind loopback unless asked; a certificate from a public CA spares every device a
  private CA, and DNS-01 is the challenge that works for a host nobody outside can reach.
- **2026-10-13, M4b Web access.** A milestone between M4 and M5: the TLS listener, accounts and
  sessions, per-user project visibility and permission routing, credential profiles, and a
  Playwright run from a second browser context. Lead model Sol at high effort (it is persistence
  and permissions), with an independent review. Its completion evidence is a real run from a
  second computer, not a second browser context alone: a browser on another machine reaching
  `https://home.adanext.si`, a separate local account, that user's own project and task on their
  own provider sign-in, a permission prompt answered by that user and invisible to the other, and
  reconnection after a dropped connection. Section 6's v1 gate gains a fifth lane for it, the web
  workflow from a second computer against a Windows host; an Ubuntu host joins when lane 4's host
  exists. The four desktop lanes stay. M4b completes before M5 completes; section 8's parallel
  policy, under which the M5 image factory and VMware spike may start from M3, is unchanged.
  Reason: the first second user is on the network now, and multi-user state is cheapest before
  M5 to M8 build on the store.
- **2026-10-13, adanext.si and public repositories.** The site distributes the product: downloads,
  documentation, later a sponsorship link. It hosts no AdaNext service, no accounts and no model
  calls; nothing here is for a hosted or many-tenant deployment, and Harness's `server/` work
  stays out of it. The repositories go public under MIT, like `harness` and `adanext-map`.
  With it: a LICENSE (M0.4, #12), and the owner's call on which machine details and local paths
  stay in `docs/plan.md` and this file. Branch protection on `main`, which GitHub Free denies a
  private repository, becomes available (owner action 6). The owner flipped the visibility the
  same day. Reason: the owner's decision of 2026-10-13, replacing "private" of 2026-10-11.
- **2026-10-13, CI on GitHub's runners.** The owner's rule for public repositories: they run on
  GitHub's runners, free for public repositories, never the lab runners (a stranger's pull
  request must not reach the owner's PC). So this repository has no scale set and no `CI_RUNNER`
  variable: pull requests run on `ubuntu-latest`, pushes to `main` on `ubuntu-latest` and
  `windows-latest` (#14; infra #61 removes the `lab-ada-os` scale set and lists the repository as
  GitHub-hosted). `docs/plan.md` section 8's runner gap and the CI notes above are superseded;
  the lab runner's limits no longer bound this repository's jobs.
- **2026-10-13, repositories.** `adanext-os` holds `adad`, the UI, the Electron shell,
  `ada-guest` and packaging, because the API and the UI change together and splitting them
  would mean releasing the UI as an artifact for `adad` to embed. `adanext-images` (VM templates,
  the image factory, later the bootable system) is created when M5 starts: another toolchain,
  large artifacts, its own cadence. The site is a repository of its own. In this repository the
  React app lives in `ui/`, loaded by the Electron shell and served by `adad`, from M2.4;
  `desktop/` keeps the Electron shell and its pins.
- **2026-10-13, amendment review.** The independent review of this amendment (Codex Sol, read-only,
  run `20261013-121541-eba24c`) found two blocking gaps, both fixed above: the credential profiles
  promised isolation without an enforceable boundary (now: selection fails closed, an explicit
  environment, a boundary mechanism decided and tested before a second user runs anything), and
  M4b's evidence could pass on one machine (now: a real run from a second computer, a fifth lane
  of the v1 gate). Its eight non-blocking findings are issues #3 to #10: identity and
  authorization semantics, the SSE cursor contract, the browser boundary of the shared UI, LAN
  session security, the provider sign-in relay and approval prompts, the Cloudflare token's blast
  radius and renewal, stopping a shared coordinator, and the images compatibility manifest.

- **2026-10-13, identity in the store (issue #3, part one).** An actor is a `users` row,
  always supplied by the caller from its authenticated context, never from request bodies. The
  desktop form will create one implicit local user when `adad` first opens; the web form's accounts
  come in M4b. A project has members with role owner or member, and every task, attempt, event and
  listing is reached only through membership of the task's project. Idempotency keys are per user.
  An attempt's actor is a member at creation, and a continuation keeps the actor of the attempt it
  continues. Who may cancel, review or approve, and what a revoked membership does to a pending
  permission prompt, are decided with the executor (M2.3) and review (M7) slices, where those
  records arrive.

- **2026-10-13, M2.2 in two slices.** M2.2 is split into M2.2a, `internal/ipc` with the
  single-coordinator lock (#25), and M2.2b, `cmd/adad` with the `/v1` API (#26), so each is
  reviewable on its own. M2.2a's choices: the Windows pipe through go-winio v0.6.2 (v0.6.3 since #38; first instance,
  remote clients rejected, owner and DACL the user's SID alone), and the Unix socket under the
  state directory rather than `$XDG_RUNTIME_DIR`, so that the lock and the socket cover the same
  coordinators.

- **2026-10-13, M2.3b execution policy, states and recovery.** Start names the backend and mode
  and may name a model and effort; the same transaction moves the task to preparing and records a
  pending attempt with its actor, policy, local executor, coordinator generation and engine hash.
  Migration 0004 adds the attempt mode and a coordinator generation row without changing earlier
  migrations. Preparing may fail when launch is refused or interrupted when the coordinator stops
  before acceptance. Any current project member may cancel; protocol v1 cannot deliver approval
  prompts, so prompts addressed to the task actor wait for a later engine protocol. An okay result
  leaves the task verifying, a completed cancel leaves it canceled, and other results or protocol,
  storage and artifact failures fail it. On startup the coordinator advances its generation and
  marks abandoned pending, launching and running attempts and tasks for recovery, including a
  cancellation whose process exit was not observed, with the old generation in an event; it never
  relaunches them. These choices make launch intent durable,
  distinguish an engine exit from accepted work, and prevent a stale coordinator from writing.
  CancelRequested may move to Interrupted: an unobserved cancel cannot be reported canceled because
  the plan requires the process tree to be confirmed stopped.

- **2026-10-14, review, acceptance and revoked membership (#3).** What M2.1, M2.2b and M2.3b
  settled stands: actors from the authenticated context, membership for listing
  and direct access, per-user idempotency keys and continuations, cancel by any member. Three
  roles are kept apart: the task's creator (`tasks.created_by`), an attempt's actor
  (`attempts.actor`, the user who started it and under whose provider sign-in it runs, recorded at
  start by M2.3b), and a reviewer (any current member who records findings); the project role
  owner is a membership role, not one of these. Any current member of the task's project may open
  its review package and record findings. Accepting a reviewed result, requesting a refinement and
  exporting the branch are allowed to the actor of the attempt that produced the result and to
  the project's members with role owner, to no one else: not the creator as such, not a reviewer
  as such. An acceptance binds the content hash and ownership generation (`docs/plan.md` section
  2), so one recorded against an older hash is refused. A permission prompt (#7's pre-launch
  decision now, an engine prompt from the first protocol with approvals) is answered by the
  attempt's actor alone; a project owner may deny it but never approve it on the actor's behalf.
  Artifacts read directly (by id, outside a review package) need the reader's current membership
  of the project of the attempt or task that references the artifact, checked on every read,
  so a revoked member's next read is refused; an artifact no record references is readable by no
  one through `/v1`. Removing a member, in one transaction, denies that user's pending prompts in
  the project with the reason "membership revoked", requests cancellation of every attempt in the
  project whose actor is that user with the reason "actor removed from the project", and leaves
  their later responses refused like any non-member's request. Removing a project's last owner is
  refused. Carried by M7 (review and acceptance), M4b (membership removal through the owner's
  account administration, pre-launch decisions) and the slice that pins an engine with approvals
  (engine prompts); the artifact read check lands with the first `/v1` artifact route. Tests:
  findings from a non-member refused; an acceptance, refinement or export by the creator or a
  reviewer who is neither the attempt's actor nor an owner refused; an acceptance against a stale
  content hash refused; a member removed while their attempt runs, which ends canceled with the
  reason; a member removed while a prompt is pending, the prompt denied and the late response
  refused; an owner's denial of another user's prompt accepted and an owner's approval refused;
  an artifact read by a member accepted, by a non-member refused, and by the same user after
  their removal refused; the last owner's removal refused. Reason: acceptance leads to
  integration, which is the starter's or the project owner's call, and an attempt runs on its
  actor's provider plan, so it must not outlive that actor's membership.

- **2026-10-14, SSE limits, cursors and the desktop client (#4).** M2.2b settled per-attempt
  sequences, `Last-Event-ID` over `?after=`, `id:` lines and a replay-to-live handoff with no gap
  or duplicate. The remainder: bytes are bounded as well as counts. Every byte limit below counts
  encoded bytes on the wire: an event as written, with its `id:`, `event:` and `data:` framing in
  the stream form and its JSON encoding in the JSON form, not the stored payload, which escaping
  and framing make larger. An encoded event is at most 256 KiB, matching the engine's bound on
  event text; the executor stores a payload whose encoded event would exceed it as an artifact
  at ingestion and the event carries its reference and size instead, so no stored event exceeds
  the cap. The stream reads the store in pages that end at 256 events or 1 MiB encoded,
  whichever comes first (one event always fits, being under the cap), and writes each
  page under a 10-second write deadline (`http.ResponseController`); a subscriber that misses it
  is disconnected and resumes from its cursor, so the server holds at most one page per
  subscriber and the store is the buffer. Across all streams and JSON reads the pages held at
  once share a 32 MiB budget (a byte-weighted semaphore). Capacity is reserved before a page is
  read from the store: a reader acquires the page maximum, 1 MiB, then reads and encodes, and
  releases what the encoded page did not use; it holds the rest until the page is written or the
  stream ends. A reader that cannot get its reservation waits without touching the store, and a
  stream that cannot get it within its write deadline is disconnected like a slow one. At most
  16 open streams per user and 128 in all; one more is answered 429 `too_many_streams` before any
  event. Every stream starts with `retry: 2000`. The JSON form returns one page by the same rule
  and the client asks again from the last sequence. A cursor that is not a non-negative integer
  stays 400 `bad_request`. A cursor above the attempt's last recorded sequence (a restored
  database, a confused client) is answered by form: the JSON form returns 409 `cursor_ahead`; the
  stream form, since `EventSource` cannot read a refused response's body and would retry the
  stale `Last-Event-ID` for ever, answers 200 with one message, `event: cursor_ahead` with a JSON
  body naming the last recorded sequence and no `id:` line, and then closes. A client receiving
  it closes its connection before any retry, discards its cursor and view, and opens a fresh
  stream from 0 without `Last-Event-ID` (the browser adapter a new `EventSource`, which sends none;
  the Node client a new request without the header). Events are
  not pruned in v1, so a gap in retained history cannot occur; when a retention policy arrives
  (M8), a cursor below the first retained sequence gets a `gap` event naming that sequence and
  then the events from there, never a silent jump. On shutdown `adad` ends every stream after the
  last complete event. Electron's main process reads SSE over the pipe or socket with its own
  parser (Node's `http` with `socketPath`; it has no `EventSource`), keeps the last `id:`, and
  reconnects with `Last-Event-ID` after 0.5 s doubling to 10 s, or after the server's `retry:`;
  when the pipe or socket is gone it follows M2.4's reconnect-or-start path. The renderer gets
  events through the bridge and keeps no cursor; the browser (M4b) uses `EventSource`, which sends
  `Last-Event-ID` itself, and handles `cursor_ahead` as above. Carried by M2.4, with the
  server-side limits in #30. Tests: a real Node client to Go over a Windows named pipe (the
  `windows-latest` leg) and a Linux socket; a subscriber that stops reading is disconnected within
  the deadline with the hub's subscriber count back to zero; the 17th stream of one user refused;
  a payload whose encoded event exceeds 256 KiB stored as an artifact with the event holding its
  reference, including one under 256 KiB raw that escaping takes over; a page cut at 1 MiB of
  encoded bytes before 256 events, including one whose payloads alone fit but whose framing does
  not; many slow streams together never holding more than the 32 MiB budget, the excess
  disconnected; with the budget exhausted, no store read started until a reservation is granted;
  a malformed cursor; `cursor_ahead` as 409 in the JSON form and
  as the closing event in the stream form, after which the Node client and, in a browser test, the
  `EventSource` adapter reopen from 0 without `Last-Event-ID` and do not retry the stale cursor;
  events appended while replay pages are read; `adad` killed and restarted during a replay longer
  than one page, after which the client holds every sequence exactly once. Expired cursors are
  tested when retention exists, since nothing expires before. Reason: a deadline and paged reads
  bound memory without dropping events, and the browser's reconnection works only through `id:`
  lines, which the desktop client then imitates.

- **2026-10-14, the shared UI's browser boundary (#5).** The React app in `ui/` talks only to an
  `AdaClient` interface that mirrors `/v1`, with two adapters: the preload bridge (desktop: the
  renderer calls typed methods, main forwards them to `adad` over the pipe or socket after
  validating the sender frame) and an HTTPS adapter (browser: same-origin `fetch` to `/v1` with the
  session cookie, `EventSource` for events). Authorization is always `adad`'s: the bridge forwards
  and refuses only malformed calls, and decides no permission. Import on the desktop keeps the
  native dialog in main, whose path goes to `adad` without passing through the renderer. Import in
  the browser is a server-side chooser and nothing else: `adad` lists the import roots the owner
  allowed on the coordinator host (a setting edited only from the desktop, each root granted to
  named users; none by default but the owner), browses the directories below a root, and imports
  a root id plus a relative path; `..`, absolute paths, and links or junctions leaving the root
  are refused with M3's import blockers. Importing from a git URL is not offered in M4b, in the
  browser or through `/v1`. A clone is a request the coordinator makes on a user's behalf, and
  git can connect where an address pin does not reach: another port in the URL, URL rewriting
  (`url.<base>.insteadOf`) and proxies from git configuration or the environment, submodules,
  Git LFS and filters fetching from other hosts, and IPv6 or other address forms a host check
  can miss. Git-URL import waits for a slice that controls every outbound fetch, and that slice
  must meet at least: HTTPS on port 443 only; git run with an isolated configuration (no system,
  global or repository config read, `GIT_CONFIG_NOSYSTEM`, an empty `HOME` and
  `GIT_CONFIG_GLOBAL`) and a cleared proxy environment; no submodules and no LFS or filter
  downloads (`protocol.allow=never` with `https` alone allowed, LFS and filters disabled);
  the host resolved by `adad` and the connection pinned to the checked address
  (`http.curloptResolve`), the check covering IPv4 and IPv6 and refusing loopback, link-local,
  unspecified, private (RFC 1918), unique-local (`fc00::/7`), CGNAT (`100.64.0.0/10`),
  IPv4-mapped and other embedded forms of these, and the coordinator's own addresses;
  redirects refused (`http.followRedirects=false`); no credentials in the URL; and a time and
  disk limit after which the process tree ends and the partial clone is removed. Each condition
  comes with its test then; `ssh`, `file` and private repositories wait further for per-user git
  credentials in the credential profile. Previews are served by
  `adad`'s preview listener, a reverse proxy to the managed preview process, on its own host name
  and port (`preview.home.adanext.si`, the listener's port plus one by default, a DNS-only record
  on the same private address, the certificate naming both hosts). The UI embeds a preview in an
  iframe with `sandbox="allow-scripts allow-same-origin allow-forms"` (its own origin, never Ada's;
  no top navigation, no popups escaping the sandbox) and a URL holding a ticket from `/v1`: random,
  single use, 60 seconds, bound to the user and the preview, issued to project members only and
  exchanged for a host-only `__Host-ada-preview` cookie that `/v1` never accepts. On every
  request through the preview listener, page loads, assets and the upgrade of a WebSocket alike,
  the proxy checks the cookie's user, that the cookie names this preview, and that user's
  current membership of the preview's project, read from the store at that request, not cached
  from the ticket; an open WebSocket is closed when its user's membership or session ends (the
  same hook that ends event streams, #6). Revoking membership therefore ends preview access at
  once. The two hosts
  are the same site, so `SameSite` does not separate them: the barrier is `/v1` sending no CORS
  headers, refusing any mutating request whose `Origin` is not Ada's (#6), and Ada's pages sending
  `frame-ancestors 'none'`. The desktop loads the same preview listener on loopback in a view
  with its own session partition and no preload. Carried by M2.4 (`AdaClient` and the desktop
  adapter), M4 (desktop previews) and M4b (the HTTPS adapter, the chooser, the preview host).
  Tests: no module outside the adapters touches `window.ada` or `fetch`; the component tests run
  against both adapters with one fake `adad`; an action `adad` refuses comes back as `adad`'s
  refusal through the bridge; browsing outside a granted root, through `..`, an absolute path or
  an escaping link or junction, and a root granted to another user, refused; the browser offering
  no git-URL import and `/v1` refusing an import by URL; from a preview page, reading `/v1`
  fails, a mutating request with the user's cookies is refused, framing an Ada page fails; a
  preview without a ticket, with a reused or
  expired one, or for a non-member refused; the preview cookie refused by `/v1`; a member
  removed while viewing a preview, after which their next page, asset
  and WebSocket upgrade are refused and an open WebSocket is closed; a cookie for one preview
  refused on another. Reason: a typed bridge is an interface, not authorization, so the one place
  that authorizes must be `adad` for both shells, and a browser can neither open a host dialog nor
  be trusted with a typed host path; a git clone is held to the checked address only once every
  fetch git can make is controlled, so until then the smaller safe option is no clone at all.

- **2026-10-14, LAN session security (#6).** The desktop form's implicit local user is the owner
  account: when web access is enabled the owner adds a credential to that same `users` row, so
  their projects and tasks carry over, and the desktop keeps reaching `adad` as that user through
  the pipe or socket, whose OS ownership is its authentication. As the 2026-10-13 entry says,
  accounts are created by the owner and never from the network: creating an account and issuing
  an enrollment code are desktop-channel operations only, and a network session, the owner's
  included, is refused both. Creation yields a one-time enrollment code (single use, 24 hours)
  that the new user enters on their own computer to register a credential. Without a session
  the TLS listener answers only the login, enrollment-with-code and static asset routes: being
  on the network grants nothing. Credentials: passkeys (WebAuthn, relying party
  `home.adanext.si`, user verification required, discoverable credentials) through a pinned
  WebAuthn library chosen by the
  dependency rule; a password is a fallback the owner enables per account, at least 12
  characters, hashed with Argon2id (`golang.org/x/crypto/argon2`, 64 MiB, 3 passes, 1 lane, a
  16-byte salt; at most two hashes computed at once; rehashed at login when the parameters rise),
  compared in constant time. Sessions: 32 random bytes in a `__Host-ada-session` cookie (Secure,
  HttpOnly, SameSite=Strict, Path=/), stored only as a SHA-256 hash in a `sessions` table added by
  a forward migration, with user, credential, creation, last use and address; idle expiry 8
  hours, absolute 7 days; login creates a new session with a new token, and a token presented
  from before login is never promoted. Revocation is deleting the row: the user's own sessions
  from their account page, anyone's by the owner. A role change (in any project), a credential
  added, removed or changed, and an account disabled or removed each delete, in the same
  transaction, every session of that account, not only the one in use, and close that account's
  open event streams and preview WebSockets; the user signs in again. An open event stream also
  rechecks its session at each page and heartbeat and ends when it is gone. CSRF: every mutating
  request must carry an `Origin` equal to the configured Ada origin (missing, `null` and foreign
  origins refused) and a JSON content type; GET never mutates; the `Host` header must be the
  configured name, which also refuses DNS rebinding.
  Throttling: per account, after 5 failures a delay that doubles from 1 second to 15 minutes, with
  no permanent lockout; per address, 20 failures in 10 minutes block that address for 15 minutes;
  counters in memory, bounded to 10,000 addresses. Recovery: from the desktop only (the local
  channel), the owner enrolls a new credential for themselves or any account and clears its
  throttling; there is no email or network recovery. Carried by M4b. Tests: the issue's four (two
  separate computers in the M4b evidence; session isolation and revocation, including an open
  stream; cross-origin mutations rejected, with missing, `null`, foreign and preview origins; a
  permission response from the wrong user refused, through #7's pre-launch decisions); and
  rotation (a pre-login token refused after login); a role change, a credential change and an
  account disabled each ending all of that account's sessions on two browsers at once, with
  their open streams closed; idle and absolute
  expiry and both throttles on a fake clock, a wrong `Host` refused, enrollment codes single use
  and expiring, account creation and code issuance refused to every network session including a
  freshly authenticated owner's, the stored hash and token
  never equal to their inputs. Reason: passkeys leave nothing reusable to steal on the LAN, the
  password fallback covers devices without one, and server-side sessions make revocation
  immediate; the owner's desktop is the root of trust because it is the one channel the network
  cannot reach.

- **2026-10-14, provider sign-in relay and approvals (#7).** The relay runs a provider CLI's own
  sign-in only where it needs no callback to the coordinator's loopback: a device code, or an
  authorization code the user copies from the provider's page into Ada, which passes it to the
  CLI's input. `adad` starts the CLI's sign-in command (the executable the launcher would give
  the engine) with the user's credential-profile environment, built explicitly as the 2026-10-13
  profile entry says, and parses its output with a per-CLI, per-version extractor. Only
  allow-listed fields reach the browser: the verification URL (checked against the provider's
  known sign-in hosts), the user code (checked against the provider's code format) and the
  expiry; the CLI's raw output never does. These go only to the session that started the job. A
  sign-in job is bound to that user and session: no other session can see, answer or cancel it;
  one job per user and provider at a time; 15 minutes, then the process tree ends. Success is the
  CLI's own status command reporting the profile signed in. The raw output is held in memory for
  the extractor and dropped with the job; it is not persisted, and neither it nor the extracted
  fields go to logs, events, artifacts or the store (an extractor failure is logged as the
  provider, the CLI version and "unrecognised sign-in output", nothing more). A provider whose
  CLI offers only a loopback callback is not relayed; its sign-in is done in the desktop shell
  on the coordinator host until Harness supports a relay. Which CLIs qualify
  is checked live per CLI version in the M4b slice and recorded with the date. Approvals: the
  pinned protocol advertises `approvals: false` and rejects mode `ask`, so Ada never describes
  any of its decisions as intercepting a CLI's tool approvals and offers `ask` only when the
  engine's handshake advertises approvals, which needs a Harness protocol version and a new
  engine pin, each its own decision. Until then the permission prompts of the M4b evidence are
  Ada's own decisions before launch (`PermissionDecision`, `docs/plan.md` section 2), held by a
  scheduler gate. The scheduler, #9's queue, is the only launch path in M4b: it replaces the
  direct launch after a start, so every attempt, needing a decision or not, launches through the
  same claim. The claim stays M2.3b's atomic claim-once update of `pending` to `launching`,
  extended with the decision check in the same transaction: an attempt moves from `pending` to
  `launching` only if no decision is required for it (its scope, judged at the claim against the
  project's current default, does not exceed it), or a matching, unexpired allow decision exists.
  A decision binds the task revision, the source snapshot's content identity, the exact
  permitted scope (backend, mode, network access and workspace), the permission policy's version
  and the coordinator's ownership generation (`docs/plan.md` section 2); it matches only while
  all five still equal the attempt's, and any change to a binding (a task edited, a new
  snapshot, another scope, a changed policy) requires a fresh decision, never a reuse of the old
  one. A restart never revives a decision or its attempt: recovery (M2.3b) moves an abandoned
  pending attempt to `recovery_required`, and work continues only as a new attempt of the new
  generation with a new decision. A decision expires 24 hours after it is made. A forward
  migration in the M4b slice adds
  `permission_decisions`: the attempt, the five bindings, the decision (allow or deny), the
  deciding user, the time, the expiry and the request's idempotency key. A start whose scope
  exceeds the project's default (a write mode or network access) records the attempt `pending` as
  M2.3b does and marks it as needing a decision; in the queue such an attempt keeps its place but
  is passed over until decided, so it never holds up other users' work, and no process starts
  before approval. `POST /v1/attempts/{id}/decision` takes the decision and an
  `Idempotency-Key` (the
  same key and body return the first result, a different body under it is refused); allow and
  deny are accepted from the attempt's actor alone, and a project owner may deny but not allow,
  as #3 says; anyone else, and any decision on an attempt no longer pending, is refused. A deny
  closes the attempt canceled with the reason "permission denied". Carried by M4b. Tests:
  a provider sign-in completed from the remote computer for each relayed provider (M4b
  evidence); a missing or invalid selected sign-in refuses execution with no fallback; a fake CLI
  whose output carries a secret marker beside the URL and code, after which the marker appears
  in nothing the browser receives and in no log, event, artifact or database row, while the
  allow-listed fields arrive; output the extractor does not recognise refused with no output
  forwarded; a sign-in job viewed, answered or cancelled from another session refused; a
  loopback-only provider not offered in the browser; mode `ask` refused while the handshake says
  `approvals: false`; an attempt needing a decision never launched (the fake engine never
  started) until its actor allows it, then launched once; after an allow, a changed task
  revision, a new source snapshot, a changed backend, mode, network access or workspace, and a
  changed policy version each leaving the attempt unlaunched until a fresh decision; a restart
  with an allowed but unlaunched attempt sending it to `recovery_required`, never launched, and a
  new attempt of the new generation needing its own decision; an expired
  allow not launching on a fake clock; an attempt needing no decision launched through the same
  claim, with no other code path moving an attempt to `launching`; two claims racing on one
  attempt, one wins; a decision from another member refused, an owner's allow
  refused and an owner's deny accepted; a repeated request with the same key returning the first
  result; a decision on a launched or closed attempt refused.
  Reason: a callback to `localhost` lands on the wrong computer, and binding the job to its
  session is what keeps one user's sign-in out of another's profile.

- **2026-10-14, Cloudflare token and certificate renewal (#8).** The token: `Zone > DNS > Edit` on
  `adanext.si` alone, the narrowest scope Cloudflare offers (it has no per-record tokens, and a
  separate zone for the challenge would cost a second domain). It lives in a file in `adad`'s
  configuration directory, owner-only (0600 on Unix, a DACL with the user's SID alone on Windows,
  as the store's files), which `adad` refuses to use when anyone else can read it; never in a
  repository, the store, the engine's environment, logs or error text (the client strips the
  `Authorization` header from every error). `adad`'s DNS client only lists, creates and deletes TXT
  records named `_acme-challenge.<configured host>`, checked before every call; the residual risk,
  that a stolen file could change the zone, is accepted and bounded by the owner setting an expiry
  on the token. Rotation is the owner's: a new token replaces the file from the desktop settings,
  `adad` reads it at the next issuance, and the owner revokes the old one at Cloudflare. Renewal
  starts 30 days before expiry, or at half the lifetime for certificates shorter than 60 days,
  whichever comes first; a failure retries with backoff from 1 hour doubling to 24 hours, within
  Let's Encrypt's rate limits. Each failed attempt is shown to the owner when it happens (a
  notice on the desktop and on the owner's web session, with the time, the step that failed and
  the next retry), and from 14 days before expiry the warning stays on screen until a renewal
  succeeds. The
  old certificate is served until it expires; after that the TLS listener refuses handshakes
  rather than serve an expired certificate, and the desktop channel, unaffected, is where the
  owner repairs it. Missing or mismatched files (the first slice, from files) leave the TLS
  listener off with the reason shown on the desktop; never plain HTTP, never a self-signed
  fallback. Replacement: each issued pair is written, synced, into its own directory, checked
  (key matches, chain parses, not expired), and then a pointer file is replaced by rename; in
  memory, `GetCertificate` reads an atomic pointer swapped after the same checks, so new
  handshakes get the new pair and open connections keep theirs. At start `adad` loads the pair the
  pointer names, or the previous pair while it is still valid. Challenge cleanup survives a
  crash: before calling Cloudflare, `adad` records the challenge intent (the record name, the
  exact TXT value and the ACME order) durably in its store; after the create call it adds the
  record id. The record is deleted after each validation, successful or not, and the intent is
  closed only once the deletion is confirmed: the delete call succeeds or answers not found, or a
  listing shows no record with the intent's value. While listing or deleting fails, the intent
  stays open and cleanup retries on its own with backoff from 1 minute doubling to 1 hour until
  the deletion is confirmed, independent of the next issuance, which neither waits for it nor
  stands in for it. At start, and before each new challenge, `adad` also reconciles: for every
  open intent it
  lists the TXT records at `_acme-challenge.<configured host>` and deletes those whose value
  equals a recorded intent's value, whether or not their id was recorded, and no others; a record
  with a value no intent holds is left alone and reported to the owner. Some home resolvers and
  routers drop public answers holding private addresses (DNS rebinding protection); the setup
  page then
  directs the owner to a local entry for `home.adanext.si` and `preview.home.adanext.si` in the
  router's local DNS or each computer's hosts file, which keeps the certificate valid. Carried by
  M4b's DNS-01 slice (files first, as decided on 2026-10-13). Tests: missing, mismatched and
  expired certificate files; renewal failure and its retries on a fake clock, each failure
  notified to the owner when it happens and not only within 14 days of expiry; a restart at each
  step of the replacement, ending on a complete pair; a connection open across a renewal keeping
  its certificate while a new one gets the new pair; the challenge record deleted after success,
  after failure and at restart; a crash after the create call and before the record id is
  recorded, after which the restart deletes that record by its value; a fake Cloudflare whose
  listing and then deletion fail, after which the intent stays open and cleanup retries on a
  fake clock with no issuance due, closing the intent only after a confirmed deletion; a TXT
  record at the
  challenge name with a value no intent holds left in place; a fake Cloudflare API that fails the
  test on any call other than the challenge TXT record; the token absent from logs and errors; a
  token file readable by
  others refused. The ACME side runs against a fake ACME server, never Let's Encrypt in tests.
  Reason: zone scope is the narrowest the provider allows, so the client's own limits and the
  token's expiry carry the rest; refusing TLS after expiry is visible and safe, an expired
  certificate teaches users to click through warnings.

- **2026-10-14, stopping a shared coordinator and queueing (#9).** Only the owner account may
  stop the coordinator: the coordinator's owner, the desktop's implicit local user (#6), which is
  distinct from the owner role a member holds in a project. "Quit Ada and stop work" is offered in
  the desktop shell, which only the owner account runs; in the browser, other users get "Leave",
  which signs out of the page and stops nothing.
  When anyone else's work is affected, the owner first sees how many running and queued tasks
  of other users will be cancelled. Stopping cancels every running attempt through M2.3b's
  cancel path and every queued one, each with an event and a recorded reason "coordinator stopped
  by the owner", which affected users see on the task when they next load it or as the last event
  of an open stream; an attempt whose exit is not confirmed within the stop budget goes to
  recovery as M2.3b decided. Host logout and reboot keep recovery semantics (`docs/plan.md`
  section 2). Queueing keeps the recorded limits: the concurrency cap stays one and one writer per
  workspace. Starting a task while another attempt is launching or running, from any user, moves
  it to `preparing` with a pending attempt, as M2.3b already records; pending attempts launch one
  at a time in order of creation across all users, and the snapshot is revalidated at launch,
  not at queueing. A user may have at most 3 queued attempts; a fourth start is refused with
  `queue_full`. The UI shows a queued task's position. A queued task is withdrawn by cancelling it:
  the store gains a `preparing → canceled` edge, allowed only in the transaction that closes the
  task's attempt as canceled while it is still `pending` (a forward migration if the attempt's
  states need it); an attempt already launching follows the running cancel path once the engine
  accepts it, or ends failed or interrupted as M2.3b decided. Any member may cancel a queued task,
  as for a running one. A queued attempt left by a crash is marked for recovery, never relaunched,
  as M2.3b decided. Carried by M2.4 (the queue, the new edge, "Quit" with its reason) and M4b
  (the owner-only stop, "Leave", the notice to other users). Tests: two tasks started at once, the
  second queued and launched only after the first ends; FIFO order across two users; the fourth
  queued start refused; a queued task cancelled with no process started; a cancel racing the
  launch ends either canceled before launch or through the running path, never both; `preparing →
  canceled` refused once the attempt left `pending`; "Quit" cancels running and queued tasks with
  the reason visible to their actors; a stop request from any other account refused, a project
  owner's included; "Leave"
  leaves running work untouched. Reason: one user's quit ending another's work must be the owner's
  explicit act, and a FIFO queue in `preparing` reuses the pending attempt M2.3b already records
  instead of adding a state.

- **2026-10-14, adanext-images compatibility manifest (#10).** The manifest lives in
  `adanext-images` as one JSON file per image revision, `manifests/<image-version>.json`,
  immutable once added. It holds `manifest_version` (1), the image name and version; the source
  commit, the `adanext-images` commit the image was built from, which comes before the commit
  that adds the manifest (a commit cannot name itself); the exact built artifact Ada is given,
  named by its file (the exported VM disk, `<image>-<version>.vmdk`, or an OVA,
  `<image>-<version>.ova`, whichever M5's slice exports) with that file's SHA-256; the
  `adanext-os` commit of the
  `ada-guest` it contains with that binary's SHA-256, the guest protocol versions `ada-guest`
  speaks, the Harness revision and binary SHA-256 inside the guest, the minimum `adad` version, the
  base system (Ubuntu 24.04 and its build date), the Packer and plugin versions, and the
  toolchains with exact versions (Git, Go, Node). The image takes `hrn` built by
  `scripts/build-engine.sh` at the pinned revision and `ada-guest` built from the named
  `adanext-os` commit, both built outside the image and copied in, so no toolchain inside the
  guest produces a binary Ada depends on. The toolchain versions are therefore inventory:
  recorded in the manifest from what the build finds in the artifact, shown in `adad`'s
  diagnostics, and never
  a reason to refuse an image in M5. If a later image builds `hrn` or `ada-guest` inside the
  guest, the Go toolchain becomes a gate, decided then. `adad` computes the SHA-256 of the
  artifact file it is given and refuses an image without a manifest, with an unknown
  `manifest_version`, whose artifact hash differs from the manifest's, whose guest protocol it
  does not speak,
  whose Harness revision differs from its own `engine/engine.json`, or whose minimum version it
  does not meet, each with its own reason, before any VM is created from it. In `adanext-images`,
  a pull request that changes an image changes its source; once merged, CI builds the artifact
  from that source commit, hashes those bytes, and inspects the same bytes (the exported disk
  opened read-only, never a rebuild or the build VM): it reads `ada-guest` and `hrn` from the
  artifact's filesystem and fails unless their SHA-256 equal those of the binaries built outside
  the image from the named `adanext-os` commit and the pinned Harness revision, asks `ada-guest`
  and `hrn` for their protocol versions and revision and fails on a mismatch, and records the
  toolchain versions it finds, so a manifest never claims a binary its artifact does not contain.
  It then writes the manifest with the source commit, the artifact's name and hash and what it
  inspected, adds it in a pull request of its own, and publishes exactly the bytes it hashed and
  inspected, checking their hash again at upload. CI on the manifest's pull request validates
  the schema, that the source commit is an ancestor of the manifest's commit, that the
  `adanext-os` commit exists, and that its `engine/engine.json` names the manifest's Harness
  revision. A pull request in
  `adanext-os` that changes the guest protocol, the engine pin or the minimum image it accepts
  links the `adanext-images` pull request
  that publishes a matching manifest; `adad`'s refusal of non-matching images is what keeps the
  pair honest if one lands first. Carried by M5. Tests: an image without a manifest, with an
  unknown manifest version, an artifact changed by one byte after the manifest was written, an
  unsupported protocol, another Harness revision and a minimum above `adad`'s version, each
  refused with its reason; a matching image accepted; the `adanext-images` CI check failing a
  manifest whose Harness revision differs from the named `adanext-os` commit's pin, and one whose
  source commit is not an ancestor of the manifest's commit; against a built artifact, the
  build check failing when `ada-guest` or `hrn` inside it is replaced by another binary, when a
  guest protocol or revision differs, and passing on the unchanged artifact; the publish step
  refusing a file whose hash differs from the inspected one; an image accepted by `adad` with
  the manifest's toolchain versions shown in its diagnostics. Reason: the guest and the
  coordinator must run the
  same engine protocol, and a manifest checked on both sides turns a silent mismatch into a
  refusal before launch.

- **2026-10-14, successful result with slow engine exit: a proposal awaiting the owner.** Issue
  #39 item 3 leaves this to the owner, and no confirmation exists yet. What the branch does until
  then: an `ok` result followed by a forced termination (a kill for a slow exit, or a cancel or
  a Stop the engine does not answer by exiting within the grace period) moves the task
  `running` → `interrupted` → `recovery_required`, with the stored result kept and a `recovery`
  event saying the engine had to be killed after its result. A cancel or a Stop after the result
  that the engine answers by exiting naturally before the kill goes through `finish`, which ends
  the task `canceled` (the cancel request was recorded) with the result kept. Reason: an
  unobserved or forced end must be reconciled before the stored result is trusted. Cost: a
  `recovery_required` task has no way out until a reconcile flow exists, so the stored result
  cannot be accepted yet. Owner action 12 below.

- **2026-10-15, M2.4a queue.** The Go half of M2.4 (#64) lands what the #9 decision carried, and
  settles what it left open. No migration: the attempt states have no CHECK constraint and
  `canceled` is already a task state, so `preparing → canceled` is an edge in the store's table,
  which `transition` admits only when the task's newest attempt is already closed as `canceled`;
  `Tx.CancelQueued` does both in one transaction while the attempt is still `pending` and
  returns `ErrAttemptLaunched` otherwise, writing nothing. The launch claim (`pending` →
  `launching` under the ownership generation) and that withdrawal are two updates of the same
  attempt row in SQLite's single writer, which is what serialises a cancel against a launch.
  The queue is the set of `pending` attempts in row-id order, which is creation order: attempts
  are inserted under SQLite's single writer and never deleted, and the `started` text cannot be
  sorted (RFC3339Nano drops trailing zeros, so `...00.12Z` sorts after `...00.123Z`); the
  executor launches from it (`NextPendingAttempt`) one at a time, skipping a project with an
  attempt active (one writer per workspace) once the cap is above one. The cap is
  `executor.Config.Concurrency`, set by `adad -concurrency` (default 1, the plan's section 8
  default); it is a flag, not a stored setting, until settings exist. The per-user limit of 3
  counts the user's `pending` attempts, so one about to launch counts until the claim; a
  withdrawn attempt frees its place. The position `GET /v1/tasks/{id}` reports as
  `queue_position` is 1-based across all users, present only while the task is `preparing` with
  a `pending` attempt, and read after the task so a launched task never shows one. A withdrawal
  and the owner's stop record their reason as a `cancel` event on the attempt (`{"reason":
  "withdrawn by <user>"}` or `"coordinator stopped by the owner"`), which is what the actor sees
  on the attempt's stream or the task's events; the stop writes it before the executor acts, so
  for a running attempt the engine's own `result` event follows it. The stop endpoint is
  `POST /v1/coordinator/stop`, an empty body with an `Idempotency-Key` and no `If-Match` (nothing
  has a revision), answered `202` with `{"running", "queued", "reason"}` counting what it ended;
  any account but the coordinator's owner (`local`, the implicit desktop user, as `api.
  Execution.Owner`) gets `403 forbidden`, a project owner included. In one idempotent
  transaction `Tx.StopWork` withdraws every queued attempt, puts the reason on every launching
  or running one and moves each running task to `cancel_requested` (a cancel its actor already
  requested stays as it is), so a result the engine returns before the executor acts cannot end
  the task ok; a launching attempt's task is still `preparing`, and the executor's accept, which
  moves it to `running`, moves it on to `cancel_requested` in the same transaction when a cancel
  is on record. Start admission is held closed from before the stop's transaction until the
  executor has been told (a lock and a stopping flag in the API handler, never held with the
  executor's own lock), so no start commits in that window and every later one is refused
  `503 shutting_down`; a replayed stop answers from the idempotency cache and tells the
  coordinator nothing, so after a restart the old key cannot stop the new coordinator. Then
  `adad` stops launching, cancels the running attempts through M2.3b's cancel path, drains the
  hub (the stop wakes the affected attempts' streams, and every open stream sends what is
  committed and ends, within the write deadline, before the HTTP shutdown closes the hub) so an
  open stream receives the committed reason before it closes (a running attempt may still append
  its result after it), and ends its serve loop as a signal would,
  so an attempt that does not exit within the stop budget reaches recovery as before. The
  signal path is unchanged: no reason is recorded, and a queued attempt it leaves is marked for
  recovery at the next start, which now records `attempt_state` in its `recovery` event. Not in
  this slice: the stop's preview of other users' affected work and the notice they see (M4b),
  and the UI (M2.4b). Reason: the pending attempt M2.3b already records is the queue, so no
  state, column or migration was needed, and the one row the launch and the cancel both update
  is the simplest serialisation that holds across coordinators.

- **2026-10-16, M3 in four slices, and reading a repository without running it.** M3 is
  `docs/backlog.md`'s M3.1 safe git and inspection, M3.2 import and snapshots, M3.3 managed clone
  and task workspaces, and M3.4 drift and launch revalidation, taken one at a time in that order,
  since each touches the schema or the API. No git process ever uses the original's git
  directory as its repository. Its config can name commands (clean and smudge filters, fsmonitor,
  textconv, an ssh command), and `git status` alone can run clean filters and fsmonitor, so
  "inspect without executing repository code" cannot hold if git reads that config. Inspection
  instead reads the layout and the index in Go and builds a mirror git directory under Ada's
  state: a config Ada writes, copies of HEAD, the refs, `shallow` and the index, and the
  original's objects reached read-only through `objects/info/alternates`. Git runs with that
  mirror as its git directory and the original as its work tree, with system and global config
  off, hooks pointed at an empty directory and submodules ignored, so filter and diff drivers
  named in `.gitattributes` are undefined, nothing runs, and no child git starts inside a
  submodule. The mirror's config is not empty: an allow-list of data-only keys (`core.filemode`,
  `core.autocrlf`, `core.ignorecase` and the like) is copied from the repository, global and
  system config, with the excludes and attributes files, because without them status and diffs
  are wrong on Windows (every executable a mode change, every CRLF file a whole rewrite). The
  Ada-owned clone is made from the mirror, never from the original
  (`git clone --bare --no-local --no-hardlinks --template=`, with `GIT_NO_LAZY_FETCH=1` and
  `transfer.bundleURI=false`, the file protocol allowed for that call alone), and later imports
  rebuild the mirror and fetch from it. Upload-pack therefore reads only Ada's config, and
  pack-objects copies the objects into the clone's own pack, so the clone borrows no objects and
  keeps none of the original's settings. Git older than 2.45.1 is refused at start-up, so
  `GIT_NO_LAZY_FETCH` and the fixes for CVE-2024-32004 and CVE-2024-32465 are always present.
  Amended in M3.1: the floor is the first release of each maintained line carrying the May 2024
  clone fixes (2.39.4, 2.40.2, 2.41.1, 2.42.2, 2.43.4, 2.44.1, 2.45.1). It accepts upstream
  maintenance releases only: a distribution backport that reports an older version number
  (Ubuntu 24.04's patched 2.43.0) is refused. `core.longpaths` joined the mirror's data-only keys
  for Windows paths over 260 characters.
  Two amendments to `docs/plan.md` section 3, Import: v1 blocks every submodule (the plan said
  "unresolved submodules") and every Git LFS path (the plan said "missing LFS objects") until a
  later milestone supports them (not yet in the backlog), because supporting them means running
  or reimplementing their transports; and sparse checkout is blocked in every form (the plan said
  "unsupported sparse-checkout cases"). Reason: a blocker the user can see is better than an
  import that quietly runs a transport or reads a partial work tree. M3.1 also blocks what a
  snapshot cannot represent: `core.bare` or `core.worktree` set, a partial clone, and a merge,
  rebase, cherry-pick, revert or bisect in progress or unmerged entries, and assume-unchanged
  entries (git skips those files, so their edits would not show). Of section 3's
  "existing Ada tasks and verifiable Harness history", M3.2's import lists the existing Ada tasks
  for the source; Harness history is deferred to M6.

- **2026-10-18, editor integration: VS Code first, JetBrains later.** The owner asked for Ada
  inside an editor and chose VS Code first, then JetBrains. It needs no new design: `adad` is
  already a local service with a versioned API on a per-user endpoint, and the #5 decision put
  the UI behind `AdaClient`, so a VS Code extension is one more client and its webview a third
  adapter. The extension's Node side holds the token, as Electron's main process does, and the
  webview gets typed calls only. Ada still never touches the user's checkout: the editor shows
  and reviews Ada's results, and the user brings a branch in. The desktop app stays, since it
  runs Ada with no editor open and owns "Quit Ada and stop work"; the extension starts `adad`
  if needed but never stops it, and closing VS Code leaves it running. It lands as M4c, after M4,
  because before M4 there is no real workflow, diff or review to show; the branch hand-off joins
  M7. JetBrains (Kotlin, its own pipe or socket client and handshake) is a later slice.

- **2026-10-19, M3.3 managed clone and task workspaces.** Points the backlog entry left open.
  A task binds to a snapshot when it is created (`POST /v1/tasks` takes an optional `snapshot`,
  one of the project's, else 404) and keeps it; migration 0006 adds the nullable
  `tasks.snapshot` and the `workspaces` table. Starting a task of an imported project with no
  snapshot answers 409 `snapshot_required`, and one whose snapshot was taken on an unborn branch
  409 `unborn_base`; a project with no source (only test seeds) keeps the scratch directory. The
  clone is made on the first start that needs it, not at import, and every start fetches, each
  from a freshly built mirror under a per-project lock. The mirror records the original's HEAD
  commit as `refs/ada/head` and the snapshot's base as `refs/ada/base`, the latter written only
  when the mirror has that commit (an original that has pruned it would make the fetch fail; the
  clone may still hold it through an earlier task's branch, else Prepare names it missing), so a
  detached or since-moved base reaches the clone (#79). The fetch runs with `--prune` and never
  takes or prunes the names `ada` and `ada/*`, which stay Ada's: a fresh clone deletes the
  original's branches of those names. The match ignores case (a branch whose first component is
  `ada` in any case, so `Ada` and `ADA/x` but not `adam`, `ada-x` or `feature/ada`), since on
  NTFS and APFS a user's `Ada` shares a file or directory with Ada's `ada/<task>` and would make
  every later fetch in the project fail. Before a fetch, lock files a killed clone or fetch left are
  removed, except under `refs/heads/ada/`, which only engines write; the limit is an engine whose
  exit nobody confirmed (`recovery_required`) still running git, whose `packed-refs.lock` can be
  removed. The clone has `gc.auto=0` and `maintenance.auto=false`, and fetches with
  `--update-shallow` so a shallow original's new shallow roots arrive. The workspace is prepared
  after the launch claim and before the engine starts; a cancel during preparation ends the
  attempt `ended_failed` and a coordinator stop ends it `recovery_required`; the engine never
  starts. Under `core.autocrlf=true` a file stored with LF is checked out with CRLF in Ada's
  worktree (git's own checkout; status stays clean). The manifest's policy is the attempt's
  mode. Untracked files are created through a parent-directory handle
  per component that never follows a link (`openat` with `O_NOFOLLOW`, or `NtCreateFile` with
  `FILE_OPEN_REPARSE_POINT` relative to the parent's handle). Just before the engine starts the
  executor compares the workspace row with the attempt's identity, the row with its hash, and
  the copy in the worktree's git directory with the row, and the worktree's branch and HEAD with
  the manifest's branch and prepared head, which the row's hash binds; any difference, or a
  missing worktree, moves the attempt to `recovery_required` with a recovery event naming it,
  and nothing is rebuilt. Worktrees and task branches are never removed in M3.3; M7 exports and removes them.

- **2026-10-19, M3.4 drift and launch revalidation.** Points the backlog entry left open. A
  snapshot recorded neither its branch nor what each path looked like, only the fingerprint's
  single hash, so drift could not tell which paths changed or which untracked files are new.
  Migration 0007 adds `source_snapshots.branch` (empty when detached) and `snapshot_paths`: for
  each changed or untracked path, its part of the fingerprint (its index entries without stat
  data and its state on disk, hashed apart from the rest), a flag for untracked, taken from the
  capture's first inspection. A snapshot from before 0007 (NULL branch) answers drift with 409
  `snapshot_outdated`; take a new one. `GET /v1/projects/{id}/drift` reads the newest snapshot
  (by insertion order) for a member and answers, in this order, 404 for a non-member, 409
  `no_source`, 409 `no_snapshot`, 409 `snapshot_outdated`, 503 `git_unavailable`; it takes no
  lock of its own, as inspect takes none. When the path now holds another repository (another
  path or git common directory than the imported one) it answers 409 `source_changed`, as a
  snapshot does; with no repository left there, the `not_git_repository` blocker says so. The
  kinds, each apart: `new_commits` are those on the snapshot's branch (on HEAD when the snapshot
  was detached or the branch is gone) that the base does not reach, newest first, at most 100
  listed with `new_commit_count` the total, and none when the original no longer has the base;
  the walk starts from the HEAD commit already read when the branch is the one checked out, so
  `head` and `new_commits` agree; `head_moved` when HEAD is another commit;
  `branch_changed` (with `branch` and `detached`) when the checked-out branch differs, both only
  when HEAD was read; `untracked` the untracked paths the snapshot did not have; `changed` every
  other path whose entry differs, which includes a staged or unstaged change made or undone and
  an untracked file edited or removed; each of the two lists at most 100 paths, as a blocker
  does, with `changed_count` and `untracked_count` the totals; `blockers` every blocker the
  inspection finds, since a snapshot never has one, and a blocker that stops the inspection
  leaves HEAD, the commits and the paths unread. Ignored files are outside the comparison, as
  they are outside the fingerprint. `snapshot_paths` keeps every changed or untracked path
  uncapped, unselected untracked files included, since drift needs each one to tell a new path
  from an old one. The cost: one permanent row per such path per snapshot, so a repository with
  an unignored `node_modules` stores hundreds of thousands of rows a snapshot, and drift holds
  them in memory, as inspect already holds the same paths; capping or expiring them is the
  owner's decision. The workspace
  manifest's version 2 adds the task's revision and the attempt's ownership generation (two
  nullable `workspaces` columns; a version 1 row reads them as -1 and is refused). The revision
  stays put from the start through the launch (a cancel of a launching attempt is handed to the
  executor without a transition), so a moved revision means the manifest is stale. A snapshot
  artifact that is missing or no longer holds its recorded size and hash now moves the attempt
  to `recovery_required` with the reason `snapshot artifact mismatch` (M3.3 ended it
  `ended_failed`): on a first preparation every artifact is read and checked before any of it is
  applied, and just before the engine starts, after the manifest check, every artifact is read
  again.
- **2026-10-19, the engine's environment, and the executor points of #58.** The owner's decision:
  the engine's environment is an allow-list that grows by exactly what the real claude and codex
  CLIs need to find the owner's sign-ins and tools, nothing of `adad`'s own and no provider API
  keys. Besides `PATH`, `SystemRoot`, `WINDIR`, `ComSpec` and `PATHEXT`: on Windows `USERPROFILE`,
  `HOMEDRIVE`, `HOMEPATH`, `APPDATA`, `LOCALAPPDATA`, `TEMP`, `TMP`, `USERNAME`, and the standard
  system variables the CLIs' tools (PowerShell, Git, vswhere) look for, none of them secret:
  `ProgramFiles`, `ProgramFiles(x86)`, `ProgramW6432`, `CommonProgramFiles`,
  `CommonProgramFiles(x86)`, `CommonProgramW6432`, `ProgramData`, `ALLUSERSPROFILE`, `SystemDrive`,
  `PUBLIC`, `OS`, `PROCESSOR_ARCHITECTURE`, `NUMBER_OF_PROCESSORS`, `COMPUTERNAME`, `PSModulePath`;
  on Unix `HOME`,
  `USER`, `LOGNAME`, `TMPDIR`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`,
  `XDG_CACHE_HOME`, `XDG_RUNTIME_DIR`; on both `LANG`, `LC_ALL`, `LC_CTYPE`, and `CLAUDE_CONFIG_DIR`
  and `CODEX_HOME` when set (the credential profiles of the 2026-10-13 entry). Never passed, even
  when set: `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `OPENAI_API_KEY`, any `*_API_KEY` or
  `*_TOKEN`, and any `ADANEXT_*` or `ADA_*` variable; the executor checks that rule on top of the
  list. `HRN_HOME` stays per attempt, and Ada always names the backend, so `hrn` never falls back to
  its `api` backend. Reason: the real CLIs look for their sign-ins and configuration under the
  user's home and these directories, which the earlier list left out. Only the fake engine has run
  with this list so far: the live check with the real claude and codex CLIs happens at M4's first
  real run, and that run is the evidence for this list. The rest of #58: the preparing
  deadline is `adad -preparing-deadline` (default 2m, positive only). The event kinds, and the old
  names M2.3b databases keep (documented, not migrated), are listed in the API notes
  (`internal/api/doc.go`). The executor records one `executor_error` per failure it detects: `abort`
  records the cause before the shutdown and no longer records it again with the exit error; when the
  engine's stream had already ended, the cause waits for the exit and carries its status in that one
  event. After a fatal engine error, the `engine_notice` marked fatal is the cause and no
  `executor_error` repeats it. On Windows a
  coordinator crash kills the engine and its descendants (the Job Object is kill-on-close;
  `TestCrashAndRestartRecoversTheRunningAttemptWithoutRelaunch`), and the next start moves the
  attempt to `recovery_required`. An `ok` result followed by a stdout the post-exit check had to
  close (a descendant held it) ends `ended_ok` when the engine itself exited: the result was
  complete, and only the descendant's later output is lost. This differs from the 2026-10-14
  proposal's case, where the engine itself had to be killed after its result.

- **2026-10-19, executor follow-ups (#57, #62, #73).** On Unix the engine now starts in a
  session of its own (`Setsid`, where it had only a process group), so a backend running without
  a terminal of its own cannot open `/dev/tty`. On Linux, Ada finds the whole tree by session id
  and by parentage: descendants reparented to init or a subreaper, and groups whose leader has
  exited, are both found. Before the kill, the engine's group is stopped (SIGSTOP), and each
  group found is stopped and the tree scanned again until nothing new appears. Each group is
  then killed through a member still bearing the creation time recorded for it. If the group
  kill fails, the engine itself is killed. What the engine leaves behind after a natural exit is
  killed the same way when the attempt ends, as the Job Object does on Windows. A forced kill
  now cuts off the event lines already buffered, as a cancel behind an exited engine does, and
  records "the engine was killed after the grace period" as the reason stdout closed. A
  member's cancel that finds the attempt launching writes a `cancel` event in the transaction
  that decides `ErrAttemptLaunched`, and the accept honours it as it does the owner's stop. The
  macOS limit stays: without `/proc`, Ada kills only the engine's own group while the engine
  runs. Backends that Harness puts in other groups survive a kill there, and nothing is killed
  after a natural exit, because a group id with no creation time to check could name another
  process. A tree walk through `sysctl` would lift this, but only a macOS runner could test it.

- **2026-10-20, M4 in five slices.** M4 is `docs/backlog.md`'s M4.1 the real engine end to
  end, M4.2 checks, M4.3 changes and review, M4.4 the workspace UI, and M4.5 demonstrations.
  M4.1 comes first; M4.2 and M4.3 follow one at a time, since each touches the schema or the
  API; M4.4 can start beside M4.2 and finishes after M4.3; M4.5 is last. The decisions they
  need:
  Check commands are declared per task (argv lists, a working directory inside the worktree, a
  timeout) and are never discovered from the repository's scripts, as `docs/plan.md` section 3
  already says of import: running a repository's package scripts is running its code.
  Ada commits the agent's result as its own commit ("Agent changes", Ada's author) on
  `ada/<task>` before any check runs, so a check result and a review each bind a commit and not
  a work tree that can still change.
  The same checks run once on the snapshot's imported-changes commit as a baseline, so a failure
  that existed before the task is told apart from one the task introduced (plan section 3).
  A review binds the reviewed commit and its check results; any later change to the branch
  invalidates it, as #3 already says of the content hash.
  Refine continues on `ada/<task>` from its tip as a new attempt of the same task, with the
  findings in its prompt; the earlier attempt's worktree, which holds the branch, is removed
  once its record is kept. This settles #98, and moves refinement from M7 to M4.3; the M3.3
  entry's "M7 removes worktrees" now covers export only. Reason: one branch per task keeps the
  lineage plain, and a second branch per attempt would make export choose between them.
  The capability matrix is the pinned `hrn`'s accept and reject answers plus a static table
  for what the engine cannot report (read-only enforcement, network, usage reporting), recorded
  here when M4.1 lands. Mode `ask` stays refused until an engine protocol with approvals exists
  (#7).
  A folder enters only through Electron's dialog in main, which hands the path to `adad`; the
  renderer never types one (#5).
  Fonts (Source Sans 3, JetBrains Mono) are bundled; the app loads nothing remote.
  The live runs of M4.1 are the first real claude and codex runs through `adad`, and with them
  the live check of the #58 environment list.

## Owner actions pending

1. ~~Create the repository~~ (done by the owner, 2026-10-11).
2. ~~Rename the repository~~ (done by the owner, 2026-10-13). Runners: none here, since the
   repository is public and runs on GitHub's runners (decisions log, 2026-10-13); the owner
   merges infra #61 and follows its steps to retire the `lab-ada-os` scale set created by
   infra #55.
3. Decide whether Harness `antigravity-cli` lands through a reviewed pull request.
4. Confirm the owner-decisions table in `docs/plan.md` section 1 and the amendments of 2026-10-13
   in the decisions log.
5. Before M5: approve installing Packer and its VMware plugin at pinned versions, and choose the
   VM storage drive.
6. Branch protection for `main`: GitHub Free offers it to public repositories only, and the
   repository has been public since 2026-10-13. Protect `main` with a ruleset: pull requests
   only, no force pushes, no deletion, the `check` status required once a run has reported on
   `main`. Until then the protection is the deny rules, the push guard and merging only through
   pull requests.
7. For M4b: a Cloudflare API token with `Zone > DNS > Edit` on `adanext.si` alone, kept in
   `adad`'s configuration and never in a repository, and two DNS-only records on the same
   private address: `home.adanext.si` and `preview.home.adanext.si` (the preview host, decisions
   log, 2026-10-14, #5).
8. Now that the repository is public: say which machine details and local paths leave
   `docs/plan.md` (section 8's availability snapshot) and this file (the VMware host table).
   Suggested: the provider sign-in states and the local paths go; the hardware stays.
9. Confirm the M4b credential defaults (decisions log, 2026-10-14, #6): passkeys first, a
   password only where the owner enables it per account, sessions idle 8 hours and absolute 7
   days.
10. Confirm which providers M4b relays first (decisions log, 2026-10-14, #7). Suggested: Codex
    and Claude, the two plans in use, each relayed only if its CLI offers a sign-in without a
    loopback callback when M4b starts; the others desktop-only.
11. When creating the Cloudflare token of item 7 (decisions log, 2026-10-14, #8): set an expiry
    (suggested one year) and say whether to restrict it to the home's public address, which
    breaks renewal whenever that address changes.
12. Confirm or reject the proposal of 2026-10-14 in the decisions log: a successful result
    followed by a forced termination (a slow exit, or a cancel or a Stop the engine does not
    answer by exiting) goes to `recovery_required` with the result kept, until a reconcile flow
    exists; a cancel or a Stop the engine answers by exiting naturally ends the task `canceled`
    with the result kept.
13. For M4.5 (decisions log, 2026-10-20): an Ubuntu machine with a desktop session Ada's
    desktop app can open in, and a claude or codex CLI signed in there on the owner's plan, so
    the Ubuntu demonstration can run. Say which machine.
