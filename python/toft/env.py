"""What Toft injects, and what to do when it has not.

Anything that talks to the platform raises off-platform and names the variable
that is missing. It does not quietly become a no-op, and there is deliberately
no dry-run mode: an SDK that prints to a console instead of paging a channel is
a trap that only springs in production, and it springs at 2am on the one night
somebody needed the page. Develop against a real install.

The single exception is the scratch helpers, which are pure filesystem and fall
back to the system temp directory — a path is a path (see ``toft.scratch``).
"""

from __future__ import annotations

import os

from .errors import ToftError

ENV_APP_TOKEN = "TOFT_APP_TOKEN"
ENV_API_URL = "TOFT_API_URL"
ENV_APP_ID = "TOFT_APP_ID"
ENV_APP_SLUG = "TOFT_APP_SLUG"
ENV_STORAGE = "TOFT_STORAGE"
ENV_BASE_PATH = "BASE_PATH"

# Not read for their values here, only to tell one absence from another: inside
# a job the app token is *meant* to be missing, and the run id is exactly the
# condition "we are inside a job".
ENV_RUN_ID = "TOFT_RUN_ID"
ENV_JOB_NAME = "TOFT_JOB_NAME"

# The app contract version this SDK speaks, sent on every /api/v1/app/* call.
# The platform refuses a mismatch rather than tolerating it, so a partial
# upgrade fails loudly instead of half-working in a way nobody traces back to a
# version skew.
APP_CONTRACT_VERSION = "1"

CONTRACT_HEADER = "X-Toft-App-Contract"


def require(name: str) -> str:
    """Read an injected variable, or raise saying what sets it.

    The message names the variable rather than describing the situation,
    because the fix is always the same: run this on Toft — except in the
    one case where it is not, which :func:`_missing` explains.
    """
    value = os.environ.get(name, "").strip()
    if value:
        return value
    raise ToftError(_missing(name))


def _missing(name: str) -> str:
    """Why the variable is absent, in the two cases that differ.

    **The app token missing inside a job is not a broken deploy.** A job
    workload is given none, deliberately: the app's credential belongs to the
    process serving its requests, and a job serves none. The generic sentence
    is actively misleading there — it tells an author running a deployed app on
    the platform that they are not running on the platform, and sends them
    looking for a missing variable, a broken deploy or a platform bug. The
    answer is architectural, so it has to be in the sentence.

    ``TOFT_RUN_ID`` is what says we are inside a job; the runner exports it
    for every run and for nothing else.
    """
    run_id = os.environ.get(ENV_RUN_ID, "").strip()
    if name == ENV_APP_TOKEN and run_id:
        job = os.environ.get(ENV_JOB_NAME, "").strip()
        where = f"job {job!r}, run {run_id}" if job else f"run {run_id}"
        return (
            f"{name} is not set because this is a job ({where}), and a job is "
            f"deliberately given no app token — this is not a broken deploy. "
            f"toft.data and toft.app belong to the process serving this app's "
            f"requests, and a job serves none (toft.notify works from a job). Two "
            f"ways round it: do this "
            f"from the service process, or carry the result out of the run with "
            f"run.set_result(...) or run.write_output_json(...) and let an "
            f"endpoint fold it in."
        )
    return (
        f"{name} is not set. This call needs the environment Toft injects "
        f"into a deployed app — it only works in an app running on the platform."
    )


def app_or_job_token() -> str:
    """The credential the app surface is called with.

    The app's own token in its service process. Inside a job — which holds no
    app token — the run's own credential, which the platform accepts for
    sending through the app's integrations and nothing else (ADR §108): so
    ``toft.notify`` works from a job, and ``toft.data`` from a job is refused by the
    platform with ``job_send_only``. Absent both, the explanation
    ``require`` gives.
    """
    token = os.environ.get(ENV_APP_TOKEN, "").strip()
    if token:
        return token
    if os.environ.get(ENV_RUN_ID, "").strip():
        job_token = os.environ.get("TOFT_JOB_TOKEN", "").strip()
        if job_token:
            return job_token
    return require(ENV_APP_TOKEN)
