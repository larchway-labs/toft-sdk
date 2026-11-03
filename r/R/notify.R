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
  # Checked before the request: a NULL kind built the route /notify/, which
  # the platform answered route_not_found — a message about routing for what
  # was a missing argument.
  if (missing(kind) || !is.character(kind) || length(kind) != 1 || is.na(kind) || !nzchar(trimws(kind))) {
    stop(toft_refusal("bad_request", "notify() needs the integration kind as its first argument, such as \"email\" or \"slack\"",
                      hint = "notify(\"email\", to = \"a@example.com\", subject = \"...\", text = \"...\")"))
  }
  body <- list(...)
  # Checked before the request, as the other SDKs do: a refusal the SDK can
  # make itself should not cost a rate-limit token and come back as a 400.
  if ("text" %in% names(body) && !is_text(body$text)) {
    stop(toft_refusal("bad_request", sprintf("%s text needs some text", kind),
                      hint = sprintf("notify(\"%s\", text = \"...\")", kind)))
  }
  if (identical(kind, "email")) {
    to <- body$to
    if (is.list(to) && all(vapply(to, function(a) is.character(a) && length(a) == 1, logical(1)))) {
      to <- unlist(to, use.names = FALSE)
    }
    if (is.null(to) || !is.character(to) || anyNA(to)) {
      stop(toft_refusal("bad_request", sprintf(
        "'to' takes an address or a character vector of addresses, got %s",
        if (is.null(to)) "NULL" else class(to)[1]),
        hint = "notify(\"email\", to = \"someone@example.com\", subject = \"...\", text = \"...\")"))
    }
    if (!any(nzchar(trimws(to)))) {
      stop(toft_refusal("no_recipient", "email needs at least one recipient in 'to'",
                        hint = "name who it goes to: pass an address in to (or cc, bcc) - an email has no default recipient"))
    }
    for (f in c("subject", "text")) {
      if (!is_text(body[[f]])) {
        stop(toft_refusal("bad_request", sprintf("email %s needs some text", f)))
      }
    }
  } else if (kind %in% c("slack", "teams") && !("text" %in% names(body))) {
    stop(toft_refusal("bad_request", sprintf("%s needs the message as text", kind),
                      hint = sprintf("notify(\"%s\", text = \"...\")", kind)))
  }
  # A recipient field is always a list on the wire. toJSON's auto_unbox turns
  # a lone address, to = "a@x", into a JSON string, which the platform refuses
  # (drills skill review 2026-11-02); one address works as several do.
  for (f in intersect(c("to", "cc", "bcc"), names(body))) {
    if (is.character(body[[f]])) body[[f]] <- as.list(body[[f]])
  }
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

is_text <- function(x) is.character(x) && length(x) == 1 && !is.na(x) && nzchar(trimws(x))
