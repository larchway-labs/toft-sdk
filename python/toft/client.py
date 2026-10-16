"""A running job's view of itself, over the runner callback contract.

This is the *inside* of a job — code executing as the workload, reporting back
over ``/api/v1/runner/*`` with the run-scoped token the runner exported. It is
the other side of :mod:`toft.jobs`, which is how an app *starts* one::

    r = toft.run                              # None outside a job
    params = r.params
    rows = reconcile(**params)
    r.progress(80)
    r.write_output(csv_bytes)               # presigned PUT, both modes
    r.set_result({"rows": len(rows), "output": "reconcile/2026-03.csv"})

``set_result`` and ``write_output`` are the two durable channels, in the order
to reach for them. The structured result is right for what most internal tools
produce — counts, a summary, a few hundred rows — costs no configuration, and
comes back on every read of the run. The output object is for the artifact
itself.

**Never write a durable result into scratch.** ``TOFT_SCRATCH_DIR`` is
wiped on every start and ``session_scratch_dir`` is per-login, so a report
written there is a dead path tomorrow while the run row cheerfully still says
``succeeded``. Status outliving its payload is the worst failure shape available
here, which is why ``result`` should carry a storage-relative reference rather
than a raw path.

``Launcher`` used to live here and is gone. It could never work: it read
``TOFT_APP_ID``, which nothing set, and presented ``TOFT_JOB_TOKEN`` —
minted with the *run* audience — to a route that wanted a session or a personal
key, so the call authenticated as nobody and 404d. Enqueueing arrives as
``toft.jobs`` rather than as a repair to that, because there was nothing in it
worth carrying forward under a deprecation.

Standard library only, on purpose. This runs inside a customer's app image
beside their own dependency tree, and a version conflict between the SDK's
HTTP library and theirs would be our bug appearing in their build.
"""

from __future__ import annotations

import datetime
import json
import os
import urllib.error
import urllib.request
from typing import Any, Optional

# Re-exported so `from toft.client import ToftError` keeps working.
# The definition moved to toft.errors when the app surface arrived, because
# everything in the package needs the error and nothing else should need the
# jobs client to get it.
from .caller import ROLE_UNKNOWN
from .errors import ToftError

DEFAULT_TIMEOUT = 30.0

#: The runner callback contract this SDK speaks, and it must equal the platform's
#: ``runner.ContractVersion``. A partial upgrade is reported rather than
#: tolerated, so a stale number here is not a warning — it is a 409 on every
#: ``progress`` and ``set_result`` call a job makes. ``sdk/contract_test.go``
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
            with urllib.request.urlopen(req, timeout=self._timeout) as resp:
                payload = resp.read()
                return json.loads(payload) if payload else None
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode(errors="replace")
            try:
                detail = json.loads(detail).get("error", detail)
            except Exception:
                pass
            raise ToftError(detail or exc.reason, status=exc.code) from None
        except urllib.error.URLError as exc:
            raise ToftError(f"cannot reach Toft at {self._base}: {exc.reason}") from None


def _input_refusal(status: int) -> str:
    """What a failed input read means, in the two cases that are not "it broke".

    Written out because the failures are indistinguishable at the HTTP layer and
    the fixes are opposite: one is a job asking for something it was never
    given, the other is a job that took too long.
    """
    if status == 404:
        return (
            "this run was given no payload — TOFT_INPUT_URI points at an "
            "object that was never written. A run started with parameters alone "
            "has no input; read run.params for those. (A current platform leaves "
            "run.input_uri empty in that case, so an install still exporting it "
            "is older than this SDK.)"
        )
    if status in (403, 410):
        return (
            "this run's payload is no longer readable: the presigned link the "
            "platform minted for it has expired. Read the input at the start of "
            "a run rather than hours into one."
        )
    return f"reading this run's input failed: HTTP {status}"


class Run:
    """The job's own view of itself: report progress, read input, write output.

    ``Run.from_env()`` inside a job configures itself from what the runner
    already exported, so a job never handles a credential explicitly.
    """

    def __init__(self, base_url: str, token: str, run_id: str) -> None:
        self._t = _RunnerTransport(base_url, token)
        self.id = run_id
        self.input_uri = os.environ.get("TOFT_INPUT_URI", "")
        self.output_uri = os.environ.get("TOFT_OUTPUT_URI", "")
        #: *How* this run started: "cron", "manual", "api" or "app".
        self.trigger = os.environ.get("TOFT_TRIGGER", "")
        #: *Who* started it: one of the ROLE_* words, ``"system"`` for a
        #: schedule firing on its schedule, ``"app"`` for the app starting work
        #: for itself, and ``"unknown"`` where the platform did not say.
        #:
        #: The pair is the point. "manual" tells a report job nothing about
        #: whether to include the private columns; "manual" plus "viewer" does.
        #: Both have been recorded on every run since jobs existed and neither
        #: had a name in this SDK until now.
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
    def params(self) -> dict:
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
        raw = os.environ.get("TOFT_SCHEDULED_FOR")
        if not raw:
            return None
        return datetime.datetime.fromisoformat(raw.replace("Z", "+00:00"))

    def progress(self, percent: int) -> None:
        """Report how far along this run is, 0–100."""
        self._t.request("POST", "/api/v1/runner/progress", {"version": RUNNER_CONTRACT_VERSION, "percent": int(percent)})

    def set_result(self, result: Any) -> None:
        """Record the run's structured result."""
        # record_only: the run is still going, and only the runner's own report
        # at exit ends it — without the flag this call finished the run as
        # succeeded, and everything the job did afterwards was lost.
        self._t.request("POST", "/api/v1/runner/result", {
            "version": RUNNER_CONTRACT_VERSION, "result": result, "exit_code": 0, "record_only": True,
        })

    def read_input(self) -> bytes:
        """Fetch the payload this run was given, or ``b""`` when it was given none.

        Over a presigned URL: a job holds no object-store credential, because
        one that could read this run's input could read every other app's.

        **A refusal here is named rather than relayed.** The platform mints this
        URL only for a run that was actually given a payload, so a 404 means an
        install that predates that rule; a 403 or 410 is the link's six-hour
        window having closed under a very long run. Both used to surface as
        urllib's bare ``HTTPError: 404`` — a failure an author could not tell
        from a store that was down.
        """
        if not self.input_uri:
            return b""
        try:
            with urllib.request.urlopen(self.input_uri, timeout=DEFAULT_TIMEOUT) as resp:
                return resp.read()
        except urllib.error.HTTPError as exc:
            raise ToftError(_input_refusal(exc.code), status=exc.code) from None

    def read_input_json(self) -> Any:
        raw = self.read_input()
        return json.loads(raw) if raw else None

    def write_output(self, data: bytes | str) -> None:
        """Upload this run's output.

        Also a presigned URL, and also in both execution modes — a filesystem
        path would work in shared mode and would then make an app written
        against it unable to move to isolated mode.
        """
        if not self.output_uri:
            raise ToftError("this run has no output location")
        body = data.encode() if isinstance(data, str) else data
        req = urllib.request.Request(self.output_uri, data=body, method="PUT")
        try:
            with urllib.request.urlopen(req, timeout=DEFAULT_TIMEOUT):
                return
        except urllib.error.HTTPError as exc:
            raise ToftError(f"uploading output: {exc.reason}", status=exc.code) from None

    def write_output_json(self, value: Any) -> None:
        self.write_output(json.dumps(value))
