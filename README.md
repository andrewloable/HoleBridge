# HoleBridge

Reach the services on your server from your TV, phone or laptop, anywhere, with a 9-character key.
No port forwarding, no account, no central server.

HoleBridge is a peer-to-peer tunnel built on the Holepunch stack (HyperDHT, UDX, Noise, Protomux),
like holesail.io. It is a tunnel, not a mesh: an app reaches the services one host shares, and
devices never join a network or see each other. On phones and TVs it works like a VPN app such as
Cloudflare WARP, except that the network behind it is peer-to-peer. Run the host on the machine that
has the services, open the app on any screen, and scan the host's QR code (a TV takes the host from
your phone).

> **Status: planning.** Nothing is implemented yet. This repository holds the design, the
> decisions behind it and the project plan. Commands and screens below show the **planned**
> interface.

## Why another tunnel

[Holesail](https://holesail.io) proved the idea: share a local port over the Holepunch DHT. HoleBridge
keeps that idea and changes the parts that get in the way day to day:

| | Holesail | HoleBridge |
|---|---|---|
| Services per running instance | one port | **many**, by name (`web`, `jellyfin`, `ssh`, ...), TCP and UDP |
| What you share | a long hex `hs://...` connection string | a **9-character key**: `7KQ-M4X-9TR` (your apps get the deployment's application key once, by QR) |
| License | AGPL-3.0 | **MIT** |

The client is an app for TVs, phones and desktops, and it picks the best route by itself:

- **Same network:** a direct LAN connection, found by a signed probe. It works without the internet
  and at full LAN speed.
- **Different networks:** a hole-punched peer-to-peer connection over the internet.
- **Both on mobile data:** through a relay you run yourself. The relay server is part of this
  project, and it cannot read your traffic.

Every TCP connection and UDP flow rides one encrypted session per host, so the fiftieth connection
costs no new handshake, and TCP connections survive a switch between Wi-Fi and mobile data. Planned
later: keys scoped to some of your services, expiring keys and an optional PIN
([roadmap](docs/roadmap.md)).

## Planned usage

On the machine with the services, install the host (or run it with Docker, see
[docs/cli.md](docs/cli.md#install)):

```sh
curl -fsSL https://holebridge.app/install.sh | sh
```

Add your services and start hosting:

```sh
$ holebridge service add web 127.0.0.1:8080
$ holebridge service add jellyfin 8096
$ holebridge service add ssh 22 --kind tcp
$ holebridge service add dns 53 --kind udp
$ holebridge host
Hosting 4 services: web (https), jellyfin (http), ssh (tcp), dns (udp)
Key: 7KQ-M4X-9TR
[QR code]  https://holebridge.app/k#7KQM4X9TR.<application key>
```

The install created your deployment's application key (`holebridge app-key` shows it). The QR code
and link carry it together with the 9 characters, so share them only with people you trust.

Or share one port right now with a temporary key:

```sh
$ holebridge share 8080
Sharing 127.0.0.1:8080 as "8080" (temporary key)
Key: 4HD-Q8N-2VW
[QR code]  https://holebridge.app/k#4HDQ8N2VW.<application key>
```

Scan the QR code with your phone (it opens the app, or the store if you don't have it yet). On a TV,
choose **Add from phone** and scan the code the TV shows with that phone: it hands the host over
your home network. An app that already holds your application key also takes the 9 characters
typed. Either way you see:

```
Living room server                                   ● LAN
  web        https   [Open]   [Use the native app]
  jellyfin   http    [Open]   [Use the native app]
  ssh        tcp     127.0.0.1:53817  [Copy address]
  dns        udp     127.0.0.1:5353   [Copy address]
```

Web services open in the app's own browser, each with its own logins. Other apps on the device (a
media player, an SSH client) use the `127.0.0.1` address shown. On phones and TVs, turn on **VPN
mode** instead: every app on the device then reaches the services by name, such as
`jellyfin.living-room.internal`, even with HoleBridge closed. Keys are case-insensitive and the
dashes are optional. Details: [docs/cli.md](docs/cli.md).

## How it works

```
 app (Flutter)                                           host (Go)
 ┌─────────────────────┐                        ┌──────────────────────┐
 │ UI                  │                        │ CLI, host.json       │
 │ engine (Bare)  ◀════╪═══ one Noise session ══╪═▶ pears-go (Go) ─────┼──▶ web, jellyfin, ssh
 └─────────────────────┘   LAN, direct or relay └──────────────────────┘
```

1. The host generates a random 9-character key. From it and the deployment's application key, both
   sides derive (with Argon2id) a **host** key pair, a **client** key pair and a LAN probe key.
2. The host listens on the HyperDHT and on its LAN under the host key pair, and admits only peers
   that connect with the client key pair, which only a key holder can derive.
3. The app tries the LAN first, then the DHT, which hole-punches a direct path or falls back to your
   relay. Whatever the route, the session is end-to-end encrypted, and the host's key pair proves to
   the app it reached a holder of the key. A session opens when you use a service and closes when
   you stop, and open TCP connections survive a switch between Wi-Fi and mobile data.
4. The host sends its list of services. The app opens a local port per service. Each TCP
   connection to it becomes one stream in the session, with per-stream flow control; each UDP
   sender becomes a flow.

The host and the relay are a single Go binary built on **pears-go**, a pure-Go implementation of
the Holepunch stack that HoleBridge builds, wire-compatible with the JavaScript one. The app runs
HoleBridge's own JavaScript engine, built from the original Holepunch libraries, in a Bare worklet
hosted by [flutter_pear](https://github.com/andrewloable/flutter_pear)'s runtime (unmodified). Both
speak the same HoleBridge protocol, checked by shared test vectors and Go↔JS interop tests. The
host only ever connects to the services it was configured with: an app cannot use it to reach
anything else.

Details: [docs/architecture.md](docs/architecture.md).

## About the key

A key has two halves:

- **The 9 characters** (45 random bits). This is the half you read out or type with a TV remote.
- **Your deployment's application key.** A 256-bit secret generated when you install the host. Your
  apps get it once: from the QR code or link, from your phone (on a TV), or because you built them
  with it.

Hosts and apps with different application keys never find each other, even with the same 9
characters, and without your application key nobody can even start guessing. Argon2id makes each
guess slow for anyone who does have it.

**Treat the QR code, the link and the application key like a Wi-Fi password.** Never expose an
admin page without its own login. If either half leaks, rotate it with `holebridge key --rotate` or
`holebridge app-key --rotate`.

The numbers and the threat model are in [docs/security.md](docs/security.md).

## Platforms (planned)

| Part | Runs on |
|---|---|
| Host and relay (`holebridge`) | Linux, macOS, Windows; x64 and arm64 |
| App | Android 10+ phones, Android TV and Google TV (with VPN mode), iOS (while the app is open; VPN mode planned for M5), macOS, Windows, Linux |

Over the internet, only outbound UDP is needed. The LAN route needs the host's LAN ports reachable
from its own network (your OS firewall may ask). A relay needs a server with a public IPv4 address.

## Documentation

| Doc | What it covers |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Components, routes (LAN, direct, relay), wire protocol, platforms |
| [docs/security.md](docs/security.md) | Key format and derivation, relay keys, threat model, rules for implementers |
| [docs/cli.md](docs/cli.md) | Host CLI, `host.json`, relay command, the app |
| [docs/decisions.md](docs/decisions.md) | Decision log and open questions |
| [docs/roadmap.md](docs/roadmap.md) | Milestones, exit criteria, issue tracking |
| [docs/designs/p2p-tunnel-mvp.md](docs/designs/p2p-tunnel-mvp.md) | The design behind the first release: goals, approaches considered, DX review |

## Credits

Built on [Holepunch](https://holepunch.to)'s open-source P2P stack. Inspired by Holesail; no
Holesail code is used.

## License

[MIT](LICENSE)
