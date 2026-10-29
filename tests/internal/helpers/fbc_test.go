package helpers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/clients"
	olmV1alpha1 "github.com/rh-ecosystem-edge/eco-goinfra/pkg/schemes/olm/operators/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPodContainerUsesDigestPinnedImageThroughTaggedSpec(t *testing.T) {
	digest := strings.Repeat("a", 64)
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "manager", Image: "ttl.sh/operator:3h",
		}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "manager", ImageID: "ttl.sh/operator@sha256:" + digest,
		}}},
	}

	if !PodContainerUsesImage(pod, "manager", "ttl.sh/operator@sha256:"+digest) {
		t.Fatal("tagged container spec did not match its runtime digest")
	}

	if PodContainerUsesImage(pod, "manager", "ttl.sh/operator@sha256:"+strings.Repeat("b", 64)) {
		t.Fatal("container matched a different runtime digest")
	}
}

func catalogSwitchSubscription() *olmV1alpha1.Subscription {
	return &olmV1alpha1.Subscription{
		ObjectMeta: metav1.ObjectMeta{Name: "upgrade-sub", Namespace: "operator-tests"},
		Spec: &olmV1alpha1.SubscriptionSpec{
			Package: "test-operator", CatalogSource: "redhat-operators",
			CatalogSourceNamespace: "openshift-marketplace", Channel: "stable",
		},
	}
}

func TestSwitchSubscriptionCatalogPreservesConcurrentUpdate(t *testing.T) {
	original := catalogSwitchSubscription()
	updateCalls := 0
	api := clients.GetTestClients(clients.TestClientParams{
		K8sMockObjects:  []runtime.Object{original},
		SchemeAttachers: []clients.SchemeAttacher{olmV1alpha1.AddToScheme},
		InterceptorFuncs: interceptor.Funcs{
			Update: func(ctx context.Context, fakeClient client.WithWatch, object client.Object,
				options ...client.UpdateOption,
			) error {
				updateCalls++
				if updateCalls == 1 {
					latest := &olmV1alpha1.Subscription{}
					if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(original), latest); err != nil {
						return err
					}
					latest.Annotations = map[string]string{"olm-progress": "preserved"}
					if err := fakeClient.Update(ctx, latest); err != nil {
						return err
					}

					return apierrors.NewConflict(schema.GroupResource{
						Group: "operators.coreos.com", Resource: "subscriptions",
					}, original.Name, errors.New("OLM update"))
				}

				return fakeClient.Update(ctx, object, options...)
			},
		},
	})
	updated, err := SwitchSubscriptionCatalog(api, original.Name, original.Namespace, "candidate", "candidate-channel")
	if err != nil {
		t.Fatalf("catalog switch must recover from a concurrent update: %v", err)
	}
	if updateCalls != 2 {
		t.Fatalf("wanted 2 update attempts, got %d", updateCalls)
	}
	live := &olmV1alpha1.Subscription{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(original), live); err != nil {
		t.Fatal(err)
	}
	if live.Spec.CatalogSource != "candidate" || live.Spec.Channel != "candidate-channel" {
		t.Fatalf("catalog and channel were not switched: %+v", live.Spec)
	}
	if live.Spec.Package != original.Spec.Package ||
		live.Spec.CatalogSourceNamespace != original.Spec.CatalogSourceNamespace {
		t.Fatalf("unrelated Subscription fields changed: %+v", live.Spec)
	}
	if live.Annotations["olm-progress"] != "preserved" || updated.Object.ResourceVersion != live.ResourceVersion {
		t.Fatalf("catalog switch must preserve the concurrent update: %+v", live)
	}
}

func TestSwitchSubscriptionCatalogDoesNotRetryForbidden(t *testing.T) {
	original := catalogSwitchSubscription()
	updateCalls := 0
	api := clients.GetTestClients(clients.TestClientParams{
		K8sMockObjects:  []runtime.Object{original},
		SchemeAttachers: []clients.SchemeAttacher{olmV1alpha1.AddToScheme},
		InterceptorFuncs: interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				updateCalls++

				return apierrors.NewForbidden(schema.GroupResource{
					Group: "operators.coreos.com", Resource: "subscriptions",
				}, original.Name, errors.New("denied"))
			},
		},
	})
	updated, err := SwitchSubscriptionCatalog(api, original.Name, original.Namespace, "candidate", "candidate-channel")
	if !apierrors.IsForbidden(err) || updated != nil || updateCalls != 1 {
		t.Fatalf("forbidden update must fail without retrying, got subscription=%v error=%v calls=%d",
			updated, err, updateCalls)
	}
}
