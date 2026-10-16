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
# `job_start(act_for =)` put it in the body, losing the attribution it was
# passed to establish, and `app_data_set(if_version =)` sent a precondition that
# was ignored, quietly turning a compare-and-set into last-write-wins. All three
# failed silently, which is the one thing the note at the top of this file says
# an SDK must not do.
toft_request <- function(method, path, body = NULL, query = NULL, extra_headers = NULL) {
  token <- app_or_job_token()
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
  curl::handle_setheaders(handle, .list = as.list(headers))

  resp <- curl::curl_fetch_memory(url, handle = handle)
  text <- rawToChar(resp$content)
  Encoding(text) <- "UTF-8"

  if (resp$status_code >= 400) {
    detail <- text
    parsed <- tryCatch(jsonlite::fromJSON(text), error = function(e) NULL)
    if (!is.null(parsed) && !is.null(parsed$error)) {
      detail <- parsed$error
    }
    stop(sprintf("Toft %s %s failed (%d): %s",
                 method, path, resp$status_code, detail), call. = FALSE)
  }
  if (!nzchar(trimws(text))) {
    return(invisible(NULL))
  }
  jsonlite::fromJSON(text, simplifyVector = FALSE)
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
