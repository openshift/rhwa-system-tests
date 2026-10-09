package farparams

import (
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/openshift-kni/k8sreporter"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
)

var (
	// Labels represents the range of labels that can be used for test cases selection.
	Labels = []string{medik8sparams.Label, Label}

	// OperatorDeploymentName represents FAR deployment name.
	OperatorDeploymentName = "fence-agents-remediation-controller-manager"

	// OperatorControllerPodLabel is how the controller pod is labeled.
	OperatorControllerPodLabel = "fence-agents-remediation-operator"

	// OperatorControllerPodLabelSelector selects FAR controller-manager pods by label.
	OperatorControllerPodLabelSelector = ControllerPodLabelKey + "=" + OperatorControllerPodLabel

	// OperatorControllerPodLabels is the label set for selecting FAR controller-manager pods.
	OperatorControllerPodLabels = map[string]string{
		ControllerPodLabelKey: OperatorControllerPodLabel,
		"control-plane":       "controller-manager",
	}

	// ReporterNamespacesToDump tells to the reporter from where to collect logs.
	ReporterNamespacesToDump = map[string]string{
		medik8sparams.OperatorNs: medik8sparams.OperatorNs,
		"openshift-machine-api":  "openshift-machine-api",
	}

	operatorNs = medik8sparams.OperatorNs

	// ReporterCRDsToDump tells to the reporter what CRs to dump.
	ReporterCRDsToDump = []k8sreporter.CRData{
		{Cr: &corev1.PodList{}},
		{Cr: medik8sparams.NewUnstructuredList("fence-agents-remediation.medik8s.io", "v1alpha1",
			"FenceAgentsRemediationList")},
		{Cr: medik8sparams.NewUnstructuredList("fence-agents-remediation.medik8s.io", "v1alpha1",
			"FenceAgentsRemediationTemplateList")},
		{Cr: &coordinationv1.LeaseList{}, Namespace: &operatorNs},
	}

	// MinExpectedFenceAgents is a stable subset of fence agent binaries that must exist
	// in every FAR controller container image, regardless of version.
	MinExpectedFenceAgents = []string{
		"fence_aws",
		"fence_azure_arm",
		"fence_gce",
		"fence_ipmilan",
		"fence_kubevirt",
		"fence_redfish",
	}
)
