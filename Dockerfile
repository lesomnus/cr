# cr, as an image: the binary and nothing else.
#
# `docker buildx bake` builds `app`; `docker buildx bake test` runs the Go tests
# on a machine with no toolchain. The stages that do work run on the builder's
# architecture and cross-compile, so a second platform is a second link rather
# than a second run under emulation. SQLite is the wazero engine, so nothing
# here needs cgo.

# syft, for the bill of materials below. On the builder's platform, like the
# stages that use it: it reads a binary, it does not run one.
FROM --platform=$BUILDPLATFORM anchore/syft:v1.54.1 AS syft

# What the image runs on, named once: `app` is built on it, and `notices` reads
# its package list.
FROM gcr.io/distroless/static-debian13:nonroot AS runtime

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

# What is in the binary, and what its licenses ask to be passed on with it.
#
# Here and not off the finished image, because the finished image has the
# binary and nothing else: a scan of it finds every module -- Go writes the list
# into the binary -- and not one license, since the license files are in the
# module cache, which only this side of the build has (`go mod download` in
# `base`). Scanning the BINARY rather than go.mod names what is linked, not
# what the module graph mentions: no test-only or tool dependencies.
#
#   sbom.cdx.json          CycloneDX, with each module's license as detected.
#                          The binary only: CI merges it with a scan of the
#                          pushed image, which sees the base's packages, and
#                          attaches that (lesomnus/sbom-attach, `ci.yml`).
#                          CycloneDX and not SPDX because of where syft puts a
#                          license it found: SPDX's `licenseConcluded`, which
#                          Trivy does not read -- it reads `licenseDeclared` --
#                          so the same scan read 102 of 106 licenses as
#                          CycloneDX and 1 as SPDX.
#   THIRD_PARTY_NOTICES    each module's license and notice files, verbatim --
#                          the text and copyright lines MIT, BSD and Apache-2.0
#                          ask to travel with the binary, which an SBOM does
#                          not carry. Written from the SBOM's own module list,
#                          so the two cannot disagree (scripts/notices).
#                          Strict: a module that ships no license file fails
#                          the build, rather than an image going out that
#                          cannot say under what terms it carries it.
#                          It ends with the base image's Debian packages -- some
#                          are GPL-2.0 -- each with where its copyright is in
#                          the image and where the source of that version is.
#   LICENSE                cr's own
FROM base AS notices
ARG APP_VERSION="0.0.0-dev"
COPY --from=syft /syft /usr/local/bin/syft
COPY --from=build /out/cr /scan/cr
# The base's package list, for the platform being built.
COPY --from=runtime /var/lib/dpkg/status.d /scan-dpkg
RUN mkdir -p /out \
	&& SYFT_GOLANG_SEARCH_LOCAL_MOD_CACHE_LICENSES=true \
	syft scan dir:/scan --quiet \
	--source-name cr --source-version "${APP_VERSION}" \
	-o cyclonedx-json=/out/sbom.cdx.json \
	&& go run ./scripts/notices -strict -dpkg /scan-dpkg /out/sbom.cdx.json >/out/THIRD_PARTY_NOTICES \
	&& cp LICENSE /out/LICENSE

# The three files alone, for CI to attach the SBOM to the pushed image
# (`docker buildx bake notices`).
FROM scratch AS notices-out
COPY --from=notices /out/ /

FROM runtime AS app

COPY --from=build /out/cr /usr/local/bin/cr

# Where a Debian system -- this base included -- keeps what it owes the
# software it ships: the base's own packages are in /usr/share/doc/<package>/
# already, and cr's sit beside them.
COPY --from=notices /out/ /usr/share/doc/cr/

USER nonroot:nonroot
WORKDIR /home/nonroot

# The registry's listener. Everything else about a deployment is its
# configuration: `--config`, or `CR_*` variables (`cr config env`).
EXPOSE 5000

# `docker stop` and Kubernetes send SIGTERM, which `cr serve` stops on the way
# `shutdown` in its configuration says.
ENTRYPOINT ["/usr/local/bin/cr"]
CMD ["serve"]
