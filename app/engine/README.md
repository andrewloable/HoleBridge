# App engine

HoleBridge's app role: the engine the Flutter app runs in a Bare worklet, started with
`BareWorklet.start(bundlePath:)` from flutter_pear_bare. The engine code is ours. Its Holepunch
dependencies are pinned exactly in `package.json` and `package-lock.json`.

## Run the tests

```sh
npm ci
npm run check-pins   # fails on any dependency that is not an exact x.y.z version
npm test             # runs test/all.js under the Bare runtime, then test/desktop-stdio.js under node
```

Add each new `*.test.js` file to `test/all.js`.

## Build the bundle

```sh
npm run bundle
```

This writes `dist/engine.bundle` from `index.js`, and the native addons it loads under
`dist/node_modules`, because `bare-pack --offload-addons` puts them on disk. `dist/` is git-ignored.
`tool/copy_engine_bundle.sh` runs this and copies the bundle and the addons into the Flutter assets.

## Stdout stays clean

`index.js` sends `console.log`, `console.info` and `console.debug` to stderr before anything else
loads. On desktop, stdout carries the IPC frames, so one stray log line would corrupt them.

Frames cross the channel as a 4-byte big-endian length and then the frame bytes. On mobile the
channel is `BareKit.IPC`. Desktop has no `BareKit`, so `index.js` uses this process's own stdin and
stdout through `bare-pipe`. `test/desktop-stdio.js` runs the engine that way and checks the echo.
Until the IPC task replaces it, `index.js` echoes every frame back to the host.

## Native addons on mobile

The bundle loads native addons: today only `bare-pipe`, since `index.js` requires nothing else. The
engine modules will add `udx-native`, `sodium-native` (through `sodium-universal`) and `bare-tcp`.
On desktop the bundle loads each addon from disk, next to itself. The copy script puts each addon in
`app/assets/engine_addons/`, and `EngineHost` writes it back beside the bundle copy. The copies are
for the build host only, so this works for macOS arm64 (checked by the desktop smoke test). How
addons are packaged for iOS and Android is pending the worklet spike (bead HoleBridge-3jo.2) and the
owner decision in HoleBridge-3y9.16. Until then the bundle is not proven on a device.
