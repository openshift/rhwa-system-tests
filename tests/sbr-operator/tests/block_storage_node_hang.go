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
	"SBR Block Storage — Ceph RBD Node Hang (NHC-driven remediation)",
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
				Skip("NodeHealthCheck CRD not found; NHC operator not installed — skipping block node hang test")
			}

			// Remove this test's own NodeHealthCheck from a crashed prior run first: it references the
			// SBR template, so requireNoSBRNodeHealthCheck would otherwise see it and skip the test.
			By("Removing any NodeHealthCheck left by a previous run of this test")
			deleteNHCAndWait(sbrparams.NHCBlockNodeHangTestName)

			By("Verifying no other NodeHealthCheck already remediates through the SBR template")
			requireNoSBRNodeHealthCheck()

			// Set up the Block-mode SBRC from its sub-helpers rather than setupBlockModeSBRC so the
			// init Job can be waited on before the DaemonSet readiness wait: an init failure then
			// surfaces as "init Job failed" instead of an opaque "DaemonSet not ready" timeout.
			failIfSNRRunning()

			By("Discovering the Ceph RBD block storage class")
			rbdStorageClass := discoverCephRBDStorageClass()

			By(fmt.Sprintf("Pre-cleaning any stale SBRC %q", sbrparams.SBRCBlockNodeHangTestName))
			deleteSBRCAndWait(
				buildSBRC(sbrparams.SBRCBlockNodeHangTestName, map[string]interface{}{}),
				sbrparams.SBRCBlockNodeHangTestName)

			By("Creating the Block-mode StorageBasedRemediationConfig on Ceph RBD")
			testSBRC = buildSBRC(sbrparams.SBRCBlockNodeHangTestName,
				buildBlockModeSBRC(rbdStorageClass, nil))
			Expect(APIClient.Create(context.TODO(), testSBRC)).To(Succeed(),
				"StorageBasedRemediationConfig %q must be created", sbrparams.SBRCBlockNodeHangTestName)

			By("Waiting for the Block device-init Job to succeed")
			waitForBlockInitJobSucceeded(sbrparams.SBRCBlockNodeHangTestName, testSBRC.GetUID())

			By("Waiting for the agent DaemonSet to be ready")
			waitForSBRCReady(sbrparams.SBRCBlockNodeHangTestName)

			By("Asserting the SBRC reports concurrent-writeable shared storage")

			Eventually(func() (bool, error) {
				return sbrcConcurrentWriteable(sbrparams.SBRCBlockNodeHangTestName)
			}, sbrparams.StorageInjectionTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
				"SBRC %q must report status.storageValidation.concurrentWriteable=true before fencing "+
					"can proceed", sbrparams.SBRCBlockNodeHangTestName)

			By("Creating the NodeHealthCheck CR that remediates SBRStorageUnhealthy nodes through SBR")

			ensureSBRTemplate()
			// maxUnhealthy: 1 (rather than the shared builder's minHealthy: "51%") so exactly one node
			// may be unhealthy regardless of cluster size — "51%" can block remediation on a small
			// cluster.
			nhc := buildNHCWithMaxUnhealthy(sbrparams.NHCBlockNodeHangTestName, 1)
			Expect(APIClient.Create(context.TODO(), nhc)).To(Succeed(),
				"NodeHealthCheck CR %q must be created", sbrparams.NHCBlockNodeHangTestName)

			By("Selecting a victim worker node that hosts no SBR controller pod")

			victimNodeName = pickTargetWorkerNode()
			if victimNodeName == "" {
				Skip("No schedulable worker node found that does not host an SBR controller pod")
			}

			GinkgoWriter.Printf("Victim node for the Ceph RBD node hang: %q\n", victimNodeName)
		})

		AfterAll(func() {
			By("Removing the NodeHealthCheck CR")
			cleanupNHCCR(context.Background(), sbrparams.NHCBlockNodeHangTestName)

			if victimNodeName != "" {
				By("Removing the injector pod if still present")
				deleteHostCommandPod(sbrparams.BlockNodeHangInjectorPodName)

				By("Removing any StorageBasedRemediation CR left for the victim node")
				cleanupSBRCR(context.Background(), victimNodeName)

				By("Uncordoning the victim node if it is still cordoned")
				uncordonNodeIfNeeded(victimNodeName)
			}

			if testSBRC != nil {
				By(fmt.Sprintf("Removing StorageBasedRemediationConfig %q", sbrparams.SBRCBlockNodeHangTestName))
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCBlockNodeHangTestName)
			}
		})

		It("Node hang: kernel panic on a Ceph RBD block-mode node triggers NHC fencing and recovery",
			reportxml.ID("1931"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.ComponentRemediation,
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				By("Recording the victim node's boot-id before the kernel panic")

				preCrashBootID, bootErr := getNodeBootID(victimNodeName)
				Expect(bootErr).ToNot(HaveOccurred(),
					"Must read the boot-id of node %q before the kernel panic", victimNodeName)
				GinkgoWriter.Printf("Pre-crash boot-id: %s\n", preCrashBootID)

				By("Asserting SBRStorageUnhealthy is not True on the victim node")

				// "not True" per the plan: absent, False or Unknown all pass. Unknown is a normal
				// state for a node whose condition recently cycled, so it must not fail the precondition.
				// A failed API read is surfaced, not silently treated as "not True".
				victimNode, victimGetErr := APIClient.CoreV1Interface.Nodes().Get(
					context.TODO(), victimNodeName, metav1.GetOptions{})
				Expect(victimGetErr).ToNot(HaveOccurred(), "Must read node %q before the kernel panic", victimNodeName)

				for _, cond := range victimNode.Status.Conditions {
					if string(cond.Type) == sbrparams.SBRStorageUnhealthyCondition {
						Expect(cond.Status).ToNot(Equal(corev1.ConditionTrue),
							"Node %q must not report %s=True before the kernel panic",
							victimNodeName, sbrparams.SBRStorageUnhealthyCondition)
					}
				}

				By(fmt.Sprintf("Starting a privileged injector pod on node %q", victimNodeName))

				injectorPod, injectorErr := startPrivilegedInjectorPod(
					victimNodeName, sbrparams.BlockNodeHangInjectorPodName)
				Expect(injectorErr).ToNot(HaveOccurred(),
					"Privileged injector pod must start on node %q", victimNodeName)

				By(fmt.Sprintf("Probing that nsenter into PID 1 works and /proc/sysrq-trigger is writable "+
					"on node %q", victimNodeName))

				// This test enables SysRq itself (echo 1 > /proc/sys/kernel/sysrq), so the current enabled
				// value does not matter — what must hold is that we can enter the host namespaces and write
				// the trigger. A failure here is a real inability to crash the node, so fail rather than
				// skip: a silent skip would hide that the node-hang path was never exercised.
				probeOut, probeErr := sysRqTriggerWritable(injectorPod)
				Expect(probeErr).ToNot(HaveOccurred(),
					"nsenter into PID 1 and a writable /proc/sysrq-trigger are required to kernel-panic "+
						"node %q (probe output: %s)", victimNodeName, probeOut)

				By(fmt.Sprintf("Triggering a kernel panic on node %q via /proc/sysrq-trigger", victimNodeName))
				GinkgoWriter.Printf("Executing SysRq kernel panic on node %s\n", victimNodeName)
				fireSysRqKernelPanic(injectorPod)

				By(fmt.Sprintf("Waiting for SBRStorageUnhealthy=True (reason %s) on the victim node",
					sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout))

				Eventually(func() error {
					return nodeConditionIs(victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
						corev1.ConditionTrue, sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout)
				}, sbrparams.StorageInjectionTimeout, sbrparams.StorageInjectionPollInterval).Should(Succeed(),
					"A peer must report node %q %s=True (reason %s) after its kernel panic",
					victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
					sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout)

				By("Waiting for NHC to create a StorageBasedRemediation CR for the victim node")

				Eventually(func() error {
					_, pullErr := pullSBRCR(victimNodeName)
					if k8serrors.IsNotFound(pullErr) {
						return fmt.Errorf("StorageBasedRemediation/%s not yet created by NHC", victimNodeName)
					}

					return pullErr
				}, sbrparams.NHCSBRCRCreationTimeout, sbrparams.NHCSBRCRCreationPollInterval).Should(Succeed(),
					"NHC must create a StorageBasedRemediation CR for node %q", victimNodeName)

				GinkgoWriter.Printf("StorageBasedRemediation/%s created by NHC\n", victimNodeName)

				By("Waiting for FencingSucceeded=True and Succeeded=True on the victim's StorageBasedRemediation CR")

				Eventually(func() error {
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
					"Node %q must carry the %s taint after fencing is proven",
					victimNodeName, sbrparams.OutOfServiceTaintKey)

				By(fmt.Sprintf("Waiting for the victim node %q to come back Ready", victimNodeName))

				Eventually(func() error {
					return nodeConditionIs(victimNodeName, string(corev1.NodeReady), corev1.ConditionTrue, "")
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %q must return Ready after the fencing reboot", victimNodeName)

				By("Asserting the victim node's boot-id changed (the node actually rebooted)")

				postCrashBootID, postBootErr := getNodeBootID(victimNodeName)
				Expect(postBootErr).ToNot(HaveOccurred(),
					"Must read the boot-id of node %q after the reboot", victimNodeName)
				Expect(postCrashBootID).ToNot(Equal(preCrashBootID),
					"Boot-id of node %q must change after the reboot (stayed %q)", victimNodeName, preCrashBootID)
				GinkgoWriter.Printf("Boot-id changed: %q -> %q\n", preCrashBootID, postCrashBootID)

				By("Waiting for the victim node's agent pod to be Ready again")

				Eventually(func() error {
					return agentPodReadyOnNode(sbrparams.SBRCBlockNodeHangTestName, victimNodeName)
				}, sbrparams.BlockFencingStateCheckTimeout,
					sbrparams.BlockFencingStateCheckInterval).Should(Succeed(),
					"The SBRC %q agent pod on node %q must become Ready after it rebooted",
					sbrparams.SBRCBlockNodeHangTestName, victimNodeName)

				By(fmt.Sprintf("Consistently asserting for %s that the victim is not fenced again "+
					"(stable boot-id and Ready)", sbrparams.BlockNoFencingCheckDuration))

				// The boot-id alone is a weak signal: it only changes once kubelet reports in after a
				// reboot finishes, which usually takes longer than this window, so a second fence that
				// starts inside the window would not show as a boot-id change. Asserting the node also
				// stays Ready catches a re-fence in progress.
				Consistently(func() error {
					bootID, bootIDErr := getNodeBootID(victimNodeName)
					if bootIDErr != nil {
						return bootIDErr
					}

					if bootID != postCrashBootID {
						return fmt.Errorf("node %s rebooted again: boot-id %s -> %s",
							victimNodeName, postCrashBootID, bootID)
					}

					return nodeConditionIs(victimNodeName, string(corev1.NodeReady), corev1.ConditionTrue, "")
				}, sbrparams.BlockNoFencingCheckDuration, sbrparams.BlockNoFencingCheckInterval).Should(Succeed(),
					"Node %q must not be fenced again after recovery (boot-id must stay %q and it must stay Ready)",
					victimNodeName, postCrashBootID)

				By("Waiting for NHC to delete the StorageBasedRemediation CR")

				Eventually(func() error {
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

				GinkgoWriter.Printf("Block-mode node hang remediation complete for node %q\n", victimNodeName)
			})
	})
