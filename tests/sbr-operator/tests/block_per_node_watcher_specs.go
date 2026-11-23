package tests

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("SBR BlockPerNode", Serial,
	Label(labels.OperatorSBR, labels.TierResiliency, labels.TierPreview, labels.FeatureBlockPerNode,
		labels.ComponentRemediation, labels.DisruptionDestructive, labels.PlatformAny, labels.FrequencyWeekly), func() {
		It("RHWA-2092-20 Watcher storage reads fail - Healthy workers remain available",
			reportxml.ID("RHWA-2092-20"), FlakeAttempts(1), func() {
				test := newBPNFixture("RHWA-2092-20")
				test.watcherGroupSize = 1

				By("Configure one target per watcher group. Establish a healthy baseline with active NHC.")
				test.prepareNodesAndCreateSBRConfig()
				test.createActiveNHCAndSBRTemplate()
				test.waitForAgentReporterAndStorageReadiness()
				probe := test.prepareWatcherProbe()
				test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

				By("Confirm both probe mappings can read and both Pods can reach the API.")
				address, port := test.watcherAPIEndpoint()
				for _, device := range []*bpnProbeDevice{&probe.writer, &probe.watcher} {
					test.skipOrFailOnPrerequisiteError(device.directRead(test.ctx), "the RBD mapping must support direct reads")
					test.skipOrFailOnPrerequisiteError(device.probeAPI(test.ctx, address, port),
						"native TCP probes must reach the Kubernetes API from both Pods")
				}
				fault := &bpnWatcherReadFault{device: &probe.watcher, owner: string(test.config.GetUID())}
				test.skipOrFailOnPrerequisiteError(fault.prepare(test.ctx), "isolated watcher read fault is unavailable")

				By("Inject an I/O error on the watcher mapping only. Confirm the watcher direct read fails.")
				Expect(test.checkWatcherProbeBindings(test.ctx, probe, true)).To(Succeed(),
					"The writer and watcher must still reference the original probe claim")
				DeferCleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), sbrparams.BPNCleanupPeriod)
					defer cancel()
					err := bpnCleanupPollWithin(ctx, fault.restore)
					if err != nil {
						bpnRecoveryFailed = true
					}
					Expect(err).NotTo(HaveOccurred(), "Restore the watcher device and host fault controls")
				})
				test.disrupted = true
				Expect(fault.activate(test.ctx)).To(Succeed(), "The fault must apply only to the watcher mapping")
				Expect(fault.confirm(test.ctx)).To(Succeed(), "The watcher mapping must return a direct read I/O error")
				activeAt := time.Now()
				initialWrites, err := probe.writer.completedWrites(test.ctx)
				Expect(err).NotTo(HaveOccurred(), "The writer's completed writes must be readable at the start of the fault")

				By("Observe for NO_REBOOT. Require healthy storage, API access, and no request, cordon, taint, or reboot.")
				test.observeWatcherReadFailure(probe, fault, address, port)
				finalWrites, err := probe.writer.completedWrites(test.ctx)
				Expect(err).NotTo(HaveOccurred(), "The writer's completed writes must be readable while the fault persists")
				Expect(finalWrites).To(BeNumerically(">", initialWrites), "The writer must complete writes during the read fault")
				Expect(probe.writer.directRead(test.ctx)).To(Succeed(), "The writer must still read its probe")
				Expect(fault.confirm(test.ctx)).To(Succeed(), "The watcher's direct reads must still fail")
				AddReportEntry("bpn-watcher-read-fault", map[string]interface{}{
					"activeAt": activeAt, "checkedThrough": time.Now(),
					"writerCompletedWritesAtStart": initialWrites, "writerCompletedWritesDuring": finalWrites,
				})

				By("Restore the watcher mapping. Require successful reads and full worker recovery before common cleanup.")
				Expect(fault.restore(test.ctx)).To(Succeed(), "Restore the watcher device and host fault controls")
				Eventually(func() error {
					if err := test.checkWatcherProbeBindings(test.ctx, probe, true); err != nil {
						return err
					}

					return probe.watcher.directRead(test.ctx)
				}, sbrparams.BPNFaultReady, sbrparams.BPNPoll).Should(Succeed(),
					"The original Ready watcher must read the probe after the fault is removed")
				test.waitForWorkerRecovery(corev1.ConditionFalse)
				test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
			})
	})

func (test *bpnFixture) watcherAPIEndpoint() (string, int32) {
	GinkgoHelper()
	var service *corev1.Service
	Expect(test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
		var err error
		service, err = APIClient.CoreV1Interface.Services("default").Get(ctx, "kubernetes", metav1.GetOptions{})

		return err
	})).To(Succeed(), "The Kubernetes API Service must exist")
	addr, err := netip.ParseAddr(service.Spec.ClusterIP)
	Expect(err).NotTo(HaveOccurred(), "The Kubernetes API Service must have a ClusterIP")
	for _, port := range service.Spec.Ports {
		if port.Name == "https" && port.Port > 0 && port.Protocol == corev1.ProtocolTCP {
			return addr.String(), port.Port
		}
	}
	Fail(fmt.Sprintf("Kubernetes API Service %s/%s must expose its HTTPS port", service.Namespace, service.Name))

	return "", 0
}

func (test *bpnFixture) observeWatcherReadFailure(
	probe *bpnWatcherProbe, fault *bpnWatcherReadFault, address string, port int32,
) {
	GinkgoHelper()
	test.observeInterval(sbrparams.BPNNoReboot, test.baseline, func() error {
		for _, check := range []func() error{
			func() error { return test.checkBootsWithoutRequests(test.baseline, test.baseline) },
			func() error {
				return test.checkRecordedNodeStorageHealth(corev1.ConditionFalse, test.baseline, time.Now())
			},
			func() error { return test.checkWatcherProbeBindings(test.ctx, probe, false) },
			func() error { return test.checkWorkerRecoveryInAPI(test.ctx, corev1.ConditionFalse) },
			func() error { return fault.confirm(test.ctx) },
			func() error { return probe.writer.probeAPI(test.ctx, address, port) },
			func() error { return probe.watcher.probeAPI(test.ctx, address, port) },
		} {
			if err := check(); err != nil {
				return err
			}
		}

		return nil
	})
}
