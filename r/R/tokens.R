# A file connection's own access token, for the provider's own API.
#
# Where an administrator has switched on "Let apps get access tokens" on the
# connection attached to this app, the app calls Graph or Drive directly and
# holds no secret. The folder in the answer is a hint, not a fence: the token
# reaches everything the connection's grant reaches.

# How long before expiry, in seconds, a cached token is replaced.
TOKEN_REFRESH_MARGIN <- 5 * 60

.token_cache <- new.env(parent = emptyenv())

#' Get the attached file connection's access token
#'
#' For `"sharepoint"`, `"onedrive"` or `"gdrive"`, while the connection lets
#' apps have one. Cached per kind and fetched again five minutes before it
#' expires, so calling it before every request costs one request to Toft an
#' hour. With httr2:
#'
#' ```r
#' tok <- access_token("sharepoint")
#' httr2::request("https://graph.microsoft.com/v1.0") |>
#'   httr2::req_url_path_append("drives", tok$folder$drive_id,
#'                              "items", tok$folder$item_id, "children") |>
#'   httr2::req_auth_bearer_token(tok$value) |>
#'   httr2::req_perform()
#' ```
#'
#' @param kind The integration: `"sharepoint"`, `"onedrive"` or `"gdrive"`.
#' @return A list: `value` (the bearer token — never print it), `expires_at`
#'   (POSIXct, UTC), `resource` (the API it is for) and `folder` (`drive_id`,
#'   `item_id`, `label` — where this app was attached).
#' @export
access_token <- function(kind) {
  cached <- .token_cache[[kind]]
  if (!is.null(cached) &&
      as.numeric(cached$expires_at) - as.numeric(Sys.time()) > TOKEN_REFRESH_MARGIN) {
    return(cached)
  }
  resp <- toft_request("POST", paste0("/api/v1/app/tokens/", utils::URLencode(kind, reserved = TRUE)),
                       body = stats::setNames(list(), character(0)))
  folder <- if (is.null(resp$folder)) list() else resp$folder
  token <- structure(list(
    value = resp$access_token,
    expires_at = as.POSIXct(sub("Z$", "", resp$expires_at), format = "%Y-%m-%dT%H:%M:%OS", tz = "UTC"),
    resource = if (is.null(resp$resource)) "" else resp$resource,
    folder = list(
      drive_id = if (is.null(folder$drive_id)) "" else folder$drive_id,
      item_id = if (is.null(folder$item_id)) "" else folder$item_id,
      label = if (is.null(folder$label)) "" else folder$label
    )
  ), class = "toft_token")
  assign(kind, token, envir = .token_cache)
  token
}

#' @export
print.toft_token <- function(x, ...) {
  # Never the value: a token in a console is held by whoever reads the log.
  cat("<toft token for ", x$resource, ", expires ", format(x$expires_at, tz = "UTC"),
      " UTC>\n", sep = "")
  invisible(x)
}
