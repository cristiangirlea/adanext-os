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

step "gofmt"
unformatted=$(git ls-files -co --exclude-standard -- '*.go' | xargs -r gofmt -l)
if [ -n "$unformatted" ]; then
  printf 'gofmt would change:\n%s\n' "$unformatted"
  exit 1
fi

step "no Harness internal imports"
if git grep -n 'cristiangirlea/harness/internal' -- '*.go'; then
  echo "Ada runs the pinned hrn binary; it must not import Harness's private packages"
  exit 1
fi

step "scripts parse"
for f in scripts/*.sh .claude/hooks/*.sh; do
  bash -n "$f"
done

step "desktop lockfile installs"
(cd desktop && npm ci --ignore-scripts --no-audit --no-fund)

printf '\nall checks passed\n'
