#!/usr/bin/env bash
# Moving a registry off zot, the way CI checks it: a real zot at the pinned
# release, images pushed to it, its root taken in with `cr import`'s package
# while it serves, and every answer compared (importer/zot_test.go).
#
#     ./scripts/zot-import.sh
#
# Linux on amd64: the pinned zot is.
set -o errexit
set -o pipefail
set -o nounset

__root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${__root}"

# shellcheck source=lib/registries.sh
. "${__root}/scripts/lib/registries.sh"

CR_TEST_ZOT="$(fetch_zot)" go test -count=1 -run 'TestZot' -v ./importer/
