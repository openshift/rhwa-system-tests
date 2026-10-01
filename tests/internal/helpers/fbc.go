//nolint:wsl_v5 // Polling state is kept adjacent to the checks that update it.
package helpers

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/clients"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/olm"
	olmV1alpha1 "github.com/rh-ecosystem-edge/eco-goinfra/pkg/schemes/olm/operators/v1alpha1"
)

// OperatorOLMSpec identifies one operator's OLM resources.
type OperatorOLMSpec struct {
	Package          string
	SubscriptionName string
	Namespace        string
	CSVNamePattern   string
	Channel          string
}

// InstalledOperator records an installed CSV identity.
type InstalledOperator struct {
	CSVName string `json:"csvName"`
	Version string `json:"version"`
}

// UpgradeResult records the OLM transition observed by an FBC upgrade test.
type UpgradeResult struct {
	BaselineCSV      string `json:"baselineCSV"`
	BaselineVersion  string `json:"baselineVersion"`
	CandidateCSV     string `json:"candidateCSV"`
	CandidateVersion string `json:"candidateVersion"`
}

// PreparedFBC records the shared cluster state prepared for an FBC upgrade.
type PreparedFBC struct {
	CatalogName string `json:"catalogName"`
	IDMSChanged bool   `json:"idmsChanged"`
}

// EnsureNamespace creates the shared operator namespace when a test starts on a clean cluster.
func EnsureNamespace(ctx context.Context, apiClient *clients.Settings, name string) error {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := apiClient.Create(ctx, namespace); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", name, err)
	}

	return nil
}

// CreateCandidateCatalog creates a test-owned CatalogSource for a digest-pinned FBC image.
func CreateCandidateCatalog(
	apiClient *clients.Settings, name, image string,
) (*olm.CatalogSourceBuilder, error) {
	catalog := olm.NewCatalogSourceBuilder(
		apiClient, name, medik8sparams.GACatalogNamespace)
	if catalog == nil {
		return nil, fmt.Errorf("create candidate CatalogSource builder")
	}

	catalog.Definition.Spec.SourceType = olmV1alpha1.SourceTypeGrpc
	catalog.Definition.Spec.Image = image
	catalog.Definition.Spec.DisplayName = "RHWA FBC upgrade candidate"
	catalog.Definition.Spec.Publisher = "rhwa-system-tests"

	created, err := catalog.Create()
	if err != nil {
		return nil, fmt.Errorf("create candidate CatalogSource %s: %w", name, err)
	}

	return created, nil
}

// DeleteCandidateCatalog removes the test-owned candidate CatalogSource.
func DeleteCandidateCatalog(apiClient *clients.Settings, name string) error {
	catalog := olm.NewCatalogSourceBuilder(
		apiClient, name, medik8sparams.GACatalogNamespace)
	if catalog == nil {
		return fmt.Errorf("create candidate CatalogSource builder")
	}

	if !catalog.Exists() {
		return nil
	}

	if err := catalog.Delete(); err != nil {
		return fmt.Errorf("delete candidate CatalogSource %s: %w", name, err)
	}

	return nil
}

// WaitForCatalogReady waits until the candidate CatalogSource reports a READY gRPC connection.
func WaitForCatalogReady(
	ctx context.Context, apiClient *clients.Settings, name string, timeout, pollInterval time.Duration,
) error {
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(context.Context) (bool, error) {
			catalog, pullErr := olm.PullCatalogSource(
				apiClient, name, medik8sparams.GACatalogNamespace)
			if pullErr != nil {
				return false, nil
			}

			state := catalog.Object.Status.GRPCConnectionState
			if state == nil {
				return false, nil
			}

			return state.LastObservedState == "READY", nil
		})
	if err != nil {
		return fmt.Errorf("candidate CatalogSource %s did not become READY: %w", name, err)
	}

	return nil
}

// WaitForDeploymentImage requires a ready Deployment whose live, non-terminating
// pods all run the expected image in the named container. A source-built bundle
// may declare a unique tag, so the runtime image digest is also accepted.
func WaitForDeploymentImage(
	ctx context.Context,
	apiClient *clients.Settings,
	namespace, deploymentName, containerName, expectedImage string,
	timeout, pollInterval time.Duration,
) error {
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(context.Context) (bool, error) {
			controller, pullErr := deployment.Pull(apiClient, deploymentName, namespace)
			if pullErr != nil || !controller.IsReady(pollInterval) {
				return false, nil
			}

			pods := &corev1.PodList{}
			if listErr := apiClient.List(ctx, pods, client.InNamespace(namespace),
				client.MatchingLabels(controller.Object.Spec.Selector.MatchLabels)); listErr != nil {
				return false, nil
			}

			if len(pods.Items) == 0 {
				return false, nil
			}

			for _, pod := range pods.Items {
				if pod.DeletionTimestamp != nil {
					return false, nil
				}

				if !podContainerUsesImage(&pod, containerName, expectedImage) {
					return false, nil
				}
			}

			return true, nil
		})
	if err != nil {
		return fmt.Errorf("deployment %s/%s did not run %s=%s: %w",
			namespace, deploymentName, containerName, expectedImage, err)
	}

	return nil
}

func podContainerUsesImage(pod *corev1.Pod, containerName, expectedImage string) bool {
	for _, container := range pod.Spec.Containers {
		if container.Name != containerName {
			continue
		}

		if container.Image == expectedImage {
			return true
		}

		expectedDigest := imageDigest(expectedImage)
		if expectedDigest == "" {
			return false
		}

		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == containerName {
				return imageDigest(status.ImageID) == expectedDigest
			}
		}

		return false
	}

	return false
}

func imageDigest(reference string) string {
	const marker = "sha256:"

	index := strings.LastIndex(reference, marker)
	if index == -1 {
		return ""
	}

	digest := reference[index+len(marker):]
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return ""
	}

	return digest
}

// PrepareFBC applies an optional release IDMS, waits for any MCP rollout, then creates a ready CatalogSource.
func PrepareFBC(
	ctx context.Context,
	apiClient *clients.Settings,
	inputs medik8sparams.FBCUpgradeInputs,
	logf func(string, ...interface{}),
) (*PreparedFBC, error) {
	preIDMSGens, err := GetMCPGenerations(ctx)
	if err != nil {
		return nil, fmt.Errorf("capture MCP generations before IDMS apply: %w", err)
	}

	idmsChanged, err := ApplyIDMSFile(ctx, inputs.IDMSPath, logf)
	if err != nil {
		return nil, err
	}

	if idmsChanged {
		if err := WaitForMCPRollout(
			ctx,
			preIDMSGens,
			medik8sparams.MCPDetectionTimeout,
			medik8sparams.MCPRolloutTimeout,
			10*time.Second,
			logf,
		); err != nil {
			return nil, err
		}
	} else {
		logf("IDMS unchanged, skipping MachineConfigPool rollout wait\n")
	}

	if _, err := CreateCandidateCatalog(apiClient, inputs.CatalogName, inputs.CatalogImage); err != nil {
		return nil, err
	}

	if err := WaitForCatalogReady(
		ctx,
		apiClient,
		inputs.CatalogName,
		medik8sparams.OperatorUpgradeTimeout,
		10*time.Second,
	); err != nil {
		return nil, err
	}

	return &PreparedFBC{CatalogName: inputs.CatalogName, IDMSChanged: idmsChanged}, nil
}

// CaptureInstalledOperator returns the succeeded CSV matching an operator's configured pattern.
func CaptureInstalledOperator(
	apiClient *clients.Settings, spec OperatorOLMSpec,
) (InstalledOperator, error) {
	csv, err := FindSucceededCSV(apiClient, spec.CSVNamePattern, spec.Namespace)
	if err != nil {
		return InstalledOperator{}, err
	}

	return InstalledOperator{
		CSVName: csv.Object.Name,
		Version: csv.Object.Spec.Version.String(),
	}, nil
}

// WaitForInstalledOperator waits for the GA operator CSV to reach Succeeded.
func WaitForInstalledOperator(
	ctx context.Context,
	apiClient *clients.Settings,
	spec OperatorOLMSpec,
	timeout, pollInterval time.Duration,
) (InstalledOperator, error) {
	var installed InstalledOperator

	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(context.Context) (bool, error) {
			var captureErr error

			installed, captureErr = CaptureInstalledOperator(apiClient, spec)

			return captureErr == nil, nil
		})
	if err != nil {
		return InstalledOperator{}, fmt.Errorf(
			"operator %s did not reach a succeeded CSV: %w", spec.Package, err)
	}

	return installed, nil
}

// WaitForSubscriptionUpgrade waits for OLM to install the exact candidate from the target catalog.
//
//nolint:gocognit // OLM readiness requires checking each related status field together.
func WaitForSubscriptionUpgrade(
	ctx context.Context,
	apiClient *clients.Settings,
	spec OperatorOLMSpec,
	catalogName, expectedVersion string,
	baseline InstalledOperator,
	timeout, pollInterval time.Duration,
) (UpgradeResult, error) {
	var candidate InstalledOperator

	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(context.Context) (bool, error) {
			sub, pullErr := olm.PullSubscription(apiClient, spec.SubscriptionName, spec.Namespace)
			if pullErr != nil || sub == nil || sub.Object == nil {
				return false, nil
			}

			if sub.Object.Spec.CatalogSource != catalogName {
				return false, nil
			}

			for _, condition := range sub.Object.Status.Conditions {
				if condition.Type == olmV1alpha1.SubscriptionCatalogSourcesUnhealthy &&
					condition.Status == corev1.ConditionTrue {
					return false, nil
				}
			}

			currentCSV := sub.Object.Status.CurrentCSV
			if currentCSV == "" || sub.Object.Status.InstalledCSV != currentCSV {
				return false, nil
			}

			catalogHealthy := false
			for _, health := range sub.Object.Status.CatalogHealth {
				if health.CatalogSourceRef != nil &&
					health.CatalogSourceRef.Name == catalogName && health.Healthy {
					catalogHealthy = true

					break
				}
			}
			if !catalogHealthy {
				return false, nil
			}

			csv, csvErr := olm.PullClusterServiceVersion(apiClient, currentCSV, spec.Namespace)
			if csvErr != nil {
				return false, nil
			}

			phase, phaseErr := csv.GetPhase()
			if phaseErr != nil || phase != olmV1alpha1.CSVPhaseSucceeded {
				return false, nil
			}

			version := csv.Object.Spec.Version.String()
			if version != expectedVersion {
				return false, nil
			}

			candidate = InstalledOperator{CSVName: currentCSV, Version: version}

			return true, nil
		})
	if err != nil {
		return UpgradeResult{}, fmt.Errorf(
			"subscription %s/%s did not install candidate version %s from catalog %s: %w",
			spec.Namespace, spec.SubscriptionName, expectedVersion, catalogName, err)
	}

	if candidate.CSVName == baseline.CSVName {
		return UpgradeResult{}, fmt.Errorf(
			"candidate CSV %s is unchanged from baseline", candidate.CSVName)
	}

	if candidate.Version == baseline.Version {
		return UpgradeResult{}, fmt.Errorf(
			"candidate version %s is unchanged from baseline", candidate.Version)
	}

	return UpgradeResult{
		BaselineCSV:      baseline.CSVName,
		BaselineVersion:  baseline.Version,
		CandidateCSV:     candidate.CSVName,
		CandidateVersion: candidate.Version,
	}, nil
}
