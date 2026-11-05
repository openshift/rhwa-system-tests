#!/usr/bin/env bash
#
# Snapshot of what the RHWA collection test is about to collect: every medik8s
# CRD with its live instance count, the custom resources themselves, the
# must-gather namespace and pod, the collection archives published so far, and
# node Ready/cordon state.
#
#   watch -n 5 -d tools/watch/collection-watch.sh   # Linux, or macOS with `brew install watch`
#   tools/watch/collection-watch.sh --loop          # anywhere: self-refreshing, marks changes with *
#   tools/watch/collection-watch.sh                 # one snapshot and exit
#
# `watch -d` highlights whatever changed since the last tick. macOS has no
# `watch`, so --loop does the same job in-script, marking every line that was
# not in the previous tick with a leading '*'.
#
# What to expect during a run:
#
#   1. The `crd instances` block fills in as the fixtures are created. Each
#      count is what the collection is then required to produce a file for, so
#      a count that drops to 0 before the gather finishes is the one thing that
#      turns an expectation into a missing artifact.
#   2. An `openshift-must-gather-xxxxx` namespace appears with a single pod,
#      and lives for the few minutes the collection runs. The pod reaching
#      Completed, not Running, is what finishes the gather.
#   3. An `rhwa-<operator acronyms>-must-gather-<timestamp>.tar.gz` appears
#      under `archives` as each spec finishes its collection -- three by the
#      end of a full run, their acronyms naming the operators whose custom
#      resources each one holds. The full path is printed so it can be copied
#      straight onto a support case.
#   4. On the destructive specs a worker goes cordoned (NodeMaintenance) or
#      NotReady and back (SelfNodeRemediation). The remediation CR must still
#      exist while the must-gather pod runs -- that overlap is the whole point
#      of those two specs, and it is visible here and nowhere in the Ginkgo log.
#
# The counts must not drop while the must-gather pod is Running. A node
# returning Ready is expected; the CR disappearing early is not.
#
# Portability: POSIX shell constructs, bash 3.2 (macOS system bash) and bash 5
# alike. Needs only `oc` and `awk`. Set COLLECTION_WATCH_NS to override the
# operator namespace.
#
# Each tick is four reads, one of which lists all medik8s CRs. Raise the
# interval rather than lowering it: the collection itself is API-heavy.

set -u

NS="${COLLECTION_WATCH_NS:-openshift-workload-availability}"

# Where the specs publish their collection archives, resolved the same way
# mustgather.ArchiveDestination does: ARTIFACT_DIR first, then
# ECO_REPORTS_DUMP_DIR, then the shared default. The default is what makes
# this work with no setup at all -- the run and the watch are normally two
# separate terminals, and the watching one rarely has the test's environment.
# COLLECTION_WATCH_ARCHIVE_DIR overrides the lot.
#
# The default path is duplicated from mustgather.DefaultArchiveDir in
# tests/internal/mustgather/archive.go; keep the two in step.
ARCHIVE_DIR="${COLLECTION_WATCH_ARCHIVE_DIR:-${ARTIFACT_DIR:-${ECO_REPORTS_DUMP_DIR:-/tmp/rhwa-collection-artifacts}}}"

interval=5
loop=0

usage() {
	cat <<'EOF'
usage: collection-watch.sh [--loop] [-n SECONDS]

  --loop         refresh in-script instead of exiting, marking changed lines
                 with '*' (use when `watch` is unavailable, e.g. on macOS)
  -n SECONDS     refresh interval for --loop (default 5)
  -h, --help     this message

Set COLLECTION_WATCH_NS to watch a namespace other than openshift-workload-availability.

Collection archives are looked for in COLLECTION_WATCH_ARCHIVE_DIR, else
ARTIFACT_DIR, else ECO_REPORTS_DUMP_DIR, else /tmp/rhwa-collection-artifacts --
the same default the specs publish to, so no setup is needed for a local run.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--loop) loop=1 ;;
	-n | --interval)
		if [ $# -lt 2 ]; then
			echo "collection-watch: $1 needs a value" >&2
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
		echo "collection-watch: unknown option: $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

if ! command -v oc >/dev/null 2>&1; then
	echo "collection-watch: 'oc' not found in PATH" >&2
	exit 2
fi

# medik8s_crds lists the CRDs the collection script selects, by the same rule it
# uses: a name containing "medik8s". Printed sorted so the block holds a stable
# line order across ticks.
medik8s_crds() {
	oc get crds -o custom-columns=N:.metadata.name --no-headers 2>/dev/null |
		awk '/medik8s/ { print }' | sort
}

# crd_instances prints one line per CRD with its live instance count. The count
# is what the test turns into required files, so this is the block to watch.
crd_instances() {
	crds="$(medik8s_crds)"
	if [ -z "$crds" ]; then
		echo "  <no medik8s CRDs, or api unreachable>"

		return
	fi

	for crd in $crds; do
		# `oc get <crd> -A` prints nothing on stdout when there are none, so a
		# plain line count is the instance count.
		count="$(oc get "$crd" -A --no-headers 2>/dev/null | awk 'NF { n++ } END { print n + 0 }')"
		# Width fits the longest name an RHWA install has
		# (machinedeletionremediationtemplates...), so the count column stays
		# put instead of shifting per row.
		printf '  %-76s %s\n' "$crd" "$count"
	done
}

# instances lists the custom resources themselves, so a fixture can be told
# apart from a resource that was already on the cluster.
instances() {
	crds="$(medik8s_crds | tr '\n' ',' | sed 's/,$//')"
	if [ -z "$crds" ]; then
		echo "  <api unreachable>"

		return
	fi

	oc get "$crds" -A --no-headers -o custom-columns=\
'NS:.metadata.namespace,K:.kind,N:.metadata.name' 2>/dev/null |
		awk '
			NF {
				ns = ($1 == "<none>" || $1 == "") ? "-" : $1
				printf "  %-32s %-34s %s\n", ns, $2, $3
				seen++
			}
			END { if (seen == 0) print "  none" }
		' | sort
}

# gather shows the transient namespace and pod `oc adm must-gather` creates.
gather() {
	oc get ns --no-headers -o custom-columns=N:.metadata.name 2>/dev/null |
		awk '/^openshift-must-gather-/ { print "  ns " $1; seen++ }
		     END { if (seen == 0) print "  no must-gather namespace" }'
	oc get pods -A --no-headers -o wide 2>/dev/null |
		awk '
			$1 ~ /^openshift-must-gather-/ {
				age = $(NF - 4)
				# "6m21s" -> "6m": a per-second age would mark this line
				# changed on every tick and bury the real changes.
				sub(/m[0-9]+s$/, "m", age)
				printf "  pod %-42s %-5s %-12s %-6s %s\n", $2, $3, $4, age, $(NF - 2)
			}
		'
}

# archives lists the collection tarballs published so far, with their full
# paths, so the files to hand to a support case can be copied straight off the
# screen. Each spec writes one when its collection finishes, so this block
# fills in as a run progresses and is the quickest confirmation that a spec
# got as far as producing output.
archives() {
	echo "  in $ARCHIVE_DIR"

	if [ ! -d "$ARCHIVE_DIR" ]; then
		echo "  <directory does not exist yet>"

		return
	fi

	# Captured rather than piped straight into the loop so an empty result can
	# be reported; a `while read` in a pipeline runs in a subshell, where a
	# flag set inside would not survive. -maxdepth is understood by GNU and
	# BSD find alike, and the archives are written flat.
	list="$(find "$ARCHIVE_DIR" -maxdepth 1 -type f -name '*.tar.gz' 2>/dev/null | sort)"
	if [ -z "$list" ]; then
		echo "  none yet"

		return
	fi

	# Sorted by name, which the UTC timestamp in it makes chronological, so
	# the line order is stable between ticks.
	printf '%s\n' "$list" | while IFS= read -r archive; do
		size="$(du -h "$archive" 2>/dev/null | awk '{print $1}')"
		printf '  %-8s %s\n' "${size:-?}" "$archive"
	done
}

# nodes prints one line per node, from custom-columns rather than JSON so no
# python3 is needed -- macOS does not ship one.
nodes() {
	oc get nodes --no-headers -o custom-columns=\
'N:.metadata.name,R:.status.conditions[?(@.type=="Ready")].status,C:.spec.unschedulable' \
		2>/dev/null |
		awk '
			BEGIN { printf "  %-14s %-8s %s\n", "NAME", "READY", "CORDON" }
			{
				cordon = ($3 == "true") ? "YES" : "-"
				printf "  %-14s %-8s %s\n", $1, $2, cordon
				seen++
			}
			END { if (seen == 0) print "  <api unreachable>" }
		'
}

snapshot() {
	echo "=== crd instances (what the collection must produce a file for) ==="
	crd_instances

	echo
	echo "=== medik8s custom resources ==="
	instances

	echo
	echo "=== must-gather ==="
	gather

	echo
	echo "=== archives ==="
	archives

	echo
	echo "=== nodes ==="
	nodes

	echo
	echo "=== operator pods not Running ==="
	oc get pods -n "$NS" --no-headers 2>/dev/null |
		awk '$3 != "Running" && NF { printf "  %-58s %-5s %s\n", $1, $2, $3; seen++ }
		     END { if (seen == 0) print "  all Running" }'
}

if [ "$loop" -eq 0 ]; then
	echo "collection-watch $(date +%H:%M:%S)"
	snapshot
	exit 0
fi

prev="$(mktemp "${TMPDIR:-/tmp}/collection-watch.XXXXXX")"
cur="$(mktemp "${TMPDIR:-/tmp}/collection-watch.XXXXXX")"
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
	echo "collection-watch $(date +%H:%M:%S) -- every ${interval}s, '*' marks a change (ctrl-c to stop)"
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
