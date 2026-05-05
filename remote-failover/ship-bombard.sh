#!/bin/bash
# Build bombard locally and scp the binary to the AWS control node.
# Run from this dev box, not from control. Reads CONTROL_IP + SSH_KEY from .env.
set -euo pipefail

cd "$(dirname "$0")"
set -a
source .env
set +a

SRC=/home/ubuntu/avalanche-benchmark/local
OUT=/tmp/bombard-shipped

echo "==> building bombard (linux/amd64) from $SRC"
GOOS=linux GOARCH=amd64 go build -C "$SRC" -o "$OUT" ./cmd/bombard

echo "==> shipping to ubuntu@$CONTROL_IP:~/bombard"
scp -i "$SSH_KEY" \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR \
    -o ControlMaster=no \
    -o ControlPath=none \
    -o ConnectTimeout=10 \
    "$OUT" "ubuntu@$CONTROL_IP:/home/ubuntu/bombard"

rm -f "$OUT"
echo "==> done. run on control: ~/bombard -rpc <URL> -tps <N>"
