"""The process-wide transport, built on first use.

One connection story for the whole SDK, so ``toft.app`` and ``toft.notify`` cannot
end up talking to two different places, and so importing ``toft`` never
touches the environment. ``reset()`` exists for tests and for an app that
re-reads its own configuration; nothing in normal app code calls it.
"""

from __future__ import annotations

from .transport import AppTransport

_shared: AppTransport | None = None


def transport() -> AppTransport:
    global _shared
    if _shared is None:
        _shared = AppTransport()
    return _shared


def reset(t: AppTransport | None = None) -> None:
    global _shared
    _shared = t
