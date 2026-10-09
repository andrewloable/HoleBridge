# Security

Status: design. The key derivation below is a **proposal until the owner commits its test vectors**.
The vectors are written and provisional. After the first release, changing any part of it
invalidates every key in use.

## The key

```
7KQ-M4X-9TR
```

- **9 symbols** from Crockford Base32: `0123456789ABCDEFGHJKMNPQRSTVWXYZ` (no `I`, `L`, `O`, `U`).
  Each symbol carries 5 bits, so a key is **45 random bits**.
- **Forgiving input.** Case-insensitive; dashes and spaces ignored; `O` reads as `0`, `I` and `L`
  as `1`. `U` is rejected. The same rule applies to the key part of a key link: the fragment is
  percent-decoded, then the key is normalized. The application key is not forgiving: it must be
  64 lowercase hex digits. Displayed in three groups of three, which also suits a TV remote.
- **No checksum.** All nine symbols are entropy. A mistyped key is simply another key: the app
  reports "no host found for this key" when the lookup gives up.
- **Generated, never chosen.** The host draws it from a CSPRNG. A key a person picks is a key
  someone else can guess. `holebridge key --set` exists only to move a host to a new machine with
  its existing key.

## Derivation

Every side derives the same values from two inputs: the 9-symbol **key** (the user's half) and the
**application key** (the deployment's half, below). Derivation happens in each engine: pears-go on
the host and relay (Go), and the app engine in `app/engine/` (JS). Dart never reimplements it. The
test vector runs in both engines' test suites, so the two implementations cannot drift apart. The
derivation freezes when the owner commits the test vectors.

```
appKey     = the deployment's 32-byte application key (see below)
normalized = the 9 canonical symbols, uppercase ASCII, no separators
salt       = BLAKE2b-128("holebridge key v1 salt" || appKey)
root       = Argon2id13(normalized, salt, opslimit = OPS, memlimit = MEM) -> 32 bytes
seed(role) = BLAKE2b-256("holebridge key v1 " + role || root), keyed with appKey
role       = "host" | "client" | "lan"   (host key pair, client key pair, LAN probe key)
```

The 9-symbol key is a seed. Argon2id and BLAKE2b stretch it into full 256-bit Ed25519 seeds, the
size a P2P identity needs. Stretching alone adds no randomness. The extra strength comes from the
application key being a secret only the deployment knows.

- **`OPS` and `MEM`** are 3 and 64 MiB (`MEM` is 67108864 bytes). The owner accepted this proposed
  cost on 2026-10-08 and chose to build without waiting for the device benchmark (HoleBridge-3vg).
  That benchmark still checks it on the slowest device we intend to support, most likely an Android
  TV box: derivation should stay under about one second there, and if it does not, the cost changes
  before the vectors freeze. They are written in code as numbers, never as a library's named
  constants, because a library default can change and the derivation must not.
- **A test vector** (a fixed key, a fixed test application key, and every derived public value) is
  in `spec/vectors/key-derivation.json`. A test fails if the derivation ever drifts. What freezes is
  the algorithm, not any deployment's application key. The vectors are provisional: the owner commits
  them once HoleBridge-3vg has run or the proposed cost is accepted, and the derivation freezes at
  that commit.
- **Normalization is the one rule implemented three times.** `holebridge key --set` (Go), key entry
  in the app (Dart) and the engine (JS) all normalize input. They share `spec/vectors/key.json`:
  inputs, normalized outputs and rejections. Every suite runs it, next to the derivation vector.
- **The salt is fixed per deployment:** it depends only on the application key, because a client
  has nothing else but the nine symbols.

### The application key

Each deployment has its own **application key**: 32 random bytes, set by whoever deploys HoleBridge
and known only to that deployment. Hosts and apps with different application keys derive different
DHT keys, so **with the same 9 symbols but a different application key, they never find each
other**.

- **Host and relay.** Generated at install or first run, or by `holebridge app-key --new`. Stored as
  `app.key` in the config directory, with the same `0600` rule as `host.json`. It can be changed on
  deployment (`app-key --set`, `app-key --rotate`) with no rebuild. The relay reads the same file,
  so a relay serves only its own deployment.
- **Apps get it one of three ways:**
  - **Built in:** a deployer's own client build compiles it in, read from a file the deployer passes
    at build time and never committed.
  - **Provisioned at pairing:** a store app receives it once from the host's QR code or link, which
    carries both halves, and keeps it in secure storage. After that, typing the 9 symbols of
    another host in the same deployment works too: the app tries each application key it holds.
  - **Handed off from a phone:** a TV, which cannot scan, receives a host and its application key
    from a phone that holds them, sealed to a one-time key the TV shows on screen
    ([architecture.md](architecture.md#adding-a-host-to-a-tv-handoff), decisions D36).
- **Never in the repository.** The repo holds only a fixed test value for the test vectors, under
  `spec/vectors/`.
- **Written as 64 lowercase hex digits** everywhere: the `app.key` file (followed by a newline),
  `holebridge app-key` output and `--set`, and the QR code and link
  (`https://holebridge.app/k#<9 symbols>.<64 hex digits>`).

What it gives, stated exactly:

| Attacker | Can they test key guesses? |
|---|---|
| Without the deployment's application key | **No.** They would have to guess 256 bits of application key first. Harvested host public keys are useless to them. |
| With it (a leaked QR or link, a published client build, access to the host's config) | Yes, at the 2⁴⁵ cost below, and only for that one deployment: no precomputation is shared across deployments. |

So the application key must be guarded like the host key:
- **QR code and link:** carry both halves; share them only with people you trust.
- **Client builds:** a build with the key compiled in must not be published.
- **Leaks:** if it leaks, `holebridge app-key --rotate` replaces it, and every app must be
  re-provisioned or rebuilt.

### Relay keys

A relay key has the same 9-symbol format and is generated by `holebridge relay --new-key`. It is
derived the same way, with the deployment's application key and its own labels (`"holebridge relay
v1 salt"`, `"holebridge relay v1 " + role`), into two key pairs. A relay therefore serves only its
own deployment:

| Role | Used for |
|---|---|
| `server` | The relay listens under it, so members know which relay to dial. |
| `member` | Hosts and apps set it as their DHT `defaultKeyPair`. The relay admits only this public key, so a relay serves its owner's hosts and apps and nobody else's. |

A relay key is separate from host keys: one relay serves every host and app its owner configures,
and changing a host key never touches the relay. Someone who has a relay key can use the relay. They
still cannot reach any host without that host's key.

## How hard is a key to guess?

**Everything in this section assumes the attacker already has the deployment's application key**
(above); without it, guessing cannot even start.

There are 32⁹ = 35,184,372,088,832 keys (about 3.5 × 10¹³, or 2⁴⁵). Holesail's keys are 256 bits
and cannot be guessed at all. HoleBridge gives that up so a person can read a key aloud or type it
with a TV remote, and makes up part of the gap with Argon2id.

The figures below assume an attacker spends **0.1 core-seconds per Argon2id guess** (memory-hard,
so GPUs help much less than with plain hashes).

| Attack | What the attacker needs | Expected cost |
|---|---|---|
| **One specific host** | Its host public key (seen on the DHT) | 1.8 × 10¹³ guesses ≈ **56,000 core-years** |
| **Any of the deployment's M hosts, offline** | Their host public keys, harvested from the DHT | ≈ 3.5 × 10¹³ / M guesses: **about 22,000 core-years** at M = 5 |
| **Online, no harvested keys** | A DHT lookup per guess, which takes seconds and hundreds of packets | ≈ 3.5 × 10¹³ / M lookups: hundreds of years even at 1,000 lookups per second, and a visible flood |
| **On the LAN** | One captured probe or reply from the host's network | The MAC is an offline verifier, like the host public key: the same 1.8 × 10¹³ guesses for that host. Without a capture, guessing means a probe per candidate, which the responder never answers |

Read that honestly:

- **Targeted guessing is not practical.**
- **Opportunistic guessing stays inside one deployment.** The salt is shared by the hosts of one
  deployment, so an attacker who has that deployment's application key can check one Argon2id
  computation against all of its harvested host public keys. A deployment has a handful of hosts,
  not thousands, so adoption elsewhere does not make anyone's keys cheaper to guess.
- **So:** the key keeps strangers from reaching your services. It is not the authentication for a
  sensitive service. Keep a password on the NAS, keys on SSH, a login on the admin panel.

A leaked key is answered by rotation from M2 on: `holebridge key --rotate` replaces it, and the
old key stops working when the host restarts (M3: when it reloads). A leaked application key is
answered by `holebridge app-key --rotate` and re-provisioning every app. Planned in M4: stronger keys
for those who want them, a PIN and expiring keys. See below.

**When this is decided.** Key length is part of the derivation, which freezes when the owner commits the test vectors. The owner decided it on
2026-10-08 (Q2, D38): 9 symbols by default, with strong keys and a PIN in M4. The extra secret is
settled as the application key (D35). Before the M2 build, the owner also decides whether scoped
keys join the MVP as its minimum
access-control floor. If 45 bits stays and scoped keys stay in M4, the accepted risk is that every
key holder reaches every service. The mitigations are rotation, services keeping their own login,
and the README's advice never to expose an unauthenticated admin page.

## What a key holder can do

Everyone with the key is a trusted equal:

- They can use every service the key covers.
- They learn the host's LAN addresses (sent in the handshake for the LAN route) and any `origins` the
  owner listed for a web service. They never learn a target's address otherwise.
- They can derive the **host** key pair too, so they could run an impostor host under the same
  key. Apps would reach whichever host they find first.

**Host identity pinning (M4)** closes the last point for apps that have connected before. The host
keeps a separate, random, long-term identity key pair and signs each session's Noise handshake hash
with it in its channel handshake. An app pins that identity on first contact, as SSH does with
`known_hosts`, and refuses a host that cannot sign. The signature covers the handshake hash, so it
cannot be replayed into another session.

## Planned hardening options (M4)

| Option | What it does |
|---|---|
| **Scoped keys** | Extra keys that each cover only some services: give a friend `minecraft`, not `nas`. Each is its own DHT server on the same host process. |
| **Expiring keys** | A key that stops working at a set time. For one-off sharing. |
| **PIN** | A second secret per key, sent inside the encrypted session to an already-authenticated host, compared in constant time, failures rate-limited. It never leaves the session and nothing derived from it is published, so it can only be guessed online, slowly. Pair it with identity pinning, or an impostor host could collect PINs. |
| **Strong keys** | 18 symbols (90 bits), same alphabet and derivation, for hosts that want no guessing risk at all. |

## What others can see

| Who | Sees | Does not see |
|---|---|---|
| DHT nodes | The host public key, the IP addresses of host and apps, connection times | The key, service names, any traffic |
| Devices on the host's LAN | That something listens on the LAN port, and probe traffic | That it is HoleBridge: the responder is silent to anything not signed with the key |
| An owner-run relay | IP addresses, connection times and volumes | Any traffic: the session is end-to-end encrypted |
| Other apps on the app's device | They can reach the services, through VPN mode or the 127.0.0.1 listeners, like any local client | The keys: they never leave HoleBridge |
| Devices on the TV's LAN during a handoff | A TCP port open on the TV for up to 5 minutes, and one sealed box | The host or the application key: only the TV can open the box |
| Someone who learns the host public key only | That a host exists; they can attempt connections | Anything else. The host turns them away without the client key pair. |

## Rules for implementers

1. **Never log** a key, the application key, derived secrets, PINs, resume tokens, payload bytes or
   DNS queries forwarded in VPN mode.
   Public keys are fine to log. Error messages and the app's diagnostics also redact addresses and
   service names.
2. **Randomness** comes from `crypto/rand` (Go), `sodium` (engine) or `Random.secure` (Dart) only.
3. **`host.json` holds the key, `app.key` the application key and `relay.key` the relay key.** The Go
   host and relay write them with mode `0600` and, on POSIX systems, refuse to start if one is
   readable by group or others, as `ssh` does with private keys. `relay check` refuses a relay key
   file that others can read too.
4. **The app stores keys and application keys in the platform's secure storage** (Keychain, Android
   Keystore-backed storage, the desktop equivalents), never in plain preferences. On iOS the
   keychain group is shared only with HoleBridge's own VPN extension. On Android, app backup is off
   and the secure storage preference files are excluded from device-to-device transfer, because their
   wrapping key never leaves the device: a restored copy could not be read, and the ciphertext would
   leave the device.
5. **In the app, keys and application keys reach the worklet over its IPC**, never on a command
   line or in the environment.
6. **Compare secrets in constant time:** MACs, resume tokens, PINs and the handoff secret.
7. **Forward only to configured targets.** An app names a service, never an address, for TCP and
   UDP alike. The host must never become an open proxy or a UDP reflector: each UDP flow's host
   socket is connected to the service target, and datagrams from any other source are dropped.
8. **App listeners bind `127.0.0.1`.** Other devices on the app's network cannot use a tunnel unless
   the user turns that on per host, with a warning that says so. The one exception is the TV's
   handoff listener (D36): it carries no service, lives at most 5 minutes and accepts only a box
   sealed to it. In VPN mode (D37) the service addresses live only on the device's VPN interface,
   which routes nothing in from the network.
9. **Cap every resource** ([architecture.md](architecture.md#limits)) and treat malformed input as
   a value, never an exception. The LAN responder never answers with more bytes than it received.
10. **Pin exactly:** flutter_pear_bare and the app engine's Holepunch dependencies (lockfile) in the
    app; the Go module's dependencies (`go.sum`) in the host. The key derivation depends on
    `sodium-universal` (JS) and pears-go's Go crypto producing identical output; the shared test
    vector guards both.
11. **Clean room.** Holesail is AGPL-3.0 and HoleBridge is MIT. Read Holesail's docs, never copy its
    code.
