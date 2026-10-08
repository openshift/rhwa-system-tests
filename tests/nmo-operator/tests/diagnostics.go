package tests

import (
	"context"

	. "github.com/onsi/ginkgo/v2"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoparams"
)

// NodeMaintenance is cluster-scoped and exposes status fields, not conditions.
func logNMORemediationDiagnostics(ctx context.Context, names ...string) {
	helpers.LogRemediationDiagnostics(ctx, APIClient, helpers.DiagnosticResourcesFor(nmGVK, "", names...),
		helpers.ControllerDiagnostics{
			Namespace: medik8sparams.OperatorNs, LeaseName: nmoparams.ControllerLeaseName,
			ContainerName: nmoparams.ManagerContainerName, TailLines: nmoparams.DiagnosticsLogTailLines,
		}, GinkgoWriter.Printf)
}
