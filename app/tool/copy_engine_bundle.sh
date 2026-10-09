#!/bin/sh
# Builds the app engine (app/engine) and copies its bundle into the Flutter assets.
# Needs Node and npm. The copy app/assets/engine.bundle is git-ignored.
set -eu

app_dir=$(cd "$(dirname "$0")/.." && pwd)

cd "$app_dir/engine"
npm ci
npm run bundle

mkdir -p "$app_dir/assets"
cp "$app_dir/engine/dist/engine.bundle" "$app_dir/assets/engine.bundle"
echo "copied engine bundle to app/assets/engine.bundle"
