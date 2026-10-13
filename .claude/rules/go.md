---
paths:
  - "**/*.go"
---
# Go code and tests

- Write the test first and see it fail. Name it as a sentence about behaviour
  (`TestPinRefusesAShortRevision`), not `TestFoo`.
- Code that starts a process is tested by re-running the test binary as the child (`os.Args[0]`
  with `-test.run=^TestHelper...$` and an environment flag). Never depend on an installed `hrn`,
  claude, codex or VMware in a unit test; a fake engine is a helper process speaking the same
  protocol.
- SQLite tests open a database under `t.TempDir()`. Crash and disk-full behaviour is tested with a
  helper process that is killed, or a filesystem fault injected behind an interface, never by
  filling the real disk.
- Never import `github.com/cristiangirlea/harness/internal/...`. Talk to Harness through the pinned
  binary and its versioned protocol.
- On Windows the temp dir may have 8.3 short names: compare paths after `filepath.EvalSymlinks`.
- A process is identified by its PID together with its creation time, never by PID alone.
- Locally `go test -count=3 ./...` for anything concurrent; `-race` runs in CI (no cgo on the
  owner's PC).
