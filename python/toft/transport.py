"""The HTTP transport for ``/api/v1/app/*``.

Standard library only, on purpose. This runs inside somebody's app beside their
own dependency tree, and a version conflict between the SDK's HTTP library and
theirs would be our bug appearing in their build.

Two things here are worth reading before changing them.

**The retry is exactly one, and it is not a retry policy.** The platform's own
queue does the real work (one retry against the transport, then the send is
recorded failed). This retry covers the far narrower case of the request never
reaching a handler: a 429 from the rate limiter, or a 5xx from in front of it.
Anything more here would be a second source of truth about how a send behaves,
and the record in the platform — not this client — is what a workflow relies on.

**A retried request reuses its idempotency key**, which is the whole reason the
key is chosen by the caller of :meth:`send` rather than generated per attempt.
Without that, this retry is precisely the thing that sends the weekly report
twice.
"""

from __future__ import annotations

import json
import mimetypes
import os
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from collections.abc import Mapping
from typing import Any, Iterable

from .env import APP_CONTRACT_VERSION, CONTRACT_HEADER, ENV_API_URL, ENV_APP_TOKEN, app_or_job_token, require
from .errors import ToftError

DEFAULT_TIMEOUT = 30.0

# How long to wait before the single retry when the platform did not say.
DEFAULT_RETRY_AFTER = 2.0

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
            raise ToftError(f"cannot read {p}: {exc.strerror or exc}") from None
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
            if isinstance(content, str):
                content = content.encode()
            return cls(field, str(name), content)
        if isinstance(value, Mapping):
            content = value.get("content", b"")
            if isinstance(content, str):
                content = content.encode()
            return cls(
                field,
                str(value.get("name", "attachment")),
                content,
                str(value.get("type", "")),
            )
        raise ToftError(
            "an attachment must be a path, a (filename, bytes) pair, or a "
            f"mapping with 'name' and 'content' — got {type(value).__name__}"
        )


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
        data = json.dumps(body).encode() if body is not None else b"{}"
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
        data = json.dumps(body).encode() if body is not None else b"{}"
        return self._attempt(
            "PUT", path, data, "application/json", dict(headers or {}), self._timeout
        )

    def delete(self, path: str, headers: Mapping[str, str] | None = None) -> Any:
        return self._attempt("DELETE", path, None, None, dict(headers or {}), self._timeout)

    def follow(self, path: str, headers: Mapping[str, str] | None = None) -> bytes:
        """GET a route that redirects to a store, and return the bytes.

        The redirect is followed by hand, deliberately. ``urllib`` copies the
        original request's headers onto a redirected one, which would hand this
        app's platform token to whatever host the object store lives on — a
        credential leak in exchange for three lines saved. The second request
        carries no Toft headers at all, because a presigned URL is already
        the whole authorization.

        Returns ``b""`` when the platform says there is nothing to fetch; a
        refusal it can explain (an expired artifact, a capability that is off)
        raises with the platform's own words.
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
            if exc.code == 404:
                return b""
            if exc.code not in (301, 302, 303, 307, 308):
                raise _from_http_error(exc) from None
            location = exc.headers.get("Location", "") if exc.headers else ""
            if not location:
                raise ToftError("the platform redirected without a location") from None
        except urllib.error.URLError as exc:
            raise ToftError(
                f"cannot reach Toft at {self._base}: {exc.reason}"
            ) from None

        try:
            with urllib.request.urlopen(location, timeout=UPLOAD_TIMEOUT) as resp:
                return resp.read()
        except urllib.error.HTTPError as exc:
            raise ToftError(
                f"fetching the object: {exc.reason}", status=exc.code
            ) from None
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
        try:
            return self._once(method, path, data, content_type, headers, timeout)
        except ToftError as exc:
            if not exc.retryable:
                raise
            time.sleep(exc.retry_after if exc.retry_after is not None else DEFAULT_RETRY_AFTER)
            return self._once(method, path, data, content_type, headers, timeout)

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
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                payload = resp.read()
                return json.loads(payload) if payload else None
        except urllib.error.HTTPError as exc:
            raise _from_http_error(exc) from None
        except urllib.error.URLError as exc:
            raise ToftError(
                f"cannot reach Toft at {self._base}: {exc.reason}"
            ) from None


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
