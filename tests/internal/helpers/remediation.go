package helpers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DeleteRemediationCR deletes an unstructured remediation CR by GVK and name,
// polling until the resource is gone or the timeout expires. Returns true when
// the CR is confirmed gone.
func DeleteRemediationCR(
	ctx context.Context, k8sClient client.Client,
	gvk schema.GroupVersionKind, name, namespace string,
	pollInterval, timeout time.Duration,
	logf func(string, ...interface{}),
) bool {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	key := client.ObjectKey{Name: name, Namespace: namespace}

	// lastErr keeps the most recent Get/Delete failure so a timeout reports why
	// deletion did not happen (for example an admission webhook denial).
	var lastErr error

	recordErr := func(pollCtx context.Context, err error) {
		// Errors caused by the poll deadline itself would hide the real cause.
		if IsPollDeadlineError(pollCtx, err) {
			return
		}

		if lastErr == nil || lastErr.Error() != err.Error() {
			logf("DeleteRemediationCR(%s %s): %v\n", gvk.Kind, name, err)
		}

		lastErr = err
	}

	if waitErr := wait.PollUntilContextTimeout(
		ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if err := k8sClient.Get(ctx, key, obj); err != nil {
				if k8serrors.IsNotFound(err) {
					return true, nil
				}

				recordErr(ctx, err)

				return false, permanentAPIError(err)
			}

			if delErr := k8sClient.Delete(ctx, obj); delErr != nil {
				if k8serrors.IsNotFound(delErr) {
					return true, nil
				}

				recordErr(ctx, delErr)

				return false, permanentAPIError(delErr)
			}

			return false, nil
		},
	); waitErr != nil {
		logf("Warning: %s %s not fully deleted within %s: %v (last API error: %v)\n",
			gvk.Kind, name, timeout, waitErr, lastErr)

		return false
	}

	return true
}

// permanentAPIError returns err when retrying cannot succeed (Forbidden or
// Unauthorized), which stops the poll early, and nil for errors worth retrying.
func permanentAPIError(err error) error {
	if k8serrors.IsForbidden(err) || k8serrors.IsUnauthorized(err) {
		return err
	}

	return nil
}

// IsPollDeadlineError reports whether err was caused by the poll context
// running out rather than by the API server. client-go's rate limiter fails
// with "would exceed context deadline" shortly before the deadline passes,
// so ctx.Err() alone does not catch it.
func IsPollDeadlineError(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "would exceed context deadline")
}

// GetLeaderPodName reads the leader election Lease and returns the leader pod
// name together with the raw holder identity, validating the <podname>_<uuid>
// holderIdentity format. Callers that only need the node name should use
// GetActiveControllerNode, which builds on this helper.
func GetLeaderPodName(
	ctx context.Context, k8sClient client.Client,
	leaseName, namespace string,
) (podName, identity string, err error) {
	lease := &coordinationv1.Lease{}

	if getErr := k8sClient.Get(ctx, client.ObjectKey{
		Name:      leaseName,
		Namespace: namespace,
	}, lease); getErr != nil {
		if k8serrors.IsNotFound(getErr) {
			return "", "", fmt.Errorf("controller lease %q not found in namespace %s",
				leaseName, namespace)
		}

		return "", "", fmt.Errorf("failed to get controller lease: %w", getErr)
	}

	if lease.Spec.HolderIdentity == nil {
		return "", "", fmt.Errorf("controller lease %q has no holder", leaseName)
	}

	identity = *lease.Spec.HolderIdentity
	podName, _, ok := strings.Cut(identity, "_")
	if !ok || podName == "" {
		return "", "", fmt.Errorf("unexpected leader holderIdentity format: %q", identity)
	}

	return podName, identity, nil
}

// GetActiveControllerNode returns the node name hosting the active controller
// pod by inspecting the leader election Lease.
func GetActiveControllerNode(
	ctx context.Context, k8sClient client.Client,
	leaseName, namespace string,
) (string, error) {
	podName, _, err := GetLeaderPodName(ctx, k8sClient, leaseName, namespace)
	if err != nil {
		return "", err
	}

	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: podName, Namespace: namespace}, pod); err != nil {
		return "", fmt.Errorf("failed to get leader pod %s: %w", podName, err)
	}

	if pod.Spec.NodeName == "" {
		return "", fmt.Errorf("leader pod %s is not scheduled to a node", podName)
	}

	return pod.Spec.NodeName, nil
}
