# Other registries at pinned releases, for scripts that measure cr beside
# them. Sourced, not run.
#
# Each is checked against the checksum its project publishes, and kept in
# FOOTPRINT_CACHE between runs. Linux on amd64: the pinned binaries are.

ZOT_VERSION="v2.1.21"
ZOT_SHA256="8751cc0daf739634835a3bd8206e3094c84d552e2c462e4a4baf80f40dd92685"
ZOT_MINIMAL_SHA256="d2422616a28dbae10a92c1df9daa23980e2f7f3deb0079968b928f23492196cb"
DISTRIBUTION3_VERSION="3.1.2"
DISTRIBUTION3_SHA256="40df2224d410f72ae425c3371873b078bbdbda3b8b612be9571f0e6751f3acc8"
DISTRIBUTION2_VERSION="2.8.3"
DISTRIBUTION2_SHA256="b1f750ecbe09f38e2143e22c61a25e3da2afe1510d9522859230b480e642ceff"

registries_cache="${FOOTPRINT_CACHE:-${XDG_CACHE_HOME:-${HOME}/.cache}/cr-footprint}"

# fetch URL SHA256 OUT: download URL to OUT unless OUT already matches.
fetch() {
	if [ ! -f "$3" ] || ! echo "$2  $3" | sha256sum --check --status; then
		curl -fsSL -o "$3.part" "$1"
		echo "$2  $3.part" | sha256sum --check --status || {
			echo "$1 does not match its pinned checksum" >&2
			exit 1
		}
		mv "$3.part" "$3"
	fi
}

# fetch_zot [minimal]: the path of zot, or of its minimal build.
fetch_zot() {
	mkdir -p "${registries_cache}"
	local name="zot-linux-amd64" sum="${ZOT_SHA256}" out="${registries_cache}/zot-${ZOT_VERSION}"
	if [ "${1:-}" = minimal ]; then
		name="zot-linux-amd64-minimal" sum="${ZOT_MINIMAL_SHA256}" out="${registries_cache}/zot-minimal-${ZOT_VERSION}"
	fi
	fetch "https://github.com/project-zot/zot/releases/download/${ZOT_VERSION}/${name}" "${sum}" "${out}"
	chmod +x "${out}"
	echo "${out}"
}

# fetch_distribution VERSION SHA256 DIR: distribution's registry, extracted
# into DIR; prints its path.
fetch_distribution() {
	mkdir -p "${registries_cache}" "$3"
	local tarball="${registries_cache}/registry_$1_linux_amd64.tar.gz"
	fetch "https://github.com/distribution/distribution/releases/download/v$1/registry_$1_linux_amd64.tar.gz" "$2" "${tarball}"
	tar -xzf "${tarball}" -C "$3" registry
	echo "$3/registry"
}

# build_cr DIR OUT: cr from the checkout at DIR, built the way the image is.
build_cr() {
	(cd "$1" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$2" ./cmd/cr)
}
