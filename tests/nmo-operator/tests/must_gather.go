package tests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	nmov1beta1 "github.com/medik8s/node-maintenance-operator/api/v1beta1"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/internal/mustgather"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoparams"
)

var _ = Describe("NMO Must-Gather Tests",
	Serial,
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorNMO, nmoparams.Label,
		labels.TierAcceptance, labels.FrequencyWeekly),
	func() {
		var (
			ctx            context.Context
			nmoPodName     string
			nmoPodNode     string
			targetNodeName string
			nmCRName       string
			clusterNodes   []string
		)

		// The Python equivalent inherits an active maintenance from an incremental pytest class
		// (setup_class creates the NodeMaintenance, setup_method re-asserts it before every
		// test). Go has no ordering guarantee across files, so this BeforeAll establishes the
		// state and holds it. Without it must-gather still collects the CRD and node YAMLs and
		// every presence check would pass on an idle cluster.
		BeforeAll(func() {
			ctx = context.Background()

			By("Registering NMO API scheme")
			Expect(APIClient.AttachScheme(nmov1beta1.AddToScheme)).To(Succeed(),
				"Failed to register NMO scheme")

			By("Verifying NMO operator deployment is ready")

			nmoDeployment, err := deployment.Pull(
				APIClient, nmoparams.OperatorDeploymentName, medik8sparams.OperatorNs)
			Expect(err).ToNot(HaveOccurred(), "Failed to get NMO deployment")
			Expect(nmoDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
				"NMO deployment is not Ready")

			By("Verifying sufficient schedulable worker nodes")

			eligible, err := listSchedulableWorkers(ctx)
			Expect(err).ToNot(HaveOccurred(), "Failed to list schedulable worker nodes")

			if len(eligible) < nmoparams.MinWorkerNodesForMaintenance {
				Skip(fmt.Sprintf(
					"NMO must-gather test requires at least %d schedulable worker nodes, found %d",
					nmoparams.MinWorkerNodesForMaintenance, len(eligible)))
			}

			By("Identifying the NMO controller pod")

			nmoPodName, nmoPodNode = nmoControllerPod()
			GinkgoWriter.Printf("NMO controller pod %s runs on node %s\n", nmoPodName, nmoPodNode)

			By("Selecting a worker node that does not host the NMO controller")

			targetNodeName = selectSchedulableWorkerExcluding(ctx, nmoPodNode)
			nmCRName = nmoparams.NMNamePrefix + targetNodeName
			GinkgoWriter.Printf("Target node for must-gather test: %s\n", targetNodeName)

			DeferCleanup(func() {
				By("Cleaning up NodeMaintenance " + nmCRName)
				deleteNMBestEffort(context.Background(), nmCRName)

				By("Waiting for node " + targetNodeName + " to become Ready and uncordoned")
				waitForNodeRecoveryBestEffort(context.Background(), targetNodeName)
			})

			// The candidate pool can be a single node -- the gate admits two schedulable
			// workers and one of them may host the controller -- so the CR name is often
			// identical between runs. A run killed before DeferCleanup fires would then make
			// every later run fail on Create with AlreadyExists, before must-gather executes.
			By("Pre-cleaning a stale NodeMaintenance CR from a previous interrupted run")
			deleteNMBestEffort(ctx, nmCRName)

			By("Creating NodeMaintenance " + nmCRName + " for node " + targetNodeName)

			nmCR := newNodeMaintenanceWithReason(nmCRName, targetNodeName, nmoparams.MustGatherReason)
			Expect(APIClient.Create(ctx, nmCR)).To(Succeed(),
				"Failed to create NodeMaintenance %s for node %s", nmCRName, targetNodeName)

			By("Waiting for node " + targetNodeName + " to enter maintenance")
			assertMaintenanceSucceeded(ctx, nmCRName, targetNodeName)

			By("Verifying the SucceedMaintenance event was emitted")
			assertMaintenanceEvent(ctx, nmCRName, nmoparams.EventReasonSucceedMaintenance)

			By("Recording cluster node names before collection")
			clusterNodes = listAllNodeNames(ctx)
		})

		JustAfterEach(func() {
			if CurrentSpecReport().Failed() {
				GinkgoWriter.Println("Test failed - collecting NMO controller diagnostics")
				helpers.LogControllerState(ctx, APIClient,
					medik8sparams.OperatorNs, nmoparams.OperatorControllerPodLabels,
					GinkgoWriter.Printf)
			}
		})

		Context("must-gather", func() {
			It("collects NMO data via oc adm must-gather while a node is under maintenance",
				Label(labels.DisruptionDestructive, labels.PlatformAny, labels.ComponentController),
				reportxml.ID("48777"),
				func() {
					By("Creating temporary must-gather output directory")

					mustGatherDir, mkdirErr := os.MkdirTemp("", "nmo-must-gather-*")
					Expect(mkdirErr).ToNot(HaveOccurred(), "Failed to create must-gather output directory")

					DeferCleanup(func() {
						if removeErr := os.RemoveAll(mustGatherDir); removeErr != nil {
							GinkgoWriter.Printf("WARNING: failed to remove must-gather dir %s: %v\n",
								mustGatherDir, removeErr)
						}
					})

					By("Running oc adm must-gather")

					mustGatherImage := os.Getenv(mustgather.ImageEnvVar)
					if mustGatherImage == "" {
						mustGatherImage = mustgather.DefaultImage
					}

					Expect(mustgather.Run(ctx, mustGatherImage, mustGatherDir, mustgather.Options{
						CommandTimeout:   nmoparams.MustGatherTimeout,
						ImageInfoTimeout: mustgather.DefaultImageInfoTimeout,
					}, GinkgoWriter.Printf)).To(Succeed(), "oc adm must-gather failed")

					By("Validating must-gather output contains NMO data")
					assertNMOArtifactsCollected(mustGatherDir, clusterNodes, nmCRName, nmoPodName)

					By("Validating the collected NMO artifacts record the active maintenance")
					assertNMOArtifactContents(mustGatherDir, targetNodeName, nmCRName, nmoPodName)
				})
		})
	})

// nmoControllerPod returns the name of a running NMO controller pod and the node hosting it.
// The label selector alone is not NMO-specific: every RHWA operator carries
// control-plane=controller-manager in the shared operator namespace, so the pods are also
// filtered by the NMO deployment that owns them.
func nmoControllerPod() (string, string) {
	GinkgoHelper()

	var selected *pod.Builder

	Eventually(func() error {
		allPods, listErr := pod.List(APIClient, medik8sparams.OperatorNs,
			metav1.ListOptions{LabelSelector: nmoparams.OperatorControllerPodLabelSelector})
		if listErr != nil {
			return listErr
		}

		running := helpers.FilterRunningPods(
			helpers.FilterPodsByDeployment(allPods, nmoparams.OperatorDeploymentName))
		if len(running) == 0 {
			return fmt.Errorf("no running NMO controller pod found in %s", medik8sparams.OperatorNs)
		}

		selected = running[0]

		return nil
	}, medik8sparams.DefaultTimeout, nmoparams.DefaultPollInterval).Should(Succeed(),
		"A running NMO controller pod should be found")

	return selected.Object.Name, selected.Object.Spec.NodeName
}

// listAllNodeNames snapshots every node name in the cluster. It is taken before collection so
// a node joining during the gather cannot fail the per-node expectations.
func listAllNodeNames(ctx context.Context) []string {
	GinkgoHelper()
	nodeList := &corev1.NodeList{}
	Expect(APIClient.List(ctx, nodeList)).To(Succeed(), "Failed to list cluster nodes")
	Expect(nodeList.Items).ToNot(BeEmpty(), "Cluster reported no nodes")
	names := make([]string, 0, len(nodeList.Items))

	for i := range nodeList.Items {
		names = append(names, nodeList.Items[i].Name)
	}

	return names
}

// assertNMOArtifactsCollected mirrors the Python presence checks and the plan's `ls -R` step: a
// YAML for every node, the NodeMaintenance CR, the NMO CRD, and a log file under the controller
// pod's path. The log expectation requires an actual *.log file, because a path match on the
// pod name alone also passes when only <pod>.yaml was collected.
func assertNMOArtifactsCollected(baseDir string, clusterNodes []string, nmCRName, nmoPodName string) {
	GinkgoHelper()
	expectations := make([]mustgather.MustGatherExpectation, 0, len(clusterNodes)+3)

	for _, nodeName := range clusterNodes {
		expectations = append(expectations, mustgather.MustGatherExpectation{
			Description:  "YAML for node " + nodeName,
			PathContains: "nodes",
			NameGlob:     nodeName + ".yaml",
			MinCount:     1,
		})
	}

	expectations = append(expectations,
		mustgather.MustGatherExpectation{
			Description:  "NodeMaintenance CR " + nmCRName,
			PathContains: "nodemaintenances",
			NameGlob:     nmCRName + ".yaml",
			MinCount:     1,
		},
		mustgather.MustGatherExpectation{
			Description: "NMO CRD definition",
			NameGlob:    nmoparams.NMResourceQualified + ".yaml",
			MinCount:    1,
		},
		mustgather.MustGatherExpectation{
			Description:  "NMO controller pod log file",
			PathContains: nmoPodName,
			NameGlob:     "*.log",
			MinCount:     1,
		},
	)
	missingItems := mustgather.ValidateMustGatherContents(baseDir, expectations)
	Expect(missingItems).To(BeEmpty(),
		"Must-gather validation failed:\n%s", strings.Join(missingItems, "\n"))
}

// assertNMOArtifactContents reads the collected files, which MustGatherExpectation cannot do --
// it matches paths and filenames only. All checks run before reporting, mirroring the Python
// validator, so one miss does not hide the others.
func assertNMOArtifactContents(baseDir, targetNodeName, nmCRName, nmoPodName string) {
	GinkgoHelper()
	relPaths, walkErr := mustgather.CollectRelativePaths(baseDir)
	Expect(walkErr).ToNot(HaveOccurred(), "Failed to walk must-gather output directory")

	var failures []string

	By("Verifying the collected NMO CRD names the NodeMaintenance resource")

	failures = append(failures, checkCollectedText(baseDir, relPaths, "collected NMO CRD",
		[]string{nmoparams.NMResourceQualified},
		func(relPath string) bool {
			return filepath.Base(relPath) == nmoparams.NMResourceQualified+".yaml"
		})...)

	By("Verifying the collected node YAML carries both NoSchedule taints")

	failures = append(failures, checkCollectedNode(baseDir, relPaths, targetNodeName)...)

	By("Verifying the collected NodeMaintenance CR records a succeeded maintenance")

	failures = append(failures, checkCollectedNM(baseDir, relPaths, nmCRName)...)

	// Only the eviction message is asserted. The plan's step 10 also expects "Reconcile
	// completed", but the controller defer-logs that on every reconcile of any
	// NodeMaintenance, so its presence discriminates nothing; the plan's `tail -n 2`
	// recency, which is what would have made it meaningful, is not reproducible here.
	By("Verifying the collected NMO controller log reports the completed drain")

	failures = append(failures, checkCollectedText(baseDir, relPaths, "collected NMO controller log",
		[]string{nmoparams.ControllerLogEvictedText},
		func(relPath string) bool {
			return strings.Contains(relPath, nmoPodName) &&
				strings.Contains(relPath, "/"+nmoparams.ManagerContainerName+"/") &&
				filepath.Base(relPath) == nmoparams.ControllerLogFileName
		})...)

	Expect(failures).To(BeEmpty(),
		"Must-gather content validation failed:\n%s", strings.Join(failures, "\n"))
}

// readCollectedFile returns the contents of the first collected regular file whose relative
// path satisfies match, or a one-entry failure list explaining why it could not be read.
// Matches are sorted so the choice is deterministic when more than one file qualifies.
func readCollectedFile(
	baseDir string, relPaths []string, description string, match func(relPath string) bool,
) ([]byte, []string) {
	var matched []string

	for _, relPath := range relPaths {
		if !match(relPath) {
			continue
		}

		fullPath := filepath.Join(baseDir, relPath)
		info, statErr := os.Stat(fullPath)
		if statErr != nil {
			return nil, []string{fmt.Sprintf("%s: cannot stat %s: %v", description, fullPath, statErr)}
		}

		if !info.IsDir() {
			matched = append(matched, fullPath)
		}
	}

	if len(matched) == 0 {
		return nil, []string{description + ": must-gather collected no matching file"}
	}

	sort.Strings(matched)
	data, readErr := os.ReadFile(matched[0])
	if readErr != nil {
		return nil, []string{fmt.Sprintf("%s: cannot read %s: %v", description, matched[0], readErr)}
	}

	return data, nil
}

// checkCollectedText reports which of wants are absent from the matched collected file.
func checkCollectedText(
	baseDir string, relPaths []string, description string, wants []string, match func(relPath string) bool,
) []string {
	data, failures := readCollectedFile(baseDir, relPaths, description, match)
	if failures != nil {
		return failures
	}

	var missing []string

	for _, want := range wants {
		if !strings.Contains(string(data), want) {
			missing = append(missing, fmt.Sprintf("%s: does not contain %q", description, want))
		}
	}

	return missing
}

// checkCollectedNode parses the collected node YAML and asserts both NoSchedule taints and the
// cordon. The Python matches one exact two-line literal; YAML guarantees no ordering or
// indentation, so that would fail on a serializer change for the wrong reason. Parsing keeps
// the effect asserted and bound to its key, which a substring match cannot express.
func checkCollectedNode(baseDir string, relPaths []string, nodeName string) []string {
	description := "collected YAML for node under maintenance " + nodeName

	data, failures := readCollectedFile(baseDir, relPaths, description, func(relPath string) bool {
		return strings.Contains(relPath, "nodes") && filepath.Base(relPath) == nodeName+".yaml"
	})
	if failures != nil {
		return failures
	}

	node := &corev1.Node{}
	if err := yaml.Unmarshal(data, node); err != nil {
		return []string{fmt.Sprintf("%s: cannot parse: %v", description, err)}
	}

	var missing []string

	for _, key := range []string{nmoparams.DrainTaintKey, nmoparams.UnschedulableTaintKey} {
		if !hasNoScheduleTaint(node.Spec.Taints, key) {
			missing = append(missing, fmt.Sprintf("%s: missing NoSchedule taint %q", description, key))
		}
	}

	if !node.Spec.Unschedulable {
		missing = append(missing, description+": spec.unschedulable is not true")
	}

	return missing
}

// hasNoScheduleTaint reports whether taints carry key with a NoSchedule effect.
func hasNoScheduleTaint(taints []corev1.Taint, key string) bool {
	for _, taint := range taints {
		if taint.Key == key && taint.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}

	return false
}

// checkCollectedNM parses the collected NodeMaintenance CR and asserts the Succeeded phase.
func checkCollectedNM(baseDir string, relPaths []string, nmCRName string) []string {
	description := "collected NodeMaintenance CR " + nmCRName

	data, failures := readCollectedFile(baseDir, relPaths, description, func(relPath string) bool {
		return filepath.Base(relPath) == nmCRName+".yaml"
	})
	if failures != nil {
		return failures
	}

	nmCR := &nmov1beta1.NodeMaintenance{}
	if err := yaml.Unmarshal(data, nmCR); err != nil {
		return []string{fmt.Sprintf("%s: cannot parse: %v", description, err)}
	}

	if nmCR.Status.Phase != nmov1beta1.MaintenanceSucceeded {
		return []string{fmt.Sprintf("%s: phase is %q, want %q",
			description, nmCR.Status.Phase, nmov1beta1.MaintenanceSucceeded)}
	}

	return nil
}
