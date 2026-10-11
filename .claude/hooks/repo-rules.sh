#!/bin/sh
# Rules that must always hold. Claude runs this after every edit, and scripts/check.sh runs it in
# CI, so both enforce the same list:
# 1. gofmt prints nothing for the repository's own Go files.
# 2. No Go file imports Harness's private packages; Ada runs the pinned hrn binary instead.
# 3. Nothing under desktop/ turns off Electron's isolation.
# Exit 0 when all hold; 2 when one is broken (a blocking result for the hook); 1 when a check could
# not run. A file that could not be listed or read never passes: each file is read on its own, so a
# read error is told apart from "no match" (git grep folds unreadable files into "no match").
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
list=$(mktemp) || cannot "no temporary file"
trap 'rm -f "$list"' EXIT

# files <pathspec>...: the repository's tracked and untracked (not ignored) files, NUL-separated,
# into $list. A shell variable cannot hold NUL, so the list goes through a file and git's own exit
# status is checked.
files() {
  git ls-files -z -co --exclude-standard -- "$@" >"$list" || cannot "git ls-files failed for: $*"
}

# each <shell snippet using $f>: runs the snippet for every listed file that exists (a deleted file
# can still be listed). The snippet exits 0 when the file is fine, 1 when it matches, anything else
# when the file could not be inspected; a match prints the file name.
each() {
  xargs -0 -r sh -c '
    snippet=$1; shift
    for f do
      [ -e "$f" ] || continue
      ( eval "$snippet" )
      rc=$?
      [ "$rc" -eq 0 ] && continue
      [ "$rc" -eq 1 ] && { printf "%s\n" "$f"; continue; }
      exit 255
    done' sh "$1" <"$list"
}

command -v gofmt >/dev/null 2>&1 || cannot "gofmt is not on PATH"

files '*.go'
out=$(each 'u=$(gofmt -l -- "$f") || exit 2; [ -z "$u" ] || exit 1') || cannot "gofmt could not read every Go file"
[ -n "$out" ] && add "gofmt would change these files (run gofmt -w on them):
$out"

# grep -q exits 0 on a match, 1 on none, 2 on an error; the snippet turns that into the convention
# above.
match() { # match <extended regex>: a snippet that "matches" (exit 1) when the file contains it
  printf 'grep -q -E -e %s -- "$f"; case $? in 0) exit 1 ;; 1) exit 0 ;; *) exit 2 ;; esac' "'$1'"
}

files '*.go'
out=$(each "$(match 'cristiangirlea/harness/internal')") || cannot "a Go file could not be read"
[ -n "$out" ] && add "these files import Harness internals; use the pinned hrn exec protocol instead:
$out"

files 'desktop/' ':(exclude)desktop/package-lock.json'
out=$(each "$(match 'nodeIntegration[[:space:]]*:[[:space:]]*true|contextIsolation[[:space:]]*:[[:space:]]*false|sandbox[[:space:]]*:[[:space:]]*false|webSecurity[[:space:]]*:[[:space:]]*false|allowRunningInsecureContent[[:space:]]*:[[:space:]]*true')") ||
  cannot "a desktop file could not be read"
[ -n "$out" ] && add "these desktop files weaken Electron isolation (see .claude/rules/desktop.md):
$out"

if [ -n "$msg" ]; then
  printf '%s\n' "$msg" >&2
  exit 2
fi
exit 0
