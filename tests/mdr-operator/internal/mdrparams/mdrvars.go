package mdrparams

import (
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/openshift-kni/k8sreporter"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
)

var (
	// Labels represents the range of labels that can be used for test cases selection.
	Labels = []string{medik8sparams.Label, Label}

	// OperatorDeploymentName represents MDR deployment name.
	OperatorDeploymentName = "machine-deletion-remediation-controller-manager"

	// OperatorControllerPodLabelSelector selects MDR controller-manager pods by label.
	OperatorControllerPodLabelSelector = "control-plane=controller-manager"

	// MDRCRDNames lists the full CRD names (plural.group) for all MDR custom resource definitions.
	MDRCRDNames = []string{
		"machinedeletionremediations." + CRDGroup,
		"machinedeletionremediationtemplates." + CRDGroup,
	}

	// ReporterNamespacesToDump tells the reporter from where to collect logs.
	ReporterNamespacesToDump = map[string]string{
		medik8sparams.OperatorNs: medik8sparams.OperatorNs,
		MachineAPINamespace:      MachineAPINamespace,
	}

	operatorNs = medik8sparams.OperatorNs

	// ReporterCRDsToDump tells the reporter what CRs to dump.
	ReporterCRDsToDump = []k8sreporter.CRData{
		{Cr: &corev1.PodList{}},
		{Cr: medik8sparams.NewUnstructuredList(CRDGroup, CRDVersion, "MachineDeletionRemediationList")},
		{Cr: medik8sparams.NewUnstructuredList(CRDGroup, CRDVersion, "MachineDeletionRemediationTemplateList")},
		{Cr: medik8sparams.NewUnstructuredList(NHCAPIGroup, NHCAPIVersion, "NodeHealthCheckList")},
		{Cr: &coordinationv1.LeaseList{}, Namespace: &operatorNs},
	}
)
