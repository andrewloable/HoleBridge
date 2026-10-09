#!/usr/bin/env bash
# Builds the holebridge release archives: linux, darwin and windows, each on amd64 and arm64.
#
# Usage: scripts/release-build.sh <version>
#
# Writes into $OUT_DIR (default: dist/ in the repo root; a relative OUT_DIR is taken from the
# current directory):
#   holebridge_<version>_<os>_<arch>.tar.gz   (.zip on windows)
#   SHA256SUMS                                 sha256sum format, one line per archive
# Each archive holds, at its top level, the holebridge binary (holebridge.exe on windows), LICENSE,
# THIRD_PARTY_NOTICES.md and licenses/ (written by scripts/collect-licenses.sh).
#
# The binary is built with CGO_ENABLED=0 and -trimpath, and internal/version.Version is set to
# <version>. npm ci runs in app/engine first, because collect-licenses.sh reads the npm ports from
# app/engine/node_modules. Needs go, node, npm, zip, tar, and sha256sum or shasum. Build files go to
# a temporary staging directory, which is removed on exit.
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: scripts/release-build.sh <version>" >&2
  exit 2
fi
version="$1"
case "$version" in
  ''|-*|*[!A-Za-z0-9._+-]*)
    echo "release-build: version '$version' must be non-empty and use only letters, digits, '.', '_', '+' and '-'" >&2
    exit 2
    ;;
esac

root="$(cd "$(dirname "$0")/.." && pwd)"
out="${OUT_DIR:-$root/dist}"
case "$out" in
  /*) ;;
  *) out="$PWD/$out" ;;
esac

for tool in go node npm zip tar; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "release-build: $tool is required" >&2
    exit 1
  fi
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  echo "release-build: sha256sum or shasum is required" >&2
  exit 1
fi

# sha256_of <files...>: sha256sum on Linux, shasum on macOS.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi
}

stage="$(mktemp -d "${TMPDIR:-/tmp}/holebridge-release.XXXXXX")"
trap 'rm -rf "$stage"' EXIT

cd "$root"
ldflags="-s -w -X github.com/andrewloable/HoleBridge/internal/version.Version=$version"

echo "release-build: npm ci in app/engine"
(
  cd "$root/app/engine"
  npm ci
)

echo "release-build: collecting license texts into $stage/licenses"
"$root/scripts/collect-licenses.sh" "$stage"

mkdir -p "$out"
archives=()

# Ownership options for the tar archives, chosen once. Every entry is recorded as uid 0 and gid 0 with
# the name root, so the archives do not carry the builder's uid, gid or account names. bsdtar (macOS)
# and GNU tar spell these options differently.
case "$(tar --version 2>/dev/null || true)" in
  *bsdtar*) tar_owner=(--uid 0 --gid 0 --uname root --gname root) ;;
  *) tar_owner=(--owner=0 --group=0 --numeric-owner) ;;
esac

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  goos="${target%/*}"
  goarch="${target#*/}"
  exe=holebridge
  if [ "$goos" = windows ]; then
    exe=holebridge.exe
  fi
  name="holebridge_${version}_${goos}_${goarch}"
  pkg="$stage/pkg/$name"
  mkdir -p "$pkg"

  echo "release-build: go build $goos/$goarch"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "$ldflags" -o "$pkg/$exe" ./cmd/holebridge
  cp LICENSE THIRD_PARTY_NOTICES.md "$pkg/"
  cp -R "$stage/licenses" "$pkg/"

  if [ "$goos" = windows ]; then
    archive="$name.zip"
    rm -f "$out/$archive"
    (cd "$pkg" && zip -q -X -r "$out/$archive" "$exe" LICENSE THIRD_PARTY_NOTICES.md licenses)
  else
    archive="$name.tar.gz"
    (cd "$pkg" && COPYFILE_DISABLE=1 tar -czf "$out/$archive" "${tar_owner[@]}" "$exe" LICENSE THIRD_PARTY_NOTICES.md licenses)
  fi
  archives+=("$archive")
done

(
  cd "$out"
  sha256_of "${archives[@]}"
) > "$stage/SHA256SUMS"
cp "$stage/SHA256SUMS" "$out/SHA256SUMS"

echo "release-build: wrote ${#archives[@]} archives and SHA256SUMS to $out"
