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
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var _ = Describe(
	"SBR Block Storage — Ceph RBD Detection-Only Mode",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR, labels.ComponentRemediation), func() {
		var (
			testSBRC        *unstructured.Unstructured
			victimNodeName  string
			observerPodName string
			templateCreated bool
		)

		BeforeAll(func() {
			By("Verifying the installed SBR operator supports sharedStorageVolumeMode")

			requireSBRCBlockVolumeModeSupport()

			By("Checking the NodeHealthCheck CRD is installed")

			// The second half of the test needs NHC to create the remediation CR for a node whose
			// storage is gone, the same way it would in production.
			if !isNHCCRDInstalled() {
				Skip("NodeHealthCheck CRD not found; NHC operator not installed — skipping block " +
					"detection-only test")
			}

			By("Removing any NodeHealthCheck left by a previous run of this test")

			// Before the "no NHC uses SBR" precondition, so this test's own leftover does not skip it.
			deleteNHCAndWait(sbrparams.NHCBlockDetectOnlyTestName)

			By("Verifying no NodeHealthCheck already remediates through the SBR template")

			// The first half of the test owns the only StorageBasedRemediation CR there is: a
			// foreign NHC creating its own would make "the victim stayed up" unattributable.
			requireNoSBRNodeHealthCheck()

			By("Selecting a victim worker node that hosts no SBR controller pod")

			victimNodeName = pickTargetWorkerNode()
			if victimNodeName == "" {
				Skip("No schedulable worker node found that does not host an SBR controller pod")
			}

			GinkgoWriter.Printf("Victim node for the Ceph RBD detection-only test: %q\n", victimNodeName)

			By("Verifying iptables is usable in the victim node's host namespaces")

			// Probed before anything is created: on a node where iptables cannot be used in the host
			// namespaces (an nftables-only node without the compatibility layer) the storage fault
			// of the second half cannot be injected at all, which is a skip rather than a failure.
			probePhase, probeLog := runHostCommandPod(victimNodeName, sbrparams.BlockPartitionProbePodName,
				cephRBDIPTablesProbeScript(), sbrparams.BlockPartitionPodTimeout)
			if probePhase != corev1.PodSucceeded {
				Skip(fmt.Sprintf("iptables is not usable in the host namespaces of node %q (probe pod %s): %s",
					victimNodeName, probePhase, probeLog))
			}

			testSBRC = createBlockModeSBRCAndWaitReady(sbrparams.SBRCBlockDetectOnlyTestName,
				map[string]interface{}{"detectOnlyMode": sbrparams.DetectOnlyModeEnabled})

			By("Asserting the SBRC reports concurrent-writeable shared storage")

			// The storage write gate is what a peer would have to pass to fence anyone, so proving
			// it is open first is what makes the later "nobody was fenced" mean detect-only mode
			// and not an SBRC that could never have fenced anything in the first place.
			Eventually(func() (bool, error) {
				return sbrcConcurrentWriteable(sbrparams.SBRCBlockDetectOnlyTestName)
			}, sbrparams.StorageInjectionTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
				"SBRC %q must report status.storageValidation.concurrentWriteable=true, so fencing is "+
					"withheld by detect-only mode rather than blocked by storage",
				sbrparams.SBRCBlockDetectOnlyTestName)

			By("Selecting an observer agent pod on a node other than the victim")

			// Every device read goes through this pod: the victim's own agent loses storage access
			// in the second half, so it can never be the reader.
			var observerErr error
			observerPodName, observerErr = agentPodNameByNode(
				sbrparams.SBRCBlockDetectOnlyTestName, victimNodeName, false)
			Expect(observerErr).ToNot(HaveOccurred(),
				"SBRC %q must have a Running agent pod on a node other than victim %q to read the "+
					"shared device through", sbrparams.SBRCBlockDetectOnlyTestName, victimNodeName)

			GinkgoWriter.Printf("Observer agent pod for device reads: %q\n", observerPodName)
		})

		AfterAll(func() {
			// Ordered by what unblocks what, not by what the test created. Lifting the storage
			// fault first is what lets everything after it finish: while the victim is cut off its
			// remediation is still in flight, so the NHC webhook refuses the NHC's deletion and the
			// remediation CR will not drain. The template goes last because both the NHC and the
			// CR still reference it on the way out.
			if victimNodeName != "" {
				By("Lifting the storage block: removing the injector pod and flushing Ceph REJECT rules")
				deleteHostCommandPod(sbrparams.BlockDetectOnlyInjectorPodName)
				flushCephRBDRules(victimNodeName)
			}

			By("Removing the NodeHealthCheck CR")
			cleanupNHCCR(context.Background(), sbrparams.NHCBlockDetectOnlyTestName)

			if victimNodeName != "" {
				By("Removing any StorageBasedRemediation CR left for the victim node")

				// Deleted the ordinary way first. With the NHC gone and storage back, the agent
				// finalizer drains on its own and undoes the cordon and the out-of-service taint it
				// applied. Stripping finalizers skips all of that, so it is the fallback, not the
				// first move — and the CR here is usually NHC's, not the one the test wrote.
				sbrCRGVK := schema.GroupVersionKind{
					Group: sbrparams.CRDGroup, Version: sbrparams.CRDVersion, Kind: "StorageBasedRemediation",
				}
				if !helpers.DeleteRemediationCR(context.Background(), APIClient, sbrCRGVK,
					victimNodeName, medik8sparams.OperatorNs,
					sbrparams.DefaultPollInterval, sbrparams.SBRCRCleanupTimeout, GinkgoWriter.Printf) {
					By("Force-removing the StorageBasedRemediation CR that would not drain")
					cleanupSBRCR(context.Background(), victimNodeName)
				}

				By("Uncordoning the victim node if it is still cordoned")
				uncordonNodeIfNeeded(victimNodeName)
			}

			if testSBRC != nil {
				By(fmt.Sprintf("Removing StorageBasedRemediationConfig %q",
					sbrparams.SBRCBlockDetectOnlyTestName))
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCBlockDetectOnlyTestName)
			}

			if templateCreated {
				By("Removing the StorageBasedRemediationTemplate this test created")
				deleteSBRTemplate()
			}
		})

		It("Detection-only mode: a Ceph RBD block-mode node is reported unhealthy but never "+
			"cordoned, fenced or rebooted",
			reportxml.ID("1961"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.ComponentRemediation,
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				// Detect-only mode has to hold at both ends of a remediation, and the two are
				// enforced by different components, so the test drives each in turn. The peer
				// reconciler must refuse to cordon or fence a node a StorageBasedRemediation CR
				// names (api/v1alpha1: "the controller does not write fence messages"), and the
				// victim's own agent must keep petting its watchdog when it sees a CR for itself.
				By("Step 7: Recording the victim's boot-id before any remediation")

				preFenceBootID, bootErr := getNodeBootID(victimNodeName)
				Expect(bootErr).ToNot(HaveOccurred(),
					"Must read the boot-id of victim node %q before a remediation is requested",
					victimNodeName)
				GinkgoWriter.Printf("Pre-remediation boot-id of node %q: %s\n", victimNodeName, preFenceBootID)

				By("Step 8: Creating a StorageBasedRemediation CR for the victim node")

				// Created by hand rather than through NHC: this half is about what a peer does with
				// a remediation request on its own, so the victim's storage stays healthy and
				// nothing marks it SBRStorageUnhealthy.
				sbrCR := buildSBR(victimNodeName)
				Expect(APIClient.Create(context.TODO(), sbrCR)).To(Succeed(),
					"StorageBasedRemediation CR for node %q must be created", victimNodeName)

				By(fmt.Sprintf("Step 9: Waiting for a peer to report an %s Warning withholding "+
					"remediation for the victim node", sbrparams.DetectOnlyWithholdReason))

				// The positive proof, not just the absence of a fence: the reconciler returns on
				// detect-only before it adds its finalizer, so this event is the only trace that a
				// peer picked the CR up at all. Without it a withheld remediation would be
				// indistinguishable from one no peer ever saw.
				//
				// The victim's own agent also emits against this CR ("Skipping remediation for own
				// node"), so the reason narrows it to the peers; the UID narrows it to this CR
				// rather than a same-named one from an earlier run.
				withholdErr := helpers.WaitForEvents(context.TODO(), EventsClient,
					helpers.InvolvedObjectRef{
						Kind:      sbrparams.SBRRemediationKind,
						Name:      victimNodeName,
						Namespace: medik8sparams.OperatorNs,
						UID:       string(sbrCR.GetUID()),
					},
					[]helpers.EventExpectation{{
						Reason:        sbrparams.DetectOnlyWithholdReason,
						Type:          corev1.EventTypeWarning,
						MessageSubstr: fmt.Sprintf("for node '%s'", victimNodeName),
					}},
					sbrparams.BlockDetectOnlyWithholdTimeout, sbrparams.DefaultPollInterval)
				Expect(withholdErr).ToNot(HaveOccurred(),
					"A peer must emit an %s Warning withholding cordon/fence/taint for node %q "+
						"instead of remediating it", sbrparams.DetectOnlyWithholdReason, victimNodeName)

				By(fmt.Sprintf("Step 10: Consistently asserting for %s that no peer cordons, fences or "+
					"taints the victim", sbrparams.DetectOnlySuppressionCheckDuration))

				// Every clause is one of the three things the withheld path names, checked where it
				// would actually land: the fence in the slot the victim polls, read off the device
				// through the observer rather than inferred from the CR's conditions, and the cordon
				// and taint on the Node itself.
				//
				// Ready is asserted alongside the boot-id because the boot-id only moves once
				// kubelet reports in after a reboot has finished — a reboot starting inside this
				// window would show as NotReady first and could otherwise slip through.
				Consistently(func() error {
					if fenceErr := noLiveFenceMessageFor(observerPodName, victimNodeName); fenceErr != nil {
						return fmt.Errorf("a peer fenced node %s on storage while detectOnlyMode is "+
							"Enabled: %w", victimNodeName, fenceErr)
					}

					node, getErr := APIClient.CoreV1Interface.Nodes().Get(
						context.TODO(), victimNodeName, metav1.GetOptions{})
					if getErr != nil {
						return fmt.Errorf("get node %s: %w", victimNodeName, getErr)
					}

					if node.Spec.Unschedulable {
						return fmt.Errorf("node %s was cordoned while detectOnlyMode is Enabled",
							victimNodeName)
					}

					if hasOutOfServiceTaint(node) {
						return fmt.Errorf("node %s was given the %s taint while detectOnlyMode is "+
							"Enabled", victimNodeName, sbrparams.OutOfServiceTaintKey)
					}

					bootID, bootIDErr := getNodeBootID(victimNodeName)
					if bootIDErr != nil {
						return bootIDErr
					}

					if bootID != preFenceBootID {
						return fmt.Errorf("node %s rebooted on a remediation request while "+
							"detectOnlyMode is Enabled: boot-id %s -> %s",
							victimNodeName, preFenceBootID, bootID)
					}

					return nodeConditionIs(victimNodeName, string(corev1.NodeReady), corev1.ConditionTrue, "")
				}, sbrparams.DetectOnlySuppressionCheckDuration,
					sbrparams.DetectOnlySuppressionCheckInterval).Should(Succeed(),
					"Node %q must stay up, Ready, uncordoned, untainted and unfenced while a "+
						"StorageBasedRemediation CR names it: detect-only mode remediates nothing",
					victimNodeName)

				By("Step 11: Deleting the StorageBasedRemediation CR and confirming the victim is " +
					"still schedulable")

				// The withheld path returns before the finalizer is added, so the CR goes away at
				// once and there is nothing for a peer to undo. The wait is kept as the guard on
				// entering the NHC half: it must start from a node that is genuinely untouched.
				Expect(APIClient.Delete(context.TODO(), sbrCR)).To(Succeed(),
					"StorageBasedRemediation CR for node %q must be deletable", victimNodeName)

				Eventually(func() error {
					node, getErr := APIClient.CoreV1Interface.Nodes().Get(
						context.TODO(), victimNodeName, metav1.GetOptions{})
					if getErr != nil {
						return fmt.Errorf("get node %s: %w", victimNodeName, getErr)
					}

					if !isNodeSchedulable(node) {
						return fmt.Errorf("node %s is not schedulable yet (cordoned or tainted)",
							victimNodeName)
					}

					if hasOutOfServiceTaint(node) {
						return fmt.Errorf("node %s still carries the %s taint",
							victimNodeName, sbrparams.OutOfServiceTaintKey)
					}

					return nil
				}, sbrparams.SBRCRCleanupTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Node %q must be schedulable with no %s taint once its remediation CR is deleted",
					victimNodeName, sbrparams.OutOfServiceTaintKey)

				By("Step 12: Creating the NodeHealthCheck that remediates SBRStorageUnhealthy nodes " +
					"through SBR")

				// From here the test drives the other reboot route: the victim loses its storage
				// entirely, so it can no longer read its own fence slot, and only the watchdog could
				// still reboot it. NHC has to be the one creating the CR, because that is what the
				// victim's own agent looks for before it stops petting.
				//
				// The template is owned by AfterAll rather than a DeferCleanup: the template has to outlive both
				// the NHC that references it and the remediation CR still draining under it.
				templateCreated = createSBRTemplateIfMissing()

				// maxUnhealthy: 1 rather than the shared builder's minHealthy: "51%", so exactly one
				// node may be unhealthy regardless of cluster size.
				nhc := buildNHCWithMaxUnhealthy(sbrparams.NHCBlockDetectOnlyTestName, 1)
				Expect(APIClient.Create(context.TODO(), nhc)).To(Succeed(),
					"NodeHealthCheck CR %q must be created", sbrparams.NHCBlockDetectOnlyTestName)

				By("Asserting SBRStorageUnhealthy is not True on the victim node before its Ceph " +
					"traffic is blocked")

				// Absent, False or Unknown all pass; only True is a failed precondition.
				Expect(transientNodeHasCondition(victimNodeName,
					sbrparams.SBRStorageUnhealthyCondition, false)).To(Succeed(),
					"Node %q must not report %s=True before its Ceph traffic is blocked",
					victimNodeName, sbrparams.SBRStorageUnhealthyCondition)

				preIsolationBootID, isolationBootErr := getNodeBootID(victimNodeName)
				Expect(isolationBootErr).ToNot(HaveOccurred(),
					"Must read the boot-id of victim node %q before its storage is cut off", victimNodeName)

				By(fmt.Sprintf("Step 13: Starting a privileged injector pod that blocks Ceph traffic in "+
					"both directions on the victim node %q", victimNodeName))

				injectorPod, injectorErr := startPrivilegedInjectorPod(
					victimNodeName, sbrparams.BlockDetectOnlyInjectorPodName)
				Expect(injectorErr).ToNot(HaveOccurred(),
					"Privileged injector pod must start on victim node %q", victimNodeName)

				// Both directions and no time limit: the victim must lose the shared device entirely
				// and keep it lost, so it can neither heartbeat nor read its own fence slot. The only
				// route to a reboot left is its watchdog, which detect-only mode never arms.
				injectOut, injectErr := injectCephRBDRulesBidirectional(injectorPod)
				Expect(injectErr).ToNot(HaveOccurred(),
					"Ceph REJECT rules must be inserted on both chains on victim node %q (output: %s)",
					victimNodeName, injectOut)
				GinkgoWriter.Printf("Ceph port REJECT rules applied in both directions on node %q\n",
					victimNodeName)

				By(fmt.Sprintf("Step 14: Waiting for SBRStorageUnhealthy=True (reason %s) on the victim node",
					sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout))

				// Detection is the half of detect-only mode that must still work: a peer reads the
				// silent heartbeat slot off the device and flags the node for NHC.
				Eventually(func() error {
					return nodeConditionIs(victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
						corev1.ConditionTrue, sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout)
				}, sbrparams.StorageInjectionTimeout, sbrparams.StorageInjectionPollInterval).Should(Succeed(),
					"A peer must report storage-isolated node %q %s=True (reason %s) even in "+
						"detect-only mode", victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
					sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout)

				By("Step 15: Waiting for NHC to create a StorageBasedRemediation CR for the victim node")

				Eventually(func() error {
					_, pullErr := pullSBRCR(victimNodeName)
					if k8serrors.IsNotFound(pullErr) {
						return fmt.Errorf("StorageBasedRemediation/%s not yet created by NHC", victimNodeName)
					}

					return pullErr
				}, sbrparams.NHCSBRCRCreationTimeout, sbrparams.NHCSBRCRCreationPollInterval).Should(Succeed(),
					"NHC must create a StorageBasedRemediation CR for storage-isolated node %q",
					victimNodeName)

				// The no-reboot window is counted from here, not from the event in step 16: this is
				// the moment both reboot routes open, and anything spent getting to step 17 is time
				// the victim was already free to reboot in.
				crAppearedAt := time.Now()

				GinkgoWriter.Printf("StorageBasedRemediation/%s created by NHC\n", victimNodeName)

				By(fmt.Sprintf("Step 16: Waiting for an %s Warning on the SBRC naming the victim node",
					sbrparams.SBRUnhealthyDetectOnlyReason))

				// This event is the positive proof, not just the absence of a reboot: it is what the
				// agent's watchdog tick emits in place of stopping to pet, so seeing it means the
				// agent reached the exact decision point where it would otherwise have rebooted.
				//
				// The agent records it against the StorageBasedRemediationConfig it runs under, not
				// against the node or its own pod, so the node is identified by the message text
				// instead. Matching on the message matters: every agent shares the same involved
				// object, so without it another node's event would satisfy this wait.
				eventErr := helpers.WaitForEvents(context.TODO(), EventsClient,
					helpers.InvolvedObjectRef{
						Kind:      sbrparams.SBRCKind,
						Name:      sbrparams.SBRCBlockDetectOnlyTestName,
						Namespace: medik8sparams.OperatorNs,
						UID:       string(testSBRC.GetUID()),
					},
					[]helpers.EventExpectation{{
						Reason:        sbrparams.SBRUnhealthyDetectOnlyReason,
						Type:          corev1.EventTypeWarning,
						MessageSubstr: victimNodeName,
					}},
					sbrparams.StorageInjectionTimeout, sbrparams.DefaultPollInterval)
				Expect(eventErr).ToNot(HaveOccurred(),
					"The agent on storage-isolated node %q must emit an %s Warning instead of stopping "+
						"to pet its watchdog", victimNodeName, sbrparams.SBRUnhealthyDetectOnlyReason)

				noRebootWindow := sbrparams.BlockDetectOnlyNoRebootWindow - time.Since(crAppearedAt)
				if noRebootWindow < sbrparams.BlockDetectOnlyNoRebootMinWindow {
					noRebootWindow = sbrparams.BlockDetectOnlyNoRebootMinWindow
				}

				By(fmt.Sprintf("Step 17: Consistently asserting for %s that the victim is never "+
					"rebooted by its watchdog", noRebootWindow))

				// Ready is the real assertion here; the boot-id is the corroboration. A boot-id only
				// moves once the node has rebooted all the way and kubelet has reported back in, so
				// a reboot that starts late in the window, or a slow bare-metal one, would never
				// show up in it. A node that has begun going down loses Ready long before that.
				Consistently(func() error {
					if readyErr := nodeConditionIs(victimNodeName, string(corev1.NodeReady),
						corev1.ConditionTrue, ""); readyErr != nil {
						return fmt.Errorf("node %s stopped being Ready while detectOnlyMode is "+
							"Enabled, which is how a reboot starts: %w", victimNodeName, readyErr)
					}

					bootID, bootIDErr := getNodeBootID(victimNodeName)
					if bootIDErr != nil {
						return bootIDErr
					}

					if bootID != preIsolationBootID {
						return fmt.Errorf("node %s was rebooted while detectOnlyMode is Enabled: "+
							"boot-id %s -> %s", victimNodeName, preIsolationBootID, bootID)
					}

					return nil
				}, noRebootWindow, sbrparams.BlockDetectOnlyNoRebootInterval).Should(Succeed(),
					"Storage-isolated node %q must stay Ready on boot-id %q: detect-only mode never "+
						"arms the watchdog, so nothing may reboot it", victimNodeName, preIsolationBootID)

				By("Asserting one last time that the victim is still on its original boot-id")

				// The window closes on the last poll before its deadline, so a reboot that landed in
				// the gap after it would otherwise go unseen. This is the final word on the whole
				// spec: whatever else happened, the node was never restarted.
				finalBootID, finalBootErr := getNodeBootID(victimNodeName)
				Expect(finalBootErr).ToNot(HaveOccurred(),
					"Must read the boot-id of node %q at the end of the no-reboot window", victimNodeName)
				Expect(finalBootID).To(Equal(preIsolationBootID),
					"Node %q must still be on boot-id %q when the test ends: nothing may reboot a node "+
						"while detectOnlyMode is Enabled", victimNodeName, preIsolationBootID)

				// Recorded, not asserted. The reconciler withholds the taint along with the cordon
				// and the fence once detectOnlyMode is Enabled, so on a fixed build this never
				// fires; it stays in as a regression tripwire for RHWA-1877, where peers proved a
				// live node's death from storage and tainted it. Left as a report entry rather than
				// an expectation so the spec keeps passing on what it set out to measure — that
				// nothing rebooted — and the taint is visible if it ever comes back.
				victimNode, victimGetErr := APIClient.CoreV1Interface.Nodes().Get(
					context.TODO(), victimNodeName, metav1.GetOptions{})
				if victimGetErr == nil && hasOutOfServiceTaint(victimNode) {
					AddReportEntry("sbr-block-detect-only-oos-taint", fmt.Sprintf(
						"Node %s carries the %s taint while detectOnlyMode is Enabled: a peer fenced "+
							"a live node, so the remediation reconciler is not honouring "+
							"detectOnlyMode", victimNodeName, sbrparams.OutOfServiceTaintKey))
					GinkgoWriter.Printf("Note: node %q was tainted %s although it was never rebooted\n",
						victimNodeName, sbrparams.OutOfServiceTaintKey)
				}

				GinkgoWriter.Printf("Block-mode detection-only complete: node %q was flagged "+
					"unhealthy, never remediated, and stayed on boot-id %q throughout\n",
					victimNodeName, preIsolationBootID)
			})
	})
