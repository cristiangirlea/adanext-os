"""Write ada-os/.claude/settings.json with a PowerShell twin for every Bash rule."""
import json
import sys

ALLOW = [
    "go vet *", "go build *", "go test *", "go list *", "go doc *", "go version", "go env",
    "go mod tidy", "go mod verify", "go mod download",
    "gofmt -l *", "gofmt -d *", "gofmt -w *",
    "bash -n *", "bash scripts/check.sh", "bash scripts/check.sh *",
    "npm ci *", "npm run build", "npm run test", "npm run typecheck", "npm run lint",
    "npm ls *", "npm view *", "npm audit *",
    "git status *", "git diff *", "git log *", "git show *", "git grep *", "git blame *",
    "git worktree list *", "git branch", "git branch --show-current", "git branch --list *",
    "git branch -a", "git branch -r", "git branch -v", "git branch -vv", "git rev-parse *",
    "gh pr view *", "gh pr list *", "gh pr checks *", "gh pr diff *",
    "gh issue view *", "gh issue list *", "gh run view *", "gh run list *",
]
ASK = [
    "gh pr create *", "npm install *", "npm uninstall *", "npm update *",
    "go get *", "hrn run *", "hrn batch *", "hrn eval *",
    "bash scripts/build-engine.sh *",
]
DENY = [
    "gh release *", "gh repo delete *", "gh repo edit *",
    "git tag *", "git push --force *", "git push -f *", "git push --tags *",
    "git push *--force*", "git push * -f", "git push * -f *", "git push * +*",
    "git push * main", "git push * *:main", "git push *--tags*", "git push *refs/tags/*",
    "git push *--mirror*", "git push *--delete*", "git push *--follow-tags*",
    "git push -f", "git push --force", "git push *refs/heads/main*", "git push * main *",
    "git push *:refs/tags/*",
    "gh pr merge *--auto*", "gh pr merge *--admin*",
    "gh api *-X PUT*", "gh api *--method PUT*", "gh api *-XPUT*",
    "gh api *-X DELETE*", "gh api *--method DELETE*", "gh api *-XDELETE*",
    "gh api *--method=PUT*", "gh api *--method=DELETE*", "gh api *-X=PUT*", "gh api *-X=DELETE*",
    "go env -w*", "go env -u*",
    "npx electron-builder *--publish*", "npm publish *",
]
READ_DENY = [
    "Read(**/.env)", "Read(**/.env.*)", "Read(**/*.key)", "Read(**/*.pem)", "Read(**/*.pfx)",
    "Read(**/*.p12)", "Read(~/.claude/.credentials.json)", "Read(~/.codex/auth.json)",
    "Read(~/.ssh/**)",
]
TOOL_DENY = ["mcp__hrn__batch_merge"]


def twins(cmds):
    return [f"Bash({c})" for c in cmds] + [f"PowerShell({c})" for c in cmds]


settings = {
    "$schema": "https://json.schemastore.org/claude-code-settings.json",
    "attribution": {"commit": "", "pr": "", "sessionUrl": False},
    "permissions": {
        "allow": twins(ALLOW),
        "ask": twins(ASK),
        "deny": twins(DENY) + READ_DENY + TOOL_DENY,
    },
    "hooks": {
        "PreToolUse": [
            {
                "matcher": "Bash|PowerShell",
                "hooks": [
                    {
                        "type": "command",
                        "command": "node \"$CLAUDE_PROJECT_DIR\"/.claude/hooks/push-guard.mjs",
                        "timeout": 20,
                    }
                ],
            }
        ],
        "PostToolUse": [
            {
                "matcher": "Edit|Write|MultiEdit",
                "hooks": [
                    {
                        "type": "command",
                        "command": "sh \"$CLAUDE_PROJECT_DIR\"/.claude/hooks/repo-rules.sh",
                        "timeout": 20,
                    }
                ],
            }
        ]
    },
}

with open(sys.argv[1], "w", encoding="utf-8", newline="\n") as f:
    json.dump(settings, f, indent=2)
    f.write("\n")

bash = [r for r in settings["permissions"]["allow"] + settings["permissions"]["ask"] + settings["permissions"]["deny"] if r.startswith("Bash(")]
ps = {r for r in settings["permissions"]["allow"] + settings["permissions"]["ask"] + settings["permissions"]["deny"] if r.startswith("PowerShell(")}
missing = [b for b in bash if "PowerShell(" + b[5:] not in ps]
print(f"rules: allow={len(settings['permissions']['allow'])} ask={len(settings['permissions']['ask'])} deny={len(settings['permissions']['deny'])}; bash rules without a twin: {missing}")
