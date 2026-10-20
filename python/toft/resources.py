"""Durable storage, resolved by name.

``TOFT_STORAGE`` is a JSON array of the resources an administrator mapped
to this app. App code resolves a resource by the **name** it was given, never
by the path it happens to be mounted at today — that is what lets an
administrator move a resource without every app that uses it breaking, and
hard-coding ``/data/reports`` is exactly what this exists to stop::

    reports = toft.storage("reports")
    (reports.path / "2026-08.xlsx").write_bytes(data)

A miss names what *is* mounted. "no such resource" on its own sends somebody to
their administrator to ask a question the process could already answer.

**A store is also reached over signed URLs**, through ``list``, ``upload``,
``download``, ``url`` and ``remove`` — in both execution modes, so an app
written against them moves between modes unchanged. On a shared host a store is
never mounted at all: its entry carries no path, ``.path`` raises, and these
are the whole of how the app reaches it::

    uploads = toft.store("uploads")
    uploads.upload("report.csv")                    # a new object, or a refusal
    link = uploads.url("report.csv", attachment=True)  # hand to a browser
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .env import ENV_STORAGE
from .errors import ToftError
from .session import transport
from .transport import UPLOAD_TIMEOUT


@dataclass(frozen=True)
class Mount:
    """One storage resource mapped to this app."""

    name: str
    kind: str = ""
    # The path as the platform described it, and "" for a store that is not
    # mounted here. Read through ``path``, which refuses rather than handing
    # back ``Path("")`` — that is the current directory, and joining a file name
    # onto it writes into the app's own tree.
    mount_path: str = ""
    access: str = ""
    # Which directory of the resource this mount is rooted at, and "" is the
    # whole resource. It is here because **two mounts of one resource share a
    # name**, and the name is how app code locates a resource — so this is what
    # tells them apart. It is deliberately not part of ``path``: where a mount
    # appears stays the administrator's choice.
    folder: str = ""

    @property
    def read_only(self) -> bool:
        return self.access == "read"

    @property
    def mounted(self) -> bool:
        """Whether this resource is on this workload's filesystem at all."""
        return self.mount_path != ""

    @property
    def path(self) -> Path:
        if not self.mounted:
            raise ToftError(
                f"{self.name!r} is not mounted here — on this install the app reaches it "
                f"through the platform: use list(), upload(), download() or url()"
            )
        return Path(self.mount_path)

    # --- Over the platform's signed URLs, in both modes ---

    def list(self, path: str = "", cursor: str = "") -> "Listing":
        """One page of what is under ``path``, folders first-class.

        Names are relative to ``path``. ``next_cursor`` is empty on the last
        page; pass it back to read the next one.
        """
        if path:
            _check_relative_path(path)
        query = {"path": path, "cursor": cursor}
        page = transport().get(self._route("/api/v1/app/store/{slug}/objects", query=query)) or {}
        return Listing(
            objects=list(page.get("objects") or []),
            prefixes=list(page.get("prefixes") or []),
            next_cursor=str(page.get("next_cursor") or ""),
            access=str(page.get("access") or ""),
        )

    def upload(self, source: "str | Path", name: str | None = None) -> str:
        """Upload a local file as ``name`` (its own name by default).

        At ``write`` this creates a new object only, and an existing name is
        a refusal that says so; ``overwrite`` and ``full`` replace. The bytes go
        straight to the object store — the platform signs, and never sees them.
        """
        src = Path(source)
        target = name or src.name
        _check_relative_name(target)
        data = src.read_bytes()
        signed = transport().post_json(self._route("/api/v1/app/store/{slug}/objects"), {
            "name": target, "size": len(data),
            "sha256": hashlib.sha256(data).hexdigest(),
            "md5": hashlib.md5(data).hexdigest(),  # noqa: S324 — the store's integrity check
        })
        try:
            _send(signed, data)
        except ToftError as exc:
            if exc.status == 412:
                raise ToftError(
                    f"{target!r} already exists in {self.name!r}, and this app's access "
                    f"({self.access}) creates new objects only", status=412) from None
            raise
        return target

    def download(self, name: str, dest: "str | Path | None" = None) -> "bytes | Path":
        """Fetch one object: its bytes, or written to ``dest`` and that path returned."""
        data = _send(self._sign_get(name), None)
        if dest is None:
            return data
        out = Path(dest)
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_bytes(data)
        return out

    def url(self, name: str, attachment: bool = False) -> str:
        """A signed GET for one object, to hand to a browser.

        It lives fifteen minutes and anyone holding it reads that object until
        then — which is the point for a download link, and the reason not to log
        it. ``attachment`` makes a browser save it under the object's own name.
        """
        return str(self._sign_get(name, attachment)["url"])

    def remove(self, name: str) -> None:
        """Delete one object. Needs ``full``."""
        _check_relative_name(name)
        signed = transport().delete(self._route("/api/v1/app/store/{slug}/objects/{name}", name))
        _send(signed, None)

    def _sign_get(self, name: str, attachment: bool = False) -> dict:
        _check_relative_name(name)
        query = {"disposition": "attachment"} if attachment else {}
        return transport().get(self._route("/api/v1/app/store/{slug}/objects/{name}", name, query)) or {}

    def _route(self, route: str, name: str = "", query: "dict[str, str] | None" = None) -> str:
        """``route`` is the platform's route, spelled whole so a reader — and the
        SDK contract test — sees which one each call reaches."""
        if self.kind and self.kind != "store":
            raise ToftError(
                f"{self.name!r} is a {self.kind}, which is reached at its path — only an "
                f"object store is reached over signed URLs")
        # The folder always travels, "" included: it names *this* mapping, so a
        # resource mapped twice is never a question the platform has to ask back.
        params = {"folder": self.folder}
        params.update({k: v for k, v in (query or {}).items() if v})
        path = route.replace("{slug}", urllib.parse.quote(self.name, safe=""))
        path = path.replace("{name}", _quote_name(name))
        return f"{path}?{urllib.parse.urlencode(params)}"

    def put(self, source: "str | Path", name: str | None = None) -> Path:
        """Copy a local file into this mount, and return where it landed.

        Sugar over the mounted path and nothing more::

            toft.store("reports").put("nightly.parquet", "2026-08.parquet")

        An app that has the store mounted already has the folder on its
        filesystem; this saves it from joining paths, and calls nothing. Where
        the store is not mounted — on an install with no cluster — this refuses,
        and :meth:`upload` reaches it through the platform instead, at the same
        level, under the same mapping.

        The access level is checked here only for a better message. **The mount
        is the enforcement**: Mountpoint refuses an unsupported operation, so an
        app that tries to replace an object it was granted `write` on fails at
        the first wrong call rather than corrupting data slowly.
        """
        src = Path(source)
        target_name = name or src.name
        _check_relative_name(target_name)

        if not self.mounted:
            raise ToftError(
                f"{self.name!r} is not mounted here, so put() has nowhere to copy to — "
                f"use upload(), which reaches it through the platform"
            )
        if self.read_only:
            raise ToftError(
                f"{self.name!r} is mounted read-only, so this app cannot write to it. "
                f"An administrator sets the access level where the resource is mapped."
            )

        destination = self.path / target_name
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(src, destination)
        return destination


@dataclass(frozen=True)
class Listing:
    """One page of a store's listing."""

    objects: "list[dict[str, Any]]" = field(default_factory=list)
    prefixes: "list[str]" = field(default_factory=list)
    next_cursor: str = ""
    access: str = ""


def _quote_name(name: str) -> str:
    return urllib.parse.quote(name, safe="/")


def _send(signed: Any, data: "bytes | None") -> bytes:
    """Carry out a signed request against the object store.

    No Toft header goes with it: the URL is the whole authorization, and the
    app's platform token has no business reaching the object store's host. The
    signed headers are sent verbatim, because they are part of the signature.
    """
    if not isinstance(signed, dict) or not signed.get("url"):
        raise ToftError("the platform returned no signed URL")
    req = urllib.request.Request(str(signed["url"]), data=data,
                                 method=str(signed.get("method") or "GET"))
    for key, value in (signed.get("headers") or {}).items():
        req.add_header(str(key), str(value))
    try:
        with urllib.request.urlopen(req, timeout=UPLOAD_TIMEOUT) as resp:
            return resp.read()
    except urllib.error.HTTPError as exc:
        raise ToftError(f"the object store refused the request: {exc.code} {exc.reason}",
                        status=exc.code) from None
    except urllib.error.URLError as exc:
        raise ToftError(f"cannot reach the object store: {exc.reason}") from None


def _check_relative_path(path: str) -> None:
    """A listing path: a relative name, with one trailing slash allowed."""
    _check_relative_name(path[:-1] if path.endswith("/") else path)


def _check_relative_name(name: str) -> None:
    """Refuse a name that would land outside the mount.

    The same discipline the platform applies to an uploaded object's name, for
    the same reason: the name is the caller's and the location is not, so a
    traversal segment here would write into the app's own filesystem — over its
    code, in the worst case — rather than into the storage it was given.
    """
    if not name:
        raise ToftError("a name is required")
    if name.startswith("/") or name.endswith("/"):
        raise ToftError(f"a name is relative to the mount, got {name!r}")
    if "\\" in name:
        raise ToftError(f"a name must use forward slashes, got {name!r}")
    for segment in name.split("/"):
        if segment in ("", ".", ".."):
            raise ToftError(f"a name must not contain {segment!r} segments, got {name!r}")


def mounts() -> list[Mount]:
    """Every resource mapped to this app, in the order the platform listed them.

    That order is the platform's ``ORDER BY m.path``, which is worth knowing
    only because two mounts can share a name: it is stable, and it is not a
    ranking. Ask for the one you mean with ``folder=``.

    Empty off-platform and empty when an administrator has mapped nothing —
    deliberately not an error, because "this app has no durable storage" is a
    normal state that app code can branch on.
    """
    raw = os.environ.get(ENV_STORAGE, "").strip()
    if not raw:
        return []
    try:
        parsed = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ToftError(f"{ENV_STORAGE} is not valid JSON: {exc}") from None
    if not isinstance(parsed, list):
        raise ToftError(f"{ENV_STORAGE} should be a JSON array, got {type(parsed).__name__}")

    out: list[Mount] = []
    for entry in parsed:
        if not isinstance(entry, dict):
            continue
        out.append(
            Mount(
                name=str(entry.get("name", "")),
                kind=str(entry.get("kind", "")),
                mount_path=str(entry.get("path") or ""),
                access=str(entry.get("access", "")),
                folder=str(entry.get("folder", "")),
            )
        )
    return out


def storage(name: str, folder: str | None = None) -> Mount:
    """Resolve one resource by name, and — where it is mapped twice — by folder.

        toft.storage("warehouse").path
        toft.storage("drop", folder="backup").path

    **A name is not always one mount.** A mapping's identity is
    ``(resource, app, folder)``, so an administrator can map one resource twice:
    the root read-only and a folder at ``write``, at two paths in one workload.
    Both arrive under the same name, and ``folder`` is what tells them apart.

    So a name that matches two mounts, with no folder named, **raises rather
    than picking one**. Returning the first was the earlier behaviour and it
    made the choice by the alphabetical order of paths an administrator chose —
    a silent answer to a question the caller did not know they were asking.

    ``folder=None`` means *do not care*, which is the ordinary case and is only
    ambiguous when there is genuinely more than one. ``folder=""`` names the
    root explicitly.
    """
    if not name:
        raise ToftError("storage() needs the name an administrator gave the resource")
    available = mounts()
    named = [m for m in available if m.name == name]

    if folder is not None:
        for mount in named:
            if mount.folder == folder:
                return mount
        if named:
            raise ToftError(
                f"{name!r} is mounted, but not at folder {folder!r}. "
                f"Mounted here: {_folders(named)}"
            )
    elif len(named) == 1:
        return named[0]
    elif len(named) > 1:
        raise ToftError(
            f"{len(named)} mounts are named {name!r} — {_folders(named)}. "
            f"Name one: toft.storage({name!r}, folder={named[0].folder!r})"
        )

    if not available:
        raise ToftError(
            f"no storage resource named {name!r}, and this app has none mapped at "
            f"all — an administrator maps storage on the app's Storage tab"
        )
    names = ", ".join(sorted(m.name for m in available)) or "(none)"
    raise ToftError(f"no storage resource named {name!r}. Mapped to this app: {names}")


def _folders(named: list[Mount]) -> str:
    """The folders one name is mounted at, with the level of each.

    The level is in the message because it is usually the reason there are two:
    a read corpus and a write drop are the shape this feature exists for, and a
    caller who picked the wrong one wants to see that immediately.
    """
    return ", ".join(
        f"folder {m.folder!r} at {m.access or 'read'}" for m in named
    )


def store(name: str, folder: str | None = None) -> Mount:
    """Resolve one **store** by name.

    A kind-checked alias of :func:`storage`. Uploads are a store feature end to
    end — the CLI command is ``toft store``, the grant table refuses a volume, and
    this is the accessor that matches — so a reader never has to remember which
    kinds are in scope, because the name is the kind.

    ``storage()`` keeps answering for both kinds, because app code that has a
    volume mounted has done nothing wrong.
    """
    mount = storage(name, folder)
    if mount.kind and mount.kind != "store":
        raise ToftError(
            f"{name!r} is a {mount.kind}, not a store. A volume is a real filesystem — "
            f"write to toft.storage({name!r}).path directly, with the ordinary file APIs."
        )
    return mount
