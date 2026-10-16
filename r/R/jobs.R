# Starting a different entrypoint of *this* app, never another one.
#
# Both functions here used to call routes the platform does not have.
# `job_start` posted to `/api/v1/app/runs`, which is registered for GET only and
# is the *task* surface besides; `job_status` read `/api/v1/app/runs/{id}`,
# which is registered at all only with `/logs` on the end. The job routes are
# `/api/v1/app/jobs/{name}/runs` and `/api/v1/app/job-runs/{id}`, which is what
# the Go, Node and Python SDKs have always called. Nothing caught it because
# nothing tests this package.

#' Start one of this app's jobs
#'
#' An app starts a different entrypoint of itself. It cannot start another app's
#' job, and the platform enforces that rather than trusting this function.
#'
#' @param name The job's name, as declared in toft.toml or created on the
#'   Automation tab. It travels in the path, which is where the route takes it.
#' @param params Optional named list of parameters. Never interpolated into a
#'   command — the platform passes them as environment.
#' @param act_for Optional. Who the run is on behalf of. An app never asserts
#'   who a viewer is: this relays a platform-signed value obtained from
#'   \code{viewer()}, and nobody-in-particular is assertable. It is sent as the
#'   \code{X-Toft-Act-For} header, which is the only channel the platform
#'   reads it from — in the body it was ignored, and the run was recorded
#'   against nobody.
#' @return The run id, invisibly.
#' @export
job_start <- function(name, params = NULL, act_for = NULL) {
  # No params means no body at all, not an empty one: jsonlite renders an empty
  # R list as `[]`, and the route decodes its body into an object, so sending
  # one would turn "this job takes no parameters" into a 400. The route accepts
  # an absent body and this is how it is sent.
  body <- NULL
  if (!is.null(params)) {
    body <- list(params = params)
  }
  headers <- NULL
  if (!is.null(act_for)) {
    headers <- list()
    headers[[ACT_FOR_HEADER]] <- act_for
  }
  resp <- toft_request(
    "POST",
    paste0("/api/v1/app/jobs/", utils::URLencode(name, reserved = TRUE), "/runs"),
    body = body,
    extra_headers = headers
  )
  invisible(resp$id)
}

#' Read the status of a run this app started
#'
#' An app can only resolve the runs it triggered; a run id belonging to another
#' app is a 404 rather than a refusal, which is the platform's rule everywhere
#' an id comes from the caller.
#'
#' @param run_id The id returned by \code{job_start}.
#' @return A list describing the run.
#' @export
job_status <- function(run_id) {
  toft_request("GET", paste0("/api/v1/app/job-runs/", utils::URLencode(run_id, reserved = TRUE)))
}
