package tests

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// agentBaseline is one SBR agent's state before its node is put under CPU load.
type agentBaseline struct {
	nodeName     string
	agentPodName string
	bootID       string
	selfFenced   float64
	ioErrors     float64
}

// cpuStressScript saturates every CPU the container can see with shell busy loops for the given
// duration and then exits. The pods are removed in DeferCleanup; the duration bounds the load if
// the test dies before cleaning up, the same way the partition disruptor bounds its own outage.
func cpuStressScript(duration time.Duration) string {
	return fmt.Sprintf(`set -e
cpus=$(nproc)
started=0
while [ "$started" -lt "$cpus" ]; do
  (while :; do :; done) &
  started=$((started + 1))
done
echo "started $cpus busy loops for %ds"
sleep %d
exit 0
`, int(duration.Seconds()), int(duration.Seconds()))
}

var _ = Describe(
	"SBR Block Storage — Ceph RBD Heartbeat Stability Under Load",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR, labels.ComponentDaemonSet), func() {
		var (
			testSBRC        *unstructured.Unstructured
			observerPodName string
		)

		BeforeAll(func() {
			requireNoSBRNodeHealthCheck()

			// The minimum sbrTimeoutSeconds (10) halves to a 5s heartbeat interval, the tightest
			// write deadline the CRD allows. If CPU contention can delay a heartbeat into a false
			// storage-loss verdict, this is the configuration where it shows.
			testSBRC = setupBlockModeSBRCWithSpec(sbrparams.SBRCBlockLoadTestName,
				map[string]interface{}{"sbrTimeoutSeconds": int64(sbrparams.SBRCTimeoutSecondsMin)})

			By("Selecting an observer agent pod to read the shared device through")

			agentPods, listErr := sbrcAgentPods(sbrparams.SBRCBlockLoadTestName)
			Expect(listErr).ToNot(HaveOccurred(), "Failed to list the agent pods of SBRC %q",
				sbrparams.SBRCBlockLoadTestName)
			Expect(agentPods).ToNot(BeEmpty(), "SBRC %q must have at least one Running agent pod",
				sbrparams.SBRCBlockLoadTestName)
			observerPodName = agentPods[0].Object.Name
			GinkgoWriter.Printf("Observer agent pod: %q\n", observerPodName)
		})

		AfterAll(func() {
			if testSBRC != nil {
				By(fmt.Sprintf("Removing StorageBasedRemediationConfig %q", sbrparams.SBRCBlockLoadTestName))
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCBlockLoadTestName)
			}
		})

		It("Verify SBR heartbeats remain stable on Ceph RBD under load",
			reportxml.ID("1925"),
			Label(
				labels.OperatorSBR,
				labels.TierResiliency,
				labels.ComponentDaemonSet,
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.FrequencyWeekly,
			), func() {
				baselines := captureAgentBaselines()
				sequences := readBaselineSequences(observerPodName)
				startCPULoad(baselines)
				assertNoAgentUnhealthyUnderLoad(baselines)
				assertHeartbeatsAdvanced(observerPodName, sequences)
				assertNothingRemediatedUnderLoad(baselines)
				assertNoAgentSelfFencedUnderLoad(baselines)
			})
	})

// captureAgentBaselines records, for every node running an agent of the load-test SBRC, the state
// the load must leave untouched. Each agent is scraped once and both counters are parsed out of
// that one body.
func captureAgentBaselines() []agentBaseline {
	agentPods, listErr := sbrcAgentPods(sbrparams.SBRCBlockLoadTestName)
	Expect(listErr).ToNot(HaveOccurred(), "Failed to list the agent pods of SBRC %q",
		sbrparams.SBRCBlockLoadTestName)
	Expect(agentPods).ToNot(BeEmpty(), "SBRC %q must have at least one Running agent pod",
		sbrparams.SBRCBlockLoadTestName)
	baselines := make([]agentBaseline, 0, len(agentPods))
	metricBodies := make(map[string]string, len(agentPods))

	By("Recording the boot-id of every node running an SBR agent")

	for _, agentPod := range agentPods {
		nodeName := agentPod.Object.Spec.NodeName
		bootID, bootErr := getNodeBootID(nodeName)
		Expect(bootErr).ToNot(HaveOccurred(), "Must read the boot-id of node %q", nodeName)
		baselines = append(baselines, agentBaseline{
			nodeName:     nodeName,
			agentPodName: agentPod.Object.Name,
			bootID:       bootID,
		})
	}

	By("Recording each agent's sbr_self_fenced_total counter")

	for idx := range baselines {
		body, fetchErr := fetchAgentMetrics(baselines[idx].agentPodName)
		Expect(fetchErr).ToNot(HaveOccurred(),
			"Must scrape the metrics of agent pod %q", baselines[idx].agentPodName)
		metricBodies[baselines[idx].agentPodName] = body
		baselines[idx].selfFenced = agentCounter(body,
			baselines[idx].agentPodName, sbrparams.SelfFencedMetricName)
	}

	By("Recording each agent's sbr_device_io_errors_total counter")

	for idx := range baselines {
		baselines[idx].ioErrors = agentCounter(metricBodies[baselines[idx].agentPodName],
			baselines[idx].agentPodName, sbrparams.DeviceIOErrorsMetricName)
	}

	return baselines
}

// agentCounter parses one counter out of an already scraped metrics body, failing the spec when it
// is missing or unparseable.
func agentCounter(body, agentPodName, metricName string) float64 {
	value, parseErr := sumPrometheusSamples(body, metricName)
	Expect(parseErr).ToNot(HaveOccurred(),
		"Must read %s from agent pod %q", metricName, agentPodName)

	return value
}

// readBaselineSequences records where every node's heartbeat slot stands before the load starts.
func readBaselineSequences(observerPodName string) map[uint16]uint64 {
	By("Recording every node's heartbeat sequence number through the observer agent")

	sequences, seqErr := heartbeatSequences(observerPodName)
	Expect(seqErr).ToNot(HaveOccurred(),
		"Must read the heartbeat region through observer pod %q", observerPodName)
	Expect(sequences).ToNot(BeEmpty(), "The heartbeat region must hold at least one written slot")
	GinkgoWriter.Printf("Baseline heartbeat sequences by node ID: %v\n", sequences)

	return sequences
}

// startCPULoad saturates every agent node and registers the teardown that removes the load again.
func startCPULoad(baselines []agentBaseline) {
	By("Starting a CPU stress pod on every node running an SBR agent")

	stressPods := make([]*pod.Builder, 0, len(baselines))
	stressPodNames := make([]string, 0, len(baselines))

	for _, baseline := range baselines {
		stressPodName := sbrparams.BlockStressPodPrefix + baseline.nodeName
		stressPodNames = append(stressPodNames, stressPodName)
		stressPods = append(stressPods, createHostCommandPod(baseline.nodeName, stressPodName,
			cpuStressScript(sbrparams.BlockLoadStressDuration)))
	}

	DeferCleanup(func() {
		By("DeferCleanup: removing the CPU stress pods")

		for _, stressPodName := range stressPodNames {
			deleteHostCommandPod(stressPodName)
		}
	})

	By("Waiting for every stress pod to be Running")

	for idx, stressPod := range stressPods {
		Expect(stressPod.WaitUntilRunning(medik8sparams.DefaultTimeout)).To(Succeed(),
			"CPU stress pod %q must start on node %q", stressPodNames[idx], baselines[idx].nodeName)
	}
}

// assertNoAgentUnhealthyUnderLoad is the heart of the test: under saturated CPUs, no agent may miss
// enough heartbeats for a peer to call its node storage-unhealthy.
func assertNoAgentUnhealthyUnderLoad(baselines []agentBaseline) {
	By(fmt.Sprintf("Consistently asserting for %s that no node reports SBRStorageUnhealthy=True",
		sbrparams.BlockLoadCheckDuration))

	Consistently(func() error {
		for _, baseline := range baselines {
			if condErr := transientNodeHasCondition(baseline.nodeName, false); condErr != nil {
				return condErr
			}
		}

		return nil
	}, sbrparams.BlockLoadCheckDuration, sbrparams.BlockLoadCheckInterval).Should(Succeed(),
		"CPU load must not delay any agent's heartbeats enough to be read as storage loss")
}

// assertHeartbeatsAdvanced proves the agents kept writing, rather than all going quiet together in
// a way no peer would flag.
func assertHeartbeatsAdvanced(observerPodName string, baselineSequences map[uint16]uint64) {
	By("Asserting every node's heartbeat sequence number advanced through the observer agent")

	underLoad, seqErr := heartbeatSequences(observerPodName)
	Expect(seqErr).ToNot(HaveOccurred(),
		"Must read the heartbeat region through observer pod %q", observerPodName)
	stalled := stalledNodeIDs(baselineSequences, underLoad)
	Expect(stalled).To(BeEmpty(),
		"Every agent must keep writing heartbeats under load, but node IDs %v did not advance "+
			"(baseline %v, under load %v)", stalled, baselineSequences, underLoad)
	GinkgoWriter.Printf("Heartbeat sequences under load by node ID: %v\n", underLoad)
}

// assertNothingRemediatedUnderLoad checks the cluster-level outcome: no remediation was created and
// no node was rebooted behind the test's back.
func assertNothingRemediatedUnderLoad(baselines []agentBaseline) {
	By("Asserting no StorageBasedRemediation CR exists")

	crCount, countErr := transientSBRCRCount()
	Expect(countErr).ToNot(HaveOccurred(), "Failed to count StorageBasedRemediation CRs")
	Expect(crCount).To(BeZero(),
		"CPU load must not cause any StorageBasedRemediation CR to be created")

	By("Asserting every node still has the boot-id it started with")

	for _, baseline := range baselines {
		currentBootID, bootErr := getNodeBootID(baseline.nodeName)
		Expect(bootErr).ToNot(HaveOccurred(), "Must read the boot-id of node %q", baseline.nodeName)
		Expect(currentBootID).To(Equal(baseline.bootID),
			"Node %q must not reboot under CPU load: boot-id went from %s to %s",
			baseline.nodeName, baseline.bootID, currentBootID)
	}
}

// assertNoAgentSelfFencedUnderLoad closes the loop from the agents' own side: nobody self-fenced while
// its node was saturated. The device I/O-error counter is logged, not asserted: it increments on
// any transient failed read/write before the retry succeeds, so a single blip under saturation does
// not mean a false storage-loss verdict. That thesis is carried by sbr_self_fenced_total and the
// SBRStorageUnhealthy check instead.
func assertNoAgentSelfFencedUnderLoad(baselines []agentBaseline) {
	metricBodies := make(map[string]string, len(baselines))

	By("Asserting each agent's sbr_self_fenced_total counter is unchanged")

	for _, baseline := range baselines {
		body, fetchErr := fetchAgentMetrics(baseline.agentPodName)
		Expect(fetchErr).ToNot(HaveOccurred(),
			"Must scrape the metrics of agent pod %q", baseline.agentPodName)
		metricBodies[baseline.agentPodName] = body
		selfFenced := agentCounter(body, baseline.agentPodName, sbrparams.SelfFencedMetricName)
		Expect(selfFenced).To(Equal(baseline.selfFenced),
			"Agent pod %q on node %q must not have self-fenced: %s went from %v to %v",
			baseline.agentPodName, baseline.nodeName, sbrparams.SelfFencedMetricName,
			baseline.selfFenced, selfFenced)
	}

	By("Logging each agent's sbr_device_io_errors_total delta under load")

	for _, baseline := range baselines {
		ioErrors := agentCounter(metricBodies[baseline.agentPodName], baseline.agentPodName,
			sbrparams.DeviceIOErrorsMetricName)
		GinkgoWriter.Printf("Agent pod %q on node %q: %s went from %v to %v under load (delta %v)\n",
			baseline.agentPodName, baseline.nodeName, sbrparams.DeviceIOErrorsMetricName,
			baseline.ioErrors, ioErrors, ioErrors-baseline.ioErrors)
	}
}
