package waoparams

import (
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/openshift-kni/k8sreporter"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

var (
	// Labels represents the range of labels that can be used for test cases selection.
	Labels = []string{medik8sparams.Label, Label}

	// OperandDeployments maps each operand key to the Deployments WAO installs for it. Defined
	// in medik8sparams because every suite needs it to tell whether an operand is running.
	OperandDeployments = medik8sparams.WAOOperandDeployments

	// ReporterNamespacesToDump tells the reporter from where to collect logs.
	ReporterNamespacesToDump = map[string]string{
		medik8sparams.OperatorNs: medik8sparams.OperatorNs,
	}

	// ReporterCRDsToDump tells the reporter what CRs to dump.
	ReporterCRDsToDump = []k8sreporter.CRData{
		{Cr: &corev1.PodList{}},
		{Cr: &appsv1.DeploymentList{}},
		{Cr: &appsv1.DaemonSetList{}},
		{Cr: medik8sparams.NewUnstructuredList(
			medik8sparams.WAOCRDGroup, medik8sparams.WAOCRDVersion, medik8sparams.WAOConfigKind+"List")},
	}
)
