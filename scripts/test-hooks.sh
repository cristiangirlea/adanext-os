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

allow() { expect 0 "$(guard_exit "$g" "$1" "${2:-Bash}")" "guard allows (${2:-Bash}): $1"; }
refuse() { expect 2 "$(guard_exit "$g" "$1" "${2:-Bash}")" "guard refuses (${2:-Bash}): $1"; }

# Commands that do not push, or one plain push of a feature branch.
allow 'git status'
allow 'echo main && git log'
allow 'git push -u origin feature'
allow 'git push --set-upstream origin feature'
allow 'git push origin feature:feature'
allow 'git push -n origin feature'
allow 'git push -u origin feature' PowerShell
allow 'git commit -m "fix: git push docs"'
allow "git commit -m 'push the fix'"
allow 'git log --grep "push"' PowerShell

# Anything else that mentions git and push: the push must run alone, as one plain command.
refuse 'git push'
refuse 'git push origin'
refuse 'git push origin main'
refuse 'git push origin HEAD'
refuse 'git push origin HEAD:main'
refuse 'git push origin HEAD:refs/heads/main'
refuse 'git push origin feature:main'
refuse 'git push origin feature:other'
refuse 'git push origin :'
refuse "git push origin 'refs/heads/*:refs/heads/*'"
refuse 'git push origin :feature'
refuse 'git push -u origin main --dry-run'
refuse 'git push -f'
refuse 'git push -uf origin feature'
refuse 'git push --force-with-lease origin feature'
refuse 'git push origin +feature'
refuse 'git push --tags'
refuse 'git push --all origin'
refuse 'git push origin refs/tags/v1'
refuse 'git push --delete origin feature'
refuse 'git push --mirror'
refuse 'git push origin v1'
refuse 'git push origin both'
refuse 'git push origin no-such-branch'
refuse 'git push -u origin feature 2>&1'
refuse "git commit -m 'msg' && git push -u origin feature"
refuse 'cd x && git push origin main'
refuse 'cd ../other && git push origin feature'
refuse 'git -C . push origin feature'
refuse 'git -C "some dir" push origin feature'
refuse 'git --git-dir=.git push origin feature'
refuse 'git -c remote.origin.push=refs/heads/feature:refs/heads/main push origin feature'
refuse 'GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=push.followTags GIT_CONFIG_VALUE_0=true git push origin feature'
refuse 'git.exe push origin main; echo done'
refuse 'sh -c "git push -u origin feature"'
refuse 'sh -c "git push origin main"'
refuse "bash -lc 'git push origin main'"
refuse 'echo "a;b" && git push origin main'
refuse 'git push origin "main'
refuse $'git \\\npush origin main'
refuse $'git push origin \\\nmain'
refuse $'git `\npush origin main' PowerShell
refuse '$env:git_config_count=1; $env:git_config_key_0="push.followTags"; $env:git_config_value_0="true"; git push origin feature' PowerShell
refuse 'pwsh -Command "git push origin main"' PowerShell
refuse 'git push origin $(echo main)'
refuse 'git push origin `echo main`'

# Other ways to push: plumbing, subtree, and aliases (which git ignores for built-in names).
refuse 'git subtree push --prefix=lib origin main'
refuse 'git send-pack origin main'
refuse 'git http-push origin main'
refuse 'git remote set-url --push origin x'
git -C "$g" config alias.p push
git -C "$g" config alias.sh '!git push origin main'
git -C "$g" config alias.st status
refuse 'git p origin main'
refuse 'git add . && git p origin main'
refuse 'git sh'
allow 'git st'
allow 'git fetch origin'
git -C "$g" config --unset alias.p
git -C "$g" config --unset alias.sh
git -C "$g" config --unset alias.st

git -C "$g" config remote.origin.push refs/heads/feature:refs/heads/main
refuse 'git push -u origin feature'
allow 'git push -u origin feature:feature'
git -C "$g" config --unset remote.origin.push

git -C "$g" config push.followTags true
refuse 'git push -u origin feature'
git -C "$g" config --unset push.followTags

if [ "$failures" -ne 0 ]; then
  echo "$failures hook test(s) failed"
  exit 1
fi
echo "all hook tests passed"
