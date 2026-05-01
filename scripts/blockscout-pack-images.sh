#!/usr/bin/env bash
#
# Pulls the Blockscout-stack images on a build machine (with internet) and
# saves them into a single OCI archive that can be `docker load -i` /
# `podman load -i`-ed on an air-gapped target.
#
# Usage:
#   ./scripts/blockscout-pack-images.sh [OUTPUT_PATH]
#
# OUTPUT_PATH defaults to blockscout/images.tar.gz at the repo root.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

# shellcheck source=_blockscout_runtime.sh
source "${SCRIPT_DIR}/_blockscout_runtime.sh"
# shellcheck source=/dev/null
source "${ROOT_DIR}/blockscout/images.conf"

OUTPUT="${1:-${ROOT_DIR}/blockscout/images.tar.gz}"

IMAGES=(
  "${BLOCKSCOUT_BACKEND_IMAGE}"
  "${BLOCKSCOUT_FRONTEND_IMAGE}"
  "${BLOCKSCOUT_POSTGRES_IMAGE}"
  "${BLOCKSCOUT_REDIS_IMAGE}"
)

detect_runtime

echo "Using ${RUNTIME} for platform ${BLOCKSCOUT_PLATFORM}."
echo "Pulling images:"
for img in "${IMAGES[@]}"; do
  echo "  - ${img}"
  "${RUNTIME}" pull --platform "${BLOCKSCOUT_PLATFORM}" "${img}"
done

mkdir -p "$(dirname "${OUTPUT}")"

TMP="$(mktemp -t blockscout-images-XXXX.tar)"
trap 'rm -f "${TMP}"' EXIT

echo
echo "Saving multi-image archive..."
case "${RUNTIME}" in
  podman)
    podman save --multi-image-archive --output "${TMP}" "${IMAGES[@]}"
    ;;
  docker)
    docker save --output "${TMP}" "${IMAGES[@]}"
    ;;
esac

echo "Compressing to ${OUTPUT}..."
gzip -c "${TMP}" > "${OUTPUT}"

echo
echo "Done."
ls -lh "${OUTPUT}"
