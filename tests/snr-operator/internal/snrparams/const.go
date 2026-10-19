package snrparams

import (
	"os"
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

	// --- Control plane isolation (RHWA-1927) constants ---.

	// DefaultPeerHostPort is the SNR agent peer health port assumed when the
	// SelfNodeRemediationConfig does not set spec.hostPort explicitly.
	DefaultPeerHostPort = 30001

	// AgentProcessPattern matches the SNR agent process in the host PID
	// namespace. The controller runs the same binary with --is-manager=true,
	// so the flag is what distinguishes the agent.
	AgentProcessPattern = "manager --is-manager=false"

	// CPIsolationInjectorPodName is the privileged pod that adds the REJECT
	// rules inside the victim agent's network namespace.
	CPIsolationInjectorPodName = "snr-cp-isolation-injector"

	// CPIsolationFlusherPodName is the privileged pod that removes them again.
	CPIsolationFlusherPodName = "snr-cp-isolation-flusher"

	// CPIsolationProbePodName is the privileged pod that proves, before the
	// test starts, that the agent PID can be found and iptables is usable in
	// its network namespace.
	CPIsolationProbePodName = "snr-cp-isolation-probe"

	// CPIsolationPodTimeout is the budget for a one-shot injector, flush or
	// probe pod to pull its image and finish.
	CPIsolationPodTimeout = 3 * time.Minute

	// CPIsolationReadyGraceDuration is how long the victim node must stay
	// Ready after its agent is isolated. Three apiCheckInterval ticks: long
	// enough to prove /readyz is still passing, short enough to finish before
	// the agent decides to reboot.
	CPIsolationReadyGraceDuration = 45 * time.Second

	// CPIsolationDetectTimeout is how long the victim agent may take to log
	// the isolation decision. Derived from the SNRC defaults: three ticks to
	// cross maxApiErrorThreshold, each paying two rounds of peer dial
	// timeouts, then maxTimeForNoPeersResponse.
	CPIsolationDetectTimeout = 5 * time.Minute

	// CPIsolationRebootTimeout is how long the victim node may take to change
	// its boot ID after the isolation decision is logged.
	CPIsolationRebootTimeout = 10 * time.Minute

	// CPIsolationLogWindow is how far back the agent log is searched.
	CPIsolationLogWindow = 10 * time.Minute

	// CPIsolationDetectedMessage is logged by the agent when /readyz passed
	// but no control plane peer could be reached. This is the behaviour added
	// by self-node-remediation#339 and the thing this test exists to catch.
	//
	// It is logged on every tick once peers are unreachable, including ticks
	// where the agent then decides to wait for the error threshold, so on its
	// own it proves detection but not the decision to reboot. Pair it with
	// CPIsolationRebootTriggeredMessage.
	CPIsolationDetectedMessage = "CP node: readyz passed but no CP peers reachable"

	// PeerConsultationMessage is logged once errorCount crosses
	// maxApiErrorThreshold and the agent starts asking peers about itself.
	PeerConsultationMessage = "Error count exceeds threshold, trying to ask other nodes if I'm healthy"

	// CPIsolationRebootTriggeredMessage is logged at the moment the agent
	// calls Rebooter.Reboot() on the isolation path -- the observable form of
	// "the agent stops feeding the watchdog".
	CPIsolationRebootTriggeredMessage = "CP isolation detected despite passing readyz, triggering reboot"

	// APICheckFailureMessage is logged by the agent when /readyz itself fails.
	// Its absence before the partition is what proves the test exercises the
	// isolation gate rather than the pre-existing API-error branch.
	APICheckFailureMessage = "failed to check api server"

	// CPIsolationRebootSLA is the end-to-end budget from blocking the peer
	// port to the victim's boot ID changing, covering the behaviour the
	// customer specified in RHWA-1384: detect, consult peers, stop feeding the
	// watchdog, reboot.
	//
	// The customer stated ~2 minutes, which the SNRC defaults support: three
	// 15s apiCheckInterval ticks to cross maxApiErrorThreshold (~45s),
	// maxTimeForNoPeersResponse (30s) measured from the last successful peer
	// response, then the watchdog timeout. This constant carries headroom over
	// that so a slow cluster does not flake, while still failing if detection
	// regresses by an order of magnitude. The measured value is recorded with
	// AddReportEntry on every run.
	CPIsolationRebootSLA = 4 * time.Minute
)

// WorkloadTestImage is the container image used for test workload pods.
var WorkloadTestImage = medik8sparams.WorkloadImage

// InjectorImage is the container image for the privileged pods that inject
// iptables rules into the SNR agent's network namespace. It must provide
// nsenter, pgrep and iptables; ubi-minimal provides none of them.
//
// Set SNR_INJECTOR_IMAGE to override. SBR_INJECTOR_IMAGE is honoured as a
// fallback so a job that already mirrors an image for the SBR suite does not
// need a second variable.
var InjectorImage = func() string {
	if img := os.Getenv("SNR_INJECTOR_IMAGE"); img != "" {
		return img
	}

	if img := os.Getenv("SBR_INJECTOR_IMAGE"); img != "" {
		return img
	}

	return "registry.access.redhat.com/ubi9/ubi:latest"
}()
