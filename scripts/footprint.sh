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

# shellcheck source=lib/registries.sh
. "${__root}/scripts/lib/registries.sh"

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
build_cr "${__root}" "${work}/cr"
servers=("cr=cr:${work}/cr")
baseline=()

if [ -n "${BASE:-}" ]; then
	base_tree="${work}/base"
	git worktree add --detach "${base_tree}" "${BASE}" >/dev/null
	build_cr "${base_tree}" "${work}/cr-base"
	servers=("base=cr:${work}/cr-base" "head=cr:${work}/cr")
	baseline=(--baseline base)
else
	zot_minimal="$(fetch_zot minimal)"
	zot="$(fetch_zot)"
	distribution3="$(fetch_distribution "${DISTRIBUTION3_VERSION}" "${DISTRIBUTION3_SHA256}" "${work}/distribution-${DISTRIBUTION3_VERSION}")"
	distribution2="$(fetch_distribution "${DISTRIBUTION2_VERSION}" "${DISTRIBUTION2_SHA256}" "${work}/distribution-${DISTRIBUTION2_VERSION}")"

	servers+=(
		"distribution-${DISTRIBUTION3_VERSION}=distribution:${distribution3}"
		"distribution-${DISTRIBUTION2_VERSION}=distribution:${distribution2}"
		"zot-minimal-${ZOT_VERSION}=zot:${zot_minimal}"
		"zot-${ZOT_VERSION}=zot:${zot}"
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
