# Starting a different entrypoint of *this* app, never another one — and, from
# inside a job, reporting back.
#
# The same operations the other three SDKs have, in R's shape: named arguments,
# a run as a list you read with `$`, and every refusal a `toft_error`
# condition with `code`, `retryable` and `hint`.
#
# **A plumber endpoint responds; it never waits.** job_start() returns as soon
# as the run is queued, and job_respond() answers the browser with the run's
# live status page. The gateway cuts every request at a deadline it writes on
# the request (`X-Toft-Request-Deadline`), and a handler that waited past it
# gave its caller a bare 504 while the work went on unseen — so job_wait()
# inside a request, with no timeout or one past that deadline, is refused at
# once with `would_outlive_request`. Waiting belongs to scripts and jobs.
#
# **A run ends exactly once.** Nothing re-runs by itself: a failure is one run
# with its `failure_reason`, and running it again is the app calling
# job_start() again, or a person pressing Run again in Toft.

# The routes are written out in each call rather than held in a constant:
# sdk/contract_test.go reads them there, against the platform's router, method
# and all — which is how this file's old routes were found never to have existed.
REQUEST_DEADLINE_HEADER <- "X-Toft-Request-Deadline"

# Every status but these two is final.
OPEN_RUN_STATUSES <- c("queued", "running")

# How often job_wait() re-reads a run when not told. A variable so a test need
# not sleep for it.
WAIT_POLL_SECONDS <- 2

# --- the run object ---------------------------------------------------------

# as_run turns the platform's run into a `toft_run`: the platform's own fields,
# read with `run$status`, plus the two things the SDK carries beside them as
# attributes — the act-for blob of the viewer it was started or read for, so a
# refresh or a cancel stays in that person's name, and the deadline of the
# request it was started in, so job_wait() knows without being told twice.
as_run <- function(body, act_for = NULL, request_deadline = NULL) {
  if (!is.list(body)) {
    body <- list()
  }
  structure(body, class = "toft_run", act_for = act_for,
            request_deadline = request_deadline)
}

#' @export
print.toft_run <- function(x, ...) {
  status <- if (is.null(x$status)) "?" else x$status
  extra <- ""
  if (identical(status, "queued") && !is.null(x$queue_position)) {
    extra <- sprintf(" (%s in line)", x$queue_position)
  } else if (identical(status, "running") && !is.null(x$progress)) {
    extra <- sprintf(" %s%%%s", x$progress,
                     if (is.null(x$progress_message)) "" else paste0(" ", x$progress_message))
  } else if (!is.null(x$failure_reason)) {
    extra <- paste0(": ", x$failure_reason)
  }
  cat(sprintf("<toft run %s of %s: %s%s>\n", x$id, x$job, status, extra))
  invisible(x)
}

run_headers <- function(run) {
  act_for <- attr(run, "act_for")
  if (is.null(act_for)) {
    return(NULL)
  }
  headers <- list()
  headers[[ACT_FOR_HEADER]] <- act_for
  headers
}

check_run <- function(run, what) {
  if (!inherits(run, "toft_run") || is.null(run$id) || !nzchar(run$id)) {
    stop(toft_refusal("bad_request", sprintf(
      "%s needs a run, as job_start(), job_get() or job_list() returned it", what)))
  }
}

# request_deadline reads the gateway's deadline off a request — plumber's
# `req`, its `req$HEADERS`, Shiny's `session$request` or a named list — as
# POSIXct, or NULL outside a request, where nothing will cut the connection.
request_deadline <- function(request) {
  raw <- trimws(request_header(request, REQUEST_DEADLINE_HEADER))
  if (!nzchar(raw)) {
    return(NULL)
  }
  parse_rfc3339(raw)
}

# request_header is find_header that also looks inside plumber's `req$HEADERS`,
# so a handler can pass `req` itself — whose CGI-named entries carry the same
# headers — or the vector, and get one answer.
request_header <- function(request, name) {
  value <- find_header(request, name)
  if (!nzchar(value) && (is.list(request) || is.environment(request)) &&
      !is.null(request[["HEADERS"]])) {
    value <- find_header(request[["HEADERS"]], name)
  }
  value
}

# RFC 3339 with a numeric offset or Z, fractional seconds dropped. strptime's
# %z wants +0100, not +01:00.
parse_rfc3339 <- function(raw) {
  normal <- sub("Z$", "+0000", raw)
  normal <- sub("([+-][0-9]{2}):([0-9]{2})$", "\\1\\2", normal)
  normal <- sub("\\.[0-9]+", "", normal)
  at <- as.POSIXct(strptime(normal, "%Y-%m-%dT%H:%M:%S%z", tz = "UTC"))
  if (is.na(at)) NULL else at
}

# --- starting work ----------------------------------------------------------

#' Start one of this app's jobs for the signed-in viewer
#'
#' The viewer half of \code{job_start()}: \code{viewer_jobs(req) |> job_start("export")}
#' starts the run in the name of the person whose request this is, so it is
#' attributed to them (\code{started_by: viewer}) and appears in their
#' \code{job_mine()}. The app never asserts who that is: it relays the
#' \code{X-Toft-Act-For} blob the gateway put on the request, which it can
#' neither mint nor read.
#'
#' The platform adds no role gate here — declaring the job in toft.toml is what
#' authorized the app to start it. Whether this person may press the button is
#' the app's rule, with \code{viewer()} or \code{caller_role()}.
#'
#' @param request The request being served: plumber's \code{req} or
#'   \code{req$HEADERS}, Shiny's \code{session$request}, or a named list.
#' @return A \code{toft_viewer_jobs} to pass to \code{job_start()} or
#'   \code{job_mine()}. Refused with \code{no_viewer} when nobody is signed in.
#' @export
viewer_jobs <- function(request) {
  blob <- request_header(request, ACT_FOR_HEADER)
  if (!nzchar(blob)) {
    stop(toft_refusal("no_viewer", paste0(
      "no ", ACT_FOR_HEADER, " header on this request, so there is no viewer to act ",
      "for. Toft's gateway sets it only for a signed-in browser session; a request ",
      "made with an API key, or one that reached the app around the gateway, carries ",
      "none. Start the job as the app itself, deliberately, rather than treating this ",
      "as the same thing."),
      hint = "use job_start(...) to start it as the app"))
  }
  structure(list(act_for = blob, request_deadline = request_deadline(request)),
            class = "toft_viewer_jobs")
}

#' @export
print.toft_viewer_jobs <- function(x, ...) {
  # Never the blob: it is a bearer credential for one session.
  cat("<toft viewer jobs (a signed-in person; the app cannot see who)>\n")
  invisible(x)
}

#' Start one of this app's jobs
#'
#' An app starts a different entrypoint of itself, declared in toft.toml. It
#' returns as soon as the run is queued: in a plumber endpoint, pass the run to
#' \code{job_respond()} and return.
#'
#' A repeated start is not a second run: a start matching a run already in
#' flight comes back as that run with \code{deduplicated} TRUE.
#'
#' @param job The job's name as toft.toml declares it — or, to start it for the
#'   signed-in person, the value of \code{viewer_jobs(req)} with the name next:
#'   \code{viewer_jobs(req) |> job_start("export", params = list(month = "2026-03"))}.
#' @param ... The job's name, when \code{job} is a \code{viewer_jobs()}.
#' @param params Optional named list — the run's only input, checked against
#'   the types, choices and required flags toft.toml declares. Large input goes
#'   into the app's storage first, and its name goes here.
#' @param idempotency_key Optional. A repeated start with the same key returns
#'   the first run for 24 hours.
#' @return A \code{toft_run}.
#' @export
job_start <- function(job, ..., params = NULL, idempotency_key = NULL) {
  viewer <- NULL
  rest <- list(...)
  if (inherits(job, "toft_viewer_jobs")) {
    viewer <- job
    job <- if (length(rest)) rest[[1]] else NULL
    rest <- rest[-1]
  }
  # Named arguments only after the name: a list in second place would
  # otherwise vanish into `...` and the run would start without its params.
  if (length(rest)) {
    stop(toft_refusal("bad_request",
      "job_start takes params and idempotency_key by name: job_start(\"export\", params = list(...))"))
  }
  if (!is.character(job) || length(job) != 1 || !nzchar(trimws(job))) {
    stop(toft_refusal("bad_request", "job_start needs the name of a job declared in toft.toml"))
  }
  # No params means no body at all, not an empty one: jsonlite renders an empty
  # R list as `[]`, and the route decodes its body into an object, so sending
  # one would turn "this job takes no parameters" into a 400.
  body <- NULL
  if (length(params)) {
    body <- list(params = params)
  }
  headers <- list()
  if (!is.null(viewer)) headers[[ACT_FOR_HEADER]] <- viewer$act_for
  if (!is.null(idempotency_key)) headers[["Idempotency-Key"]] <- idempotency_key
  resp <- toft_request(
    "POST",
    paste0("/api/v1/app/jobs/", utils::URLencode(job, reserved = TRUE), "/runs"),
    body = body,
    extra_headers = headers
  )
  as_run(resp, act_for = viewer$act_for, request_deadline = viewer$request_deadline)
}

#' Answer the request that started a run
#'
#' What a plumber endpoint returns instead of waiting. A browser — a request
#' whose Accept names text/html — is sent \code{303 See Other} to the run's live
#' status page, which Toft's gateway serves on the app's own origin: the job,
#' its progress and message, the outcome and its reason, with no app code.
#' Anything else gets \code{202 Accepted}, \code{Location:} the status page, and
#' the run as JSON.
#'
#' \preformatted{
#' #* @post /export
#' function(req, res) {
#'   run <- job_start("export", params = list(month = req$args$month))
#'   job_respond(run)
#' }
#' }
#'
#' @param run The \code{toft_run} \code{job_start()} returned.
#' @param res,req plumber's response and request. Found in the calling
#'   endpoint when it names them \code{res} and \code{req}, as plumber's do.
#' @return \code{res}, filled in — plumber sends a returned response as it is.
#'   With no plumber response to fill, a list of \code{status}, \code{headers}
#'   and \code{body} for any other framework to send.
#' @export
job_respond <- function(run, res = NULL, req = NULL) {
  check_run(run, "job_respond")
  caller <- parent.frame()
  if (is.null(res)) res <- get0("res", envir = caller, inherits = FALSE)
  if (is.null(req)) req <- get0("req", envir = caller, inherits = FALSE)

  location <- if (is.null(run$status_url)) "" else run$status_url
  accept <- tolower(request_header(req, "Accept"))
  browser <- nzchar(location) && grepl("text/html", accept, fixed = TRUE)

  status <- if (browser) 303L else 202L
  headers <- list()
  if (nzchar(location)) headers[["Location"]] <- location
  body <- ""
  if (!browser) {
    headers[["Content-Type"]] <- "application/json"
    fields <- unclass(run)
    attributes(fields) <- list(names = names(fields))
    body <- as.character(jsonlite::toJSON(fields, auto_unbox = TRUE, null = "null"))
  }

  # A plumber response is an R6 object with setHeader; filled in and returned,
  # plumber sends it without serializing it again.
  if (is.environment(res) && is.function(res$setHeader)) {
    res$status <- status
    for (name in names(headers)) res$setHeader(name, headers[[name]])
    res$body <- body
    return(res)
  }
  list(status = status, headers = headers, body = body)
}

# --- reading runs -----------------------------------------------------------

#' Read one run
#'
#' @param id The run's id.
#' @return A \code{toft_run}.
#' @export
job_get <- function(id) {
  read_run(id)
}

read_run <- function(id, act_for = NULL, request_deadline = NULL) {
  if (!is.character(id) || length(id) != 1 || !nzchar(trimws(id))) {
    stop(toft_refusal("bad_request", "a run id is needed to read a run"))
  }
  headers <- NULL
  if (!is.null(act_for)) {
    headers <- list()
    headers[[ACT_FOR_HEADER]] <- act_for
  }
  resp <- toft_request("GET", paste0("/api/v1/app/job-runs/", utils::URLencode(id, reserved = TRUE)),
                       extra_headers = headers)
  as_run(resp, act_for = act_for, request_deadline = request_deadline)
}

#' This app's runs, newest first
#'
#' @param job Optional. One job's runs; NULL is every job's.
#' @param status Optional. Runs in one status: "queued", "running",
#'   "succeeded", "failed", "timed_out", "out_of_memory", "cancelled" or
#'   "skipped".
#' @param limit How many.
#' @return A list of \code{toft_run}.
#' @export
job_list <- function(job = NULL, status = NULL, limit = 50) {
  list_runs(job = job, status = status, limit = limit)
}

#' This viewer's runs of this app, newest first
#'
#' What makes coming back tomorrow work with nothing stored by the app: the
#' platform keeps the run against the person, and answers for the person it
#' resolves from the relayed act-for blob. The app still never learns who.
#'
#' @param request The request being served (see \code{viewer_jobs()}), or the
#'   value of \code{viewer_jobs()} itself.
#' @param job Optional. One job's runs.
#' @param limit How many.
#' @return A list of \code{toft_run}. Refused with \code{no_viewer} when nobody
#'   is signed in.
#' @export
job_mine <- function(request, job = NULL, limit = 20) {
  viewer <- if (inherits(request, "toft_viewer_jobs")) request else viewer_jobs(request)
  list_runs(job = job, limit = limit, scope = "viewer", viewer = viewer)
}

list_runs <- function(job = NULL, status = NULL, limit = 50, scope = NULL, viewer = NULL) {
  query <- list(limit = limit)
  if (!is.null(scope)) query$scope <- scope
  if (!is.null(job)) query$job <- job
  if (!is.null(status)) query$status <- status
  headers <- NULL
  if (!is.null(viewer)) {
    headers <- list()
    headers[[ACT_FOR_HEADER]] <- viewer$act_for
  }
  resp <- toft_request("GET", "/api/v1/app/job-runs", query = query, extra_headers = headers)
  lapply(if (is.null(resp$runs)) list() else resp$runs, as_run,
         act_for = viewer$act_for, request_deadline = viewer$request_deadline)
}

#' Read a run again
#'
#' @param run A \code{toft_run}.
#' @return A fresh \code{toft_run}, still in the same viewer's name.
#' @export
job_refresh <- function(run) {
  check_run(run, "job_refresh")
  read_run(run$id, act_for = attr(run, "act_for"),
           request_deadline = attr(run, "request_deadline"))
}

#' Wait for a run to end
#'
#' For scripts and jobs. **Inside a request it is refused at once** — code
#' \code{would_outlive_request}, not retryable — with no \code{timeout}, or one
#' past what is left of the request: the gateway would cut the connection at
#' its deadline and the browser would get a bare 504. The hint is the fix:
#' \code{return(job_respond(run))}. A short wait that fits is allowed.
#'
#' A run started through \code{viewer_jobs(req)} already knows its request;
#' for one the app started as itself, pass \code{request}.
#'
#' @param run A \code{toft_run}.
#' @param timeout Seconds to wait; NULL is until the run ends. A timeout that
#'   runs out is refused with \code{wait_timeout}, retryable: the run is still
#'   going.
#' @param request The request being served, if any (see \code{viewer_jobs()}).
#' @param poll Seconds between reads.
#' @return The run in its final state.
#' @export
job_wait <- function(run, timeout = NULL, request = NULL, poll = WAIT_POLL_SECONDS) {
  check_run(run, "job_wait")
  deadline <- if (!is.null(request)) request_deadline(request) else attr(run, "request_deadline")
  if (!is.null(deadline)) {
    left <- as.numeric(difftime(deadline, Sys.time(), units = "secs"))
    if (is.null(timeout) || timeout >= left) {
      asked <- if (is.null(timeout)) "no timeout" else sprintf("a timeout of %ss", timeout)
      stop(toft_refusal("would_outlive_request", sprintf(paste0(
        "waiting on run %s with %s would outlive the request this code is serving, ",
        "which the gateway cuts off in %.0fs - the caller would get a bare 504. ",
        "Respond with the run and let the browser follow it."), run$id, asked, max(left, 0)),
        hint = "return(job_respond(run))"))
    }
  }
  started <- Sys.time()
  current <- run
  repeat {
    # A read the app's own rate limit refused is not the run failing: wait out
    # Retry-After and read again. Every other app call spends the same
    # limiter, so a busy app hits it mid-wait.
    pause <- NULL
    current <- tryCatch(job_refresh(current), toft_error = function(e) {
      if (!identical(e$code, "rate_limited")) stop(e)
      pause <<- if (is.null(e$retry_after)) poll else e$retry_after
      current
    })
    if (!is.null(pause)) {
      Sys.sleep(pause)
      next
    }
    if (!is.null(current$status) && !current$status %in% OPEN_RUN_STATUSES) {
      return(current)
    }
    if (!is.null(timeout) &&
        as.numeric(difftime(Sys.time(), started, units = "secs")) + poll > timeout) {
      stop(toft_refusal("wait_timeout", sprintf(
        "run %s was still %s when the wait was cut short", run$id, current$status),
        retryable = TRUE))
    }
    Sys.sleep(poll)
  }
}

#' A run's captured output lines
#'
#' @param run A \code{toft_run}.
#' @param after_seq Lines after this sequence number; 0 is from the start.
#' @return A data frame of \code{seq}, \code{at}, \code{stream} and
#'   \code{line}, one row per line.
#' @export
job_logs <- function(run, after_seq = 0) {
  check_run(run, "job_logs")
  resp <- toft_request("GET", paste0("/api/v1/app/job-runs/", utils::URLencode(run$id, reserved = TRUE), "/logs"),
                       query = list(after = after_seq), extra_headers = run_headers(run))
  lines <- if (is.null(resp$lines)) list() else resp$lines
  pick <- function(field, empty) {
    vapply(lines, function(l) if (is.null(l[[field]])) empty else l[[field]], empty)
  }
  data.frame(seq = pick("seq", 0), at = pick("at", ""), stream = pick("stream", ""),
             line = pick("line", ""), stringsAsFactors = FALSE)
}

#' A run's output
#'
#' Fetched over a presigned URL the platform mints; the app's own token is never
#' sent to the store it points at.
#'
#' @param run A \code{toft_run}.
#' @param json TRUE to decode the output as JSON (rows become a data frame).
#' @return The output as a raw vector, or decoded; NULL when the run produced
#'   none. An output that existed and has expired is refused with
#'   \code{output_expired} rather than read as none.
#' @export
job_output <- function(run, json = FALSE) {
  check_run(run, "job_output")
  path <- paste0("/api/v1/app/job-runs/", utils::URLencode(run$id, reserved = TRUE), "/output")
  resp <- tryCatch(
    toft_request("GET", path, extra_headers = run_headers(run), parse = FALSE),
    toft_error = function(e) {
      if (identical(e$status, 404L) || identical(e$status, 404)) NULL else stop(e)
    }
  )
  if (is.null(resp)) {
    return(NULL)
  }
  content <- resp$content
  if (resp$status_code >= 300 && resp$status_code < 400) {
    location <- curl::parse_headers_list(resp$headers)[["location"]]
    # A fresh handle: the presigned URL is the whole authorization, and the
    # app's bearer token has no business at the store's host.
    fetched <- curl::curl_fetch_memory(location, handle = curl::new_handle())
    if (fetched$status_code >= 400) {
      stop(toft_refusal("output_unreadable", sprintf(
        "fetching run %s's output from the store answered %d", run$id, fetched$status_code),
        retryable = fetched$status_code >= 500))
    }
    content <- fetched$content
  }
  if (!json) {
    return(content)
  }
  text <- rawToChar(content)
  Encoding(text) <- "UTF-8"
  jsonlite::fromJSON(text)
}

#' Stop a run
#'
#' A run a viewer started needs that viewer to cancel it — the run from
#' \code{viewer_jobs(req) |> job_start()} carries them; the app acting as itself
#' may cancel a run with no person behind it.
#'
#' @param run A \code{toft_run}.
#' @return The run, invisibly.
#' @export
job_cancel <- function(run) {
  check_run(run, "job_cancel")
  toft_request("POST", paste0("/api/v1/app/job-runs/", utils::URLencode(run$id, reserved = TRUE), "/cancel"),
               extra_headers = run_headers(run))
  invisible(run)
}

# --- inside a job -----------------------------------------------------------

#' This job's own run, or NULL outside a job
#'
#' NULL rather than an error, because \code{if (!is.null(current_run()))} is how
#' code shared between an app's web entrypoint and its job tells which one it
#' is in.
#'
#' @return A list: \code{id}, \code{job}, \code{rerun_of} (the run this one
#'   repeats, or NULL), \code{trigger} and \code{trigger_role}, \code{params}
#'   (declared params with defaults applied, typed — the only input),
#'   \code{scheduled_for} and \code{deadline} (POSIXct, or NULL).
#' @export
current_run <- function() {
  id <- trimws(Sys.getenv(ENV_RUN_ID, ""))
  token <- trimws(Sys.getenv(ENV_JOB_TOKEN, ""))
  if (!nzchar(id) || !nzchar(token)) {
    return(NULL)
  }
  blank_null <- function(name) {
    value <- trimws(Sys.getenv(name, ""))
    if (nzchar(value)) value else NULL
  }
  raw_params <- blank_null(ENV_PARAMS)
  deadline <- blank_null(ENV_DEADLINE)
  structure(list(
    id = id,
    job = blank_null(ENV_JOB_NAME),
    rerun_of = blank_null(ENV_RERUN_OF),
    trigger = blank_null(ENV_TRIGGER),
    trigger_role = if (is.null(blank_null(ENV_TRIGGER_ROLE))) "unknown" else blank_null(ENV_TRIGGER_ROLE),
    params = if (is.null(raw_params)) list() else jsonlite::fromJSON(raw_params, simplifyVector = FALSE),
    scheduled_for = scheduled_for(),
    deadline = if (is.null(deadline)) NULL else parse_rfc3339(deadline)
  ), class = "toft_current_run")
}

require_run <- function(what) {
  if (!nzchar(trimws(Sys.getenv(ENV_RUN_ID, ""))) || !nzchar(trimws(Sys.getenv(ENV_JOB_TOKEN, "")))) {
    stop(toft_refusal("no_run", sprintf(
      "%s works only inside a job: TOFT_RUN_ID and TOFT_JOB_TOKEN are not set", what)))
  }
}

#' Report how far along this run is
#'
#' Shown on the run's status page and in Toft, beside the message.
#'
#' @param percent 0 to 100.
#' @param message Optional short text — "loading rows".
#' @return NULL, invisibly.
#' @export
run_progress <- function(percent, message = NULL) {
  require_run("run_progress()")
  body <- list(version = RUNNER_CONTRACT_VERSION, percent = as.integer(percent))
  if (!is.null(message) && nzchar(message)) body$message <- message
  headers <- list()
  headers[[RUNNER_VERSION_HEADER]] <- RUNNER_CONTRACT_VERSION
  # The run's own token, not the app's: it reaches the callback routes and its
  # own run, and expires with the run.
  toft_request("POST", "/api/v1/runner/progress", body = body, extra_headers = headers,
               token = trimws(Sys.getenv(ENV_JOB_TOKEN, "")))
  invisible(NULL)
}

#' Write this run's output
#'
#' The run's one output, uploaded to the presigned location the platform minted
#' for it; the app reads it back with \code{job_output()}. For anything large,
#' write it to the app's storage and make the output name it.
#'
#' @param data A raw vector or a character string — or, with \code{json = TRUE},
#'   any value jsonlite can encode.
#' @param json TRUE to encode \code{data} as JSON.
#' @return NULL, invisibly.
#' @export
run_write_output <- function(data, json = FALSE) {
  require_run("run_write_output()")
  uri <- trimws(Sys.getenv(ENV_OUTPUT_URI, ""))
  if (!nzchar(uri)) {
    stop(toft_refusal("no_output", "this run has no output location"))
  }
  if (json) {
    data <- jsonlite::toJSON(data, auto_unbox = TRUE, null = "null")
  }
  if (!is.raw(data)) {
    data <- charToRaw(enc2utf8(paste(as.character(data), collapse = "\n")))
  }
  handle <- curl::new_handle()
  curl::handle_setopt(handle, customrequest = "PUT", post = TRUE,
                      postfieldsize = length(data), postfields = data)
  resp <- curl::curl_fetch_memory(uri, handle = handle)
  if (resp$status_code >= 400) {
    stop(toft_refusal("output_unwritable", sprintf(
      "uploading this run's output answered %d", resp$status_code),
      retryable = resp$status_code >= 500))
  }
  invisible(NULL)
}

#' Write one line to this run's log
#'
#' The runner captures stdout and stderr, so this is a print with the stream
#' named — and the last line on stderr is what a failed run gives as its reason.
#'
#' @param message The line.
#' @param error TRUE for stderr.
#' @export
run_log <- function(message, error = FALSE) {
  cat(message, "\n", sep = "", file = if (error) stderr() else stdout())
  invisible(NULL)
}
