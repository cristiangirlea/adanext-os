#!/bin/sh
# After an edit: three things that must always hold in this repository.
# 1. gofmt prints nothing for the repository's own Go files.
# 2. No Go file imports Harness's private packages; Ada runs the pinned hrn binary instead.
# 3. No desktop source turns off Electron's isolation: nodeIntegration on, or
#    contextIsolation, sandbox or webSecurity off.
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
msg=""
add() { msg="${msg:+$msg
}$1"; }

go_files=$(git ls-files -co --exclude-standard -- '*.go' 2>/dev/null)
if [ -n "$go_files" ] && command -v gofmt >/dev/null 2>&1; then
  unformatted=$(printf '%s\n' "$go_files" | xargs gofmt -l 2>/dev/null)
  [ -n "$unformatted" ] && add "gofmt would change these files (run gofmt -w on them):
$unformatted"
fi
if [ -n "$go_files" ]; then
  internal=$(printf '%s\n' "$go_files" | xargs grep -ln 'cristiangirlea/harness/internal' 2>/dev/null)
  [ -n "$internal" ] && add "these files import Harness internals; use the pinned hrn exec protocol instead:
$internal"
fi

desktop_files=$(git ls-files -co --exclude-standard -- 'desktop/src/*' 'desktop/electron/*' 2>/dev/null)
if [ -n "$desktop_files" ]; then
  unsafe=$(printf '%s\n' "$desktop_files" | xargs grep -lE 'nodeIntegration:[[:space:]]*true|contextIsolation:[[:space:]]*false|sandbox:[[:space:]]*false|webSecurity:[[:space:]]*false' 2>/dev/null)
  [ -n "$unsafe" ] && add "these desktop files weaken Electron isolation (see .claude/rules/desktop.md):
$unsafe"
fi

if [ -n "$msg" ]; then
  printf '%s\n' "$msg" >&2
  exit 2
fi
exit 0
