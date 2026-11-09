# SBR Operator Post-Deployment Tests

Automated tests validating the Storage-Based Remediation (SBR) operator
deployment, security posture, and high-availability configuration.

## Prerequisites

- OpenShift with cluster-admin access through `KUBECONFIG` and the WAO
  **storageBasedRemediation** operand enabled and Available in
  `openshift-workload-availability`.
- The WAO **nodeHealthCheck** operand enabled and Available for NHC integration,
  node-hang, storage-loss fencing, and split-brain scenarios. SNR, FAR, MDR and
  NMO are not dependencies of this suite.
- **SNR agents stopped** for watchdog/fencing scenarios. Some isolated regression
  specs require its DaemonSet to be absent. Disabling SNR's manager can leave
  agents behind (`DisableIncomplete`); it does not by itself free the watchdog.
- Two Ready, schedulable workers for most storage scenarios. Ceph RBD and
  filesystem split-brain need at least three Ready, schedulable workers, with a
  target that does not host an SBR controller. Healthy witnesses may host SBR
  controllers.
- An existing **RWX filesystem StorageClass**. Set `SBR_STORAGE_CLASS`, or let
  the suite discover a CephFS provisioner. Total I/O loss and transient filesystem
  partition scenarios specifically require CephFS.
- An existing **Ceph RBD StorageClass supporting RWX raw Block volumes** for
  block scenarios. Set `SBR_RBD_STORAGE_CLASS`, or let the suite discover an
  `rbd.csi.ceph.com` provisioner. The SBRC CRD must expose
  `sharedStorageVolumeMode`. The suite does not install ODF or a storage provider.
  Ordinary AWS EBS storage does not satisfy these shared-storage paths.
- Hardware watchdog devices or the softdog kernel module available on nodes.
- Pullable `WORKLOAD_IMAGE` (shell/sleep/ls), `SBR_INJECTOR_IMAGE` (defaults to
  `registry.access.redhat.com/ubi9/ubi:latest`, with `nsenter` and `iptables`),
  and optionally `SBR_WATCHDOG_DEBUG_IMAGE`. Mirror these and the must-gather
  image for disconnected clusters.

No SSH key or bastion is required. The specs execute node commands through
privileged host-namespace pods and the Kubernetes API. Destructive cases include
storage/network faults, kernel panics and watchdog reboots.

Use a disposable cluster and clear conflicting configurations from earlier runs.
Several specs require an isolated SBRC or no existing NHC targeting SBR. Tests
create their own SBRCs, remediation templates and NHC configurations. An SBRC
provisions the shared PVC and agent DaemonSet; enabling WAO's SBR operand alone
only starts its manager.

## Running

Enable the operands before running the full suite:

```bash
export KUBECONFIG=/path/to/kubeconfig
oc patch workloadavailabilityconfig cluster --type=merge \
  -p '{"spec":{"components":{"storageBasedRemediation":{"enabled":true},"nodeHealthCheck":{"enabled":true},"selfNodeRemediation":{"enabled":false}}}}'
oc -n openshift-workload-availability rollout status deployment/sbr-operator-controller-manager --timeout=5m
oc -n openshift-workload-availability rollout status deployment/node-healthcheck-controller-manager --timeout=5m
```

If the cluster previously ran SNR tests, finish active remediations and disable
SNR through WAO before removing its retained agents:

```bash
oc -n openshift-workload-availability delete daemonset self-node-remediation-ds --ignore-not-found
oc -n openshift-workload-availability wait --for=delete pod \
  -l app.kubernetes.io/name=self-node-remediation --timeout=5m
```

Set storage overrides if Ceph auto-discovery would select the wrong class, then
run the suite package directly:

```bash
export WORKLOAD_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest
# Optional when suitable Ceph classes can be auto-discovered:
# export SBR_STORAGE_CLASS=your-cephfs-rwx-class
# export SBR_RBD_STORAGE_CLASS=your-ceph-rbd-class

set -o pipefail
ginkgo --timeout=3h -v --fail-fast --label-filter='operator:sbr' \
  ./tests/sbr-operator 2>&1 | tee /tmp/sbr-test.log
```

Use `--label-filter='operator:sbr && tier:smoke'` for smoke checks, or
`--label-filter='operator:sbr && !disruption:destructive'` to exclude destructive
specs. Nondestructive functional checks may still provision storage or restart
agents. The standalone fresh-install and upgrade scenarios are removed, so no
upgrade exclusion filter is needed. The path without `/...` selects the cluster
suite directly instead of also running ordinary Go unit tests.

Known-bug skips remain for Ceph RBD persistent fencing (RHWA-1987), transient RBD
partitions (RHWA-1966), and detection-only coverage (RHWA-2033/RHWA-2036).
Storage-specific specs skip if the required class is unavailable. Those skips
are separate from the removed installation scenarios.

Ceph stale-fence recovery discovers worker host addresses from
`Node.status.addresses` (`InternalIP`, including IPv6) and the client identity
from Rook's Ceph configuration. External clusters can therefore use their
configured healthchecker identity instead of requiring `client.admin` to list
fences. Range removal uses a matching CSI `NetworkFenceClass` and a temporary
`NetworkFence` requesting `Unfenced`, so CSI uses its existing fencing credentials
without the tests reading or copying keys. The request is removed afterward.
Clusters without a matching class fall back to the configured Ceph CLI identity,
which must then have permission to remove blocklist ranges. Authentication or
permission failures are reported as recovery warnings.

Recovery only removes a single-host range belonging to a completed fault target
in the current test process, after confirming fault cleanup, node readiness,
no active SBR remediation or SBR-based NHC, and the fence age/cooldown. It checks
again after each fault spec and during later storage setup. Unrelated fences
and individual CephFS client eviction entries remain intact. Fences left by a
previous process require separate recovery because this suite cannot establish
their ownership from a blocklist address alone.

## Watching a run

```bash
watch -n 5 -d tools/watch/sbr-watch.sh
# On macOS without watch:
tools/watch/sbr-watch.sh --loop
```

The script reads WAO operand phases, nodes and boot IDs, SBRC conditions, SBR
remediation conditions, NHC status, PVCs/init jobs, workloads, controller lease
and pods. Record boot IDs before fencing. `SBRStorageUnhealthy=True` indicates
storage health detection; fencing conditions and a later boot ID change establish
subsequent stages. NHC may delete an SBR CR before the next snapshot. Pending
PVCs or failed init Jobs explain why agents have not started. The SBR manager
should remain Ready during agent-only faults.

## Tests

### 1. Verify SBR Operator Pod is Running ([OCP-89232](https://polarion.engineering.redhat.com/polarion/#/project/OSE/workitem?id=OCP-89232))

Validates that SBR controller-manager pods are in Running state and the
pod count matches the cluster topology (2 on multi-node, 1 on SNO).

- **Operators**: SBR v0.3.0
- **Cluster**: Any topology (MNO or SNO)
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="pod is running" ./tests/sbr-operator`
- **Pass criteria**: All pods Running, count matches expected replicas

### 3. Verify SBR Controller Replicas and Node Distribution ([OCP-89234](https://polarion.engineering.redhat.com/polarion/#/project/OSE/workitem?id=OCP-89234))

Validates that 2 replicas are running and scheduled on different nodes
for high availability. Skipped on SNO clusters where only 1 replica is
expected.

- **Operators**: SBR v0.3.0
- **Cluster**: Multi-node only (skips on SNO)
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="correct number of replicas" ./tests/sbr-operator`
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
- **Standalone**: `ginkgo --label-filter="sbr" --focus="non-root user" ./tests/sbr-operator`
- **Pass criteria**: All security context fields match restricted profile

### 5. Verify SBR CRD API Group and Served Version ([OCP-88822](https://polarion.engineering.redhat.com/polarion/#/project/OSE/workitem?id=OCP-88822))

Reads each installed SBR CRD directly and verifies its Kind, API group
`storage-based-remediation.medik8s.io`, and served `v1alpha1` version. WAO owns
these CRDs; there is no separate SBR CSV to inspect.

- **Operators**: SBR enabled through WAO
- **Cluster**: Any topology
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="operator:sbr" --focus="CRDs use the expected API" ./tests/sbr-operator`
- **Pass criteria**: All three CRDs exist with the expected Kind, group and served version

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
- **Standalone**: `ginkgo --label-filter="sbr" --focus="StorageBasedRemediationConfig" ./tests/sbr-operator`
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
- **Standalone**: `ginkgo --label-filter="sbr" --focus="invalid watchdog path and non-matching nodeSelector" ./tests/sbr-operator`
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
- **Standalone**: `ginkgo --label-filter="sbr" --focus="Verify watchdog device" ./tests/sbr-operator`
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
- **Standalone**: `ginkgo --label-filter="sbr" --focus="must-gather" ./tests/sbr-operator`
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
- **Standalone**: `ginkgo --label-filter="sbr" --focus="controller availability with one worker" ./tests/sbr-operator`
- **Pass criteria**: At least 1 controller pod remains Running on the uncordoned node; availability sustained for 30s (Consistently); deployment scales back to 2 replicas on different nodes after uncordoning

### 11. Verify Controller Leadership Handover

Validates that when the active SBR controller pod (the lease holder) is deleted, leadership
transfers to a different controller pod. Follows the same pattern as the FAR controller
lifecycle test (OCP-70636).

- **Operators**: SBR v0.3.0
- **Cluster**: Any topology with at least 2 schedulable worker-only (non-control-plane) nodes
- **Storage**: None
- **Environment**: Connected or disconnected
- **Standalone**: `ginkgo --label-filter="sbr" --focus="controller leadership" ./tests/sbr-operator`
- **Pass criteria**: Lease `holderIdentity` changes to a different Running controller pod; deployment returns to full ready replicas

### 12. Verify Fencing State Persists on Ceph RBD Block Storage Across Node Reboot ([RHWA-1916](https://redhat.atlassian.net/browse/RHWA-1916))

Validates that a node fenced against a Ceph RBD block device stays fenced exactly once. The test
creates an SBRC with `sharedStorageVolumeMode: Block`, triggers fencing with a manually created
`StorageBasedRemediation` CR, and follows the node through reboot and recovery, checking that the
operator does not create a second remediation CR for it afterwards.

Full specification: [`../../docs/TESTCASES/rhwa-1916.md`](../../docs/TESTCASES/rhwa-1916.md).

- **Operators**: SBR ≥ 5.8.0 with `sharedStorageVolumeMode` in the served CRD schema ([RHWA-1313](https://redhat.atlassian.net/browse/RHWA-1313))
- **Cluster**: At least one schedulable worker node that hosts no SBR controller pod and may be rebooted
- **Storage**: Ceph RBD block StorageClass (provisioner containing `rbd.csi.ceph.com`), auto-discovered or set with `SBR_RBD_STORAGE_CLASS`
- **Environment**: Connected or disconnected. **Destructive** — the target node is fenced and hard-rebooted
- **Standalone**: `ginkgo --label-filter="sbr" --focus="fencing state persists on Ceph RBD" ./tests/sbr-operator`
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
- **Standalone**: `ginkgo --label-filter="sbr" --focus="write concurrently to Ceph RBD block device" ./tests/sbr-operator`
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
- **Standalone**: `ginkgo --label-filter="sbr" --focus="transient Ceph RBD network partition" ./tests/sbr-operator`
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
- **Standalone**: `ginkgo --label-filter="sbr" --focus="heartbeats remain stable on Ceph RBD under load" ./tests/sbr-operator`
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
- **Standalone**: `ginkgo --label-filter="sbr" --focus="device inspection works on Ceph RBD" ./tests/sbr-operator`
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
own ID. The CR and the Node are recorded (list + watch) from before the panic, because NHC can
delete the CR, and the reconciler the taint, as soon as `SBRStorageUnhealthy` cycles to `Unknown`;
`FencingInProgress` is deliberately not asserted
because the 60s fencing window and 60s staleness threshold make it flap.

For DFBUGS-11731, post-reboot recovery waits for natural remediation deletion,
pauses only this test's NHC, and removes an eligible stale ODF fence before the
agent-readiness wait completes. It restarts only that node's RBD CSI nodeplugin
after unfencing to discard cached failed connections. NHC resumes after the agent
and storage condition recover, before the 90-second stability check. This storage
workaround is recorded in the report and preserves all fencing and reboot assertions.

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
- **Standalone**: `ginkgo --label-filter="sbr" --focus="kernel panic on a Ceph RBD block-mode node" ./tests/sbr-operator`
- **Pass criteria**: `SBRStorageUnhealthy=True` (reason `HeartbeatTimeout`) after the panic; NHC
  creates exactly one `StorageBasedRemediation` CR for the victim; it reaches `FencingSucceeded=True`
  and `Succeeded=True`; the node carries the `node.kubernetes.io/out-of-service` taint, then returns
  Ready with a changed boot-id and a Ready agent pod; the boot-id holds steady for 90s; NHC deletes
  the CR and the node becomes schedulable with the taint gone

### 18. Verify Split-Brain Prevention on Ceph RBD Block Storage ([RHWA-1934](https://issues.redhat.com/browse/RHWA-1934))

Proves that Block-mode fencing hits only the storage-isolated node. A privileged injector pod blocks
outbound Ceph traffic (ports 3300, 6789, 6800–7300) on one victim worker while its peers keep their
device access. A peer sets `SBRStorageUnhealthy=True` (reason `HeartbeatTimeout`) on the victim; NHC
creates a `StorageBasedRemediation` CR; a peer agent fences it; and the reconciler proves death from
storage, applies the `node.kubernetes.io/out-of-service` taint, and sets `FencingSucceeded=True` /
`Succeeded=True`. The victim reboots, rejoins, and is uncordoned with the taint removed once NHC
deletes the CR.

Every poll during fencing, reboot and storage recovery checks that no witness is flagged unhealthy
or gets a CR. Witnesses must be Ready and schedulable with their original boot IDs, agent pods,
restart counts and `sbr_self_fenced_total`, including a recheck after recovery and stability. The victim's heartbeat node ID is looked up in the
on-device node map through an observer agent on a witness, its slot is read at index nodeID-1, and the
`FencingSucceeded` transition is asserted to be later than the victim's last recorded heartbeat —
death cannot be proven before the node went silent.

The block-mode counterpart of the Filesystem split-brain test (`split_brain.go`, OCP-88877), with its
own ID. The CR is looked up by node name on every poll because it can be recreated when
`SBRStorageUnhealthy` cycles to `Unknown`; `FencingInProgress` is deliberately not asserted because
the 60s fencing window and 60s staleness threshold make it flap.

Post-reboot storage recovery shares the guarded DFBUGS-11731 workaround with the block node-hang
spec: wait for natural remediation deletion, pause only this test's NHC, recover the victim's
stale Ceph fence and local RBD CSI plugin, then require agent readiness and healthy storage before
resuming NHC. Witness checks continue throughout, and the final 90-second stability check runs
with NHC Enabled. The agent-readiness budget is ten minutes.

Full test case: [docs/TESTCASES/rhwa-1934.md](../../docs/TESTCASES/rhwa-1934.md)

- **Operators**: SBR v5.8.0, NHC (skips when the NodeHealthCheck CRD is absent)
- **Cluster**: Multi-node, at least 3 Ready, schedulable workers. Only the victim must host no SBR
  controller pod; controller-hosting workers may be witnesses
- **Storage**: ODF Ceph RBD StorageClass, RWX, `volumeMode: Block`. Auto-discovered by
  provisioner (`rbd.csi.ceph.com`); `SBR_RBD_STORAGE_CLASS` overrides it. Skips when the
  cluster has none
- **Environment**: Connected or disconnected; `SBR_INJECTOR_IMAGE` must be mirrored when
  disconnected. **Destructive** — one worker is cut off from Ceph, fenced and rebooted. Self Node
  Remediation must not be running. Skips when another `NodeHealthCheck` already remediates through
  the SBR template
- **Standalone**: `ginkgo --label-filter="sbr" --focus="only the storage-isolated Ceph RBD node is fenced" ./tests/sbr-operator`
- **Pass criteria**: victim reaches `SBRStorageUnhealthy=True` (reason `HeartbeatTimeout`); no
  witness is flagged unhealthy or gets a CR during the guarded waits; NHC creates a `StorageBasedRemediation` CR for
  the victim; it reaches `FencingSucceeded=True` / `Succeeded=True` with the transition later than the
  victim's last heartbeat, and the victim carries the `out-of-service` taint; every witness stays
  Ready, schedulable, same boot-id, unchanged `sbr_self_fenced_total`; the victim returns Ready with a
  changed boot-id and a Ready agent pod, holds steady for 90s, then NHC deletes the CR and the node
  becomes schedulable with the taint gone

### 19. Verify the SBR Agent DaemonSet Carries No Process-Table Probe ([RHWA-1965](https://redhat.atlassian.net/browse/RHWA-1965))

Regression cover for [RHWA-1955](https://redhat.atlassian.net/browse/RHWA-1955). Proves the agent
DaemonSet the controller renders carries no `livenessProbe` and no `startupProbe`,
and that no probe on the agent container reads the host process table. The removed check,
`grep -l sbr-agent /proc/*/cmdline`, was both blind and flaky: the agent pod sets `hostPID`, so the
probe's own `sh` and `grep` carry the string `sbr-agent` and the grep matched itself, while any host
process exiting mid-scan made it exit 2. On a busy node kubelet therefore restarted healthy agents —
and a restart releases the watchdog, so an agent killed while its node is cut off from shared storage
never comes back and nothing is left to reboot a node its peers have already fenced.

The spec also pins the surviving readiness probe: command, the five tuning fields, and the
detect-only variant that drops the watchdog check. It creates one config whose `nodeSelector` matches
no node, so the DaemonSet renders the pod template in full while scheduling zero agent pods; the
probe contract lives in that template, so no running agent is touched.

Runtime behaviour is deliberately not asserted here. That a healthy agent survives process churn,
that a dead agent still restarts (the agent is the container `ENTRYPOINT`), and that a
storage-partitioned node still self-fences belong to the destructive specs and to the manual
scenario on the bug.

Full test case: [docs/TESTCASES/rhwa-1965.md](../../docs/TESTCASES/rhwa-1965.md)

- **Operators**: SBR v0.3.0 (fix from storage-based-remediation PR #151)
- **Cluster**: Any topology, any node count
- **Storage**: Any RWX StorageClass; CephFS auto-discovered, `SBR_STORAGE_CLASS` overrides. Skips
  when the cluster has none. No agent pod is scheduled
- **Environment**: Connected or disconnected. **Non-destructive** — no node, no running agent and no
  node label is touched
- **Standalone**: `ginkgo --label-filter="sbr" --focus="no liveness or startup probe" ./tests/sbr-operator`
- **Pass criteria**: `livenessProbe` and `startupProbe` are `nil` on the agent container in both
  remediation and detect-only mode; no probe command contains `/proc`, `grep` or `cmdline`; the
  readiness command is exactly `test -f /var/run/sbr-agent/preflight-ok && test -c /dev/watchdog`
  with tuning `60/10/5/6/1`, and exactly `test -f /var/run/sbr-agent/preflight-ok` in detect-only
  mode; `hostPID` is still true; the DaemonSet schedules zero pods and is garbage-collected after
  the config is deleted

### 20. Verify Startup Clears the Unused Preflight Slot ([RHWA-1964](https://redhat.atlassian.net/browse/RHWA-1964))

Regression coverage for RHWA-1058. Creates a filesystem SBRC, verifies the shared
node map assigns every real agent a unique ID other than 1, and reads slot 1
through every agent for a minute. Skips if a real node owns ID 1.

Full test case: [docs/TESTCASES/rhwa-1964.md](../../docs/TESTCASES/rhwa-1964.md)

- **Operators**: SBR v5.8.0
- **Cluster**: At least two Ready, schedulable workers. SNR agents must be stopped,
  no unrelated SBRC may exist, and no NHC may reference an SBR template
- **Storage**: RWX filesystem, CephFS auto-discovery or `SBR_STORAGE_CLASS`
- **Environment**: Connected or disconnected. Non-destructive, no fault injection
- **Standalone**: `ginkgo -v --label-filter='operator:sbr && tier:acceptance' --focus='startup clears the unassigned preflight slot' ./tests/sbr-operator`
- **Pass criteria**: Valid node map with ID 1 unused, full slot 1 zero through every
  agent for one minute, unchanged Ready pods and restart counts. Test SBRC and
  operands removed

 
### 21. Verify Detect-Only Agents Stay Ready While Watchdog Is Held Open ([RHWA-1933](https://issues.redhat.com/browse/RHWA-1933))

Validates the RHWA-1184 regression fix: with `detectOnlyMode: Enabled`, SBR agent pods start and
remain Ready while a privileged keepalive pod holds `/dev/watchdog` open on every schedulable
worker. The observation also checks agent pod identities and restart counts, keepalive processes,
and that no worker reports `SBRStorageUnhealthy=True`. It runs unconditionally; no
`SBR_TEST_RHWA1068` environment variable is required.

Full test case: [docs/TESTCASES/rhwa-1933.md](../../docs/TESTCASES/rhwa-1933.md)

- **Operators**: SBR v5.8.0, with the RHWA-1184 fix
- **Cluster**: Multi-node, at least two schedulable workers
- **Storage**: RWX StorageClass (CephFS auto-discovered or set with `SBR_STORAGE_CLASS`); skips when absent
- **Environment**: Connected or disconnected. Non-destructive
- **Standalone**: `ginkgo -v --label-filter="operator:sbr && tier:acceptance" --focus="RHWA-1184 regression" ./tests/sbr-operator`
- **Pass criteria**: All agent pods stay Ready with the same UIDs and restart counts for 90 seconds; every keepalive remains running; no worker reports `SBRStorageUnhealthy=True`
 
### 22. Verify SBR Manages the Agent DaemonSet NetworkPolicy ([RHWA-1970](https://issues.redhat.com/browse/RHWA-1970))

Validates that the operator renders, owns, and reconciles the `sbr-agent-<sbrcName>`
`NetworkPolicy` for the agent DaemonSet ([RHWA-1646](https://redhat.atlassian.net/browse/RHWA-1646)):
automatic creation and controller ownership by its `StorageBasedRemediationConfig`, an ingress rule
scoped to the two metrics ports (`8080`/`8082`) and restricted to namespaces labeled
`metrics: enabled`, a deliberately unrestricted egress rule, recreation after manual deletion,
restoration of a manually widened ingress rule, and the `MetricsNamespaceLabelMissing` warning
event when `openshift-monitoring` lacks the required label. The test's SBRC uses a `nodeSelector`
that matches no node, so the `NetworkPolicy` and DaemonSet render in full while scheduling zero
agent pods.

Full test case: [docs/TESTCASES/rhwa-1970.md](../../docs/TESTCASES/rhwa-1970.md)

- **Operators**: SBR v5.8.0
- **Cluster**: Any topology
- **Storage**: Any RWX StorageClass; CephFS auto-discovered, `SBR_STORAGE_CLASS` overrides. Skips
  when the cluster has none. No agent pod is scheduled
- **Environment**: Connected or disconnected. **Non-destructive** — no node, running agent, or node
  label is touched
- **Standalone**: `ginkgo --label-filter="operator:sbr && tier:smoke" --focus="manages the agent DaemonSet NetworkPolicy" ./tests/sbr-operator`
- **Pass criteria**: `NetworkPolicy` exists, owned by the SBRC, selecting only its agent pods;
  ingress allows only the two metrics ports from namespaces labeled `metrics: enabled`; egress is
  the single documented allow-all rule; a deleted `NetworkPolicy` is recreated and a manually
  widened ingress rule is restored; the SBRC gets a `MetricsNamespaceLabelMissing` warning event
  when `openshift-monitoring` lacks the label
 

### 23. Verify Fence Slot Geometry on Ceph RBD Block Storage ([RHWA-2031](https://issues.redhat.com/browse/RHWA-2031))

Regression cover for the reconciler/agent slot offset mismatch fixed upstream in `4131c812`. In
Block mode the agent reads its own fence slot at `(nodeID - 1) * 4096` while the controller wrote at
`nodeID * slotSize`: the fence landed one slot past the victim, which therefore never saw it, and
the completion check read the neighbour's zero-filled heartbeat slot — an unparseable slot counts as
"stopped", so fencing was reported as succeeded while the victim was still alive.

The test injects no fault. It creates a `StorageBasedRemediation` CR directly for the node holding
the highest node ID in the on-device node map — the node whose neighbouring slots are still
zero-filled, which is the case the defect was dangerous in — and lets the fencing run for real. Read
through an observer agent on another node, the fence message must appear in fence slot index
nodeID-1 and target the victim's node ID, while the slot after it stays empty on every poll; a fence
found there fails the spec immediately rather than waiting out the timeout. The victim must then act
on the fence and reboot, and `FencingSucceeded` must only go True after its heartbeat slot has gone
stale: a `FencingSucceeded=True` observed while that slot is still fresh fails the spec.

Staleness is measured between timestamps written on the device — the victim's slot against the
newest slot on the device — never against the test runner's clock, because an exec into the observer
pod can stall for tens of seconds and a read-time measure would turn that stall into a false "the
victim stopped". The spec deletes the CR itself once the victim is Ready and only then waits for its
agent: the `out-of-service` taint evicts the Ceph RBD CSI node plugin, so until the CR is gone the
agent cannot attach the shared device.

Full test case: [docs/TESTCASES/rhwa-2031.md](../../docs/TESTCASES/rhwa-2031.md)

- **Operators**: SBR v5.8.0 (no NHC — the spec creates the remediation CR itself)
- **Cluster**: Multi-node, agents on at least 2 nodes; skips when the node holding the highest node
  ID hosts an SBR controller pod, since that node is rebooted
- **Storage**: ODF Ceph RBD StorageClass, RWX, `volumeMode: Block`. Auto-discovered by provisioner
  (`rbd.csi.ceph.com`); `SBR_RBD_STORAGE_CLASS` overrides it. Skips when the cluster has none
- **Environment**: Connected or disconnected. **Destructive** — one node is cordoned, fenced,
  tainted `out-of-service` and rebooted. Self Node Remediation must not be running. Skips when a
  `NodeHealthCheck` already remediates through the SBR template
- **Standalone**: `ginkgo --label-filter="operator:sbr && component:remediation" --focus="the fence reaches the slot the victim reads" ./tests/sbr-operator`
- **Pass criteria**: before the CR exists the victim's fence slot asks for nothing and the fence and
  heartbeat slots after it are zero-filled; a fence message targeting the victim's node ID appears in
  slot index nodeID-1 within 3m with the next slot still empty; the victim's heartbeat slot falls
  more than 45s behind the newest heartbeat on the device with no `FencingSucceeded=True` while it is
  under 30s behind; the CR reaches `FencingSucceeded=True` / `Succeeded=True` with the transition
  later than the victim's last heartbeat; the victim carries the `out-of-service` taint, reboots with
  a changed boot-id and returns Ready; after the CR is deleted it is schedulable with the taint gone
  and its agent pod is Ready again

### 24. Verify SBRC Node Map Cleanup and Recreation (RHWA-1929)

Creates a filesystem-mode SBRC, records the live node map, and verifies that its
deletion runs a successful cleanup Job and clears the map on the original
volume. It then recreates the same name and spec, requires the same PVC UID and
PV name, and checks agent readiness, restart counts, and the recreated node map.

Design: [test plan PR #63](https://github.com/openshift/rhwa-system-tests/pull/63).
Test task: [RHWA-1929](https://redhat.atlassian.net/browse/RHWA-1929).
Product fix: [SBR #69](https://github.com/medik8s/storage-based-remediation/pull/69).

- **Operators**: SBR v5.8.0 already installed. This is not an operator-upgrade test.
- **Cluster**: At least two Ready, schedulable workers. SNR agents must not be
  running or scheduled. The SNR operator itself may remain installed. No NHC may
  reference an SBR template, directly or through escalating remediation.
- **Storage**: Existing RWX filesystem storage. `SBR_STORAGE_CLASS` selects it,
  otherwise CephFS is auto-discovered. The test does not install ODF.
- **Environment**: Connected or disconnected. For a disconnected cluster,
  `SBR_INJECTOR_IMAGE` must provide `sh`, `sleep`, `dd`, and `base64` from a reachable
  registry. It defaults to `registry.access.redhat.com/ubi9/ubi:latest`.
- **Standalone**: `ginkgo -vv --label-filter="operator:sbr && tier:acceptance" --focus="SBRC deletion clears the node map" ./tests/sbr-operator`
- **Pass criteria**: Exactly one spec with ID `1929` passes. The original map is
  absent or empty after cleanup. The recreated agents become Ready within three minutes
  with at most one restart, then keep their UIDs and restart counts for three
  minutes. The recreated map contains only active agent nodes, and the SBRC reports Ready.
  Test-owned resources are removed, including the retained PVC.

#### Storage identity and cleanup

The test deliberately retains its own PVC across SBRC deletion by adding the
observer pod as a second, non-controller owner of that PVC. Normal garbage
collection removes the old SBRC, agents and init Job without waiting for a
test-only finalizer. The test never strips an operator finalizer.

This is necessary because recreating against the same StorageClass does not
guarantee reuse of the same data, and the controller changes the PV reclaim
policy to `Delete`. The read-only observer mounts the original PVC before
deletion and remains on it through verification. Both its PVC UID and PV name
must remain unchanged on recreation.

The observer uses SBR's existing agent service account and privileged SCC access
to read the volume's SELinux-labeled files. It has no host mounts or API token.
Both the PVC volume and container mount remain read-only. Cleanup explicitly
removes the test's cleanup Jobs and their pods before deleting the PVC, so
orphaned completed pods cannot leave that PVC stuck in Terminating.

An empty node-map file is allowed because the agent's file-lock operation can
recreate it during shutdown. Any nonempty map fails. The probe also requires
the original heartbeat device to remain readable and nonempty, so an inaccessible
or empty mount cannot produce a false pass.

The implementation retains the original PVC until final cleanup, rather than
waiting for its deletion as originally proposed in PR #63. This proves cleanup
of the original map and healthy recreation on the same storage, not a guaranteed
reproduction of the historical restart loop.

The test removes only resources bearing its own marker or belonging to its
uniquely named SBRC. It never uninstalls operators, disables SNR, or sweeps all
cleanup Jobs in the namespace. A missing SNR/storage prerequisite is a skip,
not evidence that the regression passed.

Local invocation after prerequisites are satisfied:

```bash
export KUBECONFIG=/absolute/path/to/kubeconfig
export ECO_TEST_FEATURES=sbr-operator
export ECO_TEST_LABELS='operator:sbr && tier:acceptance'
export ECO_TEST_FOCUS='SBRC deletion clears the node map'
export ECO_TEST_VERBOSE=true
export WORKLOAD_IMAGE=unused-by-sbr-node-map-cleanup
export SBR_STORAGE_CLASS=ocs-external-storagecluster-cephfs
export ECO_REPORTS_DUMP_DIR="$(mktemp -d)"
make run-tests
```

Live validation passed on 2026-10-22 against SBR v5.8.0 with CephFS, including
same-volume recreation, zero agent restarts during the stability window, and
complete test cleanup. A before/after comparison still requires a pre-fix build.

### 25. Verify NHC Triggers SBR Fencing (OCP-88879)

Partitions one worker from CephFS and requires NHC to create its SBR remediation.
The target must reboot with a changed boot ID and return Ready. A successful
fencing condition alone cannot satisfy the test.

- **Operands**: SBR and NHC enabled through WAO; SNR agents stopped.
- **Storage**: CephFS RWX, selected by `SBR_STORAGE_CLASS` or auto-discovery.
- **Timing**: A 30-second SBR timeout leaves room for NHC's 30-second unhealthy
  duration and the hardware watchdog reset before the condition becomes Unknown.
- **Cleanup**: Removes injected host rules, recreating an evicted injector on
  its recorded Ready node when necessary. Removes remediation resources before
  the SBRC, and purges the injector image only after confirming a reboot.
- **Standalone**: `ginkgo --timeout=40m -v --label-filter='operator:sbr && test_id:88879' ./tests/sbr-operator`

### 26. Verify Filesystem Split-Brain Fences Only the Isolated Worker (OCP-88877)

Partitions one worker from CephFS and verifies it reboots through NHC/SBR
remediation while every healthy witness keeps its original boot ID and remains
schedulable.

- **Topology**: At least three Ready, schedulable workers. Only the target must
  host no SBR controller pod; controller-hosting workers count as witnesses.
- **Operands/storage**: SBR and NHC enabled, SNR agents stopped, and CephFS RWX
  selected by `SBR_STORAGE_CLASS` or auto-discovery.
- **Standalone**: `ginkgo --timeout=40m -v --label-filter='operator:sbr && test_id:88877' ./tests/sbr-operator`
