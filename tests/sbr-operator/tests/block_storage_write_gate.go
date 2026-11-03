package tests

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The remediation reconciler reads status.storageValidation.concurrentWriteable on the owning SBRC
// before it cordons or fences anything. The operator only sets it once min(2, desired) agents are
// Ready, which is the cluster's evidence that the shared device takes concurrent writes. Every
// other Block-mode spec runs with that gate open; this one holds it shut and proves a
// StorageBasedRemediation CR is withheld rather than acted on.
//
// The gate is held shut by keeping /dev/watchdog open on every worker but one before the SBRC is
// created. An agent whose watchdog check fails also fails preflight, never writes its readiness
// sentinel, and never becomes Ready — so one agent is Ready out of at least two desired.
var _ = Describe(
	"SBR Block Storage - Storage Write Gate Withheld on Ceph RBD",
	Ordered,
	Serial,
	Label(labels.OperatorSBR, labels.ComponentRemediation), func() {
		var (
			testSBRC        *unstructured.Unstructured
			sbrCR           *unstructured.Unstructured
			storageClass    string
			victimNodeName  string
			readyNodeName   string
			blockedNodes    []string
			workerNodes     []string
			readyWorkers    []string
			keepalivePods   []string
			observerPodName string
		)

		BeforeAll(func() {
			requireNoSBRNodeHealthCheck()

			By("Verifying the installed SBR operator supports sharedStorageVolumeMode")
			requireSBRCBlockVolumeModeSupport()
			failIfSNRRunning()
			By("Clearing residual Ceph OSD blocklist entries from prior tests")
			clearCephOSDBlocklist()

			By("Discovering Ceph RBD block storage class")

			storageClass = discoverCephRBDStorageClass()
			GinkgoWriter.Printf("Using Ceph RBD storage class: %s\n", storageClass)

			By("Listing schedulable worker nodes")

			nodeList, listErr := APIClient.CoreV1Interface.Nodes().List(context.TODO(), metav1.ListOptions{
				LabelSelector: "node-role.kubernetes.io/worker",
			})
			Expect(listErr).ToNot(HaveOccurred(), "Failed to list worker nodes")

			// The agent DaemonSet ignores cordons, so a cordoned but Ready worker still runs an
			// agent that would become Ready and open the gate. Every Ready worker therefore needs
			// a keepalive, while the victim and the unblocked node come from schedulable workers.
			for idx := range nodeList.Items {
				node := &nodeList.Items[idx]
				if isNodeSchedulable(node) {
					workerNodes = append(workerNodes, node.Name)
				}

				for _, cond := range node.Status.Conditions {
					if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
						readyWorkers = append(readyWorkers, node.Name)
					}
				}
			}

			if len(workerNodes) < 2 {
				Skip("The storage write gate needs at least two schedulable workers: " +
					"one agent must stay Ready while the rest are blocked")
			}

			By("Selecting the victim worker node (schedulable, not an SBR controller pod host)")

			victimNodeName = pickTargetWorkerNode()
			if victimNodeName == "" {
				Skip("No schedulable worker node without an SBR controller pod found")
			}

			// One agent is allowed to start so the DaemonSet reports exactly one Ready pod out of
			// at least two desired: enough for the gate to stay shut, and it gives the spec a
			// healthy agent to read the device through.
			for _, nodeName := range workerNodes {
				if nodeName != victimNodeName {
					readyNodeName = nodeName

					break
				}
			}

			for _, nodeName := range readyWorkers {
				if nodeName != readyNodeName {
					blockedNodes = append(blockedNodes, nodeName)
				}
			}

			Expect(readyNodeName).ToNot(BeEmpty(),
				"A worker other than the victim %q must be left unblocked", victimNodeName)
			GinkgoWriter.Printf("Victim node: %s; unblocked node: %s; blocked nodes: %v\n",
				victimNodeName, readyNodeName, blockedNodes)

			By("Removing any SBRC and watchdog-keepalive pods left by a previous run")
			deleteSBRCAndWait(
				buildSBRC(sbrparams.SBRCBlockWriteGateTestName, map[string]interface{}{}),
				sbrparams.SBRCBlockWriteGateTestName)
			stalePods := make([]string, 0, len(nodeList.Items))
			for idx := range nodeList.Items {
				nodeName := nodeList.Items[idx].Name
				stalePods = append(stalePods, keepalivePodName(nodeName))
			}

			deleteKeepalivePods(stalePods)

			By("Holding /dev/watchdog open on every worker except the unblocked one")

			// Before the SBRC exists, not after: once concurrentWriteable goes true the operator
			// keeps it true through readiness dips, so blocking the agents later would not shut
			// the gate again.
			for _, nodeName := range blockedNodes {
				podName, createErr := startWatchdogKeepalivePod(nodeName)
				keepalivePods = append(keepalivePods, podName)
				Expect(createErr).ToNot(HaveOccurred(),
					func() string {
						return fmt.Sprintf("Watchdog keepalive pod %s on worker %s must start\n%s",
							podName, nodeName, watchdogKeepaliveDiagnostics(podName))
					})

				Eventually(func() error {
					return keepalivePodOpenedWatchdog(podName)
				}, sbrparams.WatchdogKeepaliveTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					func() string {
						return fmt.Sprintf("Watchdog keepalive pod %s must open and hold /dev/watchdog\n%s",
							podName, watchdogKeepaliveDiagnostics(podName))
					})
				GinkgoWriter.Printf("Keepalive pod %s confirmed /dev/watchdog open on node %s\n",
					podName, nodeName)
			}

			By("Creating a Block-mode SBRC on Ceph RBD with detect-only disabled")

			// detectOnlyMode must stay disabled: a detect-only agent skips the watchdog check, so
			// the keepalives would not block it, and the reconciler would withhold the remediation
			// for detect-only before it ever reached the storage write gate.
			testSBRC = buildSBRC(sbrparams.SBRCBlockWriteGateTestName, buildBlockModeSBRC(storageClass, nil))
			createErr := APIClient.Create(context.TODO(), testSBRC)
			Expect(createErr).ToNot(HaveOccurred(),
				"StorageBasedRemediationConfig %q must be created", sbrparams.SBRCBlockWriteGateTestName)

			By("Waiting for the block device-init Job to succeed")
			waitForBlockInitJobSucceeded(sbrparams.SBRCBlockWriteGateTestName, testSBRC.GetUID())

			By(fmt.Sprintf("Waiting for the agent on the unblocked node %q to be Ready", readyNodeName))

			// Not waitForSBRCReady: it waits for every agent, and the blocked ones never become
			// Ready, so it would only ever time out here.
			Eventually(func() error {
				return agentPodReadyOnNode(sbrparams.SBRCBlockWriteGateTestName, readyNodeName)
			}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
				"Agent pod on node %q must become Ready", readyNodeName)

			By("Verifying exactly one agent is Ready out of at least two desired")

			dsName := sbrparams.SBRAgentDaemonSetPrefix + sbrparams.SBRCBlockWriteGateTestName
			agentDS, dsErr := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
				context.TODO(), dsName, metav1.GetOptions{})
			Expect(dsErr).ToNot(HaveOccurred(), "Failed to get agent DaemonSet %s", dsName)
			Expect(agentDS.Status.DesiredNumberScheduled).To(BeNumerically(">=", 2),
				"The storage write gate only stays shut while at least two agents are desired; "+
					"DaemonSet %s desires %d", dsName, agentDS.Status.DesiredNumberScheduled)
			Expect(agentDS.Status.NumberReady).To(BeEquivalentTo(1),
				"Exactly one agent may be Ready: DaemonSet %s has %d/%d Ready, so a watchdog "+
					"keepalive did not block its agent", dsName,
				agentDS.Status.NumberReady, agentDS.Status.DesiredNumberScheduled)

			By("Verifying the storage write gate is shut before the remediation CR is created")

			writeable, gateErr := sbrcConcurrentWriteable(sbrparams.SBRCBlockWriteGateTestName)
			Expect(gateErr).ToNot(HaveOccurred(),
				"status.storageValidation.concurrentWriteable must be readable on SBRC %q",
				sbrparams.SBRCBlockWriteGateTestName)
			Expect(writeable).To(BeFalse(),
				"SBRC %q must not report concurrentWriteable while only one agent is Ready",
				sbrparams.SBRCBlockWriteGateTestName)

			By(fmt.Sprintf("Selecting the agent on %q as the device observer", readyNodeName))

			// By node, not "any agent off the victim": a blocked agent's pod is briefly Running on
			// each restart, so a lookup by node-inequality could pick one that is about to exit.
			var observerErr error
			observerPodName, observerErr = agentPodNameByNode(
				sbrparams.SBRCBlockWriteGateTestName, readyNodeName, true)
			Expect(observerErr).ToNot(HaveOccurred(),
				"A Running agent pod on node %q must be available to read the device", readyNodeName)
			GinkgoWriter.Printf("Observer agent pod: %s\n", observerPodName)
		})

		AfterAll(func() {
			// The CR goes first. Once the keepalive pods are gone the blocked agents start, the
			// gate opens, and a CR that still exists would be fenced for real.
			if sbrCR != nil {
				By("AfterAll: deleting the StorageBasedRemediation CR")

				if deleteErr := APIClient.Delete(context.TODO(), sbrCR); deleteErr != nil &&
					!k8serrors.IsNotFound(deleteErr) {
					GinkgoWriter.Printf("Warning: delete StorageBasedRemediation/%s: %v\n",
						victimNodeName, deleteErr)
				}

				// Polled rather than asserted: a failed Expect here would abort the rest of this
				// AfterAll and leak the keepalive pods and the SBRC into the next suite.
				gone := false
				deadline := time.Now().Add(sbrparams.SBRCRCleanupTimeout)

				for !gone && time.Now().Before(deadline) {
					_, pullErr := pullSBRCR(victimNodeName)
					gone = k8serrors.IsNotFound(pullErr)

					if !gone {
						time.Sleep(sbrparams.DefaultPollInterval)
					}
				}

				if !gone {
					By("AfterAll: force-removing the StorageBasedRemediation CR")
					cleanupSBRCR(context.Background(), victimNodeName)
				}

				sbrCR = nil
			}

			if victimNodeName != "" {
				By("AfterAll: uncordoning the victim node if it is still cordoned")
				uncordonNodeIfNeeded(victimNodeName)
			}

			if len(keepalivePods) > 0 {
				By("AfterAll: removing watchdog-keepalive pods to release /dev/watchdog")
				deleteKeepalivePods(keepalivePods)
				keepalivePods = nil
			}

			if testSBRC != nil {
				By("AfterAll: deleting the StorageBasedRemediationConfig")
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCBlockWriteGateTestName)
				testSBRC = nil
			}

			By("AfterAll: clearing residual Ceph OSD blocklist entries")
			clearCephOSDBlocklist()
		})

		It("Verify fencing is withheld while the storage write check has not passed in Block mode",
			reportxml.ID("2039"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.ComponentRemediation,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				By(fmt.Sprintf("Creating a StorageBasedRemediation CR for victim node %q", victimNodeName))

				sbrCR = buildSBR(victimNodeName)
				createErr := APIClient.Create(context.TODO(), sbrCR)
				Expect(createErr).ToNot(HaveOccurred(),
					"StorageBasedRemediation CR for %s must be created", victimNodeName)

				By(fmt.Sprintf("Waiting for a %s warning event on the CR", sbrparams.FencingWithheldReason))

				Expect(helpers.WaitForEvents(context.TODO(), EventsClient,
					helpers.InvolvedObjectRef{
						Kind:      "StorageBasedRemediation",
						Name:      victimNodeName,
						Namespace: medik8sparams.OperatorNs,
						UID:       string(sbrCR.GetUID()),
					},
					[]helpers.EventExpectation{
						{
							Reason:        sbrparams.FencingWithheldReason,
							Type:          corev1.EventTypeWarning,
							MessageSubstr: fmt.Sprintf("Withholding fencing for node '%s'", victimNodeName),
						},
					},
					sbrparams.StorageInjectionTimeout, sbrparams.DefaultPollInterval,
				)).To(Succeed(),
					"StorageBasedRemediation/%s must get a %s warning event while the storage write "+
						"check has not passed", victimNodeName, sbrparams.FencingWithheldReason)

				By(fmt.Sprintf("Consistently asserting nothing is fenced for %s",
					sbrparams.BlockNoFencingCheckDuration))

				Consistently(func() error {
					writeable, gateErr := sbrcConcurrentWriteable(sbrparams.SBRCBlockWriteGateTestName)
					if gateErr != nil {
						return fmt.Errorf("read storage write gate on SBRC %s: %w",
							sbrparams.SBRCBlockWriteGateTestName, gateErr)
					}

					if writeable {
						return fmt.Errorf("SBRC %s now reports concurrentWriteable: the gate opened, "+
							"so the rest of this check no longer proves anything",
							sbrparams.SBRCBlockWriteGateTestName)
					}

					// Cordoning is the first thing the reconciler does once past the gate, so this
					// is the primary signal that it never got there.
					node, nodeErr := APIClient.CoreV1Interface.Nodes().Get(
						context.TODO(), victimNodeName, metav1.GetOptions{})
					if nodeErr != nil {
						return fmt.Errorf("get node %s: %w", victimNodeName, nodeErr)
					}

					if !isNodeSchedulable(node) {
						return fmt.Errorf("node %s is no longer schedulable: the reconciler cordoned "+
							"a node it should have withheld", victimNodeName)
					}

					if hasOutOfServiceTaint(node) {
						return fmt.Errorf("node %s carries the out-of-service taint: fencing was "+
							"completed for a node it should have withheld", victimNodeName)
					}

					currentCR, pullErr := pullSBRCR(victimNodeName)
					if pullErr != nil {
						return fmt.Errorf("get StorageBasedRemediation/%s: %w", victimNodeName, pullErr)
					}

					if conditionTrue(currentCR, sbrparams.FencingInProgressCondition) {
						return fmt.Errorf("StorageBasedRemediation/%s reports %s=True",
							victimNodeName, sbrparams.FencingInProgressCondition)
					}

					if conditionTrue(currentCR, sbrparams.FencingSucceededCondition) {
						return fmt.Errorf("StorageBasedRemediation/%s reports %s=True",
							victimNodeName, sbrparams.FencingSucceededCondition)
					}

					// Backs up the cordon check. Weak on its own here: the victim's agent never
					// registered in the node map, so even a reconciler that skipped the gate would
					// fail to resolve the victim's node ID and write nothing.
					fenceSlots, fenceErr := readFenceSlotsFromPod(observerPodName)
					if fenceErr != nil {
						return fmt.Errorf("read fence region through agent pod %s: %w",
							observerPodName, fenceErr)
					}

					for slotIdx, slot := range fenceSlots {
						if slot.Corrupt {
							return fmt.Errorf("fence slot %d holds data that is not a valid SBD fence "+
								"message", slotIdx)
						}

						if slot.Reason != fenceReasonNone {
							return fmt.Errorf("fence slot %d carries reason 0x%02x for target node ID "+
								"%d, written by node ID %d: a fence was delivered while the storage "+
								"write check had not passed",
								slotIdx, slot.Reason, slot.TargetNodeID, slot.WriterNodeID)
						}
					}

					return nil
				}, sbrparams.BlockNoFencingCheckDuration, sbrparams.BlockNoFencingCheckInterval).
					Should(Succeed(),
						"Node %q must not be cordoned, fenced or tainted while SBRC %q has not "+
							"confirmed concurrent writeability",
						victimNodeName, sbrparams.SBRCBlockWriteGateTestName)
			})
	})
