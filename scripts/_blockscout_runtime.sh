# shellcheck shell=bash
#
# Sourced by blockscout-up.sh / blockscout-down.sh / blockscout-pack-images.sh.
# Picks a container runtime (docker or podman) and a compose CLI variant.
#
# Resolution order for $RUNTIME:
#   1. $BLOCKSCOUT_RUNTIME (explicit override)
#   2. podman if installed (RHEL default)
#   3. docker if installed (typical macOS/Linux dev box)
#
# Resolution order for $COMPOSE (set as a bash array):
#   1. `<runtime> compose` (compose v2 plugin, both docker and podman 4.x+)
#   2. `<runtime>-compose` (legacy podman-compose / docker-compose)

detect_runtime() {
  if [[ -n "${BLOCKSCOUT_RUNTIME:-}" ]]; then
    RUNTIME="${BLOCKSCOUT_RUNTIME}"
  elif command -v podman >/dev/null 2>&1; then
    RUNTIME=podman
  elif command -v docker >/dev/null 2>&1; then
    RUNTIME=docker
  else
    echo "no container runtime found; install podman (RHEL) or docker" >&2
    exit 1
  fi

  if ! command -v "${RUNTIME}" >/dev/null 2>&1; then
    echo "BLOCKSCOUT_RUNTIME=${RUNTIME} but '${RUNTIME}' is not on PATH" >&2
    exit 1
  fi

  case "${RUNTIME}" in
    docker)
      if docker compose version >/dev/null 2>&1; then
        COMPOSE=(docker compose)
      elif command -v docker-compose >/dev/null 2>&1; then
        COMPOSE=(docker-compose)
      else
        echo "docker found but neither 'docker compose' nor 'docker-compose' available" >&2
        exit 1
      fi
      ;;
    podman)
      if podman compose version >/dev/null 2>&1; then
        COMPOSE=(podman compose)
      elif command -v podman-compose >/dev/null 2>&1; then
        COMPOSE=(podman-compose)
      else
        echo "podman found but neither 'podman compose' nor 'podman-compose' available." >&2
        echo "install podman-compose (RHEL: 'sudo dnf install podman-compose')." >&2
        exit 1
      fi
      ;;
    *)
      echo "unsupported BLOCKSCOUT_RUNTIME=${RUNTIME}" >&2
      exit 1
      ;;
  esac
}
