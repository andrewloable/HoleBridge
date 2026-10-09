#!/bin/sh
# Self-test for check-go-deps.sh: the check must fail on a copy of the module that imports a
# disallowed package and uses cgo, and must pass on the real tree.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
check="$root/scripts/check-go-deps.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for f in go.mod go.sum; do
	if [ -f "$root/$f" ]; then cp "$root/$f" "$tmp/"; fi
done
for d in cmd internal pears; do
	if [ -d "$root/$d" ]; then cp -R "$root/$d" "$tmp/"; fi
done

# A fake dependency outside the allowed set, pulled in through a replace directive.
mkdir -p "$tmp/fake" "$tmp/cmd/badfake" "$tmp/cmd/badcgo"
cat > "$tmp/fake/go.mod" <<'GOMOD'
module example.org/fake

go 1.25.0
GOMOD
cat > "$tmp/fake/fake.go" <<'GOSRC'
package fake

// Hello exists only so that the bad command compiles.
func Hello() string { return "fake" }
GOSRC
cat > "$tmp/cmd/badfake/main.go" <<'GOSRC'
package main

import "example.org/fake"

func main() { _ = fake.Hello() }
GOSRC
cat > "$tmp/cmd/badcgo/main.go" <<'GOSRC'
package main

// #include <stdlib.h>
import "C"

func main() { C.free(nil) }
GOSRC
cat >> "$tmp/go.mod" <<'GOMOD'

require example.org/fake v0.0.0

replace example.org/fake => ./fake
GOMOD

rc=0
out=$(sh "$check" "$tmp" 2>&1) || rc=$?
if [ "$rc" -ne 1 ]; then
	echo "FAIL: check exited $rc on a copy with a disallowed package and cgo, want 1:" >&2
	echo "$out" >&2
	exit 1
fi
case "$out" in
*example.org/fake*) ;;
*)
	echo "FAIL: check did not name example.org/fake:" >&2
	echo "$out" >&2
	exit 1
	;;
esac
case "$out" in
*badcgo*) ;;
*)
	echo "FAIL: check did not name the cgo package:" >&2
	echo "$out" >&2
	exit 1
	;;
esac
echo "ok: check fails on a disallowed package and on cgo"

if ! out=$(sh "$check" "$root" 2>&1); then
	echo "FAIL: check fails on the real tree:" >&2
	echo "$out" >&2
	exit 1
fi
echo "ok: check passes on the real tree"
