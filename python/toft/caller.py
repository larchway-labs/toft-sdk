"""What the caller in front of this request may do, in one word.

    @app.post("/rebuild")
    def rebuild(request):
        c = toft.caller(request.headers)
        if not c.at_least(toft.ROLE_EDITOR):
            return Response(status_code=403)
        ...

Toft puts a signed role on every request it forwards, so an app that wants
to protect an expensive endpoint no longer has to choose between exposing it to
everyone who can open the page and asking the platform who the viewer is. The
three person-words are the ones the Sharing tab granted from — ``owner``,
``editor``, ``viewer`` — plus ``anonymous`` for a visitor who is not signed in,
``system`` for Toft firing a scheduled job, and ``app`` for the app's own
credential.

**A role is not an identity and it is not domain authority.** It says what this
caller may do to *this deployment in Toft* — redeploy it, edit its jobs,
change its schedules. "Owners may approve invoices" is a rule about invoices and
belongs on a group: see ``toft.who``. Nothing here names a person.

**This is the app's decision, not the platform's.** Toft enforces nothing
new: it tells you, you refuse. Where the *platform* decides — starting a job,
running a scheduled job — it re-resolves live and never reads this header.

**A missing or unverifiable header is ``unknown``, and ``at_least`` is False for
it.** Absent key, absent header, bad signature and expired all give one answer,
deliberately: four branches would be four ways to get a refusal wrong. The one
deployment where the header is missing — a health check, a direct port, a
laptop — is the one nobody configured, so it must refuse.

**Verify on arrival.** The assertion is about one request and is good for two
minutes. Streamlit is the case worth stating: headers arrive at the WebSocket
handshake and the session lasts hours, so read the role once at the top of the
script and hold your own decision from there. A change of role during a
long-held connection does not reach you; the platform's own enforcement is
unaffected, and the app's window is the app's.

``hmac`` and ``hashlib`` are the standard library, which is the whole reason the
signature is an HMAC rather than something better.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import os
import time
from typing import Any, Mapping

#: The request header the Toft proxy sets on every request it forwards, and
#: on every scheduled firing it makes.
ROLE_HEADER = "X-Toft-Role"

#: The environment variable Toft injects the verification key into. Per app
#: and per start: a redeploy replaces it, which is why nothing signed for the
#: previous release verifies against this one.
ENV_ROLE_KEY = "TOFT_ROLE_KEY"

#: Every failure: no header, no key, a bad signature, an expired blob. Never
#: privileged.
ROLE_UNKNOWN = "unknown"
#: A visitor who is not signed in to Toft. Only a public app sees it.
ROLE_ANONYMOUS = "anonymous"
#: What this person may do to this deployment. A Toft administrator with no
#: grant on the app reads as ``viewer`` — the header carries no admin bit, and an
#: app that needs to know is asking about a person, which is what ``toft.who`` is
#: for.
ROLE_VIEWER = "viewer"
ROLE_EDITOR = "editor"
ROLE_OWNER = "owner"
#: Toft acting on its own: today, a scheduled job firing on its schedule.
#: A *manual* firing of the same schedule carries the role of whoever pressed the
#: button.
ROLE_SYSTEM = "system"
#: The app's own credential — an app key at the gateway, an app-started job's
#: callback.
ROLE_APP = "app"

_ROLE_VERSION = "v1"

#: anonymous < viewer < editor < owner. ``system`` and ``app`` are not on it.
_PERSON_RANK = {ROLE_ANONYMOUS: 0, ROLE_VIEWER: 1, ROLE_EDITOR: 2, ROLE_OWNER: 3}


class Caller:
    """Who is asking, as a role rather than a person."""

    __slots__ = ("role",)

    def __init__(self, role: str) -> None:
        #: One of the seven constants above and never anything else — a word this
        #: SDK does not know is ``unknown``, so an install running ahead of an
        #: app's SDK refuses rather than handing app code a value it has never
        #: branched on.
        self.role = role

    def at_least(self, role: str) -> bool:
        """Whether this caller holds at least ``role`` on the person ladder.

        **It answers only about the ladder.** ``system`` and ``app`` are not on
        it and are False for every rung: they are not more or less authorized
        than a person, they are a different kind of caller, and ranking
        Toft's scheduler above an owner would be a guess this SDK has no
        business making. An endpoint that serves both a person and its own
        schedule says so::

            if c.role != toft.ROLE_SYSTEM and not c.at_least(toft.ROLE_EDITOR):
                refuse()

        ``unknown`` is False for everything, ``anonymous`` included — an
        unverifiable caller is not "at least a stranger", it is nobody this app
        can say anything about.

        **A rung does not mean a human.** An app key may be minted carrying
        ``viewer`` or ``editor``, by an owner or editor of this app who chose to
        describe their machine client that way, and such a key satisfies
        ``at_least`` exactly as a person with the same word does. That is the
        point of the word — the caller may do what it says — and it is why
        nothing here answers "is a human in front of me". See :attr:`is_person`.
        """
        mine = _PERSON_RANK.get(self.role)
        want = _PERSON_RANK.get(role)
        return mine is not None and want is not None and mine >= want

    @property
    def is_person(self) -> bool:
        """On the person ladder — anonymous, viewer, editor or owner. False for
        system/app/unknown.

        **It does not mean a human, and it never checked for one.** This has
        always been membership of the ladder and nothing more; what changed is
        that a rung can now be reached by something other than a person, because
        an app key may be minted carrying ``viewer`` or ``editor``. Before that
        the two coincided by accident: no machine credential could reach the
        ladder at all.

        **This platform cannot tell you a human is in front of you.** Nothing
        here can, and nothing here pretends to — the honest question an app can
        ask is what the caller may do, which is what the word says.
        """
        return self.role in _PERSON_RANK

    @property
    def is_system(self) -> bool:
        """Toft firing a scheduled job on its schedule.

        A *manual* firing of the same schedule is the person who pressed the
        button, not this.
        """
        return self.role == ROLE_SYSTEM

    @property
    def signed_in(self) -> bool:
        """Above anonymous on the person ladder. False for an anonymous visitor.

        Like :attr:`is_person`, a fact about the ladder rather than about a
        human: an app key minted with ``viewer`` or ``editor`` is True here, and
        no account was signed into to produce it.

        This is the one bit a public app gains that it did not have: it can tell
        a reader with a rung from a stranger. It names nobody — but a log line per
        request would build a record of which of your readers have Toft
        accounts, which is worth deciding about rather than doing by default.
        """
        rank = _PERSON_RANK.get(self.role)
        return rank is not None and rank > 0

    def __repr__(self) -> str:
        # Safe to log: a role, never a person and never the blob.
        return f"<toft.Caller {self.role}>"


def caller(headers: Mapping[str, Any] | None = None) -> Caller:
    """Read the role off the request headers this app is serving.

    Never raises. Off-platform, or on a request that did not come through the
    gateway, the role is ``unknown`` and every ``at_least`` is False.

    ``headers`` is any mapping — Flask's ``request.headers``, Starlette's, a
    plain dict. The lookup is case-insensitive either way, because a framework's
    own header type is where a case-sensitive read becomes a bug on somebody
    else's stack.
    """
    return Caller(verify_role(os.environ.get(ENV_ROLE_KEY, ""), _header(headers, ROLE_HEADER), time.time()))


def _header(headers: Mapping[str, Any] | None, name: str) -> str:
    if not headers:
        return ""
    getter = getattr(headers, "get", None)
    if getter is not None:
        value = getter(name) or getter(name.lower())
        if value:
            return value if isinstance(value, str) else str(value)
    try:
        items = headers.items()
    except AttributeError:
        return ""
    wanted = name.lower()
    for key, value in items:
        if str(key).lower() == wanted and value:
            if isinstance(value, (list, tuple)):
                value = value[0] if value else ""
            return value if isinstance(value, str) else str(value)
    return ""


def verify_role(key: str, blob: str, now: float) -> str:
    """The app side of the platform's mint, and the two are one specification.

    ``v1.<role>.<base64url app id>.<expiry>.<mac>``, where the MAC is
    HMAC-SHA256 over ``b"toft-role\\x00"`` plus the payload, keyed with
    TOFT_ROLE_KEY, base64url without padding.

    The app id is not checked here and does not need to be: the key is derived
    per app, so a blob minted for another app fails the MAC. Checking it as well
    would need this process to know its own id — a second thing to get wrong for
    no property gained.

    Public for the test that compares this MAC against the platform's over the
    same input. App code calls :func:`caller`.
    """
    if not key or not blob:
        return ROLE_UNKNOWN
    payload, sep, sig = blob.rpartition(".")
    if not sep:
        return ROLE_UNKNOWN

    mac = hmac.new(key.encode(), b"toft-role\x00" + payload.encode(), hashlib.sha256).digest()
    want = base64.urlsafe_b64encode(mac).decode().rstrip("=")
    # Constant time, and before anything is parsed: structure decoded ahead of
    # authentication is how a verifier grows a second entry point.
    if not hmac.compare_digest(sig, want):
        return ROLE_UNKNOWN

    fields = payload.split(".")
    if len(fields) != 4 or fields[0] != _ROLE_VERSION:
        return ROLE_UNKNOWN
    try:
        expiry = int(fields[3])
    except ValueError:
        return ROLE_UNKNOWN
    if expiry <= now:
        return ROLE_UNKNOWN

    # A word this SDK does not know is unknown rather than passed through. The
    # app holds this key and could sign anything with it; app code branching on
    # six values must not be handed a seventh, whoever minted it.
    role = fields[1]
    if role in _PERSON_RANK or role in (ROLE_SYSTEM, ROLE_APP):
        return role
    return ROLE_UNKNOWN
