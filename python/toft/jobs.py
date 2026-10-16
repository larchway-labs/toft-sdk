"""Starting background work from inside an app.

This is an app starting **a different entrypoint of itself**. Same repo, same
release, same storage, different command — ``toft.toml`` declares it, and
the platform runs it as an isolated workload with a memory limit, a deadline and
a durable record::

    import toft

    # The app acting as itself — no viewer, no actor.
    run = toft.jobs.start("reconcile", params={"month": "2026-03"})

    # The app acting for whoever is looking at the page.
    me = toft.viewer(st.context.headers)
    run = me.jobs.start("reconcile", params={"month": month, "ledgers": pair})

Which object you call is what says whether a run is attributed to a person, and
that is deliberate. A flag would make the anonymous case the default spelling of
the attributed one, and attribution would then be lost by omission — a run
sliding quietly out of somebody's name and into nobody's, noticed at the next
audit if at all.

**Nothing here blocks on the work.** :meth:`Jobs.start` returns as soon as the
run is queued. :meth:`JobRun.wait` exists, is for scripts and notebooks, and is
never what ``start`` does — an isolated workload's cold start alone is measured
in tens of seconds, so a call that could return a result would be a call that
sometimes holds a web request for a minute.

**A repeated start is not a second run.** Streamlit re-executes the script on
every widget interaction, so a naive ``start()`` in a button branch fires again
on the next click of anything. The platform derives an idempotency key from the
app, the job, the parameters and the resolved person, and a start matching a run
already in flight — or one that finished in the last minute — comes back as
*that* run with :attr:`JobRun.deduplicated` set. Reported, never silent: an app
that appeared to ignore a click is indistinguishable from a broken one.

**Coming back tomorrow works, and the app stores nothing to make it work.**
``me.jobs.mine()`` asks the platform for this viewer's runs. The app relays the
act-for token; the platform resolves it and filters on the person it resolved.
The app gets that person's runs and still never learns who they are.

**A run you start may be retried, and this object follows the chain.** Retries
are off by default and switched on per job — on the app's Automation tab, or as
``retries = N`` in ``toft.toml`` — never in code here. What the SDK does is
make sure the chain is not something you have to know about:

* :attr:`JobRun.done` is **false** on a failed run the platform has already
  queued another attempt at. The work is still in progress, in a row with a
  different id, and reporting that as finished would be a failure announced for
  something about to succeed.
* :meth:`JobRun.refresh` returns the *newer attempt* when there is one, so
  :meth:`JobRun.wait` resolves on the outcome of the work rather than of the
  first try at it.
* :attr:`JobRun.attempt` is which try you are looking at, and
  :attr:`JobRun.retried_by` names the successor when one exists. Read them when
  the distinction matters; ignore them and the defaults are right.

Which runs are eligible is the platform's rule, not this library's: a schedule
and anything the app started — including a run started on a viewer's behalf,
since the app fired it and did not block on it. A run somebody started from the
Toft panel is never retried behind them, and neither is one that ran out of
time or memory, because a second attempt would end the same way.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from collections.abc import Mapping
from typing import Any, Iterator

from .errors import ToftError
from .session import transport

JOBS_PATH = "/api/v1/app/jobs"
RUNS_PATH = "/api/v1/app/job-runs"

# Statuses that will not change again. Polling stops here.
TERMINAL_STATUSES = frozenset(
    {
        "succeeded",
        "failed",
        "cancelled",
        "timed_out",
        "resource_exceeded",
        "infrastructure_error",
    }
)


class QueueFull(ToftError):
    """This app already has as many runs in flight as it is allowed.

    A first-class, actionable refusal rather than a generic error, and it
    carries the numbers: an app renders "3 runs already queued, and 3 is the
    limit — try again in a moment" instead of putting a traceback in the middle
    of somebody's page. :attr:`retryable` is true, which no other 4xx here is.
    """

    def __init__(self, message: str, in_flight: int = 0, limit: int = 0, **kw: Any) -> None:
        super().__init__(message, **kw)
        self.in_flight = in_flight
        self.limit = limit


@dataclass
class JobRun:
    """A run, and everything an app can do with one.

    Returned by :meth:`Jobs.start` before the work begins — which is the point.
    Every method here is a fresh read or an action; nothing is cached, because a
    run's whole purpose is to change while you are not looking.
    """

    id: str
    status: str = "pending"
    job_name: str = ""
    progress: int = 0
    #: 1-based while pending, ``None`` once the run starts. "3rd in line" is
    #: most of what somebody wants when they ask about a queue.
    queue_position: int | None = None
    #: True when this start returned a run that already existed.
    deduplicated: bool = False
    #: True when the run was started on a person's behalf rather than by the
    #: app itself. Never says *which* person: an app does not learn that.
    started_by_viewer: bool = False
    has_output: bool = False
    exit_code: int | None = None
    failure_stage: str = ""
    failure_reason: str = ""
    #: The structured result the job recorded with ``toft.run.set_result``. The
    #: smallest of the three result channels and the right one for what most
    #: internal tools produce: counts, a summary, a few hundred rows.
    result: Any = None
    #: Which try this run is, and the run that superseded it when the platform
    #: queued another attempt. A failed run with ``retried_by`` set is not the
    #: end of the story — see :meth:`wait`.
    attempt: int = 1
    retried_by: str = ""
    #: The instant this run is for — its schedule's tick, or one a person named
    #: — as the platform wrote it (RFC 3339), or ``""`` when nobody gave one.
    scheduled_for: str = ""
    raw: Mapping[str, Any] = field(default_factory=dict)

    _headers: Mapping[str, str] = field(default_factory=dict, repr=False)

    @classmethod
    def from_raw(cls, raw: Any, headers: Mapping[str, str] | None = None) -> "JobRun":
        data = raw if isinstance(raw, Mapping) else {}
        return cls(
            id=str(data.get("id") or ""),
            status=str(data.get("status") or "pending"),
            job_name=str(data.get("job_name") or ""),
            progress=int(data.get("progress") or 0),
            queue_position=data.get("queue_position") or None,
            deduplicated=bool(data.get("deduplicated")),
            started_by_viewer=bool(data.get("started_by_viewer")),
            has_output=bool(data.get("has_output")),
            exit_code=data.get("exit_code"),
            failure_stage=str(data.get("failure_stage") or ""),
            failure_reason=str(data.get("failure_reason") or ""),
            result=data.get("result"),
            attempt=int(data.get("attempt") or 1),
            retried_by=str(data.get("retried_by") or ""),
            scheduled_for=str(data.get("scheduled_for") or ""),
            raw=data,
            _headers=dict(headers or {}),
        )

    # --- State ---

    @property
    def done(self) -> bool:
        """Whether this run will change again.

        A failed run the platform has already queued another attempt at is
        *not* done: the work is still in progress, in a row with a different id.
        Reading only this row's status there would report a failure for
        something that is about to succeed.
        """
        return self.status in TERMINAL_STATUSES and not self.retried_by

    @property
    def succeeded(self) -> bool:
        return self.status == "succeeded"

    @property
    def out_of_resources(self) -> bool:
        """Killed for exceeding its limits, rather than crashing.

        Worth telling apart because the fix is different: a bigger limit or less
        work, not a code change.
        """
        return self.status in ("resource_exceeded", "timed_out")

    def refresh(self) -> "JobRun":
        """Re-read this run and return a fresh handle.

        **Follows a retry.** If the platform has queued another attempt at this
        work, the handle you get back is that attempt — same chain, newer row.
        An app that started a run and waits on it should see the outcome of the
        work, not of the first try at it; the attempt number and
        :attr:`retry_of` in ``raw`` are there for anyone who needs the
        distinction.
        """
        raw = transport().get(f"{RUNS_PATH}/{self.id}", self._headers)
        run = JobRun.from_raw(raw, self._headers)
        if run.retried_by:
            return JobRun.from_raw(
                transport().get(f"{RUNS_PATH}/{run.retried_by}", self._headers),
                self._headers,
            )
        return run

    def wait(self, timeout: float | None = None, poll_interval: float = 2.0) -> "JobRun":
        """Block until the run finishes.

        For scripts and notebooks. **Never call this on a web request path** —
        that is the twelve-minute block this whole feature exists to remove.
        """
        deadline = None if timeout is None else time.monotonic() + timeout
        current = self
        while True:
            current = current.refresh()
            if current.done:
                return current
            if deadline is not None and time.monotonic() >= deadline:
                raise TimeoutError(
                    f"run {self.id} was still {current.status} after {timeout}s"
                )
            time.sleep(poll_interval)

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

    # --- Logs ---

    def logs(self, after_seq: int = 0) -> list[dict]:
        raw = transport().get(f"{RUNS_PATH}/{self.id}/logs?after={after_seq}", self._headers)
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

    def __init__(self, headers: Mapping[str, str] | None = None) -> None:
        self._headers = dict(headers or {})

    def start(
        self,
        job: str,
        params: Mapping[str, Any] | None = None,
        payload: Any = None,
        idempotency_key: str | None = None,
    ) -> JobRun:
        """Queue a run and return its handle immediately.

        ``params`` are recorded against the run for reproducibility and audit,
        so they must not carry secrets — those belong in the app's managed
        environment, which a run references rather than copies.

        ``payload`` is bulk input, stored in the object store and handed to the
        run as a presigned URL; the job reads it with ``toft.run.read_input_json``.

        ``idempotency_key`` overrides the derived one. The derivation is already
        correct for the case that actually bites (a framework re-running the
        script), so pass one only when the app has a better notion of "the same
        request" than the parameters give.
        """
        body: dict[str, Any] = {}
        if params:
            body["params"] = dict(params)
        if payload is not None:
            body["payload"] = payload

        try:
            raw = transport().post_json(
                f"{JOBS_PATH}/{job}/runs", body,
                idempotency_key=idempotency_key, headers=self._headers,
            )
        except ToftError as exc:
            raise _typed(exc) from None
        return JobRun.from_raw(raw, self._headers)

    def get(self, run_id: str) -> JobRun:
        raw = transport().get(f"{RUNS_PATH}/{run_id}", self._headers)
        return JobRun.from_raw(raw, self._headers)

    def list(self, job: str | None = None, limit: int = 50) -> list[JobRun]:
        """Every run of this app, newest first."""
        return self._list(scope="app", job=job, limit=limit)

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
                "with every run would be a silent widening rather than an answer."
            )
        return self._list(scope="viewer", job=job, limit=limit)

    def _list(self, scope: str, job: str | None, limit: int) -> list[JobRun]:
        query = f"?scope={scope}&limit={int(limit)}"
        if job:
            query += f"&job={job}"
        raw = transport().get(f"{RUNS_PATH}{query}", self._headers)
        runs = (raw or {}).get("runs") or []
        return [JobRun.from_raw(r, self._headers) for r in runs]


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
    )
