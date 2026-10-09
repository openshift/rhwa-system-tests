package nmoparams

import (
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/openshift-kni/k8sreporter"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
)

var (
	// Labels represents the range of labels that can be used for test cases selection.
	Labels = []string{medik8sparams.Label, Label}

	operatorNs = medik8sparams.OperatorNs

	// WorkloadTestImage is the container image used for test workload pods.
	WorkloadTestImage = medik8sparams.WorkloadImage

	// ReporterNamespacesToDump tells the reporter from where to collect logs.
	ReporterNamespacesToDump = map[string]string{
		medik8sparams.OperatorNs: medik8sparams.OperatorNs,
		"openshift-machine-api":  "openshift-machine-api",
	}

	// ReporterCRDsToDump tells the reporter what CRs to dump.
	ReporterCRDsToDump = []k8sreporter.CRData{
		{Cr: &corev1.PodList{}},
		{Cr: &coordinationv1.LeaseList{}, Namespace: &operatorNs},
		{Cr: medik8sparams.NewUnstructuredList("nodemaintenance.medik8s.io", "v1beta1", "NodeMaintenanceList")},
	}
)
