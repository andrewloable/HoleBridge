#!/bin/sh
# Collects the license texts of everything the Flutter app ships into app/assets/licenses/, which
# Flutter bundles as the licenses/ asset directory. The directory is git-ignored and rebuilt on every
# run. Called at the end of tool/copy_engine_bundle.sh; it needs the engine's node_modules (npm ci).
#
# Usage: tool/collect_app_licenses.sh
#
# Sources. Every one must exist; when one is missing the script exits 1 and leaves the previous
# output in place:
#   app/licenses/                 texts vendored from upstream at the pinned versions (committed):
#                                 Bare Kit, the bare runtime, the pear-end texts of flutter_pear, the
#                                 13 native addons and the Bare builtins (npm), and the C and C++
#                                 libraries statically linked into those binaries
#   app/engine/node_modules/      LICENSE and NOTICE of every non-dev package in
#                                 app/engine/package-lock.json
#   app/native/netstack/          LICENSE files of the Android VPN network stack (libholebridge_tun.so)
#   packaging/licenses/           the noise-curve-ed text, for the one engine package whose npm
#                                 tarball ships no license file
#
# app/licenses/ is also checked against THIRD_PARTY_NOTICES.md (section 1b): every directory that a
# notices table lists must exist, and every directory under native-addons, bare-builtins and static
# must have a row. A missing or unlisted directory exits 1.
#
# Each text gets a flat name: its path below the source root, with "/" as "__" and "@" as "-", for
# example static__v8-14.8.178.31__LICENSE, native-addons__bare-fs-4.8.1__LICENSE or
# engine__bare-fs-4.8.3__LICENSE.
set -eu

app_dir=$(cd "$(dirname "$0")/.." && pwd)
repo_dir=$(cd "$app_dir/.." && pwd)
out="$app_dir/assets/licenses"
tmp="$app_dir/assets/.licenses.tmp"

rm -rf "$tmp"
mkdir -p "$tmp"
list=$(mktemp)
trap 'rm -rf "$tmp" "$list"' EXIT

fail() {
  echo "collect_app_licenses: $*" >&2
  exit 1
}

# flat <relative path>: the flat name of a text, from its path below its source root.
flat() {
  printf '%s' "$1" | sed -e 's#@#-#g' -e 's#/#__#g'
}

# put <source file> <flat name>: copies one non-empty text into the staging directory.
put() {
  [ -s "$1" ] || fail "missing or empty: $1"
  [ ! -e "$tmp/$2" ] || fail "two texts would be named $2"
  cp "$1" "$tmp/$2"
}

# 1. Vendored texts. The release-critical ones are required by name; every other file is copied.
[ -d "$app_dir/licenses" ] || fail "missing $app_dir/licenses"
for f in bare-kit-2.5.5/LICENSE bare-kit-2.5.5/NOTICE \
  bare-runtime-1.30.3/LICENSE bare-runtime-1.30.3/NOTICE \
  flutter_pear-0.4.9/THIRD_PARTY_LICENSES flutter_pear-0.4.9/NOTICE; do
  [ -s "$app_dir/licenses/$f" ] || fail "missing vendored text: licenses/$f"
done
for d in "$app_dir"/licenses/native-addons/*/ "$app_dir"/licenses/bare-builtins/*/; do
  [ -s "$d/LICENSE" ] || fail "no LICENSE in $d"
done
for d in "$app_dir"/licenses/static/*/; do
  [ -n "$(ls -A "$d")" ] || fail "no texts in $d"
done
for f in $(find "$app_dir/licenses" -type f | sort); do
  rel=${f#"$app_dir/licenses/"}
  put "$f" "$(flat "$rel")"
done

# 1b. The vendored directories must agree with THIRD_PARTY_NOTICES.md. Each row of its three
#     vendored tables names a directory: native-addons/<package>@<version>,
#     bare-builtins/<package>@<version> or static/<last column>. The node program below checks that
#     each named directory exists, and that each directory under the three is named by a row. The
#     SECTIONS headings in it must stay in step with the table headings of THIRD_PARTY_NOTICES.md.
#     Node failing (a missing or unreadable file) also exits 1.
notices="$repo_dir/THIRD_PARTY_NOTICES.md"
[ -s "$notices" ] || fail "missing $notices"
problems=$(node -e '
  const fs = require("fs");
  const path = require("path");
  const [repo, licenses] = process.argv.slice(1);
  const lines = fs.readFileSync(path.join(repo, "THIRD_PARTY_NOTICES.md"), "utf8").split("\n");
  // Table headings of THIRD_PARTY_NOTICES.md: keep in step with that file.
  const SECTIONS = [
    { heading: "## Native addons (flutter_pear_bare)",
      dir: (c) => "native-addons/" + c[0] + "@" + c[1] },
    { heading: "## Bare builtins (npm, embedded in Bare Kit and the bare runtime)",
      dir: (c) => "bare-builtins/" + c[0] + "@" + c[1] },
    { heading: "## Statically linked libraries (Bare Kit, the bare runtime, the native addons)",
      dir: (c) => "static/" + c[c.length - 1] },
  ];
  const isSep = (l) => /^\|[-:| ]+\|$/.test(l);
  const problems = [];
  const expected = new Set();
  for (const s of SECTIONS) {
    const start = lines.indexOf(s.heading);
    if (start < 0) { problems.push("no heading in THIRD_PARTY_NOTICES.md: " + s.heading); continue; }
    let rows = 0;
    for (let i = start + 1; i < lines.length && !lines[i].startsWith("## "); i++) {
      const line = lines[i];
      // Skip the separator row, and the header row that sits just above it.
      if (!line.startsWith("|") || isSep(line) || isSep(lines[i + 1] || "")) continue;
      expected.add(s.dir(line.split("|").slice(1, -1).map((c) => c.trim())));
      rows++;
    }
    if (rows === 0) problems.push("no table rows under: " + s.heading);
  }
  for (const dir of expected) {
    const p = path.join(licenses, dir);
    if (!fs.existsSync(p) || !fs.statSync(p).isDirectory()) problems.push("listed but missing: " + dir);
  }
  for (const sub of ["native-addons", "bare-builtins", "static"]) {
    for (const name of fs.readdirSync(path.join(licenses, sub))) {
      const dir = sub + "/" + name;
      if (fs.statSync(path.join(licenses, dir)).isDirectory() && !expected.has(dir)) {
        problems.push("in app/licenses but no row names it: " + dir);
      }
    }
  }
  if (problems.length) console.log(problems.join("\n"));
' "$repo_dir" "$app_dir/licenses") || fail "cannot check app/licenses against $notices (node failed or is not on PATH)"
if [ -n "$problems" ]; then
  printf '%s\n' "$problems" >&2
  fail "app/licenses does not match THIRD_PARTY_NOTICES.md (see above)"
fi

# 2. Engine npm packages: the non-dev entries of the lockfile, with the texts their installed
#    packages ship.
engine="$app_dir/engine"
[ -d "$engine/node_modules" ] || fail "no $engine/node_modules; run npm ci in $engine"
# The list goes to a file, not into a pipe: the status of a pipeline is that of its last command, so
# a failing node (a missing or corrupt lockfile, or no node on PATH) would leave the loop with no
# input and the engine texts silently dropped.
node -e '
  const lock = require(process.argv[1] + "/package-lock.json").packages;
  for (const [key, m] of Object.entries(lock)) {
    if (!key || m.dev) continue;
    console.log(key, key.replace(/^.*node_modules\//, ""), m.version);
  }' "$engine" > "$list" || fail "cannot read $engine/package-lock.json (node failed or is not on PATH)"
[ -s "$list" ] || fail "no packages in $engine/package-lock.json"
while read -r key name version; do
  dir="$engine/$key"
  pkg=$(printf '%s' "$name" | sed -e 's#^@##' -e 's#/#+#g')-$version
  found=0
  for f in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/NOTICE* "$dir"/COPYING*; do
    [ -f "$f" ] || continue
    put "$f" "engine__${pkg}__$(basename "$f")"
    found=1
  done
  if [ "$found" -eq 0 ]; then
    case "$name@$version" in
      noise-curve-ed@2.1.0) put "$repo_dir/packaging/licenses/noise-curve-ed/LICENSE" "engine__${pkg}__LICENSE" ;;
      *) fail "no LICENSE, NOTICE or COPYING in $dir" ;;
    esac
  fi
done < "$list"

# 3. The Android VPN network stack: every LICENSE file under app/native/netstack, by name.
netstack="$repo_dir/app/native/netstack"
for f in LICENSE src/core/LICENSE third-part/lwip/LICENSE third-part/hev-task-system/LICENSE \
  third-part/yaml/License third-part/yaml/LICENSE-libyaml; do
  put "$netstack/$f" "netstack__$(printf '%s' "$f" | sed -e 's#/#__#g')"
done

rm -rf "$out"
mv "$tmp" "$out"
echo "collect_app_licenses: wrote $(find "$out" -type f | wc -l | tr -d ' ') texts to $out"
