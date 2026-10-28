"""A running job's view of itself, over the runner callback contract.

This is the *inside* of a job — code executing as the workload, reporting back
over ``/api/v1/runner/*`` with the run-scoped token the runner exported. It is
the other side of :mod:`toft.jobs`, which is how an app *starts* one::

    r = toft.run                              # None outside a job
    region = r.params["region"]               # typed by toml: ints are ints
    r.progress(40, "reading ledgers")         # the status page shows both
    r.log("12,044 rows")                      # stdout; the runner ships it
    r.write_output_json({"rows": 12044})      # presigned PUT, both modes

**Params are the only input.** There is no payload: anything bigger than a
param belongs in the app's own storage, and the param names where.
``write_output`` is the one durable result channel, and it outlives scratch —
``TOFT_SCRATCH_DIR`` is wiped on every start, so a report written there is a
dead path tomorrow while the run row cheerfully still says ``succeeded``.

:attr:`Run.deadline` is when the platform stops this run. A job with a long
loop checks it and stops cleanly, rather than being stopped mid-write.

Standard library only, on purpose. This runs inside a customer's app image
beside their own dependency tree, and a version conflict between the SDK's
HTTP library and theirs would be our bug appearing in their build.
"""

from __future__ import annotations

import datetime
import json
import os
import sys
import urllib.error
import urllib.request
from typing import Any, Optional

# Re-exported so `from toft.client import ToftError` keeps working.
# The definition moved to toft.errors when the app surface arrived, because
# everything in the package needs the error and nothing else should need the
# jobs client to get it.
from .caller import ROLE_UNKNOWN
from .errors import ToftError
from .jobs import _parse_time
from .transport import _from_http_error_or_redirect, _open_without_redirects

DEFAULT_TIMEOUT = 30.0

#: The runner callback contract this SDK speaks, and it must equal the platform's
#: ``runner.ContractVersion``. A partial upgrade is reported rather than
#: tolerated, so a stale number here is not a warning — it is a 409 on every
#: ``progress`` call a job makes. ``sdk/contract_test.go``
#: is what keeps the four copies of it in step.
RUNNER_CONTRACT_VERSION = "2"


class _RunnerTransport:
    """Calls to ``/api/v1/runner/*`` with the run-scoped token.

    Deliberately not the app transport in :mod:`toft.transport`. That one
    carries the app token to ``/api/v1/app/*``; this one carries a credential
    minted for one run, valid only until that run's deadline, and accepted only
    on the callback routes. The audiences are what keep a job's progress bar from
    being able to act as the app — sharing one client between them would be the
    first step towards sharing a credential.
    """

    def __init__(self, base_url: str, token: str, timeout: float = DEFAULT_TIMEOUT) -> None:
        self._base = base_url.rstrip("/")
        self._token = token
        self._timeout = timeout

    def request(self, method: str, path: str, body: Any = None) -> Any:
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(f"{self._base}{path}", data=data, method=method)
        req.add_header("Authorization", f"Bearer {self._token}")
        req.add_header("Content-Type", "application/json")
        req.add_header("X-Toft-Runner-Version", RUNNER_CONTRACT_VERSION)

        try:
            # Redirects refused, as on the app transport: the run token must
            # not follow a Location to another host.
            with _open_without_redirects(req, self._timeout) as resp:
                payload = resp.read()
                return json.loads(payload) if payload else None
        except urllib.error.HTTPError as exc:
            # The transport's own reading of a refusal, so a job's run calls
            # carry the envelope — code, retryable, hint — as the app's do.
            raise _from_http_error_or_redirect(exc, path) from None
        except urllib.error.URLError as exc:
            raise ToftError(f"cannot reach Toft at {self._base}: {exc.reason}") from None


class Run:
    """The job's own view of itself: who started it, its params, its deadline;
    report progress, log, write output.

    ``Run.from_env()`` inside a job configures itself from what the runner
    already exported, so a job never handles a credential explicitly.
    """

    def __init__(self, base_url: str, token: str, run_id: str) -> None:
        self._t = _RunnerTransport(base_url, token)
        self.id = run_id
        #: The job this run is of — its ``[jobs.<name>]`` in ``toft.toml``.
        self.job = os.environ.get("TOFT_JOB_NAME", "")
        #: The run this one re-ran, or ``None``. A rerun carries the same
        #: params; a job that must not repeat a side effect checks this.
        self.rerun_of = os.environ.get("TOFT_RERUN_OF") or None
        self.output_uri = os.environ.get("TOFT_OUTPUT_URI", "")
        #: *How* this run started: ``app``, ``viewer``, ``cli``, ``schedule`` or
        #: ``ui`` — the run's ``started_by``.
        self.trigger = os.environ.get("TOFT_TRIGGER", "")
        #: *Who* started it: one of the ROLE_* words, ``"system"`` for a
        #: schedule firing on its schedule, ``"app"`` for the app starting work
        #: for itself, and ``"unknown"`` where the platform did not say.
        #:
        #: The pair is the point. "ui" tells a report job nothing about
        #: whether to include the private columns; "ui" plus "viewer" does.
        #:
        #: **The role is today's, not the day the run was queued.** It is
        #: derived from the run's actor when the workload starts, so somebody
        #: demoted in between reports their current role — the right answer for
        #: a job deciding what work to do, and the wrong one for an audit. The
        #: audit question is answered by the run's actor in Toft's own
        #: history, which does not move.
        self.trigger_role = os.environ.get("TOFT_TRIGGER_ROLE") or ROLE_UNKNOWN

    @classmethod
    def from_env(cls) -> "Run":
        base_url = os.environ.get("TOFT_API_URL")
        token = os.environ.get("TOFT_JOB_TOKEN")
        run_id = os.environ.get("TOFT_RUN_ID")
        if not (base_url and token and run_id):
            raise ToftError(
                "not running inside a Toft job — TOFT_API_URL, "
                "TOFT_JOB_TOKEN and TOFT_RUN_ID are set by the runner"
            )
        return cls(base_url, token, run_id)

    @property
    def started_by(self) -> str:
        """The contract's name for :attr:`trigger`."""
        return self.trigger

    @property
    def params(self) -> dict:
        """The run's params, typed as ``toft.toml`` declares them."""
        raw = os.environ.get("TOFT_PARAMS")
        return json.loads(raw) if raw else {}

    @property
    def scheduled_for(self) -> Optional[datetime.datetime]:
        """The instant this run is for, or ``None`` when nobody gave one.

        A scheduled run's is the tick it fired for — the 03:00 run is the 03:00
        run even if it started at 03:40 — in the schedule's own zone. A run
        somebody started with ``toft job run --for`` carries what they named. It is
        **never defaulted to now**: a re-run of Tuesday's work on Thursday must
        not quietly process Thursday.
        """
        return _parse_time(os.environ.get("TOFT_SCHEDULED_FOR"))

    @property
    def deadline(self) -> Optional[datetime.datetime]:
        """When the platform stops this run, or ``None`` where it did not say."""
        return _parse_time(os.environ.get("TOFT_DEADLINE"))

    def progress(self, percent: int, message: Optional[str] = None) -> None:
        """Report how far along this run is, 0–100, and optionally what it is
        doing — a few words the status page shows under the bar."""
        body: dict[str, Any] = {"version": RUNNER_CONTRACT_VERSION, "percent": int(percent)}
        if message:
            body["message"] = str(message)
        self._t.request("POST", "/api/v1/runner/progress", body)

    def log(self, message: str, error: bool = False) -> None:
        """Write one line to this run's log.

        Nothing but a print — the runner ships stdout and stderr — and here so a
        job does not have to know that, or that a buffered stdout loses the last
        lines of a run that is then killed.
        """
        print(message, file=sys.stderr if error else sys.stdout, flush=True)

    def write_output(self, data: bytes | str) -> None:
        """Upload this run's output.

        Also a presigned URL, and also in both execution modes — a filesystem
        path would work in shared mode and would then make an app written
        against it unable to move to isolated mode.
        """
        if not self.output_uri:
            raise ToftError("this run has no output location",
                            body={"code": "no_output", "retryable": False})
        body = data.encode() if isinstance(data, str) else data
        req = urllib.request.Request(self.output_uri, data=body, method="PUT")
        try:
            with urllib.request.urlopen(req, timeout=DEFAULT_TIMEOUT):
                return
        except urllib.error.HTTPError as exc:
            raise ToftError(f"uploading output: {exc.reason}", status=exc.code) from None

    def write_output_json(self, value: Any) -> None:
        self.write_output(json.dumps(value))
