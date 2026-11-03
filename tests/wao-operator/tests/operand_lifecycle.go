package tests

import (
	"context"
	"fmt"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/wao-operator/internal/waoparams"

	appsv1 "k8s.io/api/apps/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe(
	"WAO operand lifecycle",
	Ordered,
	Label(labels.OperatorWAO), func() {
		BeforeAll(func() {
			By("Verifying the WorkloadAvailabilityConfig singleton exists")

			config, err := helpers.GetWAOConfig(context.Background(), APIClient)
			Expect(err).ToNot(HaveOccurred(), "WAO is not installed on this cluster")

			By("Verifying every operand is disabled before the run")

			for _, operand := range medik8sparams.WAOAllComponents {
				enabled, enabledErr := helpers.WAOComponentEnabled(config, operand)
				Expect(enabledErr).ToNot(HaveOccurred(), "Failed to read the intent of operand %s", operand)
				Expect(enabled).To(BeFalse(),
					"Operand %s is enabled; the spec installs and removes every operand and needs them all "+
						"disabled first", operand)
				phase, phaseErr := helpers.WAOComponentPhase(config, operand)
				Expect(phaseErr).ToNot(HaveOccurred(), "Failed to read the phase of operand %s", operand)
				Expect(phase).To(Equal(medik8sparams.WAOPhaseDisabled),
					"Operand %s reports phase %q rather than Disabled; resources from an earlier run are "+
						"still on the cluster: %v", operand, phase, retainedResources(config, operand))
			}

			By("Verifying no medik8s operand workload is running in the operator namespace")

			running, runningErr := runningOperandWorkloads(context.Background())
			Expect(runningErr).ToNot(HaveOccurred(), "Failed to list workloads in %s", medik8sparams.OperatorNs)
			Expect(running).To(BeEmpty(),
				"medik8s workloads are already running in %s; the spec requires a cluster where WAO manages "+
					"nothing yet", medik8sparams.OperatorNs)
		})

		It("RHWA-2040: enables each operand, waits for it to start, then disables it",
			reportxml.ID("2040"),
			Label(
				labels.OperatorWAO,
				labels.TierSmoke,
				labels.ComponentController,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
			), func() {
				DeferCleanup(func() {
					By("DeferCleanup: disabling every operand")
					restoreAllOperandsDisabled()
				})

				for _, operand := range medik8sparams.WAOAllComponents {
					enableOperand(operand)
					disableOperand(operand)
				}
			})
	})

// enableOperand enables one operand and waits for WAO to report it running.
func enableOperand(operand string) {
	GinkgoHelper()

	By(fmt.Sprintf("Enabling operand %s", operand))
	Expect(helpers.EnableWAOComponents(context.Background(), APIClient, operand)).
		To(Succeed(), "Failed to enable operand %s", operand)

	By(fmt.Sprintf("Waiting for operand %s to report phase Available", operand))
	Eventually(func() (string, error) {
		return operandPhase(operand)
	}, waoparams.OperandAvailableTimeout, waoparams.DefaultPollInterval).
		Should(Equal(medik8sparams.WAOPhaseAvailable),
			"Operand %s did not reach phase Available", operand)

	By(fmt.Sprintf("Verifying the %s manager Deployments are ready", operand))

	for _, name := range waoparams.OperandDeployments[operand] {
		operandDeployment, pullErr := deployment.Pull(APIClient, name, medik8sparams.OperatorNs)
		Expect(pullErr).ToNot(HaveOccurred(),
			"Deployment %s was not installed for operand %s", name, operand)
		Expect(operandDeployment.IsReady(waoparams.OperandAvailableTimeout)).To(BeTrue(),
			"Deployment %s of operand %s is not ready", name, operand)
	}
}

// disableOperand disables one operand and waits for its manager to be removed. An operand whose
// agents, configuration or native resources outlive the manager reports DisableIncomplete rather
// than Disabled; both mean the manager is gone, which is what this spec checks.
func disableOperand(operand string) {
	GinkgoHelper()

	By(fmt.Sprintf("Disabling operand %s", operand))
	Expect(helpers.DisableWAOComponents(context.Background(), APIClient, operand)).
		To(Succeed(), "Failed to disable operand %s", operand)

	By(fmt.Sprintf("Waiting for operand %s to stop reporting a running manager", operand))
	Eventually(func() (string, error) {
		return operandPhase(operand)
	}, waoparams.OperandDisabledTimeout, waoparams.DefaultPollInterval).
		Should(BeElementOf(medik8sparams.WAOPhaseDisabled, medik8sparams.WAOPhaseDisableIncomplete),
			"Operand %s is still reporting a running manager", operand)

	By(fmt.Sprintf("Verifying the %s manager Deployments are gone", operand))

	for _, name := range waoparams.OperandDeployments[operand] {
		Eventually(func() (bool, error) {
			return deploymentGone(context.Background(), name)
		}, waoparams.OperandDisabledTimeout, waoparams.DefaultPollInterval).
			Should(BeTrue(), "Deployment %s of operand %s was not removed", name, operand)
	}

	config, err := helpers.GetWAOConfig(context.Background(), APIClient)
	Expect(err).ToNot(HaveOccurred(), "Failed to read the WAO configuration after disabling %s", operand)

	if retained := retainedResources(config, operand); len(retained) > 0 {
		GinkgoWriter.Printf("operand %s retained after disable: %v\n", operand, retained)
	}
}

// restoreAllOperandsDisabled returns the configuration to the state the spec found it in. It runs
// after a failure too, so it reports problems instead of asserting on them.
func restoreAllOperandsDisabled() {
	err := helpers.DisableWAOComponents(context.Background(), APIClient, medik8sparams.WAOAllComponents...)
	if err != nil {
		GinkgoWriter.Printf("DeferCleanup: failed to disable operands: %v\n", err)
	}
}

// operandPhase reads the observed phase of one operand from the WAO configuration.
func operandPhase(operand string) (string, error) {
	config, err := helpers.GetWAOConfig(context.Background(), APIClient)
	if err != nil {
		return "", err
	}

	return helpers.WAOComponentPhase(config, operand)
}

// retainedResources lists what a disable left behind, for use in a failure message.
func retainedResources(config *unstructured.Unstructured, operand string) []string {
	retained, err := helpers.WAOComponentRetainedResources(config, operand)
	if err != nil {
		return []string{fmt.Sprintf("<unreadable: %v>", err)}
	}

	return retained
}

// deploymentGone reports whether a Deployment is absent from the operator namespace.
func deploymentGone(ctx context.Context, name string) (bool, error) {
	err := APIClient.Get(ctx,
		client.ObjectKey{Namespace: medik8sparams.OperatorNs, Name: name}, &appsv1.Deployment{})
	if k8serrors.IsNotFound(err) {
		return true, nil
	}

	return false, err
}

// runningOperandWorkloads names every Deployment and DaemonSet in the operator namespace other
// than WAO's own manager. On a cluster where WAO manages nothing yet the list is empty.
func runningOperandWorkloads(ctx context.Context) ([]string, error) {
	var running []string

	deployments := &appsv1.DeploymentList{}
	if err := APIClient.List(ctx, deployments, client.InNamespace(medik8sparams.OperatorNs)); err != nil {
		return nil, fmt.Errorf("list Deployments: %w", err)
	}

	for idx := range deployments.Items {
		if deployments.Items[idx].Name != waoparams.WAODeploymentName {
			running = append(running, "Deployment/"+deployments.Items[idx].Name)
		}
	}

	daemonSets := &appsv1.DaemonSetList{}
	if err := APIClient.List(ctx, daemonSets, client.InNamespace(medik8sparams.OperatorNs)); err != nil {
		return nil, fmt.Errorf("list DaemonSets: %w", err)
	}

	for idx := range daemonSets.Items {
		running = append(running, "DaemonSet/"+daemonSets.Items[idx].Name)
	}
	sort.Strings(running)

	return running, nil
}
