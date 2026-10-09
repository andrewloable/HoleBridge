#!/bin/sh
# Runs every step of the pears-go feasibility spike on localhost, in order. Every child process
# is bounded by timeout. Needs Go 1.25, Node 24 with npm, and the repo-local bare 1.34.1 that
# npm ci installs in js/. Output goes to stdout; the recorded copy is results.txt.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
export CGO_ENABLED=0 GOTOOLCHAIN=local
(cd "$here/go" && timeout 300 go build -o bin/spike ./cmd/spike)
(cd "$here/js" && timeout 300 npm ci --no-audit --no-fund)
spike="$here/go/bin/spike"
bare="$here/js/node_modules/.bin/bare"
peer="$here/js/udx-peer.js"

echo "== steps 1 and 2: testnet, JS control, Go PING and FIND_NODE"
(cd "$here/js" && timeout 180 node dht-driver.js)

echo "== step 3: Noise IK, Go initiator then Go responder, then the wrong-key negative control"
timeout 90 "$spike" noise -node "$here/js/noise-peer.js" -go-role initiator
timeout 90 "$spike" noise -node "$here/js/noise-peer.js" -go-role responder
timeout 90 "$spike" noise -node "$here/js/noise-peer.js" -go-role initiator -negative

echo "== step 4: UDX, 100 MiB, three runs each, for the JS runtime node and then bare"
for rt in node bare; do
  if [ "$rt" = node ]; then nodebin=node; else nodebin="$bare"; fi
  echo "-- runtime $rt"
  timeout 600 "$spike" udx -nodebin "$nodebin" -node "$peer" -dir go2js -bytes 104857600 -runs 3
  timeout 600 "$spike" udx -nodebin "$nodebin" -node "$peer" -dir js2go -bytes 104857600 -runs 3
  UDX_RUNTIME="$nodebin" timeout 600 node "$here/js/udx-pair.js" 104857600 3
done
echo "-- JS to JS in one process, node (not the baseline: both ends share one thread)"
(cd "$here/js" && timeout 120 node udx-peer.js self 104857600)
echo "== done"
