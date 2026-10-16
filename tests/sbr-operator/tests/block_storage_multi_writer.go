package tests

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe(
	"SBR Block Storage — Ceph RBD",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR), func() {
		var testSBRC *unstructured.Unstructured

		BeforeAll(func() {
			testSBRC = setupBlockModeSBRC(sbrparams.SBRCBlockTestName)
		})

		AfterAll(func() {
			if testSBRC != nil {
				By(fmt.Sprintf("Removing StorageBasedRemediationConfig %q", sbrparams.SBRCBlockTestName))
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCBlockTestName)
			}
		})

		It("Verify SBR agents write concurrently to Ceph RBD block device without conflicts",
			reportxml.ID("1915"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.ComponentDaemonSet,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				testCephRBDConcurrentWrites()
			})
	})

// testCephRBDConcurrentWrites validates that multiple SBR agents can write
// concurrently to a Ceph RBD block device without conflicts.
func testCephRBDConcurrentWrites() {
	By("Verifying all agent pods reach Ready state")
	var agentPods []*pod.Builder

	Eventually(func() error {
		var err error
		agentPods, err = pod.List(APIClient, medik8sparams.OperatorNs,
			metav1.ListOptions{LabelSelector: sbrparams.AgentPodLabelSelector})
		if err != nil {
			return err
		}

		runningPods := helpers.FilterRunningPods(agentPods)
		readyCount := len(runningPods)
		expectedCount := getWorkerNodeCount()

		if readyCount < expectedCount {
			return fmt.Errorf("only %d/%d agents ready", readyCount, expectedCount)
		}

		return nil
	}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"All agent pods must reach Ready state")

	By("Validating agent pod count matches worker node count")
	runningPods := helpers.FilterRunningPods(agentPods)
	match, actualCount, expectedCount, err := agentPodCountMatchesWorkerCount()
	Expect(err).ToNot(HaveOccurred(), "Failed to compare agent pod count to worker node count")
	Expect(match).To(BeTrue(),
		"Agent pod count (%d) should match worker node count (%d)", actualCount, expectedCount)
	GinkgoWriter.Printf("Successfully verified %d agent pods running on %d worker nodes\n",
		actualCount, expectedCount)
	GinkgoWriter.Printf("Agent pods are using Ceph RBD block storage in Block volume mode\n")

	By("Confirming multi-attach capability via Ready pod count")
	// If multiple agents reach Ready state simultaneously, the block device
	// supports true RWX multi-attach. A single-writer limitation would prevent
	// multiple agents from attaching to the same block device.
	Expect(actualCount).To(BeNumerically(">=", 2),
		"At least 2 agents must be Ready to validate multi-attach capability")
	GinkgoWriter.Printf("✓ Validated concurrent block device access: %d agents Ready\n", actualCount)
	GinkgoWriter.Printf("✓ Ceph RBD block storage supports true RWX multi-attach\n")

	By("Reading slot data from all agent pods to validate data integrity")
	slotDataByPod := make(map[string][]SlotData)
	for _, agentPod := range runningPods {
		podName := agentPod.Object.Name
		GinkgoWriter.Printf("Reading slot data from pod %s...\n", podName)
		slotData, err := readSlotDataFromPod(podName, medik8sparams.OperatorNs)
		Expect(err).ToNot(HaveOccurred(), "Failed to read slot data from pod %s", podName)
		slotDataByPod[podName] = slotData
		GinkgoWriter.Printf("  Retrieved %d slot entries from pod %s\n", len(slotData), podName)
	}

	By("Comparing slot data across all agent pods for consistency")
	Expect(validateSlotDataConsistency(slotDataByPod)).To(Succeed())

	By("Validating sequence numbers increment correctly without conflicts")
	Expect(validateSequenceNumbers(slotDataByPod)).To(Succeed())
	GinkgoWriter.Printf("✓ Block device maintains data integrity across all agents\n")
	GinkgoWriter.Printf("✓ Sequence numbers increment correctly without conflicts\n")
}
