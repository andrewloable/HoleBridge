# Working on a task

How any contributor, human or AI, picks up a beads task and finishes it. Every task in beads assumes
you have read this page; it does not repeat it.

## Pick and claim

```sh
bd ready --label ai-ready          # tasks an agent can do on one machine
bd show <id>                       # read the whole task: description, design, acceptance
bd update <id> --claim
```

- Tasks labelled **`owner`** need the project owner: a decision, a real device, a store account or
  a real network. Do not pick them.
- Tasks labelled **`design`** need a design written and approved by the owner before any code.
- Tasks labelled **`emulator`** also need an Android emulator (or the iOS simulator) on this
  machine. If you cannot start one, do everything else, then write what is left in the task notes
  and leave it open.
- Each task's **TEST COMMAND** runs only that task's tests. Its ACCEPTANCE also lists the full
  checks to run before closing.
- **Containers** (type `feature` or `epic`) group tasks. Never implement a container; work on its
  children.
- Read every file and doc section the task lists under **Read first** before writing code.

## Tools you need

Check what the task's language needs before starting. If a tool is missing, install it (Homebrew on
macOS, the distribution's package manager on Linux) or note in the task that it is missing and stop.

| For | Tool | Check |
|---|---|---|
| Go | the Go version in `go.mod` | `go version` |
| Engine, spec generators, scripts | Node.js LTS and npm | `node --version` |
| Engine tests | `bare`, installed by `npm ci` in `app/engine` | `cd app/engine && npx bare --version` |
| App | Flutter, the version in `app/.flutter-version` | `flutter --version` |
| Android | Android SDK, NDK and an emulator image (Android TV image for TV tasks) | `flutter doctor` |
| iOS, macOS | Xcode with the iOS simulator | `xcodebuild -version` |
| CI files | `actionlint` | `actionlint --version` |
| Shell scripts | `shellcheck` | `shellcheck --version` |
| Packaging | Docker | `docker version` |

## Test-driven development

Code is built in pairs of tasks. The pair shares a unit name, e.g. `[go/keys] TEST: normalization`
and `[go/keys] IMPL: normalization`. The IMPL task depends on its TEST task.

**A TEST task (label `tdd-test`):**
1. Create the test file(s) named in the task, covering every case the task lists.
2. Create the code file(s) with the exact names and signatures in the task's design, each body
   only returning "not implemented":
   - Go: `return ..., errors.ErrUnsupported` (or `panic("not implemented")` where nothing can be
     returned);
   - JavaScript: `throw new Error('not implemented')`;
   - Dart: `throw UnimplementedError();`
   - Kotlin: `TODO()`; Swift: `fatalError("not implemented")`.
3. Run the test command. It must **compile and run**, and the new tests must **fail** because the
   code is not implemented, not because of a mistake in the test.
4. Write no implementation logic.

**An IMPL task (label `tdd-impl`):**
1. Make the paired TEST task's tests pass.
2. Never delete, skip or weaken an assertion from the TEST task. You may add tests.
3. If a test looks wrong, stop: write why in the task notes (`bd update <id> --notes "..."`) and
   leave the task open.

Spikes (type `spike`) are throwaway experiments that answer a question with measurements; they are
not test-driven.

## Code layout

| Path | What lives there | Language |
|---|---|---|
| `go.mod` | module `github.com/andrewloable/HoleBridge` | Go |
| `cmd/holebridge/` | the `holebridge` binary's `main` | Go |
| `pears/` | pears-go, the Holepunch stack: `compact/`, `dhtrpc/`, `noise/`, `secretstream/`, `udx/`, `hyperdht/`, `protomux/`, `blindrelay/` | Go |
| `internal/` | host and relay: `keys/`, `protocol/`, `mux/`, `config/`, `host/`, `lan/`, `udpflow/`, `kinds/`, `relay/`, `cli/`, `qr/`, `log/`, `errs/`, `status/`, `testvec/` | Go |
| `app/` | the Flutter app (`lib/`, `test/`, `integration_test/`, `android/`, `ios/`, `macos/`, `windows/`, `linux/`) | Dart, Kotlin, Swift |
| `app/engine/` | the app engine bundle: `index.js`, `lib/`, `test/` | JavaScript (Bare) |
| `app/native/` | the VPN mode network stack and its glue | C |
| `spec/` | shared truth for both implementations: `vectors/*.json`, `errors.json`, `ipc.md`, `gen/` (scripts that generate vectors from the JS reference libraries) | JSON, Markdown, JS |
| `site/` | the static key-link page | HTML |
| `packaging/` | install script, Dockerfile, service units, compose examples | shell, YAML |
| `interop/` | Go↔JS interop tests and their JS peers | Go, JS |

## Commands

| What | Command (run from the repo root) |
|---|---|
| Go tests | `CGO_ENABLED=0 go test ./...` |
| Go checks | `go vet ./...` and `scripts/check-go-deps.sh` |
| Engine tests | `cd app/engine && npm test` |
| Engine bundle | `cd app/engine && npm run bundle` |
| App tests | `cd app && flutter test` |
| App checks | `cd app && flutter analyze` |
| Vector generators | `cd spec/gen && npm ci && node <script>.js` |
| Interop tests | `CGO_ENABLED=0 go test ./interop/...` (needs `npm ci` in `interop/js`) |
| Docs links and anchors | `node scripts/check-docs.js` |

A task's **Test command** line says which of these proves it.

## Rules every task follows

- **Pure Go:** no cgo, no new Go modules beyond `golang.org/x/crypto`,
  `filippo.io/edwards25519` and `golang.org/x/sys` (decisions D32).
- **Exact pins:** npm dependencies without `^` or `~`, committed `package-lock.json`; pub
  dependencies without `^`, committed `pubspec.lock`.
- **Porting from Holepunch libraries is allowed** (MIT and Apache-2.0); keep the upstream copyright
  line in the file header and add the library to `THIRD_PARTY_NOTICES.md`. Never read or copy
  Holesail code (AGPL-3.0).
- **Never log** keys, application keys, derived secrets, tokens, PINs, payload bytes or forwarded
  DNS queries. In Go, wrap them in `internal/log.Secret`.
- **Errors** use codes from `spec/errors.json`. Adding a code means adding its entry there (problem,
  cause, fix) and regenerating (`go generate ./internal/errs`).
- **Docs move with behaviour:** if the task changes what a doc says, update the doc in the same
  task.
- **Never `git commit` or `git push`.** Leave changes in the working tree; the owner commits.

## Done

1. Every acceptance criterion holds; run each check it names.
2. The repo's full test command for the languages you touched passes.
3. `bd close <id> --reason "<one line: what was done>"`.
4. Anything you noticed but did not do becomes a new task:
   `bd create --title "..." --description "..." --parent <same parent>`.
