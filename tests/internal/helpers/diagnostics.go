package helpers

import (
	"context"
	"encoding/json"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DiagnosticsRequestTimeout bounds each CR status read and the controller log
// collection (lease lookup, pod resolution, and log stream) independently.
const DiagnosticsRequestTimeout = 15 * time.Second

// DiagnosticClient provides CR/lease reads and pod log access.
type DiagnosticClient interface {
	client.Client
	corev1client.PodsGetter
}

// DiagnosticResource identifies a CR to inspect before cleanup deletes it.
type DiagnosticResource struct {
	GVK schema.GroupVersionKind
	Key client.ObjectKey
}

// DiagnosticResourcesFor builds one DiagnosticResource per name for a single kind.
// Pass an empty namespace for cluster-scoped kinds.
func DiagnosticResourcesFor(gvk schema.GroupVersionKind, namespace string, names ...string) []DiagnosticResource {
	resources := make([]DiagnosticResource, 0, len(names))

	for _, name := range names {
		resources = append(resources, DiagnosticResource{
			GVK: gvk, Key: client.ObjectKey{Name: name, Namespace: namespace},
		})
	}

	return resources
}

// ControllerDiagnostics identifies the controller's election lease and log container.
type ControllerDiagnostics struct {
	Namespace     string
	LeaseName     string
	ContainerName string
	TailLines     int64
}

// LogRemediationDiagnostics collects CR state and current leader logs independently.
// Collection remains bounded even when the failed spec's context is cancelled.
func LogRemediationDiagnostics(
	ctx context.Context, apiClient DiagnosticClient,
	resources []DiagnosticResource, controller ControllerDiagnostics,
	logf func(string, ...interface{}),
) {
	for _, resource := range resources {
		if resource.Key.Name == "" {
			continue
		}

		diagCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DiagnosticsRequestTimeout)
		LogCRStatus(diagCtx, apiClient, resource.GVK, resource.Key, logf)
		cancel()
	}

	diagCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DiagnosticsRequestTimeout)
	defer cancel()
	LogActiveControllerLogs(diagCtx, apiClient, controller, logf)
}

// LogCRStatus fetches fresh status, including conditions and non-condition fields.
// It never asserts or dumps the CR spec, which may contain credentials.
func LogCRStatus(
	ctx context.Context, k8sClient client.Client,
	gvk schema.GroupVersionKind, key client.ObjectKey,
	logf func(string, ...interface{}),
) {
	if key.Name == "" {
		return
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)

	if err := k8sClient.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			logf("%s %s not present\n", gvk.Kind, key)

			return
		}

		logf("WARNING: could not fetch %s %s for diagnostics: %v\n", gvk.Kind, key, err)

		return
	}

	status, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil {
		logf("WARNING: could not read %s %s status: %v\n", gvk.Kind, key, err)

		return
	}

	if !found {
		logf("%s %s (UID=%s) has no status yet\n", gvk.Kind, key, obj.GetUID())

		return
	}

	statusJSON, err := json.Marshal(status)
	if err != nil {
		logf("WARNING: could not format %s %s status: %v\n", gvk.Kind, key, err)

		return
	}

	logf("%s %s (UID=%s) status: %s\n", gvk.Kind, key, obj.GetUID(), statusJSON)
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		logf("WARNING: could not read %s %s conditions: %v\n", gvk.Kind, key, err)

		return
	}

	if !found {
		return
	}

	for _, condition := range conditions {
		fields, ok := condition.(map[string]interface{})
		if !ok {
			logf("WARNING: malformed condition on %s %s: %v\n", gvk.Kind, key, condition)

			continue
		}

		logf("  type=%v status=%v reason=%v message=%q\n",
			fields["type"], fields["status"], fields["reason"], fields["message"])
	}
}

// LogActiveControllerLogs prints a server-side log tail from the exact lease holder.
// The current leader may differ from the replica that reconciled before failover.
func LogActiveControllerLogs(
	ctx context.Context, apiClient DiagnosticClient,
	controller ControllerDiagnostics, logf func(string, ...interface{}),
) {
	podName, identity, err := GetLeaderPodName(ctx, apiClient, controller.LeaseName, controller.Namespace)
	if err != nil {
		logf("WARNING: could not resolve controller lease %s/%s: %v\n",
			controller.Namespace, controller.LeaseName, err)

		return
	}

	logf("Controller lease %s/%s holder=%s; requesting last %d lines from %s/%s container=%s\n",
		controller.Namespace, controller.LeaseName, identity, controller.TailLines,
		controller.Namespace, podName, controller.ContainerName)
	stream, err := apiClient.Pods(controller.Namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: controller.ContainerName,
		TailLines: &controller.TailLines,
	}).Stream(ctx)
	if err != nil {
		logf("WARNING: could not fetch controller pod %s logs: %v\n", podName, err)

		return
	}

	defer stream.Close()
	logs, err := io.ReadAll(stream)
	if err != nil {
		logf("WARNING: could not read controller pod %s logs: %v\n", podName, err)

		return
	}

	logf("Controller pod %s log tail:\n%s\n", podName, logs)
}

// LogControllerState lists controller pods matching the given labels and logs
// their phase, node, and readiness for debugging.
func LogControllerState(
	ctx context.Context, k8sClient client.Client,
	namespace string, podLabels map[string]string,
	logf func(string, ...interface{}),
) {
	pods := &corev1.PodList{}

	if err := k8sClient.List(ctx, pods,
		client.InNamespace(namespace),
		client.MatchingLabels(podLabels)); err != nil {
		logf("WARNING: could not list controller pods: %v\n", err)

		return
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		ready := false

		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = true

				break
			}
		}

		logf("Controller pod %s: Phase=%s, Node=%s, Ready=%v\n",
			pod.Name, pod.Status.Phase, pod.Spec.NodeName, ready)
	}
}
