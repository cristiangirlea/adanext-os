// Package api exposes the coordinator's versioned HTTP interface.
//
// API notes, kept with the behaviour they describe (issue #46):
//
// Oversize stored events. An event is stored only after the executor has
// bounded its encoded form to eventBytes, so page never meets a larger row and
// no such row exists in any shipped database. Should a database from before
// the cap ever be opened, page fails at that cursor on every read (500 in the
// JSON form, a silent close for a stream that EventSource retries every 2 s);
// the remedy is a migration moving those rows to artifacts, or an error event
// at that cursor, not a larger page.
//
// Memory outside the budget. The budget counts a page as held: the reservation
// before the read, the encoded page while it is written. While a page is read
// and encoded, the stored payloads, their encodings and the page itself exist
// together, up to about three times a page per reader. That is transient and
// bounded by the readers the budget admits at once (32 at the default 32 MiB).
//
// Refused streams. A stream refused after its headers are decided (429
// too_many_streams, 503 shutting_down or, when no budget frees within the
// write deadline, 503 busy, both with Retry-After) is a JSON error with a
// non-200 status. EventSource treats any non-200 as a fatal error, never reads
// the body and ignores Retry-After; the browser adapter (M4b) must therefore
// handle a refused open itself, backing off before a new EventSource, and must
// not rely on the retry: line, which only an accepted stream carries.
//
// Event kinds. An attempt's events carry one of these kinds (the SSE event
// name and the JSON kind). From the engine, its protocol v1 event as sent:
// `text`, `thinking`, `tool_call`, `tool_result`, `info`, `error` and
// `session`; any other kind fails the attempt. `engine_notice` is a protocol
// error message the engine sent (fatal or not), `result` its result line, with
// a large final moved to an artifact. From the executor: `executor_error`, a
// cause of failure ({"error": ...}), one per failure the executor detects, plus
// one saying the engine was killed after the grace period when it was; after a
// fatal engine error, the `engine_notice` marked fatal is the cause and no
// `executor_error` repeats it; `info` also when a descendant kept stderr open
// after a clean exit; `recovery` ({"reason": ...}) when an attempt moves to
// recovery_required. From the store: `recovery` with
// old_generation and attempt_state for an attempt a restart abandoned, and
// `cancel` ({"reason": ...}) for a recorded cancel or stop.
//
// Old kind names. Databases written by M2.3b (PR #40, 2026-10-14, until PR #59
// on 2026-10-15) keep the names they were written with; they are documented
// here, not migrated. There `error` was also the executor's causes and the
// killed-after-grace marker (now `executor_error`) and the engine's protocol
// error messages (now `engine_notice`), and an engine event of any non-empty
// kind was stored as sent. An `error` event in such a database is told apart
// by its payload: an engine event or protocol message is a protocol line with
// "v" and "type", an executor cause is {"error": ...} alone. From PR #59 until
// 2026-10-19 one failure could record its cause twice, the second with
// "; engine exit: ..." appended; those rows stay as well.
//
// Preparing deadline. A launched engine has adad's -preparing-deadline
// (default 2 minutes) to say hello and accept the start; otherwise the attempt
// ends ended_failed with the cause "preparing deadline exceeded".
//
// Ends that look surprising. On Windows the engine runs in a kill-on-close Job
// Object, so a coordinator crash kills the engine and its descendants; the next
// start finds the attempt abandoned and moves it to recovery_required. An `ok`
// result whose engine then exits by itself ends ended_ok even when a
// descendant still held stdout and the post-exit check had to close it: the
// result was complete, and only the descendant's output after it is lost.
//
// Capabilities. GET /v1/capabilities returns the pinned engine's capability
// matrix (internal/engine's Capabilities, decisions log 2026-10-25) to any
// account of the coordinator, 403 otherwise: per backend and mode, each row's
// cell is enforced (by whom, and the test that shows it), requested (passed,
// not verified) or unsupported; checks are unsandboxed. "installed" is what
// this host has of the CLI, from the engine's hello and the CLI's own version
// command; a CLI-dependent cell reads unverified, with the table's status in
// "recorded", when the installed version is not the recorded one or no CLI was
// found. It holds no path and no secret.
//
// Unsupported options. A start the matrix refuses (an unknown backend, mode
// ask, an effort the backend cannot apply, max_turns on codex) is answered 409
// {"error":"unsupported_options","message":<reason>} before anything is
// recorded, prepared or cloned. A start may set max_turns (1 to 1000) and
// timeout (a Go duration up to 24h, stored normalised, e.g. "3m0s"); the
// engine receives each only when it is set.
//
// Provider configuration. An inspection's "findings" hold the non-blocking
// provider_configuration: work-tree paths under .claude/ or .codex/, or named
// .mcp.json. A snapshot records those its worktree will hold (tracked ones,
// and the untracked ones it selected) as provider_configuration {paths,count};
// a task shows its snapshot's, null when it has none or the snapshot predates
// migration 0008. A task's attempt shows provider_changes, what it added,
// altered or removed of them, recorded once the engine's whole tree has ended
// (null until then, and for an attempt without a workspace), and
// provider_flagged, set when that must be shown before review or refine, and
// for an attempt that ended with a workspace but no assessment.
//
// Checks. A task carries its declared checks (name, argv, workdir, timeout up
// to 30m, max_output_bytes up to 16 MiB, env keys; at most 16), set on
// POST /v1/tasks or PUT /v1/tasks/{id}/checks under If-Match while the task is
// in draft or ready. After an attempt ends ok in an imported
// workspace, Ada commits the agent's changes on ada/<task> and runs each check
// in a fresh detached worktree against the snapshot's baseline, moving the task
// running -> verifying -> awaiting_review; cancel during verifying kills the
// running check and records the rest as not run. GET /v1/tasks/{id}/checks and
// the task's attempt return the recorded check results, their baseline
// comparison and invalidated, set unless the result binds the current tip of
// ada/<task> (an unknown tip invalidates it); GET /v1/checks/{id}/log (or GET
// /v1/tasks/{task}/checks/{id}/log, only for that task's runs and the
// baselines they were compared with) streams a check or baseline log artifact
// with X-Ada-Truncated after checking project membership on each read.
// Checks and agents run with the user's rights, outside any sandbox, and can
// still write Ada's state directory, so a result catches a tree changed by
// mistake or by a careless agent, not by a determined one; the M5 guest is
// the boundary against hostile code.
package api
