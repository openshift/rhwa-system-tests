#!/usr/bin/env bash
#
# Snapshot of everything a NodeHealthCheck remediation touches: node
# Ready/cordon/boot ID/taints, each NodeHealthCheck CR with its phase, healthy
# and observed node counts, in-flight remediation count and reason, the
# remediation CRs NHC spawns (SNR, FAR, MDR and the dummy TestRemediation used
# by the logic specs), the controller leader Lease holder, and the controller
# pods with their ages.
#
#   watch -n 5 -d tools/watch/nhc-watch.sh   # Linux, or macOS with `brew install watch`
#   tools/watch/nhc-watch.sh --loop          # anywhere: self-refreshing, marks changes with *
#   tools/watch/nhc-watch.sh                 # one snapshot and exit
#
# `watch -d` highlights whatever changed since the last tick -- an NHC phase
# moving Enabled -> Remediating, healthyNodes dropping, a remediation CR
# appearing, a boot ID flipping as the remediator reboots the node. macOS has no
# `watch`, so --loop does the same job in-script: it marks every line that was
# not in the previous tick with a leading '*'.
#
# Useful for every NHC spec: the remediation-trigger and escalation specs that
# break a node and watch NHC react, the negative-validation and status-field
# specs that assert on phase/reason without disrupting a node, and the
# controller-lifecycle failover coverage. For a destructive run, record the boot
# IDs of the nodes under test before starting: NHC detecting an unhealthy node
# and the remediator rebooting it are two separate events, and the boot ID is
# what tells a real reboot from a node that only briefly went unhealthy.
#
# How a remediation reads, in order: a node's Ready goes False or Unknown, NHC
# holds it for the unhealthyConditions duration (default 30s), then the NHC CR
# moves Enabled -> Remediating, healthyNodes drops and inFlightRemediations
# rises, a remediation CR named after the node appears, the remediator taints and
# reboots the node (boot ID changes on the way back), and finally the CR is
# deleted and the NHC CR returns to Enabled.
#
# NHC goes Disabled -- not Remediating -- when its remediationTemplate is missing
# or the remediator is installed but not running. That is the subject of the
# negative-validation specs, not a fault, but on a remediation run a Disabled NHC
# means the remediator operand is down, so check the controller pods block.
#
# Portability: POSIX shell constructs, bash 3.2 (macOS system bash) and bash 5
# alike. Needs only `oc` and `awk` -- the BSD awk on macOS and gawk on Fedora
# both handle what is used here. Set NHC_WATCH_NS to override the namespace and
# NHC_WATCH_LEASE to override the controller lease name.
#
# Each tick is several small reads -- one per remediation Kind probed. Raise the
# interval if you would rather not add API traffic to a spec about controller
# behaviour.

set -u

NS="${NHC_WATCH_NS:-openshift-workload-availability}"
LEASE_NAME="${NHC_WATCH_LEASE:-e1f13584.medik8s.io}"
interval=5
loop=0

# Remediation Kinds NHC can create. Probed one at a time rather than in a single
# `oc get a,b,c` so a Kind whose CRD is not installed -- an operand that is not
# enabled, or a dummy test CRD that has been torn down -- yields nothing instead
# of erroring the whole batch and blanking the Kinds that do exist.
REMEDIATION_KINDS="selfnoderemediation fenceagentsremediation machinedeletionremediation testremediation"

usage() {
	cat <<'EOF'
usage: nhc-watch.sh [--loop] [-n SECONDS]

  --loop         refresh in-script instead of exiting, marking changed lines
                 with '*' (use when `watch` is unavailable, e.g. on macOS)
  -n SECONDS     refresh interval for --loop (default 5)
  -h, --help     this message

Set NHC_WATCH_NS to watch a namespace other than openshift-workload-availability,
and NHC_WATCH_LEASE to read a controller lease other than e1f13584.medik8s.io.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--loop) loop=1 ;;
	-n | --interval)
		if [ $# -lt 2 ]; then
			echo "nhc-watch: $1 needs a value" >&2
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
		echo "nhc-watch: unknown option: $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

if ! command -v oc >/dev/null 2>&1; then
	echo "nhc-watch: 'oc' not found in PATH" >&2
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
				# Keep only the meaningful segment of each taint key so a
				# remediator taint (out-of-service, the FAR NoSchedule taint) and
				# unreachable all stay readable in a narrow column.
				gsub(/node-role\.kubernetes\.io\/|node\.kubernetes\.io\/|remediation\.medik8s\.io\//, "", taints)
				if (taints == "<none>" || taints == "") taints = "-"
				printf "  %-12s %-8s %-7s %-9s %s\n", $1, $2, cordon, boot, taints
				seen++
			}
			END { if (seen == 0) print "  <api unreachable>" }
		'
}

# nhc_crs prints one line per NodeHealthCheck with the status fields the specs
# assert on. Fields are tab-separated so an empty one cannot shift the columns
# after it, and inFlightRemediations -- a map keyed by node -- is reduced to an
# entry count rather than printed, so one long node/timestamp pair cannot push
# the block off the terminal.
nhc_crs() {
	oc get nodehealthcheck -A -o jsonpath=\
'{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{.status.healthyNodes}{"\t"}{.status.observedNodes}{"\t"}{.status.inFlightRemediations}{"\t"}{.status.reason}{"\n"}{end}' \
		2>/dev/null |
		awk -F'\t' '
			BEGIN { printf "  %-26s %-11s %-7s %-8s %-8s %s\n", "NAME", "PHASE", "HEALTHY", "OBSERVED", "INFLIGHT", "REASON" }
			$1 != "" {
				phase    = ($2 == "") ? "-" : $2
				healthy  = ($3 == "") ? "?" : $3
				observed = ($4 == "") ? "?" : $4
				# "map[node:timestamp node:timestamp]" -> count of pairs.
				# Timestamps carry no spaces, so pairs are space-separated.
				inflight = $5
				sub(/^map\[/, "", inflight)
				sub(/\]$/, "", inflight)
				count = (inflight == "") ? 0 : split(inflight, pairs, " ")
				reason = ($6 == "") ? "-" : $6
				printf "  %-26s %-11s %-7s %-8s %-8s %s\n", $1, phase, healthy, observed, count, reason
				seen++
			}
			END { if (seen == 0) print "  <no NodeHealthCheck CRs>" }
		'
}

# remediation_crs lists the remediation CRs NHC creates, probing each Kind
# separately (see REMEDIATION_KINDS) so an uninstalled Kind is silently skipped.
remediation_crs() {
	for kind in $REMEDIATION_KINDS; do
		oc get "$kind" -A --no-headers 2>/dev/null |
			awk -v k="$kind" '{ print "  " k "/" $0 }'
	done |
		awk '
			{ print; seen++ }
			END { if (seen == 0) print "  none" }
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
	echo "=== NodeHealthCheck CRs ==="
	nhc_crs

	echo
	echo "=== remediation CRs ==="
	remediation_crs

	echo
	echo "=== controller lease ==="
	lease

	echo
	echo "=== controller pods ($NS) ==="
	pods
}

if [ "$loop" -eq 0 ]; then
	echo "nhc-watch $(date +%H:%M:%S)"
	snapshot
	exit 0
fi

prev="$(mktemp "${TMPDIR:-/tmp}/nhc-watch.XXXXXX")"
cur="$(mktemp "${TMPDIR:-/tmp}/nhc-watch.XXXXXX")"
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
	echo "nhc-watch $(date +%H:%M:%S) -- every ${interval}s, '*' marks a change (ctrl-c to stop)"
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
