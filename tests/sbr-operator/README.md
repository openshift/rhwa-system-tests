# SBR Operator Post-Deployment Tests

Automated tests validating the Storage-Based Remediation (SBR) operator
deployment, security posture, and high-availability configuration.

The shared FBC upgrade scenario is documented in
[`../../docs/fbc-upgrades.md`](../../docs/fbc-upgrades.md). It preserves and freshly
reconciles the same safe `StorageBasedRemediationConfig` used by the source
upgrade and requires `SBR_STORAGE_CLASS` to identify an RWX storage class.

## Standalone operator-bundle upgrade (OpenShift 5.0)

The `tier:upgrade-operator` scenario discovers and installs a downstream SBR
baseline bundle, upgrades it in place to a supplied candidate, and checks the
new CSV, exact manager image, preserved `StorageBasedRemediationConfig` UID and
full spec, and a fresh controller response — without ever matching a real node
or touching any node's watchdog device. The independently selectable
`tier:fresh-install` scenario starts from the same clean cluster, installs
only the candidate SBR bundle (no baseline, no upgrade step), and verifies the
candidate version, image, and a fresh controller response to the same
non-destructive probe.

### Requirements

- A disposable OpenShift 5.0 cluster (see preflight/cleanup constraints below)
- An RWX-capable `StorageClass` already provisioned on the cluster (see below)
- `operator-sdk` **v1.42.2** available
- Registry credentials that can read
  `registry.redhat.io/workload-availability/storage-based-remediation-operator-bundle`
  (unless pinning a baseline explicitly, see below)
- A candidate SBR bundle/controller image already built and pushed somewhere
  the cluster can pull from (`SBR_UPGRADE_CANDIDATE_SBR_{VERSION,IMAGE,BUNDLE}`)

The cluster must have a real, RWX-capable `StorageClass` available. The test 
reuses the same `SBR_STORAGE_CLASS` env var / CephFS auto-discovery used elsewhere in this
suite (see `discoverRWXStorageClass`). 

The namespace `openshift-workload-availability` must **not exist**. Preflight also rejects
existing SBR CRs/CSV/Subscription/InstallPlan and any leftover OLM cluster
objects associated with that namespace. A rejected preflight makes no changes.
Setup marks a newly created namespace and its `StorageBasedRemediationConfig`
with a random run identifier; cleanup checks UIDs, attempts package cleanup
(including partial failures), removes owned objects and the namespace, and
retains shared CRDs. The second run must pass the same clean preflight.

The probe `StorageBasedRemediationConfig` uses a `nodeSelector` keyed on a
random per-run token, so no current or future node can ever match it: its
agent DaemonSet always schedules zero pods, so no watchdog action is ever
taken. Reconciling shared storage does run one short-lived, unrestricted
`<name>-sbr-device-init` Job on a real node to initialize the dedicated PVC
this scenario creates — this only touches that fresh PVC, never a watchdog
device, and the whole namespace (including the PVC and Job) is removed by
cleanup. To prove that the
*post-upgrade* controller is actively reconciling (not a stale cached one),
the test patches `maxConsecutiveFailures` to a probe value and back, requiring
the agent DaemonSet's `generation`/`observedGeneration` to advance each time
while `desiredNumberScheduled` stays 0.

Cleanup is enabled by default. For debugging only, set
`SBR_UPGRADE_SKIP_CLEANUP=true` to preserve resources after the run. The next
run will reject those leftovers, so remove them manually before rerunning.

```bash
export KUBECONFIG=/absolute/path/to/ocp-5-kubeconfig
export ECO_TEST_FEATURES=sbr-operator
export ECO_TEST_LABELS='tier:upgrade-operator'
export WORKLOAD_IMAGE=unused-by-sbr-operator-upgrade

export SBR_UPGRADE_CANDIDATE_SBR_VERSION=5.8.0
export SBR_UPGRADE_CANDIDATE_SBR_IMAGE='the-candidate-controller-image-pullspec'
export SBR_UPGRADE_CANDIDATE_SBR_BUNDLE='the-candidate-bundle-pullspec'
export SBR_UPGRADE_OPERATOR_SDK="$(command -v operator-sdk)"
# discoverRWXStorageClass only auto-discovers a CephFS-provisioned StorageClass.
# On AWS (EFS), Azure (Files), GCP (Filestore), or plain NFS clusters, set this explicitly.
export SBR_STORAGE_CLASS='the-cluster-RWX-capable-storage-class-name'

export ECO_REPORTS_DUMP_DIR="$(mktemp -d)"
make run-tests
```

By default the test discovers the latest downstream GA baseline bundle from
`registry.redhat.io/workload-availability/storage-based-remediation-operator-bundle`
using Skopeo, resolves it to a digest, and extracts its CSV version and manager
image. Registry credentials must allow that read. To pin a specific baseline
instead, set `SBR_UPGRADE_BASELINE_SBR_{BUNDLE,VERSION,IMAGE}` explicitly.
`SBR_UPGRADE_TEST_REVISION` is optional; when unset, the test records
`git rev-parse HEAD`.

Success means exactly one `tier:upgrade-operator` spec passes and its cleanup
finishes.

### Fresh install (`tier:fresh-install`)

Proves the candidate bundle installs cleanly on its own. It reuses the same preflight, 
`OwnedRun` cleanup, `SafeSpec` probe, and RWX `StorageClass` requirement described above.

```bash
export KUBECONFIG=/absolute/path/to/ocp-5-kubeconfig
export ECO_TEST_FEATURES=sbr-operator
export ECO_TEST_LABELS='tier:fresh-install'
export WORKLOAD_IMAGE=unused-by-sbr-operator-upgrade

export SBR_UPGRADE_CANDIDATE_SBR_VERSION=5.8.0
export SBR_UPGRADE_CANDIDATE_SBR_IMAGE='the-candidate-controller-image-pullspec'
export SBR_UPGRADE_CANDIDATE_SBR_BUNDLE='the-candidate-bundle-pullspec'
export SBR_UPGRADE_OPERATOR_SDK="$(command -v operator-sdk)"
export SBR_STORAGE_CLASS='the-cluster-RWX-capable-storage-class-name'

export ECO_REPORTS_DUMP_DIR="$(mktemp -d)"
make run-tests
```

Success means exactly one `tier:fresh-install` spec passes and its cleanup
finishes. Run this in a separate invocation (new `ECO_REPORTS_DUMP_DIR`) from
`tier:upgrade-operator`; both scenarios use the same fixed namespace and
resource names, so preflight rejects one if the other's cleanup did not finish.

## Prerequisites

- OpenShift cluster with SBR operator installed via OLM
- `KUBECONFIG` set with cluster-admin access
- SBR installed in `openshift-workload-availability` namespace

## Running

```bash
ginkgo --label-filter="sbr" ./tests/sbr-operator/...
```

## Tests

### 1. Verify SBR Operator Pod is Running ([OCP-89232](https://polarion.engineering.redhat.com/polarion/#/project/OSE/workitem?id=OCP-89232))

Validates that SBR controller-manager pods are in Running state and the
pod count matches the cluster topology (2 on multi-node, 1 on SNO).

- **Operators**: SBR v0.3.0
- **Cluster**: Any topology (MNO or SNO)
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="pod is running" ./tests/sbr-operator/...`
- **Pass criteria**: All pods Running, count matches expected replicas

### 2. Verify SBR CSV Has Required Annotations ([OCP-89233](https://polarion.engineering.redhat.com/polarion/#/project/OSE/workitem?id=OCP-89233))

Validates that the active SBR ClusterServiceVersion (in Succeeded phase)
has all required OLM feature annotations: disconnected support, FIPS
compliance flag, suggested namespace, and feature flags.

- **Operators**: SBR v0.3.0
- **Cluster**: Any topology
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="required annotations" ./tests/sbr-operator/...`
- **Pass criteria**: Required annotations present with expected values

### 3. Verify SBR Controller Replicas and Node Distribution ([OCP-89234](https://polarion.engineering.redhat.com/polarion/#/project/OSE/workitem?id=OCP-89234))

Validates that 2 replicas are running and scheduled on different nodes
for high availability. Skipped on SNO clusters where only 1 replica is
expected.

- **Operators**: SBR v0.3.0
- **Cluster**: Multi-node only (skips on SNO)
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="correct number of replicas" ./tests/sbr-operator/...`
- **Pass criteria**: 2 ready replicas on 2 different nodes

### 4. Verify SBR Container Security Context ([OCP-89235](https://polarion.engineering.redhat.com/polarion/#/project/OSE/workitem?id=OCP-89235))

Validates the manager container follows the restricted security posture:
runAsNonRoot at pod level, allowPrivilegeEscalation=false,
capabilities.drop=ALL, and seccompProfile=RuntimeDefault (at container
or pod level). Only checks the `manager` container, not sidecars.

- **Operators**: SBR v0.3.0
- **Cluster**: Any topology
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="non-root user" ./tests/sbr-operator/...`
- **Pass criteria**: All security context fields match restricted profile

### 5. Verify SBR Uses Correct API and OLM Naming ([OCP-88822](https://polarion.engineering.redhat.com/polarion/#/project/OSE/workitem?id=OCP-88822))

Validates that the active SBR CSV display name uses "Storage-Based Remediation"
(not the legacy "SBD" branding) and that all owned CRDs are registered under the
correct API group `storage-based-remediation.medik8s.io`.

- **Operators**: SBR v0.3.0
- **Cluster**: Any topology
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="correct API and OLM naming" ./tests/sbr-operator/...`
- **Pass criteria**: CSV display name contains "Storage-Based Remediation", does not contain "SBD", all CRD API groups match expected value

### 6. Verify StorageBasedRemediationConfig CRD Schema Rejects Invalid Values ([OCP-88881](https://polarion.engineering.redhat.com/polarion/#/project/OSE/workitem?id=OCP-88881))

Validates two layers of StorageBasedRemediationConfig validation:

**Layer 1 (CRD OpenAPI schema)**: The API server rejects StorageBasedRemediationConfig resources with
out-of-range field values for `sbrTimeoutSeconds` and `maxConsecutiveFailures`.

**Layer 2 (Controller validation)**: A StorageBasedRemediationConfig referencing a non-existent
StorageClass is admitted by the API server but the controller does not schedule
a DaemonSet for it.

- **Operators**: SBR v0.3.0
- **Cluster**: Any topology
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="StorageBasedRemediationConfig" ./tests/sbr-operator/...`
- **Pass criteria**: Out-of-range StorageBasedRemediationConfig fields rejected; invalid-StorageClass StorageBasedRemediationConfig admitted but no DaemonSet created

### 7. Verify StorageBasedRemediationConfig Controller Handles Invalid Inputs Without Scheduling Agent Pods ([OCP-88741](https://polarion.engineering.redhat.com/polarion/#/project/OSE/workitem?id=OCP-88741))

Validates that the SBR controller does not schedule agent DaemonSets when
`StorageBasedRemediationConfig` resources specify inputs the controller cannot
act on:

- **Invalid watchdog path**: StorageBasedRemediationConfig with a non-existent watchdog device path
  (`/dev/sbr-test-nonexistent-watchdog`) is admitted by the API server but the
  controller schedules no DaemonSet.
- **Non-matching nodeSelector**: StorageBasedRemediationConfig with a nodeSelector that matches no cluster
  nodes is admitted and may produce a DaemonSet, but `DesiredNumberScheduled`
  must remain 0 for the duration of the observation window.

- **Operators**: SBR v0.3.0
- **Cluster**: Any topology
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="invalid watchdog path and non-matching nodeSelector" ./tests/sbr-operator/...`
- **Pass criteria**: No agent pods scheduled for either invalid StorageBasedRemediationConfig input; StorageBasedRemediationConfig CRs remain present after controller reconciliation

### 8. Verify Watchdog Device Accessibility and Softdog Module Availability ([OCP-88878](https://polarion.engineering.redhat.com/polarion/#/project/OSE/workitem?id=OCP-88878))

Validates that every schedulable cluster node either has accessible hardware watchdog character
devices or has the softdog kernel module available as a fallback.

The test reuses the `/dev/watchdog*` inventory populated by the "SBR Debug — Cluster Watchdog
Inventory" suite when it ran in the same Ginkgo session; otherwise it discovers devices
independently using a short-lived privileged hostPID pod per node.

- **Operators**: SBR v0.3.0
- **Cluster**: Any topology (BM or VM nodes with watchdog hardware or softdog)
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="Verify watchdog device" ./tests/sbr-operator/...`
- **Pass criteria**: All hardware watchdog devices are character devices; nodes without hardware watchdog have softdog.ko present in the kernel module tree

### 9. Verify SBR Must-Gather Collects Diagnostic Data ([OCP-88733](https://polarion.engineering.redhat.com/polarion/#/project/OSE/workitem?id=OCP-88733))

Validates that `oc adm must-gather` with the medik8s must-gather image collects
SBR-related diagnostic data: node manifests, CRD definitions, and
MachineHealthCheck resources.

- **Operators**: SBR v0.3.0
- **Cluster**: Non-HyperShift topologies only (IPI, SNO, etc.). **Skipped on HyperShift** due to architectural limitations: node collection via `oc adm inspect nodes` fails (0 nodes collected), and MachineHealthCheck is only accessible on the management cluster
- **Storage**: None
- **Environment**: Connected by default; the image defaults to the upstream
  `quay.io/medik8s/must-gather:latest` (matching the FAR suite), and its resolved
  digest is logged on every run. Disconnected clusters can set `MUST_GATHER_IMAGE`
  to an accessible mirrored image
- **Standalone**: `ginkgo --label-filter="sbr" --focus="must-gather" ./tests/sbr-operator/...`
- **Pass criteria**: SBR deployment is Ready; must-gather completes successfully; output contains node YAMLs for all cluster nodes, all 3 SBR CRD definition files, and MachineHealthCheck data. On HyperShift, the test skips with an explanatory message

### 10. Verify Controller Availability With One Worker (Controller Resilience)

Validates that the SBR controller maintains at least one available replica when all but one
worker node is cordoned. The SBR deployment uses `topologySpreadConstraints` with
`whenUnsatisfiable: DoNotSchedule`, so only one replica can schedule on a single node.

The test cordons all eligible workers except one, deletes controller pods from cordoned nodes,
and verifies the surviving replica stays available throughout the degraded phase. After
uncordoning, verifies the deployment scales back to the expected replica count on different nodes.

- **Operators**: SBR v0.3.0
- **Cluster**: 3+ schedulable worker-only nodes (standard AWS Prow cluster)
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="controller availability with one worker" ./tests/sbr-operator/...`
- **Pass criteria**: At least 1 controller pod remains Running on the uncordoned node; availability sustained for 30s (Consistently); deployment scales back to 2 replicas on different nodes after uncordoning

### 11. Verify Controller Leadership Handover

Validates that when the active SBR controller pod (the lease holder) is deleted, leadership
transfers to a different controller pod. Follows the same pattern as the FAR controller
lifecycle test (OCP-70636).

- **Operators**: SBR v0.3.0
- **Cluster**: Any topology with at least 2 schedulable worker-only (non-control-plane) nodes
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="controller leadership" ./tests/sbr-operator/...`
- **Pass criteria**: Lease `holderIdentity` changes to a different Running controller pod; deployment returns to full ready replicas
