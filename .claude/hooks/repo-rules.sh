#!/bin/sh
# Rules that must always hold. Claude runs this after every edit, and scripts/check.sh runs it in
# CI, so both enforce the same list:
# 1. gofmt prints nothing for the repository's own Go files.
# 2. No Go file imports Harness's private packages; Ada runs the pinned hrn binary instead.
# 3. Nothing under desktop/ turns off Electron's isolation.
# Exit 0 when all hold; 2 when one is broken (a blocking result for the hook); 1 when a check could
# not run. A file that could not be inspected never passes.
cd "${CLAUDE_PROJECT_DIR:-$(dirname "$0")/../..}" || exit 1
msg=""
add() {
  msg="${msg:+$msg
}$1"
}
cannot() {
  printf 'repo rules could not be checked: %s\n' "$1" >&2
  exit 1
}

# git grep reads tracked and untracked files (honouring .gitignore) whatever their names, and
# exits 0 on a match, 1 on none, and above 1 on an error.
scan() { # scan <what is wrong> <extended regex> <pathspec>...
  what=$1
  re=$2
  shift 2
  out=$(git grep --untracked -l -E -e "$re" -- "$@")
  rc=$?
  case $rc in
    0) add "$what:
$out" ;;
    1) ;;
    *) cannot "git grep exited $rc while looking for: $what" ;;
  esac
}

command -v gofmt >/dev/null 2>&1 || cannot "gofmt is not on PATH"
unformatted=$(git ls-files -z -co --exclude-standard -- '*.go' | xargs -0 -r gofmt -l) ||
  cannot "gofmt could not read every Go file"
if [ -n "$unformatted" ]; then
  add "gofmt would change these files (run gofmt -w on them):
$unformatted"
fi

scan "these files import Harness internals; use the pinned hrn exec protocol instead" \
  'cristiangirlea/harness/internal' '*.go'

scan "these desktop files weaken Electron isolation (see .claude/rules/desktop.md)" \
  'nodeIntegration[[:space:]]*:[[:space:]]*true|contextIsolation[[:space:]]*:[[:space:]]*false|sandbox[[:space:]]*:[[:space:]]*false|webSecurity[[:space:]]*:[[:space:]]*false|allowRunningInsecureContent[[:space:]]*:[[:space:]]*true' \
  'desktop/' ':(exclude)desktop/package-lock.json'

if [ -n "$msg" ]; then
  printf '%s\n' "$msg" >&2
  exit 2
fi
exit 0
