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
#'   holds one JSON object: an unnamed list encodes as an array and the platform
#'   refuses it, saying so. 256 KiB is the ceiling and the platform refuses
#'   anything larger rather than truncating it.
#' @param if_version Optional. When given, the write succeeds only if the stored
#'   version still matches — the precondition is opt-in, and a caller that does
#'   not pass one is saying last-write-wins is what it wants.
#' @return The new version, invisibly.
#' @export
app_data_set <- function(value, if_version = NULL) {
  # **The body is the document, not a wrapper around it.** This used to send
  # `{"data": value, "if_version": n}`, and the route stores the body it is
  # given: every R app's document was its own value wrapped one layer deep, so
  # a write followed by a read did not round-trip.
  #
  # And the precondition is a header. In the body it was ignored, which turned
  # every conditional write into last-write-wins — the exact lost update the
  # argument is passed to prevent, and silently, since the call still succeeded.
  headers <- NULL
  if (!is.null(if_version)) {
    headers <- list("If-Match" = paste0("\"", if_version, "\""))
  }
  resp <- toft_request("PUT", "/api/v1/app/data", body = value, extra_headers = headers)
  invisible(resp$version)
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
#' @return The new version, invisibly.
#' @export
app_data_update <- function(fn, attempts = 5L) {
  for (i in seq_len(attempts)) {
    cur <- app_data_read()
    value <- fn(if (is.null(cur$data)) list() else cur$data)
    version <- tryCatch(
      app_data_set(value, if_version = cur$version),
      toft_error = function(e) {
        if (identical(e$code, "version_conflict")) return(NULL)
        stop(e)
      }
    )
    if (!is.null(version)) return(invisible(version))
  }
  stop(toft_refusal("version_conflict", sprintf(
    "the document kept changing under %d attempts; another writer is updating it constantly", attempts)))
}
