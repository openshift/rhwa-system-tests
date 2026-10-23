package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrparams"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe(
	"SNR preferredAddressTypes tests",
	Ordered,
	ContinueOnFailure,
	Serial,
	Label(snrparams.Label), func() {
		var (
			// originalAddressTypes and originalAddressTypesFound capture the
			// SNRC field as found, so cleanup can restore "unset" as faithfully
			// as it restores a value.
			originalAddressTypes      []string
			originalAddressTypesFound bool
			// controlPlaneNodes is the sorted list of control plane nodes, and
			// controlPlaneInternalIPs maps each to its first InternalIP.
			controlPlaneNodes       []string
			controlPlaneInternalIPs map[string]string
			controlPlaneBootIDs     map[string]string
		)

		BeforeAll(func(ctx SpecContext) {
			By("Verifying the SNR deployment is ready")

			// deployment.IsReady aborts its whole poll on the first API read
			// error that is not a timeout, so a single transient failure fails
			// the suite outright. Retry around it rather than trusting one call.
			Eventually(func() error {
				snrDeployment, pullErr := deployment.Pull(
					APIClient, snrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
				if pullErr != nil {
					return fmt.Errorf("failed to get SNR deployment: %w", pullErr)
				}

				if !snrDeployment.IsReady(snrparams.DefaultPollInterval) {
					return fmt.Errorf("SNR deployment %q is not Ready",
						snrparams.OperatorDeploymentName)
				}

				return nil
			}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
				"SNR deployment %q must be Ready", snrparams.OperatorDeploymentName)

			By("Reading the default SelfNodeRemediationConfig and capturing preferredAddressTypes")

			snrc := &unstructured.Unstructured{}
			snrc.SetGroupVersionKind(snrcGVK)
			Expect(APIClient.Get(ctx, client.ObjectKey{
				Name: snrparams.SNRConfigName, Namespace: medik8sparams.OperatorNs,
			}, snrc)).To(Succeed(),
				"Default SNRC %q must exist", snrparams.SNRConfigName)
			captured, found, fieldErr := unstructured.NestedStringSlice(
				snrc.Object, "spec", "preferredAddressTypes")
			Expect(fieldErr).ToNot(HaveOccurred(),
				"spec.preferredAddressTypes on SNRC %q must be a string slice",
				snrparams.SNRConfigName)
			originalAddressTypes, originalAddressTypesFound = captured, found
			GinkgoWriter.Printf("Captured original preferredAddressTypes: found=%t value=%v\n",
				originalAddressTypesFound, originalAddressTypes)

			By("Verifying every SNR agent pod is Running and Ready")

			Eventually(verifyDSPodsRunning,
				snrparams.DSPodRestartTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
				"SNR DaemonSet pods must all be Running before the configuration changes")

			By("Collecting control plane nodes and their InternalIP addresses")

			nodeList, listErr := listMasterNodes(ctx, APIClient)
			Expect(listErr).ToNot(HaveOccurred(), "Failed to list control plane nodes")
			Expect(nodeList.Items).ToNot(BeEmpty(), "Cluster must have at least one control plane node")
			controlPlaneInternalIPs = make(map[string]string, len(nodeList.Items))
			controlPlaneNodes = make([]string, 0, len(nodeList.Items))

			for nodeIdx := range nodeList.Items {
				node := &nodeList.Items[nodeIdx]
				controlPlaneNodes = append(controlPlaneNodes, node.Name)
				internalIP := firstNodeAddressOfType(node, corev1.NodeInternalIP)

				if internalIP == "" {
					Skip(fmt.Sprintf(
						"Control plane node %q has no InternalIP in status.addresses; "+
							"preferredAddressTypes cannot be exercised with InternalIP on this cluster",
						node.Name))
				}

				controlPlaneInternalIPs[node.Name] = internalIP
			}

			By("Verifying no NodeHealthCheck selects control plane nodes")

			selectingNHCs, nhcErr := nhcNamesSelectingNodes(ctx, nodeList.Items)
			Expect(nhcErr).ToNot(HaveOccurred(), "Failed to evaluate NodeHealthCheck selectors")

			if len(selectingNHCs) > 0 {
				Skip(fmt.Sprintf(
					"NodeHealthCheck(s) %v select control plane nodes and could create a "+
						"SelfNodeRemediation during the observation window; skipping rather than "+
						"reporting a false failure", selectingNHCs))
			}

			By("Verifying no SelfNodeRemediation CR exists for any control plane node")

			staleCRs, snrErr := snrCRsForNodes(ctx, controlPlaneNodes)
			Expect(snrErr).ToNot(HaveOccurred(), "Failed to list SelfNodeRemediation CRs")
			Expect(staleCRs).To(BeEmpty(),
				"No SelfNodeRemediation CR may exist for a control plane node before the test starts")

			By("Recording the boot ID of every control plane node")

			controlPlaneBootIDs = make(map[string]string, len(controlPlaneNodes))

			for _, nodeName := range controlPlaneNodes {
				bootID, bootErr := helpers.GetNodeBootIDFromAPI(ctx, APIClient, nodeName)
				Expect(bootErr).ToNot(HaveOccurred(),
					"Failed to read boot ID of control plane node %q", nodeName)
				controlPlaneBootIDs[nodeName] = bootID
			}
		})

		AfterAll(func(ctx SpecContext) {
			By("Re-asserting that every SNR agent pod is Running after the suite")

			// Bail out rather than block when the SNRC is gone: the DaemonSet
			// is owned by it, so with no SNRC there will never be agent pods
			// and this would poll for the full DSPodRestartTimeout before
			// failing. Something outside this spec removed it -- say so
			// plainly instead of burning 15 minutes to reach the same
			// conclusion.
			snrc := &unstructured.Unstructured{}
			snrc.SetGroupVersionKind(snrcGVK)

			if err := APIClient.Get(ctx, client.ObjectKey{
				Name: snrparams.SNRConfigName, Namespace: medik8sparams.OperatorNs,
			}, snrc); k8serrors.IsNotFound(err) {
				Fail(fmt.Sprintf(
					"SNRC %q does not exist, so the agent DaemonSet cannot exist either. "+
						"This spec never deletes it; something outside the spec did.",
					snrparams.SNRConfigName))
			}

			Eventually(verifyDSPodsRunning,
				snrparams.DSPodRestartTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
				"SNR DaemonSet pods must all be Running after the preferredAddressTypes test")
		})

		It("Verify preferredAddressTypes is validated, reaches the agent DaemonSet, and causes no remediation",
			reportxml.ID("1928"),
			Label(
				labels.OperatorSNR,
				labels.TierAcceptance,
				labels.ComponentDaemonSet,
				// Destructive in practice on current builds: rolling the agent
				// DaemonSet has driven control plane nodes into watchdog
				// self-remediation on two unrelated clusters (RHWA-1942).
				// Revert to DisruptionNonDestructive once that is fixed.
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.TopologyControlPlane,
				labels.FrequencyNightly,
			), func(ctx SpecContext) {
				DeferCleanup(func(cleanupCtx SpecContext) {
					By("DeferCleanup: restoring the original preferredAddressTypes")

					restorePreferredAddressTypes(
						cleanupCtx, originalAddressTypes, originalAddressTypesFound)
				})

				By("Verifying the agent DaemonSet renders an empty PREFERRED_ADDRESS_TYPES by default")

				renderedDefault, defaultFound := agentEnvVar(
					ctx, snrparams.PreferredAddressTypesEnvVar)
				Expect(defaultFound).To(BeTrue(),
					"Agent container must define %s even when preferredAddressTypes is unset",
					snrparams.PreferredAddressTypesEnvVar)

				if originalAddressTypesFound {
					// A pre-configured cluster cannot show the default. Assert the
					// rendering that does apply and say so, rather than skipping.
					GinkgoWriter.Printf(
						"SNRC already sets preferredAddressTypes=%v; asserting that rendering "+
							"instead of the unset default\n", originalAddressTypes)
					Expect(renderedDefault).To(Equal(strings.Join(originalAddressTypes, ",")),
						"%s must render the configured preferredAddressTypes",
						snrparams.PreferredAddressTypesEnvVar)
				} else {
					Expect(renderedDefault).To(BeEmpty(),
						"%s must be empty when preferredAddressTypes is unset, which the agent "+
							"treats as [\"NodeName\"]", snrparams.PreferredAddressTypesEnvVar)
				}

				By("Reading the preferredAddressTypes enum from the installed SelfNodeRemediationConfig CRD")

				Expect(preferredAddressTypesEnum(ctx)).To(Equal(snrparams.PreferredAddressTypesValid),
					"The installed CRD must offer exactly the documented preferredAddressTypes enum")

				By("Rejecting a SelfNodeRemediationConfig with an invalid preferredAddressTypes value")

				invalidErr := patchPreferredAddressTypes(
					ctx, []string{snrparams.PreferredAddressTypesInvalid}, client.DryRunAll)
				Expect(invalidErr).To(HaveOccurred(),
					"API server must reject preferredAddressTypes value %q",
					snrparams.PreferredAddressTypesInvalid)
				Expect(invalidErr.Error()).To(ContainSubstring("preferredAddressTypes"),
					"The rejection must name the offending field")

				By("Accepting every documented preferredAddressTypes value")

				for _, addressType := range snrparams.PreferredAddressTypesValid {
					Expect(patchPreferredAddressTypes(
						ctx, []string{addressType}, client.DryRunAll)).To(Succeed(),
						"API server must accept documented preferredAddressTypes value %q", addressType)
				}

				By("Verifying the kubelet answers on the InternalIP of each control plane node from the pod network")

				for probeIndex, nodeName := range controlPlaneNodes {
					internalIP := controlPlaneInternalIPs[nodeName]
					statusCode := probeKubeletFromPodNetwork(ctx, probeIndex, nodeName, internalIP)
					Expect(statusCode).To(MatchRegexp(`^[1-5][0-9]{2}$`),
						"Kubelet on node %q must answer on https://%s/pods from the pod network "+
							"(401 counts: it proves the port is served); curl reported %q",
						nodeName, net.JoinHostPort(internalIP, snrparams.KubeletSecurePort), statusCode)
				}

				By("Verifying the agent NetworkPolicy permits egress to the kubelet port")

				verifyAgentNetworkPolicyAllowsKubeletEgress(ctx)

				By("Capturing the current agent pod UIDs")

				prePatchUIDs := collectDSPodUIDs()
				Expect(prePatchUIDs).ToNot(BeEmpty(), "There must be agent pods to replace")

				By("Patching the SelfNodeRemediationConfig with preferredAddressTypes InternalIP and NodeName")

				Expect(patchPreferredAddressTypes(
					ctx, snrparams.PreferredAddressTypesConfigured)).To(Succeed(),
					"Failed to set preferredAddressTypes on SNRC %q", snrparams.SNRConfigName)
				Eventually(func() ([]string, error) {
					return currentPreferredAddressTypes(ctx)
				}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(
					Equal(snrparams.PreferredAddressTypesConfigured),
					"SNRC spec must reflect both values in the configured order")

				By("Waiting for every agent pod to be replaced after the configuration change")

				Eventually(func() error {
					return verifyDSPodsReplaced(prePatchUIDs)
				}, snrparams.DSPodRestartTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
					"SNR DaemonSet pods must be replaced after preferredAddressTypes changes")

				By("Verifying PREFERRED_ADDRESS_TYPES is rendered comma-separated in the configured order")

				expectedEnv := strings.Join(snrparams.PreferredAddressTypesConfigured, ",")
				renderedEnv, renderedFound := agentEnvVar(ctx, snrparams.PreferredAddressTypesEnvVar)
				Expect(renderedFound).To(BeTrue(),
					"Agent container must define %s", snrparams.PreferredAddressTypesEnvVar)
				Expect(renderedEnv).To(Equal(expectedEnv),
					"%s must preserve the configured order, since it is the fallback precedence",
					snrparams.PreferredAddressTypesEnvVar)
				Eventually(func() error {
					return verifyAgentPodsRenderEnvVar(
						ctx, snrparams.PreferredAddressTypesEnvVar, expectedEnv)
				}, snrparams.DSPodRestartTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
					"Every running agent pod must carry the new %s value",
					snrparams.PreferredAddressTypesEnvVar)

				By("Verifying every agent pod on a control plane node is Running and Ready")

				Eventually(func() error {
					return verifyControlPlaneAgentPods(ctx, controlPlaneNodes)
				}, snrparams.DSPodRestartTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
					"Every control plane node must have a Running, Ready SNR agent pod")

				By("Verifying no agent pod logs a kubelet service failure for the configured addresses")

				Expect(controlPlaneAgentPodsLoggingMessage(
					controlPlaneNodes, snrparams.KubeletDownMessage)).To(BeEmpty(),
					"No control plane agent pod may log %q after preferredAddressTypes is set",
					snrparams.KubeletDownMessage)

				By("Consistently verifying no control plane node is remediated or rebooted")

				observedOK := false
				Consistently(func() error {
					return noControlPlaneRemediation(
						ctx, controlPlaneNodes, controlPlaneBootIDs, &observedOK)
				}, snrparams.PreferredAddressNoRemediationDuration,
					snrparams.DefaultPollInterval).Should(Succeed(),
					"No control plane node may be remediated or rebooted after "+
						"preferredAddressTypes is set")
				Expect(observedOK).To(BeTrue(),
					"Every API read during the %s observation window failed, so the "+
						"no-remediation assertion proved nothing; treat this as inconclusive, "+
						"not a pass", snrparams.PreferredAddressNoRemediationDuration)
			})
	})

// firstNodeAddressOfType returns the first address of the given type in the
// node's status.addresses, or "" when the node has none.
func firstNodeAddressOfType(node *corev1.Node, addressType corev1.NodeAddressType) string {
	for _, address := range node.Status.Addresses {
		if address.Type == addressType {
			return address.Address
		}
	}

	return ""
}

// agentDaemonSetContainer returns the manager container of the SNR agent
// DaemonSet pod template.
func agentDaemonSetContainer(ctx context.Context) (*corev1.Container, error) {
	agentDS := &appsv1.DaemonSet{}
	if err := APIClient.Get(ctx, client.ObjectKey{
		Name: snrparams.AgentDaemonSetName, Namespace: medik8sparams.OperatorNs,
	}, agentDS); err != nil {
		return nil, fmt.Errorf("failed to get agent DaemonSet %q: %w",
			snrparams.AgentDaemonSetName, err)
	}

	for containerIdx := range agentDS.Spec.Template.Spec.Containers {
		container := &agentDS.Spec.Template.Spec.Containers[containerIdx]
		if container.Name == snrparams.ManagerContainerName {
			return container, nil
		}
	}

	return nil, fmt.Errorf("agent DaemonSet %q has no %q container",
		snrparams.AgentDaemonSetName, snrparams.ManagerContainerName)
}

// agentEnvVar returns the value of an environment variable on the agent
// DaemonSet's manager container, and whether the variable is defined at all.
// An empty value with found=true is meaningful: that is how an unset
// preferredAddressTypes renders.
func agentEnvVar(ctx context.Context, name string) (string, bool) {
	container, err := agentDaemonSetContainer(ctx)
	Expect(err).ToNot(HaveOccurred(), "Failed to read the SNR agent container definition")

	for _, env := range container.Env {
		if env.Name == name {
			return env.Value, true
		}
	}

	return "", false
}

// verifyAgentPodsRenderEnvVar checks that every running agent pod -- not just
// the DaemonSet template -- carries the expected environment variable value.
// The agent reads preferredAddressTypes once at startup, so the pods are what
// matters, and a pod still on the old value means the rollout is incomplete.
// It polls, so it reads through the high-QPS EventsClient -- see the note on
// noControlPlaneRemediation about APIClient's default rate limiter.
func verifyAgentPodsRenderEnvVar(ctx context.Context, name, expected string) error {
	agentPods, err := listAgentPods(ctx)
	if err != nil {
		return err
	}

	if len(agentPods.Items) == 0 {
		return fmt.Errorf("no SNR agent pods found")
	}

	for podIdx := range agentPods.Items {
		agentPod := &agentPods.Items[podIdx]
		found := false

		for _, container := range agentPod.Spec.Containers {
			if container.Name != snrparams.ManagerContainerName {
				continue
			}

			for _, env := range container.Env {
				if env.Name != name {
					continue
				}

				found = true

				if env.Value != expected {
					return fmt.Errorf("agent pod %q has %s=%q, expected %q",
						agentPod.Name, name, env.Value, expected)
				}
			}
		}

		if !found {
			return fmt.Errorf("agent pod %q does not define %s", agentPod.Name, name)
		}
	}

	return nil
}

// preferredAddressTypesEnum reads the enum the installed CRD accepts for
// spec.preferredAddressTypes, from the storage version's schema.
func preferredAddressTypesEnum(ctx context.Context) ([]string, error) {
	snrCRD := &apiextensionsv1.CustomResourceDefinition{}
	if err := APIClient.Get(ctx, client.ObjectKey{Name: snrparams.SNRCRDName}, snrCRD); err != nil {
		return nil, fmt.Errorf("failed to get CRD %q: %w", snrparams.SNRCRDName, err)
	}

	for _, version := range snrCRD.Spec.Versions {
		if !version.Storage || version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			continue
		}

		specProps, specFound := version.Schema.OpenAPIV3Schema.Properties["spec"]
		if !specFound {
			continue
		}

		field, fieldFound := specProps.Properties["preferredAddressTypes"]
		if !fieldFound {
			return nil, fmt.Errorf(
				"CRD %q storage version %q has no spec.preferredAddressTypes property; "+
					"the build under test predates self-node-remediation#331",
				snrparams.SNRCRDName, version.Name)
		}

		if field.Items == nil || field.Items.Schema == nil {
			return nil, fmt.Errorf("spec.preferredAddressTypes in CRD %q has no items schema",
				snrparams.SNRCRDName)
		}

		enum := make([]string, 0, len(field.Items.Schema.Enum))

		for _, raw := range field.Items.Schema.Enum {
			var value string
			if err := json.Unmarshal(raw.Raw, &value); err != nil {
				return nil, fmt.Errorf("failed to decode enum entry %s: %w", raw.Raw, err)
			}

			enum = append(enum, value)
		}

		return enum, nil
	}

	return nil, fmt.Errorf("CRD %q has no storage version with a schema", snrparams.SNRCRDName)
}

// patchPreferredAddressTypes merge-patches spec.preferredAddressTypes on the
// default SNRC. Pass client.DryRunAll to exercise API server validation
// without rolling every agent pod in the cluster.
func patchPreferredAddressTypes(
	ctx context.Context, addressTypes []string, opts ...client.PatchOption,
) error {
	encoded, marshalErr := json.Marshal(addressTypes)
	if marshalErr != nil {
		return fmt.Errorf("failed to marshal preferredAddressTypes %v: %w", addressTypes, marshalErr)
	}

	patch := fmt.Appendf(nil, `{"spec":{"preferredAddressTypes":%s}}`, encoded)

	return APIClient.Patch(ctx,
		snrcForPatch(),
		client.RawPatch(types.MergePatchType, patch),
		opts...)
}

// currentPreferredAddressTypes reads spec.preferredAddressTypes back from the
// live SNRC.
func currentPreferredAddressTypes(ctx context.Context) ([]string, error) {
	snrc := &unstructured.Unstructured{}
	snrc.SetGroupVersionKind(snrcGVK)

	if err := APIClient.Get(ctx, client.ObjectKey{
		Name: snrparams.SNRConfigName, Namespace: medik8sparams.OperatorNs,
	}, snrc); err != nil {
		return nil, fmt.Errorf("failed to get SNRC %q: %w", snrparams.SNRConfigName, err)
	}

	addressTypes, _, err := unstructured.NestedStringSlice(
		snrc.Object, "spec", "preferredAddressTypes")
	if err != nil {
		return nil, fmt.Errorf("failed to read spec.preferredAddressTypes: %w", err)
	}

	return addressTypes, nil
}

// restorePreferredAddressTypes puts the SNRC field back exactly as BeforeAll
// found it -- including removing it when it was unset -- and waits for the
// agent DaemonSet to settle. Tolerates the SNRC having been deleted by
// another actor.
func restorePreferredAddressTypes(ctx context.Context, original []string, found bool) {
	patch := []byte(`{"spec":{"preferredAddressTypes":null}}`)

	if found {
		encoded, marshalErr := json.Marshal(original)
		if marshalErr != nil {
			GinkgoWriter.Printf(
				"DeferCleanup: failed to marshal original preferredAddressTypes %v: %v\n",
				original, marshalErr)

			return
		}

		patch = fmt.Appendf(nil, `{"spec":{"preferredAddressTypes":%s}}`, encoded)
	}

	// Nothing to undo when the spec failed before it ever patched. Patching
	// anyway is harmless, but then waiting for a pod rollout that will never
	// happen would block for DSPodRestartTimeout.
	current, readErr := currentPreferredAddressTypes(ctx)
	if readErr == nil && slices.Equal(current, original) {
		GinkgoWriter.Printf(
			"DeferCleanup: preferredAddressTypes is already %v; nothing to restore\n", original)

		return
	}

	// Captured before the patch so the wait below can tell the pre-restore
	// pods from their replacements.
	preRestoreUIDs := collectDSPodUIDs()
	restored := false

	Eventually(func() error {
		patchErr := APIClient.Patch(ctx,
			snrcForPatch(),
			client.RawPatch(types.MergePatchType, patch))
		if k8serrors.IsNotFound(patchErr) {
			GinkgoWriter.Printf(
				"DeferCleanup: SNRC %q no longer exists; nothing to restore\n",
				snrparams.SNRConfigName)

			return nil
		}

		if patchErr == nil {
			restored = true
		}

		return patchErr
	}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
		"Failed to restore the original preferredAddressTypes on SNRC %q",
		snrparams.SNRConfigName)

	if !restored {
		return
	}

	By("DeferCleanup: waiting for the agent DaemonSet to roll out after the restore")

	// Wait for the pods to be *replaced*, not merely Running. The restore
	// changes the pod template, so a rollout is pending but has not started
	// yet; the pre-restore pods are still Running at this instant and a bare
	// verifyDSPodsRunning returns immediately. The spec would then report done
	// while the DaemonSet is still converging, leaving the next spec to start
	// against a cluster mid-rollout.
	Eventually(func() error {
		return verifyDSPodsReplaced(preRestoreUIDs)
	}, snrparams.DSPodRestartTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
		"SNR DaemonSet pods must be replaced and Running after preferredAddressTypes is restored")
}

// probeKubeletFromPodNetwork curls the kubelet on the given address from a
// run-to-completion pod pinned to that node, and returns the HTTP status code
// curl reported ("000" when it could not connect).
//
// The probe must run from the pod network: the SNR agent DaemonSet is not
// host-networked (dnsPolicy: ClusterFirst, no hostNetwork), so that is the
// path RHWA-1596 breaks. Probing from the test runner would exercise the
// runner's network instead, and an `oc debug node/` pod is host-networked.
//
// Results come from pod logs rather than ExecCommand for the same reason the
// SBR watchdog probe does it: exec resolves the external API hostname over
// SPDY, which fails in disconnected clusters, while logs go through
// kubernetes.default.svc.
// probeIndex identifies the probe pod for a node. Node names are often long
// FQDNs, and prefixing one would push the pod name's first DNS label past 63
// characters, so the pods are numbered and the mapping is logged instead.
func probeKubeletFromPodNetwork(
	ctx context.Context, probeIndex int, nodeName, address string,
) string {
	target := net.JoinHostPort(address, snrparams.KubeletSecurePort)
	probeName := fmt.Sprintf("snr-kubelet-probe-%d", probeIndex)
	GinkgoWriter.Printf("Probing kubelet at %s on node %q from pod %q\n",
		target, nodeName, probeName)
	probePod, createErr := pod.NewBuilder(
		APIClient, probeName, medik8sparams.OperatorNs, snrparams.WorkloadTestImage).
		DefineOnNode(nodeName).
		// WithTolerationToMaster and WithTolerationToControlPlane each *replace*
		// the toleration slice, so they cannot be chained. Append both.
		WithToleration(corev1.Toleration{
			Key:      "node-role.kubernetes.io/master",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}).
		WithToleration(corev1.Toleration{
			Key:      "node-role.kubernetes.io/control-plane",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}).
		WithRestartPolicy(corev1.RestartPolicyNever).
		RedefineDefaultCMD([]string{
			"sh", "-c",
			fmt.Sprintf("curl -sS -k -o /dev/null --max-time %d -w '%%{http_code}' https://%s/pods",
				int(snrparams.KubeletProbeTimeout.Seconds()), target),
		}).
		Create()
	Expect(createErr).ToNot(HaveOccurred(),
		"Failed to create kubelet probe pod %q on node %q", probeName, nodeName)

	DeferCleanup(func(name string) {
		existing, pullErr := pod.Pull(APIClient, name, medik8sparams.OperatorNs)
		if pullErr != nil {
			GinkgoWriter.Printf("DeferCleanup: kubelet probe pod %s already gone: %v\n", name, pullErr)

			return
		}

		if _, delErr := existing.Delete(); delErr != nil {
			GinkgoWriter.Printf("DeferCleanup: failed to delete kubelet probe pod %s: %v\n",
				name, delErr)
		}
	}, probeName)

	Expect(probePod.WaitUntilInStatus(
		corev1.PodSucceeded, snrparams.KubeletProbePodTimeout)).To(Succeed(),
		"Kubelet probe pod %q on node %q must run to completion; it reports %s",
		probeName, nodeName, describePodState(ctx, probeName))
	logs, logErr := probePod.GetLog(snrparams.KubeletProbePodTimeout, "")
	Expect(logErr).ToNot(HaveOccurred(),
		"Failed to read logs of kubelet probe pod %q on node %q", probeName, nodeName)

	return strings.TrimSpace(logs)
}

// describePodState renders a pod's phase and container states for an
// assertion message, so a probe that never ran says why.
func describePodState(ctx context.Context, name string) string {
	probe := &corev1.Pod{}
	if err := APIClient.Get(ctx, client.ObjectKey{
		Name: name, Namespace: medik8sparams.OperatorNs,
	}, probe); err != nil {
		return fmt.Sprintf("no pod %q: %v", name, err)
	}

	states := make([]string, 0, len(probe.Status.ContainerStatuses))

	for _, cs := range probe.Status.ContainerStatuses {
		states = append(states, fmt.Sprintf("%s=%+v", cs.Name, cs.State))
	}

	return fmt.Sprintf("phase=%s node=%q reason=%q containers=[%s]",
		probe.Status.Phase, probe.Spec.NodeName, probe.Status.Reason, strings.Join(states, " "))
}

// verifyAgentNetworkPolicyAllowsKubeletEgress checks that the NetworkPolicy
// the SNRC controller reconciles for the agent pods permits egress to the
// kubelet port. That allowance is what makes preferredAddressTypes usable;
// upstream #373 added the policy after #331, and nothing upstream tests the
// two together.
//
// A missing NetworkPolicy is not a failure: no policy means no egress
// restriction, so the self-diagnostics path is still reachable. Only a policy
// that exists and omits the port is a defect.
func verifyAgentNetworkPolicyAllowsKubeletEgress(ctx context.Context) {
	policy := &networkingv1.NetworkPolicy{}
	getErr := APIClient.Get(ctx, client.ObjectKey{
		Name: snrparams.AgentNetworkPolicyName, Namespace: medik8sparams.OperatorNs,
	}, policy)

	if k8serrors.IsNotFound(getErr) {
		GinkgoWriter.Printf(
			"NetworkPolicy %q is absent; agent egress is unrestricted, so the kubelet check "+
				"is reachable. Builds before self-node-remediation#373 have no such policy.\n",
			snrparams.AgentNetworkPolicyName)

		return
	}

	Expect(getErr).ToNot(HaveOccurred(),
		"Failed to get NetworkPolicy %q", snrparams.AgentNetworkPolicyName)
	policySelector, selectorErr := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
	Expect(selectorErr).ToNot(HaveOccurred(),
		"NetworkPolicy %q has an unparseable podSelector", snrparams.AgentNetworkPolicyName)
	agentSelector, agentErr := k8slabels.Parse(snrparams.DaemonSetPodLabelSelector)
	Expect(agentErr).ToNot(HaveOccurred(), "Failed to parse the agent pod selector")
	agentLabels := k8slabels.Set{}
	requirements, _ := agentSelector.Requirements()

	for _, requirement := range requirements {
		agentLabels[requirement.Key()] = requirement.Values().List()[0]
	}

	Expect(policySelector.Matches(agentLabels)).To(BeTrue(),
		"NetworkPolicy %q must select the SNR agent pods (%s), but its podSelector is %s",
		snrparams.AgentNetworkPolicyName, agentLabels, policySelector)
	allowed := false

	for _, rule := range policy.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port == nil || port.Port.String() != snrparams.KubeletSecurePort {
				continue
			}

			if port.Protocol == nil || *port.Protocol == corev1.ProtocolTCP {
				allowed = true

				break
			}
		}
	}

	Expect(allowed).To(BeTrue(),
		"NetworkPolicy %q must allow agent egress to TCP %s; without it the control plane "+
			"self-diagnostics cannot reach the kubelet and RHWA-1596 reopens",
		snrparams.AgentNetworkPolicyName, snrparams.KubeletSecurePort)
}

// listAgentPods lists the SNR agent pods through the high-QPS EventsClient.
// Every caller below polls, and APIClient's default QPS 5 / Burst 10 limiter
// starves poll-heavy readers -- see tests/internal/inittools/inittools.go.
func listAgentPods(ctx context.Context) (*corev1.PodList, error) {
	agentPods, err := EventsClient.CoreV1().Pods(medik8sparams.OperatorNs).List(
		ctx, metav1.ListOptions{LabelSelector: snrparams.DaemonSetPodLabelSelector})
	if err != nil {
		return nil, fmt.Errorf("failed to list SNR agent pods: %w", err)
	}

	return agentPods, nil
}

// verifyControlPlaneAgentPods checks that every control plane node has an SNR
// agent pod that is Running with all containers Ready and not restarting.
func verifyControlPlaneAgentPods(ctx context.Context, controlPlaneNodes []string) error {
	agentPods, err := listAgentPods(ctx)
	if err != nil {
		return err
	}

	byNode := make(map[string]*corev1.Pod, len(agentPods.Items))

	for podIdx := range agentPods.Items {
		byNode[agentPods.Items[podIdx].Spec.NodeName] = &agentPods.Items[podIdx]
	}

	for _, nodeName := range controlPlaneNodes {
		agentPod, found := byNode[nodeName]
		if !found {
			return fmt.Errorf("control plane node %q has no SNR agent pod", nodeName)
		}

		if agentPod.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("agent pod %q on node %q is %s, expected Running",
				agentPod.Name, nodeName, agentPod.Status.Phase)
		}

		for _, containerStatus := range agentPod.Status.ContainerStatuses {
			if !containerStatus.Ready {
				return fmt.Errorf("agent pod %q on node %q: container %q is not ready (%+v)",
					agentPod.Name, nodeName, containerStatus.Name, containerStatus.State)
			}

			if containerStatus.State.Waiting != nil &&
				containerStatus.State.Waiting.Reason == "CrashLoopBackOff" {
				return fmt.Errorf("agent pod %q on node %q is in CrashLoopBackOff",
					agentPod.Name, nodeName)
			}
		}
	}

	return nil
}

// controlPlaneAgentPodsLoggingMessage returns the names of control plane agent
// pods whose logs contain the message, over the pod's whole lifetime.
// eco-goinfra's pod.List and GetLog take no context, so neither does this.
func controlPlaneAgentPodsLoggingMessage(
	controlPlaneNodes []string, message string,
) ([]string, error) {
	onControlPlane := make(map[string]bool, len(controlPlaneNodes))
	for _, nodeName := range controlPlaneNodes {
		onControlPlane[nodeName] = true
	}

	dsListOptions := metav1.ListOptions{LabelSelector: snrparams.DaemonSetPodLabelSelector}
	agentPods, listErr := pod.List(APIClient, medik8sparams.OperatorNs, dsListOptions)
	if listErr != nil {
		return nil, fmt.Errorf("failed to list SNR agent pods: %w", listErr)
	}

	offenders := make([]string, 0)

	for _, agentPod := range agentPods {
		if !onControlPlane[agentPod.Object.Spec.NodeName] {
			continue
		}

		logs, logErr := agentPod.GetLog(snrparams.DSPodRestartTimeout, "")
		if logErr != nil {
			return nil, fmt.Errorf("failed to read logs of agent pod %q: %w",
				agentPod.Object.Name, logErr)
		}

		if strings.Contains(logs, message) {
			offenders = append(offenders, agentPod.Object.Name)
		}
	}

	return offenders, nil
}

// noControlPlaneRemediation reports an error when any control plane node has a
// SelfNodeRemediation CR or has rebooted since BeforeAll recorded its boot ID.
// A transient API error is not a remediation. Over a multi-minute window a
// flaky API server will drop at least one request, and failing the assertion
// on that would report "a control plane node was remediated" when nothing of
// the sort happened. Read errors are therefore logged and tolerated; only a
// positive observation -- a CR that exists, or a boot ID that moved -- fails.
// observedOK records whether any poll ever managed a clean read, so a window
// in which every single request failed is not mistaken for a pass.
//
// This runs every DefaultPollInterval for PreferredAddressNoRemediationDuration,
// so its call volume matters: APIClient is built with client-go's default
// rate limiter (QPS 5 / Burst 10) and the repo documents it starving
// poll-heavy readers (tests/internal/inittools/inittools.go). Boot IDs are
// therefore read with one List on the high-QPS EventsClient rather than one
// Get per node on APIClient, leaving a single APIClient call per poll.
func noControlPlaneRemediation(
	ctx context.Context, controlPlaneNodes []string, bootIDs map[string]string,
	observedOK *bool,
) error {
	remediated, err := snrCRsForNodes(ctx, controlPlaneNodes)
	if err != nil {
		GinkgoWriter.Printf(
			"no-remediation check: tolerating transient API error listing SNR CRs: %v\n", err)

		return nil
	}

	if len(remediated) > 0 {
		return fmt.Errorf(
			"SelfNodeRemediation CR(s) %v exist for control plane nodes; "+
				"preferredAddressTypes must not trigger remediation", remediated)
	}

	nodeList, listErr := EventsClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if listErr != nil {
		GinkgoWriter.Printf(
			"no-remediation check: tolerating transient API error listing nodes: %v\n", listErr)

		return nil
	}

	observed := make(map[string]*corev1.Node, len(nodeList.Items))
	for nodeIdx := range nodeList.Items {
		observed[nodeList.Items[nodeIdx].Name] = &nodeList.Items[nodeIdx]
	}

	for _, nodeName := range controlPlaneNodes {
		node, found := observed[nodeName]
		if !found {
			GinkgoWriter.Printf(
				"no-remediation check: node %q absent from this read; tolerating\n", nodeName)

			return nil
		}

		// Ready is the primary trip-wire, and it has to be: a watchdog reset
		// does not change status.nodeInfo.bootID until the node has rebooted,
		// come back and re-registered -- about five minutes, longer than this
		// whole observation window. Ready flips to False/Unknown within
		// node-monitor-grace-period (~40s), so it is the only signal that
		// actually fits inside the window. BeforeAll asserted every control
		// plane node was Ready, so a transition here is caused by this test.
		if ready := nodeReadyStatus(node); ready != corev1.ConditionTrue {
			return fmt.Errorf(
				"control plane node %q left Ready (now %q) after preferredAddressTypes was set; "+
					"this is how a watchdog self-remediation first becomes visible",
				nodeName, ready)
		}

		currentBootID := node.Status.NodeInfo.BootID
		if currentBootID == "" {
			GinkgoWriter.Printf(
				"no-remediation check: node %q has no boot ID in this read; tolerating\n", nodeName)

			return nil
		}

		// Confirmation rather than detection: if a reboot completes fast
		// enough for the new boot ID to land inside the window, catch it too.
		if currentBootID != bootIDs[nodeName] {
			return fmt.Errorf("control plane node %q rebooted: boot ID %q -> %q",
				nodeName, bootIDs[nodeName], currentBootID)
		}
	}

	*observedOK = true

	return nil
}

// nodeReadyStatus returns the node's Ready condition status, or
// ConditionUnknown when the condition is absent.
func nodeReadyStatus(node *corev1.Node) corev1.ConditionStatus {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status
		}
	}

	return corev1.ConditionUnknown
}

// snrCRsForNodes returns the names of SelfNodeRemediation CRs whose name
// matches one of the given nodes. SNR names the CR after the node it
// remediates.
func snrCRsForNodes(ctx context.Context, nodeNames []string) ([]string, error) {
	wanted := make(map[string]bool, len(nodeNames))
	for _, nodeName := range nodeNames {
		wanted[nodeName] = true
	}

	snrList := &unstructured.UnstructuredList{}
	snrList.SetGroupVersionKind(snrGVK)

	if err := APIClient.List(ctx, snrList,
		client.InNamespace(medik8sparams.OperatorNs)); err != nil {
		return nil, fmt.Errorf("failed to list SelfNodeRemediation CRs: %w", err)
	}

	matched := make([]string, 0)

	for _, snrCR := range snrList.Items {
		if wanted[snrCR.GetName()] {
			matched = append(matched, snrCR.GetName())
		}
	}

	return matched, nil
}

// nhcNamesSelectingNodes returns the names of NodeHealthCheck CRs whose
// selector matches any of the given nodes. Such an NHC could legitimately
// create a SelfNodeRemediation during the observation window, which would
// fail the no-remediation assertion for a reason unrelated to this fix.
func nhcNamesSelectingNodes(ctx context.Context, nodes []corev1.Node) ([]string, error) {
	if !isNHCCRDInstalled() {
		return nil, nil
	}

	nhcList := &unstructured.UnstructuredList{}
	nhcList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   snrparams.NHCAPIGroup,
		Version: snrparams.NHCAPIVersion,
		Kind:    "NodeHealthCheckList",
	})

	if err := APIClient.List(ctx, nhcList); err != nil {
		return nil, fmt.Errorf("failed to list NodeHealthCheck CRs: %w", err)
	}

	matched := make([]string, 0)

	for _, nhc := range nhcList.Items {
		rawSelector, found, err := unstructured.NestedMap(nhc.Object, "spec", "selector")
		if err != nil {
			return nil, fmt.Errorf("failed to read spec.selector of NHC %q: %w", nhc.GetName(), err)
		}

		// An absent or empty selector matches every node, control plane included.
		selector := k8slabels.Everything()

		if found && len(rawSelector) > 0 {
			labelSelector := &metav1.LabelSelector{}
			if convErr := runtime.DefaultUnstructuredConverter.FromUnstructured(
				rawSelector, labelSelector); convErr != nil {
				return nil, fmt.Errorf("failed to decode selector of NHC %q: %w",
					nhc.GetName(), convErr)
			}

			selector, err = metav1.LabelSelectorAsSelector(labelSelector)
			if err != nil {
				return nil, fmt.Errorf("failed to convert selector of NHC %q: %w",
					nhc.GetName(), err)
			}
		}

		for nodeIdx := range nodes {
			if selector.Matches(k8slabels.Set(nodes[nodeIdx].Labels)) {
				matched = append(matched, nhc.GetName())

				break
			}
		}
	}

	return matched, nil
}
