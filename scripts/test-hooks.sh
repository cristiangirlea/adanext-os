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

printf 'package gone\n' > "$r/gone.go"
git -C "$r" add gone.go
rm -f "$r/gone.go"
expect 0 "$(rules_exit "$r")" "rules: a tracked file deleted from disk is not an inspection error"
git -C "$r" rm -q --cached gone.go

mkdir -p "$r/desktop/src"
printf 'const ok = { contextIsolation: true }\n' > "$r/desktop/src/locked.ts"
chmod 000 "$r/desktop/src/locked.ts"
if cat "$r/desktop/src/locked.ts" >/dev/null 2>&1; then
  echo "skip: rules: an unreadable desktop file is an error (chmod cannot remove read access here)"
else
  expect 1 "$(rules_exit "$r")" "rules: an unreadable desktop file is an error, not a pass"
fi
chmod 644 "$r/desktop/src/locked.ts"
rm -rf "$r/desktop"

mkdir -p "$r/desktop/hidden"
printf 'const ok = { contextIsolation: true }\n' > "$r/desktop/hidden/inside.ts"
# Tracked, so git lists it from the index even when it cannot read the directory.
git -C "$r" add desktop/hidden/inside.ts
chmod 000 "$r/desktop/hidden"
if [ -e "$r/desktop/hidden/inside.ts" ]; then
  echo "skip: rules: a file behind a directory without search permission is an error (chmod cannot remove it here)"
else
  expect 1 "$(rules_exit "$r")" "rules: a file behind a directory without search permission is an error, not a deletion"
fi
chmod 755 "$r/desktop/hidden"
git -C "$r" rm -q --cached desktop/hidden/inside.ts
rm -rf "$r/desktop"

# --- push-guard.mjs
# The fixture's path has a space, so quoting around -C is exercised.
g="$tmp/guard repo"
new_repo "$g"
git -C "$g" switch -q -c feature
git -C "$g" branch -q both
git -C "$g" tag both
git -C "$g" tag v1

guard_exit() { # guard_exit <cwd> <command> [tool]
  local rc=0
  local json
  json=$(node -e 'process.stdout.write(JSON.stringify({cwd: process.argv[1], tool_name: process.argv[3], tool_input: {command: process.argv[2]}}))' "$1" "$2" "${3:-Bash}")
  printf '%s' "$json" | node "$guard" >/dev/null 2>&1 || rc=$?
  echo "$rc"
}

for cmd in \
  "git status" \
  "git push -u origin feature" \
  "git push --set-upstream origin feature" \
  "git push origin feature:feature" \
  "git push -n origin feature" \
  "git push -u origin feature 2>&1" \
  "git commit -m 'msg' && git push -u origin feature" \
  "git commit -m \"fix: git push docs\"" \
  "sh -c \"git push -u origin feature\"" \
  "echo main && git log"; do
  expect 0 "$(guard_exit "$g" "$cmd")" "guard allows: $cmd"
done
# Inside a command string a path is not translated for Windows programs, so give git a native one.
gpath=$(cygpath -m "$g" 2>/dev/null || printf '%s' "$g")
expect 0 "$(guard_exit "$tmp" "git -C \"$gpath\" push -u origin feature")" "guard allows a quoted -C path with a space"
expect 0 "$(guard_exit "$g" 'git push -u origin feature; echo `"done`"' PowerShell)" "guard allows a PowerShell push with backtick escapes"

for cmd in \
  "git push" \
  "git push origin" \
  "git push origin main" \
  "git push origin HEAD" \
  "git push origin HEAD:main" \
  "git push origin HEAD:refs/heads/main" \
  "git push origin feature:main" \
  "git push origin feature:other" \
  "git push origin :" \
  "git push origin 'refs/heads/*:refs/heads/*'" \
  "git push origin :feature" \
  "git push -u origin main --dry-run" \
  "git push -f" \
  "git push -uf origin feature" \
  "git push --force-with-lease origin feature" \
  "git push origin +feature" \
  "git push --tags" \
  "git push --all origin" \
  "git push origin refs/tags/v1" \
  "git push --delete origin feature" \
  "git push --mirror" \
  "cd x && git push origin main" \
  "git -C . push origin main" \
  "git --git-dir=.git push origin feature" \
  "git.exe push origin main; echo done" \
  "git push origin v1" \
  "git push origin both" \
  "git push origin no-such-branch" \
  "git -c remote.origin.push=refs/heads/feature:refs/heads/main push origin feature" \
  "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=remote.origin.push GIT_CONFIG_VALUE_0=feature:main git push origin feature" \
  "sh -c \"git push origin main\"" \
  "bash -lc 'git push origin main'" \
  "echo \"a;b\" && git push origin main" \
  "git push origin \"main"; do
  expect 2 "$(guard_exit "$g" "$cmd")" "guard refuses: $cmd"
done
expect 2 "$(guard_exit "$tmp" "git -C \"$gpath\" push origin main")" "guard refuses main behind a quoted -C path with a space"
expect 2 "$(guard_exit "$g" "\$env:GIT_CONFIG_PARAMETERS=\"'remote.origin.push=feature:main'\"; git push origin feature" PowerShell)" "guard refuses a PowerShell config override in the environment"

git -C "$g" config remote.origin.push refs/heads/feature:refs/heads/main
expect 2 "$(guard_exit "$g" "git push -u origin feature")" "guard refuses a bare branch when remote.origin.push could redirect it"
expect 0 "$(guard_exit "$g" "git push -u origin feature:feature")" "guard allows an explicit destination, which overrides remote.origin.push"
git -C "$g" config --unset remote.origin.push

git -C "$g" config push.followTags true
expect 2 "$(guard_exit "$g" "git push -u origin feature")" "guard refuses while push.followTags would send tags too"
git -C "$g" config --unset push.followTags

if [ "$failures" -ne 0 ]; then
  echo "$failures hook test(s) failed"
  exit 1
fi
echo "all hook tests passed"
