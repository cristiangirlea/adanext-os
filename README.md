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

Early. Milestone M0 (baseline) is in progress. Nothing here is usable yet.

- `docs/plan.md`: the accepted plan.
- `docs/implementation-status.md`: what is done, with evidence, and the decisions log.
- `docs/backlog.md`: the slices, in order.

## Building

```bash
bash scripts/check.sh
```

Needs Go 1.26.4 and Node 22.16.0. Agents and contributors: read `AGENTS.md` first.
