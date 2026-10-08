package tests

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcparams"
)

// Capture NHC and the remediations involved in this scenario before cleanup.
func logNHCRemediationDiagnostics(ctx context.Context, nhcNames, nodeNames []string, kinds ...schema.GroupVersionKind) {
	resources := helpers.DiagnosticResourcesFor(nhcGVK, "", nhcNames...)

	for _, kind := range kinds {
		namespace := ""
		if kind == snrGVK {
			namespace = medik8sparams.OperatorNs
		}

		resources = append(resources, helpers.DiagnosticResourcesFor(kind, namespace, nodeNames...)...)
	}

	helpers.LogRemediationDiagnostics(ctx, APIClient, resources,
		helpers.ControllerDiagnostics{
			Namespace: medik8sparams.OperatorNs, LeaseName: nhcparams.ControllerLeaseName,
			ContainerName: nhcparams.ManagerContainerName, TailLines: nhcparams.DiagnosticsLogTailLines,
		}, GinkgoWriter.Printf)
}
