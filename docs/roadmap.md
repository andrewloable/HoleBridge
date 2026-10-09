# Roadmap

Each milestone ends with something that can be checked, not a date. Work is tracked in beads
(`bd ready`, `bd show <id>`); every milestone below is an epic there, and its tasks are children of
it. This page states the intent; beads holds the live status. The reasoning behind the order is in
[designs/p2p-tunnel-mvp.md](designs/p2p-tunnel-mvp.md).

```
M0 initiation ──▶ M1 spike ──┬──▶ pears-go (Go Pear library) ──┐
                             └──▶ app engine (app/engine)   ──┴──▶ M2 MVP: every route ──┬──▶ M3 robustness
                                                                                          ├──▶ M4 access control
                                                                                          ├──▶ M5 iOS + background
                                                                                          └──▶ M6 distribution
```

HoleBridge is a peer-to-peer tunnel like holesail.io, not a mesh network (decisions D30). The goal
of the first usable release: **reach your home services from your Android phone, Android TV and
laptop, from anywhere, with no port forwarding**. On Android the app works like a VPN app from the
start (decisions D37); the iPhone gets the same VPN mode in M5.

- **Host and relay** are Go, built on **pears-go**, a pure-Go Pear library in this repo
  (decisions D31, D32).
- **The app's engine** (the app role, LAN probe, codec, multiplexer, resume) is HoleBridge's own
  JavaScript bundle in `app/engine/`. It runs through flutter_pear_bare's existing custom-worklet
  API, and **flutter_pear is not modified** (decisions D34).
- **Both protocol implementations** are kept in step by shared vectors and Go↔JS interop tests.

## M0. Initiation — done

Epic: `HoleBridge-qul`

README, architecture, security, CLI and decision documents, the office-hours design, a DX review,
this plan, beads set up.

## M1. Feasibility spike

Epic: `HoleBridge-3jo`

**Goal:** prove the risky parts on real devices and networks before building on them, and freeze
the key derivation. Throwaway code; findings go into `docs/spike-m1.md`. Every spike has a pass
rule, and the [design](designs/p2p-tunnel-mvp.md) says what happens if it fails.

- **Direct dial:** the app engine (hyperdht in Bare) dials a key-derived host key pair directly,
  with the client-key firewall, on the local testnet and across real NATs.
- **pears-go feasibility (pure Go, decisions D32):** a Go node completes the Noise handshake with a
  JS hyperdht 6.x peer, announces and looks up through dht-rpc, and connects across a real NAT. The
  pure-Go UDX port is measured against JS UDX on the LAN; a gap is a performance task, never a
  switch to cgo.
- **Sockets in the worklet (decisions D5):** loopback listeners and outbound TCP on every app
  platform. Pass rule: on the slowest supported Android TV box, at least 80 Mbit/s over the LAN
  route for one stream and for four combined, using at most one CPU core.
- **LAN route:** signed unicast probes, the sliced /24 sweep, Noise over plain TCP; default ports;
  whether the DHT route connects two peers behind one NAT.
- **Relay fallback:** whether HyperDHT falls back to the relay after a failed punch between one
  randomizing and one consistent NAT.
- **iOS Network Extension:** the engine and the network stack run inside a packet tunnel
  extension with a session and 10 streams, with 20% memory headroom (VPN mode, decisions D37).
- **VPN mode on Android:** on the slowest supported Android TV box, the network stack plus the
  engine meet the same 80 Mbit/s bar; DNS answers HoleBridge's names and forwards the rest; the
  VPN approval works on a TV.
- **Lazy connect:** a cold connect from mobile data takes under 10 s at p50 and 20 s at p95, with
  at least 95% success.
- **WebView:** per-platform WebView availability and storage separated per service.
- **Resume:** stall time after a network switch, byte-exact integrity across repeated switches,
  and the share of switches that recover.
- **UDP:** which routes carry unordered session datagrams (direct, relay; LAN falls back to the
  ordered queue), and the largest datagram that fits a UDX packet after encryption (`maxDatagram`).
- **Key cost:** Argon2id benchmarked on the slowest device we will support (decisions Q1).
- **Key decisions:** Q2 (the 45-bit key) is settled (D38, 2026-10-08). The owner decides Q10
  (scoped keys in the MVP) before the M2 build (D28). Before the iOS spike, the owner checks the Apple developer
  enrollment (Q14).
- **Freeze key format and derivation v1** with a committed test vector, built on a fixed test
  application key (decisions D35).

**Done when:** the spike report has the measurements, the derivation is frozen with its test vector,
D5 is confirmed or replaced, and architecture.md and security.md reflect what the spike found.

## pears-go: the Go Pear library

Epic: `HoleBridge-85m`

**Goal:** a pure-Go implementation of the Holepunch stack in this repo (`pears/`, decisions D32),
wire-compatible with the hyperdht 6.x release the app engine's lockfile pins, enough for the
HoleBridge host and relay. No cgo; builds with `CGO_ENABLED=0`.

- compact-encoding, and dht-rpc (Kademlia, bootstrap, NAT analysis).
- HyperDHT: announce and lookup, a server with a firewall, connect, hole punching, `relayThrough`,
  and a default key pair.
- The Noise handshake (HyperDHT's Ed25519-based curve) and secret-stream framing with keepalive.
- UDX, ported to pure Go.
- libsodium-compatible `secretstream_xchacha20poly1305`, built from `x/crypto` primitives and
  checked against libsodium vectors.
- Protomux.
- blind-relay, server and client.
- Interop CI against JS hyperdht on a local testnet, plus a cross-NAT rig: Go↔Go, Go server ↔ JS
  client, JS server ↔ Go client.

**Done when:** all three interop directions pass on the testnet and across a real NAT, including
through a pears-go relay, and the library's own tests cover every message it encodes.

## M2. MVP: every route

Epic: `HoleBridge-hb5`

**Goal:** many named **TCP and UDP** services under one key (decisions D33), reachable over the
**LAN, direct and relay** routes, surviving network switches, from the app on desktops, Android
phones and Android TV.

- **Scaffold:** Go module, Flutter app with its engine bundle in `app/engine/` (built with
  `bare-pack`, started via `BareWorklet.start(bundlePath:)`), CI.
- **App engine (`app/engine/`, the app role):**
  - key derivation and the protocol v1 codec;
  - the stream multiplexer with credit flow control and TCP sockets;
  - UDP services (local UDP ports, flows);
  - connect with on-demand sessions;
  - the LAN probe, the relay route and route selection;
  - stream resume;
  - the TV handoff (decisions D36).
- **Go host (on pears-go, the host role):**
  - the same codec, multiplexer and resume;
  - UDP services (one target socket per flow);
  - the DHT server with the client-key firewall;
  - the LAN responder and listener with its caps;
  - owns `host.json` and forwards only to configured targets;
  - detects service kinds and prints the QR code and link.
- **Conformance:** shared vectors (key, golden frames) and Go↔JS interop tests in CI.
- **Go CLI:** `share`, `host`, `service`, `key`, `app-key`, `relay`; error codes with docs
  entries.
- **Install:** the install script and the Docker image (decisions D22); example compose setups
  for common home-lab stacks.
- **App:**
  - hosts added by QR code or link (both halves of the key), by typed key once the app holds the
    deployment's application key (decisions D35), and on a TV by handoff from a phone (D36);
  - tiles by kind and a route badge;
  - the in-app browser (separate storage, trust on first use, origin mapping, per-platform
    fallback);
  - on Android TV, the D-pad cursor and the native-app hint;
  - VPN mode on Android phones and TVs: `VpnService`, the network stack, DNS for service names
    (decisions D37);
  - the diagnostics screen.
- **Docs that ship with it:** `docs/install.md`, `docs/apps.md` (using native apps through
  HoleBridge), [docs/relay.md](relay.md) (VPS walkthrough), `docs/errors.md`.
- **The key-link page:** on the project domain (decisions Q8); it also serves the TV handoff
  link.
- **Tests and checks:** end-to-end tests at each layer, and the real-network checklist.

**Done when:**

- **Engine tests** pass on the testnet:
  - one host with three services and concurrent streams;
  - a slow reader that does not stall the other streams;
  - a wrong key turned away, and an unknown service rejected with a reason;
  - a LAN probe answered only when signed;
  - a stream that survives a dropped session byte-exact;
  - a DNS query through a `udp` service on the LAN and direct routes, an oversized datagram
    dropped and counted, and a flow closing after 60 s idle;
  - a handoff box accepted only with the right one-time secret, and refused after the code
    expires;
  - in VPN mode, names that resolve only for configured services, and every other DNS query
    forwarded.
- **Go↔JS interop tests** pass in CI on Linux, macOS and Windows: the Go host against the JS app
  engine on the testnet, plus the shared vectors.
- **The real-network checklist** passes by hand:
  - a TV on the host's LAN connects over the LAN route;
  - a phone on another network connects directly;
  - with both on mobile data, it connects through the relay;
  - a Wi-Fi to mobile-data switch keeps playback going;
  - a DNS query works through a `udp` service on every route;
  - a TV gets a host from a phone by handoff;
  - with VPN mode on, the TV's Jellyfin app and an SSH client on the phone reach services by name
    with HoleBridge in the background;
  - **a fresh device goes from the store to the first Open in under 2 minutes** on an Android
    phone, a TV and a laptop.
- **Decisions D28 and Q8** are recorded.

## M3. Robustness

Epic: `HoleBridge-box`

- `holebridge status` over a local control socket.
- Live reload of services, pushed to connected apps.
- Tests proving no log line carries a key, the application key, a secret or payload.

**Done when:** adding a service on a running host shows up on a connected app without a reconnect.

## M4. Access control and trust

Epic: `HoleBridge-ped`

Scoped keys (unless Q10 pulls them into the MVP), expiring keys, PIN, host identity pinning,
18-symbol strong keys ([security.md](security.md#planned-hardening-options-m4)).

**Done when:** each has end-to-end tests and security.md describes them as built.

## M5. iOS and background operation

Epic: `HoleBridge-are`

The iOS app on real devices: the in-app browser, the local network permission, and VPN mode in a
packet tunnel extension (decisions D37) if M1 says it fits. Q3 decides whether it moves into the
MVP.

**Done when:**
- The iOS app connects over the LAN, direct and relay routes on a real device.
- One of these holds:
  - with VPN mode, the two-week trial passes on the iPhone too;
  - without it, the app says plainly that the tunnel works only while it is open.

## M6. Distribution

Epic: `HoleBridge-6yy`

- Release archives for every OS and architecture, built from a tag.
- Start-at-boot units: systemd, launchd, Windows (decisions D27).
- App releases: Play Store (phone and TV), App Store, desktop installers.
- A changelog.

**Done when:**
- The app installs from the stores and desktop installers.
- Bare-metal hosts start at boot.
- A tag produces the host release with no manual steps.

## Not planned until asked

Apple TV and Android TVs older than Android 10 (decisions Q4), a headless client in the Go binary
(Q5), a public relay (Q7), a community channel, a web dashboard.
