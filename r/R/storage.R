# Durable storage: a path where the resource is mounted, and signed URLs for a
# store in either mode.
#
# An administrator maps a resource to an app, the platform mounts it where it
# can and describes it in TOFT_STORAGE, and mounts() reads that description.
# A store is also reached through the platform's signed URLs — store_list(),
# store_upload(), store_download(), store_url() and store_remove() — in both
# execution modes, so an app written against them moves between modes
# unchanged. On a shared host a store is never mounted: its `path` is "", and
# those five are the whole of how the app reaches it. The same functions exist
# in Python, Node and Go, and answer the same way.

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
#' Like the other three, it is sugar over the mounted path and calls nothing.
#' Where the store is not mounted — on an install with no cluster — it refuses,
#' and [store_upload()] reaches the store through the platform instead.
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
  if (is.null(mount) || is.null(mount$name)) {
    stop("store_put() needs a mount, as store() returns it", call. = FALSE)
  }
  # "" is the working directory, and copying there writes into the app's own
  # tree: a store that is not mounted is reached through the platform instead.
  if (is.null(mount$path) || !nzchar(mount$path)) {
    stop(sprintf(paste0("'%s' is not mounted here, so store_put() has nowhere to copy to — ",
                        "use store_upload(), which reaches it through the platform"),
                 mount$name), call. = FALSE)
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

#' List one page of a store, over the platform's signed URLs
#'
#' @param mount A store, as [store()] returns it.
#' @param path A folder under the mount, `""` for its top.
#' @param cursor `next_cursor` from the previous page, `""` for the first.
#' @return A list of `objects`, `prefixes`, `next_cursor` and `access`. Names
#'   are relative to `path`.
#' @export
store_list <- function(mount, path = "", cursor = "") {
  if (nzchar(path)) check_relative_name(sub("/$", "", path))
  page <- toft_request("GET", store_route(mount, "/api/v1/app/store/{slug}/objects"),
                       query = store_query(mount, list(path = path, cursor = cursor)))
  list(
    objects = if (is.null(page$objects)) list() else page$objects,
    prefixes = if (is.null(page$prefixes)) list() else page$prefixes,
    next_cursor = or_empty(page$next_cursor),
    access = or_empty(page$access)
  )
}

#' Upload a local file to a store, over the platform's signed URLs
#'
#' At `write` this creates new objects only, and an existing name is a refusal
#' that says so; `overwrite` and `full` replace. The bytes go straight to the
#' object store — the platform signs, and never sees them.
#'
#' @param mount A store, as [store()] returns it.
#' @param source Path to the local file.
#' @param name What to call it, relative to the mount. Defaults to its own name.
#' @return The name it landed under, invisibly.
#' @export
store_upload <- function(mount, source, name = NULL) {
  if (missing(source) || !file.exists(source)) {
    stop(sprintf("no such file: %s", if (missing(source)) "" else source), call. = FALSE)
  }
  target <- if (is.null(name) || !nzchar(name)) basename(source) else name
  check_relative_name(target)
  data <- readBin(source, "raw", n = file.info(source)$size)
  signed <- toft_request("POST", store_route(mount, "/api/v1/app/store/{slug}/objects"),
                         body = list(name = target, size = length(data),
                                     # paste0: a plain string, not the "hash"
                                     # class jsonlite has no method for.
                                     sha256 = paste0(as.character(openssl::sha256(data)), collapse = ""),
                                     md5 = paste0(as.character(openssl::md5(data)), collapse = "")),
                         query = store_query(mount))
  status <- send_signed(signed, data)
  if (status == 412) {
    stop(sprintf("'%s' already exists in '%s', and this app's access (%s) creates new objects only",
                 target, mount$name, mount$access), call. = FALSE)
  }
  check_signed_status(status)
  invisible(target)
}

#' Download one object from a store
#'
#' @param mount A store, as [store()] returns it.
#' @param name The object, relative to the mount.
#' @param dest Where to write it. `NULL` returns the bytes as a raw vector.
#' @return The raw bytes, or `dest` invisibly.
#' @export
store_download <- function(mount, name, dest = NULL) {
  signed <- sign_get(mount, name, FALSE)
  got <- send_signed(signed, NULL, keep = TRUE)
  check_signed_status(got$status)
  if (is.null(dest)) {
    return(got$content)
  }
  dir.create(dirname(dest), recursive = TRUE, showWarnings = FALSE)
  writeBin(got$content, dest)
  invisible(dest)
}

#' A signed link to one object, to hand to a browser
#'
#' It lives fifteen minutes, and anyone holding it reads that object until then
#' — the point for a download link, and the reason not to log one.
#'
#' @param mount A store, as [store()] returns it.
#' @param name The object, relative to the mount.
#' @param attachment `TRUE` makes a browser save it under the object's own name.
#' @return The URL.
#' @export
store_url <- function(mount, name, attachment = FALSE) {
  sign_get(mount, name, attachment)$url
}

#' Delete one object from a store. Needs `full`.
#'
#' @param mount A store, as [store()] returns it.
#' @param name The object, relative to the mount.
#' @export
store_remove <- function(mount, name) {
  check_relative_name(name)
  signed <- toft_request("DELETE", store_route(mount, "/api/v1/app/store/{slug}/objects/{name}", name),
                         query = store_query(mount))
  check_signed_status(send_signed(signed, NULL))
  invisible(NULL)
}

sign_get <- function(mount, name, attachment) {
  check_relative_name(name)
  extra <- if (isTRUE(attachment)) list(disposition = "attachment") else list()
  toft_request("GET", store_route(mount, "/api/v1/app/store/{slug}/objects/{name}", name),
               query = store_query(mount, extra))
}

# `route` is the platform's route, spelled whole so a reader — and the SDK
# contract test — sees which one each call reaches.
store_route <- function(mount, route, name = "") {
  if (is.null(mount) || is.null(mount$name)) {
    stop("this needs a store, as store() returns it", call. = FALSE)
  }
  if (nzchar(or_empty(mount$kind)) && !identical(mount$kind, "store")) {
    stop(sprintf(paste0("'%s' is a %s, which is reached at its path — only an object store ",
                        "is reached over signed URLs"), mount$name, mount$kind), call. = FALSE)
  }
  route <- sub("{slug}", utils::URLencode(mount$name, reserved = TRUE), route, fixed = TRUE)
  sub("{name}", encode_name(name), route, fixed = TRUE)
}

# The folder always travels, "" included: it names *this* mapping, so a
# resource mapped twice is never a question the platform has to ask back.
store_query <- function(mount, extra = list()) {
  query <- list(folder = or_empty(mount$folder))
  for (k in names(extra)) {
    if (nzchar(extra[[k]])) query[[k]] <- extra[[k]]
  }
  query
}

encode_name <- function(name) {
  parts <- strsplit(name, "/", fixed = TRUE)[[1]]
  paste(vapply(parts, utils::URLencode, "", reserved = TRUE), collapse = "/")
}

# Carry out a signed request against the object store: the signed headers
# verbatim, and no Toft header at all — the URL is the whole authorization.
send_signed <- function(signed, data, keep = FALSE) {
  if (is.null(signed) || is.null(signed$url) || !nzchar(signed$url)) {
    stop("the platform returned no signed URL", call. = FALSE)
  }
  handle <- curl::new_handle()
  method <- if (is.null(signed$method) || !nzchar(signed$method)) "GET" else signed$method
  curl::handle_setopt(handle, customrequest = method)
  if (!is.null(data)) {
    curl::handle_setopt(handle, post = TRUE, postfieldsize = length(data), postfields = data)
  }
  headers <- signed$headers
  if (length(headers)) {
    curl::handle_setheaders(handle, .list = lapply(headers, as.character))
  }
  resp <- curl::curl_fetch_memory(signed$url, handle = handle)
  if (keep) list(status = resp$status_code, content = resp$content) else resp$status_code
}

check_signed_status <- function(status) {
  if (status >= 400) {
    stop(sprintf("the object store refused the request (%d)", status), call. = FALSE)
  }
  invisible(TRUE)
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
