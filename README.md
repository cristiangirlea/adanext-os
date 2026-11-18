# AdaNext OS

A workspace that remembers where a project was left, proposes the next bounded task, runs the
chosen coding agent on a feature branch, locally or in a VMware virtual machine, and brings back
verified changes for review. One product in three forms: an installed application for Windows 11
and Ubuntu 24.04, a web interface that the same coordinator serves to browsers on your own network,
and, later, a bootable system. It is distributed from https://adanext.si; nothing runs there.

The first complete workflow: open a project, review what was left unfinished, press Continue, let
the agent work on a feature branch, run the checks, inspect the diff and preview, then review and
integrate.

## Status

Milestones M1 (in the Harness repository) to M3 are merged, and so is the code of M4. M4 stays
in progress until its demonstrations are recorded (#124) and the Windows CI job on `main` passes
again (#127). M0 waits on two owner actions: branch protection for `main`, and which machine
details and local paths leave the docs.

With a development build, the Electron app in `desktop/` started from source, a person can import
a project, start a task on the claude or codex CLI through the pinned engine, follow its activity,
run the checks the task declares, read its changes and files, and review it: findings, refine or
accept. Agents run locally only; the VMware virtual machine is milestone M5 and not built yet.
Also not there yet: an installer or a release (M9), the web interface (M4b), and the later
milestones.

- `docs/plan.md`: the accepted plan.
- `docs/implementation-status.md`: what is done, with evidence, and the decisions log.
- `docs/backlog.md`: the slices, in order.

## Building

```bash
bash scripts/check.sh
```

Needs Go 1.26.4 and Node 22.16.0. Agents and contributors: read `AGENTS.md` first.
