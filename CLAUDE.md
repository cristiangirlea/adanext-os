@AGENTS.md

Claude-specific: path-scoped rules are in `.claude/rules/`, `/check` runs what CI runs, and the
post-edit hook keeps gofmt clean, bans Harness-internal imports, and refuses insecure Electron
window settings. Your own notes go in `CLAUDE.local.md` (ignored).
Multi-step work: the user-level `/autonomous-delivery` skill and its `delivery-reviewer` agent
follow this file's rules.
