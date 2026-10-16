"""The app's view of itself.

``toft.app`` answers two different kinds of question and it is worth keeping them
apart.

*Who am I* — id, slug, name, execution mode, base path — is for the app's own
logs and for building links back to itself.

*What may I do* — :meth:`App.can` and :attr:`App.capabilities` — is the one that
changes how an app should be written. An administrator attaches integrations to
an app; app code that asks first can render "email is not enabled for this app;
ask an administrator" on a settings screen at startup, instead of discovering it
at 2am as a refusal in a log nobody is reading::

    if toft.app.can("email"):
        toft.notify.email(to=recipients, subject=..., text=...)
    else:
        log.info("email not attached to this app; report written to storage only")

The answer is attached-or-not and never *why* not. The reason is an
administrator's business, and an app is not a principal that gets told about the
install's configuration — the same line ``/system/settings`` draws by 404ing to
everyone who is not an administrator.

The whole view is fetched once and cached for the life of the process, because
it is read on hot paths and an administrator's change lands at the next deploy
anyway. :meth:`App.refresh` re-reads it for a long-lived app that would rather
notice sooner.
"""

from __future__ import annotations

import os
from typing import Any

from .env import ENV_BASE_PATH
from .session import transport


class App:
    """This app, as the platform sees it."""

    def __init__(self) -> None:
        self._view: dict[str, Any] | None = None

    def _load(self) -> dict[str, Any]:
        if self._view is None:
            self._view = transport().get("/api/v1/app/self") or {}
        return self._view

    def refresh(self) -> "App":
        """Drop the cached view; the next read re-fetches it."""
        self._view = None
        return self

    @property
    def id(self) -> str:
        return str(self._load().get("id", ""))

    @property
    def slug(self) -> str:
        return str(self._load().get("slug", ""))

    @property
    def name(self) -> str:
        return str(self._load().get("name", ""))

    @property
    def mode(self) -> str:
        """``shared`` or ``isolated``."""
        return str(self._load().get("mode", ""))

    @property
    def base_path(self) -> str:
        """Where this app is mounted, e.g. ``/apps/reporter``.

        **Build every link and form action from this**, because a root-absolute
        URL asks the platform rather than the app and 404s.

        The platform sends it; ``BASE_PATH`` in the environment is the fallback,
        and it is here because of what the failure looked like without it. An
        install older than the field left the key out of the payload, this
        returned ``""``, and an app that built its links from the accessor —
        the obvious thing to do once ``toft`` is imported — emitted exactly the
        URLs that 404, on every link, with nothing in any log explaining why.
        There was also no value the app could have checked: ``""`` is the
        legitimate answer for an install that gives each app its own origin,
        so "not mounted" and "the platform did not say" looked identical.

        The environment variable is the same value the deploy injected into
        this very process, so the fallback cannot disagree with the platform —
        it can only answer where the platform stayed silent.
        """
        value = self._load().get("base_path")
        if value is None:
            return os.environ.get(ENV_BASE_PATH, "").strip()
        return str(value)

    @property
    def policy(self) -> dict[str, Any]:
        """The read-only policy block: the rules this app is judged by.

        Version retention, whether the app may sleep and why. The same block
        ``GET /apps/{id}`` returns, for the same reason it exists there — a
        platform decision about one app is visible on that app.
        """
        value = self._load().get("policy")
        return dict(value) if isinstance(value, dict) else {}

    @property
    def capabilities(self) -> tuple[str, ...]:
        """Every capability an administrator attached, e.g. ``email.send``."""
        raw = self._load().get("capabilities") or []
        return tuple(str(c) for c in raw)

    def can(self, capability: str) -> bool:
        """Whether this app holds a capability.

        Takes either a full capability (``email.send``) or an integration name
        (``email``), which is true when *any* of that integration's actions is
        attached. Both spellings are accepted because both are the obvious
        thing to type, and guessing wrong should not be a silent false.
        """
        if not capability:
            return False
        held = self.capabilities
        if capability in held:
            return True
        prefix = capability + "."
        return any(c.startswith(prefix) for c in held)

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        if self._view is None:
            return "<toft.App (not yet loaded)>"
        return f"<toft.App {self.slug!r} mode={self.mode!r}>"
