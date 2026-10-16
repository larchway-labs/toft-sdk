# What Toft injects, and what to do when it has not.
#
# Anything that talks to the platform stops with an error naming the variable
# that is missing. It does not quietly become a no-op, and there is deliberately
# no dry-run mode: an SDK that prints to a console instead of paging a channel
# is a trap that only springs in production, and it springs at 2am on the one
# night somebody needed the page. Develop against a real install.
#
# The single exception is scratch_dir(), which is pure filesystem and falls back
# to the session temp directory — a path is a path.

ENV_APP_TOKEN <- "TOFT_APP_TOKEN"
ENV_API_URL <- "TOFT_API_URL"
ENV_APP_ID <- "TOFT_APP_ID"
ENV_APP_SLUG <- "TOFT_APP_SLUG"
ENV_STORAGE <- "TOFT_STORAGE"
ENV_SCRATCH_DIR <- "TOFT_SCRATCH_DIR"
ENV_BASE_PATH <- "BASE_PATH"

# Not read for their values, only to tell one absence from another: inside a job
# the app token is *meant* to be missing, and the run id is exactly the
# condition "we are inside a job".
ENV_RUN_ID <- "TOFT_RUN_ID"
ENV_JOB_NAME <- "TOFT_JOB_NAME"
ENV_SCHEDULED_FOR <- "TOFT_SCHEDULED_FOR"

# The app contract version this SDK speaks, sent on every /api/v1/app/* call.
# The platform refuses a mismatch rather than tolerating it, so a partial
# upgrade fails loudly instead of half-working in a way nobody traces back to a
# version skew.
APP_CONTRACT_VERSION <- "1"
CONTRACT_HEADER <- "X-Toft-App-Contract"

# Who a call is on behalf of. The same name the other three SDKs export, and the
# only channel the platform reads it from — it is a signed assertion the app
# relays and can neither mint nor alter, which is why it travels as a header
# rather than as something the app composes into a body.
ACT_FOR_HEADER <- "X-Toft-Act-For"

# require_env reads an injected variable, or stops saying what sets it. The
# message names the variable rather than describing the situation, because the
# fix is always the same: run this on Toft - except in the one case where
# it is not, which missing_env() explains.
require_env <- function(name) {
  value <- trimws(Sys.getenv(name, ""))
  if (nzchar(value)) {
    return(value)
  }
  stop(missing_env(name), call. = FALSE)
}

# missing_env is why the variable is absent, in the two cases that differ.
#
# **The app token missing inside a job is not a broken deploy.** A job workload
# is given none, deliberately: the app's credential belongs to the process
# serving its requests, and a job serves none. The generic sentence is actively
# misleading there - it tells an author running a deployed app on the platform
# that they are not running on the platform, and sends them looking for a
# missing variable, a broken deploy or a platform bug. The answer is
# architectural, so it has to be in the sentence.
#
# TOFT_RUN_ID is what says we are inside a job; the runner exports it for
# every run and for nothing else. The second way round it differs from the other
# three SDKs because this package differs: it carries the app API and no runner
# surface at all, so there is no set_result() here to hand a result back with.
# What an R job has is its mounts, which are a filesystem and need no token.
missing_env <- function(name) {
  run_id <- trimws(Sys.getenv(ENV_RUN_ID, ""))
  if (identical(name, ENV_APP_TOKEN) && nzchar(run_id)) {
    job <- trimws(Sys.getenv(ENV_JOB_NAME, ""))
    where <- if (nzchar(job)) sprintf("job '%s', run %s", job, run_id) else paste("run", run_id)
    return(sprintf(
      paste0(
        "%s is not set because this is a job (%s), and a job is deliberately given ",
        "no app token - this is not a broken deploy. app_data_get(), app_info() ",
        "and notify() belong to the process serving this app's requests, and a ",
        "job serves none. Two ways round it: do this from the service process, or ",
        "write what the run produced into one of its mounts (see mounts()) and let ",
        "an endpoint fold it in."
      ),
      name, where
    ))
  }
  sprintf(
    paste0(
      "%s is not set. This call needs the environment Toft injects into a ",
      "deployed app - it only works in an app running on the platform."
    ),
    name
  )
}
