package tests

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// pauseBlockStorageRecovery blocks new remediations only after NHC deleted the proven
// remediation. Its pause is released on failure as well as on the normal path.
func pauseBlockStorageRecovery(
	nhc *unstructured.Unstructured, nodeName, bootID string, guard func() error,
) func() error {
	GinkgoHelper()
	token := "sbr-storage-recovery-" + string(nhc.GetUID())
	target := recordODFFenceTarget(nodeName)
	recovery := &odfRebootRecovery{bootID: bootID, nhc: nhc.DeepCopy(), pauseToken: token}
	setPause := func(paused bool) error {
		ctx, cancel := context.WithTimeout(context.Background(), sbrparams.StaleODFFenceTimeout)
		defer cancel()

		return sbrutils.SetRecoveryPause(ctx, APIClient, nhc, token, paused)
	}
	target.recovery = recovery
	DeferCleanup(func() {
		if err := resumeBlockStorageRecovery(nodeName); err != nil {
			GinkgoWriter.Printf("Warning: release NHC storage recovery pause: %v\n", err)
		}
	})
	Expect(setPause(true)).To(Succeed(), "Pause only this test's NHC before storage recovery")
	Eventually(func() error {
		if guard != nil {
			if err := guard(); err != nil {
				return err
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), sbrparams.StaleODFFenceTimeout)
		defer cancel()
		current := nhc.DeepCopy()
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(nhc), current); err != nil {
			return err
		}

		return sbrutils.RecoveryPaused(current, nhc.GetUID(), token)
	}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"NHC must acknowledge its recovery pause before any fence is removed")

	return func() error { return resumeBlockStorageRecovery(nodeName) }
}

func waitForBlockNHCEnabled(nhc *unstructured.Unstructured, guard func() error) {
	GinkgoHelper()
	Eventually(func() (string, error) {
		if guard != nil {
			if err := guard(); err != nil {
				return "", err
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), sbrparams.StaleODFFenceTimeout)
		defer cancel()
		current := nhc.DeepCopy()
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(nhc), current); err != nil {
			return "", err
		}
		if current.GetUID() != nhc.GetUID() {
			return "", fmt.Errorf("NodeHealthCheck %s was replaced during recovery", nhc.GetName())
		}
		phase, _, err := unstructured.NestedString(current.Object, "status", "phase")

		return phase, err
	}, sbrparams.StorageInjectionTimeout, sbrparams.DefaultPollInterval).Should(Equal("Enabled"),
		"NHC must resume monitoring before the no-refencing stability check")
}

func resumeBlockStorageRecovery(nodeName string) error {
	target := odfFenceTargets[nodeName]
	if target == nil || target.recovery == nil {
		return nil
	}
	recovery := target.recovery
	ctx, cancel := context.WithTimeout(context.Background(), sbrparams.StaleODFFenceTimeout)
	defer cancel()
	err := sbrutils.SetRecoveryPause(ctx, APIClient, recovery.nhc, recovery.pauseToken, false)
	if err == nil || k8serrors.IsNotFound(err) {
		target.recovery = nil
	}

	return client.IgnoreNotFound(err)
}

// recoverRebootedODFFence runs only with the current test's NHC acknowledged
// Paused, a verified new boot ID, a recovered node and no active SBR remediation.
// A CSI plugin can retain a shut-down RADOS connection after unfencing, so only
// the recovered node's RBD nodeplugin is restarted after a range was removed.
func recoverRebootedODFFence(nodeName string) error {
	target := odfFenceTargets[nodeName]
	if target == nil || target.recovery == nil {
		return fmt.Errorf("no verified storage recovery for %s", nodeName)
	}
	if time.Now().Before(target.recovery.nextCheck) {
		return nil
	}
	target.recovery.nextCheck = time.Now().Add(sbrparams.StaleODFFenceRetryInterval)
	ctx, cancel := context.WithTimeout(context.Background(), sbrparams.StaleODFFenceTimeout)
	defer cancel()
	if err := odfFenceNodeRecovered(ctx, nodeName); err != nil {
		return err
	}
	// Clusters without ODF keep their normal CSI recovery path.
	if _, err := APIClient.CoreV1Interface.Namespaces().Get(ctx, sbrparams.ODFNamespace, metav1.GetOptions{}); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}

		return err
	}
	node, err := APIClient.CoreV1Interface.Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	cidrs, err := sbrutils.NodeFenceCIDRs(node)
	if err != nil {
		return err
	}
	cephCmd, err := rookCephCommand()
	if err != nil {
		return err
	}
	entries, err := readRebootRecoveryFences(cephCmd)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		if entry.Range == "" {
			continue
		}
		cidr, err := sbrutils.FenceRangeCIDR(entry.Range)
		if err != nil || !slices.Contains(cidrs, cidr) {
			continue
		}
		if err := recoverODFFence(ctx, cephCmd, nodeName, entry, cidr); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return restartRecoveredRBDPlugin(ctx, nodeName)
	}

	return nil
}

func readRebootRecoveryFences(cephCmd func(...string) (string, error)) ([]odfBlocklistEntry, error) {
	out, err := cephCmd("osd", "blocklist", "ls", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("read Ceph fences: %w", err)
	}
	entries, parsed := parseODFBlocklist(out)
	if !parsed {
		return nil, fmt.Errorf("cannot decode Ceph fences")
	}

	return entries, nil
}

func restartRecoveredRBDPlugin(ctx context.Context, nodeName string) error {
	pods, err := APIClient.CoreV1Interface.Pods(sbrparams.ODFNamespace).List(ctx,
		metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		for _, owner := range pod.OwnerReferences {
			if owner.Kind != "DaemonSet" || !ptr.Deref(owner.Controller, false) ||
				!strings.HasSuffix(owner.Name, ".rbd.csi.ceph.com-nodeplugin") {
				continue
			}
			GinkgoWriter.Printf("Restarting RBD CSI nodeplugin %s on recovered node %s after unfencing\n", pod.Name, nodeName)
			err := APIClient.CoreV1Interface.Pods(pod.Namespace).Delete(ctx, pod.Name,
				metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: ptr.To(pod.UID)}})
			if err != nil && !k8serrors.IsNotFound(err) {
				return err
			}
		}
	}

	return nil
}
