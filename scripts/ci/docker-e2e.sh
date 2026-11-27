#!/usr/bin/env bash
# Orchestrates the isolated Docker Compose E2E test suite (./e2e).
# Env overrides: HOST_PORT (default 127.0.0.1:, trailing colon allocates an ephemeral
# loopback port via 127.0.0.1::4533; e.g., 127.0.0.1:14533 for a fixed port),
# PUID (1000), PGID (1000), MUSIC_MOUNT_OPTS (ro),
# HEALTH_TIMEOUT (default 90, positive integer in seconds),
# SCAN_TIMEOUT (default 30s, Go duration).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="${REPO_ROOT}/docker-compose.yml"
PROJECT_NAME="headunit-e2e-$$"

HOST_PORT="${HOST_PORT:-127.0.0.1:}"
PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
MUSIC_MOUNT_OPTS="${MUSIC_MOUNT_OPTS:-ro}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-90}"
SCAN_TIMEOUT="${SCAN_TIMEOUT:-30s}"

TMP_DIR=""
COMPOSE_STARTED=0

cleanup() {
  local status=$?
  set +e
  if [[ "$status" -ne 0 && "$COMPOSE_STARTED" -eq 1 ]]; then
    echo "::group::Docker Compose diagnostics"
    docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" ps -a
    docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" logs --no-color
    echo "::endgroup::"
  fi
  if [[ "$COMPOSE_STARTED" -eq 1 ]]; then
    docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" down -v --rmi local --remove-orphans
  fi
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf "$TMP_DIR"
  fi
  exit "$status"
}

trap 'exit 130' INT
trap 'exit 143' TERM
trap cleanup EXIT

wait_for_healthy() {
  local container_id="$1"
  local elapsed=0
  local state_line=""
  local status=""
  local restart_count=0
  local health=""

  while (( elapsed < HEALTH_TIMEOUT )); do
    state_line="$(docker inspect --format '{{.State.Status}} {{.RestartCount}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container_id")"
    read -r status restart_count health <<< "$state_line"

    case "$status" in
      exited|dead|restarting)
        echo "ERROR: container entered terminal/restarting state: ${state_line}" >&2
        return 1
        ;;
    esac

    if (( restart_count > 0 )); then
      echo "ERROR: container restarted unexpectedly (RestartCount=${restart_count}): ${state_line}" >&2
      return 1
    fi

    if [[ "$health" == "unhealthy" ]]; then
      echo "ERROR: container healthcheck reported unhealthy: ${state_line}" >&2
      return 1
    fi

    # Fail fast if healthcheck is missing so restart: unless-stopped cannot spin until HEALTH_TIMEOUT.
    if [[ "$status" == "running" && "$health" == "none" ]]; then
      echo "ERROR: container is running without a healthcheck: ${state_line}" >&2
      return 1
    fi

    if [[ "$health" == "healthy" ]]; then
      echo "Container is healthy after ${elapsed}s (${state_line})."
      return 0
    fi

    sleep 1
    elapsed=$((elapsed + 1))
  done

  echo "ERROR: timed out after ${HEALTH_TIMEOUT}s waiting for container healthcheck (last state: ${state_line})" >&2
  return 1
}

verify_container_runtime() {
  local container_id="$1"
  local pid1_uid=""
  local pid1_gid=""

  pid1_uid="$(docker exec "$container_id" awk '$1 == "Uid:" { print $2 ":" $3 }' /proc/1/status)"
  pid1_gid="$(docker exec "$container_id" awk '$1 == "Gid:" { print $2 ":" $3 }' /proc/1/status)"

  if [[ "$pid1_uid" != "${PUID}:${PUID}" ]]; then
    echo "ERROR: PID 1 Uid mismatch: got ${pid1_uid}, want ${PUID}:${PUID}" >&2
    return 1
  fi
  if [[ "$pid1_gid" != "${PGID}:${PGID}" ]]; then
    echo "ERROR: PID 1 Gid mismatch: got ${pid1_gid}, want ${PGID}:${PGID}" >&2
    return 1
  fi

  if docker exec "$container_id" sh -c 'touch /music/.ci-write-probe' 2>/dev/null; then
    echo "ERROR: /music is writable inside container; expected read-only mount" >&2
    return 1
  fi

  docker exec "$container_id" ffmpeg -version >/dev/null
}

main() {
  if [[ ! "$HEALTH_TIMEOUT" =~ ^[1-9][0-9]*$ ]]; then
    echo "ERROR: HEALTH_TIMEOUT must be a positive integer in seconds (got: '${HEALTH_TIMEOUT}')" >&2
    return 1
  fi

  TMP_DIR="$(mktemp -d)"
  MUSIC_DIR="${TMP_DIR}/music"
  mkdir -p "$MUSIC_DIR"
  # Host runner UID (e.g., 1001) may differ from non-root container PUID (1000), so allow traversal.
  chmod 0755 "$TMP_DIR" "$MUSIC_DIR"

  export MUSIC_DIR HOST_PORT PUID PGID SCAN_INTERVAL=0 MUSIC_MOUNT_OPTS

  COMPOSE_STARTED=1
  docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" up -d --build

  local container_id=""
  container_id="$(docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" ps -a -q headunit)"
  container_id="${container_id%%$'\n'*}"
  if [[ -z "$container_id" ]]; then
    echo "ERROR: failed to resolve container ID for headunit service" >&2
    return 1
  fi

  wait_for_healthy "$container_id"
  verify_container_runtime "$container_id"

  local port_output=""
  local port_mapping=""
  local mapped_host=""
  local mapped_port=""
  port_output="$(docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" port headunit 4533)"
  port_mapping="${port_output%%$'\n'*}"
  mapped_host="${port_mapping%:*}"
  mapped_port="${port_mapping##*:}"
  if [[ "$port_mapping" != *:* || -z "$mapped_host" || ! "$mapped_port" =~ ^[1-9][0-9]*$ ]]; then
    echo "ERROR: failed to resolve published host port for headunit:4533 (got: '${port_output}')" >&2
    return 1
  fi
  case "$mapped_host" in
    0.0.0.0|::|"[::]")
      mapped_host="127.0.0.1"
      ;;
    *:*)
      if [[ "$mapped_host" != \[?*\] ]]; then
        mapped_host="[${mapped_host}]"
      fi
      ;;
  esac
  local base_url="http://${mapped_host}:${mapped_port}"

  (
    cd "$REPO_ROOT"
    HEADUNIT_E2E_URL="$base_url" \
    HEADUNIT_E2E_MUSIC_DIR="$MUSIC_DIR" \
    HEADUNIT_E2E_SCAN_TIMEOUT="$SCAN_TIMEOUT" \
      go test -tags=e2e -v -count=1 ./e2e
  )
}

main "$@"
