#!/usr/bin/env bash
# Read-only SBR snapshot. Supports Fedora and macOS system bash (3.2).
# Record node boot IDs before a fencing test: the new boot ID appears on return.
# Agent DaemonSets are created by StorageBasedRemediationConfig, not by enabling
# the WAO operand alone. PVC/init-job failures can therefore explain absent agents.
set -u

NS="${SBR_WATCH_NS:-openshift-workload-availability}"
LEASE_NAME="${SBR_WATCH_LEASE:-sbr-operator-leader-election}"
interval=5
loop=0

usage() {
	cat <<'EOF'
usage: sbr-watch.sh [--loop] [-n SECONDS]

  --loop         refresh and mark changed lines with '*' (no watch required)
  -n SECONDS     refresh interval for --loop (positive integer, default 5)
  -h, --help     show this message

Requires oc and awk. Uses the current KUBECONFIG/context; never changes the cluster.
SBR_WATCH_NS overrides openshift-workload-availability.
SBR_WATCH_LEASE overrides sbr-operator-leader-election.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--loop) loop=1 ;;
	-n | --interval)
		if [ $# -lt 2 ]; then
			echo "sbr-watch: $1 needs a value" >&2
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
		echo "sbr-watch: unknown option: $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

case "$interval" in
'' | *[!0-9]* | 0*)
	echo 'sbr-watch: interval must be a positive integer without leading zeros' >&2
	exit 2
	;;
esac

for dependency in oc awk; do
	if ! command -v "$dependency" >/dev/null 2>&1; then
		echo "sbr-watch: '$dependency' not found in PATH" >&2
		exit 2
	fi
done

oc_get() {
	oc get --request-timeout=10s "$@"
}

operands() {
	oc_get workloadavailabilityconfigs.workloadavailability.medik8s.io cluster -o jsonpath=\
'{range .status.components[*]}{.name}{"\t"}{.desiredState}{"\t"}{.phase}{"\n"}{end}' 2>/dev/null | sort |
		awk -F'\t' '
			BEGIN { printf "  %-25s %-10s %s\n", "OPERAND", "DESIRED", "PHASE" }
			$1 == "storageBasedRemediation" || $1 == "nodeHealthCheck" || $1 == "selfNodeRemediation" {
				printf "  %-25s %-10s %s\n", $1, $2, $3; seen++
			}
			END { if (!seen) print "  <no WAO status, or API unavailable>" }
		'
}

nodes() {
	oc_get nodes --no-headers -o custom-columns=\
'N:.metadata.name,R:.status.conditions[?(@.type=="Ready")].status,S:.status.conditions[?(@.type=="SBRStorageUnhealthy")].status,C:.spec.unschedulable,B:.status.nodeInfo.bootID,T:.spec.taints[*].key' 2>/dev/null |
		awk '
			BEGIN { printf "  %-44s %-8s %-10s %-7s %-9s %s\n", "NODE", "READY", "UNHEALTHY", "CORDON", "BOOTID", "TAINTS" }
			{
				cordon = ($4 == "true") ? "YES" : "-"
				boot = ($5 == "<none>" || $5 == "") ? "?" : substr($5, 1, 8)
				taints = $6
				gsub(/node-role\.kubernetes\.io\/|node\.kubernetes\.io\/|remediation\.medik8s\.io\//, "", taints)
				printf "  %-44s %-8s %-10s %-7s %-9s %s\n", $1, $2, $3, cordon, boot, taints
				seen++
			}
			END { if (!seen) print "  <no nodes, or API unavailable>" }
		'
}

configs() {
	oc_get storagebasedremediationconfigs.storage-based-remediation.medik8s.io -n "$NS" -o jsonpath=\
'{range .items[*]}{.metadata.name}{"\t"}{.spec.sharedStorageClass}{"\t"}{.spec.sharedStorageVolumeMode}{"\t"}{.spec.detectOnlyMode}{"\t"}{.spec.watchdogPath}{"\t"}{range .status.conditions[*]}{.type}{"="}{.status}{"("}{.reason}{") "}{end}{"\n"}{end}' 2>/dev/null |
		awk -F'\t' '
			BEGIN { printf "  %-30s %-24s %-10s %-9s %-16s %s\n", "CONFIG", "STORAGECLASS", "MODE", "DETECT", "WATCHDOG", "CONDITIONS" }
			$1 != "" {
				for (idx = 2; idx <= 6; idx++) if ($idx == "") $idx = "-"
				printf "  %-30s %-24s %-10s %-9s %-16s %s\n", $1, $2, $3, $4, $5, $6; seen++
			}
			END { if (!seen) print "  <no SBRCs, or API unavailable>" }
		'
}

remediations() {
	oc_get storagebasedremediations.storage-based-remediation.medik8s.io -n "$NS" -o jsonpath=\
'{range .items[*]}{.metadata.name}{"\t"}{range .status.conditions[*]}{.type}{"="}{.status}{"("}{.reason}{") "}{end}{"\n"}{end}' 2>/dev/null |
		awk -F'\t' '
			BEGIN { printf "  %-44s %s\n", "TARGET NODE", "CONDITIONS" }
			$1 != "" { printf "  %-44s %s\n", $1, ($2 == "" ? "-" : $2); seen++ }
			END { if (!seen) print "  <no SBR CRs, or API unavailable>" }
		'
}

nhcs() {
	oc_get nodehealthchecks.remediation.medik8s.io -o jsonpath=\
'{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{.status.healthyNodes}{"\t"}{.status.observedNodes}{"\t"}{.status.reason}{"\n"}{end}' 2>/dev/null |
		awk -F'\t' '
			BEGIN { printf "  %-30s %-12s %-7s %-8s %s\n", "NHC", "PHASE", "HEALTHY", "OBSERVED", "REASON" }
			$1 != "" {
				for (idx = 2; idx <= 5; idx++) if ($idx == "") $idx = "-"
				printf "  %-30s %-12s %-7s %-8s %s\n", $1, $2, $3, $4, $5; seen++
			}
			END { if (!seen) print "  <no NHCs, or API unavailable>" }
		'
}

storage() {
	oc_get pvc -n "$NS" --no-headers -o custom-columns=\
'NAME:.metadata.name,PHASE:.status.phase,CLASS:.spec.storageClassName,MODE:.spec.volumeMode,VOLUME:.spec.volumeName' 2>/dev/null |
		awk 'BEGIN { print "  PVC                                  PHASE     STORAGECLASS                  MODE        VOLUME" }
			{ print "  " $0; seen++ } END { if (!seen) print "  <no PVCs, or API unavailable>" }'
	oc_get jobs -n "$NS" --no-headers -o custom-columns=\
'NAME:.metadata.name,ACTIVE:.status.active,SUCCEEDED:.status.succeeded,FAILED:.status.failed' 2>/dev/null |
		awk 'BEGIN { print "  JOB                                  ACTIVE    SUCCEEDED    FAILED" }
			{ print "  " $0; seen++ } END { if (!seen) print "  <no jobs, or API unavailable>" }'
}

workloads() {
	oc_get deployments,daemonsets -n "$NS" -o jsonpath=\
'{range .items[*]}{.kind}{"/"}{.metadata.name}{"\t"}{.spec.replicas}{.status.desiredNumberScheduled}{"\t"}{.status.readyReplicas}{.status.numberReady}{"\n"}{end}' 2>/dev/null |
		awk -F'\t' '
			BEGIN { printf "  %-64s %-8s %s\n", "WORKLOAD", "DESIRED", "READY" }
			$1 != "" { printf "  %-64s %-8s %s\n", $1, ($2 == "" ? "0" : $2), ($3 == "" ? "0" : $3); seen++ }
			END { if (!seen) print "  <no workloads, or API unavailable>" }
		'
	holder="$(oc_get lease "$LEASE_NAME" -n "$NS" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null)"
	echo "  controller lease: ${holder:-<absent, or API unavailable>}"
	oc_get pods -n "$NS" --no-headers -o wide 2>/dev/null |
		awk '
			BEGIN { printf "  %-58s %-5s %-18s %-6s %s\n", "POD", "READY", "STATUS", "AGE", "NODE" }
			NF >= 7 {
				age = $(NF - 4); sub(/m[0-9]+s$/, "m", age)
				printf "  %-58s %-5s %-18s %-6s %s\n", $1, $2, $3, age, $(NF - 2); seen++
			}
			END { if (!seen) print "  <no pods, or API unavailable>" }
		'
}

snapshot() {
	echo '=== WAO operands ==='
	operands
	echo
	echo '=== nodes (SBRStorageUnhealthy) ==='
	nodes
	echo
	echo '=== StorageBasedRemediationConfig ==='
	configs
	echo
	echo '=== StorageBasedRemediation ==='
	remediations
	echo
	echo '=== NodeHealthCheck ==='
	nhcs
	echo
	echo "=== storage ($NS) ==="
	storage
	echo
	echo "=== workloads ($NS) ==="
	workloads
}

if [ "$loop" -eq 0 ]; then
	echo "sbr-watch $(date +%H:%M:%S)"
	snapshot
	exit 0
fi

prev="$(mktemp "${TMPDIR:-/tmp}/sbr-watch.XXXXXX")" || exit 1
cur="$(mktemp "${TMPDIR:-/tmp}/sbr-watch.XXXXXX")" || { rm -f "$prev"; exit 1; }
trap 'rm -f "$prev" "$cur"' EXIT
trap 'exit 130' INT TERM

while :; do
	snapshot >"$cur"
	printf '\033[H\033[2J'
	echo "sbr-watch $(date +%H:%M:%S) -- every ${interval}s, '*' marks a change (ctrl-c to stop)"
	awk -v prev="$prev" '
		FILENAME == prev { was[$0]++; next }
		{ print (($0 in was) ? "  " : "* ") $0 }
	' "$prev" "$cur"
	cat "$cur" >"$prev"
	sleep "$interval"
done
