#!/usr/bin/env bash
# Real Electron -> real coordinator -> test-only protocol peer. No model calls.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOFLAGS="${GOFLAGS:-} -p=4"
if [ "${ADANEXT_SKIP_E2E:-}" = "1" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "ADANEXT_SKIP_E2E=1 is only for local runs; CI must run e2e"
    exit 1
  fi
  echo "e2e explicitly skipped: ADANEXT_SKIP_E2E=1"
  exit 0
fi
(cd desktop && node node_modules/electron/install.js)
mkdir -p .hrn
root=$(pwd -P)
build=$(mktemp -d "$root/.hrn/e2e-bin.XXXXXX")
# Only this mktemp directory inside this checkout can be removed by the trap.
case "$build" in "$root"/.hrn/e2e-bin.*) ;; *) echo "unexpected e2e build directory"; exit 1;; esac
trap 'rm -rf -- "$build"' EXIT
suffix=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) suffix=".exe";; esac
go build -o "$build/adad$suffix" ./cmd/adad
go build -tags=fakeengine -o "$build/fakeengine$suffix" ./internal/executor/testdata/fakeengine
go build -tags=fakeengine -o "$build/slowengine$suffix" ./desktop/e2e/slowengine
go build -tags=fakeengine -o "$build/testseed$suffix" ./internal/store/testseed
if [ -n "$suffix" ]; then
  export ADANEXT_E2E_BIN
  ADANEXT_E2E_BIN=$(cygpath -m "$build")
else
  export ADANEXT_E2E_BIN="$build"
fi
cd desktop
if [ "$(uname -s)" = "Linux" ] && [ -z "${DISPLAY:-}" ]; then
  xvfb-run -a npx playwright test --workers=1 "$@"
else
  npx playwright test --workers=1 "$@"
fi
