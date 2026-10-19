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
package api
