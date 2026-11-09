package sbrutils

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// NodeFenceCIDRs returns host ranges for the node addresses used by host-network CSI pods.
// OVN node IDs do not identify these addresses, particularly with external Ceph.
func NodeFenceCIDRs(node *corev1.Node) ([]string, error) {
	var cidrs []string
	for _, address := range node.Status.Addresses {
		if address.Type != corev1.NodeInternalIP {
			continue
		}
		ip, err := netip.ParseAddr(address.Address)
		if err != nil {
			return nil, fmt.Errorf("node %s has invalid InternalIP %q: %w", node.Name, address.Address, err)
		}
		ip = ip.Unmap()
		cidr := netip.PrefixFrom(ip, ip.BitLen()).String()
		if !slices.Contains(cidrs, cidr) {
			cidrs = append(cidrs, cidr)
		}
	}
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("node %s has no InternalIP for Ceph fence recovery", node.Name)
	}

	return cidrs, nil
}

// FenceRangeCIDR normalizes Ceph's address:0/prefix representation to a host CIDR.
// Wider ranges must never be attributed to, or removed for, a single test node.
func FenceRangeCIDR(value string) (string, error) {
	if slash := strings.LastIndex(value, "/"); slash >= 0 {
		if host, port, err := net.SplitHostPort(value[:slash]); err == nil && port == "0" {
			value = host + value[slash:]
		}
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return "", fmt.Errorf("parse Ceph fence range %q: %w", value, err)
	}
	if prefix.Bits() != prefix.Addr().BitLen() {
		return "", fmt.Errorf("ceph fence range %q covers more than one host", value)
	}

	return prefix.String(), nil
}

// CephCommand discovers the configured client without reading or copying its key.
// Configurations without a client section retain Ceph's client.admin default.
func CephCommand(
	exec func([]string) (string, error), configFile string,
) (func(...string) (string, error), error) {
	sections, err := exec([]string{"ceph-conf", "--conf", configFile, "--list-sections", "client."})
	if err != nil {
		return nil, fmt.Errorf("discover Ceph client in %s: %w", configFile, err)
	}
	clients := strings.Fields(sections)
	clientName := "client.admin"
	if len(clients) == 1 {
		clientName = clients[0]
	} else if len(clients) > 1 && !slices.Contains(clients, clientName) {
		return nil, fmt.Errorf("ambiguous Ceph clients in %s: %v", configFile, clients)
	}
	if !strings.HasPrefix(clientName, "client.") || clientName == "client." {
		return nil, fmt.Errorf("invalid Ceph client name %q in %s", clientName, configFile)
	}

	return func(args ...string) (string, error) {
		command := []string{"ceph", "--conf", configFile, "--name", clientName, "--connect-timeout", "30"}

		return exec(append(command, args...))
	}, nil
}
