# Implementation status

Updated 2026-10-13. Statuses: not started, in progress, implemented (code merged), verified
(evidence below names the revision, commands and results). The plan is `docs/plan.md`; the slices
are in `docs/backlog.md`.

| Milestone | Status | Where | Next |
|---|---|---|---|
| M0 Baseline | in progress | this repository: #1, #12, #14 merged | M0.3 (this amendment), then owner actions 6 and 8 |
| M1 Execution protocol | implemented | Harness issue #49 (closed): #50 to #53, `main` at `8dd2659` | Windows CI leg on Harness `main`, then verified |
| M2 Durable coordinator | in progress | M2.1 store on a local branch stacked on the bootstrap | Its pull request opens once the bootstrap merges |
| M3 Project workspaces | not started | | |
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
   `adad`'s configuration and never in a repository, and the `home.adanext.si` record.
8. Now that the repository is public: say which machine details and local paths leave
   `docs/plan.md` (section 8's availability snapshot) and this file (the VMware host table).
   Suggested: the provider sign-in states and the local paths go; the hardware stays.
