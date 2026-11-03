# The app's own document.
#
# One JSON document, not a store: no keys, no listing, no queries. The bytes are
# the app's — the platform stores them as text and never reads them — and
# never-written, cleared and an empty object stay tellable apart.

#' Read this app's stored document
#'
#' Returns a list, or NULL when nothing has ever been written. NULL and an empty
#' list are different answers and the platform keeps them apart, so an app can
#' tell "first run" from "cleared".
#'
#' @return The stored document as a list, or NULL.
#' @export
app_data_get <- function() {
  resp <- toft_request("GET", "/api/v1/app/data")
  if (is.null(resp)) {
    return(NULL)
  }
  resp$data
}

#' Replace this app's stored document
#'
#' @param value A **named** list, written as the JSON document itself. App data
#'   holds one JSON object, so anything else — a vector, a string, an unnamed
#'   list, a data frame — is refused `bad_request` before any request. An empty
#'   `list()` is written as the empty object `{}`. 256 KiB is the ceiling and
#'   the platform refuses anything larger rather than truncating it.
#' @param if_version Optional. When given, the write succeeds only if the stored
#'   version still matches — the precondition is opt-in, and a caller that does
#'   not pass one is saying last-write-wins is what it wants.
#' @return Invisibly, the document written, as `app_data_read()` describes one:
#'   `data` (what a read now answers), `version` and `empty` (`FALSE`). The
#'   same shape the other three SDKs' `set()` returns.
#' @export
app_data_set <- function(value, if_version = NULL) {
  value <- as_document(value, "app_data_set()")
  # **The body is the document, not a wrapper around it.** This used to send
  # `{"data": value, "if_version": n}`, and the route stores the body it is
  # given: every R app's document was its own value wrapped one layer deep, so
  # a write followed by a read did not round-trip.
  #
  # And the precondition is a header. In the body it was ignored, which turned
  # every conditional write into last-write-wins — the exact lost update the
  # argument is passed to prevent, and silently, since the call still succeeded.
  resp <- toft_request("PUT", "/api/v1/app/data", body = value, extra_headers = if_match(if_version))
  # The platform's answer is metadata only. The document comes back as a read
  # would decode it — through JSON — so a write and the read after it agree.
  written <- jsonlite::fromJSON(jsonlite::toJSON(value, auto_unbox = TRUE, null = "null"),
                                simplifyVector = FALSE)
  if (length(written) == 0) written <- stats::setNames(list(), character(0))
  version <- if (is.null(resp$version)) NA_integer_ else as.integer(resp$version)
  invisible(list(data = written, version = version, empty = FALSE))
}

#' Throw this app's stored document away
#'
#' Puts the app back to never having written one: the next
#' `app_data_get()` is `NULL`, not an empty list. Not the same as writing
#' `list()`, deliberately — an app that cleared its settings and one that saved
#' an empty document mean different things, and the next read tells them apart.
#'
#' @param if_version Optional. When given, the document is removed only if the
#'   stored version still matches; otherwise `version_conflict`.
#' @return `TRUE` when there was a document to remove, `FALSE` when there was
#'   none.
#' @export
app_data_clear <- function(if_version = NULL) {
  resp <- toft_request("DELETE", "/api/v1/app/data", extra_headers = if_match(if_version))
  isTRUE(resp$deleted)
}

# The precondition header, or none.
if_match <- function(if_version) {
  if (is.null(if_version)) {
    return(NULL)
  }
  text <- if (is.numeric(if_version) && length(if_version) == 1 && !is.na(if_version)) {
    format(if_version, scientific = FALSE)
  } else if (is.character(if_version) && length(if_version) == 1) {
    if_version
  } else {
    ""
  }
  if (!grepl("^[0-9]+$", text)) {
    stop(toft_refusal("bad_request", "if_version is the whole-number version a read returned"))
  }
  list("If-Match" = paste0("\"", text, "\""))
}

# as_document checks that value is one JSON object, and returns it ready to
# encode. Refused locally, bad_request, because the alternatives were all worse:
# 1:3 and "str" reached the platform as an array and a string, list(1, 2) as an
# array, and list() as `[]` — so an R app could not write the empty object at
# all.
as_document <- function(value, what) {
  if (!is.list(value) || is.data.frame(value)) {
    stop(toft_refusal("bad_request", sprintf(paste0(
      "%s takes a named list — app data holds one JSON object, and a %s is not one"),
      what, if (is.data.frame(value)) "data frame" else class(value)[1]),
      hint = "pass a named list, such as list(period = \"monthly\")"))
  }
  if (length(value) == 0) {
    # Named and empty, which jsonlite writes as {} rather than [].
    return(stats::setNames(list(), character(0)))
  }
  keys <- names(value)
  if (is.null(keys) || any(is.na(keys)) || any(!nzchar(keys))) {
    stop(toft_refusal("bad_request", sprintf(paste0(
      "%s takes a named list — every element needs a name, because an unnamed ",
      "list is a JSON array and app data holds one JSON object"), what),
      hint = "name every element: list(a = 1, b = 2)"))
  }
  value
}

#' Read this app's stored document with its version
#'
#' `app_data_get()` answers the document alone, which is enough to read and not
#' enough to write safely: a conditional `app_data_set(if_version = )` needs the
#' version the document was read at, and only this call returns it.
#'
#' @return A list: `data` (the document, or NULL when never written), `version`
#'   (0 when never written) and `empty` (TRUE when never written).
#' @export
app_data_read <- function() {
  resp <- toft_request("GET", "/api/v1/app/data")
  data <- if (is.null(resp)) NULL else resp$data
  version <- if (is.null(resp) || is.null(resp$version)) 0L else as.integer(resp$version)
  list(data = data, version = version, empty = is.null(data))
}

#' Change this app's stored document without losing another writer's change
#'
#' Reads the document and its version, applies `fn`, and writes the result only
#' if nobody wrote in between — retrying the read on a conflict. Two requests
#' updating the document at once both land, where `app_data_get()` then
#' `app_data_set()` keeps whichever wrote last. The transport retries a
#' `rate_limited` refusal once, as every R call does; past that it is raised.
#'
#' @param fn A function taking the current document (a list; an empty list when
#'   never written) and returning the new one, a named list.
#' @param attempts How many conflicts to retry before giving up.
#' @return Invisibly, the document written, as `app_data_set()` returns it.
#' @export
app_data_update <- function(fn, attempts = 5L) {
  if (!is.function(fn)) {
    stop(toft_refusal("bad_request",
      "app_data_update() takes a function that is given the current document and returns the new one",
      hint = "app_data_update(function(doc) { doc$runs <- (if (is.null(doc$runs)) 0 else doc$runs) + 1; doc })"))
  }
  if (!is.numeric(attempts) || length(attempts) != 1 || is.na(attempts) || attempts < 1) {
    stop(toft_refusal("bad_request", "attempts is how many conflicts to retry, at least 1"))
  }
  for (i in seq_len(attempts)) {
    cur <- app_data_read()
    value <- fn(if (is.null(cur$data)) list() else cur$data)
    written <- tryCatch(
      app_data_set(value, if_version = cur$version),
      toft_error = function(e) {
        if (identical(e$code, "version_conflict")) return(NULL)
        stop(e)
      }
    )
    if (!is.null(written)) return(invisible(written))
  }
  stop(toft_refusal("version_conflict", sprintf(
    "the document kept changing under %d attempts; another writer is updating it constantly", attempts)))
}
