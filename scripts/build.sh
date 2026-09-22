#!/usr/bin/env bash
# Builds the provider release: one archive per platform, laid out as a
# Terraform packed mirror expects it, and the checksum list `subako terraform`
# verifies a download against.
#
#   scripts/build.sh [version] [output-dir]
#
# The version defaults to the tag this is building, without its leading `v`.
# The archive and binary keep the `terraform-provider-subako` spelling the
# mirror layout requires, which is not this repository's name.
set -euo pipefail
cd "$(dirname "$0")/.."

version="${1:-${GITHUB_REF_NAME#v}}"
if [ -z "$version" ]; then
	echo "usage: $0 <version> [output-dir]" >&2
	exit 2
fi
out="${2:-target}"
out="$(mkdir -p "$out" && cd "$out" && pwd)"
checksums="terraform-provider-subako_${version}_SHA256SUMS"

# The platforms the Subako CLI is released for, as Terraform spells them. The
# CLI installs the provider for the platform it runs on, so one missing here is
# a download that 404s.
platforms=(
	darwin/amd64
	darwin/arm64
	linux/amd64
	linux/arm64
	windows/amd64
)

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

rm -f "$out"/terraform-provider-subako_"${version}"_*
for platform in "${platforms[@]}"; do
	os="${platform%/*}"
	arch="${platform#*/}"
	binary="terraform-provider-subako_v${version}"
	if [ "$os" = windows ]; then
		binary="${binary}.exe"
	fi
	dir="$work/${os}_${arch}"
	mkdir -p "$dir"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
		-trimpath \
		-ldflags "-s -w -X main.version=${version}" \
		-o "$dir/$binary" .
	archive="terraform-provider-subako_${version}_${os}_${arch}.zip"
	# A fixed mtime, read in one zone, and -X dropping extra file attributes
	# leave the archive depending on the binary alone.
	(cd "$dir" && export TZ=UTC && touch -t 198001010000 "$binary" && zip -X -q "$out/$archive" "$binary")
	echo "built $archive"
done

(cd "$out" && sha256sum terraform-provider-subako_"${version}"_*.zip > "$checksums")
echo "wrote $checksums"
