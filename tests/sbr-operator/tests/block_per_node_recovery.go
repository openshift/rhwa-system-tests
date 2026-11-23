package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func (test *bpnFixture) waitForWorkerRecovery(targetStorageStatus corev1.ConditionStatus) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNNodeReady)
	defer cancel()
	Expect(bpnCleanupPollWithin(ctx, func(call context.Context) error {
		return test.checkWorkerRecovery(call, targetStorageStatus)
	})).To(Succeed(), "Every selected worker must recover with its expected storage condition and baseline restrictions")
}

func (test *bpnFixture) checkWorkerRecoveryWithinPoll(ctx context.Context) error {
	call, cancel := context.WithTimeout(ctx, sbrparams.BPNPoll)
	defer cancel()

	return test.checkWorkerRecovery(call, corev1.ConditionFalse)
}

func (test *bpnFixture) checkWorkerRecovery(ctx context.Context, targetStorageStatus corev1.ConditionStatus) error {
	if err := test.checkNodeIdentities(); err != nil {
		return err
	}
	if err := test.checkWorkerRecoveryInAPI(ctx, targetStorageStatus); err != nil {
		return err
	}
	if err := bpnCleanupAPI(ctx, test.checkNoCurrentWorkerRequests); err != nil {
		return err
	}
	runningTargetContainers, err := test.checkRecoveredHosts(ctx)
	if err != nil {
		return err
	}

	if err := bpnCleanupAPI(ctx, func(call context.Context) error {
		return test.checkAgentAndReporterState(call, runningTargetContainers, targetStorageStatus == corev1.ConditionTrue)
	}); err != nil {
		return err
	}

	return ctx.Err()
}

func (test *bpnFixture) checkWorkerRecoveryInAPI(
	ctx context.Context, targetStorageStatus corev1.ConditionStatus,
) error {
	for index, node := range test.nodes {
		current, err := test.getNodeForCleanup(ctx, index)
		if err != nil {
			return err
		}
		if !helpers.IsNodeReady(current) {
			return fmt.Errorf("node %s is not Ready", current.Name)
		}
		if current.Status.NodeInfo.BootID != node.boot {
			return fmt.Errorf("node %s API boot ID %q must match expected boot ID %q",
				current.Name, current.Status.NodeInfo.BootID, node.boot)
		}
		expectedStorage := corev1.ConditionFalse
		if index == 0 {
			expectedStorage = targetStorageStatus
		}
		if observed := bpnStorageUnhealthyStatus(current); observed != expectedStorage {
			return fmt.Errorf("node %s storage condition is %s, expected %s", current.Name, observed, expectedStorage)
		}
		if err := test.checkNodeRestrictions(index, current); err != nil {
			return err
		}
	}

	return ctx.Err()
}

func (test *bpnFixture) checkNoCurrentWorkerRequests(ctx context.Context) error {
	requests, err := APIClient.Resource(schema.GroupVersionResource{
		Group: sbrparams.CRDGroup, Version: sbrparams.CRDVersion, Resource: "storagebasedremediations",
	}).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, request := range requests.Items {
		for _, node := range test.nodes {
			if request.GetName() == node.initial.Name {
				return fmt.Errorf("node %s still has SBR CR %s/%s UID %s",
					node.initial.Name, request.GetNamespace(), request.GetName(), request.GetUID())
			}
		}
	}

	return nil
}

func (test *bpnFixture) checkRecoveredHosts(ctx context.Context) (map[string]bool, error) {
	var pending sync.WaitGroup
	failures := make([]error, len(test.nodes))
	var runningTargetContainers map[string]bool
	for index, node := range test.nodes {
		pending.Go(func() {
			var boot string
			var err error
			if index == 0 && !node.rebootObservedAt.IsZero() {
				boot, runningTargetContainers, err = bpnReadRebootedHostContainers(ctx, node)
			} else {
				boot, err = node.host.BootID(ctx, sbrparams.BPNHostCall)
			}
			if err == nil && boot != node.boot {
				err = fmt.Errorf("host boot ID %q must match expected boot ID %q", boot, node.boot)
			}
			if err != nil {
				failures[index] = fmt.Errorf("node %s: %w", node.initial.Name, err)
			}
		})
	}
	pending.Wait()

	return runningTargetContainers, errors.Join(failures...)
}

func bpnReadRebootedHostContainers(ctx context.Context, node *bpnNode) (string, map[string]bool, error) {
	output, err := node.host.Run(ctx, sbrparams.BPNHostCall,
		"cat /proc/sys/kernel/random/boot_id && sudo crictl ps --output json")
	if err != nil {
		return "", nil, err
	}
	boot, data, found := strings.Cut(output, "\n")
	if !found || boot == "" {
		return "", nil, fmt.Errorf("missing host boot ID or CRI output")
	}
	var list struct {
		Containers []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"containers"`
	}
	if err := json.Unmarshal([]byte(data), &list); err != nil {
		return "", nil, fmt.Errorf("decode running CRI containers: %w", err)
	}
	running := make(map[string]bool, len(list.Containers))
	for _, container := range list.Containers {
		if container.State == "CONTAINER_RUNNING" && container.ID != "" {
			running[container.ID] = true
		}
	}

	return boot, running, nil
}
