"""The one error type the SDK raises.

It lives in its own module because everything else in the package needs it and
nothing else in the package should need the jobs client. ``client`` re-exports
it, so ``from toft.client import ToftError`` keeps working.
"""

from __future__ import annotations


class ToftError(RuntimeError):
    """An error from the platform.

    Carries the HTTP status so a caller can tell "you asked for something
    impossible" from "try again later" — the difference between a bug to fix
    and a blip to retry.

    ``retry_after`` is the platform's own answer to *how long*, in seconds,
    when it gave one. The SDK honours it for its single built-in retry; a
    caller writing its own loop should honour it too rather than guessing.

    ``body`` is the decoded refusal, when there was one. Some refusals carry
    figures an app can render — a full queue says how many runs are in flight
    and what the limit is — and flattening those into a message string would
    throw away the only part that could go on a page. ``code`` is that body's
    stable machine-readable name, which is what app code should branch on
    rather than matching prose that may be reworded.

    **Whether to try again is the platform's call, not this class's.** Every
    error body the platform writes carries ``code``, ``retryable`` and, for
    codes that have one, ``hint`` — the next step in its own words.
    :attr:`retryable` returns the platform's answer when the body has one,
    because only the handler knows that a 503 means "this install is configured
    without it, forever" or that a 409 means "wait for a run to finish". The
    status rule is the fallback for refusals that never reached a handler: a
    transport failure, a proxy's HTML page, an install older than the envelope.
    """

    def __init__(
        self,
        message: str,
        status: int | None = None,
        retry_after: float | None = None,
        body: dict | None = None,
    ) -> None:
        super().__init__(message)
        self.status = status
        self.retry_after = retry_after
        self.body = body or {}

    @property
    def code(self) -> str:
        return str(self.body.get("code", ""))

    @property
    def retryable(self) -> bool:
        """Whether trying again could plausibly work — the platform's word first."""
        said = self.body.get("retryable")
        if isinstance(said, bool):
            return said
        return self._status_retryable()

    @property
    def hint(self) -> str | None:
        """The platform's next step for this refusal, or None when it sent none.

        Written for whoever reads the failure — "mint a new key", "an
        administrator has to lift the lock" — so an app logging the error should
        log this too.
        """
        hint = self.body.get("hint")
        return hint if isinstance(hint, str) and hint else None

    def _status_retryable(self) -> bool:
        return self.status is None or self.status >= 500 or self.status == 429

    @property
    def _auto_retry(self) -> bool:
        """Whether the transport's single built-in retry fires.

        Narrower than :attr:`retryable` on purpose. The built-in retry covers a
        request that never reached a handler — a rate limiter, a gateway — so
        the platform can veto it (a 503 saying "not configured" is not slept
        over) but cannot widen it to a 4xx: a 409 the platform calls retryable
        means "wait for something", and how long is the caller's loop to
        decide, not two seconds here.
        """
        return self._status_retryable() and self.retryable
