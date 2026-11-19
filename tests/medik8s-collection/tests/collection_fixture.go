package tests

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/medik8s-collection/internal/collectionparams"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// fixture is one custom resource the suite creates so that the collection has
// an instance of its CRD to collect. crdName ties it back to the CRD the
// inventory discovered, so a failure names the operator whose data is missing.
type fixture struct {
	description string
	crdName     string
	object      *unstructured.Unstructured
}

// nonDestructiveFixtures returns one CR for every medik8s CRD that can be
// instantiated without disrupting a node, in creation order: the
// NodeHealthCheck references the SelfNodeRemediationTemplate, so the template
// is created first.
//
// storageConfigPresent reports whether the cluster has a
// StorageBasedRemediationConfig, which is what activates the SBR remediation
// controller and so decides whether the StorageBasedRemediation fixture is
// safe to create.
//
// Only fenceagentsremediations is absent by design: FAR taints the node
// before it attempts to fence, so even a CR that can never succeed leaves
// state behind. The destructive specs cover selfnoderemediations and
// nodemaintenances; see the test case document for the rest.
//
// storagebasedremediationconfigs is also absent: creating one activates the
// SBR controller, which provisions shared storage and adds a watchdog
// DaemonSet. That is a cluster configuration change, not a test fixture. When
// a cluster already has one, discovery finds it and the collection is checked
// against it like any other instance.
func nonDestructiveFixtures(storageConfigPresent bool) []fixture {
	snrTemplateName := collectionparams.FixturePrefix + "snr-template"
	fixtures := []fixture{
		{
			description: "SelfNodeRemediationTemplate",
			crdName:     "selfnoderemediationtemplates." + collectionparams.SNRGroup,
			object: namespacedCR(collectionparams.SNRGroup, collectionparams.SNRVersion,
				"SelfNodeRemediationTemplate", snrTemplateName, map[string]interface{}{
					"template": map[string]interface{}{
						"spec": map[string]interface{}{
							"remediationStrategy": "Automatic",
						},
					},
				}),
		},
		{
			description: "FenceAgentsRemediationTemplate",
			crdName:     "fenceagentsremediationtemplates." + collectionparams.FARGroup,
			// The FAR webhook rejects a template whose agent is unknown or whose
			// mandatory parameters are missing, so these values have to be
			// well-formed. Nothing ever fences: no FenceAgentsRemediation CR
			// references this template.
			object: namespacedCR(collectionparams.FARGroup, collectionparams.FARVersion,
				"FenceAgentsRemediationTemplate", collectionparams.FixturePrefix+"far-template",
				map[string]interface{}{
					"template": map[string]interface{}{
						"spec": map[string]interface{}{
							"agent":               collectionparams.FenceAgent,
							"remediationStrategy": "ResourceDeletion",
							"sharedparameters": map[string]interface{}{
								"--username": "unused",
								"--password": "unused",
								"--action":   "reboot",
							},
							"nodeparameters": map[string]interface{}{
								"--ip": map[string]interface{}{},
							},
						},
					},
				}),
		},
		{
			description: "MachineDeletionRemediationTemplate",
			crdName:     "machinedeletionremediationtemplates." + collectionparams.MDRGroup,
			object: namespacedCR(collectionparams.MDRGroup, collectionparams.MDRVersion,
				"MachineDeletionRemediationTemplate", collectionparams.FixturePrefix+"mdr-template",
				map[string]interface{}{
					"template": map[string]interface{}{
						"spec": map[string]interface{}{},
					},
				}),
		},
		{
			description: "StorageBasedRemediationTemplate",
			crdName:     "storagebasedremediationtemplates." + collectionparams.SBRGroup,
			object: namespacedCR(collectionparams.SBRGroup, collectionparams.SBRVersion,
				"StorageBasedRemediationTemplate", collectionparams.FixturePrefix+"sbr-template",
				map[string]interface{}{
					"template": map[string]interface{}{
						"spec": map[string]interface{}{},
					},
				}),
		},
		{
			description: "MachineDeletionRemediation",
			crdName:     "machinedeletionremediations." + collectionparams.MDRGroup,
			// Named after a node that does not exist. MDR resolves the node
			// its CR is named for to a Machine and deletes it, so this is the
			// one fixture whose name is load-bearing: against a real node it
			// would delete that node's Machine. The controller reconciles it,
			// finds no such node, and records
			// MDRNodeNotFoundReason without adding a finalizer -- verified
			// against the live controller, including that the CR deletes
			// cleanly afterwards.
			object: namespacedCR(collectionparams.MDRGroup, collectionparams.MDRVersion,
				"MachineDeletionRemediation", collectionparams.AbsentNodeName,
				map[string]interface{}{}),
		},
		{
			description: "NodeHealthCheck",
			crdName:     "nodehealthchecks." + collectionparams.NHCGroup,
			// The selector matches no node, so the CR is reconciled and
			// collected but can never remediate one.
			object: clusterScopedCR(collectionparams.NHCGroup, collectionparams.NHCVersion,
				"NodeHealthCheck", collectionparams.FixturePrefix+"nhc", map[string]interface{}{
					"minHealthy": collectionparams.MinHealthy,
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							collectionparams.NoMatchLabelKey: "true",
						},
					},
					"remediationTemplate": map[string]interface{}{
						"apiVersion": collectionparams.SNRGroup + "/" + collectionparams.SNRVersion,
						"kind":       "SelfNodeRemediationTemplate",
						"name":       snrTemplateName,
						"namespace":  medik8sparams.OperatorNs,
					},
					"unhealthyConditions": []interface{}{
						map[string]interface{}{
							"type":     "Ready",
							"status":   "False",
							"duration": collectionparams.UnhealthyConditionDuration,
						},
					},
				}),
		},
	}

	if !storageConfigPresent {
		fixtures = append(fixtures, storageBasedRemediationFixture())
	}

	return fixtures
}

// storageBasedRemediationFixture is appended only when the cluster has no
// StorageBasedRemediationConfig. The SBR remediation controller reconciles
// only while a config exists; with none, the CR is stored and never acted on
// -- no finalizer, no taint, and it deletes in milliseconds, verified against
// this controller. With a config present the controller would take the CR up
// and attempt to storage-fence, so the fixture is dropped rather than risk
// fencing a node to prove a file path exists.
//
// Unlike the MDR fixture this one is therefore not reconciled. It is still a
// real stored instance of its CRD, which is all the collection has to find.
func storageBasedRemediationFixture() fixture {
	return fixture{
		description: "StorageBasedRemediation",
		crdName:     "storagebasedremediations." + collectionparams.SBRGroup,
		object: namespacedCR(collectionparams.SBRGroup, collectionparams.SBRVersion,
			"StorageBasedRemediation", collectionparams.AbsentNodeName,
			map[string]interface{}{}),
	}
}

// sbrFixtures returns one custom resource for each of the three SBR CRDs, in
// creation order: the config first, so the controller is active for the
// remediation CR that follows.
//
// The config deliberately omits sharedStorageClass. Per sbrutils.SafeSpec in
// the SBR suite, that is what keeps it inert -- the controller reports a
// storage validation error and creates neither the agent DaemonSet nor a PVC
// -- while still reconciling the object and taking ownership of it. Combined
// with a nodeSelector no node can match, nothing is ever fenced.
func sbrFixtures() []fixture {
	return []fixture{
		{
			description: "StorageBasedRemediationConfig",
			crdName:     "storagebasedremediationconfigs." + collectionparams.SBRGroup,
			object: namespacedCR(collectionparams.SBRGroup, collectionparams.SBRVersion,
				"StorageBasedRemediationConfig", collectionparams.SBRConfigName,
				map[string]interface{}{
					"nodeSelector": map[string]interface{}{
						collectionparams.NoMatchLabelKey: collectionparams.SBRConfigSelectorValue,
					},
					"maxConsecutiveFailures": collectionparams.SBRCMaxConsecutiveFailures,
				}),
		},
		{
			description: "StorageBasedRemediationTemplate",
			crdName:     "storagebasedremediationtemplates." + collectionparams.SBRGroup,
			object: namespacedCR(collectionparams.SBRGroup, collectionparams.SBRVersion,
				"StorageBasedRemediationTemplate", collectionparams.FixturePrefix+"sbr-template",
				map[string]interface{}{
					"template": map[string]interface{}{
						"spec": map[string]interface{}{},
					},
				}),
		},
		storageBasedRemediationFixture(),
	}
}

// buildNodeMaintenance returns a NodeMaintenance CR for nodeName. Applying it
// cordons and drains the node.
func buildNodeMaintenance(nodeName string) *unstructured.Unstructured {
	return clusterScopedCR(collectionparams.NMOGroup, collectionparams.NMOVersion,
		"NodeMaintenance", collectionparams.FixturePrefix+nodeName, map[string]interface{}{
			"nodeName": nodeName,
			"reason":   collectionparams.MaintenanceReason,
		})
}

// buildSelfNodeRemediation returns a SelfNodeRemediation CR for nodeName.
// Applying it reboots the node. The CR is named after the node, which is how
// the SNR controller addresses it.
func buildSelfNodeRemediation(nodeName string) *unstructured.Unstructured {
	return namespacedCR(collectionparams.SNRGroup, collectionparams.SNRVersion,
		"SelfNodeRemediation", nodeName, map[string]interface{}{
			"remediationStrategy": "Automatic",
		})
}

// namespacedCR builds a custom resource in the operator namespace.
func namespacedCR(group, version, kind, name string, spec map[string]interface{}) *unstructured.Unstructured {
	object := clusterScopedCR(group, version, kind, name, spec)
	object.SetNamespace(medik8sparams.OperatorNs)

	return object
}

// clusterScopedCR builds a cluster-scoped custom resource.
func clusterScopedCR(group, version, kind, name string, spec map[string]interface{}) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(group + "/" + version)
	object.SetKind(kind)
	object.SetName(name)
	object.Object["spec"] = spec

	return object
}

// createFixtures creates each fixture in order and registers its removal.
// Cleanup is registered per object as it is created, so a failure part way
// through the list still removes what already exists.
func createFixtures(ctx context.Context, fixtures []fixture) {
	GinkgoHelper()

	for _, item := range fixtures {
		By("Creating fixture " + item.description + " " + item.object.GetName())
		createFixtureObject(ctx, item.object)
	}
}

// createFixtureObject creates one CR, tolerating one left behind by an earlier
// interrupted run, and registers its deletion.
func createFixtureObject(ctx context.Context, object *unstructured.Unstructured) {
	GinkgoHelper()

	DeferCleanup(func() {
		By("Removing fixture " + object.GetKind() + " " + object.GetName())
		deleteFixtureObject(context.Background(), object)
	})

	err := APIClient.Create(ctx, object)
	if k8serrors.IsAlreadyExists(err) {
		GinkgoWriter.Printf("Fixture %s %s already exists, reusing it\n",
			object.GetKind(), object.GetName())

		return
	}

	Expect(err).ToNot(HaveOccurred(), "Failed to create %s %s", object.GetKind(), object.GetName())
}

// deleteFixtureObject removes a fixture CR and waits for it to be gone. It
// reports failures without failing the spec: a leaked fixture is a cleanup
// problem to surface, not a reason to turn a passed assertion into a failure.
func deleteFixtureObject(ctx context.Context, object *unstructured.Unstructured) {
	if err := APIClient.Delete(ctx, object); err != nil && !k8serrors.IsNotFound(err) {
		GinkgoWriter.Printf("WARNING: failed to delete %s %s: %v\n",
			object.GetKind(), object.GetName(), err)
		AddReportEntry("fixture-cleanup-failed",
			fmt.Sprintf("%s %s: %v", object.GetKind(), object.GetName(), err))

		return
	}

	if err := waitForGone(ctx, object); err != nil {
		GinkgoWriter.Printf("WARNING: %s %s was not removed: %v\n",
			object.GetKind(), object.GetName(), err)
		AddReportEntry("fixture-cleanup-failed",
			fmt.Sprintf("%s %s not removed: %v", object.GetKind(), object.GetName(), err))
	}
}

// waitForGone polls until the object no longer exists. It returns an error
// rather than asserting, so its caller can report a cleanup problem without
// turning a passed spec into a failed one.
func waitForGone(ctx context.Context, object *unstructured.Unstructured) error {
	key := runtimeclient.ObjectKeyFromObject(object)

	err := wait.PollUntilContextTimeout(ctx,
		collectionparams.DefaultPollInterval, collectionparams.FixtureCleanupTimeout, true,
		func(pollCtx context.Context) (bool, error) {
			probe := &unstructured.Unstructured{}
			probe.SetGroupVersionKind(object.GroupVersionKind())
			getErr := APIClient.Get(pollCtx, key, probe)
			if k8serrors.IsNotFound(getErr) {
				return true, nil
			}

			return false, getErr
		})
	if err != nil {
		return fmt.Errorf("waiting for %s %s to be removed: %w", object.GetKind(), object.GetName(), err)
	}

	return nil
}
