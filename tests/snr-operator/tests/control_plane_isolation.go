package tests

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrparams"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RHWA-1927 -- regression test for the control plane isolation gate added by
// self-node-remediation#339, which fixes RHWA-1384.
//
// A control plane node whose local kube-apiserver still answers /readyz with
// 200, but whose agent cannot reach any peer, must reboot itself. Before the
// fix the passing /readyz reset errorCount on every tick and the node fed its
// watchdog forever.
//
// See docs/TESTCASES/rhwa-1927.md for the full specification.
var _ = Describe("SNR Functional - Control Plane Isolation",
	Serial, Ordered, ContinueOnFailure,
	Label(labels.OperatorSNR, snrparams.Label,
		labels.DisruptionDestructive, labels.FrequencyNightly),
	func() {
		var (
			ctx              context.Context
			targetMasterName string
			peerMasterNames  []string
			peerHealthPort   int
		)

		BeforeAll(func() {
			ctx = context.Background()

			By("Verifying at least 3 Ready master nodes for etcd quorum safety")
			masterCount, err := countReadyMasterNodes(ctx, APIClient)
			Expect(err).ToNot(HaveOccurred())
			Expect(masterCount).To(BeNumerically(">=", snrparams.MinReadyMasterNodes),
				"Control plane isolation requires at least %d Ready master nodes for etcd quorum",
				snrparams.MinReadyMasterNodes)

			By("Neutralizing any NodeHealthCheck that selects control plane nodes")
			neutralizedNHCs, err := neutralizeNHCsSelectingCP(ctx)
			if len(neutralizedNHCs) > 0 {
				DeferCleanup(restoreNHCSelectors, ctx, neutralizedNHCs)
			}
			Expect(err).ToNot(HaveOccurred(),
				"Must neutralize NodeHealthCheck CRs that select control plane nodes")

			By("Selecting target master node")
			masterNode, err := selectMasterNode(ctx, APIClient)
			Expect(err).ToNot(HaveOccurred(), "Failed to select master node")
			targetMasterName = masterNode.Name
			GinkgoWriter.Printf("Target master node: %s\n", targetMasterName)

			By("Recording the peer master nodes")
			masterList, err := listMasterNodes(ctx, APIClient)
			Expect(err).ToNot(HaveOccurred())

			for idx := range masterList.Items {
				if masterList.Items[idx].Name != targetMasterName {
					peerMasterNames = append(peerMasterNames, masterList.Items[idx].Name)
				}
			}

			Expect(peerMasterNames).ToNot(BeEmpty(), "At least one peer master node must exist")

			By("Verifying SNR DaemonSet pods are running on every control plane node")
			Eventually(verifyDSPodsRunning,
				medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
				"SNR DaemonSet pods must be running before the partition")
			_, err = agentPodOnNode(targetMasterName)
			Expect(err).ToNot(HaveOccurred(),
				"Target master %s must host a running SNR agent pod", targetMasterName)

			By("Reading the SNR peer health port from the SelfNodeRemediationConfig")
			peerHealthPort = peerHealthPortFromSNRC(ctx)
			GinkgoWriter.Printf("SNR peer health port: %d\n", peerHealthPort)

			By("Skipping when iptables is not usable in the victim agent's network namespace")
			usable, probeLog := agentNamespaceUsable(targetMasterName)
			if !usable {
				Skip(fmt.Sprintf("Cannot run iptables in the SNR agent namespace on node %q: %s",
					targetMasterName, probeLog))
			}

			By("Pre-cleaning any stale SNR CR for the target master")
			cleanupSNRCR(targetMasterName)
		})

		BeforeEach(func() {
			By("Verifying SNR operator deployment is ready")
			snrDeployment, err := deployment.Pull(
				APIClient, snrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
			Expect(err).ToNot(HaveOccurred(), "Failed to get SNR deployment")
			Expect(snrDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
				"SNR deployment is not Ready")
		})

		JustAfterEach(func() {
			// Cleanup order: CRs first (only needs the API server), then node recovery.
			if targetMasterName != "" {
				By("Safety net: deleting any leftover SNR CR for master " + targetMasterName)
				cleanupSNRCR(targetMasterName)

				By("Safety net: waiting for master " + targetMasterName + " to become Ready")

				if err := helpers.WaitForNodeReady(
					ctx, APIClient, targetMasterName,
					snrparams.DefaultPollInterval, snrparams.NodeReadyTimeout,
					GinkgoWriter.Printf,
				); err != nil {
					GinkgoWriter.Printf(
						"WARNING: master %s did not become Ready within %s: %v\n",
						targetMasterName, snrparams.NodeReadyTimeout, err)
					AddReportEntry("safety-net-recovery-failed",
						fmt.Sprintf("master %s did not recover: %v", targetMasterName, err))
				}
			}

			By("Safety net: verifying SNR DS pods are running")
			Eventually(verifyDSPodsRunning,
				snrparams.DSPodRestartTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
				"SNR DaemonSet pods did not recover after remediation")
		})

		It("should remediate a control plane node isolated from its SNR peers while readyz passes",
			reportxml.ID("1927"),
			Label(labels.TierResiliency,
				labels.PlatformAny, labels.TopologyControlPlane, labels.ComponentRemediation),
			func() {
				By("Recording boot ID and creation timestamp for every control plane node")

				oldBootID, err := helpers.GetNodeBootIDFromAPI(ctx, APIClient, targetMasterName)
				Expect(err).ToNot(HaveOccurred(),
					"Must read boot ID from master node %s", targetMasterName)
				node := &corev1.Node{}
				Expect(APIClient.Get(ctx, client.ObjectKey{Name: targetMasterName}, node)).To(Succeed())
				creationTimestamp := node.CreationTimestamp
				peerBootIDs := map[string]string{}

				for _, peerName := range peerMasterNames {
					peerBootID, bootErr := helpers.GetNodeBootIDFromAPI(ctx, APIClient, peerName)
					Expect(bootErr).ToNot(HaveOccurred(), "Must read boot ID from peer master %s", peerName)
					peerBootIDs[peerName] = peerBootID
				}

				GinkgoWriter.Printf("Pre-isolation master boot ID: %s\n", oldBootID)

				By("Confirming the victim agent reports no API error before the partition")

				preIsolationLog, ok := agentLogOnNode(targetMasterName, snrparams.CPIsolationLogWindow)
				Expect(ok).To(BeTrue(),
					"Must read the victim agent log on %s before the partition", targetMasterName)
				Expect(preIsolationLog).NotTo(ContainSubstring(snrparams.APICheckFailureMessage),
					"Agent on %s must not report an API error before the peer port is blocked; "+
						"this test must exercise the isolation gate, not the API-error branch",
					targetMasterName)

				DeferCleanup(func() {
					By("DeferCleanup: flushing the peer port rules from the agent namespace")
					flushAgentPeerPort(targetMasterName, peerHealthPort)
				})

				By("Blocking the SNR peer health port inside the victim agent's network namespace")

				blockAgentPeerPort(targetMasterName, peerHealthPort)
				blockedAt := time.Now()

				By("Confirming the victim node stays Ready while its agent is isolated")

				Consistently(func() bool {
					currentNode := &corev1.Node{}
					if getErr := APIClient.Get(ctx,
						client.ObjectKey{Name: targetMasterName}, currentNode); getErr != nil {
						return false
					}

					return helpers.IsNodeReady(currentNode)
				}, snrparams.CPIsolationReadyGraceDuration, snrparams.DefaultPollInterval).Should(BeTrue(),
					"Master %s must stay Ready while only its agent peer port is blocked; "+
						"this proves the partition is surgical and the kubelet is still healthy "+
						"(the SNR agent's own /readyz check is a separate path not visible here)",
					targetMasterName)

				By("Best-effort: capturing the victim agent log that shows the isolation sequence")

				// This capture is advisory, not the pass gate. The reboot can land
				// between two polls and replace the agent container, whose fresh log
				// no longer holds the pre-reboot messages, so waiting for the log
				// alone would time out on a correct build that reboots quickly. The
				// loop therefore stops as soon as it sees the reboot-trigger message
				// OR the node reboots (boot ID changes), and never fails the spec on
				// its own. The observable gate below -- the boot ID change, the
				// unchanged creation timestamp, and the absent SelfNodeRemediation CR
				// while readyz stayed 200 -- is what proves the agent's own isolation
				// gate rebooted the node. When the log is captured in time its three
				// messages are asserted; otherwise the miss is recorded for triage.
				var (
					isolationLog string
					sequenceSeen bool
				)

				Eventually(func() bool {
					if logText, ok := agentLogOnNode(targetMasterName, snrparams.CPIsolationLogWindow); ok {
						isolationLog = logText
						if strings.Contains(logText, snrparams.CPIsolationRebootTriggeredMessage) {
							sequenceSeen = true

							return true
						}
					}

					newBootID, bootErr := helpers.GetNodeBootIDFromAPI(ctx, APIClient, targetMasterName)

					return bootErr == nil && newBootID != oldBootID
				}, snrparams.CPIsolationRebootTimeout, snrparams.DefaultPollInterval).Should(BeTrue(),
					"Master %s must either log the reboot trigger or change its boot ID after its agent "+
						"is isolated", targetMasterName)

				if sequenceSeen {
					By("Verifying the captured agent log shows the full isolation sequence")

					Expect(isolationLog).To(And(
						ContainSubstring(snrparams.PeerConsultationMessage),
						ContainSubstring(snrparams.CPIsolationDetectedMessage),
						ContainSubstring(snrparams.CPIsolationRebootTriggeredMessage),
					), "Agent on %s must consult peers after maxApiErrorThreshold failures, detect that "+
						"readyz passed while no CP peer is reachable, and trigger a reboot", targetMasterName)
				} else {
					AddReportEntry("cp-isolation-log-not-captured",
						"victim rebooted before its agent log could be captured; relying on the boot-ID "+
							"change and the absent SelfNodeRemediation CR as the gate")
				}

				By("Waiting for the victim node's boot ID to change")

				Eventually(func() string {
					newBootID, bootErr := helpers.GetNodeBootIDFromAPI(ctx, APIClient, targetMasterName)
					if bootErr != nil {
						return oldBootID
					}

					return newBootID
				}, snrparams.CPIsolationRebootTimeout, snrparams.DefaultPollInterval).
					ShouldNot(Equal(oldBootID),
						"Master %s must reboot itself after detecting peer isolation", targetMasterName)

				rebootedAfter := time.Since(blockedAt)
				AddReportEntry("cp-isolation-reboot-duration", rebootedAfter.String())
				GinkgoWriter.Printf("Master %s rebooted %s after the peer port was blocked\n",
					targetMasterName, rebootedAfter)

				By("Verifying the reboot met the remediation time budget from RHWA-1384")

				Expect(rebootedAfter).To(BeNumerically("<", snrparams.CPIsolationRebootSLA),
					"Master %s must reboot within %s of being isolated (RHWA-1384 expects ~2 minutes), took %s",
					targetMasterName, snrparams.CPIsolationRebootSLA, rebootedAfter)

				By("Verifying the victim node was rebooted and not deleted or recreated")

				updatedNode := &corev1.Node{}
				Expect(APIClient.Get(ctx,
					client.ObjectKey{Name: targetMasterName}, updatedNode)).To(Succeed())
				Expect(updatedNode.CreationTimestamp.Equal(&creationTimestamp)).To(BeTrue(),
					"Master creation timestamp changed -- node was re-created instead of rebooted")

				By("Verifying no SelfNodeRemediation CR was created for the victim node")

				snrCR := buildSNRCR("SelfNodeRemediation", targetMasterName, nil)
				getErr := APIClient.Get(ctx, client.ObjectKey{
					Name: targetMasterName, Namespace: medik8sparams.OperatorNs,
				}, snrCR)
				Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
					"SelfNodeRemediation CR for %s must be absent (IsNotFound); the reboot must come "+
						"from the agent's own isolation gate, not from NHC. Get returned: %v",
					targetMasterName, getErr)

				By("Waiting for the victim node to become Ready")

				Expect(helpers.WaitForNodeReady(
					ctx, APIClient, targetMasterName,
					snrparams.DefaultPollInterval, snrparams.NodeReadyTimeout,
					GinkgoWriter.Printf,
				)).To(Succeed(),
					"Master %s did not become Ready after remediation", targetMasterName)

				By("Verifying the surviving control plane nodes never rebooted")

				for peerName, expectedBootID := range peerBootIDs {
					peerBootID, bootErr := helpers.GetNodeBootIDFromAPI(ctx, APIClient, peerName)
					Expect(bootErr).ToNot(HaveOccurred(), "Must read boot ID from peer master %s", peerName)
					Expect(peerBootID).To(Equal(expectedBootID),
						"Peer master %s must not reboot; isolating one control plane node "+
							"must not cascade across the control plane", peerName)
				}

				By("Verifying SNR DaemonSet pods are running again")

				Eventually(verifyDSPodsRunning,
					snrparams.DSPodRestartTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
					"SNR DaemonSet pods must recover after the victim reboots")
			})
	})
