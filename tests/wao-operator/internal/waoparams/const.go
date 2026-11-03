package waoparams

import "time"

const (
	// Label is the operator name used in the suite-level Labels array.
	Label = "wao"

	// DefaultPollInterval is the polling interval used with Eventually calls.
	DefaultPollInterval = 5 * time.Second

	// OperandAvailableTimeout bounds the wait for an enabled operand to report Available. It
	// covers the image pull, the manager Deployment rollout and, for the operands that ship
	// webhooks, the service CA controller injecting the CA bundle the phase also waits on.
	OperandAvailableTimeout = 5 * time.Minute

	// OperandDisabledTimeout bounds the wait for a disabled operand to stop reporting a manager.
	// Removal is a delete of objects WAO owns, so it is quicker than an install.
	OperandDisabledTimeout = 3 * time.Minute

	// WAODeploymentName is WAO's own manager. It is the one Deployment the operator namespace
	// is expected to hold while every operand is disabled.
	WAODeploymentName = "workload-availability-operator-controller-manager"

	// CSVNamePattern is the substring matching the WAO ClusterServiceVersion by name.
	CSVNamePattern = "workload-availability-operator"
)
