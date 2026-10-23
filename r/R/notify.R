# The app supplies the message; the platform supplies the identity.

#' Send a message through one of this app's integrations
#'
#' The app never speaks in the platform's voice, and it never chooses where the
#' message lands: an administrator attaches a connection to this app, and this
#' names which kind to use. All four gates are checked by the platform before
#' the transport, so a kind that is not attached is a refusal with a distinct
#' code rather than a message that goes nowhere.
#'
#' @param kind The integration kind, e.g. "slack" or "email".
#' @param ... Fields for that kind — the platform documents each one. Passed
#'   through as the message body.
#' @param idempotency_key Optional. A retry carrying the same key is answered
#'   with the first send rather than sent again. It travels as the
#'   `Idempotency-Key` header, as in the other SDKs: the platform reads only
#'   the header, and as a body field it was ignored without a word.
#' @return Invisibly, the platform's response.
#' @export
notify <- function(kind, ..., idempotency_key = NULL) {
  body <- list(...)
  invisible(toft_request("POST",
    paste0("/api/v1/app/notify/", utils::URLencode(kind, reserved = TRUE)),
    body = body,
    extra_headers = list("Idempotency-Key" = idempotency_key)))
}

#' Read back one send this app made
#'
#' The send's record, as the platform keeps it: whether it went, was refused by
#' the transport, or was blocked before it — the answer `notify()` cannot give,
#' because a send is accepted and recorded before it is delivered. The only
#' thing an app may read about its own sends, and deliberately so: there is no
#' listing, and an id it minted tells it nothing it did not know.
#'
#' @param send The send's id, or what `notify()` returned (its `send_id`).
#' @return A list: `id`, `status` (`queued`, `sent`, `failed`, `blocked`,
#'   `abandoned`), `kind`, `action`, `error`, and the derived `done` (finished,
#'   one way or another), `delivered` (`sent`) and `blocked`.
#' @export
notify_status <- function(send) {
  # No `%||%`: it is base R only from 4.4, and this package runs on 4.3.
  id <- if (is.list(send)) {
    if (!is.null(send$send_id)) send$send_id else send$id
  } else send
  if (is.null(id) || !nzchar(as.character(id))) {
    stop(toft_refusal("bad_request", "notify_status() needs the id of a send"))
  }
  raw <- toft_request("GET", paste0("/api/v1/app/sends/", utils::URLencode(as.character(id), reserved = TRUE)))
  status <- if (is.null(raw$status)) "queued" else as.character(raw$status)
  kind <- if (!is.null(raw$kind)) raw$kind else raw$integration
  list(
    id = if (!is.null(raw$send_id)) raw$send_id else if (!is.null(raw$id)) raw$id else as.character(id),
    status = status,
    kind = or_empty(kind),
    action = or_empty(raw$action),
    error = or_empty(raw$error),
    done = status %in% c("sent", "failed", "blocked", "abandoned"),
    delivered = identical(status, "sent"),
    blocked = identical(status, "blocked")
  )
}
