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

	// MinPeersEnvVarName is the agent environment variable the controller
	// renders from spec.minPeersForRemediation of the SNRC.
	MinPeersEnvVarName = "MIN_PEERS_FOR_REMEDIATION"

	// MinPeersRemediateAlone is the minPeersForRemediation value that lets an
	// agent remediate without any peer confirming the diagnosis. The reboot
	// spec configures it so an isolated node reboots itself.
	MinPeersRemediateAlone = 0

	// MinWorkersForPeerCheck is the minimum number of Ready worker nodes the
	// minPeersForRemediation specs need: one worker to isolate and reboot,
	// a second to isolate without rebooting, and a peer for each of them --
	// a node with an empty peer list takes the isolated-node branch whatever
	// minPeersForRemediation says.
	MinWorkersForPeerCheck = 2

	// PeerListSettleTimeout is how long the agent pods must stay Running after
	// a DaemonSet rollout before a fault is injected, so that every agent has
	// a complete peer list and sees the peers that rolled after it.
	//
	// It is sized against the agent's *error* retry, not peerUpdateInterval:
	// that defaults to 15m, far too long to wait out. An agent that lists its
	// peers while a sibling pod is missing gets an error back from
	// mapNodesToPrimaryPodIPs along with the short list, and errors put it on
	// a quickPeerUpdateInterval loop of 2m until the list comes back clean.
	// Waiting past that, rather than past peerUpdateInterval, is what makes
	// the peer list trustworthy.
	PeerListSettleTimeout = 3 * time.Minute

	// IsolationHoldDuration is how long the api-server and peer agent
	// addresses stay blackholed inside the agent pod when the node is not
	// expected to reboot. It must exceed maxApiErrorThreshold *
	// apiCheckInterval (3 * 15s by default) so the agent reaches the
	// peer-check path.
	IsolationHoldDuration = 2 * time.Minute

	// IsolationRebootHoldDuration is the same window for the spec that does
	// expect a reboot. It must outlast the agent reaching its verdict plus
	// the watchdog timeout that follows, so the isolation is never lifted
	// before the node goes down. The exec ends early anyway, when the reboot
	// takes the agent pod with it.
	IsolationRebootHoldDuration = 5 * time.Minute

	// IsolationExecTimeout bounds the blocking exec that injects and then
	// removes the blackhole routes. It must exceed every hold duration above.
	IsolationExecTimeout = 8 * time.Minute

	// RebootSettleWindow is how long the node that must not reboot is watched
	// after its isolation window closes. The agent reaches its verdict partway
	// into that window and a watchdog reboot lands some way after it, so a
	// single reading at the end of the window could miss a late reboot.
	RebootSettleWindow = 1 * time.Minute

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

	// --- NetworkPolicy (RHWA-1645) constants ---.

	// NetworkPolicyName is the name of the NetworkPolicy the operator
	// renders and reconciles for the SNR agent DaemonSet pods.
	NetworkPolicyName = "self-node-remediation-ds-np"

	// NetworkPolicyLabelKey is the label key the operator stamps onto the
	// rendered NetworkPolicy.
	NetworkPolicyLabelKey = "k8s-app"
	// NetworkPolicyLabelValue is the label value the operator stamps onto the
	// rendered NetworkPolicy.
	NetworkPolicyLabelValue = "self-node-remediation"

	// NetworkPolicyStaticEgressRules is the number of static egress rules the
	// NetworkPolicy always carries (peer, DNS, HTTPS, own-node kubelet).
	NetworkPolicyStaticEgressRules = 4

	// DefaultHostPort is the kubebuilder default for SelfNodeRemediationConfig.
	DefaultHostPort = 30001

	// NetworkPolicyDNSPort is the DNS port allowed in the NetworkPolicy's
	// static DNS egress rule.
	NetworkPolicyDNSPort = int32(53)

	// NetworkPolicyDNSServicePort is the OVN-Kubernetes Service->pod DNAT
	// target port also allowed in the static DNS egress rule.
	NetworkPolicyDNSServicePort = int32(5353)

	// NetworkPolicyHTTPSPort is the HTTPS port allowed in the NetworkPolicy's
	// static HTTPS egress rule (reaching the Kubernetes API service).
	NetworkPolicyHTTPSPort = int32(443)

	// NetworkPolicyAPIServerPort is the Kubernetes API server port also
	// allowed in the static HTTPS egress rule.
	NetworkPolicyAPIServerPort = int32(6443)

	// NetworkPolicyKubeletPort is the port allowed in the NetworkPolicy's
	// static egress rule for the control-plane agent's own-node kubelet check.
	NetworkPolicyKubeletPort = int32(10250)

	// EndpointTestIPv4A is an RFC 5737 TEST-NET-3 literal used as a stand-in
	// EndpointHealthCheckUrl. IP literals resolve immediately via net.LookupIP
	// without an actual DNS lookup, keeping the test deterministic and
	// network-independent while still exercising the real resolution path.
	EndpointTestIPv4A = "203.0.113.5"

	// EndpointTestIPv4ACIDR is the /32 CIDR the controller renders for
	// EndpointTestIPv4A.
	EndpointTestIPv4ACIDR = EndpointTestIPv4A + "/32"

	// EndpointTestIPv4B is a second RFC 5737 TEST-NET-3 literal, used to
	// simulate EndpointHealthCheckUrl resolving to a different address
	// (a "DNS flip").
	EndpointTestIPv4B = "203.0.113.6"

	// EndpointTestIPv4BCIDR is the /32 CIDR the controller renders for
	// EndpointTestIPv4B.
	EndpointTestIPv4BCIDR = EndpointTestIPv4B + "/32"

	// NetworkPolicyRaceIterations is the number of rapid, alternating
	// EndpointHealthCheckUrl patches applied in the sole-writer stability
	// (race) step.
	NetworkPolicyRaceIterations = 6

	// NetworkPolicyStabilityWindow is how long the NetworkPolicy's resolved
	// ipBlock egress rule must remain unchanged after the rapid changes settle.
	NetworkPolicyStabilityWindow = 30 * time.Second
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

)

// DefaultCPIsolationRebootSLA is the default end-to-end budget from blocking the
// peer port to the victim's boot ID changing.
//
// RHWA-1384 specifies ~2 minutes for the detection the customer cares about:
// three 15s apiCheckInterval ticks to cross maxApiErrorThreshold (~45s),
// maxTimeForNoPeersResponse (30s) from the last peer response, then the watchdog
// timeout. The end-to-end measurement also includes the node reboot and kubelet
// rejoin, which is platform-dependent: on baremetal with a hardware watchdog it
// is quick, but on cloud VMs (softdog, full instance reboot, rejoin) it adds
// several minutes. The default carries headroom for the cloud case so the test
// does not flake there, while still failing if detection regresses by an order
// of magnitude.
const DefaultCPIsolationRebootSLA = 8 * time.Minute

// CPIsolationRebootSLA is the budget enforced by the control plane isolation
// test. Override with SNR_CP_ISOLATION_REBOOT_SLA (a Go duration string such as
// "4m") to tighten it on baremetal where the reboot itself is fast, or to adjust
// it per platform. Falls back to DefaultCPIsolationRebootSLA when unset or
// unparseable. The measured value is recorded with AddReportEntry on every run.
var CPIsolationRebootSLA = func() time.Duration {
	if v := os.Getenv("SNR_CP_ISOLATION_REBOOT_SLA"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}

	return DefaultCPIsolationRebootSLA
}()

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
var (
	PreferredAddressTypesConfigured = []string{"InternalIP", "NodeName"}
	// InjectorImage is the container image for the privileged pods that inject
	// iptables rules into the SNR agent's network namespace. It must provide all
	// three of nsenter, pgrep and iptables. Verified image coverage:
	//   - ubi-minimal:    none
	//   - ubi9/ubi:       nsenter only (no pgrep, no iptables)
	//   - rhel9/support-tools: pgrep + nsenter, but no iptables
	//   - nicolaka/netshoot: all three
	// So the ubi9/ubi fallback below does NOT satisfy the requirement and the
	// test will skip at the iptables-usability probe when it is used. Prow sets a
	// purpose-built image via medik8s-lib (SHARED_DIR/injector_image); for a local
	// run set SNR_INJECTOR_IMAGE to an image that carries all three tools.
	//
	// Set SNR_INJECTOR_IMAGE to override. SBR_INJECTOR_IMAGE is honoured as a
	// fallback so a job that already mirrors an image for the SBR suite does not
	// need a second variable.
	InjectorImage = func() string {
		if img := os.Getenv("SNR_INJECTOR_IMAGE"); img != "" {
			return img
		}

		if img := os.Getenv("SBR_INJECTOR_IMAGE"); img != "" {
			return img
		}

		return "registry.access.redhat.com/ubi9/ubi:latest"
	}()
)
