#!/usr/bin/env bash
# M4.5's live demonstration: the real app, the release adad and the pinned engine, on the owner's
# plan. Never run by check.sh or CI. Needs ADANEXT_LIVE=1, ADANEXT_ENGINE (the hrn built by
# scripts/build-engine.sh) and ADANEXT_LIVE_SCRATCH, an absolute directory outside the home folder
# (its path shows on screen); ADANEXT_LIVE_BACKEND picks claude or codex (codex by default).
# `--check-inputs` validates these and exits before building (desktop/src/live.test.ts).
set -euo pipefail
cd "$(dirname "$0")/.."
if [ "${ADANEXT_LIVE:-}" != "1" ]; then
  echo "live-demo: refused: it runs on the owner's plan; set ADANEXT_LIVE=1"
  exit 2
fi
if [ -z "${ADANEXT_ENGINE:-}" ] || [ ! -f "$ADANEXT_ENGINE" ]; then
  echo "live-demo: ADANEXT_ENGINE must name the engine built by scripts/build-engine.sh"
  exit 2
fi
if [ -z "${ADANEXT_LIVE_SCRATCH:-}" ]; then
  echo "live-demo: ADANEXT_LIVE_SCRATCH must name a directory outside the home folder"
  exit 2
fi
# One slash form, lower case, Windows form under Git Bash: how the paths are compared.
norm() {
  local p="$1"
  case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) p=$(cygpath -m "$p");; esac
  p=$(printf '%s' "$p" | tr '\\' /)
  p="${p,,}"
  printf '%s' "${p%/}"
}
scratch=$(norm "$ADANEXT_LIVE_SCRATCH")
case "$scratch" in /*|[a-z]:/*) ;; *) echo "live-demo: ADANEXT_LIVE_SCRATCH must be absolute"; exit 2;; esac
for home in "${HOME:-}" "${USERPROFILE:-}"; do
  [ -n "$home" ] || continue
  h=$(norm "$home")
  if [ "$scratch" = "$h" ] || [ "${scratch#"$h"/}" != "$scratch" ]; then
    echo "live-demo: ADANEXT_LIVE_SCRATCH lies in the home folder, whose path would show on screen"
    exit 2
  fi
done
# --check-inputs stops here, so the refusals above can be tested without building anything.
if [ "${1:-}" = "--check-inputs" ]; then
  echo "live-demo: inputs accepted"
  exit 0
fi
ADANEXT_ENGINE="$(cd "$(dirname "$ADANEXT_ENGINE")" && pwd -P)/$(basename "$ADANEXT_ENGINE")"
export GOFLAGS="${GOFLAGS:-} -p=4"
mkdir -p .hrn
root=$(pwd -P)
build=$(mktemp -d "$root/.hrn/live-bin.XXXXXX")
case "$build" in "$root"/.hrn/live-bin.*) ;; *) echo "unexpected build directory"; exit 1;; esac
trap 'rm -rf -- "$build"' EXIT
suffix=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) suffix=".exe";; esac
# Release builds: adad trusts only the engine hash embedded in it.
go build -o "$build/adad$suffix" ./cmd/adad
go build -o "$build/ada-live$suffix" ./cmd/ada-live
(cd ui && npm run build)
(cd desktop && node node_modules/electron/install.js && npm run build)
if [ -n "$suffix" ]; then
  ADANEXT_LIVE_BIN=$(cygpath -m "$build")
  ADANEXT_ENGINE=$(cygpath -m "$ADANEXT_ENGINE")
else
  ADANEXT_LIVE_BIN="$build"
fi
export ADANEXT_LIVE_BIN ADANEXT_ENGINE
cd desktop
if [ "$(uname -s)" = "Linux" ] && [ -z "${DISPLAY:-}" ]; then
  xvfb-run -a npx playwright test -c playwright.live.config.ts "$@"
else
  npx playwright test -c playwright.live.config.ts "$@"
fi
