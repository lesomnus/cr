# cr, as an image: the binary and nothing else.
#
# `docker buildx bake` builds `app`; `docker buildx bake test` runs the Go tests
# on a machine with no toolchain. The stages that do work run on the builder's
# architecture and cross-compile, so a second platform is a second link rather
# than a second run under emulation. SQLite is the wazero engine, so nothing
# here needs cgo.

FROM --platform=$BUILDPLATFORM golang:1.27 AS base

WORKDIR /src

# The module graph first, so that editing a `.go` file does not re-download it.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Not part of any image, and not in bake's default group: the gate is CI's `go`
# job, which also runs gofmt and `pd gen --check`. This is its vet and tests,
# for `docker buildx bake test`.
FROM base AS test
RUN --mount=type=cache,target=/root/.cache/go-build \
	go vet ./... && go test ./...

FROM base AS build

ARG TARGETOS
ARG TARGETARCH

# What `cr version` prints. `.git/` is not in the context, so the toolchain
# cannot stamp a revision; the version is handed in and the revision goes on
# the image as a label instead (`docker-bake.hcl`).
ARG APP_VERSION="0.0.0-dev"

RUN --mount=type=cache,target=/root/.cache/go-build \
	CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
	go build -trimpath \
	-ldflags="-s -w -X github.com/lesomnus/payday/version.version=${APP_VERSION}" \
	-o /out/cr ./cmd/cr

FROM gcr.io/distroless/static-debian12:nonroot AS app

COPY --from=build /out/cr /usr/local/bin/cr

USER nonroot:nonroot
WORKDIR /home/nonroot

# The registry's listener. Everything else about a deployment is its
# configuration: `--config`, or `CR_*` variables (`cr config env`).
EXPOSE 5000

# `docker stop` and Kubernetes send SIGTERM, which `cr serve` stops on the way
# `shutdown` in its configuration says.
ENTRYPOINT ["/usr/local/bin/cr"]
CMD ["serve"]
