# Architecture

Status: design. Nothing here is implemented yet; where a part is scheduled for a later milestone,
the milestone is named ([roadmap](roadmap.md)). Entries marked **proposed** are settled by the M1
spike.

HoleBridge is a peer-to-peer tunnel like holesail.io: a host shares named services, and an app
holding the key connects straight to it. It is not a mesh. On phones and TVs it works like a VPN
app, but the network behind it is peer-to-peer ([VPN mode](#vpn-mode-android-and-ios)).

## Terms

| Term | Meaning |
|---|---|
| **Host** | The Go program on the machine that has the services: a server, a NAS, a desktop. |
| **Service** | A named TCP or UDP endpoint the host forwards to, e.g. `web` → `127.0.0.1:8080`. The target can be any address the host can reach, including another machine on its LAN. |
| **Key** | 9 characters, e.g. `7KQ-M4X-9TR`: the user's half. Everything else is derived from it together with the deployment's application key ([security.md](security.md#the-application-key)). |
| **App** | The Flutter client on a TV, phone or desktop. It connects to a host and makes its services usable on that device. |
| **Engine** | The code that does the peer-to-peer work: **pears-go** in the Go host and relay, and HoleBridge's own JavaScript bundle (`app/engine/`, in a Bare worklet) in the app. |
| **Route** | How a session reaches the host: **LAN** (same network), **direct** (hole-punched over the internet) or **relay**. |
| **Relay** | An owner-run server with a public IP that forwards encrypted sessions when no direct path exists. Its code ships in this repo. |
| **Session** | One Noise-encrypted connection between an app and a host, over any route. An app keeps at most one per host, opened on demand ([sessions](#sessions-and-reconnects)). |
| **Stream** | One forwarded TCP connection inside a session. |
| **Flow** | One UDP conversation inside a session: a source address on the app's local UDP port, paired with one host-side socket to the target. |
| **VPN mode** | On Android and iOS: the app works like a VPN app, giving each service an address and a name on the device. The network behind it is still the peer-to-peer session ([VPN mode](#vpn-mode-android-and-ios)). |

## Components

```
 client device: TV, phone, desktop                     host machine: server, NAS, desktop
┌─────────────────────────────────────┐               ┌─────────────────────────────────────┐
│ HoleBridge app (Flutter)            │               │ holebridge (Go, one process)        │
│  UI, lifecycle, foreground service  │               │  CLI, host.json, status             │
│         │ binary IPC: control only  │               │  HoleBridge host role               │
│ engine (app/engine, Bare worklet)   │               │  pears-go: HyperDHT, Noise, UDX,    │
│  HoleBridge app role                │  one session  │            Protomux, blind-relay    │
│  127.0.0.1:8080 ◀─ browser, apps    │◀═ Noise ═════▶│  ──tcp──▶ web  127.0.0.1:8080       │
│  127.0.0.1:2222 ◀─ ssh client       │ LAN, direct   │  ──tcp──▶ ssh  127.0.0.1:22         │
└─────────────────────────────────────┘  or relay     └─────────────────────────────────────┘
          │                                                           │
          └──── lookup ───▶  public HyperDHT  ◀── announce ───────────┘
                                   ▲
                  holebridge relay (owner's VPS, optional, pure Go)
```

Three codebases plus a library, and one optional server ([decisions](decisions.md) D31):

1. **pears-go, a Go Pear library.** A wire-compatible Go implementation of the Holepunch stack:
   - compact-encoding and dht-rpc;
   - HyperDHT: announce and lookup, a server with a firewall, connect, hole punching and
     `relayThrough`;
   - the Noise handshake and secret-stream, UDX, Protomux and blind-relay.

   No usable Go implementation exists: the only Go HyperDHT is from 2021 and speaks the pre-UDX
   protocol. A D reimplementation proves a port can interoperate with hyperdht 6.x across NATs.
   pears-go stays compatible with the hyperdht release the app engine's lockfile pins, and its CI
   runs Go↔JS interop tests against that version. It lives in this repo under `pears/` and is pure
   Go: no cgo, built with `CGO_ENABLED=0`, depending only on the standard library,
   `golang.org/x/crypto` and `filippo.io/edwards25519` ([decisions](decisions.md) D32).
2. **Host: Go, in this repo.** A single process on pears-go, with no `bare` and no IPC. It owns
   `host.json`, the CLI and the status socket. It also implements HoleBridge's **host role**: the DHT
   server under the host key pair with the client-key firewall, the LAN responder and listener, the
   protocol codec, the multiplexer with flow control and resume, the target connections and service
   kind detection.
3. **App: Flutter, in this repo.** Its engine is HoleBridge's own JavaScript bundle (`app/engine/`),
   built from the original Holepunch libraries. It runs in a Bare worklet started through
   flutter_pear_bare's existing `BareWorklet.start(bundlePath:)`, and it implements the **app
   role**: the connect, the LAN probe, the same codec, multiplexer and resume, and the TV handoff.
   Dart sends commands and gets events over HoleBridge's own binary IPC. flutter_pear is not
   modified ([decisions](decisions.md) D34). You scan the host's QR code, type a key, or on a TV
   take the host from a phone, then see the services and their local ports, and open web services in
   an in-app browser or the system one. On Android and iOS, VPN mode lets every app on the device
   reach the services by name ([VPN mode](#vpn-mode-android-and-ios)). Targets: Android phones,
   Android TV and Google TV, iOS, macOS, Windows, Linux.
4. **Relay: the same Go binary,** `holebridge relay`, running pears-go's blind-relay server. One
   download serves as host or relay. It is optional; HoleBridge ships no relay address and no
   default relay ([Relay route](#relay-route)).

### One protocol, two implementations

The host role is Go and the app role is JavaScript, so HoleBridge's shared core exists in both
languages:
- key normalization and derivation;
- the protocol codec;
- the multiplexer with credit flow control, the receive budget and resume;
- UDP flows.

Three things keep them in step:
- **Conformance vectors** shared by both test suites: `spec/vectors/key.json` and golden protocol
  frames under `spec/vectors/`.
- **Interop tests in CI:** the Go host against the JS app on a local HyperDHT testnet, covering
  every route the testnet can exercise, resume and the error codes.
- **The cross-NAT checks** in the real-network checklist.

A protocol change lands in both implementations and the vectors in the same commit, or not at
all.

### Why the app's data path stays inside the worklet (proposed)

On the app, Dart sends commands and receives events; **bytes never cross the IPC**. The worklet
accepts the local TCP connections and moves bytes between those sockets and the session itself.

- **No IPC hop for the data.** Even with HoleBridge's binary IPC, every byte crossing into Dart
  would be copied through a platform channel. That is fine for control messages and wrong for
  video to a TV.
- **Real backpressure locally.** Socket and session streams sit in one runtime, so a slow local
  socket pauses its own stream through the credit window, with no IPC queue in between.
- **One app-side implementation**, not a JS one plus a Dart one.

The alternative is to have the worklet carry session messages only, with Dart running the
multiplexer and owning the sockets. It is the fallback on a platform that misses the throughput bar
in decisions D5 or cannot open loopback listeners in the worklet.

### Why the app dials a public key, not a Hyperswarm topic

The app derives the host's public key and dials it with HyperDHT. Topic discovery is weaker here:

- **Topic peers prove nothing.** Anyone who learns a topic can answer on it, so a client would
  need a second authentication layer (TLS inside the stream, pinned) to know it reached the right
  peer. A key-derived host key pair makes the Noise handshake itself that proof.
- **Topic connections race.** Hyperswarm attributes an inbound connection to a topic only after
  its own lookup finds the peer. A host on its topic for hours sees a new client's connection
  before that, and a client behind a slow NAT may not land its announcement in time. Dialing a key
  has no announcement race.
- **Random identities leave debris.** A peer with a fresh key pair per start leaves a dead DHT
  record per restart that later dialers try first. The host's key pair is derived, so it is the
  same after every restart.

The app engine calls `hyperdht` directly (D34); pears-go implements the same on the host.

## Routes

| Route | When | Path | Found by |
|---|---|---|---|
| **LAN** | App and host on the same network | TCP straight to the host, Noise on top | A signed UDP probe the host answers |
| **Direct** | Different networks, punchable NATs | UDX over a hole-punched UDP path | HyperDHT lookup of the host public key |
| **Relay** | Both sides behind randomizing NATs, e.g. both on mobile data | Through the owner's relay, still end-to-end encrypted | HyperDHT, when hole punching cannot work and a relay is set on both sides |

Every route carries the **same session**: Noise authenticated by the same key pairs, then the same
Protomux channel and protocol. Only the carrier differs.

### Choosing a route (app)

1. **LAN first, briefly.** Probe the host's last known LAN address alone, then sweep the device's
   own /24 ([LAN route](#lan-route)). A whole sweep takes about 1.5 s. If the host answers, connect
   over the LAN.
2. **Otherwise HyperDHT.** `dht.connect` finds the host and hole-punches. With a relay set on both
   sides, HyperDHT falls back to it by itself when punching cannot work; connections that work
   directly stay direct.
3. **Re-evaluate** when the device's addresses change (polled every 5 s: it joined or left a
   network) and when the session drops. A TV that was on the DHT route moves to the LAN when it
   finds the host there; a phone that walks out of the house moves from the LAN to the DHT.
4. **"Looking" and "can't reach" are different states.** A DHT lookup can take 30 s or more on a
   slow network, and "still looking" must never read as "failed". After a failure the app retries
   by itself after 30 s.
5. **Every search is a fresh attempt**, never a wait on an old one. A long-lived dial that missed
   a host restart does not notice the host came back.

Open streams survive a route change ([Sessions and reconnects](#sessions-and-reconnects)).

**A first connect can skip typing.** The host prints a QR code and an https link carrying both
halves of the key, the 9 symbols and the deployment's application key ([cli.md](cli.md#hosting)).
A store app needs that once per deployment. Scanning or opening it starts the app with the key
filled in. When the app is not installed yet, the link opens the store page, and you scan again
after installing: stores never pass a link into a freshly installed app. Typing the 9 symbols works
once the app holds the deployment's application key. A TV, which has no camera, gets the host from
a phone instead ([handoff](#adding-a-host-to-a-tv-handoff)).

### Adding a host to a TV (handoff)

A TV cannot scan the host's QR code, and a store app on it has no application key to go with typed
symbols. So a phone that already holds the host hands it over the LAN ([decisions](decisions.md)
D36). Both ends are the HoleBridge app: the app engine on each side, no host involved.

1. **The TV shows a code.** **Add from phone** makes the TV's engine generate a one-time X25519 key
   pair and a one-time 16-byte secret, and listen on a TCP port on its LAN addresses. The TV shows a
   QR code with a link, `https://holebridge.app/h#...`, carrying the public key, the secret, its LAN
   addresses and the port. As with the key link, the part after `#` never reaches a web server.
2. **The phone scans it.** The phone's camera or the app's scanner opens the link in the app, and
   the user picks one of the hosts the phone holds.
3. **The phone sends one sealed box.** Its engine connects to the TV over the LAN and sends
   `crypto_box_seal` to the TV's public key, containing the one-time secret, the host's name, its 9
   symbols and the deployment's application key.
4. **The TV checks and adds.** It accepts only a box that opens and carries the one-time secret
   (compared in constant time), adds the host, closes the listener and connects.

- **Who can read or inject.** Only the TV holds the private key, so nobody on the network can read
  the box. Only someone who saw the TV's screen knows the secret, so nobody else can push a host
  onto the TV. The code itself gives away nothing that outlives the 5-minute pairing.
- **Bounds.** One box of at most 1 KiB per connection, a 5 s read deadline, at most 4 connections at
  once, and any invalid box closes its connection. The listener closes after the first valid box,
  after 5 minutes, or when the user leaves the screen.
- **Errors.** `HB-HANDOFF-UNREACHABLE` when the phone cannot reach the TV. The fix it states: the
  same network, and not a guest network with client isolation. `HB-HANDOFF-EXPIRED` when the code
  timed out.

**Formats (proposed until M2 commits `spec/vectors/handoff.json`):**
- **The TV's link:** `https://holebridge.app/h#1.<public key>.<secret>.<port>.<addresses>`. `1` is
  the format version; the public key is 64 lowercase hex digits and the secret 32; the port is
  decimal; the addresses are the TV's IPv4 LAN addresses, comma-separated.
- **The box's plaintext,** compact-encoded in this order: `version` (uint, `1`), `secret`
  (fixed 16 bytes), `name` (string, the host's display name), `key` (string, the 9 canonical
  symbols), `appKey` (fixed 32 bytes).
- **On the wire:** the phone writes the sealed box and ends its side of the TCP connection. The TV
  reads to the end (at most 1 KiB), then writes one byte, `1` for added or `0` for refused, and
  closes.

### Direct route (connection flow)

1. **Derive.** The app hands the key and the deployment's application key to its engine, which
   normalizes the key and derives the host public key, the client key pair and the LAN probe key
   with Argon2id ([security.md](security.md#derivation)). This takes a fraction of a
   second, on purpose.
2. **Find.** `dht.connect(hostPublicKey, { keyPair: clientKeyPair })` looks up the host's
   announcement on the DHT.
3. **Admit.** The host engine's server `firewall` rejects every remote public key except the
   client key pair's (and, from M4, other admitted keys). A peer without the key never gets a
   session.
4. **Punch and encrypt.** HyperDHT hole-punches a UDP path and runs the Noise handshake. Because
   the host listens under the key-derived host key pair, completing the handshake proves to the
   app that it reached a holder of the key, not an arbitrary DHT peer.
5. **Open the channel.** Both engines open the Protomux channel `holebridge` and exchange
   handshakes ([Wire protocol](#wire-protocol-v1)). The host's handshake carries its service list.
6. **Bind.** The app's engine opens one local listener per service on `127.0.0.1` and reports the
   ports to the app ([cli.md](cli.md#the-app) for the port rules).
7. **Forward.** Each accepted local connection becomes an `open` for that service. The host engine
   connects to the service's configured target and answers `opened` or `reject`. Bytes then flow
   both ways as `data`, paced by `window` credit, until a `close` handshake.

The host never connects anywhere an app names. An app asks for a service by name; the host looks
the name up in its own configuration. No name, no connection.

### LAN route

When the app and host share a network, the DHT route is the wrong one. It needs the internet, it
adds latency and caps throughput below the LAN's. Two peers behind the same router can also fail
to reach each other through it: punching back into your own public address depends on router
hairpinning, which many routers lack. HyperDHT tries local addresses first when both sides share a
public IP; how often that succeeds is measured in M1. Either way, the same network is the most
common HoleBridge case, the TV in the living room and the server in the closet, so the LAN route is
part of the MVP and works with no internet at all.

**Discovery: a signed probe, answered unicast.** The host runs a responder on a UDP port (default
chosen in M1, configurable). The app sends it a probe:

- **Exactly 256 bytes:** magic `HBLANQ1`, a 16-byte nonce, the sender's wall-clock timestamp, a MAC
  (keyed BLAKE2b) under the LAN probe key derived from the key, then zero padding. The reply is
  enforced to be smaller, so the responder can never amplify traffic.
- **The reply** goes unicast to the sender. It echoes the nonce, carries the host's LAN TCP port,
  and is MACed with the same key. It carries routing data only, never a secret.
- **Silent to anything else:** unsigned, mis-sized, replayed or stale. Answering would tell everyone
  on the network a host is there.
  - **Freshness** compares the probe's timestamp with the host's wall clock, with a deliberately
    wide ±24 h window, so clock skew between devices never refuses a real probe.
  - **Replay protection** is a cache of seen nonces whose entries expire on the host's monotonic
    clock, so a wall-clock jump after boot neither expires nor extends them.
  - **Known ceiling:** the cache lives in memory, so after a host restart a probe captured in the
    last 24 h can be answered once more. The only effect is revealing that a host is present.
- **The host answers; it never advertises.** No broadcasts, no mDNS announcements.

**Probe and reply bytes.** The vectors are committed in `spec/vectors/lan-probe.json`, generated by
`spec/gen/lan-probe.js`. Integers are little-endian. The MAC is BLAKE2b-256 keyed with the LAN probe
key, over bytes 0 to 31.

| Bytes | Probe (256 bytes) | Reply (64 bytes) |
|---|---|---|
| 0-6 | magic `HBLANQ1` (ASCII) | magic `HBLANR1` (ASCII) |
| 7 | format version, `1` | format version, `1` |
| 8-23 | nonce, 16 random bytes | the probe's nonce |
| 24-31 | sender's wall clock, milliseconds since the Unix epoch, uint64 | bytes 24-25: the host's LAN TCP port, uint16; bytes 26-31: zero |
| 32-63 | MAC | MAC |
| 64-255 | zero | (none) |

**Probes are unicast, never broadcast.** Broadcast and multicast are unreliable on exactly the
devices HoleBridge targets. Android Wi-Fi drivers commonly drop them unless the app holds a
multicast lock, and holding one keeps Wi-Fi awake. iOS forbids *sending* broadcast without a
special Apple entitlement. So the app:

1. **Probes the last known address alone.** Each session's host handshake reports the host's LAN
   addresses (behind a flag), and the app keeps them per host. A lone probe is answered reliably.
   Probing it inside a sweep is not: every probe to an empty address costs an ARP request, and that
   storm starves the resolution of the one address that matters.
2. **Then sweeps its own /24, in slices of 32 probes, each slice on its own socket, 100 ms apart**,
   then waits 800 ms for replies (about 1.5 s in all). A single socket sending 254 probes fills its
   send buffer with probes stuck waiting on ARP for empty addresses, and a non-blocking send then
   drops the rest without an error. Android TV devices showed exactly this: a one-socket burst never
   reached a host near the end of the /24, while slices of 32 found it every time.

**Session over the LAN:** the app opens TCP to the host's LAN port and runs the same Noise
handshake over it (`@hyperswarm/secret-stream` wraps any duplex stream), as initiator with the
client key pair, expecting the host public key. The host destroys any connection whose remote
static key is not an admitted client key, and bounds connections that have not proven it yet
([limits](#limits)). From there it is the same Protomux channel and protocol.

The LAN responder and listener can be turned off in `host.json` for hosts on untrusted networks.

### Relay route

When **both** ends sit behind randomizing NATs, as with a phone on mobile data and a host on carrier
NAT, hole punching does not work at all: HyperDHT aborts with `HOLEPUNCH_DOUBLE_RANDOMIZED_NATS`
without trying. When one end randomizes, punching is probabilistic and slow; in field measurements
from a phone hotspot, a third of cold starts never formed a connection. IPv6 does not help: HyperDHT
exchanges IPv4 addresses only. A relay is the only fix.

**An owner-run blind relay.** `holebridge relay` runs on a server with a public IPv4 address and an
open UDP port range. It forwards the Noise-encrypted session and cannot read it.

**One relay key, set on both sides.** The relay owner creates a relay key (the same 9-symbol format)
and enters it on the relay, in `host.json` (`relay`) and in the app (Settings > **Relay**). Each
side derives two key pairs from it ([security.md](security.md#relay-keys)):

- **server:** the relay listens under it, so members know which relay to dial;
- **member:** hosts and apps connect under it, and the relay's firewall rejects every other key.

HyperDHT opens relay connections with the DHT's `defaultKeyPair`, not with the key pair a server
listens under or a client dials with. So each side sets its DHT `defaultKeyPair` to the member key
pair. If only one side has the relay key, the relay turns the other away and the connection fails
as it would without a relay.

**Wiring it in, on both sides.**
- **App.** The app engine creates its HyperDHT instance with the member key pair as
  `defaultKeyPair`, and passes the `relayThrough` policy to `dht.connect` itself.
- **Host.** pears-go implements the same: its default key pair is the member key pair, and its
  server takes the `relayThrough` policy.
- **Proof.** An interop test shows a Go host and a JS app meeting through a pears-go relay.

**When it is used.** The policy offers the relay when the dial is forced (HyperDHT retrying after a
failed punch) or when *that side's own* NAT randomizes. A phone on mobile data offers it from the
app side, and a host on carrier NAT offers it from the host side. Either way a connection that can
work directly stays direct. Whether HyperDHT falls back to the relay after a failed punch between
one randomizing and one consistent NAT is verified in M1.

**What the relay sees:** IP addresses, connection times and volumes, never content. It never logs
the relay key, and logs how many pairings it matched every 10 minutes, so an owner can spot use
that is not theirs and change the key.

**Many relays?** The MVP supports one relay key per host and per app, the same on both. Each DHT has
one `defaultKeyPair`, so two relay keys at once would need two DHT instances. Whether several relay
servers can share one relay key for redundancy is tested alongside the relay work.

## Wire protocol v1

One Protomux channel per session, protocol name `holebridge`. All integers are compact-encoding
`uint`; strings are UTF-8. It is implemented twice, in Go for the host and in JavaScript for the
app, held together by the shared vectors and interop tests
([One protocol, two implementations](#one-protocol-two-implementations)).

### Handshake

Each side sends one when the channel opens.

| Field | App → host | Host → app |
|---|---|---|
| `version` | `1` | `1` |
| `flags` | capability bits | capability bits |
| `services` | | list of `{ name, kind, port, origins }`, described below |
| `lan` | | behind a flag: the host's LAN addresses and port, for the app's next LAN probe |

Each `services` entry carries:

- **`kind`:** `https`, `http`, `tcp`, `udp` or `unknown`, from
  [kind detection](#service-kinds).
- **`port`:** only a hint for the app's local listener.
- **`origins`:** the optional list a host owner sets for a web service, used by the in-app browser
  to map absolute links ([in-app browser](#the-in-app-browser)).

Nothing else about the target is sent: not its address, not its hostname. The handshake list
replaces the app's cached list for that host.

Optional fields are added behind `flags` bits, so a new feature does not bump `version`: stream
resume (MVP), PIN (M4) and the host identity proof (M4) all arrive this way. A side that sees a
different `version` closes the channel and reports, e.g., "This host speaks protocol v2 and this app
speaks v1: update the app from the store" (or "re-run the install script on the host"). Before 1.0
there is no compatibility promise between versions ([decisions](decisions.md) D26).

### Messages

Message indexes are append-only: never renumber, never reuse.

| # | Message | Direction | Fields |
|---|---|---|---|
| 0 | `open` | app → host | `stream`, `service`, `window` |
| 1 | `opened` | host → app | `stream`, `window`, resume token |
| 2 | `reject` | host → app | `stream`, `code`, `reason` |
| 3 | `data` | both | `stream`, 1..65536 bytes |
| 4 | `window` | both | `stream`, `credit` (> 0), `received` (total bytes so far) |
| 5 | `close` | both | `stream` |
| 6 | `reattach` | app → host | `stream`, `token`, `received`, `limit` |
| 7 | `reattached` | host → app | `stream`, `received`, `limit` |
| 8 | `services` | host → app | M3: the new service list after a config reload |
| 9 | `flow` | app → host | `flow`, `service`, the flow's first datagram |
| 10 | `datagram` | both | `flow`, 1..`maxDatagram` bytes; only on routes without unordered datagrams |

- **Stream ids** are chosen by the app, which is the only side that opens streams.
- **`reject` codes:** unknown service, limit reached, target refused, target timed out. The app
  closes the local connection and shows the reason, so "ssh refused: connection refused at
  127.0.0.1:22" reaches the user instead of a silent drop. The host sends limit reached (code 2)
  without calling the service's accept, when its session or the process is full. The app refuses a
  stream the same way, locally and without sending, when its own limit is full.
- **`close` is a half-close.** A side receiving `close` ends only its read side: it delivers the bytes
  the peer sent, then end of stream, and its own write side stays open. The side that finishes first
  keeps the stream until the other side's `close` comes back.
  - **Answering close.** A received `close` is never answered by itself, even when nothing is unread
    and no read is waiting. The answering `close` is sent when the local side ends its write side
    (CloseWrite or end) or destroys the stream. Until then the local side can still write back, so a
    copy loop that reads a request, sees the peer's close and writes the reply is not cut off.
  - **A close for a stream that never opened** (the peer ended it before `opened`) resets the stream,
    and is answered at once.
  - **Both closes.** Each side's `close` is sent once, and again after a reattach in case the first was
    lost. A repeated `close` is ignored. A stream is freed when both have come back.
- **Finished ids are remembered for 60 s.** After a stream finishes, or the host refuses it, its id
  is remembered for 60 s. A `window`, `close`, `opened` or `reject` for a remembered id is ignored in
  that time, because it raced the close. After 60 s the id is forgotten, and the same message ends
  the session.
- **Cancelled open.** An app that cancels an open before `opened` or `reject` arrives drops the
  stream, gives its slot back, and sends `close` for it, so the host frees its side. A late `opened`
  or `reject` for that id is ignored.

### Encoding

Proposed until M2 commits the golden frames in `spec/vectors/frames.json`; after that, frozen like
the message indexes. Every message and the handshake are
[compact-encoding](https://github.com/holepunchto/compact-encoding) structs, fields in the order
listed:

| Message | Fields and types |
|---|---|
| handshake | `version` uint; `flags` uint; `services` array of service; `lan` (only when flag bit 1 is set): `addresses` array of string (IPv4), `port` uint |
| service | `name` string; `kind` uint (0 `unknown`, 1 `https`, 2 `http`, 3 `tcp`, 4 `udp`); `port` uint; `origins` array of string |
| 0 `open` | `stream` uint; `service` string; `window` uint |
| 1 `opened` | `stream` uint; `window` uint; `token` fixed 16 bytes (zeros when resume is off) |
| 2 `reject` | `stream` uint; `code` uint (1 unknown service, 2 limit reached, 3 target refused, 4 target timed out); `reason` string |
| 3 `data` | `stream` uint; `payload` buffer (1 to 65536 bytes) |
| 4 `window` | `stream` uint; `credit` uint; `received` uint |
| 5 `close` | `stream` uint |
| 6 `reattach` | `stream` uint; `token` fixed 16 bytes; `received` uint; `limit` uint |
| 7 `reattached` | `stream` uint; `received` uint; `limit` uint |
| 8 `services` | `services` array of service |
| 9 `flow` | `flow` uint; `service` string; `payload` buffer |
| 10 `datagram` | `flow` uint; `payload` buffer |
| unordered datagram (UDX message, not a channel message) | `flow` uint; `payload` buffer |

- **Flags:** bit 0 (value 1) stream resume; bit 1 (2) `lan` present; bit 2 (4) unordered datagrams
  supported. Bits 3 (PIN) and 4 (host identity proof) are reserved for M4.
- **`received`** is the total number of payload bytes received on the stream so far. **`limit`**
  is `received` plus the sender's free window: the highest offset the other side may send up to.
- The app's handshake carries an empty `services` list and no `lan`.

### UDP services

HoleBridge forwards UDP as well as TCP, in the same session and under the same key. A `udp` service
uses flows, not streams.

- **Flows.**
  - **App side:** the app binds a local UDP port per `udp` service, on 127.0.0.1 like its TCP
    listeners. Each source address that sends to it becomes a flow, with an id the app chooses.
  - **Host side:** the host opens one ephemeral UDP socket per flow, connected to the service's
    configured target. Replies route back to that flow, and datagrams from any other source are
    dropped.
- **Opening a flow.** The app sends `flow` (message 9) on the ordered channel, carrying the service
  and the flow's first datagram, so the first packet (a DNS query, a game handshake) is never lost
  to a setup race.
- **Datagrams after the first.**
  - On the direct and relay routes, datagrams ride the session's unordered datagram path (UDX
    messages). Each one is `{ flow, payload }`, with no retransmission and no ordering, as UDP
    expects.
  - The LAN route is TCP, so there they travel as `datagram` (message 10) on the ordered channel,
    through a 256 KiB queue per session that drops new datagrams when full instead of delaying
    them.
  - The M1 datagram spike confirms which routes carry unordered datagrams, including through the
    relay. Any route that cannot falls back to message 10.
- **Size.** A datagram larger than `maxDatagram` (default 1144 bytes) is dropped and counted, and the
  app logs `HB-UDP-TOO-LARGE` once per flow. There is no fragmentation. The default is the 1156-byte
  largest unordered message (measured in M1; see [spike-m1](spike-m1.md#unordered-datagrams)) less the
  frame header that an unordered datagram adds: the `flow` uint and the payload's length prefix. A
  flow id of 2^32 or more takes 9 bytes and a payload of 253 bytes or more takes a 3-byte prefix, so
  the header is 12 bytes at most (measured with `protocol.EncodeUnordered` in Go and with the engine's
  `unordered` codec in JavaScript: 1144 + 12 = 1156 on the wire). The limit holds for every flow id, so it
  is set for the widest one. The limit is the same on every route.
- **Lifetime.** A flow closes on each side after 60 s with no datagram either way. A service's `idle`
  setting overrides this. There is no close message: either side simply forgets the flow.
- **Reconnects.** Flows are not resumed. The app keeps its local UDP port; after a new session, the
  next datagram from a source opens a fresh flow. Datagrams in flight during the switch are lost,
  as on any UDP path.
- **Kinds.** `udp` is always explicit (`--kind udp`); the host never probes a UDP target.
- **In the app,** a `udp` tile shows **Copy address**, which copies the local `127.0.0.1` UDP port.

```
 app device                          session                         host
 udp client ──▶ 127.0.0.1:5353  ──▶  flow{f1, dns, first datagram}  ──▶ socket f1 ──▶ 127.0.0.1:53
            ◀──                ◀──  {f1, reply} (unordered, or     ◀──           ◀──
                                     datagram msg 10 on LAN)
```

### Flow control

Every stream has credit in both directions. Each side states how much it is willing to buffer
(`window` in `open` and `opened`, default 2 MiB) and grants more with `window` only as it hands
bytes to its local socket. A sender never has more than its credit in flight.

All streams share one ordered session. Pausing the session for one slow socket would stall every
other stream, and buffering without a limit would let one slow consumer grow the process without
bound. A peer that sends past its credit loses the stream.

**A total budget caps the sum.** Each process also has a receive budget ([limits](#limits)), so many
stalled streams slow down instead of exhausting memory. A stream left with no credit by the budget is
not stranded: when credit frees (another stream closes or is drained), it is granted its share.

Why 2 MiB: one stream moves at most one window per round trip. A 256 KiB window caps a stream at
about 2.4 Mbit/s over an 850 ms mobile round trip (measured), below a 6 Mbit/s video. Each receiver
picks its own window, so a memory-tight TV box can grant less.

**Trade-off accepted:** one session means one ordered transport, so a lost packet briefly delays
every stream behind it (the same as HTTP/2 over TCP). In exchange, opening a stream costs one
message instead of a DHT lookup and handshake.

### Malformed input

Everything on the wire comes from the far side of the internet. A message that does not decode or
breaks the protocol is handled as a value, never as an exception:

- **`data` before `opened`, `data` after the peer's `close`, or `data` past the credit** resets that
  stream: its unread bytes are dropped, its credit and slot go back, and `close` is sent for it. The
  session stays up.
- **An id that is not open** (a message for a stream never opened, or forgotten after 60 s) and **an
  `open` for an id already open or remembered** end the session, since the peer has broken the
  protocol. A remembered id is ignored instead (see [Messages](#messages)).

No input from a peer can crash the host process or the app's worklet. **Every socket gets an `error`
handler:** a peer that vanishes mid-stream, such as a phone losing signal, resets its stream, and an
unhandled error there would take down every other connection in the process.

## App engine IPC

The host has no IPC: pears-go runs inside the Go process. On the app, Dart drives HoleBridge's engine
bundle over flutter_pear_bare's binary frames, with a HoleBridge-defined message set
(compact-encoded, not JSON). The command names below are a sketch, settled in M1:

| Command | Does |
|---|---|
| `connect` | Connect to a host (its key and application key); reply with the route, the services and the local ports bound |
| `close` | Disconnect |
| `status` | Route, sessions, streams, flows, bytes, NAT type |
| `relay` | Use a relay key (member key pair as the DHT default key pair) |
| `handoff` | TV: open a handoff listener and report the host it receives; phone: send a host to a scanned TV code ([handoff](#adding-a-host-to-a-tv-handoff)) |

Events flow back: route changes, session up or down, a `reject` with its reason, errors with their
codes. Keys and application keys cross this IPC from Dart into the worklet. **A key never goes on a
command line or in the environment**, where other local users can read it with `ps`.

**The stdout channel stays clean.** On desktop apps, flutter_pear_bare runs `bare` as a subprocess,
and frames travel on the worklet's stdout. The engine's entry routes `console.*` to stderr before
anything else loads. HoleBridge's Dart IPC client treats a malformed frame as fatal
(`HB-IPC-DESYNC`) and restarts the worklet, because native code can still write to fd 1 directly.

## Limits

Anyone with the key can connect, so every resource is capped. Starting defaults, all configurable
in `host.json`, tuned during M2:

| Limit | Default | Why |
|---|---|---|
| Sessions per key | 32 | One key shared too widely cannot exhaust the host |
| Streams per session | 128 | Browsers open many connections; still bounded |
| Streams in total | 1024 | Bounds sockets and memory |
| Target connect timeout | 10 s | A dead service answers `reject`, not a hung stream |
| LAN handshake deadline | 5 s | A LAN connection that has not proven the client key by then is closed |
| Unauthenticated LAN connections | 32 in total, 4 per source IP | Any device on the network can connect before proving the key; extras are closed at accept, counted, and logged at most once a minute |
| Receive window per stream | 2 MiB | Throughput on high-latency links (above) |
| UDP flows | 256 per session, 4096 in total | Each flow holds a host-side socket |
| UDP flow idle | 60 s, or the service's `idle` | Mirrors common NAT UDP timeouts |
| Max datagram (`maxDatagram`) | 1144 bytes (the 1156-byte unordered message measured in M1, [spike-m1](spike-m1.md#unordered-datagrams), less the 12-byte frame header at the widest flow id) | The largest payload that fits one unordered message after the frame header; larger datagrams are dropped and counted |
| Largest protocol frame (Protomux, `maxFrame`) | 16777215 bytes (2^24 - 1) | The secret stream writes a frame atomically up to this size, and upstream Protomux splits a batch at 8 MiB so a batch stays under it. A frame over it is refused on send and fails the stream on receive. A data message carries at most 65536 bytes, so its frame is about 65544 bytes |
| Ordered datagram queue (LAN route) | 256 KiB per session, drop when full | UDP never waits; a stalled channel drops instead |
| Receive budget per process | 64 MiB in the app, 256 MiB on the host | Bounds memory however many streams stall: new grants are min(2 MiB, budget left / open streams), and once it is spent credit only follows draining; a stream left with none is granted its share when credit frees |
| Idle timeout | per service, **off** by default | SSH and database connections sit idle for hours; a fixed HTTP-style idle timeout would cut them |

## Sessions and reconnects

HoleBridge is a point-to-point tunnel, not a mesh ([decisions](decisions.md) D30): an app reaches
one host's services, and devices never join a network or see each other. So a session exists only
while something uses it.

**On demand.** A session opens when a service tile is tapped, or when a connection arrives on one of
the app's 127.0.0.1 listeners and no session exists. A local connection that wakes a session waits
up to 30 s for it, which covers the slow end of a cold DHT lookup. If no session forms by then, the
socket is closed, and the app shows the error with its code.

**Idle.** One rule: the session closes 5 minutes after its last stream closes. While any stream is
open, even a quiet one (an idle SSH login, paused playback), the session stays up. Keepalives on the
encrypted stream hold the NAT mapping open and detect a dead path; they go out every 5 s while the
stream is idle. The app keeps the last route and
the host's LAN addresses, so the next connect is fast.

**Who holds the listeners while the app is not on screen:**

| Platform | Listeners and wake-on-connect |
|---|---|
| Desktop | The app process (tray, start at login) always holds them; a local connection wakes the session. |
| Android (phone, TV) | With **VPN mode** on, the `VpnService` (a foreground service) keeps the service addresses up with no session, and a connection to one wakes the session. With no session it sends no network traffic. With VPN mode off, listeners exist only while the app is open. |
| iPhone | With **VPN mode** on (M5), the packet tunnel extension does the same. Until then, only while the app is in the foreground, with no wake-on-connect. |

**Reconnects.** When the session drops, the app finds a route again
([Choosing a route](#choosing-a-route-app)), backing off from 1 s to 30 s with jitter.

**Streams survive a reconnect and a route change (MVP).** A session is reliable while it lives, so
bytes are only lost when it dies.
- **Token.** `opened` carries a 16-byte random token.
- **Keeping bytes.** Both sides keep sent bytes until a `window` acknowledges them. After a drop,
  both keep their sockets open for a 60 s grace period; the local 127.0.0.1 sockets stall but do
  not close.
- **Reattach.** The app's new session, on whatever route, sends `reattach` with each stream's token
  (compared in constant time), and both resend from the other's `received` offset. A reattach whose
  token is wrong, whose stream has expired, or that the host cannot take gets `close` for that stream
  and no `reattached`; the session stays up. The host may get a reattach before it sees the old
  transport die, and then it takes the stream from the old session.
- **No crossed bytes.** The app ignores `data` and `window` on a stream until `reattached` arrives,
  because the host may still be sending into the old session's gap.
- **Caps.** The bytes kept for resending are capped per stream (4 MiB) and in total (32 MiB), so a
  peer that grants credit and never acknowledges cannot grow the host's memory. A stream whose new
  session does not form within the grace period, or that exceeds the caps, closes as it would
  without resume.

This is byte-exact for every TCP stream, whatever it carries. It is what makes a phone walking from
Wi-Fi to mobile data, or a TV moving from the DHT route to the LAN, invisible to the video playing
through it. The M1 resume spike measures the stall, integrity (a hashed transfer across repeated
switches) and the share of switches that recover. If the spike fails, resume moves out of the MVP
and drops close streams.

**Session lifecycle (app side):**

```
            tap tile / local connection, no session
 [idle] ─────────────────────────────────────────▶ [looking: LAN probe, then DHT]
   ▲                                                   │ found          │ gave up (60 s)
   │ 5 min after last stream closes                    ▼                ▼
   │                                              [connected:      [can't reach]
   └──────────────────────────────────────────────  LAN|direct|relay]   │ retry after 30 s
                                                      │ path dies        └──▶ [looking]
                                                      ▼
                                       [resuming: sockets stall, 60 s grace]
                                       new session + reattached ──▶ [connected]
                                       grace expires ──▶ streams close ──▶ [looking]
```

## VPN mode (Android and iOS)

On phones and TVs the app works like a VPN app such as Cloudflare WARP, but the network behind it
is HoleBridge's own peer-to-peer session to the host ([decisions](decisions.md) D37). It is still a
tunnel to one host's named services, not a mesh.

```
 phone or TV                                                                  host
 Jellyfin app ──▶ jellyfin.living-room.internal
                    │ DNS, answered by HoleBridge: 198.18.0.2
                    ▼
 VPN interface ──▶ network stack ──▶ engine ═══ session (LAN, direct, relay) ═══▶ 127.0.0.1:8096
 (198.18.0.0/16)    packets → TCP, UDP    streams, flows                         configured target
```

- **What it catches.** Only a private range, proposed as `198.18.0.0/16`: it is set aside for
  benchmarking, so neither the internet nor carriers use it (M1 confirms). All other traffic leaves
  the device as usual.
- **Names.** Each service gets one address in the range and the name `<service>.<host>.internal`
  (`.internal` is reserved for private use). `<host>` is the host's name in the app, lowercased
  with dashes, and editable. Hostnames a host owner listed in `origins` resolve to the service's
  address too. Any port on a service's address reaches that service's target.
- **DNS.** The app answers queries for its names and forwards every other query, unread and
  unlogged, to the network's normal DNS. iOS sends it only the matching domains; on Android every
  query passes through it. Answers are IPv4 only.
- **Packets to streams.** A small user-space network stack in C turns packets back into TCP
  connections and UDP flows and hands each to the engine's local listener for that service, as
  tun2socks does. From there it is the same stream or flow as without VPN mode: same protocol, same
  resume, same limits.
- **Sessions stay on demand.** VPN mode keeps the addresses up, not a session. A connection to a
  service's address wakes the session as a connection to a local listener does
  ([sessions](#sessions-and-reconnects)).
- **Android.** `VpnService` runs in the app's process as its foreground service. Turning VPN mode
  on asks for the system's one-time VPN approval.
- **iOS (M5).** A packet tunnel extension written in Swift runs the stack and the engine on Bare
  Kit directly (D34), within the extension's memory limit (about 50 MB; M1 measures the fit). Keys
  reach it through a keychain group shared with the app. While VPN mode is on, only the extension
  holds sessions, and the Flutter app shows its status through the system's app-to-extension
  messages.
- **In the app.** **Copy address** copies the name (`ssh.living-room.internal`), and **Open**
  loads the service by name, so each web service has its own origin. A service opened by a
  hostname in its `origins` also presents its real certificate, and name-based virtual hosts and
  OAuth redirects work.
- **Costs.** It takes the device's one VPN slot: another VPN app, WARP included, cannot run at the
  same time. The VPN icon shows, and the user approves it once. Google Play needs a `VpnService`
  declaration (decisions Q9), and Apple publishes VPN apps only from developers enrolled as an
  organization (Q14).

## Service kinds

The app shows **Open** only for services the host knows to be web services. The host decides; the
app never probes.

- **Explicit.** `holebridge service add --kind https|http|tcp|udp` is stored in `host.json` and
  never re-probed. A service of kind `tcp` or `udp` is never sent HTTP, and `udp` is always
  explicit.
- **Detected.** A service added without `--kind` is checked by the Go host itself, from the host,
  when it is added and at host start.
  1. It connects to the target (3 s connect timeout).
  2. It tries a TLS handshake plus one HTTP request, accepting self-signed certificates for the
     check (3 s response timeout).
  3. Then it tries plain HTTP the same way.
- **Results.**

  | Outcome | Kind |
  |---|---|
  | HTTP response over TLS | `https` |
  | HTTP response over plain TCP | `http` |
  | A plain-HTTP reply that says the port expects TLS | `https` |
  | The target stays open and sends non-HTTP bytes, or stays silent past the timeout on two probes | `tcp` |
  | Refused, timed out on connect, name not resolved, or accepted and then closed or reset | *inconclusive*: the previous kind is kept, or `unknown` if there is none |

- **Never downgraded on one probe.** A detected `https` or `http` never turns into `tcp` on a single
  probe; that takes two consistent results.
- **Retry while unknown.** Inconclusive services are re-checked when the next stream to them opens
  and once a minute until classified. Then the timer stops.
- **Persistence.** Detected kinds are saved in the host's state file next to `host.json`, so a
  restart starts from the last known kind.

```
 service added / host start / stream opened while unknown / 1-min timer while unknown
                                   │
                                   ▼
            connect (3 s) ── refused / timeout / no DNS / closed ──▶ inconclusive: keep kind
                │ open
                ▼
     TLS + HTTP (3 s) ── HTTP reply ──▶ https
                │ no
                ▼
     plain HTTP (3 s) ── HTTP reply ──▶ http   (a "TLS expected" reply ──▶ https)
                │ no
                ▼
     non-HTTP bytes, or silent twice ──▶ tcp   (never downgrades a known web kind on one probe)
```
- **Side effects.** Unlabeled services receive one TLS hello and one HTTP request per check. Owners
  who want no probe at all, e.g. for an SSH server watched by fail2ban, pass `--kind`.
- **In the app:**
  - `https` and `http` show **Open**;
  - `tcp` and `udp` show **Copy address**;
  - `unknown` shows **Copy address** and **Try opening**, which loads `https://` and falls back to
    `http://` on a TLS failure.

  Kinds reach the app in each session's handshake. A kind learned mid-session appears at the next
  session start, or on the M3 `services` push.

## The in-app browser

Web services open in HoleBridge's own browser by default. It fixes what breaks when tunneled
services share one loopback address:

- **Separate storage per service.** Cookies, local storage and caches are kept apart, so two
  services' logins never overwrite each other. Cookies are scoped by host, not port, so a shared
  127.0.0.1 store would collide.
- **Trust on first use for self-signed HTTPS.**
  - The first time a service presents a certificate the platform does not trust, the browser shows
    its SHA-256 fingerprint and asks once. It then pins that fingerprint for that service.
  - A pinned certificate is accepted even though it names the service's real hostname, not
    127.0.0.1.
  - A *different* certificate later shows a warning with both fingerprints and the choice to
    re-trust.
  - The service's settings show the pin and remove it.
- **Links that point at the service's real address.**
  - **By default** the browser maps navigation to the host's own LAN addresses (sent for the LAN
    route) at each service's port hint. That covers host-local targets only, and nothing when the
    LAN route is off.
  - **With `origins`,** a host owner can list a web service's own origins in `host.json`
    (`"origins": ["https://jellyfin.example"]`). Navigation to those is mapped onto the tunnel too.
    Only listed origins are ever sent, only for web kinds, only to key holders.
- **Known limits, out of the MVP's scope.** A 127.0.0.1 origin cannot fix:
  - services that require their real hostname in the `Host` header (name-based virtual hosts);
  - OAuth or SSO redirect URIs registered for the real hostname;
  - absolute subresource, XHR and WebSocket URLs to unlisted origins;
  - `Secure` cookies or secure-context APIs on plain `http`.

  On phones and TVs, [VPN mode](#vpn-mode-android-and-ios) removes most of these by giving each
  service its own name and, with `origins`, its real hostname.
- **Android TV.** A D-pad cursor mode: the arrows move a pointer, OK clicks, and a long press
  scrolls. Web service tiles also offer **Use the native app**, which copies the address for
  the service's own TV app.

**Where the in-app browser is not possible.** The M1 spike checks, per platform, for a usable
WebView with storage separated per service:

| Spike result | Open does |
|---|---|
| Pass | Opens the in-app browser |
| WebView present, no separation | Opens the in-app browser with one shared store, and warns that logins can collide |
| No usable WebView (Linux desktop, where Flutter has no first-party WebView; possibly some TVs) | Opens the system browser at the 127.0.0.1 address, with the same login warning. Trust pins and origin mapping are lost there. On a TV with no browser, the tile shows the address only. |

Plain HTTP on the loopback needs an explicit allowance on each platform, granted for 127.0.0.1 only:

- **Android:** a network security config that permits cleartext to `127.0.0.1` and nothing else.
- **iOS and macOS:** `NSAllowsLocalNetworking`.

## Platforms

**Host and relay (Go):** Linux, macOS and Windows on x64 and arm64, so the host also runs on
Raspberry Pi class boards and many NASes. One Go binary with pears-go inside; no `bare` runtime or
bundle (decisions D31). Pure Go with `CGO_ENABLED=0` (D32), so one build cross-compiles to every
target.

**App (Flutter):**

| Platform | Notes |
|---|---|
| Android phones | Android 10+ (flutter_pear_bare `minSdk` 29). **VPN mode** runs as a `VpnService`, the foreground service that keeps the app running ([VPN mode](#vpn-mode-android-and-ios)). The app drives the worklet's lifecycle itself through `BareWorklet` (it does not use flutter_pear's `Pear` class), so the worklet is not suspended while VPN mode is on. If Play rejects the `VpnService` declaration, VPN mode ships only in the APK from GitHub Releases. |
| Android TV, Google TV | Same APK. D-pad focus navigation, a leanback launcher entry, and the browser's cursor mode. Android 10+ only: older TVs, including Fire TV on Fire OS 7 (Android 9), are out. No camera: a host is added from a phone over the LAN ([handoff](#adding-a-host-to-a-tv-handoff)). After that, other hosts of the same deployment can be typed in three groups of three. VPN mode as on phones, so TV apps such as Jellyfin's reach services by name; M1 checks the VPN approval on a TV box. |
| iOS | iOS suspends a backgrounded app's networking. VPN mode (M5) runs in a packet tunnel extension; until then, or if it cannot fit, the tunnel works while the app is on screen, and the in-app browser covers web services. The M1 spike decides whether the engine and the network stack fit in the extension (pass rule in the [design](designs/p2p-tunnel-mvp.md)). LAN probes need the local network permission prompt. flutter_pear_bare on iOS is simulator-validated so far; real-device validation starts in M1. |
| macOS, Windows | flutter_pear_bare runs `bare` as a subprocess; the in-app browser uses WKWebView or WebView2. On macOS the app sandbox is off, so the subprocess can run, and LAN probes need `NSLocalNetworkUsageDescription`. |
| Linux desktop | As above, but Flutter has no first-party WebView, so **Open** uses the system browser ([fallback](#the-in-app-browser)). |
| Apple TV | Not planned: Flutter has no official tvOS support. |

## State, status and logs

- **Host:** `host.json` ([cli.md](cli.md#hostjson)) plus a state file of detected service kinds.
  One Go process per host; one DHT server per key (one in the MVP; scoped keys in M4 add more
  servers to the same DHT node).
- **Status (M3):** a local control socket (a Unix socket in the config directory, a named pipe on
  Windows) answers `holebridge status`: services, sessions and their routes, streams, bytes, NAT
  type.
- **App:** keeps the following in the platform's secure storage (on iOS, a keychain group shared
  with its own VPN extension), because a saved key is a secret:
  - the hosts you added and their last LAN addresses;
  - the application keys it was provisioned with;
  - the cached service list;
  - the local port each service got;
  - certificate pins;
  - the relay key.
- **Diagnostics (app).** The diagnostics screen shows:
  - the route tried and the NAT type;
  - whether a relay is set;
  - the last error code;
  - app, engine and protocol versions.

  **Copy diagnostics** and **Report a problem** (a prefilled GitHub issue) redact keys, addresses
  and service names.
- **Errors** carry a stable code (e.g. `HB-LOOKUP-TIMEOUT`) and link to its entry in
  `docs/errors.md`. Each entry states the problem, the cause and the fix.
- **Logs:** never a key or an application key, never payload bytes, never a DNS query forwarded in
  VPN mode, never anything derived from a key except public keys. This applies to the Go host, the
  relay, the app and the engine.
