package tests

import (
	"context"
	"fmt"
	"reflect"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Block counterpart of the filesystem ghost peer regression in ghost_peer.go. Every agent writes a
// test heartbeat as node ID 1 during preflight and must zero that slot again once the node map
// assigns it a different ID; otherwise every peer keeps monitoring a node that does not exist. In
// Block mode node ID 1 owns heartbeat slot index 0 ((nodeID - 1) * 4096), not slot 1 as on a
// filesystem, and the agent clears the whole 4 KB slot.
var _ = Describe("SBR Block Storage — Ceph RBD startup preflight heartbeat", Ordered, Serial, func() {
	var storageClass string

	BeforeAll(func() {
		By("Verifying SBR is Ready")
		controller, err := deployment.Pull(APIClient, sbrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
		Expect(err).ToNot(HaveOccurred(), "SBR controller must exist")
		Expect(controller.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(), "SBR controller must be Ready")
		images := make([]string, 0, len(controller.Object.Spec.Template.Spec.Containers))
		for _, container := range controller.Object.Spec.Template.Spec.Containers {
			images = append(images, container.Image)
		}
		AddReportEntry("SBR controller images", images)
		requireNoSBRNodeHealthCheck()
		requireStoppedSNRForGhostPeer()
		removeStaleGhostPeerConfigs()

		By("Checking worker and Ceph RBD storage prerequisites")
		nodes, err := APIClient.CoreV1Interface.Nodes().List(context.TODO(),
			metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/worker"})
		Expect(err).ToNot(HaveOccurred(), "Worker nodes must be readable")
		readyWorkers := 0
		for idx := range nodes.Items {
			if isNodeSchedulable(&nodes.Items[idx]) {
				readyWorkers++
			}
		}
		if readyWorkers < 2 {
			Skip("The Block preflight slot regression requires at least two Ready, schedulable workers")
		}
		storageClass = discoverCephRBDStorageClass()

		// Entries left by earlier destructive Block specs can stop the RBD device from attaching or
		// slow the heartbeats down, so this runs before the SBRC is created.
		By("Clearing residual Ceph OSD blocklist entries from prior tests")
		clearCephOSDBlocklist()
	})

	AfterAll(func() {
		By("Clearing residual Ceph OSD blocklist entries")
		clearCephOSDBlocklist()
	})

	It("Verify SBR startup clears the unassigned preflight slot on a Block device",
		reportxml.ID("2038"),
		Label(
			labels.OperatorSBR,
			labels.TierAcceptance,
			labels.ComponentDaemonSet,
			labels.DisruptionNonDestructive,
			labels.PlatformAny,
			labels.FrequencyNightly,
			labels.TopologyMinimalWorker,
		), func() {
			By("Creating a Block-mode SBRC and waiting for its init Job and agents")
			config := buildSBRC("", buildBlockModeSBRC(storageClass, nil))
			config.SetGenerateName("test-sbrc-block-ghost-peer-")
			config.SetLabels(map[string]string{ghostPeerTestLabel: "true"})
			created, err := ghostPeerConfigs().Create(context.TODO(), config, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred(), "Test SBRC must be created")
			DeferCleanup(func() {
				By("Removing the test configuration and waiting for its operands")
				cleanupGhostPeerConfig(created)
			})
			waitForBlockInitJobSucceeded(created.GetName(), created.GetUID())
			waitForSBRCReady(created.GetName())
			agents, err := ghostPeerAgents(created.GetName())
			Expect(err).ToNot(HaveOccurred(), "Every scheduled SBR agent must be Ready")

			By("Verifying node ID 1 has no real owner in the shared Block node map")
			// Ready does not mean registered: the agent writes its readiness sentinel before it joins
			// the node map, so keep re-reading until every agent's node is in it.
			var (
				nodeMap *blockNodeMap
				payload string
			)
			Eventually(func() error {
				readMap, readPayload, readErr := readNodeMapFromPod(agents[0].Name)
				if readErr != nil {
					return readErr
				}
				for idx := range agents {
					if _, found := readMap.Entries[agents[idx].Spec.NodeName]; !found {
						return fmt.Errorf("agent node %q is not in the node map yet: %s",
							agents[idx].Spec.NodeName, readPayload)
					}
				}
				nodeMap, payload = readMap, readPayload

				return nil
			}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
				"Every agent node must be registered in the Block node map")
			AddReportEntry("Shared Block node map", payload)
			Expect(nodeMap.Entries).To(HaveLen(len(agents)), "Every agent node must have one assignment")
			Expect(nodeMap.NodeUsage).To(HaveLen(len(agents)), "Reverse node map must contain only real agents")
			for idx := range agents {
				nodeName := agents[idx].Spec.NodeName
				entry, found := nodeMap.Entries[nodeName]
				Expect(found).To(BeTrue(), "Agent node %q must be in the node map", nodeName)
				Expect(entry.NodeName).To(Equal(nodeName), "Node map entry must identify agent node %q", nodeName)
				Expect(entry.NodeID).To(BeNumerically(">=", 1), "Node ID must be in 1-%d", blockMaxNodes)
				Expect(entry.NodeID).To(BeNumerically("<=", blockMaxNodes), "Node ID must be in 1-%d", blockMaxNodes)
				Expect(nodeMap.NodeUsage[strconv.Itoa(int(entry.NodeID))]).To(Equal(nodeName),
					"Node ID %d must uniquely identify node %q", entry.NodeID, nodeName)
			}
			if owner, assigned := nodeMap.NodeUsage["1"]; assigned {
				Skip(fmt.Sprintf("Node %s legitimately owns ID 1, so heartbeat slot index 0 holds its own "+
					"heartbeat and the unused preflight slot regression cannot run", owner))
			}

			// Ready does not mean the slot has been cleared: the agent writes its readiness sentinel
			// right after preflight, and clearing slot index 0 only happens once the node manager has
			// assigned the real node ID, which on Block includes the node map's write-verify delay.
			By("Waiting for heartbeat slot index 0 to be zero-filled through every agent")
			Eventually(func() error {
				for idx := range agents {
					if slotErr := heartbeatSlotUnwritten(agents[idx].Name, 0); slotErr != nil {
						return fmt.Errorf("through agent %q: %w", agents[idx].Name, slotErr)
					}
				}

				return nil
			}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
				"The preflight heartbeat written as node ID 1 must be cleared from heartbeat slot index 0")

			// The first mismatch is returned rather than asserted inside the poll, so the failure
			// message names the agent and what changed.
			By("Observing the unused slot and agent stability for one minute")
			Consistently(func() error {
				current, agentsErr := ghostPeerAgents(created.GetName())
				if agentsErr != nil {
					return agentsErr
				}
				if len(current) != len(agents) {
					return fmt.Errorf("agent count went from %d to %d", len(agents), len(current))
				}
				for idx := range agents {
					original := &agents[idx]
					var samePod *corev1.Pod
					for other := range current {
						if current[other].UID == original.UID {
							samePod = &current[other]
						}
					}
					if samePod == nil {
						return fmt.Errorf("agent %q was replaced during observation", original.Name)
					}
					if !reflect.DeepEqual(samePod.Status.ContainerStatuses, original.Status.ContainerStatuses) {
						return fmt.Errorf("agent %q container state and restart counts changed from %+v to %+v",
							original.Name, original.Status.ContainerStatuses, samePod.Status.ContainerStatuses)
					}
					if slotErr := heartbeatSlotUnwritten(original.Name, 0); slotErr != nil {
						return fmt.Errorf("through agent %q: %w", original.Name, slotErr)
					}
				}

				return nil
			}, sbrparams.GhostPeerObservationDuration, sbrparams.DefaultPollInterval).Should(Succeed(),
				"SBRC %q agents must stay Ready without restarts and read a zero-filled heartbeat slot "+
					"index 0", created.GetName())
			AddReportEntry("Unused Block heartbeat slot index 0",
				"4096 zero bytes through every agent for one minute")
		})
})
