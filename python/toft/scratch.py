"""Ephemeral scratch storage.

Toft gives every app a scratch directory — ``TOFT_SCRATCH_DIR``,
with ``TMPDIR`` pointed at the same place — that is wiped on every start,
restart, and redeploy. It is per-app, never backed up, and never shared with
another app: the right home for uploaded files being processed, intermediate
results, and anything else that must not outlive the running app. Durable
data belongs on a mapped storage resource (``TOFT_STORAGE``), not here.

Frameworks with their own session machinery (Streamlit, Shiny) already keep
one viewer's uploads away from another's and need none of this. Hand-rolled
Flask/FastAPI apps do: the proxy sends an opaque per-login id in the
``X-Toft-Session`` header, and :func:`session_scratch_dir` turns it into
a directory, so two browser logins working in the same app do not collide.

**Anonymous viewers carry no header, so the app mints its own id** and hands
it back in a cookie of its own. Substituting a constant — ``"anon"``, or any
other fixed string — is the one thing this must not do: it puts every
anonymous viewer of a public app in the same directory, where one viewer's
upload overwrites another's and the analysis reads back the wrong file::

    from toft import new_session_id, session_scratch_dir

    VIEWER_SID = "viewer_sid"   # the app's own cookie; never a platform name

    @app.post("/upload")
    def upload():
        sid = (request.headers.get("X-Toft-Session")
               or request.cookies.get(VIEWER_SID)
               or new_session_id())
        workdir = session_scratch_dir(sid)
        (workdir / upload.filename).write_bytes(upload.read())

        response = make_response(...)
        response.set_cookie(VIEWER_SID, sid, httponly=True, samesite="Lax", secure=True)
        return response

Header first, then the app's cookie: a viewer who signs in part-way through
moves to the login's directory and leaves the anonymous one behind, which is
the right way round — anonymous work does not follow somebody into their
account.

The platform id is per browser login, not per tab: two tabs in one browser
share the login cookie and therefore the directory; two browsers or devices
signed in as the same person do not. A framework that has a finer-grained
session id of its own (a websocket connection, say) can pass that instead —
the argument is any app-chosen string.

The per-session split is a convention inside one app process, not a security
boundary: all sessions run under one OS identity, and the app's own code can
list every session's directory. Anything needing a real boundary between
viewers should not be keyed on this id.
"""

from __future__ import annotations

import hashlib
import os
import secrets
import tempfile
from pathlib import Path

from .errors import ToftError

ENV_SCRATCH_DIR = "TOFT_SCRATCH_DIR"

# The request header carrying the viewer's opaque per-login session id.
SESSION_HEADER = "X-Toft-Session"


def scratch_dir(*parts: str) -> Path:
    """The app's scratch directory, created if missing.

    Optional ``parts`` name a subpath, which is created too. Outside
    Toft the system temp directory stands in, so app code runs
    unchanged in local development.
    """
    root = Path(os.environ.get(ENV_SCRATCH_DIR) or tempfile.gettempdir())
    path = root.joinpath(*parts)
    path.mkdir(parents=True, exist_ok=True)
    return path


def new_session_id() -> str:
    """A fresh opaque session id, for a viewer the platform cannot name.

    Anonymous and API-key requests carry no ``X-Toft-Session``, because
    there is no server-side session to derive one from. An app that still
    wants per-viewer scratch mints one here and carries it in a cookie of its
    own, checked after the header and before this.

    Unguessable on purpose. The directory name is a hash of this string, so
    anyone who can guess an id can name another viewer's directory — which is
    why this is :mod:`secrets` and 128 bits rather than a counter or a
    timestamp.

    Two rules for the cookie that carries it:

    - **Never a name under the ``toft_`` prefix** — that prefix is the
      platform's, and every cookie under it is stripped from a request before
      the app sees it, so an id stored there never survives a round trip and
      every request would silently mint another. Every other name, ``session``
      included, is the app's own.
    - ``HttpOnly``, ``SameSite=Lax``, and ``Secure`` wherever the app is
      served over https.
    """
    return secrets.token_urlsafe(16)


def session_scratch_dir(session_id: str, *parts: str) -> Path:
    """A per-session directory under :func:`scratch_dir`, created if missing.

    ``session_id`` is whatever the app read from the ``X-Toft-Session``
    header — or any string of its own choosing, such as a framework's finer
    per-connection session id. It is hashed before it becomes a directory
    name, so a malformed or hostile value cannot traverse outside the
    scratch root.
    """
    if not session_id:
        # Coded like every other refusal the SDK makes before a request, which
        # is what the skill promises; a bare ValueError carried no code.
        raise ToftError(
            "session_scratch_dir needs a non-empty session id — an empty one "
            "would silently collapse every session into the same directory",
            body={"code": "bad_request", "retryable": False},
        )
    encoded = hashlib.sha256(session_id.encode()).hexdigest()[:32]
    return scratch_dir(encoded, *parts)
