# Implementation status

Updated 2026-10-11. Statuses: not started, in progress, implemented (code merged), verified
(evidence below names the revision, commands and results). The plan is `docs/plan.md`; the slices
are in `docs/backlog.md`.

| Milestone | Status | Where | Next |
|---|---|---|---|
| M0 Baseline | in progress | this repository, bootstrap branch | Repository on GitHub, CI runner, merge the bootstrap |
| M1 Execution protocol | implemented | Harness issue #49 (closed): #50 to #53, `main` at `8dd2659` | Windows CI leg on Harness `main`, then verified |
| M2 Durable coordinator | in progress | M2.1 store on a local branch stacked on the bootstrap | Its pull request opens once the bootstrap merges |
| M3 Project workspaces | not started | | |
| M4 First local workflow | not started | | |
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

At `2dc2cdd`, the last code commit after six review rounds; the commits after it change only docs:

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

No runner exists for this repository yet. The workflow is gated on `vars.CI_RUNNER` and is
skipped until then, which does not count as a pass. Local checks are the only evidence until the
owner's steps below are done.

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

## Decisions log

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
  permission check; the owner creates it. The bootstrap is built locally meanwhile, then replayed
  onto the new `main` (`git rebase --onto origin/main --root m0/bootstrap`, keeping this README)
  so its pull request shares history with `main`.
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

## Owner actions pending

1. Create the repository, private, with a README so `main` exists:
   `gh repo create cristiangirlea/ada-os --private --add-readme --description "Ada OS"`
2. Runners, in this order: add `ada-os` to the runners' GitHub App `cristiangirlea-lab-runners`;
   merge the infra pull request that adds `lab-ada-os` (Flux applies it); add `"ada-os"` to
   `ci_runner_repos` in the untracked `infra/tofu/terraform.tfvars`; then, from `infra/tofu` on
   `origin/main`:
   `GITHUB_TOKEN=$(gh auth token) tofu apply -target='module.runner_vars.github_actions_variable.ci_runner["ada-os"]'`.
3. Decide whether Harness `antigravity-cli` lands through a reviewed pull request.
4. Confirm the owner-decisions table in `docs/plan.md` section 1.
5. Before M5: approve installing Packer and its VMware plugin at pinned versions, and choose the
   VM storage drive.
6. Turn on branch protection for `main` in `ada-os` and `harness`: require a pull request, and
   the CI check, before merging; no force pushes or deletions. That is the real boundary the
   push guard only approximates, and it is an account setting only the owner can change.
