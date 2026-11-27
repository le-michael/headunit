#!/usr/bin/env bash
set -euo pipefail

HEADUNIT_SERVICE="${HEADUNIT_SERVICE:-headunit}"

usage() {
    cat >&2 <<'EOF'
Usage: headunit-ctl.sh <command> [args...]

Runs headunit CLI commands inside the running Docker Compose container.

Examples:
  headunit-ctl.sh user create --username alice --admin
  headunit-ctl.sh user list
  headunit-ctl.sh apikey create --username alice --name laptop
  headunit-ctl.sh --help

Environment:
  HEADUNIT_SERVICE  Docker Compose service name (default: headunit)
EOF
}

if [[ "$#" -eq 0 ]]; then
    usage
    exit 1
fi

validate_env_var() {
    local var_name="$1"
    if [[ ! "$var_name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
        echo "headunit-ctl: invalid environment variable name for --password-env (must match ^[A-Za-z_][A-Za-z0-9_]*$)" >&2
        exit 1
    fi
}

has_help=0
has_password_source=0
compose_env_args=()

args=("$@")
i=0
while (( i < ${#args[@]} )); do
    arg="${args[$i]}"
    case "$arg" in
        --)
            break
            ;;
        --username|-username|--name|-name|--id|-id)
            i=$((i + 2))
            continue
            ;;
        -h|--help|-help)
            has_help=1
            ;;
        --password-stdin|-password-stdin|--password-stdin=*|-password-stdin=*)
            has_password_source=1
            ;;
        --password-env|-password-env)
            if (( i + 1 >= ${#args[@]} )); then
                printf 'headunit-ctl: %s requires an environment variable name\n' "$arg" >&2
                exit 1
            fi
            var_name="${args[$((i + 1))]}"
            validate_env_var "$var_name"
            compose_env_args+=(-e "$var_name")
            has_password_source=1
            i=$((i + 2))
            continue
            ;;
        --password-env=*|-password-env=*)
            var_name="${arg#*=}"
            validate_env_var "$var_name"
            compose_env_args+=(-e "$var_name")
            has_password_source=1
            ;;
    esac
    i=$((i + 1))
done

SAVED_STTY=""

restore_tty() {
    if [[ -n "$SAVED_STTY" ]]; then
        stty "$SAVED_STTY" 2>/dev/null || true
        SAVED_STTY=""
    fi
}

on_int() {
    restore_tty
    printf '\n' >&2
    exit 130
}

on_term() {
    restore_tty
    printf '\n' >&2
    exit 143
}

if [[ "$#" -ge 2 ]] && [[ "$1" == "user" ]] && { [[ "$2" == "create" ]] || [[ "$2" == "passwd" ]]; } && [[ "$has_password_source" -eq 0 ]] && [[ "$has_help" -eq 0 ]] && [[ -t 0 ]]; then
    SAVED_STTY="$(stty -g)"
    trap restore_tty EXIT
    trap on_int INT
    trap on_term TERM

    stty -echo

    printf 'Password: ' >&2
    if ! IFS= read -r pw1; then
        restore_tty
        trap - INT TERM EXIT
        printf '\n' >&2
        exit 1
    fi
    printf '\n' >&2

    printf 'Confirm password: ' >&2
    if ! IFS= read -r pw2; then
        restore_tty
        trap - INT TERM EXIT
        printf '\n' >&2
        exit 1
    fi
    printf '\n' >&2

    restore_tty
    trap - INT TERM EXIT

    if [[ -z "$pw1" ]]; then
        echo "headunit-ctl: password must not be empty" >&2
        exit 1
    fi
    if [[ "$pw1" != "$pw2" ]]; then
        echo "headunit-ctl: passwords do not match" >&2
        exit 1
    fi
    unset pw2

    printf '%s\n' "$pw1" | docker compose exec -T "$HEADUNIT_SERVICE" /usr/local/bin/entrypoint.sh "$1" "$2" --password-stdin "${@:3}"
    exit $?
fi

exec docker compose exec -T ${compose_env_args[@]+"${compose_env_args[@]}"} "$HEADUNIT_SERVICE" /usr/local/bin/entrypoint.sh "$@"
