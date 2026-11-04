package sbr

import (
	"context"
	"runtime"
	"testing"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/internal/reporter"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	_ "github.com/medik8s/system-tests/tests/sbr-operator/tests"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _, currentFile, _, _ = runtime.Caller(0)

func TestSBR(t *testing.T) {
	_, reporterConfig := GinkgoConfiguration()
	reporterConfig.JUnitReport = Medik8sConfig.GetJunitReportPath(currentFile)
	RegisterFailHandler(Fail)
	RunSpecs(t, "SBR", Label(sbrparams.Labels...), reporterConfig)
}

var _ = BeforeSuite(func() {
	By("Checking if Self Node Remediation (SNR) is installed and running")

	// The SNR manager Deployment, not the SNR CRD. Under the workload-availability-operator
	// every operand CRD is installed with the bundle and stays installed whether the operand
	// is enabled or not, so a CRD check reports SNR as present on every WAO cluster. The
	// Deployment belongs to SNR itself and so answers the question on either layout.
	snrRunning, snrStatus, err := helpers.WAOComponentRunning(
		context.TODO(), APIClient, medik8sparams.WAOComponentSNR)
	if err != nil {
		GinkgoWriter.Printf("⚠ Warning: could not check whether SNR is running: %v\n", err)

		return
	}

	if !snrRunning {
		GinkgoWriter.Printf("✓ SNR is NOT running (%s)\n", snrStatus)
		GinkgoWriter.Printf("  SBR tests can proceed without SNR config validation\n")

		return
	}

	GinkgoWriter.Printf("✓ SNR is installed and running (%s)\n", snrStatus)

	// SNR is running, check for SelfNodeRemediationConfig
	By("Checking SNR SelfNodeRemediationConfig for isSoftwareRebootEnabled setting")

	snrcList := &unstructured.UnstructuredList{}
	snrcList.SetAPIVersion("self-node-remediation.medik8s.io/v1alpha1")
	snrcList.SetKind("SelfNodeRemediationConfigList")
	err = APIClient.List(context.TODO(), snrcList)
	if err != nil {
		GinkgoWriter.Printf("⚠ Warning: could not list SelfNodeRemediationConfigs: %v\n", err)

		return
	}

	if len(snrcList.Items) == 0 {
		GinkgoWriter.Printf("⚠ No SelfNodeRemediationConfig found in cluster\n")

		return
	}

	// Check each config
	for idx := range snrcList.Items {
		config := &snrcList.Items[idx]
		configName := config.GetName()
		configNs := config.GetNamespace()
		isSoftwareRebootEnabled, found, err := unstructured.NestedBool(
			config.Object, "spec", "isSoftwareRebootEnabled")
		if err != nil {
			GinkgoWriter.Printf("⚠ Warning: could not read isSoftwareRebootEnabled from %s/%s: %v\n",
				configNs, configName, err)

			continue
		}

		if !found {
			GinkgoWriter.Printf("ℹ SelfNodeRemediationConfig %s/%s: isSoftwareRebootEnabled field not set "+
				"(defaults to true)\n", configNs, configName)
			GinkgoWriter.Printf("  RECOMMENDATION: Set spec.isSoftwareRebootEnabled: false to avoid conflicts " +
				"with SBR watchdog-based fencing\n")

			continue
		}

		if isSoftwareRebootEnabled {
			GinkgoWriter.Printf("⚠ WARNING: SelfNodeRemediationConfig %s/%s has isSoftwareRebootEnabled: true\n",
				configNs, configName)
			GinkgoWriter.Printf("  This may interfere with SBR watchdog-based fencing tests\n")
			GinkgoWriter.Printf("  RECOMMENDATION: Set spec.isSoftwareRebootEnabled: false\n")
		} else {
			GinkgoWriter.Printf("✓ SelfNodeRemediationConfig %s/%s has isSoftwareRebootEnabled: false (correct for SBR tests)\n",
				configNs, configName)
		}
	}
})

var _ = JustAfterEach(func() {
	reporter.ReportIfFailed(
		CurrentSpecReport(), currentFile, sbrparams.ReporterNamespacesToDump, sbrparams.ReporterCRDsToDump)
})

var _ = ReportAfterSuite("", func(report Report) {
	reportxml.Create(
		report, Medik8sConfig.GetReportPath(), Medik8sConfig.TCPrefix)
})
