package tests

import (
	"context"
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe(
	"SBR Block Storage — Ceph RBD Split-Brain Prevention (Storage Arbitration)",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR, labels.ComponentRemediation), func() {
		var (
			testSBRC        *unstructured.Unstructured
			victimNodeName  string
			witnessNodes    []string
			observerPodName string
		)

		BeforeAll(func() {
			By("Checking the NodeHealthCheck CRD is installed")

			if !isNHCCRDInstalled() {
				Skip("NodeHealthCheck CRD not found; NHC operator not installed — skipping block split-brain test")
			}

			// Remove this test's own NodeHealthCheck from a crashed prior run first: it references the
			// SBR template, so requireNoSBRNodeHealthCheck would otherwise see it and skip the test.
			By("Removing any NodeHealthCheck left by a previous run of this test")
			deleteNHCAndWait(sbrparams.NHCBlockSplitBrainTestName)

			By("Verifying no other NodeHealthCheck already remediates through the SBR template")
			requireNoSBRNodeHealthCheck()

			By(fmt.Sprintf("Selecting a victim and witnesses from the schedulable worker nodes (need at least %d)",
				sbrparams.MinWorkersForBlockSplitBrain))

			var pickErr error

			victimNodeName, witnessNodes, pickErr = splitBrainNodes()
			Expect(pickErr).ToNot(HaveOccurred(), "Failed to list worker nodes")

			if victimNodeName == "" {
				Skip(fmt.Sprintf("Need at least %d schedulable worker nodes, one of them not hosting an SBR "+
					"controller pod", sbrparams.MinWorkersForBlockSplitBrain))
			}

			GinkgoWriter.Printf("Victim node: %q; witness nodes: %v\n", victimNodeName, witnessNodes)

			By("Verifying iptables is usable in the victim node's host namespaces")

			probePhase, probeLog := runHostCommandPod(victimNodeName, sbrparams.BlockPartitionProbePodName,
				cephRBDIPTablesProbeScript(), sbrparams.BlockPartitionPodTimeout)
			if probePhase != corev1.PodSucceeded {
				Skip(fmt.Sprintf("iptables is not usable in the host namespaces of node %q (probe pod %s): %s",
					victimNodeName, probePhase, probeLog))
			}

			// Set up the Block-mode SBRC from its sub-helpers rather than setupBlockModeSBRC so the init
			// Job can be waited on before the DaemonSet readiness wait: an init failure then surfaces as
			// "init Job failed" instead of an opaque "DaemonSet not ready" timeout.
			failIfSNRRunning()

			By("Discovering the Ceph RBD block storage class")
			rbdStorageClass := discoverCephRBDStorageClass()

			By(fmt.Sprintf("Pre-cleaning any stale SBRC %q", sbrparams.SBRCBlockSplitBrainTestName))
			deleteSBRCAndWait(
				buildSBRC(sbrparams.SBRCBlockSplitBrainTestName, map[string]interface{}{}),
				sbrparams.SBRCBlockSplitBrainTestName)

			By("Creating the Block-mode StorageBasedRemediationConfig on Ceph RBD")
			testSBRC = buildSBRC(sbrparams.SBRCBlockSplitBrainTestName,
				buildBlockModeSBRC(rbdStorageClass, nil))
			Expect(APIClient.Create(context.TODO(), testSBRC)).To(Succeed(),
				"StorageBasedRemediationConfig %q must be created", sbrparams.SBRCBlockSplitBrainTestName)

			By("Clearing residual Ceph OSD blocklist entries from prior tests")
			clearCephOSDBlocklist()

			By("Waiting for the Block device-init Job to succeed")
			waitForBlockInitJobSucceeded(sbrparams.SBRCBlockSplitBrainTestName, testSBRC.GetUID())

			By("Waiting for the agent DaemonSet to be ready")
			waitForSBRCReady(sbrparams.SBRCBlockSplitBrainTestName)

			By("Asserting the SBRC reports concurrent-writeable shared storage")

			Eventually(func() (bool, error) {
				return sbrcConcurrentWriteable(sbrparams.SBRCBlockSplitBrainTestName)
			}, sbrparams.StorageInjectionTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
				"SBRC %q must report status.storageValidation.concurrentWriteable=true before fencing "+
					"can proceed", sbrparams.SBRCBlockSplitBrainTestName)

			By("Selecting an observer agent pod on a witness node to read the shared device through")

			var observerErr error

			for _, witness := range witnessNodes {
				observerPodName, observerErr = agentPodNameByNode(sbrparams.SBRCBlockSplitBrainTestName, witness, true)
				if observerErr == nil {
					break
				}
			}

			Expect(observerErr).ToNot(HaveOccurred(),
				"An agent pod on one of the witness nodes %v must be available to read the shared device",
				witnessNodes)
			GinkgoWriter.Printf("Observer agent pod: %q\n", observerPodName)

			By("Creating the NodeHealthCheck CR that remediates SBRStorageUnhealthy nodes through SBR")

			ensureSBRTemplate()
			// maxUnhealthy: 1 (rather than the shared builder's minHealthy: "51%") so exactly one node
			// may be unhealthy regardless of cluster size — "51%" can block remediation on a small cluster.
			nhc := buildNHCWithMaxUnhealthy(sbrparams.NHCBlockSplitBrainTestName, 1)
			Expect(APIClient.Create(context.TODO(), nhc)).To(Succeed(),
				"NodeHealthCheck CR %q must be created", sbrparams.NHCBlockSplitBrainTestName)
		})

		AfterAll(func() {
			By("Removing the NodeHealthCheck CR")
			cleanupNHCCR(context.Background(), sbrparams.NHCBlockSplitBrainTestName)

			By("Removing the split-brain injector pod if still present")
			deleteHostCommandPod(sbrparams.BlockSplitBrainInjectorPodName)

			if victimNodeName != "" {
				By("Flushing any leftover Ceph REJECT rule on the victim node")
				flushCephRBDRules(victimNodeName)

				By("Clearing residual Ceph OSD blocklist entries left by the fencing")
				clearCephOSDBlocklist()

				By("Uncordoning the victim node if it is still cordoned")
				uncordonNodeIfNeeded(victimNodeName)

				By("Removing any StorageBasedRemediation CR left for the involved nodes")

				for _, nodeName := range append([]string{victimNodeName}, witnessNodes...) {
					cleanupSBRCR(context.Background(), nodeName)
				}
			}

			if testSBRC != nil {
				By(fmt.Sprintf("Removing StorageBasedRemediationConfig %q", sbrparams.SBRCBlockSplitBrainTestName))
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCBlockSplitBrainTestName)
			}
		})

		It("Split-brain: only the storage-isolated Ceph RBD node is fenced; its witnesses are untouched",
			reportxml.ID("1934"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.ComponentRemediation,
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				By("Recording the victim node's boot-id before the partition")

				victimBootID, victimBootErr := getNodeBootID(victimNodeName)
				Expect(victimBootErr).ToNot(HaveOccurred(),
					"Must read the boot-id of victim node %q before the partition", victimNodeName)

				By("Asserting SBRStorageUnhealthy is not True on the victim node")

				// Absent, False or Unknown all pass; only True is a failed precondition.
				Expect(transientNodeHasCondition(victimNodeName, false)).To(Succeed(),
					"Node %q must not report %s=True before its Ceph traffic is blocked",
					victimNodeName, sbrparams.SBRStorageUnhealthyCondition)

				By("Recording every witness node's boot-id and its agent's sbr_self_fenced_total counter " +
					"and restart count")

				baseline, baselineErr := splitBrainBaselineFor(victimBootID, witnessNodes)
				Expect(baselineErr).ToNot(HaveOccurred(),
					"Must record the pre-partition baseline of witness nodes %v", witnessNodes)

				By(fmt.Sprintf("Starting a privileged injector pod that blocks Ceph ports on the victim "+
					"node %q", victimNodeName))

				injectorPod, injectorErr := startPrivilegedInjectorPod(
					victimNodeName, sbrparams.BlockSplitBrainInjectorPodName)
				Expect(injectorErr).ToNot(HaveOccurred(),
					"Privileged injector pod must start on victim node %q", victimNodeName)

				DeferCleanup(func() {
					By("DeferCleanup: removing the split-brain injector pod and flushing leftover Ceph " +
						"REJECT rules")
					deleteHostCommandPod(sbrparams.BlockSplitBrainInjectorPodName)
					flushCephRBDRules(victimNodeName)
				})

				injectOut, injectErr := injectCephRBDRules(injectorPod)
				Expect(injectErr).ToNot(HaveOccurred(),
					"Ceph REJECT rules must be inserted on victim node %q (output: %s)",
					victimNodeName, injectOut)
				GinkgoWriter.Printf("Ceph port REJECT rules applied on victim node %q only\n", victimNodeName)

				By(fmt.Sprintf("Waiting for SBRStorageUnhealthy=True (reason %s) on the victim node",
					sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout))

				Eventually(func() error {
					return nodeConditionIs(victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
						corev1.ConditionTrue, sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout)
				}, sbrparams.StorageInjectionTimeout, sbrparams.StorageInjectionPollInterval).Should(Succeed(),
					"A peer must report isolated node %q %s=True (reason %s)",
					victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
					sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout)

				By("Looking up the victim's and the witnesses' heartbeat slots in the node map and " +
					"recording the sequence the victim stalled at")

				state, stateErr := splitBrainHeartbeatState(observerPodName, victimNodeName, witnessNodes)
				Expect(stateErr).ToNot(HaveOccurred(),
					"Must read the heartbeat slots of victim node %q and witness nodes %v through observer "+
						"pod %q", victimNodeName, witnessNodes, observerPodName)
				GinkgoWriter.Printf("Victim node %q owns heartbeat slot node ID %d, stalled at sequence %d, "+
					"last heartbeat at %s\n", victimNodeName, state.victimNodeID, state.victimSequence,
					time.Unix(0, state.heartbeatTS).UTC().Format(time.RFC3339Nano))

				// Checked on every poll of every wait from here on, rather than over one fixed window:
				// the SBRStorageUnhealthy condition cycles and NHC deletes and recreates CRs with it, so
				// a witness wrongly flagged or remediated can come and go inside an unwatched window. A
				// violation is not something a later poll recovers from, hence StopTrying.
				witnessGuard := func() error {
					if witnessErr := witnessesStillHealthy(witnessNodes); witnessErr != nil {
						return StopTrying("the fence landed on the wrong geometry: a witness node was " +
							"flagged unhealthy or remediated while only the victim was isolated").Wrap(witnessErr)
					}

					return nil
				}

				By("Waiting for NHC to create a StorageBasedRemediation CR for the victim node, while no " +
					"witness becomes unhealthy or gets an SBR CR")

				Eventually(func() error {
					if witnessErr := witnessGuard(); witnessErr != nil {
						return witnessErr
					}

					_, pullErr := pullSBRCR(victimNodeName)
					if k8serrors.IsNotFound(pullErr) {
						return fmt.Errorf("StorageBasedRemediation/%s not yet created by NHC", victimNodeName)
					}

					return pullErr
				}, sbrparams.NHCSBRCRCreationTimeout, sbrparams.NHCSBRCRCreationPollInterval).Should(Succeed(),
					"NHC must create a StorageBasedRemediation CR for isolated node %q", victimNodeName)

				// The proof is awaited before the reboot, not after it: once the victim is back and
				// heartbeating, NHC deletes the CR and the reconciler removes the out-of-service taint, so
				// a check that waits out the reboot first can find neither. Death is proven from storage
				// while the victim is still cut off, so the proof always precedes the victim's recovery.
				By("Waiting for FencingSucceeded=True and Succeeded=True on the victim's StorageBasedRemediation CR")

				var provenCR *unstructured.Unstructured

				Eventually(func() error {
					if witnessErr := witnessGuard(); witnessErr != nil {
						return witnessErr
					}

					var provenErr error

					provenCR, provenErr = fencingProven(victimNodeName)

					return provenErr
				}, sbrparams.FencingProvenTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"StorageBasedRemediation/%s must reach %s=True and %s=True",
					victimNodeName, sbrparams.FencingSucceededCondition, sbrparams.SucceededCondition)

				By("Asserting the device agrees the victim went silent and the witnesses did not")

				// Without this the ordering assertion below is nearly free: any transition is later than
				// a timestamp read before it. What makes the proof mean something is that the victim's
				// slot never moved again while the witnesses' kept moving. This is a second read of the
				// region compared against the one taken while the victim was already isolated, and it is
				// polled because heartbeats tick every few seconds and unevenly: a witness that has not
				// written yet is an ordinary not-yet, while a victim that has written is not.
				Eventually(func() error {
					progressErr := splitBrainHeartbeatProgress(observerPodName, state)
					if errors.Is(progressErr, errVictimResumedHeartbeating) {
						return StopTrying(fmt.Sprintf("victim node %q kept heartbeating while it was "+
							"isolated", victimNodeName)).Wrap(progressErr)
					}

					return progressErr
				}, sbrparams.BlockWitnessProgressTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Fencing node %q may only be proven from storage while its heartbeat slot is stalled "+
						"and its witnesses %v are still heartbeating", victimNodeName, witnessNodes)

				By("Asserting the FencingSucceeded transition happened after the victim's last heartbeat")

				Expect(fencingProvenAfterHeartbeat(provenCR, state.heartbeatTS)).To(Succeed(),
					"FencingSucceeded for node %q must transition after its last heartbeat", victimNodeName)

				By("Asserting the victim node carries the out-of-service taint")

				fencedNode, fencedGetErr := APIClient.CoreV1Interface.Nodes().Get(
					context.TODO(), victimNodeName, metav1.GetOptions{})
				Expect(fencedGetErr).ToNot(HaveOccurred(),
					"Must read node %q to check the %s taint", victimNodeName, sbrparams.OutOfServiceTaintKey)
				Expect(hasOutOfServiceTaint(fencedNode)).To(BeTrue(),
					"Fenced node %q must carry the %s taint", victimNodeName, sbrparams.OutOfServiceTaintKey)

				By("Waiting for the victim node's boot-id to change (confirms it actually rebooted)")

				Eventually(func() error {
					if witnessErr := witnessGuard(); witnessErr != nil {
						return witnessErr
					}

					bootID, bootIDErr := getNodeBootID(victimNodeName)
					if bootIDErr != nil {
						return bootIDErr
					}

					if bootID == baseline.victimBootID {
						return fmt.Errorf("node %s still has boot-id %s; waiting for the fencing reboot",
							victimNodeName, bootID)
					}

					return nil
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Isolated node %q must reboot (boot-id must change from %q)",
					victimNodeName, baseline.victimBootID)

				newVictimBootID, bootErr := getNodeBootID(victimNodeName)
				Expect(bootErr).ToNot(HaveOccurred(),
					"Must read the victim node %q boot-id after the reboot", victimNodeName)
				GinkgoWriter.Printf("Victim node %q rebooted: boot-id %q -> %q\n",
					victimNodeName, baseline.victimBootID, newVictimBootID)

				By(fmt.Sprintf("Waiting for the victim node %q to come back Ready", victimNodeName))

				Eventually(func() error {
					if witnessErr := witnessGuard(); witnessErr != nil {
						return witnessErr
					}

					return nodeConditionIs(victimNodeName, string(corev1.NodeReady), corev1.ConditionTrue, "")
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %q must return Ready after the fencing reboot", victimNodeName)

				By("Asserting every witness node is Ready, schedulable and keeps its original boot-id")

				Expect(witnessesUntouched(witnessNodes, baseline)).To(Succeed(),
					"Only the victim node %q may be fenced; witness nodes %v must be untouched",
					victimNodeName, witnessNodes)

				By("Waiting for the victim node's agent pod to be Ready again")

				Eventually(func() error {
					return agentPodReadyOnNode(sbrparams.SBRCBlockSplitBrainTestName, victimNodeName)
				}, sbrparams.BlockFencingStateCheckTimeout,
					sbrparams.BlockFencingStateCheckInterval).Should(Succeed(),
					"The SBRC %q agent pod on node %q must become Ready after it rebooted",
					sbrparams.SBRCBlockSplitBrainTestName, victimNodeName)

				By(fmt.Sprintf("Consistently asserting for %s that the victim is not fenced again "+
					"(stable boot-id and Ready)", sbrparams.BlockNoFencingCheckDuration))

				// The boot-id alone is a weak signal: it only changes once kubelet reports in after a
				// reboot finishes, so a second fence starting inside this window would not show as a
				// boot-id change. Asserting the node also stays Ready catches a re-fence in progress.
				Consistently(func() error {
					if witnessErr := witnessGuard(); witnessErr != nil {
						return witnessErr
					}

					bootID, bootIDErr := getNodeBootID(victimNodeName)
					if bootIDErr != nil {
						return bootIDErr
					}

					if bootID != newVictimBootID {
						return fmt.Errorf("node %s rebooted again: boot-id %s -> %s",
							victimNodeName, newVictimBootID, bootID)
					}

					return nodeConditionIs(victimNodeName, string(corev1.NodeReady), corev1.ConditionTrue, "")
				}, sbrparams.BlockNoFencingCheckDuration, sbrparams.BlockNoFencingCheckInterval).Should(Succeed(),
					"Node %q must not be fenced again after recovery (boot-id must stay %q and it must stay Ready)",
					victimNodeName, newVictimBootID)

				By("Waiting for NHC to delete the StorageBasedRemediation CR")

				Eventually(func() error {
					if witnessErr := witnessGuard(); witnessErr != nil {
						return witnessErr
					}

					_, pullErr := pullSBRCR(victimNodeName)
					if k8serrors.IsNotFound(pullErr) {
						return nil
					}

					if pullErr != nil {
						return pullErr
					}

					return fmt.Errorf("StorageBasedRemediation/%s still exists; waiting for NHC cleanup", victimNodeName)
				}, sbrparams.SBRCRCleanupTimeout, sbrparams.SBRCRCleanupPollInterval).Should(Succeed(),
					"NHC must delete the StorageBasedRemediation CR for node %q after recovery", victimNodeName)

				By("Asserting the victim node is schedulable again and the out-of-service taint is gone")

				Eventually(func() error {
					node, getErr := APIClient.CoreV1Interface.Nodes().Get(
						context.TODO(), victimNodeName, metav1.GetOptions{})
					if getErr != nil {
						return fmt.Errorf("get node %s: %w", victimNodeName, getErr)
					}

					if !isNodeSchedulable(node) {
						return fmt.Errorf("node %s is not schedulable yet (cordoned or tainted)", victimNodeName)
					}

					if hasOutOfServiceTaint(node) {
						return fmt.Errorf("node %s still carries the %s taint",
							victimNodeName, sbrparams.OutOfServiceTaintKey)
					}

					return nil
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %q must be schedulable with no %s taint after the CR is deleted",
					victimNodeName, sbrparams.OutOfServiceTaintKey)

				GinkgoWriter.Printf("Block-mode split-brain complete: victim %q fenced and recovered; "+
					"witnesses %v untouched\n", victimNodeName, witnessNodes)
			})
	})
