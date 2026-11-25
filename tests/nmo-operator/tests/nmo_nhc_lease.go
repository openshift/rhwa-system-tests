package tests

import (
	"context"
	"fmt"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// The NMO+NHC node lease interop test (OCP-65651, RHWA-1255) ports the Python
// test_nmo_node_lease_on_worker: while a worker is under NMO maintenance, NMO holds
// the node's lease in medik8s-leases and NHC must not start remediating the node;
// when the NodeMaintenance CR is deleted, NHC takes the lease and remediates; when
// kubelet is restarted, remediation ends and the node's lease is released. The
// negative half is deliberately paired with a positive control: after the
// hand-over, the same node with kubelet still down does get remediated, proving
// the node was remediable once maintenance ended. Maintenance blocks remediation
// both through the lease and through the exclude-from-remediation label NMO sets
// on the node; both are removed at hand-over, so the window shows NMO maintenance
// as a whole blocking remediation, with the lease as the mechanism under test.
// The spec requires both operators and skips when either is missing or when no
// SSH key is available (kubelet restart is only possible over SSH: oc debug
// cannot schedule on a node whose kubelet is down). All skip guards run before
// any cluster state is mutated. The NHC-side helpers this spec needs are
// package-private to the NHC suite, so local mirrors live in this file.
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
			kubeletStopped bool
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
			if !isCRDInstalled(ctx, nmoparams.NHCCRDName) {
				Skip(fmt.Sprintf("NodeHealthCheck CRD %s not found -- OCP-65651 requires NHC alongside NMO",
					nmoparams.NHCCRDName))
			}

			By("Verifying NHC deployment is Ready")
			nhcDeployment, err := deployment.Pull(
				APIClient, nmoparams.NHCOperatorDeploymentName, medik8sparams.OperatorNs)
			Expect(err).ToNot(HaveOccurred(), "Failed to get NHC deployment")
			Expect(nhcDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
				"NHC deployment is not Ready")

			// Every skip guard has run: from here on the spec mutates cluster
			// state, so a skipped run must leave the cluster untouched.
			By("Checking enough eligible worker nodes are available")
			requireLeaseEligibleWorkers(ctx)

			By("Snapshotting pre-existing NodeHealthCheck CRs")
			preservedNHCs := listAllNHCs(ctx)

			By("Deleting every NodeHealthCheck CR so no other NHC competes for the node lease")
			deleteAllNHCs(ctx, preservedNHCs)

			DeferCleanup(func() {
				restorePreservedNHCs(preservedNHCs)
			})

			By("Creating the TestRemediation fixture the NHC CR will point at")
			// Cleanup is registered before setup (it is idempotent and only
			// touches objects this run created), so a failure part-way through
			// setup cannot leak the resources created so far.
			var createdFixture []leaseFixtureObject
			DeferCleanup(func() {
				cleanupLeaseTestRemediation(ctx, createdFixture)
			})
			createdFixture = setupLeaseTestRemediation(ctx)

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
				// must still delete the CRs and recover the node. kubeletStopped is
				// read through the closure, so cleanup sees the latest value.
				DeferCleanup(func() {
					recoverLeaseTestNode(targetNodeName, nmCRName, nmoparams.NHCLeaseCRName, kubeletStopped)
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
				verifyLeaseHeldByNMODuringDisruption(
					ctx, targetNodeName, nmoparams.NHCLeaseCRName, timeline, &kubeletStopped)
				verifyLeaseHandoverToNHC(ctx, targetNodeName, nmCRName, nmoparams.NHCLeaseCRName, timeline)
				verifyLeaseReleaseAndRecovery(ctx, targetNodeName, nmoparams.NHCLeaseCRName, timeline)
				AddReportEntry("lease-interop-timeline", timeline)
			})
	})

// nhcGVK, nhcListGVK, leaseTRGVK and leaseTRListGVK identify unstructured objects this
// spec reads: the NMO suite has no typed NHC or TestRemediation APIs.
var (
	nhcGVK = schema.GroupVersionKind{
		Group: nmoparams.NHCGroup, Version: nmoparams.NHCVersion, Kind: nmoparams.NHCKind,
	}
	nhcListGVK = schema.GroupVersionKind{
		Group: nmoparams.NHCGroup, Version: nmoparams.NHCVersion, Kind: nmoparams.NHCKind + "List",
	}
	leaseTRGVK = schema.GroupVersionKind{
		Group: nmoparams.TestRemediationGroup, Version: nmoparams.TestRemediationVersion, Kind: "TestRemediation",
	}
	leaseTRListGVK = schema.GroupVersionKind{
		Group: nmoparams.TestRemediationGroup, Version: nmoparams.TestRemediationVersion, Kind: "TestRemediationList",
	}
)

// leaseFixtureObject is one TestRemediation fixture object this run created, paired
// with the description teardown logs when deleting it.
type leaseFixtureObject struct {
	obj         client.Object
	description string
}

// listAllNHCs returns a deep copy of every NodeHealthCheck CR in the cluster. The
// snapshot is what deleteAllNHCs removes and restorePreservedNHCs recreates, so
// pre-existing remediation config survives the run.
func listAllNHCs(ctx context.Context) []unstructured.Unstructured {
	GinkgoHelper()
	nhcList := &unstructured.UnstructuredList{}
	nhcList.SetGroupVersionKind(nhcListGVK)
	Expect(APIClient.List(ctx, nhcList)).To(Succeed(), "Failed to list NodeHealthCheck CRs")
	if len(nhcList.Items) == 0 {
		GinkgoWriter.Printf("No NodeHealthCheck CRs present\n")

		return nil
	}
	preserved := make([]unstructured.Unstructured, len(nhcList.Items))
	copy(preserved, nhcList.Items)

	return preserved
}

// deleteAllNHCs deletes every NodeHealthCheck CR in the snapshot and waits until each
// is gone, mirroring the Python setup_env_for_nhc_tests: any live NHC selecting
// workers would compete for the node lease once NMO releases it. Unconfirmed
// deletions are surfaced in the spec report: a surviving NHC could make the
// negative window fail with a confusing holder identity.
func deleteAllNHCs(ctx context.Context, nhcs []unstructured.Unstructured) {
	GinkgoHelper()
	for i := range nhcs {
		name := nhcs[i].GetName()
		By(fmt.Sprintf("Deleting NodeHealthCheck CR %s", name))
		if !deleteObjectBestEffort(ctx, &nhcs[i], "NodeHealthCheck "+name) {
			AddReportEntry("lease-interop-nhc-delete-failed", name)
		}
	}
}

// restorePreservedNHCs recreates the NodeHealthCheck CRs snapshotted before the spec
// deleted them, so pre-existing remediation config (including the NHC operator's
// default CR) survives the run. Best-effort with report entries: a restore failure
// must not mask the spec's own result.
func restorePreservedNHCs(nhcs []unstructured.Unstructured) {
	restoreCtx := context.Background()
	for i := range nhcs {
		name := nhcs[i].GetName()
		By(fmt.Sprintf("Cleanup: restoring pre-existing NodeHealthCheck CR %s", name))
		nhc := nhcs[i].DeepCopy()
		nhc.SetResourceVersion("")
		nhc.SetUID("")
		nhc.SetCreationTimestamp(metav1.Time{})
		err := APIClient.Create(restoreCtx, nhc)
		if err != nil && !errors.IsAlreadyExists(err) {
			GinkgoWriter.Printf("WARNING: failed to restore NodeHealthCheck %s: %v\n", name, err)
			AddReportEntry("lease-interop-nhc-restore-failed", fmt.Sprintf("NodeHealthCheck %s: %v", name, err))
		}
	}
}

// leaseExcludedNodes lists the node names the lease interop target must avoid: the
// node hosting the NMO controller pod (the maintenance drain would evict it and
// force a reschedule under a new name) and the nodes hosting the SSH bastion
// (recovery SSH depends on it). The Python reference picked any worker-label node
// without these filters.
func leaseExcludedNodes(ctx context.Context) []string {
	GinkgoHelper()
	_, nmoPodNodeName := nmoControllerPod()
	bastionNodes, err := helpers.SSHBastionNodes(ctx, APIClient)
	Expect(err).ToNot(HaveOccurred(), "Failed to list SSH bastion nodes")

	return append([]string{nmoPodNodeName}, bastionNodes...)
}

// requireLeaseEligibleWorkers skips the spec when fewer than NHCLeaseMinWorkers
// eligible workers exist. It runs before any destructive change, so a skipped spec
// leaves the cluster untouched.
func requireLeaseEligibleWorkers(ctx context.Context) {
	GinkgoHelper()
	excluded := leaseExcludedNodes(ctx)
	eligible, err := listSchedulableWorkers(ctx)
	Expect(err).ToNot(HaveOccurred(), "Failed to list schedulable worker nodes")
	count := 0
	for _, node := range eligible {
		if !slices.Contains(excluded, node.Name) {
			count++
		}
	}
	if count < nmoparams.NHCLeaseMinWorkers {
		Skip(fmt.Sprintf(
			"The NHC lease interop test requires at least %d schedulable non-control-plane "+
				"worker nodes excluding %v (the NMO controller and SSH bastion nodes), found %d",
			nmoparams.NHCLeaseMinWorkers, excluded, count))
	}
}

// selectLeaseTargetWorker returns a random Ready, schedulable, non-control-plane
// worker node excluding the NMO controller and SSH bastion nodes, reusing the
// suite's shared selection helper.
func selectLeaseTargetWorker(ctx context.Context) string {
	GinkgoHelper()

	return selectSchedulableWorkerExcluding(ctx, leaseExcludedNodes(ctx)...)
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
	ctx context.Context, nodeName, nhcName string, timeline map[string]string, kubeletStopped *bool,
) {
	GinkgoHelper()
	// Set before the stop is attempted: if the SSH command fails while kubelet
	// actually stopped, or the spec fails before the node leaves Ready, cleanup
	// must still restart it. Starting a running kubelet is a no-op; a missed
	// restart leaves the node NotReady for the rest of the suite.
	*kubeletStopped = true
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
// TestRemediation CR exists. Invariant violations -- including the NHC CR, the
// lease or their absence -- are reported as (false, nil) with a log line naming
// the broken invariant; API failures are returned as errors so the caller can
// retry them without masking a violation.
func leaseWindowHolds(ctx context.Context, nodeName, nhcName string) (bool, error) {
	phase, err := getNHCPhaseValue(ctx, nhcName)
	if err != nil {
		if errors.IsNotFound(err) {
			GinkgoWriter.Printf("Lease window violated: NodeHealthCheck %s is gone\n", nhcName)

			return false, nil
		}

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
		if errors.IsNotFound(err) {
			GinkgoWriter.Printf("Lease window violated: lease %s/%s does not exist\n",
				nmoparams.LeaseNamespace, nodeLeaseName(nodeName))

			return false, nil
		}

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
// same window used for the negative half, creates the node's TestRemediation CR and
// becomes the lease holder. Same node, same kubelet-down state: this is the positive
// control proving the negative window was not vacuous. The observed hand-over latency
// is recorded in the timeline so the margin against the negative window's budget is
// visible in the report.
func verifyLeaseHandoverToNHC(
	ctx context.Context, nodeName, nmCRName, nhcName string, timeline map[string]string,
) {
	GinkgoHelper()
	By(fmt.Sprintf("Deleting NodeMaintenance CR %s and waiting for removal", nmCRName))
	nmDeletedAt := time.Now()
	deleteAndWaitForNMCR(ctx, nmCRName, nmoparams.MaintenanceTimeout)
	timeline["nmDeleted"] = nmDeletedAt.UTC().Format(time.RFC3339)

	By("Verifying NHC started remediating the node")
	waitForNHCPhaseValue(ctx, nhcName, nmoparams.NHCPhaseRemediating, nmoparams.NHCLeaseRemediationWindow)
	remediatingObservedAt := time.Now()
	timeline["remediatingObserved"] = remediatingObservedAt.UTC().Format(time.RFC3339)
	timeline["handoverLatency"] = remediatingObservedAt.Sub(nmDeletedAt).String()

	By("Verifying a TestRemediation CR was created for the node")
	waitForLeaseTestRemediationCR(ctx, nodeName)

	By("Verifying NHC holds the node lease")
	waitForNHCLeaseHolder(ctx, nodeName, nhcName)
}

// waitForLeaseTestRemediationCR blocks until the node's TestRemediation CR exists.
// The NHC controller names remediation CRs after the node (mirrors the NHC suite's
// testRemediationCRExists). Asserting the concrete artifact closes the attribution
// loop: the positive control must show the very object whose absence the negative
// window polices.
func waitForLeaseTestRemediationCR(ctx context.Context, nodeName string) {
	GinkgoHelper()
	Eventually(func() (bool, error) {
		tr := &unstructured.Unstructured{}
		tr.SetGroupVersionKind(leaseTRGVK)
		err := APIClient.Get(ctx, client.ObjectKey{Name: nodeName}, tr)
		if err == nil {
			return true, nil
		}

		if errors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("getting TestRemediation %s: %w", nodeName, err)
	}, nmoparams.NHCLeaseRemediationWindow, nmoparams.DefaultPollInterval).Should(BeTrue(),
		"TestRemediation CR should be created for node %s", nodeName)
}

// verifyLeaseReleaseAndRecovery ends the remediation the way the Python reference
// does -- by restarting kubelet, since the TestRemediation Wait strategy never
// finishes on its own -- and verifies NHC returns to Enabled and the node's own
// lease is released. Only the target node's lease is asserted: unrelated medik8s
// operators may legitimately hold leases for other nodes on a shared cluster.
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
// phase. A missing CR is terminal, not transient: retrying cannot reach the phase,
// so it stops the wait immediately instead of burning the whole timeout.
func waitForNHCPhaseValue(ctx context.Context, name, expectedPhase string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func() (string, error) {
		phase, err := getNHCPhaseValue(ctx, name)
		if err != nil {
			if errors.IsNotFound(err) {
				return "", StopTrying("NodeHealthCheck CR is gone").Wrap(
					fmt.Errorf("NodeHealthCheck %s: %w", name, err))
			}

			return "", err
		}

		return phase, nil
	}, timeout, nmoparams.DefaultPollInterval).Should(Equal(expectedPhase),
		"NodeHealthCheck %s did not reach phase %s", name, expectedPhase)
}

// waitForNHCLeaseHolder waits until the node's lease is held by the NHC CR. The
// holder identity is matched by substring, as in the Python reference; the observed
// value is logged whenever it changes so the first live run can confirm the exact
// format before this is tightened to an equality check.
func waitForNHCLeaseHolder(ctx context.Context, nodeName, nhcName string) {
	GinkgoHelper()
	expected := nmoparams.NHCLeaseHolderPrefix + nhcName
	var lastLoggedHolder string
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

		holder := *lease.Spec.HolderIdentity
		if holder != lastLoggedHolder {
			lastLoggedHolder = holder
			GinkgoWriter.Printf("Lease %s holder identity: %q\n", nodeLeaseName(nodeName), holder)
		}

		return holder, nil
	}, nmoparams.LeaseTimeout, nmoparams.DefaultPollInterval).Should(ContainSubstring(expected),
		"Node %s lease should be held by NHC %s once maintenance ended", nodeName, nhcName)
}

// recoverLeaseTestNode restores the cluster after the lease interop scenario. The
// NHC CR is deleted first -- it is the component that could act on the released
// lease -- then the NodeMaintenance CR (NMO's stopMaintenance uncordons and untaints
// through the API and needs no running kubelet), then kubelet is restarted over SSH
// whenever the spec attempted to stop it, regardless of the node's current Ready
// condition: a failure between the stop and the node leaving Ready would otherwise
// leave the node NotReady, and starting an already-running kubelet is a no-op. When
// the spec never attempted a stop, the restart runs only if the node is NotReady or
// its state cannot be read. Every step is best-effort with report entries, so one
// failing step can never leave a worker NotReady for the rest of the suite.
func recoverLeaseTestNode(nodeName, nmName, nhcName string, kubeletStopped bool) {
	cleanupCtx := context.Background()

	By(fmt.Sprintf("Cleanup: deleting NodeHealthCheck CR %s if it still exists", nhcName))
	nhc := &unstructured.Unstructured{}
	nhc.SetGroupVersionKind(nhcGVK)
	nhc.SetName(nhcName)
	if !deleteObjectBestEffort(cleanupCtx, nhc, "NodeHealthCheck "+nhcName) {
		AddReportEntry("lease-interop-nhc-cleanup-failed", nhcName)
	}

	By(fmt.Sprintf("Cleanup: deleting NodeMaintenance CR %s if it still exists", nmName))
	if !deleteNMBestEffort(cleanupCtx, nmName) {
		AddReportEntry("lease-interop-nm-cleanup-failed", nmName)
	}

	restartKubelet := func() {
		if sshErr := helpers.StartKubeletSSHWithRetry(
			cleanupCtx, APIClient, nodeName, nmoparams.RebootTimeout, GinkgoWriter.Printf,
		); sshErr != nil {
			GinkgoWriter.Printf("WARNING: failed to restart kubelet on %s: %v\n", nodeName, sshErr)
			AddReportEntry("lease-interop-kubelet-restart-failed",
				fmt.Sprintf("node %s: %v", nodeName, sshErr))
		}
	}
	if kubeletStopped {
		By(fmt.Sprintf("Cleanup: starting kubelet on node %s (the spec stopped it)", nodeName))
		restartKubelet()
	} else {
		By(fmt.Sprintf("Cleanup: starting kubelet on node %s if it is NotReady", nodeName))
		node := &corev1.Node{}
		getErr := APIClient.Get(cleanupCtx, client.ObjectKey{Name: nodeName}, node)
		if getErr != nil || !helpers.IsNodeReady(node) {
			// On a read failure the node state is unknown, so attempt the restart
			// anyway: starting an already-running kubelet is a no-op, a missed
			// restart is not.
			restartKubelet()
		}
	}

	By(fmt.Sprintf("Cleanup: waiting for node %s to become Ready, uncordoned and untainted", nodeName))
	// The failure path gets the same budget as the happy path: a worker whose
	// kubelet was stopped needs the reboot-scale recovery window, not the
	// uncordon-scale one.
	if recErr := waitForNodeRecoveryWithTimeoutBestEffort(cleanupCtx, nodeName, nmoparams.RebootTimeout); recErr != nil {
		AddReportEntry("lease-interop-node-recovery-failed", fmt.Sprintf("node %s: %v", nodeName, recErr))
	}
}

// setupLeaseTestRemediation creates the TestRemediation dummy-remediator fixture the
// NHC CR points at: the two cluster-scoped CRDs, the TestRemediationTemplate CR, and
// the RBAC that lets the NHC controller manage TestRemediation CRs. It mirrors
// setupTestRemediationResources from the NHC suite (nhc_helpers.go), which is
// package-private there; the constant values mirror nhcparams. It returns only the
// objects this run created: pre-existing objects are left to their owner, and
// teardown does not remove them.
func setupLeaseTestRemediation(ctx context.Context) []leaseFixtureObject {
	GinkgoHelper()
	created := make([]leaseFixtureObject, 0, 5)
	By("Creating TestRemediationTemplate CRD")
	trtCRD := buildLeaseTestCRD(nmoparams.TestRemediationTemplateCRDName,
		"TestRemediationTemplate", "testremediationtemplates", "testremediationtemplate", "trt")
	if createLeaseFixtureIfAbsent(ctx, trtCRD, "TestRemediationTemplate CRD") {
		created = append(created, leaseFixtureObject{obj: trtCRD, description: "TestRemediationTemplate CRD"})
	}

	By("Creating TestRemediation CRD")
	trCRD := buildLeaseTestCRD(nmoparams.TestRemediationCRDName,
		"TestRemediation", "testremediations", "testremediation", "tr")
	if createLeaseFixtureIfAbsent(ctx, trCRD, "TestRemediation CRD") {
		created = append(created, leaseFixtureObject{obj: trCRD, description: "TestRemediation CRD"})
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
	if createLeaseFixtureIfAbsent(ctx, trtCR, "TestRemediationTemplate CR") {
		created = append(created, leaseFixtureObject{obj: trtCR, description: "TestRemediationTemplate CR"})
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
	if createLeaseFixtureIfAbsent(ctx, clusterRole, "ClusterRole") {
		created = append(created, leaseFixtureObject{obj: clusterRole, description: "ClusterRole " +
			nmoparams.TestRemediationClusterRoleName})
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
	if createLeaseFixtureIfAbsent(ctx, clusterRoleBinding, "ClusterRoleBinding") {
		created = append(created, leaseFixtureObject{obj: clusterRoleBinding, description: "ClusterRoleBinding " +
			nmoparams.TestRemediationClusterRoleBindingName})
	}

	return created
}

// createLeaseFixtureIfAbsent creates obj when it does not already exist, reporting
// whether this run created it. Pre-existing objects are reused, not adopted: the
// teardown only removes what the spec itself created.
func createLeaseFixtureIfAbsent(ctx context.Context, obj client.Object, description string) bool {
	GinkgoHelper()
	err := APIClient.Create(ctx, obj)
	if err == nil {
		return true
	}

	if errors.IsAlreadyExists(err) {
		GinkgoWriter.Printf("%s already exists, reusing it\n", description)

		return false
	}

	Fail(fmt.Sprintf("Failed to create %s: %v", description, err))

	return false
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
// "no TestRemediation CR exists" invariant. Best-effort (it also runs from
// teardown): a listing failure warns and skips.
func preCleanLeaseTestRemediationCRs(ctx context.Context) {
	GinkgoHelper()
	trList := &unstructured.UnstructuredList{}
	trList.SetGroupVersionKind(leaseTRListGVK)
	if err := APIClient.List(ctx, trList); err != nil {
		GinkgoWriter.Printf("WARNING: failed to list TestRemediation CRs: %v\n", err)

		return
	}
	for i := range trList.Items {
		name := trList.Items[i].GetName()
		By(fmt.Sprintf("Deleting leftover TestRemediation CR %s", name))
		deleteObjectBestEffort(ctx, &trList.Items[i], "TestRemediation "+name)
	}
}

// cleanupLeaseTestRemediation removes the TestRemediation fixture: first any
// TestRemediation CRs the NHC controller created during the spec, then the objects
// this run created (pre-existing objects are left in place). CRs and RBAC go first,
// while the CRDs still exist. Every step is cleanup-safe and mirrors
// cleanupTestRemediationResources from the NHC suite.
func cleanupLeaseTestRemediation(ctx context.Context, created []leaseFixtureObject) {
	preCleanLeaseTestRemediationCRs(ctx)
	for i := range created {
		By(fmt.Sprintf("Cleanup: deleting %s", created[i].description))
		if !deleteObjectBestEffortWithTimeout(ctx, created[i].obj, created[i].description,
			nmoparams.TestRemediationFixtureDeletionTimeout) {
			AddReportEntry("lease-interop-fixture-cleanup-failed", created[i].description)
		}
	}
}
