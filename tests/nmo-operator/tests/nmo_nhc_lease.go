package tests

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nmov1beta1 "github.com/medik8s/node-maintenance-operator/api/v1beta1"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoparams"
)

// The NMO+NHC node lease interop test (OCP-65651) ports the Python
// test_nmo_node_lease_on_worker: while a worker is under NMO maintenance, NMO holds
// the node's lease in medik8s-leases and NHC must not start remediating the node;
// when the NodeMaintenance CR is deleted, NHC takes the lease and remediates; when
// kubelet is restarted, remediation ends and no lease remains. The negative half is
// deliberately paired with a positive control: after the hand-over, the same node
// with kubelet still down does get remediated, proving NHC would have remediated
// inside the negative window had the lease not been held. The spec requires both
// operators and skips when either is missing or when no SSH key is available
// (kubelet restart is only possible over SSH: oc debug cannot schedule on a node
// whose kubelet is down). The NHC-side helpers this spec needs are package-private
// to the NHC suite, so local mirrors live in this file.
var _ = Describe("NMO+NHC Node Lease Interop",
	Serial,
	Ordered,
	Label(
		labels.OperatorNMO,
		labels.OperatorNHC,
		labels.OperatorInterop,
		labels.TierInterop,
		labels.DisruptionDestructive,
		labels.PlatformAny,
		labels.FrequencyWeekly,
		labels.ComponentRemediation,
	),
	func() {
		var (
			targetNodeName string
			nmCRName       string
		)

		BeforeAll(func() {
			ctx := context.Background()

			By("Checking SSH access is available for kubelet stop/start")
			if !isSSHAvailable() {
				Skip("SSH not available -- the NHC lease interop test needs SSH to stop and restart kubelet on the worker")
			}

			By("Registering NMO API scheme")
			err := APIClient.AttachScheme(nmov1beta1.AddToScheme)
			Expect(err).ToNot(HaveOccurred(), "Failed to register NMO scheme")

			By("Verifying NMO deployment is Ready")
			nmoDeployment, err := deployment.Pull(
				APIClient, nmoparams.OperatorDeploymentName, medik8sparams.OperatorNs)
			Expect(err).ToNot(HaveOccurred(), "Failed to get NMO deployment")
			Expect(nmoDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
				"NMO deployment is not Ready")

			By("Verifying NHC is installed")
			if !isNHCCRDInstalled(ctx) {
				Skip(fmt.Sprintf("NodeHealthCheck CRD %s not found -- OCP-65651 requires NHC alongside NMO",
					nmoparams.NHCCRDName))
			}

			By("Verifying NHC deployment is Ready")
			nhcDeployment, err := deployment.Pull(
				APIClient, nmoparams.NHCOperatorDeploymentName, medik8sparams.OperatorNs)
			Expect(err).ToNot(HaveOccurred(), "Failed to get NHC deployment")
			Expect(nhcDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
				"NHC deployment is not Ready")

			By("Deleting every NodeHealthCheck CR so no other NHC competes for the node lease")
			deleteAllNHCs(ctx)

			By("Creating the TestRemediation fixture the NHC CR will point at")
			setupLeaseTestRemediation(ctx)
			DeferCleanup(func() { cleanupLeaseTestRemediation(ctx) })

			By("Removing leftover TestRemediation CRs from a previous interrupted run")
			preCleanLeaseTestRemediationCRs(ctx)

			By("Selecting a worker node that hosts neither the NMO controller pod nor the SSH bastion")
			targetNodeName = selectLeaseTargetWorker(ctx)
			nmCRName = nmoparams.NMNamePrefix + targetNodeName
			By(fmt.Sprintf("Selected worker node %s", targetNodeName))

			By("Pre-cleaning any stale NodeMaintenance CR targeting the selected node")
			deleteNMsForNode(ctx, targetNodeName, nmoparams.MaintenanceTimeout)
		})

		It("Verify NHC does not remediate a worker node leased by NMO and remediates it once maintenance ends",
			reportxml.ID("65651"),
			func() {
				ctx := context.Background()
				timeline := map[string]string{}

				// Registered before anything disruptive: a failure in any later step
				// must still delete the CRs and recover the node.
				DeferCleanup(func() {
					recoverLeaseTestNode(targetNodeName, nmCRName, nmoparams.NHCLeaseCRName)
				})

				By(fmt.Sprintf("Creating NodeMaintenance CR %s for node %s", nmCRName, targetNodeName))
				Expect(APIClient.Create(ctx, newNodeMaintenanceWithReason(
					nmCRName, targetNodeName, nmoparams.NHCLeaseInteropReason))).To(Succeed(),
					"Failed to create NodeMaintenance CR %s", nmCRName)

				By("Waiting for the node to fully enter maintenance")
				assertMaintenanceSucceeded(ctx, nmCRName, targetNodeName)

				By("Verifying NMO holds the node lease")
				assertMaintenanceLease(ctx, targetNodeName, true)
				createLeaseNHC(ctx)
				verifyLeaseHeldByNMODuringDisruption(ctx, targetNodeName, nmoparams.NHCLeaseCRName, timeline)
				verifyLeaseHandoverToNHC(ctx, targetNodeName, nmCRName, nmoparams.NHCLeaseCRName, timeline)
				verifyLeaseReleaseAndRecovery(ctx, targetNodeName, nmoparams.NHCLeaseCRName, timeline)
				AddReportEntry("lease-interop-timeline", timeline)
			})
	})

// nhcGVK, nhcListGVK and leaseTRListGVK identify unstructured objects this spec reads:
// the NMO suite has no typed NHC or TestRemediation APIs.
var (
	nhcGVK = schema.GroupVersionKind{
		Group: nmoparams.NHCGroup, Version: nmoparams.NHCVersion, Kind: nmoparams.NHCKind,
	}
	nhcListGVK = schema.GroupVersionKind{
		Group: nmoparams.NHCGroup, Version: nmoparams.NHCVersion, Kind: nmoparams.NHCKind + "List",
	}
	leaseTRListGVK = schema.GroupVersionKind{
		Group: nmoparams.TestRemediationGroup, Version: nmoparams.TestRemediationVersion, Kind: "TestRemediationList",
	}
)

// isNHCCRDInstalled reports whether NHC is installed by checking for its CRD. It
// fails closed on unexpected errors: an API failure must not be mistaken for "NHC
// absent" and silently skip the spec.
func isNHCCRDInstalled(ctx context.Context) bool {
	GinkgoHelper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := APIClient.Get(ctx, client.ObjectKey{Name: nmoparams.NHCCRDName}, crd)
	if err == nil {
		return true
	}

	if k8serrors.IsNotFound(err) {
		return false
	}

	Fail(fmt.Sprintf("checking NHC CRD %s: %v", nmoparams.NHCCRDName, err))

	return false
}

// deleteAllNHCs deletes every NodeHealthCheck CR in the cluster and waits until each
// is gone, mirroring the Python setup_env_for_nhc_tests: any live NHC selecting
// workers would compete for the node lease once NMO releases it.
func deleteAllNHCs(ctx context.Context) {
	GinkgoHelper()
	nhcList := &unstructured.UnstructuredList{}
	nhcList.SetGroupVersionKind(nhcListGVK)
	Expect(APIClient.List(ctx, nhcList)).To(Succeed(), "Failed to list NodeHealthCheck CRs")
	if len(nhcList.Items) == 0 {
		GinkgoWriter.Printf("No NodeHealthCheck CRs present\n")

		return
	}

	for i := range nhcList.Items {
		name := nhcList.Items[i].GetName()
		By(fmt.Sprintf("Deleting NodeHealthCheck CR %s", name))
		deleteObjectBestEffort(ctx, &nhcList.Items[i], "NodeHealthCheck "+name)
	}
}

// selectLeaseTargetWorker returns the name of a random Ready, schedulable,
// non-control-plane worker node, excluding the node hosting the NMO controller pod
// and the nodes hosting the SSH bastion. The maintenance drain evicts everything from
// the target -- including the NMO controller, which would then reschedule under a new
// name, and on AWS the ssh-bastion pod that recovery SSH depends on. The Python
// reference picked any worker-label node without these filters.
func selectLeaseTargetWorker(ctx context.Context) string {
	GinkgoHelper()
	_, nmoPodNodeName := nmoControllerPod()
	bastionNodes, err := helpers.SSHBastionNodes(ctx, APIClient)
	Expect(err).ToNot(HaveOccurred(), "Failed to list SSH bastion nodes")
	excluded := append([]string{nmoPodNodeName}, bastionNodes...)
	eligible, err := listSchedulableWorkers(ctx)
	Expect(err).ToNot(HaveOccurred(), "Failed to list schedulable worker nodes")
	candidates := make([]corev1.Node, 0, len(eligible))
	for _, node := range eligible {
		if !containsName(excluded, node.Name) {
			candidates = append(candidates, node)
		}
	}

	if len(candidates) < nmoparams.NHCLeaseMinWorkers {
		Skip(fmt.Sprintf(
			"The NHC lease interop test requires at least %d schedulable non-control-plane "+
				"worker nodes excluding %v (the NMO controller and SSH bastion nodes), found %d",
			nmoparams.NHCLeaseMinWorkers, excluded, len(candidates)))
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })

	return candidates[rand.Intn(len(candidates))].Name
}

// containsName reports whether names contains name.
func containsName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}

	return false
}

// createLeaseNHC creates the test's NodeHealthCheck CR and waits until it is Enabled,
// proving NHC observes the workers as healthy before the disruption starts.
func createLeaseNHC(ctx context.Context) {
	GinkgoHelper()
	By(fmt.Sprintf("Creating NodeHealthCheck CR %s pointing at the TestRemediation template",
		nmoparams.NHCLeaseCRName))
	Expect(APIClient.Create(ctx, buildLeaseNHC(nmoparams.NHCLeaseCRName))).To(Succeed(),
		"Failed to create NodeHealthCheck CR %s", nmoparams.NHCLeaseCRName)

	By("Waiting for NHC to become Enabled before the disruption")
	waitForNHCPhaseValue(ctx, nmoparams.NHCLeaseCRName, nmoparams.NHCPhaseEnabled, nmoparams.NHCEnabledTimeout)
}

// buildLeaseNHC builds the NodeHealthCheck CR for the lease interop test: worker
// selector, minHealthy 30%, 30s unhealthy conditions and the TestRemediation dummy
// template. It mirrors the Python reference's nhc-node-lease-test CR (node-health-
// checks.yaml with the remediationTemplate overridden to the test fixture) and the
// NHC suite's buildNHCWithTestRemediation.
func buildLeaseNHC(name string) *unstructured.Unstructured {
	nhc := &unstructured.Unstructured{}
	nhc.SetGroupVersionKind(nhcGVK)
	nhc.SetName(name)
	nhc.Object["spec"] = map[string]interface{}{
		"minHealthy": nmoparams.NHCMinHealthy,
		"remediationTemplate": map[string]interface{}{
			"apiVersion": nmoparams.TestRemediationGroup + "/" + nmoparams.TestRemediationVersion,
			"kind":       "TestRemediationTemplate",
			"name":       nmoparams.TestRemediationTemplateName,
		},
		"selector": map[string]interface{}{
			"matchExpressions": []interface{}{
				map[string]interface{}{
					"key": "node-role.kubernetes.io/worker", "operator": "Exists",
				},
			},
		},
		"unhealthyConditions": []interface{}{
			map[string]interface{}{
				"type": "Ready", "status": "False", "duration": nmoparams.NHCUnhealthyDuration,
			},
			map[string]interface{}{
				"type": "Ready", "status": "Unknown", "duration": nmoparams.NHCUnhealthyDuration,
			},
		},
	}

	return nhc
}

// verifyLeaseHeldByNMODuringDisruption stops kubelet on the target node, waits for
// the node to become NotReady, and then verifies -- for the whole remediation window
// -- the three invariants of the NMO-held lease: NHC stays Enabled, the node lease's
// holder stays node-maintenance, and no TestRemediation CR is created. This is the
// spec's negative half; the hand-over that follows is its positive control.
func verifyLeaseHeldByNMODuringDisruption(
	ctx context.Context, nodeName, nhcName string, timeline map[string]string,
) {
	GinkgoHelper()
	By(fmt.Sprintf("Stopping kubelet on node %s via SSH", nodeName))
	Expect(helpers.StopKubeletSSH(ctx, APIClient, nodeName, nmoparams.SSHTimeout)).To(Succeed(),
		"Failed to stop kubelet on node %s", nodeName)

	By("Waiting for the node Ready condition to leave True")
	Eventually(func() (corev1.ConditionStatus, error) {
		node, err := getTargetNode(ctx, nodeName)
		if err != nil {
			return "", err
		}

		return nodeReadyConditionStatus(node)
	}, nmoparams.NodeNotReadyTimeout, nmoparams.DefaultPollInterval).Should(Not(Equal(corev1.ConditionTrue)),
		"Node %s Ready condition should leave True after kubelet is stopped", nodeName)
	timeline["notReadyObserved"] = time.Now().UTC().Format(time.RFC3339)

	By(fmt.Sprintf("Verifying NHC stays Enabled and does not take the lease for %s while NMO holds it",
		nmoparams.NHCLeaseRemediationWindow))
	Consistently(func() (bool, error) {
		held, err := leaseWindowHolds(ctx, nodeName, nhcName)
		if err != nil {
			// A transient API error must not count as a lease violation: retry the
			// poll. A persistent error still fails once the window ends.
			return false, TryAgainAfter(nmoparams.DefaultPollInterval).Wrap(err)
		}

		return held, nil
	}, nmoparams.NHCLeaseRemediationWindow, nmoparams.DefaultPollInterval).Should(BeTrue(),
		"NHC must stay Enabled, the lease holder must stay %s and no TestRemediation CR "+
			"may exist while NMO maintains node %s",
		nmoparams.LeaseHolderIdentity, nodeName)
	timeline["negativeWindowEnd"] = time.Now().UTC().Format(time.RFC3339)
}

// leaseWindowHolds checks the three invariants of the NMO-held lease window: NHC
// stays in the Enabled phase, the node lease is still held by NMO, and no
// TestRemediation CR exists. Invariant violations are reported as (false, nil) with
// a log line naming the broken invariant; API failures are returned as errors so the
// caller can retry them without masking a violation.
func leaseWindowHolds(ctx context.Context, nodeName, nhcName string) (bool, error) {
	phase, err := getNHCPhaseValue(ctx, nhcName)
	if err != nil {
		return false, err
	}

	if phase != nmoparams.NHCPhaseEnabled {
		GinkgoWriter.Printf("Lease window violated: NHC %s phase is %q, want %q\n",
			nhcName, phase, nmoparams.NHCPhaseEnabled)

		return false, nil
	}

	lease := &coordinationv1.Lease{}
	if err := APIClient.Get(ctx, client.ObjectKey{
		Namespace: nmoparams.LeaseNamespace,
		Name:      nodeLeaseName(nodeName),
	}, lease); err != nil {
		return false, fmt.Errorf("getting lease %s/%s: %w",
			nmoparams.LeaseNamespace, nodeLeaseName(nodeName), err)
	}

	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != nmoparams.LeaseHolderIdentity {
		holder := "<unset>"
		if lease.Spec.HolderIdentity != nil {
			holder = *lease.Spec.HolderIdentity
		}

		GinkgoWriter.Printf("Lease window violated: lease %s holder is %q, want %q\n",
			nodeLeaseName(nodeName), holder, nmoparams.LeaseHolderIdentity)

		return false, nil
	}

	trList := &unstructured.UnstructuredList{}
	trList.SetGroupVersionKind(leaseTRListGVK)
	if err := APIClient.List(ctx, trList); err != nil {
		return false, fmt.Errorf("listing TestRemediation CRs: %w", err)
	}

	if len(trList.Items) > 0 {
		GinkgoWriter.Printf("Lease window violated: %d TestRemediation CR(s) exist, first: %s\n",
			len(trList.Items), trList.Items[0].GetName())

		return false, nil
	}

	return true, nil
}

// verifyLeaseHandoverToNHC deletes the NodeMaintenance CR -- releasing the lease with
// kubelet still down -- and verifies NHC takes over: it starts remediating within the
// same window used for the negative half and becomes the lease holder. Same node,
// same kubelet-down state: this is the positive control proving the negative window
// was not vacuous.
func verifyLeaseHandoverToNHC(
	ctx context.Context, nodeName, nmCRName, nhcName string, timeline map[string]string,
) {
	GinkgoHelper()
	By(fmt.Sprintf("Deleting NodeMaintenance CR %s and waiting for removal", nmCRName))
	deleteAndWaitForNMCR(ctx, nmCRName, nmoparams.MaintenanceTimeout)
	timeline["nmDeleted"] = time.Now().UTC().Format(time.RFC3339)

	By("Verifying NHC started remediating the node")
	waitForNHCPhaseValue(ctx, nhcName, nmoparams.NHCPhaseRemediating, nmoparams.NHCLeaseRemediationWindow)
	timeline["remediatingObserved"] = time.Now().UTC().Format(time.RFC3339)

	By("Verifying NHC holds the node lease")
	waitForNHCLeaseHolder(ctx, nodeName, nhcName)
}

// verifyLeaseReleaseAndRecovery ends the remediation the way the Python reference
// does -- by restarting kubelet, since the TestRemediation Wait strategy never
// finishes on its own -- and verifies NHC returns to Enabled, the node's lease is
// released, and no lease remains in medik8s-leases at all.
func verifyLeaseReleaseAndRecovery(
	ctx context.Context, nodeName, nhcName string, timeline map[string]string,
) {
	GinkgoHelper()
	By(fmt.Sprintf("Starting kubelet on node %s via SSH", nodeName))
	Expect(helpers.StartKubeletSSH(ctx, APIClient, nodeName, nmoparams.SSHTimeout)).To(Succeed(),
		"Failed to start kubelet on node %s", nodeName)

	By("Waiting for the node to become Ready again")
	Expect(helpers.WaitForNodeReady(
		ctx, APIClient, nodeName,
		nmoparams.DefaultPollInterval, nmoparams.RebootTimeout,
		GinkgoWriter.Printf,
	)).To(Succeed(), "Node %s did not become Ready after kubelet restart", nodeName)
	timeline["nodeReadyAgain"] = time.Now().UTC().Format(time.RFC3339)

	By("Verifying NHC finished remediating and is Enabled again")
	waitForNHCPhaseValue(ctx, nhcName, nmoparams.NHCPhaseEnabled, nmoparams.NHCEnabledTimeout)

	By("Verifying the node lease was released")
	assertMaintenanceLease(ctx, nodeName, false)

	By(fmt.Sprintf("Verifying no lease remains in namespace %s", nmoparams.LeaseNamespace))
	Eventually(func() (int, error) {
		leaseList := &coordinationv1.LeaseList{}
		if err := APIClient.List(ctx, leaseList, client.InNamespace(nmoparams.LeaseNamespace)); err != nil {
			return 0, fmt.Errorf("listing leases in %s: %w", nmoparams.LeaseNamespace, err)
		}

		return len(leaseList.Items), nil
	}, nmoparams.LeaseTimeout, nmoparams.DefaultPollInterval).Should(BeZero(),
		"No lease should remain in namespace %s after remediation finished", nmoparams.LeaseNamespace)
}

// nodeLeaseName returns the name of a node's lease in medik8s-leases.
func nodeLeaseName(nodeName string) string {
	return "node-" + nodeName
}

// getNHCPhaseValue reads the current status.phase of the named NodeHealthCheck CR,
// fetching it fresh on every call.
func getNHCPhaseValue(ctx context.Context, name string) (string, error) {
	nhc := &unstructured.Unstructured{}
	nhc.SetGroupVersionKind(nhcGVK)
	if err := APIClient.Get(ctx, client.ObjectKey{Name: name}, nhc); err != nil {
		return "", fmt.Errorf("getting NodeHealthCheck %s: %w", name, err)
	}

	phase, found, err := unstructured.NestedString(nhc.Object, "status", "phase")
	if err != nil {
		return "", fmt.Errorf("reading status.phase of NodeHealthCheck %s: %w", name, err)
	}

	if !found {
		return "", fmt.Errorf("NodeHealthCheck %s has no status.phase", name)
	}

	return phase, nil
}

// waitForNHCPhaseValue blocks until the named NodeHealthCheck reaches the expected
// phase.
func waitForNHCPhaseValue(ctx context.Context, name, expectedPhase string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func() (string, error) {
		return getNHCPhaseValue(ctx, name)
	}, timeout, nmoparams.DefaultPollInterval).Should(Equal(expectedPhase),
		"NodeHealthCheck %s did not reach phase %s", name, expectedPhase)
}

// waitForNHCLeaseHolder waits until the node's lease is held by the NHC CR. The
// holder identity is matched by substring, as in the Python reference; the observed
// value is logged on every poll so the first live run can confirm the exact format
// before this is tightened to an equality check.
func waitForNHCLeaseHolder(ctx context.Context, nodeName, nhcName string) {
	GinkgoHelper()
	expected := nmoparams.NHCLeaseHolderPrefix + nhcName
	Eventually(func() (string, error) {
		lease := &coordinationv1.Lease{}
		if err := APIClient.Get(ctx, client.ObjectKey{
			Namespace: nmoparams.LeaseNamespace,
			Name:      nodeLeaseName(nodeName),
		}, lease); err != nil {
			return "", fmt.Errorf("getting lease %s/%s: %w",
				nmoparams.LeaseNamespace, nodeLeaseName(nodeName), err)
		}

		if lease.Spec.HolderIdentity == nil {
			return "", nil
		}

		GinkgoWriter.Printf("Lease %s holder identity: %q\n", nodeLeaseName(nodeName), *lease.Spec.HolderIdentity)

		return *lease.Spec.HolderIdentity, nil
	}, nmoparams.LeaseTimeout, nmoparams.DefaultPollInterval).Should(ContainSubstring(expected),
		"Node %s lease should be held by NHC %s once maintenance ended", nodeName, nhcName)
}

// recoverLeaseTestNode restores the cluster after the lease interop scenario. The
// NHC CR is deleted first -- it is the component that could act on the released
// lease -- then the NodeMaintenance CR (NMO's stopMaintenance uncordons and untaints
// through the API and needs no running kubelet), then kubelet is restarted over SSH
// only when the node is actually NotReady (a start on a running kubelet is a no-op,
// but flaky SSH against a healthy node would burn the full retry budget). Every step
// is best-effort: a failure logs a warning and the remaining steps still run, so one
// failing step can never leave a worker NotReady for the rest of the suite.
func recoverLeaseTestNode(nodeName, nmName, nhcName string) {
	cleanupCtx := context.Background()

	By(fmt.Sprintf("Cleanup: deleting NodeHealthCheck CR %s if it still exists", nhcName))
	nhc := &unstructured.Unstructured{}
	nhc.SetGroupVersionKind(nhcGVK)
	nhc.SetName(nhcName)
	deleteObjectBestEffort(cleanupCtx, nhc, "NodeHealthCheck "+nhcName)

	By(fmt.Sprintf("Cleanup: deleting NodeMaintenance CR %s if it still exists", nmName))
	deleteNMBestEffort(cleanupCtx, nmName)

	By(fmt.Sprintf("Cleanup: starting kubelet on node %s if it is NotReady", nodeName))
	node := &corev1.Node{}
	getErr := APIClient.Get(cleanupCtx, client.ObjectKey{Name: nodeName}, node)
	if getErr != nil || !helpers.IsNodeReady(node) {
		// On a read failure the node state is unknown, so attempt the restart anyway:
		// starting an already-running kubelet is a no-op, a missed restart is not.
		if sshErr := helpers.StartKubeletSSHWithRetry(
			cleanupCtx, APIClient, nodeName, nmoparams.RebootTimeout, GinkgoWriter.Printf,
		); sshErr != nil {
			GinkgoWriter.Printf("WARNING: failed to restart kubelet on %s: %v\n", nodeName, sshErr)
			AddReportEntry("lease-interop-kubelet-restart-failed",
				fmt.Sprintf("node %s: %v", nodeName, sshErr))
		}
	}

	By(fmt.Sprintf("Cleanup: waiting for node %s to become Ready, uncordoned and untainted", nodeName))
	waitForNodeRecoveryBestEffort(cleanupCtx, nodeName)
}

// setupLeaseTestRemediation creates the TestRemediation dummy-remediator fixture the
// NHC CR points at: the two cluster-scoped CRDs, the TestRemediationTemplate CR, and
// the RBAC that lets the NHC controller manage TestRemediation CRs. It mirrors
// setupTestRemediationResources from the NHC suite (nhc_helpers.go), which is
// package-private there; the constant values mirror nhcparams.
func setupLeaseTestRemediation(ctx context.Context) {
	GinkgoHelper()
	By("Creating TestRemediationTemplate CRD")
	trtCRD := buildLeaseTestCRD(nmoparams.TestRemediationTemplateCRDName,
		"TestRemediationTemplate", "testremediationtemplates", "testremediationtemplate", "trt")
	if err := APIClient.Create(ctx, trtCRD); err != nil && !k8serrors.IsAlreadyExists(err) {
		Fail(fmt.Sprintf("Failed to create TestRemediationTemplate CRD: %v", err))
	}

	By("Creating TestRemediation CRD")
	trCRD := buildLeaseTestCRD(nmoparams.TestRemediationCRDName,
		"TestRemediation", "testremediations", "testremediation", "tr")
	if err := APIClient.Create(ctx, trCRD); err != nil && !k8serrors.IsAlreadyExists(err) {
		Fail(fmt.Sprintf("Failed to create TestRemediation CRD: %v", err))
	}

	By("Waiting for both CRDs to become Established")
	waitForLeaseCRDEstablished(ctx, nmoparams.TestRemediationTemplateCRDName)
	waitForLeaseCRDEstablished(ctx, nmoparams.TestRemediationCRDName)

	By("Creating TestRemediationTemplate CR")
	trtCR := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": nmoparams.TestRemediationGroup + "/" + nmoparams.TestRemediationVersion,
			"kind":       "TestRemediationTemplate",
			"metadata": map[string]interface{}{
				"name": nmoparams.TestRemediationTemplateName,
			},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"strategy": map[string]interface{}{
							"retryLimit": nmoparams.TestRemediationRetryLimit,
							"timeout":    nmoparams.TestRemediationStrategyTimeout,
							"type":       nmoparams.TestRemediationStrategyType,
						},
					},
				},
			},
		},
	}
	if err := APIClient.Create(ctx, trtCR); err != nil && !k8serrors.IsAlreadyExists(err) {
		Fail(fmt.Sprintf("Failed to create TestRemediationTemplate CR: %v", err))
	}

	By("Creating RBAC for the NHC controller to manage TestRemediation CRs")
	clusterRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: nmoparams.TestRemediationClusterRoleName},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{nmoparams.TestRemediationGroup},
			Resources: []string{"testremediations", "testremediations/status", "testremediationtemplates"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}},
	}
	if err := APIClient.Create(ctx, clusterRole); err != nil && !k8serrors.IsAlreadyExists(err) {
		Fail(fmt.Sprintf("Failed to create ClusterRole: %v", err))
	}

	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: nmoparams.TestRemediationClusterRoleBindingName},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      nmoparams.NHCControllerServiceAccount,
			Namespace: medik8sparams.OperatorNs,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     nmoparams.TestRemediationClusterRoleName,
		},
	}
	if err := APIClient.Create(ctx, clusterRoleBinding); err != nil && !k8serrors.IsAlreadyExists(err) {
		Fail(fmt.Sprintf("Failed to create ClusterRoleBinding: %v", err))
	}
}

// buildLeaseTestCRD creates a cluster-scoped CRD with spec/status preserve-unknown
// schema, the shape the NHC suite uses for the TestRemediation dummy CRDs.
func buildLeaseTestCRD(
	name, kind, plural, singular, shortName string,
) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: nmoparams.TestRemediationGroup,
			Scope: apiextensionsv1.ClusterScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:       kind,
				Plural:     plural,
				Singular:   singular,
				ShortNames: []string{shortName},
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    nmoparams.TestRemediationVersion,
				Served:  true,
				Storage: true,
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec":   {Type: "object", XPreserveUnknownFields: ptr.To(true)},
							"status": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
						},
					},
				},
			}},
		},
	}
}

// waitForLeaseCRDEstablished blocks until the CRD reports its Established condition.
func waitForLeaseCRDEstablished(ctx context.Context, crdName string) {
	GinkgoHelper()
	Eventually(func() (bool, error) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := APIClient.Get(ctx, client.ObjectKey{Name: crdName}, crd); err != nil {
			return false, fmt.Errorf("getting CRD %s: %w", crdName, err)
		}

		for i := range crd.Status.Conditions {
			if crd.Status.Conditions[i].Type == apiextensionsv1.Established {
				return crd.Status.Conditions[i].Status == apiextensionsv1.ConditionTrue, nil
			}
		}

		return false, nil
	}, nmoparams.TestRemediationCRDEstablishedTimeout, nmoparams.DefaultPollInterval).Should(BeTrue(),
		"CRD %s did not become Established", crdName)
}

// preCleanLeaseTestRemediationCRs deletes TestRemediation CRs left behind by a
// previous interrupted run. A stale CR would false-fail the negative window's
// "no TestRemediation CR exists" invariant.
func preCleanLeaseTestRemediationCRs(ctx context.Context) {
	GinkgoHelper()
	trList := &unstructured.UnstructuredList{}
	trList.SetGroupVersionKind(leaseTRListGVK)
	Expect(APIClient.List(ctx, trList)).To(Succeed(), "Failed to list TestRemediation CRs")
	for i := range trList.Items {
		name := trList.Items[i].GetName()
		By(fmt.Sprintf("Deleting leftover TestRemediation CR %s", name))
		deleteObjectBestEffort(ctx, &trList.Items[i], "TestRemediation "+name)
	}
}

// cleanupLeaseTestRemediation removes the TestRemediation fixture: the template CR,
// the RBAC and both CRDs, whose deletion garbage-collects any TestRemediation CRs
// left behind. CRs and RBAC go first, while the CRDs still exist. Every step is
// cleanup-safe and mirrors cleanupTestRemediationResources from the NHC suite.
func cleanupLeaseTestRemediation(ctx context.Context) {
	trtCR := newLeaseUnstructured("TestRemediationTemplate", nmoparams.TestRemediationTemplateName)
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: nmoparams.TestRemediationClusterRoleBindingName},
	}
	clusterRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: nmoparams.TestRemediationClusterRoleName},
	}
	trCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: nmoparams.TestRemediationCRDName},
	}
	trtCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: nmoparams.TestRemediationTemplateCRDName},
	}
	deleteLeaseFixtureObject(ctx, trtCR, "TestRemediationTemplate CR")
	deleteLeaseFixtureObject(ctx, crb, "ClusterRoleBinding "+nmoparams.TestRemediationClusterRoleBindingName)
	deleteLeaseFixtureObject(ctx, clusterRole, "ClusterRole "+nmoparams.TestRemediationClusterRoleName)
	deleteLeaseFixtureObject(ctx, trCRD, "TestRemediation CRD")
	deleteLeaseFixtureObject(ctx, trtCRD, "TestRemediationTemplate CRD")
}

// newLeaseUnstructured builds a minimal cluster-scoped unstructured TestRemediation
// object carrying only the GVK and name, enough for deletion.
func newLeaseUnstructured(kind, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   nmoparams.TestRemediationGroup,
		Version: nmoparams.TestRemediationVersion,
		Kind:    kind,
	})
	obj.SetName(name)

	return obj
}

// deleteLeaseFixtureObject deletes the object and waits until it is gone, retrying
// through transient errors. Cleanup-safe: PollUntilContextTimeout with warning
// logging, never Fail. The budget covers CRD deletion, which waits on
// finalizer-driven CR garbage collection.
func deleteLeaseFixtureObject(ctx context.Context, obj client.Object, description string) {
	waitErr := wait.PollUntilContextTimeout(
		ctx, nmoparams.DefaultPollInterval, nmoparams.TestRemediationFixtureDeletionTimeout, true,
		func(ctx context.Context) (bool, error) {
			if err := APIClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				if k8serrors.IsNotFound(err) {
					return true, nil
				}

				return false, nil
			}

			if delErr := APIClient.Delete(ctx, obj); delErr != nil && !k8serrors.IsNotFound(delErr) {
				return false, nil
			}

			return false, nil
		})
	if waitErr != nil {
		GinkgoWriter.Printf("WARNING: %s not fully deleted within %s: %v\n",
			description, nmoparams.TestRemediationFixtureDeletionTimeout, waitErr)
	}
}
