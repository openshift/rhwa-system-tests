package tests

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type nodeMapCleanupTest struct {
	config     *unstructured.Unstructured
	claim      *corev1.PersistentVolumeClaim
	observer   *pod.Builder
	agentNodes map[string]bool
}

var _ = Describe("SBR Functional - Node map cleanup", Ordered, Serial,
	Label(labels.OperatorSBR, labels.TierAcceptance, labels.ComponentController,
		labels.DisruptionNonDestructive, labels.PlatformAny, labels.FrequencyNightly,
		labels.TopologyMinimalWorker), func() {
		var storageClass string
		BeforeAll(func(ctx SpecContext) {
			storageClass = nodeMapCleanupPreflight(ctx)
		})
		It("Verify SBRC deletion clears the node map before recreation on the same volume",
			reportxml.ID("1929"), func(ctx SpecContext) {
				test := &nodeMapCleanupTest{config: buildSBRC("", map[string]interface{}{
					"sharedStorageClass": storageClass,
				})}
				test.config.SetGenerateName(sbrparams.SBRCNodeMapTestName + "-")
				test.config.SetLabels(map[string]string{sbrparams.NodeMapTestLabel: "true"})
				DeferCleanup(test.cleanupClaim, NodeTimeout(sbrparams.NodeMapCleanupTimeout))

				By("Creating the StorageBasedRemediationConfig and waiting for its agent DaemonSet to be ready")
				Expect(APIClient.Create(ctx, test.config)).To(Succeed())
				DeferCleanup(test.cleanupConfig, NodeTimeout(sbrparams.NodeMapCleanupTimeout))
				waitForSBRCReady(test.config.GetName())
				test.mountOriginalClaim(ctx)

				By("Recording the node map entries and the agent pod restart counts before deletion")
				var payload string
				Eventually(func(assertion Gomega) {
					snapshot := test.agentSnapshot(ctx, assertion)
					nodeMap, recorded, err := test.readNodeMap()
					assertion.Expect(err).NotTo(HaveOccurred())
					assertion.Expect(nodeMap.Entries).To(HaveLen(len(snapshot)))
					for node := range test.agentNodes {
						assertion.Expect(nodeMap.Entries).To(HaveKey(node), "Agent node %s must own a node-map entry", node)
					}
					payload = recorded
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed())
				AddReportEntry("node-map-before-deletion", payload, ReportEntryVisibilityFailureOrVerbose)

				By("Deleting the StorageBasedRemediationConfig and waiting for the cleanup job to complete")
				test.deleteAndRetainClaim(ctx)

				By("Verifying the node map is absent or empty on the original shared storage")
				test.expectNodeMapCleared(ctx)

				By("Recreating the StorageBasedRemediationConfig with the name and spec captured in step 1")
				recreated := buildSBRC(test.config.GetName(), nil)
				recreated.Object["spec"] = test.config.DeepCopy().Object["spec"]
				recreated.SetLabels(map[string]string{sbrparams.NodeMapTestLabel: "true"})
				started := time.Now()
				Expect(APIClient.Create(ctx, recreated)).To(Succeed())
				Expect(recreated.GetUID()).NotTo(Equal(test.config.GetUID()))
				test.config = recreated
				By("Asserting every agent pod reaches Running and Ready with at most one restart")
				var restarts map[types.UID]int32
				Eventually(func(assertion Gomega) {
					test.expectOriginalClaim(ctx, assertion)
					restarts = test.agentSnapshot(ctx, assertion)
				}, sbrparams.NodeMapRecreateReadyTimeout, sbrparams.DefaultPollInterval).Should(Succeed())
				AddReportEntry("recreated-agent-restart-counts", restarts)
				AddReportEntry("recreate-ready-elapsed", time.Since(started))

				By("Consistently verifying no agent pod restarts during the convergence window")
				Consistently(func(assertion Gomega) {
					assertion.Expect(test.agentSnapshot(ctx, assertion)).To(Equal(restarts),
						"Every recreated agent must keep its UID and restart count")
				}, sbrparams.NodeMapConvergenceDuration, sbrparams.DefaultPollInterval).Should(Succeed())
				By("Verifying no agent logs a peer-unhealthy event for a node that is not an active target")
				test.verifyPeerLogs(ctx)
				By("Verifying the recreated SBRC reports Ready")
				test.agentSnapshot(ctx, Default)
			})
	})

func nodeMapCleanupPreflight(ctx context.Context) string {
	By("Checking the installed SBR operator and requiring an isolated test configuration")
	controller, err := deployment.Pull(APIClient, sbrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
	Expect(err).NotTo(HaveOccurred())
	Expect(controller.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue())
	images := make(map[string]string)
	for _, container := range controller.Object.Spec.Template.Spec.Containers {
		images[container.Name] = container.Image
	}
	AddReportEntry("sbr-controller-images", images)
	requireNoSBRNodeHealthCheck()
	By("Verifying no SNR agents are running or scheduled")
	daemonSets, err := APIClient.DaemonSets("").List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred(), "SNR agent DaemonSets must be discoverable")
	for _, daemonSet := range daemonSets.Items {
		if daemonSet.Name == "self-node-remediation-ds" {
			Skip("SNR agents must be disabled before this test because SNR and SBR both use /dev/watchdog. " +
				"The SNR operator can remain installed.")
		}
	}
	By("Removing only node-map test resources left by an interrupted run")
	cleanupStaleNodeMapResources(ctx)
	configs := medik8sparams.NewUnstructuredList(sbrparams.CRDGroup, sbrparams.CRDVersion,
		"StorageBasedRemediationConfigList")
	Expect(APIClient.List(ctx, configs, client.InNamespace(medik8sparams.OperatorNs))).To(Succeed())
	Expect(configs.Items).To(BeEmpty(), "Remove existing SBRCs before running this isolated regression test")
	nodes, err := APIClient.CoreV1Interface.Nodes().List(ctx,
		metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/worker"})
	Expect(err).NotTo(HaveOccurred())
	var readyWorkers int
	for idx := range nodes.Items {
		if isNodeSchedulable(&nodes.Items[idx]) {
			readyWorkers++
		}
	}
	if readyWorkers < 2 {
		Skip("Node map cleanup requires at least two Ready, schedulable workers")
	}

	storageClass := discoverRWXStorageClass()
	class, err := APIClient.StorageV1Interface.StorageClasses().Get(ctx, storageClass, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "The selected RWX StorageClass must exist")
	AddReportEntry("node-map-storage-class", class.Name)
	Expect(class.ReclaimPolicy).NotTo(BeNil())
	AddReportEntry("node-map-storage-reclaim-policy", *class.ReclaimPolicy)

	return storageClass
}

func (test *nodeMapCleanupTest) expectNodeMapCleared(ctx context.Context) {
	test.expectOriginalClaim(ctx, Default)
	// SBR's O_CREATE lock can recreate an empty file during shutdown, but no
	// mapping data may survive. Require readable original storage, not an empty mount.
	output, err := test.observer.ExecCommandWithTimeout([]string{"sh", "-ec", `
device=/sbr-data/sbr-device
node_map=/sbr-data/sbr-device-nodemap

test -r /sbr-data && test -x /sbr-data
test -s "$device"
dd if="$device" of=/dev/null bs=1 count=1 status=none

if [ -e "$node_map" ] || [ -L "$node_map" ]; then
    test -f "$node_map"
    test -r "$node_map"
    if [ -s "$node_map" ]; then
        echo 'RHWA-1056: nonempty node map remains after cleanup and agent shutdown'
        ls -l "$node_map"
        exit 1
    fi
fi

echo 'Node map absent or empty on the original volume'
`}, sbrparams.NodeMapExecTimeout)
	AddReportEntry("node-map-after-deletion", output.String())
	Expect(err).NotTo(HaveOccurred(), "Node map cleanup failed: %s", output.String())
}

func (test *nodeMapCleanupTest) readNodeMap() (*blockNodeMap, string, error) {
	output, err := test.observer.ExecCommandWithTimeout(
		[]string{"base64", "/sbr-data/sbr-device-nodemap"}, sbrparams.NodeMapExecTimeout)
	if err != nil {
		return nil, "", fmt.Errorf("read original node map: %w: %s", err, strings.TrimSpace(output.String()))
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(output.String()))
	if err != nil {
		return nil, "", fmt.Errorf("decode original node map: %w", err)
	}
	if len(payload) < nodeMapJSONCRCSize {
		return nil, "", fmt.Errorf("node map is too short: %d bytes", len(payload))
	}

	// Filesystem and block modes use the same CRC32-prefixed NodeMapTable payload.
	return parseNodeMapPayload(payload)
}

func (test *nodeMapCleanupTest) agentSnapshot(ctx context.Context, assertion Gomega) map[types.UID]int32 {
	daemonSet, err := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(ctx,
		sbrparams.SBRAgentDaemonSetPrefix+test.config.GetName(), metav1.GetOptions{})
	assertion.Expect(err).NotTo(HaveOccurred())
	assertion.Expect(daemonSet.Status.ObservedGeneration).To(BeNumerically(">=", daemonSet.Generation))
	assertion.Expect(daemonSet.Status.DesiredNumberScheduled).To(BeNumerically(">=", 2))
	assertion.Expect(daemonSet.Status.NumberReady).To(Equal(daemonSet.Status.DesiredNumberScheduled))
	selector, err := metav1.LabelSelectorAsSelector(daemonSet.Spec.Selector)
	assertion.Expect(err).NotTo(HaveOccurred())
	pods, err := APIClient.Pods(medik8sparams.OperatorNs).List(ctx,
		metav1.ListOptions{LabelSelector: selector.String()})
	assertion.Expect(err).NotTo(HaveOccurred())
	assertion.Expect(pods.Items).To(HaveLen(int(daemonSet.Status.DesiredNumberScheduled)))
	restarts := make(map[types.UID]int32, len(pods.Items))
	nodes := make(map[string]bool, len(pods.Items))
	for _, agent := range pods.Items {
		assertion.Expect(metav1.IsControlledBy(&agent, daemonSet)).To(BeTrue())
		assertion.Expect(agent.DeletionTimestamp).To(BeNil())
		assertion.Expect(agent.Status.Phase).To(Equal(corev1.PodRunning))
		assertion.Expect(agent.Status.Conditions).To(ContainElement(And(
			HaveField("Type", corev1.PodReady), HaveField("Status", corev1.ConditionTrue))))
		assertion.Expect(agent.Status.ContainerStatuses).To(HaveLen(len(agent.Spec.Containers)))
		for _, status := range agent.Status.ContainerStatuses {
			assertion.Expect(status.Ready).To(BeTrue())
			assertion.Expect(status.State.Running).NotTo(BeNil())
			restarts[agent.UID] += status.RestartCount
		}
		assertion.Expect(restarts[agent.UID]).To(BeNumerically("<=", sbrparams.NodeMapMaxAgentRestarts),
			"Agent %s must stay within the restart budget", agent.Name)
		nodes[agent.Spec.NodeName] = true
	}
	assertion.Expect(nodes).To(HaveLen(len(pods.Items)), "Agents must run on distinct nodes")
	config := buildSBRC(test.config.GetName(), nil)
	assertion.Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(config), config)).To(Succeed())
	assertion.Expect(config.GetUID()).To(Equal(test.config.GetUID()))
	assertion.Expect(config.Object["status"]).To(HaveKeyWithValue("conditions", ContainElement(And(
		HaveKeyWithValue("type", "Ready"), HaveKeyWithValue("status", "True")))))
	test.agentNodes = nodes

	return restarts
}

func (test *nodeMapCleanupTest) verifyPeerLogs(ctx context.Context) {
	nodeMap, payload, err := test.readNodeMap()
	Expect(err).NotTo(HaveOccurred(), "The recreated node map must be readable")
	Expect(nodeMap.Entries).To(HaveLen(len(test.agentNodes)))
	activeIDs := make(map[uint16]bool, len(test.agentNodes))
	for node := range test.agentNodes {
		Expect(nodeMap.Entries).To(HaveKey(node), "Agent node %s must have a mapping", node)
		activeIDs[nodeMap.Entries[node].NodeID] = true
	}
	AddReportEntry("node-map-after-recreation", payload, ReportEntryVisibilityFailureOrVerbose)
	agents, err := APIClient.Pods(medik8sparams.OperatorNs).List(ctx,
		metav1.ListOptions{LabelSelector: "app=sbr-agent,sbrconfig=" + test.config.GetName()})
	Expect(err).NotTo(HaveOccurred())
	Expect(agents.Items).To(HaveLen(len(test.agentNodes)))
	since := test.config.GetCreationTimestamp()
	for _, agent := range agents.Items {
		logContext, cancel := context.WithTimeout(ctx, sbrparams.NodeMapExecTimeout)
		logs, logErr := APIClient.Pods(medik8sparams.OperatorNs).GetLogs(agent.Name, &corev1.PodLogOptions{
			Container: "sbr-agent", SinceTime: &since,
		}).DoRaw(logContext)
		cancel()
		Expect(logErr).NotTo(HaveOccurred(), "Logs for agent %s must be readable", agent.Name)
		AddReportEntry("agent-log-"+agent.Name, string(logs), ReportEntryVisibilityFailureOrVerbose)
		for _, line := range strings.Split(string(logs), "\n") {
			if !strings.Contains(line, sbrparams.NodeMapPeerUnhealthyMessage) {
				continue
			}
			var event struct{ NodeID uint16 }
			Expect(json.Unmarshal([]byte(line), &event)).To(Succeed(), "Peer-health events must be valid JSON")
			Expect(activeIDs).To(HaveKey(event.NodeID),
				"Agent %s must not report a ghost peer as unhealthy: %s", agent.Name, line)
		}
	}
}
