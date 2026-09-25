package tests

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/olm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
)

// sbrcUpgradeGVK is the GroupVersionKind for the StorageBasedRemediationConfig CR owned by the
// standalone operator-upgrade scenario.
var sbrcUpgradeGVK = schema.GroupVersionKind{
	Group:   sbrparams.CRDGroup,
	Version: sbrparams.CRDVersion,
	Kind:    "StorageBasedRemediationConfig",
}

// upgradeSBRCAgentDaemonSetName is the fixed name of the agent DaemonSet the SBR controller
// creates for the probe StorageBasedRemediationConfig.
var upgradeSBRCAgentDaemonSetName = sbrparams.SBRAgentDaemonSetPrefix + sbrparams.SBRUpgradeConfigTestName

func upgradeSBRC() *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(sbrcUpgradeGVK)
	object.SetName(sbrparams.SBRUpgradeConfigTestName)
	object.SetNamespace(medik8sparams.OperatorNs)

	return object
}

// waitForUpgradeAPI polls until the given CRD's List API is served, tolerating a genuinely
// empty/not-yet-registered cluster but never hiding authorization or transport failures.
func waitForUpgradeAPI(ctx context.Context, object *unstructured.Unstructured) error {
	return wait.PollUntilContextTimeout(
		ctx, sbrparams.DefaultPollInterval, medik8sparams.DefaultTimeout, true, func(ctx context.Context) (bool, error) {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(object.GroupVersionKind().GroupVersion().WithKind(object.GetKind() + "List"))

			err := APIClient.List(ctx, list, client.InNamespace(object.GetNamespace()))
			if sbrutils.MissingAPI(err) {
				return false, nil
			}

			return err == nil, err
		})
}

// waitForSBRUpgradeCSV waits for the SBR CSV to report expectedVersion, and for every running
// controller pod to be running expectedImage. Checking the live pod image (not just the CSV)
// guarantees the old controller is fully gone before the caller proceeds.
func waitForSBRUpgradeCSV(namespace, expectedVersion, expectedImage, phase string) *olm.ClusterServiceVersionBuilder {
	var found *olm.ClusterServiceVersionBuilder

	Eventually(func(assertion Gomega) {
		csv, err := helpers.FindSucceededCSV(APIClient, sbrparams.CSVNamePattern, namespace)
		assertion.Expect(err).NotTo(HaveOccurred())
		assertion.Expect(csv.Object.Spec.Version.String()).To(Equal(expectedVersion))

		controller, err := deployment.Pull(APIClient, sbrparams.OperatorDeploymentName, namespace)
		assertion.Expect(err).NotTo(HaveOccurred())
		assertion.Expect(controller.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue())

		pods := &corev1.PodList{}
		assertion.Expect(APIClient.List(context.Background(), pods, client.InNamespace(namespace),
			client.MatchingLabels(controller.Object.Spec.Selector.MatchLabels))).To(Succeed())
		assertion.Expect(pods.Items).NotTo(BeEmpty())

		for _, pod := range pods.Items {
			// Even a terminating old controller must be gone before the probe.
			assertion.Expect(pod.DeletionTimestamp).To(BeNil())

			managerFound := false

			for _, container := range pod.Spec.Containers {
				if container.Name == sbrparams.ManagerContainerName {
					managerFound = true

					assertion.Expect(container.Image).To(Equal(expectedImage))
				}
			}

			assertion.Expect(managerFound).To(BeTrue())
		}

		found = csv
	}, 15*time.Minute, sbrparams.DefaultPollInterval).Should(Succeed(), "%s SBR CSV did not become ready", phase)
	GinkgoWriter.Printf("%s SBR CSV: %s version=%s\n", phase, found.Object.Name, found.Object.Spec.Version.String())

	return found
}

// captureSBRCConfiguration returns the UID and full spec of the probe StorageBasedRemediationConfig.
func captureSBRCConfiguration(ctx context.Context) (string, map[string]interface{}) {
	sbrc := upgradeSBRC()
	Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(sbrc), sbrc)).To(Succeed())
	spec, found, err := unstructured.NestedMap(sbrc.Object, "spec")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())

	return string(sbrc.GetUID()), spec
}

// waitForUpgradeAgentDaemonSetExists waits for the SBR controller to create the probe SBRC's
// agent DaemonSet, and returns its generation. The nodeSelector never matches a node, so
// DesiredNumberScheduled must stay 0 for the lifetime of this scenario.
func waitForUpgradeAgentDaemonSetExists(ctx context.Context) int64 {
	var generation int64

	Eventually(func() error {
		agentDS, err := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
			ctx, upgradeSBRCAgentDaemonSetName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("agent DaemonSet %s not found yet: %w", upgradeSBRCAgentDaemonSetName, err)
		}

		if agentDS.Status.DesiredNumberScheduled != 0 {
			return fmt.Errorf("agent DaemonSet %s unexpectedly scheduled %d pod(s); "+
				"the probe nodeSelector must never match a node",
				upgradeSBRCAgentDaemonSetName, agentDS.Status.DesiredNumberScheduled)
		}

		generation = agentDS.Generation

		return nil
	}, sbrparams.SBRCReadyTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"the SBR controller must create an agent DaemonSet for %s (with zero pods scheduled) "+
			"before the standalone upgrade can be verified", sbrparams.SBRUpgradeConfigTestName)

	return generation
}

// patchSBRCMaxConsecutiveFailures patches the probe SBRC's maxConsecutiveFailures field and
// waits for its agent DaemonSet to observe a fresh generation, proving that the *current*
// (post-upgrade) controller is actively reconciling this object rather than a stale one.
// DesiredNumberScheduled is required to remain 0 throughout: the nodeSelector never matches
// any node, so no agent pod is ever created and no watchdog/storage action is ever taken.
func patchSBRCMaxConsecutiveFailures(ctx context.Context, uid types.UID, baselineGeneration, value int64) int64 {
	Eventually(func() error {
		sbrc := upgradeSBRC()
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(sbrc), sbrc); err != nil {
			return err
		}

		if sbrc.GetUID() != uid {
			return fmt.Errorf("StorageBasedRemediationConfig UID changed during the reconciliation probe")
		}

		original := sbrc.DeepCopy()
		if err := unstructured.SetNestedField(sbrc.Object, value, "spec", "maxConsecutiveFailures"); err != nil {
			return err
		}

		return APIClient.Patch(ctx, sbrc,
			client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
	}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"StorageBasedRemediationConfig update did not succeed after the upgraded controller became available")

	var newGeneration int64

	Eventually(func() error {
		agentDS, err := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
			ctx, upgradeSBRCAgentDaemonSetName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("agent DaemonSet %s not found: %w", upgradeSBRCAgentDaemonSetName, err)
		}

		if agentDS.Generation <= baselineGeneration {
			return fmt.Errorf("agent DaemonSet %s generation not advanced (current: %d, baseline: %d)",
				upgradeSBRCAgentDaemonSetName, agentDS.Generation, baselineGeneration)
		}

		if agentDS.Status.ObservedGeneration < agentDS.Generation {
			return fmt.Errorf("agent DaemonSet %s not yet reconciled (observed: %d, current: %d)",
				upgradeSBRCAgentDaemonSetName, agentDS.Status.ObservedGeneration, agentDS.Generation)
		}

		if agentDS.Status.DesiredNumberScheduled != 0 {
			return fmt.Errorf("agent DaemonSet %s unexpectedly scheduled %d pod(s); "+
				"the probe nodeSelector must never match a node",
				upgradeSBRCAgentDaemonSetName, agentDS.Status.DesiredNumberScheduled)
		}

		newGeneration = agentDS.Generation

		return nil
	}, sbrparams.SBRCLifecyclePatchedTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"agent DaemonSet %s must reconcile a fresh generation after the probe patch", upgradeSBRCAgentDaemonSetName)

	return newGeneration
}

// upgradeRunSBRRemediationCycle drives one full SBR remediation cycle:
// select a target worker node, record its boot ID, create a real SBRC matching the node,
// wait for the agent pod to be ready, create an SBR CR, and verify the node reboots.
func upgradeRunSBRRemediationCycle(ctx context.Context, owned *sbrutils.OwnedRun) error {
	nodeName := pickTargetWorkerNode()
	if nodeName == "" {
		return fmt.Errorf("failed to select target worker node")
	}

	GinkgoWriter.Printf("Target node for SBR remediation cycle: %s\n", nodeName)

	bootID, err := getNodeBootID(nodeName)
	if err != nil {
		return fmt.Errorf("failed to get boot ID for %s: %w", nodeName, err)
	}

	storageClass := discoverRWXStorageClass()
	Expect(storageClass).ToNot(BeEmpty(), "RWX storage class is required for SBR remediation")

	By(fmt.Sprintf("Creating a real StorageBasedRemediationConfig for node %s", nodeName))

	// sbr-upgrade-remediation-<token> is owned by this run and removed during Cleanup.
	sbrcName := "sbr-upgrade-remediation-" + owned.Token
	sbrc := buildSBRC(sbrcName, map[string]interface{}{
		"sharedStorageClass": storageClass,
		"nodeSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{
				"kubernetes.io/hostname": nodeName,
			},
		},
	})

	if createErr := owned.Create(ctx, sbrc); createErr != nil {
		return fmt.Errorf("failed to create SBRC %s: %w", sbrcName, createErr)
	}

	waitForSBRCReady(sbrcName)

	By(fmt.Sprintf("Triggering SBR remediation by creating SBR CR for node %s", nodeName))

	sbr := buildSBR(nodeName)
	if createErr := owned.Create(ctx, sbr); createErr != nil {
		return fmt.Errorf("failed to create SBR CR for %s: %w", nodeName, createErr)
	}

	By(fmt.Sprintf("Waiting for node %s to reboot (boot ID change)", nodeName))

	Eventually(func() (string, error) {
		return getNodeBootID(nodeName)
	}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).ShouldNot(Equal(bootID),
		"Node %s must reboot after SBR CR creation", nodeName)

	By(fmt.Sprintf("Waiting for node %s to return to Ready", nodeName))

	if waitErr := helpers.WaitForNodeReady(ctx, APIClient, nodeName,
		sbrparams.NodeRebootPollInterval, sbrparams.NodeRebootTimeout, GinkgoWriter.Printf); waitErr != nil {
		return fmt.Errorf("node %s did not return to Ready: %w", nodeName, waitErr)
	}

	GinkgoWriter.Printf("SBR remediation cycle completed for node %s\n", nodeName)

	return nil
}
