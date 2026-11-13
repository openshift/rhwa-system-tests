package tests

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func TestBPNReadFixedSpecDurations(t *testing.T) {
	gomega.RegisterTestingT(t)
	for _, testCase := range []struct {
		name     string
		field    string
		expected string
		actual   interface{}
		wantFail bool
	}{
		{name: "outage grace", field: "totalOutageGracePeriod", expected: "60s", actual: "1m0s"},
		{name: "recovery period", field: "autonomousRebootRecoveryPeriod", expected: "1m", actual: "60s"},
		{name: "I/O timeout", field: "ioTimeout", expected: "15s", actual: "15000ms"},
		{name: "NHC healthy delay", field: "healthyDelay", expected: "1m", actual: "60s"},
		{name: "changed duration", field: "totalOutageGracePeriod", expected: "60s", actual: "61s", wantFail: true},
		{name: "invalid duration", field: "totalOutageGracePeriod", expected: "60s", actual: "invalid", wantFail: true},
		{
			name: "invalid expected duration", field: "totalOutageGracePeriod",
			expected: "invalid", actual: "invalid", wantFail: true,
		},
		{name: "wrong type", field: "totalOutageGracePeriod", expected: "60s", actual: int64(60), wantFail: true},
		{name: "missing duration", field: "totalOutageGracePeriod", expected: "60s", wantFail: true},
		{name: "same string", field: "sharedStorageClass", expected: "60s", actual: "60s"},
		{name: "changed string", field: "sharedStorageClass", expected: "60s", actual: "1m0s", wantFail: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			object := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "storage-based-remediation.medik8s.io/v1alpha1",
				"kind":       "StorageBasedRemediationConfig",
				"metadata":   map[string]interface{}{"name": "duration-test", "uid": "config-uid"},
				"spec":       map[string]interface{}{},
			}}
			if testCase.actual != nil {
				if err := unstructured.SetNestedField(object.Object, testCase.actual, "spec", testCase.field); err != nil {
					t.Fatal(err)
				}
			}
			resource := fake.NewSimpleDynamicClient(runtime.NewScheme(), object).Resource(schema.GroupVersionResource{
				Group: "storage-based-remediation.medik8s.io", Version: "v1alpha1", Resource: "storagebasedremediationconfigs",
			})
			fixture := &bpnFixture{ctx: context.Background(), cleaning: true}
			err := gomega.InterceptGomegaFailure(func() {
				fixture.readFixedSpec(object, map[string]interface{}{testCase.field: testCase.expected}, resource)
			})
			if (err != nil) != testCase.wantFail {
				t.Fatalf("readFixedSpec failure = %v, want failure = %t", err, testCase.wantFail)
			}
		})
	}
}
