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
package api
