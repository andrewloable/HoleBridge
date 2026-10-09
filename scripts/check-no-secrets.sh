#!/bin/sh
# Fails when git tracks a file that holds a secret: app.key, relay.key or host.json anywhere in the
# tree, or any *.key file outside spec/ (spec/ holds only test values). Lists each offender.
# Usage: scripts/check-no-secrets.sh
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

tracked=$(git ls-files)
named=$(printf '%s\n' "$tracked" | grep -E '(^|/)(app\.key|relay\.key|host\.json)$' || true)
keys=$(printf '%s\n' "$tracked" | grep -E '\.key$' | grep -v '^spec/' || true)
offenders=$(printf '%s\n%s\n' "$named" "$keys" | grep . | sort -u || true)

if [ -n "$offenders" ]; then
	echo "check-no-secrets: git tracks files that hold secrets:" >&2
	printf '%s\n' "$offenders" | sed 's/^/  /' >&2
	exit 1
fi
echo "check-no-secrets: no tracked key or host.json files."
