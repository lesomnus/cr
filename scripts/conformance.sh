#!/usr/bin/env bash
# The OCI distribution-spec conformance suite against a cr built from this
# checkout, on SQLite and a directory, the way CI runs it.
#
#     ./scripts/conformance.sh
#     RESULTS_DIR=./results ./scripts/conformance.sh     # keep report.html
#
# The suite is pinned by commit: it was redesigned in 2026 and moves faster
# than its releases. Which APIs it exercises is set below, and every one of
# them is on.
set -o errexit
set -o pipefail
set -o nounset

__root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${__root}"

CONFORMANCE_REF="${CONFORMANCE_REF:-97274622c11112caa21efb8c52acca3c6b8fa7f1}"
PORT="${PORT:-5055}"

work="$(mktemp -d)"
pid=""
cleanup() {
	if [ -n "${pid}" ]; then
		kill "${pid}" 2>/dev/null || true
		wait "${pid}" 2>/dev/null || true
	fi
	if [ -z "${KEEP_WORK:-}" ]; then
		rm -rf "${work}"
	fi
}
trap cleanup EXIT

go build -o "${work}/cr" ./cmd/cr
GOBIN="${work}" go install "github.com/opencontainers/distribution-spec/conformance@${CONFORMANCE_REF}"

cat >"${work}/cr.yaml" <<YAML
db:
  driver: sqlite3
  dsn: "file:${work}/cr.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"
  migrate: true
server:
  addr: "127.0.0.1:0"
  http:
    addr: "127.0.0.1:${PORT}"
watch:
  broker: memory
registry:
  storage:
    driver: os
    os:
      root: "${work}/data"
YAML

"${work}/cr" --config "${work}/cr.yaml" serve >"${work}/cr.log" 2>&1 &
pid=$!

for _ in $(seq 1 100); do
	if curl -fsS "http://127.0.0.1:${PORT}/v2/" >/dev/null 2>&1; then
		break
	fi
	sleep 0.1
done
curl -fsS "http://127.0.0.1:${PORT}/v2/" >/dev/null || {
	cat "${work}/cr.log" >&2
	exit 1
}

export OCI_REGISTRY="127.0.0.1:${PORT}"
export OCI_TLS=disabled
export OCI_REPO1=conformance/repo1
export OCI_REPO2=conformance/repo2
export OCI_VERSION=1.1
export OCI_RESULTS_DIR="${RESULTS_DIR:-${work}/results}"
export OCI_API_BLOBS_DIGEST_HEADER=true
export OCI_API_BLOBS_UPLOAD_CANCEL=true
export OCI_API_MANIFESTS_DIGEST_HEADER=true
mkdir -p "${OCI_RESULTS_DIR}"

status=0
(cd "${work}" && ./conformance) || status=$?

if [ "${status}" -ne 0 ]; then
	echo "--- cr log" >&2
	tail -n 50 "${work}/cr.log" >&2
fi
exit "${status}"
