"""The app's one JSON document.

Settings and small state that outlive a restart, a redeploy and a rollback, and
belong to nobody's session::

    settings = toft.data.get() or {"period": "monthly"}
    toft.data.set({**settings, "period": period})

It sits between the two things that already existed, neither of which fits a
handful of settings: :func:`toft.scratch_dir` is wiped on every start, and
mapped storage is durable but is an administrator's separate decision per app.
This needs nothing switched on — every app has it, because it holds only what the
app put there and reaches nothing outside the install.

**One document, not a store.** No keys, no listing, no queries, and a cap of
256 KiB. An app with more than that has outgrown the feature rather than hit a
limit worth raising, and the platform's refusal says so and names what to use
instead.

**Never written and cleared are the same state, and both differ from ``{}``.**
:meth:`Data.get` answers ``None`` for either, which is what lets an app write an
empty document and mean it.

**Two writers need :meth:`Data.update`.** A ``set()`` is last-write-wins, which
is right for the single-writer app and silently loses a change when a job and the
web process both save. ``update()`` reads, applies your change, and writes it
back under the version it read — retrying if somebody got there first, so the
other writer's change survives instead of being overwritten.
"""

from __future__ import annotations

from dataclasses import dataclass, replace
from datetime import datetime
from typing import Any, Callable

from .errors import ToftError
from .session import transport

# How many times update() re-reads and re-applies before giving up.
DEFAULT_ATTEMPTS = 5


class VersionConflict(ToftError):
    """The document changed under a write that named the version it replaced.

    Carries the version that *is* stored, so recovery is one read rather than a
    guess. :meth:`Data.update` catches this for you; app code driving
    :meth:`Data.set` with ``if_version`` catches it itself.
    """

    def __init__(self, message: str, version: int = 0, **kw: Any) -> None:
        super().__init__(message, **kw)
        self.version = version


@dataclass
class Document:
    """One read of the document, with the platform's two facts about it."""

    #: The document, or None when the app has never written one.
    data: dict | None = None
    #: 0 when nothing is stored; the compare-and-set token otherwise.
    version: int = 0
    #: When it was last written, or None.
    updated_at: datetime | None = None
    #: Its size in bytes, as the platform counted it against the cap.
    bytes: int = 0

    @property
    def empty(self) -> bool:
        """True when nothing has been saved, or it was cleared."""
        return self.data is None

    @classmethod
    def from_json(cls, raw: Any) -> "Document":
        raw = raw or {}
        stamp = raw.get("updated_at")
        when: datetime | None = None
        if isinstance(stamp, str) and stamp:
            try:
                when = datetime.fromisoformat(stamp.replace("Z", "+00:00"))
            except ValueError:
                when = None
        return cls(
            data=raw.get("data"),
            version=int(raw.get("version", 0)),
            updated_at=when,
            bytes=int(raw.get("bytes", 0)),
        )


class Data:
    """The app's one JSON document."""

    def get(self) -> dict | None:
        """The document, or None when the app has never written one."""
        return self.read().data

    def read(self) -> Document:
        """The document *and* its version — what :meth:`update` is built on."""
        return Document.from_json(transport().get("/api/v1/app/data"))

    def set(self, document: dict, if_version: int | None = None) -> Document:
        """Replace the document.

        ``if_version`` makes the write conditional: it happens only if that is
        still the stored version, and raises :class:`VersionConflict` otherwise.
        ``0`` means "only if nothing is stored", which is how an app claims first
        write without a separate call.
        """
        # The one thing checked locally, and not a duplicated platform limit:
        # ``None`` here would be serialized as ``{}`` and quietly wipe the app's
        # settings, which is a wrong request rather than a refused one.
        if not isinstance(document, dict):
            raise ToftError(
                "app data holds one JSON object — a list or a bare value has nowhere "
                'to grow a field later, so wrap it, e.g. {"items": [...]}.',
                body={"code": "bad_request", "retryable": False},
            )
        headers = {} if if_version is None else {"If-Match": f'"{if_version}"'}
        raw = _typed(lambda: transport().put_json("/api/v1/app/data", document, headers))
        # **The write's answer carries what was written.** The platform's reply
        # to a PUT is deliberately metadata only — version, timestamp, size —
        # rather than the document echoed back, so without this the handle came
        # back with ``data=None``: the value this API reserves for "never
        # written", handed to a caller who had just written. ``doc =
        # toft.data.set(...)`` then read ``doc.data`` as empty for a document that
        # is not.
        #
        # Filled from the document that went out rather than by a second read,
        # which would cost a round trip and could answer with somebody else's
        # later write.
        return replace(Document.from_json(raw), data=document)

    def update(
        self,
        mutator: Callable[[dict], dict | None],
        attempts: int = DEFAULT_ATTEMPTS,
    ) -> Document:
        """Read, change, write back — without losing a concurrent writer's change.

        ``mutator`` is given the current document (``{}`` when there is none) and
        may either mutate it in place or return a replacement. The write is
        conditional on the version that was read, so if somebody else wrote in
        between, this re-reads and applies your change again rather than
        overwriting theirs::

            toft.data.update(lambda s: s.update(runs=s.get("runs", 0) + 1))

        Use :meth:`read` and :meth:`set` directly when "there was nothing here"
        has to be told apart from "there was an empty document" — that
        distinction is the one thing ``{}`` papers over.
        """
        last: VersionConflict | None = None
        for _ in range(max(1, attempts)):
            current = self.read()
            working = current.data if current.data is not None else {}
            changed = mutator(working)
            try:
                return self.set(working if changed is None else changed, current.version)
            except VersionConflict as exc:
                last = exc
        raise ToftError(
            f"app data changed under {attempts} attempts to update it — something "
            "is writing it continuously",
            status=last.status if last else None,
            body=last.body if last else None,
        )

    def clear(self, if_version: int | None = None) -> bool:
        """Throw the document away, putting the app back to never having one.

        Not the same as writing ``{}``, deliberately: an app that cleared its
        settings and one that saved an empty document mean different things, and
        the next read tells them apart. Returns whether there was anything to
        remove.
        """
        headers = {} if if_version is None else {"If-Match": f'"{if_version}"'}
        raw = _typed(lambda: transport().delete("/api/v1/app/data", headers)) or {}
        return bool(raw.get("deleted"))


def _typed(call: Callable[[], Any]) -> Any:
    """Give a failed precondition a type.

    So :meth:`Data.update` can retry it and app code can catch it, rather than
    matching on a status code that other refusals also use.
    """
    try:
        return call()
    except ToftError as exc:
        if exc.code == "version_conflict":
            raise VersionConflict(
                str(exc),
                version=int(exc.body.get("version", 0)),
                status=exc.status,
                retry_after=exc.retry_after,
                body=exc.body,
            ) from None
        raise
