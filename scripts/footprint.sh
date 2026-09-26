#!/usr/bin/env bash
# What cr costs to start and to keep around -- startup time, memory, idle CPU
# -- beside other registries, or beside cr at another commit. The measuring
# is `scripts/footprint.py`; this builds and fetches what it measures.
#
#     ./scripts/footprint.sh                  # cr from this checkout, zot and distribution
#     BASE=origin/main ./scripts/footprint.sh # cr from this checkout against cr at BASE
#     RUNS=5 IDLE=10 ./scripts/footprint.sh   # quicker, noisier
#
# cr is built the way the image builds it. The others are release binaries,
# pinned and checked against the checksums their projects publish. Results go
# to RESULTS_DIR as footprint.json and footprint.md, and to the job summary
# on GitHub Actions. Linux on amd64 only: it reads /proc, and the pinned
# binaries are amd64 ones.
set -o errexit
set -o pipefail
set -o nounset

__root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${__root}"

ZOT_VERSION="v2.1.21"
ZOT_SHA256="8751cc0daf739634835a3bd8206e3094c84d552e2c462e4a4baf80f40dd92685"
ZOT_MINIMAL_SHA256="d2422616a28dbae10a92c1df9daa23980e2f7f3deb0079968b928f23492196cb"
DISTRIBUTION3_VERSION="3.1.2"
DISTRIBUTION3_SHA256="40df2224d410f72ae425c3371873b078bbdbda3b8b612be9571f0e6751f3acc8"
DISTRIBUTION2_VERSION="2.8.3"
DISTRIBUTION2_SHA256="b1f750ecbe09f38e2143e22c61a25e3da2afe1510d9522859230b480e642ceff"

if [ "$(uname -s)/$(uname -m)" != "Linux/x86_64" ]; then
	echo "footprint: Linux on amd64 only" >&2
	exit 1
fi

work="$(mktemp -d)"
base_tree=""
cleanup() {
	if [ -n "${base_tree}" ]; then
		git worktree remove --force "${base_tree}" >/dev/null 2>&1 || true
	fi
	rm -rf "${work}"
}
trap cleanup EXIT

RESULTS_DIR="${RESULTS_DIR:-${__root}/footprint-results}"
mkdir -p "${RESULTS_DIR}"
cache="${FOOTPRINT_CACHE:-${XDG_CACHE_HOME:-${HOME}/.cache}/cr-footprint}"
mkdir -p "${cache}"

build() { # dir out
	(cd "$1" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$2" ./cmd/cr)
}

fetch() { # url sha256 out
	if [ ! -f "$3" ] || ! echo "$2  $3" | sha256sum --check --status; then
		curl -fsSL -o "$3.part" "$1"
		echo "$2  $3.part" | sha256sum --check --status || {
			echo "footprint: $1 does not match its pinned checksum" >&2
			exit 1
		}
		mv "$3.part" "$3"
	fi
}

build "${__root}" "${work}/cr"
servers=("cr=cr:${work}/cr")
baseline=()

if [ -n "${BASE:-}" ]; then
	base_tree="${work}/base"
	git worktree add --detach "${base_tree}" "${BASE}" >/dev/null
	build "${base_tree}" "${work}/cr-base"
	servers=("base=cr:${work}/cr-base" "head=cr:${work}/cr")
	baseline=(--baseline base)
else
	z="https://github.com/project-zot/zot/releases/download/${ZOT_VERSION}"
	fetch "${z}/zot-linux-amd64-minimal" "${ZOT_MINIMAL_SHA256}" "${cache}/zot-minimal-${ZOT_VERSION}"
	fetch "${z}/zot-linux-amd64" "${ZOT_SHA256}" "${cache}/zot-${ZOT_VERSION}"
	chmod +x "${cache}/zot-minimal-${ZOT_VERSION}" "${cache}/zot-${ZOT_VERSION}"

	for v in "${DISTRIBUTION3_VERSION}:${DISTRIBUTION3_SHA256}" "${DISTRIBUTION2_VERSION}:${DISTRIBUTION2_SHA256}"; do
		version="${v%%:*}"
		tarball="${cache}/registry_${version}_linux_amd64.tar.gz"
		fetch "https://github.com/distribution/distribution/releases/download/v${version}/registry_${version}_linux_amd64.tar.gz" "${v#*:}" "${tarball}"
		mkdir -p "${work}/distribution-${version}"
		tar -xzf "${tarball}" -C "${work}/distribution-${version}" registry
	done

	servers+=(
		"distribution-${DISTRIBUTION3_VERSION}=distribution:${work}/distribution-${DISTRIBUTION3_VERSION}/registry"
		"distribution-${DISTRIBUTION2_VERSION}=distribution:${work}/distribution-${DISTRIBUTION2_VERSION}/registry"
		"zot-minimal-${ZOT_VERSION}=zot:${cache}/zot-minimal-${ZOT_VERSION}"
		"zot-${ZOT_VERSION}=zot:${cache}/zot-${ZOT_VERSION}"
	)
fi

python3 "${__root}/scripts/footprint.py" "${baseline[@]}" \
	--json "${RESULTS_DIR}/footprint.json" \
	--markdown "${RESULTS_DIR}/footprint.md" \
	"${servers[@]}"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		if [ -n "${BASE:-}" ]; then
			echo "## Footprint: this change against \`${BASE}\`"
		else
			echo "## Footprint: cr beside other registries"
		fi
		echo
		cat "${RESULTS_DIR}/footprint.md"
	} >>"${GITHUB_STEP_SUMMARY}"
fi
