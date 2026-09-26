#!/usr/bin/env bash
# What a pull-through cache costs while several cold pulls are in flight at
# once (#35): the memory the process and its container reach -- page cache
# and socket buffers included, which is what a container limit counts --
# what the kernel had to do about it, and how long the clients waited.
#
#     ./scripts/coldpull.sh                             # cr, distribution and zot, no limit
#     MEMORY=128m ./scripts/coldpull.sh                 # each registry under a 128 MiB limit
#     REGISTRIES=cr CLIENTS="4 8" ./scripts/coldpull.sh
#     UPSTREAM=https://registry-1.docker.io IMAGES=library/node:22,library/python:3.13 \
#       ./scripts/coldpull.sh                           # real images from a real upstream
#
# Everything runs in containers from one image this builds, on a network of
# its own, so it needs docker and nothing mounted from here: the registry
# under test, wrapped in `coldpull run` to sample it; a synthetic upstream,
# unless UPSTREAM is given; and the clients. Every run starts a registry with
# an empty cache.
#
#   REGISTRIES   which to measure: cr, distribution, zot (default all three)
#   SCENARIOS    `same`: every client pulls one image, the herd a CI fleet
#                makes on a base image; `distinct`: each pulls its own
#                (default both)
#   CLIENTS      how many pull at once, a run each (default "1 2 4 8")
#   PARALLEL     blobs one client reads at once (default 3, docker's)
#   MEMORY       a container limit for the registry, as docker takes it;
#                empty is none, which still reports what it reached
#   LAYERS, LAYER_MIB, IMAGE_COUNT, RATE_MIB
#                the synthetic upstream: images of LAYERS layers of LAYER_MIB
#                each, served at RATE_MIB a response so that fills overlap
#                the way they do from a real upstream (default 4, 64, 8, 25)
#   UPSTREAM, IMAGES
#                a real upstream and comma-separated images on it instead
#
# Results go to RESULTS_DIR as coldpull.jsonl, a line a run, and coldpull.md.
# Not in CI: it moves gigabytes, and the numbers are the hardware's as much
# as the registry's. Linux on amd64, for the pinned binaries.
set -o errexit
set -o pipefail
set -o nounset

__root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${__root}"

# shellcheck source=lib/registries.sh
. "${__root}/scripts/lib/registries.sh"

REGISTRIES="${REGISTRIES:-cr distribution zot}"
SCENARIOS="${SCENARIOS:-same distinct}"
CLIENTS="${CLIENTS:-1 2 4 8}"
PARALLEL="${PARALLEL:-3}"
MEMORY="${MEMORY:-}"
LAYERS="${LAYERS:-4}"
LAYER_MIB="${LAYER_MIB:-64}"
IMAGE_COUNT="${IMAGE_COUNT:-8}"
RATE_MIB="${RATE_MIB:-25}"
UPSTREAM="${UPSTREAM:-}"
IMAGES="${IMAGES:-}"
RESULTS_DIR="${RESULTS_DIR:-${__root}/coldpull-results}"

if [ -n "${UPSTREAM}" ] && [ -z "${IMAGES}" ]; then
	echo "coldpull: UPSTREAM needs IMAGES, the images to pull from it" >&2
	exit 1
fi

id="coldpull-$$"
image="${id}:local"
net="${id}"
work="$(mktemp -d)"
cleanup() {
	docker rm -f "${id}-registry" "${id}-upstream" >/dev/null 2>&1 || true
	docker network rm "${net}" >/dev/null 2>&1 || true
	docker image rm "${image}" >/dev/null 2>&1 || true
	rm -rf "${work}"
}
trap cleanup EXIT

# The image: every binary, and a configuration per registry pointing at the
# upstream by its name on the network.
ctx="${work}/context"
mkdir -p "${ctx}/bin" "${ctx}/etc"
(CGO_ENABLED=0 go build -trimpath -o "${ctx}/bin/coldpull" ./scripts/coldpull)
build_cr "${__root}" "${ctx}/bin/cr"
cp "$(fetch_distribution "${DISTRIBUTION3_VERSION}" "${DISTRIBUTION3_SHA256}" "${work}/distribution")" "${ctx}/bin/distribution"
cp "$(fetch_zot)" "${ctx}/bin/zot"

upstream="${UPSTREAM:-http://${id}-upstream:5000}"
cat >"${ctx}/etc/cr.yaml" <<YAML
db:
  driver: sqlite3
  dsn: "file:/data/cr.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"
  migrate: true
server:
  addr: "127.0.0.1:0"
  http:
    addr: ":5000"
watch:
  broker: memory
registry:
  storage:
    driver: os
    os:
      root: /data/blobs
  proxies:
    - prefix: ""
      upstream: ${upstream}
YAML
cat >"${ctx}/etc/distribution.yml" <<YAML
version: 0.1
log:
  level: info
storage:
  filesystem:
    rootdirectory: /data
http:
  addr: :5000
proxy:
  remoteurl: ${upstream}
YAML
cat >"${ctx}/etc/zot.json" <<JSON
{
  "distSpecVersion": "1.1.1",
  "storage": {"rootDirectory": "/data"},
  "http": {"address": "0.0.0.0", "port": "5000"},
  "log": {"level": "info"},
  "extensions": {
    "sync": {
      "enable": true,
      "registries": [
        {"urls": ["${upstream}"], "onDemand": true, "tlsVerify": false, "content": [{"prefix": "**"}]}
      ]
    }
  }
}
JSON
cat >"${ctx}/Dockerfile" <<'DOCKERFILE'
FROM busybox:1.36
COPY bin/ /usr/local/bin/
COPY etc/ /etc/coldpull/
RUN mkdir /data
DOCKERFILE
docker build -q -t "${image}" "${ctx}" >/dev/null
docker network create "${net}" >/dev/null

pull_images=(-image-count "${IMAGE_COUNT}")
if [ -n "${UPSTREAM}" ]; then
	pull_images=(-images "${IMAGES}")
else
	docker run -d --name "${id}-upstream" --network "${net}" "${image}" \
		coldpull upstream -listen :5000 -images "${IMAGE_COUNT}" -layers "${LAYERS}" -layer-mib "${LAYER_MIB}" -rate-mib "${RATE_MIB}" >/dev/null
	# It hashes every layer before it listens, which takes a few seconds for
	# gigabytes; a registry asking before then would measure a 504.
	docker run --rm --network "${net}" "${image}" sh -c \
		"for _ in \$(seq 1 120); do wget -q -O /dev/null http://${id}-upstream:5000/v2/ && exit 0; sleep 0.5; done; exit 1" || {
		echo "coldpull: the upstream did not come up" >&2
		docker logs "${id}-upstream" >&2
		exit 1
	}
fi

limit=()
if [ -n "${MEMORY}" ]; then
	# No swap: a limit that can spill to disk is not the limit being asked
	# about.
	limit=(--memory "${MEMORY}" --memory-swap "${MEMORY}")
fi

command_of() {
	case "$1" in
	cr) echo "cr --config /etc/coldpull/cr.yaml serve" ;;
	distribution) echo "distribution serve /etc/coldpull/distribution.yml" ;;
	zot) echo "zot serve /etc/coldpull/zot.json" ;;
	*)
		echo "coldpull: unknown registry $1" >&2
		exit 1
		;;
	esac
}

mkdir -p "${RESULTS_DIR}"
out="${RESULTS_DIR}/coldpull.jsonl"
: >"${out}"
run=0
for reg in ${REGISTRIES}; do
	# shellcheck disable=SC2046
	cmd=$(command_of "${reg}")
	for scenario in ${SCENARIOS}; do
		for n in ${CLIENTS}; do
			run=$((run + 1))
			echo "coldpull: ${reg}, ${scenario}, ${n} at once${MEMORY:+, under ${MEMORY}}" >&2
			docker rm -f "${id}-registry" >/dev/null 2>&1 || true
			# shellcheck disable=SC2086
			docker run -d --name "${id}-registry" --network "${net}" "${limit[@]}" "${image}" \
				coldpull run -- ${cmd} >/dev/null
			docker run --rm --network "${net}" "${image}" \
				coldpull pull -registry "http://${id}-registry:5000" -stats "http://${id}-registry:9100" \
				-clients "${n}" -scenario "${scenario}" -parallel "${PARALLEL}" -label "${reg}" "${pull_images[@]}" \
				>>"${out}" || echo "coldpull: the pull itself failed" >&2
			# What the registry said, for a run that failed or was OOM-killed,
			# and what docker says became of its container.
			if tail -n 1 "${out}" | grep -q -e '"ok":false' -e '"oom_kill":[1-9]'; then
				{
					docker inspect -f 'container: {{.State.Status}}, OOMKilled {{.State.OOMKilled}}, exit code {{.State.ExitCode}}' "${id}-registry"
					docker logs "${id}-registry"
				} >"${RESULTS_DIR}/${run}-${reg}-${scenario}-${n}.log" 2>&1 || true
			fi
		done
	done
done
docker rm -f "${id}-registry" >/dev/null 2>&1 || true

docker run --rm -i "${image}" coldpull report <"${out}" >"${RESULTS_DIR}/coldpull.md"
{
	echo
	if [ -n "${UPSTREAM}" ]; then
		echo "Upstream ${UPSTREAM}: ${IMAGES}."
	else
		echo "Synthetic upstream: ${IMAGE_COUNT} images of ${LAYERS} × ${LAYER_MIB} MiB, ${RATE_MIB} MiB/s a response."
	fi
	echo "Clients read ${PARALLEL} blobs at once. Registry memory limit: ${MEMORY:-none}."
	echo "cgroup figures are the registry's container, including the sampler's few MiB; RssAnon is the registry process alone."
	echo "A cgroup peak above the limit is socket buffers: the kernel charges those past memory.max rather than drop traffic."
	echo "\"container ended\": the OOM killer took the registry and then, the limit still exceeded, the sampler; figures are then as last read during the pulls."
	echo "The log of a run that failed or was OOM-killed is beside this file, with what docker says became of the container."
} >>"${RESULTS_DIR}/coldpull.md"
cat "${RESULTS_DIR}/coldpull.md"
