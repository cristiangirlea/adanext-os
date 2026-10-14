#!/usr/bin/env bash
# Everything CI runs, in order; the /check skill runs this file too. Stops at the first failure.
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n== %s\n' "$*"; }

step "go vet"
go vet ./...

step "go module tidy and verify"
go mod tidy -diff
go mod verify

step "go build"
go build ./...

step "go test"
go test -count=1 ./...

step "go test -race"
# Captured in assignments so a failing go env stops the script instead of looking like "no cgo".
cgo=$(go env CGO_ENABLED)
cc=$(go env CC)
if [ "$cgo" = "1" ] && command -v "$cc" >/dev/null 2>&1; then
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

step ".claude/settings.json matches scripts/gen-settings.py"
py=""
for candidate in python3 python; do
  if command -v "$candidate" >/dev/null 2>&1 && "$candidate" -c 'import sys' >/dev/null 2>&1; then
    py=$candidate
    break
  fi
done
[ -n "$py" ] || { echo "python is needed to check .claude/settings.json against its generator"; exit 1; }
generated=$(mktemp)
"$py" -I scripts/gen-settings.py "$generated" >/dev/null
if ! cmp -s "$generated" .claude/settings.json; then
  rm -f "$generated"
  echo ".claude/settings.json differs from what scripts/gen-settings.py writes: edit the lists there and run it"
  exit 1
fi
rm -f "$generated"

step "desktop lockfile installs"
(cd desktop && npm ci --ignore-scripts --no-audit --no-fund)

printf '\nall checks passed\n'
