package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type bpnProbeDevice struct {
	pod         *corev1.Pod
	container   string
	devicePath  string
	containerID string
	host        *helpers.PreparedNodeAccess
	pid         int
	disk        string
	diskSeq     string
	image       string
}

type bpnWatcherProbe struct {
	writer, watcher bpnProbeDevice
	claim           *corev1.PersistentVolumeClaim
}

func bpnFindWriterProbe(pods []corev1.Pod, config, target string) (bpnProbeDevice, string, error) {
	var writer bpnProbeDevice
	claimName := ""
	for index := range pods {
		pod := &pods[index]
		if !bpnSelectedAgentPod(pod, config, target) || !bpnPodReady(pod) {
			continue
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.Ephemeral == nil || volume.Ephemeral.VolumeClaimTemplate == nil {
				continue
			}
			match, err := bpnProbeDeviceForVolume(pod, volume.Name, sbrparams.AgentContainerName)
			if err != nil {
				return bpnProbeDevice{}, "", err
			}
			if match.pod == nil {
				continue
			}
			if claimName != "" {
				return bpnProbeDevice{}, "", fmt.Errorf("node %s has multiple agent probe devices", target)
			}
			writer = match
			claimName = pod.Name + "-" + volume.Name
		}
	}
	if claimName == "" {
		return bpnProbeDevice{}, "", fmt.Errorf("node %s requires a Ready agent with one ephemeral Block probe", target)
	}

	return writer, claimName, nil
}

func bpnFindWatcherProbe(
	pods []corev1.Pod, writer *corev1.Pod, claimName string, requireReady bool,
) (bpnProbeDevice, error) {
	var watcher bpnProbeDevice
	for index := range pods {
		pod := &pods[index]
		if pod.Namespace != writer.Namespace || pod.UID == writer.UID {
			continue
		}
		for _, volume := range pod.Spec.Volumes {
			claim := volume.PersistentVolumeClaim
			if claim == nil || claim.ClaimName != claimName {
				continue
			}
			if watcher.pod != nil || !claim.ReadOnly || pod.DeletionTimestamp != nil ||
				pod.Status.Phase != corev1.PodRunning || (requireReady && !bpnPodReady(pod)) ||
				pod.Spec.NodeName == "" || pod.Spec.NodeName == writer.Spec.NodeName {
				return bpnProbeDevice{}, fmt.Errorf(
					"claim %s must have one running read-only consumer on another node", claimName)
			}
			match, err := bpnProbeDeviceForVolume(pod, volume.Name, "")
			if err != nil {
				return bpnProbeDevice{}, err
			}
			if match.pod == nil {
				return bpnProbeDevice{}, fmt.Errorf("pod %s must consume its probe as a Block device", pod.Name)
			}
			watcher = match
		}
	}
	if watcher.pod == nil {
		return bpnProbeDevice{}, fmt.Errorf("claim %s has no Ready read-only watcher", claimName)
	}

	return watcher, nil
}

func bpnProbeDeviceForVolume(pod *corev1.Pod, volumeName, containerName string) (bpnProbeDevice, error) {
	var result bpnProbeDevice
	for _, container := range pod.Spec.Containers {
		if containerName != "" && container.Name != containerName {
			continue
		}
		for _, device := range container.VolumeDevices {
			if device.Name != volumeName {
				continue
			}
			if result.pod != nil {
				return bpnProbeDevice{}, fmt.Errorf("pod %s has multiple consumers for the probe volume", pod.Name)
			}
			result = bpnProbeDevice{pod: pod, container: container.Name, devicePath: device.DevicePath}
		}
	}

	return result, nil
}

func (test *bpnFixture) findWatcherProbe(ctx context.Context, requireReady bool) (*bpnWatcherProbe, error) {
	pods, err := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	writer, name, err := bpnFindWriterProbe(pods.Items, test.name, test.nodes[0].initial.Name)
	if err != nil {
		return nil, err
	}
	claim, err := APIClient.CoreV1Interface.PersistentVolumeClaims(writer.pod.Namespace).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(claim, writer.pod) {
		return nil, fmt.Errorf("probe claim %s must belong to agent pod %s", name, writer.pod.Name)
	}
	if claim.Spec.VolumeMode == nil || *claim.Spec.VolumeMode != corev1.PersistentVolumeBlock ||
		claim.Status.Phase != corev1.ClaimBound || claim.UID == "" {
		return nil, fmt.Errorf("claim %s must be a bound Block probe", name)
	}
	watcher, err := bpnFindWatcherProbe(pods.Items, writer.pod, name, requireReady)
	if err != nil {
		return nil, err
	}

	return &bpnWatcherProbe{writer: writer, watcher: watcher, claim: claim}, nil
}

func (test *bpnFixture) prepareWatcherProbe() *bpnWatcherProbe {
	GinkgoHelper()
	var probe *bpnWatcherProbe
	test.waitForAPICheck(sbrparams.BPNAgentReady, "The target PVC must have a Ready watcher on another node",
		func(ctx context.Context) error {
			var err error
			probe, err = test.findWatcherProbe(ctx, true)

			return err
		})
	probe.writer.host = test.nodes[0].host
	for _, node := range test.nodes {
		if node.initial.Name == probe.watcher.pod.Spec.NodeName {
			probe.watcher.host = node.host

			break
		}
	}
	if probe.watcher.host == nil {
		ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNAPICall)
		node, err := APIClient.CoreV1Interface.Nodes().Get(ctx, probe.watcher.pod.Spec.NodeName, metav1.GetOptions{})
		cancel()
		Expect(err).NotTo(HaveOccurred(), "The watcher node must exist")
		probe.watcher.host, err = helpers.PrepareNodeAccess(
			test.ctx, APIClient, node.Name, node.UID, sbrparams.BPNObserverStart)
		test.skipOrFailOnPrerequisiteError(err, "the watcher node must permit native SSH access")
		DeferCleanup(probe.watcher.host.Close)
	}
	test.skipOrFailOnPrerequisiteError(probe.writer.resolve(test.ctx), "the writer must expose its kernel RBD device")
	test.skipOrFailOnPrerequisiteError(probe.watcher.resolve(test.ctx), "the watcher must expose its kernel RBD device")
	Expect(probe.watcher.image).To(Equal(probe.writer.image), "Both devices must map the same Ceph RBD image")
	AddReportEntry("bpn-watcher-probe", map[string]string{
		"writerPodUID": string(probe.writer.pod.UID), "watcherPodUID": string(probe.watcher.pod.UID),
		"probePVCUID": string(probe.claim.UID), "watcherNode": probe.watcher.pod.Spec.NodeName,
		"watcherBlockDevice": probe.watcher.disk,
	})

	return probe
}

func bpnProbeContainerID(pod *corev1.Pod, name string) string {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == name && status.State.Running != nil && strings.HasPrefix(status.ContainerID, "cri-o://") {
			return strings.TrimPrefix(status.ContainerID, "cri-o://")
		}
	}

	return ""
}

func (device *bpnProbeDevice) resolve(ctx context.Context) error {
	device.containerID = bpnProbeContainerID(device.pod, device.container)
	if len(device.containerID) != 64 || !bpnHex(device.containerID) ||
		!path.IsAbs(device.devicePath) || path.Clean(device.devicePath) != device.devicePath {
		return fmt.Errorf("pod %s lacks a running CRI-O container or an absolute device path", device.pod.Name)
	}
	output, err := device.host.Run(ctx, sbrparams.BPNHostCall, "sudo crictl inspect "+bpnQuote(device.containerID))
	if err != nil {
		return err
	}
	var inspection struct {
		Status struct {
			ID     string            `json:"id"`
			Labels map[string]string `json:"labels"`
		} `json:"status"`
		Info struct {
			PID int `json:"pid"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(output), &inspection); err != nil {
		return err
	}
	if inspection.Info.PID <= 1 || inspection.Status.ID != device.containerID ||
		inspection.Status.Labels["io.kubernetes.pod.uid"] != string(device.pod.UID) {
		return fmt.Errorf("CRI-O inspection did not match the identity of pod %s", device.pod.Name)
	}
	device.pid = inspection.Info.PID

	return device.resolveRBD(ctx)
}

func bpnHex(value string) bool {
	return strings.Trim(value, "0123456789abcdef") == ""
}

func (test *bpnFixture) checkWatcherProbeBindings(
	ctx context.Context, probe *bpnWatcherProbe, requireReady bool,
) error {
	return bpnCleanupAPI(ctx, func(call context.Context) error {
		current, err := test.findWatcherProbe(call, requireReady)
		if err != nil {
			return err
		}
		if current.claim.UID != probe.claim.UID ||
			!bpnSameProbeBinding(&probe.writer, &current.writer) ||
			!bpnSameProbeBinding(&probe.watcher, &current.watcher) {
			return fmt.Errorf("writer, watcher or probe claim identity changed")
		}
		node, err := APIClient.CoreV1Interface.Nodes().Get(call, probe.watcher.host.NodeName, metav1.GetOptions{})
		if err == nil && node.UID != probe.watcher.host.NodeUID {
			return fmt.Errorf("watcher node %s changed UID", node.Name)
		}

		return err
	})
}

func bpnSameProbeBinding(original, current *bpnProbeDevice) bool {
	return original.pod.UID == current.pod.UID && original.pod.Spec.NodeName == current.pod.Spec.NodeName &&
		original.container == current.container && original.devicePath == current.devicePath &&
		original.containerID == bpnProbeContainerID(current.pod, current.container)
}

func (device *bpnProbeDevice) resolveRBD(ctx context.Context) error {
	mounted := "/proc/" + strconv.Itoa(device.pid) + "/root" + device.devicePath
	output, err := bpnWatcherHostCommand(ctx, device.host, "mounted="+bpnQuote(mounted)+`
test -b "$mounted"
pair=$(stat -Lc '%t %T' "$mounted")
set -- $pair
disk="/sys/dev/block/$((0x$1)):$((0x$2))"
test "$(basename "$(readlink -f "$disk/device/subsystem")")" = rbd
test ! -e "$disk/partition"
printf '%s\n' "$disk"
cat "$disk/diskseq"
for attr in cluster_fsid pool_id pool_ns image_id snap_id; do
    value=$(cat "$disk/device/$attr")
    printf '%s=%s\n' "$attr" "$value"
done`)
	if err != nil {
		return err
	}
	fields := strings.Split(strings.TrimSpace(output), "\n")
	if len(fields) != 7 || !strings.HasPrefix(fields[0], "/sys/dev/block/") ||
		fields[2] == "cluster_fsid=" || fields[3] == "pool_id=" || fields[5] == "image_id=" || fields[6] == "snap_id=" {
		return fmt.Errorf("no complete RBD mapping identity for pod %s", device.pod.Name)
	}
	if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
		return fmt.Errorf("invalid block device disk sequence: %w", err)
	}
	device.disk = fields[0]
	device.diskSeq = fields[1]
	device.image = strings.Join(fields[2:], "\n")

	return nil
}

func bpnWatcherHostCommand(ctx context.Context, host *helpers.PreparedNodeAccess, script string) (string, error) {
	return host.Run(ctx, sbrparams.BPNHostCall, "sudo sh -c "+bpnQuote("set -eu\n"+script))
}
