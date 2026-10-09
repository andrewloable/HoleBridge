# Host CLI, configuration and the app

Status: design. This is the interface the M2 MVP builds; later milestones are marked.

The Go binary `holebridge` hosts services and runs relays. Connecting to a host is the app's job
([The app](#the-app)).

## Install

On the machine with the services, either:

```sh
curl -fsSL https://holebridge.app/install.sh | sh
```

The script detects the OS and architecture, downloads the release archive, checks its checksum, and
installs the single `holebridge` binary (pure Go with pears-go compiled in; no other runtime). Or
with Docker:

```sh
docker run -d --name holebridge --restart unless-stopped --network host \
  -v holebridge:/config ghcr.io/andrewloable/holebridge host
```

Host networking lets the LAN route and the targets on the host's network work as they would on bare
metal. The restart policy brings the host back after a reboot. Host networking behaves this way on
Linux; on Docker Desktop for macOS and Windows, run the installed binary instead. The `/config`
volume holds `host.json` and `app.key`, so keep it across upgrades.

- **Bare-metal installs:** start-at-boot units for systemd, launchd and Windows arrive with M6
  packaging.
- **Upgrading:** run the same install command again, or pull the new image. Check the result with
  `holebridge --version`.

The domain `holebridge.app` and the image path above are placeholders until the project registers
them (decisions Q8).

## Commands

| Command | What it does |
|---|---|
| `holebridge share <target> [--name <n>] [--kind <k>]` | Host one service right now with a **new, temporary key**, printed with its QR code and link. The key dies with the process; the deployment's application key stays. |
| `holebridge host` | Run the host with the services in `host.json`. |
| `holebridge service add <name> <target> [--kind https\|http\|tcp\|udp] [--idle <duration>]` | Add a service to `host.json`. Without `--kind`, the host detects it ([service kinds](architecture.md#service-kinds)). |
| `holebridge service rm <name>` | Remove one. |
| `holebridge service ls` | List them with their kinds. |
| `holebridge key` | Print the host key, its QR code and its link (creates the key on first use). |
| `holebridge key --rotate` | Replace the key. On a running host it says so: the old key stays active until `holebridge host` restarts (M3: reloads). |
| `holebridge key --set <key>` | Use an existing key, to move a host to a new machine. |
| `holebridge app-key` | Print the deployment's application key (a secret). Created at install or first run. |
| `holebridge app-key --new` / `--rotate` / `--set <hex>` | Create, replace or set it. Rotating cuts off every app until it is re-provisioned or rebuilt. |
| `holebridge relay --new-key` | Create `relay.key` in the config directory with mode `0600` (the key as `XXX-XXX-XXX` and a newline), and print the key once. |
| `holebridge relay` | Run a relay with the key in `relay.key`. |
| `holebridge relay check <relay-key-file>` | Test a relay from another machine with the same `app.key`: a member is let in, a stranger turned away. |
| `holebridge status` | M3: what a running host or relay is doing. |

Global options: `--config <dir>`, `--log-level <error|warn|info|debug>`, `--version`, `--help`.

Exit codes: `0` success, `1` failure, `2` bad usage. Every error message states the problem, the
cause and the fix, and carries a code that links to its entry in `docs/errors.md`. That page is
generated from one catalog, `spec/errors.json`, shared by the Go host, the app and the engine; CI
fails on any `HB-` code missing from it. Example:

```
error HB-CONFIG-PERMS: host.json can be read by other users (mode 0644).
  The file holds the host key.
  Fix: chmod 600 ~/.config/holebridge/host.json
  More: https://holebridge.app/errors#hb-config-perms
```

### Targets

A target is where the **host** forwards a service:

| Written | Means |
|---|---|
| `8080` | `127.0.0.1:8080` |
| `192.168.1.20:445` | that address on the host's network |
| `nas.local:445` | resolved by the host when a stream opens |
| `[::1]:8080` | IPv6 |

### Service names

Lowercase letters, digits and `-`, 1 to 32 characters, starting with a letter or digit. `share`
names its single service after the target port (`8080`) unless `--name` says otherwise.

## Hosting

```sh
$ holebridge service add web 127.0.0.1:8080
$ holebridge service add jellyfin 8096
$ holebridge service add ssh 22 --kind tcp
$ holebridge service add dns 53 --kind udp
$ holebridge host
Hosting 4 services: web (https), jellyfin (http), ssh (tcp), dns (udp)
Key: 7KQ-M4X-9TR
     ▄▄▄▄▄▄▄ ▄▄ ▄▄▄▄▄▄▄
     █ ▄▄▄ █ ▄█ █ ▄▄▄ █      Scan with your phone, or open:
     █ ███ █ ▀▄ █ ███ █      https://holebridge.app/k#7KQM4X9TR.<application key>
     ▀▀▀▀▀▀▀ ▀▀ ▀▀▀▀▀▀▀
LAN: listening on 192.168.1.10
Internet: reachable (NAT: consistent)
Relay: none set
```

**The QR code and link carry both halves of the key after the `#`:** the 9 symbols and the
deployment's application key (`https://holebridge.app/k#7KQM4X9TR.<application key>`). Browsers
never send that part to the web server, so holebridge.app never sees either. Share the QR code and
the link only with people you trust. Opening the link:

- opens the app with the key filled in, if the app is installed;
- otherwise opens a static page that sends you to the right app store and says "after installing,
  scan or open this link again". The stores never pass a link into a freshly installed app, so the
  app's first screen opens on **Scan QR** and **Paste link** (on a TV, **Add from phone**).

The same page serves the TV handoff link (`/h#...`). It holds no state, logs nothing, loads no
third-party scripts (any script on the page could read the key) and sets a strict Content Security
Policy.

`host` runs in the foreground; under Docker the restart policy keeps it up. Before M3, changes to
`host.json` need a host restart, and `service add` on a running host says so. From M3 the host
reloads on `SIGHUP` or `holebridge service add/rm`, and pushes the new list to connected apps.

## Running a relay

For when both ends are on mobile data or other networks that cannot hole-punch. On a server with a
public IPv4 address and an open UDP port range:

```sh
$ holebridge relay --new-key
Relay key: R4N-W8P-2KD   (saved to ~/.config/holebridge/relay.key, mode 0600)
$ holebridge relay
Relay public key 4f1c...e9
Public UDP 203.0.113.7:49737 firewalled=false randomized=false
```

- `firewalled=true` means the UDP ports are not open yet.
- `randomized=true` means the server is behind a NAT that changes ports, so it cannot be a relay.

Then put the same relay key in `host.json` (`relay`) and in the app (Settings > Relay). The full VPS
walkthrough is `docs/relay.md`, written with the MVP.

## Configuration

### Location

| Platform | Directory |
|---|---|
| Linux, macOS | `$XDG_CONFIG_HOME/holebridge`, else `~/.config/holebridge` |
| Windows | `%APPDATA%\holebridge` |
| Docker | `/config` (a volume) |

`--config <dir>` or `HOLEBRIDGE_CONFIG` overrides it. Two hosts on one machine use two directories
and two sets of LAN ports; a port already taken fails with `HB-LAN-PORT-IN-USE` and the fix.

### `host.json`

```json
{
  "key": "7KQ-M4X-9TR",
  "services": {
    "web": { "target": "127.0.0.1:8080" },
    "jellyfin": { "target": "127.0.0.1:8096", "origins": ["http://jellyfin.example:8096"] },
    "ssh": { "target": "127.0.0.1:22", "kind": "tcp", "idle": "8h" },
    "dns": { "target": "127.0.0.1:53", "kind": "udp" }
  },
  "lan": { "enabled": true },
  "relay": "R4N-W8P-2KD",
  "limits": {}
}
```

| Field | Default | Notes |
|---|---|---|
| `key` | created on first `key` or `host` | A secret. The file must be mode `0600` ([security.md](security.md#rules-for-implementers)). |
| `services.<name>.target` | required | See [Targets](#targets). |
| `services.<name>.kind` | detected | Set it to skip detection; `tcp` and `udp` services are never probed with HTTP. `udp` must be set explicitly. |
| `services.<name>.origins` | none | Web services only: the origins the service uses in its own absolute links, sent to key holders so the in-app browser can map them. |
| `services.<name>.idle` | off | Close a stream after this long with no bytes either way. For `udp` services it replaces the 60 s flow idle timeout. |
| `lan.enabled` | `true` | The LAN responder and listener ([architecture.md](architecture.md#lan-route)). Turn off on untrusted networks. The host's OS firewall must allow these ports from its LAN. |
| `lan.discoveryPort`, `lan.port` | chosen in M1 | UDP probe port and TCP session port. |
| `relay` | none | A relay key. The apps need the same one. |
| `limits` | see [architecture.md](architecture.md#limits) | Overrides, e.g. `{ "sessionsPerKey": 8 }`. |

The CLI writes a `lan` or `limits` value only when it differs from its default or the file already
holds it, so a later release that changes a default reaches every host that never set it.

Detected kinds live in a state file next to `host.json`, not in it. The application key is not in
`host.json` either: it lives in `app.key` in the same directory (mode `0600`, shared by the host and
the relay, [security.md](security.md#the-application-key)).

JSON, not YAML or TOML: Go reads it with the standard library, and the CLI's `service` commands
write it, so few people edit it by hand. Scoped keys, expiry and PINs (M4) extend this file; their
shape is decided then.

## The app

One Flutter app for TVs, phones and desktops.

**Adding a host.** An app needs both halves, the 9 symbols and the deployment's application key:
- **Phones and laptops:** scan the host's QR code or open its link. Both carry both halves.
- **Typing the 9 symbols** (three groups of three, built for a remote; case and dashes do not
  matter) works once the app already holds that deployment's application key: from an earlier
  pairing or handoff, or compiled into a self-built app. The app tries each application key it holds.
  The key part of a key link reads the same way, after the fragment is percent-decoded
  ([security](security.md#the-key)). The application key in a link is always 64 lowercase hex digits.
- **TV:** choose **Add from phone**. The TV shows a QR code; scan it with a phone that already has
  the host, pick the host, and the phone sends it, application key included, over your home
  network. Both must be on the same network
  ([handoff](architecture.md#adding-a-host-to-a-tv-handoff)).

The app keeps keys and application keys in secure storage.

**Connecting.** The app tries the LAN first, then the internet, and opens a session on demand
([sessions](architecture.md#sessions-and-reconnects)).

```
Living room server                                   ● LAN
  web        https   [Open]   [Use the native app]
  jellyfin   http    [Open]   [Use the native app]
  ssh        tcp     127.0.0.1:53817  [Copy address]
  dns        udp     127.0.0.1:5353   [Copy address]
```

- **Route badge:** LAN, Direct or Relay. "Looking for host..." while a lookup runs; "Can't reach
  host" (with its error code) only when it has given up. The app retries on its own.
- **Open** loads the service in the in-app browser with the matching scheme
  ([in-app browser](architecture.md#the-in-app-browser)). On Linux desktop it uses the system
  browser. On a TV the browser has a D-pad cursor.
- **Use the native app**, on every web service, copies the address for the service's own app,
  e.g. Jellyfin's TV app.
- **Copy address** is for non-web services: point an SSH client, database tool or game at it.
  For `udp` services it copies the local UDP port. With VPN mode on, it copies the service's name
  instead (`ssh.living-room.internal`). Services of kind `unknown` also offer
  **Try opening**.
- **Local ports** (without VPN mode). For each service the app tries the service's own port (sent as
  a hint; the target address never is). If that port is taken or not allowed, the operating system
  picks a free one, and the app keeps it per host and service so the address you saved in other apps
  stays valid. You can set a port by hand.

**Settings:**

| Setting | Does |
|---|---|
| **Relay** | Use my relay: the relay key. Needed only when both ends are on networks that cannot hole-punch, such as mobile data on both sides. |
| **VPN mode** | Android now, iOS from M5: the app works like a VPN app. Every app on the device reaches the services by name (`jellyfin.living-room.internal`), even with HoleBridge closed; nothing else goes through it, and it sends nothing while idle. Takes the device's one VPN slot ([architecture.md](architecture.md#vpn-mode-android-and-ios)). |
| **Share with my network** | Without VPN mode, per host, off by default: bind `0.0.0.0` instead of `127.0.0.1`, so other devices on this network can use the services. Shows a warning. |
| **Diagnostics** | The route tried, NAT type, relay status, last error code and versions. **Copy diagnostics** and **Report a problem** (a prefilled GitHub issue) never include keys, addresses or service names. |
