package toft

import (
	"errors"
	"time"
)

// Error is the one error type the SDK returns for a platform refusal.
//
// It carries the HTTP status so a caller can tell "you asked for something
// impossible" from "try again later" — the difference between a bug to fix and
// a blip to retry.
//
// RetryAfter is the platform's own answer to *how long* when it gave one. The
// SDK honours it for its single built-in retry; a caller writing its own loop
// should honour it too rather than guessing.
//
// Body is the decoded refusal, when there was one. Some refusals carry figures
// an app can render — a full queue says how many runs are in flight and what
// the limit is — and flattening those into a message string would throw away
// the only part that could go on a page. [Error.Code] is that body's stable
// machine-readable name, which is what app code should branch on rather than
// matching prose that may be reworded.
//
// **Whether to try again is the platform's call, not this type's.** Every
// error body the platform writes carries `code`, `retryable` and, for codes
// that have one, `hint` — the next step in its own words. [Error.Retryable]
// returns the platform's `retryable` when the body has one, because only the
// handler knows that a 503 means "this install is configured without it,
// forever" or that a 409 means "wait for a run to finish". The status rule is
// the fallback for the refusals that never reached a handler: a transport
// failure, a proxy's HTML page, an install older than the envelope.
type Error struct {
	Message    string
	Status     int
	RetryAfter time.Duration
	Body       map[string]any

	// retryGiven records that the platform sent a Retry-After, so a deliberate
	// "retry now" is not read as "the platform said nothing" and slept over.
	retryGiven bool

	// Wrapped is the transport failure underneath, when there was one — a DNS
	// answer, a refused connection, a cancelled context. Kept so errors.Is
	// reaches context.Canceled, which is the one a caller genuinely branches on.
	Wrapped error
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error { return e.Wrapped }

// Code is the refusal's stable machine-readable name, or "" when the platform
// did not send one.
func (e *Error) Code() string {
	if e == nil {
		return ""
	}
	code, _ := e.Body["code"].(string)
	return code
}

// Retryable reports whether trying again could plausibly work. The platform's
// own `retryable` decides when the body carries one; otherwise a transport
// failure, anything at 500 and above, or a rate limit.
func (e *Error) Retryable() bool {
	if e == nil {
		return false
	}
	if said, ok := e.Body["retryable"].(bool); ok {
		return said
	}
	return e.statusRetryable()
}

// statusRetryable is the rule for an answer that said nothing about retrying.
func (e *Error) statusRetryable() bool {
	return e.Status == 0 || e.Status >= 500 || e.Status == 429
}

// Hint is the platform's next step for this refusal — "mint a new key", "an
// administrator has to lift the lock" — or "" when it sent none. It is written
// for whoever reads the failure, so an app logging an Error should log it too.
func (e *Error) Hint() string {
	if e == nil {
		return ""
	}
	hint, _ := e.Body["hint"].(string)
	return hint
}

// autoRetry is whether the transport's single built-in retry fires.
//
// It is narrower than [Error.Retryable] on purpose. The built-in retry covers a
// request that never reached a handler — a rate limiter, a gateway — so the
// platform can veto it (a 503 saying "not configured" is not slept over) but
// cannot widen it to a 4xx: a 409 the platform calls retryable means "wait for
// something", and how long is the caller's loop to decide, not two seconds
// here.
func (e *Error) autoRetry() bool {
	return e.statusRetryable() && e.Retryable()
}

// VersionConflictError is returned when the document changed under a write that
// named the version it replaced.
//
// It carries the version that *is* stored, so recovery is one read rather than
// a guess. [DataClient.Update] catches this for you; app code driving
// [DataClient.Set] with a version catches it itself with errors.As.
type VersionConflictError struct {
	Err     *Error
	Version int
}

func (e *VersionConflictError) Error() string { return e.Err.Error() }

// Unwrap is what makes errors.As reach the platform's [Error] underneath, so a
// caller that wants the status or the decoded body gets it from either type.
func (e *VersionConflictError) Unwrap() error { return e.Err }

// QueueFullError says this app already has as many runs in flight as it is
// allowed.
//
// A first-class, actionable refusal rather than a generic error, and it carries
// the numbers: an app renders "3 runs already queued, and 3 is the limit — try
// again in a moment" instead of putting a stack trace in the middle of
// somebody's page. Retryable is true, because the platform says a full queue
// drains.
type QueueFullError struct {
	Err      *Error
	InFlight int
	Limit    int
}

func (e *QueueFullError) Error() string { return e.Err.Error() }

func (e *QueueFullError) Unwrap() error { return e.Err }

// AsError extracts the platform's [Error] from err, for the caller who wants
// the status or the decoded body. It is errors.As with the type already named.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
