package tests

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The SBR disruptive specs simulate storage loss with iptables REJECT rules on CephFS ports,
// injected into the host namespaces of a target node. Those rules outlive the pod that
// created them: if a run is interrupted, or the injector pod is deleted before its flush
// runs, the node keeps rejecting CephFS traffic. The ODF CSI node plugin then deregisters
// from that node, and every later spec that needs shared storage fails with an
// unrelated-looking "CSINode <node> does not contain driver ...cephfs..." after a ten-minute
// SBRC readiness timeout.
//
// Cleanup therefore must not depend on the injector pod still existing. The helpers below
// sweep a node with a pod of their own, are idempotent, and recognise every rule shape the
// suite injects (OUTPUT-only or bidirectional, plain --dport or -m multiport --dports).

const (
	// cephFSRejectRulePattern is an ERE matching any iptables rule in the INPUT or OUTPUT
	// chain that REJECTs a CephFS port group: 3300 (msgr2), 6789 (msgr1 mon) or 6800:7300
	// (OSD/MDS). The optional trailing "s" covers both --dport and multiport --dports.
	cephFSRejectRulePattern = `^-A (INPUT|OUTPUT) .*(sport|dport)s? (3300|6789|6800:7300)( |$).*-j REJECT`

	// cephFSSweepMaxPasses bounds the delete loop. Injection uses iptables -I and is retried
	// across specs, so a chain can hold duplicates of a rule; each pass removes one copy.
	cephFSSweepMaxPasses = 5

	// cephFSSweepResultPrefix marks the one machine-readable line the sweep script prints.
	cephFSSweepResultPrefix = "CEPHFS_SWEEP"

	// maxPodNameLength is the DNS-1123 label limit applied to generated pod names.
	maxPodNameLength = 63
)

// cephFSRejectSweepScript returns a POSIX shell script that deletes every rule matching
// cephFSRejectRulePattern from the host's netfilter tables, then prints
// "CEPHFS_SWEEP before=<n> after=<n>". Deleting by the spec iptables itself reports (-S
// output rewritten from -A to -D) is what makes the sweep shape-agnostic: no caller has to
// know which inject variant wrote the rule. A node with no CephFS REJECT rules is left
// untouched and reports before=0 after=0.
func cephFSRejectSweepScript() string {
	return fmt.Sprintf(`set -u
pattern='%s'
count() { iptables -S 2>/dev/null | grep -cE "$pattern" || true; }
before=$(count)
pass=0
while [ "$pass" -lt %d ]; do
	matched=$(iptables -S 2>/dev/null | grep -E "$pattern" || true)
	[ -n "$matched" ] || break
	printf '%%s\n' "$matched" | sed 's/^-A /-D /' | while IFS= read -r spec; do
		# $spec is deliberately unquoted: it carries a whole rule specification to word-split.
		[ -n "$spec" ] && iptables $spec 2>/dev/null
	done
	pass=$((pass + 1))
done
echo "%s before=$before after=$(count)"
`, cephFSRejectRulePattern, cephFSSweepMaxPasses, cephFSSweepResultPrefix)
}

// nodeScopedPodName builds a DNS-1123 pod name from a prefix and a node name. Node names are
// short hostnames in the libvirt lab but FQDNs on cloud platforms, so they need sanitising.
func nodeScopedPodName(prefix, nodeName string) string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}

		return '-'
	}, strings.ToLower(nodeName))

	name := prefix + safe
	if len(name) > maxPodNameLength {
		name = name[:maxPodNameLength]
	}

	return strings.TrimRight(name, "-")
}

// startCephFSSweepPod creates a privileged pod on nodeName that can nsenter the host network
// and mount namespaces, replacing any pod left over from an earlier attempt.
//
// The blanket NoSchedule and NoExecute tolerations are not optional: a node that has lost
// storage is frequently also carrying a fencing taint such as
// node.kubernetes.io/out-of-service, and an untolerated sweep pod would never be scheduled
// onto the one node that needs cleaning.
func startCephFSSweepPod(nodeName string) (*pod.Builder, error) {
	name := nodeScopedPodName(sbrparams.CephFSSweepPodName, nodeName)
	if existing, pullErr := pod.Pull(APIClient, name, medik8sparams.OperatorNs); pullErr == nil {
		if _, delErr := existing.DeleteAndWait(medik8sparams.DefaultTimeout); delErr != nil {
			return nil, fmt.Errorf("delete stale sweep pod %s: %w", name, delErr)
		}
	}

	sweeper := pod.NewBuilder(APIClient, name, medik8sparams.OperatorNs, sbrparams.InjectorImage).
		DefineOnNode(nodeName).
		WithHostPid(true).
		WithPrivilegedFlag().
		WithToleration(corev1.Toleration{
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}).
		WithToleration(corev1.Toleration{
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoExecute,
		}).
		WithRestartPolicy(corev1.RestartPolicyNever).
		RedefineDefaultCMD([]string{"sleep", sbrparams.CephFSSweepPodLifetime})

	if _, createErr := sweeper.CreateAndWaitUntilRunning(sbrparams.CephFSSweepPodTimeout); createErr != nil {
		// A pod that never reached Running is still an object on the API server; leaving it
		// behind would sit sleeping on the node once it finally schedules.
		if _, delErr := sweeper.Delete(); delErr != nil {
			GinkgoWriter.Printf("Warning: delete sweep pod %s after failed start: %v\n", name, delErr)
		}

		return nil, fmt.Errorf("start sweep pod %s on node %s: %w", name, nodeName, createErr)
	}

	return sweeper, nil
}

// parseCephFSSweepResult extracts the rule counts from the sweep script's result line.
func parseCephFSSweepResult(output string) (int, int, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != cephFSSweepResultPrefix {
			continue
		}

		before, beforeErr := strconv.Atoi(strings.TrimPrefix(fields[1], "before="))
		if beforeErr != nil {
			return 0, 0, fmt.Errorf("parse %q: %w", line, beforeErr)
		}

		after, afterErr := strconv.Atoi(strings.TrimPrefix(fields[2], "after="))
		if afterErr != nil {
			return 0, 0, fmt.Errorf("parse %q: %w", line, afterErr)
		}

		return before, after, nil
	}

	return 0, 0, fmt.Errorf("sweep output has no %s line: %q", cephFSSweepResultPrefix, output)
}

// sweepCephFSRejectRules removes every CephFS REJECT rule from nodeName and returns how many
// rules were found and how many survived the sweep. It runs through a pod of its own, so
// unlike removeCephFSRejectOutput and removeCephFSRejectBidirectional it does not need the
// injector pod that created the rules to still exist.
func sweepCephFSRejectRules(nodeName string) (int, int, error) {
	sweeper, startErr := startCephFSSweepPod(nodeName)
	if startErr != nil {
		return 0, 0, startErr
	}

	defer func() {
		if _, delErr := sweeper.Delete(); delErr != nil {
			GinkgoWriter.Printf("Warning: delete CephFS sweep pod on node %s: %v\n", nodeName, delErr)
		}
	}()

	output, execErr := sweeper.ExecCommand([]string{
		"nsenter", "--target", "1", "--net", "--mount", "--", "sh", "-c", cephFSRejectSweepScript(),
	})
	if execErr != nil {
		return 0, 0, fmt.Errorf("sweep CephFS REJECT rules on node %s: %w", nodeName, execErr)
	}

	return parseCephFSSweepResult(output.String())
}

// ensureCephFSRejectRulesCleared is the teardown guard every CephFS injection must end with.
//
// A sweep that cannot run at all — node rebooting, pod unschedulable, API unavailable — is
// only a warning, because the rules are not persisted and the reboot these specs trigger
// clears them anyway. Rules that are still present after a sweep that did run are a failure:
// the node is left unable to mount CephFS, and reporting it here is the difference between
// one honest failure and a run of misleading timeouts in unrelated specs.
func ensureCephFSRejectRulesCleared(nodeName string) {
	if nodeName == "" {
		return
	}

	found, remaining, err := sweepCephFSRejectRules(nodeName)
	if err != nil {
		GinkgoWriter.Printf("Warning: could not sweep CephFS REJECT rules on node %s: %v\n", nodeName, err)

		return
	}

	if found > 0 {
		GinkgoWriter.Printf("Swept %d leftover CephFS REJECT rule(s) from node %s\n", found, nodeName)
	}

	if remaining > 0 {
		Fail(fmt.Sprintf("Node %s still has %d CephFS REJECT rule(s) after cleanup; "+
			"CephFS mounts on that node will fail until they are removed", nodeName, remaining))
	}
}

// deleteInjectorPod removes a named injector pod if it is still present. Teardown paths used
// to pull the pod and skip the iptables flush entirely when the pull failed, which is exactly
// how rules leaked; flushing is now the sweep's job and this only reaps the pod.
func deleteInjectorPod(podName string) {
	existing, pullErr := pod.Pull(APIClient, podName, medik8sparams.OperatorNs)
	if pullErr != nil {
		return
	}

	if _, delErr := existing.Delete(); delErr != nil {
		GinkgoWriter.Printf("Warning: delete injector pod %s: %v\n", podName, delErr)
	}
}

// sweepCephFSRejectRulesFromWorkers clears CephFS REJECT rules left on any worker node by an
// earlier interrupted run, so a poisoned cluster heals itself instead of failing the first
// spec that needs shared storage. Best-effort throughout: this runs before the suite knows
// whether the operator is even installed.
func sweepCephFSRejectRulesFromWorkers() {
	if _, nsErr := APIClient.CoreV1Interface.Namespaces().Get(
		context.TODO(), medik8sparams.OperatorNs, metav1.GetOptions{},
	); nsErr != nil {
		GinkgoWriter.Printf("Skipping CephFS REJECT-rule pre-flight: namespace %s unavailable: %v\n",
			medik8sparams.OperatorNs, nsErr)

		return
	}

	nodeList, listErr := APIClient.CoreV1Interface.Nodes().List(context.TODO(), metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/worker",
	})
	if listErr != nil {
		GinkgoWriter.Printf("Skipping CephFS REJECT-rule pre-flight: cannot list worker nodes: %v\n", listErr)

		return
	}

	for nodeIdx := range nodeList.Items {
		nodeName := nodeList.Items[nodeIdx].Name
		found, remaining, err := sweepCephFSRejectRules(nodeName)
		switch {
		case err != nil:
			GinkgoWriter.Printf("Pre-flight: could not sweep CephFS REJECT rules on node %s: %v\n", nodeName, err)
		case remaining > 0:
			GinkgoWriter.Printf("Pre-flight: node %s still has %d CephFS REJECT rule(s); "+
				"CephFS mounts there will fail\n", nodeName, remaining)
		case found > 0:
			GinkgoWriter.Printf("Pre-flight: swept %d stale CephFS REJECT rule(s) from node %s\n", found, nodeName)
		}
	}
}

var _ = BeforeSuite(func() {
	By("Pre-flight: clearing CephFS REJECT rules left on worker nodes by any earlier run")

	sweepCephFSRejectRulesFromWorkers()
})
