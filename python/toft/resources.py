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
from .errors import ToftError, local_file_error, store_refused
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
    # The level the mapping had when this process started (TOFT_STORAGE). A
    # store reached over signed URLs is authorized per request against the
    # mapping as it is *now*, so after an administrator changes the level the
    # platform's answer moves at once and this value only at the next start.
    access: str = ""
    # Which directory of the resource this mount is rooted at, and "" is the
    # whole resource. It is here because **two mounts of one resource share a
    # name**, and the name is how app code locates a resource — so this is what
    # tells them apart. It is deliberately not part of ``path``: where a mount
    # appears stays the administrator's choice.
    folder: str = ""

    def __repr__(self) -> str:
        # Named for the attribute app code reads — ``path`` — rather than the
        # field behind it; "" where the resource is not mounted here, as
        # to_dict() and the other three SDKs say it (BB-096).
        return (f"Mount(name={self.name!r}, kind={self.kind!r}, path={self.mount_path!r}, "
                f"access={self.access!r}, folder={self.folder!r}, "
                f"read_only={self.read_only!r}, mounted={self.mounted!r})")

    def to_dict(self) -> dict:
        """The mount under the names Node, Go and R serialise it with —
        ``path`` ("" where not mounted here), ``read_only`` and ``mounted``
        among them — so ``json.dumps([m.to_dict() for m in toft.mounts()])``
        renders what the other three print (BB-044 parity)."""
        return {"name": self.name, "kind": self.kind, "path": self.mount_path,
                "access": self.access, "folder": self.folder,
                "read_only": self.read_only, "mounted": self.mounted}

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
            raise _refused("not_mounted",
                f"{self.name!r} is not mounted here — on this install the app reaches it "
                f"through the platform: use list(), upload(), download() or url()",
                hint="use upload(), download() or url() — this install reaches the store "
                     "through the platform")
        return Path(self.mount_path)

    # --- Over the platform's signed URLs, in both modes ---

    def list(self, path: str = "", cursor: str = "") -> "Listing":
        """One page of what is under ``path``, folders first-class.

        Names are relative to ``path``. ``next_cursor`` is empty on the last
        page; pass it back to read the next one.
        """
        if path:
            _check_relative_path(path, "list")
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
        a refusal that says so; ``full`` replaces. The bytes go
        straight to the object store — the platform signs, and never sees them.
        """
        src = _check_source(source, "upload()")
        _check_name_type(name, "upload", optional=True)
        target = name or src.name
        _check_relative_name(target, "upload")
        try:
            data = src.read_bytes()
        except OSError as exc:
            raise local_file_error(src, exc) from None
        signed = transport().post_json(self._route("/api/v1/app/store/{slug}/objects"), {
            "name": target, "size": len(data),
            "sha256": hashlib.sha256(data).hexdigest(),
            "md5": hashlib.md5(data).hexdigest(),  # noqa: S324 — the store's integrity check
        })
        try:
            _send(signed, data)
        except ToftError as exc:
            if exc.status == 412:
                # `write`, not self.access: the store signs at the mapping's
                # level *now*, and only `write` is refused an existing name —
                # the cached level was the one the app started with.
                raise _refused("already_exists",
                    f"{target!r} already exists in {self.name!r}, and this app's access "
                    "there is `write`, which creates new objects only", status=412,
                    hint="upload under a new name, or ask an administrator for `full` access "
                         "where the store is mapped") from None
            raise
        return target

    def download(self, name: str, dest: "str | Path | None" = None) -> "bytes | Path":
        """Fetch one object: its bytes, or written to ``dest`` and that path returned."""
        try:
            data = _send(self._sign_get(name, call="download"), None)
        except ToftError as exc:
            if exc.status == 404:
                # The URL is signed for any name; the store is what answers
                # that nothing is there — the first night of every nightly job.
                raise _refused("not_found",
                    f"{name!r} is not in {self.name!r} — nothing has been put there "
                    "under that name yet", status=404,
                    hint="list() shows the names that are there") from None
            raise
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
        return str(self._sign_get(name, attachment, call="url")["url"])

    def remove(self, name: str) -> None:
        """Delete one object. Needs ``full``."""
        _check_relative_name(name, "remove")
        signed = transport().delete(self._route("/api/v1/app/store/{slug}/objects/{name}", name))
        _send(signed, None)

    def _sign_get(self, name: str, attachment: bool = False, call: str = "download") -> dict:
        _check_relative_name(name, call)
        query = {"disposition": "attachment"} if attachment else {}
        return transport().get(self._route("/api/v1/app/store/{slug}/objects/{name}", name, query)) or {}

    def _route(self, route: str, name: str = "", query: "dict[str, str] | None" = None) -> str:
        """``route`` is the platform's route, spelled whole so a reader — and the
        SDK contract test — sees which one each call reaches."""
        if self.kind and self.kind != "store":
            raise _refused("not_a_store",
                f"{self.name!r} is a {self.kind}, which is reached at its path — only an "
                f"object store is reached over signed URLs",
                hint=f"use {_storage_call(self)}.path with the ordinary file APIs")
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
        src = _check_source(source, "put()")
        _check_name_type(name, "put", optional=True)
        target_name = name or src.name
        _check_relative_name(target_name, "put")

        if not self.mounted:
            raise _refused("not_mounted",
                f"{self.name!r} is not mounted here, so put() has nowhere to copy to — "
                f"use upload(), which reaches it through the platform",
                hint="use upload(), which reaches the store through the platform")
        if self.read_only:
            raise _refused("insufficient_access",
                f"{self.name!r} is mounted read-only, so this app cannot write to it. "
                f"An administrator sets the access level where the resource is mapped.",
                hint="ask an administrator for `write` access where the resource is mapped")

        destination = self.path / target_name
        destination.parent.mkdir(parents=True, exist_ok=True)
        try:
            shutil.copyfile(src, destination)
        except OSError as exc:
            # The source is the caller's file: coded, never retryable. A
            # failure writing the mount is the mount's, and raised as itself.
            if str(exc.filename or "") == str(src):
                raise local_file_error(src, exc) from None
            raise
        return destination


@dataclass(frozen=True)
class Listing:
    """One page of a store's listing.

    Iterating it walks ``objects`` — ``for obj in store.list()`` is the obvious
    spelling and it raised ``TypeError``. Folders are ``prefixes``; the next
    page is ``next_cursor``.
    """

    def __iter__(self) -> "Iterator[dict[str, Any]]":
        return iter(self.objects)

    def __len__(self) -> int:
        return len(self.objects)


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
        raise _refused("unexpected_response", "the platform returned no signed URL")
    req = urllib.request.Request(str(signed["url"]), data=data,
                                 method=str(signed.get("method") or "GET"))
    for key, value in (signed.get("headers") or {}).items():
        req.add_header(str(key), str(value))
    try:
        with urllib.request.urlopen(req, timeout=UPLOAD_TIMEOUT) as resp:
            return resp.read()
    except urllib.error.HTTPError as exc:
        raise store_refused("store_refused",
                            f"the object store refused the request: {exc.code} {exc.reason}",
                            exc.code) from None
    except urllib.error.URLError as exc:
        raise ToftError(f"cannot reach the object store: {exc.reason}") from None


def _refused(code: str, message: str, status: int | None = None,
             hint: str | None = None) -> ToftError:
    """A refusal this SDK makes itself, carrying the platform's envelope.

    None of these is helped by trying again — a mapping, a name or a level is
    wrong — and without a body the status rule would call a statusless error
    retryable, which is the answer for a network failure. So each says so, with
    a code an app can branch on, exactly as a refusal from the platform does —
    and, where there is a next step, a ``hint`` naming it. A wrong name gets
    the one hint every name refusal shares.
    """
    if hint is None and code == "invalid_name":
        hint = _NAME_HINT
    body: dict[str, Any] = {"code": code, "retryable": False}
    if hint is not None:
        body["hint"] = hint
    return ToftError(message, status=status, body=body)


_NAME_HINT = "use a relative name with forward slashes and no '.' or '..' segments"


#: The example each call's not-a-string refusal names, so the hint is the call
#: that was made — download(5) was told to call upload() (BB-109).
_NAME_EXAMPLE = {
    "upload": 'upload("local.csv", "reports/2026-08.csv")',
    "put": 'put("local.csv", "reports/2026-08.csv")',
    "download": 'download("reports/2026-08.csv")',
    "url": 'url("reports/2026-08.csv")',
    "remove": 'remove("reports/2026-08.csv")',
    "list": 'list("reports/")',
}


def _check_name_type(name: Any, call: str, optional: bool = False) -> None:
    """A name that is not a string — ``upload(f, 5)`` — reached ``startswith``
    and escaped as an uncoded AttributeError (BB-008's sweep). ``None`` is one
    too, except where the name is optional (``upload``, ``put``): it was
    ``invalid_name`` "a name is required" (BB-109)."""
    if (name is None and optional) or isinstance(name, str):
        return
    raise _refused("bad_request", f"{call}()'s object name is a string, got {type(name).__name__}",
                   hint=f"pass the name as a string: {_NAME_EXAMPLE[call]}")


def _check_source(source: Any, call: str) -> Path:
    """The local file ``put()`` and ``upload()`` read, as a path — ``Path({})``
    was an uncoded TypeError (BB-008's sweep)."""
    if isinstance(source, (str, os.PathLike)) and str(source):
        return Path(source)
    raise _refused("bad_request",
                   f"{call} needs the path of a local file, got {type(source).__name__}",
                   hint=f'pass the path of a file on this machine first: {call[:-2]}("report.csv")')


def _check_relative_path(path: str, call: str) -> None:
    """A listing path: a relative name, with one trailing slash allowed."""
    _check_name_type(path, call)
    _check_relative_name(path[:-1] if path.endswith("/") else path, call)


def _check_relative_name(name: str, call: str) -> None:
    """Refuse a name that would land outside the mount.

    The same discipline the platform applies to an uploaded object's name, for
    the same reason: the name is the caller's and the location is not, so a
    traversal segment here would write into the app's own filesystem — over its
    code, in the worst case — rather than into the storage it was given.
    """
    _check_name_type(name, call)
    if not name:
        raise _refused("invalid_name", "a name is required")
    if name.startswith("/") or name.endswith("/"):
        raise _refused("invalid_name", f"a name is relative to the mount, got {name!r}")
    if "\\" in name:
        raise _refused("invalid_name", f"a name must use forward slashes, got {name!r}")
    for segment in name.split("/"):
        if segment in ("", ".", ".."):
            raise _refused("invalid_name", f"a name must not contain {segment!r} segments, got {name!r}")


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
        raise _refused("storage_env_invalid", f"{ENV_STORAGE} is not valid JSON: {exc}",
                       hint=_STORAGE_ENV_HINT) from None
    if not isinstance(parsed, list):
        raise _refused("storage_env_invalid", f"{ENV_STORAGE} should be a JSON array, got {type(parsed).__name__}",
                       hint=_STORAGE_ENV_HINT)

    out: list[Mount] = []
    for entry in parsed:
        # An entry that is not an object — a number, null, a bare string — has
        # no name, kind or access to be read from, and dropping it left the
        # mapping silently missing: mounts() said [] and storage() not_mapped.
        if not isinstance(entry, dict):
            raise _refused("storage_env_invalid",
                           f"a {ENV_STORAGE} entry is not an object with a name, kind and access: "
                           f"{json.dumps(entry)}",
                           hint=_STORAGE_ENV_HINT)
        # The platform writes every entry with a name, a kind and an access
        # level (and a path, "" for a store reached over signed URLs). An entry
        # missing one is a hand-written stand-in that would resolve to a mount
        # with no kind — which store() takes for a store and storage() for a
        # volume — so it is refused rather than read as empty strings.
        for key in ("name", "kind", "access"):
            value = entry.get(key)
            if not isinstance(value, str) or not value.strip():
                raise _refused("storage_env_invalid",
                               f"a {ENV_STORAGE} entry has no {key}: {json.dumps(entry)}",
                               hint=_STORAGE_ENV_HINT)
        # Held to the values the platform writes: a kind it does not know is a
        # mount store() and storage() would each guess at, and an access level
        # it does not know makes read_only answer False for a mount nothing
        # granted write on.
        for key, allowed in (("kind", _STORAGE_KINDS), ("access", _STORAGE_ACCESS)):
            if entry[key] not in allowed:
                raise _refused("storage_env_invalid",
                               f"a {ENV_STORAGE} entry's {key} is {entry[key]!r}, not one of "
                               f"{', '.join(allowed)}: {json.dumps(entry)}",
                               hint=_STORAGE_ENV_HINT)
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
    return _resolve(name, folder, "storage")


_STORAGE_KINDS = ("volume", "store")
_STORAGE_ACCESS = ("read", "write", "full")

_STORAGE_ENV_HINT = ("TOFT_STORAGE is a JSON array of entries, each with a name, a kind (volume or "
                     "store) and an access (read, write or full); path is optional — a store reached "
                     "over signed URLs has none. The platform writes it; a local stand-in needs "
                     "the same keys")


def _check_folder(folder: Any, accessor: str) -> None:
    """Hold a folder to the rule an object's name is held to — relative, forward
    slashes, no ``.`` or ``..`` segments — with ``""`` naming the root. Without
    it ``storage("x", folder="..")`` was not_mapped, "mounted, but not at
    folder", a lookup failing where the argument was the fault."""
    hint = f"name a folder of the resource, like toft.{accessor}(\"drop\", folder=\"backup\"), or \"\" for its root"
    if not isinstance(folder, str):
        raise _refused("invalid_name", f"{accessor}()'s folder is a string, got {type(folder).__name__}", hint=hint)
    if not folder:
        return
    if (folder.startswith("/") or folder.endswith("/") or "\\" in folder
            or any(seg in ("", ".", "..") for seg in folder.split("/"))):
        raise _refused("invalid_name",
                       "a folder is a relative path inside the resource, with forward slashes and "
                       f"no '.' or '..' segments, got {folder!r}", hint=hint)


def _resolve(name: str, folder: str | None, accessor: str) -> Mount:
    """:func:`storage`'s body, with the accessor the caller used — ``storage``
    or ``store`` — so a refusal names the call to make rather than the other
    one (drills D17: ``store("x")`` was told to call ``toft.storage``)."""
    if not name:
        raise _refused("invalid_name", f"{accessor}() needs the name an administrator gave the resource",
                       hint="pass one of the names toft.mounts() lists")
    if folder is not None:
        _check_folder(folder, accessor)
    available = mounts()
    named = [m for m in available if m.name == name]

    if folder is not None:
        for mount in named:
            if mount.folder == folder:
                return mount
        if named:
            raise _refused("not_mapped",
                f"{name!r} is mounted, but not at folder {folder!r}. "
                f"Mounted here: {_folders(named)}",
                hint=f"name a mounted folder: toft.{accessor}({name!r}, folder={named[0].folder!r})")
    elif len(named) == 1:
        return named[0]
    elif len(named) > 1:
        raise _refused("ambiguous_mount",
            f"{len(named)} mounts are named {name!r} — {_folders(named)}. "
            f"Name one: toft.{accessor}({name!r}, folder={named[0].folder!r})",
            hint=f"name the folder: toft.{accessor}({name!r}, folder={named[0].folder!r})")

    # Only with no TOFT_STORAGE (unset or blank): "[]" is the platform saying
    # nothing is mapped, and that is not_mapped (BB-023).
    if not os.environ.get(ENV_STORAGE, "").strip() and not any(
            os.environ.get(v, "").strip()
            for v in ("TOFT_API_URL", "TOFT_APP_ID", "TOFT_RUN_ID", "TOFT_SCRATCH_DIR")):
        # Off the platform there is nothing to map, and "ask an administrator"
        # would send somebody to one over a laptop — the code Node raises.
        raise _refused("off_platform",
            f"{accessor}({name!r}) needs the storage Toft maps into a deployed app "
            f"({ENV_STORAGE}) — it only resolves in an app running on the platform",
            hint=f"set {ENV_STORAGE} to a JSON array of mounts to stand one in locally")
    if not available:
        raise _refused("not_mapped",
            f"no storage resource named {name!r}, and this app has none mapped at "
            f"all — an administrator maps storage on the app's Storage tab",
            hint="ask an administrator to map storage on the app's Storage tab")
    # Each name once: a resource mapped at two folders is one resource.
    names = ", ".join(sorted({m.name for m in available})) or "(none)"
    raise _refused("not_mapped", f"no storage resource named {name!r}. Mapped to this app: {names}",
                   hint=f"use one of the names mapped to this app: {names}")


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
    mount = _resolve(name, folder, "store")
    if mount.kind and mount.kind != "store":
        call = _storage_call(mount, folder is not None)
        raise _refused("not_a_store",
            f"{name!r} is a {mount.kind}, not a store. A volume is a real filesystem — "
            f"write to {call}.path directly, with the ordinary file APIs.",
            hint=f"use {call}.path with the ordinary file APIs")
    return mount


def _storage_call(mount: Mount, folder_given: bool = False) -> str:
    """The ``toft.storage(…)`` call that resolves this very mount — with its
    folder when one was named or the name is mapped more than once, since the
    bare name then raises ambiguous_mount (BB-008)."""
    if not folder_given and not mount.folder:
        try:
            folder_given = sum(1 for m in mounts() if m.name == mount.name) > 1
        except ToftError:
            pass
    if folder_given or mount.folder:
        return f"toft.storage({mount.name!r}, folder={mount.folder!r})"
    return f"toft.storage({mount.name!r})"
