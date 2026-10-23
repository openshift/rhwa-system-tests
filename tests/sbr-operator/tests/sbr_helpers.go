package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
)

// getNodeBootID returns the bootID of the named node.
func getNodeBootID(nodeName string) (string, error) {
	node, err := APIClient.CoreV1Interface.Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get node %s: %w", nodeName, err)
	}

	bootID := node.Status.NodeInfo.BootID
	if bootID == "" {
		return "", fmt.Errorf("node %s boot-id is empty", nodeName)
	}

	return bootID, nil
}

// cephFSRejectBidirectional defines nsenter + iptables REJECT rules for both INPUT and OUTPUT
// chains covering all CephFS port groups: 3300 (msgr2), 6789 (msgr1 mon), 6800-7300 (OSD/MDS).
var cephFSRejectBidirectional = [][]string{
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-I", "OUTPUT", "-p", "tcp", "--dport", "3300", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-I", "INPUT", "-p", "tcp", "--sport", "3300", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-I", "OUTPUT", "-p", "tcp", "--dport", "6789", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-I", "INPUT", "-p", "tcp", "--sport", "6789", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-I", "OUTPUT", "-p", "tcp", "--dport", "6800:7300", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-I", "INPUT", "-p", "tcp", "--sport", "6800:7300", "-j", "REJECT",
	},
}

// cephFSFlushBidirectional defines the corresponding -D (delete) rules for cephFSRejectBidirectional.
var cephFSFlushBidirectional = [][]string{
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-D", "OUTPUT", "-p", "tcp", "--dport", "3300", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-D", "INPUT", "-p", "tcp", "--sport", "3300", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-D", "OUTPUT", "-p", "tcp", "--dport", "6789", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-D", "INPUT", "-p", "tcp", "--sport", "6789", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-D", "OUTPUT", "-p", "tcp", "--dport", "6800:7300", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-D", "INPUT", "-p", "tcp", "--sport", "6800:7300", "-j", "REJECT",
	},
}

// cephFSRejectOutput defines nsenter + iptables REJECT rules for the OUTPUT chain only.
var cephFSRejectOutput = [][]string{
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-I", "OUTPUT", "-p", "tcp", "--dport", "3300", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-I", "OUTPUT", "-p", "tcp", "--dport", "6789", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-I", "OUTPUT", "-p", "tcp", "--match", "multiport",
		"--dports", "6800:7300", "-j", "REJECT",
	},
}

// cephFSFlushOutput defines the corresponding -D (delete) rules for cephFSRejectOutput.
var cephFSFlushOutput = [][]string{
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-D", "OUTPUT", "-p", "tcp", "--dport", "3300", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-D", "OUTPUT", "-p", "tcp", "--dport", "6789", "-j", "REJECT",
	},
	{
		"nsenter", "--target", "1", "--net", "--mount", "--",
		"iptables", "-D", "OUTPUT", "-p", "tcp", "--match", "multiport",
		"--dports", "6800:7300", "-j", "REJECT",
	},
}

// injectCephFSRejectBidirectional inserts iptables REJECT rules on both INPUT and OUTPUT chains
// for all CephFS ports via nsenter into the host network and mount namespaces.
// Calls Skip when the first iptables rule fails (e.g. nftables-only node without the
// iptables compatibility layer) so the test is skipped rather than hard-failed.
func injectCephFSRejectBidirectional(injectorPod *pod.Builder, nodeName string) {
	By(fmt.Sprintf("Injecting CephFS iptables REJECT rules (INPUT+OUTPUT) on node %s", nodeName))
	recordInjectionBootID(injectorPod.Definition.Name, nodeName)

	for ruleIdx, rule := range cephFSRejectBidirectional {
		if _, execErr := injectorPod.ExecCommand(rule); execErr != nil {
			if ruleIdx == 0 {
				Skip(fmt.Sprintf("iptables bidirectional injection not available on node %s: %v; "+
					"skipping test", nodeName, execErr))
			}

			Fail(fmt.Sprintf("Failed to inject iptables rule %v on node %q (rule %d of %d succeeded earlier): %v",
				rule, nodeName, ruleIdx, len(cephFSRejectBidirectional), execErr))
		}
	}
}

// removeCephFSRejectBidirectional deletes the iptables REJECT rules added by
// injectCephFSRejectBidirectional. Failures are logged as warnings; cleanup is best-effort.
func removeCephFSRejectBidirectional(injectorPod *pod.Builder) bool {
	removed := true

	for _, rule := range cephFSFlushBidirectional {
		if _, flushErr := injectorPod.ExecCommand(rule); flushErr != nil {
			removed = false
			GinkgoWriter.Printf("Warning: iptables cleanup (cmd %v): %v\n", rule, flushErr)
		}
	}
	if removed {
		confirmODFFaultCleared(injectorPod.Definition.Name)
	}

	return removed
}

// removeInjectedRejectRules removes the CephFS REJECT rules through the named injector
// pod when it still exists, using remove (removeCephFSRejectOutput or
// removeCephFSRejectBidirectional). AfterAll runs before the last It's DeferCleanup,
// and the NHC webhook refuses to delete an NHC while the injected fault keeps its
// remediation running, so AfterAll calls this before deleting the NHC. The pod is
// left in place for the later cleanup steps that delete it. Only confirmed removal
// returns true, so callers can retain their deferred fallback after a failed attempt.
// A pod that is gone while its target node is mid-reboot is not reported as
// unconfirmed yet: waitForInjectionReboot waits for the boot ID change that proves
// the reboot cleared the rules.
func removeInjectedRejectRules(injectorPodName string, remove func(*pod.Builder) bool) bool {
	if injectorPodName == "" {
		return false
	}

	injectorPod := pod.NewBuilder(APIClient, injectorPodName, medik8sparams.OperatorNs, sbrparams.InjectorImage)
	getCtx, getCancel := context.WithTimeout(context.Background(), sbrparams.InjectorRuleCleanupTimeout)
	defer getCancel()
	getErr := APIClient.Get(getCtx, types.NamespacedName{
		Name: injectorPodName, Namespace: medik8sparams.OperatorNs,
	}, injectorPod.Definition)
	if getErr != nil {
		if k8serrors.IsNotFound(getErr) && waitForInjectionReboot(injectorPodName) {
			confirmODFFaultCleared(injectorPodName)
			delete(injectionBootIDs, injectorPodName)

			return true
		}

		if k8serrors.IsNotFound(getErr) && nodeIsDown(injectionTargetNode(injectorPodName)) {
			// The reboot was still in progress when the wait expired. It clears the
			// non-persistent rules once kubelet posts the new boot ID, so this is a
			// pending confirmation, not an unconfirmed removal.
			AddReportEntry("injected-rule-removal-pending-reboot", fmt.Sprintf(
				"pod %s: node %s still rebooting; the pending reboot clears the rules",
				injectorPodName, injectionTargetNode(injectorPodName)))

			return false
		}

		if k8serrors.IsNotFound(getErr) {
			GinkgoWriter.Printf("Injector pod %s is absent; rule removal could not be confirmed\n", injectorPodName)
		} else {
			GinkgoWriter.Printf("Warning: lookup injector pod %s for rule removal: %v\n", injectorPodName, getErr)
		}

		AddReportEntry("injected-rule-removal-unconfirmed", fmt.Sprintf("pod %s: %v", injectorPodName, getErr))

		return false
	}

	removed := remove(injectorPod)
	if removed {
		delete(injectionBootIDs, injectorPodName)
	} else {
		AddReportEntry("injected-rule-removal-unconfirmed",
			fmt.Sprintf("pod %s: see cleanup command warnings", injectorPodName))
	}

	return removed
}

// injectCephFSRejectOutput inserts iptables OUTPUT-only REJECT rules for all CephFS ports.
// Calls Skip when iptables returns a non-zero exit code so that CI runs on nodes where the
// OUTPUT chain is not writable do not hard-fail.
func injectCephFSRejectOutput(injectorPod *pod.Builder, nodeName string) {
	By(fmt.Sprintf("Injecting CephFS iptables OUTPUT REJECT rules on node %s", nodeName))
	recordInjectionBootID(injectorPod.Definition.Name, nodeName)

	for _, rule := range cephFSRejectOutput {
		if _, execErr := injectorPod.ExecCommand(rule); execErr != nil {
			Skip(fmt.Sprintf("iptables OUTPUT injection not available on node %s (exit code 1): %v; "+
				"skipping test", nodeName, execErr))
		}
	}
}

// removeCephFSRejectOutput deletes the iptables OUTPUT rules added by injectCephFSRejectOutput.
// Failures are logged as warnings; cleanup is best-effort.
func removeCephFSRejectOutput(injectorPod *pod.Builder) bool {
	removed := true

	for _, rule := range cephFSFlushOutput {
		if _, flushErr := injectorPod.ExecCommand(rule); flushErr != nil {
			removed = false
			GinkgoWriter.Printf("Warning: iptables cleanup (cmd %v): %v\n", rule, flushErr)
		}
	}
	if removed {
		confirmODFFaultCleared(injectorPod.Definition.Name)
	}

	return removed
}

// isNHCCRDInstalled returns true when the NodeHealthCheck CRD is registered in the cluster.
// Returns false only when the CRD is genuinely absent (IsNotFound). Transient API errors
// are propagated as test failures to avoid silently skipping NHC-dependent tests.
func isNHCCRDInstalled() bool {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := APIClient.Get(context.TODO(),
		types.NamespacedName{Name: sbrparams.NHCCRDName}, crd)
	if err == nil {
		return true
	}

	if k8serrors.IsNotFound(err) {
		GinkgoWriter.Printf("isNHCCRDInstalled: CRD %s not found\n", sbrparams.NHCCRDName)

		return false
	}

	Fail(fmt.Sprintf("isNHCCRDInstalled: unexpected error checking CRD %s: %v",
		sbrparams.NHCCRDName, err))

	return false
}

// sbrcCRDHasSpecField reports whether the StorageBasedRemediationConfig CRD declares
// the given field in the spec properties of any served version.
func sbrcCRDHasSpecField(crd *apiextensionsv1.CustomResourceDefinition, field string) bool {
	for versionIdx := range crd.Spec.Versions {
		crdVersion := crd.Spec.Versions[versionIdx]
		if crdVersion.Schema == nil || crdVersion.Schema.OpenAPIV3Schema == nil {
			continue
		}

		specProps, found := crdVersion.Schema.OpenAPIV3Schema.Properties["spec"]
		if !found {
			continue
		}

		if _, found := specProps.Properties[field]; found {
			return true
		}
	}

	return false
}

// installedSBRCsvVersion returns the SBR operator CSV version recorded in the CRD's
// operatorframework.io/installed-alongside annotation, for example "0.3.1" for
// "openshift-workload-availability/storage-based-remediation.v0.3.1". It returns an
// empty string when OLM did not record the installing CSV. Annotations whose CSV
// name does not match CSVNamePattern are ignored, so an unrelated CSV recorded on
// the same CRD cannot be selected instead; OLM writes one annotation per installed
// CSV and map iteration order is not deterministic.
func installedSBRCsvVersion(crd *apiextensionsv1.CustomResourceDefinition) string {
	for key, value := range crd.Annotations {
		if !strings.HasPrefix(key, "operatorframework.io/installed-alongside-") {
			continue
		}

		csvName := value[strings.LastIndex(value, "/")+1:]
		if !strings.HasPrefix(csvName, sbrparams.CSVNamePattern+".v") {
			continue
		}
		versionIdx := strings.LastIndex(csvName, ".v")
		if versionIdx < 0 {
			continue
		}

		return csvName[versionIdx+len(".v"):]
	}

	return ""
}

// requireSBRCBlockVolumeModeSupport verifies that the installed SBR operator can honor
// sharedStorageVolumeMode (RHWA-1313, RWX Block volume support).
//
// Operators at or above FirstSBRCsvWithBlockVolumeMode must ship the field: a missing
// field there is an operator regression and fails the suite. Older operators predate
// the feature, so the calling spec skips instead of provisioning RWX Filesystem PVCs
// on Ceph RBD, which ceph-csi rejects. An unknown CSV version also fails, so a missing
// field can never silently skip a capable operator.
func requireSBRCBlockVolumeModeSupport() {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := APIClient.Get(context.TODO(),
		types.NamespacedName{Name: sbrparams.SBRCRDName}, crd)
	if err != nil {
		Fail(fmt.Sprintf("requireSBRCBlockVolumeModeSupport: cannot read CRD %s: %v",
			sbrparams.SBRCRDName, err))

		return
	}

	if sbrcCRDHasSpecField(crd, sbrparams.SBRCBlockVolumeModeField) {
		GinkgoWriter.Printf("SBRC CRD %s includes the %s field (installed CSV v%s)\n",
			sbrparams.SBRCRDName, sbrparams.SBRCBlockVolumeModeField, installedSBRCsvVersion(crd))

		return
	}

	csvVersion := installedSBRCsvVersion(crd)
	if csvVersion == "" {
		Fail(fmt.Sprintf(
			"SBR operator CRD %s lacks the %s field and no installed CSV version was recorded; "+
				"refusing to skip block volume mode tests",
			sbrparams.SBRCRDName, sbrparams.SBRCBlockVolumeModeField))

		return
	}

	installedCsv, parseErr := version.ParseGeneric(csvVersion)
	if parseErr != nil {
		Fail(fmt.Sprintf(
			"requireSBRCBlockVolumeModeSupport: cannot parse installed SBR CSV version %q: %v",
			csvVersion, parseErr))

		return
	}

	firstCsvWithField, parseErr := version.ParseGeneric(sbrparams.FirstSBRCsvWithBlockVolumeMode)
	if parseErr != nil {
		Fail(fmt.Sprintf(
			"requireSBRCBlockVolumeModeSupport: cannot parse %q: %v",
			sbrparams.FirstSBRCsvWithBlockVolumeMode, parseErr))

		return
	}

	if installedCsv.AtLeast(firstCsvWithField) {
		Fail(fmt.Sprintf(
			"SBR operator CSV v%s must support %s (RHWA-1313, first shipped in v%s) "+
				"but the installed CRD %s lacks the field",
			csvVersion, sbrparams.SBRCBlockVolumeModeField,
			sbrparams.FirstSBRCsvWithBlockVolumeMode, sbrparams.SBRCRDName))

		return
	}

	Skip(fmt.Sprintf(
		"installed SBR operator CSV v%s predates %s (RHWA-1313, first shipped in v%s); "+
			"skipping Ceph RBD block persistent fencing",
		csvVersion, sbrparams.SBRCBlockVolumeModeField, sbrparams.FirstSBRCsvWithBlockVolumeMode))
}

// nhcsUsingSBRTemplate returns the names of NodeHealthChecks that use SBR as a remediator,
// directly or through an escalating remediation. Returns (nil, nil) when the NHC API is not
// registered, and a wrapped error for any other list failure.
func nhcsUsingSBRTemplate(ctx context.Context) ([]string, error) {
	nhcList := &unstructured.UnstructuredList{}
	nhcList.SetAPIVersion(sbrparams.NHCAPIGroup + "/" + sbrparams.NHCAPIVersion)
	nhcList.SetKind("NodeHealthCheckList")
	if err := APIClient.List(ctx, nhcList); err != nil {
		if meta.IsNoMatchError(err) || k8serrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("listing NodeHealthChecks: %w", err)
	}

	var names []string

	for idx := range nhcList.Items {
		if helpers.NHCReferencesTemplateKind(nhcList.Items[idx].Object, sbrparams.SBRTemplateKind) {
			names = append(names, nhcList.Items[idx].GetName())
		}
	}

	return names, nil
}

// ensureSBRTemplate creates the StorageBasedRemediationTemplate referenced by every SBR
// NodeHealthCheck when it does not already exist. The SBR operator does not create it (the
// sample only lives in the CSV alm-examples), and without it NHC disables itself with
// RemediationTemplateNotFound and never creates StorageBasedRemediation CRs. A template
// created here is deleted by DeferCleanup at the end of the calling node; a pre-existing
// template is left untouched.
func ensureSBRTemplate() {
	template := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": sbrparams.CRDGroup + "/" + sbrparams.CRDVersion,
			"kind":       sbrparams.SBRTemplateKind,
			"metadata": map[string]interface{}{
				"name":      sbrparams.SBRTemplateName,
				"namespace": medik8sparams.OperatorNs,
			},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{},
				},
			},
		},
	}
	err := APIClient.Create(context.TODO(), template)
	if k8serrors.IsAlreadyExists(err) {
		return
	}

	Expect(err).ToNot(HaveOccurred(),
		"StorageBasedRemediationTemplate %q must be created in %s", sbrparams.SBRTemplateName, medik8sparams.OperatorNs)
	GinkgoWriter.Printf("Created StorageBasedRemediationTemplate %s/%s\n",
		medik8sparams.OperatorNs, sbrparams.SBRTemplateName)

	DeferCleanup(func() {
		if delErr := APIClient.Delete(context.TODO(), template); delErr != nil && !k8serrors.IsNotFound(delErr) {
			GinkgoWriter.Printf("Warning: cleanup StorageBasedRemediationTemplate %s: %v\n",
				sbrparams.SBRTemplateName, delErr)
		}
	})
}

// buildNHC returns an unstructured NodeHealthCheck CR that triggers SBR-based remediation
// when a worker node reports SBRStorageUnhealthy=True for NHCUnhealthyDuration.
func buildNHC(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": sbrparams.NHCAPIGroup + "/" + sbrparams.NHCAPIVersion,
			"kind":       "NodeHealthCheck",
			"metadata": map[string]interface{}{
				"name": name,
			},
			"spec": map[string]interface{}{
				// NHC requires exactly one of minHealthy/maxUnhealthy; omitting both is
				// rejected by the validating webhook ("one of minHealthy and maxUnhealthy
				// should be specified"). Since NHC v0.10.0 (maxUnhealthy support, PR #372)
				// the previous 51% default on minHealthy was removed to make the two fields
				// mutually exclusive, so it must now be set explicitly. 51% matches the
				// former default and the sample CRs.
				"minHealthy": "51%",
				"selector": map[string]interface{}{
					"matchExpressions": []interface{}{
						map[string]interface{}{
							"key":      "node-role.kubernetes.io/worker",
							"operator": "Exists",
						},
					},
				},
				"unhealthyConditions": []interface{}{
					map[string]interface{}{
						"type":     sbrparams.SBRStorageUnhealthyCondition,
						"status":   string(corev1.ConditionTrue),
						"duration": sbrparams.NHCUnhealthyDuration,
					},
				},
				"remediationTemplate": map[string]interface{}{
					"apiVersion": sbrparams.CRDGroup + "/" + sbrparams.CRDVersion,
					"kind":       sbrparams.SBRTemplateKind,
					"name":       sbrparams.SBRTemplateName,
					"namespace":  medik8sparams.OperatorNs,
				},
			},
		},
	}
}

// buildNHCWithMaxUnhealthy returns the same SBR NodeHealthCheck as buildNHC but gated on maxUnhealthy
// instead of minHealthy, so a fixed number of nodes may be unhealthy regardless of cluster size
// (minHealthy: "51%" blocks any remediation on a 2-worker cluster). It delegates to buildNHC and swaps
// the health field, so the two stay in sync on selector, template and unhealthyConditions.
func buildNHCWithMaxUnhealthy(name string, maxUnhealthy int) *unstructured.Unstructured {
	nhc := buildNHC(name)
	spec, _ := nhc.Object["spec"].(map[string]interface{})
	delete(spec, "minHealthy")
	spec["maxUnhealthy"] = int64(maxUnhealthy)

	return nhc
}

// cleanupNHCCR deletes the named NodeHealthCheck CR, retrying until it is gone.
// The NHC webhook rejects deletion while the NHC has an ongoing remediation, so
// call this after the injected storage fault is removed; NHC then ends the
// remediation (deleting the SBR CR through the agent finalizer) and the delete
// is admitted. Safe to call when the CR may not exist. A CR that is still present
// after the timeout is recorded as a report entry.
func cleanupNHCCR(ctx context.Context, name string) {
	nhcGVK := schema.GroupVersionKind{
		Group: sbrparams.NHCAPIGroup, Version: sbrparams.NHCAPIVersion, Kind: "NodeHealthCheck",
	}
	gone := helpers.DeleteRemediationCR(ctx, APIClient, nhcGVK, name, "",
		sbrparams.DefaultPollInterval, sbrparams.NHCCleanupTimeout, GinkgoWriter.Printf)
	if !gone {
		AddReportEntry("sbr-nhc-cleanup-failed",
			fmt.Sprintf("NodeHealthCheck %s still exists and may remediate nodes in later specs", name))
	}
}

// deleteNHCAndWait deletes the named NodeHealthCheck CR and blocks until it is fully gone. The NHC
// operator uses finalizers, so a terminating CR from a prior run must be waited out before a fresh one
// is created under the same name. Safe to call when the CR does not exist.
func deleteNHCAndWait(name string) {
	cleanupNHCCR(context.Background(), name)
	nhc := &unstructured.Unstructured{}
	nhc.SetAPIVersion(sbrparams.NHCAPIGroup + "/" + sbrparams.NHCAPIVersion)
	nhc.SetKind("NodeHealthCheck")

	Eventually(func() error {
		getErr := APIClient.Get(context.TODO(), types.NamespacedName{Name: name}, nhc)
		if k8serrors.IsNotFound(getErr) {
			return nil
		}

		if getErr != nil {
			return getErr
		}

		return fmt.Errorf("NodeHealthCheck %s still terminating", name)
	}, sbrparams.SBRCReadyTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"NodeHealthCheck %q must be fully removed before a fresh one is created", name)
}

// pickTargetWorkerNode returns the first schedulable worker node that does not host an SBR controller pod.
func pickTargetWorkerNode() string {
	controllerNodes := controllerPodNodes()
	nodeList, err := APIClient.CoreV1Interface.Nodes().List(context.TODO(), metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/worker",
	})
	Expect(err).ToNot(HaveOccurred(), "Failed to list worker nodes")

	for nodeIdx := range nodeList.Items {
		node := &nodeList.Items[nodeIdx]
		if controllerNodes[node.Name] {
			GinkgoWriter.Printf("Skipping node %s (SBR controller pod runs there)\n", node.Name)

			continue
		}

		if isNodeSchedulable(node) {
			return node.Name
		}
	}

	return ""
}

// purgeInjectorImageAfterReboot removes the injector image from a node that was hard-rebooted.
// An unclean reboot can corrupt the image's CRI-O overlay links, after which every new
// container from that image fails with "readlink .../overlay/l: invalid argument" while the
// node still reports Ready. Removing the image forces a clean pull for the next spec.
// Containers still using the image (for example an injector pod whose container is being
// restarted after the reboot) are removed first, since crictl rmi refuses an image in use.
// Best effort: failures are logged, not asserted. The command reports whether the image is
// actually gone afterwards, so the logs never claim a removal that did not happen: only
// crictl's confirmed "no such image" result is reported as absent, and any other inspection
// failure is reported as inspect-failed so it is never mistaken for absence.
func purgeInjectorImageAfterReboot(ctx context.Context, nodeName string) {
	GinkgoWriter.Printf("Removing injector image %s from node %s to drop overlay layers written before the reboot\n",
		sbrparams.InjectorImage, nodeName)
	output, err := helpers.RunOnNode(ctx, nodeName, sbrparams.CrioCleanupTimeout,
		"bash", "-c", fmt.Sprintf(
			"if err=$(crictl inspecti %[1]s 2>&1); then "+
				"crictl ps -a -q --image %[1]s | xargs -r crictl rm -f >/dev/null 2>&1; "+
				"if crictl rmi %[1]s >/dev/null 2>&1; then echo removed; else echo still-present; fi; "+
				"else case \"$err\" in *\"no such image \\\"%[1]s\\\" present\"*) echo absent;; "+
				"*) echo inspect-failed;; esac; fi",
			sbrparams.InjectorImage))

	switch {
	case err != nil:
		GinkgoT().Logf("Warning: injector image removal on node %s failed: %v (output: %s)",
			nodeName, err, output)
	case strings.Contains(output, "still-present"):
		GinkgoT().Logf("Warning: injector image %s is still present on node %s after the purge, "+
			"probably because a container is using it again (output: %s)",
			sbrparams.InjectorImage, nodeName, output)
	case strings.Contains(output, "inspect-failed"):
		GinkgoT().Logf("Warning: could not determine whether injector image %s is present on node %s, "+
			"so the purge was skipped (crictl inspecti failed; output: %s)",
			sbrparams.InjectorImage, nodeName, output)
	case strings.Contains(output, "absent"):
		GinkgoT().Logf("Injector image %s already absent from node %s", sbrparams.InjectorImage, nodeName)
	default:
		GinkgoWriter.Printf("Injector image removed from node %s (output: %s)\n", nodeName, output)
	}
}

// getSBRCRCondition returns the named status condition from an unstructured SBR CR, or nil.
func getSBRCRCondition(sbrObj *unstructured.Unstructured, condType string) map[string]interface{} {
	conditions, found, err := unstructured.NestedSlice(sbrObj.Object, "status", "conditions")
	if err != nil || !found {
		return nil
	}

	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}

		if cond["type"] == condType {
			return cond
		}
	}

	return nil
}

// agentPodCountMatchesWorkerCount reports whether the number of running SBR agent
// pods equals the worker node count. It returns the result along with the actual and
// expected counts so the caller can assert and compose its own failure message.
// No assertion happens here, so it is safe to call inside Eventually.
func agentPodCountMatchesWorkerCount() (match bool, actual, expected int, err error) {
	runningAgentPods, err := runningAgentPods()
	if err != nil {
		return false, 0, 0, err
	}

	actual = len(runningAgentPods)
	expected = getWorkerNodeCount()

	return actual == expected, actual, expected, nil
}

// runningAgentPods returns all SBR agent pods currently in Running state.
func runningAgentPods() ([]*pod.Builder, error) {
	agentPods, listErr := pod.List(APIClient, medik8sparams.OperatorNs,
		metav1.ListOptions{LabelSelector: sbrparams.AgentPodLabelSelector})
	if listErr != nil {
		return nil, fmt.Errorf("list SBR agent pods: %w", listErr)
	}

	return helpers.FilterRunningPods(agentPods), nil
}

// requireNoSBRNodeHealthCheck skips the spec when a NodeHealthCheck remediates through the SBR
// template. Such an NHC would create a StorageBasedRemediation CR for a node the test is
// deliberately making unhealthy and fence it, which is correct NHC behaviour but the opposite of
// what a "nothing was fenced" test measures.
func requireNoSBRNodeHealthCheck() {
	By("Verifying no NodeHealthCheck remediates through the SBR template")

	nhcNames, nhcErr := nhcsUsingSBRTemplate(context.TODO())
	Expect(nhcErr).ToNot(HaveOccurred(), "Failed to list NodeHealthChecks")

	if len(nhcNames) > 0 {
		Skip(fmt.Sprintf("NodeHealthCheck(s) %v remediate through %s, so they would create a "+
			"StorageBasedRemediation CR for an unhealthy node and fence it. "+
			"Delete them to run this test.", nhcNames, sbrparams.SBRTemplateKind))
	}
}

// nodeConditionIs returns nil when the named node condition has the wanted status and, when
// wantReason is set, the wanted reason. A condition that is absent satisfies a plain "must be
// False": that is how the agent leaves a node it has never reported on.
func nodeConditionIs(nodeName, condType string, wantStatus corev1.ConditionStatus, wantReason string) error {
	node, getErr := APIClient.CoreV1Interface.Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("get node %s: %w", nodeName, getErr)
	}

	for _, condition := range node.Status.Conditions {
		if string(condition.Type) != condType {
			continue
		}

		if condition.Status != wantStatus {
			return fmt.Errorf("node %s condition %s is %s (reason %q); want %s",
				nodeName, condType, condition.Status, condition.Reason, wantStatus)
		}

		if wantReason != "" && condition.Reason != wantReason {
			return fmt.Errorf("node %s condition %s is %s with reason %q; want reason %q",
				nodeName, condType, condition.Status, condition.Reason, wantReason)
		}

		return nil
	}

	if wantStatus == corev1.ConditionFalse && wantReason == "" {
		return nil
	}

	return fmt.Errorf("node %s has no %s condition; want %s", nodeName, condType, wantStatus)
}

// nodeRebootStarted returns nil once the node is NotReady or its bootID differs from preBootID.
// A fast reboot (e.g. on AWS) can finish inside the node-monitor grace period, so the node
// never reports NotReady; the changed bootID is then the only evidence that it rebooted.
func nodeRebootStarted(nodeName, preBootID string) error {
	node, err := APIClient.CoreV1Interface.Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}

	if bootID := node.Status.NodeInfo.BootID; bootID != "" && bootID != preBootID {
		return nil
	}

	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
			return nil
		}
	}

	return fmt.Errorf("node %s is still Ready with boot ID %s; waiting for reboot", nodeName, preBootID)
}

// sbrCRObserver records the lifecycle of the first StorageBasedRemediation CR created for one
// node. The SBR agents fence the node as soon as the CR exists, then set SBRStorageUnhealthy to
// Unknown after a short stale period so NHC deletes the CR, often seconds after creation and long
// before the node is Ready again. Sampling the CR inside every wait loop captures its
// FencingSucceeded condition before that cleanup. The observer is bound to the first CR's UID,
// so a later CR for the same node (a new remediation cycle) cannot satisfy the check.
type sbrCRObserver struct {
	nodeName string
	uid      string
	seen     bool
	gone     bool
	fenced   bool
}

// observe pulls the CR once and records whether the first CR exists and whether fencing
// succeeded on it. It returns the pull error so callers can tell NotFound from a transient
// API failure.
func (o *sbrCRObserver) observe() error {
	sbrCR, err := pullSBRCR(o.nodeName)
	if err != nil {
		if k8serrors.IsNotFound(err) && o.seen {
			o.gone = true
		}

		return err
	}

	if !o.seen {
		o.seen = true
		o.uid = string(sbrCR.GetUID())
	}

	if string(sbrCR.GetUID()) != o.uid {
		o.gone = true

		return nil
	}

	cond := getSBRCRCondition(sbrCR, sbrparams.FencingSucceededCondition)
	if cond != nil && cond["status"] == string(corev1.ConditionTrue) {
		o.fenced = true
	}

	return nil
}

// observeBestEffort samples the CR and logs unexpected pull errors. The node reboot and
// recovery waits use it because a failed sample only risks missing CR lifecycle evidence,
// not the wait's own subject. NotFound is expected and stays silent while NHC has not
// created the CR yet.
func (o *sbrCRObserver) observeBestEffort() {
	if err := o.observe(); err != nil && !k8serrors.IsNotFound(err) {
		GinkgoWriter.Printf("Warning: StorageBasedRemediation/%s sample failed: %v\n", o.nodeName, err)
	}
}

// crFenced is an Eventually body: nil once FencingSucceeded=True was observed on the first CR.
// Polling from the first sighting keeps sampling during the CR's short life, which may start
// before the agents write the status. It stops retrying when that CR is removed or replaced
// without the observation, so a remediation that never fenced fails at once with a clear message.
func (o *sbrCRObserver) crFenced() error {
	if err := o.observe(); err != nil && !k8serrors.IsNotFound(err) {
		return err
	}

	if o.fenced {
		return nil
	}

	if o.gone {
		return StopTrying(fmt.Sprintf(
			"StorageBasedRemediation/%s (uid %s) was removed before FencingSucceeded=True was observed",
			o.nodeName, o.uid))
	}

	if o.seen {
		return fmt.Errorf("StorageBasedRemediation/%s exists (uid %s) but FencingSucceeded=True is not set yet",
			o.nodeName, o.uid)
	}

	return fmt.Errorf("StorageBasedRemediation/%s not yet created by NHC", o.nodeName)
}

// crRemoved is an Eventually body: nil once the first CR is deleted or replaced by a newer CR
// for the same node. A later remediation cycle must not keep this wait pending.
func (o *sbrCRObserver) crRemoved() error {
	if err := o.observe(); err != nil && !k8serrors.IsNotFound(err) {
		return err
	}

	if o.gone {
		return nil
	}

	return fmt.Errorf("StorageBasedRemediation/%s (uid %s) still exists; waiting for cleanup", o.nodeName, o.uid)
}

// waitForSBRCRFenced waits until NHC creates the first StorageBasedRemediation CR for the
// observed node and that CR reaches FencingSucceeded=True. The SBR agents fence as soon as
// the CR exists and NHC deletes it soon after, so the observer must sample it while polling.
func waitForSBRCRFenced(observer *sbrCRObserver) {
	GinkgoHelper()
	Eventually(observer.crFenced, sbrparams.NHCSBRCRCreationTimeout,
		sbrparams.DefaultPollInterval).Should(Succeed(),
		"NHC must create StorageBasedRemediation CR for node %s and it must reach FencingSucceeded=True",
		observer.nodeName)
	GinkgoWriter.Printf("StorageBasedRemediation/%s (uid %s) created by NHC and FencingSucceeded=True\n",
		observer.nodeName, observer.uid)
}

// odfBlocklistEntry is one entry of "ceph osd blocklist ls --format json".
type odfBlocklistEntry struct {
	Addr  string `json:"addr"`
	Range string `json:"range"`
	Until string `json:"until"`
}

// Recovery is limited to this process's completed fault specs, never arbitrary workers.
type odfFenceTarget struct {
	spec       string
	started    time.Time
	completed  bool
	injections map[string]string
}

var (
	odfFenceTargets   = map[string]*odfFenceTarget{}
	nextODFFenceCheck time.Time
)

var _ = ReportAfterEach(func(report SpecReport) {
	for _, target := range odfFenceTargets {
		if target.spec == report.FullText() {
			target.completed = true
		}
	}
})

func recordODFFenceTarget(nodeName string) *odfFenceTarget {
	report := CurrentSpecReport()
	target := odfFenceTargets[nodeName]
	if target == nil {
		target = &odfFenceTarget{started: report.StartTime, injections: map[string]string{}}
		odfFenceTargets[nodeName] = target
	}
	target.spec = report.FullText()
	target.completed = false

	return target
}

func confirmODFFaultCleared(injectorPodName string) {
	for _, target := range odfFenceTargets {
		delete(target.injections, injectorPodName)
	}
}

// odfBlocklistTimeLayout is the timestamp format of the "until" field.
const odfBlocklistTimeLayout = "2006-01-02T15:04:05.999999-0700"

// clearStaleODFFences works around DFBUGS-11731 during later specs' storage setup.
// Only a completed spec's recovered target is eligible. A still-active injected
// fault or remediation must not be unfenced. Polling is throttled independently
// of readiness polling, so cooldown and transient cleanup delays can resolve.
// Remove this workaround once DFBUGS-11731 is fixed in the tested ODF version.
func clearStaleODFFences() {
	if len(odfFenceTargets) == 0 || time.Now().Before(nextODFFenceCheck) {
		return
	}
	nextODFFenceCheck = time.Now().Add(sbrparams.StaleODFFenceRetryInterval)
	ctx, cancel := context.WithTimeout(context.Background(), sbrparams.StaleODFFenceTimeout)
	defer cancel()

	rookPods, err := pod.List(APIClient, sbrparams.ODFNamespace,
		metav1.ListOptions{LabelSelector: sbrparams.RookOperatorPodLabelSelector})
	var rookPod *pod.Builder

	for _, candidate := range rookPods {
		if candidate.Object.Status.Phase == corev1.PodRunning && candidate.Object.DeletionTimestamp == nil {
			rookPod = candidate

			break
		}
	}

	if err != nil || rookPod == nil {
		GinkgoWriter.Printf("Stale ODF fence check skipped: no running Rook operator pod in %s (err: %v)\n",
			sbrparams.ODFNamespace, err)

		return
	}
	cephCmd := func(args ...string) (string, error) {
		cmd := append([]string{"ceph", "--conf", sbrparams.RookCephConfig, "--connect-timeout", "30"}, args...)
		out, execErr := rookPod.ExecCommandWithTimeout(cmd, sbrparams.StaleODFFenceTimeout,
			sbrparams.RookOperatorContainer)

		return out.String(), execErr
	}

	fencedAddrs := staleFenceCandidates(ctx)
	GinkgoWriter.Printf("ODF fence recovery (DFBUGS-11731): eligible workers=%v\n", fencedAddrs)
	if len(fencedAddrs) == 0 {
		return
	}

	out, err := cephCmd("osd", "blocklist", "ls", "--format", "json")
	if err != nil {
		GinkgoWriter.Printf("Warning: stale ODF fence check: ceph osd blocklist ls: %v: %s\n", err, out)

		return
	}

	GinkgoWriter.Printf("ODF fence recovery: blocklist reply (first 8192 bytes): %.8192s\n", out)
	entries, parsed := parseODFBlocklist(out)
	if !parsed {
		GinkgoWriter.Printf("Warning: stale ODF fence check: cannot parse blocklist: %q\n", out)

		return
	}

	GinkgoWriter.Printf("ODF fence recovery: decoded %d blocklist entries\n", len(entries))
	for _, entry := range entries {
		if entry.Range == "" {
			continue
		}
		nodeName, ok := fencedAddrs[entry.Range]
		if !ok {
			GinkgoWriter.Printf("ODF fence recovery: keeping range %s until %s (not an eligible test target)\n",
				entry.Range, entry.Until)

			continue
		}
		recoverODFFence(ctx, cephCmd, nodeName, entry)
	}
}

func odfFenceAge(until string, testStart time.Time) (time.Duration, error) {
	expiry, err := time.Parse(odfBlocklistTimeLayout, until)
	if err != nil {
		return 0, fmt.Errorf("parse expiry %q: %w", until, err)
	}
	age := sbrparams.ODFAutoFenceExpiry - time.Until(expiry)
	if age < sbrparams.ODFAutoFenceCooldown || age > sbrparams.StaleODFFenceMaxAge ||
		expiry.Add(-sbrparams.ODFAutoFenceExpiry).Before(testStart.Add(-time.Minute)) {
		return age, fmt.Errorf("age=%s, test start=%s, cooldown=%s: outside recovery window",
			age, testStart, sbrparams.ODFAutoFenceCooldown)
	}

	return age, nil
}

func recoverODFFence(
	ctx context.Context,
	cephCmd func(...string) (string, error),
	nodeName string,
	entry odfBlocklistEntry,
) {
	age, ageErr := odfFenceAge(entry.Until, odfFenceTargets[nodeName].started)
	if ageErr != nil {
		GinkgoWriter.Printf("ODF fence recovery: keeping range %s for %s: %v\n", entry.Range, nodeName, ageErr)

		return
	}
	if recoverErr := odfFenceNodeRecovered(ctx, nodeName); recoverErr != nil {
		GinkgoWriter.Printf("ODF fence recovery: keeping range %s for %s: %v\n", entry.Range, nodeName, recoverErr)

		return
	}

	cidr := strings.Replace(entry.Range, ":0/", "/", 1)
	GinkgoWriter.Printf("ODF fence recovery: removing range %s for %s (until=%s, age=%s)\n",
		cidr, nodeName, entry.Until, age)
	rmOut, rmErr := cephCmd("osd", "blocklist", "range", "rm", cidr)
	GinkgoWriter.Printf("ODF fence recovery: removal result for %s: error=%v output=%.8192s\n", cidr, rmErr, rmOut)
	if rmErr == nil {
		var remaining string
		remaining, rmErr = cephCmd("osd", "blocklist", "ls", "--format", "json")
		if rmErr == nil {
			rmErr = verifyODFFenceRemoved(remaining, entry.Range)
		}
		GinkgoWriter.Printf("ODF fence recovery: read-back for %s: error=%v reply=%.8192s\n", cidr, rmErr, remaining)
	}
	if rmErr != nil {
		AddReportEntry("odf-stale-fence-removal-failed (DFBUGS-11731)",
			fmt.Sprintf("node %s fence %s: %v", nodeName, cidr, rmErr))

		return
	}

	GinkgoWriter.Printf("Removed stale ODF fence %s (until %s) for node %s (DFBUGS-11731)\n",
		cidr, entry.Until, nodeName)
	AddReportEntry("odf-stale-fence-removed (DFBUGS-11731)",
		fmt.Sprintf("node %s fence %s until %s", nodeName, cidr, entry.Until))
}

func verifyODFFenceRemoved(out, fenceRange string) error {
	entries, parsed := parseODFBlocklist(out)
	if !parsed {
		return fmt.Errorf("cannot decode the blocklist read-back")
	}
	for _, entry := range entries {
		if entry.Range == fenceRange {
			return fmt.Errorf("range %s remains in the blocklist", fenceRange)
		}
	}

	return nil
}

func odfFenceNodeRecovered(ctx context.Context, nodeName string) error {
	node, err := APIClient.CoreV1Interface.Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("recheck node: %w", err)
	}
	target := odfFenceTargets[nodeName]
	if target == nil || !target.completed {
		return fmt.Errorf("fault spec has not completed")
	}
	for injector, bootID := range target.injections {
		if bootID == "" || node.Status.NodeInfo.BootID == "" || bootID == node.Status.NodeInfo.BootID {
			return fmt.Errorf("injector %s cleanup is unconfirmed and no reboot is recorded", injector)
		}
	}
	if !isNodeReady(node) || node.Spec.Unschedulable || hasOutOfServiceTaint(node) {
		return fmt.Errorf("node is not recovered (Ready=%t, cordoned=%t, taints=%v, bootID=%s)",
			isNodeReady(node), node.Spec.Unschedulable, node.Spec.Taints, node.Status.NodeInfo.BootID)
	}
	remediations := &unstructured.UnstructuredList{}
	remediations.SetGroupVersionKind(schema.GroupVersionKind{
		Group: sbrparams.CRDGroup, Version: sbrparams.CRDVersion, Kind: "StorageBasedRemediationList",
	})
	if err := APIClient.List(ctx, remediations); err != nil {
		return fmt.Errorf("check outstanding remediations: %w", err)
	}
	for _, remediation := range remediations.Items {
		if remediation.GetName() == nodeName {
			return fmt.Errorf("StorageBasedRemediation %s/%s still exists", remediation.GetNamespace(), nodeName)
		}
	}
	nhcs, err := nhcsUsingSBRTemplate(ctx)
	if err != nil {
		return err
	}
	if len(nhcs) != 0 {
		return fmt.Errorf("NodeHealthChecks can still start SBR remediation: %v", nhcs)
	}

	return nil
}

// parseODFBlocklist decodes "ceph osd blocklist ls --format json" output. Depending on
// the Ceph release, range entries can follow the address entries as a second array or
// sit in an object field, so every JSON value in the output is decoded and merged.
func parseODFBlocklist(out string) ([]odfBlocklistEntry, bool) {
	start := strings.IndexAny(out, "[{")
	if start < 0 {
		return nil, false
	}

	var entries []odfBlocklistEntry

	parsed := false
	decoder := json.NewDecoder(strings.NewReader(out[start:]))

	for decoder.More() {
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			// Pod exec uses a TTY, so Ceph's stderr status can follow stdout JSON.
			trailer := strings.Fields(out[start+int(decoder.InputOffset()):])
			if !parsed || len(trailer) != 3 || trailer[0] != "listed" || trailer[2] != "entries" {
				return nil, false
			}
			count, err := strconv.Atoi(trailer[1])
			if err != nil || count != len(entries) {
				return nil, false
			}

			break
		}

		var list []odfBlocklistEntry
		if json.Unmarshal(value, &list) == nil {
			entries = append(entries, list...)
			parsed = true

			continue
		}

		var fields map[string]json.RawMessage
		if json.Unmarshal(value, &fields) != nil {
			continue
		}

		for _, field := range fields {
			if json.Unmarshal(field, &list) == nil {
				entries = append(entries, list...)
				parsed = true
			}
		}
	}

	for _, entry := range entries {
		if entry.Addr == "" && entry.Range == "" {
			return nil, false
		}
	}

	return entries, parsed
}

// staleFenceCandidates maps only completed fault specs' recovered worker addresses.
func staleFenceCandidates(ctx context.Context) map[string]string {
	nodes, err := APIClient.CoreV1Interface.Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/worker",
	})
	if err != nil {
		GinkgoWriter.Printf("Warning: stale ODF fence check: list worker nodes: %v\n", err)

		return nil
	}

	candidates := make(map[string]string)

	for nodeIdx := range nodes.Items {
		node := &nodes.Items[nodeIdx]
		if odfFenceTargets[node.Name] == nil {
			continue
		}
		nodeID, parseErr := strconv.Atoi(node.Annotations["k8s.ovn.org/node-id"])
		if parseErr != nil || nodeID < 1 || nodeID > 254 {
			GinkgoWriter.Printf("ODF fence recovery: skipping %s (unsupported OVN node ID %q)\n",
				node.Name, node.Annotations["k8s.ovn.org/node-id"])

			continue
		}
		if recoverErr := odfFenceNodeRecovered(ctx, node.Name); recoverErr != nil {
			GinkgoWriter.Printf("ODF fence recovery: skipping %s: %v\n", node.Name, recoverErr)

			continue
		}

		candidates[fmt.Sprintf("100.64.0.%d:0/32", nodeID)] = node.Name
	}

	return candidates
}

// isNodeReady reports whether the node's Ready condition is True.
func isNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

// hasOutOfServiceTaint reports whether the node has the out-of-service taint.
func hasOutOfServiceTaint(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == corev1.TaintNodeOutOfService {
			return true
		}
	}

	return false
}

// injectionBootIDs records, per injector pod, the target node and its boot ID when
// iptables rules were injected. The rules are not persistent, so a later boot ID
// change proves a reboot already removed them.
var injectionBootIDs = map[string][2]string{}

// recordInjectionBootID remembers the node's current boot ID for the injector pod.
func recordInjectionBootID(injectorPodName, nodeName string) {
	delete(injectionBootIDs, injectorPodName)
	target := recordODFFenceTarget(nodeName)
	target.injections[injectorPodName] = ""
	bootID, err := getNodeBootID(nodeName)
	if err != nil || bootID == "" {
		GinkgoWriter.Printf("Warning: cannot record boot ID of node %s before rule injection: %v\n", nodeName, err)

		return
	}

	injectionBootIDs[injectorPodName] = [2]string{nodeName, bootID}
	target.injections[injectorPodName] = bootID
}

// nodeRebootedSinceInjection reports whether the node the injector pod targeted has a
// different boot ID than at injection time, which means the reboot cleared the rules.
func nodeRebootedSinceInjection(injectorPodName string) bool {
	recorded, ok := injectionBootIDs[injectorPodName]
	if !ok {
		return false
	}

	currentBootID, err := getNodeBootID(recorded[0])
	if err != nil || currentBootID == "" || currentBootID == recorded[1] {
		return false
	}

	GinkgoWriter.Printf("Injector pod %s is absent and node %s rebooted (boot ID %s -> %s); "+
		"the injected rules were cleared by the reboot\n", injectorPodName, recorded[0], recorded[1], currentBootID)

	return true
}

// injectionTargetNode returns the node the injector pod targeted, or "" when unknown.
func injectionTargetNode(injectorPodName string) string {
	return injectionBootIDs[injectorPodName][0]
}

// nodeIsDown reports whether the node's Ready condition is not True. A node that is
// mid-reboot keeps its previous boot ID until kubelet posts the new one after coming
// back, so a boot-ID check during that window cannot yet see the reboot.
func nodeIsDown(nodeName string) bool {
	if nodeName == "" {
		return false
	}

	node, err := APIClient.CoreV1Interface.Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return false
	}

	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status != corev1.ConditionTrue
		}
	}

	return false
}

// waitForInjectionReboot waits for a reboot that is still in progress to finish and
// change the target node's boot ID, returning true as soon as
// nodeRebootedSinceInjection can confirm the change. A node that is down only updates
// its boot ID after kubelet comes back, so the check is retried within
// sbrparams.InjectorRuleCleanupTimeout instead of reporting an unconfirmed removal
// for a reboot that has not landed yet.
func waitForInjectionReboot(injectorPodName string) bool {
	if nodeRebootedSinceInjection(injectorPodName) {
		return true
	}

	nodeName := injectionTargetNode(injectorPodName)
	if nodeName == "" || !nodeIsDown(nodeName) {
		return false
	}

	GinkgoWriter.Printf("Injector pod %s is absent and node %s is rebooting; "+
		"waiting up to %s for its boot ID to change\n",
		injectorPodName, nodeName, sbrparams.InjectorRuleCleanupTimeout)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), sbrparams.InjectorRuleCleanupTimeout)
	defer waitCancel()

	rebooted := false
	_ = wait.PollUntilContextTimeout(waitCtx, sbrparams.DefaultPollInterval,
		sbrparams.InjectorRuleCleanupTimeout, true,
		func(context.Context) (bool, error) {
			if nodeRebootedSinceInjection(injectorPodName) {
				rebooted = true

				return true, nil
			}

			// A node that came back without a boot ID change has no reboot pending.
			return !nodeIsDown(nodeName), nil
		})

	return rebooted
}

// sbrcConcurrentWriteable reports status.storageValidation.concurrentWriteable on the SBRC, the gate
// the reconciler reads before it will fence anything. The operator sets it once min(2, desired)
// agents are Ready; the status write can lag the DaemonSet becoming ready, so callers poll it rather
// than read it once.
func sbrcConcurrentWriteable(sbrcName string) (bool, error) {
	sbrc := buildSBRC(sbrcName, map[string]interface{}{})
	if getErr := APIClient.Get(context.TODO(),
		types.NamespacedName{Name: sbrcName, Namespace: medik8sparams.OperatorNs}, sbrc); getErr != nil {
		return false, fmt.Errorf("get SBRC %s: %w", sbrcName, getErr)
	}

	writeable, _, nestedErr := unstructured.NestedBool(
		sbrc.Object, "status", "storageValidation", "concurrentWriteable")

	return writeable, nestedErr
}

// startPrivilegedInjectorPod starts a long-running privileged host-PID pod on nodeName for callers
// that need to exec into the host namespaces. A pod left behind by a previous run is removed first.
func startPrivilegedInjectorPod(nodeName, podName string) (*pod.Builder, error) {
	deleteHostCommandPod(podName)

	return pod.NewBuilder(APIClient, podName, medik8sparams.OperatorNs, sbrparams.InjectorImage).
		DefineOnNode(nodeName).
		WithHostPid(true).
		WithPrivilegedFlag().
		CreateAndWaitUntilRunning(medik8sparams.DefaultTimeout)
}

// sysRqTriggerWritable probes, through the injector pod, that nsenter into PID 1 works and that
// /proc/sysrq-trigger is writable on the host. The probe output is returned alongside the error so
// the caller can report it.
func sysRqTriggerWritable(injectorPod *pod.Builder) (string, error) {
	probeOut, probeErr := injectorPod.ExecCommand([]string{
		"nsenter", "-t", "1", "--pid", "--mount", "--", "sh", "-c",
		"test -w /proc/sysrq-trigger",
	})

	return probeOut.String(), probeErr
}

// fireSysRqKernelPanic crashes the injector pod's host node through the SysRq trigger. The exec is
// fire-and-forget: the node reboots immediately, so the command never returns cleanly.
func fireSysRqKernelPanic(injectorPod *pod.Builder) {
	_, _ = injectorPod.ExecCommand([]string{
		"nsenter", "-t", "1", "--pid", "--mount", "--", "sh", "-c",
		"echo 1 > /proc/sys/kernel/sysrq && echo c > /proc/sysrq-trigger",
	})
}

// uncordonNodeIfNeeded clears the unschedulable flag on a node that is still cordoned. Failures are
// logged rather than asserted so teardown never masks the test result.
func uncordonNodeIfNeeded(nodeName string) {
	node, getErr := APIClient.CoreV1Interface.Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if getErr != nil {
		GinkgoWriter.Printf("Warning: get node %s to uncordon: %v\n", nodeName, getErr)

		return
	}

	if !node.Spec.Unschedulable {
		return
	}

	patch := []byte(`{"spec":{"unschedulable":false}}`)
	if _, patchErr := APIClient.CoreV1Interface.Nodes().Patch(
		context.TODO(), nodeName, types.MergePatchType, patch, metav1.PatchOptions{}); patchErr != nil {
		GinkgoWriter.Printf("Warning: uncordon node %s: %v\n", nodeName, patchErr)
	}
}

// fencingProven returns nil once the victim's StorageBasedRemediation CR reports both
// FencingSucceeded=True and Succeeded=True. The CR is looked up by node name on every call because it
// can be deleted and recreated mid-remediation when the node's SBRStorageUnhealthy condition cycles to
// Unknown; a transient NotFound is therefore just "keep waiting". The CR persists after the conditions
// are set (NHC only deletes it once the node recovers), so there is no "proven then deleted" race to
// special-case here. Intended as an Eventually body.
func fencingProven(nodeName string) error {
	sbrCR, pullErr := pullSBRCR(nodeName)
	if pullErr != nil {
		if k8serrors.IsNotFound(pullErr) {
			return fmt.Errorf("StorageBasedRemediation/%s not present yet "+
				"(it may be recreated as the node's condition cycles)", nodeName)
		}

		return fmt.Errorf("get StorageBasedRemediation/%s: %w", nodeName, pullErr)
	}

	if conditionTrue(sbrCR, sbrparams.FencingSucceededCondition) &&
		conditionTrue(sbrCR, sbrparams.SucceededCondition) {
		return nil
	}

	return fmt.Errorf("StorageBasedRemediation/%s: %s and %s are not both True yet",
		nodeName, sbrparams.FencingSucceededCondition, sbrparams.SucceededCondition)
}

// conditionTrue reports whether the named status condition on an SBR CR is present and True.
func conditionTrue(sbrCR *unstructured.Unstructured, condType string) bool {
	cond := getSBRCRCondition(sbrCR, condType)
	if cond == nil {
		return false
	}

	status, _, _ := unstructured.NestedString(cond, "status")

	return status == string(corev1.ConditionTrue)
}
