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

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// buildNHCNodeHang returns an unstructured NodeHealthCheck CR configured to trigger SBR-based
// remediation when a worker node is NotReady for more than 60 seconds.
// Uses the standard SBRTemplate already deployed by the operator.
func buildNHCNodeHang(name string) *unstructured.Unstructured {
	nhc := &unstructured.Unstructured{}
	nhc.SetAPIVersion(sbrparams.NHCAPIGroup + "/" + sbrparams.NHCAPIVersion)
	nhc.SetKind("NodeHealthCheck")
	nhc.SetName(name)

	// NOTE: this selector matches ALL worker nodes. If another worker goes NotReady
	// concurrently during the test, NHC may trigger unintended remediation on it.
	// Narrowing to a per-node label would require labeling the target in BeforeAll
	// and is deferred to a follow-up.
	_ = unstructured.SetNestedField(nhc.Object, map[string]interface{}{
		// NHC requires exactly one of minHealthy/maxUnhealthy; omitting both is rejected
		// by the validating webhook ("one of minHealthy and maxUnhealthy should be
		// specified"). Since NHC v0.10.0 (maxUnhealthy support, PR #372) the previous 51%
		// default on minHealthy was removed to make the fields mutually exclusive, so it
		// must now be set explicitly. 51% matches the former default and the sample CRs.
		"minHealthy": "51%",
		"selector": map[string]interface{}{
			"matchExpressions": []interface{}{
				map[string]interface{}{
					"key":      "node-role.kubernetes.io/worker",
					"operator": "Exists",
				},
			},
		},
		"remediationTemplate": map[string]interface{}{
			"apiVersion": sbrparams.CRDGroup + "/" + sbrparams.CRDVersion,
			"kind":       "StorageBasedRemediationTemplate",
			"name":       sbrparams.SBRTemplateName,
			"namespace":  medik8sparams.OperatorNs,
		},
		"unhealthyConditions": []interface{}{
			map[string]interface{}{
				"type":     "Ready",
				"status":   "False",
				"duration": "60s",
			},
			map[string]interface{}{
				"type":     "Ready",
				"status":   "Unknown",
				"duration": "60s",
			},
		},
	}, "spec")

	return nhc
}

var _ = Describe(
	"SBR Functional - Node Failure: Hard Node Hang / Kernel Freeze",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR), func() {
		var (
			targetNodeName  string
			setupSBRC       *unstructured.Unstructured
			nhcCreated      bool
			sysrqAvailable  bool
			rwxStorageClass string
		)

		BeforeAll(func() {
			By("Checking NHC CRD is installed")

			if !isNHCCRDInstalled() {
				Skip("NodeHealthCheck CRD not found; NHC operator not installed - skipping node hang test")
			}

			By("Discovering RWX storage class")

			rwxStorageClass = discoverRWXStorageClass()
			Expect(rwxStorageClass).ToNot(BeEmpty(),
				"No RWX storage class found; set SBR_STORAGE_CLASS env or deploy ODF/CephFS before running")
			GinkgoWriter.Printf("Using storage class: %s\n", rwxStorageClass)

			By("Creating SBRC with shared storage class")

			setupSBRC = buildSBRC(sbrparams.SBRCNodeHangTestName, map[string]interface{}{
				"sharedStorageClass": rwxStorageClass,
			})
			createErr := APIClient.Create(context.TODO(), setupSBRC)
			Expect(createErr).ToNot(HaveOccurred(),
				"StorageBasedRemediationConfig %q must be created", sbrparams.SBRCNodeHangTestName)
			waitForSBRCReady(sbrparams.SBRCNodeHangTestName)

			By("Creating NodeHealthCheck CR")

			ensureSBRTemplate()
			nhc := buildNHCNodeHang(sbrparams.NHCNodeHangTestName)
			nhcErr := APIClient.Create(context.TODO(), nhc)
			if nhcErr != nil && !k8serrors.IsAlreadyExists(nhcErr) {
				Expect(nhcErr).ToNot(HaveOccurred(),
					"NodeHealthCheck CR %q must be created", sbrparams.NHCNodeHangTestName)
			}

			nhcCreated = nhcErr == nil

			By("Selecting target worker node (schedulable, not controller pod host)")

			controllerNodes := controllerPodNodes()
			nodeList, err := APIClient.CoreV1Interface.Nodes().List(context.TODO(), metav1.ListOptions{
				LabelSelector: "node-role.kubernetes.io/worker",
			})
			Expect(err).ToNot(HaveOccurred(), "Failed to list worker nodes")

			for idx := range nodeList.Items {
				node := &nodeList.Items[idx]
				if controllerNodes[node.Name] {
					GinkgoWriter.Printf("Skipping controller node: %s\n", node.Name)

					continue
				}

				if isNodeSchedulable(node) {
					targetNodeName = node.Name

					break
				}
			}

			if targetNodeName == "" {
				Skip("No schedulable worker node available (excluding controller nodes); skipping node hang test")
			}

			GinkgoWriter.Printf("Target node: %s\n", targetNodeName)

			By("Probing sysrq availability on target node")

			safeName := strings.Map(func(r rune) rune {
				if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
					return r
				}

				return '-'
			}, strings.ToLower(targetNodeName))

			probePodName := "sbr-sysrq-probe-" + safeName
			if len(probePodName) > 63 {
				probePodName = probePodName[:63]
			}

			probePodName = strings.TrimRight(probePodName, "-")
			probePod, probeErr := pod.NewBuilder(
				APIClient, probePodName, medik8sparams.OperatorNs, sbrparams.InjectorImage).
				DefineOnNode(targetNodeName).
				WithHostPid(true).
				WithPrivilegedFlag().
				CreateAndWaitUntilRunning(medik8sparams.DefaultTimeout)
			if probeErr != nil {
				GinkgoWriter.Printf("Warning: sysrq probe pod failed: %v - assuming sysrq unavailable\n", probeErr)
			} else {
				DeferCleanup(func(name string) {
					if existing, pullErr := pod.Pull(APIClient, name, medik8sparams.OperatorNs); pullErr == nil {
						if _, delErr := existing.Delete(); delErr != nil {
							GinkgoWriter.Printf("DeferCleanup: failed to delete sysrq probe pod %s: %v\n",
								name, delErr)
						}
					}
				}, probePodName)

				buf, execErr := probePod.ExecCommand(
					[]string{
						"nsenter", "-t", "1", "--pid", "--mount", "--",
						"cat", "/proc/sys/kernel/sysrq",
					})

				if _, delErr := probePod.Delete(); delErr != nil {
					GinkgoWriter.Printf("Warning: failed to delete sysrq probe pod: %v\n", delErr)
				}

				if execErr == nil {
					val := strings.TrimSpace(buf.String())
					sysrqAvailable = val != "0"
					GinkgoWriter.Printf("sysrq on node %s: %q (available=%v)\n",
						targetNodeName, val, sysrqAvailable)
				} else {
					GinkgoWriter.Printf("Warning: sysrq probe exec failed: %v - assuming unavailable\n", execErr)
				}
			}
		})

		AfterAll(func() {
			if nhcCreated {
				By("Deleting NodeHealthCheck CR")
				cleanupNHCCR(context.Background(), sbrparams.NHCNodeHangTestName)
			}

			if setupSBRC != nil {
				By("Deleting SBRC created for node hang test")

				Eventually(func() error {
					deleteErr := APIClient.Delete(context.TODO(), setupSBRC)
					if deleteErr == nil || k8serrors.IsNotFound(deleteErr) {
						return nil
					}

					return deleteErr
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Failed to delete StorageBasedRemediationConfig %s", sbrparams.SBRCNodeHangTestName)

				Eventually(func() error {
					getErr := APIClient.Get(context.TODO(),
						types.NamespacedName{
							Name:      sbrparams.SBRCNodeHangTestName,
							Namespace: medik8sparams.OperatorNs,
						},
						setupSBRC.DeepCopy())

					if k8serrors.IsNotFound(getErr) {
						return nil
					}

					if getErr != nil {
						return getErr
					}

					return fmt.Errorf("SBRC %s still present", sbrparams.SBRCNodeHangTestName)
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed())
			}

			if targetNodeName != "" {
				By("Force-deleting any leftover StorageBasedRemediation CR for target node")
				cleanupSBRCR(context.Background(), targetNodeName)

				By("AfterAll: removing the injector image from the rebooted target node")
				purgeInjectorImageAfterReboot(targetNodeName)
			}
		})

		It("Node hang: kernel panic triggers reboot, NHC fences, node recovers",
			reportxml.ID("88738"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.FrequencyNightly,
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.ComponentRemediation,
			), func() {
				By(fmt.Sprintf("Recording boot-id for node %s before crash trigger", targetNodeName))

				preCrashBootID, bootIDErr := getNodeBootID(targetNodeName)
				Expect(bootIDErr).ToNot(HaveOccurred(), "Must read pre-crash boot-id from node %s", targetNodeName)
				GinkgoWriter.Printf("Pre-crash boot-id: %s\n", preCrashBootID)

				By("Creating privileged injector pod on target node")

				injectorPodName := sbrparams.InjectorPodName + "-hang"

				DeferCleanup(func(name string) {
					if existing, pullErr := pod.Pull(APIClient, name, medik8sparams.OperatorNs); pullErr == nil {
						if !sysrqAvailable {
							// Best-effort: flush DROP-all rules injected via the fallback path.
							// Iptables rules persist in the host kernel after pod deletion.
							_, _ = existing.ExecCommand([]string{
								"nsenter", "-t", "1", "--net", "--mount", "--", "sh", "-c",
								"iptables -D INPUT -j DROP 2>/dev/null; iptables -D OUTPUT -j DROP 2>/dev/null || true",
							})
						}

						if _, delErr := existing.Delete(); delErr != nil {
							GinkgoWriter.Printf("DeferCleanup: failed to delete injector pod %s: %v\n",
								name, delErr)
						}
					}
				}, injectorPodName)

				injectorPod, createErr := pod.NewBuilder(
					APIClient, injectorPodName, medik8sparams.OperatorNs, sbrparams.InjectorImage).
					DefineOnNode(targetNodeName).
					WithHostPid(true).
					WithPrivilegedFlag().
					CreateAndWaitUntilRunning(medik8sparams.DefaultTimeout)
				Expect(createErr).ToNot(HaveOccurred(),
					"Privileged injector pod must start on node %s", targetNodeName)

				if sysrqAvailable {
					By("Triggering kernel panic via sysrq-trigger (primary path)")
					GinkgoWriter.Printf("Executing kernel panic on node %s via sysrq (reboot delayed %ds)\n",
						targetNodeName, sbrparams.NodeHangPanicRebootDelaySeconds)

					// Fire-and-forget: exec fails because the node panics.
					// sync + sysrq-s flush dirty pages first; an unflushed crash corrupts CRI-O
					// overlay links and breaks every later pod on this node.
					// kernel.panic delays the automatic reboot so the node stays NotReady long
					// enough for NHC (60s unhealthy duration) to create a remediation.
					_, _ = injectorPod.ExecCommand([]string{
						"nsenter", "-t", "1", "--pid", "--mount", "--", "sh", "-c",
						fmt.Sprintf("echo 1 > /proc/sys/kernel/sysrq && sync && echo s > /proc/sysrq-trigger && "+
							"sleep 2 && echo %d > /proc/sys/kernel/panic && echo c > /proc/sysrq-trigger",
							sbrparams.NodeHangPanicRebootDelaySeconds),
					})
				} else {
					By("Triggering node hang via iptables DROP-all (fallback - sysrq not available)")
					GinkgoWriter.Printf(
						"Note: sysrq unavailable on node %s; using iptables DROP-all fallback\n",
						targetNodeName)

					// Start a background self-clearing timer before the DROP rules so the node
					// recovers automatically if the watchdog does not fire within 5 minutes.
					// Uses --pid to enter the host PID namespace so the timer process is adopted
					// by init and survives container termination.
					_, _ = injectorPod.ExecCommand([]string{
						"nsenter", "-t", "1", "--pid", "--net", "--mount", "--", "sh", "-c",
						"(sleep 300 && iptables -D INPUT -j DROP && iptables -D OUTPUT -j DROP) & " +
							"iptables -I INPUT -j DROP && iptables -I OUTPUT -j DROP",
					})
				}

				By(fmt.Sprintf("Waiting for node %s to reboot (NotReady or boot ID change, timeout %s)",
					targetNodeName, sbrparams.NodeRebootTimeout))

				// NHC deletes the CR once the node is healthy again, so sample it during every
				// wait instead of only after the node is Ready.
				sbrObserver := &sbrCRObserver{nodeName: targetNodeName}

				Eventually(func() error {
					_ = sbrObserver.observe()

					return nodeRebootStarted(targetNodeName, preCrashBootID)
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %s must reboot after crash trigger", targetNodeName)

				GinkgoWriter.Printf("Node %s is NotReady or already rebooted\n", targetNodeName)

				By(fmt.Sprintf("Waiting for node %s to recover and return Ready (timeout %s)",
					targetNodeName, sbrparams.NodeRebootTimeout))

				Eventually(func() error {
					_ = sbrObserver.observe()
					node, nodeErr := APIClient.CoreV1Interface.Nodes().Get(
						context.TODO(), targetNodeName, metav1.GetOptions{})
					if nodeErr != nil {
						return fmt.Errorf("get node %s: %w", targetNodeName, nodeErr)
					}

					if isNodeSchedulable(node) {
						return nil
					}

					return fmt.Errorf("node %s not yet Ready/schedulable", targetNodeName)
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %s must return Ready after reboot", targetNodeName)

				GinkgoWriter.Printf("Node %s is Ready\n", targetNodeName)

				By("Verifying boot-id changed (confirming node actually rebooted)")

				postCrashBootID, postBootIDErr := getNodeBootID(targetNodeName)
				Expect(postBootIDErr).ToNot(HaveOccurred(),
					"Must read post-crash boot-id from node %s", targetNodeName)
				Expect(postCrashBootID).ToNot(Equal(preCrashBootID),
					"Boot-id must change after reboot: node %s boot-id stayed %q",
					targetNodeName, preCrashBootID)
				GinkgoWriter.Printf("Boot-id changed: %q -> %q\n", preCrashBootID, postCrashBootID)

				By("Waiting for NHC to have created a StorageBasedRemediation CR for the target node")

				Eventually(sbrObserver.crCreated, sbrparams.NHCSBRCRCreationTimeout,
					sbrparams.NHCSBRCRCreationPollInterval).Should(Succeed(),
					"NHC must create StorageBasedRemediation CR for node %s within %s",
					targetNodeName, sbrparams.NHCSBRCRCreationTimeout)
				GinkgoWriter.Printf("StorageBasedRemediation/%s observed\n", targetNodeName)

				By("Verifying FencingSucceeded=True was set on the StorageBasedRemediation CR")

				Eventually(sbrObserver.fencingSucceeded, sbrparams.SBRCRCleanupTimeout,
					sbrparams.DefaultPollInterval).Should(Succeed(),
					"StorageBasedRemediation/%s must reach %s=True",
					targetNodeName, sbrparams.FencingSucceededCondition)
				GinkgoWriter.Printf("FencingSucceeded=True on StorageBasedRemediation/%s\n", targetNodeName)

				By("Waiting for the StorageBasedRemediation CR to be cleaned up")

				Eventually(func() error {
					_, pullErr := pullSBRCR(targetNodeName)

					if k8serrors.IsNotFound(pullErr) {
						return nil
					}

					if pullErr != nil {
						return pullErr
					}

					return fmt.Errorf(
						"StorageBasedRemediation/%s still exists; waiting for cleanup", targetNodeName)
				}, sbrparams.SBRCRCleanupTimeout, sbrparams.SBRCRCleanupPollInterval).Should(Succeed(),
					"StorageBasedRemediation/%s must be deleted after fencing completes", targetNodeName)

				GinkgoWriter.Printf("StorageBasedRemediation/%s deleted - fencing complete\n", targetNodeName)
			})
	})
