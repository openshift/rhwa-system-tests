package sbrutils

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type blockTestHost struct {
	commands []string
	run      func(string) (string, error)
}

func (host *blockTestHost) Run(_ context.Context, _ time.Duration, command string) (string, error) {
	host.commands = append(host.commands, command)
	if host.run != nil {
		return host.run(command)
	}

	return "", nil
}

func blockTestNetwork() BlockNetwork {
	return BlockNetwork{
		API:        []BlockEndpoint{{Address: "192.0.2.1", Port: 6443}, {Address: "2001:db8::1", Port: 6443}},
		Storage:    []BlockEndpoint{{Address: "192.0.2.2", Port: 8123}, {Address: "2001:db8::2", Port: 8124}},
		Peers:      []string{"192.0.2.3", "2001:db8::3"},
		Management: []string{"192.0.2.4"},
	}
}

func TestBlockRestoreRejectsFirewallQueryErrors(t *testing.T) {
	fault, err := NewBlockNetworkFault(&blockTestHost{}, blockTestNetwork(), BlockStorageLoss, "query-error", false)
	if err != nil {
		t.Fatal(err)
	}

	for _, script := range []string{fault.applyScript(), fault.removeScript()} {
		command := exec.CommandContext(t.Context(), "sh", "-c",
			"iptables() { return 4; }\nip6tables() { return 4; }\n"+script)
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatalf("a firewall query error must prevent success: %s", output)
		}
	}
}

func TestBlockRestorePreservesUnrelatedFirewallRules(t *testing.T) {
	fault, err := NewBlockNetworkFault(&blockTestHost{}, blockTestNetwork(), BlockStorageLoss, "unrelated", false)
	if err != nil {
		t.Fatal(err)
	}

	prelude := `iptables() {
shift 2
if [ "$1" != -S ]; then echo UNEXPECTED_MUTATION; return 99; fi
printf '%s\n' '-N EXISTING' '-A OUTPUT -j EXISTING'
}

ip6tables() { iptables "$@"; }
`
	command := exec.CommandContext(t.Context(), "sh", "-c", prelude+fault.removeScript())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("absence of owned rules must preserve unrelated rules: %v, %s", err, output)
	}
}

func TestBlockFaultRestorationIsIdempotentAndActivationStartsNewCycle(t *testing.T) {
	host := &blockTestHost{}
	fault, err := NewBlockNetworkFault(host, blockTestNetwork(), BlockStorageLoss, "reuse", false)
	if err != nil {
		t.Fatal(err)
	}

	fault.prepared = true
	if err := fault.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := fault.Confirm(t.Context()); err != nil {
		t.Fatal(err)
	}

	firstActivation := fault.Evidence().ActiveAt
	if firstActivation.IsZero() {
		t.Fatal("confirmation must record its actual activation time")
	}

	if err := fault.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}

	commands := len(host.commands)
	if err := fault.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(host.commands) != commands {
		t.Fatal("a second restoration must not mutate host state")
	}

	if err := fault.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}

	evidence := fault.Evidence()
	if !evidence.ActiveAt.IsZero() || !evidence.RestoredAt.IsZero() || !evidence.DispatchedAt.After(firstActivation) {
		t.Fatalf("a new fault cycle must not reuse prior confirmation times: %+v", evidence)
	}
}

func TestBlockStorageHandlePermitsIndependentAPIFault(t *testing.T) {
	host := &blockTestHost{run: func(command string) (string, error) {
		if strings.Contains(command, "/dev/tcp/") {
			return "", errors.New("a separate API fault remains active")
		}

		return "", nil
	}}
	fault, err := NewBlockNetworkFault(host, blockTestNetwork(), BlockStorageLoss, "independent", false)
	if err != nil {
		t.Fatal(err)
	}

	fault.prepared = true
	if err := fault.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := fault.Confirm(t.Context()); err != nil {
		t.Fatalf("a storage handle must check its own fault while API loss continues: %v", err)
	}

	if err := fault.Restore(t.Context()); err != nil {
		t.Fatalf("storage restoration must permit a separate active API fault: %v", err)
	}
}

func TestBlockPartitionPreservesStorageInBothDirections(t *testing.T) {
	fault, err := NewBlockNetworkFault(&blockTestHost{}, blockTestNetwork(),
		BlockSingletonIsolation, "storage-exception", false)
	if err != nil {
		t.Fatal(err)
	}

	for _, tool := range []string{"iptables", "ip6tables"} {
		rules := fault.rules(tool)
		for _, endpoint := range fault.network.Storage {
			if blockFamily(endpoint.Address) != tool {
				continue
			}

			outgoing, incoming, rejected := -1, -1, len(rules)
			for ruleIndex, rule := range rules {
				if strings.Contains(rule, "-d "+endpoint.Address+" --dport") && strings.HasSuffix(rule, "-j RETURN") {
					outgoing = ruleIndex
				}

				if strings.Contains(rule, "-s "+endpoint.Address+" --sport") && strings.HasSuffix(rule, "-j RETURN") {
					incoming = ruleIndex
				}

				if strings.HasSuffix(rule, "-j REJECT") && ruleIndex < rejected {
					rejected = ruleIndex
				}
			}

			if outgoing < 0 || incoming < 0 || outgoing >= rejected || incoming >= rejected {
				t.Fatalf("partition must preserve storage requests and responses before rejecting cluster traffic: %v", rules)
			}
		}
	}
}

func TestBlockLatencyRequiresCurrentEndpointFilters(t *testing.T) {
	host := &blockTestHost{run: func(command string) (string, error) {
		if strings.Contains(command, "qdisc show") {
			return `[{"kind":"netem","handle":"7991:","packets":42,"options":{"delay":{"delay":20,"jitter":0}}}]`, nil
		}

		if strings.Contains(command, "filter show") {
			return `[]`, nil
		}

		return "", nil
	}}
	fault, err := NewBlockNetworkFault(host, blockTestNetwork(), BlockStorageLatency, "latency", false)
	if err != nil {
		t.Fatal(err)
	}

	fault.prepared = true
	fault.device = "test0"
	if err := fault.Confirm(t.Context()); err == nil {
		t.Fatal("historical latency packets must not replace current endpoint filters")
	}

	if !fault.Evidence().ActiveAt.IsZero() {
		t.Fatal("an unconfirmed latency fault must not have an activation time")
	}
}

func TestBlockJournalRestoresAfterLostActivationAcknowledgement(t *testing.T) {
	host := &blockTestHost{run: func(command string) (string, error) {
		if command == "sudo chattr +i /var/lib/medik8s/state" {
			return "", errors.New("SSH acknowledgement lost")
		}

		if strings.Contains(command, "command -v chattr") {
			return "---------------------- /var/lib/medik8s/state", nil
		}

		return "", nil
	}}
	fault, err := NewBlockJournalFault(host, "lost-ack")
	if err != nil {
		t.Fatal(err)
	}

	if err := fault.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := fault.Activate(t.Context()); err == nil {
		t.Fatal("activation must report its lost acknowledgement")
	}

	if err := fault.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}

	for _, command := range host.commands {
		if command == "sudo chattr -i /var/lib/medik8s/state" {
			return
		}
	}

	t.Fatal("cleanup must restore a possibly applied immutable flag")
}

func TestParseCephEndpointsPreservesAdvertisedPortsAndIPv6(t *testing.T) {
	monitors := []byte(`{"mons":[{"public_addrs":{"addrvec":[` +
		`{"addr":"192.0.2.2:4330/0"},{"addr":"[2001:db8::2]:4331/0"}]}}]}`)
	osds := []byte(`{"osds":[{"public_addrs":{"addrvec":[{"addr":"192.0.2.2:8123/0"},{"addr":"[2001:db8::2]:8124/0"}]}}]}`)
	endpoints, err := ParseCephEndpoints(monitors, osds)
	if err != nil {
		t.Fatal(err)
	}

	if len(endpoints) != 4 || endpoints[0].Port != 4330 || endpoints[3].Port != 8124 {
		t.Fatalf("use every advertised endpoint without a fixed port range: %v", endpoints)
	}
}

func TestDiscoverRBDStorageClassRequiresUnambiguousSelection(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "rbd-a"}, Provisioner: "rook-ceph.rbd.csi.ceph.com"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "rbd-b"}, Provisioner: "rook-ceph.rbd.csi.ceph.com"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "nfs"}, Provisioner: "nfs.example.com"},
	).Build()
	_, err := DiscoverRBDStorageClass(t.Context(), k8sClient, "")
	var prerequisite *BlockPrerequisiteError
	if !errors.As(err, &prerequisite) {
		t.Fatalf("ambiguous classes must report a prerequisite: %v", err)
	}

	selected, err := DiscoverRBDStorageClass(t.Context(), k8sClient, "rbd-b")
	if err != nil || selected != "rbd-b" {
		t.Fatalf("select the existing explicit RBD class: %s, %v", selected, err)
	}

	if _, err := DiscoverRBDStorageClass(t.Context(), k8sClient, "nfs"); !errors.As(err, &prerequisite) {
		t.Fatalf("an override cannot select a different provisioner: %v", err)
	}
}
