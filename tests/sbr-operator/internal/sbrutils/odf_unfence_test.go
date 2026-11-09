package sbrutils

import (
	"errors"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func networkFenceClass(name, cluster, provisioner string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": name},
		"spec": map[string]interface{}{
			"provisioner": provisioner, "parameters": map[string]interface{}{"clusterID": cluster},
		},
	}}
}

func TestNetworkUnfenceRequestUsesMatchingClusterCredentials(t *testing.T) {
	classes := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		networkFenceClass("other-cluster", "other", "rook-ceph.rbd.csi.ceph.com"),
		networkFenceClass("external-cephfs", "openshift-storage", "openshift-storage.cephfs.csi.ceph.com"),
		networkFenceClass("external-rbd", "openshift-storage", "openshift-storage.rbd.csi.ceph.com"),
	}}
	request, err := NetworkUnfenceRequest(classes, "openshift-storage", "192.168.126.22/32")
	if err != nil {
		t.Fatal(err)
	}
	if request == nil {
		t.Fatal("expected CSI unfencing for the matching external Ceph cluster")
	}
	expected := map[string]interface{}{
		"networkFenceClassName": "external-rbd", "fenceState": "Unfenced",
		"cidrs": []interface{}{"192.168.126.22/32"},
	}
	if !reflect.DeepEqual(request.Object["spec"], expected) {
		t.Fatalf("expected only the selected host and existing credential class %v, got %v", expected, request.Object["spec"])
	}
}

func TestNetworkUnfenceRequestSupportsCephFSClass(t *testing.T) {
	classes := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		networkFenceClass("cephfs", "openshift-storage", "openshift-storage.cephfs.csi.ceph.com"),
	}}
	request, err := NetworkUnfenceRequest(classes, "openshift-storage", "192.168.126.22/32")
	if err != nil || request == nil {
		t.Fatalf("expected CephFS class support, got request %v, error %v", request, err)
	}
	className, _, _ := unstructured.NestedString(request.Object, "spec", "networkFenceClassName")
	if className != "cephfs" {
		t.Fatalf("expected CephFS credential class, got %s", className)
	}
}

func TestNetworkUnfenceRequestRejectsUnrelatedClassesAndBroadRanges(t *testing.T) {
	classes := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		networkFenceClass("other-cluster", "other", "rook-ceph.rbd.csi.ceph.com"),
		networkFenceClass("other-driver", "openshift-storage", "other.csi.example.com"),
	}}
	request, err := NetworkUnfenceRequest(classes, "openshift-storage", "192.168.126.22/32")
	if !errors.Is(err, ErrNoNetworkFenceClass) || request != nil {
		t.Fatalf("unrelated classes must not be used: request %v, error %v", request, err)
	}
	if _, err := NetworkUnfenceRequest(classes, "openshift-storage", "192.168.126.0/24"); err == nil {
		t.Fatal("must refuse an unfencing request spanning multiple hosts")
	}
}
