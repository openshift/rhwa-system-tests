package collectionparams

import "time"

const (
	// Label represents the collection suite label used for test case selection.
	Label = "collection"

	// FixturePrefix names every resource this suite creates, so a leaked
	// fixture is attributable and a cleanup sweep can find it by prefix alone.
	FixturePrefix = "rhwa-collection-"

	// NoMatchLabelKey is applied to nothing. The fixture NodeHealthCheck selects
	// on it so the CR exists, is reconciled and is collected without ever being
	// able to remediate a node.
	NoMatchLabelKey = "medik8s.io/rhwa-collection-never-matches"

	// MinHealthy keeps the fixture NodeHealthCheck's spec valid; with a selector
	// that matches no node the value is never acted on.
	MinHealthy = "51%"

	// UnhealthyConditionDuration is the fixture NodeHealthCheck's condition
	// duration. Long enough that an accidentally matching node could not be
	// remediated before the spec's own cleanup removes the CR.
	UnhealthyConditionDuration = "300s"

	// FenceAgent is the fence agent named by the fixture
	// FenceAgentsRemediationTemplate. The FAR webhook rejects a template whose
	// agent is unknown or whose mandatory parameters are missing, so the value
	// and the parameters below have to be real even though nothing fences.
	FenceAgent = "fence_ipmilan"

	// MaintenanceReason is recorded on the fixture NodeMaintenance.
	MaintenanceReason = "RHWA must-gather collection coverage test"

	// AbsentNodeName is the node the MachineDeletionRemediation and
	// StorageBasedRemediation fixtures are named after, and it must not
	// exist. Both operators identify the node to remediate by the CR's own
	// name: MDR deletes that node's Machine, and SBR storage-fences it. A CR
	// named after a real node would therefore remediate it, while a name no
	// node has yields a real, collectable CR that remediates nothing. The
	// suite asserts the name is absent before creating either fixture, so
	// that is checked rather than assumed.
	AbsentNodeName = FixturePrefix + "absent-node"

	// MDRNodeNotFoundReason is the condition reason the MDR controller sets
	// when the node named by a MachineDeletionRemediation does not exist.
	// Observing it proves the controller reconciled the CR and declined to
	// remediate, rather than the CR having merely been stored by the API.
	MDRNodeNotFoundReason = "RemediationCannotStartNodeNotFound"

	// MDRProcessingCondition is the condition MDRNodeNotFoundReason appears on.
	MDRProcessingCondition = "Processing"

	// MDRConditionTimeout is the maximum wait for the MDR controller to
	// reconcile the fixture and report why it will not remediate.
	MDRConditionTimeout = 2 * time.Minute

	// SBRConfigName is the StorageBasedRemediationConfig the SBR spec creates
	// to activate the SBR controller.
	SBRConfigName = FixturePrefix + "sbrc"

	// SBRConfigSelectorValue is the nodeSelector value on that config. Paired
	// with NoMatchLabelKey it matches no node, so the agent DaemonSet the
	// controller would own schedules zero pods and no watchdog or storage
	// action is ever taken. This mirrors sbrutils.SafeSpec in the SBR suite.
	SBRConfigSelectorValue = "rhwa-collection-no-node"

	// SBRCMaxConsecutiveFailures is the CRD's minimum accepted value. Nothing
	// ever counts failures here, but the field has to be valid.
	SBRCMaxConsecutiveFailures = int64(2)

	// SBRCFinalizer is the finalizer the SBR operator adds to a config it has
	// taken ownership of. Its appearance is the observable proof that the
	// controller reconciled the object, rather than the API merely storing
	// it -- the config reports no status conditions to assert on instead.
	SBRCFinalizer = "sbr-operator.medik8s.io/cleanup"

	// SBRCReconciledTimeout is the maximum wait for that finalizer.
	SBRCReconciledTimeout = 2 * time.Minute

	// SBRInertCheckDuration is how long the operator namespace is watched to
	// confirm the config creates no agent DaemonSet and no shared-storage
	// PVC. Omitting sharedStorageClass is what makes it inert; this proves it
	// rather than trusting it, because the alternative is a watchdog
	// DaemonSet landing on real nodes.
	SBRInertCheckDuration = 30 * time.Second

	// SBRInertCheckInterval is the polling interval for that check.
	SBRInertCheckInterval = 5 * time.Second

	// DefaultPollInterval is the polling interval used with Eventually calls.
	DefaultPollInterval = 5 * time.Second

	// InventoryTimeout bounds the API reads that build the expected inventory.
	InventoryTimeout = 2 * time.Minute

	// FixtureReadyTimeout is the maximum wait for a created fixture CR to be
	// readable back through the API.
	FixtureReadyTimeout = 2 * time.Minute

	// FixtureCleanupTimeout is the maximum wait for a fixture CR to disappear.
	FixtureCleanupTimeout = 5 * time.Minute

	// MaintenanceTimeout is the maximum wait for a NodeMaintenance CR to cordon
	// and drain its node.
	MaintenanceTimeout = 10 * time.Minute

	// UncordonTimeout is the maximum wait for a node to become schedulable
	// again after its NodeMaintenance CR is deleted.
	UncordonTimeout = 5 * time.Minute

	// RemediationVisibleTimeout is the maximum wait for a manually created
	// SelfNodeRemediation CR to be observable through the API before the
	// collection starts.
	RemediationVisibleTimeout = 2 * time.Minute

	// NodeRecoveryTimeout is the maximum wait for a node to return Ready after
	// the SelfNodeRemediation reboot.
	NodeRecoveryTimeout = 20 * time.Minute

	// MinWorkersForDestructive is the number of Ready workers the destructive
	// specs need: one to disrupt and enough left to keep the operators running.
	MinWorkersForDestructive = 3
)

// API groups and versions of every CRD this suite instantiates. They are
// repeated here rather than imported from each operator's params package so
// the suite does not depend on six packages, two of which panic at init when
// WORKLOAD_IMAGE is unset.
const (
	// NHCGroup is the API group for NodeHealthCheck.
	NHCGroup = "remediation.medik8s.io"
	// NHCVersion is the API version for NodeHealthCheck.
	NHCVersion = "v1alpha1"

	// SNRGroup is the API group for Self Node Remediation resources.
	SNRGroup = "self-node-remediation.medik8s.io"
	// SNRVersion is the API version for Self Node Remediation resources.
	SNRVersion = "v1alpha1"

	// FARGroup is the API group for Fence Agents Remediation resources.
	FARGroup = "fence-agents-remediation.medik8s.io"
	// FARVersion is the API version for Fence Agents Remediation resources.
	FARVersion = "v1alpha1"

	// MDRGroup is the API group for Machine Deletion Remediation resources.
	MDRGroup = "machine-deletion-remediation.medik8s.io"
	// MDRVersion is the API version for Machine Deletion Remediation resources.
	MDRVersion = "v1alpha1"

	// SBRGroup is the API group for Storage Based Remediation resources.
	SBRGroup = "storage-based-remediation.medik8s.io"
	// SBRVersion is the API version for Storage Based Remediation resources.
	SBRVersion = "v1alpha1"

	// NMOGroup is the API group for NodeMaintenance.
	NMOGroup = "nodemaintenance.medik8s.io"
	// NMOVersion is the API version for NodeMaintenance.
	NMOVersion = "v1beta1"
)
