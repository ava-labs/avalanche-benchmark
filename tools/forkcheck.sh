#!/usr/bin/env bash
# Compare one block hash across every L1 node. One distinct hash means no
# fork. Run this after every load run: TPS cannot see a fork, because a
# minority branch changes no throughput number.
#
# The check reads eth_blockNumber from every main-L1 node, takes the
# minimum, and compares the block hash at (minimum - 5) on every node.
# Run it from the deployment root (it reads nodes.ini and network.env).
set -euo pipefail

CHAIN_ID="$(grep '^CHAIN_ID=' deployment/network.env | cut -d= -f2)"
test -n "$CHAIN_ID" || { echo "forkcheck: no CHAIN_ID in deployment/network.env" >&2; exit 1; }

rpc() { # rpc <host> <port> <method> <params-json>
  curl -sf -m 10 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$3\",\"params\":$4}" \
    "http://$1:$2/ext/bc/$CHAIN_ID/rpc" | sed -E 's/.*"result":"?([^",}]*)"?.*/\1/'
}

# Build "<number> <host> <port>" for every main-L1 node. Ports are
# positional per host, in inventory order, and the pchain and oracle nodes
# are skipped after their port slot is counted.
targets="$(awk '
  /^[[:space:]]*#/ || NF == 0 { next }
  {
    number = $1; host = ""; role = ""
    for (i = 2; i <= NF; i++) {
      if ($i ~ /^host=/) { host = substr($i, 6) }
      if ($i ~ /^role=/) { role = substr($i, 6) }
    }
    port = 9650 + 2 * seen[host]; seen[host]++
    if (role == "validator" || role == "rpc" || role == "archive") {
      print number, host, port
    }
  }' nodes.ini)"

minimum=""
while read -r number host port; do
  raw="$(rpc "$host" "$port" eth_blockNumber '[]' || true)"
  test -n "$raw" || { echo "node $number ($host:$port): no answer" >&2; continue; }
  height=$((16#${raw#0x}))
  echo "node $number height $height"
  if test -z "$minimum" || test "$height" -lt "$minimum"; then minimum=$height; fi
done <<< "$targets"
test -n "$minimum" || { echo "forkcheck: no node answered" >&2; exit 1; }

target=$((minimum - 5))
test "$target" -ge 0 || target=0
probe="$(printf '0x%x' "$target")"
echo "comparing block $target on every node"

hashes=""
while read -r number host port; do
  hash="$(curl -sf -m 10 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"eth_getBlockByNumber\",\"params\":[\"$probe\",false]}" \
    "http://$host:$port/ext/bc/$CHAIN_ID/rpc" \
    | tr ',' '\n' | grep '"hash"' | head -1 | sed -E 's/.*"hash":"([^"]*)".*/\1/' || true)"
  test -n "$hash" || { echo "node $number ($host:$port): no block" >&2; continue; }
  echo "node $number $hash"
  hashes="$hashes$hash"$'\n'
done <<< "$targets"

distinct="$(printf '%s' "$hashes" | sort -u | grep -c . || true)"
if test "$distinct" = 1; then
  echo "OK: one hash at block $target"
else
  echo "FORK: $distinct distinct hashes at block $target" >&2
  exit 1
fi
