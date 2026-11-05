#!/usr/bin/env bash
#
# Snapshot of everything an SNR remediation touches: node Ready/cordon/full boot
# ID/taints, any SelfNodeRemediation or NodeHealthCheck CR, the live
# minPeersForRemediation, and the agent pods with their ages.
#
#   watch -n 5 -d tools/watch/snr-watch.sh   # Linux, or macOS with `brew install watch`
#   tools/watch/snr-watch.sh --loop          # anywhere: self-refreshing, marks changes with *
#   tools/watch/snr-watch.sh                 # one snapshot and exit
#
# `watch -d` highlights whatever changed since the last tick -- a boot ID
# flipping, a node going Unknown, a taint or cordon appearing. macOS has no
# `watch`, so --loop does the same job in-script: it marks every line that was
# not in the previous tick with a leading '*'.
#
# Useful for any destructive medik8s spec, not just one: record the boot IDs of
# the nodes under test before the run and watch which ones move.
#
# A rebooting node shows up in three stages, slowest last: Ready becomes
# Unknown (~40s, stale heartbeat, not False), then the unreachable/not-ready
# taints land, then the boot ID changes -- which only happens once the kubelet
# re-registers, so it marks the node coming *back*.
#
# Portability: POSIX shell constructs, bash 3.2 (macOS system bash) and bash 5
# alike. Needs only `oc` and `awk` -- the BSD awk on macOS and gawk on Fedora
# both handle what is used here. Set SNR_WATCH_NS to override the namespace.
#
# Each tick is four small reads. Raise the interval if you would rather not add
# API traffic to a spec that is itself about api-server connectivity.

set -u

NS="${SNR_WATCH_NS:-openshift-workload-availability}"
interval=5
loop=0

usage() {
	cat <<'EOF'
usage: snr-watch.sh [--loop] [-n SECONDS]

  --loop         refresh in-script instead of exiting, marking changed lines
                 with '*' (use when `watch` is unavailable, e.g. on macOS)
  -n SECONDS     refresh interval for --loop (default 5)
  -h, --help     this message

Set SNR_WATCH_NS to watch a namespace other than openshift-workload-availability.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--loop) loop=1 ;;
	-n | --interval)
		if [ $# -lt 2 ]; then
			echo "snr-watch: $1 needs a value" >&2
			exit 2
		fi
		interval="$2"
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "snr-watch: unknown option: $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

if ! command -v oc >/dev/null 2>&1; then
	echo "snr-watch: 'oc' not found in PATH" >&2
	exit 2
fi

# nodes prints one line per node. Built from custom-columns rather than JSON so
# no python3 is needed -- macOS does not ship one unless the Xcode command line
# tools are installed.
nodes() {
	oc get nodes --no-headers -o custom-columns=\
	'N:.metadata.name,R:.status.conditions[?(@.type=="Ready")].status,C:.spec.unschedulable,B:.status.nodeInfo.bootID,TK:.spec.taints[*].key,TV:.spec.taints[*].value,TE:.spec.taints[*].effect' \
		2>/dev/null |
		awk '
			BEGIN { printf "  %-12s %-8s %-7s %-36s %s\n", "NAME", "READY", "CORDON", "BOOTID", "TAINTS (KEY | VALUE | EFFECT)" }
			{
				cordon = ($3 == "true") ? "YES" : "-"
				boot   = ($4 == "<none>" || $4 == "") ? "?" : $4
				taints = $5
				if (taints == "<none>" || taints == "") {
					taints = "-"
				} else {
					taints = $5 " | " $6 " | " $7
				}
				printf "  %-12s %-8s %-7s %-36s %s\n", $1, $2, cordon, boot, taints
				seen++
			}
			END { if (seen == 0) print "  <api unreachable>" }
		'
}

# pods indexes the trailing columns from NF rather than counting from the left:
# `oc get pods -o wide` renders a recently restarted pod as "1 (5m ago)", three
# whitespace-separated fields where normally there is one, which shifts every
# column after RESTARTS. NAME/READY/STATUS are stable from the front, AGE and
# NODE are stable from the end.
pods() {
	oc get pods -n "$NS" --no-headers -o wide 2>/dev/null |
		awk '
			NF >= 7 {
				age = $(NF - 4)
				# "6m21s" -> "6m". Ages tick every second, so keeping the
				# seconds would mark every pod line as changed on every
				# refresh and drown out the changes that matter.
				sub(/m[0-9]+s$/, "m", age)
				printf "  %-58s %-5s %-18s %-6s %s\n", $1, $2, $3, age, $(NF - 2)
				seen++
			}
			END { if (seen == 0) print "  <no pods, or api unreachable>" }
		'
}

snapshot() {
	echo "=== nodes ==="
	nodes

	echo
	echo "=== remediation CRs ==="
	crs="$(oc get selfnoderemediation,nodehealthcheck -A --no-headers 2>/dev/null)"
	if [ -n "$crs" ]; then
		printf '%s\n' "$crs" | awk 'NR <= 10 { print "  " $0 }'
	else
		echo "  none"
	fi

	echo
	echo "=== snrc ==="
	min="$(oc get selfnoderemediationconfig -n "$NS" \
		-o jsonpath='{.items[0].spec.minPeersForRemediation}' 2>/dev/null)"
	if [ -n "$min" ]; then
		echo "  minPeersForRemediation=$min"
	else
		echo "  minPeersForRemediation=<unset, or SNRC unreadable>"
	fi

	echo
	echo "=== agent pods ==="
	pods
}

if [ "$loop" -eq 0 ]; then
	echo "snr-watch $(date +%H:%M:%S)"
	snapshot
	exit 0
fi

prev="$(mktemp "${TMPDIR:-/tmp}/snr-watch.XXXXXX")"
cur="$(mktemp "${TMPDIR:-/tmp}/snr-watch.XXXXXX")"
# INT/TERM need their own handler that exits: a trap body returns to where the
# signal arrived, so cleaning up without exiting would leave ctrl-c unable to
# stop the loop at all.
trap 'rm -f "$prev" "$cur"' EXIT
trap 'rm -f "$prev" "$cur"; exit 130' INT TERM
: >"$prev"

while :; do
	snapshot >"$cur"
	# Home the cursor and clear, rather than shelling out to clear/tput: the
	# escape is understood by Terminal.app, iTerm2 and every Linux terminal.
	printf '\033[H\033[2J'
	echo "snr-watch $(date +%H:%M:%S) -- every ${interval}s, '*' marks a change (ctrl-c to stop)"
	# Keyed on FILENAME, not the usual NR==FNR: $prev is empty on the first
	# tick, and an empty first file leaves NR==FNR true for every line of the
	# second, swallowing the entire snapshot instead of printing it.
	awk -v prev="$prev" '
		FILENAME == prev { was[$0]++; next }
		{ print (($0 in was) ? "  " : "* ") $0 }
	' "$prev" "$cur"
	cat "$cur" >"$prev"
	sleep "$interval"
done
