#!/bin/sh
# Self-test for scripts/extract-changelog.sh. Runs it on a sample changelog in a temporary directory (removed
# on exit) and checks the section printed for each version, the failure cases, and the default CHANGELOG.md
# path when the script sits in a repo of its own.
#
# Usage: scripts/extract-changelog_test.sh
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
script="$root/scripts/extract-changelog.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/extract-changelog-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT

fail() {
  echo "extract-changelog_test: FAIL: $*" >&2
  exit 1
}

sample="$work/CHANGELOG.md"
cat > "$sample" <<'EOF'
# Changelog

Intro text that is not part of any section.

## [Unreleased]

### Added

- Upcoming thing.

## [1.1.0] - 2026-11-02

### Fixed

- Second fix.

## [1.0.0] - 2026-10-09

### Added

- First thing.
- Second thing.

  A continued paragraph.

## [0.9.0]

- Old thing.

## [0.8.0] - 2026-01-02

## [0.7.0] - 2025-12-31

- Seven.

[Unreleased]: https://example.com/compare/v1.1.0...HEAD
[1.1.0]: https://example.com/compare/v1.0.0...v1.1.0
EOF

# expect_section <version>: the section for the version must equal the file expect/<version>.
expect_section() {
  "$script" "$1" "$sample" > "$work/out" 2> "$work/err" || fail "$1 exited $?: $(cat "$work/err")"
  cmp -s "$work/out" "$work/expect/$1" || {
    echo "--- got" >&2
    cat "$work/out" >&2
    echo "--- want" >&2
    cat "$work/expect/$1" >&2
    fail "section for $1 differs from the expected text"
  }
  echo "ok: section for $1"
}

# expect_status <want> <args...>: the script must exit with <want>, and a failure must say why on stderr.
expect_status() {
  want="$1"
  shift
  got=0
  "$script" "$@" > "$work/out" 2> "$work/err" || got=$?
  [ "$got" -eq "$want" ] || fail "extract-changelog.sh $* exited $got, want $want"
  if [ "$want" -ne 0 ] && [ ! -s "$work/err" ]; then
    fail "extract-changelog.sh $* failed without a message"
  fi
  echo "ok: extract-changelog.sh $* exits $want"
}

mkdir -p "$work/expect"
cat > "$work/expect/Unreleased" <<'EOF'
### Added

- Upcoming thing.
EOF
cat > "$work/expect/1.1.0" <<'EOF'
### Fixed

- Second fix.
EOF
cat > "$work/expect/1.0.0" <<'EOF'
### Added

- First thing.
- Second thing.

  A continued paragraph.
EOF
cat > "$work/expect/0.9.0" <<'EOF'
- Old thing.
EOF

expect_section Unreleased
expect_section 1.1.0
expect_section 1.0.0
expect_section 0.9.0

# A heading with no date is a section too; the link references and the next heading end it.
"$script" 0.9.0 "$sample" | grep -q 'Old thing' || fail "0.9.0 (no date) was not found"

# 1.0 is a prefix of 1.0.0 and must not print its section.
expect_status 1 1.0 "$sample"
# A heading with no content (0.8.0) is a failure, not an empty notes file.
expect_status 1 0.8.0 "$sample"
# A version with no heading at all.
expect_status 1 2.0.0 "$sample"
# A missing file.
expect_status 1 1.0.0 "$work/missing.md"
# Usage errors, and a version that would be unsafe in the awk program or in a file name.
expect_status 2
expect_status 2 1.0.0 "$sample" extra
expect_status 2 '1.0.0*' "$sample"
expect_status 2 '1.0.0]' "$sample"
expect_status 2 ''

# With no file argument the script reads CHANGELOG.md beside its own scripts/ directory.
mkdir -p "$work/repo/scripts"
cp "$script" "$work/repo/scripts/extract-changelog.sh"
cp "$sample" "$work/repo/CHANGELOG.md"
"$work/repo/scripts/extract-changelog.sh" 1.0.0 > "$work/out" || fail "default CHANGELOG.md path failed"
cmp -s "$work/out" "$work/expect/1.0.0" || fail "default CHANGELOG.md path printed the wrong section"
echo "ok: default CHANGELOG.md path"

echo "extract-changelog_test: PASS"
