package tests

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	bpnRejoinCordon  = "sbr.medik8s.io/rejoin-cordon"
	bpnRejoinPending = "SBRRejoinPending"
)

type bpnBootUnitTiming struct {
	Start, Exit, KubeletStart time.Duration
}

func (test *bpnFixture) enableBootUnit() {
	GinkgoHelper()
	test.bootUnit = os.Getenv("SBR_BOOT_UNIT_NAME")
	if !sbrutils.ValidBlockBootServiceName(test.bootUnit) {
		Skip("BLOCKED: set SBR_BOOT_UNIT_NAME to the SBR agent's host service name, including .service")
	}
}

func (test *bpnFixture) requireInstalledBootUnit() {
	GinkgoHelper()
	for _, node := range test.nodes {
		ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNAgentReady)
		err := bpnCleanupPollWithin(ctx, func(call context.Context) error {
			values, err := bpnSystemdProperties(call, node, test.bootUnit, "LoadState", "Before")
			if err != nil {
				return err
			}
			if values["LoadState"] != "loaded" || !slices.Contains(strings.Fields(values["Before"]), "kubelet.service") {
				return fmt.Errorf("node %s must have %s loaded and ordered before kubelet", node.initial.Name, test.bootUnit)
			}

			return nil
		})
		cancel()
		Expect(err).NotTo(HaveOccurred(), "The SBR agent must install the configured boot unit on node %s", node.initial.Name)
	}
}

func bpnSystemdProperties(
	ctx context.Context, node *bpnNode, unit string, properties ...string,
) (map[string]string, error) {
	command := "systemctl show"
	for _, property := range properties {
		command += " --property=" + bpnQuote(property)
	}
	command += " -- " + bpnQuote(unit)
	output, err := node.host.Run(ctx, sbrparams.BPNHostCall, command)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(properties))
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			values[key] = value
		}
	}

	return values, nil
}

func (test *bpnFixture) readBootUnitTiming(ctx context.Context) (bpnBootUnitTiming, error) {
	const startProperty = "ExecMainStartTimestampMonotonic"
	const exitProperty = "ExecMainExitTimestampMonotonic"
	var timing bpnBootUnitTiming
	node := test.nodes[0]
	unit, err := bpnSystemdProperties(ctx, node, test.bootUnit, startProperty, exitProperty)
	if err != nil {
		return timing, err
	}
	kubelet, err := bpnSystemdProperties(ctx, node, "kubelet.service", startProperty)
	if err != nil {
		return timing, err
	}
	for _, field := range []struct {
		raw   string
		value *time.Duration
	}{
		{raw: unit[startProperty], value: &timing.Start},
		{raw: unit[exitProperty], value: &timing.Exit},
		{raw: kubelet[startProperty], value: &timing.KubeletStart},
	} {
		value, parseErr := time.ParseDuration(field.raw + "us")
		if parseErr != nil || value < 0 {
			return timing, fmt.Errorf("invalid systemd monotonic timestamp %q", field.raw)
		}
		*field.value = value
	}
	boot, err := node.host.BootID(ctx, sbrparams.BPNHostCall)
	if err != nil {
		return timing, err
	}
	if boot != node.boot {
		return timing, bpnPermanentError{fmt.Errorf("node %s rebooted during the systemd timing check", node.initial.Name)}
	}

	return timing, nil
}

func bpnCheckBootOrdering(timing bpnBootUnitTiming, timedOut bool) error {
	const timeout = sbrparams.BPNBootUnitTimeout
	if timing.Start == 0 || timing.Exit <= timing.Start || timing.KubeletStart < timing.Exit {
		return fmt.Errorf("boot service start, exit, and kubelet start must be ordered: %+v", timing)
	}
	duration := timing.Exit - timing.Start
	if duration > timeout+sbrparams.BPNPoll || timedOut && duration < timeout-sbrparams.BPNPoll {
		return fmt.Errorf("unexpected boot service runtime %s with timeout %s", duration, timeout)
	}
	if timedOut && timing.KubeletStart-timing.Exit > sbrparams.BPNBootUnitStartAllowance {
		return fmt.Errorf("kubelet took more than %s to start after expiry", sbrparams.BPNBootUnitStartAllowance)
	}

	return nil
}

func (test *bpnFixture) waitForBootUnitAndKubelet(timedOut bool) {
	GinkgoHelper()
	var timing bpnBootUnitTiming
	Eventually(func() error {
		var err error
		timing, err = test.readBootUnitTiming(test.ctx)
		if err != nil {
			return bpnRetryResult(err)
		}
		if timing.KubeletStart == 0 {
			return fmt.Errorf("kubelet has not started")
		}
		if err := bpnCheckBootOrdering(timing, timedOut); err != nil {
			return StopTrying("Unexpected boot timing").Wrap(err)
		}

		return nil
	}, sbrparams.BPNBootUnitTimeout+sbrparams.BPNBootUnitStartAllowance, sbrparams.BPNPoll).
		Should(Succeed(), "INCONCLUSIVE: systemd timestamps must establish boot service and kubelet ordering")
	AddReportEntry("bpn-boot-unit-timing", timing)
}

func (test *bpnFixture) restoreAPIWhileBootUnitRuns(api *sbrutils.BlockNetworkFault) {
	GinkgoHelper()
	Eventually(func() error {
		timing, err := test.readBootUnitTiming(test.ctx)
		if err != nil {
			return bpnRetryResult(err)
		}
		if timing.Exit != 0 || timing.KubeletStart != 0 {
			return StopTrying("INCONCLUSIVE: the boot service window closed before API restoration")
		}
		if timing.Start == 0 {
			return fmt.Errorf("the boot service has not started")
		}

		return nil
	}, sbrparams.BPNFaultReady, sbrparams.BPNPoll).Should(Succeed(), "The boot service must run before API restoration")
	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNFaultReady)
	defer cancel()
	api.SetNamespacePIDs(nil)
	Expect(api.Restore(ctx)).To(Succeed(), "Host API access must return during the boot service window")
	AddReportEntry("bpn-fault-restored", api.Evidence())
}

func bpnRejoinState(node *corev1.Node) (bool, corev1.ConditionStatus) {
	_, keyed := node.Annotations[bpnRejoinCordon]
	for _, condition := range node.Status.Conditions {
		if condition.Type == bpnRejoinPending {
			return keyed, condition.Status
		}
	}

	return keyed, ""
}

func bpnCheckClearedRejoin(node *corev1.Node) error {
	keyed, pending := bpnRejoinState(node)
	if keyed || pending != "" && pending != corev1.ConditionFalse {
		return fmt.Errorf("node %s must clear its rejoin cordon annotation and pending condition", node.Name)
	}

	return nil
}

func (test *bpnFixture) checkNoRequestsDuringRejoin(from time.Time) error {
	return test.checkOnlyExpectedSBRWithoutRestrictionChanges(from, "", true)
}

func (test *bpnFixture) checkRetainedRejoinCordon(ctx context.Context) error {
	node, err := test.getNode(ctx, 0)
	if err != nil {
		return err
	}

	return bpnCheckRejoinCordon(node)
}

func bpnCheckRejoinHistory(samples []helpers.RecorderObservation[*corev1.Node], uid types.UID,
	from, through, earliestRelease time.Time,
) error {
	var held, cleared, released bool
	for _, sample := range samples {
		if sample.UID != uid || sample.At.Before(from) || sample.At.After(through) {
			continue
		}
		keyed, pending := bpnRejoinState(sample.Object)
		cordoned := sample.Object.Spec.Unschedulable
		isPending := pending != "" && pending != corev1.ConditionFalse
		if !held || earliestRelease.IsZero() || sample.At.Before(earliestRelease) {
			if !keyed || !cordoned || pending != corev1.ConditionTrue {
				return fmt.Errorf("node %s must retain its rejoin restrictions at %s", sample.Object.Name, sample.At)
			}
			held = true

			continue
		}
		if keyed != cordoned || released && keyed || cleared && isPending {
			return fmt.Errorf("node %s has inconsistent rejoin restrictions at %s", sample.Object.Name, sample.At)
		}
		if !keyed && !cleared {
			return fmt.Errorf("node %s removed the rejoin cordon before clearing the pending condition", sample.Object.Name)
		}
		cleared = cleared || !isPending
		released = released || !keyed
	}
	if !held || !earliestRelease.IsZero() && !released {
		return fmt.Errorf("INCONCLUSIVE: rejoin restriction history is incomplete")
	}

	return nil
}

func (test *bpnFixture) waitForWorkloadSchedulingBlocked() {
	reason := "The target cordon must block scheduling of the affinity Pod"
	test.waitForAPICheck(sbrparams.BPNWorkloadReady, reason, func(ctx context.Context) error {
		pod, err := test.getWorkloadPod(ctx)
		if err != nil {
			return err
		}
		if pod.Spec.NodeName != "" {
			return bpnPermanentError{fmt.Errorf("pod %s scheduled while the node had a rejoin cordon", pod.Name)}
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse &&
				condition.Reason == "Unschedulable" && strings.Contains(condition.Message, "node(s) were unschedulable") {
				return test.checkRetainedRejoinCordon(ctx)
			}
		}

		return fmt.Errorf("pod %s has no Unschedulable scheduling result", pod.Name)
	})
}

func (test *bpnFixture) waitForWorkloadScheduledOnTarget() {
	reason := "The scheduler must assign the same Pod to the released Node"
	test.waitForAPICheck(sbrparams.BPNWorkloadReady, reason, func(ctx context.Context) error {
		pod, err := test.getWorkloadPod(ctx)
		if err != nil {
			return err
		}
		if pod.Spec.NodeName != test.nodes[0].initial.Name {
			return fmt.Errorf("pod %s is assigned to %q", pod.Name, pod.Spec.NodeName)
		}

		return nil
	})
}

func bpnCheckRejoinCordon(node *corev1.Node) error {
	keyed, pending := bpnRejoinState(node)
	if !keyed || pending != corev1.ConditionTrue || !node.Spec.Unschedulable || bpnOutOfService(node) != "" {
		return fmt.Errorf("node %s must have the rejoin key, cordon, and pending condition, with no OutOfService", node.Name)
	}

	return nil
}

func (test *bpnFixture) waitForBootUnitCordon(from time.Time) time.Time {
	GinkgoHelper()
	var observed time.Time
	reason := "The boot unit must publish its own cordon"
	test.waitForAPICheck(sbrparams.BPNPublish, reason, func(ctx context.Context) error {
		node, err := test.getNode(ctx, 0)
		if err != nil {
			return err
		}
		if err := bpnCheckRejoinCordon(node); err != nil {
			return err
		}
		for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
			if sample.UID == node.UID && !sample.At.Before(from) && bpnCheckRejoinCordon(sample.Object) == nil {
				observed = sample.At

				return test.checkNoCurrentWorkerRequests(ctx)
			}
		}

		return fmt.Errorf("node %s has no recorded rejoin cordon", node.Name)
	})

	return observed
}

func (test *bpnFixture) waitForBootUnitRemoval() error {
	return bpnCleanupPoll(sbrparams.BPNAgentReady, func(ctx context.Context) error {
		for _, node := range test.nodes {
			state, err := bpnSystemdProperties(ctx, node, test.bootUnit, "LoadState", "UnitFileState")
			if err != nil {
				return err
			}
			if state["LoadState"] != "not-found" && state["UnitFileState"] != "disabled" && state["UnitFileState"] != "masked" {
				return fmt.Errorf("node %s still has boot unit %s in state %v", node.initial.Name, test.bootUnit, state)
			}
		}

		return nil
	})
}
