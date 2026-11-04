package medik8sparams

import "time"

const (
	// WAOCRDGroup is the API group of the workload-availability-operator configuration CRD.
	WAOCRDGroup = "workloadavailability.medik8s.io"

	// WAOCRDVersion is the served version of the WorkloadAvailabilityConfig CRD.
	WAOCRDVersion = "v1alpha1"

	// WAOConfigKind is the kind of the workload-availability-operator configuration.
	WAOConfigKind = "WorkloadAvailabilityConfig"

	// WAOConfigResource is the plural resource name of the configuration CRD.
	WAOConfigResource = "workloadavailabilityconfigs"

	// WAOConfigName is the only permitted name of the configuration: the CR is a
	// cluster-scoped singleton that WAO creates automatically with every operand disabled.
	WAOConfigName = "cluster"
)

// Operand keys under spec.components of the WorkloadAvailabilityConfig. The same keys
// identify an operand in status.components[].name.
const (
	// WAOComponentNHC is the Node Health Check operand.
	WAOComponentNHC = "nodeHealthCheck"

	// WAOComponentSNR is the Self Node Remediation operand.
	WAOComponentSNR = "selfNodeRemediation"

	// WAOComponentSBR is the Storage Based Remediation operand.
	WAOComponentSBR = "storageBasedRemediation"

	// WAOComponentNMO is the Node Maintenance operand.
	WAOComponentNMO = "nodeMaintenance"

	// WAOComponentFAR is the Fence Agents Remediation operand.
	WAOComponentFAR = "fenceAgentsRemediation"

	// WAOComponentMDR is the Machine Deletion Remediation operand.
	WAOComponentMDR = "machineDeletionRemediation"
)

// WAOAllComponents lists every operand key the configuration accepts, in API display order.
var WAOAllComponents = []string{
	WAOComponentNHC, WAOComponentSNR, WAOComponentSBR,
	WAOComponentNMO, WAOComponentFAR, WAOComponentMDR,
}

// Observed lifecycle phases reported in status.components[].phase.
const (
	// WAOPhaseDisabled means the operand is not installed.
	WAOPhaseDisabled = "Disabled"

	// WAOPhaseProgressing means the operand is converging on its desired state.
	WAOPhaseProgressing = "Progressing"

	// WAOPhaseAvailable means the operand is installed and ready.
	WAOPhaseAvailable = "Available"

	// WAOPhaseDegraded means the operand failed to reach its desired state.
	WAOPhaseDegraded = "Degraded"

	// WAOPhaseDisableIncomplete means the manager is gone but resources it created remain.
	WAOPhaseDisableIncomplete = "DisableIncomplete"
)

// WAOOperandDeployments maps each operand key to the Deployments that run it. An operand
// reports Available only once all of them are ready, and WAO removes all of them when it is
// disabled.
//
// The names belong to the operand, not to WAO, so they are the same on a pre-WAO cluster where
// the operator was installed on its own. That is what lets a Deployment check answer "is this
// operand installed and running" without knowing which layout the cluster uses — unlike a CRD
// check, which under WAO is true for every operand from the moment OLM installs the bundle.
//
// Keep in step with the component definitions in workload-availability-operator/internal.
var WAOOperandDeployments = map[string][]string{
	WAOComponentNHC: {
		"node-healthcheck-controller-manager",
		"node-healthcheck-node-remediation-console-plugin",
	},
	WAOComponentSNR: {"self-node-remediation-controller-manager"},
	WAOComponentSBR: {"sbr-operator-controller-manager"},
	WAOComponentNMO: {"node-maintenance-operator-controller-manager"},
	WAOComponentFAR: {"fence-agents-remediation-controller-manager"},
	WAOComponentMDR: {"machine-deletion-remediation-controller-manager"},
}

// Timeouts for an operand changing state. They live here rather than in waoparams because
// every suite waits on them from its own precondition, and tests/wao-operator/internal is
// importable only from tests/wao-operator.
const (
	// WAOOperandAvailableTimeout bounds the wait for an enabled operand to report Available. It
	// covers the image pull, the manager Deployment rollout and, for the operands that ship
	// webhooks, the service CA controller injecting the CA bundle the phase also waits on.
	WAOOperandAvailableTimeout = 5 * time.Minute

	// WAOOperandDisabledTimeout bounds the wait for a disabled operand to stop reporting a
	// manager. Removal is a delete of objects WAO owns, so it is quicker than an install.
	WAOOperandDisabledTimeout = 3 * time.Minute

	// WAOPollInterval is the polling interval used while waiting on an operand phase.
	WAOPollInterval = 5 * time.Second
)
