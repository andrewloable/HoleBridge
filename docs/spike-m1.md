# M1 spike report

## Direct dial (testnet)

Question: can the app engine (hyperdht in Bare) dial a host that listens under a key-derived key
pair, with a server firewall that admits only the client key pair?

**Verdict: PASS** against the PASS RULE. The right client connected 20 of 20 times on the testnet
in each of two runs. The wrong client never connected (0 of 3 attempts in each run).

### Setup

- Device: one Mac, Apple silicon (arm64), macOS 27.0.1. Node v24.15.0, npm 11.12.1.
- Packages, pinned exactly in `spikes/direct-dial/package.json`: hyperdht 6.34.1, sodium-universal
  5.0.1, b4a 1.9.0, and bare 1.34.1 as a dev dependency.
- Bare: the client runs under the repo-local bare 1.34.1, the same version `app/engine` pins. The
  global bare 1.30.3 on PATH cannot load this tree: requiring sodium-universal fails with
  UNSUPPORTED_ENGINE, because sodium-native, then bare-assert, then bare-type needs bare >= 1.32.0.
- Testnet: `hyperdht/testnet` `createTestnet(10)`, 10 DHT nodes on 127.0.0.1. The testnet and the
  host server run in one Node process (`spikes/direct-dial/run.js`). The client runs in a bare
  child process (`spikes/direct-dial/client.js`).
- Networks: local only. Loopback, with no LAN, no internet route, no NAT and no hole punch across
  a router.
- Keys: the host, client and wrong-client key pairs come from fixed label seeds (BLAKE2b-256 of a
  label), made with sodium-universal `crypto_sign_seed_keypair`. These are spike fixtures, not
  secrets. No seed is printed or written to a file.

### Steps run

1. Derive the host, client and wrong-client key pairs (`keys.js`).
2. Start the testnet. Start the server with `createServer({ firewall })`, where the firewall
   returns true (drop) for every remote key except the client public key. Listen on the host key
   pair.
3. Right client under bare: `dht.connect(hostPublicKey, { keyPair: client })`. Connect time runs
   from the dial call to the secret stream's `connect` event. Then send a 1 MB random payload, which
   the host echoes, and byte-compare the echo. Done for all 20 connects.
4. Wrong client under bare: the same dial with the wrong key pair, 3 attempts with a 20 s window
   each.
5. Control, not in the spec: the wrong client again, with the firewall admitting every key. This
   shows the firewall is the only thing refusing the wrong client.

### Measurements

Connect is the dial call to `connect`, in ms. Echo is the 1 MB round trip, in ms (1 MB up, 1 MB
back). p50 and p95 use nearest rank over the 20 values.

| Run | Right client | Connect p50 / p95 | 1 MB echo p50 / p95 | Wrong client |
|---|---|---|---|---|
| main (`results.txt`) | 20 of 20 | 2 / 3 | 41 / 43 | 0 of 3, error after about 8.0 s each |
| repeat (`results-run2.txt`) | 20 of 20 | 2 / 5 | 41 / 48 | 0 of 3, error after about 8.0 s each |
| control, firewall open (`results-control.txt`) | 20 of 20 | 2 / 4 | 41 / 47 | 3 of 3 connected, in 2 to 6 ms |

Other raw values:

- Connect min / max / mean: main 1 / 9 / 2.3 ms; repeat 1 / 10 / 2.8 ms.
- 1 MB echo min / max / mean: main 34 / 56 / 41.2 ms; repeat 33 / 55 / 42.6 ms.
- Client DHT bootstrap: 4 ms, measured separately and not in the connect time.
- Server counters, right run: 20 connections, 20 admitted handshakes, 0 rejected, 20 MiB echoed
  (1 MiB each way per connect).
- Server counters, wrong runs: 0 connections, 3 rejected handshakes. The firewall dropped every
  one.

### What the wrong client saw

The host never replies to a rejected handshake. The wrong client got no connection and no refusal
message. Its dial failed with `PEER_CONNECTION_FAILED` about 8.0 s after the attempt started, in
all three attempts of both runs. The control shows that with the firewall open, the same wrong key
pair connects at once. So the refusal comes from the firewall, not from the key pair being unusable.

### Caveats

- Timing uses `Date.now()`, which has 1 ms resolution. A p50 of 2 ms is coarse.
- n = 20 per run, and two main runs. These are loopback numbers. A DHT lookup across the internet
  will take far longer, so treat these values as a floor, not as an estimate for real networks.
- Hole punching across NAT was not exercised. The direct route's punch step is untested here.
- A rejected dial takes about 8 s to fail, not an instant refusal. The app must time out and show
  that, rather than wait for a refusal that never comes.

### Follow-ups

None required, because the PASS RULE passed. The bare version finding is recorded above. The
global bare on this machine is too old for hyperdht 6.34.1. `app/engine` already pins bare 1.34.1,
which matches the repo-local runtime used here. D2 (dial a public key) is not contradicted by
this run.

### Files

`spikes/direct-dial/`: `run.js` (driver), `client.js` (bare client), `keys.js` (fixed-label key
pairs), `package.json` and `package-lock.json` (exact pins), `.gitignore` (node_modules),
`results.txt`, `results-run2.txt` and `results-control.txt` (raw output). Run it with
`cd spikes/direct-dial && npm ci && node run.js`.

## Unordered datagrams

Question: which routes carry unordered datagrams over the encrypted session (`send`, `trySend` and
the `message` event of @hyperswarm/secret-stream), and what is the largest datagram that fits one
UDX packet after encryption (maxDatagram)?

**Verdict: PASS** against the PASS RULE. The direct route and the blind-relay route both carry
unordered datagrams in both directions, and maxDatagram is measured: 1156 bytes on both routes.
That is 44 bytes less than the 1200 in the architecture default (see Findings). Plain TCP, the LAN
route, has no unordered path.

### Setup

- Device: one Mac, Apple silicon (arm64), macOS 27.0.1, loopback interface MTU 16384. Node v24.15.0,
  npm 11.12.1.
- Packages, pinned exactly in `spikes/unordered-datagrams/package.json`: @hyperswarm/secret-stream
  6.9.2, hyperdht 6.34.1, blind-relay 1.6.1, udx-native 1.21.3, sodium-universal 5.0.1, b4a 1.9.0,
  bare-tcp 2.6.1, and bare 1.34.1 as a dev dependency. Every pin matches `app/engine` except
  blind-relay, which the engine does not depend on.
- Bare: the client runs under the repo-local bare 1.34.1, not the global bare on PATH.
- Testnet: `hyperdht/testnet` with 10 DHT nodes, all bound to 127.0.0.1. Relay: a blind-relay
  server on a DHT node bound to 127.0.0.1. Both are on localhost.
- Host: a DHT node with `firewalled: true` (no public address), listening on a fixed-label key
  pair with `shareLocalAddress: false`. The relay route also sets `holepunch: false`, so the host
  never answers a hole punch and only the relay can carry the connection.
- Client: under bare, `localConnection: false` (the LAN shortcut is off), bound to 127.0.0.1. The
  relay route passes `relayThrough` set to the relay's key.
- Networks: loopback only. No LAN, no router, no NAT, no internet route. No packet leaves the machine.
- Keys: fixed label seeds (BLAKE2b-256 of a label) through sodium-universal, as in the direct-dial
  spike. These are spike fixtures, not secrets.
- Route check: the direct route has no relay in play and the host has no public address, so the
  only path is the hole-punched UDX connection. On the relay route, blind-relay's counters were read
  after all measurements and before teardown: 1 pairing matched, 2 relay streams active, 0 closed.
  The connection stayed on the relay for the whole run. A switch to a direct path would have closed
  the relay streams.

### Steps run

1. Testnet and host on localhost. The bare client dials the host over the direct route, then over
   the relay route.
2. Warm-up: resend a 16-byte probe with `trySend` until one echoes. The time from connect to the
   first echo is recorded.
3. Both directions: the client sends each test message with `send` (awaited) or `trySend`,
   alternately. The host echoes each one with `trySend`, and the client byte-compares every echo.
   Each size gets 4 trials, 2 of each method.
4. Sweep from 16 to 60000 bytes. Then a binary search between the largest size that always arrives
   and the first size that does not.
5. maxDatagram: the on-wire size of each unordered packet, read from the receiver's `bytesReceived`
   (a sent unordered message does not move `bytesTransmitted`). The overhead is the wire size minus
   the payload, from a 1000-byte probe. maxDatagram is stream.mtu minus that overhead. The probe
   runs at maxDatagram and at maxDatagram plus 1.
6. Plain TCP (step 4): two secret streams over loopback TCP. Each side calls `send` and `trySend` 4
   times with 100-byte messages, then counts `message` events for 1.5 s. An ordered write is the
   control. Run under node and under bare with bare-tcp.

Each route ran twice (`results-*-run2.txt`). The numbers match, except the relay connect time
(16 ms and 15 ms).

### Measurements

| Measure | Direct | Relay |
|---|---|---|
| Connect, dial to secret-stream `connect` | 8 ms (both runs) | 16 ms and 15 ms |
| First unordered echo after connect | 1 ms | 1 ms |
| Payloads 16 to 2000 bytes, every trial arrives both ways | yes, 4 of 4 at each size | yes, 4 of 4 at each size |
| Largest payload that arrives | 2004 bytes (2005 fails) | 2004 bytes (2005 fails) |
| Payloads 2040 bytes and up | never arrive (0 of 4 each); `send` still resolves true | same |
| Packet on the wire for one payload | payload + 44 bytes, one packet | same |
| stream.mtu | 1200 | 1200 |
| **maxDatagram: largest payload in one 1200-byte UDX packet** | **1156 bytes** (packet 1200) | **1156 bytes** (packet 1200) |
| 1157 bytes (packet 1201) | arrives | arrives |

Raw wire sizes, same on both routes: 1000 bytes gives a 1044-byte packet, 1156 gives 1200, 1157 gives
1201, and 2004 gives 2048.

### What the numbers mean

- An unordered message is one UDX packet of payload + 44 bytes. The 44 bytes are the secret-stream
  envelope (24 bytes: an 8-byte nonce prefix and a 16-byte MAC) and the UDX packet header (20 bytes).
  The split is inferred from the sizes; the total is measured.
- libudx does not enforce stream.mtu on unordered messages. A 1157-byte payload goes out as a
  1201-byte packet and arrives. The 1200 cap is a convention, so the product must enforce
  maxDatagram itself.
- The 2004-byte ceiling is on the receiver. udx-native's `lib/stream.js` sets MAX_PACKET to 2048,
  and a bigger packet is dropped with no error on the sender side: `send` resolves true.
- IP fragmentation cannot be seen on this machine, because lo0 has MTU 16384. The 1156 figure comes
  from the packet arithmetic. A 1200-byte UDX packet fits the IPv6 minimum MTU of 1280 with 48 bytes
  of IPv6 and UDP headers to spare (1232 bytes of payload). A 1244-byte packet, which a 1200-byte
  payload would produce, does not fit.

### Findings for the architecture doc

- The default maxDatagram of 1200 in docs/architecture.md (UDX services, Size) does not fit a
  1200-byte UDX packet after encryption: a 1200-byte payload is a 1244-byte packet. The measured
  value is 1156. HoleBridge-uk4 replaces the "M1 confirms" wording with this number (its step 2).
- The sender cannot see a dropped oversize datagram: `send` resolves true at 2040 bytes and up, and
  nothing arrives. The architecture's rule to drop and count anything over maxDatagram has to hold
  on the sending side, before the send.
- Only the LAN route lacks an unordered path, so it alone uses message 10.

### TCP (LAN route)

- A plain TCP secret-stream has no unordered path. Its raw stream has no `send` or `trySend`, so the
  secret-stream calls return undefined and send nothing. Over 1.5 s, 8 unordered calls per side (100
  bytes each) gave 0 `message` events on either side. The ordered control write arrived (21 bytes).
  The result is the same under node and under bare-tcp 2.6.1 (`results-tcp.txt`,
  `results-tcp-bare.txt`).
- So the LAN route carries datagrams as message 10 on the ordered channel, as architecture.md says.

### Caveats

- Loopback only. NAT and a real hole punch across a router were not tested. Two loopback nodes punch
  trivially, so the connect times are a floor, not an estimate for real networks.
- The relay route is forced: the host refuses to hole punch. A real owner-run relay is used after a
  punch fails, which the spike does not exercise. The relay check uses blind-relay's own counters,
  not a packet capture.
- Wire size is read from the receiver's byte counters on the echo, because the sender-side counters
  do not move for unordered sends. The overhead is measured, not derived from the source.
- An early exploratory relay run, made before the warm-up was added, lost the first unordered
  message sent right after `connect`. The recorded runs did not show this: the first probe echoed in
  1 ms in all four runs. The app must not assume the first unordered datagram after connect arrives.
  In the architecture, the first datagram of a flow rides the ordered `flow` message anyway.
- The client runs under bare. The host and the driver run under Node. Nothing was tested on a phone,
  a TV, Wi-Fi or any real device.

### Follow-ups

None required, because the PASS RULE passed. The IF IT FAILS path did not apply. The default change
(1200 to 1156) is folded into HoleBridge-uk4 per its step 2.

### Files

`spikes/unordered-datagrams/`: `run.js` (driver: testnet, host and relay; `node run.js direct|relay
[run-tag]`), `client.js` (bare client), `payload.js` (test payloads with a sequence number and a
pattern), `keys.js` (fixed-label key fixtures), `tcp.js` (plain TCP check, run under node or bare),
`package.json` and `package-lock.json` (exact pins), `.gitignore` (node_modules), `results-direct.txt`,
`results-direct-run2.txt`, `results-relay.txt`, `results-relay-run2.txt`, `results-tcp.txt` and
`results-tcp-bare.txt` (raw output). Run it with
`cd spikes/unordered-datagrams && npm ci && node run.js direct && node run.js relay && node tcp.js`.

## pears-go feasibility (localhost)

Question: can pure Go (CGO_ENABLED=0, no libudx or libsodium) speak the Holepunch wire protocols well
enough to build pears-go? Four steps: dht-rpc PING and FIND_NODE against a testnet node, a Noise IK
handshake against the JS peer, one UDX stream moving 100 MiB against the pinned udx-native, and the
protocol notes.

**Verdict: PASS** against the PASS RULE. Step 2 gets valid replies (21 of 21 PINGs and 21 of 21
FIND_NODEs). Step 3 completes the Noise IK handshake in both roles, with each side's transport keys
opening the other's records. Step 4 moves 100 MiB intact in all six transfers (digests match), and
Go-to-JS runs at 0.73 of the JS-to-JS baseline on node (0.79 on bare), which clears the half bar.

### Setup

- Device: one Mac, Apple silicon (arm64), macOS 27.0.1. Network: loopback only (127.0.0.1). The DHT
  testnet, the Go client and every JS peer run on this machine. Nothing leaves the loopback interface.
- Go: go1.25.0 darwin/arm64, built with CGO_ENABLED=0 and GOTOOLCHAIN=local. The spike module is
  `spikes/pears-go/go` with its own go.mod: golang.org/x/crypto v0.45.0 (go 1.24 in its go.mod; the
  latest release, v0.57.0, needs Go 1.26), filippo.io/edwards25519 v1.2.0, and golang.org/x/sys
  v0.38.0 (indirect). No other module.
- Node: v24.15.0, npm 11.12.1. Bare: the repo-local bare 1.34.1 in `spikes/pears-go/js`, not the
  global bare 1.30.3.
- JS packages, pinned exactly in `spikes/pears-go/js/package.json`: hyperdht 6.34.1, dht-rpc 6.27.0,
  udx-native 1.21.3, noise-handshake 4.2.0, noise-curve-ed 2.1.0, sodium-universal 5.0.1, b4a 1.9.0,
  and bare 1.34.1 as a dev dependency. The first five are the versions `app/engine/package-lock.json`
  pins for the same names. Transitive versions come from `package-lock.json`.
- Keys: the Noise static keys and the ephemeral keys are fixtures from fixed labels (BLAKE2b-256 of a
  label). The spike prints no key bytes, only booleans, short public prefixes and digests.

### Steps run

1. Testnet: `hyperdht/testnet` with 3 nodes on 127.0.0.1, bootstrap printed as host:port. A JS
   control client (hyperdht 6.34.1) pings and FIND_NODEs the bootstrap node first, as a reference.
2. Go client (`spikes/pears-go/go`, package `internal/dhtrpc`): PING and FIND_NODE to the bootstrap
   node, 1 warm-up and 20 timed requests of each kind, every reply decoded and checked (tid echoed,
   error 0, closer nodes present for FIND_NODE, sender id checked when present). The Go encoder is
   also compared byte for byte with dht-rpc's own encoder on fixed inputs.
3. Noise IK (`internal/noise`, peer in `js/noise-peer.js`): Go as initiator against the JS responder,
   then Go as responder against the JS initiator. The two sides exchange a payload in each message
   and an authenticated transport check under the new keys, in both directions. Each side reports
   whether the other opened its record and whether the handshake hashes match. A negative control
   pins a wrong responder key on the initiator and must fail.
4. UDX (`internal/udx`, peer `js/udx-peer.js` on udx-native 1.21.3): 100 MiB, three runs per
   direction, once with node and once with bare as the JS runtime. The stream is one random 1 MiB
   block repeated. The sender's BLAKE2b-256 digest of the stream is computed before timing, and the
   receiver's digest must match. Throughput is measured at the receiver, from the first byte
   delivered to end of stream. The JS-to-JS baseline runs two JS processes, one sending and one
   receiving, the same topology as Go-to-JS. The bare baseline runs the same way.

Everything runs from `spikes/pears-go/run-all.sh`. The recorded output is the last run, saved with
`sh spikes/pears-go/run-all.sh | tee spikes/pears-go/results.txt`.

### Measurements

| Step | Result |
|---|---|
| 1. Testnet | 3 nodes on 127.0.0.1; the JS control ping ok, FIND_NODE returned 2 closer nodes |
| 2. Go PING | 21 of 21 valid replies; RTT p50 0.08 ms (min 0.07, max 0.15) |
| 2. Go FIND_NODE | 21 of 21 valid replies; RTT p50 0.07 ms (min 0.07, max 0.09); 2 closer nodes each |
| 2. Encoding check | Go PING (11 bytes) and FIND_NODE (43 bytes) match dht-rpc's encoder byte for byte |
| 2. Peer id | Go PeerID matches hyperdht's `peer.id` for the bootstrap address |
| 3. Noise, Go initiator | payload ok, JS responder authenticated the Go check, handshake hashes equal; 43 ms including the child's start-up |
| 3. Noise, Go responder | payload ok, JS initiator authenticated the reply, handshake hashes equal; 40 ms including the child's start-up |
| 3. Negative control | wrong responder key: JS side fails at message 1 (MAC mismatch), Go side gets EOF, as expected |
| 4. Digests | 6 of 6 transfers intact (digests equal); 3 runs per direction per runtime |

Step 4, throughput in Mbit/s (100 MiB = 104,857,600 bytes; receiver span; median of 3, with range).
The half bar is half the JS-to-JS median.

| Runtime | Go to JS | JS to Go | JS to JS, two processes (baseline) | Go to JS over baseline | Half bar |
|---|---|---|---|---|---|
| node 24.15 | 831 (789 to 862) | 1569 (1475 to 1772) | 1142 (1106 to 1142) | 0.73 | 571 |
| bare 1.34.1 | 863 (797 to 885) | 1758 (1570 to 1762) | 1088 (1087 to 1107) | 0.79 | 544 |

Other step 4 numbers: JS to JS in one process on node, one run, 934 ms (about 898 Mbit/s); it shares
one thread between sender and receiver, so it is not the baseline. Go-to-JS sent 91,023 packets per
run, JS-to-Go about 72,400. Zero retransmissions and zero SACK blocks in every run, so loss recovery
was never exercised. Sender and receiver spans agree within 2 ms in the Go-to-JS runs.

### Protocol details that were hard to find

Upstream files are in the npm packages named in each line. Versions as pinned above.

- dht-rpc, byte 0 of every message is (type << 4) | 3: request 0x03, response 0x13. Request flags:
  1 sender id, 2 token, 4 internal, 8 target, 16 value. Response flags: 1 id, 2 token, 4 closer
  nodes, 8 error, 16 value. Layout: byte 0, flags, tid (uint16 LE), the destination as IPv4 (4 bytes,
  then port uint16 LE), then the optional fields, then command (compact uint), target, value
  (`lib/io.js`: `_encodeRequest`, `_sendReply`, `Request.decode`, `decodeReply`).
- dht-rpc, the internal flag decides the handler. `request()` sends internal = false, so a FIND_NODE
  sent that way gets error 1 (UNKNOWN_COMMAND) with closer nodes. `findNode()` and `ping()` send
  internal = true. Observed in the JS control (error 1 before the fix, 0 after). Upstream:
  `index.js` `_onrequest`, `request`, `findNode`, and `lib/errors.js`.
- dht-rpc, node id = BLAKE2b-256 (unkeyed) over the 6-byte IPv4 address encoding (`lib/peer.js`
  `id`). A reply carries the sender id only when the responder is non-ephemeral and answers on its
  server socket (`lib/io.js` `_sendReply`). The 6.34.1 testnet nodes are ephemeral, so their replies
  carry no id, and the id check was not exercised live. The routing table id starts random and is
  replaced by the id of the NAT-sampled address on promotion (`index.js`, `table = new Table(id)`).
- compact-encoding 3.5.2 (`index.js`): uint is one byte up to 0xfc, else 0xfd, 0xfe or 0xff with a
  uint16, uint32 or uint64 LE; uint16 is LE; ipv4Address is 4 dotted bytes then uint16 LE port;
  array is uint count then elements; buffer is uint length then bytes.
- Noise protocol name is "Noise_IK_Ed25519_ChaChaPoly_BLAKE2b". The DH algorithm name is "Ed25519"
  (`noise-handshake/noise.js`, `noise-curve-ed/index.js`). The digest starts as the protocol name
  zero-padded to 64 bytes, and the responder mixes its own static key (`noise.js` `initialise`).
- Prologue: NS.PEER_HANDSHAKE = BLAKE2b-256(BLAKE2b-256("hyperswarm/dht") || 0x00), prefix 14d6d4b4.
  Derived by `hypercore-crypto/index.js` `namespace()`, used in `hyperdht/lib/constants.js` and
  `hyperdht/lib/noise-wrap.js`.
- HKDF is HMAC over BLAKE2b-512 with a 128-byte block, giving two 64-byte outputs. The chaining key
  update is HKDF(ck, dh). Split is HKDF(ck, empty), and the initiator sends with the first half
  (`noise-handshake/hkdf.js`, `hmac.js`, `symmetric-state.js`, `noise.js` `final`).
- AEAD nonce: 12 bytes, all zero except a uint32 LE counter at byte offset 4. The associated data is
  the running handshake digest (`noise-handshake/cipher.js` `encryptWithAD`, `setUint32(4, ...)`).
  This matches the Noise spec for counts below 2^32. Message 1 is e (32), then s encrypted (48), then
  the payload; message 2 is e (32), then the payload.
- Ed25519 DH is scalar multiplication with the clamped SHA-512 scalar of the seed, output as the
  compressed point, using libsodium's noclamp variant (`noise-curve-ed/index.js` `dh`). The Go port
  reduces the scalar mod l before multiplying. The results agree for prime-order keys, which every
  honest key is, and the handshake succeeded with that rule.
- UDX header, 20 bytes (`vendor/libudx/src/udx.c` `init_stream_packet`, `process_packet`;
  `include/udx.h`): byte 0 magic 0xFF, byte 1 version 1, byte 2 type flags (DATA 1, END 2, SACK 4,
  MESSAGE 8, DESTROY 16), byte 3 data offset (MTU-probe padding), then four uint32 LE fields: the
  recipient's stream id, a receive window fixed at 0xffffffff, seq, ack.
- UDX addressing: the header carries the recipient's stream id. There is no handshake. A stream is
  connected as soon as `connect` runs locally, and packets for an unknown id are dropped
  (`process_packet`, `udx__cirbuf_get` returns NULL).
- UDX sequence: seq starts at 0 and increments per data packet. ack is the next expected seq, which is
  cumulative. Pure acks do not advance seq. END is a flag on the last data packet, or on an empty
  packet that takes its own seq (`udx.c` `process_data_packet`, `process_packet`).
- UDX SACK: payload is (start, end) uint32 LE pairs, end exclusive, in SACK-only packets; a SACK packet
  never carries data (`udx.c` `process_sacks`, `udx__shift_packet`).
- UDX payload size: MTU base 1200 minus 48 (UDX_IPV4_HEADER_SIZE, which includes the 20-byte UDX
  header) gives 1152 bytes per packet (`include/udx.h`, `udx.c` `max_payload`). A nonzero data offset
  marks MTU-probe padding before the data (`process_packet`).
- udx-native 1.21.3 has no `src/` directory in its npm tarball, only `binding.cc`, the CMake file and
  prebuilt binaries. libudx is fetched at build time from GitHub (`CMakeLists.txt`, commit ae8bff7).
  This spike ported from the copy of libudx that udx-native 1.12.0 vendors under `vendor/libudx/`.
  Later releases checked (1.14.5, 1.16.2, 1.17.8, 1.18.3, 1.20.7) have no vendored C. Interop with
  1.21.3 was shown for DATA, END and cumulative ack, in both directions. SACK blocks, MTU probes and
  destroy were not exercised.
- udx-native, `finish` does not mean acked. The stream emits `finish` once END is queued. Observed: a
  sender that destroyed on `finish` cut off a 1 MiB transfer. `stream.flush()` resolves once every
  write is acknowledged (`lib/stream.js` `_final`, `flush`, `_onack`).
- bare has no `process` global and no `path` builtin under that name. The UDX peer uses
  `globalThis.process || require('bare-process')` and a local `once` in place of `events`, which
  udx-native maps to `bare-events` in its own `imports` field. The app engine port will need the same.
- Go module: the newest golang.org/x/crypto (v0.57.0) requires Go 1.26. The spike pins v0.45.0, the
  newest release checked that needs only Go 1.24, so the module builds on Go 1.25.

### What the numbers mean

- The Go port is correct on the exercised paths. Encodings match hyperdht's own encoder, both Noise
  roles authenticate, and a wrong key fails. Each 100 MiB transfer arrives with a matching digest.
- Throughput is loopback only. The Go sender uses a fixed window of 128 packets and no congestion
  control. Loopback has no loss, so the window never backs off. A real path would need the cubic
  congestion, RACK and TLP logic that the port leaves out.
- Go-to-JS is the slower direction. The spike did not profile which side limits it. The Go sender's
  per-packet syscalls and fixed window are the first suspects. JS-to-Go ran at 1.5 to 1.8 Gbit/s from
  a JS sender, so the Go receiver is not the limit at these rates.

### Caveats

- No loss, no NAT, no real network. Retransmission on timeout, SACK-driven fast retransmit and SACK
  blocks from the Go receiver are written but were not exercised: 0 retransmits and 0 SACK blocks in
  every run. Hole punching across a router is covered by HoleBridge-3jo.12.
- Not ported: the congestion control, RACK, TLP, MTU probing, messages, destroy handling, hyperdht's
  noisePayload framing, and the secret-stream layer. The Noise payloads are opaque bytes here.
- One stream at a time, one direction per run. Concurrent streams, the multiplexer and the sender's
  id check (testnet nodes are ephemeral) were not tested.
- Protocol source: libudx from the vendored copy in udx-native 1.12.0, not the 1.21.3 build. The
  interop runs are the evidence for the subset they cover. The npm package named libudx (version 0.0.0)
  is not the Holepunch library and was not used.
- Bare: the UDX peer ran under bare 1.34.1 and under node. The Noise peer and the DHT driver ran under
  node only. Nothing was run on a phone, a TV, Wi-Fi or a real device.

### Follow-ups

None required, because the PASS RULE passed. The IF IT FAILS path did not apply. The gaps above
(loss recovery and SACK, congestion control, concurrent streams, the sender id path, and the
noisePayload framing) are for the owner to schedule. They are not filed as beads.

### Files

`spikes/pears-go/`: `run-all.sh` (every step, bounded by timeouts), `results.txt` (raw output of the
last run), `go/` (module `holebridge-spike-pears-go`: `cmd/spike` with the `dht`, `noise` and `udx`
subcommands; `internal/compact`, `internal/dhtrpc`, `internal/noise`, `internal/udx`; `go.mod`,
`go.sum`, `.gitignore` for bin/), and `js/` (`dht-driver.js` for steps 1 and the JS control,
`noise-peer.js`, `udx-peer.js` (one stream, every mode), `udx-pair.js` (two-process JS baseline),
`package.json` and `package-lock.json` (exact pins), `.gitignore` for node_modules). Rebuild and rerun
with `sh spikes/pears-go/run-all.sh`.

## UDX throughput, Go vs JS

Question: how fast is pears/udx against udx-native on loopback, in each direction, and do 1 GiB
transfers keep the same SHA-256 at both ends?

**Verdict: PASS** for integrity and messages. Both 1 GiB transfers hashed the same at both ends, in
both directions, and unordered messages arrived both ways. The throughput table is recorded below.
It is loopback only, on a busy machine, so treat it as a rough figure.

### Setup

- Run date: 2026-10-09. Device: one Mac, Apple silicon (arm64), macOS 27.0.1. Go 1.25.0 with
  CGO_ENABLED=0. Node v24.15.0.
- JS peer: udx-native 1.21.3, pinned in `interop/js/package-lock.json`.
- Machine load: noisy. Other agents were running tests during the runs; the load average was 3 to 6.
- Networks: 127.0.0.1 only. No packet leaves the machine.

### Steps run

Four full runs of the UDX suite, all passing: one with `-count=1`, then one with `-count=3`. The
table has one value per suite pass, so each cell is the median of 3 transfers, and 12 transfers
per row in total.

1. TestUDX_GoToJSEcho: Go sends 1 GiB to the JS echo peer. The peer hashes what it receives and
   writes it back. Go hashes the echo. Both hashes must equal the hash of what Go sent.
2. TestUDX_JSToGo: the JS side sends 1 GiB to Go. Go hashes what it receives, and the hashes must
   match.
3. TestUDX_Messages: unordered messages, Go to JS and JS to Go.
4. TestUDX_Throughput: 100 MiB per run, 3 runs per row. The receiver's span, first byte to end of
   stream.

Command: `CGO_ENABLED=0 go test -count=3 -timeout 30m -v ./interop/ -run UDX` (about 2 minutes).

### Measurements

Mbit/s on 127.0.0.1. Medians per suite pass: the `-count=1` run, then the three iterations of the
`-count=3` run.

| Pair | Median per suite pass | Range of single transfers |
|---|---|---|
| Go to Go (one process) | 789, 774, 811, 795 | 656 to 849 |
| JS to JS (one process, udx-native both ends) | 1121, 985, 967, 1157 | 896 to 1208 |
| Go to JS (two processes) | 890, 876, 837, 829 | 783 to 940 |
| JS to Go (two processes) | 1170, 1112, 1154, 1186 | 1046 to 1195 |

### What the numbers mean

- In every pass, the Go sender to a JS receiver is slower than the JS sender to a Go receiver
  (829 to 890 against 1112 to 1186).
- In every pass, Go to Go is slower than JS to JS (at most 811 against at least 967).
- Loopback has no link limit, so these figures measure the protocol code and the host CPU, not a
  network.

### Caveats

- Loopback only, one Mac, and a busy machine. Single transfers vary by up to about 25 percent (Go
  to Go ranged from 656 to 849). Do not read one run as a benchmark.
- The runs were made after the fix for HoleBridge-85m.4.19 (a Go sender that stopped moving data
  in a 100 MiB transfer).
- The throughput figure is the receiver's span, so it excludes process start-up.

### Files

`interop/udx_test.go` (the four tests above), `interop/js/udx-peer.js` (the JS peer). The
throughput table is printed by TestUDX_Throughput; it is copied here by hand.
