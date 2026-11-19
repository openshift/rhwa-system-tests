package tests

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/internal/mustgather"
	"github.com/medik8s/system-tests/tests/medik8s-collection/internal/collectionparams"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// artifactDirPermissions is used when the artifact directory does not
	// already exist, which is the case for a local run.
	artifactDirPermissions = 0o755

	// mebibyte converts an archive size into the unit the log reports.
	mebibyte = 1024 * 1024

	// collectionTempDirPrefix names the throwaway directory the collection is
	// written into. It never reaches the artifact directory -- the archive is
	// named from the inventory instead -- so it only has to be recognisable
	// if a run is interrupted and leaves one behind.
	collectionTempDirPrefix = "rhwa-collection-"
)

var _ = Describe("RHWA diagnostic collection coverage",
	Serial, Ordered,
	Label(labels.OperatorInterop, collectionparams.Label),
	func() {
		var ctx context.Context

		BeforeAll(func() {
			ctx = context.Background()

			By("Verifying every RHWA operator's CRDs are installed")

			verifyRequiredCRDsInstalled(ctx)

			By("Verifying no node is named " + collectionparams.AbsentNodeName)

			// The MachineDeletionRemediation and StorageBasedRemediation
			// fixtures are both named after this node, and both operators
			// identify their target by the CR's own name. If a node ever
			// carried this name, creating those fixtures would delete its
			// Machine and storage-fence it, so absence is asserted rather
			// than assumed.
			verifyNodeAbsent(ctx, collectionparams.AbsentNodeName)
		})

		Context("with every operator configured", func() {
			It("collects every medik8s CRD definition and custom resource",
				Label(labels.DisruptionNonDestructive, labels.TierAcceptance,
					labels.FrequencyWeekly, labels.PlatformAny, labels.ComponentPostDeploy),
				reportxml.ID("1994"),
				func() {
					By("Creating a custom resource for every non-destructive medik8s CRD")

					fixtures := nonDestructiveFixtures(storageConfigPresent(ctx))
					createFixtures(ctx, fixtures)

					By("Snapshotting the expected inventory of medik8s CRDs and custom resources")

					inventory := snapshotInventory(ctx)

					By("Verifying every fixture is present in the inventory")

					verifyFixturesInInventory(inventory, fixtures)

					By("Verifying the MachineDeletionRemediation refused to remediate an absent node")

					verifyMDRRefusedAbsentNode(ctx)

					By("Running the collection and comparing it against the inventory")

					gatherRoot := runCollection(ctx, inventory)
					verifyInventoryCollected(gatherRoot, inventory)

					By("Verifying node data was collected for every cluster node")

					verifyNodesCollected(ctx, gatherRoot)

					By("Verifying operator namespace workloads and pod logs were collected")

					verifyOperatorNamespaceCollected(ctx, gatherRoot)
				})
		})

		Context("with the SBR controller active", func() {
			It("collects every StorageBasedRemediation resource",
				Label(labels.OperatorSBR, labels.DisruptionNonDestructive, labels.TierAcceptance,
					labels.FrequencyWeekly, labels.PlatformAny, labels.ComponentController),
				reportxml.ID("1994"),
				func() {
					By("Verifying the cluster has no StorageBasedRemediationConfig of its own")

					// A cluster with a real, storage-backed config is already
					// running the SBR agent on real nodes. Adding a second
					// config there would change how a configured cluster
					// behaves, so the spec declines rather than interfere.
					if storageConfigPresent(ctx) {
						skipWithReason("the cluster already has a StorageBasedRemediationConfig; " +
							"this spec will not add a second one to a configured cluster")
					}

					By("Recording the operator namespace's DaemonSets and PVCs")

					daemonSetsBefore := listDaemonSetNames(ctx)
					claimsBefore := listPVCNames(ctx)
					GinkgoWriter.Printf("Before: %d DaemonSet(s), %d PVC(s)\n",
						len(daemonSetsBefore), len(claimsBefore))

					By("Creating a StorageBasedRemediationConfig, template and remediation")

					createFixtures(ctx, sbrFixtures())

					By("Waiting for the SBR controller to take ownership of the config")

					verifySBRConfigReconciled(ctx)

					By("Verifying the config created no agent DaemonSet and no shared-storage PVC")

					verifySBRConfigInert(ctx, daemonSetsBefore, claimsBefore)

					By("Snapshotting the expected inventory of medik8s CRDs and custom resources")

					inventory := snapshotInventory(ctx)

					By("Verifying all three SBR CRDs have a live instance")

					verifyAllSBRCRDsPopulated(inventory)

					By("Running the collection and comparing it against the inventory")

					gatherRoot := runCollection(ctx, inventory)
					verifyInventoryCollected(gatherRoot, inventory)

					By("Verifying no node was fenced")

					verifyNoOutOfServiceTaint(ctx)
				})
		})

		Context("with a node under maintenance", func() {
			It("collects a live NodeMaintenance custom resource",
				Label(labels.DisruptionDestructive, labels.TierResiliency,
					labels.FrequencyWeekly, labels.PlatformAny, labels.ComponentPostDeploy),
				reportxml.ID("1994"),
				func() {
					targetNode := selectDestructiveTargetNode(ctx)

					By("Creating a NodeMaintenance CR for " + targetNode)

					maintenance := buildNodeMaintenance(targetNode)
					createFixtureObject(ctx, maintenance)

					By("Waiting for " + targetNode + " to be cordoned")

					Eventually(ctx, func(pollCtx context.Context) (bool, error) {
						return nodeUnschedulable(pollCtx, targetNode)
					}).WithPolling(collectionparams.DefaultPollInterval).
						WithTimeout(collectionparams.MaintenanceTimeout).Should(BeTrue(),
						"NodeMaintenance did not cordon node %s", targetNode)

					By("Snapshotting the expected inventory of medik8s CRDs and custom resources")

					inventory := snapshotInventory(ctx)

					By("Verifying the NodeMaintenance CR is in the inventory")

					Expect(inventory.InstancesOf("nodemaintenances."+collectionparams.NMOGroup)).
						ToNot(BeEmpty(), "No NodeMaintenance instance found while the node is under maintenance")

					By("Running the collection and comparing it against the inventory")

					gatherRoot := runCollection(ctx, inventory)
					verifyInventoryCollected(gatherRoot, inventory)

					By("Waiting for " + targetNode + " to become schedulable after cleanup")

					DeferCleanup(func() {
						waitForNodeSchedulable(context.Background(), targetNode)
					})
				})
		})

		Context("with a node under remediation", func() {
			It("collects a live SelfNodeRemediation custom resource",
				Label(labels.DisruptionDestructive, labels.TierResiliency,
					labels.FrequencyWeekly, labels.PlatformAny, labels.ComponentRemediation),
				reportxml.ID("1994"),
				func() {
					targetNode := selectDestructiveTargetNode(ctx)

					By("Recording boot ID before remediation")

					oldBootID, err := helpers.GetNodeBootIDFromAPI(ctx, APIClient, targetNode)
					Expect(err).ToNot(HaveOccurred(), "Must read boot ID from node %s", targetNode)
					GinkgoWriter.Printf("Boot ID of %s before remediation: %s\n", targetNode, oldBootID)

					By("Creating a SelfNodeRemediation CR for " + targetNode)

					// Recovery is registered before the CR exists so a failure
					// between creation and the gather still waits the node out.
					DeferCleanup(func() {
						By("Waiting for " + targetNode + " to return Ready after the reboot")

						if recoverErr := helpers.WaitForNodeReady(context.Background(), APIClient, targetNode,
							collectionparams.DefaultPollInterval, collectionparams.NodeRecoveryTimeout,
							GinkgoWriter.Printf); recoverErr != nil {
							GinkgoWriter.Printf("WARNING: node %s did not recover: %v\n", targetNode, recoverErr)
							AddReportEntry("node-recovery-failed",
								fmt.Sprintf("node %s did not recover: %v", targetNode, recoverErr))
						}
					})

					remediation := buildSelfNodeRemediation(targetNode)
					createFixtureObject(ctx, remediation)

					By("Waiting for the SelfNodeRemediation CR to be readable")

					Eventually(ctx, func(pollCtx context.Context) (bool, error) {
						return objectExists(pollCtx, remediation)
					}).WithPolling(collectionparams.DefaultPollInterval).
						WithTimeout(collectionparams.RemediationVisibleTimeout).Should(BeTrue(),
						"SelfNodeRemediation CR for %s was not readable", targetNode)

					By("Snapshotting the expected inventory of medik8s CRDs and custom resources")

					inventory := snapshotInventory(ctx)

					By("Verifying the SelfNodeRemediation CR is in the inventory")

					Expect(inventory.InstancesOf("selfnoderemediations."+collectionparams.SNRGroup)).
						ToNot(BeEmpty(), "No SelfNodeRemediation instance found while remediation is active")

					By("Running the collection and comparing it against the inventory")

					gatherRoot := runCollection(ctx, inventory)
					verifyInventoryCollected(gatherRoot, inventory)
				})
		})
	})

// verifyRequiredCRDsInstalled fails the suite when an RHWA operator is absent.
// Every expectation below is derived from the cluster, so without this check a
// cluster missing an operator would simply expect less and pass.
func verifyRequiredCRDsInstalled(ctx context.Context) {
	GinkgoHelper()
	discoverCtx, cancel := context.WithTimeout(ctx, collectionparams.InventoryTimeout)
	defer cancel()
	crds, err := mustgather.DiscoverCRDs(discoverCtx, APIClient, mustgather.CRDNameFilter)
	Expect(err).ToNot(HaveOccurred(), "Failed to discover medik8s CRDs")
	names := make([]string, 0, len(crds))
	for _, crd := range crds {
		names = append(names, crd.Name)
	}

	GinkgoWriter.Printf("Discovered %d medik8s CRDs:\n  %s\n", len(names), strings.Join(names, "\n  "))
	missing := mustgather.MissingNames(names, collectionparams.RequiredCRDNames)
	Expect(missing).To(BeEmpty(),
		"Not every RHWA operator is installed; missing CRDs:\n%s", strings.Join(missing, "\n"))
}

// snapshotInventory records the medik8s CRDs and every live custom resource.
// It runs immediately before the collection: a resource created afterwards is
// legitimately absent from the output.
func snapshotInventory(ctx context.Context) mustgather.Inventory {
	GinkgoHelper()
	snapshotCtx, cancel := context.WithTimeout(ctx, collectionparams.InventoryTimeout)
	defer cancel()
	inventory, err := mustgather.SnapshotInventory(
		snapshotCtx, APIClient, APIClient.Interface, mustgather.CRDNameFilter)
	Expect(err).ToNot(HaveOccurred(), "Failed to snapshot the medik8s inventory")
	Expect(inventory.CRDs).ToNot(BeEmpty(), "No medik8s CRDs discovered")
	GinkgoWriter.Printf("Expected inventory (%d CRDs, %d custom resources):\n%s\n",
		len(inventory.CRDs), len(inventory.Instances), inventory.Summary())

	return inventory
}

// verifyFixturesInInventory confirms each created fixture was picked up by
// discovery, so a silently rejected fixture cannot leave its CRD untested.
func verifyFixturesInInventory(inventory mustgather.Inventory, fixtures []fixture) {
	GinkgoHelper()

	for _, item := range fixtures {
		instances := inventory.InstancesOf(item.crdName)
		names := make([]string, 0, len(instances))

		for _, instance := range instances {
			names = append(names, instance.Name)
		}

		Expect(names).To(ContainElement(item.object.GetName()),
			"Fixture %s %s is not in the discovered inventory of %s (found %v)",
			item.description, item.object.GetName(), item.crdName, names)
	}
}

// storageConfigPresent reports whether the cluster has a
// StorageBasedRemediationConfig. That config is what activates the SBR
// remediation controller, and so decides whether a StorageBasedRemediation
// fixture would be reconciled -- and storage-fence a node -- or simply sit
// there as a collectable object. The decision is logged either way, so a run
// with reduced SBR coverage says so rather than quietly covering less.
func storageConfigPresent(ctx context.Context) bool {
	GinkgoHelper()
	configs := &unstructured.UnstructuredList{}
	configs.SetAPIVersion(collectionparams.SBRGroup + "/" + collectionparams.SBRVersion)
	configs.SetKind("StorageBasedRemediationConfigList")
	Expect(APIClient.List(ctx, configs)).To(Succeed(),
		"Failed to list StorageBasedRemediationConfigs")

	if len(configs.Items) == 0 {
		GinkgoWriter.Printf("No StorageBasedRemediationConfig on the cluster: " +
			"the SBR remediation controller is inactive, so a StorageBasedRemediation " +
			"fixture is safe to create\n")

		return false
	}

	reason := fmt.Sprintf(
		"StorageBasedRemediation: %d StorageBasedRemediationConfig(s) present, so the SBR "+
			"remediation controller is active and would attempt to fence", len(configs.Items))
	GinkgoWriter.Printf("Skipping the StorageBasedRemediation fixture -- %s\n", reason)
	AddReportEntry("skipped-fixture", reason)

	return true
}

// listDaemonSetNames returns the DaemonSets in the operator namespace.
func listDaemonSetNames(ctx context.Context) []string {
	GinkgoHelper()
	listCtx, cancel := context.WithTimeout(ctx, medik8sparams.DefaultTimeout)
	defer cancel()
	list, err := APIClient.AppsV1Interface.DaemonSets(medik8sparams.OperatorNs).
		List(listCtx, metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred(), "Failed to list DaemonSets in %s", medik8sparams.OperatorNs)
	names := make([]string, 0, len(list.Items))
	for idx := range list.Items {
		names = append(names, list.Items[idx].Name)
	}

	sort.Strings(names)

	return names
}

// listPVCNames returns the PersistentVolumeClaims in the operator namespace.
func listPVCNames(ctx context.Context) []string {
	GinkgoHelper()
	listCtx, cancel := context.WithTimeout(ctx, medik8sparams.DefaultTimeout)
	defer cancel()
	list, err := APIClient.CoreV1Interface.PersistentVolumeClaims(medik8sparams.OperatorNs).
		List(listCtx, metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred(), "Failed to list PVCs in %s", medik8sparams.OperatorNs)
	names := make([]string, 0, len(list.Items))
	for idx := range list.Items {
		names = append(names, list.Items[idx].Name)
	}

	sort.Strings(names)

	return names
}

// verifySBRConfigReconciled waits for the operator's finalizer on the config.
// The config reports no status conditions, so the finalizer is the only
// externally observable evidence that the controller took the object up --
// which is what makes the remediation CR beside it meaningful rather than
// just an object the API happened to store.
func verifySBRConfigReconciled(ctx context.Context) {
	GinkgoHelper()
	probe := &unstructured.Unstructured{}
	probe.SetAPIVersion(collectionparams.SBRGroup + "/" + collectionparams.SBRVersion)
	probe.SetKind("StorageBasedRemediationConfig")
	key := runtimeclient.ObjectKey{
		Namespace: medik8sparams.OperatorNs,
		Name:      collectionparams.SBRConfigName,
	}

	Eventually(ctx, func(pollCtx context.Context) ([]string, error) {
		if err := APIClient.Get(pollCtx, key, probe); err != nil {
			return nil, err
		}

		return probe.GetFinalizers(), nil
	}).WithPolling(collectionparams.DefaultPollInterval).
		WithTimeout(collectionparams.SBRCReconciledTimeout).
		Should(ContainElement(collectionparams.SBRCFinalizer),
			"The SBR controller did not take ownership of config %q", collectionparams.SBRConfigName)
}

// verifySBRConfigInert confirms the config produced no agent DaemonSet and no
// shared-storage PVC, holding that true for a period rather than checking
// once: the controller could create either a moment after the config is
// accepted. Omitting sharedStorageClass is what makes it inert, and the cost
// of being wrong is a watchdog DaemonSet on real nodes.
func verifySBRConfigInert(ctx context.Context, daemonSetsBefore, claimsBefore []string) {
	GinkgoHelper()

	Consistently(ctx, func(pollCtx context.Context) []string {
		return listDaemonSetNames(pollCtx)
	}).WithPolling(collectionparams.SBRInertCheckInterval).
		WithTimeout(collectionparams.SBRInertCheckDuration).
		Should(Equal(daemonSetsBefore),
			"The SBR config created an agent DaemonSet; it was expected to stay inert")

	Consistently(ctx, func(pollCtx context.Context) []string {
		return listPVCNames(pollCtx)
	}).WithPolling(collectionparams.SBRInertCheckInterval).
		WithTimeout(collectionparams.SBRInertCheckDuration).
		Should(Equal(claimsBefore),
			"The SBR config created a PersistentVolumeClaim; it was expected to stay inert")
}

// verifyAllSBRCRDsPopulated confirms the inventory has an instance of each of
// the three SBR CRDs, so the comparison that follows actually exercises them.
func verifyAllSBRCRDsPopulated(inventory mustgather.Inventory) {
	GinkgoHelper()

	for _, crdName := range []string{
		"storagebasedremediationconfigs." + collectionparams.SBRGroup,
		"storagebasedremediationtemplates." + collectionparams.SBRGroup,
		"storagebasedremediations." + collectionparams.SBRGroup,
	} {
		Expect(inventory.InstancesOf(crdName)).ToNot(BeEmpty(),
			"No live instance of %s; the collection would not be exercising it", crdName)
	}
}

// verifyNoOutOfServiceTaint confirms the spec fenced nothing. SBR's remedy is
// the out-of-service taint, so its absence is the check that this spec stayed
// as harmless as it is meant to be.
func verifyNoOutOfServiceTaint(ctx context.Context) {
	GinkgoHelper()
	listCtx, cancel := context.WithTimeout(ctx, medik8sparams.DefaultTimeout)
	defer cancel()
	nodeList, err := APIClient.CoreV1Interface.Nodes().List(listCtx, metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred(), "Failed to list cluster nodes")

	var tainted []string

	for nodeIdx := range nodeList.Items {
		node := &nodeList.Items[nodeIdx]
		for taintIdx := range node.Spec.Taints {
			if strings.Contains(node.Spec.Taints[taintIdx].Key, "out-of-service") {
				tainted = append(tainted, node.Name)
			}
		}
	}

	Expect(tainted).To(BeEmpty(), "Nodes carry an out-of-service taint: %v", tainted)
}

// verifyNodeAbsent fails unless no node carries the given name. Any error
// other than NotFound also fails: absence that cannot be read is not absence,
// and the caller is about to create a resource that is only safe if the node
// really does not exist.
func verifyNodeAbsent(ctx context.Context, nodeName string) {
	GinkgoHelper()
	err := APIClient.Get(ctx, runtimeclient.ObjectKey{Name: nodeName}, &corev1.Node{})
	Expect(k8serrors.IsNotFound(err)).To(BeTrue(),
		"Node %q must not exist: the MachineDeletionRemediation fixture is named after it, and "+
			"MDR deletes the Machine backing the node its CR names (got error %v)", nodeName, err)
}

// verifyMDRRefusedAbsentNode waits for the MDR controller to report that it
// will not remediate the fixture's node. This is what makes the fixture
// trustworthy: it proves the controller reconciled the CR and declined, so
// the collected resource is a genuinely reconciled one and nothing was
// deleted on the way.
func verifyMDRRefusedAbsentNode(ctx context.Context) {
	GinkgoHelper()
	probe := &unstructured.Unstructured{}
	probe.SetAPIVersion(collectionparams.MDRGroup + "/" + collectionparams.MDRVersion)
	probe.SetKind("MachineDeletionRemediation")
	key := runtimeclient.ObjectKey{
		Namespace: medik8sparams.OperatorNs,
		Name:      collectionparams.AbsentNodeName,
	}

	Eventually(ctx, func(pollCtx context.Context) (string, error) {
		if err := APIClient.Get(pollCtx, key, probe); err != nil {
			return "", err
		}

		return conditionReason(probe, collectionparams.MDRProcessingCondition), nil
	}).WithPolling(collectionparams.DefaultPollInterval).
		WithTimeout(collectionparams.MDRConditionTimeout).
		Should(Equal(collectionparams.MDRNodeNotFoundReason),
			"The MDR controller should have refused to remediate absent node %q",
			collectionparams.AbsentNodeName)
}

// conditionReason returns the reason of the named status condition, or "" when
// the object has no such condition yet.
func conditionReason(object *unstructured.Unstructured, conditionType string) string {
	conditions, found, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil || !found {
		return ""
	}

	for _, entry := range conditions {
		condition, isMap := entry.(map[string]interface{})
		if !isMap || condition["type"] != conditionType {
			continue
		}

		reason, _ := condition["reason"].(string)

		return reason
	}

	return ""
}

// runCollection runs the must-gather image and returns the root of the
// collected tree. It also checks the one artifact `oc adm inspect` does not
// write: the version file the collection script itself produces, which is
// absent when the script exits early.
func runCollection(ctx context.Context, inventory mustgather.Inventory) string {
	GinkgoHelper()

	By("Resolving the must-gather image")

	image := mustgather.ResolveImage(mustgather.ImageEnvVar, mustgather.DefaultImage, GinkgoWriter.Printf)
	Expect(image).To(ContainSubstring(":"), "must-gather image %q should contain a tag separator", image)

	By("Creating artifact directory for must-gather output")

	// Deliberately not mustgather.CreateDestDir, which roots the raw tree in
	// ARTIFACT_DIR: that publishes ~5 MB of loose files per collection. The
	// tree is collected into a temporary directory and published below as one
	// tar.gz, which is both an order of magnitude smaller and the form a
	// support case asks for.
	destDir, mkdirErr := os.MkdirTemp(GinkgoT().TempDir(), collectionTempDirPrefix)
	Expect(mkdirErr).ToNot(HaveOccurred(), "Failed to create must-gather output directory")
	GinkgoWriter.Printf("must-gather output directory: %s\n", destDir)

	By("Running oc adm must-gather")

	testStartTime := time.Now()

	DeferCleanup(func() {
		By("Cleaning up leftover must-gather namespaces")
		mustgather.CleanupNamespaces(context.Background(), testStartTime,
			mustgather.DefaultCleanupTimeout, GinkgoWriter.Printf)
	})

	gatherCtx, cancel := context.WithTimeout(ctx, mustgather.DefaultContextTimeout)
	defer cancel()
	// Started before the collection and read after it: the gather pod's
	// namespace is deleted as soon as it finishes, so the image the kubelet
	// actually pulled can only be read while the collection is running.
	imageWatcher := mustgather.StartImageWatcher(gatherCtx, APIClient.K8sClient, GinkgoWriter.Printf)
	gatherErr := mustgather.Run(gatherCtx, image, destDir, mustgather.Options{
		ImageInfoTimeout: mustgather.DefaultImageInfoTimeout,
		OCTimeout:        mustgather.DefaultOCTimeout,
		SaveCommandLog:   true,
		HomeFallback:     true,
	}, GinkgoWriter.Printf)
	reportGatherImages(image, imageWatcher.Stop())
	archiveCollection(inventory, destDir)
	Expect(gatherErr).ToNot(HaveOccurred(), "oc adm must-gather failed")

	By("Locating the collected tree")

	gatherRoot, rootErr := mustgather.GatherRoot(destDir)
	Expect(rootErr).ToNot(HaveOccurred(), "Failed to locate must-gather output under %s", destDir)

	By("Verifying the collection script ran to completion")

	versionFile := filepath.Join(gatherRoot, mustgather.VersionFile)
	versionContent, versionErr := os.ReadFile(versionFile)
	Expect(versionErr).ToNot(HaveOccurred(),
		"The collection script did not write %s, so it exited before finishing", mustgather.VersionFile)
	GinkgoWriter.Printf("Collection image reported:\n%s\n", string(versionContent))

	return gatherRoot
}

// archiveCollection publishes the collected tree as a single tar.gz in the
// job's artifact directory, so a run leaves behind the diagnostics it
// gathered rather than only the verdict. With no artifact directory
// configured it still archives, into mustgather.DefaultArchiveDir.
//
// The artifact directory is only ever created and added to. Nothing here, or
// anywhere else in this suite, removes it or prunes what earlier runs left:
// archives accumulate across suites and across runs, which is what makes it
// possible to compare a failing collection against the last good one. Their
// names carry a UTC timestamp so they never collide. Pruning is the operator's
// call, not the test's -- do not add cleanup here.
//
// It runs before the collection's own error is asserted: a failed or partial
// gather is the one most worth keeping. An archiving problem is reported but
// never fails the spec -- losing the artifact must not turn a passing
// comparison into a failure, nor mask a real one.
func archiveCollection(inventory mustgather.Inventory, destDir string) {
	destination := mustgather.ArchiveDestination()
	if err := os.MkdirAll(destination, artifactDirPermissions); err != nil {
		GinkgoWriter.Printf("WARNING: cannot create artifact directory %s: %v\n", destination, err)
		AddReportEntry("must-gather-archive-failed", fmt.Sprintf("%s: %v", destination, err))

		return
	}

	archivePath := filepath.Join(destination, mustgather.ArchiveName(inventory.OperatorAcronyms()))
	size, err := mustgather.Archive(destDir, archivePath)
	if err != nil {
		GinkgoWriter.Printf("WARNING: failed to archive the collection: %v\n", err)
		AddReportEntry("must-gather-archive-failed", err.Error())

		return
	}

	GinkgoWriter.Printf("Collection archived to %s (%.1f MiB)\n",
		archivePath, float64(size)/mebibyte)
	AddReportEntry("must-gather-archive", archivePath)
}

// reportGatherImages logs which image the collection actually ran and records
// it as a report entry, so the JUnit artifact carries the provenance too. It
// runs whether or not the collection succeeded -- a failed gather is exactly
// when the question "which image was that?" needs an answer.
func reportGatherImages(requestedImage string, observed []mustgather.GatherPodImages) {
	GinkgoWriter.Printf("%s\n", mustgather.FormatGatherPodImages(requestedImage, observed))
	// The run log already has the full block; the report entry stays a single
	// line so it does not print the same thing twice under `ginkgo -v`.
	pulled := mustgather.PulledImages(observed)
	if len(pulled) == 0 {
		AddReportEntry("must-gather-image", requestedImage+" (pulled image not observed)")

		return
	}

	AddReportEntry("must-gather-image", requestedImage+" -> "+strings.Join(pulled, " "))
}

// verifyInventoryCollected is the comparison the suite exists for: every CRD
// definition and every live custom resource in the inventory must have a file
// in the collected tree.
func verifyInventoryCollected(gatherRoot string, inventory mustgather.Inventory) {
	GinkgoHelper()
	expected := inventory.ExpectedPaths()
	Expect(expected).ToNot(BeEmpty(), "Inventory produced no expected paths")
	missing := mustgather.MissingPaths(gatherRoot, expected)

	if len(missing) > 0 {
		logCollectedMedik8sPaths(gatherRoot)
	}

	Expect(missing).To(BeEmpty(),
		"The collection is missing %d of %d expected medik8s artifacts:\n%s",
		len(missing), len(expected), strings.Join(missing, "\n"))
	GinkgoWriter.Printf("All %d expected medik8s artifacts were collected\n", len(expected))
	logUnexpectedInstances(gatherRoot, inventory)
}

// verifyNodesCollected checks the node YAML the collection script gathers for
// every node, which is what makes the output usable for a remediation triage.
func verifyNodesCollected(ctx context.Context, gatherRoot string) {
	GinkgoHelper()
	listCtx, cancel := context.WithTimeout(ctx, medik8sparams.DefaultTimeout)
	defer cancel()
	nodeList, err := APIClient.CoreV1Interface.Nodes().List(listCtx, metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred(), "Failed to list cluster nodes")
	Expect(nodeList.Items).ToNot(BeEmpty(), "Cluster has no nodes")
	expected := make([]string, 0, len(nodeList.Items))
	for nodeIdx := range nodeList.Items {
		expected = append(expected, mustgather.NodePath(nodeList.Items[nodeIdx].Name))
	}

	missing := mustgather.MissingPaths(gatherRoot, expected)
	Expect(missing).To(BeEmpty(), "Node data was not collected for:\n%s", strings.Join(missing, "\n"))
	GinkgoWriter.Printf("Node data collected for all %d nodes\n", len(expected))
}

// verifyOperatorNamespaceCollected checks the operator namespace itself, its
// workloads and a log directory for every running controller pod.
func verifyOperatorNamespaceCollected(ctx context.Context, gatherRoot string) {
	GinkgoHelper()
	namespaceDir := path.Join("namespaces", medik8sparams.OperatorNs)
	expected := []string{
		path.Join(namespaceDir, medik8sparams.OperatorNs+".yaml"),
		path.Join(namespaceDir, "core", "pods.yaml"),
		path.Join(namespaceDir, "apps", "deployments.yaml"),
	}
	listCtx, cancel := context.WithTimeout(ctx, medik8sparams.DefaultTimeout)
	defer cancel()
	podList, err := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).
		List(listCtx, metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred(), "Failed to list pods in %s", medik8sparams.OperatorNs)
	Expect(podList.Items).ToNot(BeEmpty(), "No pods running in %s", medik8sparams.OperatorNs)

	for podIdx := range podList.Items {
		runningPod := &podList.Items[podIdx]
		// A pod being evicted during a destructive run can legitimately be gone
		// by the time the collection reaches it.
		if runningPod.DeletionTimestamp != nil {
			continue
		}

		expected = append(expected, path.Join(namespaceDir, "pods", runningPod.Name))
	}

	missing := mustgather.MissingPaths(gatherRoot, expected)
	Expect(missing).To(BeEmpty(),
		"Operator namespace data was not collected for:\n%s", strings.Join(missing, "\n"))
	GinkgoWriter.Printf("Operator namespace data collected, including %d pod directories\n",
		len(expected)-3)
}

// logUnexpectedInstances reports collected custom resource files that were not
// in the inventory. These are not failures -- a controller can create a CR
// while the collection runs -- but they explain a surprising output tree.
func logUnexpectedInstances(gatherRoot string, inventory mustgather.Inventory) {
	expected := make(map[string]bool)
	for _, relPath := range inventory.ExpectedPaths() {
		expected[relPath] = true
	}

	collected, walkErr := mustgather.CollectRelativePaths(gatherRoot)
	if walkErr != nil {
		GinkgoWriter.Printf("WARNING: failed to list collected paths: %v\n", walkErr)

		return
	}

	var extra []string

	for _, relPath := range collected {
		if strings.Contains(relPath, mustgather.CRDNameFilter) &&
			strings.HasSuffix(relPath, ".yaml") && !expected[relPath] {
			extra = append(extra, relPath)
		}
	}

	if len(extra) > 0 {
		sort.Strings(extra)
		GinkgoWriter.Printf("Collected %d medik8s file(s) not in the inventory "+
			"(created while the collection ran):\n%s\n", len(extra), strings.Join(extra, "\n"))
	}
}

// logCollectedMedik8sPaths dumps the medik8s part of the tree so a comparison
// failure can be diagnosed from the CI log without the output directory.
func logCollectedMedik8sPaths(gatherRoot string) {
	collected, walkErr := mustgather.CollectRelativePaths(gatherRoot)
	if walkErr != nil {
		GinkgoWriter.Printf("WARNING: failed to list collected paths in %s: %v\n", gatherRoot, walkErr)

		return
	}

	var medik8sPaths []string

	for _, relPath := range collected {
		if strings.Contains(relPath, mustgather.CRDNameFilter) {
			medik8sPaths = append(medik8sPaths, relPath)
		}
	}

	GinkgoWriter.Printf("Collection contains %d medik8s path(s) out of %d total:\n%s\n",
		len(medik8sPaths), len(collected), strings.Join(medik8sPaths, "\n"))
}

// selectDestructiveTargetNode picks a worker to disrupt, skipping the spec when
// the cluster is too small to lose one.
func selectDestructiveTargetNode(ctx context.Context) string {
	GinkgoHelper()
	workerCount, err := helpers.CountReadyWorkerNodes(ctx, APIClient)
	Expect(err).ToNot(HaveOccurred(), "Failed to count Ready worker nodes")

	if workerCount < collectionparams.MinWorkersForDestructive {
		skipWithReason(fmt.Sprintf(
			"needs at least %d Ready worker nodes to disrupt one, found %d",
			collectionparams.MinWorkersForDestructive, workerCount))
	}

	targetNode, selectErr := helpers.SelectWorkerNode(ctx, APIClient)
	Expect(selectErr).ToNot(HaveOccurred(), "Failed to select a worker node")
	GinkgoWriter.Printf("Target node: %s\n", targetNode.Name)

	return targetNode.Name
}

// nodeUnschedulable reports whether the node is cordoned.
func nodeUnschedulable(ctx context.Context, nodeName string) (bool, error) {
	node := &corev1.Node{}
	if err := APIClient.Get(ctx, runtimeclient.ObjectKey{Name: nodeName}, node); err != nil {
		return false, err
	}

	return node.Spec.Unschedulable, nil
}

// objectExists reports whether the object is readable through the API.
func objectExists(ctx context.Context, object *unstructured.Unstructured) (bool, error) {
	probe := &unstructured.Unstructured{}
	probe.SetGroupVersionKind(object.GroupVersionKind())
	err := APIClient.Get(ctx, runtimeclient.ObjectKeyFromObject(object), probe)
	if k8serrors.IsNotFound(err) {
		return false, nil
	}

	return err == nil, err
}

// waitForNodeSchedulable waits out the uncordon that follows NodeMaintenance
// deletion. A node left cordoned would break every later spec on this cluster,
// so a failure here is reported rather than swallowed.
func waitForNodeSchedulable(ctx context.Context, nodeName string) {
	err := wait.PollUntilContextTimeout(ctx,
		collectionparams.DefaultPollInterval, collectionparams.UncordonTimeout, true,
		func(pollCtx context.Context) (bool, error) {
			unschedulable, getErr := nodeUnschedulable(pollCtx, nodeName)
			if getErr != nil {
				return false, nil //nolint:nilerr // a transient read keeps polling
			}

			return !unschedulable, nil
		})
	if err != nil {
		GinkgoWriter.Printf("WARNING: node %s is still cordoned: %v\n", nodeName, err)
		AddReportEntry("uncordon-failed", fmt.Sprintf("node %s still cordoned: %v", nodeName, err))
	}
}

// skipWithReason skips the spec and records the reason as a report entry, so a
// skipped run is attributable from the JUnit artifact rather than only from the
// console log.
func skipWithReason(reason string) {
	GinkgoHelper()
	AddReportEntry("skipped-reason", reason)
	Skip(reason)
}
