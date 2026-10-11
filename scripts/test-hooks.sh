#!/usr/bin/env bash
# Tests for the two Claude hooks, run by scripts/check.sh: repo-rules.sh against throwaway
# repositories, and push-guard.mjs against command lines that must or must not be refused.
set -euo pipefail
repo_root=$(cd "$(dirname "$0")/.." && pwd)
rules="$repo_root/.claude/hooks/repo-rules.sh"
guard="$repo_root/.claude/hooks/push-guard.mjs"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failures=0

expect() { # expect <wanted exit> <actual exit> <label>
  if [ "$1" != "$2" ]; then
    echo "FAIL: $3 (exit $2, want $1)"
    failures=$((failures + 1))
  else
    echo "ok:   $3"
  fi
}

new_repo() { # new_repo <dir>: an empty repository on main with one commit
  git init -q -b main "$1"
  git -C "$1" -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m init
}

rules_exit() { # rules_exit <repo>
  local rc=0
  CLAUDE_PROJECT_DIR="$1" sh "$rules" >/dev/null 2>&1 || rc=$?
  echo "$rc"
}

# --- repo-rules.sh
r="$tmp/rules"
new_repo "$r"
printf 'package ok\n' > "$r/ok.go"
expect 0 "$(rules_exit "$r")" "rules: a clean repository passes"

mkdir -p "$r/internal/with space"
printf 'package x\n\nimport _ "github.com/cristiangirlea/harness/internal/agent"\n' > "$r/internal/with space/x.go"
expect 2 "$(rules_exit "$r")" "rules: a Harness internal import in a path with a space is refused"
rm -rf "$r/internal"

mkdir -p "$r/desktop/electron"
printf 'new BrowserWindow({ webPreferences: { nodeIntegration: true } })\n' > "$r/desktop/electron/main window.ts"
expect 2 "$(rules_exit "$r")" "rules: nodeIntegration on in a desktop file with a space is refused"
rm -rf "$r/desktop"

mkdir -p "$r/desktop/preview"
printf 'const opts = { contextIsolation : false }\n' > "$r/desktop/preview/opts.ts"
expect 2 "$(rules_exit "$r")" "rules: isolation off anywhere under desktop/ is refused"
rm -rf "$r/desktop"

printf 'package bad\nfunc  F( ) {}\n' > "$r/bad.go"
expect 2 "$(rules_exit "$r")" "rules: an unformatted Go file is refused"
rm -f "$r/bad.go"

printf 'package broken\nfunc {\n' > "$r/broken.go"
expect 1 "$(rules_exit "$r")" "rules: a Go file gofmt cannot parse is an error, not a pass"
rm -f "$r/broken.go"

mkdir -p "$tmp/not-a-repo"
expect 1 "$(rules_exit "$tmp/not-a-repo")" "rules: outside a repository the check fails instead of passing"

# --- push-guard.mjs
g="$tmp/guard"
new_repo "$g"
git -C "$g" switch -q -c feature

guard_exit() { # guard_exit <cwd> <command>
  local rc=0
  local json
  json=$(node -e 'process.stdout.write(JSON.stringify({cwd: process.argv[1], tool_input: {command: process.argv[2]}}))' "$1" "$2")
  printf '%s' "$json" | node "$guard" >/dev/null 2>&1 || rc=$?
  echo "$rc"
}

for cmd in \
  "git status" \
  "git push -u origin feature" \
  "git push origin feature:feature" \
  "git push" \
  "echo main && git log"; do
  expect 0 "$(guard_exit "$g" "$cmd")" "guard allows on a feature branch: $cmd"
done

for cmd in \
  "git push origin main" \
  "git push origin HEAD:main" \
  "git push origin HEAD:refs/heads/main" \
  "git push -u origin main --dry-run" \
  "git push -f" \
  "git push --force-with-lease origin feature" \
  "git push origin +feature" \
  "git push --tags" \
  "git push origin refs/tags/v1" \
  "git push --delete origin feature" \
  "git push --mirror" \
  "cd x && git push origin main" \
  "git -C . push origin main" \
  "git.exe push origin main; echo done"; do
  expect 2 "$(guard_exit "$g" "$cmd")" "guard refuses: $cmd"
done

git -C "$g" switch -q main
expect 2 "$(guard_exit "$g" "git push")" "guard refuses a bare push while main is checked out"
expect 2 "$(guard_exit "$g" "git push -u origin HEAD")" "guard refuses pushing HEAD while main is checked out"

if [ "$failures" -ne 0 ]; then
  echo "$failures hook test(s) failed"
  exit 1
fi
echo "all hook tests passed"
