package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

func bpnQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func bpnResolveAPI(ctx context.Context, value string) ([]sbrutils.BlockEndpoint, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("invalid API address %q", value)
	}

	port := 443
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil {
			return nil, err
		}
	}

	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, parsed.Hostname())
	if err != nil {
		return nil, err
	}

	var endpoints []sbrutils.BlockEndpoint
	for _, address := range addresses {
		endpoints = append(endpoints, sbrutils.BlockEndpoint{Address: address.IP.String(), Port: port})
	}

	return endpoints, nil
}

func bpnUniqueEndpoints(endpoints []sbrutils.BlockEndpoint) []sbrutils.BlockEndpoint {
	seen := map[sbrutils.BlockEndpoint]bool{}
	result := make([]sbrutils.BlockEndpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if !seen[endpoint] {
			seen[endpoint] = true
			result = append(result, endpoint)
		}
	}

	return result
}

func (test *bpnCase) hostAPIEndpoints(node *bpnNode) ([]sbrutils.BlockEndpoint, error) {
	command := "sudo sh -c " + bpnQuote("set -eu\n"+
		"for file in /etc/kubernetes/kubeconfig /var/lib/kubelet/kubeconfig; do\n"+
		"if test -r \"$file\"; then awk '$1 == \"server:\" { print $2 }' \"$file\"; fi\ndone")
	output, err := node.host.Run(test.ctx, sbrparams.BPNHostCall, command)
	if err != nil {
		return nil, err
	}

	var result []sbrutils.BlockEndpoint
	for _, server := range strings.Fields(output) {
		parsed, parseErr := url.Parse(server)
		if parseErr != nil || parsed.Hostname() == "" {
			return nil, fmt.Errorf("host kubeconfig has an invalid API server address")
		}

		port := 443
		if parsed.Port() != "" {
			port, parseErr = strconv.Atoi(parsed.Port())
			if parseErr != nil {
				return nil, parseErr
			}
		}

		if address := net.ParseIP(parsed.Hostname()); address != nil {
			result = append(result, sbrutils.BlockEndpoint{Address: address.String(), Port: port})

			continue
		}

		resolved, resolveErr := node.host.Run(test.ctx, sbrparams.BPNHostCall, "getent ahosts "+bpnQuote(parsed.Hostname()))
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve host API name %s: %w", parsed.Hostname(), resolveErr)
		}

		for _, line := range strings.Split(resolved, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 && net.ParseIP(fields[0]) != nil {
				result = append(result, sbrutils.BlockEndpoint{Address: net.ParseIP(fields[0]).String(), Port: port})
			}
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("the host kubelet configuration must identify an API endpoint")
	}

	return bpnUniqueEndpoints(result), nil
}

func (test *bpnCase) prerequisite(err error, reason string) {
	GinkgoHelper()
	if err == nil {
		return
	}
	if !test.disrupted {
		Skip("BLOCKED: " + reason + ": " + err.Error())
	}
	Fail(test.faultFailure(reason + ": " + err.Error()))
}

func (test *bpnCase) faultFailure(message string) string {
	if !test.trigger.IsZero() && (test.plan.id == "RHWA-1839-12A" || test.plan.id == "RHWA-1839-12B") {
		return "FAIL: Row 12 timing or ordering failure: " + message
	}

	return "INCONCLUSIVE: " + message
}

func (test *bpnCase) execPod(ctx context.Context, pod *corev1.Pod, container string, command []string) ([]byte, error) {
	request := APIClient.CoreV1Interface.RESTClient().Post().Resource("pods").Namespace(pod.Namespace).
		Name(pod.Name).SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: container, Command: command, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(APIClient.Config, "POST", request.URL())
	if err != nil {
		return nil, err
	}
	var output, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &output, Stderr: &stderr})
	if err != nil {
		return nil, fmt.Errorf("the Pod %s/%s: %w: %s", pod.Namespace, pod.Name, err, stderr.String())
	}

	return output.Bytes(), nil
}

func (test *bpnCase) apiEndpoints(ctx context.Context) ([]sbrutils.BlockEndpoint, error) {
	var endpoints []sbrutils.BlockEndpoint
	resolved, err := bpnResolveAPI(ctx, APIClient.Config.Host)
	if err != nil {
		return nil, err
	}
	endpoints = append(endpoints, resolved...)
	service, err := APIClient.CoreV1Interface.Services("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	for _, address := range service.Spec.ClusterIPs {
		for _, servicePort := range service.Spec.Ports {
			endpoints = append(endpoints, sbrutils.BlockEndpoint{Address: address, Port: int(servicePort.Port)})
		}
	}
	backends, err := APIClient.CoreV1Interface.Endpoints("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	for _, subset := range backends.Subsets {
		for _, address := range subset.Addresses {
			for _, backendPort := range subset.Ports {
				endpoints = append(endpoints, sbrutils.BlockEndpoint{Address: address.IP, Port: int(backendPort.Port)})
			}
		}
	}

	infrastructure := &unstructured.Unstructured{}
	infrastructure.SetAPIVersion("config.openshift.io/v1")
	infrastructure.SetKind("Infrastructure")
	if err := APIClient.Get(ctx, types.NamespacedName{Name: "cluster"}, infrastructure); err != nil {
		return nil, err
	}

	for _, field := range []string{"apiServerURL", "apiServerInternalURI"} {
		address, found, fieldErr := unstructured.NestedString(infrastructure.Object, "status", field)
		if fieldErr != nil || !found || address == "" {
			return nil, fmt.Errorf("the Infrastructure status.%s must expose its API address", field)
		}

		resolved, resolveErr := bpnResolveAPI(ctx, address)
		if resolveErr != nil {
			return nil, resolveErr
		}

		endpoints = append(endpoints, resolved...)
	}

	return endpoints, nil
}

func (test *bpnCase) discoverNetwork() {
	if len(test.networks) > 0 {
		return
	}
	var endpoints []sbrutils.BlockEndpoint
	Expect(test.api(func(ctx context.Context) error {
		var err error
		endpoints, err = test.apiEndpoints(ctx)

		return err
	})).To(Succeed(), "The runner must discover every configured API endpoint")
	storage, err := test.cephEndpoints()
	test.prerequisite(err, "authoritative Ceph monitor and OSD endpoints are required")
	for _, node := range test.nodes {
		management, hostErr := node.host.Run(test.ctx, sbrparams.BPNHostCall, "printf '%s\\n' \"$SSH_CONNECTION\"")
		test.prerequisite(hostErr, "the runner must retain its SSH management address")
		parts := strings.Fields(management)
		if len(parts) != 4 {
			test.prerequisite(fmt.Errorf("SSH_CONNECTION is unavailable"), "management path")
		}
		hostAPI, hostAPIErr := test.hostAPIEndpoints(node)
		test.prerequisite(hostAPIErr, "host kubeconfig API endpoints must be discoverable")
		network := sbrutils.BlockNetwork{
			API:     bpnUniqueEndpoints(append(append([]sbrutils.BlockEndpoint{}, endpoints...), hostAPI...)),
			Storage: storage, Management: []string{parts[0]},
		}
		for _, peer := range test.nodes {
			if peer == node {
				continue
			}
			for _, address := range peer.initial.Status.Addresses {
				if address.Type == corev1.NodeInternalIP {
					network.Peers = append(network.Peers, address.Address)
				}
			}

			network.Peers = append(network.Peers, peer.initial.Spec.PodCIDRs...)
		}
		test.networks = append(test.networks, network)
	}
	AddReportEntry("bpn-endpoints", test.networks)
}

func (test *bpnCase) cephCSIConfig() (string, []string, error) {
	className, _, _ := unstructured.NestedString(test.config.Object, "spec", "sharedStorageClass")
	var storageNamespace string
	var monitors []string
	err := test.api(func(ctx context.Context) error {
		class, getErr := APIClient.StorageV1Interface.StorageClasses().Get(ctx, className, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		clusterID := class.Parameters["clusterID"]
		storageNamespace = class.Parameters["csi.storage.k8s.io/provisioner-secret-namespace"]
		if storageNamespace == "" {
			storageNamespace = clusterID
		}
		maps, getErr := APIClient.CoreV1Interface.ConfigMaps(storageNamespace).List(ctx, metav1.ListOptions{})
		if getErr != nil {
			return getErr
		}
		for _, config := range maps.Items {
			for _, value := range config.Data {
				var clusters []struct {
					ClusterID string   `json:"clusterID"`
					Monitors  []string `json:"monitors"`
				}
				if json.Unmarshal([]byte(value), &clusters) != nil {
					continue
				}
				for _, cluster := range clusters {
					if cluster.ClusterID == clusterID {
						monitors = cluster.Monitors
					}
				}
			}
		}
		if len(monitors) == 0 {
			return fmt.Errorf("CSI cluster %s has no authoritative monitor configuration", clusterID)
		}

		return nil
	})

	return storageNamespace, monitors, err
}

func (test *bpnCase) cephEndpoints() ([]sbrutils.BlockEndpoint, error) {
	storageNamespace, monitors, err := test.cephCSIConfig()
	if err != nil {
		return nil, err
	}
	var pods *corev1.PodList
	err = test.api(func(ctx context.Context) error {
		var listErr error
		pods, listErr = APIClient.CoreV1Interface.Pods(storageNamespace).List(ctx, metav1.ListOptions{})

		return listErr
	})
	if err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		if !bpnPodReady(&pod) {
			continue
		}
		for _, container := range pod.Spec.Containers {
			if !strings.Contains(container.Name, "rook-ceph-operator") && !strings.Contains(container.Name, "ceph-tools") {
				continue
			}
			endpoints, parseErr := test.cephMapsFromPod(&pod, container.Name, storageNamespace)
			if parseErr != nil {
				continue
			}
			if bpnCephMonitorsMatch(monitors, endpoints) {
				return endpoints, nil
			}
		}
	}

	return nil, fmt.Errorf("no existing Ceph command returned maps matching the selected CSI monitors")
}

func bpnCephMonitorsMatch(monitors []string, endpoints []sbrutils.BlockEndpoint) bool {
	for _, monitor := range monitors {
		address, portText, err := net.SplitHostPort(monitor)
		if err != nil {
			return false
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return false
		}
		parsed := net.ParseIP(address)
		if parsed == nil {
			return false
		}
		found := false
		for _, endpoint := range endpoints {
			if endpoint.Address == parsed.String() && endpoint.Port == port {
				found = true
			}
		}
		if !found {
			return false
		}
	}

	return len(monitors) > 0
}

func (test *bpnCase) cephMapsFromPod(pod *corev1.Pod, container, namespace string) ([]sbrutils.BlockEndpoint, error) {
	commands := []string{"ceph --conf " + bpnQuote("/var/lib/rook/"+namespace+"/"+namespace+".config"), "ceph"}
	for _, command := range commands {
		ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNAPICall)
		monDump, monErr := test.execPod(ctx, pod, container, []string{"sh", "-c", command + " mon dump --format json"})
		cancel()
		if monErr != nil {
			continue
		}
		ctx, cancel = context.WithTimeout(test.ctx, sbrparams.BPNAPICall)
		osdDump, osdErr := test.execPod(ctx, pod, container, []string{"sh", "-c", command + " osd dump --format json"})
		cancel()
		if osdErr != nil {
			continue
		}
		endpoints, parseErr := sbrutils.ParseCephEndpoints(monDump, osdDump)
		if parseErr == nil {
			return endpoints, nil
		}
	}

	return nil, fmt.Errorf("existing Pod %s/%s did not return valid Ceph maps", pod.Namespace, pod.Name)
}

func (test *bpnCase) namespacePIDs(index int) []int {
	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNFaultReady)
	defer cancel()
	pids, err := test.currentNamespacePIDs(ctx, index, true)
	test.prerequisite(err, "native CRI access must identify every SBR network namespace")

	return pids
}

type bpnCRISandbox struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Metadata struct {
		UID string `json:"uid"`
	} `json:"metadata"`
}

func bpnReadySandboxID(sandboxes []bpnCRISandbox, podUID types.UID) (string, error) {
	sandboxID := ""
	for _, sandbox := range sandboxes {
		if sandbox.Metadata.UID != string(podUID) || sandbox.State != "SANDBOX_READY" {
			continue
		}
		if sandboxID != "" {
			return "", fmt.Errorf("the Pod %s has multiple Ready CRI sandboxes", podUID)
		}
		sandboxID = sandbox.ID
	}

	return sandboxID, nil
}

func (test *bpnCase) currentNamespacePIDs(ctx context.Context, index int, required bool) ([]int, error) {
	apiCtx, cancel := context.WithTimeout(ctx, sbrparams.BPNAPICall)
	pods, err := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).List(apiCtx,
		metav1.ListOptions{
			LabelSelector: "sbrconfig=" + test.name,
			FieldSelector: "spec.nodeName=" + test.nodes[index].initial.Name,
		})
	cancel()
	if err != nil {
		return nil, err
	}

	result := []int{1}
	var sandboxes struct {
		Items []bpnCRISandbox `json:"items"`
	}
	loaded := false
	for _, pod := range pods.Items {
		if pod.Spec.HostNetwork || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}

		if !loaded {
			output, listErr := test.nodes[index].host.Run(ctx, sbrparams.BPNHostCall, "sudo crictl pods --output json")
			if listErr != nil {
				return nil, listErr
			}
			if decodeErr := json.Unmarshal([]byte(output), &sandboxes); decodeErr != nil {
				return nil, decodeErr
			}
			loaded = true
		}

		sandboxID, sandboxErr := bpnReadySandboxID(sandboxes.Items, pod.UID)
		if sandboxErr != nil {
			return nil, sandboxErr
		}
		if sandboxID == "" {
			if required {
				return nil, fmt.Errorf("the Pod %s has no Ready CRI sandbox", pod.UID)
			}

			continue
		}

		output, inspectErr := test.nodes[index].host.Run(ctx, sbrparams.BPNHostCall,
			"sudo crictl inspectp "+bpnQuote(sandboxID))
		if inspectErr != nil {
			return nil, inspectErr
		}
		var inspection struct {
			Info struct {
				PID int `json:"pid"`
			} `json:"info"`
		}
		if err := json.Unmarshal([]byte(output), &inspection); err != nil || inspection.Info.PID <= 1 {
			return nil, fmt.Errorf("the Pod %s sandbox PID unavailable", pod.UID)
		}
		result = append(result, inspection.Info.PID)
	}

	return result, nil
}

func (test *bpnCase) prepareFault(
	index int, kind sbrutils.BlockFaultKind, persistent bool,
) *sbrutils.BlockNetworkFault {
	GinkgoHelper()
	test.discoverNetwork()
	network := test.networks[index]
	if kind == sbrutils.BlockSingletonIsolation {
		var clusterNodes *corev1.NodeList
		Expect(test.api(func(ctx context.Context) error {
			var err error
			clusterNodes, err = APIClient.CoreV1Interface.Nodes().List(ctx, metav1.ListOptions{})

			return err
		})).To(Succeed(), "Singleton isolation must cover all cluster Nodes")
		network.Peers = nil
		for _, clusterNode := range clusterNodes.Items {
			if clusterNode.UID != test.nodes[index].initial.UID {
				for _, address := range clusterNode.Status.Addresses {
					if address.Type == corev1.NodeInternalIP {
						network.Peers = append(network.Peers, address.Address)
					}
				}
			}

			network.Peers = append(network.Peers, clusterNode.Spec.PodCIDRs...)
		}
	}

	network.NamespacePIDs = test.namespacePIDs(index)
	fault, err := sbrutils.NewBlockNetworkFault(test.nodes[index].host, network, kind,
		fmt.Sprintf("%s-%d-%d", test.name, index, len(test.faults)), persistent)
	test.prerequisite(err, "native fault preparation")
	test.faults = append(test.faults, fault)
	if test.faultNodes == nil {
		test.faultNodes = map[*sbrutils.BlockNetworkFault]int{}
	}
	test.faultNodes[fault] = index
	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNFaultReady)
	defer cancel()
	err = fault.Prepare(ctx)
	var prerequisite *sbrutils.BlockPrerequisiteError
	if errors.As(err, &prerequisite) {
		test.prerequisite(err, "native fault preparation")
	}
	Expect(err).NotTo(HaveOccurred(), "Native fault preparation must finish")

	return fault
}

func (test *bpnCase) activate(fault *sbrutils.BlockNetworkFault) {
	GinkgoHelper()
	test.disrupted = true
	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNFaultReady)
	defer cancel()
	Expect(fault.Activate(ctx)).To(Succeed(), test.faultFailure("the owned fault must be installed"))
	Eventually(func() error { return fault.Confirm(ctx) }, sbrparams.BPNFaultReady, sbrparams.BPNPoll).
		Should(Succeed(), test.faultFailure("the intended fault must be confirmed"))
	if fault.Kind() == sbrutils.BlockStorageLoss || fault.Kind() == sbrutils.BlockStorageTimeout ||
		fault.Kind() == sbrutils.BlockStorageLatency {
		if test.healthFaults == nil {
			test.healthFaults = map[int]sbrutils.BlockFaultEvidence{}
		}
		test.healthFaults[test.faultNodes[fault]] = fault.Evidence()
	}
	if fault.Kind() == sbrutils.BlockStorageLoss || fault.Kind() == sbrutils.BlockStorageTimeout {
		Expect(test.liveAPI(ctx, test.faultNodes[fault])).To(Succeed(), "Storage faults must preserve API transport")
	}

	AddReportEntry("bpn-fault-active", fault.Evidence())
}

func (test *bpnCase) restoreFault(fault *sbrutils.BlockNetworkFault) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNFaultReady)
	defer cancel()
	index := test.faultNodes[fault]
	pids, err := test.currentNamespacePIDs(ctx, index, false)
	Expect(err).NotTo(HaveOccurred(), "Fault restoration must use the current SBR network namespaces")
	fault.SetNamespacePIDs(pids)
	Expect(fault.Restore(ctx)).To(Succeed(), "Owned fault removal must restore access")
	AddReportEntry("bpn-fault-restored", fault.Evidence())
}

func (test *bpnCase) restoreFaults() {
	for _, fault := range test.faults {
		test.restoreFault(fault)
	}
}

func (test *bpnCase) combined() (*sbrutils.BlockNetworkFault, *sbrutils.BlockNetworkFault, time.Time) {
	storage := test.prepareFault(0, sbrutils.BlockStorageLoss, true)
	api := test.prepareFault(0, sbrutils.BlockAPILoss, true)
	test.activate(storage)
	test.requireUnhealthy(0)
	test.finalBoots(0)
	test.activate(api)

	return storage, api, time.Now()
}

func (test *bpnCase) checkFaultAfterReboot(faults ...*sbrutils.BlockNetworkFault) {
	for _, fault := range faults {
		index := test.faultNodes[fault]
		ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNFaultReady)
		pids, err := test.currentNamespacePIDs(ctx, index, false)
		Expect(err).NotTo(HaveOccurred(), test.faultFailure("live SBR network namespaces must be identifiable after reboot"))
		fault.SetNamespacePIDs(pids)
		Expect(fault.Confirm(ctx)).To(Succeed(), test.faultFailure("persistent rules must remain effective after reboot"))
		cancel()
	}
}

func (test *bpnCase) liveAPI(ctx context.Context, index int) error {
	pids, err := test.currentNamespacePIDs(ctx, index, false)
	if err != nil {
		return err
	}

	network := test.networks[index]
	network.NamespacePIDs = pids
	probe, err := sbrutils.NewBlockNetworkFault(test.nodes[index].host, network,
		sbrutils.BlockAPILoss, test.name+"-probe", false)
	if err != nil {
		return err
	}

	return probe.ProbeAPI(ctx)
}

func bpnJournalMountMatches(mount corev1.VolumeMount, volumes []corev1.Volume) bool {
	const journalDirectory = "/var/lib/medik8s/state"
	if mount.ReadOnly || mount.SubPathExpr != "" {
		return false
	}
	mountDirectory := path.Clean(mount.MountPath)
	if journalDirectory != mountDirectory && !strings.HasPrefix(journalDirectory, mountDirectory+"/") {
		return false
	}
	relative := strings.TrimPrefix(strings.TrimPrefix(journalDirectory, mountDirectory), "/")
	for _, volume := range volumes {
		if volume.Name == mount.Name && volume.HostPath != nil {
			return path.Join(volume.HostPath.Path, mount.SubPath, relative) == journalDirectory
		}
	}

	return false
}

func (test *bpnCase) requireJournalMount(index int, agent *corev1.Pod) {
	GinkgoHelper()
	_, commandErr := test.nodes[index].host.Run(test.ctx, sbrparams.BPNHostCall, "command -v stat >/dev/null")
	test.prerequisite(commandErr, "native stat must check the mounted journal directory")
	containerID := ""
	for _, status := range agent.Status.ContainerStatuses {
		if status.Name == sbrparams.AgentContainerName {
			_, containerID, _ = strings.Cut(status.ContainerID, "://")
		}
	}
	Expect(containerID).NotTo(BeEmpty(), "The Ready agent must expose its actual container identity")
	output, err := test.nodes[index].host.Run(test.ctx, sbrparams.BPNHostCall,
		"sudo crictl inspect "+bpnQuote(containerID))
	test.prerequisite(err, "native CRI inspection must identify the agent mount namespace")
	var inspection struct {
		Info struct {
			PID int `json:"pid"`
		} `json:"info"`
	}
	Expect(json.Unmarshal([]byte(output), &inspection)).To(Succeed())
	Expect(inspection.Info.PID).To(BeNumerically(">", 1), "The agent container must have a live host PID")
	command := "sudo stat -Lc '%d:%i' /var/lib/medik8s/state /proc/" +
		strconv.Itoa(inspection.Info.PID) + "/root/var/lib/medik8s/state"
	output, err = test.nodes[index].host.Run(test.ctx, sbrparams.BPNHostCall, command)
	Expect(err).NotTo(HaveOccurred(), "The host and agent must expose their state directories")
	identities := strings.Fields(output)
	Expect(identities).To(HaveLen(2))
	Expect(identities[0]).To(Equal(identities[1]), "The agent and host must use the same state directory")
}

func (test *bpnCase) journal(index int) *sbrutils.BlockJournalFault {
	GinkgoHelper()
	var pods *corev1.PodList
	Expect(test.api(func(ctx context.Context) error {
		var err error
		pods, err = APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).List(ctx,
			metav1.ListOptions{
				LabelSelector: "sbrconfig=" + test.name,
				FieldSelector: "spec.nodeName=" + test.nodes[index].initial.Name,
			})

		return err
	})).To(Succeed())
	var agent *corev1.Pod
	for podIndex := range pods.Items {
		pod := &pods.Items[podIndex]
		if pod.DeletionTimestamp != nil || !bpnPodReady(pod) {
			continue
		}
		for _, container := range pod.Spec.Containers {
			if container.Name != sbrparams.AgentContainerName {
				continue
			}
			for _, mount := range container.VolumeMounts {
				if bpnJournalMountMatches(mount, pod.Spec.Volumes) {
					agent = pod
				}
			}
		}
	}
	Expect(agent).NotTo(BeNil(), "A Ready agent must mount the host state directory as its journal backing directory")
	test.requireJournalMount(index, agent)
	fault, err := sbrutils.NewBlockJournalFault(test.nodes[index].host, test.name+"-"+strconv.Itoa(index))
	test.prerequisite(err, "journal access fault")
	test.journalFaults = append(test.journalFaults, fault)
	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNFaultReady)
	defer cancel()
	test.prerequisite(fault.Prepare(ctx), "native immutable-directory support")
	// The helper tests a disposable file before any remediation trigger.
	test.prerequisite(fault.Activate(ctx), "effective immutable-directory fault")
	test.disrupted = true

	return fault
}

func (test *bpnCase) restoreJournal(fault *sbrutils.BlockJournalFault) {
	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNFaultReady)
	defer cancel()
	Expect(fault.Restore(ctx)).To(Succeed(), "The test must restore the original state-directory flags")
}

func (test *bpnCase) waitUntil(deadline time.Time) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-test.ctx.Done():
		Fail("The case context expired")
	}
}
