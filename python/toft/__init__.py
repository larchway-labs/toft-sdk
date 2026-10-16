"""The Toft app SDK.

One import, one entry point::

    import toft

    toft.app.id, toft.app.slug, toft.app.mode      # who am I
    toft.app.can("email")                      # what did an administrator attach

    toft.scratch_dir("uploads")                # ephemeral, per app, no network
    toft.session_scratch_dir(session_id)       # one browser login's corner of it
    toft.new_session_id()                      # …for a viewer who signed in nowhere
    toft.storage("reports").path               # durable, resolved by name
    toft.storage("drop", folder="backup").path # …and by folder, where one
                                             # resource is mapped twice
    toft.store("reports").put("nightly.parquet")   # a file into a store

    toft.data.get()                            # this app's one JSON document
    toft.data.set({"period": period})          # survives restart, redeploy, rollback
    toft.data.update(lambda s: s.update(runs=s.get("runs", 0) + 1))  # …and a second writer

    toft.notify.slack("Ingest finished: 1,204 rows")
    toft.notify.slack_file("/tmp/recon.xlsx", title="Weekly reconciliation")
    toft.notify.email(to=["finance@example.com"], subject="Weekly report", text=...)

    toft.jobs.start("reconcile", params={"month": "2026-03"})   # as the app
    me = toft.viewer(request.headers)                           # as a person
    me.jobs.start("reconcile", params={"month": month})
    me.jobs.mine("reconcile", limit=20)

    me = toft.who(request.headers)             # who is looking: Identity or None
    if me and me.role in ("owner", "editor"):
        render_admin_tab()

    toft.run                                   # inside a job: None everywhere else
    task = toft.task(request.headers)          # a scheduled firing, else None
    if task: task.log("read 1,204 rows")     # a log the platform cannot capture

**The credential stays with the platform.** An app never holds the Slack bot
token or the relay password, never names the channel it posts to, and never
chooses the address its mail comes from. It supplies the message; Toft
supplies the identity. An administrator attaches an integration to an app, and
detaching it is a row they delete rather than a token they rotate everywhere.

**Ask before you send.** ``toft.app.can("email")`` is cheap and cached. An app
that checks at startup can tell someone "email is not enabled for this app" on a
screen, instead of leaving it to be discovered at 2am in a log.

**Nothing here blocks on delivery.** A send returns once the platform has
accepted and recorded it; delivery happens after. The returned
:class:`~toft.notifier.Send` names that record, and ``toft.notify.status(send)``
reads it back. The record — not the message — is what a workflow may rely on.

**Off-platform it raises rather than pretending.** Every call that needs the
platform names the environment variable that is missing. There is deliberately
no dry-run mode: an SDK that silently printed to a console instead of paging a
channel would be a trap that only springs in production. Develop against a real
install.

**Background work is a different entrypoint of this same app.** ``toft.jobs.start``
runs a command ``toft.toml`` declares, from the same release, as an isolated
workload with a memory limit, a deadline and a durable record — in *both*
execution modes, because moving twelve minutes and six gigabytes out of the
app's own process is the point rather than a side effect. It never blocks on the
work. See :mod:`toft.jobs`.

**Two objects, and which one you call is the decision.** ``toft.jobs`` is the app
acting as itself; ``toft.viewer(headers).jobs`` is the app acting for the person
looking at the page. Not a flag, because a flag would make the anonymous case
the default spelling of the attributed one, and attribution would then be lost
by omission. See :mod:`toft.viewer`.

**Two entrypoints for a viewer, and which one raises is the difference.**
``toft.viewer(headers)`` is *act as this person* and raises when there is nobody
to act for — a silent downgrade would move a run out of somebody's name and into
nobody's. ``toft.who(headers)`` is *who is this*, and returns ``None`` for an
anonymous visitor because that is an answer rather than a failure. It raises for
one thing only: an app an administrator has not enabled viewer identity on,
which is a configuration gap to close and not a runtime state to branch on.

**A scheduled task's log is the one thing the platform cannot capture for you.**
Toft triggers a task by making an HTTP request to a path you published; it
owns no process, so your prints land in the app's own stream interleaved with
every other request. ``toft.task(request.headers)`` is that firing — ``None`` on an
ordinary visit, so one endpoint can serve both a schedule and a person — and
``task.log(...)`` is what makes a run's log exist. Worth adding to anything that
might outlive the timeout: a run that never answers is recorded as a failure
with nothing else in it. See :mod:`toft.task`.

**A failed run may be retried, and you configure that in Toft, not here.**
Retries are off by default and switched on per task or job — on the app's
Automation tab, or as ``retries = N`` in ``toft.toml``. What the SDK does is
keep the chain from being your problem: a run the platform has queued another
attempt at is not :attr:`~toft.jobs.JobRun.done`, and ``refresh()`` follows
it, so ``wait()`` gives you the outcome of the work rather than of the first try
at it. Only runs nobody is waiting on are eligible — a schedule, and what the app
started — never one somebody kicked off from the Toft panel.

The library is deliberately thin. It owns no delivery semantics — no client-side
scheduling, no size limits duplicated from the platform, and **no retry policy
of its own**: the one repeat it makes is of a request that never reached a
handler, which is a transport detail and not the platform's retry of a *run*
described above. The stable surface is the HTTP API; anything clever added here
becomes a second source of truth about how a send behaves.
"""

from __future__ import annotations

import os

from .appinfo import App
from .caller import (
    ENV_ROLE_KEY,
    ROLE_ANONYMOUS,
    ROLE_APP,
    ROLE_EDITOR,
    ROLE_HEADER,
    ROLE_OWNER,
    ROLE_SYSTEM,
    ROLE_UNKNOWN,
    ROLE_VIEWER,
    Caller,
    caller,
)
from .client import Run
from .data import Data, Document, VersionConflict
from .errors import ToftError
from .jobs import JobRun, Jobs, QueueFull
from .notifier import Notify, Send
from .resources import Mount, mounts, storage, store
from .scratch import SESSION_HEADER, new_session_id, scratch_dir, session_scratch_dir
from .task import CRON_HEADER, RUN_HEADER, SCHEDULE_HEADER, Task, task
from .viewer import ACT_FOR_HEADER, Identity, Viewer, viewer, who

# The singletons app code reaches for. All lazy: constructing them touches
# nothing, so importing this package off-platform never raises and never makes a
# network call.
app = App()
notify = Notify()
jobs = Jobs()
data = Data()


def _run_or_none() -> Run | None:
    """The job's own view of itself, or None outside a job.

    None rather than raising, because ``if toft.run:`` is how a module shared
    between an app's web entrypoint and its job entrypoint tells which one it is
    in — and that is exactly the shape this feature encourages, one deployable
    with two entrypoints.
    """
    if not os.environ.get("TOFT_RUN_ID"):
        return None
    try:
        return Run.from_env()
    except ToftError:
        return None


run = _run_or_none()

__all__ = [
    # The app-facing surface.
    "app",
    "notify",
    "jobs",
    "data",
    "viewer",
    "who",
    # What the caller in front of this request may do, verified locally with no
    # round trip. `who` is who; this is what they may do.
    "caller",
    # Inside a scheduled task: the run's own log. None on an ordinary visit.
    "task",
    "storage",
    "store",
    "mounts",
    "scratch_dir",
    "session_scratch_dir",
    "new_session_id",
    # Inside a job: the run's own view of itself. None everywhere else.
    "run",
    # Types worth naming in app code.
    "App",
    "Notify",
    "Send",
    "Jobs",
    "JobRun",
    "QueueFull",
    "Identity",
    "Viewer",
    "Caller",
    "Task",
    "Mount",
    "Run",
    "Data",
    "Document",
    "VersionConflict",
    "ToftError",
    "SESSION_HEADER",
    "ACT_FOR_HEADER",
    "ROLE_HEADER",
    "ENV_ROLE_KEY",
    "ROLE_UNKNOWN",
    "ROLE_ANONYMOUS",
    "ROLE_VIEWER",
    "ROLE_EDITOR",
    "ROLE_OWNER",
    "ROLE_SYSTEM",
    "ROLE_APP",
    "CRON_HEADER",
    "RUN_HEADER",
    "SCHEDULE_HEADER",
]

__version__ = "0.3.0"

# The callback contract version this SDK speaks to /api/v1/runner/*. The
# platform refuses a mismatch rather than tolerating it, so a partial upgrade
# fails loudly instead of behaving strangely. The app contract
# (/api/v1/app/*) is versioned separately — see toft.env.
CONTRACT_VERSION = "1"
