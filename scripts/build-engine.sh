#!/usr/bin/env bash
# Build the Harness engine Ada runs, from a clean Harness checkout at the revision pinned in
# engine/engine.json, with the pinned Go toolchain, into .engine/. Prints the binary's SHA-256 for
# run evidence.
#
#   bash scripts/build-engine.sh <path-to-harness-checkout>
#
# The checkout must be at exactly the pinned commit with no modified or untracked files, so the
# binary is what the revision says. -trimpath keeps local paths out of the binary. Go's own VCS
# stamp is not relied on: it was absent from builds in a linked worktree on the owner's PC, so this
# script's revision check and the printed hash are the evidence. The pin file's full validation is
# internal/engine's LoadPin, which CI runs against this same file (TestTheRepositoryPinLoads).
#
# Every value is captured in an assignment, so under `set -e` a failing git, go or hash command
# stops the script instead of leaving an empty value that looks like success.
set -euo pipefail
repo_root=$(cd "$(dirname "$0")/.." && pwd)

die() {
  echo "build-engine: $*" >&2
  exit 1
}

src=${1:?usage: build-engine.sh <path-to-harness-checkout>}
pin="$repo_root/engine/engine.json"
want=$(sed -n 's/.*"revision"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{40\}\)".*/\1/p' "$pin")
want_go=$(sed -n 's/.*"go"[[:space:]]*:[[:space:]]*"\(go[0-9][0-9.]*\)".*/\1/p' "$pin")
[ -n "$want" ] || die "no 40-character revision in $pin"
[ -n "$want_go" ] || die "no go toolchain in $pin"

have=$(git -C "$src" rev-parse --verify HEAD)
[ "$have" = "$want" ] || die "$src is at $have; engine/engine.json pins $want"
dirty=$(git -C "$src" status --porcelain --untracked-files=normal)
[ -z "$dirty" ] || die "$src has modified or untracked files; build the engine from a clean checkout"

# The toolchain is chosen per directory (go.mod, go.work, GOTOOLCHAIN), so pin it for the build and
# ask which one is in effect from inside the Harness checkout, where the build runs.
export GOTOOLCHAIN="$want_go"
have_go=$(cd "$src" && go env GOVERSION)
[ "$have_go" = "$want_go" ] || die "the build in $src would use $have_go; engine/engine.json pins $want_go"

goos=$(cd "$src" && go env GOOS)
goarch=$(cd "$src" && go env GOARCH)
exe=hrn
[ "$goos" = windows ] && exe=hrn.exe
mkdir -p "$repo_root/.engine"
out="$repo_root/.engine/$exe"
(cd "$src" && go build -trimpath -o "$out" ./cmd/hrn)

if command -v sha256sum >/dev/null 2>&1; then
  sum=$(sha256sum "$out")
else
  sum=$(shasum -a 256 "$out")
fi
sum=${sum%% *}
[[ "$sum" =~ ^[0-9a-f]{64}$ ]] || die "could not hash $out"

echo "engine: $out"
echo "revision: $want"
echo "go: $have_go $goos/$goarch"
echo "sha256: $sum"
