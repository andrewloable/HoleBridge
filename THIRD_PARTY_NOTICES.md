# Third-party notices

HoleBridge is MIT licensed (see LICENSE). This file lists the third-party code that the shipped host,
relay and app include, with its license. Each row is one package at one exact version.
`node scripts/check-notices.js` exits 1 when a pinned package is missing from this file, so update
the row in the same change as the pin.

## Go modules

Compiled into the host and relay binaries. Decision D32 allows only these three modules beyond the
standard library. go.mod requires golang.org/x/crypto v0.50.0, the newest release that builds with the
Go 1.25 toolchain (v0.57.0 needs Go 1.26). Each version below is the one whose LICENSE file was read
from the Go module cache. When a change adds one of them to go.mod, its row must carry the same
version.

| Module | Version | License | Copyright |
|---|---|---|---|
| golang.org/x/crypto | v0.50.0 | BSD-3-Clause | Copyright 2009 The Go Authors. |
| golang.org/x/sys | v0.43.0 | BSD-3-Clause | Copyright 2009 The Go Authors. |
| filippo.io/edwards25519 | v1.2.0 | BSD-3-Clause | Copyright (c) 2009 The Go Authors. All rights reserved. |

The full license text is each module's LICENSE file. golang.org/x/crypto and golang.org/x/sys also ship
a PATENTS file; a binary release should carry it too.

## App engine (npm)

The non-dev packages in app/engine/package-lock.json. The engine bundle carries the JavaScript
packages, and the native addons ship with the app. Each license is the license field of the installed
package.json. The test-vector generators in spec/gen pin the same packages, so they add nothing here;
the generator-only packages are listed in the section after this one.

| Package | Version | License |
|---|---|---|
| @hyperswarm/secret-stream | 6.9.2 | Apache-2.0 |
| adaptive-timeout | 1.0.1 | Apache-2.0 |
| b4a | 1.9.0 | Apache-2.0 |
| bare-addon-resolve | 1.10.1 | Apache-2.0 |
| bare-ansi-escapes | 2.2.3 | Apache-2.0 |
| bare-assert | 1.3.0 | Apache-2.0 |
| bare-buffer | 3.7.1 | Apache-2.0 |
| bare-dns | 2.2.1 | Apache-2.0 |
| bare-events | 2.9.2 | Apache-2.0 |
| bare-fs | 4.8.3 | Apache-2.0 |
| bare-inspect | 3.1.10 | Apache-2.0 |
| bare-module-resolve | 1.12.5 | Apache-2.0 |
| bare-path | 3.1.2 | Apache-2.0 |
| bare-pipe | 4.3.1 | Apache-2.0 |
| bare-semver | 1.1.0 | Apache-2.0 |
| bare-stream | 2.13.4 | Apache-2.0 |
| bare-tcp | 2.6.1 | Apache-2.0 |
| bare-type | 1.4.0 | Apache-2.0 |
| bare-url | 2.5.4 | Apache-2.0 |
| bits-to-bytes | 1.3.0 | ISC |
| blind-relay | 1.6.1 | Apache-2.0 |
| bogon | 1.3.0 | MIT |
| compact-encoding | 3.5.2 | Apache-2.0 |
| compact-encoding-bitfield | 1.1.0 | Apache-2.0 |
| compact-encoding-net | 1.3.0 | Apache-2.0 |
| dht-rpc | 6.27.0 | MIT |
| events-universal | 1.0.1 | Apache-2.0 |
| fast-fifo | 1.3.2 | MIT |
| generate-object-property | 2.0.0 | MIT |
| generate-string | 1.0.1 | MIT |
| hypercore-crypto | 3.7.0 | MIT |
| hypercore-id-encoding | 1.3.0 | Apache-2.0 |
| hyperdht | 6.34.1 | MIT |
| hyperdht-address | 1.1.1 | Apache-2.0 |
| hyperschema | 1.26.2 | Apache-2.0 |
| is-property | 1.0.2 | MIT |
| kademlia-routing-table | 1.0.6 | MIT |
| nanoassert | 2.0.0 | ISC |
| nat-sampler | 1.0.1 | MIT |
| noise-curve-ed | 2.1.0 | ISC |
| noise-handshake | 4.2.0 | Apache-2.0 |
| protomux | 3.12.1 | MIT |
| queue-tick | 1.0.1 | MIT |
| record-cache | 1.2.0 | MIT |
| require-addon | 1.3.0 | Apache-2.0 |
| safety-catch | 1.0.3 | MIT |
| signal-promise | 1.0.3 | MIT |
| sodium-native | 5.1.0 | MIT |
| sodium-secretstream | 1.2.0 | MIT |
| sodium-universal | 5.0.1 | MIT |
| streamx | 2.28.1 | MIT |
| teex | 1.0.1 | MIT |
| text-decoder | 1.2.7 | Apache-2.0 |
| time-ordered-set | 2.0.1 | MIT |
| timeout-refresh | 2.0.1 | MIT |
| udx-native | 1.21.3 | Apache-2.0 |
| unslab | 1.3.0 | Apache-2.0 |
| which-runtime | 1.4.0 | Apache-2.0 |
| xache | 1.3.0 | MIT |
| z32 | 1.1.0 | MIT |

## Test-vector generators (npm, not shipped)

The packages in spec/gen/package-lock.json that the app engine does not pin: qrcode, which
spec/gen/qr.js uses to write spec/vectors/qr.json, and the packages it brings in. Nothing here is
compiled into a binary or bundled into the app (the Go QR encoder in internal/qr is hand-written).
They are listed because check-notices.js checks the generators' lockfile too. Each license is the
license field of the installed package.json.

| Package | Version | License |
|---|---|---|
| ansi-regex | 5.0.1 | MIT |
| ansi-styles | 4.3.0 | MIT |
| camelcase | 5.3.1 | MIT |
| cliui | 6.0.0 | ISC |
| color-convert | 2.0.1 | MIT |
| color-name | 1.1.4 | MIT |
| decamelize | 1.2.0 | MIT |
| dijkstrajs | 1.0.3 | MIT |
| emoji-regex | 8.0.0 | MIT |
| find-up | 4.1.0 | MIT |
| get-caller-file | 2.0.5 | ISC |
| is-fullwidth-code-point | 3.0.0 | MIT |
| locate-path | 5.0.0 | MIT |
| p-limit | 2.3.0 | MIT |
| p-locate | 4.1.0 | MIT |
| p-try | 2.2.0 | MIT |
| path-exists | 4.0.0 | MIT |
| pngjs | 5.0.0 | MIT |
| qrcode | 1.5.4 | MIT |
| require-directory | 2.1.1 | MIT |
| require-main-filename | 2.0.0 | ISC |
| set-blocking | 2.0.0 | ISC |
| string-width | 4.2.3 | MIT |
| strip-ansi | 6.0.1 | MIT |
| which-module | 2.0.1 | ISC |
| wrap-ansi | 6.2.0 | MIT |
| y18n | 4.0.3 | ISC |
| yargs | 15.4.1 | MIT |
| yargs-parser | 18.1.3 | ISC |

## Upstream NOTICE files

Two packages in the engine ship a NOTICE file. Binary distributions must reproduce it, so its text
is copied here.

### bare-path 3.1.2 (Apache-2.0)

```
Copyright 2023 Holepunch Inc

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

---

Copyright Joyent, Inc. and other Node contributors.

Permission is hereby granted, free of charge, to any person obtaining a
copy of this software and associated documentation files (the
"Software"), to deal in the Software without restriction, including
without limitation the rights to use, copy, modify, merge, publish,
distribute, sublicense, and/or sell copies of the Software, and to permit
persons to whom the Software is furnished to do so, subject to the
following conditions:

The above copyright notice and this permission notice shall be included
in all copies or substantial portions of the Software.
```

### compact-encoding-net 1.3.0 (Apache-2.0)

```
Copyright 2023 Holepunch Inc

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```

## Ported code

Code ported from upstream libraries into this repo (decisions D31 and D34). Each port adds a row with
the upstream library, the upstream files it came from, and its license. The upstream copyright line
also stays in the header of each ported file.

| Library | Upstream files | License |
|---|---|---|
| kademlia-routing-table 1.0.6 | index.js | MIT |
| libsodium 1.0.20, crypto_secretstream_xchacha20poly1305 | src/libsodium/crypto_secretstream/xchacha20poly1305/secretstream_xchacha20poly1305.c | ISC |
| libudx (UDX transport) | udx.h, udx.c | Apache-2.0 |
| compact-encoding 3.5.2 (pears/compact) | index.js | Apache-2.0 |
| noise-handshake 4.2.0 (pears/noise) | noise.js, symmetric-state.js, cipher.js, hkdf.js, hmac.js | Apache-2.0 |
| noise-curve-ed 2.1.0 (pears/noise) | index.js | ISC |
| dht-rpc 6.27.0 (pears/dhtrpc messages, node, nat and query) | lib/io.js, lib/peer.js, lib/query.js, index.js, lib/commands.js, lib/errors.js | MIT |
| nat-sampler 1.0.1 (pears/dhtrpc nat) | index.js | MIT |
| hyperdht 6.34.1 (pears/hyperdht messages, dht, announce, server, router, connect, relay and holepunch) | lib/messages.js, index.js (announce, unannounce, lookup, constructor key pair, createServer, connect), lib/persistent.js (lookup, announce and unannounce records, routes, refresh, find-peer, signatures), lib/constants.js (command numbers), lib/server.js (listen, close, firewall, handshake reply, the relay of a handshake, the holepunch reply and the direct connection), lib/router.js (handshake and holepunch routing), lib/connect.js (find-peer walk, connect through a node, the direct reply, the relay-through selection and relay pairing, localAddresses and matchAddress), lib/holepuncher.js (the punch state machine), lib/nat.js (the NAT sampler), lib/secure-payload.js (the encrypted holepunch payload) | MIT |
| protomux 3.12.1 (pears/protomux) | index.js | MIT |
| blind-relay 1.6.1 (pears/blindrelay, pairing, message encodings and forwarding) | index.js | Apache-2.0 |
| @hyperswarm/secret-stream 6.9.2 (pears/secretstream) | index.js, lib/handshake.js | Apache-2.0 |
| hypercore-crypto 3.7.0 (pears/secretstream namespaces) | index.js (namespace) | MIT |

## Not yet listed

- Flutter app pub packages. The app has no pubspec.lock yet; add them when it is pinned.
- Bare runtime packages (bare-runtime and bare-runtime-<platform>, 1.34.1, Apache-2.0). They are
  dev-only in app/engine/package-lock.json, so check-notices.js does not require them. Add them if the
  app ships them.
- License texts. The npm rows give license identifiers only. A binary release must also carry each
  package's copyright notice and license text (MIT, ISC and Apache-2.0 require it). The release
  archives do not carry them yet.
