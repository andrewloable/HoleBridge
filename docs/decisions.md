# Decisions

Each entry: what was decided, why, and what was turned down. **Decided** entries change only with
a new entry that supersedes them. **Proposed** entries stand until the owner confirms or the named
milestone settles them.

## D1. Go host, Flutter app, Holepunch engine in a Bare worklet — Decided, superseded in part by D31 and D34

**Superseded for the host and relay by D31:** they no longer run a `bare` worklet; they use
pears-go, a pure-Go Pear library in this repo (D32). **Refined for the app by D34:** the app runs
HoleBridge's own JS engine in a Bare worklet through flutter_pear_bare, not flutter_pear's pear-end.
What stands: a Go host, a Flutter app, and the Holepunch stack in JavaScript on the app. The
original text follows.

The host (and the relay) is a Go program; the client is a Flutter app for TVs, phones and desktops.
Both run the peer-to-peer engine, JavaScript on the Holepunch stack, in a Bare worklet from
flutter_pear's pear-end: in-process on mobile through Bare Kit, and as a `bare` child process on
desktops and on the Go host.

Holepunch's stack (`hyperdht`, `udx-native`, `@hyperswarm/secret-stream`, `protomux`) exists in
JavaScript only. Go and Dart therefore drive the engine instead of reimplementing it.

Turned down: a pure Go reimplementation of HyperDHT, UDX and the Noise handshake (a project of its
own, tracking upstream forever); a Node.js host (Go is the owner's choice: one binary, service
management, cross-compiling).

## D2. Dial the host's public key with HyperDHT, not Hyperswarm topics — Decided

The app derives the host's public key and calls `dht.connect` on it, with a key-derived client key
pair the host's firewall checks. Compared with topics, this authenticates the host in the Noise
handshake itself, has no announcement race, and keeps one stable identity across restarts.
[architecture.md](architecture.md#why-the-app-dials-a-public-key-not-a-hyperswarm-topic) has the
detail.

Cost: none upstream since D34. The app engine calls `hyperdht` directly, and pears-go implements
the same on the host.

## D3. The key: 9 symbols of Crockford Base32, Argon2id derivation — Decided (parameters Proposed until M1)

Nine symbols is the owner's requirement. Crockford Base32 gives the most bits per symbol (45 in
all) while staying case-insensitive and free of look-alikes, which matters on a TV remote. All
nine symbols are entropy; there is no check symbol. The full specification and the guessing-cost
analysis are in [security.md](security.md).

Turned down: digits only (30 bits in 9 symbols); base58 (case-sensitive, hard to read aloud and to
type on a TV); 8 symbols plus a check symbol (gives up 5 bits to catch typos, which a failed lookup
reports anyway).

## D4. One session per app and host; streams multiplexed with credit flow control — Decided

Every TCP connection rides one Noise session as a stream on a Protomux channel, paced by
per-stream credit stated by each receiver. UDP flows share the same session (D33).

Turned down: one DHT connection per TCP connection. It is simpler, but every new connection pays a
DHT lookup and handshake, often seconds, and a browser opens dozens.

Accepted cost: one ordered transport, so packet loss delays all streams together (as HTTP/2).

## D5. The data path stays inside the worklet — Proposed (M1), app only since D31

Since D31 this applies to the app only; the Go host moves bytes in-process.

On the app, the worklet owns the local sockets; Dart only sends commands and receives events. One
app-side implementation of protocol and multiplexer, no IPC hop for the data, real local
backpressure.
[architecture.md](architecture.md#why-the-apps-data-path-stays-inside-the-worklet-proposed).

**Pass rule (eng D8).** On the slowest supported Android TV box, one LAN-route stream must sustain at
least 80 Mbit/s, and four concurrent streams at least 80 Mbit/s combined, with the engine using at
most one CPU core.

**Fallback.** It applies on a platform that misses that bar, or where the worklet cannot open
loopback listeners: the worklet carries session messages, and Dart multiplexes and owns the
sockets.

## D6. Three routes: LAN, direct, relay — Decided

- **LAN**, found by a signed unicast UDP probe, then Noise over TCP. Same-network peers often cannot
  reach each other through the DHT (router hairpinning), and that is HoleBridge's most common case.
- **Direct**, hole-punched by HyperDHT.
- **Relay**, an owner-run blind relay, for when both ends are behind randomizing NATs, where
  HyperDHT does not even attempt a punch.

Every route carries the same authenticated session and protocol. The app prefers LAN, then
HyperDHT, which falls back to the relay by itself. It re-evaluates on network changes.
[architecture.md](architecture.md#routes).

Turned down: broadcast or mDNS discovery (dropped by many Android Wi-Fi drivers without a
multicast lock; sending broadcast on iOS needs a special entitlement); reading the Wi-Fi SSID to
detect "home" (needs location permission on Android).

## D7. Relays are owner-run, and their code ships here — Decided

`holebridge relay` is the same Go binary in relay mode. A relay serves only members holding its
relay key. HoleBridge ships no relay address and no default relay key: a user without a relay
connects exactly as before, over LAN or direct routes.

## D8. The host forwards only to configured targets — Decided

An app names a service; only the host's configuration maps names to addresses. HoleBridge must
never be an open proxy into the host's network.

## D9. App listeners bind 127.0.0.1 by default — Decided

Exposing a host's services to the app device's whole network must be a per-host choice, never a
default. The TV's handoff listener (D36) is the one app listener on the LAN: it carries no service,
lives at most 5 minutes and accepts only a box sealed to it. In VPN mode (D37) the service addresses
exist only on the device's own VPN interface.

## D10. Host configuration is JSON — Decided

Go reads it with the standard library, and the CLI writes it. Turned down: YAML and TOML, a
dependency each, for comments.

## D11. MIT license, clean room from Holesail — Decided

Holesail is AGPL-3.0. HoleBridge may read its documentation and must never copy its code.

## D12. Exact pins — Decided, refined by D31, D32 and D34

Originally flutter_pear in the app and its bundle by sha256 in the host. Now:
- **App:** flutter_pear_bare and the app engine's Holepunch dependencies, pinned exactly in its
  lockfile (D34).
- **Host and relay:** the Go module's dependencies in `go.sum`. pears-go lives in the same module
  (D31, D32), so it moves with the host.

The key derivation depends on `sodium-universal` (JS) and pears-go's Go crypto producing the same
output forever, and pears-go must match the hyperdht release the app engine pins. Versions move
together and deliberately.

## D13. Many services per key, by name; scoped keys as extra DHT servers — Decided

One host serves many named services under one key (the owner's main requirement). M4's scoped keys
are further DHT servers in the same engine and on the same DHT node, each admitting its own client
key and serving its own subset of services.

## D14. Idle timeout per service, off by default — Decided

A fixed idle timeout suits HTTP, but a generic tunnel carries SSH and databases, whose connections
sit idle for hours.

## D15. Tests at each layer, no extra frameworks — Decided

Updated for D31 and D34.

- **App engine (JS):** its own tests in `app/engine/`, end to end on `hyperdht/testnet` (a DHT on
  localhost), so tests need no internet and no second machine.
- **pears-go and the Go host:** `go test`, with pears-go checked against libsodium and Holepunch
  wire vectors.
- **Interop:** Go↔JS tests in CI, the Go host against the JS app engine on a local testnet, plus the
  shared vectors in `spec/vectors/`.
- **App:** `flutter test`, with a fake of the engine IPC for unit tests.
- **Real networks:** a written checklist run by hand (LAN, direct, relay, mobile data, TV).

## D16. The first usable release covers every route and every network switch — Decided

The MVP includes the LAN, direct and relay routes, `holebridge relay`, and stream resume across
network and route changes. "Just works everywhere" is the product (office-hours approach A,
[design](designs/p2p-tunnel-mvp.md)).

Turned down:
- **A browser proxy giving each service its own origin, with the engine in an iOS Network Extension
  (approach B).** Too unproven for the first release.
- **Per-service virtual addresses through the OS VPN interface (approach C).** It would start
  turning a tunnel into a network (D30). It also takes the device's single VPN slot and needs a Go
  network stack plus Bare in one process. (Adopted later for phones and TVs by D37, with a C network
  stack and the tunnel rules of D30 kept.)

## D17. Sessions open on demand — Decided

HoleBridge is a tunnel to services (D30), so a connection exists only while it is used. A session
opens when a service is used and closes 5 minutes after its last stream closes, so nothing runs over
the network while idle.

On Android and iOS, VPN mode (D37) keeps the services reachable while the app is closed; a session
still opens only when a service is used.
[architecture.md](architecture.md#sessions-and-reconnects) has the per-platform table.

## D18. The in-app browser is the default for web services — Decided

It keeps storage separate per service, pins self-signed certificates on first use, and maps links to
the service's own origins. Each platform falls back by M1 spike result:
- a shared store with a warning;
- the system browser, on Linux desktop.

Its known limits (name-based virtual hosts, OAuth redirect URIs, `Secure` cookies on `http`) are
listed, not fixed, in the MVP. [architecture.md](architecture.md#the-in-app-browser).

## D19. `origins` is the only target detail ever sent, and only by opt-in — Decided

This narrows "the target address is never sent". A host owner may list a web service's own
origins; only those are sent, only for web kinds, only to key holders. Nothing is derived from the
target automatically.

## D20. The host classifies services; the app never probes — Decided

Explicit `--kind`, or detection by the Go host with stated timeouts, where an inconclusive result
never downgrades a known web kind. Detected kinds persist across restarts.
[architecture.md](architecture.md#service-kinds).

## D21. QR code and https link, key after the `#` — Decided

`holebridge host`, `holebridge key` and `holebridge share` print the key, a terminal QR code and
`https://holebridge.app/k#KEY.APPKEY`, which carries both halves of the key (D35). The fragment
never reaches the web server. The link opens the app, or a static page that sends you to the store.
Typing the 9 symbols works once the app holds the application key; a TV gets the host from a
phone instead (D36).

Turned down: a `holebridge://` link only, which does nothing on a phone without the app.

## D22. Install: one script, or Docker — Decided

`curl … | sh` installs the binary, checksum-verified (since D31 it needs no `bare` or bundle); a
Docker image runs the host with a restart policy. Both create the deployment's `app.key` (D35).

Turned down for the MVP: Homebrew, apt and winget packages, which mean three pipelines before the
first release.

## D23. Connect order: LAN first, then the DHT — Decided

The owner kept the sequential order: a brief LAN probe, then the DHT lookup. A connect away from home
pays about 1.5 s more.

Turned down: starting the derivation on the ninth character and racing the LAN probe against the
DHT.

## D24. Android TV: a cursor in the browser, and a native-app hint — Decided

The TV browser gets a D-pad cursor mode. Web service tiles also offer **Use the native app**, which
copies the address for the service's own TV app.

## D25. Errors have codes and docs; the app has a diagnostics screen — Decided

- Every app and host error states the problem, the cause and the fix, and carries a code
  (`HB-LOOKUP-TIMEOUT`) linking to `docs/errors.md`.
- The app's diagnostics screen copies, or files as a GitHub issue, the route, NAT type, relay status,
  last error and versions, with keys, addresses and service names redacted.

## D26. No compatibility promise before 1.0 — Decided

An app and a host speaking different protocol versions may not work together. The mismatch error
names which side to update and how (the store or the install script).

Turned down: a v1 promise (any v1 app with any v1 host, v2 kept beside v1 for 12 months).

## D27. Start at boot: Docker's restart policy now, service units in M6 — Decided

Bare-metal hosts do not start at boot until M6's systemd, launchd and Windows units.

Turned down: `holebridge autostart` in the MVP.

## D28. Key decisions are taken before the derivation freezes — Decided

Q2 (key length; the extra secret became the application key in D35) is decided at M1 exit, before
the derivation and test vector freeze. Whether scoped keys join the MVP is decided before the M2
build. The M2 release gate only checks that both are recorded here.

## D29. Engine development loop: a flutter_pear branch with local overrides — Superseded by D34

Since D34 the app engine lives in this repo, so there is no flutter_pear branch or override loop:
engine and app change in the same commit.

Since D31 the Go host runs no bundle, so `--dev-bundle` was already gone. The original text
follows.

While M2 is built, engine work happens on a flutter_pear branch:
- the app points `dependency_overrides` at a local checkout;
- (until D31) the Go host accepted `--dev-bundle <path>`, refused in release builds. Dropped: the
  host now runs no bundle.

flutter_pear releases are published at milestone ends, and release builds keep the exact pins of
D12.

Turned down: publishing a flutter_pear release for every engine change.

## D30. HoleBridge is a peer-to-peer tunnel, not a mesh — Decided

HoleBridge is a peer-to-peer tunnel, like holesail.io. A host shares named services; an app holding
the key connects straight to that host, over the LAN, a hole-punched path or the owner's relay.
- **Not a mesh.** Devices never join a network, never get addresses on one, and never see each
  other. On Android and iOS the app uses the system's VPN mechanism (D37), but only to reach one
  host's services by name.
- **One app, one host.** An app talks to one host per key; hosts never talk to each other.

Turned down: features that need network membership, such as device-to-device reachability, subnet
routing or virtual addresses for every device.

## D31. The host and relay use a Go Pear library; the app keeps flutter_pear — Decided, app part refined by D34

Since D34 the app's engine is HoleBridge's own Bare bundle in this repo, run through
flutter_pear_bare; flutter_pear itself is not modified.

No usable Go implementation of the Holepunch stack exists today. The only Go HyperDHT is from 2021,
targets the pre-UDX protocol, and cannot talk to today's network. A wire-compatible port is
feasible: a D reimplementation interoperates with hyperdht 6.34 across NATs. So HoleBridge builds
one, working name **pears-go**: compact-encoding, dht-rpc, HyperDHT (announce, lookup, server with
firewall, connect, hole punching, `relayThrough`), the Noise handshake and secret-stream, UDX,
Protomux and blind-relay.

- **The Go host and relay** speak the network in-process, with no `bare` subprocess and no IPC.
  That makes the stdout guard, the engine crash policy and `--dev-bundle` unnecessary on the host.
- **The app** keeps a JS engine in Bare (since D34, HoleBridge's own bundle through
  flutter_pear_bare).
- **HoleBridge's protocol is implemented twice, once per role.** Go implements the host role and
  the relay; JS implements the app role. The shared core (key derivation, the protocol codec, the
  multiplexer with flow control and resume) exists in both. It is kept in step by:
  - shared conformance vectors (key, golden frames);
  - Go↔JS interop tests on a local testnet in CI;
  - a cross-NAT check.
- **pears-go stays wire-compatible with the hyperdht 6.x release the app engine's lockfile pins.**
  Interop CI runs against that exact version.

Turned down:
- the Go library everywhere, with the app calling Go through FFI;
- keeping `bare` on the host now and switching later;
- the plan before D31 (the host supervising the JS engine).

## D32. pears-go lives in this repo and is pure Go — Decided

Settles Q11 and Q12.

**Where it lives.** pears-go is implemented in this repository, under `pears/`. It is a set of Go
packages in the same Go module as the host. The layout:
- `go.mod` at the root;
- `pears/` for the library;
- `cmd/holebridge/` for the binary;
- `internal/` for host and relay code;
- `app/` for the Flutter app.

**Pure Go.** No cgo, no C libudx, no libuv, no JavaScript. Every Go build uses `CGO_ENABLED=0`, so
one command cross-compiles for every OS and architecture. Dependencies are the Go standard library,
`golang.org/x/crypto` and `filippo.io/edwards25519` (pure Go), plus `golang.org/x/sys`, which
`x/crypto` itself requires.

| Primitive the Holepunch stack uses | In pears-go |
|---|---|
| ChaCha20-Poly1305 (Noise) | `x/crypto/chacha20poly1305` |
| BLAKE2b, Argon2id | `x/crypto` |
| Ed25519 signatures | `crypto/ed25519` |
| Ed25519 scalar multiplication (HyperDHT's Noise curve) | `filippo.io/edwards25519` |
| libsodium `secretstream_xchacha20poly1305` | implemented in pears-go from `x/crypto`'s HChaCha20, ChaCha20 and Poly1305, checked against libsodium vectors |
| UDX | a pure-Go port of the protocol |

Turned down: cgo with libudx and libuv, and a separate repository for the library.

## D33. TCP and UDP services, both in the MVP — Decided

A host shares TCP and UDP services under one key, in the same session, from the first usable
release. UDP uses flows ([architecture.md](architecture.md#udp-services)):
- **Host side:** one socket per flow, connected to the configured target.
- **App side:** one local UDP port per service.
- **Opening:** the first datagram rides the reliable `flow` message.
- **After that:** datagrams ride unordered UDX messages on the direct and relay routes, and a
  drop-when-full ordered queue on the TCP-based LAN route.
- **Bounds:** no fragmentation (1200-byte default maximum), a 60 s idle timeout, and no resume.

Supersedes "UDP later" in D16 and the roadmap.

## D34. flutter_pear stays a faithful Pear port; HoleBridge's app engine lives here — Decided

flutter_pear is a Flutter port of the Pear (Holepunch) libraries, kept fully compatible with them.
**HoleBridge adds no features to it.** Anything HoleBridge needs that the original Pear libraries
do not have is implemented in this project.

- **The app engine** is HoleBridge's own Bare bundle in `app/engine/` (JavaScript). It is built
  from the original Holepunch libraries:
  - `hyperdht` for direct dialing with a key pair, plus `relayThrough` and the relay's default key
    pair;
  - `@hyperswarm/secret-stream`, `protomux`, `compact-encoding` and `sodium-universal`;
  - `udx-native` for unordered datagrams and the raw UDP sockets used by LAN probes and UDP
    services;
  - `bare-tcp` for local listeners.

  It holds the app role: the key, the codec, the multiplexer, resume, the LAN probe, route
  selection, sessions, TCP and UDP listeners, and the TV handoff (D36).
- **It runs through flutter_pear_bare's existing custom-worklet API,** `BareWorklet.start(bundlePath:)`,
  with its raw binary frames. HoleBridge defines its own binary IPC on those frames, so payloads
  are never base64. The app does not use flutter_pear's `Pear` class.
- **Both protocol implementations live in this repo:** Go in `pears/` and `internal/`, JS in
  `app/engine/`. They change in one commit, with the shared vectors.
- **iOS background work** (VPN mode's packet tunnel extension, D37) is built in this repo's iOS
  code, embedding Bare Kit directly. It never becomes a flutter_pear feature.

Turned down:
- adding HoleBridge features to pear-end or flutter_pear;
- a generic extension hook in flutter_pear (also a new feature);
- forking pear-end.

## D35. Keys have two halves: the user's 9 symbols and the deployment's application key — Decided

Derivation mixes a 32-byte **application key** into the Argon2id salt and the BLAKE2b role seeds
([security.md](security.md#the-application-key)). It is a **per-deployment secret**, set by
whoever deploys HoleBridge and known only to that deployment. Hosts and apps with different
application keys never find each other, even with the same 9 symbols.

- **Host and relay:** generated at install or first run (or `holebridge app-key --new`), stored as
  `app.key` (mode `0600`) in the config directory, and changeable on deployment with no rebuild.
- **Apps:** a deployer's own client build compiles it in. A store app receives it once at pairing,
  from the host's QR code or link (which carries both halves), or on a TV from a phone (D36), and
  keeps it in secure storage.
- **The repository** holds no real application key, only a fixed test value for the vectors. What
  freezes in M1 is the derivation algorithm, not any deployment's key.
- **What it gives:** without the deployment's application key, an attacker cannot test a single
  key guess. With it, guessing costs 2⁴⁵ Argon2id computations, confined to that one deployment.
- **What it costs:** the QR code and link now carry a secret, and so does any client build with
  the key compiled in. A leak means rotating the application key and re-provisioning every app.

Turned down:
- application keys compiled into clients only (no store apps, and iPhone builds need the deployer's
  own Apple account);
- provisioning at pairing only;
- one public application key for everyone, which adds separation but no strength.

## D36. A TV gets a host, application key included, from a phone over the LAN — Decided

Settles Q13. A TV has no camera to scan the host's QR code, so a phone that already holds the host
hands it over ([architecture.md](architecture.md#adding-a-host-to-a-tv-handoff)):
- **The TV** chooses **Add from phone** and shows a QR code. The code carries a one-time X25519
  public key, a one-time 16-byte secret, and the TV's LAN addresses and port. The TV's engine
  listens on that port for 5 minutes.
- **The phone** scans it, the user picks a host, and the phone's engine connects over the LAN and
  sends one sealed box (`crypto_box_seal`) to the TV's public key. The box carries the one-time
  secret, the host's name, its 9 symbols and the deployment's application key.
- **The TV** accepts only a box that opens and carries the one-time secret, adds the host, and
  closes the listener.

Only the TV can open the box, and only someone who saw the TV's screen knows the secret, so nobody
else on the network can read the key or push a host onto the TV. It lives entirely in the app (the
engine on both ends, plus the Flutter UI): no host change, no change to protocol v1, nothing in
flutter_pear.

Accepted limit: the phone and the TV must be on the same network, with device-to-device traffic
allowed (not a guest network with client isolation). A TV with no such phone needs an app built
with the key compiled in (D35).

Turned down:
- a self-built TV app as the only way in, which leaves store users with none;
- typing the application key on the TV (52 symbols with a remote).

## D37. On Android and iOS the app works like a VPN app; the network stays peer-to-peer — Decided

On phones and TVs the app works the way VPN apps such as Cloudflare WARP do. What carries the
traffic is HoleBridge's own peer-to-peer session to the host (LAN, direct or relay), not a
provider's servers ([architecture.md](architecture.md#vpn-mode-android-and-ios)).
- **One switch.** The user turns on **VPN mode**. Android's `VpnService`, or a packet tunnel
  extension on iOS, then catches traffic for a small private address range, and nothing else.
- **Every service gets an address and a name,** `<service>.<host>.internal`, plus the hostnames in
  its `origins`. The app answers DNS for those names; every other query goes to the network's
  normal DNS.
- **Packets become streams on the device.** A small user-space network stack in C turns packets
  back into TCP connections and UDP flows and hands them to the engine's local listeners. They
  cross the session as ordinary streams and flows, so the host, protocol v1, resume and "forward
  only to configured targets" are unchanged.
- **Still a tunnel, not a mesh (D30).** Only the host's configured services get addresses. No
  device gets an address anyone else can reach, nothing is routed to a subnet, and nothing can
  reach the phone.
- **Android, in the MVP:** VPN mode replaces Keep running; the `VpnService` is the foreground
  service.
- **iOS, in M5:** the extension is Swift, embedding Bare Kit and the same engine (D34). Keys reach
  it through a keychain group shared with the app. While VPN mode is on, only the extension holds
  sessions, and the app shows its status.
- **Flutter** draws the screens and the switch; the VPN pieces are native (Kotlin, Swift) plus the
  engine.
- **Without VPN mode** (switched off, approval refused, or on desktop) the app works as before:
  127.0.0.1 listeners while it runs, and the in-app browser.

Costs accepted:
- it takes the device's one VPN slot, so it cannot run beside another VPN app, WARP included; the
  VPN icon shows, and the user approves it once;
- Google Play's `VpnService` policy needs a declaration (Q9);
- Apple publishes VPN apps only from developers enrolled as an organization (App Review Guideline
  5.4, Q14);
- on iOS the engine and the stack must fit the extension's memory limit (about 50 MB), proven in
  M1.

This adopts D16's approach C for phones and TVs. D16's objection that it needs "a Go network stack
plus Bare in one process" does not apply: the stack is C.

Turned down:
- **sending raw IP packets to the host,** as WARP does to Cloudflare: it needs a new message type,
  runs TCP inside the LAN route's TCP, loses byte-exact resume, and a Go network stack would break
  the dependency rule (D32);
- **VPN mode on Android after the MVP,** which builds Keep running only to replace it.

## Open questions

| # | Question | Recommendation | Settled by |
|---|---|---|---|
| Q1 | Argon2id cost: which is the slowest device HoleBridge must run on? | Benchmark on the slowest Android TV box we support; highest cost under ~1 s there | M1, owner names the devices |
| Q2 | Is a 45-bit key acceptable as the default, given its guessing cost for anyone who has the deployment's application key ([security.md](security.md#how-hard-is-a-key-to-guess))? | Yes, with optional 18-symbol strong keys and a PIN in M4 | Owner, at M1 exit (D28) |
| Q3 | Does iOS VPN mode (D37) join the MVP instead of waiting for M5? | It does if the M1 extension spike passes *and* the owner's service list shows a native iPhone app used daily; otherwise M5 | M1 spike + owner |
| Q4 | Apple TV, and Android TVs older than Android 10 (including Fire OS 7)? | Not planned: no official Flutter tvOS support; flutter_pear_bare needs Android 10+ | Owner |
| Q5 | A headless client in the Go binary (`holebridge connect`) for servers and scripts? | pears-go already has the transport, but the app role would need a Go implementation too; add when asked | Owner |
| Q6 | Several relay servers sharing one relay key, for redundancy? | Test with the relay work | M2 |
| Q7 | Should the project run a public relay? | No: cost, abuse and a party in the middle. Owner-run relays only. | Owner |
| Q8 | Which domain hosts the install script, the key link page and the docs? `holebridge.app` is a placeholder. | Register one short domain for all three; static hosting only | Owner, before M2 |
| Q9 | Will Play accept HoleBridge under its `VpnService` policy (D37)? | Declare it early; if refused, ship VPN mode in the GitHub Releases APK only | M2 |
| Q10 | Do scoped keys join the MVP as its minimum access-control floor? | Decide with Q2 | Owner, before the M2 build (D28) |
| Q11 | Where does pears-go live? | **Settled (D32):** in this repo | Owner, 2026-10-08 |
| Q12 | UDX in pure Go or through cgo? | **Settled (D32):** pure Go, no cgo | Owner, 2026-10-08 |
| Q13 | How does a store app on a TV (no camera) get the application key? | **Settled (D36):** a handoff from a phone that already holds the host, over the LAN | Owner, 2026-10-08 |
| Q14 | Is the Apple developer account enrolled as an organization? Apple publishes VPN apps only from organizations (App Review Guideline 5.4, D37). | Check before the M1 iOS spike. With an individual account, enroll an organization (needs a legal entity and a D-U-N-S number) or ship iOS without VPN mode | Owner, before the M1 iOS spike |
