package tests

import (
	"context"
	"fmt"
	"strings"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrparams"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type snrUpgradeOperatorFBCTest struct {
	inputs      medik8sparams.FBCUpgradeInputs
	owned       *helpers.FBCNamespace
	configUID   types.UID
	configSpec  map[string]interface{}
	remediation *unstructured.Unstructured
	targetNode  string
}

func (hooks *snrUpgradeOperatorFBCTest) Setup(ctx context.Context) error {
	By("rejecting pre-existing SNR installations and acquiring an owned namespace")
	if err := hooks.owned.CheckClean(ctx, "self-node-remediation", snrGVK, snrcGVK, snrtGVK); err != nil {
		return err
	}
	version := &configv1.ClusterVersion{}
	if err := APIClient.Get(ctx, client.ObjectKey{Name: "version"}, version); err != nil {
		return err
	}
	if !strings.HasPrefix(version.Status.Desired.Version, "5.0.") {
		return fmt.Errorf("SNR FBC upgrade requires OpenShift 5.0, got %s", version.Status.Desired.Version)
	}
	count, err := helpers.CountReadyWorkerNodes(ctx, APIClient)
	if err != nil {
		return err
	}
	if count < 2 {
		return fmt.Errorf("SNR upgrade remediation requires at least two Ready workers, got %d", count)
	}

	return hooks.owned.Create(ctx)
}

func (hooks *snrUpgradeOperatorFBCTest) BeforeUpgrade(ctx context.Context) error {
	By("customizing the GA-created default config and capturing its complete specification")
	object := upgradeSNRC()
	Eventually(func() error {
		return APIClient.Get(ctx, client.ObjectKeyFromObject(object), object)
	}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed())
	hooks.configUID = object.GetUID()
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		return fmt.Errorf("read default SNR config spec: %w", err)
	}
	if !found {
		return fmt.Errorf("default SNR config has no spec")
	}
	spec["peerUpdateInterval"] = "17m"
	spec["maxApiErrorThreshold"] = int64(5)
	hooks.patchConfiguration(ctx, spec)
	Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
	hooks.configSpec, _, err = unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		return err
	}
	Expect(helpers.VerifyConfigurationPreserved(ctx, APIClient, object, hooks.configUID, hooks.configSpec)).To(Succeed())
	baselineImage, err := hooks.controllerAgentImage(ctx)
	if err != nil {
		return err
	}
	hooks.waitForAgents(ctx, baselineImage, nil, hooks.configSpec)
	hooks.reportConfiguration(ctx, "before")

	// Exercise remediation with the candidate after upgrading.
	return nil
}

func (hooks *snrUpgradeOperatorFBCTest) AfterUpgrade(ctx context.Context) error {
	By("verifying the original SNR config UID and complete spec survived")
	hooks.reportConfiguration(ctx, "after")
	hooks.waitForAgents(ctx, hooks.inputs.CandidateImage, nil, hooks.configSpec)
	oldPods := hooks.agentPodUIDs(ctx)
	object := upgradeSNRC()
	Expect(unstructured.SetNestedMap(object.Object, hooks.configSpec, "spec")).To(Succeed())
	probe, _, err := unstructured.NestedMap(object.Object, "spec")
	Expect(err).NotTo(HaveOccurred())
	// A unique, harmless toleration changes the desired agent pod template.
	tolerations, _, err := unstructured.NestedSlice(probe, "customDsTolerations")
	Expect(err).NotTo(HaveOccurred())
	tolerations = append(tolerations, map[string]interface{}{
		"key": helpers.FBCRunLabel, "operator": "Equal", "value": hooks.owned.Token, "effect": "NoSchedule",
	})
	probe["customDsTolerations"] = tolerations

	By("requiring fresh candidate reconciliation and newly rolled agent pods")
	hooks.patchConfiguration(ctx, probe)
	hooks.waitForAgents(ctx, hooks.inputs.CandidateImage, oldPods, probe)
	AddReportEntry("snr-upgrade-fresh-reconciliation", map[string]interface{}{
		"configUID": hooks.configUID, "probeToleration": hooks.owned.Token, "allAgentPodsReplaced": true,
	})

	By("restoring and verifying the original config without replacing it")
	hooks.patchConfiguration(ctx, hooks.configSpec)
	hooks.waitForAgents(ctx, hooks.inputs.CandidateImage, nil, hooks.configSpec)
	hooks.reportConfiguration(ctx, "restored")

	By("requiring a real worker reboot and recovery from candidate SNR")

	return hooks.runRemediationCycle(ctx, "after", hooks.inputs.CandidateImage)
}

func (hooks *snrUpgradeOperatorFBCTest) reportConfiguration(ctx context.Context, phase string) {
	object := upgradeSNRC()
	Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
	Expect(helpers.VerifyConfigurationPreserved(ctx, APIClient, object, hooks.configUID, hooks.configSpec)).To(Succeed())
	AddReportEntry("snr-config-"+phase+"-operator-upgrade", map[string]interface{}{
		"uid": hooks.configUID, "spec": hooks.configSpec,
	})
	GinkgoWriter.Printf("SNR config %s operator upgrade: uid=%s spec=%v\n", phase, hooks.configUID, hooks.configSpec)
}

func (hooks *snrUpgradeOperatorFBCTest) patchConfiguration(ctx context.Context, spec map[string]interface{}) {
	Eventually(func() error {
		return hooks.writeConfiguration(ctx, spec)
	}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed())
}

func (hooks *snrUpgradeOperatorFBCTest) writeConfiguration(ctx context.Context, spec map[string]interface{}) error {
	if err := hooks.owned.VerifyOwner(ctx); err != nil {
		return err
	}
	object := upgradeSNRC()
	if err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		return err
	}
	if hooks.configUID == "" || object.GetUID() != hooks.configUID {
		return fmt.Errorf("refusing to patch a replaced/unowned SelfNodeRemediationConfig")
	}
	original := object.DeepCopy()
	if err := unstructured.SetNestedMap(object.Object, spec, "spec"); err != nil {
		return err
	}

	return APIClient.Patch(ctx, object,
		client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
}

func (hooks *snrUpgradeOperatorFBCTest) Cleanup(ctx context.Context) {
	hooks.Recover(ctx)
	if hooks.configSpec != nil {
		if err := hooks.writeConfiguration(ctx, hooks.configSpec); err != nil {
			AddReportEntry("snr-upgrade-config-restore-failure", err.Error())
		}
	}
	if err := hooks.owned.VerifyOwner(ctx); err != nil {
		AddReportEntry("snr-upgrade-cleanup-owner-check", err.Error())

		return
	}
	// Stop the config/agents before removing OLM-owned admission resources.
	object := upgradeSNRC()
	if err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), object); err == nil {
		uid := object.GetUID()
		if err := APIClient.Delete(ctx, object, client.Preconditions{UID: &uid}); err != nil {
			AddReportEntry("snr-upgrade-config-delete-failure", err.Error())
		}
	}
	if err := hooks.owned.Cleanup(ctx); err != nil {
		AddReportEntry("snr-upgrade-cleanup-failure", err.Error())
	}
}

func (hooks *snrUpgradeOperatorFBCTest) FailureEvidence(ctx context.Context) interface{} {
	evidence := map[string]interface{}{}
	object := upgradeSNRC()
	if err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		evidence["configError"] = err.Error()
	} else {
		evidence["config"] = object
	}
	pods := &corev1.PodList{}
	if err := APIClient.List(ctx, pods, client.InNamespace(hooks.owned.Name)); err != nil {
		evidence["podsError"] = err.Error()
	} else {
		evidence["pods"] = pods
	}
	evidence["remediation"] = hooks.remediation
	evidence["targetNode"] = hooks.targetNode

	return evidence
}
