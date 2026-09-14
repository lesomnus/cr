# One image, because cr is one binary: `cr serve` answers the registry and the
# management API, and every other command runs from the same image with other
# arguments.
#
# `docker buildx bake` builds it. `test` is a target and not part of the
# default group: the gate is CI's `go` job, and this is its vet and tests for a
# machine that has no toolchain -- see `Dockerfile`.

variable "TAG" {
  default = "local"
}

variable "REPO" {
  # A user or organisation has to be in here. GHCR answers a path without one
  # with `400 Bad Request` on a blob HEAD, which says nothing about the missing
  # segment that caused it.
  default = "ghcr.io/lesomnus/cr"
}

variable "BUILD_HASH" {
  default = "0000000000000000000000000000000000000000"
}

variable "BUILD_TIMESTAMP" {
  default = "${timestamp()}"
}

variable "BUILD_DATE" {
  default = "${formatdate("YYMMDD", BUILD_TIMESTAMP)}"
}

variable "BUILD_ID" {
  default = "r0"
}

variable "APP_VERSION" {
  default = "${BUILD_DATE}-${BUILD_ID}"
}

variable "PLATFORMS" {
  # The stage that builds runs on the builder's architecture and
  # cross-compiles, so the second platform is a second link rather than a
  # second run under emulation. See `Dockerfile`.
  default = ["linux/amd64", "linux/arm64"]
}

# Four tags for one build, which is three more than a `docker push` gives you
# and each answers a different question.
#
#   :edge            what CI last pushed from main -- moves, and is the one a
#                    development deployment follows
#   :r<run>          which build this was, and nothing else has that number
#   :YYMMDD          the last build of that day, for saying "the one from
#                    Tuesday" without looking a run id up
#   :YYMMDD-r<run>   both, and the only one of the four that never moves --
#                    which is what a deployment that is meant to stay put pins
function "tags" {
  params = [name]
  result = [
    "${name}:${TAG}",
    "${name}:${BUILD_ID}",
    "${name}:${BUILD_DATE}",
    "${name}:${BUILD_DATE}-${BUILD_ID}",
  ]
}

target "app" {
  target     = "app"
  dockerfile = "Dockerfile"
  platforms  = PLATFORMS
  tags       = tags(REPO)

  # `APP_VERSION` rather than the revision, because it is what the binary can
  # be told: `cr version` reads a variable payday exports for exactly this, and
  # the toolchain cannot stamp `vcs.revision` from a context with no `.git/`.
  # The revision is a label instead, which is where a registry client looks for
  # it.
  args = {
    APP_VERSION = APP_VERSION
  }

  labels = {
    "org.opencontainers.image.title"       = "cr"
    "org.opencontainers.image.description" = "An OCI container registry with per-repository permissions, tag rules, pull-through caches, and garbage collection that never stops it"

    # Not decoration: GHCR reads `source` to attach the package to this
    # repository, and without it the package is an orphan with no README and no
    # link back.
    "org.opencontainers.image.source"   = "https://github.com/lesomnus/cr"
    "org.opencontainers.image.url"      = "https://github.com/lesomnus/cr"
    "org.opencontainers.image.licenses" = "Apache-2.0"
    "org.opencontainers.image.revision" = "${BUILD_HASH}"
    "org.opencontainers.image.version"  = "${APP_VERSION}"
    "org.opencontainers.image.created"  = "${BUILD_TIMESTAMP}"
  }
}

target "test" {
  target     = "test"
  dockerfile = "Dockerfile"

  # One architecture and no image. The tests are not architecture-dependent, so
  # running them twice would be running them twice, and there is nothing to load
  # or push at the end of them.
  output = [{ type = "cacheonly" }]
}

group "default" {
  targets = ["app"]
}
