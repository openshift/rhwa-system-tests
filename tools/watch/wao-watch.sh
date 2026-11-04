#!/usr/bin/env bash
#
# Snapshot of what the Workload Availability Operator is installing or removing:
# the per-operand rows of the WorkloadAvailabilityConfig status, WAO's own
# reconciliation conditions, and the Deployments, DaemonSets and pods in the
# operator namespace.
#
#   watch -n 5 -d tools/watch/wao-watch.sh   # Linux, or macOS with `brew install watch`
#   tools/watch/wao-watch.sh --loop          # anywhere: self-refreshing, marks changes with *
#   tools/watch/wao-watch.sh                 # one snapshot and exit
#
# `watch -d` highlights whatever changed since the last tick -- an operand
# moving Disabled -> Progressing -> Available, a manager Deployment appearing,
# agent pods rolling out. macOS has no `watch`, so --loop does the same job
# in-script, marking every line that was not in the previous tick with a '*'.
#
# What to expect while an operand is cycled:
#
#   1. PHASE goes Progressing the moment it is enabled, and stays there until
#      every Deployment of that operand is ready -- for nodeHealthCheck that is
#      two, the manager and the console plugin, so it lags the others.
#   2. Operands with webhooks (snr, nmo, nhc) also wait on the service CA
#      controller injecting a CA bundle. ADMIT shows that gate: Progressing with
#      every Deployment ready and ADMIT=false is the CA bundle, not a crash.
#   3. On disable, PHASE usually returns to Disabled, but DisableIncomplete is a
#      normal end state, not a failure: it means agents, a config or native CRs
#      outlived the manager. RETAINED counts them -- selfNodeRemediation leaves
#      its SelfNodeRemediationConfig and agent DaemonSet behind, and any
#      NodeHealthCheck CR on the cluster keeps nodeHealthCheck incomplete.
#
# What must NOT change: the workload-availability-operator-controller-manager
# Deployment. WAO installs and removes the operands; if its own manager restarts
# mid-run, the operand phases are reporting a WAO recovery, not operand health.
#
# Portability: POSIX shell constructs, bash 3.2 (macOS system bash) and bash 5
# alike. Needs only `oc` and `awk`. Set WAO_WATCH_NS to override the namespace.
#
# Each tick is five small reads. Raise the interval to cut API traffic.

set -u

NS="${WAO_WATCH_NS:-openshift-workload-availability}"
WAC_NAME="${WAO_WATCH_WAC:-cluster}"
interval=5
loop=0

usage() {
	cat <<'EOF'
usage: wao-watch.sh [--loop] [-n SECONDS]

  --loop         refresh in-script instead of exiting, marking changed lines
                 with '*' (use when `watch` is unavailable, e.g. on macOS)
  -n SECONDS     refresh interval for --loop (default 5)
  -h, --help     this message

Set WAO_WATCH_NS to watch a namespace other than openshift-workload-availability,
and WAO_WATCH_WAC to read a WorkloadAvailabilityConfig other than 'cluster'.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--loop) loop=1 ;;
	-n | --interval)
		if [ $# -lt 2 ]; then
			echo "wao-watch: $1 needs a value" >&2
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
		echo "wao-watch: unknown option: $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

if ! command -v oc >/dev/null 2>&1; then
	echo "wao-watch: 'oc' not found in PATH" >&2
	exit 2
fi

# operands prints one line per component of the singleton. jsonpath rather than
# custom-columns because the columns live in a list: custom-columns renders
# .status.components[*].phase as one comma-joined cell, which cannot be lined up
# against the names. Fields are tab-separated so an empty message cannot shift
# the columns after it.
operands() {
	oc get workloadavailabilityconfig "$WAC_NAME" -o jsonpath=\
'{range .status.components[*]}{.name}{"\t"}{.desiredState}{"\t"}{.phase}{"\t"}{.admissionReady}{"\t"}{.retainedResources}{"\n"}{end}' \
		2>/dev/null |
		awk -F'\t' '
			BEGIN { printf "  %-28s %-9s %-18s %-6s %s\n", "OPERAND", "DESIRED", "PHASE", "ADMIT", "RETAINED" }
			$1 != "" {
				admit = ($4 == "") ? "-" : $4
				# retainedResources arrives as a JSON array; count the entries
				# rather than printing them, so one long resource name cannot
				# push the whole block off a terminal width.
				retained = 0
				if ($5 != "" && $5 != "[]") {
					retained = split($5, parts, ",")
				}
				printf "  %-28s %-9s %-18s %-6s %s\n", $1, $2, $3, admit, retained
				seen++
			}
			END { if (seen == 0) print "  <no WorkloadAvailabilityConfig, or api unreachable>" }
		'
}

# conditions renders WAO's own reconciliation state on one line.
conditions() {
	oc get workloadavailabilityconfig "$WAC_NAME" -o jsonpath=\
'{range .status.conditions[*]}{.type}={.status}{" "}{end}' 2>/dev/null |
		awk '
			{ if ($0 == "") print "  <unreported>"; else print "  " $0 }
			END { if (NR == 0) print "  <no WorkloadAvailabilityConfig, or api unreachable>" }
		'
}

# workloads lists the Deployments and DaemonSets WAO owns, plus its own manager.
workloads() {
	{
		oc get deployments -n "$NS" --no-headers \
			-o custom-columns='K:kind,N:.metadata.name,R:.status.readyReplicas,D:.spec.replicas' 2>/dev/null
		oc get daemonsets -n "$NS" --no-headers \
			-o custom-columns='K:kind,N:.metadata.name,R:.status.numberReady,D:.status.desiredNumberScheduled' 2>/dev/null
	} |
		awk '
			NF >= 4 {
				ready   = ($3 == "<none>" || $3 == "") ? 0 : $3
				desired = ($4 == "<none>" || $4 == "") ? 0 : $4
				kind    = (tolower($1) == "deployment") ? "Deploy" : "DS"
				printf "  %-6s %-54s %s/%s\n", kind, $2, ready, desired
				seen++
			}
			END { if (seen == 0) print "  <none, or api unreachable>" }
		' | sort -k2
}

# pods indexes the trailing columns from NF rather than counting from the left:
# `oc get pods -o wide` renders a recently restarted pod as "1 (5m ago)", three
# whitespace-separated fields where normally there is one, which shifts every
# column after RESTARTS. NAME/READY/STATUS are stable from the front.
pods() {
	oc get pods -n "$NS" --no-headers -o wide 2>/dev/null |
		awk '
			NF >= 7 {
				age = $(NF - 4)
				# "6m21s" -> "6m". Ages tick every second, so keeping the
				# seconds would mark every pod line as changed on every refresh
				# and drown out the changes that matter.
				sub(/m[0-9]+s$/, "m", age)
				printf "  %-58s %-5s %-18s %-6s %s\n", $1, $2, $3, age, $(NF - 2)
				seen++
			}
			END { if (seen == 0) print "  <no pods, or api unreachable>" }
		' | sort
}

snapshot() {
	echo "=== operands ($WAC_NAME) ==="
	operands

	echo
	echo "=== wao conditions ==="
	conditions

	echo
	echo "=== workloads ($NS) ==="
	workloads

	echo
	echo "=== pods ($NS) ==="
	pods
}

if [ "$loop" -eq 0 ]; then
	echo "wao-watch $(date +%H:%M:%S)"
	snapshot
	exit 0
fi

prev="$(mktemp "${TMPDIR:-/tmp}/wao-watch.XXXXXX")"
cur="$(mktemp "${TMPDIR:-/tmp}/wao-watch.XXXXXX")"
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
	echo "wao-watch $(date +%H:%M:%S) -- every ${interval}s, '*' marks a change (ctrl-c to stop)"
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
