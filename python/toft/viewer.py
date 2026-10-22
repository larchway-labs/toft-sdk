"""The app acting for whoever is looking at the page.

    import toft

    me = toft.viewer(st.context.headers)      # Flask/FastAPI: request.headers
    run = me.jobs.start("reconcile", params={"month": month})
    recent = me.jobs.mine("reconcile", limit=20)

**The app never asserts who the viewer is.** What it relays is a blob the
platform signed over ``(user, app, session)`` on its way through the gateway.
The app cannot mint one, cannot alter one, and cannot read who is inside it —
which is the same contract ``X-Toft-Session`` already has: a correlation
id, not identity.

What falls out is the valuable part: the platform holds the real principal at
the moment it decides. Whether this person may start this job is evaluated
against *their* permissions, not against the app's claim about them, so an app
cannot launder a viewer into authority they do not have. And it works in the
other direction too — ``mine()`` answers a question *about* the viewer without
disclosing the viewer.

**A missing header raises.** It never falls back to the app acting as itself: a
silent downgrade would move a run out of a person's name and into nobody's, and
nobody would notice until an audit. The usual cause is the app being called
outside the Toft gateway — a health check, a direct port, a local dev
server.

**It expires with the session it came from**, which is what makes it work in
Streamlit at all. Streamlit talks over a WebSocket, so headers arrive once at
the handshake and the app holds them for hours; a token on a five-minute clock
would make the button work at first and then fail in a way nobody could
reproduce. When it does expire, the person signs in again — nothing is broken.
"""

from __future__ import annotations

import threading
import time
from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any

from .errors import ToftError
from .jobs import Jobs
from .session import transport

#: The request header the Toft proxy sets on its way to the app. Case
#: does not matter for lookup here — Express lowercases, Streamlit preserves,
#: and WSGI mangles — so :func:`viewer` searches case-insensitively.
ACT_FOR_HEADER = "X-Toft-Act-For"


#: How long a resolved identity is reused for the same blob.
#:
#: A per-widget Streamlit rerun must not become a request per rerun, and 60
#: seconds is the honest trade rather than a tuned number. What it buys and what
#: it costs, plainly: **a role change takes effect in the app within the TTL,
#: not instantly.** Where the *platform* enforces — starting a job, running a
#: scheduled task — it re-resolves live and this cache is irrelevant. Where the
#: *app* enforces, the app is the enforcer and the window is the app's to accept
#: or to shorten. ``who(headers, refresh=True)`` bypasses it for the one call an
#: app wants to be certain about.
IDENTITY_TTL_SECONDS = 60.0

#: The cache is bounded as well as expiring: an app serving many people would
#: otherwise hold one entry per session for the life of the process. Evicting
#: the whole thing when it fills is deliberate — an LRU here would be machinery
#: in aid of a cache whose entries are worth 60 seconds each.
_IDENTITY_CACHE_MAX = 512

_identity_cache: dict[str, tuple[float, "Identity | None"]] = {}
_identity_lock = threading.Lock()


@dataclass(frozen=True)
class Identity:
    """Who is looking at this app, and what they may do *here*.

    ``role`` answers one question and it is worth being exact about which:
    *what may this person do to this deployment in Toft* — redeploy it,
    edit its jobs, change its schedules. It is ``owner``, ``editor`` or
    ``viewer``, the same three words the Sharing tab granted from.

    It is **not** domain authority. App authors will reach for it as one,
    because it is the signal in front of them, and "owners may approve invoices"
    is a rule about invoices rather than about deployments. :attr:`groups` is
    where a domain rule belongs — ``finance`` and ``approvers`` are the things
    that mean what they say.

    ``is_admin`` is separate from ``role`` on purpose. Toft hands an
    administrator effective ownership of every app, so an app gating a
    destructive action on ``role == "owner"`` alone would be granting it to
    every administrator on the install with no way to refuse. The common line is
    ``me.role == "owner" or me.is_admin``; an app that means to withhold simply
    does not write the second half.

    ``email`` is ``None`` when the app's visibility is ``public``. Not a
    failure and not a person without an address: a public app's visitors did not
    consent to a mailing list by visiting. ``toft.app.can("viewer.identity.email")``
    says so at startup rather than leaving an author to infer it from a null.
    """

    id: str
    name: str = ""
    email: str | None = None
    avatar_url: str | None = None
    role: str = ""
    is_admin: bool = False
    groups: tuple[dict[str, str], ...] = field(default_factory=tuple)

    @property
    def group_names(self) -> tuple[str, ...]:
        """The group names alone, for the ordinary ``"finance" in me.group_names``."""
        return tuple(str(g.get("name", "")) for g in self.groups)


class Viewer:
    """One person, as a scoping capability rather than a fact the app holds.

    The capability half is unchanged and is still the default: everything an app
    wants to do *with* a viewer — start work in their name, list what they
    started — the platform does on its behalf, holding the real principal at the
    moment it decides.

    :meth:`identity` is the deliberate exception, and it is a *question the app
    must answer for itself*: may this person see the approvals tab. The platform
    is not in the loop there, so the app is the enforcer and needs the fact. It
    is off until an administrator enables it for this app.
    """

    __slots__ = ("_blob", "jobs")

    def __init__(self, blob: str) -> None:
        self._blob = blob
        #: The job surface, with this viewer attached.
        self.jobs = Jobs({ACT_FOR_HEADER: blob})

    def identity(self, refresh: bool = False) -> Identity | None:
        """Resolve who this viewer is, or ``None`` if there is nobody to name.

        The same call :func:`who` makes, reached from a :class:`Viewer` an app
        already built to start a job.
        """
        return _resolve(self._blob, refresh=refresh)

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        # Never the blob itself: it is a bearer credential for one session, and
        # a log line is exactly where one should not end up.
        return "<toft.Viewer (a signed-in person; the app cannot see who)>"


def viewer(headers: Mapping[str, Any] | Any) -> Viewer:
    """Build a :class:`Viewer` from the request headers the app is serving.

    Accepts anything header-shaped: a plain mapping, ``request.headers`` from
    Flask or FastAPI, ``st.context.headers`` from Streamlit. Raises when the
    act-for header is absent, naming it and the likely cause.
    """
    blob = _find_header(headers, ACT_FOR_HEADER)
    if not blob:
        raise ToftError(
            f"no {ACT_FOR_HEADER} header on this request, so there is no viewer to act "
            f"for. Toft's proxy sets it only for a signed-in browser session, so its "
            f"absence means the request came with an API key (the gateway authorizes a "
            f"key but asserts no person to the app — `toft fetch --session` is a signed-in "
            f"request), or the app was reached outside the gateway — a health check, a "
            f"direct port, or a local development server. Use toft.jobs "
            f"to act as the app itself, deliberately, rather than treating this as the "
            f"same thing.",
            body={"code": "no_viewer", "retryable": False},
        )
    return Viewer(blob)


def _find_header(headers: Any, name: str) -> str:
    """Read one header out of whatever the framework calls its header mapping.

    Frameworks disagree about case — Express lowercases, WSGI shouts, Streamlit
    passes through what the browser sent — and a case-sensitive lookup here
    would be a bug that only shows up on somebody else's stack.
    """
    if headers is None:
        return ""
    getter = getattr(headers, "get", None)
    if callable(getter):
        for spelling in (name, name.lower(), name.upper()):
            value = getter(spelling)
            if value:
                return str(value)
    try:
        for key, value in dict(headers).items():
            if str(key).lower() == name.lower() and value:
                return str(value)
    except (TypeError, ValueError):
        return ""
    return ""


def who(headers: Mapping[str, Any] | Any, refresh: bool = False) -> Identity | None:
    """Who is looking at this app, or ``None`` if there is nobody to name.

        me = toft.who(st.context.headers)          # Identity | None
        if me and me.role in ("owner", "editor"):
            render_admin_tab()
        if me and "finance" in me.group_names:
            render_ledger()

    **This never raises for an absent viewer, and** :func:`viewer` **still
    does.** The two answer different questions — *who is this* and *act as this
    person* — and only the first one can honestly answer "nobody". A public app
    rendering "sign in to see your reports" should not have to wrap the normal
    case in a ``try``; an app about to start a job in somebody's name should
    never silently start it in nobody's.

    ``None`` means nobody is there: an anonymous visitor, or somebody whose
    access was revoked while their page stayed open. **It never means "not
    allowed to ask"** — that raises, naming the setting and saying an
    administrator enables it. An app that would rather not raise at all checks
    ``toft.app.can("viewer.identity")`` at startup and never calls this on an
    install where it is off.
    """
    blob = _find_header(headers, ACT_FOR_HEADER)
    if not blob:
        return None
    return _resolve(blob, refresh=refresh)


def _resolve(blob: str, refresh: bool = False) -> Identity | None:
    """Ask the platform, through the cache.

    Keyed on the blob rather than on a user id, because the blob is all the app
    has and two people are never behind one — it is signed over
    ``(user, app, session)``.
    """
    now = time.monotonic()
    if not refresh:
        with _identity_lock:
            hit = _identity_cache.get(blob)
        if hit is not None and hit[0] > now:
            return hit[1]

    identity = _fetch(blob)

    with _identity_lock:
        if len(_identity_cache) >= _IDENTITY_CACHE_MAX:
            _identity_cache.clear()
        _identity_cache[blob] = (now + IDENTITY_TTL_SECONDS, identity)
    return identity


def _fetch(blob: str) -> Identity | None:
    body = transport().get("/api/v1/app/viewer", {ACT_FOR_HEADER: blob})
    # 204: the app's own credential was valid and the question was answerable.
    # The answer is that there is no viewer.
    if not body:
        return None
    groups = tuple(
        {"id": str(g.get("id", "")), "name": str(g.get("name", ""))}
        for g in (body.get("groups") or [])
    )
    return Identity(
        id=str(body.get("id", "")),
        name=str(body.get("name", "")),
        email=body["email"] if body.get("email") else None,
        avatar_url=body.get("avatar_url") or None,
        role=str(body.get("role", "")),
        is_admin=bool(body.get("is_admin")),
        groups=groups,
    )


def _reset_identity_cache() -> None:
    """Drop every cached identity. For tests; app code has ``refresh=True``."""
    with _identity_lock:
        _identity_cache.clear()
