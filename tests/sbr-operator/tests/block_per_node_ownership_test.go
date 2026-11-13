package tests

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/clients"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestBPNConfigurationOwnershipRetainsProbeClaims(t *testing.T) {
	objects := []*unstructured.Unstructured{
		bpnOwnershipObject("DaemonSet", "agent", "set", "config"),
		bpnOwnershipObject("Pod", "agent-a", "pod-a", "set"),
		bpnOwnershipObject("Pod", "agent-b", "pod-b", "set"),
		bpnOwnershipObject("PersistentVolumeClaim", "probe-a", "claim-a", "pod-a"),
		bpnOwnershipObject("PersistentVolumeClaim", "probe-b", "claim-b", "pod-b"),
	}
	testCase, fakeClient := bpnOwnershipFixture(t, objects...)
	owners := map[types.UID]bool{}
	if err := testCase.configurationOwners(owners); err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		if !owners[object.GetUID()] {
			t.Fatalf("cleanup did not capture %s UID %s", object.GetKind(), object.GetUID())
		}
	}
	for _, object := range objects[:3] {
		resource := "pods"
		group := ""
		if object.GetKind() == "DaemonSet" {
			resource, group = "daemonsets", "apps"
		}
		gvr := schema.GroupVersionResource{Group: group, Version: "v1", Resource: resource}
		if err := fakeClient.Tracker().Delete(gvr, object.GetNamespace(), object.GetName()); err != nil {
			t.Fatal(err)
		}
	}
	for _, claim := range objects[3:] {
		claim.SetOwnerReferences(nil)
		gvr := schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
		if err := fakeClient.Tracker().Update(gvr, claim, claim.GetNamespace()); err != nil {
			t.Fatal(err)
		}
	}
	remaining, err := testCase.configurationOperands(context.Background(), owners)
	if err != nil || len(remaining) != 2 {
		t.Fatalf("remaining probe claims = %v, error = %v", remaining, err)
	}
}

func TestBPNConfigurationOwnershipIncludesDeletedAgentHistory(t *testing.T) {
	set := bpnOwnershipObject("DaemonSet", "agent", "set", "config")
	claim := bpnOwnershipObject("PersistentVolumeClaim", "old-probe", "claim", "old-pod")
	testCase, _ := bpnOwnershipFixture(t, set, claim)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "old-agent", Namespace: medik8sparams.OperatorNs, UID: "old-pod",
		OwnerReferences: []metav1.OwnerReference{{UID: "set"}},
	}}
	stream := watch.NewRaceFreeFake()
	recorder, err := helpers.StartObjectRecorder(helpers.RecorderConfig[*corev1.Pod]{
		Description: "historical agent", StartTimeout: time.Second, APICallTimeout: time.Second,
		List: func(context.Context) ([]*corev1.Pod, string, error) {
			return []*corev1.Pod{pod}, "1", nil
		},
		Watch: func(context.Context, string) (watch.Interface, error) {
			return stream, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(recorder.Stop)
	testCase.podRecorder = recorder
	owners := map[types.UID]bool{}
	if err := testCase.configurationOwners(owners); err != nil {
		t.Fatal(err)
	}
	if !owners[pod.UID] || !owners[claim.GetUID()] {
		t.Fatal("the deleted agent must retain ownership of its probe claim")
	}
}

func TestBPNConfigurationOwnershipPreservesUnownedReplacements(t *testing.T) {
	objects := []*unstructured.Unstructured{
		bpnOwnershipObject("DaemonSet", "agent", "new-set", "foreign-config"),
		bpnOwnershipObject("Pod", "agent-a", "new-pod", "new-set"),
		bpnOwnershipObject("PersistentVolumeClaim", "probe-a", "new-claim", "new-pod"),
	}
	for _, object := range objects {
		object.SetLabels(map[string]string{"sbrconfig": "test-config"})
	}
	testCase, fakeClient := bpnOwnershipFixture(t, objects...)
	owners := map[types.UID]bool{"config": true, "old-set": true, "old-pod": true, "old-claim": true}
	if err := testCase.configurationGone(context.Background(), owners); err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		if owners[object.GetUID()] {
			t.Fatalf("cleanup adopted unowned replacement UID %s", object.GetUID())
		}
	}
	for _, action := range fakeClient.Actions() {
		if action.GetVerb() != "list" {
			t.Fatalf("ownership checks must not change unowned resources: %s", action.GetVerb())
		}
	}
}

func TestBPNConfigurationOwnershipScansAfterListFailure(t *testing.T) {
	pod := bpnOwnershipObject("Pod", "reporter", "pod", "config")
	claim := bpnOwnershipObject("PersistentVolumeClaim", "probe", "claim", "pod")
	testCase, fakeClient := bpnOwnershipFixture(t, pod, claim)
	listFailure := errors.New("daemonset list unavailable")
	fakeClient.PrependReactor("list", "daemonsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, listFailure
	})
	owners := map[types.UID]bool{}
	if err := testCase.configurationOwners(owners); !errors.Is(err, listFailure) {
		t.Fatalf("cleanup must retain the list failure: %v", err)
	}
	if !owners[pod.GetUID()] || !owners[claim.GetUID()] {
		t.Fatal("the scan must capture all reachable descendants after a list failure")
	}
	if len(fakeClient.Actions()) != 3 {
		t.Fatalf("the scan must inspect all three kinds, got %d calls", len(fakeClient.Actions()))
	}
}

func TestBPNConfigurationOwnershipFailurePreservesFixtures(t *testing.T) {
	testCase, fakeClient := bpnOwnershipFixture(t)
	listFailure := errors.New("configuration ownership unavailable")
	fakeClient.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, listFailure
	})
	failures := map[string]error{}
	testCase.removeCleanupFixtures(func(label string, operation func() error) {
		if strings.HasPrefix(label, "delete ") {
			t.Fatalf("cleanup must preserve fixtures when ownership capture fails: %s", label)
		}
		if err := operation(); err != nil {
			failures[label] = err
		}
	})
	if !errors.Is(failures["record configuration ownership"], listFailure) {
		t.Fatalf("cleanup must report the ownership failure: %v", failures)
	}
	if len(fakeClient.Actions()) != 3 {
		t.Fatalf("cleanup must attempt all three ownership lists, got %d calls", len(fakeClient.Actions()))
	}
	for _, action := range fakeClient.Actions() {
		if action.GetVerb() != "list" {
			t.Fatalf("cleanup must preserve fixtures after list failure: %s", action.GetVerb())
		}
	}
}

func bpnOwnershipObject(kind, name string, uid, owner types.UID) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetAPIVersion("v1")
	if kind == "DaemonSet" {
		object.SetAPIVersion("apps/v1")
	}
	object.SetKind(kind)
	object.SetName(name)
	object.SetNamespace(medik8sparams.OperatorNs)
	object.SetUID(uid)
	object.SetOwnerReferences([]metav1.OwnerReference{{UID: owner}})

	return object
}

func bpnOwnershipFixture(t *testing.T,
	objects ...*unstructured.Unstructured,
) (*bpnFixture, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	resources := make([]runtime.Object, 0, len(objects))
	for _, object := range objects {
		resources = append(resources, object)
	}
	fakeClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "apps", Version: "v1", Resource: "daemonsets"}: "DaemonSetList",
			{Version: "v1", Resource: "pods"}:                      "PodList",
			{Version: "v1", Resource: "persistentvolumeclaims"}:    "PersistentVolumeClaimList",
		}, resources...)
	originalClient := APIClient
	APIClient = &clients.Settings{Interface: fakeClient}
	t.Cleanup(func() { APIClient = originalClient })
	testCase := &bpnFixture{config: &unstructured.Unstructured{}}
	testCase.config.SetUID("config")

	return testCase, fakeClient
}
