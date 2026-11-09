package sbrutils

import (
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ErrNoNetworkFenceClass means that CSI unfencing is not configured for this cluster.
var ErrNoNetworkFenceClass = errors.New("no matching Ceph network fence class")

// NetworkUnfenceRequest delegates recovery to the CSI credentials of this Ceph cluster.
// The request references the existing class; it never reads or copies secret data.
// ErrNoNetworkFenceClass permits callers to fall back to a privileged Ceph CLI identity.
func NetworkUnfenceRequest(
	classes *unstructured.UnstructuredList, clusterID, cidr string,
) (*unstructured.Unstructured, error) {
	hostCIDR, err := FenceRangeCIDR(cidr)
	if err != nil {
		return nil, err
	}
	var className string
	for _, fenceClass := range classes.Items {
		configuredCluster, _, _ := unstructured.NestedString(fenceClass.Object, "spec", "parameters", "clusterID")
		if configuredCluster != clusterID {
			continue
		}
		provisioner, _, _ := unstructured.NestedString(fenceClass.Object, "spec", "provisioner")
		if strings.HasSuffix(provisioner, ".rbd.csi.ceph.com") {
			className = fenceClass.GetName()

			break
		}
		if strings.HasSuffix(provisioner, ".cephfs.csi.ceph.com") {
			className = fenceClass.GetName()
		}
	}
	if className == "" {
		return nil, ErrNoNetworkFenceClass
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "csiaddons.openshift.io/v1alpha1", "kind": "NetworkFence",
		"metadata": map[string]interface{}{"generateName": "sbr-test-unfence-"},
		"spec": map[string]interface{}{
			"networkFenceClassName": className, "fenceState": "Unfenced", "cidrs": []interface{}{hostCIDR},
		},
	}}, nil
}
