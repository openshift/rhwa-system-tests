package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const bpnOwnerAnnotation = "sbr.medik8s.io/remediation-owner"

var bpnRecoveryFailed bool

var _ = Describe("SBR BlockPerNode", Serial,
	Label(labels.OperatorSBR, labels.TierResiliency, labels.ComponentRemediation,
		labels.DisruptionDestructive, labels.PlatformAny, labels.FrequencyWeekly), func() {
		for _, plan := range bpnPlans {
			It(plan.id+" "+plan.title, reportxml.ID(plan.id), FlakeAttempts(1), func() {
				if bpnRecoveryFailed {
					Skip("BLOCKED: an earlier case did not restore the cluster")
				}

				caseRun := &bpnCase{plan: plan, ctx: context.Background()}
				DeferCleanup(caseRun.cleanup)
				caseRun.step(1)
				caseRun.prepare()
				caseRun.execute()
			})
		}
	})

type bpnNode struct {
	initial         *corev1.Node
	host            *helpers.PreparedNodeAccess
	boot            string
	watchdogTimeout time.Duration
	recorder        *helpers.ObjectRecorder[*corev1.Node]
}

type bpnBootSample struct {
	At    time.Time
	Node  string
	ID    string
	Error string
}

type bpnCase struct {
	plan                  bpnPlan
	ctx                   context.Context
	nodes                 []*bpnNode
	name                  string
	config, template, nhc *unstructured.Unstructured
	namespace             *corev1.Namespace
	requests              []*unstructured.Unstructured
	crRecorder            *helpers.ObjectRecorder[*unstructured.Unstructured]
	podRecorder           *helpers.ObjectRecorder[*corev1.Pod]
	workload              *corev1.Pod
	faults                []*sbrutils.BlockNetworkFault
	journalFaults         []*sbrutils.BlockJournalFault
	networks              []sbrutils.BlockNetwork
	baseline, trigger     time.Time
	samplingCancel        context.CancelFunc
	samplingDone          chan struct{}
	mu                    sync.Mutex
	samples               []bpnBootSample
	labelsAdded           bool
	adminAdded            bool
	kubeletStopped        bool
	sysctlSaved           string
	profile               time.Duration
	cleaning              bool
	prepared              bool
	disrupted             bool
	faultNodes            map[*sbrutils.BlockNetworkFault]int
	healthFaults          map[int]sbrutils.BlockFaultEvidence
	released              map[types.UID]time.Time
	quietSince            time.Time
	lastReboot            time.Time
}

func bpnResource(kind string) dynamic.ResourceInterface {
	return APIClient.Resource(schema.GroupVersionResource{
		Group: sbrparams.CRDGroup, Version: sbrparams.CRDVersion, Resource: kind,
	}).Namespace(medik8sparams.OperatorNs)
}

func (test *bpnCase) step(number int) {
	By(test.plan.steps[number-1])
}

func (test *bpnCase) api(operation func(context.Context) error) error {
	if !test.cleaning {
		if err := test.checkNodeIdentities(); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNAPICall)
	defer cancel()

	if err := operation(ctx); err != nil {
		return err
	}

	if !test.cleaning {
		return test.checkNodeIdentities()
	}

	return nil
}

func (test *bpnCase) eventually(timeout time.Duration, reason string, operation func(context.Context) error) {
	GinkgoHelper()
	Eventually(func() error { return test.api(operation) }, timeout, sbrparams.BPNPoll).
		Should(Succeed(), reason)
}

func (test *bpnCase) prepare() {
	GinkgoHelper()
	test.prepareNodes()
	storageClass, err := sbrutils.DiscoverRBDStorageClass(test.ctx, APIClient, sbrparams.SBRRBDStorageClass)
	if err != nil {
		Skip(fmt.Sprintf("BLOCKED: RBD storage: %v", err))
	}

	test.startObservation()
	test.startBootSampling()
	test.labelsAdded = true
	for _, node := range test.nodes {
		Expect(test.api(func(ctx context.Context) error {
			current, getErr := APIClient.CoreV1Interface.Nodes().Get(ctx, node.initial.Name, metav1.GetOptions{})
			if getErr != nil {
				return getErr
			}
			if current.UID != node.initial.UID {
				return fmt.Errorf("node %s UID changed", current.Name)
			}
			if current.Labels == nil {
				current.Labels = map[string]string{}
			}
			current.Labels["tests.medik8s.io/block-per-node"] = test.name
			_, getErr = APIClient.CoreV1Interface.Nodes().Update(ctx, current, metav1.UpdateOptions{})

			return getErr
		})).To(Succeed(), "The test must label Node %s", node.initial.Name)
	}

	test.profile = sbrparams.BPNProfileDRecovery
	if test.plan.id == "RHWA-1839-17C" || test.plan.id == "RHWA-1839-17D" || test.plan.id == "RHWA-1839-17E" {
		test.profile = sbrparams.BPNProfileRRecovery
	}

	test.config = buildSBRC(test.name, map[string]interface{}{
		"nodeSelector":            map[string]interface{}{"tests.medik8s.io/block-per-node": test.name},
		"sharedStorageVolumeMode": "BlockPerNode", "sharedStorageClass": storageClass,
		"ioTimeout": "15s", "sbrTimeoutSeconds": int64(sbrparams.BPNSBRTimeoutSeconds),
		"maxConsecutiveFailures": int64(sbrparams.BPNMaxConsecutiveFailures),
		"totalOutageGracePeriod": "60s", "autonomousRebootRecoveryPeriod": test.profile.String(),
		"detectOnlyMode": "Disabled", "watchdogPath": sbrparams.AgentDefaultWatchdogPath,
	})
	expectedConfigSpec, _, err := unstructured.NestedMap(test.config.Object, "spec")
	Expect(err).NotTo(HaveOccurred())
	Expect(test.api(func(ctx context.Context) error { return APIClient.Create(ctx, test.config) })).To(Succeed(),
		"BlockPerNode configuration must accept the proposed API")
	test.config = test.readFixedSpec(test.config, expectedConfigSpec, bpnResource("storagebasedremediationconfigs"))
	test.createPolicy()
	test.eventually(sbrparams.BPNAgentReady, "Each selected Node must run an agent and reporter", test.componentsReady)
	test.requireHealth(corev1.ConditionFalse)
	if test.plan.id == "RHWA-1839-06" || strings.HasPrefix(test.plan.id, "RHWA-1839-15") {
		test.createWorkload()
	}

	test.baseline = time.Now()
	test.quietSince = test.baseline
	test.observe(sbrparams.BPNBaseline, true, true)
	test.prepared = true
	AddReportEntry("bpn-identities", map[string]interface{}{
		"case": test.plan.id, "configUID": test.config.GetUID(),
		"storageClass": storageClass, "profile": test.profile.String(), "nodes": test.nodesEvidence(),
	})
}

func (test *bpnCase) prepareNodes() {
	GinkgoHelper()
	var nodeList *corev1.NodeList
	Expect(test.api(func(ctx context.Context) error {
		var err error
		nodeList, err = APIClient.CoreV1Interface.Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/worker",
		})

		return err
	})).To(Succeed(), "The runner must list worker Nodes")
	for nodeIndex := range nodeList.Items {
		node := &nodeList.Items[nodeIndex]
		_, controlPlane := node.Labels["node-role.kubernetes.io/control-plane"]
		_, master := node.Labels["node-role.kubernetes.io/master"]
		if !controlPlane && !master && node.Status.NodeInfo.OperatingSystem == "linux" && isNodeSchedulable(node) {
			test.nodes = append(test.nodes, &bpnNode{initial: node.DeepCopy()})
		}
	}

	if len(test.nodes) < sbrparams.BPNWorkers {
		Skip(fmt.Sprintf("BLOCKED: %s needs three Ready Linux workers, found %d", test.plan.id, len(test.nodes)))
	}

	sort.Slice(test.nodes, func(left, right int) bool {
		return test.nodes[left].initial.Name < test.nodes[right].initial.Name
	})
	test.nodes = test.nodes[:sbrparams.BPNWorkers]
	test.name = "sbr-bpn-" + strings.ToLower(strings.TrimPrefix(test.plan.id, "RHWA-1839-"))
	test.checkConflicts()
	test.recordOperatorImages()
	for _, node := range test.nodes {
		var err error
		node.host, err = helpers.PrepareNodeAccess(test.ctx, APIClient, node.initial.Name,
			node.initial.UID, sbrparams.BPNObserverStart)
		if err != nil {
			Skip(fmt.Sprintf("BLOCKED: prepare SSH for Node %s: %v", node.initial.Name, err))
		}

		node.boot, err = node.host.BootID(test.ctx, sbrparams.BPNHostCall)
		Expect(err).NotTo(HaveOccurred(), "Node %s must expose its actual boot ID", node.initial.Name)
		Expect(node.boot).NotTo(BeEmpty(), "Node %s must have a nonempty host boot ID", node.initial.Name)
		Expect(node.boot).To(Equal(node.initial.Status.NodeInfo.BootID),
			"Node %s API and host boot IDs must match", node.initial.Name)
		test.checkWatchdog(node)
	}
}

func (test *bpnCase) recordOperatorImages() {
	GinkgoHelper()
	var pods *corev1.PodList
	Expect(test.api(func(ctx context.Context) error {
		var err error
		pods, err = APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).List(ctx, metav1.ListOptions{})

		return err
	})).To(Succeed(), "The runner must record installed operator image identities")
	images := make([]map[string]interface{}, 0, len(pods.Items))
	for _, pod := range pods.Items {
		containers := make([]map[string]string, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
		imageIDs := map[string]string{}
		for _, status := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
			imageIDs[status.Name] = status.ImageID
		}
		for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			containers = append(containers, map[string]string{
				"name": container.Name, "image": container.Image, "imageID": imageIDs[container.Name],
			})
		}
		images = append(images, map[string]interface{}{
			"pod": pod.Name, "uid": pod.UID, "node": pod.Spec.NodeName, "containers": containers,
		})
	}
	AddReportEntry("bpn-existing-operator-images", images)
}

func (test *bpnCase) readFixedSpec(object *unstructured.Unstructured, expected map[string]interface{},
	resource dynamic.ResourceInterface,
) *unstructured.Unstructured {
	GinkgoHelper()
	var stored *unstructured.Unstructured
	Expect(test.api(func(ctx context.Context) error {
		var err error
		stored, err = resource.Get(ctx, object.GetName(), metav1.GetOptions{})

		return err
	})).To(Succeed(), "The runner must read back %s %s", object.GetKind(), object.GetName())
	Expect(stored.GetUID()).To(Equal(object.GetUID()), "%s identity must remain unchanged", object.GetKind())
	for field, value := range expected {
		actual, found, err := unstructured.NestedFieldNoCopy(stored.Object, "spec", field)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue(), "%s must preserve spec.%s", object.GetKind(), field)
		Expect(actual).To(Equal(value), "%s must preserve the fixed spec.%s", object.GetKind(), field)
	}

	return stored
}

func (test *bpnCase) checkConflicts() {
	for _, resource := range []struct{ group, version, resource string }{
		{sbrparams.CRDGroup, sbrparams.CRDVersion, "storagebasedremediationconfigs"},
		{"self-node-remediation.medik8s.io", "v1alpha1", "selfnoderemediationconfigs"},
		{"remediation.medik8s.io", "v1alpha1", "nodehealthchecks"},
	} {
		var existing *unstructured.UnstructuredList
		Expect(test.api(func(ctx context.Context) error {
			var err error
			existing, err = APIClient.Resource(schema.GroupVersionResource{
				Group: resource.group, Version: resource.version,
				Resource: resource.resource,
			}).List(ctx, metav1.ListOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}

			return err
		})).To(Succeed(), "The runner must check existing remediation policies")
		if existing == nil {
			continue
		}
		for _, object := range existing.Items {
			// An unselected configuration can select any worker. Do not modify it to make this test eligible.
			if test.policySelectsWorkers(&object) {
				Skip(fmt.Sprintf("BLOCKED: existing %s %s can act on a selected worker", resource.resource, object.GetName()))
			}
		}
	}

	var requests *unstructured.UnstructuredList
	Expect(test.api(func(ctx context.Context) error {
		var err error
		requests, err = APIClient.Resource(schema.GroupVersionResource{
			Group: sbrparams.CRDGroup, Version: sbrparams.CRDVersion, Resource: "storagebasedremediations",
		}).List(ctx, metav1.ListOptions{})

		return err
	})).To(Succeed(), "The runner must check existing SBR requests in every namespace")
	for _, request := range requests.Items {
		for _, node := range test.nodes {
			if request.GetName() == node.initial.Name {
				Skip(fmt.Sprintf("BLOCKED: existing SBR CR %s/%s can act on selected Node %s",
					request.GetNamespace(), request.GetName(), node.initial.Name))
			}
		}
	}
}

func (test *bpnCase) checkWatchdog(node *bpnNode) {
	GinkgoHelper()
	output, err := node.host.Run(test.ctx, sbrparams.BPNHostCall,
		"sudo sh -c 'test -c /dev/watchdog && cat /sys/class/watchdog/watchdog0/timeout'")
	if err != nil {
		Skip(fmt.Sprintf("BLOCKED: Node %s needs the existing default watchdog and a readable sysfs timeout: %v",
			node.initial.Name, err))
	}

	seconds, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil || seconds <= 0 || seconds > int(sbrparams.BPNWatchdogMaxTimeout/time.Second) {
		Skip(fmt.Sprintf("BLOCKED: Node %s watchdog timeout must be greater than zero and at most 120s, found %q",
			node.initial.Name, strings.TrimSpace(output)))
	}

	node.watchdogTimeout = time.Duration(seconds) * time.Second
}

func (test *bpnCase) policySelectsWorkers(object *unstructured.Unstructured) bool {
	selector, found, _ := unstructured.NestedMap(object.Object, "spec", "selector")
	if found {
		data, _ := json.Marshal(selector)
		parsed := metav1.LabelSelector{}
		if json.Unmarshal(data, &parsed) != nil {
			return true
		}
		match, err := metav1.LabelSelectorAsSelector(&parsed)
		if err != nil {
			return true
		}
		for _, node := range test.nodes {
			if match.Matches(klabels.Set(node.initial.Labels)) {
				return true
			}
		}

		return false
	}

	match, found, err := unstructured.NestedStringMap(object.Object, "spec", "nodeSelector")
	if err != nil || !found || len(match) == 0 {
		return true
	}
	for _, node := range test.nodes {
		matches := true
		for key, value := range match {
			if node.initial.Labels[key] != value {
				matches = false
			}
		}
		if matches {
			return true
		}
	}

	return false
}

func (test *bpnCase) createPolicy() {
	test.template = buildSBRUnstructured("StorageBasedRemediationTemplate", test.name,
		map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{}}})
	Expect(test.api(func(ctx context.Context) error { return APIClient.Create(ctx, test.template) })).To(Succeed())
	test.nhc = buildNHC(test.name)
	_ = unstructured.SetNestedField(test.nhc.Object, int64(2), "spec", "minHealthy")
	_ = unstructured.SetNestedField(test.nhc.Object, "10m", "spec", "healthyDelay")
	_ = unstructured.SetNestedField(test.nhc.Object, map[string]interface{}{
		"matchLabels": map[string]interface{}{"tests.medik8s.io/block-per-node": test.name},
	}, "spec", "selector")
	_ = unstructured.SetNestedField(test.nhc.Object, []interface{}{map[string]interface{}{
		"type": sbrparams.SBRStorageUnhealthyCondition, "status": "True", "duration": "30s",
	}}, "spec", "unhealthyConditions")
	_ = unstructured.SetNestedField(test.nhc.Object, test.name, "spec", "remediationTemplate", "name")
	active := map[string]bool{
		"RHWA-1839-01": true, "RHWA-1839-02": true, "RHWA-1839-05A": true, "RHWA-1839-05B": true,
		"RHWA-1839-06": true, "RHWA-1839-09": true, "RHWA-1839-11": true,
	}
	if !active[test.plan.id] {
		_ = unstructured.SetNestedStringSlice(test.nhc.Object, []string{"sbr-block-test"}, "spec", "pauseRequests")
	}
	expectedSpec, _, err := unstructured.NestedMap(test.nhc.Object, "spec")
	Expect(err).NotTo(HaveOccurred())
	Expect(test.api(func(ctx context.Context) error { return APIClient.Create(ctx, test.nhc) })).To(Succeed(),
		"NHC must accept the fixed storage-only policy")
	test.nhc = test.readFixedSpec(test.nhc, expectedSpec, APIClient.Resource(schema.GroupVersionResource{
		Group: sbrparams.NHCAPIGroup, Version: sbrparams.NHCAPIVersion, Resource: "nodehealthchecks",
	}))
	pauseRequests, _, err := unstructured.NestedStringSlice(test.nhc.Object, "spec", "pauseRequests")
	Expect(err).NotTo(HaveOccurred())
	if active[test.plan.id] {
		Expect(pauseRequests).To(BeEmpty(), "The active NHC policy must have no pause requests")
	} else {
		Expect(pauseRequests).To(Equal([]string{"sbr-block-test"}), "The NHC policy must keep exactly the test pause request")
	}
	for _, field := range []string{"stormCooldownDuration", "escalatingRemediations", "maxUnhealthy"} {
		_, found, readErr := unstructured.NestedFieldNoCopy(test.nhc.Object, "spec", field)
		Expect(readErr).NotTo(HaveOccurred())
		Expect(found).To(BeFalse(), "The fixed NHC policy must omit spec.%s", field)
	}
}

func (test *bpnCase) componentsReady(ctx context.Context) error {
	pods, err := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).List(ctx,
		metav1.ListOptions{LabelSelector: "sbrconfig=" + test.name})
	if err != nil {
		return err
	}
	for _, node := range test.nodes {
		agent, reporter := false, false
		for _, pod := range pods.Items {
			if pod.Spec.NodeName != node.initial.Name || !bpnPodReady(&pod) {
				continue
			}
			for _, container := range pod.Spec.Containers {
				if strings.Contains(container.Name, "reporter") {
					reporter = true
				}
				if container.Name == sbrparams.AgentContainerName {
					agent = true
				}
			}
		}
		if !agent || !reporter {
			return fmt.Errorf("node %s requires Ready agent and reporter, agent=%t reporter=%t",
				node.initial.Name, agent, reporter)
		}
	}

	return nil
}

func bpnPodReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func (test *bpnCase) nodesEvidence() []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(test.nodes))
	for _, node := range test.nodes {
		result = append(result, map[string]interface{}{
			"name": node.initial.Name, "uid": node.initial.UID,
			"bootID": node.boot, "addresses": node.initial.Status.Addresses, "watchdogPath": sbrparams.AgentDefaultWatchdogPath,
			"watchdogTimeout": node.watchdogTimeout.String(),
		})
	}

	return result
}

func (test *bpnCase) getNode(ctx context.Context, index int) (*corev1.Node, error) {
	node, err := APIClient.CoreV1Interface.Nodes().Get(ctx, test.nodes[index].initial.Name, metav1.GetOptions{})
	if err == nil && node.UID != test.nodes[index].initial.UID {
		return nil, fmt.Errorf("node UID changed for %s", node.Name)
	}

	return node, err
}
