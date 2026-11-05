#!/usr/bin/env bash
#
# Snapshot of everything a FAR remediation touches: node Ready/cordon/boot
# ID/taints, any FenceAgentsRemediation, FenceAgentsRemediationTemplate or
# NodeHealthCheck CR, the live conditions of each FAR CR, the controller leader
# Lease holder, and the controller pods with their ages.
#
#   watch -n 5 -d tools/watch/far-watch.sh   # Linux, or macOS with `brew install watch`
#   tools/watch/far-watch.sh --loop          # anywhere: self-refreshing, marks changes with *
#   tools/watch/far-watch.sh                 # one snapshot and exit
#
# `watch -d` highlights whatever changed since the last tick -- a boot ID
# flipping, a node going Unknown, a FAR taint appearing, a CR condition moving to
# True. macOS has no `watch`, so --loop does the same job in-script: it marks
# every line that was not in the previous tick with a leading '*'.
#
# Useful for any FAR spec -- destructive reboot/fence tests most of all, but also
# the controller-lifecycle (leader handover), negative-validation and FAR<->NHC
# interop specs. Record the boot IDs of the nodes under test before the run and
# watch which ones move: a real fence changes the boot ID, a node that merely
# went briefly unreachable does not.
#
# A fenced node shows up in stages, slowest last: Ready becomes Unknown (~40s,
# stale heartbeat, not False), the FAR NoSchedule taint and -- with the
# OutOfServiceTaint strategy -- the out-of-service taint land, then the
# unreachable/not-ready taints, and finally the boot ID changes once the kubelet
# re-registers, which marks the node coming *back*. The FAR CR walks Processing
# -> FenceAgentActionSucceeded -> Succeeded alongside this; the controller
# removes its NoSchedule taint once the CR is deleted.
#
# Portability: POSIX shell constructs, bash 3.2 (macOS system bash) and bash 5
# alike. Needs only `oc` and `awk` -- the BSD awk on macOS and gawk on Fedora
# both handle what is used here. Set FAR_WATCH_NS to override the namespace and
# FAR_WATCH_LEASE to override the controller lease name.
#
# Each tick is five small reads. Raise the interval if you would rather not add
# API traffic to a spec that is itself about controller or api-server behaviour.

set -u

NS="${FAR_WATCH_NS:-openshift-workload-availability}"
LEASE_NAME="${FAR_WATCH_LEASE:-cb305759.medik8s.io}"
interval=5
loop=0

usage() {
	cat <<'EOF'
usage: far-watch.sh [--loop] [-n SECONDS]

  --loop         refresh in-script instead of exiting, marking changed lines
                 with '*' (use when `watch` is unavailable, e.g. on macOS)
  -n SECONDS     refresh interval for --loop (default 5)
  -h, --help     this message

Set FAR_WATCH_NS to watch a namespace other than openshift-workload-availability,
and FAR_WATCH_LEASE to read a controller lease other than cb305759.medik8s.io.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--loop) loop=1 ;;
	-n | --interval)
		if [ $# -lt 2 ]; then
			echo "far-watch: $1 needs a value" >&2
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
		echo "far-watch: unknown option: $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

if ! command -v oc >/dev/null 2>&1; then
	echo "far-watch: 'oc' not found in PATH" >&2
	exit 2
fi

# nodes prints one line per node. Built from custom-columns rather than JSON so
# no python3 is needed -- macOS does not ship one unless the Xcode command line
# tools are installed.
nodes() {
	oc get nodes --no-headers -o custom-columns=\
'N:.metadata.name,R:.status.conditions[?(@.type=="Ready")].status,C:.spec.unschedulable,B:.status.nodeInfo.bootID,T:.spec.taints[*].key' \
		2>/dev/null |
		awk '
			BEGIN { printf "  %-12s %-8s %-7s %-9s %s\n", "NAME", "READY", "CORDON", "BOOTID", "TAINTS" }
			{
				cordon = ($3 == "true") ? "YES" : "-"
				boot   = ($4 == "<none>" || $4 == "") ? "?" : substr($4, 1, 8)
				taints = $5
				# Keep only the meaningful segment of each taint key so the FAR
				# NoSchedule taint, out-of-service and unreachable all stay
				# readable in a narrow column.
				gsub(/node-role\.kubernetes\.io\/|node\.kubernetes\.io\/|remediation\.medik8s\.io\//, "", taints)
				if (taints == "<none>" || taints == "") taints = "-"
				printf "  %-12s %-8s %-7s %-9s %s\n", $1, $2, cordon, boot, taints
				seen++
			}
			END { if (seen == 0) print "  <api unreachable>" }
		'
}

# crs lists the FAR CRs and any NodeHealthCheck across all namespaces. The
# FAR<->NHC interop path has NHC create the FAR CR, so both are worth seeing.
crs() {
	oc get fenceagentsremediation,fenceagentsremediationtemplate,nodehealthcheck \
		-A --no-headers 2>/dev/null |
		awk '
			NR <= 15 { print "  " $0; seen++ }
			END { if (seen == 0) print "  none" }
		'
}

# far_conditions prints one line per FenceAgentsRemediation CR with its three
# lifecycle conditions. The conditions are collected into a fixed column order in
# awk rather than read by three jsonpath filters, so the layout is stable no
# matter what order the conditions array is written in.
far_conditions() {
	oc get fenceagentsremediation -A -o jsonpath=\
'{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\t"}{range .status.conditions[*]}{.type}{"="}{.status}{";"}{end}{"\n"}{end}' \
		2>/dev/null |
		awk -F'\t' '
			BEGIN { printf "  %-40s %-11s %-11s %-10s\n", "FAR", "PROCESSING", "FENCEAGENT", "SUCCEEDED" }
			$1 != "" {
				proc = "-"; fa = "-"; succ = "-"
				n = split($2, conds, ";")
				for (idx = 1; idx <= n; idx++) {
					split(conds[idx], kv, "=")
					if (kv[1] == "Processing") proc = kv[2]
					else if (kv[1] == "FenceAgentActionSucceeded") fa = kv[2]
					else if (kv[1] == "Succeeded") succ = kv[2]
				}
				printf "  %-40s %-11s %-11s %-10s\n", $1, proc, fa, succ
				seen++
			}
			END { if (seen == 0) print "  <no FenceAgentsRemediation CRs>" }
		'
}

# lease prints the controller leader election holder. The holderIdentity is
# stable until a handover, so it does not churn the display; renewTime, which
# ticks every few seconds, is deliberately left out.
lease() {
	holder="$(oc get lease "$LEASE_NAME" -n "$NS" \
		-o jsonpath='{.spec.holderIdentity}' 2>/dev/null)"
	if [ -n "$holder" ]; then
		echo "  holder=$holder"
	else
		echo "  <no lease $LEASE_NAME, or api unreachable>"
	fi
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
		' | sort
}

snapshot() {
	echo "=== nodes ==="
	nodes

	echo
	echo "=== remediation CRs ==="
	crs

	echo
	echo "=== FAR conditions ==="
	far_conditions

	echo
	echo "=== controller lease ==="
	lease

	echo
	echo "=== controller pods ($NS) ==="
	pods
}

if [ "$loop" -eq 0 ]; then
	echo "far-watch $(date +%H:%M:%S)"
	snapshot
	exit 0
fi

prev="$(mktemp "${TMPDIR:-/tmp}/far-watch.XXXXXX")"
cur="$(mktemp "${TMPDIR:-/tmp}/far-watch.XXXXXX")"
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
	echo "far-watch $(date +%H:%M:%S) -- every ${interval}s, '*' marks a change (ctrl-c to stop)"
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
