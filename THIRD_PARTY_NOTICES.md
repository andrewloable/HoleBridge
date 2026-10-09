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
a PATENTS file; a binary release should carry it too. scripts/collect-licenses.sh copies these files,
and the Go standard library's LICENSE and PATENTS, into the archives' licenses/ directory.

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

Packages that ship a NOTICE file: two in the engine, and the Bare Kit and bare runtime binaries of the
app. Binary distributions must reproduce each NOTICE, so its text is copied here.

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

### bare-kit 2.5.5 (Apache-2.0)

```
Copyright 2024 Holepunch Inc

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

### bare-runtime 1.30.3 (Apache-2.0)

```
Copyright 2022 Holepunch Inc

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

## Native code (Android VPN mode)

The network stack in app/native/netstack/ and the JNI glue in app/native/jni/ are compiled into
libholebridge_tun.so for arm64-v8a, armeabi-v7a and x86_64 (app/native/CMakeLists.txt). Each row is
one component at the commit recorded in app/native/netstack/VERSION, and each component's LICENSE
file sits beside its sources in that tree (for yaml, LICENSE-libyaml holds libyaml's text and License
holds the hev fork's). libyaml, the upstream of the yaml row, is MIT
(https://github.com/yaml/libyaml). The Android app carries these LICENSE files: app/tool/collect_app_licenses.sh
copies them into app/assets/licenses/. The host and relay archives do not contain this code.

| Component | Version | License | Copyright |
|---|---|---|---|
| hev-socks5-tunnel | 2.18.0 (commit d9dca26) | MIT | Copyright (c) 2022 hev |
| hev-socks5-core (src/core, a submodule of hev-socks5-tunnel) | commit 162dd99 | MIT | Copyright (c) 2022 hev |
| hev-task-system | commit 328f35d | MIT | Copyright (c) 2022 hev |
| lwip (heiher fork) | commit e22c9d2 | BSD-3-Clause | Copyright (c) 2001, 2002 Swedish Institute of Computer Science |
| yaml (hev branch of libyaml 0.2.5) | commit efa3611 | MIT | Copyright (c) 2017-2020 Ingy döt Net; Copyright (c) 2006-2016 Kirill Simonov (libyaml); Copyright (c) 2022 hev (branch) |

## Ported code

Code ported from upstream libraries into this repo (decisions D31 and D34). Each port adds a row with
the upstream library, the upstream files it came from, its license, and the file that holds its
license text. The upstream copyright line also stays in the header of each ported file.
scripts/collect-licenses.sh copies each text into the archives' licenses/ports/<library>/. The npm
texts are read from app/engine/node_modules, so run npm ci in app/engine first. The others are
vendored under packaging/licenses/.

| Library | Upstream files | License | License text file |
|---|---|---|---|
| kademlia-routing-table 1.0.6 | index.js | MIT | app/engine/node_modules/kademlia-routing-table/LICENSE |
| libsodium 1.0.20 (tag 1.0.20-RELEASE, commit 9511c98), crypto_secretstream_xchacha20poly1305 | src/libsodium/crypto_secretstream/xchacha20poly1305/secretstream_xchacha20poly1305.c | ISC | packaging/licenses/libsodium/LICENSE |
| libudx (UDX transport, commit ae8bff7) | udx.h, udx.c, internal.h, udx_rate.c, udx_bbr.c | Apache-2.0 | packaging/licenses/libudx/LICENSE and NOTICE |
| libudx windowed min and max filter (Copyright 2017, Google Inc.; within libudx ae8bff7) | win_filter.c, win_filter_f64.c | BSD-3-Clause | packaging/licenses/libudx-win-filter/LICENSE |
| compact-encoding 3.5.2 (pears/compact) | index.js | Apache-2.0 | app/engine/node_modules/compact-encoding/LICENSE |
| noise-handshake 4.2.0 (pears/noise) | noise.js, symmetric-state.js, cipher.js, hkdf.js, hmac.js | Apache-2.0 | app/engine/node_modules/noise-handshake/LICENSE |
| noise-curve-ed 2.1.0 (pears/noise) | index.js | ISC | packaging/licenses/noise-curve-ed/LICENSE |
| dht-rpc 6.27.0 (pears/dhtrpc messages, node, nat and query) | lib/io.js, lib/peer.js, lib/query.js, index.js, lib/commands.js, lib/errors.js | MIT | app/engine/node_modules/dht-rpc/LICENSE |
| nat-sampler 1.0.1 (pears/dhtrpc nat) | index.js | MIT | app/engine/node_modules/nat-sampler/LICENSE |
| hyperdht 6.34.1 (pears/hyperdht messages, dht, announce, server, router, connect, relay and holepunch) | lib/messages.js, index.js (announce, unannounce, lookup, constructor key pair, createServer, connect, the randomized-punch limit and interval), lib/persistent.js (lookup, announce and unannounce records, routes, refresh, find-peer, signatures), lib/constants.js (command numbers and the default BOOTSTRAP_NODES list, in internal/cli/relay.go), lib/server.js (listen, close, firewall, handshake reply, the relay of a handshake, the holepunch reply and the direct connection), lib/router.js (handshake and holepunch routing), lib/connect.js (find-peer walk, connect through a node, the direct reply, the relay-through selection and relay pairing, localAddresses and matchAddress), lib/holepuncher.js (the punch state machine), lib/nat.js (the NAT sampler), lib/secure-payload.js (the encrypted holepunch payload), lib/socket-pool.js (the routing of a one-byte holepunch datagram, in pears/dhtrpc punch) | MIT | app/engine/node_modules/hyperdht/LICENSE |
| protomux 3.12.1 (pears/protomux) | index.js | MIT | app/engine/node_modules/protomux/LICENSE |
| blind-relay 1.6.1 (pears/blindrelay, pairing, message encodings and forwarding) | index.js | Apache-2.0 | app/engine/node_modules/blind-relay/LICENSE |
| @hyperswarm/secret-stream 6.9.2 (pears/secretstream) | index.js, lib/handshake.js | Apache-2.0 | app/engine/node_modules/@hyperswarm/secret-stream/LICENSE |
| hypercore-crypto 3.7.0 (pears/secretstream namespaces) | index.js (namespace) | MIT | app/engine/node_modules/hypercore-crypto/LICENSE |

The vendored texts, and how each one was taken:

- libsodium: the LICENSE file at tag 1.0.20-RELEASE, copied unchanged (ISC, Copyright (c) 2013-2024
  Frank Denis).
- libudx: the LICENSE (Apache-2.0) and NOTICE (Copyright 2021 Holepunch Inc) files at commit ae8bff7,
  copied unchanged. Apache-2.0 section 4(d) requires the NOTICE to travel with derivative works.
- libudx-win-filter: the license comment at the top of src/win_filter.c at ae8bff7, with the C comment
  markers removed. win_filter_f64.c carries the same comment.
- noise-curve-ed: the npm package ships no LICENSE file, and neither does the holepunchto/noise-curve-ed
  tag v2.1.0 (commit c58b822, whose index.js matches the npm package). The text is the ISC license, as
  the package's license field states it, with the copyright line "Copyright (c) Christophe Diederichs".
  Christophe Diederichs wrote 11 of the 16 commits to v2.1.0, and the chm-diederichs account, the
  repository that package.json names, is the top contributor. The file is derived, not upstream.

## Flutter app (pub)

The hosted packages the app ships, from app/pubspec.lock: the packages reachable from its
runtime dependencies in the pub package graph (app/.dart_tool/package_graph.json). Each license is
read from the LICENSE file of the package in the pub cache. Flutter's build also bundles these
license texts in the app (flutter_assets/NOTICES.Z).

| Package | Version | License |
|---|---|---|
| args | 2.7.0 | BSD-3-Clause |
| characters | 1.4.1 | BSD-3-Clause |
| code_assets | 1.2.1 | BSD-3-Clause |
| collection | 1.19.1 | BSD-3-Clause |
| crypto | 3.0.7 | BSD-3-Clause |
| ffi | 2.2.0 | BSD-3-Clause |
| ffi_leak_tracker | 0.1.2 | BSD-3-Clause |
| flutter_pear_bare | 0.4.9 | MIT |
| flutter_secure_storage | 11.2.0 | BSD-3-Clause |
| flutter_secure_storage_darwin | 0.4.3 | BSD-3-Clause |
| flutter_secure_storage_linux | 3.0.3 | BSD-3-Clause |
| flutter_secure_storage_platform_interface | 2.1.1 | BSD-3-Clause |
| flutter_secure_storage_web | 2.1.1 | BSD-3-Clause |
| flutter_secure_storage_windows | 4.2.2 | BSD-3-Clause |
| hooks | 2.0.2 | BSD-3-Clause |
| jni | 1.1.0 | BSD-3-Clause |
| jni_flutter | 1.0.4+1 | BSD-3-Clause |
| jni_util | 1.0.0 | BSD-3-Clause |
| logging | 1.3.0 | BSD-3-Clause |
| material_color_utilities | 0.13.0 | Apache-2.0 |
| meta | 1.18.3 | BSD-3-Clause |
| objective_c | 9.5.0 | BSD-3-Clause |
| package_config | 3.0.0 | BSD-3-Clause |
| path | 1.9.1 | BSD-3-Clause |
| path_provider | 2.1.6 | BSD-3-Clause |
| path_provider_android | 2.3.1 | BSD-3-Clause |
| path_provider_foundation | 2.6.0 | BSD-3-Clause |
| path_provider_linux | 2.2.2 | BSD-3-Clause |
| path_provider_platform_interface | 2.1.3 | BSD-3-Clause |
| path_provider_windows | 2.3.0 | BSD-3-Clause |
| platform | 3.2.0 | BSD-3-Clause |
| plugin_platform_interface | 2.1.8 | BSD-3-Clause |
| pub_semver | 2.2.1 | BSD-3-Clause |
| record_use | 0.6.0 | BSD-3-Clause |
| source_span | 1.10.2 | BSD-3-Clause |
| string_scanner | 1.4.1 | BSD-3-Clause |
| term_glyph | 1.2.2 | BSD-3-Clause |
| typed_data | 1.4.0 | BSD-3-Clause |
| url_launcher | 6.3.3 | BSD-3-Clause |
| url_launcher_android | 6.3.33 | BSD-3-Clause |
| url_launcher_ios | 6.4.2 | BSD-3-Clause |
| url_launcher_linux | 3.2.3 | BSD-3-Clause |
| url_launcher_macos | 3.2.6 | BSD-3-Clause |
| url_launcher_platform_interface | 2.3.2 | BSD-3-Clause |
| url_launcher_web | 2.4.3 | BSD-3-Clause |
| url_launcher_windows | 3.1.6 | BSD-3-Clause |
| vector_math | 2.4.3 | BSD-3-Clause |
| web | 1.1.1 | BSD-3-Clause |
| win32 | 6.4.0 | BSD-3-Clause |
| xdg_directories | 1.1.0 | BSD-3-Clause |
| yaml | 3.1.4 | MIT |

### Flutter dev-only packages (pub, not shipped)

The hosted packages in app/pubspec.lock that only the tests and lints use. They do not ship, and
they are listed because check-notices.js checks every hosted package in app/pubspec.lock.

| Package | Version | License |
|---|---|---|
| async | 2.13.1 | BSD-3-Clause |
| boolean_selector | 2.1.2 | BSD-3-Clause |
| clock | 1.1.3 | Apache-2.0 |
| fake_async | 1.3.3 | Apache-2.0 |
| file | 7.0.1 | BSD-3-Clause |
| flutter_lints | 6.0.0 | BSD-3-Clause |
| leak_tracker | 11.0.2 | BSD-3-Clause |
| leak_tracker_flutter_testing | 3.0.10 | BSD-3-Clause |
| leak_tracker_testing | 3.0.2 | BSD-3-Clause |
| lints | 6.1.0 | BSD-3-Clause |
| matcher | 0.12.20 | BSD-3-Clause |
| process | 5.0.6 | BSD-3-Clause |
| stack_trace | 1.12.2 | BSD-3-Clause |
| stream_channel | 2.1.4 | BSD-3-Clause |
| sync_http | 0.3.1 | BSD-3-Clause |
| test_api | 0.7.12 | BSD-3-Clause |
| vm_service | 15.3.0 | BSD-3-Clause |
| webdriver | 3.2.0 | Apache-2.0 |

## Bare runtime (flutter_pear_bare)

flutter_pear_bare 0.4.9 runs the engine worklet on each platform. Bare Kit 2.5.5 is bundled on
Android (libbare-kit.so, fetched at build time and checksum-pinned) and on iOS
(BareKit.xcframework). The bare runtime is inside Bare Kit on Android and iOS. On desktop (macOS,
Linux and Windows) the plugin does not bundle Bare Kit or the runtime: it fetches
bare-runtime-<host> 1.30.3 from npm, checks its SHA-256 against bare-runtime-pin.json, and runs it
as a subprocess. The engine lockfile pins bare-runtime 1.34.1, which is a dev
dependency and does not ship. The Apache-2.0 NOTICE files are copied below.

| Package | Version | License |
|---|---|---|
| bare-kit (Android libbare-kit.so and iOS BareKit.xcframework) | 2.5.5 | Apache-2.0 |
| bare-runtime-darwin-arm64 | 1.30.3 | Apache-2.0 |
| bare-runtime-darwin-x64 | 1.30.3 | Apache-2.0 |
| bare-runtime-linux-x64 | 1.30.3 | Apache-2.0 |
| bare-runtime-win32-x64 | 1.30.3 | Apache-2.0 |

## Native addons (flutter_pear_bare)

The native addons flutter_pear_bare ships in its Android jniLibs, iOS xcframeworks and desktop
node_modules, at the versions it commits. These are not the engine's addon versions listed above.
Each license is the license field of the npm package at that version, and matches its LICENSE file.

| Package | Version | License |
|---|---|---|
| bare-fs | 4.8.1 | Apache-2.0 |
| bare-inspect | 3.1.4 | Apache-2.0 |
| bare-path | 3.1.2 | Apache-2.0 |
| bare-pipe | 4.3.1 | Apache-2.0 |
| bare-type | 1.1.0 | Apache-2.0 |
| bare-url | 2.4.5 | Apache-2.0 |
| fs-native-extensions | 1.5.1 | Apache-2.0 |
| quickbit-native | 2.4.8 | Apache-2.0 |
| rabin-native | 2.0.0 | Apache-2.0 |
| rocksdb-native | 3.18.0 | Apache-2.0 |
| simdle-native | 1.3.9 | Apache-2.0 |
| sodium-native | 5.1.0 | MIT |
| udx-native | 1.20.7 | Apache-2.0 |

## Desktop worklet bundle (flutter_pear_bare pear-end.bundle)

pear-end.bundle is the desktop Bare worklet for Linux, Windows and macOS. flutter_pear_bare 0.4.9
ships a copy built with flutter_pear 0.4.9 (per its CHANGELOG), from that release's pear-end
lockfile. The rows are that lockfile's non-dev packages, and each license is the one in flutter_pear's
THIRD_PARTY_LICENSES file for the same version. These versions differ from the engine's (for example
hyperdht 6.32.0 here, 6.34.1 in the engine). The bundle file itself is not checked against the
lockfile here.

| Package | Version | License |
|---|---|---|
| @hyperswarm/secret-stream | 6.9.1 | Apache-2.0 |
| adaptive-timeout | 1.0.1 | Apache-2.0 |
| autobase | 7.28.2 | Apache-2.0 |
| b4a | 1.8.1 | Apache-2.0 |
| bare-addon-resolve | 1.10.0 | Apache-2.0 |
| bare-ansi-escapes | 2.2.3 | Apache-2.0 |
| bare-assert | 1.2.0 | Apache-2.0 |
| bare-events | 2.9.1 | Apache-2.0 |
| bare-fs | 4.8.1 | Apache-2.0 |
| bare-inspect | 3.1.4 | Apache-2.0 |
| bare-module-resolve | 1.12.2 | Apache-2.0 |
| bare-path | 3.1.2 | Apache-2.0 |
| bare-pipe | 4.3.1 | Apache-2.0 |
| bare-semver | 1.1.0 | Apache-2.0 |
| bare-stream | 2.13.3 | Apache-2.0 |
| bare-type | 1.1.0 | Apache-2.0 |
| bare-url | 2.4.5 | Apache-2.0 |
| big-sparse-array | 1.0.3 | MIT |
| binary-stream-equals | 1.0.0 | MIT |
| bits-to-bytes | 1.3.0 | ISC |
| blind-pairing | 2.3.1 | Apache-2.0 |
| blind-pairing-core | 2.10.1 | Apache-2.0 |
| blind-relay | 1.6.1 | Apache-2.0 |
| bogon | 1.3.0 | MIT |
| codecs | 3.1.0 | MIT |
| compact-encoding | 3.5.0 | Apache-2.0 |
| compact-encoding-bitfield | 1.1.0 | Apache-2.0 |
| compact-encoding-net | 1.3.0 | Apache-2.0 |
| core-coupler | 2.0.0 | Apache-2.0 |
| corestore | 7.12.5 | MIT |
| debounceify | 1.1.0 | MIT |
| device-file | 2.3.1 | Apache-2.0 |
| dht-rpc | 6.27.0 | MIT |
| encryption-encoding | 1.0.3 | Apache-2.0 |
| events-universal | 1.0.1 | Apache-2.0 |
| fast-fifo | 1.3.2 | MIT |
| fd-lock | 2.2.0 | Apache-2.0 |
| flat-tree | 1.13.0 | MIT |
| fs-native-extensions | 1.5.1 | Apache-2.0 |
| generate-object-property | 2.0.0 | MIT |
| generate-string | 1.0.1 | MIT |
| hyperbee | 2.27.3 | MIT |
| hyperblobs | 2.12.1 | Apache-2.0 |
| hypercore | 11.36.1 | MIT |
| hypercore-crypto | 3.7.0 | MIT |
| hypercore-errors | 1.5.0 | Apache-2.0 |
| hypercore-id-encoding | 1.3.0 | Apache-2.0 |
| hypercore-storage | 3.3.1 | Apache-2.0 |
| hyperdht | 6.32.0 | MIT |
| hyperdht-address | 1.1.1 | Apache-2.0 |
| hyperdrive | 13.3.4 | Apache-2.0 |
| hyperschema | 1.21.0 | Apache-2.0 |
| hyperswarm | 4.17.2 | MIT |
| index-encoder | 3.5.0 | Apache-2.0 |
| is-options | 1.0.2 | MIT |
| is-property | 1.0.2 | MIT |
| kademlia-routing-table | 1.0.6 | MIT |
| localdrive | 2.2.1 | Apache-2.0 |
| mirror-drive | 1.14.2 | Apache-2.0 |
| mutexify | 1.4.0 | MIT |
| nanoassert | 2.0.0 | ISC |
| nat-sampler | 1.0.1 | MIT |
| noise-curve-ed | 2.1.0 | ISC |
| noise-handshake | 4.2.0 | Apache-2.0 |
| protocol-buffers-encodings | 1.2.0 | MIT |
| protomux | 3.12.0 | MIT |
| protomux-wakeup | 2.9.0 | Apache-2.0 |
| queue-tick | 1.0.1 | MIT |
| quickbit-native | 2.4.8 | Apache-2.0 |
| quickbit-universal | 2.2.0 | ISC |
| rabin-native | 2.0.0 | Apache-2.0 |
| rabin-stream | 2.0.0 | Apache-2.0 |
| rache | 1.0.0 | Apache-2.0 |
| random-array-iterator | 1.0.0 | MIT |
| ready-resource | 1.2.0 | MIT |
| record-cache | 1.2.0 | MIT |
| refcounter | 1.0.0 | Apache-2.0 |
| require-addon | 1.2.0 | Apache-2.0 |
| resolve-reject-promise | 1.1.0 | MIT |
| resource-on-exit | 1.0.0 | Apache-2.0 |
| rocksdb-native | 3.18.0 | Apache-2.0 |
| safety-catch | 1.0.3 | MIT |
| same-data | 1.0.0 | MIT |
| scope-lock | 1.2.4 | Apache-2.0 |
| shuffled-priority-queue | 2.1.0 | MIT |
| signal-promise | 1.0.3 | MIT |
| signed-varint | 2.0.1 | MIT |
| simdle-native | 1.3.9 | Apache-2.0 |
| simdle-universal | 1.1.2 | ISC |
| sodium-native | 5.1.0 | MIT |
| sodium-secretstream | 1.2.0 | MIT |
| sodium-universal | 5.0.1 | MIT |
| speedometer | 1.1.0 | MIT |
| streamx | 2.28.1 | MIT |
| sub-encoder | 2.1.3 | Apache-2.0 |
| teex | 1.0.1 | MIT |
| text-decoder | 1.2.7 | Apache-2.0 |
| time-ordered-set | 2.0.1 | MIT |
| timeout-refresh | 2.0.1 | MIT |
| tiny-buffer-map | 1.1.1 | MIT |
| udx-native | 1.20.7 | Apache-2.0 |
| unix-path-resolve | 1.0.2 | MIT |
| unordered-set | 2.0.1 | MIT |
| unslab | 1.3.0 | Apache-2.0 |
| varint | 5.0.0 | MIT |
| which-runtime | 1.4.0 | Apache-2.0 |
| xache | 1.2.1 | MIT |
| z32 | 1.1.0 | MIT |

## Statically linked libraries (Bare Kit, the bare runtime, the native addons)

Libraries compiled into the binaries that the app and the desktop runtime ship. Each row is one
library at the version or commit that its build fetches: the CMake files of bare, bare-kit, the Bare
builtins and the native addons, and the DEPS file of V8 for the V8 rows. The two bare versions pin
different commits. The CMake pins of bare 1.30.3 are those of "bare runtime" in the Linked into column
(the desktop runtime); the CMake pins of bare 1.33.4 are those of "Bare Kit" (Bare Kit 2.5.5 embeds
bare 1.33.4). ICU 78, libuv 1.52.1 and V8 14.8.178.31 are also confirmed in the strings of the shipped
binaries, and so are zlib (iOS Bare Kit) and librlimit (Android Bare Kit). Each row's license text is
in app/licenses/static/, in the directory named in the last column; app/tool/collect_app_licenses.sh
copies it into the Flutter app.

| Library | Version or commit | License | Linked into | Text (app/licenses/static/) |
|---|---|---|---|---|
| V8 | 14.8.178.31 | BSD-3-Clause | Bare Kit, bare runtime | v8-14.8.178.31 |
| ICU | 78 (Chromium deps/icu, commit ee5f27ad) | Unicode-3.0 | Bare Kit, bare runtime (inside V8) | icu-78-ee5f27ad |
| libc++ | llvm-project, commit 7ab65651 (Chromium copy) | Apache-2.0 WITH LLVM-exception | Bare Kit, bare runtime (inside V8); libc++_shared.so in the Android Bare Kit (NDK copy, same license text) | libc++-7ab65651 |
| libc++abi | llvm-project, commit 8f11bb1d | Apache-2.0 WITH LLVM-exception | Bare Kit, bare runtime (inside V8) | libc++abi-8f11bb1d |
| libunwind | llvm-project, commit 092645a3 | Apache-2.0 WITH LLVM-exception | Bare Kit (Android) | libunwind-092645a3 |
| perfetto | commit 6590fe9c | Apache-2.0 | Bare Kit, bare runtime (inside V8) | perfetto-6590fe9c |
| abseil-cpp | commit 2a7d49fc | Apache-2.0 | Bare Kit, bare runtime (inside V8) | abseil-cpp-2a7d49fc |
| fast_float | commit cb1d42aa | Apache-2.0 OR MIT (BSL-1.0 for some files) | Bare Kit (Android, inside V8) | fast_float-cb1d42aa |
| dragonbox | commit beeeef91 | Apache-2.0 WITH LLVM-exception OR BSL-1.0 | Bare Kit (Android, inside V8) | dragonbox-beeeef91 |
| fp16 | commit 3d2de181 | MIT | Bare Kit, bare runtime (inside V8) | fp16-3d2de181 |
| zlib | commit b80f1d1e (Chromium copy) | zlib | bare runtime (desktop), Bare Kit (iOS) | zlib-b80f1d1e |
| libuv | 1.52.1 | MIT | Bare Kit, bare runtime | libuv-1.52.1 |
| BoringSSL | 0.20260211.0 | Apache-2.0 | bare-crypto (Bare Kit, bare runtime), bare-tls (bare runtime) | boringssl-0.20260211.0 |
| c-ares | 1.34.4 | MIT | bare-dns (bare runtime) | c-ares-1.34.4 |
| libjs | commit 56f14ed | Apache-2.0 | bare runtime | libjs-56f14ed |
| libjs | commit 72de271 | Apache-2.0 | Bare Kit | libjs-72de271 |
| libnapi | commit 60e6881 | Apache-2.0 | bare runtime | libnapi-60e6881 |
| libnapi | commit b4c5a66 | Apache-2.0 | Bare Kit | libnapi-b4c5a66 |
| librlimit | commit 6e4b390 | Apache-2.0 | Bare Kit (Android), bare runtime | librlimit-6e4b390 |
| libutf | commit 7a4e608 | Apache-2.0 | bare runtime | libutf-7a4e608 |
| libutf | commit dca86e6 | Apache-2.0 | bare-buffer 3.6.1 and 3.7.1 | libutf-dca86e6 |
| libutf | commit f0d532e | Apache-2.0 | bare-url 2.4.5 (bare runtime) | libutf-f0d532e |
| libutf | commit a1ceca8 | Apache-2.0 | bare 1.33.4 and bare-url 2.5.4 (Bare Kit) | libutf-a1ceca8 |
| libbase64 | commit 078ad06 | Apache-2.0 | bare-buffer | libbase64-078ad06 |
| libhex | commit a4a2c84 | Apache-2.0 | bare-buffer | libhex-a4a2c84 |
| libintrusive | commit 3903632 | Apache-2.0 | bare-dns (bare runtime) | libintrusive-3903632 |
| liblog | commit edcfd8f | Apache-2.0 | bare-system-logger | liblog-edcfd8f |
| libpunycode | commit e91ee34 | Apache-2.0 | bare-url 2.5.4 | libpunycode-e91ee34 |
| libnormalize | commit 0e81f65 | Apache-2.0 | bare-url 2.5.4 | libnormalize-0e81f65 |
| libidna | commit 1471406 | Apache-2.0 | bare-url 2.5.4 | libidna-1471406 |
| liburl | commit 2efedc5 | Apache-2.0 | bare-url 2.5.4 | liburl-2efedc5 |
| liburl | commit c00e0d7 | Apache-2.0 | bare-url 2.4.5 (bare runtime) | liburl-c00e0d7 |
| libuv | 1.51.0 | MIT | fs-native-extensions and rocksdb-native (native addons) | libuv-1.51.0 |
| RocksDB | 10.5.1 | Apache-2.0 (the GPL-2.0-only option is not taken), LevelDB code BSD-3-Clause, xxHash BSD-2-Clause | rocksdb-native (native addon) | rocksdb-10.5.1 |
| librocksdb | commit f3db3b1 | Apache-2.0 | rocksdb-native | librocksdb-f3db3b1 |
| libjstl | commit d08e05c | Apache-2.0 | quickbit-native | libjstl-d08e05c |
| libjstl | commit 7e31e67 | Apache-2.0 | rocksdb-native | libjstl-7e31e67 |
| libjstl | commit 098664c | Apache-2.0 | sodium-native, simdle-native, udx-native | libjstl-098664c |
| libquickbit | commit 5ecf908 | Apache-2.0 | quickbit-native | libquickbit-5ecf908 |
| librabin | commit 2e4f70b | Apache-2.0 | rabin-native | librabin-2e4f70b |
| libsimdle | commit 014ad4e | Apache-2.0 | simdle-native | libsimdle-014ad4e |
| libsodium | commit e18eee6 | ISC | sodium-native | libsodium-e18eee6 |
| libudx | commit 759bf76 | Apache-2.0 | udx-native | libudx-759bf76 |

RocksDB also carries a MurmurHash notice (util/murmurhash.cc: public domain, and MIT for business
use). The notice block is in rocksdb-10.5.1/murmurhash.cc.notice; the MIT text it refers to is not
vendored yet.

## Bare builtins (npm, embedded in Bare Kit and the bare runtime)

The npm packages whose JavaScript and addon code the Bare Kit and bare runtime binaries embed. The
versions are the ones in the package.json blobs of the shipped binaries (Bare Kit 2.5.5 for Android
and iOS, bare runtime 1.30.3 for desktop), so a package can appear at two versions. Each LICENSE and
NOTICE is in app/licenses/bare-builtins/ and is copied into the Flutter app.

| Package | Version | License | Embedded in |
|---|---|---|---|
| b4a | 1.8.1 | Apache-2.0 | bare runtime |
| b4a | 1.9.0 | Apache-2.0 | Bare Kit |
| bare-addon-resolve | 1.10.0 | Apache-2.0 | bare runtime |
| bare-addon-resolve | 1.10.1 | Apache-2.0 | Bare Kit |
| bare-ansi-escapes | 2.2.3 | Apache-2.0 | Bare Kit and bare runtime |
| bare-assert | 1.2.0 | Apache-2.0 | Bare Kit and bare runtime |
| bare-buffer | 3.6.1 | Apache-2.0 | bare runtime |
| bare-buffer | 3.7.1 | Apache-2.0 | Bare Kit |
| bare-bundle | 1.10.0 | Apache-2.0 | bare runtime |
| bare-bundle | 1.11.0 | Apache-2.0 | Bare Kit |
| bare-console | 6.2.0 | Apache-2.0 | Bare Kit and bare runtime |
| bare-crypto | 1.15.3 | Apache-2.0 | Bare Kit and bare runtime |
| bare-dns | 2.1.4 | Apache-2.0 | bare runtime |
| bare-events | 2.9.1 | Apache-2.0 | bare runtime |
| bare-events | 2.9.2 | Apache-2.0 | Bare Kit |
| bare-format | 1.0.2 | Apache-2.0 | Bare Kit and bare runtime |
| bare-fs | 4.8.1 | Apache-2.0 | Bare Kit |
| bare-hrtime | 2.1.1 | Apache-2.0 | bare runtime |
| bare-hrtime | 2.1.2 | Apache-2.0 | Bare Kit |
| bare-http-parser | 1.1.5 | Apache-2.0 | bare runtime |
| bare-http1 | 4.5.7 | Apache-2.0 | bare runtime |
| bare-https | 3.0.0 | Apache-2.0 | bare runtime |
| bare-inspect | 3.1.4 | Apache-2.0 | bare runtime |
| bare-inspect | 3.1.10 | Apache-2.0 | Bare Kit |
| bare-inspector | 6.1.0 | Apache-2.0 | bare runtime |
| bare-ipc | 1.1.2 | Apache-2.0 | Bare Kit |
| bare-logger | 2.0.3 | Apache-2.0 | bare runtime |
| bare-logger | 2.0.4 | Apache-2.0 | Bare Kit |
| bare-mime | 1.0.0 | Apache-2.0 | Bare Kit |
| bare-module | 6.4.0 | Apache-2.0 | bare runtime |
| bare-module | 7.0.3 | Apache-2.0 | Bare Kit |
| bare-module-lexer | 1.5.3 | Apache-2.0 | bare runtime |
| bare-module-lexer | 1.6.7 | Apache-2.0 | Bare Kit |
| bare-module-resolve | 1.12.2 | Apache-2.0 | bare runtime |
| bare-module-resolve | 1.12.5 | Apache-2.0 | Bare Kit |
| bare-module-traverse | 2.5.6 | Apache-2.0 | Bare Kit |
| bare-net | 2.3.2 | Apache-2.0 | bare runtime |
| bare-os | 3.9.3 | Apache-2.0 | bare runtime |
| bare-path | 3.0.1 | Apache-2.0 | bare runtime |
| bare-path | 3.1.2 | Apache-2.0 | Bare Kit |
| bare-pipe | 4.2.2 | Apache-2.0 | bare runtime |
| bare-pipe | 4.3.1 | Apache-2.0 | Bare Kit |
| bare-queue-microtask | 1.0.0 | Apache-2.0 | Bare Kit and bare runtime |
| bare-readline | 1.3.1 | Apache-2.0 | bare runtime |
| bare-repl | 6.1.0 | Apache-2.0 | bare runtime |
| bare-semver | 1.1.0 | Apache-2.0 | Bare Kit and bare runtime |
| bare-signals | 4.2.0 | Apache-2.0 | bare runtime |
| bare-stream | 2.13.3 | Apache-2.0 | bare runtime |
| bare-stream | 2.13.4 | Apache-2.0 | Bare Kit |
| bare-structured-clone | 1.6.0 | Apache-2.0 | bare runtime |
| bare-structured-clone | 2.0.0 | Apache-2.0 | Bare Kit |
| bare-system-logger | 1.0.3 | Apache-2.0 | bare runtime |
| bare-system-logger | 1.0.5 | Apache-2.0 | Bare Kit |
| bare-tcp | 2.5.1 | Apache-2.0 | bare runtime |
| bare-timers | 3.2.1 | Apache-2.0 | bare runtime |
| bare-timers | 3.2.3 | Apache-2.0 | Bare Kit |
| bare-tls | 3.1.7 | Apache-2.0 | bare runtime |
| bare-tty | 5.1.1 | Apache-2.0 | bare runtime |
| bare-type | 1.1.0 | Apache-2.0 | bare runtime |
| bare-type | 1.1.1 | Apache-2.0 | Bare Kit |
| bare-type-stripper | 0.1.4 | Apache-2.0 | bare runtime |
| bare-type-stripper | 0.1.7 | Apache-2.0 | Bare Kit |
| bare-unpack | 1.1.3 | Apache-2.0 | Bare Kit |
| bare-url | 2.4.5 | Apache-2.0 | bare runtime |
| bare-url | 2.5.4 | Apache-2.0 | Bare Kit |
| bare-ws | 3.0.0 | Apache-2.0 | bare runtime |
| compact-encoding | 3.3.0 | Apache-2.0 | bare runtime |
| compact-encoding | 3.5.0 | Apache-2.0 | Bare Kit |
| events-universal | 1.0.1 | Apache-2.0 | Bare Kit and bare runtime |
| fast-fifo | 1.3.2 | MIT | Bare Kit and bare runtime |
| paparam | 1.10.1 | Apache-2.0 | bare runtime |
| promaphore | 1.0.0 | MIT | Bare Kit |
| streamx | 2.28.0 | MIT | bare runtime |
| streamx | 2.28.1 | MIT | Bare Kit |
| teex | 1.0.1 | MIT | Bare Kit and bare runtime |
| text-decoder | 1.2.7 | Apache-2.0 | Bare Kit and bare runtime |

## Release archives

Each release archive (holebridge_<version>_<os>_<arch>.tar.gz, or .zip on windows) holds the binary,
LICENSE, this file and licenses/: the Go standard library's LICENSE and PATENTS, the LICENSE,
PATENTS and NOTICE of each Go module compiled in, and the LICENSE (and NOTICE) of each ported library
above. scripts/release-build.sh builds the archives and scripts/collect-licenses.sh collects the texts.
