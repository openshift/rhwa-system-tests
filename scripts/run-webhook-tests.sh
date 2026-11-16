#!/usr/bin/env bash
# Run the admission webhook specs: all of them, or those of one operator.
#
# Usage:
#   scripts/run-webhook-tests.sh [all|wao|far|nhc|nmo|snr]
#   make run-webhook-tests WEBHOOK_TARGET=snr
#
# The script selects specs by label, sets ECO_TEST_FEATURES and ECO_TEST_LABELS, and then
# starts scripts/test-runner.sh, so every other ECO_* variable of the runner still applies.
#
#   all   the webhook specs of the FAR, NHC, NMO and SNR suites and of the WAO suite
#   wao   the WAO suite only: registration, the four operand admission specs, resilience
#   far, nhc, nmo, snr
#         the webhook specs of that operator suite, and the WAO admission spec of that operand
#
# Destructive webhook specs (a kubelet stop, a node drain) are not selected. Set
# WEBHOOK_DESTRUCTIVE=true to select them too.
#
# Kept portable between GNU and BSD userland (Linux and macOS): no bash 4 features.
set -euo pipefail

TARGET="${1:-${WEBHOOK_TARGET:-all}}"
LABELS="component:webhook"

case "${TARGET}" in
all)
    FEATURES="far-operator nhc-operator nmo-operator snr-operator wao-operator"
    ;;
wao)
    FEATURES="wao-operator"
    LABELS="${LABELS} && operator:wao"
    ;;
far | nhc | nmo | snr)
    FEATURES="${TARGET}-operator wao-operator"
    LABELS="${LABELS} && operator:${TARGET}"
    ;;
*)
    echo "usage: $0 [all|wao|far|nhc|nmo|snr]" >&2
    exit 1
    ;;
esac

if [[ "${WEBHOOK_DESTRUCTIVE:-false}" != "true" ]]; then
    LABELS="${LABELS} && !disruption:destructive"
fi

export ECO_TEST_FEATURES="${FEATURES}"
export ECO_TEST_LABELS="${LABELS}"

exec "$(dirname "$0")/test-runner.sh"
