package snr

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/internal/reporter"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrparams"
	_ "github.com/medik8s/system-tests/tests/snr-operator/tests"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _, currentFile, _, _ = runtime.Caller(0)

func TestSNR(t *testing.T) {
	_, reporterConfig := GinkgoConfiguration()
	reporterConfig.JUnitReport = Medik8sConfig.GetJunitReportPath(currentFile)
	RegisterFailHandler(Fail)
	RunSpecs(t, "SNR", Label(snrparams.Labels...), reporterConfig)
}

// Under the workload-availability-operator, SNR is an operand that WAO installs only once it is
// enabled: OLM brings the CRDs and RBAC with WAO itself, but no self-node-remediation Deployment
// exists until spec.components.selfNodeRemediation.enabled is true. Enabling it is a precondition
// of the suite, not something any spec asserts, so it runs once here and the specs are unchanged.
//
// NHC is enabled alongside it for the same reason. The remediation specs never create a
// SelfNodeRemediation directly: they create NodeHealthCheck CRs, stop kubelet, and wait for the
// NHC controller to notice the NotReady node and instantiate SNR from its template. With the NHC
// operand disabled, those CRs are accepted -- WAO ships every CRD regardless of which operands
// are on -- and then nothing reconciles them, so no remediation is ever created and the node sits
// with a stopped kubelet until the spec times out and somebody reboots it by hand. Enabling the
// operand here is what turns that silent 45-minute hang into a working run.
//
// Both are preconditions of running the SNR suite at all, so neither is gated on the run's label
// filter. The filter says which specs the caller wants, not which layout the cluster uses, and a
// run selected with operator:snr alone needs the same two operands running as one that also asked
// for operator:wao. Gating on the filter is what let an operator:snr run start against a disabled
// NHC, stop kubelet on two nodes, and wait fifteen minutes for a remediation that nothing was
// going to create.
//
// What does decide whether there is anything to enable is the cluster: a pre-WAO cluster has no
// WorkloadAvailabilityConfig singleton, SNR and NHC are installed as their own operators, and
// patching a configuration that is not there would fail for the wrong reason. That shows up as
// helpers.ErrWAONotInstalled on the first call and the suite goes straight to the per-operator
// readiness check each Describe already makes.
//
// The operands are deliberately left enabled afterwards. Enabling is an installation step rather
// than state this suite created, and a disable is slow and can strand resources in
// DisableIncomplete for the next suite.
var _ = BeforeSuite(func() {
	ctx := context.Background()

	By("Ensuring the SNR operand is enabled, Available, configured and serving agents")

	snrErr := helpers.EnsureWAOOperandReady(ctx, APIClient, snrOperandReadiness())
	if errors.Is(snrErr, helpers.ErrWAONotInstalled) {
		GinkgoWriter.Printf(
			"No WorkloadAvailabilityConfig on this cluster: pre-WAO layout, leaving operands %s and %s alone\n",
			medik8sparams.WAOComponentSNR, medik8sparams.WAOComponentNHC)

		return
	}

	Expect(snrErr).To(Succeed(),
		"Operand %s must be fully running before the SNR suite starts",
		medik8sparams.WAOComponentSNR)

	By("Ensuring the NHC operand is enabled and Available to drive the remediation specs")
	Expect(helpers.EnsureWAOOperandReady(ctx, APIClient, nhcOperandReadiness())).
		To(Succeed(),
			"Operand %s must be fully running before the SNR suite starts: the remediation specs "+
				"create NodeHealthCheck CRs and rely on its controller to create the SelfNodeRemediation",
			medik8sparams.WAOComponentNHC)
})

// snrOperandReadiness is everything that must be running before an SNR spec may assume the
// operand works. Phase Available covers only the manager Deployment: the SNR controller then
// creates the default SelfNodeRemediationConfig and reconciles the agent DaemonSet from it,
// and specs that read either will fail if they run in between.
//
// Measured on a cold enable: Available at 0s, SelfNodeRemediationConfig at +8s, agent
// DaemonSet created at +12s and its pods ready at +17s. Waiting on the DaemonSet covers the
// whole chain -- it cannot exist before the configuration it is reconciled from -- but the
// configuration is listed too so that a failure names the step that actually stalled.
func snrOperandReadiness() helpers.WAOOperandReadiness {
	return helpers.WAOOperandReadiness{
		Component: medik8sparams.WAOComponentSNR,
		Configs: []helpers.WAOOperandConfigRef{{
			GVK: schema.GroupVersionKind{
				Group:   snrparams.CRDGroup,
				Version: snrparams.CRDVersion,
				Kind:    "SelfNodeRemediationConfig",
			},
			Namespace: medik8sparams.OperatorNs,
			Name:      snrparams.SNRConfigName,
		}},
		DaemonSets: []string{snrparams.AgentDaemonSetName},
	}
}

// nhcOperandReadiness is the dependency the remediation specs rely on rather than an operand
// this suite exercises, so it asks for less than snrOperandReadiness does. Phase Available
// already covers both Deployments WAO lists for the operand -- the controller manager and the
// console plugin -- and that controller is the whole requirement here.
//
// Nothing is listed under Configs. NHC's own default NodeHealthCheck is not a precondition: the
// specs create the NodeHealthCheck CRs they remediate through, and waiting on a resource no spec
// reads would only add a way for the suite to fail before it starts.
func nhcOperandReadiness() helpers.WAOOperandReadiness {
	return helpers.WAOOperandReadiness{
		Component: medik8sparams.WAOComponentNHC,
	}
}

// The post-run counterpart of the precondition, gated the same way so that whatever the
// precondition touched is what gets reported and swept. It does not disable the operand --
// enabling was an installation step, not state this suite created -- but it does report what the
// operand ended the run in, and sweeps up CRs the specs' own DeferCleanup did not manage to
// remove.
//
// That sweep matters more under WAO than it did before: a SelfNodeRemediation whose operand is
// disabled while the CR is still alive strands on its finalizer, because the controller that
// would remove it is gone. Finding one here names the problem while the run is still in view.
//
// Cleanup never fails the suite. Everything below logs and carries on.
var _ = AfterSuite(func() {
	if _, err := helpers.GetWAOConfig(context.Background(), APIClient); err != nil {
		return
	}

	By("Reporting the state the SNR operand ended the run in")
	reportSNROperand()

	By("Sweeping up SelfNodeRemediation CRs the suite left behind")
	sweepSNRTestCRs()
})

// reportSNROperand logs the operand's phase, anything a disable retained, and whether its
// manager is still running. A suite that failed late is much easier to read when the next
// question -- was the operand even up? -- is already answered.
func reportSNROperand() {
	config, err := helpers.GetWAOConfig(context.Background(), APIClient)
	if err != nil {
		GinkgoWriter.Printf("AfterSuite: could not read the WAO configuration: %v\n", err)

		return
	}

	phase, phaseErr := helpers.WAOComponentPhase(config, medik8sparams.WAOComponentSNR)
	if phaseErr != nil {
		GinkgoWriter.Printf("AfterSuite: could not read the operand phase: %v\n", phaseErr)
	} else {
		GinkgoWriter.Printf("AfterSuite: operand %s phase=%s (left enabled on purpose)\n",
			medik8sparams.WAOComponentSNR, phase)
	}

	retained, retainedErr := helpers.WAOComponentRetainedResources(config, medik8sparams.WAOComponentSNR)
	if retainedErr != nil {
		GinkgoWriter.Printf("AfterSuite: could not read retained resources: %v\n", retainedErr)
	} else if len(retained) > 0 {
		GinkgoWriter.Printf("AfterSuite: operand %s retained: %v\n", medik8sparams.WAOComponentSNR, retained)
	}

	running, status, runningErr := helpers.WAOComponentRunning(
		context.Background(), APIClient, medik8sparams.WAOComponentSNR)
	if runningErr != nil {
		GinkgoWriter.Printf("AfterSuite: could not read the manager Deployment: %v\n", runningErr)

		return
	}

	GinkgoWriter.Printf("AfterSuite: operand %s running=%v (%s)\n",
		medik8sparams.WAOComponentSNR, running, status)
}

// sweepSNRTestCRs deletes any SelfNodeRemediation this suite created and did not remove. It
// matches on snrparams.TestCRNamePrefix so a remediation the cluster is genuinely running is
// never touched, and it reports rather than forces a CR held by a finalizer: stripping one
// skips the controller's own cleanup, which is a decision for the person running the suite.
func sweepSNRTestCRs() {
	snrList := &unstructured.UnstructuredList{}
	snrList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   snrparams.CRDGroup,
		Version: snrparams.CRDVersion,
		Kind:    "SelfNodeRemediationList",
	})

	if err := APIClient.List(context.Background(), snrList,
		client.InNamespace(medik8sparams.OperatorNs)); err != nil {
		GinkgoWriter.Printf("AfterSuite: could not list SelfNodeRemediation CRs: %v\n", err)

		return
	}

	for idx := range snrList.Items {
		leftover := &snrList.Items[idx]
		if !strings.HasPrefix(leftover.GetName(), snrparams.TestCRNamePrefix) {
			continue
		}

		if finalizers := leftover.GetFinalizers(); len(finalizers) > 0 && leftover.GetDeletionTimestamp() != nil {
			GinkgoWriter.Printf(
				"AfterSuite: SelfNodeRemediation %q is stuck deleting on finalizers %v; "+
					"the operand was most likely disabled while it was alive. Re-enable it, or clear with "+
					"scripts/clear-remediation-state.sh --apply --force-finalizers\n",
				leftover.GetName(), finalizers)

			continue
		}

		GinkgoWriter.Printf("AfterSuite: deleting leftover SelfNodeRemediation %q\n", leftover.GetName())

		if err := APIClient.Delete(context.Background(), leftover); err != nil && !k8serrors.IsNotFound(err) {
			GinkgoWriter.Printf("AfterSuite: could not delete %q: %v\n", leftover.GetName(), err)
		}
	}
}

var _ = JustAfterEach(func() {
	reporter.ReportIfFailed(
		CurrentSpecReport(), currentFile, snrparams.ReporterNamespacesToDump, snrparams.ReporterCRDsToDump)
})

var _ = ReportAfterSuite("", func(report Report) {
	reportxml.Create(
		report, Medik8sConfig.GetReportPath(), Medik8sConfig.TCPrefix)
})
