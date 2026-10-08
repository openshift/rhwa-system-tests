# SBR Operator Post-Deployment Tests

Automated tests validating the Storage-Based Remediation (SBR) operator
deployment, security posture, and high-availability configuration.

## Operator upgrade through FBC (OpenShift 5.0)

The `tier:upgrade-operator` scenario uses the same shared FBC upgrade runner as
NHC. It installs GA SBR from `redhat-operators`, creates a safe no-storage config,
switches the existing Subscription to the candidate FBC, and verifies the exact
candidate version/image, preserved config UID/full spec, and a fresh validation
response from the candidate controller. No ODF, StorageClass, PVC, or agent pods
are needed by default. This proves upgrade/configuration compatibility, **not**
functional node remediation. See [the shared FBC guide](../../docs/fbc-upgrades.md)
for the complete contract and optional real-remediation mode.

The namespace `openshift-workload-availability` must not already exist. Preflight
also rejects existing SBR resources/installations and leftover OLM cluster objects
associated with that namespace. A rejected preflight makes no changes. Cleanup
uses run markers and UIDs to remove only owned resources; shared CRDs and the
applied IDMS remain. For debugging, `SBR_FBC_SKIP_CLEANUP=true` preserves resources.

```bash
export KUBECONFIG=/absolute/path/to/ocp-5-kubeconfig
export ECO_TEST_FEATURES=sbr-operator
export ECO_TEST_LABELS='tier:upgrade-operator'
export ECO_TEST_VERBOSE=true
export WORKLOAD_IMAGE=unused-by-sbr-fbc-upgrade

# Use actual, immutable pullspecs for the catalog and its SBR controller.
export SBR_FBC_CATALOG_IMAGE='registry.example/rhwa-fbc@sha256:...'
export SBR_FBC_CANDIDATE_VERSION=5.8.0
export SBR_FBC_CANDIDATE_IMAGE='registry.example/sbr-controller@sha256:...'
# Set only if the catalog depends on these registry mirrors:
export SBR_FBC_IDMS_PATH=/absolute/path/to/idms.yaml
export SBR_FBC_REMEDIATION=false

export ECO_REPORTS_DUMP_DIR="$(mktemp -d)"
make run-tests
```

Registry credentials must already let the cluster pull the catalog, bundles,
controller, and any operands. Source-built and supplied downstream FBCs use
the same test and inputs. Exactly one upgrade spec must pass. Its reports record
the actual baseline/candidate CSVs, config before/after, and unique validation probe.

To additionally require a real post-upgrade reboot and recovery, set
`SBR_FBC_REMEDIATION=true` and `SBR_STORAGE_CLASS` to an existing RWX class.
This is destructive and requires at least two healthy worker agents and a target
worker without an SBR controller. The test does not deploy storage infrastructure.

## Fresh install (`tier:fresh-install`)

The separate fresh-install test remains an SDK bundle installation, not an FBC
upgrade. It requires `operator-sdk` v1.42.2, a candidate bundle/controller image,
and an existing RWX StorageClass (CephFS auto-discovery or `SBR_STORAGE_CLASS`).
Its safe selector schedules zero agent pods, but storage initialization can create
a PVC and a device-init Job. It uses `SBR_UPGRADE_*` inputs, including
`SBR_UPGRADE_SKIP_CLEANUP` for debugging, independently of the FBC upgrade inputs.

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

On functional storage/remediation or controller-resilience test failure,
diagnostics print the involved StorageBasedRemediation CR status and conditions
before cleanup, followed by the last 100 log lines from the SBR controller's
current leader-election lease holder. Controller-only scenarios collect logs
without a remediation CR. Collection is best-effort; each CR read and the
controller log collection have their own 15-second timeout, and warnings
preserve the original test failure.

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
