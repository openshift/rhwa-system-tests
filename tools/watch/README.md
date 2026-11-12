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
| [`collection-watch.sh`](collection-watch.sh) | Every medik8s CRD with its live instance count, the custom resources themselves, the transient `openshift-must-gather-*` namespace and pod, the collection archives published so far, node `Ready`/cordon, and any operator pod not `Running` | [`rhwa-1994`](../../docs/TESTCASES/rhwa-1994.md) — suits any must-gather spec |

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

## `collection-watch.sh`

Linux, or macOS with `brew install watch`:

```bash
watch -n 5 -d tools/watch/collection-watch.sh
```

Anywhere, no `watch` needed:

```bash
tools/watch/collection-watch.sh --loop
```

One snapshot and exit:

```bash
tools/watch/collection-watch.sh
```

| Block | Shows |
| ----- | ----- |
| `crd instances` | every CRD whose name contains `medik8s`, with its live instance count |
| `medik8s custom resources` | namespace, kind and name of each of those instances |
| `must-gather` | the transient `openshift-must-gather-*` namespace and its pod, with status and age |
| `archives` | the collection tarballs published so far, with size and full path |
| `nodes` | name, `Ready` and cordon |
| `operator pods not Running` | anything in the operator namespace that is not `Running` |

The instance counts are the point. The collection test turns each one into a
required file, so the count is the expectation being formed in real time. The
value that must *not* change is a count dropping while the must-gather pod is
`Running`: a CR that disappears mid-gather becomes a missing artifact and the
comparison fails with no sign of why. A node returning `Ready` during the
destructive specs is expected and harmless.

`crd instances` runs one `oc get` per CRD, a dozen reads per tick. That is the
most expensive script here, and it runs alongside a collection that is itself
API-heavy — raise `-n` rather than lowering it.

The must-gather pod ends `Completed`, not `Running`; its namespace is deleted
shortly after, and the specs sweep up any that survive. A namespace still
present several minutes after a run finished is a leak worth reporting, not a
gather still in progress.

`COLLECTION_WATCH_NS` overrides the operator namespace.

The `archives` block looks in `COLLECTION_WATCH_ARCHIVE_DIR`, else
`ARTIFACT_DIR`, else `ECO_REPORTS_DUMP_DIR`, else
`/tmp/rhwa-collection-artifacts` — the same chain the specs use to decide
where to publish. Watching usually happens from a second terminal without the
run's environment, so that shared default is what makes the block work with
no setup at all. One archive appears per spec as its
collection finishes — three by the end of a full run — and the full path is
printed so it can be copied straight onto a support case.

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
