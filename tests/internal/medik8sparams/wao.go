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

	// WAOManagerServiceAccount is the ServiceAccount of the WAO manager. The manager serves the
	// admission webhooks of FAR, NHC, NMO and SNR, so admission reads use this account.
	WAOManagerServiceAccount = "workload-availability-operator-controller-manager"

	// WAOOperandAvailableTimeout bounds the wait for an enabled operand to report Available in a
	// suite precondition. It covers the image pull and the manager Deployment rollout.
	WAOOperandAvailableTimeout = 5 * time.Minute

	// WAOOperandPollInterval is the poll interval for the phase of an operand.
	WAOOperandPollInterval = 5 * time.Second
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
