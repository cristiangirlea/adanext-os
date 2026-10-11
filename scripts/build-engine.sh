#!/usr/bin/env bash
# Build the Harness engine Ada runs, from a clean Harness checkout at the revision pinned in
# engine/engine.json, into .engine/. Prints the binary's SHA-256 for run evidence.
#
#   bash scripts/build-engine.sh <path-to-harness-checkout>
#
# The checkout must be at exactly the pinned commit with no modified or untracked files, so the
# binary is what the revision says. -trimpath keeps local paths out of the binary. Go's own VCS
# stamp is not relied on: it was absent from builds in a linked worktree on the owner's PC, so this
# script's revision check and the printed hash are the evidence.
set -euo pipefail
repo_root=$(cd "$(dirname "$0")/.." && pwd)

src=${1:?usage: build-engine.sh <path-to-harness-checkout>}
pin="$repo_root/engine/engine.json"
want=$(sed -n 's/.*"revision"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{40\}\)".*/\1/p' "$pin")
if [ -z "$want" ]; then
  echo "no 40-character revision in $pin" >&2
  exit 1
fi

have=$(git -C "$src" rev-parse HEAD)
if [ "$have" != "$want" ]; then
  echo "$src is at $have; engine/engine.json pins $want" >&2
  exit 1
fi
if [ -n "$(git -C "$src" status --porcelain --untracked-files=normal)" ]; then
  echo "$src has modified or untracked files; build the engine from a clean checkout" >&2
  exit 1
fi

exe=hrn
case "$(go env GOOS)" in windows) exe=hrn.exe ;; esac
mkdir -p "$repo_root/.engine"
out="$repo_root/.engine/$exe"
(cd "$src" && go build -trimpath -o "$out" ./cmd/hrn)

echo "engine: $out"
echo "revision: $want"
echo "go: $(go env GOVERSION) $(go env GOOS)/$(go env GOARCH)"
if command -v sha256sum >/dev/null 2>&1; then
  echo "sha256: $(sha256sum "$out" | cut -d' ' -f1)"
else
  echo "sha256: $(shasum -a 256 "$out" | cut -d' ' -f1)"
fi
