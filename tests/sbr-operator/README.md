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
- **Pass criteria**: All hardware watchdog devices are character devices; nodes without hardware watchdog have softdog.ko present in the kernel module tree. A node that rebooted after the inventory retries the device check for 2 minutes; a softdog-backed device still missing afterwards is a post-reboot skip (a reboot clears the softdog module, and only a restarted SBR agent loads it again), not a failure. A hardware watchdog's device must survive reboots, so it keeps failing the check

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

### 12. Verify Fencing State Persists on Ceph RBD Block Storage Across Node Reboot ([RHWA-1916](https://redhat.atlassian.net/browse/RHWA-1916))

Validates that a node fenced against a Ceph RBD block device stays fenced exactly once. The test
creates an SBRC with `sharedStorageVolumeMode: Block`, triggers fencing with a manually created
`StorageBasedRemediation` CR, and follows the node through reboot and recovery, checking that the
operator does not create a second remediation CR for it afterwards.

Full specification: [`../../docs/TESTCASES/rhwa-1916.md`](../../docs/TESTCASES/rhwa-1916.md).

- **Operators**: SBR CSV ≥ 5.8.0 (`sharedStorageVolumeMode`, [RHWA-1313](https://redhat.atlassian.net/browse/RHWA-1313)); older operators Skip with the reason
- **Cluster**: At least one schedulable worker node that hosts no SBR controller pod and may be rebooted
- **Storage**: Ceph RBD block StorageClass (provisioner containing `rbd.csi.ceph.com`), auto-discovered or set with `SBR_RBD_STORAGE_CLASS`
- **Environment**: Connected or disconnected. **Destructive** — the target node is fenced and hard-rebooted
- **Standalone**: `ginkgo --label-filter="sbr" --focus="fencing state persists on Ceph RBD" ./tests/sbr-operator/...`
- **Pass criteria**: `FencingSucceeded=True` on the SBR CR; node boot-id changes and the node returns Ready; node becomes schedulable again after the CR is deleted; agent pod returns to Running/ContainersReady; no new `StorageBasedRemediation` CR exists for the node

### 13. Verify SBR Agents Write Concurrently to a Ceph RBD Block Device ([RHWA-1915](https://issues.redhat.com/browse/RHWA-1915))

Validates SBR in Block volume mode: with `sharedStorageVolumeMode: Block` on a Ceph RBD
StorageClass, agents on every worker node attach the same raw RWX device and write their
heartbeats into it concurrently.

Each agent pod reads the heartbeat region straight off the device with `dd` and O_DIRECT, so
the page cache is bypassed and writes made by agents on other nodes are actually observed.

Full test case: [docs/TESTCASES/rhwa-1915.md](../../docs/TESTCASES/rhwa-1915.md)

- **Operators**: SBR v5.8.0
- **Cluster**: Multi-node, at least 2 schedulable workers
- **Storage**: ODF Ceph RBD StorageClass, RWX, `volumeMode: Block`. Auto-discovered by
  provisioner (`rbd.csi.ceph.com`); `SBR_RBD_STORAGE_CLASS` overrides it. Skips when the
  cluster has none
- **Environment**: Connected or disconnected. Self Node Remediation must not be running —
  SNR and SBR both claim `/dev/watchdog`, and the test fails early with scale-down instructions
- **Standalone**: `ginkgo --label-filter="sbr" --focus="write concurrently to Ceph RBD block device" ./tests/sbr-operator/...`
- **Pass criteria**: One Ready agent pod per worker node on the same Block device; every written slot parses with a valid CRC32; heartbeats present from at least 2 distinct node IDs with advancing sequence numbers; slot counts agree across pods

### 14. Verify SBR Recovers From a Transient Ceph RBD Network Partition Without Fencing (RHWA-1924)

Validates the self-healing half of Block-mode storage detection. A privileged disruptor pod blocks
the victim worker's Ceph ports (3300, 6789, 6800-7300) for 150s and then removes its own rules, so
the outage is long enough for a peer to report `SBRStorageUnhealthy=True` (reason `HeartbeatTimeout`)
but short enough that the condition clears as `Recovered` instead of ageing into `Unknown`.

The victim's heartbeat slot is identified as the one that stops advancing during the outage — read
through an agent pod on another node, which keeps working while the victim is cut off — and must
resume once Ceph traffic returns. The node must come back on its own: same boot-id, still Ready, no
`StorageBasedRemediation` CR, and an unchanged `sbr_self_fenced_total`.

Skips when a `NodeHealthCheck` remediates through the SBR template, which would legitimately fence
the node, or when iptables is unusable in the victim's host namespaces.

Full test case: [docs/TESTCASES/rhwa-1924.md](../../docs/TESTCASES/rhwa-1924.md)

- **Operators**: SBR v5.8.0
- **Cluster**: Multi-node, at least 2 schedulable workers, one hosting no SBR controller pod
- **Storage**: ODF Ceph RBD StorageClass, RWX, `volumeMode: Block`. Auto-discovered by
  provisioner (`rbd.csi.ceph.com`); `SBR_RBD_STORAGE_CLASS` overrides it. Skips when the
  cluster has none
- **Environment**: Connected or disconnected; `SBR_INJECTOR_IMAGE` must be mirrored when
  disconnected. **Destructive** — Ceph traffic is blocked on one worker for 150s. Self Node
  Remediation must not be running
- **Standalone**: `ginkgo --label-filter="sbr" --focus="transient Ceph RBD network partition" ./tests/sbr-operator/...`
- **Pass criteria**: `SBRStorageUnhealthy=True` (reason `HeartbeatTimeout`) during the outage and
  `False` (reason `Recovered`) after it; exactly one heartbeat slot falls behind and then advances
  past the sequence it stalled at; for 90s after recovery the node keeps its boot-id, stays Ready and has no
  `StorageBasedRemediation` CR; the agent's `sbr_self_fenced_total` and restart count are unchanged

### 15. Verify SBR Heartbeats Remain Stable on Ceph RBD Under Load (RHWA-1925)

Measures the false-positive margin of the Block-mode heartbeat path. The SBRC is created with
`sbrTimeoutSeconds: 10` — the CRD minimum, halving to a 5s heartbeat interval — and a stress pod
then saturates every CPU on every node running an agent.

For three minutes under that load no node may report `SBRStorageUnhealthy=True`, every heartbeat
slot must keep advancing when read through an observer agent, and nothing may be remediated: no
`StorageBasedRemediation` CR, no boot-id change, and no movement in any agent's
`sbr_self_fenced_total` or `sbr_device_io_errors_total`.

The upstream proposal's `p99 < 500ms` latency assertion is deliberately absent — the agent exposes
no latency metric — and is replaced by the outcomes above. Stress pods stop their own load after
five minutes, so a run that dies cannot leave a node saturated.

Full test case: [docs/TESTCASES/rhwa-1925.md](../../docs/TESTCASES/rhwa-1925.md)

- **Operators**: SBR v5.8.0
- **Cluster**: Multi-node; at least two schedulable workers to exercise peer monitoring
- **Storage**: ODF Ceph RBD StorageClass, RWX, `volumeMode: Block`. Auto-discovered by
  provisioner (`rbd.csi.ceph.com`); `SBR_RBD_STORAGE_CLASS` overrides it. Skips when the
  cluster has none
- **Environment**: Connected or disconnected; `SBR_INJECTOR_IMAGE` must be mirrored when
  disconnected. Non-destructive, but every agent node runs at full CPU for up to five minutes, so
  it belongs in a job that tolerates that. Self Node Remediation must not be running
- **Standalone**: `ginkgo --label-filter="sbr" --focus="heartbeats remain stable on Ceph RBD under load" ./tests/sbr-operator/...`
- **Pass criteria**: No node reports `SBRStorageUnhealthy=True` for the whole 3-minute window under
  load; every heartbeat slot advances past its pre-load sequence; no `StorageBasedRemediation` CR
  exists; every node keeps its boot-id; every agent's `sbr_self_fenced_total` and
  `sbr_device_io_errors_total` are unchanged

### 16. Verify SBR Device Inspection Works on Ceph RBD Block Storage (RHWA-1926)

Reads every region of a live Block-mode device — superblock, node map, heartbeat slots, fence
slots — through an agent pod and checks the four agree with each other and with the agents that
are running: valid superblock with a clear quiesce flag, one node map entry per agent node with
unique in-range node IDs, each node's heartbeat in the slot its node ID points at and recently
written, and no live fence message. An agent on a second node then repeats the reads and must see
the same device, with sequence numbers that have only moved forward.

SBR has no `sbr-device-summary` command, so the regions are read raw with `dd` and O_DIRECT and
decoded against the operator's own layout constants. The decoded state is attached to the Ginkgo
report, which is what makes this test useful for diagnosing a production device.

Read-only: the test creates an SBRC and otherwise writes nothing.

Full test case: [docs/TESTCASES/rhwa-1926.md](../../docs/TESTCASES/rhwa-1926.md)

- **Operators**: SBR v5.8.0
- **Cluster**: At least two nodes running an SBR agent; skips when only one does
- **Storage**: ODF Ceph RBD StorageClass, RWX, `volumeMode: Block`. Auto-discovered by
  provisioner (`rbd.csi.ceph.com`); `SBR_RBD_STORAGE_CLASS` overrides it. Skips when the
  cluster has none
- **Environment**: Connected or disconnected. Non-destructive. Self Node Remediation must not be
  running
- **Standalone**: `ginkgo --label-filter="sbr" --focus="device inspection works on Ceph RBD" ./tests/sbr-operator/...`
- **Pass criteria**: Superblock magic `SBRBLK`, version 1, matching CRC32 and clear quiesce flag;
  one node map entry per agent node with unique node IDs in 1-255 and both CRC32s valid; every
  mapped node's heartbeat at slot `nodeID - 1`, no more than two heartbeat intervals behind the
  newest on the device; no fence slot with a reason other than `NONE`; a reader on a second node
  sees the identical node map and sequence numbers no lower; the decoded state is attached to the
  report

### 17. Verify Node Hang Remediation on Ceph RBD Block Storage ([RHWA-1931](https://issues.redhat.com/browse/RHWA-1931))

Exercises the full NHC-driven remediation path end to end in Block mode. A privileged injector pod
triggers a SysRq kernel panic on a victim worker; its peers notice the heartbeat slot stop and set
`SBRStorageUnhealthy=True` (reason `HeartbeatTimeout`); NHC creates a `StorageBasedRemediation` CR;
a peer agent fences the node; and the reconciler proves death from storage, applies the
`node.kubernetes.io/out-of-service` taint, and sets `FencingSucceeded=True` / `Succeeded=True`. Once
the node reboots and rejoins and NHC deletes the CR, the node is uncordoned and the taint is removed.

The block-mode counterpart of the Filesystem node hang test (`node_hang.go`, OCP-88738), with its
own ID. The CR is looked up by node name on every poll because it can be deleted and recreated
when `SBRStorageUnhealthy` cycles to `Unknown`; `FencingInProgress` is deliberately not asserted
because the 60s fencing window and 60s staleness threshold make it flap.

Full test case: [docs/TESTCASES/rhwa-1931.md](../../docs/TESTCASES/rhwa-1931.md)

- **Operators**: SBR v5.8.0, NHC (skips when the NodeHealthCheck CRD is absent)
- **Cluster**: Multi-node, at least 2 schedulable workers, one hosting no SBR controller pod
- **Storage**: ODF Ceph RBD StorageClass, RWX, `volumeMode: Block`. Auto-discovered by
  provisioner (`rbd.csi.ceph.com`); `SBR_RBD_STORAGE_CLASS` overrides it. Skips when the
  cluster has none
- **Environment**: Connected or disconnected; `SBR_INJECTOR_IMAGE` must be mirrored when
  disconnected. **Destructive** — one worker is kernel-panicked and rebooted. Self Node
  Remediation must not be running. Skips when another `NodeHealthCheck` already remediates through
  the SBR template
- **Standalone**: `ginkgo --label-filter="sbr" --focus="kernel panic on a Ceph RBD block-mode node" ./tests/sbr-operator/...`
- **Pass criteria**: `SBRStorageUnhealthy=True` (reason `HeartbeatTimeout`) after the panic; NHC
  creates exactly one `StorageBasedRemediation` CR for the victim; it reaches `FencingSucceeded=True`
  and `Succeeded=True`; the node carries the `node.kubernetes.io/out-of-service` taint, then returns
  Ready with a changed boot-id and a Ready agent pod; the boot-id holds steady for 90s; NHC deletes
  the CR and the node becomes schedulable with the taint gone
