#!/usr/bin/env bash
#
# Snapshot of what an SNR condition spec asserts on: every
# SelfNodeRemediation CR with its Processing and Succeeded conditions, the
# reason behind them, whether a node of that name actually exists, and the
# deletion/finalizer state that cleanup depends on.
#
#   watch -n 5 -d tools/watch/snr-conditions-watch.sh   # Linux, or macOS with `brew install watch`
#   tools/watch/snr-conditions-watch.sh --loop          # anywhere: self-refreshing, marks changes with *
#   tools/watch/snr-conditions-watch.sh                 # one snapshot and exit
#
# Written for the SNR processing-condition specs -- "Verify SNR conditions
# with non-existent node name" (70584) and "Verify SNR conditions with
# nhc-timed-out annotation" (60881) -- and suits any spec that asserts on SNR
# status conditions rather than on a reboot. Use snr-watch.sh instead when the
# spec actually remediates a node: that one watches boot IDs and taints, which
# is what a destructive run turns on.
#
# These specs are fast. Both CRs are created, asserted on, and deleted by
# DeferCleanup inside a few seconds, so start this *before* the run -- a
# snapshot taken afterwards shows an empty table and tells you nothing.
#
# Portability: POSIX shell constructs, bash 3.2 (macOS system bash) and bash 5
# alike. Needs only `oc` and `awk` -- the BSD awk on macOS and gawk on Fedora
# both handle what is used here. Set SNR_WATCH_NS to override the namespace.
#
# Each tick is four small reads.

set -u

NS="${SNR_WATCH_NS:-openshift-workload-availability}"
WAC="${WAO_WATCH_WAC:-cluster}"
interval=5
loop=0

usage() {
	cat <<'EOF'
usage: snr-conditions-watch.sh [--loop] [-n SECONDS]

  --loop         refresh in-script instead of exiting, marking changed lines
                 with '*' (use when `watch` is unavailable, e.g. on macOS)
  -n SECONDS     refresh interval for --loop (default 5)
  -h, --help     this message

Set SNR_WATCH_NS to watch a namespace other than openshift-workload-availability,
WAO_WATCH_WAC to name a WorkloadAvailabilityConfig other than `cluster`.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--loop) loop=1 ;;
	-n | --interval)
		if [ $# -lt 2 ]; then
			echo "snr-conditions-watch: $1 needs a value" >&2
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
		echo "snr-conditions-watch: unknown option: $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

if ! command -v oc >/dev/null 2>&1; then
	echo "snr-conditions-watch: 'oc' not found in PATH" >&2
	exit 2
fi

# operand prints the one WorkloadAvailabilityConfig row that matters here. The
# suite enables selfNodeRemediation in BeforeSuite, so an operand that is not
# Available means the run failed before any CR was created.
operand() {
	oc get workloadavailabilityconfig "$WAC" -o jsonpath=\
'{range .status.components[?(@.name=="selfNodeRemediation")]}{.desiredState}{"\t"}{.phase}{"\t"}{.admissionReady}{"\n"}{end}' \
		2>/dev/null |
		awk -F '\t' '
			{
				admit = ($3 == "") ? "?" : $3
				printf "  selfNodeRemediation  desired=%-9s phase=%-18s admissionReady=%s\n", $1, $2, admit
				seen++
			}
			END { if (seen == 0) print "  <no WorkloadAvailabilityConfig: pre-WAO cluster, or api unreachable>" }
		'
}

# manager prints the controller pods with their restart counts. Conditions are
# written by this controller, so a pod that is restarting is the first thing to
# rule out when a condition never appears. custom-columns rather than the
# default output because `oc get pods` renders a recent restart as
# "1 (5m ago)", which shifts every column after it.
manager() {
	oc get pods -n "$NS" --no-headers -o custom-columns=\
'N:.metadata.name,P:.status.phase,RD:.status.containerStatuses[*].ready,RS:.status.containerStatuses[*].restartCount' \
		2>/dev/null |
		awk '
			$1 ~ /^self-node-remediation-controller-manager-/ {
				printf "  %-58s %-10s ready=%-11s restarts=%s\n", $1, $2, $3, $4
				seen++
			}
			END { if (seen == 0) print "  <no controller pod: operand disabled, or api unreachable>" }
		'
}

# crs is the subject of the spec. NODE answers the question the spec asserts
# first -- an SNR CR is named after the node it fences, so "missing" is what
# keeps a conditions test from triggering real fencing.
crs() {
	node_names="$(oc get nodes --no-headers -o custom-columns=N:.metadata.name 2>/dev/null)"
	# Assembled in parts rather than written across continued lines: inside
	# single quotes a trailing backslash is a literal character, not a line
	# continuation, and jsonpath would carry both it and the newline into the
	# output as extra fields and extra records.
	jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations}{"\t"}'
	jsonpath="$jsonpath"'{.status.conditions[?(@.type=="Processing")].status}{"\t"}'
	jsonpath="$jsonpath"'{.status.conditions[?(@.type=="Processing")].reason}{"\t"}'
	jsonpath="$jsonpath"'{.status.conditions[?(@.type=="Succeeded")].status}{"\t"}'
	jsonpath="$jsonpath"'{.status.conditions[?(@.type=="Succeeded")].reason}{"\t"}'
	jsonpath="$jsonpath"'{.metadata.deletionTimestamp}{"\t"}{.metadata.finalizers}{"\n"}{end}'

	oc get selfnoderemediation -n "$NS" -o jsonpath="$jsonpath" 2>/dev/null |
		awk -F '\t' -v nodes="$node_names" '
			BEGIN {
				count = split(nodes, list, "\n")
				for (idx = 1; idx <= count; idx++) {
					name = list[idx]
					gsub(/[ \t\r]/, "", name)
					if (name != "") known[name] = 1
				}
				printf "  %-28s %-8s %-7s %-6s %-6s %-9s %s\n", \
					"NAME", "NODE", "NHC-TO", "PROC", "SUCC", "DELETING", "REASON"
			}
			$1 != "" {
				node   = ($1 in known) ? "EXISTS" : "missing"
				nhc    = ($2 ~ /nhc-timed-out/) ? "yes" : "-"
				proc   = ($3 == "") ? "-" : $3
				succ   = ($5 == "") ? "-" : $5
				# A CR with a deletionTimestamp and finalizers still on it is
				# cleanup blocked, not cleanup finished.
				del    = "-"
				if ($7 != "") del = ($8 == "" || $8 == "[]") ? "yes" : "BLOCKED"
				# Both conditions normally carry the same reason; show one
				# column, and only split it when the controller disagrees.
				reason = ($6 == "") ? "-" : $6
				if ($4 != "" && $4 != $6) reason = $4 "/" reason
				printf "  %-28s %-8s %-7s %-6s %-6s %-9s %s\n", $1, node, nhc, proc, succ, del, reason
				seen++
			}
			END { if (seen == 0) print "  none (created and deleted within seconds -- start this before the run)" }
		'
}

# nodes lists every node with the conditions and the labels and annotations SNR
# acts on. For a conditions spec this is the invariant rather than the subject --
# nothing here may change -- but the same columns are what a destructive SNR run
# needs to read, so they are shown per node and not just counted.
#
# ROLE comes from `oc get nodes` rather than the node-role labels: those carry an
# empty value, and an empty value renders as nothing and silently shifts every
# column after it. The rest is one tab-separated jsonpath, where an absent field
# stays an empty field instead of disappearing.
nodes() {
	roles="$(oc get nodes --no-headers 2>/dev/null | awk '{ print $1 "\t" $3 }')"

	jsonpath='{range .items[*]}{.metadata.name}{"\t"}'
	jsonpath="$jsonpath"'{.status.conditions[?(@.type=="Ready")].status}{"\t"}{.spec.unschedulable}{"\t"}'
	jsonpath="$jsonpath"'{.metadata.annotations.is-reboot-capable\.self-node-remediation\.medik8s\.io}{"\t"}'
	jsonpath="$jsonpath"'{.metadata.annotations.self-node-remediation\.medik8s\.io/watchdog-timeout}{"\t"}'
	jsonpath="$jsonpath"'{.metadata.labels.remediation\.medik8s\.io/exclude-from-remediation}{"\t"}'
	jsonpath="$jsonpath"'{.status.conditions[?(@.type=="MemoryPressure")].status}{"\t"}'
	jsonpath="$jsonpath"'{.status.conditions[?(@.type=="DiskPressure")].status}{"\t"}'
	jsonpath="$jsonpath"'{.status.conditions[?(@.type=="PIDPressure")].status}{"\t"}'
	jsonpath="$jsonpath"'{.status.conditions[?(@.type=="NetworkUnavailable")].status}{"\t"}'
	jsonpath="$jsonpath"'{.spec.taints[*].key}{"\n"}{end}'

	oc get nodes -o jsonpath="$jsonpath" 2>/dev/null |
		awk -F '\t' -v roles="$roles" '
			BEGIN {
				count = split(roles, lines, "\n")
				for (idx = 1; idx <= count; idx++) {
					split(lines[idx], pair, "\t")
					if (pair[1] != "") {
						role = pair[2]
						# "control-plane,master" is one role wearing two
						# labels; the second adds a column of width and
						# nothing else.
						sub(/,master$/, "", role)
						roleOf[pair[1]] = role
					}
				}
				printf "  %-24s %-14s %-6s %-7s %-11s %-4s %-9s %s\n", \
					"NAME", "ROLE", "READY", "CORDON", "REBOOT-CAP", "WD", "EXCLUDED", "NOTES"
			}
			$1 != "" {
				total++
				role   = ($1 in roleOf) ? roleOf[$1] : "?"
				ready  = ($2 == "") ? "?" : $2
				if (ready == "True") readyCount++
				cordon = ($3 == "true") ? "YES" : "-"
				if ($3 == "true") cordonCount++
				# SNR writes is-reboot-capable from the agent. Unset means no
				# agent has run on the node yet; false means SNR will not
				# watchdog-fence it, which silently changes what a
				# remediation spec can prove.
				reboot = ($4 == "") ? "unset" : $4
				if (reboot != "true") notCapable++
				wd     = ($5 == "") ? "-" : $5
				excl   = ($6 == "") ? "-" : "YES"
				if ($6 != "") exclCount++

				notes = ""
				if ($7 == "True") notes = notes "MemoryPressure "
				if ($8 == "True") notes = notes "DiskPressure "
				if ($9 == "True") notes = notes "PIDPressure "
				if ($10 == "True") notes = notes "NetworkUnavailable "
				# Only remediation taints. Every control-plane node carries a
				# node-role taint, and printing it on every line would bury
				# the one taint that means something here.
				taintCount = split($11, taintKeys, " ")
				for (tIdx = 1; tIdx <= taintCount; tIdx++) {
					if (taintKeys[tIdx] ~ /medik8s|out-of-service|unreachable|not-ready/) {
						notes = notes taintKeys[tIdx] " "
						taintFlagged++
					}
				}
				if (notes == "") notes = "-"

				printf "  %-24s %-14s %-6s %-7s %-11s %-4s %-9s %s\n", \
					$1, role, ready, cordon, reboot, wd, excl, notes
			}
			END {
				if (total == 0) {
					print "  <api unreachable>"
				} else {
					printf "  %d nodes, %d Ready, %d cordoned, %d excluded, %d not reboot-capable\n", \
						total, readyCount, cordonCount, exclCount, notCapable
				}
			}
		'
}

snapshot() {
	echo "=== operand ==="
	operand
	manager

	echo
	echo "=== SelfNodeRemediation CRs ==="
	crs

	echo
	echo "=== nodes (must not change) ==="
	nodes
}

if [ "$loop" -eq 0 ]; then
	echo "snr-conditions-watch $(date +%H:%M:%S)"
	snapshot
	exit 0
fi

prev="$(mktemp "${TMPDIR:-/tmp}/snr-cond-watch.XXXXXX")"
cur="$(mktemp "${TMPDIR:-/tmp}/snr-cond-watch.XXXXXX")"
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
	echo "snr-conditions-watch $(date +%H:%M:%S) -- every ${interval}s, '*' marks a change (ctrl-c to stop)"
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
