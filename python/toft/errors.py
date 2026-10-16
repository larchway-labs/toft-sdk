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
        return self.status is None or self.status >= 500 or self.status == 429
