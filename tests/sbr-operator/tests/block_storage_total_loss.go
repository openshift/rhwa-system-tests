package tests

import (
	"context"
	"fmt"

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
	"SBR Block Storage — Ceph RBD Total Storage I/O Loss (Watchdog Path)",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR, labels.ComponentRemediation), func() {
		var (
			testSBRC       *unstructured.Unstructured
			victimNodeName string
		)

		BeforeAll(func() {
			By("Checking the NodeHealthCheck CRD is installed")

			if !isNHCCRDInstalled() {
				Skip("NodeHealthCheck CRD not found; NHC operator not installed — skipping block " +
					"total storage I/O loss test")
			}

			By("Selecting a victim worker node that hosts no SBR controller pod")

			victimNodeName = pickTargetWorkerNode()
			if victimNodeName == "" {
				Skip("No schedulable worker node found that does not host an SBR controller pod")
			}

			GinkgoWriter.Printf("Victim node for the Ceph RBD total storage I/O loss: %q\n", victimNodeName)

			By("Verifying iptables is usable in the victim node's host namespaces")

			// Probed before anything is created: on a node where iptables cannot be used in the host
			// namespaces (an nftables-only node without the compatibility layer) the storage fault
			// cannot be injected at all, which is a skip rather than a failure.
			probePhase, probeLog := runHostCommandPod(victimNodeName, sbrparams.BlockPartitionProbePodName,
				cephRBDIPTablesProbeScript(), sbrparams.BlockPartitionPodTimeout)
			if probePhase != corev1.PodSucceeded {
				Skip(fmt.Sprintf("iptables is not usable in the host namespaces of node %q (probe pod %s): %s",
					victimNodeName, probePhase, probeLog))
			}

			testSBRC = setupBlockModeNHCScenario(
				sbrparams.SBRCBlockStorageLossTestName, sbrparams.NHCBlockStorageLossTestName)
		})

		AfterAll(func() {
			By("Removing the NodeHealthCheck CR")
			cleanupNHCCR(context.Background(), sbrparams.NHCBlockStorageLossTestName)

			if victimNodeName != "" {
				By("Removing the storage-loss injector pod if still present")
				deleteHostCommandPod(sbrparams.BlockStorageLossInjectorPodName)

				By("Flushing any leftover Ceph REJECT rule on the victim node")
				flushCephRBDRules(victimNodeName)

				By("Removing any StorageBasedRemediation CR left for the victim node")
				cleanupSBRCR(context.Background(), victimNodeName)

				By("Uncordoning the victim node if it is still cordoned")
				uncordonNodeIfNeeded(victimNodeName)
			}

			if testSBRC != nil {
				By(fmt.Sprintf("Removing StorageBasedRemediationConfig %q",
					sbrparams.SBRCBlockStorageLossTestName))
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCBlockStorageLossTestName)
			}
		})

		It("Total storage I/O loss: the isolated Ceph RBD node is rebooted by its watchdog and fencing completes",
			reportxml.ID("1944"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.ComponentRemediation,
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				By("Recording the victim node's boot-id before the storage loss")

				preLossBootID, bootErr := getNodeBootID(victimNodeName)
				Expect(bootErr).ToNot(HaveOccurred(),
					"Must read the boot-id of victim node %q before its storage is cut off", victimNodeName)
				GinkgoWriter.Printf("Pre-loss boot-id of node %q: %s\n", victimNodeName, preLossBootID)

				By("Asserting SBRStorageUnhealthy is not True on the victim node")

				// Absent, False or Unknown all pass; only True is a failed precondition.
				Expect(transientNodeHasCondition(victimNodeName,
					sbrparams.SBRStorageUnhealthyCondition, false)).To(Succeed(),
					"Node %q must not report %s=True before its Ceph traffic is blocked",
					victimNodeName, sbrparams.SBRStorageUnhealthyCondition)

				By(fmt.Sprintf("Starting a privileged injector pod that blocks Ceph traffic in both "+
					"directions on the victim node %q", victimNodeName))

				injectorPod, injectorErr := startPrivilegedInjectorPod(
					victimNodeName, sbrparams.BlockStorageLossInjectorPodName)
				Expect(injectorErr).ToNot(HaveOccurred(),
					"Privileged injector pod must start on victim node %q", victimNodeName)

				DeferCleanup(func() {
					By("DeferCleanup: removing the storage-loss injector pod and flushing leftover " +
						"Ceph REJECT rules")
					deleteHostCommandPod(sbrparams.BlockStorageLossInjectorPodName)
					flushCephRBDRules(victimNodeName)
				})

				// Both directions and no time limit: the victim must lose the shared device entirely
				// and keep it lost, so it cannot read the fence message peers write into its own fence
				// slot. The only path left to a reboot is its watchdog, which is the point of the test.
				injectOut, injectErr := injectCephRBDRulesBidirectional(injectorPod)
				Expect(injectErr).ToNot(HaveOccurred(),
					"Ceph REJECT rules must be inserted on both chains on victim node %q (output: %s)",
					victimNodeName, injectOut)
				GinkgoWriter.Printf("Ceph port REJECT rules applied in both directions on node %q\n",
					victimNodeName)

				By(fmt.Sprintf("Waiting for SBRStorageUnhealthy=True (reason %s) on the victim node",
					sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout))

				Eventually(func() error {
					return nodeConditionIs(victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
						corev1.ConditionTrue, sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout)
				}, sbrparams.StorageInjectionTimeout, sbrparams.StorageInjectionPollInterval).Should(Succeed(),
					"A peer must report storage-isolated node %q %s=True (reason %s)",
					victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
					sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout)

				By("Waiting for NHC to create a StorageBasedRemediation CR for the victim node")

				// The CR is what the victim's own agent looks for: with its storage gone it stops
				// petting the watchdog only once a remediation exists for its node.
				Eventually(func() error {
					_, pullErr := pullSBRCR(victimNodeName)
					if k8serrors.IsNotFound(pullErr) {
						return fmt.Errorf("StorageBasedRemediation/%s not yet created by NHC", victimNodeName)
					}

					return pullErr
				}, sbrparams.NHCSBRCRCreationTimeout, sbrparams.NHCSBRCRCreationPollInterval).Should(Succeed(),
					"NHC must create a StorageBasedRemediation CR for storage-isolated node %q", victimNodeName)

				GinkgoWriter.Printf("StorageBasedRemediation/%s created by NHC\n", victimNodeName)

				// The fencing proof is awaited before the reboot, not after it. Death is proven from
				// the victim's heartbeat slot going stale, which it already has, so the proof lands
				// long before kubelet reports a new boot-id; and once the victim is back its iptables
				// rules are gone, it heartbeats again, NHC deletes the CR and the reconciler removes
				// the taint — so a check that waits the reboot out first can find neither.
				By("Waiting for FencingSucceeded=True and Succeeded=True on the victim's StorageBasedRemediation CR")

				Eventually(func() error {
					// Looked up by node name on every poll: the CR can be deleted and recreated
					// mid-remediation when SBRStorageUnhealthy cycles to Unknown.
					_, provenErr := fencingProven(victimNodeName)

					return provenErr
				}, sbrparams.FencingProvenTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"StorageBasedRemediation/%s must reach %s=True and %s=True",
					victimNodeName, sbrparams.FencingSucceededCondition, sbrparams.SucceededCondition)

				By("Asserting the victim node carries the out-of-service taint")

				fencedNode, fencedGetErr := APIClient.CoreV1Interface.Nodes().Get(
					context.TODO(), victimNodeName, metav1.GetOptions{})
				Expect(fencedGetErr).ToNot(HaveOccurred(),
					"Must read node %q to check the %s taint", victimNodeName, sbrparams.OutOfServiceTaintKey)
				Expect(hasOutOfServiceTaint(fencedNode)).To(BeTrue(),
					"Fenced node %q must carry the %s taint", victimNodeName, sbrparams.OutOfServiceTaintKey)

				By("Waiting for the victim node's boot-id to change (the watchdog rebooted it)")

				// The boot-id, not NotReady: a fast reboot can finish between two polls of the Ready
				// condition and be missed entirely, while the boot-id change is permanent. The window
				// must also cover one full condition cycle — if NHC deletes the CR when
				// SBRStorageUnhealthy goes Unknown, the victim resumes petting until it is recreated.
				Eventually(func() (string, error) {
					return getNodeBootID(victimNodeName)
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).ShouldNot(Equal(preLossBootID),
					"Storage-isolated node %q must be rebooted by its watchdog (boot-id must change from %q)",
					victimNodeName, preLossBootID)

				postLossBootID, postBootErr := getNodeBootID(victimNodeName)
				Expect(postBootErr).ToNot(HaveOccurred(),
					"Must read the boot-id of node %q after the watchdog reboot", victimNodeName)
				GinkgoWriter.Printf("Node %q rebooted: boot-id %q -> %q\n",
					victimNodeName, preLossBootID, postLossBootID)

				By(fmt.Sprintf("Waiting for the victim node %q to come back Ready", victimNodeName))

				Eventually(func() error {
					return nodeConditionIs(victimNodeName, string(corev1.NodeReady), corev1.ConditionTrue, "")
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %q must return Ready after the watchdog reboot", victimNodeName)

				By("Waiting for the victim node's agent pod to be Ready again")

				Eventually(func() error {
					return agentPodReadyOnNode(sbrparams.SBRCBlockStorageLossTestName, victimNodeName)
				}, sbrparams.BlockFencingStateCheckTimeout,
					sbrparams.BlockFencingStateCheckInterval).Should(Succeed(),
					"The SBRC %q agent pod on node %q must become Ready after it rebooted",
					sbrparams.SBRCBlockStorageLossTestName, victimNodeName)

				By(fmt.Sprintf("Consistently asserting for %s that the victim is not fenced again "+
					"(stable boot-id and Ready)", sbrparams.BlockNoFencingCheckDuration))

				// The boot-id alone is a weak signal: it only changes once kubelet reports in after a
				// reboot finishes, so a second fence starting inside this window would not show as a
				// boot-id change. Asserting the node also stays Ready catches a re-fence in progress.
				Consistently(func() error {
					bootID, bootIDErr := getNodeBootID(victimNodeName)
					if bootIDErr != nil {
						return bootIDErr
					}

					if bootID != postLossBootID {
						return fmt.Errorf("node %s rebooted again: boot-id %s -> %s",
							victimNodeName, postLossBootID, bootID)
					}

					return nodeConditionIs(victimNodeName, string(corev1.NodeReady), corev1.ConditionTrue, "")
				}, sbrparams.BlockNoFencingCheckDuration, sbrparams.BlockNoFencingCheckInterval).Should(Succeed(),
					"Node %q must not be fenced again after recovery (boot-id must stay %q and it must stay Ready)",
					victimNodeName, postLossBootID)

				By("Waiting for NHC to delete the StorageBasedRemediation CR")

				Eventually(func() error {
					_, pullErr := pullSBRCR(victimNodeName)
					if k8serrors.IsNotFound(pullErr) {
						return nil
					}

					if pullErr != nil {
						return pullErr
					}

					return fmt.Errorf("StorageBasedRemediation/%s still exists; waiting for NHC cleanup",
						victimNodeName)
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

				GinkgoWriter.Printf("Block-mode total storage I/O loss complete: node %q rebooted by its "+
					"watchdog and fencing proven\n", victimNodeName)
			})
	})
