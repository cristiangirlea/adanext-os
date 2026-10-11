#!/usr/bin/env bash
# Everything CI runs, in order; the /check skill runs this file too. Stops at the first failure.
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n== %s\n' "$*"; }

step "go vet"
go vet ./...

step "go build"
go build ./...

step "go test"
go test -count=1 ./...

step "go test -race"
if [ "$(go env CGO_ENABLED)" = "1" ] && command -v "$(go env CC)" >/dev/null 2>&1; then
  go test -race -count=1 ./...
else
  echo "skipped here: the race detector needs cgo and a C compiler; CI runs it on Linux"
fi

step "repo rules (gofmt, no Harness internal imports, Electron isolation)"
# The same script Claude's post-edit hook runs. It exits non-zero when a rule is broken or a file
# could not be inspected, and set -e stops here either way.
sh .claude/hooks/repo-rules.sh

step "scripts parse"
for f in scripts/*.sh .claude/hooks/*.sh; do
  bash -n "$f"
done
node --check .claude/hooks/push-guard.mjs

step "hook tests"
bash scripts/test-hooks.sh

step "desktop lockfile installs"
(cd desktop && npm ci --ignore-scripts --no-audit --no-fund)

printf '\nall checks passed\n'
