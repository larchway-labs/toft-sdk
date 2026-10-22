#' What this app is, as the platform sees it
#'
#' Read once and kept for the life of the process, as the other three SDKs do:
#' a page that asks for its base path and two capabilities on every view would
#' otherwise spend its rate limit (60 a minute, burst 10) on the same answer.
#' Pass \code{refresh = TRUE} where a capability may have changed since — an
#' administrator switching viewer identity on, say.
#'
#' @param refresh Fetch again rather than answering from the cache.
#' @return A list carrying the app's id, slug, and the capabilities this install
#'   has granted it. A capability that is absent is absent — never present and
#'   false — so an app can tell "switched off" from "this platform is older than
#'   the feature".
#' @export
app_info <- function(refresh = FALSE) {
  if (isTRUE(refresh) || is.null(.app_cache$view)) {
    .app_cache$view <- toft_request("GET", "/api/v1/app/self")
  }
  .app_cache$view
}

.app_cache <- new.env(parent = emptyenv())

#' Where this app is mounted
#'
#' **Build every link and form action from this**, because a root-absolute URL
#' asks the platform rather than the app and 404s.
#'
#' The platform sends it; `BASE_PATH` in the environment is the fallback, and it
#' is a fallback rather than the source so that the accessor and the variable
#' cannot disagree — the variable is the value this same deploy injected into
#' this process, so it can only answer where an install older than the field
#' says nothing. Returning `""` there would be worse than useless: `""` is also
#' the honest answer on an install that gives each app its own origin, so
#' nothing an app could check would tell the two apart.
#'
#' @return The URL prefix this app is served under, e.g. `/apps/reporter`.
#' @export
app_base_path <- function() {
  value <- app_info()$base_path
  if (is.null(value) || !nzchar(value)) {
    return(trimws(Sys.getenv(ENV_BASE_PATH, "")))
  }
  value
}

#' A writable directory for this app's temporary files
#'
#' The one function here that works off-platform, because a path is a path. On
#' Toft this is the per-app scratch directory the platform provides; off it
#' this is the session's temp directory, so a script under development does not
#' have to be edited to run in either place.
#'
#' @return A path, created if it did not exist.
#' @export
scratch_dir <- function() {
  dir <- trimws(Sys.getenv(ENV_SCRATCH_DIR, ""))
  if (!nzchar(dir)) {
    dir <- tempdir()
  }
  if (!dir.exists(dir)) {
    dir.create(dir, recursive = TRUE, showWarnings = FALSE)
  }
  dir
}

#' The instant this job run is for
#'
#' A scheduled run's is the tick it fired for - the 03:00 run is the 03:00 run
#' even if it started at 03:40 - in the schedule's own zone. A run started with
#' `toft job run --for` carries what was named. Never defaulted to now: a re-run of
#' Tuesday's work on Thursday must not quietly process Thursday.
#'
#' @return A POSIXct in UTC, or NULL outside a job or when nobody gave one.
#'   `attr(x, "text")` keeps the value as sent, offset included.
#' @export
scheduled_for <- function() {
  raw <- trimws(Sys.getenv(ENV_SCHEDULED_FOR, ""))
  if (!nzchar(raw)) {
    return(NULL)
  }
  # RFC 3339 with a numeric offset or Z. strptime's %z wants +0100, not +01:00.
  normal <- sub("Z$", "+0000", raw)
  normal <- sub("([+-][0-9]{2}):([0-9]{2})$", "\\1\\2", normal)
  at <- as.POSIXct(strptime(normal, "%Y-%m-%dT%H:%M:%S%z", tz = "UTC"))
  if (is.na(at)) {
    return(NULL)
  }
  attr(at, "text") <- raw
  at
}
