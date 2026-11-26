#!/bin/sh
set -eu

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
DATA_DIR="${DATA_DIR:-/data}"
MUSIC_DIR="${MUSIC_DIR:-/music}"

case "$PUID" in
    ''|*[!0-9]*)
        echo "entrypoint: invalid PUID: '$PUID' (must be a non-negative integer)" >&2
        exit 1
        ;;
esac

case "$PGID" in
    ''|*[!0-9]*)
        echo "entrypoint: invalid PGID: '$PGID' (must be a non-negative integer)" >&2
        exit 1
        ;;
esac

# Collapse repeated slashes before stripping trailing slashes while preserving "/".
clean_path() {
    p="$1"
    while [ "${p#*//}" != "$p" ]; do
        p="${p%%//*}/${p#*//}"
    done
    while [ "$p" != "/" ] && [ "${p%/}" != "$p" ]; do
        p="${p%/}"
    done
    printf '%s' "$p"
}

DATA_DIR="$(clean_path "$DATA_DIR")"
MUSIC_DIR="$(clean_path "$MUSIC_DIR")"

case "$DATA_DIR" in
    /*) ;;
    *)
        echo "entrypoint: invalid DATA_DIR: '$DATA_DIR' (must be an absolute path)" >&2
        exit 1
        ;;
esac

case "/$DATA_DIR/" in
    *"/../"*|*"/./"*)
        echo "entrypoint: invalid DATA_DIR: '$DATA_DIR' (must not contain '.' or '..' segments)" >&2
        exit 1
        ;;
esac

if [ -z "$DATA_DIR" ] || [ "$DATA_DIR" = "/" ]; then
    echo "entrypoint: invalid DATA_DIR: '$DATA_DIR' (refusing root directory)" >&2
    exit 1
fi

case "$MUSIC_DIR" in
    /*)
        if [ "$MUSIC_DIR" != "/" ]; then
            case "$DATA_DIR" in
                "$MUSIC_DIR"|"$MUSIC_DIR"/*)
                    echo "entrypoint: invalid DATA_DIR: '$DATA_DIR' overlaps MUSIC_DIR '$MUSIC_DIR'" >&2
                    exit 1
                    ;;
            esac
            case "$MUSIC_DIR" in
                "$DATA_DIR"/*)
                    echo "entrypoint: invalid DATA_DIR: '$DATA_DIR' is an ancestor of MUSIC_DIR '$MUSIC_DIR'" >&2
                    exit 1
                    ;;
            esac
        fi
        ;;
esac

# Keep wazero's Wasm compilation cache writable under DATA_DIR when running as numeric UID/GID.
export DATA_DIR
export HOME="$DATA_DIR"
export XDG_CACHE_HOME="$DATA_DIR/.cache"

if [ "$(id -u)" = "0" ]; then
    mkdir -p "$DATA_DIR/.cache"
    chown -R "$PUID:$PGID" "$DATA_DIR"
    exec su-exec "$PUID:$PGID" /usr/local/bin/headunit
else
    mkdir -p "$DATA_DIR/.cache"
    exec /usr/local/bin/headunit
fi
