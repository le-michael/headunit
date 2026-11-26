#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="${REPO_ROOT}/docker-compose.yml"
PROJECT_NAME="headunit-e2e-$$"

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
MUSIC_MOUNT_OPTS="${MUSIC_MOUNT_OPTS:-ro}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-90}"
SCAN_TIMEOUT="${SCAN_TIMEOUT:-30}"

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

select_free_port() {
  python3 - <<'PY'
import socket

with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
    s.bind(("127.0.0.1", 0))
    print(s.getsockname()[1])
PY
}

write_smoke_wav() {
  local wav_path="$1"
  python3 - "$wav_path" <<'PY'
import os
import sys
import wave

wav_path = sys.argv[1]
with wave.open(wav_path, "wb") as wf:
    wf.setnchannels(1)
    wf.setsampwidth(2)
    wf.setframerate(8000)
    wf.writeframes(b"\x00" * 32000)

size = os.path.getsize(wav_path)
if size != 32044:
    raise SystemExit(f"unexpected WAV size {size}, want 32044")
PY
}

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

verify_opensubsonic_http() {
  local base_url="$1"
  python3 - "$base_url" "$SCAN_TIMEOUT" <<'PY'
import json
import sys
import time
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET

base_url = sys.argv[1].rstrip("/")
scan_timeout = float(sys.argv[2])


def fetch(path, headers=None):
    req = urllib.request.Request(f"{base_url}{path}", headers=headers or {})
    with urllib.request.urlopen(req, timeout=10) as resp:
        return resp.status, resp.headers, resp.read()


# 1. XML ping
status, _, xml_body = fetch("/rest/ping")
assert status == 200, f"GET /rest/ping status={status}, want 200"
root = ET.fromstring(xml_body)
assert root.attrib.get("status") == "ok", f"XML ping status={root.attrib.get('status')!r}"
assert root.attrib.get("openSubsonic") == "true", f"XML ping openSubsonic={root.attrib.get('openSubsonic')!r}"
assert root.attrib.get("type") == "headunit", f"XML ping type={root.attrib.get('type')!r}"

# 2. JSON ping
status, _, json_body = fetch("/rest/ping?f=json")
assert status == 200, f"GET /rest/ping?f=json status={status}, want 200"
ping_payload = json.loads(json_body)["subsonic-response"]
assert ping_payload.get("status") == "ok", f"JSON ping status={ping_payload.get('status')!r}"
assert ping_payload.get("openSubsonic") is True, f"JSON ping openSubsonic={ping_payload.get('openSubsonic')!r}"
assert ping_payload.get("type") == "headunit", f"JSON ping type={ping_payload.get('type')!r}"

# 3. Poll getScanStatus until scan completes with 1 track
deadline = time.monotonic() + scan_timeout
last_scan = None
while True:
    status, _, scan_body = fetch("/rest/getScanStatus?f=json")
    assert status == 200, f"GET /rest/getScanStatus status={status}, want 200"
    scan_payload = json.loads(scan_body)["subsonic-response"]
    assert scan_payload.get("status") == "ok", f"getScanStatus status={scan_payload.get('status')!r}"
    scan_status = scan_payload.get("scanStatus") or {}
    last_scan = scan_status
    if scan_status.get("scanning") is False and scan_status.get("count") == 1:
        break
    if time.monotonic() >= deadline:
        raise SystemExit(f"timed out waiting for scan completion; last scanStatus={last_scan!r}")
    time.sleep(0.5)

# 4. search3 returns the smoke WAV track with expected metadata
status, _, search_body = fetch("/rest/search3?f=json&query=%22%22")
assert status == 200, f"GET /rest/search3 status={status}, want 200"
search_payload = json.loads(search_body)["subsonic-response"]
assert search_payload.get("status") == "ok", f"search3 status={search_payload.get('status')!r}"
songs = (search_payload.get("searchResult3") or {}).get("song") or []
matches = [song for song in songs if song.get("path") == "01 - Smoke Test.wav"]
assert len(matches) == 1, f"expected 1 song matching '01 - Smoke Test.wav', got {songs!r}"
song = matches[0]
assert song.get("title") == "Smoke Test", f"song title={song.get('title')!r}"
assert song.get("duration") == 2, f"song duration={song.get('duration')!r}"
assert song.get("bitRate") == 128, f"song bitRate={song.get('bitRate')!r}"
track_id = song.get("id")
assert track_id, f"missing song id in {song!r}"

# 5. stream with byte-range request returns 206 Partial Content and RIFF header
stream_path = f"/rest/stream?id={urllib.parse.quote(track_id, safe='')}"
status, headers, stream_body = fetch(stream_path, headers={"Range": "bytes=0-3"})
assert status == 206, f"GET {stream_path} status={status}, want 206"
assert headers.get("Content-Type") == "audio/wav", f"Content-Type={headers.get('Content-Type')!r}"
assert headers.get("Content-Range") == "bytes 0-3/32044", f"Content-Range={headers.get('Content-Range')!r}"
assert stream_body == b"RIFF", f"stream body={stream_body!r}, want b'RIFF'"

print("OpenSubsonic HTTP smoke checks passed.")
PY
}

main() {
  if [[ -z "${HOST_PORT:-}" ]]; then
    HOST_PORT="$(select_free_port)"
  fi

  TMP_DIR="$(mktemp -d)"
  MUSIC_DIR="${TMP_DIR}/music"
  mkdir -p "$MUSIC_DIR"
  chmod 0755 "$TMP_DIR" "$MUSIC_DIR"

  local wav_path="${MUSIC_DIR}/01 - Smoke Test.wav"
  write_smoke_wav "$wav_path"
  chmod 0644 "$wav_path"

  export MUSIC_DIR HOST_PORT PUID PGID SCAN_INTERVAL=0 MUSIC_MOUNT_OPTS

  COMPOSE_STARTED=1
  docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" up -d --build

  local container_id=""
  container_id="$(docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" ps -q headunit)"
  if [[ -z "$container_id" ]]; then
    echo "ERROR: failed to resolve container ID for headunit service" >&2
    return 1
  fi

  wait_for_healthy "$container_id"
  verify_container_runtime "$container_id"
  verify_opensubsonic_http "http://127.0.0.1:${HOST_PORT}"
}

main "$@"
