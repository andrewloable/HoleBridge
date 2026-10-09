#!/bin/sh
# Fails when a Go package outside the allowed set is compiled in (D32), or when any non-standard
# package uses cgo. Usage: scripts/check-go-deps.sh [module-dir]
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "${1:-$root}"

# cgo files are dropped under CGO_ENABLED=0, so the cgo query needs cgo on to see them.
export CGO_ENABLED=1

# The module itself, x/crypto, x/sys and edwards25519, with their subpackages.
allowed='^(github\.com/andrewloable/HoleBridge(/.*)?|golang\.org/x/(crypto|sys)/.*|filippo\.io/edwards25519(/.*)?)$'

status=0

deps=$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./...)
offenders=$(printf '%s\n' "$deps" | grep -v -E "$allowed" | grep -v '^$' || true)
if [ -n "$offenders" ]; then
	echo "check-go-deps: packages outside the allowed set (D32):" >&2
	printf '%s\n' "$offenders" | sed 's/^/  /' >&2
	status=1
fi

cgo_list=$(go list -deps -f '{{if and (not .Standard) .CgoFiles}}{{.ImportPath}}{{end}}' ./...)
cgo=$(printf '%s\n' "$cgo_list" | grep . || true)
if [ -n "$cgo" ]; then
	echo "check-go-deps: packages that use cgo (must be pure Go):" >&2
	printf '%s\n' "$cgo" | sed 's/^/  /' >&2
	status=1
fi

exit "$status"
