#!/usr/bin/env bash

# This display does not provide the continuous evidence required by the specs.
set -u

namespace="${SBR_WATCH_NS:-openshift-workload-availability}"
interval=5
loop=0

usage() {
	printf '%s\n' 'usage: sbr-watch.sh [--loop] [-n SECONDS]' \
		'  --loop       Refresh the display. Mark changed lines with *.' \
		'  -n SECONDS   Set the refresh interval. The default is 5.' \
		'  -h, --help   Show this help.' \
		'Set SBR_WATCH_NS to select another operator namespace.'
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--loop) loop=1 ;;
	-n | --interval)
		if [ "$#" -lt 2 ]; then
			printf '%s\n' 'sbr-watch: the interval needs a value' >&2
			exit 2
		fi
		interval="$2"
		shift
		;;
	-n[0-9]*)
		interval="${1#-n}"
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		printf 'sbr-watch: unknown option: %s\n' "$1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

case "$interval" in
'' | *[!0-9]*)
	printf '%s\n' 'sbr-watch: the interval must be a positive integer' >&2
	exit 2
	;;
esac
if ! awk -v interval="$interval" 'BEGIN { exit !(interval > 0) }'; then
	printf '%s\n' 'sbr-watch: the interval must be a positive integer' >&2
	exit 2
fi

if ! command -v oc >/dev/null 2>&1; then
	printf '%s\n' 'sbr-watch: oc is not available' >&2
	exit 2
fi

nodes() {
	local records
	if ! records=$(oc get nodes --request-timeout=10s -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.metadata.uid}{"|"}{.status.conditions[?(@.type=="Ready")].status}{"|"}{.spec.unschedulable}{"|"}{.status.nodeInfo.bootID}{"|"}{.status.conditions[?(@.type=="SBRStorageUnhealthy")].status}{"|"}{range .spec.taints[*]}{.key}{"="}{.value}{":"}{.effect}{","}{end}{"\n"}{end}' 2>&1); then
		printf '  <Node read failed: %s>\n' "$records"

		return
	fi
	printf '%s\n' "$records" | awk -F '|' '
		BEGIN { print "  NAME | UID | READY | CORDON | FULL BOOT ID | STORAGE UNHEALTHY | TAINTS" }
		NF >= 6 {
			for (column = 1; column <= 7; column++) if ($column == "") $column = "<unset>"
			printf "  %s | %s | %s | %s | %s | %s | %s\n", $1, $2, $3, $4, $5, $6, $7
			seen++
		}
		END { if (!seen) print "  <no Nodes>" }
	'
}

requests() {
	local records
	if ! records=$(oc get storagebasedremediations -A --request-timeout=10s -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"|"}{.metadata.uid}{"|"}{.metadata.name}{"|"}{range .status.conditions[*]}{.type}{"="}{.status}{"("}{.reason}{"),"}{end}{"|"}{.status.cleanup.phase}{"|"}{.status.cleanup.startedAt}{"|"}{.status.cleanup.deadline}{"\n"}{end}' 2>&1); then
		printf '  <SBR request read failed: %s>\n' "$records"

		return
	fi
	printf '%s\n' "$records" | awk -F '|' '
		BEGIN { print "  NAME | UID | TARGET | CONDITIONS | CLEANUP | START | DEADLINE" }
		NF >= 6 {
			for (column = 1; column <= 7; column++) if ($column == "") $column = "<unset>"
			printf "  %s | %s | %s | %s | %s | %s | %s\n", $1, $2, $3, $4, $5, $6, $7
			seen++
		}
		END { if (!seen) print "  <no SBR requests>" }
	'
}

pods() {
	local records
	if ! records=$(oc get pods -n "$namespace" --request-timeout=10s -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.metadata.uid}{"|"}{.spec.nodeName}{"|"}{.status.phase}{"|"}{range .status.containerStatuses[*]}{.name}{"="}{.ready}{"(restarts:"}{.restartCount}{"),"}{end}{"\n"}{end}' 2>&1); then
		printf '  <Pod read failed: %s>\n' "$records"

		return
	fi
	printf '%s\n' "$records" | awk -F '|' '
		BEGIN { print "  NAME | UID | NODE | PHASE | CONTAINER READINESS" }
		NF >= 4 {
			for (column = 1; column <= 5; column++) if ($column == "") $column = "<unset>"
			printf "  %s | %s | %s | %s | %s\n", $1, $2, $3, $4, $5
			seen++
		}
		END { if (!seen) print "  <no Pods>" }
	'
}

snapshot() {
	printf '%s\n' '=== Nodes ==='
	nodes
	printf '\n%s\n' '=== SBR requests ==='
	requests
	printf '\n=== Agent and reporter Pods: %s ===\n' "$namespace"
	pods
}

if [ "$loop" -eq 0 ]; then
	snapshot
	exit 0
fi

previous=""
while :; do
	current=$(snapshot)
	printf '\033[H\033[2J'
	printf 'sbr-watch: every %ss. * marks a changed line. Press Ctrl-C to stop.\n' "$interval"
	printf '%s\n' "$previous" '---SBR-WATCH-CURRENT---' "$current" | awk '
		$0 == "---SBR-WATCH-CURRENT---" { current = 1; next }
		!current { previous[$0] = 1; next }
		{ print (($0 in previous) ? "  " : "* ") $0 }
	'
	previous="$current"
	sleep "$interval"
done
