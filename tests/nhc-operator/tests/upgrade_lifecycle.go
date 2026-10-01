//nolint:wsl_v5 // Upgrade steps are grouped by lifecycle phase.
package tests

import (
	"context"
	"fmt"
	"strings"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// nhcUpgradeOperatorFBCTest exercises persistent configuration and remediation
// around an FBC upgrade.
type nhcUpgradeOperatorFBCTest struct {
	owned             *nhcutils.OwnedRun
	namespace         string
	token             string
	prepareSNR        func(context.Context) error
	configUID         string
	configSpec        map[string]interface{}
	currentTargetNode string
}

func (hooks *nhcUpgradeOperatorFBCTest) Setup(ctx context.Context) error {
	By("rejecting leftover resources owned by this standalone scenario")
	if err := nhcutils.CheckClean(ctx, APIClient, hooks.namespace); err != nil {
		return err
	}

	clusterVersion := &configv1.ClusterVersion{}
	if err := APIClient.Get(ctx, client.ObjectKey{Name: "version"}, clusterVersion); err != nil {
		return err
	}
	if !strings.HasPrefix(clusterVersion.Status.Desired.Version, "5.0.") {
		return fmt.Errorf("NHC FBC upgrade requires OpenShift 5.0, got %s",
			clusterVersion.Status.Desired.Version)
	}

	return hooks.owned.CreateNamespace(ctx)
}

func (hooks *nhcUpgradeOperatorFBCTest) FailureEvidence(ctx context.Context) interface{} {
	return nhcutils.CollectFailureEvidence(ctx, hooks.namespace)
}

func (hooks *nhcUpgradeOperatorFBCTest) BeforeUpgrade(ctx context.Context) error {
	if hooks.prepareSNR != nil {
		if err := hooks.prepareSNR(ctx); err != nil {
			return fmt.Errorf("prepare SNR prerequisite: %w", err)
		}
	}

	By("creating the SNR template used by the upgrade lifecycle")

	if err := waitForUpgradeAPI(ctx, upgradeTemplate(hooks.namespace)); err != nil {
		return fmt.Errorf("wait for SNR template API: %w", err)
	}

	if err := hooks.owned.Create(ctx, buildSNRT(nhcparams.NHCUpgradeTemplateName)); err != nil {
		return fmt.Errorf("create SNR template: %w", err)
	}

	if err := waitForSNRTemplate(ctx, nhcparams.NHCUpgradeTemplateName); err != nil {
		return fmt.Errorf("wait for SNR template: %w", err)
	}

	if err := waitForSNRNodeAgents(ctx); err != nil {
		return fmt.Errorf("wait for SNR node agents: %w", err)
	}

	By("creating a safe, observable NodeHealthCheck configuration")

	if err := waitForUpgradeAPI(ctx, upgradeNHC()); err != nil {
		return fmt.Errorf("wait for NHC API: %w", err)
	}

	nhc := upgradeNHC()
	nhc.Object["spec"] = nhcutils.SafeSpec(nhcparams.NHCUpgradeTemplateName, hooks.namespace, hooks.token)

	if err := hooks.owned.Create(ctx, nhc); err != nil {
		return fmt.Errorf("create NodeHealthCheck: %w", err)
	}

	if err := waitForPauseResponse(ctx, nhc.GetUID(), hooks.token, nhc.GetResourceVersion()); err != nil {
		return fmt.Errorf("wait for baseline NHC reconciliation: %w", err)
	}

	hooks.configUID, hooks.configSpec = captureNHCConfiguration(ctx)
	AddReportEntry("nhc-config-before-operator-upgrade", map[string]interface{}{
		"uid": hooks.configUID, "spec": hooks.configSpec,
	})
	GinkgoWriter.Printf("NHC config before operator upgrade: uid=%s spec=%v\n",
		hooks.configUID, hooks.configSpec)

	By("requiring a real remediation from the released operator")

	targetNode, err := upgradeRunRemediationCycle(
		ctx, "pre-operator-upgrade", nhcparams.NHCUpgradeTemplateName)
	if err != nil {
		hooks.currentTargetNode = targetNode

		return err
	}
	hooks.currentTargetNode = targetNode

	AddReportEntry("nhc-baseline-remediation-node", hooks.currentTargetNode)
	cleanupPostRemediationNHC(ctx, &hooks.currentTargetNode, "pre-operator-upgrade")

	return nil
}

func (hooks *nhcUpgradeOperatorFBCTest) AfterUpgrade(ctx context.Context) error {
	By("verifying the same configuration identity, specification, and reconciliation")

	uid, spec := captureNHCConfiguration(ctx)
	if uid != hooks.configUID {
		return fmt.Errorf("NodeHealthCheck UID changed from %s to %s", hooks.configUID, uid)
	}

	Expect(spec).To(Equal(hooks.configSpec), "upgrade must preserve the NodeHealthCheck specification")

	By("requiring a fresh candidate-controller response to a unique pause request")
	changeUpgradePause(ctx, types.UID(hooks.configUID), hooks.token+"-candidate")

	By("restoring the original configuration and requiring another controller response")
	changeUpgradePause(ctx, types.UID(hooks.configUID), hooks.token)
	uid, spec = captureNHCConfiguration(ctx)
	Expect(uid).To(Equal(hooks.configUID))
	Expect(spec).To(Equal(hooks.configSpec))
	AddReportEntry("nhc-config-after-operator-upgrade", map[string]interface{}{
		"uid": uid, "spec": spec,
	})
	GinkgoWriter.Printf("NHC config after operator upgrade: uid=%s spec=%v\n", uid, spec)

	By("requiring a real remediation from the upgraded candidate")

	targetNode, err := upgradeRunRemediationCycle(
		ctx, "post-operator-upgrade", nhcparams.NHCUpgradeTemplateName)
	hooks.currentTargetNode = targetNode
	if err != nil {
		return err
	}

	AddReportEntry("nhc-upgrade-remediation-node", hooks.currentTargetNode)
	cleanupPostRemediationNHC(ctx, &hooks.currentTargetNode, "post-operator-upgrade")

	return nil
}

func (hooks *nhcUpgradeOperatorFBCTest) Cleanup(ctx context.Context) {
	hooks.Recover(ctx)

	if hooks.owned == nil {
		return
	}

	if err := hooks.owned.Cleanup(ctx); err != nil {
		AddReportEntry("nhc-upgrade-cleanup-failure", err.Error())
		AddReportEntry("nhc-upgrade-cleanup-evidence",
			nhcutils.CollectFailureEvidence(ctx, hooks.namespace))
	}
}

// Recover restores any node interrupted by a failed remediation without
// removing the remaining test resources.
func (hooks *nhcUpgradeOperatorFBCTest) Recover(ctx context.Context) {
	cleanupNHCCR(ctx, nhcparams.ClusterUpgradeTestName)

	if hooks.currentTargetNode == "" {
		return
	}

	nodeName := hooks.currentTargetNode
	hooks.currentTargetNode = ""
	cleanupSNRCR(ctx, nodeName)

	if isSSHAvailable() {
		if err := startKubeletForRemediation(ctx, nodeName); err != nil {
			GinkgoWriter.Printf("WARNING: SSH kubelet restart failed for %s: %v\n", nodeName, err)
			AddReportEntry("ssh-kubelet-restart-failed", fmt.Sprintf("node %s: %v", nodeName, err))
		}
	}

	if err := helpers.WaitForNodeReady(ctx, APIClient, nodeName,
		nhcparams.DefaultPollInterval, nhcparams.NodeReadyTimeout, GinkgoWriter.Printf); err != nil {
		GinkgoWriter.Printf("WARNING: node %s did not recover: %v\n", nodeName, err)
		AddReportEntry("upgrade-recovery-failed", fmt.Sprintf("node %s: %v", nodeName, err))
	}

	if medik8sparams.KubeletStopViaOCDebug {
		if err := helpers.RemoveKubeletStopGuard(
			ctx, nodeName, nhcparams.OCDebugKubeletStopTimeout); err != nil {
			GinkgoWriter.Printf("WARNING: failed to remove kubelet-stop guard on %s: %v\n", nodeName, err)
		}
	}
}
