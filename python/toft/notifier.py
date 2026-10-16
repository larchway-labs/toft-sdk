"""Sending messages and files *as this app*, through whatever an administrator
has connected.

The platform holds the credential. This app never sees the workspace bot token
or the relay password, never chooses which Slack channel it posts to, and never
chooses the address its mail comes from. It supplies the message; the platform
supplies the identity. That is the whole shape of the feature, and it is what
makes an administrator's "detach Slack from that app" a row they delete rather
than a token they have to rotate across every app on the install.

    toft.notify.send("slack", "Ingest finished: 1,204 rows in 42s")
    toft.notify.deliver("sharepoint", "/tmp/recon.xlsx", name="Week 32.xlsx")
    toft.notify.email(to=["finance@example.com"], subject="Weekly report", text=...)

The three older calls — :meth:`Notify.slack`, :meth:`Notify.slack_file` and
:meth:`Notify.email` — keep working unchanged, permanently. They are the general
ones with the integration supplied, and an app written against them keeps
running on every release.

Three properties to write app code against.

**Nothing here blocks on delivery.** Every send returns as soon as the platform
has accepted and recorded it, with a :class:`Send` whose ``id`` names the record.
Delivery happens afterwards. A call returning without raising means *accepted*,
not *delivered* — and that distinction is deliberate, because a report generator
that fell over because Slack was having an afternoon would be a worse outcome
than a report that arrives late.

**The record is what you rely on, not the message.** :meth:`Notify.status` reads
back one send by its id. Owners see the same thing on the app's Integrations tab;
administrators see every app's on the install's integration log.

**Where it goes is not yours to choose.** There is no ``channel`` argument, and
``from`` / ``reply_to`` are not fields. Sending one is a refusal, not a silently
ignored field — a dropped field is a caller who believes they chose. An owner who
wants a different Slack channel or a team reply-to address sets it on the app,
where it is stored, audited, and visible to everyone who can read the app.
"""

from __future__ import annotations

import json
import uuid
from dataclasses import dataclass, field
from collections.abc import Mapping, Sequence
from typing import Any, Iterable

from .errors import ToftError
from .session import transport
from .transport import Part

SLACK_PATH = "/api/v1/app/notify/slack"
SLACK_FILE_PATH = "/api/v1/app/notify/slack/file"
EMAIL_PATH = "/api/v1/app/notify/email"
SENDS_PATH = "/api/v1/app/sends"


def _notify_path(kind: str) -> str:
    """The general form: one route per shape, with the integration in the path."""
    return f"/api/v1/app/notify/{kind}"


def _deliver_path(kind: str) -> str:
    return f"/api/v1/app/deliver/{kind}"

# Statuses that will not change again.
TERMINAL_STATUSES = frozenset({"sent", "failed", "blocked", "abandoned"})


@dataclass(frozen=True)
class Send:
    """One accepted send, and the platform's record of it."""

    id: str
    status: str = "queued"
    #: Which integration carried it. ``integration`` is the older name for the
    #: same fact and is kept forever — an app written against one release keeps
    #: running on the next.
    kind: str = ""
    integration: str = ""
    action: str = ""
    error: str = ""
    raw: Mapping[str, Any] = field(default_factory=dict)

    @property
    def done(self) -> bool:
        return self.status in TERMINAL_STATUSES

    @property
    def delivered(self) -> bool:
        return self.status == "sent"

    @property
    def blocked(self) -> bool:
        """True when an administrator turned the capability off after accepting.

        A distinct outcome from a failure, and worth distinguishing in an app's
        own logs: nothing is wrong with the message, and retrying will not help
        until somebody re-attaches the integration.
        """
        return self.status == "blocked"

    @classmethod
    def from_raw(cls, raw: Any) -> "Send":
        data = raw if isinstance(raw, Mapping) else {}
        kind = str(data.get("kind") or data.get("integration") or "")
        return cls(
            id=str(data.get("send_id") or data.get("id") or ""),
            status=str(data.get("status") or "queued"),
            kind=kind,
            integration=kind,
            action=str(data.get("action") or ""),
            error=str(data.get("error") or ""),
            raw=dict(data),
        )


class Notify:
    """Messages and files, sent as this app."""

    # ---------------------------------------------------------- the general form

    def send(
        self,
        kind: str,
        text: str,
        *,
        title: str | None = None,
        severity: str | None = None,
        dedup_key: str | None = None,
        action: str | None = None,
        idempotency_key: str | None = None,
    ) -> Send:
        """Send one message through the named integration.

        The general form, and the one to write new code against::

            toft.notify.send("slack", "Ingest finished: 1,204 rows in 42s")
            toft.notify.send("teams", "Nightly load complete")
            toft.notify.send("pagerduty", "Ingest is 40 minutes late",
                           severity="error", dedup_key="ingest-late")

        Which integrations this app actually has is
        ``toft.app.can("slack.post")`` — ask at startup and render "ask an
        administrator" on a screen, rather than discovering it at 2am.

        Where it goes is not an argument and never will be. The app's own people
        choose the destination from what the connection an administrator made
        can reach; sending a ``channel``, ``folder``, ``url`` or ``routing_key``
        field is a refusal rather than a silently ignored field, because a
        dropped field is a caller who believes they chose.

        No idempotency key by default — a duplicate chat message is cheap, and a
        key would be quietly deduplicating an app that meant to post twice.
        Pass one for a message whose duplicate would matter.
        """
        name = _require_text(kind, "integration")
        if name == "email":
            raise ToftError(
                "email takes recipients and a subject — use toft.notify.email(to=…, subject=…, text=…)"
            )
        body: dict[str, Any] = {"text": _require_text(text, name)}
        if title:
            body["title"] = title
        if severity:
            body["severity"] = severity
        if dedup_key:
            body["dedup_key"] = dedup_key
        # Only where a kind has more than one thing to do with a message.
        # PagerDuty's trigger and resolve are the pair that exists.
        if action:
            body["action"] = action
        raw = transport().post_json(_notify_path(name), body, idempotency_key)
        return Send.from_raw(raw)

    def resolve(
        self,
        kind: str,
        dedup_key: str,
        *,
        reason: str | None = None,
        idempotency_key: str | None = None,
    ) -> Send:
        """Close an incident this app raised.

        ``dedup_key`` is the same key the trigger used. It is namespaced with
        this app before it leaves, which is what makes "an app can only resolve
        what it triggered" true by construction rather than by a check: the key
        another app's incident is under is not one this app can name.

        An app that may page and may not un-page leaves somebody awake with no
        way to be told it is over, which is why this is not an optional half.
        """
        return self.send(
            kind,
            reason or "Resolved by the application.",
            action="resolve",
            dedup_key=_require_text(dedup_key, "dedup key"),
            idempotency_key=idempotency_key,
        )

    def deliver(
        self,
        kind: str,
        file: Any,
        *,
        name: str | None = None,
        title: str | None = None,
        comment: str | None = None,
        idempotency_key: str | None = None,
    ) -> Send:
        """Put one file through the named integration — the "post the report" call.

        ``file`` is a path, a ``(filename, bytes)`` pair, or a mapping with
        ``name`` and ``content``::

            toft.notify.deliver("sharepoint", "/tmp/recon.xlsx",
                              name="Reconciliation W32.xlsx")

        ``name`` is the name the file arrives under, and it is a *name*: a value
        containing a separator, a leading dot or a reserved device name is
        refused rather than rewritten. A silently sanitised name is a file this
        app cannot find again. Where it lands is the administrator's folder, not
        this app's.

        A file is never replaced. Every integration that writes files adds, and
        a name that already exists is renamed by the far side — an app cannot
        overwrite a file, one it did not write or one it did.

        Defaults to an idempotency key, unlike :meth:`send` — re-uploading a
        20 MiB report because a response was slow is not cheap, in the
        destination or on the wire.
        """
        which = _require_text(kind, "integration")
        part = Part.coerce("file", file)
        if name:
            part = Part("file", name, part.content, part.content_type)

        fields: dict[str, str] = {}
        if name:
            fields["name"] = part.filename
        if title:
            fields["title"] = title
        if comment:
            fields["comment"] = comment

        raw = transport().post_multipart(
            _deliver_path(which), fields, [part], _default_key(idempotency_key)
        )
        return Send.from_raw(raw)

    # ------------------------------------------------------------------ Slack

    def slack(self, text: str, *, idempotency_key: str | None = None) -> Send:
        """Post one message to this app's Slack channel.

        :meth:`send` with ``"slack"``, under its published name and kept working
        permanently.

        ``text`` is Slack mrkdwn — ``*bold*``, ``_italic_``, ``` `code` ```,
        ``<https://example.com|links>`` and lists. The platform appends a line
        naming this app and linking to it, which app code cannot suppress: an
        app that could post indistinguishably from the platform is an app that
        can impersonate it.

        Which channel is not an argument and never will be. The app's owners
        choose it in the UI from the channels somebody invited the bot into,
        falling back to the install's default until they do.

        No idempotency key by default — a duplicate Slack post is cheap, and a
        key would be quietly deduplicating an app that meant to post twice.
        Pass one for a message whose duplicate would matter.
        """
        text = _require_text(text, "slack")
        raw = transport().post_json(SLACK_PATH, {"text": text}, idempotency_key)
        return Send.from_raw(raw)

    def slack_file(
        self,
        file: Any,
        *,
        title: str | None = None,
        comment: str | None = None,
        filename: str | None = None,
        idempotency_key: str | None = None,
    ) -> Send:
        """Upload one file to this app's Slack channel.

        :meth:`deliver` with ``"slack"``, under its published name and kept
        working permanently.

        ``file`` is a path, a ``(filename, bytes)`` pair, or a mapping with
        ``name`` and ``content``. This is the "post the report" call::

            toft.notify.slack_file("/tmp/recon.xlsx",
                                 title="Weekly reconciliation",
                                 comment="1,204 rows, 3 breaks")

        Not a separate capability from posting text: an app that may post to a
        channel may upload a file to it.

        Defaults to an idempotency key, unlike :meth:`slack` — re-uploading a
        20 MiB report because a response was slow is not cheap, in the channel
        or on the wire.
        """
        part = Part.coerce("file", file)
        if filename:
            part = Part("file", filename, part.content, part.content_type)

        fields: dict[str, str] = {}
        if title:
            fields["title"] = title
        if comment:
            fields["comment"] = comment

        raw = transport().post_multipart(
            SLACK_FILE_PATH, fields, [part], _default_key(idempotency_key)
        )
        return Send.from_raw(raw)

    # ------------------------------------------------------------------ Email

    def email(
        self,
        *,
        to: str | Sequence[str],
        subject: str,
        text: str,
        html: str | None = None,
        cc: str | Sequence[str] | None = None,
        bcc: str | Sequence[str] | None = None,
        attachments: Iterable[Any] | None = None,
        idempotency_key: str | None = None,
    ) -> Send:
        """Send one email as this app.

        Recipients are unrestricted — any address, whether or not they have a
        Toft account, which is the point: the weekly report goes to the
        finance team, and the finance team does not log in here. Every send is
        recorded in full, recipients included, and that record is the only
        control an administrator has over this. Write app code that would read
        well in an incident review.

        ``text`` is required even when ``html`` is given. The plain-text half is
        where the "an application sent this" line is unmissable, and an HTML-only
        message has no fallback for a reader whose client will not render it.

        ``html`` is sanitized by the platform: scripts, styles, frames, event
        handlers and positioning are removed. Style the content, not the frame.

        There is no ``from`` and no ``reply_to``. Mail goes out as
        ``"<App Name> (via Toft)"`` from the install's sending address, and
        replies go to the app's owners — or to the address an owner set on the
        app's Integrations tab.

        **Idempotency.** A key is generated by default, once per call, and
        reused if this SDK retries the request. That is what stops a slow
        response from turning into two copies of the report in somebody's inbox.
        It does *not* deduplicate across processes: a cron-driven sender that
        wants "at most one report per week however many times I run" should pass
        its own stable key, such as ``f"weekly-report-{iso_week}"``.
        """
        recipients = _addresses(to, "to")
        if not recipients:
            raise ToftError("email needs at least one recipient in 'to'")
        subject = _require_text(subject, "email subject")
        text = _require_text(text, "email text")

        body: dict[str, Any] = {"to": recipients, "subject": subject, "text": text}
        if html:
            body["html"] = html
        if cc:
            body["cc"] = _addresses(cc, "cc")
        if bcc:
            body["bcc"] = _addresses(bcc, "bcc")

        parts = [Part.coerce("attachment", a) for a in (attachments or [])]
        key = _default_key(idempotency_key)

        if parts:
            # Attachments travel as multipart with the message as one JSON
            # field, never base64 inside a JSON body: a 10 MiB report becomes a
            # 13 MiB string that then has to be decoded into a second 10 MiB
            # buffer, and paying that on the platform's heap for the tidiness of
            # a single content type is not a trade worth making.
            raw = transport().post_multipart(
                EMAIL_PATH, {"payload": json.dumps(body)}, parts, key
            )
        else:
            raw = transport().post_json(EMAIL_PATH, body, key)
        return Send.from_raw(raw)

    # ----------------------------------------------------------------- Status

    def status(self, send: str | Send) -> Send:
        """Read back one send this app made.

        The only thing an app may read about its own sends, and deliberately so:
        there is no listing. An app browsing the record would be learning which
        addresses its administrator finds interesting, whereas reading back an id
        it minted, about content it wrote, tells it nothing it did not know.
        """
        send_id = send.id if isinstance(send, Send) else str(send)
        if not send_id:
            raise ToftError("status() needs the id of a send")
        raw = transport().get(f"{SENDS_PATH}/{send_id}")
        return Send.from_raw(raw)


def _require_text(value: str, what: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ToftError(f"{what} needs some text")
    return value


def _addresses(value: str | Sequence[str], field_name: str) -> list[str]:
    """Accept one address or many, because both are natural to write."""
    if isinstance(value, str):
        items = [value]
    else:
        items = list(value)
    out = [str(v).strip() for v in items if str(v).strip()]
    if items and not out:
        raise ToftError(f"'{field_name}' held only blank addresses")
    return out


def _default_key(key: str | None) -> str:
    """A caller's key, or a fresh one for this call.

    Generated once here rather than per HTTP attempt, which is the entire point:
    the transport's single retry presents the same key, so a request that was in
    fact delivered before the response was lost does not send twice.
    """
    return key or uuid.uuid4().hex
