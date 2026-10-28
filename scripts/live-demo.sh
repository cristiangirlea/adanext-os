#!/usr/bin/env bash
# M4.5's live demonstration: the real app, the release adad and the pinned engine, on the owner's
# plan. Never run by check.sh or CI. Needs ADANEXT_LIVE=1 and ADANEXT_ENGINE (the hrn built by
# scripts/build-engine.sh); ADANEXT_LIVE_BACKEND picks claude or codex (codex by default).
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
