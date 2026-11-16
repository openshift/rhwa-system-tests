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

Tests here cover WAO's own behaviour — installing and removing operands, and the admission
webhooks of FAR, NHC, NMO and SNR, which the WAO manager serves in place of the operands. What
each operand does once it is running belongs to that operator's suite.

## Prerequisites

- OpenShift cluster with WAO installed via OLM in `openshift-workload-availability`
- `KUBECONFIG` set with cluster-admin access
- **For the operand lifecycle spec (RHWA-2040): every operand disabled, and no medik8s workload
  running in the namespace.** That spec fails rather than skips when something is already
  installed; see the cleanup note below. The webhook specs accept every operand state: they
  record the state of the operand they use and restore it.

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

Watch the cluster while a spec runs with [`tools/watch/wao-watch.sh`](../../tools/watch/wao-watch.sh),
or with [`tools/watch/webhook-watch.sh`](../../tools/watch/webhook-watch.sh) for a webhook spec.

### Webhook specs only

`make run-webhook-tests` selects specs by the label `component:webhook`. It never selects a
destructive spec unless `WEBHOOK_DESTRUCTIVE=true` is set.

```bash
export KUBECONFIG=/path/to/kubeconfig
export WORKLOAD_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest
make run-webhook-tests                      # FAR, NHC, NMO and SNR suites, and this suite
make run-webhook-tests WEBHOOK_TARGET=wao   # this suite only
make run-webhook-tests WEBHOOK_TARGET=snr   # the SNR suite, and the SNR spec of this suite
```

`WEBHOOK_TARGET` is `all`, `wao`, `far`, `nhc`, `nmo` or `snr`. The plan behind these specs, and
the coverage of each admission rule, is in
[`docs/wao-webhooks-test-plan.md`](../../docs/wao-webhooks-test-plan.md).

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

### 2. WAO Registers the Ten Admission Entries (RHWA-9001, placeholder number)

Reads every webhook configuration and checks that the WAO Service serves exactly the ten
expected entries, each with its rule, `failurePolicy`, timeout and CA bundle, and that no other
Service intercepts a resource that WAO serves. Full specification:
[`docs/TESTCASES/rhwa-9001.md`](../../docs/TESTCASES/rhwa-9001.md).

- **Operators**: WAO v0.1.0+ with central admission (classic OLM)
- **Cluster**: Any topology
- **Standalone**: `ginkgo --label-filter="component:webhook && operator:wao" --focus="RHWA-9001" ./tests/wao-operator/...`
- **Pass criteria**: Ten entries on the WAO Service, nine with `Fail` and
  `vselfnoderemediation.kb.io` with `Ignore`; ready endpoints equal the WAO replicas;
  `admissionReady` true for FAR, NHC, NMO and SNR

### 3–6. WAO Serves the Admission of Each Operand (RHWA-9002 to RHWA-9005, placeholder numbers)

One spec for each operand whose admission WAO serves. Each sends server-side dry-run requests
while the operand is `Disabled`, enables the operand, sends the requests again, and restores the
operand state. No remediation request is stored and no node enters maintenance.

| Spec | Operand | Document | Also checks |
| ---- | ------- | -------- | ----------- |
| RHWA-9002 | FAR | [`rhwa-9002.md`](../../docs/TESTCASES/rhwa-9002.md) | Agent inventory, Secret rules, template update rules |
| RHWA-9003 | NHC | [`rhwa-9003.md`](../../docs/TESTCASES/rhwa-9003.md) | Duplicate template kind, update and delete rules with a test status |
| RHWA-9004 | NMO | [`rhwa-9004.md`](../../docs/TESTCASES/rhwa-9004.md) | Second maintenance and node name update, with an object stored while NMO is `Disabled` |
| RHWA-9005 | SNR | [`rhwa-9005.md`](../../docs/TESTCASES/rhwa-9005.md) | Warnings, update and delete of the default config |

- **Operators**: WAO v0.1.0+ with central admission
- **Cluster**: One worker and one control plane node for RHWA-9004; any topology for the others
- **Standalone**: `ginkgo --label-filter="component:webhook && operator:wao" --focus="RHWA-9004" ./tests/wao-operator/...`
- **Pass criteria**: Each request gets the documented answer in the two operand states; the
  operand state and the cluster objects are those of the start

### 7. Each Entry Obeys Its failurePolicy (RHWA-9006, placeholder number)

Isolates the WAO admission listener with a `NetworkPolicy` for approximately 2 minutes. A valid
request must fail for the six kinds with `Fail`, and a `SelfNodeRemediation` must be admitted.
Full specification: [`docs/TESTCASES/rhwa-9006.md`](../../docs/TESTCASES/rhwa-9006.md).

- **Cluster**: A network plugin that applies a NetworkPolicy to traffic from the API server
  (OVN-Kubernetes does)
- **Standalone**: `ginkgo --label-filter="component:webhook && operator:wao" --focus="RHWA-9006" ./tests/wao-operator/...`
- **Pass criteria**: Failed webhook calls for the `Fail` kinds, an admitted `SelfNodeRemediation`,
  WAO Pods `Ready` throughout, admission back after the policy is gone

### 8. Admission Returns After a WAO Restart (RHWA-9007, placeholder number)

Deletes every WAO manager Pod and checks that the new Pods serve admission for the seven kinds
and collected the FAR agent inventory again. Full specification:
[`docs/TESTCASES/rhwa-9007.md`](../../docs/TESTCASES/rhwa-9007.md).

- **Standalone**: `ginkgo --label-filter="component:webhook && operator:wao" --focus="RHWA-9007" ./tests/wao-operator/...`
- **Pass criteria**: New Pods `Ready` and `admissionReady` true within 5 minutes; the seven
  requests admitted; `fence_incorrect` rejected

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
