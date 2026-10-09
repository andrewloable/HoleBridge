#!/bin/sh
# Builds the app engine (app/engine) and copies its bundle and native addons into the Flutter
# assets.
# Needs Node and npm. The copies under app/assets are git-ignored.
set -eu

app_dir=$(cd "$(dirname "$0")/.." && pwd)
addons="$app_dir/assets/engine_addons"

cd "$app_dir/engine"
npm ci
rm -rf dist # so no addon from an earlier build is copied
npm run bundle

mkdir -p "$app_dir/assets"
cp "$app_dir/engine/dist/engine.bundle" "$app_dir/assets/engine.bundle"

# bare-pack --offload-addons writes each native addon under dist/node_modules, at the path the
# bundle loads it from. The app copies each addon to engine_addons under its base name, and
# addons.txt lists that relative path, so EngineHost can put every addon back where the bundle
# expects it.
rm -rf "$addons"
mkdir -p "$addons"
: > "$addons/addons.txt"
for rel in $(cd "$app_dir/engine/dist" && find node_modules -name '*.bare' -type f); do
  name=$(basename "$rel")
  if [ -e "$addons/$name" ]; then
    echo "two addons are named $name; the copy would overwrite one" >&2
    exit 1
  fi
  cp "$app_dir/engine/dist/$rel" "$addons/$name"
  echo "$rel" >> "$addons/addons.txt"
done
count=$(wc -l < "$addons/addons.txt" | tr -d ' ')
echo "copied engine bundle and $count native addon(s) to app/assets"

# The license texts of the engine packages, the native addons and the Bare runtime (app/assets/licenses).
"$app_dir/tool/collect_app_licenses.sh"
