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
| [`wao-watch.sh`](wao-watch.sh) | Per-operand `desiredState`/`phase`/`admissionReady`/retained count from the `WorkloadAvailabilityConfig`, WAO's own conditions, and the Deployments, DaemonSets and pods in the operator namespace | [`rhwa-2040`](../../docs/TESTCASES/rhwa-2040.md) — suits any spec that enables or disables a WAO operand |
| [`snr-conditions-watch.sh`](snr-conditions-watch.sh) | Every `SelfNodeRemediation` CR with its `Processing`/`Succeeded` status and reason, whether a node of that name exists, and its deletion/finalizer state; plus the SNR operand phase, the controller pods' restart counts, and a node invariant line | SNR processing-condition specs `70584` and `60881` — suits any spec that asserts on SNR conditions rather than on a reboot |

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

## `wao-watch.sh`

```bash
watch -n 5 -d tools/watch/wao-watch.sh   # Linux, or macOS with `brew install watch`
tools/watch/wao-watch.sh --loop          # anywhere, no `watch` needed
tools/watch/wao-watch.sh                 # one snapshot and exit
```

| Block            | Shows                                                                                   |
| ---------------- | ----------------------------------------------------------------------------------------- |
| `operands`       | one row per component of the singleton: `DESIRED`, `PHASE`, `ADMIT` (`admissionReady`), and the number of `retainedResources` |
| `wao conditions` | WAO's own `Available`, `Progressing` and `Degraded`                                     |
| `workloads`      | Deployments and DaemonSets in the operator namespace with ready/desired counts          |
| `pods`           | name, ready, status, age and node for everything in the namespace                       |

`WAO_WATCH_NS` overrides the namespace, `WAO_WATCH_WAC` the configuration name (default
`cluster`). Requires only `oc` and `awk`.

Enabling an operand moves it `Disabled` → `Progressing` → `Available`; disabling it removes the
manager Deployment and moves it back. Three things are worth knowing before reading a run as a
failure:

1. **`nodeHealthCheck` lags the others.** It has two Deployments, the manager and the console
   plugin, and reports `Available` only when both are ready.
2. **`Progressing` with every Deployment ready and `ADMIT=false`** is the operand waiting for
   the service CA controller to inject a webhook CA bundle — `snr`, `nmo` and `nhc` all gate on
   this. It is not a crash loop.
3. **`DisableIncomplete` is a normal end state**, not an error: agents, a config or native CRs
   outlived the manager, and `RETAINED` counts them. `selfNodeRemediation` ends here whenever
   it has been enabled, because its default `SelfNodeRemediationConfig` and agent DaemonSet
   survive the disable.

What must *not* change is `workload-availability-operator-controller-manager`, which stays
`1/1` throughout. If WAO's own manager restarts mid-run, the operand phases are reporting a WAO
recovery rather than operand health.

## `snr-conditions-watch.sh`

```bash
watch -n 5 -d tools/watch/snr-conditions-watch.sh   # Linux, or macOS with `brew install watch`
tools/watch/snr-conditions-watch.sh --loop          # anywhere, no `watch` needed
tools/watch/snr-conditions-watch.sh                 # one snapshot and exit
```

For the SNR processing-condition specs — "Verify SNR conditions with non-existent node
name" (`70584`) and "Verify SNR conditions with nhc-timed-out annotation" (`60881`) — and
for any other spec that asserts on `SelfNodeRemediation` status rather than on a reboot.
Use [`snr-watch.sh`](snr-watch.sh) instead when the spec actually remediates a node: boot
IDs and taints are what a destructive run turns on, and they are not in this one.

| Block | Shows |
| ----- | ----- |
| `operand` | the `selfNodeRemediation` row of the `WorkloadAvailabilityConfig` (`desiredState`, `phase`, `admissionReady`) and the controller pods with their restart counts |
| `SelfNodeRemediation CRs` | per CR: `NODE` (does a node of that name exist), `NHC-TO` (the `nhc-timed-out` annotation), `PROC`/`SUCC` condition status, `DELETING`, and the reason |
| `nodes (must not change)` | one row per node: `ROLE`, `READY`, `CORDON`, `REBOOT-CAP`, `WD`, `EXCLUDED` and a `NOTES` column, with a count line underneath |

**Start it before the run.** Both specs create a CR, assert on it, and delete it in
`DeferCleanup` within a few seconds; a snapshot taken afterwards shows an empty table.

Reading the CR table:

1. **`NODE` must stay `missing`** for spec `70584`. An SNR CR is named after the node it
   fences, and the spec asserts no such node exists precisely so that a conditions test
   cannot trigger real fencing. `EXISTS` there means the run is about to remediate a live
   node — stop it.
2. **`PROC` never passes through `True`.** A remediation that is skipped or timed out goes
   straight to `Processing=False`, so waiting to see `True` first is waiting for something
   that will not happen. The discriminator is `REASON`:
   `RemediationSkippedNodeNotFound` for `70584`, `RemediationTimeoutByNHC` for `60881`.
3. **`REASON` splits as `proc/succ`** only when the two conditions disagree. One value
   means both carry it, which is the normal case.
4. **`DELETING=BLOCKED`** is a CR with a deletion timestamp and finalizers still attached —
   cleanup blocked, not cleanup finished. SNR's own controller removes that finalizer, so
   a CR that sits there means the controller stopped watching: most often the operand was
   disabled while the CR was still alive. Re-enable the operand and it drains.

`restarts` climbing on a controller pod explains a condition that never appears, and it is
worth ruling out before reading a timeout as a product bug.

The node block carries the conditions and the labels and annotations SNR itself acts on,
one row per node. For a conditions spec none of it may change, but these are the same
columns a destructive SNR run needs, so they are listed rather than merely counted:

| Column | Source | Why it is here |
| ------ | ------ | -------------- |
| `ROLE` | `oc get nodes` | Control-plane remediation carries an etcd-quorum constraint that worker remediation does not. `,master` is trimmed — it is one role wearing two labels. |
| `READY` | the `Ready` condition | The only node condition SNR and NHC act on by default. A remediated node goes to **`Unknown`**, not `False`. |
| `CORDON` | `.spec.unschedulable` | Left cordoned by an interrupted run; blocks scheduling for everything after it. |
| `REBOOT-CAP` | `is-reboot-capable.self-node-remediation.medik8s.io` | Written by the SNR agent. `false` means SNR will not watchdog-fence the node, and `unset` means no agent has run there yet — either way a remediation spec silently proves less than it looks like it does. |
| `WD` | `self-node-remediation.medik8s.io/watchdog-timeout` | The agent's watchdog timeout, in seconds. |
| `EXCLUDED` | `remediation.medik8s.io/exclude-from-remediation` | `YES` makes NHC skip the node permanently. A leftover from an earlier run is a common reason a remediation never fires. |
| `NOTES` | conditions and taints | Only anomalies: `MemoryPressure`/`DiskPressure`/`PIDPressure`/`NetworkUnavailable` when `True`, and taints matching `medik8s`, `out-of-service`, `unreachable` or `not-ready`. The node-role taint every control-plane node carries is deliberately not shown — printing it on every line would bury the one taint that means something. |

`ROLE` is read from `oc get nodes` rather than from the node-role labels because those
carry an empty value, and an empty value renders as nothing and silently shifts every
column after it. The rest is one tab-separated jsonpath, where an absent field stays an
empty field instead of disappearing — worth copying if you add a column.

`SNR_WATCH_NS` overrides the namespace, `WAO_WATCH_WAC` the configuration name. Requires
only `oc` and `awk`.

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
