package snr

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
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
// It runs only when the label filter asks for the WAO layout — see helpers.WAORunRequested. On a
// pre-WAO cluster SNR is installed as its own operator and there is nothing to enable, so the
// suite goes straight to the per-operator readiness check each Describe already makes.
//
// The operand is deliberately left enabled afterwards. Enabling is an installation step rather
// than state this suite created, and a disable is slow and can strand resources in
// DisableIncomplete for the next suite.
var _ = BeforeSuite(func() {
	suiteConfig, _ := GinkgoConfiguration()
	if !helpers.WAORunRequested(suiteConfig.LabelFilter) {
		GinkgoWriter.Printf("%s not in the label filter: leaving operand %s alone\n",
			labels.OperatorWAO, medik8sparams.WAOComponentSNR)

		return
	}

	By("Ensuring the SNR operand is enabled and Available")
	Expect(helpers.EnsureWAOComponents(
		context.Background(), APIClient, medik8sparams.WAOComponentSNR)).To(Succeed(),
		"Operand %s must be enabled and Available before the SNR suite runs",
		medik8sparams.WAOComponentSNR)
})

// The post-run counterpart of the precondition, gated on the same label so a pre-WAO run is
// untouched. It does not disable the operand -- enabling was an installation step, not state
// this suite created -- but it does report what the operand ended the run in, and sweeps up
// CRs the specs' own DeferCleanup did not manage to remove.
//
// That sweep matters more under WAO than it did before: a SelfNodeRemediation whose operand is
// disabled while the CR is still alive strands on its finalizer, because the controller that
// would remove it is gone. Finding one here names the problem while the run is still in view.
//
// Cleanup never fails the suite. Everything below logs and carries on.
var _ = AfterSuite(func() {
	suiteConfig, _ := GinkgoConfiguration()
	if !helpers.WAORunRequested(suiteConfig.LabelFilter) {
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
