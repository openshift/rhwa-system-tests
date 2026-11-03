# WAO Operator Tests

Automated tests for the Workload Availability Operator (WAO), the operator that consolidates
the medik8s operators into one installation. WAO installs the others as *operands*, switched on
and off through the `spec.components` block of a cluster-scoped `WorkloadAvailabilityConfig`
singleton named `cluster`:

| Operand key                  | Operator                      |
| ---------------------------- | ----------------------------- |
| `nodeHealthCheck`            | Node Health Check             |
| `selfNodeRemediation`        | Self Node Remediation         |
| `storageBasedRemediation`    | Storage Based Remediation     |
| `nodeMaintenance`            | Node Maintenance              |
| `fenceAgentsRemediation`     | Fence Agents Remediation      |
| `machineDeletionRemediation` | Machine Deletion Remediation  |

Tests here cover WAO's own behaviour — installing and removing operands. What each operand does
once it is running belongs to that operator's suite.

## Prerequisites

- OpenShift cluster with WAO installed via OLM in `openshift-workload-availability`
- `KUBECONFIG` set with cluster-admin access
- **Every operand disabled, and no medik8s workload running in the namespace.** The suite
  fails rather than skips when something is already installed; see the cleanup note below.

## Running

```bash
ginkgo --label-filter="operator:wao" ./tests/wao-operator/...
```

Or via the test runner:

```bash
export KUBECONFIG=/path/to/kubeconfig
export ECO_TEST_FEATURES="wao-operator"
make run-tests
```

Watch the cluster while a spec runs with [`tools/watch/wao-watch.sh`](../../tools/watch/wao-watch.sh).

## Tests

### 1. WAO Installs and Removes Each Operand on Demand ([RHWA-2040](https://redhat.atlassian.net/browse/RHWA-2040))

Enables each of the six operands in turn, waits for `status.components[].phase` to reach
`Available` and the operand's manager Deployments to become ready, then disables it and
confirms the manager Deployments are removed before moving to the next operand. Full
specification: [`docs/TESTCASES/rhwa-2040.md`](../../docs/TESTCASES/rhwa-2040.md).

- **Operators**: WAO v0.1.0+
- **Cluster**: Any topology (MNO or SNO)
- **Environment**: Connected (each operand pulls its manager image on first enable)
- **Standalone**: `ginkgo --label-filter="operator:wao" --focus="enables each operand" ./tests/wao-operator/...`
- **Pass criteria**: Each operand reaches phase `Available` with its Deployments ready within 5
  minutes, then reaches `Disabled` or `DisableIncomplete` with those Deployments gone within 3
  minutes; all six operands disabled again at the end

## Re-running on the same cluster

Disabling an operand stops its manager but does not remove what the operand itself created, and
WAO reports that as phase `DisableIncomplete` with a `retainedResources` list — a normal
outcome, not a failure. After a passing run, `selfNodeRemediation` is the operand that ends
there: starting it creates a default `SelfNodeRemediationConfig`, which owns the agent
DaemonSet, and disabling the component removes neither, so an agent pod stays on every node.
(`nodeHealthCheck` reports retained resources too, but only on a cluster that already has
`NodeHealthCheck` CRs — it creates none of its own.)

That fails the next run's precondition, which requires every operand at exactly `Disabled`.
Inspect and clear before re-running:

```bash
oc get workloadavailabilityconfig cluster \
    -o jsonpath='{range .status.components[*]}{.name}{" "}{.phase}{" "}{.retainedResources}{"\n"}{end}'
oc delete selfnoderemediationconfig --all -n openshift-workload-availability
```

Deleting the config is safe while SNR is disabled: it carries no finalizer, so it does not need
the stopped manager to release it, and the DaemonSet goes with it by garbage collection.
