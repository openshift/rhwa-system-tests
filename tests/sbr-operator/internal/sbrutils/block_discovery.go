package sbrutils

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	storagev1 "k8s.io/api/storage/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BlockPrerequisiteError identifies an unavailable fault prerequisite before disruption.
type BlockPrerequisiteError struct {
	Reason string
}

func (err *BlockPrerequisiteError) Error() string {
	return err.Reason
}

// BlockEndpoint identifies one advertised TCP destination.
type BlockEndpoint struct {
	Address string
	Port    int
}

// DiscoverRBDStorageClass requires one RBD class unless the caller selects an eligible class.
func DiscoverRBDStorageClass(ctx context.Context, k8sClient client.Client, override string) (string, error) {
	classes := &storagev1.StorageClassList{}
	if err := k8sClient.List(ctx, classes); err != nil {
		return "", err
	}

	var eligible []string
	for _, storageClass := range classes.Items {
		if storageClass.Provisioner != "rbd.csi.ceph.com" &&
			!strings.HasSuffix(storageClass.Provisioner, ".rbd.csi.ceph.com") {
			continue
		}

		if override != "" && storageClass.Name == override {
			return override, nil
		}

		eligible = append(eligible, storageClass.Name)
	}

	if override != "" {
		return "", &BlockPrerequisiteError{Reason: fmt.Sprintf("StorageClass %q must use Ceph RBD CSI", override)}
	}

	if len(eligible) != 1 {
		sort.Strings(eligible)

		return "", &BlockPrerequisiteError{Reason: fmt.Sprintf(
			"exactly one Ceph RBD class is required, found %v, set SBR_RBD_STORAGE_CLASS", eligible)}
	}

	return eligible[0], nil
}

type blockCephAddressVector struct {
	Entries []struct {
		Address string `json:"addr"`
	} `json:"addrvec"`
}

type blockCephDaemon struct {
	Addresses     blockCephAddressVector `json:"public_addrs"`
	Address       string                 `json:"addr"`
	PublicAddress string                 `json:"public_addr"`
}

// ParseCephEndpoints uses advertised addresses from the selected backend's maps.
func ParseCephEndpoints(monJSON, osdJSON []byte) ([]BlockEndpoint, error) {
	var monitors struct {
		FSID     string            `json:"fsid"`
		Monitors []blockCephDaemon `json:"mons"`
	}
	var osds struct {
		FSID string            `json:"fsid"`
		OSDs []blockCephDaemon `json:"osds"`
	}
	if err := json.Unmarshal(monJSON, &monitors); err != nil {
		return nil, fmt.Errorf("decode Ceph monitor map: %w", err)
	}

	if err := json.Unmarshal(osdJSON, &osds); err != nil {
		return nil, fmt.Errorf("decode Ceph OSD map: %w", err)
	}

	if len(monitors.Monitors) == 0 || len(osds.OSDs) == 0 {
		return nil, &BlockPrerequisiteError{Reason: "the selected Ceph backend must advertise monitors and OSDs"}
	}

	if monitors.FSID != "" && osds.FSID != "" && monitors.FSID != osds.FSID {
		return nil, fmt.Errorf("the Ceph monitor and OSD maps identify different backends")
	}

	unique := map[BlockEndpoint]bool{}
	for _, monitor := range monitors.Monitors {
		if err := addCephAddresses(unique, monitor.Addresses, monitor.Address); err != nil {
			return nil, err
		}
	}
	for _, osd := range osds.OSDs {
		if err := addCephAddresses(unique, osd.Addresses, osd.PublicAddress); err != nil {
			return nil, err
		}
	}

	endpoints := make([]BlockEndpoint, 0, len(unique))
	for endpoint := range unique {
		endpoints = append(endpoints, endpoint)
	}

	sort.Slice(endpoints, func(first, second int) bool {
		if endpoints[first].Address == endpoints[second].Address {
			return endpoints[first].Port < endpoints[second].Port
		}

		return endpoints[first].Address < endpoints[second].Address
	})

	return endpoints, nil
}

func addCephAddresses(unique map[BlockEndpoint]bool, vector blockCephAddressVector, fallback string) error {
	var addresses []string
	for _, entry := range vector.Entries {
		addresses = append(addresses, entry.Address)
	}
	if len(addresses) == 0 {
		addresses = append(addresses, fallback)
	}
	for _, address := range addresses {
		endpoint, err := parseCephAddress(address)
		if err != nil {
			return err
		}
		unique[endpoint] = true
	}

	return nil
}

func parseCephAddress(value string) (BlockEndpoint, error) {
	address := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(value), "v1:"), "v2:")
	address, _, _ = strings.Cut(address, "/")
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return BlockEndpoint{}, fmt.Errorf("invalid advertised Ceph address %q: %w", value, err)
	}

	parsedIP, err := netip.ParseAddr(host)
	if err != nil || parsedIP.IsUnspecified() || parsedIP.Zone() != "" {
		return BlockEndpoint{}, fmt.Errorf("the Ceph endpoint %q must have a specific IP address", value)
	}

	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return BlockEndpoint{}, fmt.Errorf("the Ceph endpoint %q must have a valid port", value)
	}

	return BlockEndpoint{Address: parsedIP.String(), Port: port}, nil
}
