# RHWA-1234 — NHC creates a SNR remediation CR for an unhealthy worker node

> **Worked example — read this, do not copy it.** Generate your own document with
> `make new-testcase JIRA=RHWA-<id>`, which starts from the empty skeleton in
> [TEMPLATE.md](TEMPLATE.md). This file shows what a *finished* document looks
> like: how much detail each section wants, and what "observable pass criteria"
> means in practice. The test below is hypothetical and is not implemented in
> this repository.
>
> Write your document *before* the Go code. It is the contract the reviewer
> reads: what is verified, on what cluster, and what counts as a pass.

## 1. Metadata

| Field                        | Value                                                   |
| ---------------------------- | ------------------------------------------------------- |
| Jira                         | [RHWA-1234](https://issues.redhat.com/browse/RHWA-1234) |
| `reportxml.ID`               | `1234`                                                  |
| Polarion (legacy) — OPTIONAL | None                                                    |
| Operators                    | NHC v0.9.0, SNR v0.10.0                                 |
| Suite                        | `tests/nhc-operator`                                    |
| Spec file                    | `tests/nhc-operator/tests/nhc_remediation_trigger.go`   |
| Author                       | your-kerberos                                           |
| Status                       | implemented                                             |

> Jira is the system of record for new tests, and RHWA-1234 here is a `Task`
> titled "TEST: NHC creates a SNR remediation CR for an unhealthy worker node"
> with the `rhwa-test` label — the three things CI checks on the ticket.
> `reportxml.ID` is the numeric part
> of the Jira key (`RHWA-1234` → `reportxml.ID("1234")`) and must match the
> `reportxml.ID("…")` argument in the spec. Every `It`/`DescribeTable` needs one.
>
> "Polarion (legacy)" is optional, as it is here — it exists only for tests
> ported from a Polarion plan that already carry an `OCP-xxxxx` ID. In that case
> keep that ID in `reportxml.ID` so historical runs stay comparable, and record
> the Polarion link in this row. Write `None` for anything new; no reviewer will
> ask you to find an ID for a test that never had one.

## 2. Summary

One or two sentences, in plain language, describing the behaviour under test.

> Example: When a worker node stops reporting Ready for longer than the
> NodeHealthCheck `unhealthyConditions` duration, NHC must create exactly one
> SelfNodeRemediation CR named after that node, and must delete the CR once the
> node returns to Ready.

## 3. Scope

- **In scope**: NHC detects the unhealthy condition and creates/deletes the
  remediation CR.
- **Out of scope**: SNR actually rebooting the node (covered by
  `snr-operator` `worker_remediation.go`); escalating remediation
  (`nhc_escalation.go`).

> Say explicitly what this test does *not* prove. Most review comments on new
> test cases are about an unstated scope boundary.

## 4. Environment

| Requirement  | Value                                                           |
| ------------ | --------------------------------------------------------------- |
| Cluster      | OCP ≥ 4.13, multi-node (≥ 2 schedulable workers)                 |
| Topology     | `topology:minimal-worker`                                        |
| Platform     | any (`platform:any`)                                             |
| Storage      | none                                                             |
| Network      | connected or disconnected                                        |
| Namespace    | `openshift-workload-availability` (`medik8sparams.OperatorNs`)   |
| Destructive? | yes — one worker node is made NotReady                           |

Environment variables:

```bash
export KUBECONFIG=/absolute/path/to/kubeconfig
export ECO_TEST_FEATURES=nhc-operator
export ECO_TEST_LABELS='operator:nhc && tier:acceptance'
export ECO_REPORTS_DUMP_DIR="$(mktemp -d)"
```

> List *only* variables this test reads. If the test adds a new variable, name it
> `<OP>_<THING>` (e.g. `SBR_STORAGE_CLASS`), document its default here, and say
> what happens when it is unset — `Skip` with an actionable message is preferred
> over a failure.

## 5. Preconditions

1. NHC and SNR operators installed via OLM and their deployments Ready.
2. At least two schedulable worker nodes; the target node runs no operator
   controller pod.
3. No pre-existing `NodeHealthCheck` or remediation CR from an earlier run.

> Verify preconditions in `BeforeAll` with real assertions, not comments. A stale
> resource from a previous run is the most common source of flakes in this repo:
> delete it, then wait for it to be fully gone before recreating.

## 6. Test steps

| # | Step (`By` text)                                                | Expected result                                                         |
| - | ---------------------------------------------------------------- | ------------------------------------------------------------------------ |
| 1 | Creating a NodeHealthCheck selecting worker nodes                 | CR admitted; `status.observedNodes` equals the worker count              |
| 2 | Making the target node NotReady                                   | Node reports `Ready=False` within `medik8sparams.DefaultTimeout`         |
| 3 | Waiting for NHC to create a remediation CR                        | Exactly one `SelfNodeRemediation` named after the target node exists     |
| 4 | Restoring the target node                                         | Node reports `Ready=True`                                                |
| 5 | Verifying the remediation CR is removed                           | CR is gone (`IsNotFound`), no finalizer left behind                      |

> One row per `By("…")` block in the spec, in order, with the same wording. A
> reviewer should be able to read this table next to the spec and match them
> line for line.

## 7. Pass criteria

- Exactly one spec with `reportxml.ID("1234")` runs and passes.
- Every assertion in the table above holds.
- Cleanup finishes: no `NodeHealthCheck` and no remediation CR remain, and the
  target node is Ready and schedulable.

## 8. Failure / known issues

| Bug       | Effect on this test                                                 | Handling                                       |
| --------- | --------------------------------------------------------------------- | ------------------------------------------------ |
| RHWA-1068 | agent pods crash-loop when `/dev/watchdog` is held                   | guard with `SBR_TEST_RHWA1068=true`, else `Skip` |

> If the test exercises an unfixed bug, gate it behind an env var and `Skip` with
> a message naming the bug and the variable, as `detect_only.go` does. Do not
> merge a spec that is known to fail on a current build without a gate.

## 9. Cleanup

- `DeferCleanup` removes anything created inside the `It`.
- `AfterAll` removes anything created in `BeforeAll`, tolerating `IsNotFound`.
- Recovery steps (uncordon, restart kubelet, flush iptables) run even when the
  spec failed mid-way.

> Cleanup must be idempotent and must never fail the suite on a missing resource;
> log a warning with `GinkgoWriter.Printf` instead.

## 10. Labels

```go
Label(
    labels.OperatorNHC,
    labels.TierAcceptance,
    labels.FrequencyNightly,
    labels.DisruptionDestructive,
    labels.PlatformAny,
    labels.ComponentRemediation,
    labels.TopologyMinimalWorker,
    labels.StabilityCandidate,
)
```

> Always set operator, tier, and component — `ECO_TEST_LABELS` filtering in CI
> depends on them. New default-lane specs also carry `labels.StabilityCandidate`
> until automatically promoted. Add frequency, disruption, platform, and
> topology whenever the test is constrained on that axis; the topology constant
> here matches the `topology:minimal-worker` declared in section 4. One constant
> per axis, taken from `tests/internal/labels` — never a bare string literal.

## 11. Running it standalone

```bash
ginkgo -v --label-filter="operator:nhc && tier:acceptance" \
    --focus="creates a SelfNodeRemediation" ./tests/nhc-operator/...
```

## 12. Implementation sketch

```go
It("RHWA-1234: NHC creates a SelfNodeRemediation CR for an unhealthy worker node",
    reportxml.ID("1234"),
    Label(
        labels.OperatorNHC,
        labels.TierAcceptance,
        labels.FrequencyNightly,
        labels.DisruptionDestructive,
        labels.PlatformAny,
        labels.ComponentRemediation,
        labels.TopologyMinimalWorker,
        labels.StabilityCandidate,
    ), func() {
        DeferCleanup(func() {
            By("DeferCleanup: restoring the target node and removing the NodeHealthCheck")
            restoreNode(targetNodeName)
        })

        By("Making the target node NotReady")
        stopKubelet(targetNodeName)

        By("Waiting for NHC to create a remediation CR for the target node")

        Eventually(func() error {
            return remediationCRExists(targetNodeName)
        }, nhcparams.RemediationCompletionTimeout, nhcparams.DefaultPollInterval).Should(Succeed(),
            "NHC must create a SelfNodeRemediation for unhealthy node %q", targetNodeName)
    })
```

> Conventions the reviewer will check (see [AGENTS.md](../../AGENTS.md)):
> `reportxml.ID` on every spec, `By` for every step, `Eventually`/`Consistently`
> with timeouts from `<op>params`/`medik8sparams` and never `time.Sleep`,
> assertion messages that say what *must* hold, Gomega only in test files, and
> `make fmt && make lint` before pushing.

## 13. Documentation to update

- `tests/<operator>/README.md` — add the test to the suite's numbered catalog
  with its Jira link, operators, cluster/storage requirements, standalone
  command, and pass criteria.
- `docs/README.md` — only when adding a new cross-cutting guide.
