#!/usr/bin/env bash
# Self-test for scripts/release-build.sh. Builds version 0.0.0-test into a temporary directory, then
# checks the six archives (their contents, the build flags and version stamp of each binary), the
# SHA256SUMS file, and the version the binary for this machine prints. The temporary directory is
# removed on exit.
#
# Usage: scripts/release-build_test.sh
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/holebridge-release-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT

version=0.0.0-test
out="$work/dist"

fail() {
  echo "release-build_test: FAIL: $*" >&2
  exit 1
}

# check_entry <archive> <listing> <exact entry>
check_entry() {
  printf '%s\n' "$2" | grep -qxF -- "$3" || fail "$1 has no entry '$3'"
}

# check_pattern <archive> <listing> <extended regex that one entry must match>
check_pattern() {
  printf '%s\n' "$2" | grep -qxE -- "$3" || fail "$1 has no entry matching '$3'"
}

# list_archive <archive>
list_archive() {
  case "$1" in
    *.zip) unzip -Z1 "$1" ;;
    *) tar -tzf "$1" ;;
  esac
}

# extract_archive <archive> <dir>
extract_archive() {
  mkdir -p "$2"
  case "$1" in
    *.zip) unzip -q "$1" -d "$2" ;;
    *) tar -xzf "$1" -C "$2" ;;
  esac
}

echo "release-build_test: building $version into a temporary directory"
OUT_DIR="$out" "$root/scripts/release-build.sh" "$version"

count="$(find "$out" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')"
[ "$count" -eq 7 ] || fail "$out should hold six archives and SHA256SUMS, found $count entries"

sums_lines="$(wc -l < "$out/SHA256SUMS" | tr -d ' ')"
[ "$sums_lines" -eq 6 ] || fail "SHA256SUMS has $sums_lines lines, want 6"
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$out" && sha256sum -c SHA256SUMS >/dev/null) || fail "sha256sum -c SHA256SUMS failed"
else
  (cd "$out" && shasum -a 256 -c SHA256SUMS >/dev/null) || fail "shasum -a 256 -c SHA256SUMS failed"
fi
echo "ok: SHA256SUMS verifies the six archives"

for os in linux darwin windows; do
  for arch in amd64 arm64; do
    name="holebridge_${version}_${os}_${arch}"
    exe=holebridge
    file="$name.tar.gz"
    if [ "$os" = windows ]; then
      exe=holebridge.exe
      file="$name.zip"
    fi
    archive="$out/$file"
    [ -f "$archive" ] || fail "missing archive $file"

    case "$file" in
      *.tar.gz)
        # Every entry must be owned by root: uid 0, gid 0, and no account name but root (or none).
        python3 -I -c 'import sys, tarfile
bad = [m.name for m in tarfile.open(sys.argv[1]) if m.uid or m.gid or m.uname not in ("", "root") or m.gname not in ("", "root")]
sys.exit(1 if bad else 0)' "$archive" || fail "$file: entries are not owned by root (uid 0, gid 0)"
        ;;
    esac

    listing="$(list_archive "$archive")"
    check_entry "$file" "$listing" "$exe"
    check_entry "$file" "$listing" LICENSE
    check_entry "$file" "$listing" THIRD_PARTY_NOTICES.md
    check_entry "$file" "$listing" licenses/go-stdlib/LICENSE
    check_entry "$file" "$listing" licenses/go-stdlib/PATENTS
    check_pattern "$file" "$listing" 'licenses/go/filippo\.io/edwards25519@[^/]+/LICENSE'
    check_pattern "$file" "$listing" 'licenses/go/golang\.org/x/crypto@[^/]+/LICENSE'
    check_pattern "$file" "$listing" 'licenses/go/golang\.org/x/sys@[^/]+/LICENSE'
    check_entry "$file" "$listing" licenses/ports/libudx/NOTICE
    check_entry "$file" "$listing" licenses/ports/@hyperswarm/secret-stream/LICENSE
    ports="$(printf '%s\n' "$listing" | grep -cE '^licenses/ports/.+/LICENSE$' || true)"
    [ "$ports" -eq 14 ] || fail "$file has $ports licenses/ports/*/LICENSE files, want 14"
    if printf '%s\n' "$listing" | grep -qE '(^|/)\._'; then
      fail "$file has AppleDouble (._) entries"
    fi

    dir="$work/extract/$name"
    extract_archive "$archive" "$dir"
    bin="$dir/$exe"
    [ -f "$bin" ] || fail "$file: $exe was not extracted"
    build_info="$(go version -m "$bin")" || fail "go version -m failed on $file"
    printf '%s\n' "$build_info" | grep -qE '[[:space:]]build[[:space:]]+-trimpath=true$' \
      || fail "$file: binary was not built with -trimpath"
    printf '%s\n' "$build_info" | grep -qE '[[:space:]]build[[:space:]]+CGO_ENABLED=0$' \
      || fail "$file: binary was not built with CGO_ENABLED=0"
    # The version is set with -ldflags -X, which go version -m does not record. The default value
    # is "dev", so the release version appearing in the binary is the stamp.
    grep -aqF -- "$version" "$bin" || fail "$file: binary does not carry version $version"

    echo "ok: $file"
  done
done

case "$(uname -s)" in
  Linux) host_os=linux ;;
  Darwin) host_os=darwin ;;
  *) host_os=unknown ;;
esac
case "$(uname -m)" in
  x86_64|amd64) host_arch=amd64 ;;
  arm64|aarch64) host_arch=arm64 ;;
  *) host_arch=unknown ;;
esac
host_bin="$work/extract/holebridge_${version}_${host_os}_${host_arch}/holebridge"
if [ -f "$host_bin" ]; then
  got="$("$host_bin" --version)" || fail "$host_os/$host_arch binary failed to run --version"
  [ "$got" = "holebridge $version" ] || fail "$host_os/$host_arch binary printed '$got', want 'holebridge $version'"
  echo "ok: $host_os/$host_arch binary prints '$got'"
else
  echo "skip: this machine ($(uname -s)/$(uname -m)) is not one of the six targets, so no binary is run"
fi

echo "release-build_test: PASS"
