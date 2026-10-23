package tests

import (
	"context"
	"fmt"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// keepalivePodName returns a valid pod name for the per-node watchdog-keepalive pod.
func keepalivePodName(nodeName string) string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}

		return '-'
	}, strings.ToLower(nodeName))

	name := sbrparams.WatchdogKeepaliveNamePrefix + safe
	if len(name) > 63 {
		name = name[:63]
	}

	return strings.TrimRight(name, "-")
}

// deleteKeepalivePods removes any running watchdog-keepalive pods. Safe to call when pods may not exist.
func deleteKeepalivePods(podNames []string) {
	for _, podName := range podNames {
		kp, pullErr := pod.Pull(APIClient, podName, medik8sparams.OperatorNs)
		if pullErr != nil {
			continue
		}

		if _, delErr := kp.Delete(); delErr != nil && !k8serrors.IsNotFound(delErr) {
			GinkgoT().Logf("Warning: delete keepalive pod %s: %v", podName, delErr)
		}
	}
}

// detectOnlyAgentPodsRunning returns nil once the agent DaemonSet has rolled out its current
// pod template and every desired pod has a running container. It checks Running instead of
// Ready because the agent probes fail on healthy agents (RHWA-1759, RHWA-1057). Switch to
// Ready once those are fixed.
func detectOnlyAgentPodsRunning(dsName string) error {
	agentDS, dsErr := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
		context.TODO(), dsName, metav1.GetOptions{})
	if dsErr != nil {
		return fmt.Errorf("DaemonSet %s not found: %w", dsName, dsErr)
	}

	desired := agentDS.Status.DesiredNumberScheduled
	if desired == 0 {
		return fmt.Errorf("DaemonSet %s: no pods scheduled yet", dsName)
	}

	if agentDS.Status.ObservedGeneration < agentDS.Generation {
		return fmt.Errorf("DaemonSet %s: generation %d not yet observed (observed %d)",
			dsName, agentDS.Generation, agentDS.Status.ObservedGeneration)
	}

	if agentDS.Status.CurrentNumberScheduled < desired || agentDS.Status.UpdatedNumberScheduled < desired {
		return fmt.Errorf("DaemonSet %s: %d/%d pods scheduled, %d/%d updated", dsName,
			agentDS.Status.CurrentNumberScheduled, desired, agentDS.Status.UpdatedNumberScheduled, desired)
	}

	listOpts := metav1.ListOptions{}

	if agentDS.Spec.Selector != nil {
		sel, selErr := metav1.LabelSelectorAsSelector(agentDS.Spec.Selector)
		if selErr != nil {
			return fmt.Errorf("converting DaemonSet selector: %w", selErr)
		}

		listOpts.LabelSelector = sel.String()
	}

	podList, podErr := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).List(
		context.TODO(), listOpts)
	if podErr != nil {
		return fmt.Errorf("listing agent pods: %w", podErr)
	}

	var running int

	for i := range podList.Items {
		agentPod := &podList.Items[i]

		if !metav1.IsControlledBy(agentPod, agentDS) || agentPod.DeletionTimestamp != nil {
			continue
		}

		if agentPod.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("pod %s on node %s is %s", agentPod.Name, agentPod.Spec.NodeName, agentPod.Status.Phase)
		}

		hasRunningContainer := false

		for j := range agentPod.Status.ContainerStatuses {
			if agentPod.Status.ContainerStatuses[j].State.Running != nil {
				hasRunningContainer = true

				break
			}
		}

		if !hasRunningContainer {
			return fmt.Errorf("pod %s is Running phase but no container has Running state (crash-looping?)",
				agentPod.Name)
		}

		running++
	}

	if running < int(desired) {
		return fmt.Errorf("DaemonSet %s: %d/%d pods with running containers", dsName, running, desired)
	}

	return nil
}

// setDetectOnlyMode patches the detect-only SBRC to the given mode, waits for the spec to
// reflect it, and waits for the agent DaemonSet to roll out pods running with the new mode.
func setDetectOnlyMode(gvkSource *unstructured.Unstructured, mode string) {
	By(fmt.Sprintf("Patching StorageBasedRemediationConfig to detectOnlyMode: %s", mode))

	patchTarget := &unstructured.Unstructured{}
	patchTarget.SetGroupVersionKind(gvkSource.GroupVersionKind())
	patchTarget.SetName(sbrparams.SBRCDetectOnlyTestName)
	patchTarget.SetNamespace(medik8sparams.OperatorNs)
	patchErr := APIClient.Patch(context.TODO(), patchTarget,
		client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"spec":{"detectOnlyMode":%q}}`, mode)))
	Expect(patchErr).ToNot(HaveOccurred(),
		"Patching StorageBasedRemediationConfig detectOnlyMode to %s must succeed", mode)

	By(fmt.Sprintf("Verifying detectOnlyMode is now %s in the StorageBasedRemediationConfig spec", mode))

	Eventually(func() error {
		freshSBRC := &unstructured.Unstructured{}
		freshSBRC.SetGroupVersionKind(gvkSource.GroupVersionKind())

		if fetchErr := APIClient.Get(context.TODO(),
			types.NamespacedName{Name: sbrparams.SBRCDetectOnlyTestName, Namespace: medik8sparams.OperatorNs},
			freshSBRC); fetchErr != nil {
			return fmt.Errorf("failed to fetch StorageBasedRemediationConfig: %w", fetchErr)
		}

		currentMode, found, nestedErr := unstructured.NestedString(freshSBRC.Object, "spec", "detectOnlyMode")
		if nestedErr != nil {
			return fmt.Errorf("reading spec.detectOnlyMode: %w", nestedErr)
		}

		if !found || currentMode != mode {
			return fmt.Errorf("detectOnlyMode is %q, want %s", currentMode, mode)
		}

		return nil
	}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"StorageBasedRemediationConfig detectOnlyMode must reflect %s after patch", mode)

	By(fmt.Sprintf("Verifying agent DaemonSet rolls out running pods with detectOnlyMode: %s", mode))

	dsName := sbrparams.SBRAgentDaemonSetPrefix + sbrparams.SBRCDetectOnlyTestName

	Eventually(func() error {
		return detectOnlyAgentPodsRunning(dsName)
	}, sbrparams.SBRCReadyTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		func() string {
			return fmt.Sprintf("Agent DaemonSet %s must run all pods after setting detectOnlyMode to %s\n%s",
				dsName, mode, sbrcReadinessDiagnostics(sbrparams.SBRCDetectOnlyTestName, dsName))
		})
}

var _ = Describe(
	"SBR Functional — detectOnlyMode",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR), func() {
		var (
			detectOnlySBRC  *unstructured.Unstructured
			rwxStorageClass string
			targetNodeName  string
			injectorPodName string
			keepalivePods   []string // pod names, one per worker node
		)

		BeforeAll(func() {
			By("Verifying SBR operator deployment is ready")

			sbrDeployment, pullErr := deployment.Pull(
				APIClient, sbrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
			Expect(pullErr).ToNot(HaveOccurred(), "Failed to get SBR operator deployment")
			Expect(sbrDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
				"SBR operator deployment must be Ready before running detectOnlyMode tests")

			By("Discovering RWX storage class")

			rwxStorageClass = discoverRWXStorageClass()
			if rwxStorageClass == "" {
				Skip("No RWX storage class found; set SBR_STORAGE_CLASS env or deploy ODF/CephFS before running")
			}

			GinkgoWriter.Printf("Using storage class: %s\n", rwxStorageClass)

			By("Listing schedulable worker nodes")

			nodeList, listErr := APIClient.CoreV1Interface.Nodes().List(context.TODO(), metav1.ListOptions{
				LabelSelector: "node-role.kubernetes.io/worker",
			})
			Expect(listErr).ToNot(HaveOccurred(), "Failed to list worker nodes")

			var workerNodes []string

			for i := range nodeList.Items {
				node := &nodeList.Items[i]
				if isNodeSchedulable(node) {
					workerNodes = append(workerNodes, node.Name)
				}
			}

			Expect(workerNodes).ToNot(BeEmpty(), "No schedulable worker nodes found")

			// Inject storage faults on a node that does not run the SBR controller: blocking
			// CephFS on the controller's own node prevents the SBRStorageUnhealthy condition
			// from being reported, so the test would time out waiting for it.
			targetNodeName = pickTargetWorkerNode()
			Expect(targetNodeName).ToNot(BeEmpty(),
				"No schedulable worker node without an SBR controller pod found")
			injectorPodName = "sbr-detect-only-injector-" + strings.Map(func(r rune) rune {
				if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
					return r
				}

				return '-'
			}, strings.ToLower(targetNodeName))

			if len(injectorPodName) > 63 {
				injectorPodName = injectorPodName[:63]
			}

			injectorPodName = strings.TrimRight(injectorPodName, "-")

			By("Cleaning up any stale detectOnly StorageBasedRemediationConfig from a previous run")

			Eventually(func() error {
				stale := buildSBRC(sbrparams.SBRCDetectOnlyTestName, map[string]interface{}{})
				deleteErr := APIClient.Delete(context.TODO(), stale)
				if deleteErr == nil || k8serrors.IsNotFound(deleteErr) {
					return nil
				}

				return deleteErr
			}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
				"Pre-test cleanup of stale SBRC %s failed", sbrparams.SBRCDetectOnlyTestName)

			By("Waiting for stale StorageBasedRemediationConfig to be fully removed before recreating")

			Eventually(func() error {
				staleCheck := buildSBRC(sbrparams.SBRCDetectOnlyTestName, map[string]interface{}{})
				getErr := APIClient.Get(context.TODO(),
					types.NamespacedName{Name: sbrparams.SBRCDetectOnlyTestName, Namespace: medik8sparams.OperatorNs},
					staleCheck)
				if k8serrors.IsNotFound(getErr) {
					return nil
				}

				if getErr != nil {
					return getErr
				}

				return fmt.Errorf("SBRC %s still terminating", sbrparams.SBRCDetectOnlyTestName)
			}, sbrparams.SBRCReadyTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
				"Stale SBRC must be fully gone before recreating")

			if os.Getenv("SBR_TEST_RHWA1068") == "true" {
				By("Creating watchdog-keepalive pods on all worker nodes (RHWA-1068 precondition)")

				for _, nodeName := range workerNodes {
					podName := keepalivePodName(nodeName)
					keepalivePod, createErr := pod.NewBuilder(
						APIClient, podName, medik8sparams.OperatorNs, sbrparams.WatchdogDebugImage).
						DefineOnNode(nodeName).
						WithHostPid(true).
						WithPrivilegedFlag().
						RedefineDefaultCMD([]string{
							"/bin/bash", "-c",
							"nsenter --target 1 --mount -- sh -c " +
								"'exec 200>/dev/watchdog 2>/dev/null; " +
								"while true; do printf V >&200 2>/dev/null; sleep 10; done' " +
								"|| sleep 999999",
						}).
						CreateAndWaitUntilRunning(sbrparams.WatchdogKeepaliveTimeout)
					if createErr != nil {
						GinkgoT().Logf("Warning: keepalive pod %s on node %s: %v; "+
							"watchdog contention may not be simulated on this node", podName, nodeName, createErr)
					} else {
						GinkgoWriter.Printf("Keepalive pod %s Running on node %s\n",
							keepalivePod.Definition.Name, nodeName)
						keepalivePods = append(keepalivePods, podName)
					}
				}
			} else {
				GinkgoWriter.Printf("Skipping watchdog-keepalive pods — " +
					"SBR_TEST_RHWA1068 not set (RHWA-1068 regression test will be skipped)\n")
			}

			By("Creating StorageBasedRemediationConfig with detectOnlyMode: Enabled")

			// Minimum sbrTimeoutSeconds (heartbeat = timeout/2) so a peer marks the
			// storage-isolated node SBRStorageUnhealthy=True after ~MaxConsecutiveFailures
			// heartbeats well within StorageInjectionTimeout. The default (30s) leaves too
			// thin a margin under the injection wait and makes the test flaky.
			detectOnlySBRC = buildSBRC(sbrparams.SBRCDetectOnlyTestName, map[string]interface{}{
				"detectOnlyMode":     "Enabled",
				"sharedStorageClass": rwxStorageClass,
				"sbrTimeoutSeconds":  int64(sbrparams.SBRCTimeoutSecondsMin),
			})
			createErr := APIClient.Create(context.TODO(), detectOnlySBRC)
			Expect(createErr).ToNot(HaveOccurred(),
				"StorageBasedRemediationConfig with detectOnlyMode: Enabled must be admitted by the API server")

			By("Waiting for agent DaemonSet pods to be Running with detectOnlyMode: Enabled")

			// The operator's readiness probe gates on /dev/watchdog existing as a char
			// device. On IPI-AWS clusters the watchdog kernel module is not loaded, so
			// pods never become Ready even though detectOnlyMode disarms the watchdog.
			// Check Running phase instead until the operator relaxes the probe (upstream bug).
			dsName := sbrparams.SBRAgentDaemonSetPrefix + sbrparams.SBRCDetectOnlyTestName

			Eventually(func() error {
				clearStaleODFFences()

				return detectOnlyAgentPodsRunning(dsName)
			}, sbrparams.SBRCReadyTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
				func() string {
					return fmt.Sprintf(
						"SBRC %q agent DaemonSet pods must be Running before detectOnlyMode tests begin\n%s",
						sbrparams.SBRCDetectOnlyTestName,
						sbrcReadinessDiagnostics(sbrparams.SBRCDetectOnlyTestName, dsName))
				})
		})

		AfterAll(func() {
			By("AfterAll: removing watchdog-keepalive pods")

			deleteKeepalivePods(keepalivePods)
			keepalivePods = nil

			By("AfterAll: flushing any remaining injector pod and iptables rules")

			cleanupPod, pullErr := pod.Pull(APIClient, injectorPodName, medik8sparams.OperatorNs)
			if pullErr == nil {
				removeCephFSRejectBidirectional(cleanupPod)

				By("AfterAll: verifying no residual CephFS iptables REJECT rules remain")
				verifyCephFSIPTablesClean(cleanupPod)

				if _, delErr := cleanupPod.Delete(); delErr != nil && !k8serrors.IsNotFound(delErr) {
					GinkgoWriter.Printf("Warning: delete injector pod %s: %v\n", injectorPodName, delErr)
				}
			}

			By("AfterAll: removing detect-only StorageBasedRemediationConfig")

			if detectOnlySBRC != nil {
				if deleteErr := APIClient.Delete(context.TODO(), detectOnlySBRC); deleteErr != nil {
					if !k8serrors.IsNotFound(deleteErr) {
						GinkgoT().Logf("Warning: AfterAll cleanup delete %s failed: %v",
							sbrparams.SBRCDetectOnlyTestName, deleteErr)
					}
				} else {
					Eventually(func() error {
						getErr := APIClient.Get(context.TODO(),
							types.NamespacedName{
								Name:      sbrparams.SBRCDetectOnlyTestName,
								Namespace: medik8sparams.OperatorNs,
							},
							detectOnlySBRC.DeepCopy())

						if k8serrors.IsNotFound(getErr) {
							return nil
						}

						if getErr != nil {
							return getErr
						}

						return fmt.Errorf("SBRC %s still present after delete", sbrparams.SBRCDetectOnlyTestName)
					}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed())
				}
			}
		})

		// RHWA-1068: SBR agent crashes on startup in detect-only mode when watchdog device is busy.
		// The keepalive pods created in BeforeAll hold /dev/watchdog open to simulate the condition.
		// This test is guarded by SBR_TEST_RHWA1068=true because the bug is not yet fixed in the
		// current operator version. Enable it once the operator fix ships.
		It("RHWA-1068 regression: agent pods reach Ready despite /dev/watchdog held by another process",
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.FrequencyNightly,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.ComponentController,
			), func() {
				if os.Getenv("SBR_TEST_RHWA1068") != "true" {
					GinkgoWriter.Printf("WARNING: skipping RHWA-1068 regression check — " +
						"bug not yet fixed in current operator version. " +
						"Set SBR_TEST_RHWA1068=true once the operator fix ships.\n")
					Skip("RHWA-1068 not yet fixed; set SBR_TEST_RHWA1068=true to enable this check")
				}

				Expect(keepalivePods).ToNot(BeEmpty(),
					"RHWA-1068 regression requires at least one keepalive pod holding /dev/watchdog; "+
						"none were successfully created")

				DeferCleanup(func() {
					By("DeferCleanup: deleting watchdog-keepalive pods to release /dev/watchdog")

					deleteKeepalivePods(keepalivePods)
					keepalivePods = nil
				})

				dsName := sbrparams.SBRAgentDaemonSetPrefix + sbrparams.SBRCDetectOnlyTestName

				By("Asserting ALL agent DaemonSet pods are Ready — none should be in CrashLoopBackOff")
				// Without the RHWA-1068 fix the watchdog preflight runs unconditionally before the
				// agent checks detectOnlyMode. All pods fail with "watchdog device is not available"
				// and NumberReady stays at 0 until this Eventually times out.
				Eventually(func() error {
					agentDS, err := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
						context.TODO(), dsName, metav1.GetOptions{})
					if err != nil {
						return fmt.Errorf("DaemonSet %s not found: %w", dsName, err)
					}

					if agentDS.Status.DesiredNumberScheduled == 0 ||
						agentDS.Status.NumberReady < agentDS.Status.DesiredNumberScheduled {
						return fmt.Errorf("DaemonSet %s: %d/%d pods ready — some agents may be in CrashLoopBackOff",
							dsName, agentDS.Status.NumberReady, agentDS.Status.DesiredNumberScheduled)
					}

					return nil
				}, sbrparams.DetectOnlyAllPodsReadyTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"All agent pods must be Ready; detect-only mode must skip the watchdog preflight (RHWA-1068)")

				By("Verifying no agent pod log contains the RHWA-1068 crash message")

				agentDS, dsErr := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
					context.TODO(), dsName, metav1.GetOptions{})
				Expect(dsErr).ToNot(HaveOccurred(), "Failed to get agent DaemonSet %s", dsName)

				var selectorParts []string

				for k, v := range agentDS.Spec.Selector.MatchLabels {
					selectorParts = append(selectorParts, k+"="+v)
				}

				agentPodList, listErr := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).List(
					context.TODO(), metav1.ListOptions{LabelSelector: strings.Join(selectorParts, ",")})
				Expect(listErr).ToNot(HaveOccurred(), "Failed to list agent pods for DaemonSet %s", dsName)

				const crashMsg = "Pre-flight checks failed: watchdog device is not available"

				for i := range agentPodList.Items {
					agentPod := &agentPodList.Items[i]
					rawLogs, logsErr := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).
						GetLogs(agentPod.Name, &corev1.PodLogOptions{}).DoRaw(context.TODO())
					if logsErr != nil {
						GinkgoWriter.Printf("Warning: could not fetch logs for agent pod %s: %v\n",
							agentPod.Name, logsErr)

						continue
					}

					Expect(string(rawLogs)).ToNot(ContainSubstring(crashMsg),
						"Agent pod %s must not log watchdog preflight failure in detect-only mode (RHWA-1068)",
						agentPod.Name)

					// If the pod restarted, the crash is in the previous (terminated) container.
					for _, cs := range agentPod.Status.ContainerStatuses {
						if cs.RestartCount > 0 {
							prevLogs, prevErr := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).
								GetLogs(agentPod.Name, &corev1.PodLogOptions{Previous: true}).DoRaw(context.TODO())
							if prevErr == nil {
								Expect(string(prevLogs)).ToNot(ContainSubstring(crashMsg),
									"Agent pod %s previous container must not log watchdog preflight failure (RHWA-1068)",
									agentPod.Name)
							}

							break
						}
					}
				}
			})

		It("Verify SBRC detectOnlyMode field: spec reflection, storage-fault suppression, toggle, and DaemonSet GC",
			reportxml.ID("88876"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.FrequencyNightly,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.ComponentController,
			), func() {
				DeferCleanup(func() {
					By("DeferCleanup: flushing iptables REJECT rules and deleting injector pod")

					cleanupPod, pullErr := pod.Pull(APIClient, injectorPodName, medik8sparams.OperatorNs)
					if pullErr == nil {
						removeCephFSRejectBidirectional(cleanupPod)

						By("DeferCleanup: verifying no residual CephFS iptables REJECT rules remain")
						verifyCephFSIPTablesClean(cleanupPod)

						if _, delErr := cleanupPod.Delete(); delErr != nil && !k8serrors.IsNotFound(delErr) {
							GinkgoWriter.Printf("Warning: delete injector pod %s: %v\n", injectorPodName, delErr)
						}
					}
				})

				By("Fetching the StorageBasedRemediationConfig from the cluster")

				liveSBRC := &unstructured.Unstructured{}
				liveSBRC.SetGroupVersionKind(detectOnlySBRC.GroupVersionKind())
				getErr := APIClient.Get(context.TODO(),
					types.NamespacedName{Name: sbrparams.SBRCDetectOnlyTestName, Namespace: medik8sparams.OperatorNs},
					liveSBRC)
				Expect(getErr).ToNot(HaveOccurred(),
					"StorageBasedRemediationConfig %q must exist on the cluster", sbrparams.SBRCDetectOnlyTestName)
				mode, found, nestedErr := unstructured.NestedString(liveSBRC.Object, "spec", "detectOnlyMode")
				Expect(nestedErr).ToNot(HaveOccurred(),
					"detectOnlyMode field must be readable from StorageBasedRemediationConfig spec")
				Expect(found).To(BeTrue(),
					"detectOnlyMode must be present in StorageBasedRemediationConfig spec")
				Expect(mode).To(Equal("Enabled"),
					"detectOnlyMode must be Enabled in the StorageBasedRemediationConfig spec")

				// Toggle before injecting the storage fault. A toggle restarts every agent pod, and
				// after the fault the target node's CephFS client can be blocklisted so a new agent
				// pod there can never mount the shared volume.
				setDetectOnlyMode(detectOnlySBRC, "Disabled")
				setDetectOnlyMode(detectOnlySBRC, "Enabled")

				// Configuring SBR as an NHC remediator while detectOnlyMode is Enabled is a
				// misconfiguration (RHWA-1876), and the SBR remediation controller does not check
				// detectOnlyMode before cordoning and fencing (RHWA-1877). Fail before injecting the
				// storage fault so such an NHC cannot fence the target node.
				By("Verifying no NodeHealthCheck uses SBR as its remediator")

				preflightCtx, cancelPreflight := context.WithTimeout(context.Background(), medik8sparams.DefaultTimeout)
				defer cancelPreflight()

				Expect(nhcsUsingSBRTemplate(preflightCtx)).To(BeEmpty(),
					"No NodeHealthCheck may reference a %s while detectOnlyMode is Enabled", sbrparams.SBRTemplateKind)

				By(fmt.Sprintf("Verifying no StorageBasedRemediation CR exists for target node %q", targetNodeName))

				// This spec never creates a remediation CR, so it must not delete one owned by
				// another actor. Allow an earlier spec's terminating CR to disappear on its own.
				Eventually(func() (bool, error) {
					staleCR, staleCRErr := pullSBRCR(targetNodeName)
					if k8serrors.IsNotFound(staleCRErr) {
						return true, nil
					}

					if k8serrors.IsForbidden(staleCRErr) || k8serrors.IsUnauthorized(staleCRErr) {
						return false, StopTrying("Cannot check the target node's remediation CR").Wrap(staleCRErr)
					}

					if staleCRErr != nil {
						return false, fmt.Errorf("checking StorageBasedRemediation/%s: %w", targetNodeName, staleCRErr)
					}

					if staleCR.GetDeletionTimestamp() == nil {
						return false, StopTrying(fmt.Sprintf(
							"StorageBasedRemediation/%s exists and is not terminating", targetNodeName))
					}

					return false, nil
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
					"StorageBasedRemediation/%s must be absent before the storage fault is injected", targetNodeName)

				By(fmt.Sprintf("Creating privileged injector pod on target node %q", targetNodeName))

				injectorPod, createErr := pod.NewBuilder(
					APIClient, injectorPodName, medik8sparams.OperatorNs, sbrparams.InjectorImage).
					DefineOnNode(targetNodeName).
					WithHostPid(true).
					WithPrivilegedFlag().
					CreateAndWaitUntilRunning(medik8sparams.DefaultTimeout)
				Expect(createErr).ToNot(HaveOccurred(),
					"Failed to create injector pod on node %q", targetNodeName)
				injectCephFSRejectBidirectional(injectorPod, targetNodeName)

				By(fmt.Sprintf("Waiting for node %q to acquire %s=True",
					targetNodeName, sbrparams.SBRStorageUnhealthyCondition))

				Eventually(func() error {
					cond := getNodeStorageCondition(targetNodeName, sbrparams.SBRStorageUnhealthyCondition)
					if cond == nil {
						return fmt.Errorf("node %s: condition %s not yet present",
							targetNodeName, sbrparams.SBRStorageUnhealthyCondition)
					}

					if cond.Status != corev1.ConditionTrue {
						return fmt.Errorf("node %s: %s=%s, want True",
							targetNodeName, sbrparams.SBRStorageUnhealthyCondition, cond.Status)
					}

					return nil
				}, sbrparams.StorageInjectionTimeout, sbrparams.StorageInjectionPollInterval).Should(Succeed(),
					"Node %q must reach %s=True after CephFS port injection",
					targetNodeName, sbrparams.SBRStorageUnhealthyCondition)

				By(fmt.Sprintf("Consistently asserting node %q is not cordoned, fenced, or remediated "+
					"while detectOnlyMode is Enabled", targetNodeName))

				Consistently(func() error {
					node, nodeErr := APIClient.CoreV1Interface.Nodes().Get(
						context.TODO(), targetNodeName, metav1.GetOptions{})
					if nodeErr != nil {
						return fmt.Errorf("failed to get node %s: %w", targetNodeName, nodeErr)
					}

					if node.Spec.Unschedulable {
						return fmt.Errorf(
							"node %s was cordoned — SBR controller must not cordon in detect-only mode",
							targetNodeName)
					}

					for _, cond := range node.Status.Conditions {
						if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
							return fmt.Errorf(
								"node %s is not Ready — unexpected reboot or fence occurred in detect-only mode",
								targetNodeName)
						}
					}

					// Nothing in this spec requests remediation, so a StorageBasedRemediation CR
					// means another actor (for example a leftover NHC) is remediating the node.
					crErr := sbrCRExists(targetNodeName)
					if crErr == nil {
						return fmt.Errorf(
							"StorageBasedRemediation/%s exists — no remediation may be requested in detect-only mode",
							targetNodeName)
					}

					if !k8serrors.IsNotFound(crErr) {
						return fmt.Errorf("failed to check StorageBasedRemediation/%s: %w", targetNodeName, crErr)
					}

					return nil
				}, sbrparams.DetectOnlySuppressionCheckDuration, sbrparams.DetectOnlySuppressionCheckInterval).
					Should(Succeed(),
						"Node %q must remain Ready and schedulable for %s while detectOnlyMode is Enabled",
						targetNodeName, sbrparams.DetectOnlySuppressionCheckDuration)

				// Delete the SBRC while detectOnlyMode is still Enabled: no agent pod is restarted on
				// the faulted node, and no agent can arm the watchdog after the fault.
				By("Removing CephFS REJECT rules")

				for _, rule := range cephFSFlushBidirectional {
					_, flushErr := injectorPod.ExecCommand(rule)
					Expect(flushErr).ToNot(HaveOccurred(), "iptables flush must succeed")
				}

				By("Verifying no residual CephFS iptables REJECT rules remain before deleting injector pod")
				verifyCephFSIPTablesClean(injectorPod)

				if _, delErr := injectorPod.Delete(); delErr != nil && !k8serrors.IsNotFound(delErr) {
					GinkgoWriter.Printf("Warning: delete injector pod %s: %v\n", injectorPodName, delErr)
				}

				By("Deleting the StorageBasedRemediationConfig")

				liveRef := detectOnlySBRC.DeepCopy()
				deleteErr := APIClient.Delete(context.TODO(), liveRef)
				Expect(deleteErr).ToNot(HaveOccurred(),
					"StorageBasedRemediationConfig %q must be deletable", sbrparams.SBRCDetectOnlyTestName)

				// Nil out so AfterAll does not attempt a second delete.
				detectOnlySBRC = nil

				By("Verifying agent DaemonSet is garbage-collected after StorageBasedRemediationConfig deletion")

				dsName := sbrparams.SBRAgentDaemonSetPrefix + sbrparams.SBRCDetectOnlyTestName

				Eventually(func() error {
					_, getErr := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
						context.TODO(), dsName, metav1.GetOptions{})

					if k8serrors.IsNotFound(getErr) {
						return nil
					}

					if getErr != nil {
						return fmt.Errorf("unexpected error checking DaemonSet %s: %w", dsName, getErr)
					}

					return fmt.Errorf("DaemonSet %q still exists after StorageBasedRemediationConfig deletion",
						dsName)
				}, sbrparams.SBRCDaemonSetGCTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Agent DaemonSet %q must be garbage-collected after StorageBasedRemediationConfig deletion",
					dsName)
			})
	})
