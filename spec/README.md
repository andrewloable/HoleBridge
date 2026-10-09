# spec

Shared truth for the Go host and the Flutter app with its engine. Both implementations test against
the files here, so a change in this directory is a protocol change and must land with both sides.

**`vectors/*.json`** is shared test data: key derivation, links, handoff, LAN probe, golden frames
and IPC messages. The Go, Dart and engine test suites read these files. Never edit them by hand unless the file itself says
so. Regenerate them with the matching script in `gen/`. Once a vector is committed, its contents are
frozen like the formats they test. `vectors/.gitkeep` only keeps the directory in git.

**`errors.json`** is the error catalog: one entry per `HB-` code with its problem, cause and fix.
The docs page of error codes is generated from it, and CI fails on any code that is missing here.
Adding a code means adding its entry and regenerating (`go generate ./internal/errs`).

**`ipc.md`** describes the messages between the Flutter app (Dart) and the engine running in the
Bare worklet: framing, message kinds and their fields. Its golden messages live in
`vectors/ipc.json`.

**`gen/`** holds the generators. Each script rebuilds one vector file from the Holepunch reference
libraries, pinned to the same versions the app engine uses. To regenerate, run
`cd spec/gen && npm ci && node <name>.js`. The output is deterministic, so a second run leaves the
files unchanged. `check-versions.js` fails when a pinned version here differs from
`app/engine/package-lock.json`. Run it after `npm ci`.
