package tests

import (
	"context"
	"fmt"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/olm"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func upgradeNHC() *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(nhcGVK)
	object.SetName(nhcparams.NHCUpgradeTestName)

	return object
}

func upgradeTemplate(namespace string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(snrtGVK)
	object.SetName(nhcparams.NHCUpgradeTemplateName)
	object.SetNamespace(namespace)

	return object
}

func waitForSNRTemplate(ctx context.Context, name string) error {
	return wait.PollUntilContextTimeout(
		ctx, nhcparams.DefaultPollInterval, medik8sparams.DefaultTimeout, true, func(context.Context) (bool, error) {
			object := upgradeTemplate(medik8sparams.OperatorNs)
			object.SetName(name)
			err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), object)

			return err == nil, client.IgnoreNotFound(err)
		})
}

func waitForSNRNodeAgents(ctx context.Context) error {
	return wait.PollUntilContextTimeout(
		ctx, nhcparams.DefaultPollInterval, medik8sparams.OperatorUpgradeTimeout, true,
		func(ctx context.Context) (bool, error) {
			daemonSet := &appsv1.DaemonSet{}
			if err := APIClient.Get(ctx, client.ObjectKey{
				Name: nhcparams.SNRDaemonSetName, Namespace: medik8sparams.OperatorNs,
			}, daemonSet); err != nil {
				return false, client.IgnoreNotFound(err)
			}

			desired := daemonSet.Status.DesiredNumberScheduled

			return desired > 0 && daemonSet.Status.NumberReady == desired &&
				daemonSet.Status.UpdatedNumberScheduled == desired, nil
		})
}

func waitForNHCUpgradeCSV(namespace, expectedVersion, expectedImage, phase string) *olm.ClusterServiceVersionBuilder {
	var found *olm.ClusterServiceVersionBuilder

	Eventually(func(assertion Gomega) {
		csv, err := helpers.FindSucceededCSV(APIClient, nhcparams.CSVNamePattern, namespace)
		assertion.Expect(err).NotTo(HaveOccurred())
		assertion.Expect(csv.Object.Spec.Version.String()).To(Equal(expectedVersion))

		controller, err := deployment.Pull(APIClient, nhcparams.OperatorDeploymentName, namespace)
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
				if container.Name == nhcparams.ManagerContainerName {
					managerFound = true

					assertion.Expect(container.Image).To(Equal(expectedImage))
				}
			}

			assertion.Expect(managerFound).To(BeTrue())
		}

		found = csv
	}, 15*time.Minute, nhcparams.DefaultPollInterval).Should(Succeed(), "%s NHC CSV did not become ready", phase)
	GinkgoWriter.Printf("%s NHC CSV: %s version=%s\n", phase, found.Object.Name, found.Object.Spec.Version.String())

	return found
}

func waitForUpgradeAPI(ctx context.Context, object *unstructured.Unstructured) error {
	return wait.PollUntilContextTimeout(
		ctx, nhcparams.DefaultPollInterval, medik8sparams.DefaultTimeout, true, func(ctx context.Context) (bool, error) {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(object.GroupVersionKind().GroupVersion().WithKind(object.GetKind() + "List"))

			err := APIClient.List(ctx, list, client.InNamespace(object.GetNamespace()))
			if nhcutils.MissingAPI(err) {
				return false, nil
			}

			return err == nil, err
		})
}

func waitForPauseResponse(ctx context.Context, uid types.UID, token, previousRV string) error {
	return wait.PollUntilContextTimeout(
		ctx, nhcparams.DefaultPollInterval, medik8sparams.DefaultTimeout, true, func(ctx context.Context) (bool, error) {
			object := upgradeNHC()
			if err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
				return false, err
			}

			if object.GetUID() != uid {
				return false, fmt.Errorf("NHC UID changed during reconciliation probe")
			}

			return nhcutils.PauseResponse(object, uid, token, previousRV), nil
		})
}

func changeUpgradePause(ctx context.Context, uid types.UID, token string) {
	var object *unstructured.Unstructured

	Eventually(func() error {
		object = upgradeNHC()
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			return err
		}

		if object.GetUID() != uid {
			return fmt.Errorf("NHC UID changed before reconciliation probe")
		}

		original := object.DeepCopy()
		if err := unstructured.SetNestedStringSlice(object.Object, []string{token}, "spec", "pauseRequests"); err != nil {
			return err
		}

		// OLM may rotate the validating-webhook certificate just after the
		// upgraded CSV first reports Succeeded. Retry through that temporary
		// TLS-unavailable window and through resource-version conflicts caused
		// by concurrent status reconciliation.
		return APIClient.Patch(ctx, object,
			client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
	}, medik8sparams.DefaultTimeout, nhcparams.DefaultPollInterval).Should(Succeed(),
		"NHC update did not succeed after the upgraded webhook became available")
	Expect(waitForPauseResponse(ctx, uid, token, object.GetResourceVersion())).To(Succeed())
}

func captureNHCConfiguration(ctx context.Context) (string, map[string]interface{}) {
	nhc := upgradeNHC()
	Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(nhc), nhc)).To(Succeed())
	spec, found, err := unstructured.NestedMap(nhc.Object, "spec")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())

	return string(nhc.GetUID()), spec
}
