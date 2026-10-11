@AGENTS.md

Claude-specific: path-scoped rules are in `.claude/rules/`, `/check` runs what CI runs, the
post-edit hook (`repo-rules.sh`) keeps gofmt clean, bans Harness-internal imports and refuses
insecure Electron settings, and the pre-command hook (`push-guard.mjs`) refuses pushes that would
update main or a tag, force, delete or mirror. Your own notes go in `CLAUDE.local.md` (ignored).
Multi-step work: the user-level `/autonomous-delivery` skill and its `delivery-reviewer` agent
follow this file's rules.
