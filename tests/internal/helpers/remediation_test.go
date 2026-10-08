package helpers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	testDeletePollInterval = 10 * time.Millisecond
	testDeleteTimeout      = 50 * time.Millisecond
)

func TestDeleteRemediationCRReportsDeleteRejection(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "remediation.medik8s.io", Version: "v1alpha1", Kind: "NodeHealthCheck"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	nhc := &unstructured.Unstructured{}
	nhc.SetGroupVersionKind(gvk)
	nhc.SetName("nhc-test-custom-template")
	rejection := errors.New("deletion prohibited due to running remediation")
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nhc).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return rejection
			},
		}).Build()

	var out strings.Builder

	gone := DeleteRemediationCR(context.Background(), k8sClient, gvk, nhc.GetName(), "",
		testDeletePollInterval, testDeleteTimeout, func(format string, args ...interface{}) {
			fmt.Fprintf(&out, format, args...)
		})
	if gone {
		t.Error("expected DeleteRemediationCR to report the rejected CR as not gone")
	}

	if got := strings.Count(out.String(), "DeleteRemediationCR(NodeHealthCheck nhc-test-custom-template): "+
		rejection.Error()); got != 1 {
		t.Errorf("expected the rejection to be logged once, got %d:\n%s", got, out.String())
	}

	if !strings.Contains(out.String(), "last API error: "+rejection.Error()) {
		t.Errorf("timeout warning does not include the last API error:\n%s", out.String())
	}
}

func TestDeleteRemediationCRReportsDeletedCRGone(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "remediation.medik8s.io", Version: "v1alpha1", Kind: "NodeHealthCheck"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	nhc := &unstructured.Unstructured{}
	nhc.SetGroupVersionKind(gvk)
	nhc.SetName("nhc-test-escalation-basic")
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nhc).Build()

	if !DeleteRemediationCR(context.Background(), k8sClient, gvk, nhc.GetName(), "",
		testDeletePollInterval, testDeleteTimeout, t.Logf) {
		t.Error("expected DeleteRemediationCR to report the deleted CR as gone")
	}
}

func TestDeleteRemediationCRStopsOnForbidden(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "remediation.medik8s.io", Version: "v1alpha1", Kind: "NodeHealthCheck"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	nhc := &unstructured.Unstructured{}
	nhc.SetGroupVersionKind(gvk)
	nhc.SetName("nhc-test-forbidden")
	deleteCalls := 0
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nhc).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				deleteCalls++

				return k8serrors.NewForbidden(gvk.GroupVersion().WithResource("nodehealthchecks").GroupResource(),
					nhc.GetName(), errors.New("no delete permission"))
			},
		}).Build()

	if DeleteRemediationCR(context.Background(), k8sClient, gvk, nhc.GetName(), "",
		testDeletePollInterval, testDeleteTimeout, t.Logf) {
		t.Error("expected DeleteRemediationCR to report the forbidden CR as not gone")
	}

	if deleteCalls != 1 {
		t.Errorf("expected Forbidden to stop retries after 1 delete, got %d", deleteCalls)
	}
}

func TestIsPollDeadlineError(t *testing.T) {
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{
			"rate limiter before deadline", context.Background(),
			errors.New("client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline"), true,
		},
		{"wrapped deadline", context.Background(), fmt.Errorf("get: %w", context.DeadlineExceeded), true},
		{"context already done", expired, errors.New("any"), true},
		{"webhook denial", context.Background(), errors.New("deletion prohibited due to running remediation"), false},
	}

	for _, tc := range cases {
		if got := IsPollDeadlineError(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: got %t, want %t", tc.name, got, tc.want)
		}
	}
}
