package tests

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// bpnHostLockNodeLabel marks only the nodes this spec puts in its own SBRC's node selector.
	bpnHostLockNodeLabel = "test.medik8s.io/bpn-host-lock"

	// bpnExecutionPhaseOffered is the execution phase a peer records on the target Node before the
	// target accepts. It is the only phase a node with no local agent can ever reach.
	bpnExecutionPhaseOffered = "Offered"

	// bpnPeerManagerLogMarker is what the losing agent's log must still show: its peer controller
	// runs even though its local agent lost the host lock. The reporting surface is the one real
	// open point of this spec — see the comment on the It. Re-check this against the merged agent
	// and prefer a metric or an event over a log line as soon as the code exposes one.
	bpnPeerManagerLogMarker = "peer manager"
)

var _ = Describe(
	"SBR BlockPerNode — Host Lock and LocalAgentConflict",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR, labels.ComponentDaemonSet), func() {
		var (
			testSBRC         *unstructured.Unstructured
			storageClass     string
			conflictNodeName string
			peerNodeName     string
			outsideNodeName  string
			realAgentPod     string
		)

		BeforeAll(func() {
			// An NHC remediating through the SBR template would create its own
			// StorageBasedRemediation CRs for the nodes this spec watches.
			requireNoSBRNodeHealthCheck()
			// BlockPerNode does not exist yet, so on today's build the spec stops here.
			requireSBRCBlockPerNodeSupport()
			failIfSNRRunning()

			By("Clearing residual Ceph OSD blocklist entries from prior tests")
			clearCephOSDBlocklist()

			By("Discovering the Ceph RBD storage class")

			// The BlockPerNode probe is RWO Block, so the Ceph RBD class serves it too.
			storageClass = discoverCephRBDStorageClass()

			By("Selecting three schedulable worker nodes")

			workers, listErr := schedulableWorkerNodeNames()
			Expect(listErr).ToNot(HaveOccurred(), "Worker nodes must be readable")

			if len(workers) < sbrparams.MinWorkersForBlockSplitBrain {
				Skip(fmt.Sprintf("The host lock spec needs three schedulable workers: two inside the "+
					"SBRC's selector so the conflicted shadow is not the only peer, and one outside it "+
					"as the request target; found %d", len(workers)))
			}

			conflictNodeName = pickTargetWorkerNode()
			if conflictNodeName == "" {
				Skip("Every schedulable worker hosts an SBR controller pod, so none can take a " +
					"second agent process")
			}

			for _, worker := range workers {
				switch {
				case worker == conflictNodeName:
				case peerNodeName == "":
					peerNodeName = worker
				case outsideNodeName == "":
					outsideNodeName = worker
				}
			}

			Expect(peerNodeName).ToNot(BeEmpty(), "A second selected worker must be available as a peer")
			Expect(outsideNodeName).ToNot(BeEmpty(),
				"A third worker must stay outside the SBRC's selector as the request target")
			GinkgoWriter.Printf("Conflict node %q, peer node %q, request target outside the selector %q\n",
				conflictNodeName, peerNodeName, outsideNodeName)

			By(fmt.Sprintf("Pre-cleaning any stale SBRC %q and shadow agent pods",
				sbrparams.SBRCBlockPerNodeLockTestName))
			deleteSBRCAndWait(
				buildSBRC(sbrparams.SBRCBlockPerNodeLockTestName, map[string]interface{}{}),
				sbrparams.SBRCBlockPerNodeLockTestName)

			for _, worker := range workers {
				deleteHostCommandPod(sbrparams.ShadowAgentPodPrefix + worker)
			}

			By("Labelling the two selected workers and creating the BlockPerNode SBRC")
			labelNodes(bpnHostLockNodeLabel, "true", conflictNodeName, peerNodeName)
			DeferCleanup(func() {
				removeNodeLabel(bpnHostLockNodeLabel, conflictNodeName, peerNodeName)
			})

			testSBRC = buildSBRC(sbrparams.SBRCBlockPerNodeLockTestName,
				buildBlockPerNodeSBRC(storageClass, map[string]interface{}{
					"nodeSelector": map[string]interface{}{bpnHostLockNodeLabel: "true"},
					"ioTimeout":    sbrparams.BPNIOTimeout.String(),
				}))
			Expect(APIClient.Create(context.TODO(), testSBRC)).To(Succeed(),
				"StorageBasedRemediationConfig %q must be created", sbrparams.SBRCBlockPerNodeLockTestName)

			By("Waiting for the agent DaemonSet to be ready")
			waitForSBRCReady(sbrparams.SBRCBlockPerNodeLockTestName)

			By("Asserting the agent DaemonSet runs on exactly the two selected nodes")

			agentDS, dsErr := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(context.TODO(),
				sbrparams.SBRAgentDaemonSetPrefix+sbrparams.SBRCBlockPerNodeLockTestName, metav1.GetOptions{})
			Expect(dsErr).ToNot(HaveOccurred(), "The agent DaemonSet of SBRC %q must exist",
				sbrparams.SBRCBlockPerNodeLockTestName)
			Expect(agentDS.Status.DesiredNumberScheduled).To(BeEquivalentTo(2),
				"The SBRC's selector must schedule an agent on exactly the two labelled nodes")
			Expect(agentDS.Status.NumberReady).To(BeEquivalentTo(2),
				"Both selected agents must be Ready before the conflict is created")
			// If the selector leaked, the request in the It would be accepted and reboot a healthy
			// worker instead of stopping at Offered.
			_, leakedErr := agentPodNameByNode(sbrparams.SBRCBlockPerNodeLockTestName, outsideNodeName, true)
			Expect(leakedErr).To(HaveOccurred(),
				"Node %q must stay outside the SBRC's selector and run no agent: the remediation "+
					"request this spec creates for it must never be accepted", outsideNodeName)

			By("Recording the real agent pod on the conflict node")

			var agentErr error

			realAgentPod, agentErr = agentPodNameByNode(
				sbrparams.SBRCBlockPerNodeLockTestName, conflictNodeName, true)
			Expect(agentErr).ToNot(HaveOccurred(),
				"An SBR agent pod must be Running on the conflict node %q", conflictNodeName)
		})

		AfterAll(func() {
			if outsideNodeName != "" {
				By("Removing any StorageBasedRemediation CR left for the request target")
				cleanupSBRCR(context.Background(), outsideNodeName)

				By("Uncordoning the request target if recording the offer left it cordoned")
				uncordonNodeIfNeeded(outsideNodeName)
			}

			if testSBRC != nil {
				By(fmt.Sprintf("Removing StorageBasedRemediationConfig %q",
					sbrparams.SBRCBlockPerNodeLockTestName))
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCBlockPerNodeLockTestName)
			}

			By("Clearing residual Ceph OSD blocklist entries")
			clearCephOSDBlocklist()
		})

		// This spec reboots nothing when the code is correct. If the bug is present and the shadow
		// does open the watchdog, deleting the shadow closes that device without a magic-close
		// character and the node resets. A node reset during teardown is therefore the finding, not
		// damage done by the test.
		It("Host lock: a second agent process on one host loses the lock, writes nothing, and keeps "+
			"reconciling peers",
			reportxml.ID("2065"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.ComponentDaemonSet,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				By("Recording the conflict node's baseline")

				preBootID, bootErr := getNodeBootID(conflictNodeName)
				Expect(bootErr).ToNot(HaveOccurred(), "Must read the boot-id of node %q", conflictNodeName)
				baselineRestarts, restartErr := agentRestartCount(realAgentPod)
				Expect(restartErr).ToNot(HaveOccurred(),
					"Must read the restart count of agent pod %q", realAgentPod)
				baselineSelfFenced, fenceErr := readAgentCounter(realAgentPod, sbrparams.SelfFencedMetricName)
				Expect(fenceErr).ToNot(HaveOccurred(),
					"Must read %s from agent pod %q", sbrparams.SelfFencedMetricName, realAgentPod)

				By("Snapshotting the host lock, journal and watchdog state before the shadow exists")

				preState := hostStateSnapshot(conflictNodeName, "pre")
				Expect(preState["lock_inode"]).ToNot(Equal("ABSENT"),
					"The host lock file %s/%s must exist on node %q before a second agent starts",
					sbrparams.BlockPerNodeStateHostPath, sbrparams.BlockPerNodeLockFileName, conflictNodeName)
				Expect(preState["lock_holders"]).ToNot(BeEmpty(),
					"The real agent on node %q must hold the host lock open", conflictNodeName)

				By("Recording the conflict node object")

				nodeRec := recordNode(conflictNodeName)

				By("Starting a second agent process on the conflict node, with a writable state mount")

				shadow, shadowErr := startShadowAgentPod(conflictNodeName,
					sbrparams.SBRCBlockPerNodeLockTestName, false)
				DeferCleanup(func() {
					if shadow != nil {
						deleteHostCommandPod(shadow.Definition.Name)
					}
				})
				Expect(shadowErr).ToNot(HaveOccurred(),
					"A second agent process must start on node %q", conflictNodeName)
				awaitLocalAgentConflict(shadow, "writable state mount")
				assertShadowWroteNothing(conflictNodeName, "post", preState, "writable state mount")
				assertShadowStillRuns(shadow, "writable state mount")

				By(fmt.Sprintf("Creating a StorageBasedRemediation CR for node %q, which runs no agent",
					outsideNodeName))

				sbrCR := buildSBR(outsideNodeName)
				Expect(APIClient.Create(context.TODO(), sbrCR)).To(Succeed(),
					"StorageBasedRemediation CR for node %q must be created", outsideNodeName)
				DeferCleanup(func() {
					deleteVictimRemediation(sbrCR, outsideNodeName)
				})

				By("Waiting for a peer to record the offer on the target node")

				// Nobody can accept it: the target runs no local agent. The request stops at
				// Offered and a cordon, which is the only way to exercise the shadow's peer
				// controller without rebooting a healthy worker.
				Eventually(func() error {
					return hostLockOfferRecorded(outsideNodeName, sbrCR.GetUID())
				}, sbrparams.StorageInjectionTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"A peer must record %s=%s for request %s on node %q",
					bpnOwnerAnnotation, bpnExecutionPhaseOffered, sbrCR.GetUID(), outsideNodeName)

				By("Asserting the conflicted shadow took part in reconciling the request")

				// The healthy agent on the peer node is an eligible peer too and may win the
				// conditional update. Both peers racing for the same record is expected: the
				// assertion is that the shadow took part, not that it won.
				shadowLog := hostCommandPodLog(shadow)
				Expect(shadowLog).To(ContainSubstring(outsideNodeName),
					"The conflicted agent's peer controller must process the request for node %q",
					outsideNodeName)
				Expect(shadowLog).To(ContainSubstring(string(sbrCR.GetUID())),
					"The conflicted agent's peer controller must process request %s", sbrCR.GetUID())

				By("Observing that the conflicted shadow stayed harmless")

				Consistently(func() error {
					return hostLockNodeUntouched(conflictNodeName, preBootID, realAgentPod, baselineRestarts)
				}, sbrparams.BlockPerNodeQuietCheckDuration,
					sbrparams.BlockPerNodeQuietCheckInterval).Should(Succeed(),
					"Node %q must stay untouched while a second agent process runs on it", conflictNodeName)

				By("Asserting the conflict node was never flagged, even between polls")

				Expect(nodeRec.NeverSaw(sbrparams.SBRStorageUnhealthyCondition+"=True",
					func(node *corev1.Node) bool {
						for _, condition := range node.Status.Conditions {
							if string(condition.Type) == sbrparams.SBRStorageUnhealthyCondition &&
								condition.Status == corev1.ConditionTrue {
								return true
							}
						}

						return false
					})()).To(Succeed(),
					"Node %q must never report %s=True while a second agent process runs on it",
					conflictNodeName, sbrparams.SBRStorageUnhealthyCondition)

				By("Asserting the real agent did not self-fence")

				selfFenced, counterErr := readAgentCounter(realAgentPod, sbrparams.SelfFencedMetricName)
				Expect(counterErr).ToNot(HaveOccurred(),
					"Must read %s from agent pod %q", sbrparams.SelfFencedMetricName, realAgentPod)
				Expect(selfFenced).To(Equal(baselineSelfFenced),
					"The agent on node %q must not self-fence: %s went from %v to %v",
					conflictNodeName, sbrparams.SelfFencedMetricName, baselineSelfFenced, selfFenced)

				By("Removing the shadow before the rollout, so it cannot take the released lock")

				// The rollout makes the real agent release the lock for a moment. A shadow still
				// running could take it, and the replacement agent would then be the one reporting
				// the conflict and never becoming Ready.
				deleteHostCommandPod(shadow.Definition.Name)
				shadow = nil

				By("Deleting the remediation request and waiting for the target to be released")

				// ioTimeout is rollout-gated: the controller waits for outstanding requests to
				// resolve and for peers to release their holder entries before it replaces the
				// local monitoring loops. This request never resolves on its own, so leaving it in
				// place would hang the rollout on correct code.
				deleteVictimRemediation(sbrCR, outsideNodeName)
				Eventually(func() error {
					_, pullErr := pullSBRCR(outsideNodeName)
					if k8serrors.IsNotFound(pullErr) {
						return nil
					}

					if pullErr != nil {
						return fmt.Errorf("get StorageBasedRemediation/%s: %w", outsideNodeName, pullErr)
					}

					return fmt.Errorf("StorageBasedRemediation/%s still exists", outsideNodeName)
				}, sbrparams.SBRCRCleanupTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"StorageBasedRemediation/%s must be gone before the configuration is replaced",
					outsideNodeName)

				By("Watching the host lock file through a full agent rollout")

				watchPodName := sbrparams.HostLockWatchPodPrefix + conflictNodeName
				createHostCommandPod(conflictNodeName, watchPodName,
					hostLockWatchScript(sbrparams.BlockPerNodeRolloutWatchDuration))
				DeferCleanup(func() {
					deleteHostCommandPod(watchPodName)
				})
				rolloutAgentUIDs, uidErr := agentPodUIDsByNode(sbrparams.SBRCBlockPerNodeLockTestName,
					conflictNodeName, peerNodeName)
				Expect(uidErr).ToNot(HaveOccurred(), "Both agent pods must be readable before the rollout")
				agentDS, dsErr := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(context.TODO(),
					sbrparams.SBRAgentDaemonSetPrefix+sbrparams.SBRCBlockPerNodeLockTestName,
					metav1.GetOptions{})
				Expect(dsErr).ToNot(HaveOccurred(), "The agent DaemonSet must be readable before the rollout")

				By("Forcing a full agent rollout by patching the SBRC's ioTimeout")

				// ioTimeout is mutable but rollout-gated, so changing it is what makes the operator
				// re-render the DaemonSet and replace both agent pods.
				patchSBRCSpecField(sbrparams.SBRCBlockPerNodeLockTestName, "ioTimeout", "20s",
					agentDS.Generation)

				Eventually(func() error {
					return agentPodsReplaced(sbrparams.SBRCBlockPerNodeLockTestName, rolloutAgentUIDs)
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Both agents of SBRC %q must be replaced and Ready after the ioTimeout patch",
					sbrparams.SBRCBlockPerNodeLockTestName)

				By("Asserting the lock file was never unlinked and never changed identity")

				watchPhase, watchLog := hostCommandPodOutcome(watchPodName,
					sbrparams.BlockPerNodeRolloutWatchDuration)
				Expect(watchPhase).To(Equal(corev1.PodSucceeded),
					"The lock watcher on node %q must end successfully: %s", conflictNodeName, watchLog)
				Expect(watchLog).To(ContainSubstring("SBR_LOCK_STABLE"),
					"The lock watcher on node %q must report a stable lock file: %s", conflictNodeName, watchLog)
				Expect(watchLog).ToNot(ContainSubstring("SBR_LOCK_GONE"),
					"The host lock file must never be unlinked while a process may hold it: %s", watchLog)
				Expect(watchLog).ToNot(ContainSubstring("SBR_LOCK_INODE_CHANGED"),
					"The host lock file must keep its identity across configuration replacement: %s", watchLog)

				By("Asserting exactly one process holds the lock again after the rollout")

				var postRolloutErr error

				realAgentPod, postRolloutErr = agentPodNameByNode(
					sbrparams.SBRCBlockPerNodeLockTestName, conflictNodeName, true)
				Expect(postRolloutErr).ToNot(HaveOccurred(),
					"A replacement agent pod must be Running on node %q", conflictNodeName)
				rolloutState := hostStateSnapshot(conflictNodeName, "rollout")
				Expect(strings.Fields(rolloutState["lock_holders"])).To(HaveLen(1),
					"Exactly one process on node %q may hold the host lock after the rollout, found %q",
					conflictNodeName, rolloutState["lock_holders"])

				By("Repeating the conflict with a read-only state mount")

				// The step 14 snapshot is stale: the rollout replaced the real agent, so its PID
				// changed in both holder sets and the journal has been rewritten since.
				preROState := hostStateSnapshot(conflictNodeName, "pre-ro")
				roShadow, roErr := startShadowAgentPod(conflictNodeName,
					sbrparams.SBRCBlockPerNodeLockTestName, true)
				DeferCleanup(func() {
					if roShadow != nil {
						deleteHostCommandPod(roShadow.Definition.Name)
					}
				})
				Expect(roErr).ToNot(HaveOccurred(),
					"A second agent process with a read-only state mount must start on node %q",
					conflictNodeName)

				// A loser that also cannot write its state must not look like a success and must
				// not take the host down with it.
				awaitLocalAgentConflict(roShadow, "read-only state mount")
				assertShadowWroteNothing(conflictNodeName, "post-ro", preROState, "read-only state mount")
				assertShadowStillRuns(roShadow, "read-only state mount")
			})
	})

// hostStateSnapshot runs the host-state inspection script on nodeName and returns its key=value
// output parsed into a map. The suffix keeps the one-shot pods of one spec run apart.
func hostStateSnapshot(nodeName, suffix string) map[string]string {
	GinkgoHelper()
	phase, podLog := runHostCommandPod(nodeName, sbrparams.HostStateInspectPodPrefix+suffix,
		hostStateInspectScript(), sbrparams.BlockPartitionPodTimeout)
	Expect(phase).To(Equal(corev1.PodSucceeded),
		"The host state inspection on node %q must succeed: %s", nodeName, podLog)
	state := make(map[string]string)

	for _, line := range strings.Split(podLog, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found {
			state[key] = strings.TrimSpace(value)
		}
	}

	AddReportEntry("bpn-host-state-"+suffix, state)

	return state
}

// awaitLocalAgentConflict waits for the losing agent process to report LocalAgentConflict.
//
// The pod log is the only surface certain to exist: RHWA-2065 says the second process "reports
// LocalAgentConflict" without saying where, and the design lists it among the bounded reasons
// alongside a writer-conflict metric. As soon as the merged agent exposes a metric or an event,
// assert that instead and keep this as the backup.
func awaitLocalAgentConflict(shadow *pod.Builder, variant string) {
	GinkgoHelper()
	By(fmt.Sprintf("Waiting for the second agent process to report %s (%s)",
		sbrparams.LocalAgentConflictReason, variant))

	Eventually(func() string {
		return hostCommandPodLog(shadow)
	}, sbrparams.BlockPerNodeConflictTimeout, sbrparams.DefaultPollInterval).Should(
		ContainSubstring(sbrparams.LocalAgentConflictReason),
		"The second agent process on the host must report %s (%s)",
		sbrparams.LocalAgentConflictReason, variant)
}

// assertShadowWroteNothing compares a fresh host snapshot with the baseline taken before the losing
// agent started. The holder sets are the strong signals: they say which process owns the lock and
// the watchdog, which is exactly what the lock decides, and the losing agent appears in neither.
// The spec never learns the shadow's own PID, so "the holder set is unchanged" is the assertion.
//
// The journal comparison is the weak one, because the lock holder rewrites the journal legitimately
// on its own schedule: a changed inode between two snapshots is normal. Only a journal that
// disappeared is asserted; the rest is recorded for the report. Do not tighten this into an exact
// inode match without first measuring how often a healthy agent rewrites it.
func assertShadowWroteNothing(nodeName, suffix string, baseline map[string]string, variant string) {
	GinkgoHelper()
	By(fmt.Sprintf("Asserting the second agent process wrote nothing on the host (%s)", variant))

	state := hostStateSnapshot(nodeName, suffix)
	Expect(state["lock_inode"]).To(Equal(baseline["lock_inode"]),
		"The losing agent must not replace the host lock file on node %q (%s)", nodeName, variant)
	Expect(state["lock_holders"]).To(Equal(baseline["lock_holders"]),
		"The host lock on node %q must still be held by exactly the original process (%s)",
		nodeName, variant)
	Expect(state["watchdog_holders"]).To(Equal(baseline["watchdog_holders"]),
		"The losing agent must never open the watchdog on node %q (%s)", nodeName, variant)

	if baseline["journal_inode"] != "ABSENT" {
		Expect(state["journal_inode"]).ToNot(Equal("ABSENT"),
			"The host journal on node %q must not be removed (%s)", nodeName, variant)
	}

	GinkgoWriter.Printf("[host-lock] node %s (%s): journal inode %s -> %s, sha256 %s -> %s\n",
		nodeName, variant, baseline["journal_inode"], state["journal_inode"],
		baseline["journal_sha256"], state["journal_sha256"])
}

// assertShadowStillRuns proves the losing agent kept running as a peer rather than dying. An agent
// that crash-loops would also "write nothing" and would pass a naive version of the previous check.
func assertShadowStillRuns(shadow *pod.Builder, variant string) {
	GinkgoHelper()
	By(fmt.Sprintf("Asserting the second agent process keeps running as a peer (%s)", variant))

	Expect(hostCommandPodLog(shadow)).To(ContainSubstring(bpnPeerManagerLogMarker),
		"The losing agent must keep its peer controller running (%s)", variant)
	restarts, restartErr := agentRestartCount(shadow.Definition.Name)
	Expect(restartErr).ToNot(HaveOccurred(),
		"Must read the restart count of the second agent process (%s)", variant)
	Expect(restarts).To(BeEquivalentTo(0),
		"The losing agent must not crash-loop: it restarted %d times (%s)", restarts, variant)
}

// hostLockOfferRecorded returns nil once the target Node carries an execution record in the Offered
// phase for the given request. Intended as an Eventually body.
func hostLockOfferRecorded(nodeName string, requestUID types.UID) error {
	node, getErr := APIClient.CoreV1Interface.Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("get node %s: %w", nodeName, getErr)
	}

	owner, ownerErr := bpnReadOwner(node)
	if ownerErr != nil {
		return ownerErr
	}

	if owner.Execution.RequestUID != requestUID {
		return fmt.Errorf("node %s records request %s, want %s",
			nodeName, owner.Execution.RequestUID, requestUID)
	}

	if owner.Execution.Phase != bpnExecutionPhaseOffered {
		return fmt.Errorf("node %s records execution phase %q, want %q",
			nodeName, owner.Execution.Phase, bpnExecutionPhaseOffered)
	}

	return nil
}

// hostLockNodeUntouched returns nil while the conflict node shows no sign of the second agent
// process: it did not reboot, it is Ready, it is not flagged, its real agent did not restart, and no
// remediation execution was ever recorded on it. Intended as a Consistently body.
func hostLockNodeUntouched(nodeName, preBootID, agentPodName string, baselineRestarts int32) error {
	bootID, bootErr := getNodeBootID(nodeName)
	if bootErr != nil {
		return bootErr
	}

	if bootID != preBootID {
		return fmt.Errorf("node %s rebooted: boot-id went from %s to %s", nodeName, preBootID, bootID)
	}

	if readyErr := nodeConditionIs(nodeName, string(corev1.NodeReady), corev1.ConditionTrue, ""); readyErr != nil {
		return readyErr
	}

	if flaggedErr := nodeConditionIs(nodeName, sbrparams.SBRStorageUnhealthyCondition,
		corev1.ConditionTrue, ""); flaggedErr == nil {
		return fmt.Errorf("node %s must not report %s=True", nodeName, sbrparams.SBRStorageUnhealthyCondition)
	}

	restarts, restartErr := agentRestartCount(agentPodName)
	if restartErr != nil {
		return restartErr
	}

	if restarts != baselineRestarts {
		return fmt.Errorf("agent pod %s restarted: count went from %d to %d",
			agentPodName, baselineRestarts, restarts)
	}

	node, getErr := APIClient.CoreV1Interface.Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("get node %s: %w", nodeName, getErr)
	}

	if node.Annotations[bpnOwnerAnnotation] == "" {
		return nil
	}

	owner, ownerErr := bpnReadOwner(node)
	if ownerErr != nil {
		return fmt.Errorf("node %s carries an unreadable %s annotation: %w",
			nodeName, bpnOwnerAnnotation, ownerErr)
	}

	if owner.Execution.RequestUID != "" || owner.Execution.Phase != "" {
		return fmt.Errorf("node %s records execution %s in phase %q, but no request was ever created "+
			"for it", nodeName, owner.Execution.RequestUID, owner.Execution.Phase)
	}

	return nil
}
