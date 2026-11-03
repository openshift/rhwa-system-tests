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

	// OperandDeployments maps each operand key to the Deployments WAO installs for it. The
	// operand reports Available only once all of them are ready, and removes all of them when
	// it is disabled. Keep in step with the component definitions in
	// workload-availability-operator/internal/components.
	OperandDeployments = map[string][]string{
		medik8sparams.WAOComponentNHC: {
			"node-healthcheck-controller-manager",
			"node-healthcheck-node-remediation-console-plugin",
		},
		medik8sparams.WAOComponentSNR: {"self-node-remediation-controller-manager"},
		medik8sparams.WAOComponentSBR: {"sbr-operator-controller-manager"},
		medik8sparams.WAOComponentNMO: {"node-maintenance-operator-controller-manager"},
		medik8sparams.WAOComponentFAR: {"fence-agents-remediation-controller-manager"},
		medik8sparams.WAOComponentMDR: {"machine-deletion-remediation-controller-manager"},
	}

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
