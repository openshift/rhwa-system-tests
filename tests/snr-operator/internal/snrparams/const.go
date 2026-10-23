package snrparams

import (
	"time"

	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
)

const (
	// Label is the operator name used in the suite-level Labels array.
	Label = "snr"
	// DefaultPollInterval is the polling interval used with Eventually calls.
	DefaultPollInterval = 5 * time.Second

	// ExpectedReplicas defines the expected number of replicas for SNR controller manager.
	ExpectedReplicas = int32(2)

	// ManagerContainerName is the name of the main controller container in the SNR pod.
	ManagerContainerName = "manager"

	// CRDGroup is the Kubernetes API group for all SNR custom resources.
	CRDGroup = "self-node-remediation.medik8s.io"

	// CRDVersion is the API version for all SNR custom resources.
	CRDVersion = "v1alpha1"

	// SNRConfigName is the name of the default SelfNodeRemediationConfig CR.
	SNRConfigName = "self-node-remediation-config"

	// SNRTemplateName is the name of the default automatic strategy template.
	SNRTemplateName = "self-node-remediation-automatic-strategy-template"

	// SNRTemplateExpectedStrategy is the expected remediation strategy for the automatic template.
	SNRTemplateExpectedStrategy = "Automatic"

	// CSVNamePattern is the CSV name pattern used to find the SNR ClusterServiceVersion.
	CSVNamePattern = "self-node-remediation"

	// SNRCRDName is the full CRD name for SelfNodeRemediationConfig.
	SNRCRDName = "selfnoderemediationconfigs.self-node-remediation.medik8s.io"

	// SafeTimeToAssumeNodeRebootedDescription is the expected description
	// text substring for the safeTimeToAssumeNodeRebootedSeconds CRD field.
	SafeTimeToAssumeNodeRebootedDescription = "SafeTimeToAssumeNodeRebootedSeconds " +
		"is the time after which the healthy self node remediation"

	// SNRCNonDefaultErrMsg is the expected error when creating a non-default SNRC.
	SNRCNonDefaultErrMsg = "to enforce only one SelfNodeRemediationConfig " +
		"in the cluster, a name other than " +
		"self-node-remediation-config is not allowed"

	// SNRTestNodeName is the fake node name used in negative tests.
	SNRTestNodeName = "incorrect-node-name"

	// SNRCTestName is the name used for test SNRC CRs.
	SNRCTestName = "test-snrc"

	// DSPodRestartTimeout is the time allowed for SNR DaemonSet pods to
	// restart or terminate after a config change.
	DSPodRestartTimeout = 15 * time.Minute

	// SoftdogAutoDetectMessage is the log message emitted by the SNR agent
	// when it auto-detects the softdog watchdog path after an invalid path is configured.
	SoftdogAutoDetectMessage = "auto detected softdog path"

	// SNRReasonConfigNotFound is the status condition reason when SNRC is missing.
	SNRReasonConfigNotFound = "ConfigurationNotFound"

	// SNRMessageConfigNotFound is the status condition message when SNRC is missing.
	SNRMessageConfigNotFound = "SelfNodeRemediation is disabled because configuration does not exist"

	// --- Destructive (remediation) test constants ---.

	// OcDebugTimeout is the timeout for oc debug node/ commands.
	// 5 minutes to allow for slow debug pod scheduling on ARM64/nested virt.
	OcDebugTimeout = 5 * time.Minute

	// SNRDeletionTimeout is how long to wait for the SNR CR to be deleted
	// after remediation completes. This is the longest wait in the
	// remediation cycle -- the Python tests use 800s.
	SNRDeletionTimeout = 15 * time.Minute

	// NodeReadyTimeout is how long to wait for a node to return to Ready
	// after reboot.
	NodeReadyTimeout = 15 * time.Minute

	// RemediationCRDeletionTimeout is how long to wait for a CR to be
	// fully deleted during cleanup (retry-safe deletion). NHC CRs can
	// take several minutes to delete due to webhook finalizer processing.
	RemediationCRDeletionTimeout = 5 * time.Minute

	// WorkloadPodReadyTimeout is how long to wait for a test workload pod
	// to reach Running phase.
	WorkloadPodReadyTimeout = 2 * time.Minute

	// WorkloadEvictionTimeout is how long to wait for workload pods to be
	// evicted or rescheduled after remediation.
	WorkloadEvictionTimeout = 5 * time.Minute

	// NHCCRDName is the CRD name for NodeHealthCheck, used to detect if
	// NHC is installed.
	NHCCRDName = "nodehealthchecks.remediation.medik8s.io"

	// NHCAPIGroup is the API group for NodeHealthCheck CRs.
	NHCAPIGroup = "remediation.medik8s.io"

	// NHCAPIVersion is the API version for NodeHealthCheck CRs.
	NHCAPIVersion = "v1alpha1"

	// NHCTestName is the name used for test NHC CRs targeting workers.
	NHCTestName = "snr-test-nhc-workers"

	// NHCMasterTestName is the name used for test NHC CRs targeting masters.
	NHCMasterTestName = "snr-test-nhc-masters"

	// SNRTResourceDeletionName is the name for the ResourceDeletion strategy SNRT.
	SNRTResourceDeletionName = "snr-test-resource-deletion-template"

	// SNRTOutOfServiceTaintName is the name for the OutOfServiceTaint strategy SNRT.
	SNRTOutOfServiceTaintName = "snr-test-out-of-service-taint-template"

	// OutOfServiceTaintKey is the taint key applied by SNR when using the
	// OutOfServiceTaint remediation strategy (standard K8s taint).
	OutOfServiceTaintKey = "node.kubernetes.io/out-of-service"

	// MinReadyMasterNodes is the minimum number of Ready master nodes
	// required for master remediation tests (etcd quorum safety).
	MinReadyMasterNodes = 3

	// --- preferredAddressTypes (RHWA-1928) constants ---.

	// AgentDaemonSetName is the name of the SNR agent DaemonSet the
	// SelfNodeRemediationConfig controller reconciles.
	AgentDaemonSetName = "self-node-remediation-ds"

	// AgentNetworkPolicyName is the NetworkPolicy the SNRC controller
	// reconciles alongside the agent DaemonSet. Its egress allowance for
	// KubeletSecurePort is what lets the control-plane self-diagnostics
	// reach the kubelet at all.
	AgentNetworkPolicyName = "self-node-remediation-ds-np"

	// PreferredAddressTypesEnvVar is the agent container environment
	// variable rendered from SelfNodeRemediationConfig.spec.preferredAddressTypes.
	// An empty value means the agent falls back to ["NodeName"].
	PreferredAddressTypesEnvVar = "PREFERRED_ADDRESS_TYPES"

	// PreferredAddressTypesInvalid is a value outside the CRD enum, used to
	// prove the API server rejects anything undocumented.
	PreferredAddressTypesInvalid = "NotAnAddressType"

	// KubeletSecurePort is the port the control-plane self-diagnostics probe.
	// Named "secure" and not "read-only": 10250 is the kubelet's
	// authenticated HTTPS port, 10255 is the read-only one. A string
	// because it is passed to net.JoinHostPort.
	KubeletSecurePort = "10250"

	// KubeletDownMessage is the agent log line emitted when the
	// self-diagnostics kubelet check fails for an address.
	KubeletDownMessage = "kubelet service is down"

	// KubeletProbeTimeout bounds the in-cluster curl probe of the kubelet
	// port. Matches the 10s http.Client timeout the agent itself uses.
	KubeletProbeTimeout = 10 * time.Second

	// KubeletProbePodTimeout is how long to wait for a kubelet probe pod to
	// be scheduled, pull its image and run to completion.
	KubeletProbePodTimeout = 5 * time.Minute

	// PreferredAddressNoRemediationDuration is the window over which no
	// control-plane node may be remediated after the agent DaemonSet rolls.
	//
	// Four minutes, set from measurement rather than derivation. SNR's own
	// timings suggest a false self-remediation should fire ~90s after the
	// agents restart (maxApiErrorThreshold 3 x apiCheckInterval 15s = 45s to
	// give up on the API, plus maxTimeForNoPeersResponse 30s). Reality is
	// slower and more variable -- two observed reboots fired at 79s and 169s
	// after the rollout completed, so the arithmetic understates it by up to
	// 2x and two samples are not enough to claim a bound. 240s leaves ~200s
	// for the node to go down after the ~40s node-monitor-grace-period
	// detection cost, against an observed worst case of 169s.
	//
	// Do not shorten this on the strength of the arithmetic alone; a 3 minute
	// window cleared the 169s event by 11 seconds, which was luck. Detection
	// latency is what decides whether a reset is caught at all -- see the
	// Ready check in noControlPlaneRemediation, and note that boot IDs lag the
	// reset by ~3m40s and so cannot be the detector at any window length.
	PreferredAddressNoRemediationDuration = 4 * time.Minute
)

// WorkloadTestImage is the container image used for test workload pods.
var WorkloadTestImage = medik8sparams.WorkloadImage

// PreferredAddressTypesValid is the enum the SelfNodeRemediationConfig CRD
// must accept for spec.preferredAddressTypes, in the order the upstream
// kubebuilder marker declares it.
var PreferredAddressTypesValid = []string{
	"NodeName",
	"Hostname",
	"InternalDNS",
	"ExternalDNS",
	"InternalIP",
	"ExternalIP",
}

// PreferredAddressTypesConfigured is the list the test configures. InternalIP
// first because that is the address type the fix exists to make usable;
// NodeName second to prove the ordering survives rendering, since order is
// the agent's fallback precedence.
var PreferredAddressTypesConfigured = []string{"InternalIP", "NodeName"}
