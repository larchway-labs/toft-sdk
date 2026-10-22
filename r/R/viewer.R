# Who is looking, and what they may do — two different questions with two
# different answers, and the difference matters.

# find_header reads one header out of whatever the framework calls its header
# collection, and it is the same lookup the other three SDKs do.
#
# **Two separate things were wrong with `headers[[NAME]]`, and the first is
# fatal.** plumber hands a route `req$HEADERS`, a *named character vector* with
# lowercased names — and `[[` on a vector **throws** for a name that is not
# there, where `[[` on a list returns NULL. So every request carrying no act-for
# header — every anonymous visit, every API-key call, which is most of them —
# raised `subscript out of bounds` instead of answering "nobody", and an
# uncaught R condition inside plumber is a 500. The message names nothing about
# headers or identity, so there is nothing for an author to search for. The
# second: a request that *did* carry the header still missed, because the SDK
# compared `X-Toft-Act-For` against plumber's `x-toft-act-for`.
#
# Matching by lowered name and indexing by *position* answers both: position
# works identically for a vector and a list, so a Shiny app's list and a
# plumber route's vector reach the same code.
#
# **And a third shape, Shiny's, which is CGI's.** `session$request` is an
# httpuv environment whose header names are `HTTP_X_TOFT_ACT_FOR` — upper case,
# prefixed, underscores for hyphens — so a lowered `x-toft-act-for` never
# matched, and in a Shiny app `viewer()` and `caller_role()` answered NULL for
# every signed-in visitor, silently. `header_key` reduces all three spellings
# to one.
#
# "" for absent rather than NULL, so every caller tests the same way.
header_key <- function(x) {
  x <- tolower(x)
  x <- sub("^http_", "", x)
  gsub("_", "-", x, fixed = TRUE)
}

find_header <- function(headers, name) {
  # Shiny's request is an environment, and an environment cannot be indexed by
  # position; as a list it can.
  if (is.environment(headers)) {
    headers <- as.list(headers, all.names = TRUE)
  }
  if (is.null(headers) || length(headers) == 0) {
    return("")
  }
  nms <- names(headers)
  if (is.null(nms)) {
    return("")
  }
  i <- match(header_key(name), header_key(nms))
  if (is.na(i)) {
    return("")
  }
  value <- headers[[i]]
  if (is.null(value) || length(value) == 0 || is.na(value[[1]])) {
    return("")
  }
  as.character(value)[[1]]
}

#' Ask who the current viewer is
#'
#' Only works where an administrator switched viewer identity on for this app.
#' A request with nobody on it — no act-for header: an API key, a health check,
#' a visitor who is not signed in — returns NULL without asking, as the other
#' three SDKs do. Otherwise, where viewer identity is off, the platform refuses
#' with \code{viewer_identity_disabled} (a \code{toft_error}), so "nobody is
#' signed in" and "this app may not ask" stay distinguishable.
#'
#' @param request_headers The incoming request's headers, as a named list or a
#'   named character vector — a Shiny app reads these from
#'   \code{session$request}, a plumber route from \code{req$HEADERS}, and the
#'   two are different shapes with different capitalisation. Both work: the
#'   lookup is case-insensitive and indexes by position.
#' @return A list describing the viewer, or NULL.
#' @export
viewer <- function(request_headers = NULL) {
  # **Relayed as the header the platform actually reads.** This used to go out
  # as a query parameter, which nothing on the server looks at: every call
  # resolved to no viewer and answered 204, so this function returned NULL for
  # everybody and an app using it showed its signed-out state to signed-in
  # people. Silent, and indistinguishable from viewer identity being switched
  # off — which is the answer it is supposed to be able to tell apart.
  forwarded <- find_header(request_headers, ACT_FOR_HEADER)
  if (!nzchar(forwarded)) {
    # Nobody to name, and asking would only turn that into the setting's 403
    # on an app without viewer identity — a 500 for a signed-out page.
    return(NULL)
  }
  headers <- list()
  headers[[ACT_FOR_HEADER]] <- forwarded
  toft_request("GET", "/api/v1/app/viewer", extra_headers = headers)
}

# The role header, and the key one workload verifies it with.
#
# The value is a signed assertion rather than a word, for the reason the other
# three SDKs state: deployed apps bind 127.0.0.1 in shared mode and app-to-app
# isolation there is an accepted risk, so anything else on the box can open a
# socket to this app's port and send whatever headers it likes without the
# gateway ever seeing the request. Stripping an inbound header answers the
# visitor; it does not answer the neighbour on loopback.
ROLE_HEADER <- "X-Toft-Role"
ENV_ROLE_KEY <- "TOFT_ROLE_KEY"

ROLE_VERSION <- "v1"

# The six words the platform mints, and there is no seventh.
#
# `unknown` is not among them: it is this SDK's word for a failure — no header,
# no key, a bad MAC, an expired blob — and never a value the platform asserts.
# `admin` and `service` are not among them either, and never were: they were in
# this file's vocabulary while nothing here verified anything.
ROLE_UNKNOWN <- "unknown"
ROLE_ANONYMOUS <- "anonymous"
ROLE_VIEWER <- "viewer"
ROLE_EDITOR <- "editor"
ROLE_OWNER <- "owner"
ROLE_SYSTEM <- "system"
ROLE_APP <- "app"

MINTED_ROLES <- c(
  ROLE_ANONYMOUS, ROLE_VIEWER, ROLE_EDITOR, ROLE_OWNER, ROLE_SYSTEM, ROLE_APP
)

# anonymous < viewer < editor < owner. `system` and `app` are not on it, and
# neither is `unknown`: they are not more or less authorized than a person, they
# are a different kind of caller, and ranking Toft's scheduler above an
# owner would be a guess this SDK has no business making.
PERSON_RANK <- c(anonymous = 0L, viewer = 1L, editor = 2L, owner = 3L)

# base64url without padding, which is what the platform emits and what every
# other SDK compares against. R's base64 is standard and padded, so the two
# substitutions and the strip are the whole difference.
base64url <- function(raw) {
  gsub("=", "", chartr("+/", "-_", openssl::base64_encode(raw)), fixed = TRUE)
}

role_signature <- function(key, payload) {
  # Domain-separated exactly as the platform separates it: the key is already
  # derived and already per-app, so the prefix is defence in depth rather than
  # the only separation — but a derived key one HMAC away from the platform
  # secret is exactly the kind of thing that ends up reused.
  message <- c(charToRaw("toft-role"), as.raw(0L), charToRaw(payload))
  base64url(openssl::sha256(message, key = charToRaw(key)))
}

# Constant time, and not R's `==`: comparing two MACs with a short-circuiting
# string compare is a timing oracle, and the fix is cheap enough that there is
# no argument for skipping it.
constant_time_equal <- function(a, b) {
  ra <- charToRaw(a)
  rb <- charToRaw(b)
  if (length(ra) != length(rb)) {
    return(FALSE)
  }
  sum(as.integer(xor(ra, rb))) == 0L
}

#' Verify one signed role assertion
#'
#' The same computation the platform performs and the other three SDKs mirror:
#' \code{v1.<role>.<base64url app id>.<expiry>.<mac>}, where the MAC is
#' HMAC-SHA256 over \code{"toft-role\\0"} plus the payload, keyed with
#' \code{TOFT_ROLE_KEY}, base64url without padding.
#'
#' The app id is not checked here and does not need to be: the key is derived
#' per app, so a blob minted for another app fails the MAC. Checking it as well
#' would need this process to know its own id — a second thing to get wrong for
#' no property gained.
#'
#' Exported for the test that compares this MAC against the platform's over the
#' same input. App code calls \code{caller_role}.
#'
#' @param key The per-app, per-start key from \code{TOFT_ROLE_KEY}.
#' @param blob The header value.
#' @param now Unix seconds to judge the expiry against.
#' @return One of the six words, or "unknown".
#' @export
verify_role <- function(key, blob, now = as.numeric(Sys.time())) {
  if (!nzchar(key) || !nzchar(blob)) {
    return(ROLE_UNKNOWN)
  }
  idx <- max(gregexpr(".", blob, fixed = TRUE)[[1]])
  if (idx < 1) {
    return(ROLE_UNKNOWN)
  }
  payload <- substr(blob, 1, idx - 1)
  sig <- substr(blob, idx + 1, nchar(blob))

  # Before anything is parsed: structure decoded ahead of authentication is how
  # a verifier grows a second entry point.
  if (!constant_time_equal(sig, role_signature(key, payload))) {
    return(ROLE_UNKNOWN)
  }

  fields <- strsplit(payload, ".", fixed = TRUE)[[1]]
  if (length(fields) != 4L || fields[1] != ROLE_VERSION) {
    return(ROLE_UNKNOWN)
  }
  expiry <- suppressWarnings(as.numeric(fields[4]))
  if (is.na(expiry) || expiry <= now) {
    return(ROLE_UNKNOWN)
  }

  # A word this SDK does not know is unknown rather than passed through. The app
  # holds this key and could sign anything with it; app code branching on six
  # values must not be handed a seventh, whoever minted it.
  role <- fields[2]
  if (!role %in% MINTED_ROLES) {
    return(ROLE_UNKNOWN)
  }
  role
}

#' The caller's role on this app
#'
#' Every forwarded request tells the app what the caller may do, and the app
#' enforces nothing on the platform's behalf — this is what the platform already
#' decided, relayed so the app can shape its own UI.
#'
#' The value is signed, per app and per start. Absent, unverifiable and expired
#' are one word, and that word refuses: this returns "unknown", which is on no
#' ladder, so a missing header can never read as a privileged caller.
#'
#' @param request_headers The incoming request's headers, as a named list or a
#'   named character vector. plumber's \code{req$HEADERS} is the second, with
#'   lowercased names, and works.
#' @return One of the six role words, or "unknown".
#' @export
caller_role <- function(request_headers) {
  if (is.null(request_headers)) {
    return(ROLE_UNKNOWN)
  }
  raw <- find_header(request_headers, ROLE_HEADER)
  if (!nzchar(trimws(raw))) {
    return(ROLE_UNKNOWN)
  }
  verify_role(trimws(Sys.getenv(ENV_ROLE_KEY, "")), trimws(raw))
}

#' Whether the caller holds at least the given role
#'
#' A convenience over \code{caller_role}, and deliberately not a permission
#' system: a role is not an identity, and this answers the same question the
#' platform already answered.
#'
#' It answers only about the ladder. \code{system} and \code{app} are not on it
#' and are FALSE for every rung, and \code{unknown} is FALSE for everything,
#' \code{anonymous} included — an unverifiable caller is not "at least a
#' stranger", it is nobody this app can say anything about.
#'
#' A rung does not mean a human. An app key may be minted carrying
#' \code{"viewer"} or \code{"editor"}, by an owner or editor of this app who
#' chose to describe their machine client that way, and such a key satisfies
#' \code{caller_can} exactly as a person with the same word does. That is the
#' point of the word — the caller may do what it says — and it is why nothing
#' here answers "is a human in front of me".
#'
#' @param request_headers The incoming request's headers, as a named list or a
#'   named character vector; see \code{caller_role}.
#' @param at_least One of "owner", "editor", "viewer".
#' @return TRUE or FALSE.
#' @export
caller_can <- function(request_headers, at_least = ROLE_VIEWER) {
  # Single brackets: `[[` on a named vector stops with "subscript out of bounds"
  # for a name it lacks, which turned an unknown role into a crash rather than
  # the FALSE every other SDK answers.
  mine <- unname(PERSON_RANK[as.character(caller_role(request_headers))])
  want <- unname(PERSON_RANK[as.character(at_least)])
  if (length(mine) != 1L || length(want) != 1L || is.na(mine) || is.na(want)) {
    return(FALSE)
  }
  mine >= want
}
