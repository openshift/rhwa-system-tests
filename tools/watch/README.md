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
| [`nhc-watch.sh`](nhc-watch.sh) | Nodes (`Ready`, cordon, boot ID, taint keys), each `NodeHealthCheck` CR's `phase`/`healthyNodes`/`observedNodes`/in-flight count/`reason`, the remediation CRs NHC spawns (SNR, FAR, MDR, `TestRemediation`), the controller leader `Lease` holder, and the controller pods with their ages | any NHC spec — remediation-trigger and escalation runs, negative-validation and status-field specs, controller failover |

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

## `nhc-watch.sh`

```bash
watch -n 5 -d tools/watch/nhc-watch.sh   # Linux, or macOS with `brew install watch`
tools/watch/nhc-watch.sh --loop          # anywhere, no `watch` needed
tools/watch/nhc-watch.sh                 # one snapshot and exit
```

| Block                 | Shows                                                                                                           |
| --------------------- | --------------------------------------------------------------------------------------------------------------- |
| `nodes`               | name, `Ready`, cordon, first 8 chars of `status.nodeInfo.bootID`, taint keys                                     |
| `NodeHealthCheck CRs` | one row per NHC: `phase`, `healthyNodes`, `observedNodes`, in-flight remediation count, and `reason`             |
| `remediation CRs`     | the CRs NHC spawns — `SelfNodeRemediation`, `FenceAgentsRemediation`, `MachineDeletionRemediation`, `TestRemediation` — across all namespaces, each Kind probed separately |
| `controller lease`    | `spec.holderIdentity` of the NHC leader election `Lease` (`e1f13584.medik8s.io`)                                 |
| `controller pods`     | name, ready, status, age and node for everything in the operator namespace                                       |

`NHC_WATCH_NS` overrides the namespace, `NHC_WATCH_LEASE` the controller lease name (default
`e1f13584.medik8s.io`). Requires only `oc` and `awk`.

A remediation reads as a sequence, and the slow part is the reboot at the end:

1. A node's `Ready` goes **`False` or `Unknown`**, and NHC holds it for the `unhealthyConditions`
   duration (default 30s) before acting — so the node block changes up to half a minute before the
   NHC CR does.
2. The NHC CR moves **`Enabled` → `Remediating`**, `healthyNodes` drops, the in-flight count rises,
   and `reason` changes from "no ongoing remediation" to a remediating message.
3. A **remediation CR named after the node** appears in the `remediation CRs` block. Which Kind
   depends on the NHC's `remediationTemplate` — the live specs use `SelfNodeRemediation` for real
   reboots and the dummy `TestRemediation` for logic-only coverage.
4. The remediator taints and reboots the node; `bootID` changes only once the kubelet re-registers,
   so it marks the node coming *back*.
5. The remediation CR is deleted and the NHC CR returns to **`Enabled`** with `healthyNodes` restored.

Two things are worth knowing before reading a run as a failure:

- **`Disabled` is not `Remediating`.** NHC reports `Disabled` when its `remediationTemplate` is missing
  or the remediator is installed but its controller is not running. That is the subject of the
  negative-validation specs; on a remediation run it means the remediator operand is down, so check
  the `controller pods` block.
- **The `remediation CRs` block probes each Kind separately on purpose.** A Kind whose CRD is not
  installed — an operand that is not enabled, or a dummy test CRD already torn down — is skipped
  silently rather than blanking the Kinds that do exist. `none` means no remediation CRs of any Kind,
  not that a query failed.

The controller runs two replicas; expect two `node-healthcheck` manager pods in the `controller pods`
block, alongside the console-plugin pod and whichever remediator operand is enabled. A `controller
lease` holder changing with no failover spec driving it means the controller restarted.

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
