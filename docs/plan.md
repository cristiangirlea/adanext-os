# Ada OS implementation plan and Claude handoff

Date: 2026-10-11

Status: Planning complete. Verified against the machine and the Harness source on 2026-10-11 (section 8). Ada implementation has not started. Repository inspection, model discovery, and one bounded read-only Harness audit have been performed. This file is the standalone handoff for continuing the work in Claude or another coding agent.

## Start here in Claude

Read this whole document before changing files. It contains the product decisions already made with Cristian, the architecture, the implementation sequence, and the acceptance gates. Do not restart product discovery or ask him to repeat these decisions. Section 8 records the verification of this file on 2026-10-11 and the gaps it found; M0 items 11-14 come from it.

Start in `E:\__Workdir\personal`. Read the applicable repository instructions before working in a checkout. Inspect current Git state because another chat may have advanced Harness since this handoff. The preferred Harness candidate inspected for this plan is `E:\__Workdir\personal\harness-ai` at `8d521f0`; it was clean when this file was prepared. The older `E:\__Workdir\personal\harness` checkout has substantial uncommitted durable-worker work. Preserve it and do not reset, overwrite, or silently incorporate it. The historical path `E:\__Workdir\harness` was absent during inspection.

Create Ada as a separate repository at `E:\__Workdir\personal\ada-os`. Work on feature branches and follow the SDLC described below. Use Harness to delegate implementation to bounded tasks, inspect the results, run independent checks, and obtain independent review. Keep the owner informed of findings and completed evidence without stopping for routine local implementation choices.

Begin with M0, then M1 and M2. Carry the authorized local work through implementation, tests, and ordinary repair. Public pushes, PR publication, merges, deployment, tags, and releases require explicit owner authorization. Do not treat a historical instruction in Harness's PLAN.md as new authorization to publish or merge.

Use GPT 6.1 Sol as the main implementation model through the Codex backend when available. Use Claude Opus 5.5 for difficult independent review, and economical models for small bounded tasks. Do not use Astra for ongoing implementation or automatic escalation. Recheck availability instead of assuming the model/quota snapshot below is still current.

The first concrete outcome is a versioned Harness execution interface plus a background Ada coordinator whose task state remains inspectable after the desktop window closes. Do not stop at a visual mockup.

## 1. Product, scope, and starting point

Ada OS will be a desktop workspace that remembers a project's state, proposes the next bounded task, runs the chosen agent, and brings back verified changes for review.

The first complete workflow is:

> Open project -> review what was left unfinished -> Continue -> agent works on a feature branch -> checks run -> inspect diff and preview -> review and integrate.

### Confirmed owner decisions

| Decision | Plan |
|---|---|
| Name | Ada OS |
| First platforms | Windows 11 x64 and Ubuntu 24.04 LTS x64 |
| Initial audience | Cristian first, then other developers |
| Execution | Both local and VM modes |
| First VM provider | VMware Workstation |
| Later execution providers | QEMU, then existing VMs over SSH |
| Development process | Feature branches, tests, independent review, controlled integration |
| Existing uncommitted work | Offer a reviewed snapshot into the task workspace |
| Closing the window | Authorized work continues in the background |
| macOS | Later, when a real Mac is available for validation |
| Primary implementation model | GPT 6.1 Sol |
| Architecture model | Astra for the planning stage; no automatic implementation routing to Astra |

V1 includes project import, an evidence-backed resume summary, task execution, live activity, changes, verification, previews, review, and recovery.

Screen capture, voice, general desktop control, calendars, an app store, and a bootable Linux distribution follow later. The desktop workflow must work before expanding Ada's access to the machine.

### Existing implementation

The newer Harness worktree inspected is `E:\__Workdir\personal\harness-ai`, clean at commit `8d521f0` (`agent: enable sandboxed autonomous Codex and Antigravity runs`). It includes the earlier AGY integration at `673ef23`. The remote main observed during planning was `1ca5619`. The installed `hrn` and connected MCP tool were older than the AGY worktree: do not assume their schemas expose its capabilities.

The older `E:\__Workdir\personal\harness` checkout is on `durable-workers` at `634507a` and contains separate, uncommitted durable-worker work.

| Existing Harness capability | Treatment in Ada |
|---|---|
| Multiple agent backends | Reuse |
| Normalized task, event, and result contracts | Extend compatibly |
| Saved runs and native session continuation | Reuse, with stronger identity and recovery rules |
| Worktrees, checks, retries, evaluation | Reuse selected machinery |
| AGY model discovery | Reuse |
| Read-only run monitoring | Use as implementation reference |
| Durable PostgreSQL worker prototype | Preserve separately; do not make it a v1 dependency |
| Desktop UI, durable local coordinator, VM management | Build |

Two verified limitations drive the architecture:

- Harness continuation currently expects a completed run. It does not reconstruct a process interrupted mid-operation.
- Current run logs cannot reliably distinguish a running process from one killed before writing its result. Event-writing errors are not adequately surfaced by the current writer.

A bounded GPT 6 Luna audit through Harness confirmed the continuation limitations. It completed successfully in 17 seconds, run `20261011-112105-4ae8ae`. This was read-only investigation, not an Ada implementation or a benchmark of general model quality.

Useful source entry points in the newer Harness checkout:

- `AGENTS.md`, `CLAUDE.md`, `README.md`, and `PLAN.md`: repository rules and current capabilities; older PLAN sections contain historical names and facts.
- `internal/agent/`: backend contract, CLI adapters, permissions, model discovery, native sessions.
- `internal/followup/followup.go`: completed-run continuation, workspace checks, cross-backend handoff.
- `internal/runlog/runlog.go`: stored task/events/results and current persistence limitations.
- `internal/batch/batch.go`: worktree lifecycle, gates, retries, automatic commits, and dirty-source behavior.
- `internal/tools/exec_windows.go`: process-tree handling and the acknowledged post-start Job Object attachment race.
- `internal/hrnmcp/`: existing delegation interfaces.

The older checkout's `server/README.md` records the durable-server fixture limits. A fake-model worker result is not evidence of real remote tool execution or native CLI crash recovery.

## 2. Architecture and contracts

Create a separate repository at `E:\__Workdir\personal\ada-os`. Keep Harness independently usable and preserve its standard-library-only Go module. Ada must not import Harness's private Go `internal` packages across repository boundaries.

### Chosen stack

| Component | Choice | Responsibility |
|---|---|---|
| Desktop | Electron, React, TypeScript | Windows/Linux interface, native dialogs, notifications |
| Coordinator | Go executable, `adad` | Projects, tasks, scheduling, durable state, recovery |
| Database | SQLite through `modernc.org/sqlite` | Local authoritative state without a C compiler dependency |
| Execution engine | Pinned Harness executable | Backend invocation, events, results, session continuation |
| VM controller | VMware adapter | Create, start, stop, inspect, and recover Ada-owned VMs |
| Guest service | Go executable, `ada-guest` | Guest execution, event journal, artifacts, process control |
| UI tests | Playwright and component tests | Real workflow and presentation verification |
| Packaging | Electron Builder | Windows installer and Ubuntu `.deb` |

Pin exact dependencies and toolchain versions in M0. Package the matching Harness binary with Ada rather than silently using whichever `hrn` happens to be on PATH. Record the engine source revision and binary hash in diagnostics and run evidence.

```text
Ada desktop renderer
  -> narrow preload bridge
  -> Electron main
  -> restricted local IPC
  -> adad coordinator
       -> SQLite and content-addressed artifacts
       -> LocalExecutor -> pinned Harness -> selected backend CLI
       -> VMwareExecutor -> ada-guest -> pinned Harness -> selected backend CLI
```

### Desktop boundary

The renderer receives a small typed API through Electron's preload bridge. It gets no direct filesystem, shell, database, or credential access.

Use context isolation, renderer sandboxing, validated IPC senders, and disabled Node integration. Project previews run in separate sandboxed content with no Ada bridge. Follow Electron's security guidance: https://www.electronjs.org/docs/latest/tutorial/security

Electron's main process connects to `adad` over a per-user Windows named pipe or owner-only Unix socket. A singleton coordinator survives the desktop window closing. Its process lifetime must not be tied to a Job Object or process group owned by the desktop window.

The desktop reconnects to an existing compatible coordinator or starts one independently. Closing the window keeps work running. Provide an explicit `Quit Ada and stop work` action with orderly cancellation. Host logout/reboot has recovery semantics, not a promise of uninterrupted execution.

### Durable state

Use SQLite WAL with foreign keys, transactional migrations, and `synchronous=FULL`. Store large artifacts outside the database by content hash; commit their references only after successful writes. Backups use a consistent SQLite backup operation, not a copy of a live database file alone.

Core records:

- `Project`: source identity, configuration, selected execution defaults.
- `SourceSnapshot`: base commit, reviewed files, staged/unstaged evidence, content fingerprint.
- `ResumeSummary`: observations, evidence references, recommendation, snapshot version.
- `Task`: objective, acceptance criteria, approved scope and execution policy.
- `Attempt`: backend/model, workspace, executor, ownership generation, native session lineage.
- `Event`: ordered sequence, attempt, event type, payload.
- `Verification`: command, environment, tested content hash, result and logs.
- `Review`: reviewed content hash, findings, decision.
- `Artifact`: hash, size, origin and retention state.
- `PermissionDecision`: exact action/scope, policy version, relevant content identity, expiry and decision.
- `RecoveryCase`: uncertain operation, last known evidence, resolution.

Keep backend exit status separate from task completion:

```text
draft -> ready -> preparing -> running -> verifying -> awaiting_review -> accepted
                               |           |                |
                               +-> failed  +-> failed       +-> refinement
                               +-> cancel_requested -> canceled
                               +-> interrupted -> recovery_required
```

An agent saying `done` does not make the task accepted. A review or permission decision binds the current content and ownership generation; it cannot authorize unrelated later work after the underlying state changes.

### Harness interface additions

Add a versioned stdio command, provisionally `hrn exec --protocol v1`, rather than parsing terminal presentation.

Its contract includes:

- Version and capability handshake.
- Explicit backend/model and validated execution options.
- Caller-supplied attempt identity.
- Accepted, progress, session, tool, error, and terminal events.
- Ordered event sequence numbers.
- Cancellation.
- Approval messages only for backends that actually support them.
- Explicit storage/protocol failures.
- Requested and resolved model identities.
- Bounded message sizes and output handling.

Retain existing CLI/MCP compatibility. Add fields to stored records compatibly. Keep existing `agent.Backend` task/events/result behavior as the execution foundation.

Extend reasoning-effort mapping: the inspected Harness forwards effort to API/AGY paths, while Codex and Claude need explicit adapter support before Ada can claim it selected a particular effort. Verify arguments against each installed CLI and test them; never silently accept an ineffective option.

The coordinator's public interface covers project import/inspection, snapshot creation, summary generation, task creation/start/cancel/continue, event subscription, checks, review, executor setup, and change export. Mutating requests carry an idempotency key and expected task revision.

The renderer cannot submit arbitrary host paths or shell commands outside the typed task/executor interfaces. Logs and arguments must not expose credentials.

## 3. The project-resume experience

### Import

The user selects a repository. Ada inspects it without executing repository scripts or hooks.

Capture:

- Canonical repository identity and selected base revision.
- Current branch and working status.
- Repository instructions and build manifests.
- Relevant recent history.
- Existing Ada tasks and verifiable Harness history.
- Staged, unstaged, and selected untracked files.

Present uncommitted work explicitly. The default offer is `Include these changes in the new feature branch`.

Create an Ada-owned clone with independent Git administration, without hardlinks or borrowed object storage. Create task worktrees beneath that clone. A normal worktree attached directly to the original repository shares Git administration and is insufficient for this source-preservation design.

Do not silently stash, reset, clean, or commit the original checkout. Compare source fingerprints before and after capture to detect changes during copying. Preserve the distinction between imported user changes and changes generated during the task.

Capture staged and unstaged evidence separately, even if the execution snapshot uses final working-file content. Review selected untracked files; exclude ignored files, credentials, caches, and generated directories unless deliberately included.

V1 import supports ordinary Git repositories and safe internal symlinks. Escaping symlinks/reparse points, unresolved submodules, missing LFS objects, and unsupported sparse-checkout cases produce specific import blockers. They are never silently omitted.

Do not use Harness's `allow_dirty` as snapshot support: its task worktrees start from HEAD and do not automatically carry working changes.

Each managed workspace has a versioned manifest binding project/task/attempt, source snapshot, base revision, executor, and policy. Missing or mismatched identity requires recovery instead of silent reconstruction.

### Resume summary

Start with deterministic evidence extraction. Use a model only to synthesize the bounded packet. Do not add a vector database in v1.

The summary answers:

1. What were we trying to accomplish?
2. What has changed?
3. What has actually been verified?
4. What remains unfinished or uncertain?
5. What is the next useful task?

Each factual claim links to evidence. Distinguish observed facts, user decisions, model reports, and inferences. Validate evidence IDs so a generated summary cannot invent references.

Example:

> The authentication form is implemented. The last recorded check failed on token refresh. Two changed files remain unreviewed. Suggested next task: fix refresh handling and rerun the authentication tests.

Do not claim checks passed merely because a previous agent said so. Store the snapshot fingerprint and generation time with each summary.

Cache summaries by project fingerprint. Opening Ada should not trigger repeated model calls for unchanged projects. Users can inspect the project without using a model.

### Continue

Continue creates an explicit task specification containing:

- Objective and acceptance criteria.
- Selected feature branch and source snapshot.
- Allowed workspace and relevant files.
- Local or VM execution.
- Backend/model policy.
- Verification commands.
- Timeout and retry allowance.
- Network and execution permissions.

Normal implementation and repair remain within that authorization. A changed objective or broader access requires a revised task specification.

Immediately before execution, revalidate the snapshot and persist the attempt intent. Refuse stale or duplicate launch requests.

### Workspace UI

Use a practical application layout:

- Left navigation: projects and active tasks.
- Main area: current task, resume summary, activity.
- Resizable work area: Changes, Checks, Preview, Files.
- Persistent status: branch, executor, model, task state, Stop.
- Overview: unfinished work and items requiring attention.

Initial visual direction: restrained slate and blue-green surfaces, a light default theme with dark mode, Source Sans 3 for interface text, and JetBrains Mono for code. Use compact rows and resizable panes. The distinctive element is the resume summary connected directly to its evidence and next action.

Support keyboard navigation, visible focus, reduced motion, scaling, and clear empty/error states. Read and apply the available frontend-design skill before creating the interface. All milestone demos must use the real coordinator; fixtures are for tests, not proof of working execution.

### Verification and review

Ada runs declared checks independently after the agent finishes. Record exact commands, environment, content hashes, exit codes, and logs.

Edits after verification invalidate the corresponding result. Existing baseline failures remain distinguishable from failures introduced by the task.

A preview is a managed process with an explicit command, directory, port, health check, and stop policy. Ada does not automatically run arbitrary package scripts during import.

Review offers:

- Inspect changes.
- Inspect verification evidence.
- Request a bounded refinement.
- Accept the reviewed result.
- Export a patch or transfer the reviewed feature branch for integration.

Refinement stays in the task lineage and preserves work. Integration revalidates the destination and stops on drift or conflicts.

Public pushes, PR publication, merges, and releases remain separate, explicit actions after the result is concrete and reviewed. Preserve the original checkout's index and unrelated working changes.

## 4. Local execution, VMware, and recovery

### Local mode

Local execution uses the managed feature workspace and a verified backend permission profile.

A worktree is a development boundary, not protection against hostile code. Ada displays the actual protections provided by the selected backend.

Build a capability matrix for:

- Read-only enforcement.
- Workspace writes and shell execution.
- Network restrictions.
- Approval support.
- Native continuation.
- Cancellation.
- Model discovery.
- Usage reporting.
- Enforceable limits.

Refuse incompatible requests before launch. The current AGY adapter rejects read-only mode, so it cannot serve as a supposedly read-only project inspector.

Do not enable permission bypass or automatic boundary escalation by default. Use explicit executable arguments, a controlled environment, and verified provider settings. Repository hooks, provider configuration, and installed plugins must be included in the capability assessment.

The API backend's command guard does not enforce the external CLI backends' tool calls. Do not describe a UI approval button as universal command interception. Do not change the user's global provider configuration silently.

### VMware mode

Use VMware Workstation first. The inspected Windows machine has Workstation 17.6.4 build 24832109, 64 GiB-class RAM, and an enabled hypervisor. `vmrun` successfully listed zero running VMs. This proves installation and basic management access, not the planned VM workflow.

Create a reproducible Ubuntu 24.04 Server template using a pinned Packer VMware plugin and unattended installation. Provision:

- `open-vm-tools`.
- `ada-guest` and the matching Harness build.
- Git and the initial Go/Node development toolchains.
- Separate control and workload accounts.
- Guest firewall and logging.
- No embedded provider credentials.

Sanitize and shut down the template. Ada creates a full clone for each managed project VM.

| Resource | Default |
|---|---|
| CPU | 4 virtual CPUs |
| Memory | 8 GiB |
| Disk | 80 GiB sparse |
| Active VMs | 1 |
| Agent tasks per workspace | 1 |

Start through `vmrun ... nogui`. Manage only VMs registered as Ada-owned. Never stop unrelated VMs.

Use pinned guest SSH identity and a restricted guest service. Do not put guest passwords into command-line arguments. Keep guest administration and model-controlled workload processes separate; the workload must not obtain coordinator credentials or unrestricted root control.

Disable shared folders, clipboard integration, and drag-and-drop. Transfer reviewed project snapshots and validated artifacts explicitly. Validate transferred paths, sizes, hashes, and symlinks before accepting content.

The managed setup uses a dedicated host-only network, host firewall rules, and controlled outbound access. VMware NAT alone is not treated as isolation. Provider connectivity and preview forwarding must pass live tests. Present concrete host network changes during setup before applying elevated configuration.

Provider login occurs inside the guest through the provider's supported flow. Host credential directories are not copied. Host and guest backend availability appear separately. Browser/device-flow callbacks and proxy compatibility must be verified for each supported guest provider.

Before provisioning, check the installed VMware version, virtualization support, available resources, and, on Linux, the actual kernel and VMware module compatibility. Do not disable Hyper-V/VBS automatically or freeze Ubuntu on an obsolete kernel to satisfy an old VMware version.

Guest image and toolchain changes create a new version. Do not replace the active image or restore a snapshot under a running task. Restoring a VM invalidates ownership/session assumptions and requires reconciliation.

### Executor abstraction

Define one shared executor interface:

```text
Probe
PrepareWorkspace
StartAttempt
SubscribeEvents
CancelAttempt
InspectOwnership
CollectArtifacts
StartPreview
StopPreview
```

Implement `LocalExecutor` and `VMwareExecutor` for v1. Add `QEMUExecutor`, followed by `SSHExecutor`, after the initial release. Keep provider-specific VM lifecycle operations outside task logic.

The later QEMU adapter uses WHPX on Windows and KVM on Linux after compatibility checks. The later SSH adapter targets an explicitly registered existing VM and does not assume provisioning ownership. These are planned follow-on capabilities, not v1 completion claims.

### Recovery rules

| Situation | Required behavior |
|---|---|
| Window closes/crashes | Task continues; reopening reconnects |
| Backend fails | Preserve workspace and evidence; allow bounded repair |
| User stops a task | Verify process-tree termination before reporting canceled |
| Coordinator crashes | Reconcile ownership before starting another attempt |
| VM disconnects | Treat the old attempt as potentially running |
| VM crashes | Preserve disk and reconcile the guest journal |
| Host restarts | Recover project/task state without blindly rerunning work |
| Storage fails | Stop advancing the task and surface the failure |
| Operation outcome is unknown | Require recovery review rather than replay |

Use ownership generations and process creation identity, not PID alone. The guest has a bounded lease; loss of ownership triggers cancellation, and a replacement attempt waits for verified termination.

Native continuation is used only with a compatible backend, session, workspace, and policy. Otherwise create a new session with an evidence handoff: objective, decisions, current diff, checks, artifacts, and remaining work.

Record whether continuation is a saved API conversation, native session fork, native conversation resumed in place, or a new session with evidence handoff. AGY resumes its conversation in place; Claude/Codex fork semantics differ. Prevent concurrent reuse of the same mutable native session.

The product promise is preserved work and explicit recovery. It does not promise exactly-once execution or resumption at an arbitrary internal CLI instruction. A Linux guest test on a Windows host establishes Linux behavior, not Windows-target correctness.

## 5. Model routing and the development process

### Model allocation

| Work | Default |
|---|---|
| Main implementation | `codex / gpt-6.1-sol`, medium effort |
| Persistence, permissions, recovery, concurrency | Sol, high effort |
| Difficult independent review | `claude / claude-opus-5-5` |
| Small inspections, fixtures, documentation | `codex / gpt-6-luna` |
| Bounded UI and routine implementation tasks | AGY Sonnet 5.5 or Gemini 3.8 Flash after qualification |
| Escalation | Small model -> Sol -> Opus when available |
| Astra | Planning already performed; excluded from automatic implementation routing |

The requested model identities must be preserved. Discover provider-specific IDs and capabilities; do not guess an alias or silently substitute another model.

### Availability snapshot from 2026-10-11

- Codex CLI is signed in through ChatGPT. The local model catalog includes `gpt-6.1-sol` and `gpt-6-luna`.
- Luna successfully completed the read-only Harness audit noted above.
- Claude Code is signed in on a Max subscription, but the attempted `claude-opus-5-5` review hit the weekly limit. That attempt did not produce a review. It also reported that workspace-local permission entries were ignored because trust had not been accepted. Do not edit trust records to bypass a required interactive trust decision.
- AGY CLI 1.3.3 returned 18 models through a live model-list query.
- API credentials were not available to the connected Harness backend inventory.
- The connected Harness MCP interface did not yet expose the new AGY backend, although the newer source checkout did. Build and use a pinned candidate, or update the configured integration deliberately; do not assume a running MCP server refreshes automatically.

AGY model IDs returned:

```text
gemini-3.8-flash-high
gemini-3.8-flash-medium
gemini-3.8-flash-low
gemini-3.7-flash-high
gemini-3.7-flash-medium
gemini-3.7-flash-low
gemini-3.6-flash-high
gemini-3.6-flash-medium
gemini-3.6-flash-low
gemini-3.1-pro-high
gemini-3.1-pro-low
claude-opus-5-5-low
claude-opus-5-5-medium
claude-opus-5-5-high
claude-sonnet-5-5-low
claude-sonnet-5-5-medium
claude-sonnet-5-5-high
gpt-oss-120b-medium
```

Use `antigravity/<slug>` where the Harness catalog expects a namespaced model identity. A catalog entry is not proof of remaining quota, sandbox behavior, or a successful execution. Recheck all availability and quotas when implementation begins.

When Claude is unavailable, use a fresh Sol reviewer. AGY Opus becomes a fallback after its execution profile passes qualification. Never switch silently to Astra or separately billed API usage.

### Cost and concurrency defaults

- Maximum two implementation workers and one reviewer.
- One writer per workspace.
- Parallel workers: the policy added on 2026-10-11 is in section 8.
- Small tasks: 10-minute timeout.
- Implementation tasks: 30-minute timeout.
- Reviews: 15-minute timeout.
- One repair attempt before escalation or task decomposition.
- No repeated retries for authentication or quota failures.
- No automatic premium speed tier.
- Summaries regenerate only when evidence changes.

Show provider-reported usage, estimated cost, or unknown explicitly. Subscription access is not free, and unsupported dollar caps are not presented as enforced. The current AGY adapter rejects cost/token/turn ceilings; use supported limits such as timeout. Configure reasoning effort only after its backend mapping is implemented and verified.

Do not send the entire project, conversation history, and full plan to every small worker. Send bounded task packets with the exact context needed. Benchmark small models on representative bounded tasks before promoting them to routine use; model labels alone do not establish quality.

### Supervision and SDLC

Every Harness task receives a bounded packet:

- Base revision and workspace identity.
- Objective and acceptance criteria.
- Owned files and shared interfaces.
- Explicit exclusions and stop conditions.
- Backend/model, effort where supported, executor and permission profile.
- Timeout and retry allowance.
- Required tests and artifacts.
- Independent reviewer.

The supervising agent inspects activity, completion state, changed files, and failed checks; intervenes when a worker expands scope or repeats an unproductive approach; and sends concrete repairs to the appropriate model.

Independent tasks can run in parallel after shared contracts are fixed. Changes to the coordinator, protocol, and data model integrate sequentially. A successful model answer is not acceptance evidence.

For Ada's own workflow, use single Harness runs initially; Ada owns branch, review, and checkpoint behavior. Harness batch's automatic commit behavior should not define the product lifecycle.

Follow repository rules: feature branch, meaningful tests, implementation, checks, independent review, repair, and reviewable integration. Preserve Cristian's configured author identity and the no-unsolicited-attribution rule. Read current instructions rather than copying outdated historical rules from memory.

Keep the original durable-worker checkout unchanged. Work from a reviewed immutable engine baseline in an appropriate isolated worktree. Do not commit, publish, or merge another chat's changes merely to make the baseline convenient.

## 6. Milestones and acceptance gates

| Milestone | Deliverable | Lead model | Completion evidence |
|---|---|---|---|
| M0 Baseline | Validate Harness `8d521f0`, pin dependencies, create Ada repository and task backlog | Sol | Clean baseline, passing Harness checks, recorded backend capabilities |
| M1 Execution protocol | Structured Harness interface, capability handshake, effort mapping, cancellation and errors | Sol; Opus/Sol review | Protocol tests, fake CLI fault tests, compatible old records |
| M2 Durable coordinator | SQLite, IPC, task/event state, singleton process, reconnect | Sol high | Duplicate-start, crash, disk-full, migration and restore tests |
| M3 Project workspaces | Import, inspection, reviewed snapshots, managed clones and feature branches | Sol high | Original checkout unchanged; accurate snapshot and drift detection |
| M4 First local workflow | Real desktop import -> task -> edit -> checks -> diff -> review | Sol; bounded UI tasks to cheaper models | Working Windows and Ubuntu demonstrations |
| M5 VMware execution | Template, provisioning, guest login, execution, artifacts and preview | Sol high; independent review | Real isolated run, cancellation and recovery on each supported host |
| M6 Resume intelligence | Evidence summaries, next-task proposals, native/evidence continuation | Sol; Luna for bounded synthesis | Valid citations, stale-state rejection, successful refinements |
| M7 Review and integration | Review package, feature-branch export, destination checks | Sol | No lost staged/unstaged work; drift/conflict tests |
| M8 Recovery and routing | Failure reconciliation, model policies, quota handling, bounded retries | Sol high | Fault matrix passes; no duplicate attempts or silent fallback |
| M9 Packaging and dogfood | Windows installer, Ubuntu package, diagnostics, accessibility | Sol; cheaper models for packaging/docs tasks | Installed-app workflow and a real Ada improvement completed through Ada |

M1-M3 establish contracts before broad UI work. The VMware compatibility spike starts during M0 because it can expose infrastructure constraints early. M4 provides the first usable product while VM work proceeds. Local-only success is an intermediate milestone, not full v1 completion.

### M0 implementation checklist

1. Re-read current source and repository instructions; verify candidate commit and working-tree state.
2. Establish an isolated engine worktree for Ada-related Harness changes without moving or overwriting another active worktree.
3. Run the relevant Harness baseline checks before changing it. Record which checks actually ran and which were unavailable.
4. Build a pinned candidate binary into task-owned build output and verify its version/capabilities.
5. Create the separate Ada repository and initial feature branch, respecting configured author identity.
6. Pin Go, Node, Electron, frontend tooling, database library, and packaging dependencies in reproducible manifests/lockfiles.
7. Create an implementation backlog from M1-M9 with owned interfaces and explicit acceptance tests.
8. Run bounded model qualification using available subscriptions. Record requested/resolved model, backend version, permissions, outcome, and usage where provided.
9. Verify VMware host requirements and image-factory feasibility before promising the four execution lanes.
10. Continue directly into M1-M2 once the baseline is sound; report a concrete blocker if it prevents progress.
11. Add the repository AI setup the owner's rules require for a private repository (section 8, gap 1) in the first `ada-os` branch, before delegating any task.
12. Request the `lab-ada-os` CI runner (an infra slice plus the owner's GitHub App step, section 8, gap 2). Cap test parallelism for the lab runner. Record in every evidence package whether CI actually ran.
13. Record the effective Codex reasoning effort taken from `~/.codex/config.toml` in run evidence until M1 maps effort explicitly (section 8, gap 3).
14. Obtain the owner's decision on landing `antigravity-cli` through a pull request before pinning the engine baseline (section 8, gap 4).
15. Prepare the `hrn batch` spec template for Ada (parallel 2, gate commands from the check skill, sandboxed backends only) so M3 onward can fan out (section 8, Parallel workers).

### Required test scenarios

- Clean and dirty repository import, including staged/untracked files.
- Source changes during capture and before integration.
- Windows junctions, symlinks, traversal, CRLF, and executable bits.
- Repeated Continue clicks and duplicate requests.
- Closing/reopening the window during an actual task.
- Backend, coordinator, and VM failure.
- Cancellation of child process trees, including startup races and PID reuse.
- Quota exhaustion, unsupported model, and denied command.
- Full disk, malformed stream, truncated events, and missing results.
- Native continuation and cross-provider evidence handoff.
- Stale verification after a file changes.
- Preview content attempting to reach desktop privileges.
- Guest attempts to access host files and unauthorized host/network services.
- Lost guest transport, stale ownership generation, wrong guest identity, and duplicate resume.
- Install, upgrade, backup, restore, and uninstall with preserved user work.

Automated tests use fake providers. Live model smoke tests are bounded and recorded separately. Windows and Linux checks run on the owner's infrastructure; preserve Harness's existing runner restrictions. Do not add a GitHub-hosted fallback. Windows race-test limitations must be stated accurately and covered on the available Linux test infrastructure.

### V1 release gate

The complete workflow must pass in four lanes:

1. Windows desktop with local execution.
2. Windows desktop with VMware execution.
3. Ubuntu desktop with local execution.
4. Ubuntu desktop with VMware execution.

At least one real backend must complete each lane. The supported backend/model/executor matrix must describe exactly what was tested; do not imply all providers support all lanes.

The Ubuntu host for lane 4 has not yet been established. The planning question about that host received no answer. Ubuntu UI testing inside a Windows-hosted VM can advance development, but does not prove Ubuntu-host VMware support. Keep that lane explicitly pending until tested on a suitable host. This is a release-evidence dependency, not a reason to stop independent Windows or core development.

After those gates, use Ada to complete one real, bounded improvement to itself. The evidence package contains the approved task, feature branch, model/executor identity, changes, independent checks, review, and a close/reopen continuity result.

The first implementation increment is M0-M2: establish the clean engine baseline, add the execution contract, and prove that a background Ada task remains inspectable after the desktop window closes.

## 7. References and status recording

Primary documentation consulted during planning:

- Electron security: https://www.electronjs.org/docs/latest/tutorial/security
- SQLite WAL: https://www.sqlite.org/wal.html
- SQLite backup and corruption cautions: https://www.sqlite.org/howtocorrupt.html
- GPT 6.1 Sol: https://developers.openai.com/api/docs/models/gpt-6.1-sol
- GPT 6 Luna: https://developers.openai.com/api/docs/models/gpt-6-luna
- Claude Opus 5.5: https://platform.claude.com/docs/en/models/opus-5-5/overview
- Antigravity CLI permissions: https://www.antigravity.google/docs/permissions?tab=cli
- Antigravity sandbox: https://www.antigravity.google/docs/sandbox?tab=cli
- VMware host support matrix: https://knowledge.broadcom.com/external/article?legacyId=80807
- VMware Hyper-V compatibility guidance: https://knowledge.broadcom.com/external/article?legacyId=76918
- Later QEMU Windows acceleration: https://www.qemu.org/docs/master/system/whpx.html

Documentation and product versions can change. Recheck exact versions and host compatibility at the relevant implementation gate. Do not substitute documentation for a live capability test, particularly for Windows versus Linux CLI sandbox behavior.

As implementation proceeds, maintain `docs/implementation-status.md` in the new Ada repository. Record each milestone as not started, in progress, implemented, or verified; include the exact revision, tests, live demonstrations, outstanding findings, and next task. Keep implemented and verified separate. Preserve this handoff as the accepted starting plan; record material changes and their reasons instead of silently rewriting decisions.

## 8. Verification of this handoff and recorded gaps (2026-10-11)

This file was checked in Claude Code on 2026-10-11, after it was written, against the machine and the Harness source. Every checkable claim held. This section records the evidence, the items that could not be checked, and the gaps the plan did not cover. The gaps feed M0 checklist items 11-14 and the milestones named in the last column.

### Claims verified

| Claim | Evidence |
|---|---|
| `harness-ai` clean at `8d521f0`, branch `antigravity-cli`, remote main `1ca5619` | `git status --porcelain` empty; `git rev-parse origin/main`; `git worktree list` |
| `harness` on `durable-workers` at `634507a` with uncommitted work | 17 dirty entries in `git status --porcelain` |
| `E:\__Workdir\harness` absent; `ada-os` not created | directory listing of `E:\__Workdir\personal` |
| Harness module has no third-party dependencies | `go.mod` holds only the module line and the Go version |
| Continuation requires a completed run | `internal/followup/followup.go:63-65` |
| A running run and a killed run look the same; event write errors are dropped | `internal/runlog/runlog.go:190-205` (`Done` means `result.json` exists); `runlog.go:97` discards the encode error |
| AGY adapter rejects read-only and ask modes, and turn, cost and budget limits | `internal/agent/antigravity.go:64` and `:80` |
| Effort is forwarded only on the API and AGY paths | `internal/agent/options.go:26`; no effort handling in `codex.go` or `claude.go` |
| `allow_dirty` worktrees branch from HEAD | `internal/batch/batch.go:279-295` |
| Post-start Job Object attachment gap is acknowledged | `internal/tools/exec_windows.go:88-90` |
| Luna audit run `20261011-112105-4ae8ae` | codex, `gpt-6-luna`, readonly, status ok, 17.0 s, one tool call; its three findings match section 1 |
| Opus 5.5 review attempt hit the weekly limit and the trust warning | run `20261011-112845-c19ad3`: status error; events contain "You've hit your weekly limit", resetting 13:00 Europe/Bucharest; the result error is the untrusted-workspace message |
| Codex signed in through ChatGPT; catalog lists Sol 6.1 and Luna | `codex login status`; `~/.codex/models_cache.json` lists `gpt-6.1-sol`, `gpt-6-luna`, `gpt-6-sol`, `gpt-6-astra`; CLI 0.162.0-alpha.17.2 |
| AGY CLI 1.3.3 returns the 18 listed models | `agy models` on 2026-10-11 returned the same 18 identifiers in the same order |
| Connected `hrn` MCP lacks Antigravity; the source build has it | MCP `backends` lists api, claude, codex, gemini, opencode; `C:\Users\crist\go\bin\hrn.exe` help has no antigravity, `harness-ai\hrn.exe` does; both print `hrn 0.2.0`, so the version string cannot tell them apart |
| VMware Workstation 17.6.4 build 24832109; `vmrun` lists zero VMs | registry `ProductVersion`; `vmrun -T ws list` |
| 64 GiB-class RAM with the hypervisor enabled | 61.6 GiB reported; `HypervisorPresent` true |
| Harness CI runs only on the owner's runners | `harness-ai/AGENTS.md:46-51`; `.github/workflows/ci.yml:17` |

Not verified: the documentation URLs in section 7 were not fetched; model names were confirmed from local CLI state instead. The owner-decisions table in section 1 is the planning chat's record and awaits the owner's confirmation. The Ubuntu host for lane 4 remains unresolved, as section 6 says. Nothing after this file's creation touched `harness-ai`; the two later Harness runs that day were tidedesk reviews.

### Gaps found and required actions

| # | Gap | Required action | Where |
|---|---|---|---|
| 1 | The new repository's AI setup is not planned. The owner's standing rule for private repositories requires `AGENTS.md` under about 100 lines, `CLAUDE.md` starting with `@AGENTS.md`, `.claude/rules/*.md` with `paths:` for part-specific guidance, `.claude/skills/check/SKILL.md` running what CI runs, `.claude/settings.json` allowing read-only and build/test commands with deploys in `ask` and key files, force pushes, pushes to the default branch and `gh pr merge --auto`/`--admin` denied, every `Bash(...)` rule with a `PowerShell(...)` twin, `"attribution": {"commit": "", "pr": "", "sessionUrl": false}`, `CLAUDE.local.md` and `.claude/settings.local.json` in `.gitignore`, and hook scripts LF in `.gitattributes`. | Create all of it in the first `ada-os` branch, before any delegated task runs. Multi-step work goes through the user-level `/autonomous-delivery` skill and its `delivery-reviewer` agent; `AGENTS.md` supplies the house rules they follow. | M0 item 11 |
| 2 | No CI runner exists for `ada-os`. The owner's repositories run CI only on the local runners. A new private repository needs a `lab-ada-os` scale set (`REPOS` in `config.mk` and `ci_runner_repos` in tofu under `E:\__Workdir\personal\infra`, then `make runners`, from a worktree there) and the owner's step of adding the repository to the runners' GitHub App. Without it workflows are skipped; they must never fall back to GitHub-hosted runners. | Plan the infra change as its own slice and ask the owner to register the repository. Until then state that CI did not run and rely on local checks. A lab runner container sees 24 CPUs under a 3 GiB memory limit, so cap vitest `--maxWorkers`, Playwright workers and Go test parallelism. Pull requests test on Linux only; Windows jobs run on pushes to main and only after the owner installs the per-repo Windows runner. Electron and Playwright tests on Linux need a display (xvfb); confirm the runner image provides one before relying on it. | M0 item 12; M4 and M9 evidence |
| 3 | Reasoning effort for Codex is not what section 5 assumes. Harness forwards effort only to the API and AGY backends, so a Codex run uses the user's Codex configuration, currently `~/.codex/config.toml` with `model = "gpt-6.1-sol"` and `model_reasoning_effort = "high"`. The "Sol, medium" default is not in force until M1 adds the adapter mapping. | Record the effective effort, not the requested one, in every run's evidence until M1 lands. Do not change the owner's Codex configuration to compensate. | M0 item 13; M1 |
| 4 | The engine baseline is unmerged. `8d521f0` sits on the local branch `antigravity-cli`, two commits ahead of `origin/main`, never pushed or reviewed. Pinning it makes Ada depend on a revision that is not on main. | Owner decision: land `antigravity-cli` through a reviewed pull request first (preferred, so Ada pins a main commit), or record in `docs/implementation-status.md` that Ada pins a non-main revision and why. The M1 protocol work branches from whichever baseline is decided. | M0 item 14 |
| 5 | Host firewall and host-only network changes for VMware mode (section 4) are elevated system-settings changes. The owner's standing rule forbids the assistant from changing firewall rules or network profiles, even when asked. | The product may present the changes and apply them under the user's elevation. During M5 development and testing the agent hands the owner the exact commands and waits; it never applies them itself. Read-only checks (`Get-NetFirewallRule`, `Get-NetConnectionProfile`) come first. | M5 |
| 6 | Merge policy wording. This file requires explicit owner authorization for every merge. The owner's standing rule already allows merging through the pull request once CI on the owner's runners, the full local checks, an independent review and a security read all pass, with the merged head being the commit they ran on. | Keep this file's stricter rule for Ada until the owner says otherwise; it does not conflict with the standing rule. Never turn on auto-merge. | SDLC, section 5 |
| 7 | The model quota snapshot is already stale. The Claude weekly limit that blocked the Opus review reset at 13:00 Europe/Bucharest on the day of the attempt. | Recheck Claude and Codex availability at the start of each working session, as section 5 already requires, and record the result. | M0 item 8 |
| 8 | No time or effort estimate. M0-M9 cover a desktop application, a coordinator, a protocol change in Harness, a VM image factory, a guest service and packaging for two platforms. | Keep M0-M2 as the first increment and re-plan after it with measured task durations and quota use. Do not promise a v1 date before then. | Section 6 |

### Parallel workers (added 2026-10-11)

Current state. Sections 5 and 6 allow parallel work for building Ada (at most two implementation workers and one reviewer; independent tasks in parallel once shared contracts are fixed; coordinator, protocol and data-model changes sequential) but say to use single Harness runs initially and never say which slices may run side by side or through what mechanism. Ada the product is single-task in v1: one agent task per workspace and one active VM (section 4). Harness already has the machinery: `hrn batch` runs a task list in parallel, each task in its own worktree and branch `hrn/<id>`, with gate commands, retries per backend and an automatic commit of what passed; `parallel` defaults to 4 (`internal/batch/batch.go:36-55`), and `hrn batch merge` merges passing branches into the current branch. The MCP interface exposes the same as `batch`, `batch_show` and `batch_merge`.

Policy for building Ada.

- M0 to M2 stay sequential, as single runs: they define the contracts everything else depends on.
- From M3, independent slices run through `hrn batch` with `parallel` set to 2 and one separate reviewer run, which is the section 5 cap. Candidates once M1 and M2 are merged: M3 import and snapshots, the M4 desktop shell, the M5 image factory and VMware spike, and M9 packaging scaffolding. Anything touching the coordinator schema, the IPC surface or the Harness protocol remains one task at a time.
- Each batch task gets the bounded packet of section 5, owns a disjoint set of files, and declares as its gate the commands the repository's check skill runs. Batch tasks run in `auto` mode, since nothing can answer prompts in a batch, so only backends with a verified sandbox profile take batch work: Codex with its workspace-write sandbox, Antigravity with `--sandbox`. Never `yolo`, never permission bypass.
- A task's branch `hrn/<id>` becomes a feature branch and pull request after independent review. `hrn batch merge` is not used to land work, and the batch's automatic commit is a checkpoint, not acceptance evidence. `hrn batch rm` removes finished worktrees.
- Limits that bound the count: subscription quotas (the Claude weekly limit was reached on 2026-10-11, and two workers spend it twice as fast), the owner's rule to keep runs small and gated until the Harness budget guard exists, the CI runner node taking five jobs at once (open the resulting pull requests one at a time rather than in a burst), and gate commands that build on every core, so more than two concurrent gates mostly compete for memory and disk.
- Record the batch id, task ids, backends, models and gate results in the evidence package exactly as section 7 asks for single runs.

Ada the product.

- V1 stays at one agent task per workspace and one active VM. The data model must not preclude more: `Task` and `Attempt` already carry workspace identity and an ownership generation, and the one-writer-per-workspace rule is what makes concurrent tasks across different workspaces safe. The scheduler therefore takes a configurable concurrency cap, default 1 in v1, so raising it to 2 later is a settings change plus the UI work to show several active tasks, not a schema change.
- Raising that default is a post-v1 decision for the owner and is not part of the v1 release gate in section 6.
