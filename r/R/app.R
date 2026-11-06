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
  if (isTRUE(refresh)) .app_cache$address <- NULL
  if (isTRUE(refresh) || is.null(.app_cache$view)) {
    .app_cache$view <- toft_request("GET", "/api/v1/app/self")
  }
  .app_cache$view
}

.app_cache <- new.env(parent = emptyenv())

#' The absolute address of a page in this app, for a link that leaves it
#'
#' For an email, a Slack post, a PDF, a ticket — anywhere a relative link names
#' nothing. Inside the app's own pages, relative links built from
#' [app_base_path()] stay the right tool.
#'
#' ```r
#' app_url("/reports/42")   # https://apps.example.com/apps/reporter/reports/42
#' app_url()                # the app's front page
#' app_url(run$status_url)  # a run's status page
#' ```
#'
#' A path is the app's: `/reports/42` and `reports/42` both mean the app's own
#' route, never the host's root, and one that already carries the base path is
#' not prefixed twice. A query or fragment is kept; an absolute URL is refused
#' `bad_request`.
#'
#' The address is asked of the platform rather than stored, and reused for
#' sixty seconds: an administrator can move the install or give the app a
#' hostname of its own while it runs. Works the same in a request, a job or a
#' schedule. If the platform cannot be reached the last address this process
#' was told is used; with none, the error is raised. An install with no address
#' configured refuses with `no_public_address`.
#'
#' @param path A path inside the app.
#' @return The absolute URL, as a string.
#' @export
app_url <- function(path = "") {
  check_app_path(path)
  address <- current_app_address()
  join_app_url(address$public_url, address$base_path, path)
}

APP_URL_TTL <- 60

current_app_address <- function() {
  now <- as.numeric(Sys.time())
  cached <- .app_cache$address
  if (!is.null(cached) && now - .app_cache$address_at < APP_URL_TTL) {
    return(cached)
  }
  got <- tryCatch(toft_request("GET", "/api/v1/app/url"), toft_error = function(e) e)
  if (inherits(got, "toft_error")) {
    # Unreachable — no status, no code — is the one failure a known address
    # outlives. A refusal is the platform's answer and stands.
    if (!is.null(cached) && is.na(got$status) && !nzchar(if (is.null(got$code)) "" else got$code)) {
      return(cached)
    }
    stop(got)
  }
  field <- function(x) if (is.null(x)) "" else as.character(x)
  .app_cache$address <- list(public_url = field(got$public_url), base_path = field(got$base_path))
  .app_cache$address_at <- now
  .app_cache$address
}

check_app_path <- function(path) {
  if (!is.character(path) || length(path) != 1 || is.na(path)) {
    stop(toft_refusal("bad_request", "app_url() takes one path, as a string",
                      hint = 'app_url("/reports/42")'))
  }
  if (grepl("^[A-Za-z][A-Za-z0-9+.-]*:", path) || startsWith(path, "//")) {
    stop(toft_refusal("bad_request",
                      sprintf("app_url() was given an absolute URL (%s), which it would join onto this app's address", path),
                      hint = 'pass the path inside the app, e.g. app_url("/reports/42")'))
  }
  invisible(TRUE)
}

# join_app_url is app_url()'s rule, held to sdk/testdata/app-url-vector.json
# with the other three SDKs.
join_app_url <- function(public_url, base_path, path) {
  check_app_path(path)
  if (nzchar(base_path) && startsWith(path, base_path)) {
    rest <- substr(path, nchar(base_path) + 1, nchar(path))
    if (!nzchar(rest) || substr(rest, 1, 1) %in% c("/", "?", "#")) path <- rest
  }
  if (!nzchar(path) || substr(path, 1, 1) != "/") path <- paste0("/", path)
  paste0(sub("/+$", "", public_url), path)
}

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
#' this is a private (0700) `toft-scratch-<uid>/<app>` under the temp directory,
#' as in Python and Go — `<app>` keyed on the working directory — so a script
#' under development does not have to be edited to run in either place, and
#' what it wrote is there on the next run as it is between restarts.
#'
#' @return A path, created if it did not exist.
#' @export
scratch_dir <- function() {
  dir <- trimws(Sys.getenv(ENV_SCRATCH_DIR, ""))
  if (!nzchar(dir)) {
    return(local_scratch_root())
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
  # The instant is right in any zone; the zone is the schedule's, so a
  # format() of it reads the tick as the schedule wrote it. The runner sends
  # only an offset, and R names a whole-hour offset Etc/GMT-N (the sign is
  # POSIX's, inverted); a half-hour offset has no such zone and stays UTC,
  # with the text attribute holding what was sent.
  attr(at, "tzone") <- offset_zone(raw)
  attr(at, "text") <- raw
  at
}

offset_zone <- function(raw) {
  m <- regmatches(raw, regexec("([+-])([0-9]{2}):?([0-9]{2})$", raw))[[1]]
  if (length(m) != 4 || m[4] != "00") return("UTC")
  hours <- as.integer(m[3])
  if (hours == 0) return("UTC")
  # +05:00 is Etc/GMT-5.
  sprintf("Etc/GMT%s%d", if (m[2] == "+") "-" else "+", hours)
}

# Off-platform scratch: <tmp>/toft-scratch-<uid>/<app>, 0700, as Python's and
# Go's (D-12; it was tempdir(), gone with the session). Never the shared temp
# directory itself, where every user on the machine can read what an app wrote;
# per user, so a directory another user created first is not adopted; per app,
# keyed on the working directory. A parent that is not a real directory owned by
# this user with no group or other access is not used — the session's own
# tempdir() stands in.
local_scratch_root <- function() {
  tmp <- dirname(tempdir())
  uid <- file.info(tempdir(), extra_cols = TRUE)$uid
  if (is.null(uid) || is.na(uid)) uid <- Sys.info()[["effective_user"]]
  parent <- file.path(tmp, paste0("toft-scratch-", uid))
  app <- substr(paste0(as.character(openssl::sha256(getwd())), collapse = ""), 1, 12)
  root <- file.path(parent, app)
  for (d in c(parent, root)) {
    if (!dir.exists(d)) dir.create(d, showWarnings = FALSE, mode = "0700")
    if (!private_dir(d, uid)) {
      return(tempdir())
    }
  }
  root
}

private_dir <- function(path, uid) {
  if (!dir.exists(path) || nzchar(Sys.readlink(path))) return(FALSE)
  info <- file.info(path, extra_cols = TRUE)
  if (.Platform$OS.type != "unix") return(TRUE)
  if (!identical(as.integer(info$uid), as.integer(uid))) return(FALSE)
  bitwAnd(as.integer(info$mode), strtoi("077", 8L)) == 0L
}
