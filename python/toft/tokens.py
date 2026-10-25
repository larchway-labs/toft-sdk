"""A file connection's own access token, for Microsoft's or Google's SDK.

Where an administrator has switched on *Let apps get access tokens* on the
connection attached to this app, the app can call Graph or Drive directly and
hold no secret::

    token = toft.access_token("sharepoint")
    requests.get(f"https://graph.microsoft.com/v1.0/drives/{token.folder.drive_id}"
                 f"/items/{token.folder.item_id}/children",
                 headers={"Authorization": f"Bearer {token.value}"})

or, with Microsoft's own library::

    from msgraph import GraphServiceClient
    client = GraphServiceClient(toft.azure_credential("sharepoint"))

**The folder is a hint, not a fence.** The token reaches everything the
connection's grant reaches; ``folder`` only says where this app was meant to
work, so its code names no site.

Tokens are cached per kind and fetched again five minutes before they expire,
so calling this before every request costs one request to Toft an hour.
"""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from .session import transport

# How long before expiry a cached token is replaced.
REFRESH_MARGIN = 5 * 60


@dataclass(frozen=True)
class Folder:
    """Where this app was attached, in the provider's own ids.

    ``drive_id`` is Graph's and empty for Google Drive.
    """

    item_id: str = ""
    label: str = ""
    drive_id: str = ""


@dataclass(frozen=True)
class Token:
    """One access token and what it is for."""

    value: str
    #: When the provider stops accepting it, in UTC.
    expires_at: datetime
    #: The API it is for — ``https://graph.microsoft.com`` or
    #: ``https://www.googleapis.com``.
    resource: str = ""
    folder: Folder = field(default_factory=Folder)

    @classmethod
    def from_json(cls, raw: Any) -> "Token":
        raw = raw or {}
        stamp = str(raw.get("expires_at", ""))
        expires = datetime.fromisoformat(stamp.replace("Z", "+00:00")) if stamp else (
            datetime.now(timezone.utc)
        )
        folder = raw.get("folder") or {}
        return cls(
            value=str(raw.get("access_token", "")),
            expires_at=expires,
            resource=str(raw.get("resource", "")),
            folder=Folder(
                item_id=str(folder.get("item_id", "")),
                label=str(folder.get("label", "")),
                drive_id=str(folder.get("drive_id", "")),
            ),
        )

    def __repr__(self) -> str:
        # Never the value: a token in a log line is a token anybody reading the
        # log holds for the next hour.
        return f"Token(resource={self.resource!r}, expires_at={self.expires_at.isoformat()})"


_cache: dict[str, Token] = {}
_lock = threading.Lock()


def access_token(kind: str) -> Token:
    """The attached ``kind`` connection's token: ``sharepoint``, ``onedrive`` or
    ``gdrive``.

    Raises :class:`toft.ToftError` with the platform's own words when the
    connection does not hand out tokens, or the token would carry permissions
    beyond files.
    """
    with _lock:
        cached = _cache.get(kind)
        if cached and cached.expires_at.timestamp() - time.time() > REFRESH_MARGIN:
            return cached
    token = Token.from_json(transport().post_json(f"/api/v1/app/tokens/{kind}"))
    with _lock:
        _cache[kind] = token
    return token


class _AccessToken:
    """``azure.core.credentials.AccessToken`` by shape: a token and its expiry."""

    def __init__(self, token: str, expires_on: int) -> None:
        self.token = token
        self.expires_on = expires_on

    def __iter__(self):
        return iter((self.token, self.expires_on))


class AzureCredential:
    """``azure.core.credentials.TokenCredential`` by shape, with no import of
    ``azure-core``: Microsoft's libraries call ``get_token`` and get Toft's.

    The scopes are ignored. The token is for Graph with whatever the connection
    was granted, and nothing an app asks for changes that.
    """

    def __init__(self, kind: str) -> None:
        self.kind = kind

    def get_token(self, *scopes: str, **kwargs: Any) -> _AccessToken:
        token = access_token(self.kind)
        return _AccessToken(token.value, int(token.expires_at.timestamp()))

    def close(self) -> None:
        pass

    def __enter__(self) -> "AzureCredential":
        return self

    def __exit__(self, *exc: Any) -> None:
        pass


def azure_credential(kind: str) -> AzureCredential:
    """A credential Microsoft's Python libraries accept, for ``sharepoint`` or
    ``onedrive``."""
    return AzureCredential(kind)


def _reset() -> None:
    """Forget every cached token. For tests."""
    with _lock:
        _cache.clear()
