package tests

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
)

func logSBRRemediationDiagnostics(ctx context.Context, nodeNames ...string) {
	resources := helpers.DiagnosticResourcesFor(schema.GroupVersionKind{
		Group: sbrparams.CRDGroup, Version: sbrparams.CRDVersion, Kind: sbrparams.RemediationKind,
	}, medik8sparams.OperatorNs, nodeNames...)
	helpers.LogRemediationDiagnostics(ctx, APIClient, resources,
		helpers.ControllerDiagnostics{
			Namespace: medik8sparams.OperatorNs, LeaseName: sbrparams.ControllerLeaseName,
			ContainerName: sbrparams.ManagerContainerName, TailLines: sbrparams.DiagnosticsLogTailLines,
		}, GinkgoWriter.Printf)
}
