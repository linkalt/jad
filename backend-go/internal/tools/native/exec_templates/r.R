# exec's fn for R, sourced at start-up through R_PROFILE_USER. fn$name(...) calls the saved helper
# functions_dir/r/<name>.R (fn$`my-help`(...) for a dash); fn$name$member is any other object it
# defines. A helper file is sourced into an environment of its own; its entry is the function named
# like the file (a dash written _ or .), else call, else its only function. The user's own profile
# (.Rprofile in the working directory, else in the home directory) is sourced first, as R would.
local({
  profile <- if (file.exists(".Rprofile")) ".Rprofile" else file.path(Sys.getenv("HOME"), ".Rprofile")
  if (nzchar(Sys.getenv("HOME")) && file.exists(profile)) sys.source(profile, envir = globalenv())
})
local({
  helpers <- new.env()
  load <- function(name) {
    if (!is.null(helpers[[name]])) return(helpers[[name]])
    if (!grepl("^[A-Za-z0-9][A-Za-z0-9_.-]*$", name)) {
      stop("fn$", name, " is not a helper name: use letters, digits, _ . and - and start with a letter or digit", call. = FALSE)
    }
    dir <- Sys.getenv("JAD_RUNNER_DIR")
    file <- file.path(dir, paste0(name, ".R"))
    if (!nzchar(dir) || !file.exists(file)) {
      stop("no helper fn$", name, " in ", if (nzchar(dir)) dir else "(the helper library does not exist)", call. = FALSE)
    }
    scope <- new.env(parent = globalenv())
    sys.source(file, envir = scope)
    helpers[[name]] <- scope
    scope
  }
  entry <- function(name, scope) {
    for (candidate in unique(c(gsub("-", "_", name), gsub("-", ".", name), "call"))) {
      if (exists(candidate, envir = scope, inherits = FALSE) && is.function(scope[[candidate]])) return(scope[[candidate]])
    }
    functions <- Filter(function(key) is.function(scope[[key]]), sort(ls(scope)))
    if (length(functions) == 1) return(scope[[functions]])
    stop("fn$", name, " has no single entry to call (",
      if (length(functions)) paste0("it defines ", paste(functions, collapse = ", "), "; call fn$", name, "$", functions[1], "(...)") else "it defines no function",
      "): define ", gsub("-", "_", name), call. = FALSE)
  }
  # S3 methods on the search path are not found since R 3.6; they are registered instead.
  registerS3method("$", "jad_fn", function(x, name) {
    scope <- load(name)
    structure(function(...) entry(name, scope)(...), class = "jad_helper", scope = scope)
  })
  registerS3method("$", "jad_helper", function(x, name) get(name, envir = attr(x, "scope"), inherits = FALSE))
  registerS3method("print", "jad_fn", function(x, ...) invisible(cat("<exec helpers: fn$name(...)>\n")))
  tools <- new.env()
  tools$fn <- structure(list(), class = "jad_fn")
  attach(tools, name = "jad:fn", warn.conflicts = FALSE)
})
