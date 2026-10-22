# The one place this package talks to the platform.
#
# Every call carries the app token and the contract version, and every failure
# is an R condition naming what the platform said — never a silent NULL. An SDK
# that swallows an error is a worse outcome than one that has no function for
# the job, because the app goes on believing it sent something.

api_url <- function() {
  sub("/+$", "", require_env(ENV_API_URL))
}

# toft_request performs one call and returns the decoded body.
#
# status is checked before the body is parsed, so a proxy's HTML error page
# becomes "the platform answered 502" rather than a JSON parse error thirty
# lines away from its cause.
# `extra_headers` is a named list of request headers, and it is not a
# convenience.
#
# **Three separate features of this SDK each need exactly one request header,
# and none of them could send it.** The platform reads who a call is on behalf
# of from `X-Toft-Act-For`, and the app-data precondition from `If-Match`.
# With no way to set either, `viewer()` relayed the act-for blob as a query
# parameter the server never looks at — so every viewer resolved to nobody —
# `job_start()` put it in the body, losing the attribution it was passed to
# establish, and `app_data_set(if_version =)` sent a precondition that
# was ignored, quietly turning a compare-and-set into last-write-wins. All three
# failed silently, which is the one thing the note at the top of this file says
# an SDK must not do.
#
# `token` replaces the app's credential for the one caller that has its own: a
# job's callbacks to /api/v1/runner/*, made with the run token. `parse = FALSE`
# returns the response itself, redirects unfollowed, for the one route that
# answers with a presigned URL the app's token must not be sent to.
toft_request <- function(method, path, body = NULL, query = NULL, extra_headers = NULL,
                         token = NULL, parse = TRUE) {
  if (is.null(token)) {
    token <- app_or_job_token()
  }
  url <- paste0(api_url(), path)
  if (!is.null(query) && length(query)) {
    parts <- vapply(names(query), function(k) {
      paste0(utils::URLencode(k, reserved = TRUE), "=",
             utils::URLencode(as.character(query[[k]]), reserved = TRUE))
    }, character(1))
    url <- paste0(url, "?", paste(parts, collapse = "&"))
  }

  handle <- curl::new_handle()
  headers <- c(
    "Authorization" = paste("Bearer", token),
    "Accept" = "application/json"
  )
  headers[[CONTRACT_HEADER]] <- APP_CONTRACT_VERSION
  for (name in names(extra_headers)) {
    value <- extra_headers[[name]]
    if (!is.null(value) && nzchar(as.character(value))) {
      headers[[name]] <- as.character(value)
    }
  }

  if (!is.null(body)) {
    payload <- jsonlite::toJSON(body, auto_unbox = TRUE, null = "null")
    headers[["Content-Type"]] <- "application/json"
    curl::handle_setopt(handle, postfields = payload)
  }
  curl::handle_setopt(handle, customrequest = method)
  if (!parse) {
    curl::handle_setopt(handle, followlocation = FALSE)
  }
  curl::handle_setheaders(handle, .list = as.list(headers))

  resp <- curl::curl_fetch_memory(url, handle = handle)
  text <- rawToChar(resp$content)
  Encoding(text) <- "UTF-8"

  if (resp$status_code >= 400) {
    stop(toft_error(method, path, resp, text))
  }
  if (!parse) {
    return(resp)
  }
  if (!nzchar(trimws(text))) {
    return(invisible(NULL))
  }
  jsonlite::fromJSON(text, simplifyVector = FALSE)
}

# toft_error is the condition a refusal signals: class "toft_error" before
# "error", so tryCatch(toft_error = ) catches the platform's refusals and
# nothing else, and fields read with e$code, e$retryable, e$hint.
#
# **Whether to try again is the platform's call.** Every error body the
# platform writes carries `code`, `retryable` and, for codes that have one,
# `hint` — the next step in its own words. `retryable` is the platform's answer
# when the body has one, because only the handler knows that a 503 means "this
# install is configured without it, forever" or that a 409 means "wait for a
# run to finish"; the status rule (429 and 5xx) is the fallback for an answer
# that never reached a handler — a proxy's HTML page, an install older than
# the envelope. The message is the one this transport has always raised, so
# code matching it keeps working.
# toft_refusal is a refusal the SDK makes itself — a name nothing maps, a folder
# mounted twice, a level too low — as the same `toft_error` condition the
# platform's refusals are, so `tryCatch(..., toft_error = )` catches both and
# `e$code` names it. Never retryable: a mapping or a name is wrong, and trying
# again changes neither — the one exception is a wait that ran out, where the
# run is still going and waiting again is fair, so `retryable` is an argument.
# `status` is NA because no request was made. `hint` is the next step, written
# to be pasted, as the platform's own hints are.
toft_refusal <- function(code, message, hint = NULL, retryable = FALSE) {
  body <- list(code = code, retryable = retryable)
  if (!is.null(hint)) body$hint <- hint
  structure(
    class = c("toft_error", "error", "condition"),
    list(message = message, call = NULL, status = NA_integer_, code = code,
         retryable = retryable, hint = hint, retry_after = NULL,
         body = body)
  )
}

toft_error <- function(method, path, resp, text) {
  status <- resp$status_code
  body <- tryCatch(jsonlite::fromJSON(text, simplifyVector = FALSE),
                   error = function(e) NULL)
  if (!is.list(body) || (length(body) && is.null(names(body)))) {
    body <- list()
  }
  detail <- text
  if (is.character(body$error) && length(body$error) == 1) {
    detail <- body$error
  }
  code <- if (is.character(body$code) && length(body$code) == 1) body$code else ""
  retryable <- if (is.logical(body$retryable) && length(body$retryable) == 1 &&
                   !is.na(body$retryable)) {
    body$retryable
  } else {
    status == 429 || (status >= 500 && status != 501)
  }
  hint <- if (is.character(body$hint) && length(body$hint) == 1 && nzchar(body$hint)) {
    body$hint
  } else {
    NULL
  }
  retry_after <- NULL
  header <- curl::parse_headers_list(resp$headers)[["retry-after"]]
  if (!is.null(header)) {
    seconds <- suppressWarnings(as.numeric(header))
    if (!is.na(seconds) && seconds >= 0) retry_after <- seconds
  }
  structure(
    class = c("toft_error", "error", "condition"),
    list(
      message = sprintf("Toft %s %s failed (%d): %s", method, path, status, detail),
      call = NULL,
      status = status,
      code = code,
      retryable = retryable,
      hint = hint,
      retry_after = retry_after,
      body = body
    )
  )
}

# The app's own token in its service process; inside a job, which holds no app
# token, the run's own credential - accepted by the platform for sending through
# the app's integrations and nothing else (ADR §108). So notify() works from a
# job, and app_data_get() from a job is refused with job_send_only.
app_or_job_token <- function() {
  token <- trimws(Sys.getenv(ENV_APP_TOKEN, ""))
  if (nzchar(token)) {
    return(token)
  }
  if (nzchar(trimws(Sys.getenv(ENV_RUN_ID, "")))) {
    job <- trimws(Sys.getenv("TOFT_JOB_TOKEN", ""))
    if (nzchar(job)) {
      return(job)
    }
  }
  require_env(ENV_APP_TOKEN)
}
