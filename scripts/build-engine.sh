#!/usr/bin/env bash
# Build the Harness engine Ada runs, from a clean Harness checkout at the revision pinned in
# engine/engine.json, with the pinned Go toolchain, into .engine/. Prints the binary's SHA-256 and
# compares it with the one engine/engine.json records for the target, which adad embeds.
#
#   bash scripts/build-engine.sh <path-to-harness-checkout>
#   GOOS=linux GOARCH=amd64 bash scripts/build-engine.sh <path>   # a cross build, into .engine/linux-amd64/
#
# The checkout must be at exactly the pinned commit with no modified or untracked files, so the
# binary is what the revision says. The build is reproducible: no cgo, -trimpath (no local paths),
# -buildvcs=false (no VCS stamp, which differs between a clone and a linked worktree), an empty
# build id, and no user go env file, GOFLAGS or GOAMD64-style tuning, so the same revision and
# toolchain give the same bytes on any host (decisions log, 2026-10-25). The pin file's full
# validation is internal/engine's LoadPin, which CI runs against this same file
# (TestTheRepositoryPinLoads). Exits 1 when the build differs from a recorded hash.
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
# ask which one is in effect from inside the Harness checkout, where the build runs. Nothing from the
# user's go env file or tuning variables reaches the build.
export GOTOOLCHAIN="$want_go" GOENV=off GOFLAGS= CGO_ENABLED=0 GOWORK=off
unset GOAMD64 GOARM GOARM64 GO386 GOMIPS GOMIPS64 GOPPC64 GORISCV64 GOWASM GOEXPERIMENT GOFIPS140
have_go=$(cd "$src" && go env GOVERSION)
[ "$have_go" = "$want_go" ] || die "the build in $src would use $have_go; engine/engine.json pins $want_go"

goos=$(cd "$src" && go env GOOS)
goarch=$(cd "$src" && go env GOARCH)
hostos=$(cd "$src" && go env GOHOSTOS)
hostarch=$(cd "$src" && go env GOHOSTARCH)
exe=hrn
[ "$goos" = windows ] && exe=hrn.exe
dir="$repo_root/.engine"
[ "$goos/$goarch" = "$hostos/$hostarch" ] || dir="$repo_root/.engine/$goos-$goarch"
mkdir -p "$dir"
out="$dir/$exe"
(cd "$src" && go build -trimpath -buildvcs=false -ldflags=-buildid= -o "$out" ./cmd/hrn)

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
echo "pin entry: \"$goos/$goarch\": \"$sum\""

recorded=$(sed -n 's|.*"'"$goos/$goarch"'"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{64\}\)".*|\1|p' "$pin")
if [ -z "$recorded" ]; then
  echo "pin: no hash recorded for $goos/$goarch"
elif [ "$recorded" = "$sum" ]; then
  echo "pin: matches"
else
  die "the build's sha256 $sum differs from engine/engine.json's $recorded for $goos/$goarch"
fi
