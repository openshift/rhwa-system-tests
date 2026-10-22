package nhcparams

import (
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/openshift-kni/k8sreporter"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
)

var (
	// Labels represents the range of labels that can be used for test cases selection.
	Labels = []string{medik8sparams.Label, Label}

	// OperatorDeploymentName represents NHC deployment name.
	OperatorDeploymentName = "node-healthcheck-controller-manager"

	// OperatorControllerPodLabelSelector selects NHC controller-manager pods.
	OperatorControllerPodLabelSelector = "app.kubernetes.io/component=controller-manager," +
		"app.kubernetes.io/name=node-healthcheck-operator"

	// SNRControllerPodLabelSelector selects SNR controller-manager pods. The NHC
	// must-gather test excludes the nodes hosting them when it picks a worker to
	// break, so that remediation is not taken down with the node under test.
	// Declared here rather than imported from snrparams: no suite imports another
	// suite's params package, and nhcparams already declares the SNR facts the NHC
	// specs need (SNRCRDGroup, SNRCRDName, SNRTemplateName, SNRDaemonSetName).
	SNRControllerPodLabelSelector = "control-plane=controller-manager,self-node-remediation-operator="

	// NHCCRDNames lists the full CRD names (plural.group) for all NHC custom
	// resource definitions. Hand-maintained; NodeHealthCheck is the operator's
	// only CRD. Mirrors the shape of snrparams.SNRCRDNames.
	NHCCRDNames = []string{
		"nodehealthchecks." + CRDGroup,
	}

	// WorkerSelectingNHCNames lists every NHC CR name in this suite whose
	// selector targets worker nodes. NHC remediates only one CR at a time, so a
	// spec that needs its own CR to reach Remediating must first clear any of
	// these left behind by an interrupted sibling spec.
	WorkerSelectingNHCNames = []string{
		NHCMustGatherTestName,
		NHCOldDefaultName,
		NHCTestName,
		NHCSecondTestName,
		NHCStatusTestName,
	}

	// ReporterNamespacesToDump tells the reporter from where to collect logs.
	ReporterNamespacesToDump = map[string]string{
		medik8sparams.OperatorNs: medik8sparams.OperatorNs,
	}

	operatorNs = medik8sparams.OperatorNs

	// ReporterCRDsToDump tells the reporter what CRs to dump.
	ReporterCRDsToDump = []k8sreporter.CRData{
		{Cr: &corev1.PodList{}},
		{Cr: &coordinationv1.LeaseList{}, Namespace: &operatorNs},
		{Cr: medik8sparams.NewUnstructuredList(CRDGroup, CRDVersion, "NodeHealthCheckList")},
	}

	// RequiredAnnotations defines the required annotations and expected values for NHC CSV.
	RequiredAnnotations = map[string]string{
		"features.operators.openshift.io/tls-profiles":     "false",
		"features.operators.openshift.io/disconnected":     "true",
		"features.operators.openshift.io/fips-compliant":   "true",
		"features.operators.openshift.io/proxy-aware":      "false",
		"features.operators.openshift.io/cnf":              "false",
		"features.operators.openshift.io/cni":              "false",
		"features.operators.openshift.io/csi":              "false",
		"features.operators.openshift.io/token-auth-aws":   "false",
		"features.operators.openshift.io/token-auth-azure": "false",
		"features.operators.openshift.io/token-auth-gcp":   "false",
		"operatorframework.io/suggested-namespace":         medik8sparams.OperatorNs,
	}
)
