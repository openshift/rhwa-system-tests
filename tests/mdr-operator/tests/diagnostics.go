package tests

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/mdr-operator/internal/mdrparams"
)

func logMDRRemediationDiagnostics(ctx context.Context, nhcName, nodeName string) {
	resources := []helpers.DiagnosticResource{
		{GVK: mdrGVK, Key: client.ObjectKey{Name: nodeName, Namespace: medik8sparams.OperatorNs}},
		{GVK: nhcGVK, Key: client.ObjectKey{Name: nhcName}},
	}
	helpers.LogRemediationDiagnostics(ctx, APIClient, resources,
		helpers.ControllerDiagnostics{
			Namespace: medik8sparams.OperatorNs, LeaseName: mdrparams.ControllerLeaseName,
			ContainerName: mdrparams.ManagerContainerName, TailLines: mdrparams.DiagnosticsLogTailLines,
		}, GinkgoWriter.Printf)
}
