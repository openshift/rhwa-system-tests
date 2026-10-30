package collectionparams

import (
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/openshift-kni/k8sreporter"
	corev1 "k8s.io/api/core/v1"
)

var (
	// Labels represents the range of labels that can be used for test case selection.
	Labels = []string{medik8sparams.Label, Label}

	// RequiredCRDNames is the floor the cluster-derived inventory is checked
	// against: the CRDs every RHWA operator set installs. The expected result
	// is otherwise discovered from the cluster, which on its own would let a
	// cluster missing an operator shrink the expectation and pass vacuously.
	//
	// Adding a CRD to a future operator release does not need an entry here --
	// discovery picks it up. An entry is needed only to make an operator's
	// absence a failure rather than a smaller test.
	RequiredCRDNames = []string{
		"fenceagentsremediations." + FARGroup,
		"fenceagentsremediationtemplates." + FARGroup,
		"machinedeletionremediations." + MDRGroup,
		"machinedeletionremediationtemplates." + MDRGroup,
		"nodehealthchecks." + NHCGroup,
		"nodemaintenances." + NMOGroup,
		"selfnoderemediationconfigs." + SNRGroup,
		"selfnoderemediations." + SNRGroup,
		"selfnoderemediationtemplates." + SNRGroup,
		"storagebasedremediationconfigs." + SBRGroup,
		"storagebasedremediations." + SBRGroup,
		"storagebasedremediationtemplates." + SBRGroup,
	}

	// ReporterNamespacesToDump tells the reporter from where to collect logs.
	ReporterNamespacesToDump = map[string]string{
		medik8sparams.OperatorNs: medik8sparams.OperatorNs,
		"openshift-machine-api":  "openshift-machine-api",
	}

	// ReporterCRDsToDump tells the reporter what CRs to dump. It covers every
	// medik8s CRD, since a failure here is usually about which CR was or was
	// not present when the collection ran.
	ReporterCRDsToDump = []k8sreporter.CRData{
		{Cr: &corev1.PodList{}},
		{Cr: medik8sparams.NewUnstructuredList(NHCGroup, NHCVersion, "NodeHealthCheckList")},
		{Cr: medik8sparams.NewUnstructuredList(NMOGroup, NMOVersion, "NodeMaintenanceList")},
		{Cr: medik8sparams.NewUnstructuredList(SNRGroup, SNRVersion, "SelfNodeRemediationList")},
		{Cr: medik8sparams.NewUnstructuredList(SNRGroup, SNRVersion, "SelfNodeRemediationConfigList")},
		{Cr: medik8sparams.NewUnstructuredList(SNRGroup, SNRVersion, "SelfNodeRemediationTemplateList")},
		{Cr: medik8sparams.NewUnstructuredList(FARGroup, FARVersion, "FenceAgentsRemediationList")},
		{Cr: medik8sparams.NewUnstructuredList(FARGroup, FARVersion, "FenceAgentsRemediationTemplateList")},
		{Cr: medik8sparams.NewUnstructuredList(MDRGroup, MDRVersion, "MachineDeletionRemediationList")},
		{Cr: medik8sparams.NewUnstructuredList(MDRGroup, MDRVersion, "MachineDeletionRemediationTemplateList")},
		{Cr: medik8sparams.NewUnstructuredList(SBRGroup, SBRVersion, "StorageBasedRemediationList")},
		{Cr: medik8sparams.NewUnstructuredList(SBRGroup, SBRVersion, "StorageBasedRemediationConfigList")},
		{Cr: medik8sparams.NewUnstructuredList(SBRGroup, SBRVersion, "StorageBasedRemediationTemplateList")},
	}
)
