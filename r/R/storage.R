# Durable storage, as a path and nothing else.
#
# There is no SDK call to the platform here and no credential: an administrator
# maps a resource to an app, the platform mounts it and describes it in
# TOFT_STORAGE, and this reads that description. The same three functions
# exist in Python, Node and Go, and answer the same way.

#' Every durable resource mapped to this app
#'
#' In the order the platform listed them, which is its `ORDER BY m.path`. That
#' is worth knowing only because two mounts can share a name: the order is
#' stable, and it is not a ranking. Ask for the one you mean with `folder`.
#'
#' Empty off-platform and empty when an administrator has mapped nothing —
#' deliberately not an error, because "this app has no durable storage" is a
#' normal state app code can branch on.
#'
#' @return A list of mounts, each a list of `name`, `kind`, `path`, `access`
#'   and `folder`. `folder` is `""` for the whole resource.
#' @export
mounts <- function() {
  raw <- trimws(Sys.getenv(ENV_STORAGE, ""))
  if (!nzchar(raw)) {
    return(list())
  }
  parsed <- tryCatch(
    jsonlite::fromJSON(raw, simplifyVector = FALSE),
    error = function(e) {
      stop(sprintf("%s is not valid JSON: %s", ENV_STORAGE, conditionMessage(e)), call. = FALSE)
    }
  )
  lapply(parsed, function(entry) {
    list(
      name = or_empty(entry[["name"]]),
      kind = or_empty(entry[["kind"]]),
      path = or_empty(entry[["path"]]),
      access = or_empty(entry[["access"]]),
      # Which directory of the resource this mount is rooted at, and "" is the
      # whole resource. It is here because **two mounts of one resource share a
      # name**, and the name is how app code locates a resource — so this is
      # what tells them apart.
      folder = or_empty(entry[["folder"]])
    )
  })
}

#' Resolve one resource by name, and — where it is mapped twice — by folder
#'
#' **A name is not always one mount.** A mapping's identity is
#' `(resource, app, folder)`, so an administrator can map one resource twice:
#' the root read-only and a folder at `write`, at two paths in one workload.
#' Both arrive under the same name, and the folder is what tells them apart.
#'
#' So a name matching two mounts, with no folder named, **stops rather than
#' picking one** — resolving it would mean choosing by the order of paths an
#' administrator happened to pick, which is a silent answer to a question the
#' caller did not know they were asking.
#'
#' @param name The name an administrator gave the resource.
#' @param folder Which folder of it. `NULL` means do not care, which is only
#'   ambiguous when the resource really is mapped more than once; `""` names the
#'   root explicitly.
#' @return One mount, as `mounts()` describes it.
#' @export
storage <- function(name, folder = NULL) {
  if (missing(name) || !nzchar(name)) {
    stop("storage() needs the name an administrator gave the resource", call. = FALSE)
  }
  available <- mounts()
  named <- Filter(function(m) identical(m$name, name), available)

  if (!is.null(folder)) {
    exact <- Filter(function(m) identical(m$folder, folder), named)
    if (length(exact) > 0) {
      return(exact[[1]])
    }
    if (length(named) > 0) {
      stop(sprintf(
        "'%s' is mounted, but not at folder '%s'. Mounted here: %s",
        name, folder, describe_folders(named)
      ), call. = FALSE)
    }
  } else if (length(named) == 1) {
    return(named[[1]])
  } else if (length(named) > 1) {
    stop(sprintf(
      "%d mounts are named '%s' — %s. Name one: storage('%s', folder = '%s')",
      length(named), name, describe_folders(named),
      name, named[[1]]$folder
    ), call. = FALSE)
  }

  if (length(available) == 0) {
    stop(sprintf(
      paste0("no storage resource named '%s', and this app has none mapped at all — ",
             "an administrator maps storage on the app's Storage tab"), name), call. = FALSE)
  }
  stop(sprintf(
    "no storage resource named '%s'. Mapped to this app: %s",
    name, paste(sort(vapply(available, function(m) m$name, "")), collapse = ", ")
  ), call. = FALSE)
}

#' Resolve one store by name
#'
#' A kind-checked alias of [storage()]. Uploads are a store feature end to end —
#' the CLI command is `toft store`, the grant table refuses a volume — so a reader
#' never has to remember which kinds are in scope, because the name is the kind.
#'
#' Write to it with [store_put()] rather than `file.copy()`. A store is not an
#' ordinary directory, and this documentation used to say it was: see
#' [store_put()].
#'
#' @param name The name an administrator gave the store.
#' @param folder Which folder of it, as in [storage()].
#' @return One mount.
#' @export
store <- function(name, folder = NULL) {
  mount <- storage(name, folder)
  if (nzchar(mount$kind) && !identical(mount$kind, "store")) {
    stop(sprintf(
      paste0("'%s' is a %s, not a store. A volume is a real filesystem — write to ",
             "storage('%s')$path directly, with the ordinary file functions."),
      name, mount$kind, name
    ), call. = FALSE)
  }
  mount
}

#' Copy a local file into a store
#'
#' The same route the other three SDKs give an app. It is a function rather than
#' a method only because R has no object to hang one on.
#'
#' **`file.copy()` is the obvious call and it does not work.** On a Mountpoint
#' store it creates the object and leaves it **empty**, returning `FALSE` rather
#' than raising — so the failure is visible only to code that checks a return
#' value most R ignores. An app archiving a report every night fills a bucket
#' with correctly named, zero-byte files, and the first person to notice is
#' whoever opens one. What defeats it is that a store is not a filesystem: there
#' is no rename and no random write, and base R's file helpers were written
#' against one that has both.
#'
#' So this streams the bytes sequentially, which is the shape a store supports,
#' in chunks so that a file larger than memory is not a separate problem.
#'
#' Like the other three, it is sugar over the mounted path and **not** a call to
#' the platform: an SDK that could presign its way into a store would let a
#' shared-mode app write to durable storage, which the platform refuses at
#' deploy time on purpose.
#'
#' The access level is checked here only for a better message. **The mount is
#' the enforcement** — Mountpoint refuses an operation the grant does not carry.
#'
#' @param mount A store, as [store()] returns it.
#' @param source Path to the local file to copy.
#' @param name What to call it in the store, relative to the mount. Defaults to
#'   the source file's own name; a leading slash, a backslash or a `..` segment
#'   is refused, because the name is the caller's and the location is not.
#' @return The path it was written to, invisibly.
#' @export
store_put <- function(mount, source, name = NULL) {
  if (is.null(mount) || is.null(mount$path) || !nzchar(mount$path)) {
    stop("store_put() needs a mount, as store() returns it", call. = FALSE)
  }
  if (missing(source) || !nzchar(source)) {
    stop("store_put() needs the path of a local file to copy", call. = FALSE)
  }
  if (!file.exists(source)) {
    stop(sprintf("no such file: %s", source), call. = FALSE)
  }
  target <- if (is.null(name)) basename(source) else name
  check_relative_name(target)

  if (identical(mount$access, "read")) {
    stop(sprintf(
      paste0("'%s' is mounted read-only, so this app cannot write to it. An administrator ",
             "sets the access level where the resource is mapped."), mount$name), call. = FALSE)
  }

  destination <- file.path(mount$path, target)
  dir.create(dirname(destination), recursive = TRUE, showWarnings = FALSE)

  input <- file(source, "rb")
  on.exit(close(input), add = TRUE)
  output <- file(destination, "wb")
  on.exit(close(output), add = TRUE)
  repeat {
    chunk <- readBin(input, "raw", n = 1048576L)
    if (length(chunk) == 0) {
      break
    }
    writeBin(chunk, output)
  }
  invisible(destination)
}

# A name that would land outside the mount is refused, on the discipline the
# platform applies to an uploaded object's name and for the same reason: the
# name is the caller's and the location is not, so a traversal segment would
# write into the app's own filesystem — over its code, in the worst case —
# rather than into the storage it was given.
check_relative_name <- function(name) {
  if (!nzchar(name)) {
    stop("a name is required", call. = FALSE)
  }
  if (startsWith(name, "/") || endsWith(name, "/")) {
    stop(sprintf("a name is relative to the mount, got '%s'", name), call. = FALSE)
  }
  if (grepl("\\", name, fixed = TRUE)) {
    stop(sprintf("a name must use forward slashes, got '%s'", name), call. = FALSE)
  }
  for (segment in strsplit(name, "/", fixed = TRUE)[[1]]) {
    if (!nzchar(segment) || identical(segment, ".") || identical(segment, "..")) {
      stop(sprintf("a name must not contain '%s' segments, got '%s'", segment, name), call. = FALSE)
    }
  }
  invisible(TRUE)
}

# A missing key is "", not NULL: an install older than a field says nothing, and
# every caller here wants a string. `%||%` is base R only from 4.4 and this
# package runs on 4.3.
or_empty <- function(value) {
  if (is.null(value)) "" else as.character(value)
}

# The folders one name is mounted at, with the level of each.
#
# The level is in the message because it is usually the reason there are two: a
# read corpus and a write drop are the shape this feature exists for, and a
# caller who picked the wrong one wants to see that immediately.
describe_folders <- function(named) {
  paste(vapply(named, function(m) {
    sprintf("folder '%s' at %s", m$folder, if (nzchar(m$access)) m$access else "read")
  }, ""), collapse = ", ")
}
