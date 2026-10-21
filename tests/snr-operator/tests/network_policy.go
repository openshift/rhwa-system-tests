package tests

import (
	"context"
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrparams"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe(
	"SNR NetworkPolicy tests",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSNR), func() {
		var (
			originalEndpointURL      string
			originalEndpointURLFound bool
			hostPort                 int32
		)

		BeforeAll(func() {
			By("Verifying the default SelfNodeRemediationConfig is ready")

			snrc := &unstructured.Unstructured{}
			snrc.SetGroupVersionKind(snrcGVK)
			err := APIClient.Get(context.TODO(),
				client.ObjectKey{
					Name:      snrparams.SNRConfigName,
					Namespace: medik8sparams.OperatorNs,
				},
				snrc)
			Expect(err).ToNot(HaveOccurred(),
				"Failed to get default SNRC %q", snrparams.SNRConfigName)
			originalEndpointURL, originalEndpointURLFound, err = unstructured.NestedString(
				snrc.Object, "spec", "endpointHealthCheckUrl")
			Expect(err).ToNot(HaveOccurred(),
				"Failed to read endpointHealthCheckUrl from SNRC")
			hostPortInt64, hostPortFound, hostPortErr := unstructured.NestedInt64(
				snrc.Object, "spec", "hostPort")
			Expect(hostPortErr).ToNot(HaveOccurred(),
				"Failed to read hostPort from SNRC")

			if hostPortFound {
				hostPort = int32(hostPortInt64)
			} else {
				hostPort = int32(snrparams.DefaultHostPort)
			}
		})

		AfterAll(func() {
			By("DeferCleanup: restoring original EndpointHealthCheckUrl on the default SNRC")
			restoreEndpointHealthCheckURL(originalEndpointURL, originalEndpointURLFound)
		})

		It("Verify SNR manages the agent DaemonSet NetworkPolicy",
			reportxml.ID("1645"),
			Label(labels.TierSmoke, labels.DisruptionNonDestructive,
				labels.PlatformAny, labels.FrequencyPresubmit,
				labels.ComponentNetworkPolicy), func() {
				By("Verifying the NetworkPolicy self-node-remediation-ds-np exists for the default SelfNodeRemediationConfig")

				networkPolicy := &networkingv1.NetworkPolicy{}
				Eventually(func() error {
					return APIClient.Get(context.TODO(),
						client.ObjectKey{
							Name:      snrparams.NetworkPolicyName,
							Namespace: medik8sparams.OperatorNs,
						}, networkPolicy)
				}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
					"NetworkPolicy %q must exist", snrparams.NetworkPolicyName)

				By("Verifying the NetworkPolicy is owned by the SelfNodeRemediationConfig CR")

				Expect(networkPolicy.OwnerReferences).To(HaveLen(1),
					"NetworkPolicy %q should have exactly one owner reference", snrparams.NetworkPolicyName)
				Expect(networkPolicy.OwnerReferences[0].Kind).To(Equal("SelfNodeRemediationConfig"),
					"NetworkPolicy %q owner reference kind must be SelfNodeRemediationConfig", snrparams.NetworkPolicyName)
				Expect(networkPolicy.OwnerReferences[0].Name).To(Equal(snrparams.SNRConfigName),
					"NetworkPolicy %q owner reference name must be %q", snrparams.NetworkPolicyName, snrparams.SNRConfigName)

				By("Verifying the NetworkPolicy's static ingress and egress rules")

				verifyStaticNetworkPolicyRules(networkPolicy, hostPort)

				By("Deleting the NetworkPolicy and verifying the operator recreates it")

				originalUID := networkPolicy.UID
				Expect(APIClient.Delete(context.TODO(), networkPolicy)).To(Succeed(),
					"Failed to delete NetworkPolicy %q", snrparams.NetworkPolicyName)

				Eventually(func() error {
					return verifyNetworkPolicyRecreated(originalUID)
				}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
					"NetworkPolicy %q must be recreated by the operator after deletion", snrparams.NetworkPolicyName)

				By("Setting EndpointHealthCheckUrl and verifying the egress ipBlock rule resolves to its IP")

				patchEndpointHealthCheckURL(snrparams.EndpointTestIPv4A)

				Eventually(func() error {
					return verifyEgressIPBlockCIDR(snrparams.EndpointTestIPv4ACIDR)
				}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
					"NetworkPolicy %q egress must include an ipBlock rule scoped to %s",
					snrparams.NetworkPolicyName, snrparams.EndpointTestIPv4ACIDR)

				By("Simulating a DNS flip and verifying the egress ipBlock rule updates to the new IP")

				patchEndpointHealthCheckURL(snrparams.EndpointTestIPv4B)

				Eventually(func() error {
					return verifyEgressIPBlockCIDR(snrparams.EndpointTestIPv4BCIDR)
				}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
					"NetworkPolicy %q egress must flip to the new ipBlock %s after the simulated DNS change",
					snrparams.NetworkPolicyName, snrparams.EndpointTestIPv4BCIDR)

				By("Validating NetworkPolicy stability under rapid EndpointHealthCheckUrl changes (race test)")

				finalCIDR := raceEndpointHealthCheckURL()

				Eventually(func() error {
					return verifyEgressIPBlockCIDR(finalCIDR)
				}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
					"NetworkPolicy %q egress must converge on the final ipBlock %s after rapid changes",
					snrparams.NetworkPolicyName, finalCIDR)

				Consistently(func() error {
					return verifyEgressIPBlockCIDR(finalCIDR)
				}, snrparams.NetworkPolicyStabilityWindow, snrparams.DefaultPollInterval).Should(Succeed(),
					"NetworkPolicy %q egress must remain stable at exactly one ipBlock rule (%s), "+
						"with no flip-flopping or race-induced clobbering",
					snrparams.NetworkPolicyName, finalCIDR)
			})
	})

// hasPort reports whether rule allows the given protocol/port combination.
func hasPort(rule networkingv1.NetworkPolicyEgressRule, port int32, protocol corev1.Protocol) bool {
	for _, p := range rule.Ports {
		if p.Protocol != nil && *p.Protocol == protocol && p.Port != nil && p.Port.IntVal == port {
			return true
		}
	}

	return false
}

// egressPortSet collapses a rule's ports into protocol -> port list, for
// comparing an entire rule's allowed ports regardless of declared order.
func egressPortSet(rule networkingv1.NetworkPolicyEgressRule) map[corev1.Protocol][]int32 {
	ports := map[corev1.Protocol][]int32{}
	for _, p := range rule.Ports {
		if p.Protocol == nil || p.Port == nil {
			continue
		}

		ports[*p.Protocol] = append(ports[*p.Protocol], p.Port.IntVal)
	}

	return ports
}

// verifyStaticNetworkPolicyRules asserts the self-node-remediation-ds-np
// NetworkPolicy's baseline (no EndpointHealthCheckUrl) shape: it selects only
// agent pods, restricts both ingress and egress, allows peer traffic on
// hostPort in both directions, and allows DNS/HTTPS/own-node-kubelet egress.
func verifyStaticNetworkPolicyRules(networkPolicy *networkingv1.NetworkPolicy, hostPort int32) {
	Expect(networkPolicy.Spec.PodSelector.MatchLabels).To(Equal(snrparams.AgentPodSelectorMatchLabels),
		"NetworkPolicy %q must select only the agent pods", networkPolicy.Name)
	Expect(networkPolicy.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress),
		"NetworkPolicy %q must restrict both ingress and egress", networkPolicy.Name)
	Expect(networkPolicy.Spec.Ingress).To(HaveLen(1),
		"NetworkPolicy %q must have exactly one ingress rule for peer traffic", networkPolicy.Name)
	ingressRule := networkPolicy.Spec.Ingress[0]
	Expect(ingressRule.From).To(HaveLen(1),
		"NetworkPolicy %q ingress rule must have exactly one peer", networkPolicy.Name)
	Expect(ingressRule.From[0].PodSelector).ToNot(BeNil(),
		"NetworkPolicy %q ingress peer must be a podSelector", networkPolicy.Name)
	Expect(ingressRule.From[0].PodSelector.MatchLabels).To(Equal(snrparams.AgentPodSelectorMatchLabels),
		"NetworkPolicy %q ingress peer selector must match the agent pods", networkPolicy.Name)
	Expect(ingressRule.Ports).To(HaveLen(1),
		"NetworkPolicy %q ingress rule must allow exactly one port", networkPolicy.Name)
	Expect(ingressRule.Ports[0].Port.IntValue()).To(Equal(int(hostPort)),
		"NetworkPolicy %q ingress port must match the configured hostPort (%d)", networkPolicy.Name, hostPort)
	Expect(ingressRule.Ports[0].Protocol).ToNot(BeNil())
	Expect(*ingressRule.Ports[0].Protocol).To(Equal(corev1.ProtocolTCP),
		"NetworkPolicy %q ingress protocol must be TCP", networkPolicy.Name)
	Expect(networkPolicy.Spec.Egress).To(HaveLen(4),
		"NetworkPolicy %q must have exactly 4 static egress rules: peer, DNS, HTTPS, kubelet", networkPolicy.Name)

	var foundPeer, foundDNS, foundHTTPS, foundKubelet bool

	for _, egressRule := range networkPolicy.Spec.Egress {
		switch {
		case len(egressRule.To) == 1 && egressRule.To[0].PodSelector != nil:
			foundPeer = true
			Expect(egressRule.To[0].PodSelector.MatchLabels).To(Equal(snrparams.AgentPodSelectorMatchLabels),
				"NetworkPolicy %q peer egress selector must match the agent pods", networkPolicy.Name)
			Expect(egressRule.Ports).To(HaveLen(1),
				"NetworkPolicy %q peer egress rule must allow exactly one port", networkPolicy.Name)
			Expect(egressRule.Ports[0].Port.IntValue()).To(Equal(int(hostPort)),
				"NetworkPolicy %q peer egress port must match the configured hostPort (%d)", networkPolicy.Name, hostPort)
			Expect(*egressRule.Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))

		case len(egressRule.To) == 0 && hasPort(egressRule, snrparams.NetworkPolicyDNSPort, corev1.ProtocolUDP):
			foundDNS = true
			Expect(egressPortSet(egressRule)).To(Equal(map[corev1.Protocol][]int32{
				corev1.ProtocolUDP: {snrparams.NetworkPolicyDNSPort, snrparams.NetworkPolicyDNSServicePort},
				corev1.ProtocolTCP: {snrparams.NetworkPolicyDNSPort, snrparams.NetworkPolicyDNSServicePort},
			}), "NetworkPolicy %q DNS egress rule must allow UDP/TCP on ports 53 and 5353", networkPolicy.Name)

		case len(egressRule.To) == 0 && len(egressRule.Ports) == 2 &&
			hasPort(egressRule, snrparams.NetworkPolicyHTTPSPort, corev1.ProtocolTCP):
			foundHTTPS = true
			Expect(egressPortSet(egressRule)).To(Equal(map[corev1.Protocol][]int32{
				corev1.ProtocolTCP: {snrparams.NetworkPolicyHTTPSPort, snrparams.NetworkPolicyAPIServerPort},
			}), "NetworkPolicy %q HTTPS egress rule must allow TCP on ports 443 and 6443", networkPolicy.Name)

		case len(egressRule.To) == 0 && len(egressRule.Ports) == 1 &&
			hasPort(egressRule, snrparams.NetworkPolicyKubeletPort, corev1.ProtocolTCP):
			foundKubelet = true
		}
	}

	Expect(foundPeer).To(BeTrue(), "NetworkPolicy %q is missing the peer egress rule", networkPolicy.Name)
	Expect(foundDNS).To(BeTrue(), "NetworkPolicy %q is missing the DNS egress rule", networkPolicy.Name)
	Expect(foundHTTPS).To(BeTrue(), "NetworkPolicy %q is missing the HTTPS egress rule", networkPolicy.Name)
	Expect(foundKubelet).To(BeTrue(), "NetworkPolicy %q is missing the own-node kubelet egress rule", networkPolicy.Name)
}

// verifyNetworkPolicyRecreated checks that the NetworkPolicy exists again
// with a UID different from originalUID, confirming the operator recreated
// it rather than the old object lingering.
func verifyNetworkPolicyRecreated(originalUID types.UID) error {
	recreated := &networkingv1.NetworkPolicy{}
	if err := APIClient.Get(context.TODO(),
		client.ObjectKey{
			Name:      snrparams.NetworkPolicyName,
			Namespace: medik8sparams.OperatorNs,
		}, recreated); err != nil {
		return err
	}

	if recreated.UID == originalUID {
		return fmt.Errorf("NetworkPolicy %q still has the pre-deletion UID %s",
			snrparams.NetworkPolicyName, originalUID)
	}

	return nil
}

// verifyEgressIPBlockCIDR fetches the live NetworkPolicy and checks that its
// egress rules contain exactly one bare ipBlock rule (no ports, a single
// peer), scoped to expectedCIDR. More than one such rule, a different CIDR,
// or none at all is an error
func verifyEgressIPBlockCIDR(expectedCIDR string) error {
	networkPolicy := &networkingv1.NetworkPolicy{}
	if err := APIClient.Get(context.TODO(),
		client.ObjectKey{
			Name:      snrparams.NetworkPolicyName,
			Namespace: medik8sparams.OperatorNs,
		}, networkPolicy); err != nil {
		return err
	}

	var foundCIDRs []string

	for _, egressRule := range networkPolicy.Spec.Egress {
		if len(egressRule.Ports) == 0 && len(egressRule.To) == 1 && egressRule.To[0].IPBlock != nil {
			foundCIDRs = append(foundCIDRs, egressRule.To[0].IPBlock.CIDR)
		}
	}

	if len(foundCIDRs) != 1 || foundCIDRs[0] != expectedCIDR {
		return fmt.Errorf("expected exactly one ipBlock egress rule %q, found %v",
			expectedCIDR, foundCIDRs)
	}

	return nil
}

// patchEndpointHealthCheckURL merge-patches the default SNRC's
// spec.endpointHealthCheckUrl to url and waits for the patch call itself to
// succeed.
func patchEndpointHealthCheckURL(url string) {
	urlJSON, err := json.Marshal(url)
	Expect(err).ToNot(HaveOccurred(), "Failed to marshal EndpointHealthCheckUrl %q", url)
	patch := []byte(fmt.Sprintf(`{"spec":{"endpointHealthCheckUrl":%s}}`, urlJSON))

	Eventually(func() error {
		return APIClient.Patch(context.TODO(),
			snrcForPatch(),
			client.RawPatch(types.MergePatchType, patch))
	}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
		"Failed to patch SNRC %q endpointHealthCheckUrl to %q", snrparams.SNRConfigName, url)
}

// raceEndpointHealthCheckURL applies NetworkPolicyRaceIterations rapid,
// alternating EndpointHealthCheckUrl patches without waiting for the
// NetworkPolicy to converge between them, then returns the CIDR expected
// to win once things settle (whichever address was patched last).
func raceEndpointHealthCheckURL() string {
	addresses := []string{snrparams.EndpointTestIPv4A, snrparams.EndpointTestIPv4B}
	cidrs := []string{snrparams.EndpointTestIPv4ACIDR, snrparams.EndpointTestIPv4BCIDR}

	var finalCIDR string

	for iteration := 0; iteration < snrparams.NetworkPolicyRaceIterations; iteration++ {
		slot := iteration % len(addresses)
		patchEndpointHealthCheckURL(addresses[slot])
		finalCIDR = cidrs[slot]
	}

	return finalCIDR
}

// restoreEndpointHealthCheckURL merge-patches the default SNRC's
// spec.endpointHealthCheckUrl back to its pre-test value, or clears it via a
// null merge-patch if it was unset.
func restoreEndpointHealthCheckURL(originalURL string, found bool) {
	var patch []byte

	if found {
		urlJSON, err := json.Marshal(originalURL)
		Expect(err).ToNot(HaveOccurred(), "Failed to marshal original EndpointHealthCheckUrl")
		patch = []byte(fmt.Sprintf(`{"spec":{"endpointHealthCheckUrl":%s}}`, urlJSON))
	} else {
		patch = []byte(`{"spec":{"endpointHealthCheckUrl":null}}`)
	}

	Eventually(func() error {
		return APIClient.Patch(context.TODO(),
			snrcForPatch(),
			client.RawPatch(types.MergePatchType, patch))
	}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
		"Failed to restore original EndpointHealthCheckUrl on SNRC %q", snrparams.SNRConfigName)
}
