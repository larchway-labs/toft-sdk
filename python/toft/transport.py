"""The HTTP transport for ``/api/v1/app/*``.

Standard library only, on purpose. This runs inside somebody's app beside their
own dependency tree, and a version conflict between the SDK's HTTP library and
theirs would be our bug appearing in their build.

Two things here are worth reading before changing them.

**The retry is exactly one, and it is not a retry policy.** The platform's own
queue does the real work (one retry against the transport, then the send is
recorded failed). This retry covers the far narrower case of a request that
never reached a handler — a 429 from the rate limiter, a connection that never
carried it — or one harmless to deliver twice: a GET, or a write with an
Idempotency-Key. See :func:`_resendable`.
Anything more here would be a second source of truth about how a send behaves,
and the record in the platform — not this client — is what a workflow relies on.

**A retried request reuses its idempotency key**, which is the whole reason the
key is chosen by the caller of :meth:`send` rather than generated per attempt.
Without that, this retry is precisely the thing that sends the weekly report
twice.
"""

from __future__ import annotations

import http.client
import json
import mimetypes
import os
import random
import socket
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from collections.abc import Mapping
from typing import Any, Iterable

from .env import APP_CONTRACT_VERSION, CONTRACT_HEADER, ENV_API_URL, ENV_APP_TOKEN, app_or_job_token, require
from .errors import ToftError, bad_request, local_file_error, refused, store_refused

DEFAULT_TIMEOUT = 30.0

# How long to wait before the single retry when the platform did not say.
DEFAULT_RETRY_AFTER = 2.0
# Added to each wait on a 429, so a burst of refused calls does not come back
# at one instant and meet a single free token together.
RATE_LIMIT_JITTER = 0.5

# An upload can be much slower than a JSON call and timing one out at 30s turns
# a working 20 MiB report into a mystery.
UPLOAD_TIMEOUT = 300.0


class Part:
    """One file in a multipart request."""

    __slots__ = ("field", "filename", "content", "content_type")

    def __init__(
        self,
        field: str,
        filename: str,
        content: bytes,
        content_type: str = "",
    ) -> None:
        self.field = field
        # Reduced to a basename here as well as on the platform. Not because
        # this side is trusted — it is not, and the platform sanitizes again —
        # but because a caller passing "/tmp/x/report.xlsx" means the file, and
        # silently mailing a filename with somebody's directory layout in it is
        # a small disclosure nobody chose.
        self.filename = os.path.basename(filename) or "attachment"
        self.content = content
        self.content_type = (
            content_type
            or mimetypes.guess_type(self.filename)[0]
            or "application/octet-stream"
        )

    @classmethod
    def from_path(cls, field: str, path: str | os.PathLike) -> "Part":
        p = Path(path)
        try:
            content = p.read_bytes()
        except OSError as exc:
            # The caller's file, not the platform: coded and not retryable —
            # not_found for a path that is not there, bad_request otherwise.
            raise local_file_error(p, exc) from None
        return cls(field, p.name, content)

    @classmethod
    def coerce(cls, field: str, value: Any) -> "Part":
        """Accept the shapes a caller naturally reaches for.

        A path, a ``(filename, bytes)`` pair, or a mapping with ``name`` and
        ``content``. Anything else is a clear error rather than a stringified
        object arriving as an attachment called ``<object at 0x…>``.
        """
        if isinstance(value, Part):
            return value
        if isinstance(value, (str, os.PathLike)):
            return cls.from_path(field, value)
        if isinstance(value, tuple) and len(value) == 2:
            name, content = value
            return cls(field, str(name), _content_bytes(content))
        if isinstance(value, Mapping):
            content = _content_bytes(value.get("content", b""))
            return cls(
                field,
                str(value.get("name", "attachment")),
                content,
                str(value.get("type", "")),
            )
        raise bad_request(
            "an attachment must be a path, a (filename, bytes) pair, or a "
            f"mapping with 'name' and 'content' — got {type(value).__name__}",
            hint="pass a file path, (\"report.csv\", data), or {\"name\": \"report.csv\", \"content\": data}",
        )


def _content_bytes(content: Any) -> bytes:
    if isinstance(content, str):
        return content.encode()
    if isinstance(content, (bytes, bytearray, memoryview)):
        return bytes(content)
    raise bad_request(
        f"an attachment's content must be bytes or text — got {type(content).__name__}",
        hint="encode it first — json.dumps(value) for data, or the file's bytes")


def encode_json(body: Any) -> bytes:
    """A request body as JSON, or a coded refusal before anything is sent.

    A value ``json`` cannot encode — an object, a set, a datetime — is the
    caller's to fix, and the raw ``TypeError`` it would otherwise raise carried
    no code and no hint that nothing reached the platform.
    """
    if body is None:
        return b"{}"
    try:
        return json.dumps(body, allow_nan=False).encode()
    except (TypeError, ValueError) as exc:
        raise bad_request(
            f"the request body is not JSON-serializable: {exc}",
            hint="send only dicts, lists, strings, finite numbers, booleans and None; replace NaN and infinities with None or a number") from None


def encode_multipart(fields: Mapping[str, str], parts: Iterable[Part]) -> tuple[bytes, str]:
    """Build a multipart/form-data body. Returns (body, content_type)."""
    boundary = f"----toft{uuid.uuid4().hex}"
    out = bytearray()
    for name, value in fields.items():
        out += f"--{boundary}\r\n".encode()
        out += f'Content-Disposition: form-data; name="{name}"\r\n\r\n'.encode()
        out += str(value).encode()
        out += b"\r\n"
    for part in parts:
        out += f"--{boundary}\r\n".encode()
        out += (
            f'Content-Disposition: form-data; name="{part.field}"; '
            f'filename="{_quote(part.filename)}"\r\n'
        ).encode()
        out += f"Content-Type: {part.content_type}\r\n\r\n".encode()
        out += part.content
        out += b"\r\n"
    out += f"--{boundary}--\r\n".encode()
    return bytes(out), f"multipart/form-data; boundary={boundary}"


def _quote(name: str) -> str:
    return name.replace("\\", "").replace('"', "").replace("\r", "").replace("\n", "")


class AppTransport:
    """Authenticated calls to ``/api/v1/app/*`` as this app.

    Built lazily from the environment on first use, so importing ``toft``
    off-platform never raises — only calling something that needs the platform
    does.
    """

    def __init__(
        self,
        base_url: str | None = None,
        token: str | None = None,
        timeout: float = DEFAULT_TIMEOUT,
    ) -> None:
        self._base = (base_url or require(ENV_API_URL)).rstrip("/")
        self._token = token or app_or_job_token()
        self._timeout = timeout

    def get(self, path: str, headers: Mapping[str, str] | None = None) -> Any:
        return self._attempt("GET", path, None, None, headers or {}, self._timeout)

    def post_json(
        self,
        path: str,
        body: Any = None,
        idempotency_key: str | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> Any:
        data = encode_json(body)
        merged = dict(headers or {})
        merged.update(_idem(idempotency_key))
        return self._attempt(
            "POST", path, data, "application/json", merged, self._timeout
        )

    def put_json(
        self,
        path: str,
        body: Any = None,
        headers: Mapping[str, str] | None = None,
    ) -> Any:
        """PUT a JSON body — one resource replaced, rather than one thing enqueued.

        No idempotency key, deliberately: a PUT replaces the same resource
        whatever the attempt number, so the key that keeps a retried POST from
        starting two runs has nothing to protect here. ``If-Match`` is the
        header that matters instead, and it belongs to the caller.
        """
        data = encode_json(body)
        return self._attempt(
            "PUT", path, data, "application/json", dict(headers or {}), self._timeout
        )

    def delete(self, path: str, headers: Mapping[str, str] | None = None) -> Any:
        return self._attempt("DELETE", path, None, None, dict(headers or {}), self._timeout)

    def follow(self, path: str, headers: Mapping[str, str] | None = None) -> bytes | None:
        """GET a route that redirects to a store, and return the bytes.

        The redirect is followed by hand, deliberately. ``urllib`` copies the
        original request's headers onto a redirected one, which would hand this
        app's platform token to whatever host the object store lives on — a
        credential leak in exchange for three lines saved. The second request
        carries no Toft headers at all, because a presigned URL is already
        the whole authorization.

        Returns ``None`` when the platform says there is nothing to fetch
        (``no_output``) — distinct from ``b""``, an object that is there and
        empty. Every other refusal, a 404 for a run that does not exist
        included, raises with the platform's own words.
        """
        req = urllib.request.Request(f"{self._base}{path}", method="GET")
        req.add_header("Authorization", f"Bearer {self._token}")
        req.add_header(CONTRACT_HEADER, APP_CONTRACT_VERSION)
        for name, value in (headers or {}).items():
            req.add_header(name, value)

        opener = urllib.request.build_opener(_NoRedirects)
        try:
            with opener.open(req, timeout=self._timeout) as resp:
                # No redirect: the route answered directly.
                return resp.read()
        except urllib.error.HTTPError as exc:
            if exc.code not in (301, 302, 303, 307, 308):
                err = _from_http_error(exc)
                if err.status == 404 and err.code == "no_output":
                    return None
                raise err from None
            location = exc.headers.get("Location", "") if exc.headers else ""
            if not location:
                raise refused("unexpected_response",
                              "the platform redirected without a location",
                              REDIRECT_HINT) from None
        except urllib.error.URLError as exc:
            raise ToftError(
                f"cannot reach Toft at {self._base}: {exc.reason}"
            ) from None

        try:
            with urllib.request.urlopen(location, timeout=UPLOAD_TIMEOUT) as resp:
                return resp.read()
        except urllib.error.HTTPError as exc:
            raise store_refused("output_unreadable",
                                f"fetching the object: {exc.code} {exc.reason}", exc.code) from None
        except urllib.error.URLError as exc:
            raise ToftError(f"fetching the object: {exc.reason}") from None

    def post_multipart(
        self,
        path: str,
        fields: Mapping[str, str],
        parts: Iterable[Part],
        idempotency_key: str | None = None,
    ) -> Any:
        body, content_type = encode_multipart(fields, list(parts))
        return self._attempt(
            "POST", path, body, content_type, _idem(idempotency_key), UPLOAD_TIMEOUT
        )

    def _attempt(
        self,
        method: str,
        path: str,
        data: bytes | None,
        content_type: str | None,
        headers: Mapping[str, str],
        timeout: float,
    ) -> Any:
        # One bound for the whole call, every attempt and the waits between
        # them: "every call gives up after 30 s" is the promise.
        #
        # The rate limiter's 429 is the exception to "one resend": it answers
        # before any handler runs, so it is waited out — Retry-After plus a
        # jitter — for as long as the bound allows. A single resend paced calls
        # made one at a time but refused a concurrent burst: every refused call
        # came back at the same instant, one token was free, and the rest were
        # raised (TOFT-BB-002).
        deadline = time.monotonic() + timeout
        try:
            return self._once(method, path, data, content_type, headers, timeout)
        except ToftError as exc:
            if not _resendable(method, headers, exc):
                raise
            err = exc
        while True:
            limited = err.status == 429
            wait = err.retry_after if err.retry_after is not None else DEFAULT_RETRY_AFTER
            if limited:
                wait += random.random() * RATE_LIMIT_JITTER
            if deadline - time.monotonic() <= wait:
                raise err
            time.sleep(wait)
            try:
                return self._once(method, path, data, content_type, headers,
                                  max(deadline - time.monotonic(), 0.001))
            except ToftError as exc:
                if not limited or exc.status != 429 or not _resendable(method, headers, exc):
                    raise
                err = exc

    def _once(
        self,
        method: str,
        path: str,
        data: bytes | None,
        content_type: str | None,
        headers: Mapping[str, str],
        timeout: float,
    ) -> Any:
        req = urllib.request.Request(f"{self._base}{path}", data=data, method=method)
        req.add_header("Authorization", f"Bearer {self._token}")
        req.add_header(CONTRACT_HEADER, APP_CONTRACT_VERSION)
        if content_type:
            req.add_header("Content-Type", content_type)
        for name, value in headers.items():
            req.add_header(name, value)

        try:
            with _open_without_redirects(req, timeout) as resp:
                payload = resp.read()
                status = resp.status
                html = (resp.headers.get("Content-Type") or "").lower().startswith("text/html")
        except urllib.error.HTTPError as exc:
            raise _from_http_error_or_redirect(exc, path) from None
        except urllib.error.URLError as exc:
            # urllib wraps what failed while connecting and writing the
            # request; nothing reached a handler — unless it timed out, which
            # spends the call's bound whichever side it happened on.
            timed_out = isinstance(exc.reason, (TimeoutError, socket.timeout))
            raise _transport_error(
                f"Toft at {self._base} did not answer within {timeout:g}s" if timed_out
                else f"cannot reach Toft at {self._base}: {exc.reason}",
                sent=timed_out, timed_out=timed_out) from None
        except (TimeoutError, socket.timeout):
            raise _transport_error(f"Toft at {self._base} did not answer within {timeout:g}s",
                                   sent=True, timed_out=True) from None
        except (OSError, http.client.HTTPException) as exc:
            # Raised while reading the answer: the request was on the wire,
            # and a handler may have acted on it.
            raise _transport_error(f"cannot reach Toft at {self._base}: {exc}", sent=True) from None
        if not payload.strip():
            return None
        try:
            return json.loads(payload)
        except ValueError:
            raise _not_toft(self._base, status, html or payload.lstrip().startswith(b"<")) from None


def _transport_error(message: str, *, sent: bool, timed_out: bool = False) -> ToftError:
    """A request that got no answer: uncoded and retryable, with what the
    resend rule needs to know about how far it got."""
    err = ToftError(message)
    err._sent = sent
    err._timed_out = timed_out
    return err


def _resendable(method: str, headers: Mapping[str, str], exc: ToftError) -> bool:
    """Whether the single built-in resend fires.

    **Only a request that cannot have reached a handler, or one harmless to
    deliver twice.** A 429 is the rate limiter, which answers before any
    handler runs, and a connection that never carried the request delivered
    nothing: both are resent whatever the method. A 5xx — which a handler may
    have written after doing the work — or a connection lost once the request
    was on the wire is resent only for a GET, or a write carrying an
    Idempotency-Key the platform deduplicates on. A POST with no key, a PUT or a
    DELETE is not. A timeout is never resent: the call's bound is spent. The
    platform's own ``retryable: false`` vetoes all of it.
    """
    idempotent = method == "GET" or bool(headers.get("Idempotency-Key"))
    if exc.status is None:
        if exc.code or getattr(exc, "_timed_out", False) or not hasattr(exc, "_sent"):
            return False
        return not exc._sent or idempotent
    if not exc._auto_retry:
        return False
    return exc.status == 429 or idempotent


def _not_toft(base: str, status: int, html: bool) -> ToftError:
    """A success whose body is not JSON — a web page, most often: the address
    names some other server, or a proxy or sign-in page in front of the install.
    Coded and not retryable, as the CLI's ``not_toft`` is: the same address
    answers the same page next time."""
    what = "a web page" if html else "a body that is not JSON"
    return ToftError(
        f"{base} answered {status} with {what}, not Toft's API — is {ENV_API_URL} this install's address?",
        status=status,
        body={"code": "not_toft", "retryable": False,
              "hint": f"{ENV_API_URL} is set by Toft for every app and job — check it names the install's "
                      "own API, not another server or a proxy or sign-in page in front of it"})


def _open_without_redirects(req: urllib.request.Request, timeout: float):
    """Open a platform request with redirects refused.

    ``urlopen``'s default opener follows a 3xx and copies the original
    request's headers onto the next one — the bearer token and the act-for
    blob included, to whatever host the Location names. Only :meth:`follow`
    has a redirect to deal with, and it deals with it by hand.
    """
    return urllib.request.build_opener(_NoRedirects).open(req, timeout=timeout)


#: The next step for a redirect the SDK will not follow — R's words, in all four.
REDIRECT_HINT = ("TOFT_API_URL should be the address Toft set for this app; a redirect usually "
                 "means it points at a page behind sign-in or at the wrong host")


def _from_http_error_or_redirect(exc: urllib.error.HTTPError, path: str) -> ToftError:
    """A refusal in the platform's words, or a redirect named as one.

    A redirect on an ordinary call is the platform answering a question this
    method did not ask, as the Go SDK says.
    """
    if 300 <= exc.code < 400:
        return ToftError(f"unexpected redirect from {path}", status=exc.code,
                         body={"code": "unexpected_redirect", "retryable": False,
                               "hint": REDIRECT_HINT})
    return _from_http_error(exc)


class _NoRedirects(urllib.request.HTTPRedirectHandler):
    """Turns a redirect into an HTTPError so the caller can inspect it.

    See :meth:`AppTransport.follow` for why following one automatically is the
    wrong default here.
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _idem(key: str | None) -> dict[str, str]:
    return {"Idempotency-Key": key} if key else {}


def _from_http_error(exc: urllib.error.HTTPError) -> ToftError:
    """Turn the platform's error body into the message a developer reads.

    Refusals from ``/api/v1/app/*`` are written for a person and name the
    remedy — "email is not enabled for this app; an administrator can enable it
    on the app's Integrations tab". Flattening that into "403 Forbidden" throws
    away the only part that helps.
    """
    detail = ""
    body: dict = {}
    try:
        raw = exc.read().decode(errors="replace")
        try:
            parsed = json.loads(raw)
            if isinstance(parsed, dict):
                body = parsed
            detail = body.get("error", raw)
        except Exception:
            detail = raw
    except Exception:
        pass

    retry_after: float | None = None
    header = exc.headers.get("Retry-After") if exc.headers else None
    if header:
        try:
            retry_after = float(header)
        except ValueError:
            retry_after = None

    return ToftError(
        detail or exc.reason, status=exc.code, retry_after=retry_after, body=body)
