# tools/watch

Read-only snapshot scripts for watching a cluster while a spec runs.

Specs here are routinely run by hand, one at a time, and the state that matters lives on
the cluster rather than in the Ginkgo log — which reports a step only once it has
finished. Watching live shows an operator rolling out, a CR moving through its conditions,
pods churning through an upgrade, or a node rebooting, and makes a run that is going wrong
obvious minutes before Ginkgo says so. Destructive tests benefit most, because their
evidence often heals before the failure is printed, but these are useful for any spec.

Nothing here changes cluster state, and nothing here is imported by a test suite. They are
for the person running the test, not for the test itself.

## Catalog

| Script | Watches | Written for |
| ------ | ------- | ----------- |
| [`snr-watch.sh`](snr-watch.sh) | Nodes (`Ready`, cordon, boot ID, taint keys), `SelfNodeRemediation` and `NodeHealthCheck` CRs, the live `minPeersForRemediation`, and the agent pods with their ages | [`rhwa-1972`](../../docs/TESTCASES/rhwa-1972.md) — suits any destructive SNR spec |
| [`sbr-watch.sh`](sbr-watch.sh) | Full Node UIDs and boot IDs, storage conditions, cordons, exact taint values, SBR request conditions and cleanup times, and namespace Pods | [`rhwa-1839`](../../docs/TESTCASES/rhwa-1839.md) — draft BlockPerNode cases |

## `snr-watch.sh`

```bash
watch -n 5 -d tools/watch/snr-watch.sh   # Linux, or macOS with `brew install watch`
tools/watch/snr-watch.sh --loop          # anywhere, no `watch` needed
tools/watch/snr-watch.sh                 # one snapshot and exit
```

`-d` is what makes `watch` useful: it highlights whatever changed since the previous tick,
so a boot ID flipping or a taint appearing stands out instead of having to be spotted by
eye. macOS ships no `watch`, so `--loop` does the same job in-script, marking every line
that was not in the previous tick with a leading `*`. Either way the marking only survives
one tick, so for a precise record of *when* something changed, log the diffs instead of
eyeballing them.

| Block | Shows |
| ----- | ----- |
| `nodes` | name, `Ready`, cordon, first 8 chars of `status.nodeInfo.bootID`, taint keys |
| `remediation CRs` | `SelfNodeRemediation` and `NodeHealthCheck` across all namespaces |
| `snrc` | live `spec.minPeersForRemediation` from the default `SelfNodeRemediationConfig` |
| `agent pods` | name, ready, status, age and node for everything in the operator namespace |

Record the boot IDs of the nodes under test **before** starting a run. That is what most
remediation specs actually assert on, and it is the one value that tells a real reboot
apart from a node that merely went briefly unreachable.

A rebooting node appears in three stages, and the last is the slowest:

1. `Ready` becomes **`Unknown`** (not `False`) roughly 40s after the kubelet stops
   heartbeating — the node-lifecycle controller marking a stale status, which is right for
   a node that vanished rather than one reporting itself unhealthy.
2. The taint manager adds `unreachable` and `not-ready`, each twice: once `NoSchedule` and
   once `NoExecute`. The script prints keys only, so the repetition is expected.
3. `bootID` changes — only once the kubelet re-registers, so this marks the node coming
   *back*, not going down.

Requires only `oc` logged in and `awk`; it runs on Fedora Linux and macOS alike. Each tick
is four small reads; raise `-n` to reduce API traffic. `SNR_WATCH_NS` overrides the
namespace.

## `sbr-watch.sh`

```bash
tools/watch/sbr-watch.sh
tools/watch/sbr-watch.sh --loop -n 5
```

The script uses `oc` and POSIX `awk`. Its shell syntax supports Bash 3.2.
Set `SBR_WATCH_NS` to select another namespace. The default is `openshift-workload-availability`.

| Block | Shows |
|---|---|
| Nodes | Name, full UID, Ready, cordon, full API boot ID, `SBRStorageUnhealthy`, and taint key/value/effect |
| SBR requests | Namespace/name, full UID, target, all conditions, cleanup phase, start, and deadline |
| Agent and reporter Pods | All namespace Pods with full UIDs, Nodes, phases, container readiness, and restart counts |

Record the initial boot IDs before the case. A no-reboot interval must preserve the relevant host boot IDs.
A requested reboot changes the target boot ID. An agent replacement changes its Pod UID.
The API boot ID can remain stale while kubelet cannot report. The display cannot replace the test's direct host observations.

The script marks changed lines with `*`. Missing fields appear as `<unset>`, and failed API reads have explicit messages.
The display provides diagnostics. It cannot establish that a transient CR or taint never existed between reads.

## Adding one

Per [`docs/TESTCASES/AGENTS.md`](../../docs/TESTCASES/AGENTS.md), a new test either reuses
a script here or adds its own, and documents the invocation in section 11 of its test case
document. Keep to the same shape:

- **Read-only.** No patches, no deletes, no `oc debug`. Someone will run this against a
  cluster mid-test; it must not perturb what is being measured.
- **Runs on Fedora Linux and on a MacBook.** No `python3` (macOS has none without the
  Xcode command line tools) — `oc -o custom-columns` plus `awk` covers most needs. Stay
  within POSIX `awk` (BSD awk on macOS, gawk on Fedora), target bash 3.2, and avoid
  GNU-only flags on `sed`, `date` and `head`. macOS has no `watch`, so offer a
  self-refreshing `--loop` as well. Test on both: the failure mode is silently wrong
  columns, not an error.
- **Do not let the display churn.** Anything that changes every tick — a pod age in
  seconds, a timestamp inside the diffed body — marks its line as changed on every
  refresh and buries the changes that matter. `snr-watch.sh` trims `6m21s` to `6m` for
  exactly this reason.
- **Show the spec's own subject.** A remediation test wants nodes and CRs; an upgrade test
  wants CSVs, subscriptions and pod ages. Watch what that spec asserts on.
- **One screen per tick, stable line order.** `watch -d` diffs the rendered text, so sort
  output and keep column widths fixed — otherwise every tick looks like it changed.
- **Degrade quietly.** Print a placeholder when the API is unreachable rather than erroring
  out. During a connectivity test, an unreachable API is a result, not a crash.
- **Say what must not change.** The gotchas above (`Unknown` not `False`, doubled taints,
  boot ID arriving last) cost real debugging time to learn. Write yours down.
- **List it in the catalog** above, and document its invocation in section 11 of the test
  case document.
