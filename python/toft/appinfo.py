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
import re
import time
from typing import Any

from .env import ENV_BASE_PATH
from .errors import ToftError, bad_request
from .session import transport

#: How long an answer from ``/api/v1/app/url`` is reused. Short, because the
#: address moves without a deploy and a link is only as good as the address it
#: was built from; long enough that a job mailing a thousand people asks once.
URL_TTL_SECONDS = 60.0

_SCHEME = re.compile(r"^[A-Za-z][A-Za-z0-9+.-]*:")


def join_app_url(public_url: str, base_path: str, path: str) -> str:
    """Join an app path onto the app's public address — :meth:`App.url`'s rule.

    A path is the app's, so ``/reports/42`` and ``reports/42`` both mean the
    app's own route, never the host's root. A path that already carries the
    base path — ``url_for`` behind ProxyFix, a run's ``status_url`` — is not
    prefixed twice. A query or fragment is kept as written. An absolute URL is
    refused: joining two origins is never what the caller meant.
    """
    if not isinstance(path, str):
        raise bad_request(f"url() takes a path, got {path!r}", hint='e.g. toft.app.url("/reports/42")')
    if _SCHEME.match(path) or path.startswith("//"):
        raise bad_request(
            f"url() was given an absolute URL ({path!r}), which it would join onto this app's address",
            hint='pass the path inside the app, e.g. toft.app.url("/reports/42")')
    if base_path and path.startswith(base_path) and path[len(base_path):][:1] in ("", "/", "?", "#"):
        path = path[len(base_path):]
    if path == "" or path[0] in "?#":
        path = "/" + path
    elif path[0] != "/":
        path = "/" + path
    return public_url.rstrip("/") + path


class App:
    """This app, as the platform sees it."""

    def __init__(self) -> None:
        self._view: dict[str, Any] | None = None
        self._address: dict[str, str] | None = None
        self._address_at = 0.0

    def _load(self) -> dict[str, Any]:
        if self._view is None:
            self._view = transport().get("/api/v1/app/self") or {}
        return self._view

    def refresh(self) -> "App":
        """Drop the cached view; the next read re-fetches it."""
        self._view = None
        self._address = None
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

    def url(self, path: str = "") -> str:
        """The absolute address of a page in this app, for a link that leaves it.

        For an email, a Slack post, a PDF, a ticket — anywhere a relative link
        names nothing. Inside the app's own pages, relative links built from
        :attr:`base_path` stay the right tool::

            toft.app.url("/reports/42")   # https://apps.example.com/apps/reporter/reports/42
            toft.app.url()                # the app's front page
            toft.app.url(run.status_url)  # a run's status page

        The address is asked of the platform rather than stored, and reused for
        :data:`URL_TTL_SECONDS`: an administrator can move the install or give
        the app a hostname of its own while it runs, and a link built from a
        value cached at start would point at the old address. Works the same in
        a request, a job or a schedule. If the platform cannot be reached, the
        last address this process was told is used; with none, the error is
        raised. An install with no address configured refuses with
        ``no_public_address``.
        """
        address = self._current_address()
        return join_app_url(address["public_url"], address.get("base_path", ""), path)

    def _current_address(self) -> dict[str, str]:
        now = time.monotonic()
        if self._address is not None and now - self._address_at < URL_TTL_SECONDS:
            return self._address
        try:
            got = transport().get("/api/v1/app/url") or {}
        except ToftError as e:
            # Unreachable — no status, no code — is the one failure a known
            # address outlives. A refusal is the platform's answer and stands.
            if self._address is not None and e.status is None and not e.code:
                return self._address
            raise
        self._address = {"public_url": str(got.get("public_url") or ""),
                         "base_path": str(got.get("base_path") or "")}
        self._address_at = now
        return self._address

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
        thing to type, and guessing wrong should not be a silent false — which
        is also why an empty name is refused (``bad_request``) rather than
        answered ``False``.
        """
        if not isinstance(capability, str) or not capability.strip():
            raise bad_request(
                f"can() needs a capability or integration name, got {capability!r}",
                hint='e.g. toft.app.can("email") or toft.app.can("email.send")')
        held = self.capabilities
        if capability in held:
            return True
        prefix = capability + "."
        return any(c.startswith(prefix) for c in held)

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        if self._view is None:
            return "<toft.App (not yet loaded)>"
        return f"<toft.App {self.slug!r} mode={self.mode!r}>"
