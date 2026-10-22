"""Starting background work from inside an app.

This is an app starting **a different entrypoint of itself**. Same repo, same
release, same storage, different command — ``toft.toml`` declares it, and
the platform runs it as an isolated workload with a memory limit, a deadline and
a durable record::

    import toft

    @app.post("/reports")
    def make_report():
        me = toft.viewer(request.headers)          # the person clicking
        run = me.jobs.start("nightly-report", params={"region": "emea"})
        return toft.jobs.respond(run)              # 202, or 303 to a live status page

**Respond, don't wait.** A web request has a deadline — the gateway, and any
load balancer in front of it, cuts it off — and a job's cold start alone can be
tens of seconds. :meth:`Jobs.respond` answers the request now: ``202 Accepted``
with ``Location`` and the run as JSON for a script, ``303 See Other`` for a
browser, which lands on the run's live status page. The gateway serves that page
on the app's own origin; the app writes no polling code and no template.

**respond() and frameworks.** Inside a Flask request it returns a
``flask.Response``. Given the request — ``respond(run, request=request)`` — a
FastAPI or Starlette app gets a Starlette ``Response``. Anything else gets a
WSGI-shaped ``(body, status, headers)`` tuple to hand to its own response
class. Whether the caller is a browser is read from the request's ``Accept``;
with no request to read, the JSON answer is the one given.

:meth:`JobRun.wait` is for scripts, notebooks and one job waiting on another.
**Called while serving a request it refuses** (``would_outlive_request``)
unless its ``timeout`` fits inside what is left of that request — the gateway
puts the cut-off on every request it forwards, as ``X-Toft-Request-Deadline``,
and a wait that would outlive it ends as a 504 nobody can explain. This module
sees the request three ways, all explicit: the headers given to
:func:`toft.viewer` (every run started or read through that viewer carries
them), a ``headers=`` argument to ``wait``, and Flask's current request. A
Streamlit session's headers are its WebSocket handshake's, whose deadline is
not the session's, so an upgrade request carries none.

Which object you call is what says whether a run is attributed to a person —
``toft.jobs`` is the app acting as itself, ``toft.viewer(headers).jobs`` the app
acting for the person looking at the page. A flag would make the anonymous case
the default spelling of the attributed one, and attribution would then be lost
by omission.

**A repeated start is not a second run.** Streamlit re-executes the script on
every widget interaction, so a naive ``start()`` in a button branch fires again
on the next click of anything. The platform derives an idempotency key from the
app, the job, the parameters and the resolved person, and a start matching a run
already in flight — or one that finished in the last minute — comes back as
*that* run with :attr:`JobRun.deduplicated` set. Reported, never silent.

**Params are the only input.** They are typed by ``toft.toml``, recorded on the
run and shown in its history, so they must not carry secrets and are capped at
64 KB. Anything bigger goes into the app's own storage first, and the param
names where.

**A run is never retried.** A failed run stays failed, with
:attr:`JobRun.failure_reason` saying why in words; trying again is a person's
decision (``toft job rerun``), which records the new run's ``rerun_of``.
"""

from __future__ import annotations

import datetime
import json
import re
import sys
import time
from dataclasses import dataclass, field
from collections.abc import Mapping
from typing import Any, Iterator
from urllib.parse import quote

from .errors import ToftError
from .session import transport

JOBS_PATH = "/api/v1/app/jobs"
RUNS_PATH = "/api/v1/app/job-runs"

#: When the request the app is serving will be cut off, RFC 3339. The gateway
#: sets it on every request it forwards; it is absent outside one.
REQUEST_DEADLINE_HEADER = "X-Toft-Request-Deadline"

#: Statuses that will not change again. Polling stops here.
TERMINAL_STATUSES = frozenset(
    {"succeeded", "failed", "timed_out", "out_of_memory", "cancelled", "skipped"}
)


class QueueFull(ToftError):
    """This app already has as many runs queued as it is allowed.

    A first-class, actionable refusal rather than a generic error, and it
    carries the numbers: an app renders "3 runs already queued, and 3 is the
    limit — try again in a moment" instead of putting a traceback in the middle
    of somebody's page. :attr:`retryable` is true, because the platform says a
    full queue drains.
    """

    def __init__(self, message: str, in_flight: int = 0, limit: int = 0, **kw: Any) -> None:
        super().__init__(message, **kw)
        self.in_flight = in_flight
        self.limit = limit


class WaitTimeout(ToftError, TimeoutError):
    """:meth:`JobRun.wait` gave up before the run finished.

    Coded ``wait_timeout`` and retryable — the run is still going and waiting
    again is a fair answer — and still a :class:`TimeoutError`, which is what
    this raised before it carried the envelope.
    """


def _parse_time(raw: Any) -> datetime.datetime | None:
    """An RFC 3339 instant as an aware datetime, or None.

    By hand rather than ``fromisoformat`` alone: before 3.11 that refuses a
    trailing ``Z`` and Go's nine fractional digits, which is exactly what the
    platform writes.
    """
    if not raw or not isinstance(raw, str):
        return None
    text = raw.strip().replace("Z", "+00:00").replace("z", "+00:00")
    text = re.sub(r"(\.\d{6})\d+", r"\1", text)
    try:
        value = datetime.datetime.fromisoformat(text)
    except ValueError:
        return None
    if value.tzinfo is None:
        value = value.replace(tzinfo=datetime.timezone.utc)
    return value


def request_deadline(headers: Any) -> datetime.datetime | None:
    """The cut-off of the request these headers came in on, or None.

    None for a WebSocket upgrade: Streamlit holds its handshake headers for the
    life of the session, and the handshake's deadline passed hours ago without
    meaning anything.
    """
    from .viewer import _find_header  # viewer imports this module

    if _find_header(headers, "Upgrade").lower() == "websocket":
        return None
    return _parse_time(_find_header(headers, REQUEST_DEADLINE_HEADER))


def _flask_request() -> Any:
    """Flask's current request, or None — without importing Flask into an app
    that does not use it."""
    flask = sys.modules.get("flask")
    if flask is None:
        return None
    try:
        return flask.request if flask.has_request_context() else None
    except Exception:  # an odd shim named flask is not this module's problem
        return None


def _headers_of(request: Any) -> Any:
    return getattr(request, "headers", request)


@dataclass
class JobRun:
    """A run, and everything an app can do with one.

    Returned by :meth:`Jobs.start` before the work begins — which is the point.
    Every method here is a fresh read or an action; nothing is cached, because a
    run's whole purpose is to change while you are not looking. Timestamps are
    the platform's RFC 3339 text, or None where the run has not reached them.
    """

    id: str
    job: str = ""
    #: queued, running, succeeded, failed, timed_out, out_of_memory,
    #: cancelled or skipped.
    status: str = "queued"
    #: app, viewer, cli, schedule or ui. ``viewer`` never says *which* person:
    #: an app does not learn that.
    started_by: str = ""
    #: The run this one re-ran, when a person asked for that.
    rerun_of: str | None = None
    params: dict = field(default_factory=dict)
    scheduled_for: str | None = None
    queued_at: str | None = None
    started_at: str | None = None
    finished_at: str | None = None
    #: When the run will be stopped if it is still going.
    deadline: str | None = None
    progress: int = 0
    progress_message: str | None = None
    #: 1-based while queued, ``None`` once the run starts. "3rd in line" is
    #: most of what somebody wants when they ask about a queue.
    queue_position: int | None = None
    has_output: bool = False
    exit_code: int | None = None
    failure_stage: str | None = None
    #: Why it did not succeed, in words: ``exit 3: connection refused``,
    #: ``timed out after 45m``, ``stopped at 1024 MB``.
    failure_reason: str | None = None
    peak_memory_mb: int | None = None
    #: The run's live status page, on the app's own origin — where
    #: :meth:`Jobs.respond` sends a browser.
    status_url: str = ""
    logs_url: str = ""
    #: True when this start returned a run that already existed.
    deduplicated: bool = False
    raw: Mapping[str, Any] = field(default_factory=dict)

    _headers: Mapping[str, str] = field(default_factory=dict, repr=False)
    _request_deadline: datetime.datetime | None = field(default=None, repr=False)

    @classmethod
    def from_raw(
        cls,
        raw: Any,
        headers: Mapping[str, str] | None = None,
        request_deadline: datetime.datetime | None = None,
    ) -> "JobRun":
        data = raw if isinstance(raw, Mapping) else {}

        def text(key: str) -> str | None:
            value = data.get(key)
            return str(value) if value not in (None, "") else None

        params = data.get("params")
        return cls(
            id=str(data.get("id") or ""),
            job=str(data.get("job") or ""),
            status=str(data.get("status") or "queued"),
            started_by=str(data.get("started_by") or ""),
            rerun_of=text("rerun_of"),
            params=dict(params) if isinstance(params, Mapping) else {},
            scheduled_for=text("scheduled_for"),
            queued_at=text("queued_at"),
            started_at=text("started_at"),
            finished_at=text("finished_at"),
            deadline=text("deadline"),
            progress=int(data.get("progress") or 0),
            progress_message=text("progress_message"),
            queue_position=data.get("queue_position") or None,
            has_output=bool(data.get("has_output")),
            exit_code=data.get("exit_code"),
            failure_stage=text("failure_stage"),
            failure_reason=text("failure_reason"),
            peak_memory_mb=data.get("peak_memory_mb"),
            status_url=str(data.get("status_url") or ""),
            logs_url=str(data.get("logs_url") or ""),
            deduplicated=bool(data.get("deduplicated")),
            raw=data,
            _headers=dict(headers or {}),
            _request_deadline=request_deadline,
        )

    def to_dict(self) -> dict:
        """The run as the platform sent it — what :meth:`Jobs.respond` answers with."""
        return dict(self.raw) if self.raw else {"id": self.id, "job": self.job, "status": self.status}

    # --- State ---

    @property
    def done(self) -> bool:
        """Whether this run will change again."""
        return self.status in TERMINAL_STATUSES

    @property
    def succeeded(self) -> bool:
        return self.status == "succeeded"

    @property
    def started_by_viewer(self) -> bool:
        """Started in a person's name rather than by the app itself."""
        return self.started_by == "viewer"

    @property
    def out_of_resources(self) -> bool:
        """Stopped for exceeding its limits, rather than crashing.

        Worth telling apart because the fix is different: a bigger limit or less
        work, not a code change.
        """
        return self.status in ("timed_out", "out_of_memory")

    def _again(self, raw: Any) -> "JobRun":
        return JobRun.from_raw(raw, self._headers, self._request_deadline)

    def refresh(self) -> "JobRun":
        """Re-read this run and return a fresh handle."""
        return self._again(transport().get(f"{RUNS_PATH}/{self.id}", self._headers))

    def wait(
        self,
        timeout: float | None = None,
        poll_interval: float = 2.0,
        headers: Any = None,
    ) -> "JobRun":
        """Block until the run finishes, and return it in its final state.

        For scripts, notebooks and jobs. **On a request path it refuses**
        (``would_outlive_request``) unless ``timeout`` fits inside what is left
        of the request — see the module docstring for how the request is seen;
        ``headers`` is the explicit way to say "I am serving this one". The
        answer there is :meth:`Jobs.respond`.

        Raises :class:`WaitTimeout` when ``timeout`` passes first.
        """
        self._guard_request(timeout, headers)
        deadline = None if timeout is None else time.monotonic() + timeout
        current = self
        while True:
            current = current.refresh()
            if current.done:
                return current
            if deadline is not None and time.monotonic() >= deadline:
                raise WaitTimeout(
                    f"run {self.id} was still {current.status} after {timeout}s",
                    body={"code": "wait_timeout", "retryable": True},
                )
            time.sleep(poll_interval)

    def _guard_request(self, timeout: float | None, headers: Any) -> None:
        if headers is not None:
            cutoff = request_deadline(headers)
        elif self._request_deadline is not None:
            cutoff = self._request_deadline
        else:
            flask_request = _flask_request()
            cutoff = request_deadline(flask_request.headers) if flask_request is not None else None
        if cutoff is None:
            return
        left = (cutoff - datetime.datetime.now(datetime.timezone.utc)).total_seconds()
        if timeout is not None and timeout <= left:
            return
        asked = "no timeout" if timeout is None else f"a {timeout:g}s timeout"
        raise ToftError(
            f"wait() on run {self.id} would outlive the request it is serving: the "
            f"gateway cuts this request off in {max(left, 0):.0f}s, and this wait has "
            f"{asked}. Answer the request now with toft.jobs.respond(run) — a browser "
            f"lands on the run's live status page — and let the run finish on its own.",
            body={
                "code": "would_outlive_request",
                "retryable": False,
                "hint": "return toft.jobs.respond(run)",
            },
        )

    # --- Output ---

    def output(self) -> bytes:
        """Fetch this run's output object, or ``b""`` if it produced none.

        Over a presigned URL the platform mints — the app holds no object-store
        credential, because one that could read this run's output could read
        every other app's data.

        Raises when the artifact *did* exist and has since expired. An output
        object lives exactly as long as the run row that explains it, and saying
        so is better than a download that quietly 404s: a run still reading
        ``succeeded`` with nothing behind it is the worst failure shape
        available here.
        """
        return transport().follow(f"{RUNS_PATH}/{self.id}/output", self._headers)

    def output_json(self) -> Any:
        """The output :meth:`Run.write_output_json` wrote, decoded — ``None``
        when the run produced none. The reading twin every caller otherwise
        writes as ``json.loads(run.output() or b"null")``."""
        raw = self.output()
        return json.loads(raw) if raw else None

    # --- Logs ---

    def logs(self, after_seq: int = 0) -> list[dict]:
        """Log lines after ``after_seq``: ``{seq, at, stream, line}`` each."""
        raw = transport().get(f"{RUNS_PATH}/{self.id}/logs?after={int(after_seq)}", self._headers)
        return list(raw.get("lines") or []) if raw else []

    def tail(self, poll_interval: float = 2.0) -> Iterator[dict]:
        """Yield log lines until the run reaches a terminal status."""
        seq = 0
        while True:
            for line in self.logs(after_seq=seq):
                seq = max(seq, int(line.get("seq") or seq))
                yield line
            if self.refresh().done:
                # One last read: lines can land between the poll and the status
                # check, and losing the end of a failing job's output loses the
                # part that explains it.
                for line in self.logs(after_seq=seq):
                    yield line
                return
            time.sleep(poll_interval)

    def cancel(self) -> None:
        """Stop this run.

        A viewer may cancel a run they started and not somebody else's; the app
        acting as itself may cancel a run with no person behind it. Cancelling
        through the wrong one of those raises rather than quietly doing nothing.
        """
        transport().post_json(f"{RUNS_PATH}/{self.id}/cancel", None, headers=self._headers)


class Jobs:
    """The job surface, either as the app or on a person's behalf.

    ``toft.jobs`` is the app acting as itself. ``toft.viewer(headers).jobs`` is the
    same surface with a viewer attached, and the difference is one relayed
    header — which the app cannot mint, alter, or read.
    """

    def __init__(
        self,
        headers: Mapping[str, str] | None = None,
        request_deadline: datetime.datetime | None = None,
    ) -> None:
        self._headers = dict(headers or {})
        self._request_deadline = request_deadline

    def _run(self, raw: Any) -> JobRun:
        return JobRun.from_raw(raw, self._headers, self._request_deadline)

    def start(
        self,
        job: str,
        params: Mapping[str, Any] | None = None,
        idempotency_key: str | None = None,
    ) -> JobRun:
        """Queue a run and return its handle immediately.

        ``params`` are checked against the job's ``[jobs.<name>.params]`` —
        a missing, mistyped or out-of-choice one is refused here
        (``param_required``, ``param_type``, ``param_choice``), not an hour into
        the run. They are recorded and shown, so they must not carry secrets.

        ``idempotency_key`` overrides the derived one (and holds for 24 hours
        rather than a minute). The derivation is already right for a framework
        re-running the script, so pass one only when the app has a better
        notion of "the same request" than the parameters give.
        """
        body: dict[str, Any] = {}
        if params:
            body["params"] = dict(params)
        try:
            raw = transport().post_json(
                f"{JOBS_PATH}/{quote(job, safe='')}/runs", body,
                idempotency_key=idempotency_key, headers=self._headers,
            )
        except ToftError as exc:
            raise _typed(exc) from None
        return self._run(raw)

    def respond(self, run: JobRun, request: Any = None) -> Any:
        """Answer the request that started ``run``, without waiting for it.

        A browser (``Accept`` naming ``text/html``) gets ``303 See Other`` to
        the run's live status page; anything else gets ``202 Accepted`` with
        ``Location`` and the run as JSON. The return type follows the framework
        — see the module docstring — so ``return toft.jobs.respond(run)`` is the
        whole of it in Flask.
        """
        return respond(run, request)

    def get(self, run_id: str) -> JobRun:
        return self._run(transport().get(f"{RUNS_PATH}/{quote(run_id, safe='')}", self._headers))

    def list(self, job: str | None = None, status: str | None = None, limit: int = 50) -> list[JobRun]:
        """Every run of this app, newest first — of one job, in one status, when given."""
        return self._list(scope="app", job=job, status=status, limit=limit)

    def mine(self, job: str | None = None, limit: int = 20) -> list[JobRun]:
        """This viewer's runs of this app, newest first.

        Only on a :func:`toft.viewer` — the app acting as itself has no
        "mine" to ask about, and calling it there raises rather than quietly
        returning everything.

        This is what makes coming back tomorrow work without the app storing a
        single per-user row. The run is a durable record on the platform keyed
        on the *person*, so she can start it on a laptop and check it from a
        phone. Authorization stays live too: if she lost read on the app
        overnight the list is empty, because the filter runs against a principal
        the platform resolved itself.
        """
        if not self._headers:
            raise ToftError(
                "mine() needs a viewer — call it on toft.viewer(request.headers).jobs. "
                "The app acting as itself has no viewer to scope to, and answering "
                "with every run would be a silent widening rather than an answer.",
                body={
                    "code": "no_viewer",
                    "retryable": False,
                    "hint": "call it on toft.viewer(request.headers).jobs",
                },
            )
        return self._list(scope="viewer", job=job, status=None, limit=limit)

    def _list(self, scope: str, job: str | None, status: str | None, limit: int) -> list[JobRun]:
        query = f"?scope={scope}&limit={int(limit)}"
        if job:
            query += f"&job={quote(job, safe='')}"
        if status:
            query += f"&status={quote(status, safe='')}"
        raw = transport().get(f"{RUNS_PATH}{query}", self._headers)
        return [self._run(r) for r in (raw or {}).get("runs") or []]


def wants_html(headers: Any) -> bool:
    """Whether a request asked for a page rather than data."""
    from .viewer import _find_header

    return "text/html" in _find_header(headers, "Accept").lower()


def respond(run: JobRun, request: Any = None) -> Any:
    """:meth:`Jobs.respond`, as a function. See there and the module docstring."""
    flask_request = _flask_request()
    asking = request if request is not None else flask_request
    location = run.status_url
    if asking is not None and wants_html(_headers_of(asking)):
        status, body, media = 303, "", None
    else:
        status, body, media = 202, json.dumps(run.to_dict()), "application/json"
    headers = {"Location": location} if location else {}

    if request is not None and type(request).__module__.split(".")[0] in ("starlette", "fastapi"):
        from starlette.responses import Response

        return Response(content=body, status_code=status, headers=headers, media_type=media)
    if flask_request is not None:
        import flask

        return flask.Response(body, status=status, headers=headers, mimetype=media)
    if media:
        headers["Content-Type"] = media
    return body, status, headers


def _typed(exc: ToftError) -> ToftError:
    """Promote a queue-full refusal to the error a caller can render.

    The platform sends the counts; flattening them into "429 Too Many Requests"
    throws away the only part an app could put on a page.
    """
    if exc.status != 429:
        return exc
    body = getattr(exc, "body", None) or {}
    if body.get("code") != "queue_full":
        return exc
    return QueueFull(
        str(exc),
        in_flight=int(body.get("in_flight") or 0),
        limit=int(body.get("limit") or 0),
        status=exc.status,
        retry_after=exc.retry_after,
        # The platform's body, whole: its code, retryable and hint are the
        # envelope every refusal carries, and rebuilding the error from the
        # counts alone dropped them — QueueFull read code "".
        body=body,
    )
