package tests

import (
	"context"

	. "github.com/onsi/ginkgo/v2"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrparams"
)

// Collect before deleting NHC, which may remove the SNR CRs it owns.
func logSNRRemediationDiagnostics(ctx context.Context, nhcNames []string, nodeNames ...string) {
	resources := append(helpers.DiagnosticResourcesFor(snrGVK, medik8sparams.OperatorNs, nodeNames...),
		helpers.DiagnosticResourcesFor(nhcGVK, "", nhcNames...)...)
	helpers.LogRemediationDiagnostics(ctx, APIClient, resources,
		helpers.ControllerDiagnostics{
			Namespace: medik8sparams.OperatorNs, LeaseName: snrparams.ControllerLeaseName,
			ContainerName: snrparams.ManagerContainerName, TailLines: snrparams.DiagnosticsLogTailLines,
		}, GinkgoWriter.Printf)
}
