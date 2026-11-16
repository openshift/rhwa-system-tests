#!/usr/bin/env bash
#
# Snapshot of the admission webhooks the Workload Availability Operator serves:
# the webhook entries that call a Service in the operator namespace, the ready
# endpoints of the WAO admission Service, the per-operand admissionReady rows of
# the WorkloadAvailabilityConfig, the objects the webhook specs store, and the
# pods in the operator namespace.
#
#   watch -n 5 -d tools/watch/webhook-watch.sh   # Linux, or macOS with `brew install watch`
#   tools/watch/webhook-watch.sh --loop          # anywhere: self-refreshing, marks changes with *
#   tools/watch/webhook-watch.sh                 # one snapshot and exit
#
# What to expect while a webhook spec runs:
#
#   1. The entries block does not change. WAO registers ten entries, and they
#      stay while an operand is enabled or disabled. A new entry for a resource
#      that WAO serves means an operand registered a webhook of its own.
#   2. An operand spec moves its operand Disabled -> Available -> back. ADMIT
#      stays true in each phase: admission does not depend on the operand.
#   3. Stored test objects appear and go: a NodeHealthCheck, a NodeMaintenance,
#      templates and one Secret, each with a name that starts with
#      wao-webhook-test (the Secret has the legacy FAR name
#      fence-agents-credentials-shared). The NodeMaintenance exists only while
#      nodeMaintenance is Disabled.
#   4. The failure policy spec adds the NetworkPolicy
#      wao-webhook-test-deny-admission for approximately 2 minutes. The WAO pods
#      stay 1/1 and the endpoints stay ready during that time.
#   5. The restart spec replaces every WAO manager pod. The endpoints block
#      empties and fills again.
#
# What must NOT change: no node becomes unschedulable, and no remediation
# request (FenceAgentsRemediation, SelfNodeRemediation) is stored.
#
# Portability: POSIX shell constructs, bash 3.2 (macOS system bash) and bash 5
# alike. Needs only `oc` and `awk`. Set WAO_WATCH_NS to override the namespace.
#
# Each tick is seven small reads. Raise the interval to cut API traffic.

set -u

NS="${WAO_WATCH_NS:-openshift-workload-availability}"
WAC_NAME="${WAO_WATCH_WAC:-cluster}"
SERVICE="${WAO_WATCH_SERVICE:-workload-availability-operator-controller-manager-service}"
interval=5
loop=0

usage() {
	cat <<'EOF'
usage: webhook-watch.sh [--loop] [-n SECONDS]

  --loop         refresh in-script instead of exiting, marking changed lines
                 with '*' (use when `watch` is unavailable, e.g. on macOS)
  -n SECONDS     refresh interval for --loop (default 5)
  -h, --help     this message

Set WAO_WATCH_NS to watch a namespace other than openshift-workload-availability,
WAO_WATCH_WAC to read a WorkloadAvailabilityConfig other than 'cluster', and
WAO_WATCH_SERVICE to read a Service other than the WAO admission Service.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--loop) loop=1 ;;
	-n | --interval)
		if [ $# -lt 2 ]; then
			echo "webhook-watch: $1 needs a value" >&2
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
		echo "webhook-watch: unknown option: $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

if ! command -v oc >/dev/null 2>&1; then
	echo "webhook-watch: 'oc' not found in PATH" >&2
	exit 2
fi


# entries lists each webhook entry that calls a Service in the operator
# namespace. The CA bundle is not printed: it is long, and its presence is what
# the registration spec asserts.
entries() {
	{
		oc get validatingwebhookconfigurations -o jsonpath=\
'{range .items[*]}{range .webhooks[*]}{"V"}{"\t"}{.name}{"\t"}{.failurePolicy}{"\t"}{.timeoutSeconds}{"\t"}{.clientConfig.service.namespace}{"\t"}{.clientConfig.service.name}{"\n"}{end}{end}' \
			2>/dev/null
		oc get mutatingwebhookconfigurations -o jsonpath=\
'{range .items[*]}{range .webhooks[*]}{"M"}{"\t"}{.name}{"\t"}{.failurePolicy}{"\t"}{.timeoutSeconds}{"\t"}{.clientConfig.service.namespace}{"\t"}{.clientConfig.service.name}{"\n"}{end}{end}' \
			2>/dev/null
	} |
		awk -F'\t' -v ns="$NS" '
			BEGIN { printf "  %-2s %-40s %-7s %-4s %s\n", "T", "ENTRY", "POLICY", "TO", "SERVICE" }
			$5 == ns {
				printf "  %-2s %-40s %-7s %-4s %s\n", $1, $2, $3, $4, $6
				seen++
			}
			END { if (seen == 0) print "  <no entry calls a Service in " ns ", or api unreachable>" }
		' | sort -k2
}

# endpoints lists the endpoints of the WAO admission Service with their
# readiness. The API server calls one of the ready ones for each request.
endpoints() {
	oc get endpointslices -n "$NS" -l "kubernetes.io/service-name=$SERVICE" -o jsonpath=\
'{range .items[*]}{range .endpoints[*]}{.targetRef.name}{"\t"}{.addresses[0]}{"\t"}{.conditions.ready}{"\n"}{end}{end}' \
		2>/dev/null |
		awk -F'\t' '
			BEGIN { printf "  %-66s %-16s %s\n", "POD", "ADDRESS", "READY" }
			$1 != "" {
				printf "  %-66s %-16s %s\n", $1, $2, $3
				seen++
			}
			END { if (seen == 0) print "  <no endpoint, or api unreachable>" }
		' | sort
}

# stored lists the objects a webhook spec can store, by kind and name.
stored() {
	{
		oc get networkpolicies,fenceagentsremediationtemplates,selfnoderemediationtemplates,selfnoderemediationconfigs,fenceagentsremediations,selfnoderemediations \
			-n "$NS" -o name 2>/dev/null
		oc get secret fence-agents-credentials-shared -n "$NS" -o name 2>/dev/null
		oc get nodehealthchecks,nodemaintenances -o name 2>/dev/null
	} |
		awk '
			{ print "  " $0; seen++ }
			END { if (seen == 0) print "  <none>" }
		' | sort
}

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
	echo "=== webhook entries that call a Service in $NS ==="
	entries

	echo
	echo "=== endpoints of $SERVICE ==="
	endpoints

	echo
	echo "=== operands ($WAC_NAME) ==="
	operands

	echo
	echo "=== stored objects of the admission kinds ==="
	stored

	echo
	echo "=== pods ($NS) ==="
	pods
}

if [ "$loop" -eq 0 ]; then
	echo "webhook-watch $(date +%H:%M:%S)"
	snapshot
	exit 0
fi

prev="$(mktemp "${TMPDIR:-/tmp}/webhook-watch.XXXXXX")"
cur="$(mktemp "${TMPDIR:-/tmp}/webhook-watch.XXXXXX")"
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
	echo "webhook-watch $(date +%H:%M:%S) -- every ${interval}s, '*' marks a change (ctrl-c to stop)"
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
