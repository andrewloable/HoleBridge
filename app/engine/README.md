# App engine

HoleBridge's app role: the engine the Flutter app runs in a Bare worklet, started with
`BareWorklet.start(bundlePath:)` from flutter_pear_bare. The engine code is ours. Its Holepunch
dependencies are pinned exactly in `package.json` and `package-lock.json`.

## Run the tests

```sh
npm ci
npm run check-pins   # fails on any dependency that is not an exact x.y.z version
npm test             # runs test/all.js under the Bare runtime
```

Add each new `*.test.js` file to `test/all.js`.

## Build the bundle

```sh
npm run bundle
```

This writes `dist/engine.bundle` from `index.js`. `dist/` is git-ignored.

## Stdout stays clean

`index.js` sends `console.log`, `console.info` and `console.debug` to stderr before anything else
loads. On desktop, stdout carries the IPC frames, so one stray log line would corrupt them. Until the
IPC task lands, `index.js` echoes every frame it gets from `BareKit.IPC` back to the host. Desktop has
no `BareKit`, so there the echo is off.

## Native addons on mobile

The engine depends on native addons (`udx-native`, `sodium-universal`, `bare-tcp`). How their
prebuilds are packaged for iOS and Android is pending the worklet spike (bead HoleBridge-3jo.2). Until
that finding lands, the bundle builds but it is not yet proven on a device.
