package sbrparams

import (
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/openshift-kni/k8sreporter"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
)

var (
	// Labels represents the range of labels that can be used for test cases selection.
	Labels = []string{medik8sparams.Label, Label}

	// ReporterNamespacesToDump tells the reporter from where to collect logs.
	ReporterNamespacesToDump = map[string]string{
		medik8sparams.OperatorNs: medik8sparams.OperatorNs,
		"openshift-machine-api":  "openshift-machine-api",
	}

	operatorNs = medik8sparams.OperatorNs

	// ReporterCRDsToDump tells the reporter what CRs to dump.
	ReporterCRDsToDump = []k8sreporter.CRData{
		{Cr: &corev1.PodList{}},
		{Cr: medik8sparams.NewUnstructuredList(CRDGroup, CRDVersion, "StorageBasedRemediationList")},
		{Cr: medik8sparams.NewUnstructuredList(CRDGroup, CRDVersion, "StorageBasedRemediationConfigList")},
		{Cr: medik8sparams.NewUnstructuredList(CRDGroup, CRDVersion, "StorageBasedRemediationTemplateList")},
		{Cr: &coordinationv1.LeaseList{}, Namespace: &operatorNs},
	}

	// ExpectedCRDKinds lists the Kubernetes kinds for all CRDs owned by the SBR operator.
	ExpectedCRDKinds = []string{
		"StorageBasedRemediation",
		"StorageBasedRemediationConfig",
		"StorageBasedRemediationTemplate",
	}

	// SBRCRDNames lists the full CRD names (plural.group) for all SBR custom resource definitions.
	SBRCRDNames = []string{
		"storagebasedremediationconfigs." + CRDGroup,
		"storagebasedremediations." + CRDGroup,
		"storagebasedremediationtemplates." + CRDGroup,
	}
)
