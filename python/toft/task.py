"""The scheduled task a request *is*, and the log the platform cannot capture.

    import toft

    @app.get("/reports/run")
    def run_report():
        task = toft.task(request.headers)     # None when this is an ordinary visit
        if task:
            task.log(f"reading {n:,} rows")
        ...
        if task:
            task.log("wrote report.xlsx")
        return {"rows": n}

**Why an app has to do this at all.** A background job's output belongs to the
platform: the runner owns the process, captures its stdout and ships it. A
scheduled task is an HTTP request against an endpoint the app published, so
there is no platform-owned process to read — the task's prints land in the app's
own log stream, interleaved with every other request being served at that
moment, and cannot be sliced apart afterwards. Pushing lines is the only way a
task's own log can exist, and it is opt-in because only the app knows which of
its prints belong to this run.

**What it is for.** A run that passes the platform's task timeout is recorded as
a failure with *nothing* in it — no result and no body, because the app never
answered. Lines pushed as the run goes are what is left: the log right up to the
kill. That is the failure this exists for, and the reason it is worth a call in
the app rather than a setting somewhere.

**Nothing here can break the run.** A push that fails — the run already closed,
the platform unreachable, the cap reached — is swallowed. Logging that could
take down the thing it observes would be worse than no logging, so this returns
``False`` and gets on with it. ``toft.task(...)`` returning ``None`` off a
scheduled firing is the same principle: a report endpoint somebody also opens in
a browser must not raise because nobody scheduled that visit.

**The app is not asserting anything.** The run id arrives in a header the
platform set; what authorizes the push is the app's own token, and the platform
scopes the run to the app that holds it. An app can log to its own runs and to
nothing else, whatever it puts in the header.

**A retry is a different run, with a log of its own.** If the task asks for
retries — on its Automation tab, never in code here — a failed firing is tried
again as a *new* run: your handler is called afresh, ``toft.task(headers)`` hands
back the new run id, and the lines you push land on that attempt. Nothing
carries over, which is the point: each attempt's log is what that attempt did,
and the failed one keeps its own. Retries apply to firings nobody is waiting on
— the schedule, and ones the app triggers through ``toft`` — while the Toft
panel's own Run now is never retried behind the person who pressed it.

**Pushing to a closed run fails quietly, and that is the interesting case.** If
your handler outlives the platform's task timeout, the run is already recorded
as failed and stops accepting lines; :meth:`Task.log` returns ``False`` and this
object stops trying. The lines you pushed *before* the deadline were kept, which
is the whole reason to push as you go rather than at the end.


**A task need not have a schedule, and the marker is still set.** A task whose
cron expression is empty is fired only on demand — ``toft task run``, the Run now
button, or the app asking for it by name — and it travels the one firing path
every other firing takes, so ``X-Toft-Cron: 1`` is on the request either
way. So :func:`toft.task` is non-``None`` for an on-demand firing too, and reading
the marker is *this was a firing*, never *a schedule came round*.
"""

from __future__ import annotations

import logging
from collections.abc import Mapping
from typing import Any

from .session import transport

#: Headers the platform sets on a scheduled trigger. ``X-Toft-Cron`` has
#: been on every trigger since before tasks had names, and apps in the wild
#: branch on it, so it stays the marker.
#:
#: ``X-Toft-Schedule`` carries **the task's name** — `refresh`, not
#: `0 3 * * *`. That is the platform's deliberate choice and the useful one: one
#: endpoint serving several schedules needs to know which fired, and a cron
#: expression does not identify a task. The header keeps the spelling it has
#: always had; :attr:`Task.name` is what it is called here.
CRON_HEADER = "X-Toft-Cron"
RUN_HEADER = "X-Toft-Run"
SCHEDULE_HEADER = "X-Toft-Schedule"

RUNS_PATH = "/api/v1/app/runs"

_log = logging.getLogger("toft.task")


class Task:
    """One run of a task, from inside the request that is running it.

    *A run of a task*, not *a scheduled run*: a task with no cron expression is
    fired on demand and arrives here the same way.
    """

    __slots__ = ("id", "name", "_closed")

    def __init__(self, run_id: str, name: str) -> None:
        #: The run id, the same one the panel shows.
        self.id = run_id
        #: **The name of the task that fired**, so one endpoint serving several
        #: can tell them apart without reading its own parameters back. The name
        #: an administrator typed on the Automation tab — `refresh` — and never
        #: its cron expression, which no call in this SDK gives an app.
        self.name = name
        # Set once the platform says the run is closed or full. After that
        # every further call is a no-op rather than a request that will be
        # refused identically — a task in a loop should not spend its remaining
        # time discovering the same 409.
        self._closed = False

    def log(self, message: Any, *, error: bool = False) -> bool:
        """Push one line onto this run's log. Never raises.

        Returns whether the line was stored, for the rare caller that wants to
        know; most should ignore it.
        """
        return self.log_lines([message], error=error)

    def log_lines(self, messages: list[Any], *, error: bool = False) -> bool:
        """Push several lines in one request.

        Batching is the app's choice, deliberately: a buffer here would mean
        lines held in memory when the process is killed, which is precisely the
        case this feature exists to serve.
        """
        if self._closed or not messages:
            return False
        stream = "stderr" if error else "stdout"
        body = {"lines": [{"line": str(m), "stream": stream} for m in messages]}
        try:
            raw = transport().post_json(f"{RUNS_PATH}/{self.id}/logs", body)
        except Exception as exc:  # noqa: BLE001 - logging must never break the run
            # A closed run answers 409 and a full one answers with dropped>0;
            # both are ordinary, and neither is the app's problem to handle.
            _log.debug("toft: could not push a task log line: %s", exc)
            self._closed = True
            return False
        if isinstance(raw, Mapping) and raw.get("dropped"):
            self._closed = True
            return bool(raw.get("stored"))
        return True

    @property
    def schedule(self) -> str:
        """Gone: this was never a schedule.

        It held the task's *name*, so a page rendering "last scheduled run:
        {task.schedule}" published *last scheduled run: refresh*. Raising here
        rather than aliasing :attr:`name` is the point — an app that wanted the
        cron expression cannot have it from any SDK call, and an alias would
        keep that page wrong under a name that reads right.
        """
        raise AttributeError(
            "Task.schedule held the task's name, not its cron expression, and is now "
            "Task.name. No SDK call gives an app its own cron expression: the platform "
            "sends the name so that one endpoint serving several schedules can tell "
            "them apart."
        )

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        return f"<toft.Task {self.name!r} run={self.id}>"


def task(headers: Mapping[str, Any] | Any) -> Task | None:
    """Build a :class:`Task` from the request headers, or ``None``.

    ``None`` means this request is not a scheduled firing — somebody opened the
    endpoint in a browser, a health check hit it, or the app is running outside
    the gateway. That is not an error and must not raise: the same endpoint
    serving both a schedule and a person is the ordinary shape of a report app.

    This mirrors :func:`toft.viewer` in construction and differs in exactly
    that: a missing act-for header raises, because a run sliding out of a
    person's name into nobody's is a fact somebody has to notice; a missing cron
    header is just a visitor.
    """
    run_id = _find_header(headers, RUN_HEADER)
    if not run_id:
        return None
    return Task(run_id, _find_header(headers, SCHEDULE_HEADER))


def _find_header(headers: Any, name: str) -> str:
    """Read one header out of whatever the framework calls its header mapping.

    The same lookup :mod:`toft.viewer` uses, and case-insensitive for the
    same reason: Express lowercases, WSGI shouts, Streamlit passes through what
    the browser sent.
    """
    if headers is None:
        return ""
    getter = getattr(headers, "get", None)
    if callable(getter):
        for spelling in (name, name.lower(), name.upper()):
            value = getter(spelling)
            if value:
                return str(value)
    try:
        for key, value in dict(headers).items():
            if str(key).lower() == name.lower() and value:
                return str(value)
    except (TypeError, ValueError):
        return ""
    return ""
