---
name: check
description: Run what CI runs on Ada OS (vet, build, tests, race where possible, gofmt, import ban, scripts, desktop lockfile) before a pull request
---
Run from the repository root and report each step's result in one line. On a failure, show the
error text and fix it before re-running.

```bash
bash scripts/check.sh
```

`scripts/check.sh` is the single list of checks; CI runs the same file, so a step added there
reaches both. It stops at the first failure.

`go test -race` needs cgo and a C compiler. On the owner's Windows PC the script reports it as
skipped and CI runs it on Linux; say so in the pull request rather than claiming it ran. The
desktop step installs from the lockfile with `--ignore-scripts`, so it needs network access to the
npm registry the first time.

Nothing here may call a model: no `hrn run`, no live provider. This skill does not merge: merging
is through the pull request, under AGENTS.md's conditions.
