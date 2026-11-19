#!/usr/bin/env bash
set -uo pipefail

child_pid=
cleanup() {
    if [[ -n $child_pid ]]; then
        kill "$child_pid" 2>/dev/null || true
        wait "$child_pid" 2>/dev/null || true
    fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# A reset on one forwarded connection can terminate oc and close every listener.
while true; do
    "$@" &
    child_pid=$!
    wait "$child_pid"
    status=$?
    child_pid=
    printf '%s port-forward exited with status %s. Retrying in 1s.\n' \
        "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$status" >&2
    sleep 1 &
    child_pid=$!
    wait "$child_pid"
    child_pid=
done
