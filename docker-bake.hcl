# `docker buildx bake` builds the image.

variable "TAG" {
  default = "local"
}

variable "REPO" {
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
  default = ["linux/amd64", "linux/arm64"]
}

group "default" {
  targets = ["app"]
}

target "app" {
  context    = "."
  dockerfile = "Dockerfile"
  target     = "app"
  platforms  = PLATFORMS
  tags       = ["${REPO}:${TAG}"]
  args = {
    APP_VERSION = APP_VERSION
  }
  labels = {
    "org.opencontainers.image.source"   = "https://github.com/lesomnus/cr"
    "org.opencontainers.image.revision" = BUILD_HASH
    "org.opencontainers.image.version"  = APP_VERSION
    "org.opencontainers.image.created"  = BUILD_TIMESTAMP
  }
}
