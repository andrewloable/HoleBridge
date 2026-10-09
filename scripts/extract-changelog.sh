#!/bin/sh
# Prints the CHANGELOG.md section for one version, without its heading, so a release can use it as its
# notes. A section starts at its "## [<version>]" heading (a date may follow after a space) and ends at the
# next "## " heading or at the first link reference line ("[1.0.0]: https://..."). Leading and trailing
# blank lines are dropped.
#
# Usage: scripts/extract-changelog.sh <version> [CHANGELOG.md]
# Exits 1, naming the problem on stderr, when the file has no section with content for the version.
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ] || [ -z "$1" ]; then
  echo "usage: scripts/extract-changelog.sh <version> [CHANGELOG.md]" >&2
  exit 2
fi
version="$1"
case "$version" in
  *[!A-Za-z0-9._+-]*)
    echo "extract-changelog: version '$version' must use only letters, digits, '.', '_', '+' and '-'" >&2
    exit 2
    ;;
esac

root="$(cd "$(dirname "$0")/.." && pwd)"
file="${2:-$root/CHANGELOG.md}"
if [ ! -f "$file" ]; then
  echo "extract-changelog: $file does not exist" >&2
  exit 1
fi

section="$(awk -v version="$version" '
  BEGIN { heading = "## [" version "]"; state = 0; n = 0 }
  state == 0 && ($0 == heading || index($0, heading " ") == 1) { state = 1; next }
  state == 1 && (/^## / || /^\[.+\]: /) { exit }
  state == 1 { line[++n] = $0 }
  END {
    first = 1
    while (first <= n && line[first] ~ /^[ \t]*$/) first++
    last = n
    while (last >= first && line[last] ~ /^[ \t]*$/) last--
    for (i = first; i <= last; i++) print line[i]
  }
' "$file")"

if [ -z "$(printf '%s' "$section" | tr -d '[:space:]')" ]; then
  echo "extract-changelog: $file has no section with content for $version (want a '## [$version]' heading)" >&2
  exit 1
fi
printf '%s\n' "$section"
