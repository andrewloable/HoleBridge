#!/usr/bin/env bash
# Collects the license texts that a host or relay release archive must carry into <outdir>/licenses/.
#
# Usage: scripts/collect-licenses.sh <outdir>      (run from anywhere; it cds to the repo root)
#
# The archive build (scripts/release-build.sh, HoleBridge-9co.1) must call this once per release,
# after go build, with the directory it packs the archives from, and pack <outdir>/licenses/ into
# every archive next to LICENSE and THIRD_PARTY_NOTICES.md.
#
# Collects, from the Go toolchain and module cache (go mod download fills the cache if needed):
#   licenses/go-stdlib/            LICENSE and PATENTS of the Go standard library ($GOROOT)
#   licenses/go/<module>@<ver>/    LICENSE, PATENTS and NOTICE of every module compiled into
#                                  cmd/holebridge (golang.org/x/crypto, golang.org/x/sys,
#                                  filippo.io/edwards25519 today)
#
#   licenses/ports/<library>/      LICENSE (and NOTICE) of each upstream library whose code is ported
#                                  into the host binary (THIRD_PARTY_NOTICES.md, "Ported code"; the
#                                  PORTS list below). The npm ports are read from app/engine/node_modules,
#                                  which git ignores, so run "npm ci" in app/engine first. The others are
#                                  vendored under packaging/licenses/<library>/.
#
# The script fails, naming the library, when a text is missing or an installed npm port is not the
# version PORTS pins.
set -euo pipefail

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
  echo "usage: scripts/collect-licenses.sh <outdir>" >&2
  exit 2
fi

case "$1" in /*) out="$1/licenses" ;; *) out="$PWD/$1/licenses" ;; esac
cd "$(dirname "$0")/.."
rm -rf "$out"
mkdir -p "$out"

# copy_texts <source dir> <destination dir>: copies the LICENSE, PATENTS and NOTICE files of one
# module or toolchain. Fails when the source has none, so a module without a license is noticed.
copy_texts() {
  local src="$1" dst="$2" found=0 f
  mkdir -p "$dst"
  for f in "$src"/LICENSE* "$src"/PATENTS* "$src"/NOTICE*; do
    [ -f "$f" ] || continue
    cp "$f" "$dst/"
    found=$((found + 1))
  done
  if [ "$found" -eq 0 ]; then
    echo "collect-licenses: no LICENSE, PATENTS or NOTICE file in $src" >&2
    exit 1
  fi
}

# The ported code: one line per upstream library, "<library> <version> <npm|vendored>". The version is
# the one in THIRD_PARTY_NOTICES.md. An npm library must be installed at it. A vendored library's texts
# are packaging/licenses/<library>/, fetched at the commit or tag its Ported code row names:
# libudx and its win_filter at ae8bff7, libsodium at its 1.0.20-RELEASE tag (commit 9511c98), and
# noise-curve-ed, which ships no LICENSE upstream (see its row).
PORTS='
kademlia-routing-table 1.0.6 npm
compact-encoding 3.5.2 npm
noise-handshake 4.2.0 npm
dht-rpc 6.27.0 npm
nat-sampler 1.0.1 npm
hyperdht 6.34.1 npm
protomux 3.12.1 npm
blind-relay 1.6.1 npm
@hyperswarm/secret-stream 6.9.2 npm
hypercore-crypto 3.7.0 npm
noise-curve-ed 2.1.0 vendored
libsodium 1.0.20 vendored
libudx ae8bff7 vendored
libudx-win-filter ae8bff7 vendored
'

# copy_port <library> <version> <npm|vendored>: copies the LICENSE, NOTICE and PATENTS files of one
# ported library into <outdir>/licenses/ports/<library>/. Fails, naming the library, when its LICENSE
# is missing, or when an npm library is not installed at the pinned version.
copy_port() {
  local name="$1" version="$2" kind="$3" src dst installed f
  dst="$out/ports/$name"
  if [ "$kind" = vendored ]; then
    src="packaging/licenses/$name"
  else
    src="app/engine/node_modules/$name"
    if [ ! -d "$src" ]; then
      echo "collect-licenses: $name is not installed in app/engine/node_modules; run (cd app/engine && npm ci) first" >&2
      exit 1
    fi
    installed="$(PKG_JSON="$PWD/$src/package.json" node -p 'require(process.env.PKG_JSON).version' < /dev/null 2>/dev/null || true)"
    if [ "$installed" != "$version" ]; then
      echo "collect-licenses: $name is installed at '${installed:-no readable version}' in $src, but THIRD_PARTY_NOTICES.md pins $version" >&2
      exit 1
    fi
  fi
  if [ ! -s "$src/LICENSE" ]; then
    echo "collect-licenses: no LICENSE for $name $version in $src" >&2
    exit 1
  fi
  mkdir -p "$dst"
  for f in "$src"/LICENSE* "$src"/NOTICE* "$src"/PATENTS*; do
    [ -f "$f" ] || continue
    cp "$f" "$dst/"
  done
}

copy_texts "$(go env GOROOT)" "$out/go-stdlib"

# The union over the six release targets: golang.org/x/sys is compiled in on some targets only.
deps=""
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  deps+="$(CGO_ENABLED=0 GOOS="${target%/*}" GOARCH="${target#*/}" \
    go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/holebridge)"$'\n'
done

modules=0
while read -r path version dir; do
  [ -n "$path" ] || continue
  copy_texts "$dir" "$out/go/$path@$version"
  modules=$((modules + 1))
done <<< "$(printf '%s' "$deps" | sort -u)"

if [ "$modules" -eq 0 ]; then
  echo "collect-licenses: go list found no third-party modules for ./cmd/holebridge" >&2
  exit 1
fi

ports=0
while read -r name version kind; do
  [ -n "$name" ] || continue
  copy_port "$name" "$version" "$kind"
  ports=$((ports + 1))
done <<< "$PORTS"

echo "collect-licenses: wrote $(find "$out" -type f | wc -l | tr -d ' ') files for $modules modules, $ports ported libraries and the Go standard library to $out"
