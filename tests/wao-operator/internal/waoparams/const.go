package waoparams

import "github.com/medik8s/system-tests/tests/internal/medik8sparams"

const (
	// Label is the operator name used in the suite-level Labels array.
	Label = "wao"

	// DefaultPollInterval is the polling interval used with Eventually calls.
	DefaultPollInterval = medik8sparams.WAOPollInterval

	// OperandAvailableTimeout bounds the wait for an enabled operand to report Available.
	OperandAvailableTimeout = medik8sparams.WAOOperandAvailableTimeout

	// OperandDisabledTimeout bounds the wait for a disabled operand to stop reporting a manager.
	OperandDisabledTimeout = medik8sparams.WAOOperandDisabledTimeout

	// WAODeploymentName is WAO's own manager. It is the one Deployment the operator namespace
	// is expected to hold while every operand is disabled.
	WAODeploymentName = "workload-availability-operator-controller-manager"

	// CSVNamePattern is the substring matching the WAO ClusterServiceVersion by name.
	CSVNamePattern = "workload-availability-operator"
)
