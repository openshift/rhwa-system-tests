package mustgather

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// GatherNamespacePrefix is the prefix `oc adm must-gather` gives the
	// temporary namespace it runs its pod in.
	GatherNamespacePrefix = "openshift-must-gather-"

	// imageWatchInterval is how often the watcher looks for the gather pod.
	// Against a warm image cache the pod can live for little more than ten
	// seconds, so this has to stay well under that.
	imageWatchInterval = 2 * time.Second
)

// ContainerImage records what one container of the gather pod ran.
type ContainerImage struct {
	Name string
	// Image is the reference from the pod spec: the image that was requested.
	Image string
	// ImageID is what the kubelet resolved and actually pulled. It differs
	// from Image when an ImageDigestMirrorSet or ImageContentSourcePolicy
	// redirects the pull to a mirror, which is the normal case on a
	// disconnected cluster, and it is the only place that redirection is
	// visible.
	ImageID string
}

// Mirrored reports whether the pulled image came from a different registry
// than the one requested.
func (image ContainerImage) Mirrored() bool {
	return registryOf(image.ImageID) != "" && registryOf(image.Image) != "" &&
		registryOf(image.ImageID) != registryOf(image.Image)
}

// GatherPodImages is the image provenance of one must-gather pod.
type GatherPodImages struct {
	Namespace  string
	Pod        string
	Node       string
	Containers []ContainerImage
}

// String renders the pod and every container's requested and pulled image,
// one per line, for the run log.
func (pod GatherPodImages) String() string {
	lines := []string{fmt.Sprintf("  pod %s/%s on node %s", pod.Namespace, pod.Pod, pod.Node)}
	for _, container := range pod.Containers {
		lines = append(lines,
			fmt.Sprintf("    container %q", container.Name),
			fmt.Sprintf("      requested: %s", container.Image),
			fmt.Sprintf("      pulled:    %s", container.ImageID))

		if container.Mirrored() {
			lines = append(lines, fmt.Sprintf(
				"      NOTE: pulled from %s, not the requested %s -- an image mirror is in effect",
				registryOf(container.ImageID), registryOf(container.Image)))
		}
	}

	return strings.Join(lines, "\n")
}

// registryOf returns the registry host of an image reference, or "" when the
// reference has no host component.
func registryOf(reference string) string {
	// An imageID is usually "registry/repo@sha256:...", but CRI-O also emits
	// a bare "sha256:..." for an image resolved purely from the local store.
	trimmed := strings.TrimPrefix(reference, "docker://")
	slash := strings.Index(trimmed, "/")

	if slash < 0 {
		return ""
	}

	host := trimmed[:slash]
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		// "library/busybox" style: the first segment is a path, not a host.
		return ""
	}

	return host
}

// ImageWatcher polls for the transient must-gather pod and records which image
// each of its containers actually pulled. `oc adm must-gather` deletes the
// namespace when it finishes, so a run that is already over cannot be asked
// what it pulled -- the watcher has to run alongside the collection.
type ImageWatcher struct {
	mutex   sync.Mutex
	found   map[string]GatherPodImages
	stop    chan struct{}
	stopped chan struct{}
}

// StartImageWatcher begins polling in the background. Call Stop to end the
// poll and collect what it saw. Errors are logged and never fail a caller:
// this is provenance reporting, not an assertion.
func StartImageWatcher(
	ctx context.Context, k8sClient kubernetes.Interface, logf func(format string, args ...interface{}),
) *ImageWatcher {
	watcher := &ImageWatcher{
		found:   make(map[string]GatherPodImages),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}

	go watcher.run(ctx, k8sClient, logf)

	return watcher
}

// run polls until Stop is called or the context ends.
func (w *ImageWatcher) run(
	ctx context.Context, k8sClient kubernetes.Interface, logf func(format string, args ...interface{}),
) {
	defer close(w.stopped)

	ticker := time.NewTicker(imageWatchInterval)
	defer ticker.Stop()

	for {
		w.sample(ctx, k8sClient, logf)

		select {
		case <-w.stop:
			// One final sample: the pod may only have reported its imageID
			// between the previous tick and the gather finishing.
			w.sample(ctx, k8sClient, logf)

			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sample records any must-gather pod whose containers have reported an image.
func (w *ImageWatcher) sample(
	ctx context.Context, k8sClient kubernetes.Interface, logf func(format string, args ...interface{}),
) {
	namespaces, err := k8sClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		// A cancelled context during teardown is expected, not worth logging.
		if ctx.Err() == nil {
			logf("Warning: image watcher could not list namespaces: %v\n", err)
		}

		return
	}

	for nsIdx := range namespaces.Items {
		namespace := namespaces.Items[nsIdx].Name
		if !strings.HasPrefix(namespace, GatherNamespacePrefix) {
			continue
		}

		pods, podErr := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if podErr != nil {
			if ctx.Err() == nil {
				logf("Warning: image watcher could not list pods in %s: %v\n", namespace, podErr)
			}

			continue
		}

		for podIdx := range pods.Items {
			w.record(&pods.Items[podIdx])
		}
	}
}

// record stores a pod's container images, replacing an earlier observation of
// the same pod: a later sample has more containers reporting an imageID, since
// the kubelet fills it in only once the image is pulled.
func (w *ImageWatcher) record(pod *corev1.Pod) {
	containers := make([]ContainerImage, 0, len(pod.Status.ContainerStatuses))

	for statusIdx := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[statusIdx]
		if status.ImageID == "" {
			continue
		}

		containers = append(containers, ContainerImage{
			Name:    status.Name,
			Image:   status.Image,
			ImageID: status.ImageID,
		})
	}

	if len(containers) == 0 {
		return
	}

	key := pod.Namespace + "/" + pod.Name
	w.mutex.Lock()
	defer w.mutex.Unlock()

	if previous, seen := w.found[key]; seen && len(previous.Containers) >= len(containers) {
		return
	}

	w.found[key] = GatherPodImages{
		Namespace:  pod.Namespace,
		Pod:        pod.Name,
		Node:       pod.Spec.NodeName,
		Containers: containers,
	}
}

// Stop ends the poll and returns what was observed, sorted by pod name so the
// log is stable between runs.
func (w *ImageWatcher) Stop() []GatherPodImages {
	close(w.stop)
	<-w.stopped

	w.mutex.Lock()
	defer w.mutex.Unlock()

	observed := make([]GatherPodImages, 0, len(w.found))
	for _, pod := range w.found {
		observed = append(observed, pod)
	}

	sort.Slice(observed, func(lhs, rhs int) bool {
		return observed[lhs].Namespace+observed[lhs].Pod < observed[rhs].Namespace+observed[rhs].Pod
	})

	return observed
}

// FormatGatherPodImages renders an image-provenance report for the run log.
// requestedImage is what the test asked `oc adm must-gather` to run.
func FormatGatherPodImages(requestedImage string, observed []GatherPodImages) string {
	lines := []string{"must-gather image provenance:",
		"  requested image: " + requestedImage}

	if len(observed) == 0 {
		lines = append(lines,
			"  no must-gather pod was observed before its namespace was deleted,",
			"  so the image the kubelet actually pulled could not be read. The requested",
			"  image and its digest above are still accurate; only the mirror-resolved",
			"  reference is missing.")

		return strings.Join(lines, "\n")
	}

	for _, pod := range observed {
		lines = append(lines, pod.String())
	}

	return strings.Join(lines, "\n")
}

// PulledImages returns the distinct images that were actually pulled, sorted.
// The full provenance goes to the run log; a report entry only has to answer
// "which image was this run?" without repeating the whole block.
func PulledImages(observed []GatherPodImages) []string {
	seen := make(map[string]bool)

	var pulled []string

	for _, pod := range observed {
		for _, container := range pod.Containers {
			if seen[container.ImageID] {
				continue
			}

			seen[container.ImageID] = true
			pulled = append(pulled, container.ImageID)
		}
	}

	sort.Strings(pulled)

	return pulled
}

// ImageLinesFromOutput returns the lines of `oc adm must-gather` output that
// name the image, namespace or pod it used. The collection's own log is long
// and mostly a file listing; these few lines are the provenance in it.
func ImageLinesFromOutput(output string) []string {
	var lines []string

	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.Contains(line, "plug-in image"),
			strings.Contains(line, GatherNamespacePrefix+"") && strings.Contains(line, "created"):
			lines = append(lines, strings.TrimSpace(line))
		}
	}

	return lines
}
