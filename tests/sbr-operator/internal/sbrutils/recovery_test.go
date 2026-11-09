package sbrutils

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func recoveryNHC() *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "remediation.medik8s.io/v1alpha1", "kind": "NodeHealthCheck",
		"metadata": map[string]interface{}{"name": "test-nhc", "uid": "owned-uid"},
		"spec":     map[string]interface{}{"pauseRequests": []interface{}{"other-actor", "our-token"}},
		"status":   map[string]interface{}{"phase": "Paused"},
	}}

	return object
}

func TestRecoveryPauseRequiresAcknowledgedOriginalNHC(t *testing.T) {
	for _, testCase := range []string{
		"acknowledged", "different UID", "request missing", "not acknowledged", "deleting", "missing UID", "missing token",
	} {
		t.Run(testCase, func(t *testing.T) {
			nhc := recoveryNHC()
			uid, token := nhc.GetUID(), "our-token"
			switch testCase {
			case "different UID":
				nhc.SetUID("replacement")
			case "request missing":
				unstructured.RemoveNestedField(nhc.Object, "spec", "pauseRequests")
			case "not acknowledged":
				nhc.Object["status"] = map[string]interface{}{"phase": "Enabled"}
			case "deleting":
				now := metav1.Now()
				nhc.SetDeletionTimestamp(&now)
			case "missing UID":
				uid = ""
			case "missing token":
				token = ""
			}
			err := RecoveryPaused(nhc, uid, token)
			if (err == nil) != (testCase == "acknowledged") {
				t.Fatalf("unexpected pause eligibility: %v", err)
			}
		})
	}
}

func TestSetRecoveryPausePreservesOtherActors(t *testing.T) {
	nhc := recoveryNHC()
	api := fake.NewClientBuilder().WithObjects(nhc.DeepCopy()).Build()
	for _, paused := range []bool{true, true, false, false} {
		if err := SetRecoveryPause(context.Background(), api, nhc, "our-token", paused); err != nil {
			t.Fatal(err)
		}
		current := nhc.DeepCopy()
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(nhc), current); err != nil {
			t.Fatal(err)
		}
		requests, _, err := unstructured.NestedStringSlice(current.Object, "spec", "pauseRequests")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"other-actor"}
		if paused {
			want = append(want, "our-token")
		}
		if !reflect.DeepEqual(requests, want) {
			t.Fatalf("pause requests = %v, want %v", requests, want)
		}
	}
}

func TestSetRecoveryPauseRejectsReplacedNHC(t *testing.T) {
	original := recoveryNHC()
	replacement := original.DeepCopy()
	replacement.SetUID("replacement")
	api := fake.NewClientBuilder().WithObjects(replacement).Build()
	if err := SetRecoveryPause(context.Background(), api, original, "our-token", true); err == nil {
		t.Fatal("must not change a replacement NHC")
	}
	current := replacement.DeepCopy()
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(replacement), current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current.Object["spec"], replacement.Object["spec"]) {
		t.Fatal("replacement NHC was modified")
	}
}
