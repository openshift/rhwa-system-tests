package sbrutils

import (
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeFenceCIDRsUsesReportedAddresses(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-1", Annotations: map[string]string{"k8s.ovn.org/node-id": "7"},
		},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: "worker-1"},
			{Type: corev1.NodeInternalIP, Address: "192.168.126.22"},
			{Type: corev1.NodeInternalIP, Address: "2001:db8::7"},
			{Type: corev1.NodeInternalIP, Address: "192.168.126.22"},
		}},
	}
	cidrs, err := NodeFenceCIDRs(node)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"192.168.126.22/32", "2001:db8::7/128"}
	if !reflect.DeepEqual(cidrs, expected) {
		t.Fatalf("expected reported node addresses %v, got %v", expected, cidrs)
	}
}

func TestNodeFenceCIDRsRejectsUnknownAddresses(t *testing.T) {
	for _, address := range []string{"", "worker-1", "192.168.126.22/24"} {
		t.Run(address, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: "worker-1", Annotations: map[string]string{"k8s.ovn.org/node-id": "7"},
			}}
			if address != "" {
				node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}}
			}
			if cidrs, err := NodeFenceCIDRs(node); err == nil {
				t.Fatalf("expected missing or invalid InternalIP to fail, got %v", cidrs)
			}
		})
	}
}

func TestFenceRangeCIDR(t *testing.T) {
	for _, testCase := range []struct{ value, expected string }{
		{"192.168.126.22:0/32", "192.168.126.22/32"},
		{"192.168.126.22/32", "192.168.126.22/32"},
		{"[2001:db8::7]:0/128", "2001:db8::7/128"},
		{"2001:db8::7/128", "2001:db8::7/128"},
		{"192.168.126.0:0/24", ""},
		{"192.168.126.22:3300/32", ""},
		{"192.168.126.22:0/3601542234", ""},
		{"invalid", ""},
	} {
		t.Run(testCase.value, func(t *testing.T) {
			actual, err := FenceRangeCIDR(testCase.value)
			if testCase.expected == "" {
				if err == nil {
					t.Fatalf("unsafe fence range was accepted: %s", actual)
				}

				return
			}
			if err != nil || actual != testCase.expected {
				t.Fatalf("expected %s, got %s, error %v", testCase.expected, actual, err)
			}
		})
	}
}

func TestCephCommandSelectsConfiguredIdentity(t *testing.T) {
	for _, testCase := range []struct{ name, sections, client string }{
		{"external", "client.healthchecker\r\n", "client.healthchecker"},
		{"rotated external", "client.healthchecker.2\n", "client.healthchecker.2"},
		{"internal", "client.admin\n", "client.admin"},
		{"default", "", "client.admin"},
		{"multiple with admin", "client.healthchecker\nclient.admin\n", "client.admin"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var commands [][]string
			exec := func(command []string) (string, error) {
				commands = append(commands, command)
				if len(commands) == 1 {
					return testCase.sections, nil
				}

				return "[]", nil
			}
			ceph, err := CephCommand(exec, "/rook/cluster.config")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ceph("osd", "blocklist", "ls", "--format", "json"); err != nil {
				t.Fatal(err)
			}
			expected := [][]string{
				{"ceph-conf", "--conf", "/rook/cluster.config", "--list-sections", "client."},
				{
					"ceph", "--conf", "/rook/cluster.config", "--name", testCase.client, "--connect-timeout", "30",
					"osd", "blocklist", "ls", "--format", "json",
				},
			}
			if !reflect.DeepEqual(commands, expected) {
				t.Fatalf("expected discovery and authenticated command %v, got %v", expected, commands)
			}
		})
	}
}

func TestCephCommandRejectsAmbiguousIdentity(t *testing.T) {
	for _, sections := range []string{"client.a\nclient.b\n", "invalid", "client."} {
		_, err := CephCommand(func([]string) (string, error) { return sections, nil }, "/rook/cluster.config")
		if err == nil {
			t.Fatalf("expected ambiguous or invalid client selection to fail: %q", sections)
		}
	}
}

func TestCephCommandPropagatesErrors(t *testing.T) {
	failure := errors.New("pod exec failed")
	_, err := CephCommand(func([]string) (string, error) { return "", failure }, "/rook/cluster.config")
	if !errors.Is(err, failure) {
		t.Fatalf("expected discovery failure, got %v", err)
	}
	ceph, err := CephCommand(func(command []string) (string, error) {
		if command[0] == "ceph-conf" {
			return "client.healthchecker", nil
		}

		return "", failure
	}, "/rook/cluster.config")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ceph("osd", "blocklist", "ls"); !errors.Is(err, failure) {
		t.Fatalf("expected Ceph command failure, got %v", err)
	}
}
