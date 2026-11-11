package tests

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (test *bpnCase) execute() {
	switch test.plan.id {
	case "RHWA-1839-01":
		test.caseHealthy()
	case "RHWA-1839-02":
		test.caseAllAPI()
	case "RHWA-1839-03A":
		test.caseAllStorage()
	case "RHWA-1839-03B":
		test.caseStorageRequest(true)
	case "RHWA-1839-04A", "RHWA-1839-04B":
		test.caseAutonomous()
	case "RHWA-1839-05A", "RHWA-1839-05B", "RHWA-1839-10A", "RHWA-1839-10B", "RHWA-1839-11":
		test.caseNetworkNegative()
	case "RHWA-1839-06":
		test.caseNHC()
	case "RHWA-1839-07":
		test.caseSysRq()
	case "RHWA-1839-08":
		test.caseAgentRestart()
	case "RHWA-1839-09":
		test.caseKubelet()
	case "RHWA-1839-10C":
		test.caseStorageRequest(false)
	case "RHWA-1839-12A", "RHWA-1839-12B":
		test.caseAcceptedRecovery()
	case "RHWA-1839-15A", "RHWA-1839-15B":
		test.caseRetainedPod()
	case "RHWA-1839-16A", "RHWA-1839-16B", "RHWA-1839-16C":
		test.caseJournal()
	case "RHWA-1839-17A":
		test.caseRepeatedRequests()
	case "RHWA-1839-17B":
		test.caseRequestedAfterAutonomous()
	case "RHWA-1839-17C", "RHWA-1839-17D", "RHWA-1839-17E":
		test.caseRecoveryPeriod()
	default:
		Fail("The registered case has no implementation: " + test.plan.id)
	}
}

func (test *bpnCase) caseHealthy() {
	test.step(2)
	test.requireHealth(corev1.ConditionFalse)
	test.finalBoots()
	test.step(3)
	test.observe(sbrparams.BPNNoReboot, true, true)
	test.step(4)
	test.finalBoots()
	test.step(5)
}

func (test *bpnCase) caseAllAPI() {
	test.step(2)
	faults := make([]*sbrutils.BlockNetworkFault, 0, 3)
	for index := range test.nodes {
		faults = append(faults, test.prepareFault(index, sbrutils.BlockAPILoss, false))
	}
	test.step(3)
	for _, fault := range faults {
		test.activate(fault)
	}
	test.step(4)
	test.step(5)
	test.observe(sbrparams.BPNNoReboot, false, true)
	test.step(6)
	test.restoreFaults()
	test.requireHealth(corev1.ConditionFalse)
	test.step(7)
	test.observe(sbrparams.BPNPostRestore, true, true)
}

func (test *bpnCase) caseAllStorage() {
	test.step(2)
	paused, _, err := unstructured.NestedStringSlice(test.nhc.Object, "spec", "pauseRequests")
	Expect(err).NotTo(HaveOccurred())
	Expect(paused).To(Equal([]string{"sbr-block-test"}))
	test.step(3)
	faults := make([]*sbrutils.BlockNetworkFault, 0, 3)
	for index := range test.nodes {
		faults = append(faults, test.prepareFault(index, sbrutils.BlockStorageLoss, false))
	}
	for _, fault := range faults {
		test.activate(fault)
	}
	test.step(4)
	test.requireUnhealthy()
	test.step(5)
	test.observe(sbrparams.BPNNoReboot, false, true)
	test.step(6)
	test.restoreFaults()
	test.requireHealth(corev1.ConditionFalse)
	test.step(7)
	test.observe(sbrparams.BPNPostRestore, true, true)
}

func (test *bpnCase) caseStorageRequest(allStorage bool) {
	test.step(2)
	kind := sbrutils.BlockStorageTimeout
	indices := []int{1, 2}
	if allStorage {
		kind = sbrutils.BlockStorageLoss
		indices = []int{0, 1, 2}
	}
	faults := make([]*sbrutils.BlockNetworkFault, 0, len(indices))
	for _, index := range indices {
		faults = append(faults, test.prepareFault(index, kind, index == 0))
	}
	for _, fault := range faults {
		test.activate(fault)
	}
	test.step(3)
	test.requireUnhealthy(indices...)
	test.step(4)
	from := time.Now()
	request := test.request()
	test.step(5)
	if allStorage {
		test.step(6)
	}
	test.requestedResult(request, test.trigger)
	if !allStorage {
		test.step(6)
	}
	for _, fault := range faults {
		test.checkFaultAfterReboot(fault)
	}
	test.peerContinuity(from, time.Now())
	Expect(test.healthHistory(corev1.ConditionTrue, from, time.Now(), 1, 2)).To(Succeed(),
		"Both peer Nodes must remain storage-unhealthy through request completion")
	test.requireHealth(corev1.ConditionTrue, 1, 2)
	test.finalBoots(1, 2)
	test.step(7)
	test.restoreFaults()
	test.requireHealth(corev1.ConditionFalse)
	test.releaseRequest(request)
	test.step(8)
}

//nolint:funlen // Keep the ordered plan steps together for both retained-history cases.
func (test *bpnCase) caseAutonomous() {
	preserve := test.plan.id == "RHWA-1839-04B"
	var retained types.UID
	if preserve {
		test.step(2)
		request := test.request()
		test.requestedResult(request, test.trigger)
		test.step(3)
		test.releaseRequest(request)
		test.step(4)
		retained = test.retainedRequest()
		Expect(retained).To(Equal(request.GetUID()))
		test.step(5)
	}
	test.autonomousPreparation()
	if preserve {
		test.step(6)
	} else {
		test.step(2)
	}
	storage := test.prepareFault(0, sbrutils.BlockStorageLoss, true)
	api := test.prepareFault(0, sbrutils.BlockAPILoss, true)
	if !preserve {
		test.step(3)
	}
	test.activate(storage)
	test.requireUnhealthy(0)
	test.finalBoots(0)
	test.activate(api)
	from := time.Now()
	if !preserve {
		test.step(4)
	}
	test.nodes[0].boot = test.waitReboot(from)
	if !preserve {
		test.step(5)
	}
	test.checkFaultAfterReboot(storage, api)
	if preserve {
		test.step(7)
	} else {
		test.step(6)
	}
	test.observe(sbrparams.BPNNoReboot, false, true)
	if preserve {
		test.step(8)
	} else {
		test.step(7)
	}
	test.restoreFaults()
	test.requireHealth(corev1.ConditionFalse)
	test.readyNodes()
	if preserve {
		Expect(test.retainedRequest()).To(Equal(retained))
		test.step(9)
	} else {
		test.step(8)
	}
	test.observe(sbrparams.BPNPostRestore, true, true)
	test.coverage(from, time.Now(), false)
	Expect(test.noRestrictions(from)).To(Succeed())
}

//nolint:funlen // These cases share one fault sequence and retain their ordered plan steps.
func (test *bpnCase) caseNetworkNegative() {
	kind := sbrutils.BlockAPILoss
	healthy := false
	group := false
	switch test.plan.id {
	case "RHWA-1839-05B":
		kind = sbrutils.BlockSingletonIsolation
	case "RHWA-1839-10A":
		kind = sbrutils.BlockStorageLatency
	case "RHWA-1839-10B":
		kind = sbrutils.BlockStorageTimeout
	case "RHWA-1839-11":
		kind = sbrutils.BlockGroupPartition
		healthy = true
		group = true
	}
	test.step(2)
	faults := []*sbrutils.BlockNetworkFault{test.prepareFault(0, kind, false)}
	if group {
		// B and C preserve their mutual traffic. Only their links to A are blocked.
		for _, index := range []int{1, 2} {
			network := test.networks[index]
			network.Peers = nil
			for _, address := range test.nodes[0].initial.Status.Addresses {
				if address.Type == corev1.NodeInternalIP || address.Type == corev1.NodeExternalIP {
					network.Peers = append(network.Peers, address.Address)
				}
			}
			test.networks[index] = network
			faults = append(faults, test.prepareFault(index, kind, false))
		}
	}
	if test.plan.id != "RHWA-1839-05A" && test.plan.id != "RHWA-1839-10B" {
		test.step(3)
	}
	for _, fault := range faults {
		test.activate(fault)
	}
	storage := kind == sbrutils.BlockStorageLatency || kind == sbrutils.BlockStorageTimeout
	switch {
	case storage:
		if test.plan.id == "RHWA-1839-10B" {
			test.step(3)
		} else {
			test.step(4)
		}
		test.requireUnhealthy(0)
		if test.plan.id == "RHWA-1839-10B" {
			test.step(4)
		} else {
			test.step(5)
		}
	case test.plan.id == "RHWA-1839-05A":
		test.step(3)
	default:
		test.step(4)
	}
	test.observe(sbrparams.BPNNoReboot, healthy, true)
	switch test.plan.id {
	case "RHWA-1839-05A":
		test.step(4)
	case "RHWA-1839-10A":
		test.step(6)
	case "RHWA-1839-10B":
		test.step(5)
	default:
		test.step(5)
	}
	test.restoreFaults()
	test.requireHealth(corev1.ConditionFalse)
	switch test.plan.id {
	case "RHWA-1839-05A":
		test.step(5)
	case "RHWA-1839-10A":
		test.step(7)
	default:
		test.step(6)
	}
	test.observe(sbrparams.BPNPostRestore, true, true)
}

func (test *bpnCase) caseNHC() {
	test.step(2)
	Expect(test.workload.UID).NotTo(BeEmpty())
	test.finalBoots()
	test.step(3)
	fault := test.prepareFault(0, sbrutils.BlockStorageLoss, false)
	test.activate(fault)
	test.step(4)
	test.requireUnhealthy(0)
	test.step(5)
	var request *unstructured.Unstructured
	test.eventually(sbrparams.BPNRequestCreate, "NHC must create the request", func(ctx context.Context) error {
		current, err := bpnResource("storagebasedremediations").Get(ctx, test.nodes[0].initial.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		owned := false
		for _, owner := range current.GetOwnerReferences() {
			if owner.UID == test.nhc.GetUID() {
				owned = true
			}
		}
		if !owned {
			return fmt.Errorf("the target request must belong to the test NHC")
		}
		request = current

		return nil
	})
	test.requests = append(test.requests, request)
	from := request.GetCreationTimestamp().Time
	if from.IsZero() {
		from = time.Now()
	}
	test.step(6)
	test.requestedResult(request, from)
	test.step(7)
	var taintObserved time.Time
	for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
		if !sample.At.Before(from) && bpnOutOfService(sample.Object) == "nodeshutdown:NoExecute" {
			taintObserved = sample.At

			break
		}
	}
	deadline := taintObserved.Add(sbrparams.BPNWorkloadGone)
	remaining := time.Until(deadline)
	checkGone := func() bool {
		return bpnPodGoneBefore(test.podRecorder.Snapshot().Observations, test.workload, taintObserved, deadline)
	}
	if remaining > 0 {
		Eventually(checkGone, remaining, sbrparams.BPNPoll).Should(BeTrue(), "Remove the Pod within WORKLOAD_GONE")
	}
	Expect(checkGone()).To(BeTrue(), "UID-scoped Pod evidence must prove removal before WORKLOAD_GONE")
	test.step(8)
	test.restoreFault(fault)
	test.requireHealth(corev1.ConditionFalse)
	healthyAt := time.Now()
	test.step(9)
	releaseBy := healthyAt.Add(sbrparams.BPNNHCHealthyDelay + sbrparams.BPNRelease)
	test.eventually(time.Until(releaseBy), "NHC must delete and release within 900s", func(ctx context.Context) error {
		_, err := test.requestCurrent(ctx, request)
		if apierrors.IsNotFound(err) {
			return test.checkRestoredRestrictions(ctx)
		}
		if err != nil {
			return err
		}

		return fmt.Errorf("NHC request remains")
	})
	test.markRequestReleased(request.GetUID())
	test.step(10)
}

func (test *bpnCase) caseSysRq() {
	test.step(2)
	test.sysctlSaved = test.runHost("cat /proc/sys/kernel/sysrq /proc/sys/kernel/panic")
	Expect(strings.Fields(test.sysctlSaved)).To(HaveLen(2))
	_, err := test.nodes[0].host.Run(test.ctx, sbrparams.BPNHostCall, "sudo test -w /proc/sysrq-trigger")
	test.prerequisite(err, "existing SysRq trigger")
	test.step(3)
	test.disrupted = true
	test.runHost("sudo sh -c 'echo 1 > /proc/sys/kernel/sysrq; sync; echo s > /proc/sysrq-trigger'")
	test.step(4)
	test.waitUntil(time.Now().Add(sbrparams.BPNSysRqSettle))
	test.runHost("sudo sysctl -w kernel.panic=180")
	test.step(5)
	test.finalBoots(0)
	from := time.Now()
	// SysRq injects a kernel panic, not a general kernel freeze.
	// A changed boot ID proves a reboot, not its mechanism.
	_, triggerErr := test.nodes[0].host.Run(test.ctx, sbrparams.BPNHostCall, "sudo sh -c 'echo c > /proc/sysrq-trigger'")
	AddReportEntry("sysrq-dispatch", fmt.Sprint(triggerErr))
	test.step(6)
	test.nodes[0].boot = test.waitReboot(from)
	test.step(7)
	values := strings.Fields(test.sysctlSaved)
	test.runHost("sudo sysctl -w kernel.sysrq=" + bpnQuote(values[0]) + " kernel.panic=" + bpnQuote(values[1]))
	test.step(8)
	test.readyNodes()
	test.requireHealth(corev1.ConditionFalse)
}

func (test *bpnCase) targetAgent(ctx context.Context) (*corev1.Pod, error) {
	pods, err := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).List(ctx,
		metav1.ListOptions{
			LabelSelector: "sbrconfig=" + test.name,
			FieldSelector: "spec.nodeName=" + test.nodes[0].initial.Name,
		})
	if err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		for _, container := range pod.Spec.Containers {
			if container.Name == sbrparams.AgentContainerName {
				return pod.DeepCopy(), nil
			}
		}
	}

	return nil, fmt.Errorf("target agent Pod is absent")
}

func (test *bpnCase) restartAgent() {
	GinkgoHelper()
	var original *corev1.Pod
	Expect(test.api(func(ctx context.Context) error {
		var err error
		original, err = test.targetAgent(ctx)

		return err
	})).To(Succeed())
	test.deleteAgent(original)
	test.waitAgentReplacement(original)
}

func (test *bpnCase) deleteAgent(original *corev1.Pod) {
	uid := original.UID
	Expect(test.api(func(ctx context.Context) error {
		return APIClient.Delete(ctx, original, client.Preconditions{UID: &uid})
	})).To(Succeed())
}

func (test *bpnCase) waitAgentReplacement(original *corev1.Pod) {
	uid := original.UID
	test.eventually(sbrparams.BPNAgentReady, "A new Ready agent must replace the Pod", func(ctx context.Context) error {
		pod, err := test.targetAgent(ctx)
		if err != nil {
			return err
		}
		if pod.UID == uid || !bpnPodReady(pod) {
			return fmt.Errorf("a different Ready agent UID is required")
		}
		old, err := APIClient.CoreV1Interface.Pods(original.Namespace).Get(ctx, original.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil && old.UID == uid {
			return fmt.Errorf("original agent Pod still exists")
		}
		node, err := test.getNode(ctx, 0)
		if err != nil {
			return err
		}
		if bpnHealth(node) != corev1.ConditionFalse {
			return fmt.Errorf("replacement must restore False storage health")
		}

		return test.checkBoots(test.baseline)
	})
	test.finalBoots()
}

func (test *bpnCase) caseAgentRestart() {
	test.step(2)
	var original *corev1.Pod
	Expect(test.api(func(ctx context.Context) error {
		var err error
		original, err = test.targetAgent(ctx)

		return err
	})).To(Succeed())
	test.finalBoots()
	test.step(3)
	test.deleteAgent(original)
	test.step(4)
	test.waitAgentReplacement(original)
	test.step(5)
	test.requireHealth(corev1.ConditionFalse)
	test.step(6)
	test.observe(sbrparams.BPNNoReboot, true, true)
	test.step(7)
}

func (test *bpnCase) caseKubelet() {
	test.step(2)
	test.kubeletStopped = true
	test.disrupted = true
	_, err := test.nodes[0].host.Run(test.ctx, sbrparams.BPNHostCall, "sudo systemctl stop kubelet")
	AddReportEntry("kubelet-stop-command", fmt.Sprint(err))
	test.step(3)
	state := test.runHost("systemctl is-active kubelet || test \"$(systemctl is-active kubelet)\" = inactive")
	Expect(state).To(ContainSubstring("inactive"))
	test.step(4)
	test.observe(sbrparams.BPNNoReboot, true, true)
	test.step(5)
	Eventually(func() error {
		_, startErr := test.nodes[0].host.Run(test.ctx, sbrparams.BPNHostCall,
			"sudo systemctl start kubelet && systemctl is-active --quiet kubelet")

		return startErr
	}, sbrparams.BPNRelease, sbrparams.BPNPoll).Should(Succeed())
	test.kubeletStopped = false
	test.step(6)
	test.readyNodes()
	test.observe(sbrparams.BPNPostRestore, true, true)
	test.step(7)
}

func (test *bpnCase) caseAcceptedRecovery() {
	storageCase := test.plan.id == "RHWA-1839-12A"
	test.step(2)
	kind := sbrutils.BlockAPILoss
	if storageCase {
		kind = sbrutils.BlockStorageLoss
	}
	fault := test.prepareFault(0, kind, false)
	if storageCase {
		test.activate(fault)
		test.requireUnhealthy(0)
	}
	test.step(3)
	request := test.request()
	test.step(4)
	accepted := test.acceptance(request)
	test.step(5)
	if storageCase {
		test.restoreFault(fault)
	} else {
		test.activate(fault)
	}
	test.step(6)
	if !storageCase {
		test.restoreFault(fault)
	}
	Expect(fault.Evidence().RestoreDispatchedAt.Before(accepted)).To(BeFalse(), "Acceptance must precede recovery")
	if !storageCase {
		test.step(7)
	}
	current, err := test.nodes[0].host.BootID(test.ctx, sbrparams.BPNHostCall)
	if storageCase {
		test.step(7)
	}
	Expect(err).NotTo(HaveOccurred(), "Row 12 requires a host read after recovery and before reboot")
	Expect(current).To(Equal(test.nodes[0].boot), "Row 12 requires the original host boot after recovery")
	Expect(test.nodes[0].recorder.Coverage(test.trigger, time.Now())).To(Succeed(), "Row 12 requires complete event order")
	test.step(8)
	test.requestedResult(request, test.trigger)
	test.step(9)
	test.releaseRequest(request)
}

func (test *bpnCase) caseRetainedPod() {
	admin := test.plan.id == "RHWA-1839-15B"
	if admin {
		test.step(2)
		test.retainPod()
		test.step(3)
		Expect(test.setAdministratorFixtures(true)).To(Succeed())
		test.step(4)
		test.eventually(sbrparams.BPNAgentReady, "Agents must tolerate existing OutOfService", test.componentsReady)
		test.finalBoots()
		test.step(5)
	} else {
		test.step(2)
		test.retainPod()
		test.step(3)
		Expect(test.api(test.heldPod)).To(Succeed())
		test.step(4)
	}
	request := test.request()
	if !admin {
		test.step(5)
	}
	test.requestedResult(request, test.trigger)
	test.step(6)
	var deadline time.Time
	Expect(test.api(func(ctx context.Context) error {
		current, err := test.requestCurrent(ctx, request)
		if err != nil {
			return err
		}
		_, deadline, err = bpnCleanupTimes(current)

		return err
	})).To(Succeed())
	test.waitUntil(deadline)
	Expect(test.api(test.heldPod)).To(Succeed(), "The same finalizer-held Pod must remain at cleanup expiry")
	if !admin {
		test.step(7)
	}
	Expect(test.api(func(ctx context.Context) error {
		current, err := test.requestCurrent(ctx, request)
		if err != nil {
			return err
		}
		phase, _, _ := unstructured.NestedString(current.Object, "status", "cleanup", "phase")
		if phase != "Incomplete" && phase != "Unknown" {
			return fmt.Errorf("retained Pod requires Incomplete or Unknown cleanup, got %q", phase)
		}
		for _, sample := range test.crRecorder.Snapshot().Observations {
			if sample.UID != request.GetUID() {
				continue
			}
			observed, _, _ := unstructured.NestedString(sample.Object.Object, "status", "cleanup", "phase")
			if observed == "Completed" {
				return fmt.Errorf("SBR reported Completed while the original Pod remained")
			}
		}

		return nil
	})).To(Succeed())
	if admin {
		test.step(7)
	} else {
		test.step(8)
	}
	test.releaseRequest(request)
	if admin {
		test.step(8)
	} else {
		test.step(9)
	}
	Expect(test.api(test.heldPod)).To(Succeed(), "The same held Pod must remain during normal CR deletion")
	test.requireRestrictionsReleased()
	if admin {
		test.step(9)
		Expect(test.setAdministratorFixtures(false)).To(Succeed())
		test.step(10)
	} else {
		test.step(10)
	}
}

//nolint:funlen // Keep each journal case's ordered plan steps in one sequence.
func (test *bpnCase) caseJournal() {
	test.step(2)
	if test.plan.id == "RHWA-1839-16C" {
		first, second := test.journal(1), test.journal(2)
		test.step(3)
		from := time.Now()
		request := test.request()
		test.step(4)
		test.requestedResult(request, test.trigger)
		test.step(5)
		for _, fault := range []*sbrutils.BlockJournalFault{first, second} {
			Expect(fault.Confirm(test.ctx)).To(Succeed())
		}
		test.peerContinuity(from, time.Now())
		test.finalBoots(1, 2)
		test.step(6)
		test.restoreJournal(first)
		test.restoreJournal(second)
		test.step(7)
		test.releaseRequest(request)

		return
	}
	journal := test.journal(0)
	test.step(3)
	if test.plan.id == "RHWA-1839-16B" {
		test.restoreJournal(journal)
		test.step(4)
		test.restartAgent()
		test.step(5)
		test.finalBoots()
		test.requireHealth(corev1.ConditionFalse)
		test.step(6)
		request := test.request()
		test.step(7)
		test.requestedResult(request, test.trigger)
		test.step(8)
		test.releaseRequest(request)

		return
	}
	Expect(journal.Confirm(test.ctx)).To(Succeed())
	test.step(4)
	request := test.request()
	test.step(5)
	test.observe(sbrparams.BPNNoReboot, true, false)
	test.step(6)
	test.finalBoots()
	test.coverage(test.trigger, time.Now(), false)
	for _, sample := range test.crRecorder.Snapshot().Observations {
		if sample.UID == request.GetUID() {
			succeeded := bpnCondition(sample.Object, "FencingSucceeded", "True") ||
				bpnCondition(sample.Object, "Succeeded", "True")
			Expect(succeeded).To(BeFalse(), "An unexecuted request must never report success")
		}
	}
	test.step(7)
	test.releaseRequest(request)
	test.step(8)
	test.step(9)
	test.restoreJournal(journal)
	test.step(10)
}

func (test *bpnCase) requireAgentRunning(inconclusive bool) {
	var last error
	timeout := sbrparams.BPNAgentReady
	if inconclusive && !test.lastReboot.IsZero() {
		timeout = time.Until(test.lastReboot.Add(sbrparams.BPNAgentReady))
	}
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	err := wait.PollUntilContextTimeout(test.ctx, sbrparams.BPNPoll, timeout, true,
		func(ctx context.Context) (bool, error) {
			last = test.api(func(call context.Context) error {
				pod, err := test.targetAgent(call)
				if err != nil {
					return err
				}

				return bpnRunningAgent(pod)
			})

			return last == nil, nil
		})
	if err != nil && inconclusive {
		csiBlocked := false
		checkErr := test.api(func(ctx context.Context) error {
			var checkErr error
			csiBlocked, checkErr = test.agentAttachmentBlocked(ctx)

			return checkErr
		})
		Expect(checkErr).To(Succeed(), "The target agent must expose its startup result through the API")
		if csiBlocked {
			Fail(fmt.Sprintf("INCONCLUSIVE: CSI attachment prevented the target agent from starting: %v", last))
		}
	}
	Expect(err).NotTo(HaveOccurred(), "The target agent must run before a valid request")
}

func (test *bpnCase) agentAttachmentBlocked(ctx context.Context) (bool, error) {
	pod, err := test.targetAgent(ctx)
	if err != nil {
		return false, err
	}
	if pod.Status.Phase != corev1.PodPending {
		return false, nil
	}
	events, err := APIClient.CoreV1Interface.Events(pod.Namespace).List(ctx,
		metav1.ListOptions{FieldSelector: "involvedObject.uid=" + string(pod.UID)})
	if err != nil {
		return false, err
	}
	for _, event := range events.Items {
		if event.Reason == "FailedAttachVolume" {
			return true, nil
		}
	}

	return false, nil
}

func (test *bpnCase) caseRepeatedRequests() {
	test.step(2)
	storage := test.prepareFault(0, sbrutils.BlockStorageLoss, true)
	test.activate(storage)
	test.requireUnhealthy(0)
	test.requireAgentRunning(false)
	test.step(3)
	first := test.request()
	test.requestedResult(first, test.trigger)
	test.checkFaultAfterReboot(storage)
	test.step(4)
	test.observe(sbrparams.BPNNoReboot, false, false)
	test.step(5)
	test.releaseRequest(first)
	test.step(6)
	second := test.request()
	Expect(second.GetUID()).NotTo(Equal(first.GetUID()))
	test.step(7)
	test.requestedResult(second, test.trigger)
	test.checkFaultAfterReboot(storage)
	test.step(8)
	test.observe(sbrparams.BPNNoReboot, false, false)
	test.step(9)
	test.restoreFault(storage)
	test.requireHealth(corev1.ConditionFalse)
	test.releaseRequest(second)
}

func (test *bpnCase) caseRequestedAfterAutonomous() {
	test.autonomousPreparation()
	test.step(2)
	storage, api, from := test.combined()
	test.nodes[0].boot = test.waitReboot(from)
	test.checkFaultAfterReboot(storage, api)
	test.step(3)
	test.observe(sbrparams.BPNNoReboot, false, true)
	test.step(4)
	test.restoreFault(api)
	test.requireHealth(corev1.ConditionTrue, 0)
	test.requireAgentRunning(true)
	test.step(5)
	request := test.request()
	test.step(6)
	test.requestedResult(request, test.trigger)
	test.checkFaultAfterReboot(storage)
	test.step(7)
	test.releaseRequest(request)
	test.step(8)
	test.activate(api)
	test.step(9)
	test.observe(sbrparams.BPNNoReboot, false, true)
	test.step(10)
	test.restoreFaults()
	test.requireHealth(corev1.ConditionFalse)
}

func (test *bpnCase) healthyUntilDeadline(start, deadline time.Time) time.Time {
	GinkgoHelper()
	var confirmed time.Time
	remaining := time.Until(deadline)
	if remaining <= 0 {
		Fail("INCONCLUSIVE: the fixed recovery window ended before health confirmation")
	}
	ctx, cancel := context.WithDeadline(test.ctx, deadline)
	defer cancel()
	err := wait.PollUntilContextTimeout(ctx, sbrparams.BPNPoll, remaining, true, func(call context.Context) (bool, error) {
		if err := test.liveAPI(call, 0); err != nil {
			return false, nil
		}
		freshFalse := false
		for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
			fresh := sample.UID == test.nodes[0].initial.UID && !sample.At.Before(start)
			if fresh && bpnHealth(sample.Object) == corev1.ConditionFalse {
				freshFalse = true
			}
		}
		if !freshFalse {
			return false, nil
		}
		confirmed = time.Now()

		return true, nil
	})
	if err != nil {
		Fail("INCONCLUSIVE: fresh healthy storage and API transport did not fit the fixed recovery window")
	}

	return confirmed
}

func (test *bpnCase) healthyContinuous(duration time.Duration) {
	from := time.Now()
	check := func() error {
		ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNAPICall)
		defer cancel()
		if err := test.liveAPI(ctx, 0); err != nil {
			return err
		}
		if err := test.healthHistory(corev1.ConditionFalse, from, time.Now(), 0); err != nil {
			return err
		}

		return test.checkBoots(from)
	}
	Consistently(check, duration, sbrparams.BPNPoll).Should(Succeed(), "Keep storage and API access healthy")
	test.coverage(from, time.Now(), false)
	Expect(check()).To(Succeed(), "Replayed events must preserve continuous healthy recovery")
	test.finalBoots()
}

func (test *bpnCase) holdHealthyUntil(from, deadline time.Time) {
	ctx, cancel := context.WithDeadline(test.ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(sbrparams.BPNPoll)
	defer ticker.Stop()
	for {
		if err := test.healthHistory(corev1.ConditionFalse, from, time.Now(), 0); err != nil {
			Fail("INCONCLUSIVE: the short recovery interval was not continuously healthy: " + err.Error())
		}
		if time.Until(deadline) <= sbrparams.BPNPoll {
			test.waitUntil(deadline)

			return
		}
		if err := test.liveAPI(ctx, 0); err != nil {
			Fail("INCONCLUSIVE: API transport did not remain reachable during the short recovery interval: " + err.Error())
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

//nolint:funlen // The fixed recovery intervals must remain visible beside their ordered plan steps.
func (test *bpnCase) caseRecoveryPeriod() {
	test.autonomousPreparation()
	test.step(2)
	storage, api, from := test.combined()
	test.nodes[0].boot = test.waitReboot(from)
	test.checkFaultAfterReboot(storage, api)
	test.step(3)
	test.observe(sbrparams.BPNNoReboot, false, true)
	test.step(4)
	recoveryStart := time.Now()
	test.restoreFault(storage)
	test.restoreFault(api)
	if test.plan.id == "RHWA-1839-17D" {
		test.requireHealth(corev1.ConditionFalse)
		test.step(5)
		test.healthyContinuous(sbrparams.BPNProfileRRecovery + sbrparams.BPNRearmSettle)
		test.step(6)
		test.activate(storage)
		test.finalBoots(0)
		test.activate(api)
		test.step(7)
		test.nodes[0].boot = test.waitReboot(time.Now())
		test.checkFaultAfterReboot(storage, api)
		test.step(8)
		test.observe(sbrparams.BPNNoReboot, false, true)
		test.step(9)
		test.restoreFaults()
		test.requireHealth(corev1.ConditionFalse)

		return
	}
	test.step(5)
	window := sbrparams.BPNBriefRecovery
	if test.plan.id == "RHWA-1839-17E" {
		window = sbrparams.BPNRecoveryPart
	}
	confirmedFirst := test.healthyUntilDeadline(recoveryStart, recoveryStart.Add(window))
	test.holdHealthyUntil(confirmedFirst, recoveryStart.Add(window))
	test.step(6)
	if test.plan.id == "RHWA-1839-17C" {
		test.activate(storage)
		test.activate(api)
		if time.Since(recoveryStart) >= sbrparams.BPNProfileRRecovery {
			Fail("INCONCLUSIVE: the possible healthy interval reached 60s")
		}
		test.step(7)
		test.observe(sbrparams.BPNNoReboot, false, true)
		test.step(8)
		test.restoreFaults()
		test.requireHealth(corev1.ConditionFalse)

		return
	}
	test.activate(api)
	if time.Since(recoveryStart) >= sbrparams.BPNProfileRRecovery {
		Fail("INCONCLUSIVE: the first possible healthy interval reached 60s")
	}
	firstEnd := api.Evidence().DispatchedAt
	test.waitUntil(api.Evidence().ActiveAt.Add(sbrparams.BPNRecoveryBreak))
	test.step(7)
	secondStart := time.Now()
	test.restoreFault(api)
	confirmedSecond := test.healthyUntilDeadline(secondStart, secondStart.Add(sbrparams.BPNRecoveryPart))
	test.holdHealthyUntil(confirmedSecond, secondStart.Add(sbrparams.BPNRecoveryPart))
	test.step(8)
	secondEnd := time.Now()
	test.activate(storage)
	test.activate(api)
	if time.Since(secondStart) >= sbrparams.BPNProfileRRecovery {
		Fail("INCONCLUSIVE: the second possible healthy interval reached 60s")
	}
	test.step(9)
	confirmedTotal := firstEnd.Sub(confirmedFirst) + secondEnd.Sub(confirmedSecond)
	if confirmedTotal <= sbrparams.BPNProfileRRecovery {
		Fail("INCONCLUSIVE: confirmed healthy time did not exceed 60s in total")
	}
	test.coverage(confirmedFirst, time.Now(), false)
	Expect(test.healthHistory(corev1.ConditionFalse, confirmedFirst, firstEnd, 0)).To(Succeed(), "First interval health")
	Expect(test.healthHistory(corev1.ConditionFalse, confirmedSecond, secondEnd, 0)).To(Succeed(), "Second interval")
	AddReportEntry("bpn-recovery-intervals", map[string]interface{}{
		"firstStart": recoveryStart, "firstConfirmed": confirmedFirst,
		"firstEnd": firstEnd, "secondStart": secondStart, "secondConfirmed": confirmedSecond,
		"secondEnd": secondEnd, "confirmedTotal": confirmedTotal.String(),
	})
	test.step(10)
	test.observe(sbrparams.BPNNoReboot, false, true)
	test.step(11)
	test.restoreFaults()
	test.requireHealth(corev1.ConditionFalse)
}

func (test *bpnCase) readyNodes() {
	test.eventually(sbrparams.BPNNodeReady, "Nodes must be Ready with matching boot IDs", func(ctx context.Context) error {
		for index, node := range test.nodes {
			current, err := test.getNode(ctx, index)
			if err != nil {
				return err
			}
			ready := false
			for _, condition := range current.Status.Conditions {
				if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
					ready = true
				}
			}
			if !ready {
				return fmt.Errorf("node %s is not Ready", node.initial.Name)
			}
			if current.Status.NodeInfo.BootID != node.boot {
				return fmt.Errorf("node %s API boot ID must match actual host boot ID", node.initial.Name)
			}
		}

		return nil
	})
}

func (test *bpnCase) peerContinuity(from, through time.Time) {
	GinkgoHelper()
	test.coverage(from, through, true)
	snapshot := test.podRecorder.Snapshot()
	for _, node := range test.nodes[1:] {
		Expect(bpnPeerAgentInterval(snapshot.Observations, test.name, node.initial.Name, from, through)).To(Succeed())
	}
}

func bpnPeerAgentInterval(observations []helpers.RecorderObservation[*corev1.Pod], configName, nodeName string,
	from, through time.Time,
) error {
	present := make(map[types.UID]*corev1.Pod)
	for _, sample := range observations {
		if sample.At.After(from) {
			continue
		}
		if sample.EventType == watch.Deleted || !bpnSelectedAgentPod(sample.Object, configName, nodeName) {
			delete(present, sample.UID)

			continue
		}
		present[sample.UID] = sample.Object
	}
	if len(present) != 1 {
		return fmt.Errorf("peer %s needs exactly one baseline agent, observed %d", nodeName, len(present))
	}
	var baseline *corev1.Pod
	for _, pod := range present {
		baseline = pod
	}
	if err := bpnRunningAgent(baseline); err != nil {
		return fmt.Errorf("peer %s needs a running baseline agent: %w", nodeName, err)
	}
	for _, sample := range observations {
		outside := !sample.At.After(from) || sample.At.After(through)
		if outside || !bpnSelectedAgentPod(sample.Object, configName, nodeName) {
			continue
		}
		if sample.UID != baseline.UID || sample.EventType == watch.Deleted {
			return fmt.Errorf("peer %s agent must retain its UID and remain present", nodeName)
		}
		if err := bpnRunningAgent(sample.Object); err != nil {
			return fmt.Errorf("peer %s agent must keep running: %w", nodeName, err)
		}
		if bpnAgentRestarts(sample.Object) != bpnAgentRestarts(baseline) {
			return fmt.Errorf("peer %s agent must not restart", nodeName)
		}
	}

	return nil
}

func bpnSelectedAgentPod(pod *corev1.Pod, configName, nodeName string) bool {
	if !bpnSelectedPod(pod, configName, nodeName) {
		return false
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == sbrparams.AgentContainerName {
			return true
		}
	}

	return false
}

func bpnAgentRestarts(pod *corev1.Pod) int32 {
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name == sbrparams.AgentContainerName {
			return container.RestartCount
		}
	}

	return -1
}

func bpnRunningAgent(pod *corev1.Pod) error {
	if pod == nil || pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("the agent Pod must be Running")
	}
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name == sbrparams.AgentContainerName && container.State.Running != nil {
			return nil
		}
	}

	return fmt.Errorf("the agent container must have a running status")
}

func bpnPodGoneBefore(observations []helpers.RecorderObservation[*corev1.Pod], original *corev1.Pod,
	from, deadline time.Time,
) bool {
	for _, sample := range observations {
		if sample.At.Before(from) || sample.At.After(deadline) {
			continue
		}
		if sample.UID == original.UID && sample.EventType == watch.Deleted {
			return true
		}
		if sample.Object != nil && sample.Object.Namespace == original.Namespace && sample.Object.Name == original.Name &&
			sample.UID != "" && sample.UID != original.UID && sample.EventType != watch.Deleted {
			return true
		}
	}

	return false
}

func (test *bpnCase) healthHistory(status corev1.ConditionStatus, from, through time.Time, indices ...int) error {
	for _, index := range indices {
		node := test.nodes[index]
		err := bpnHealthInterval(node.recorder.Snapshot().Observations, node.initial.UID, status, from, through)
		if err != nil {
			return fmt.Errorf("node %s: %w", node.initial.Name, err)
		}
	}

	return nil
}

func bpnHealthInterval(observations []helpers.RecorderObservation[*corev1.Node], uid types.UID,
	status corev1.ConditionStatus, from, through time.Time,
) error {
	var baseline *corev1.Node
	for _, sample := range observations {
		if sample.UID != uid || sample.At.After(through) {
			continue
		}
		if !sample.At.After(from) {
			baseline = sample.Object

			continue
		}
		if baseline == nil || bpnHealth(baseline) != status {
			return fmt.Errorf("the interval needs a %s storage baseline", status)
		}
		if bpnHealth(sample.Object) != status {
			return fmt.Errorf("storage health became %s at %s", bpnHealth(sample.Object), sample.At)
		}
	}
	if baseline == nil || bpnHealth(baseline) != status {
		return fmt.Errorf("the interval needs a %s storage baseline", status)
	}

	return nil
}
