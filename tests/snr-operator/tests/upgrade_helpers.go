package tests

import (
	"context"
	"fmt"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrparams"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrutils"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func upgradeSNRC() *unstructured.Unstructured {
	return buildSNRCR("SelfNodeRemediationConfig", snrparams.SNRConfigName, nil)
}

func (hooks *snrUpgradeOperatorFBCTest) controllerAgentImage(ctx context.Context) (string, error) {
	controller := &appsv1.Deployment{}
	if err := APIClient.Get(ctx, client.ObjectKey{
		Name: snrparams.OperatorDeploymentName, Namespace: hooks.owned.Name,
	}, controller); err != nil {
		return "", err
	}

	for _, container := range controller.Spec.Template.Spec.Containers {
		if container.Name == snrparams.ManagerContainerName {
			for _, env := range container.Env {
				if env.Name == "SELF_NODE_REMEDIATION_IMAGE" && env.Value != "" {
					return env.Value, nil
				}
			}
		}
	}

	return "", fmt.Errorf("SNR controller must specify SELF_NODE_REMEDIATION_IMAGE")
}

func (hooks *snrUpgradeOperatorFBCTest) agentPodUIDs(ctx context.Context) map[types.UID]bool {
	pods := &corev1.PodList{}
	Expect(APIClient.List(ctx, pods, client.InNamespace(hooks.owned.Name),
		client.MatchingLabels{
			"app.kubernetes.io/name":      "self-node-remediation",
			"app.kubernetes.io/component": "agent",
		})).To(Succeed())
	uids := make(map[types.UID]bool, len(pods.Items))

	for _, pod := range pods.Items {
		uids[pod.UID] = true
	}

	return uids
}

func (hooks *snrUpgradeOperatorFBCTest) waitForAgents(
	ctx context.Context, expectedImage string, oldUIDs map[types.UID]bool, spec map[string]interface{},
) {
	Eventually(func() error {
		daemonSet := &appsv1.DaemonSet{}
		if err := APIClient.Get(ctx, client.ObjectKey{
			Name: snrparams.AgentDaemonSetName, Namespace: hooks.owned.Name,
		}, daemonSet); err != nil {
			return err
		}
		pods := &corev1.PodList{}
		if err := APIClient.List(ctx, pods, client.InNamespace(hooks.owned.Name),
			client.MatchingLabels(daemonSet.Spec.Selector.MatchLabels)); err != nil {
			return err
		}
		if err := snrutils.VerifyAgents(daemonSet, pods.Items, expectedImage, oldUIDs); err != nil {
			return err
		}
		if err := snrutils.VerifyAgentConfiguration(&daemonSet.Spec.Template.Spec, spec, hooks.owned.Token); err != nil {
			return err
		}

		for _, pod := range pods.Items {
			if err := snrutils.VerifyAgentConfiguration(&pod.Spec, spec, hooks.owned.Token); err != nil {
				return fmt.Errorf("agent %s: %w", pod.Name, err)
			}
		}

		return nil
	}, snrparams.DSPodRestartTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
		"SNR agents must observe the current config, finish rolling out, and run the expected image")
}
