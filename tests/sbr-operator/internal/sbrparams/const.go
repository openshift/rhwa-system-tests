package sbrparams

import (
	"os"
	"time"

	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
)

const (
	// DefaultPollInterval is the polling interval used with Eventually calls.
	DefaultPollInterval = 5 * time.Second

	// NoNewDaemonSetCheckDuration is how long the controller is observed to confirm no unexpected DaemonSet is created.
	NoNewDaemonSetCheckDuration = 30 * time.Second

	// NoNewDaemonSetCheckInterval is the polling interval used with NoNewDaemonSetCheckDuration.
	NoNewDaemonSetCheckInterval = 5 * time.Second

	// Label represents SBR operator label that can be used for test cases selection.
	Label = "sbr"

	// ExpectedReplicas defines the expected number of replicas for SBR controller manager.
	ExpectedReplicas = int32(2)

	// MinReplicasWhenDegraded is the minimum number of controller pods/replicas that must remain
	// available during the single-worker degraded phase (only the keeper node is schedulable).
	MinReplicasWhenDegraded = int32(1)

	// ManagerContainerName is the name of the main controller container in the SBR pod.
	ManagerContainerName = "manager"

	// CRDGroup is the Kubernetes API group for all SBR custom resources.
	CRDGroup = "storage-based-remediation.medik8s.io"

	// CRDVersion is the API version for all SBR custom resources.
	CRDVersion = "v1alpha1"

	// SBRCTimeoutSecondsMin is the minimum allowed value for sbrTimeoutSeconds (CRD schema enforced).
	SBRCTimeoutSecondsMin = 10

	// SBRCTimeoutSecondsMax is the maximum allowed value for sbrTimeoutSeconds (CRD schema enforced).
	SBRCTimeoutSecondsMax = 300

	// SBRCMaxConsecutiveFailuresMin is the minimum allowed value for maxConsecutiveFailures (CRD schema enforced).
	SBRCMaxConsecutiveFailuresMin = 2

	// SBRCMaxConsecutiveFailuresMax is the maximum allowed value for maxConsecutiveFailures (CRD schema enforced).
	SBRCMaxConsecutiveFailuresMax = 32

	// SBRCInvalidTestName is the name used for short-lived invalid StorageBasedRemediationConfig CRs in negative tests.
	SBRCInvalidTestName = "test-invalid-sbrc"

	// SBRCControllerTestName is the name used for StorageBasedRemediationConfig CRs testing controller-layer validation.
	SBRCControllerTestName = "test-controller-invalid-sbrc"

	// SBRCWatchdogTestName is the name used for StorageBasedRemediationConfig CRs testing invalid watchdog path handling.
	SBRCWatchdogTestName = "test-sbrc-watchdog-neg"

	// SBRCNoMatchSelectorTestName is the name used for StorageBasedRemediationConfig CRs
	// testing non-matching nodeSelector handling.
	SBRCNoMatchSelectorTestName = "test-sbrc-selector-neg"

	// SBRCInvalidWatchdogPath is a watchdog device path guaranteed not to exist on test nodes.
	SBRCInvalidWatchdogPath = "/dev/sbr-test-nonexistent-watchdog"

	// SBRCNoMatchSelectorKey is the node label key used to create a StorageBasedRemediationConfig
	// nodeSelector that matches no nodes.
	SBRCNoMatchSelectorKey = "sbr-test-no-match-selector"

	// SBRCNoMatchSelectorValue is the node label value used to create a StorageBasedRemediationConfig
	// nodeSelector that matches no nodes.
	SBRCNoMatchSelectorValue = "sbr-test-no-match-value"

	// OperatorDeploymentName is the name of the SBR operator controller manager deployment.
	OperatorDeploymentName = "sbr-operator-controller-manager"

	// OperatorControllerPodLabel is the app label value used to select SBR controller pods.
	OperatorControllerPodLabel = "sbr-operator"

	// OperatorControllerPodLabelSelector is the label selector string to filter SBR controller pods,
	// excluding device-init and agent pods that share the app.kubernetes.io/name label.
	OperatorControllerPodLabelSelector = "app.kubernetes.io/name=" + OperatorControllerPodLabel +
		",control-plane=controller-manager"

	// CSVNamePattern is the substring used to match the SBR operator ClusterServiceVersion by name.
	CSVNamePattern = "storage-based-remediation"

	// SBRRemediationFinalizer is the finalizer the controller adds to every StorageBasedRemediation CR.
	// Sourced from internal/controller/storagebasedremediation_controller.go.
	SBRRemediationFinalizer = "medik8s.io/sbr-remediation-finalizer"

	// SBRCFunctionalTestName is the name of the temporary SBRC created in functional tests
	// that need agent DaemonSet pods to process StorageBasedRemediation CRs.
	// The SBRRemediationReconciler runs inside agent pods, so an SBRC (and its DaemonSet) must
	// exist before any StorageBasedRemediation CR can be reconciled.
	SBRCFunctionalTestName = "test-sbrc-functional"

	// SBRAgentDaemonSetPrefix is the naming prefix the SBRC controller uses for agent DaemonSets.
	// DaemonSet name = SBRAgentDaemonSetPrefix + <sbrcName>.
	SBRAgentDaemonSetPrefix = "sbr-agent-"

	// SBRCReadyTimeout is the time allowed for the SBRC's agent DaemonSet to have all scheduled
	// pods reach Ready before a functional test begins. The first SBRC created in a suite pays a
	// one-time cold-start cost (first CephFS PVC bind plus SBD device-init Job on a freshly
	// provisioned ODF cluster); later SBRCs reuse the warmed CSI path and become ready in ~90s.
	// The ceiling is sized for that worst-case cold start - a ready DaemonSet returns immediately,
	// so healthy runs never wait this long.
	SBRCReadyTimeout = 10 * time.Minute

	// NHCCleanupTimeout bounds how long cleanup retries deleting an NHC CR. The NHC
	// webhook admits the delete only after the NHC ends its remediation, which
	// follows the node becoming healthy again once the injected fault is removed.
	NHCCleanupTimeout = 5 * time.Minute

	// ForceSBRCRCleanupTimeout bounds the Get/Update/Delete calls that force-remove a
	// StorageBasedRemediation CR during cleanup.
	ForceSBRCRCleanupTimeout = 1 * time.Minute

	// NodeRestoreTimeout bounds the Node Get/Update calls that uncordon a node and
	// remove its out-of-service taint after an SBR CR finalizer was force-stripped.
	NodeRestoreTimeout = 1 * time.Minute

	// InjectorRuleCleanupTimeout bounds the injector-pod lookup and the mid-reboot
	// boot-ID wait in removeInjectedRejectRules, so a slow API server or a reboot
	// that outlives the window cannot hang cleanup.
	InjectorRuleCleanupTimeout = 1 * time.Minute

	// ODFNamespace is the namespace of the ODF (Rook/Ceph) operators.
	ODFNamespace = "openshift-storage"

	// RookOperatorPodLabelSelector selects the Rook operator pod, which has the ceph CLI
	// and the admin config used to clear stale ODF fences (DFBUGS-11731).
	RookOperatorPodLabelSelector = "app=rook-ceph-operator"

	// RookOperatorContainer is the container that runs ceph commands in the Rook operator pod.
	RookOperatorContainer = "rook-ceph-operator"

	// RookCephConfig is the admin config Rook writes for the openshift-storage cluster.
	RookCephConfig = "/var/lib/rook/openshift-storage/openshift-storage.config"

	// ODFAutoFenceExpiry is ceph-csi's AutoBlocklistTime: the expiry of the range blocklist
	// entry it adds for a node with the out-of-service taint. The expiry is a heuristic;
	// recovery also requires a completed test target and confirmed fault cleanup.
	ODFAutoFenceExpiry = 94670856 * time.Second

	// ODFAutoFenceCooldown matches ceph-csi's minimum age before automatic unfencing.
	ODFAutoFenceCooldown = 5 * time.Minute

	// StaleODFFenceMaxAge caps the inferred age of a recoverable test fence.
	StaleODFFenceMaxAge = 6 * time.Hour

	// StaleODFFenceRetryInterval throttles recovery checks during storage setup polling.
	StaleODFFenceRetryInterval = 30 * time.Second

	// StaleODFFenceTimeout bounds the ceph commands run to clear stale ODF fences.
	StaleODFFenceTimeout = 1 * time.Minute

	// SBRCReadyDiagMaxEvents caps how many recent Warning events are included in the on-timeout
	// readiness diagnostics dumped by waitForSBRCReady, to keep the failure message readable.
	SBRCReadyDiagMaxEvents = 15

	// SBRCReadyDiagTimeout bounds the API calls made while collecting on-timeout readiness
	// diagnostics, so a slow or wedged apiserver cannot hang the already-failed test.
	SBRCReadyDiagTimeout = 30 * time.Second

	// WatchdogProbeLogTimeout is the deadline for reading a completed watchdog probe pod's logs
	// via the in-cluster API.
	WatchdogProbeLogTimeout = 30 * time.Second

	// PVCheckTimeout is how long to wait for a PV in a static StorageClass to appear before
	// creating an SBRC. Kept short so NFS infrastructure failures surface quickly.
	PVCheckTimeout = 30 * time.Second

	// SBRStorageUnhealthyCondition is the node condition type set by the SBR agent when storage is unavailable.
	SBRStorageUnhealthyCondition = "SBRStorageUnhealthy"

	// StorageInjectionTimeout is how long to wait for SBRStorageUnhealthy=True after injecting faults.
	StorageInjectionTimeout = 3 * time.Minute

	// StorageInjectionPollInterval is the polling interval while waiting for the storage unhealthy condition.
	StorageInjectionPollInterval = 10 * time.Second

	// NHCCRDName is the CRD name for NodeHealthCheck, used to detect if NHC is installed.
	NHCCRDName = "nodehealthchecks.remediation.medik8s.io"

	// SBRCRDName is the CRD name for StorageBasedRemediationConfig, used to inspect the
	// installed SBR operator's API surface.
	SBRCRDName = "storagebasedremediationconfigs.storage-based-remediation.medik8s.io"

	// SBRCBlockVolumeModeField is the StorageBasedRemediationConfig spec field that selects
	// raw block device shared storage (RHWA-1313, RWX Block volume support).
	SBRCBlockVolumeModeField = "sharedStorageVolumeMode"

	// FirstSBRCsvWithBlockVolumeMode is the first SBR operator CSV version whose CRD ships
	// the sharedStorageVolumeMode field (RHWA-1313, upstream
	// medik8s/storage-based-remediation commit 8e9f551c32, released in 5.8.0). Operators at
	// or above this version must have the field; older operators predate it and skip the
	// Ceph RBD block persistent fencing spec instead of failing it.
	FirstSBRCsvWithBlockVolumeMode = "5.8.0"

	// NHCAPIGroup is the API group for NodeHealthCheck CRs.
	NHCAPIGroup = "remediation.medik8s.io"

	// NHCAPIVersion is the API version for NodeHealthCheck CRs.
	NHCAPIVersion = "v1alpha1"

	// SBRTemplateName is the name of the StorageBasedRemediationTemplate that NHC references.
	SBRTemplateName = "storagebasedremediationtemplate-sample"

	// SBRTemplateKind is the kind a NodeHealthCheck references to use SBR as its remediator.
	SBRTemplateKind = "StorageBasedRemediationTemplate"

	// NHCUnhealthyDuration is how long a node must hold SBRStorageUnhealthy=True before NHC acts.
	NHCUnhealthyDuration = "30s"

	// NHCSplitBrainTestName is the NodeHealthCheck CR name used for the split-brain test.
	NHCSplitBrainTestName = "test-nhc-sbr-split-brain"

	// SBRCSplitBrainTestName is the SBRC name used in the split-brain test.
	SBRCSplitBrainTestName = "test-sbrc-split-brain"

	// NHCSBRCRCreationTimeout is how long to wait for NHC to create the StorageBasedRemediation CR.
	NHCSBRCRCreationTimeout = 5 * time.Minute

	// NHCSBRCRCreationPollInterval is the poll interval while waiting for NHC to create the SBR CR.
	NHCSBRCRCreationPollInterval = 10 * time.Second

	// NodeRebootTimeout is the maximum time to wait for a node to complete a full reboot cycle.
	NodeRebootTimeout = 20 * time.Minute

	// NodeRebootPollInterval is the polling interval while waiting for a node to become NotReady then Ready.
	NodeRebootPollInterval = 15 * time.Second

	// SplitBrainHealthyNodeCheckDuration is how long to Consistently assert no SBR CR exists for healthy nodes.
	SplitBrainHealthyNodeCheckDuration = 60 * time.Second

	// SplitBrainHealthyNodeCheckInterval is the poll interval for the healthy-node Consistently assertion.
	SplitBrainHealthyNodeCheckInterval = 5 * time.Second

	// SBRCWatchdogPathTestName is the SBRC name used in the storage I/O loss watchdog test (OCP-88880).
	SBRCWatchdogPathTestName = "test-sbrc-storage-loss-watchdog"

	// NHCSBRTestName is the NodeHealthCheck CR name for the storage I/O loss test (OCP-88880).
	NHCSBRTestName = "nhc-sbr-storage-loss"

	// InjectorPodName is the name prefix for privileged host-namespace pods used in SBR disruptive tests.
	InjectorPodName = "sbr-storage-injector"

	// FencingInProgressCondition is the condition type set on a StorageBasedRemediation CR during fencing.
	FencingInProgressCondition = "FencingInProgress"

	// SBRCDetectOnlyTestName is the SBRC name used in the detectOnlyMode test (OCP-88876).
	SBRCDetectOnlyTestName = "test-sbrc-detect-only"

	// WatchdogKeepaliveNamePrefix is the name prefix for privileged pods that hold /dev/watchdog
	// open during the RHWA-1068 regression test.
	WatchdogKeepaliveNamePrefix = "sbr-wdog-keepalive-"

	// WatchdogKeepaliveTimeout is how long to wait for all watchdog-keepalive pods to reach Running.
	WatchdogKeepaliveTimeout = 2 * time.Minute

	// DetectOnlyAllPodsReadyTimeout is how long to wait for ALL agent DaemonSet pods to reach Ready
	// in the detect-only watchdog contention regression test (RHWA-1068).
	DetectOnlyAllPodsReadyTimeout = 5 * time.Minute

	// DetectOnlySuppressionCheckDuration is how long to Consistently assert no fencing occurs while
	// SBRStorageUnhealthy=True and detectOnlyMode is Enabled.
	DetectOnlySuppressionCheckDuration = 90 * time.Second

	// DetectOnlySuppressionCheckInterval is the poll interval for the suppression Consistently check.
	DetectOnlySuppressionCheckInterval = 10 * time.Second
	// FencingSucceededCondition is the condition type set on a StorageBasedRemediation CR after fencing.
	FencingSucceededCondition = "FencingSucceeded"

	// SBRCRCleanupTimeout is how long to wait for an SBR CR to be cleaned up after fencing.
	SBRCRCleanupTimeout = 10 * time.Minute

	// SBRCRCleanupPollInterval is the polling interval while waiting for SBR CR removal.
	SBRCRCleanupPollInterval = 20 * time.Second

	// SBRCNodeHangTestName is the SBRC name for the hard node hang test (OCP-88738).
	SBRCNodeHangTestName = "test-sbrc-node-hang"

	// NHCNodeHangTestName is the NodeHealthCheck CR name for the node hang test (OCP-88738).
	NHCNodeHangTestName = "nhc-sbr-node-hang"

	// SBRCStorageLossWriteName is the SBRC name for the write-only storage loss test.
	SBRCStorageLossWriteName = "test-sbrc-storage-loss-write"

	// NHCWriteLossTestName is the NHC CR name for the write-only storage loss test.
	NHCWriteLossTestName = "nhc-sbr-write-loss"

	// StorageLossWriteInjectorPodName is the injector pod name prefix for the write-only test.
	StorageLossWriteInjectorPodName = "sbr-write-injector"

	// SBRCTransientTestName is the SBRC name for the transient storage failure self-healing test.
	SBRCTransientTestName = "test-sbrc-transient"

	// TransientInjectorPodName is the name prefix for the injector pod in the transient storage test.
	TransientInjectorPodName = "sbr-transient-injector"

	// SBRCLifecycleTestNameA is the first SBRC name for the lifecycle test (OCP-88734).
	SBRCLifecycleTestNameA = "test-sbrc-lifecycle-a"

	// SBRCLifecycleTestNameB is the second SBRC name for the lifecycle test (OCP-88734).
	SBRCLifecycleTestNameB = "test-sbrc-lifecycle-b"

	// SBRCLifecyclePatchedTimeout is how long to wait for a patched SBRC DaemonSet to roll out.
	SBRCLifecyclePatchedTimeout = 3 * time.Minute

	// SBRCDaemonSetGCTimeout is how long to wait for a DaemonSet to be GC'd after SBRC deletion.
	SBRCDaemonSetGCTimeout = 5 * time.Minute

	// SBRCNHCTestName is the SBRC name for the NHC integration test (OCP-88879).
	SBRCNHCTestName = "test-sbrc-nhc-integration"

	// NHCTestName is the NodeHealthCheck CR name for the NHC integration test (OCP-88879).
	NHCTestName = "nhc-sbr-integration"

	// NHCInjectorPodName is the name prefix for the privileged injector pod in the NHC integration test.
	NHCInjectorPodName = "sbr-nhc-injector"

	// SBRCMetricsTestName is the SBRC name used exclusively by the metrics test suite (OCP-89202).
	SBRCMetricsTestName = "test-sbrc-metrics"

	// AgentPodLabelSelector selects SBR agent pods deployed by any SBRC.
	AgentPodLabelSelector = "app=sbr-agent"
	// SBRCBlockTestName is the SBRC name for the Ceph RBD block storage concurrency test.
	SBRCBlockTestName = "test-sbrc-block-rbd"
	// AgentMetricsPort is the port on which SBR agent pods expose custom Prometheus metrics.
	// Port 8080 is controller-runtime's built-in metrics; port 8082 is the SBR agent's own metrics.
	AgentMetricsPort = "8082"

	// ControllerLeaseName is the SBR leader election lease name (LeaderElectionID in cmd/main.go).
	ControllerLeaseName = "sbr-operator-leader-election"

	// ControllerHandoverTimeout is how long to wait for controller leadership transfer after pod deletion.
	ControllerHandoverTimeout = 3 * time.Minute

	// MinWorkerNodesForResilienceTest is the minimum number of worker-only nodes required to
	// safely run the controller resilience test (cordon all but one) without touching control-plane capacity.
	MinWorkerNodesForResilienceTest = 3

	// ControllerRescheduleTimeout is how long to wait for the controller to reach a stable degraded
	// state (>= 1 ready replica) after cordoning workers and deleting pods from cordoned nodes.
	ControllerRescheduleTimeout = 5 * time.Minute

	// ControllerScaleBackTimeout is how long to wait for the controller deployment to return to
	// ExpectedReplicas after uncordoning all worker nodes.
	ControllerScaleBackTimeout = 5 * time.Minute

	// ControllerDegradedConsistentDuration is how long to Consistently assert the controller
	// remains available during the single-worker degraded phase.
	ControllerDegradedConsistentDuration = 30 * time.Second

	// MinWorkerNodesForHandoverTest is the minimum number of schedulable worker-only nodes
	// required for the leadership handover test (needs 2 replicas on different nodes).
	MinWorkerNodesForHandoverTest = 2

	// SBRCBlockPersistentFencingTestName is the SBRC name for the Ceph RBD persistent fencing state test.
	SBRCBlockPersistentFencingTestName = "test-sbrc-block-persistent-fencing"

	// BlockFencingStateCheckTimeout is how long to wait for block device slot state to stabilize after reboot.
	BlockFencingStateCheckTimeout = 5 * time.Minute

	// BlockFencingStateCheckInterval is the polling interval for block device slot state checks.
	BlockFencingStateCheckInterval = 10 * time.Second

	// SBRCBlockNodeHangTestName is the SBRC name for the Ceph RBD block-mode node hang test (OCP-88738).
	SBRCBlockNodeHangTestName = "test-sbrc-block-node-hang"

	// NHCBlockNodeHangTestName is the NodeHealthCheck CR name for the block-mode node hang test (OCP-88738).
	NHCBlockNodeHangTestName = "nhc-sbr-block-node-hang"

	// BlockNodeHangInjectorPodName is the privileged pod used to trigger a kernel panic on the victim
	// node in the Ceph RBD block-mode node hang test.
	BlockNodeHangInjectorPodName = "sbr-rbd-node-hang-injector"

	// FencingProvenTimeout is how long to wait for a victim's StorageBasedRemediation CR to report
	// both FencingSucceeded=True and Succeeded=True. Proof is storage-only (the victim's heartbeat
	// slot going stale) but the 60s fencing window can flap and retry, so this is generous.
	FencingProvenTimeout = 10 * time.Minute

	// SucceededCondition is the standard medik8s remediation outcome condition on a
	// StorageBasedRemediation CR; NHC escalation reads it. Set True alongside FencingSucceeded once
	// the reconciler has proved the victim is dead and applied the out-of-service taint.
	SucceededCondition = "Succeeded"

	// OutOfServiceTaintKey is the taint the reconciler applies to a fenced node once death is proven
	// (node.kubernetes.io/out-of-service=nodeshutdown:NoExecute), and removes again on CR deletion.
	OutOfServiceTaintKey = "node.kubernetes.io/out-of-service"

	// SBRCBlockTransientTestName is the SBRC name for the Ceph RBD transient network partition test.
	SBRCBlockTransientTestName = "test-sbrc-block-transient"

	// BlockPartitionDisruptorPodName is the self-terminating pod that blocks Ceph traffic on the
	// victim node and removes its own rules again when its outage window expires.
	BlockPartitionDisruptorPodName = "sbr-rbd-partition-disruptor"

	// BlockPartitionFlusherPodName is the teardown pod that removes any Ceph REJECT rule the
	// disruptor left behind.
	BlockPartitionFlusherPodName = "sbr-rbd-partition-flusher"

	// BlockPartitionProbePodName is the precondition pod that proves iptables is usable in the
	// victim node's host namespaces before the test blocks anything.
	BlockPartitionProbePodName = "sbr-rbd-partition-probe"

	// BlockPartitionOutage is how long the disruptor keeps Ceph traffic blocked on the victim node.
	// With the SBRC defaults (sbrTimeoutSeconds 30, so a heartbeat interval of 15s) a peer needs
	// maxConsecutiveFailures x 15s of silence before it sets SBRStorageUnhealthy=True, and the agent
	// downgrades that condition to Unknown (reason GivingAgentChance) once it has been True for
	// (maxConsecutiveFailures+1) x 15s + 5s. 150s sits between the two, so the test observes
	// True and then Recovered rather than Unknown.
	BlockPartitionOutage = 150 * time.Second

	// BlockPartitionPodTimeout bounds the short-lived host-command pods: the iptables probe and the
	// teardown flush. Both run a handful of iptables calls and exit.
	BlockPartitionPodTimeout = 3 * time.Minute

	// BlockPartitionCompletionTimeout is how long to wait for the disruptor pod to serve out its
	// outage window and exit. Sized well above BlockPartitionOutage so a busy node does not fail it.
	BlockPartitionCompletionTimeout = BlockPartitionOutage + 3*time.Minute

	// BlockPartitionRecoveryTimeout is how long the victim gets to report storage healthy again once
	// Ceph traffic is restored.
	BlockPartitionRecoveryTimeout = 5 * time.Minute

	// BlockHeartbeatResumeTimeout is how long to wait for the victim's heartbeat slot to start
	// advancing again after the partition clears.
	BlockHeartbeatResumeTimeout = 2 * time.Minute

	// BlockNoFencingCheckDuration is how long the victim is observed after recovery to prove it was
	// never fenced: same boot-id, still Ready, and no StorageBasedRemediation CR.
	BlockNoFencingCheckDuration = 90 * time.Second

	// BlockNoFencingCheckInterval is the poll interval for the post-recovery no-fencing check.
	BlockNoFencingCheckInterval = 10 * time.Second

	// SelfFencedMetricName is the agent counter incremented every time an agent self-fences.
	SelfFencedMetricName = "sbr_self_fenced_total"

	// SBRStorageUnhealthyReasonHeartbeatTimeout is the reason a peer agent gives when it sets
	// SBRStorageUnhealthy=True because a node's heartbeat slot went stale.
	SBRStorageUnhealthyReasonHeartbeatTimeout = "HeartbeatTimeout"

	// SBRStorageUnhealthyReasonRecovered is the reason a peer agent gives when it clears
	// SBRStorageUnhealthy because the node's heartbeats resumed.
	SBRStorageUnhealthyReasonRecovered = "Recovered"

	// DeviceIOErrorsMetricName is the agent counter incremented on every failed read or write of
	// the shared device.
	DeviceIOErrorsMetricName = "sbr_device_io_errors_total"

	// SBRCBlockLoadTestName is the SBRC name for the Ceph RBD heartbeat-under-load test.
	SBRCBlockLoadTestName = "test-sbrc-block-load"

	// BlockStressPodPrefix is the name prefix for the CPU stress pods, one per agent node. The node
	// name is appended, which keeps the names unique and readable in a failure dump.
	BlockStressPodPrefix = "sbr-rbd-stress-"

	// BlockLoadStressDuration is how long a stress pod saturates its node before exiting. The pods
	// are deleted in DeferCleanup; this bounds the load if the test dies without cleaning up, and
	// must comfortably outlast BlockLoadCheckDuration and the assertions that follow it.
	BlockLoadStressDuration = 5 * time.Minute

	// BlockLoadCheckDuration is how long every agent node is watched under CPU load to prove no
	// heartbeat is delayed enough to be read as storage loss.
	BlockLoadCheckDuration = 3 * time.Minute

	// BlockLoadCheckInterval is the poll interval for the under-load condition check.
	BlockLoadCheckInterval = 10 * time.Second

	// SBRCBlockInspectionTestName is the SBRC name for the Ceph RBD device inspection test.
	SBRCBlockInspectionTestName = "test-sbrc-block-inspection"

	// DefaultSBRTimeoutSeconds mirrors api/v1alpha1.DefaultSBRTimeoutSeconds: what an SBRC runs
	// with when sbrTimeoutSeconds is unset. The agent writes a heartbeat every half of it.
	DefaultSBRTimeoutSeconds = 30

	// DefaultHeartbeatInterval is the agent's write cadence at the default sbrTimeoutSeconds,
	// matching StorageBasedRemediationConfigSpec.GetHeartbeatInterval.
	DefaultHeartbeatInterval = DefaultSBRTimeoutSeconds * time.Second / 2

	// BlockHeartbeatSlotMaxAge is how far behind the newest heartbeat on the device a node's own
	// slot may be before it counts as stale: two write cycles, so a single missed write is
	// tolerated and a stopped writer is not.
	BlockHeartbeatSlotMaxAge = 2 * DefaultHeartbeatInterval

	// BlockNodeMapReadAttempts is how many times a node map read is retried before giving up. The
	// agents rewrite both 64 KB buffers continuously, and a read that lands mid-write fails the
	// buffer's checksum, so a first attempt failing says nothing about the device.
	BlockNodeMapReadAttempts = 5

	// SBRCBlockSplitBrainTestName is the SBRC name for the Ceph RBD block-mode split-brain test (RHWA-1934).
	SBRCBlockSplitBrainTestName = "test-sbrc-block-split-brain"

	// NHCBlockSplitBrainTestName is the NodeHealthCheck CR name for the block-mode split-brain test (RHWA-1934).
	NHCBlockSplitBrainTestName = "nhc-sbr-block-split-brain"

	// BlockSplitBrainInjectorPodName is the privileged pod that blocks Ceph traffic on the victim node
	// for the whole split-brain scenario, so the one storage-isolated node is fenced while its peers
	// keep their device access.
	BlockSplitBrainInjectorPodName = "sbr-rbd-split-brain-injector"

	// MinWorkersForBlockSplitBrain is the number of schedulable worker nodes the split-brain test
	// needs: one victim (which must not host an SBR controller pod) plus at least two witnesses, so a
	// single spurious fence stands out against more than one untouched peer.
	MinWorkersForBlockSplitBrain = 3

	// SBRCBlockStorageLossTestName is the SBRC name for the Ceph RBD block-mode total storage I/O
	// loss test (RHWA-1944).
	SBRCBlockStorageLossTestName = "test-sbrc-block-storage-loss"

	// NHCBlockStorageLossTestName is the NodeHealthCheck CR name for the block-mode total storage
	// I/O loss test (RHWA-1944).
	NHCBlockStorageLossTestName = "nhc-sbr-block-storage-loss"

	// BlockStorageLossInjectorPodName is the privileged pod that blocks Ceph traffic in both
	// directions on the victim node, with no time limit, so the victim cannot reach the shared
	// device at all and only its watchdog can reboot it.
	BlockStorageLossInjectorPodName = "sbr-rbd-storage-loss-injector"

	// SBRCBlockDetectOnlyTestName is the SBRC name for the Ceph RBD block-mode detection-only test.
	SBRCBlockDetectOnlyTestName = "test-sbrc-block-detect-only"

	// NHCBlockDetectOnlyTestName is the NodeHealthCheck CR name for the block-mode detection-only
	// test. It is created only for the watchdog-path half of the test: the fence-read half runs with
	// no NHC at all, so the StorageBasedRemediation CR the test writes itself is the only one.
	NHCBlockDetectOnlyTestName = "nhc-sbr-block-detect-only"

	// BlockDetectOnlyInjectorPodName is the privileged pod that blocks Ceph traffic in both
	// directions on the victim node of the detection-only test, with no time limit, so the victim
	// loses the shared device entirely and only its watchdog could still reboot it.
	BlockDetectOnlyInjectorPodName = "sbr-rbd-detect-only-injector"

	// DetectOnlyModeEnabled is the spec.detectOnlyMode value that puts an SBRC in detection-only
	// mode. The field is an enum (Disabled | Enabled), not a bool.
	DetectOnlyModeEnabled = "Enabled"

	// SBRUnhealthyDetectOnlyReason is the event an agent emits, instead of stopping to pet its
	// watchdog, when its own SBR storage is unhealthy while detectOnlyMode is Enabled.
	SBRUnhealthyDetectOnlyReason = "SBRUnhealthyDetectOnly"

	// DetectOnlyWithholdReason is the Warning a peer's remediation reconciler emits, against the
	// StorageBasedRemediation CR, when it refuses to cordon or fence the node that CR names because
	// detectOnlyMode is Enabled. It is the positive proof that the peer reached the decision point
	// and withheld, rather than never having seen the CR at all.
	DetectOnlyWithholdReason = "DetectOnlyMode"

	// SBRRemediationKind is the kind of the StorageBasedRemediation CR, as it appears on the Event
	// objects the remediation reconciler emits about it.
	SBRRemediationKind = "StorageBasedRemediation"

	// BlockDetectOnlyWithholdTimeout is how long to wait for a peer to report that it withheld
	// remediation for a node whose StorageBasedRemediation CR exists while detectOnlyMode is
	// Enabled. The peers reconcile the CR within seconds of its creation, so this is generous.
	BlockDetectOnlyWithholdTimeout = 3 * time.Minute

	// BlockDetectOnlyWatchdogFireAllowance is the time a disarmed watchdog would have needed to
	// fire: the agent's own grace before it stops petting plus the device's timeout. SBR does not
	// expose either, so this is a loose upper bound rather than a measured value.
	BlockDetectOnlyWatchdogFireAllowance = 2 * time.Minute

	// BlockDetectOnlyRebootAllowance is the time a bare-metal node needs to go all the way down and
	// report back in. It is the second half of the no-reboot window: a reboot that started but has
	// not finished still shows as NotReady, which the window asserts on directly.
	BlockDetectOnlyRebootAllowance = 8 * time.Minute

	// BlockDetectOnlyNoRebootWindow is how long a storage-isolated victim must stay up and Ready to
	// prove detect-only mode never let anything reboot it. It is counted from the moment its
	// remediation CR appears, because that is when both reboot routes open, and it covers the
	// watchdog firing plus a full reboot so a slow node cannot outlast it.
	BlockDetectOnlyNoRebootWindow = BlockDetectOnlyWatchdogFireAllowance + BlockDetectOnlyRebootAllowance

	// BlockDetectOnlyNoRebootMinWindow is the shortest no-reboot window the test will settle for
	// when the steps between the CR appearing and the window opening have already eaten most of it.
	// Without a floor a slow event lookup could shrink the check to nothing and pass for free.
	BlockDetectOnlyNoRebootMinWindow = 2 * time.Minute

	// BlockDetectOnlyNoRebootInterval is the poll interval for the no-reboot window.
	BlockDetectOnlyNoRebootInterval = 10 * time.Second

	// SBRCBlockLifecycleTestNameA is the first SBRC name for the Block-mode SBRC lifecycle test.
	SBRCBlockLifecycleTestNameA = "test-sbrc-block-lifecycle-a"

	// SBRCBlockLifecycleTestNameB is the second SBRC name for the Block-mode SBRC lifecycle test.
	// It is created only after the first one is deleted: every agent holds /dev/watchdog, so two
	// agent DaemonSets on the same nodes would crash-loop.
	SBRCBlockLifecycleTestNameB = "test-sbrc-block-lifecycle-b"

	// SBRCBlockNoStorageClassTestName is the SBRC name used for the negative admission check that
	// Block mode without sharedStorageClass is rejected. It is never expected to be admitted.
	SBRCBlockNoStorageClassTestName = "test-sbrc-block-no-storage-class"

	// BlockModeHostDevMountPath is where the agent container sees the host watchdog's parent
	// directory in Block mode. A bind mount at /dev would make the OCI runtime skip creating the
	// CSI-mapped block device node, so the host /dev is never mounted at /dev in Block mode.
	BlockModeHostDevMountPath = "/host-dev"

	// DefaultWatchdogPath is the watchdog device an SBRC uses when watchdogPath is unset. In Block
	// mode the agent is pointed at BlockModeHostDevMountPath + its basename instead.
	DefaultWatchdogPath = "/dev/watchdog"

	// AgentContainerName is the name of the sbr-agent container in the agent DaemonSet pod.
	AgentContainerName = "sbr-agent"

	// SharedStorageVolumeName is the name of the DaemonSet volume backed by the shared-storage PVC.
	// In Block mode it is attached through volumeDevices; in Filesystem mode through volumeMounts.
	SharedStorageVolumeName = "shared-storage"

	// AgentSBRDeviceFlag is the agent flag carrying the shared device path.
	AgentSBRDeviceFlag = "--sbr-device"

	// AgentFileLockingFlag is the agent flag toggling flock-based node map locking. Block mode
	// always passes false: a raw device bypasses the cluster lock manager, so flock would degrade
	// to host-local semantics.
	AgentFileLockingFlag = "--sbr-file-locking"

	// AgentWatchdogPathFlag is the agent flag carrying the in-container watchdog device path.
	AgentWatchdogPathFlag = "--watchdog-path"

	// SBRCKind is the kind of the StorageBasedRemediationConfig CR, as it appears on the Event
	// objects the controller emits about it.
	SBRCKind = "StorageBasedRemediationConfig"

	// SBRCResourcePlural is the API resource name for StorageBasedRemediationConfig, used to find
	// the validating admission webhook that covers it.
	SBRCResourcePlural = "storagebasedremediationconfigs"

	// SBRCValidationErrorReason is the Warning event the controller emits when an admitted SBRC
	// fails spec validation at reconcile time. It does not requeue: the user must fix the config.
	SBRCValidationErrorReason = "ValidationError"

	// SBRCPVCGCTimeout is how long to wait for an SBRC's shared-storage PVC to be garbage-collected
	// after the SBRC is deleted. The operator patches the PV reclaim policy to Delete before
	// dropping its finalizer, so the PVC goes with the SBRC.
	SBRCPVCGCTimeout = 5 * time.Minute

	// SBRCleanupJobComponentLabelKey is the component label key the operator stamps on the
	// resources it creates for an SBRC.
	SBRCleanupJobComponentLabelKey = "app.kubernetes.io/component"

	// SBRCleanupJobComponentLabelValue is the component label value identifying the node map
	// cleanup Job (RHWA-1056) the operator runs while deleting a Filesystem-mode SBRC. The Job uses
	// a GenerateName, so its name carries a random suffix and the labels are what to select on.
	SBRCleanupJobComponentLabelValue = "sbr-cleanup"

	// SBRCConfigLabelKey is the label the operator stamps on the resources it creates for an SBRC,
	// carrying the SBRC's own name.
	SBRCConfigLabelKey = "sbrconfig"

	// CleanupJobWatchInterval is how often the operator namespace is polled for a node map cleanup
	// Job while an SBRC is being deleted. Tighter than the suite's usual poll interval: the Job is
	// created and finishes inside the finalizer drain, so a slow poll could step over it.
	CleanupJobWatchInterval = 1 * time.Second

	// SBRCCleanupJobCheckDuration is how long the operator namespace is watched after a Block-mode
	// SBRC is deleted to prove no node map cleanup Job was created. Block mode has no
	// filesystem-level node map file to wipe, so the operator skips that Job entirely.
	SBRCCleanupJobCheckDuration = 60 * time.Second

	// SBRCCleanupJobCheckInterval is the poll interval for the cleanup-Job absence check.
	SBRCCleanupJobCheckInterval = 5 * time.Second

	// BlockInspectionMaxSkew bounds how old the newest heartbeat on the device may look from the
	// machine running the test. It is deliberately loose: its job is to catch a device nobody is
	// writing to any more, not to measure cadence, which BlockHeartbeatSlotMaxAge does on
	// device-written timestamps alone and so without any dependency on the runner's clock.
	BlockInspectionMaxSkew = 5 * time.Minute
)

// AgentExpectedMetricNames lists the Prometheus metric names that must be present in the agent output.
// These are SBR operator contract and are used by the metrics test suite (OCP-89202).
var AgentExpectedMetricNames = []string{
	"sbr_agent_status_healthy",
	"sbr_device_io_errors_total",
	"sbr_watchdog_pets_total",
	"sbr_peer_status",
	"sbr_self_fenced_total",
}

// SBRStorageClass is the StorageClass name to use when creating SBRCs that require shared storage.
// Set SBR_STORAGE_CLASS env var to override auto-discovery (useful in air-gapped or non-ODF environments).
// When empty, tests auto-discover a CephFS StorageClass (provisioner containing "cephfs").
var SBRStorageClass = os.Getenv("SBR_STORAGE_CLASS")

// SBRRBDStorageClass is the StorageClass name to use for Ceph RBD block storage tests.
// Set SBR_RBD_STORAGE_CLASS env var to override auto-discovery.
// When empty, tests auto-discover a Ceph RBD StorageClass (provisioner containing "rbd.csi.ceph.com").
var SBRRBDStorageClass = os.Getenv("SBR_RBD_STORAGE_CLASS")

// WatchdogDebugImage is the container image for /dev/watchdog* discovery and keepalive pods.
// Must provide sh and ls. Set SBR_WATCHDOG_DEBUG_IMAGE to override (e.g. in disconnected clusters).
var WatchdogDebugImage = func() string {
	if img := os.Getenv("SBR_WATCHDOG_DEBUG_IMAGE"); img != "" {
		return img
	}

	return medik8sparams.WorkloadImage
}()

// InjectorImage is the container image for privileged iptables-injection pods used in SBR disruptive tests.
// Must provide nsenter and iptables; ubi-minimal lacks both.
// Set SBR_INJECTOR_IMAGE to override (required in disconnected clusters — mirror ubi9/ubi and point here).
// In Prow CI this is written to SHARED_DIR/injector_image by the medik8s-lib step.
var InjectorImage = func() string {
	if img := os.Getenv("SBR_INJECTOR_IMAGE"); img != "" {
		return img
	}

	return "registry.access.redhat.com/ubi9/ubi:latest"
}()
