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
