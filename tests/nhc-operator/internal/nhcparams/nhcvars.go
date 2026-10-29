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

	// ConsolePluginPodLabelSelector selects NHC console plugin pods (RHWA-1943).
	// The pod labels are not affected by the kustomize namePrefix that renames
	// the Deployment, so the selector is the stable way to find the plugin.
	ConsolePluginPodLabelSelector = "app.kubernetes.io/component=node-remediation-console-plugin"

	// ConsolePluginContainerName is the container name inside the console plugin pod (RHWA-1943).
	ConsolePluginContainerName = "node-remediation-console-plugin"

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
)
