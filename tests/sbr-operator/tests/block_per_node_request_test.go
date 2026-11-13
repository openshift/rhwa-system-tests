package tests

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/clients"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type bpnUncertainRequestClient struct {
	client.Client
	persisted    *unstructured.Unstructured
	createErr    error
	getErr       error
	beforeCreate func(client.Object)
	deleted      bool
}

func (requestClient *bpnUncertainRequestClient) Create(_ context.Context, object client.Object,
	_ ...client.CreateOption,
) error {
	if requestClient.beforeCreate != nil {
		requestClient.beforeCreate(object)
	}
	if requestClient.persisted != nil {
		request, ok := object.(*unstructured.Unstructured)
		if !ok {
			return fmt.Errorf("request must be unstructured")
		}
		requestClient.persisted = request.DeepCopy()
		requestClient.persisted.SetUID("persisted-request")
		if requestClient.createErr == nil {
			object.SetUID(requestClient.persisted.GetUID())
		}
	}

	return requestClient.createErr
}

func (requestClient *bpnUncertainRequestClient) Get(_ context.Context, _ client.ObjectKey, object client.Object,
	_ ...client.GetOption,
) error {
	if requestClient.getErr != nil {
		return requestClient.getErr
	}
	if requestClient.persisted == nil || requestClient.deleted {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "storagebasedremediations"}, "worker-a")
	}
	request, ok := object.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("request must be unstructured")
	}
	request.Object = requestClient.persisted.DeepCopy().Object

	return nil
}

func (requestClient *bpnUncertainRequestClient) Delete(_ context.Context, object client.Object,
	_ ...client.DeleteOption,
) error {
	if requestClient.persisted == nil || object.GetUID() != requestClient.persisted.GetUID() {
		return fmt.Errorf("cleanup must delete the resolved request UID")
	}
	requestClient.deleted = true

	return apierrors.NewNotFound(schema.GroupResource{Resource: "storagebasedremediations"}, object.GetName())
}

func TestBPNJournalCleanupResolvesUnknownRequestUID(t *testing.T) {
	request := bpnCleanupRequest()
	request.SetUID("")
	request.SetAnnotations(map[string]string{bpnRequestIntentAnnotation: "creation-intent"})
	persisted := request.DeepCopy()
	persisted.SetUID("persisted-request")
	requestClient := &bpnUncertainRequestClient{persisted: persisted}
	originalClient := APIClient
	APIClient = &clients.Settings{Client: requestClient}
	t.Cleanup(func() { APIClient = originalClient })
	fixture, host := bpnPendingJournalCase(t, request)
	fixture.restoreCleanupFaults(func(_ string, operation func() error) {
		if err := operation(); err != nil {
			t.Error(err)
		}
	})
	if !requestClient.deleted || !host.restored {
		t.Fatal("cleanup must resolve and cancel the persisted request before restoring journal writes")
	}
}

func bpnPendingJournalCase(t *testing.T, requests ...*unstructured.Unstructured) (*bpnFixture, *bpnJournalCleanupHost) {
	t.Helper()
	host := &bpnJournalCleanupHost{}
	fault, err := sbrutils.NewBlockJournalFault(host, "uncertain-request-test")
	if err != nil {
		t.Fatal(err)
	}
	if err = fault.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = fault.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixture := &bpnFixture{
		ctx: t.Context(), requests: requests,
		journalFaults: []*bpnOwnedJournalFault{{BlockJournalFault: fault, releaseRequestsBeforeRestore: true}},
	}

	return fixture, host
}

func TestBPNRequestIntentPrecedesDispatchAndSurvivesLostResponse(t *testing.T) {
	fixture, host := bpnPendingJournalCase(t)
	request := bpnCleanupRequest()
	request.SetUID("")
	var createEnteredAt time.Time
	requestClient := &bpnUncertainRequestClient{
		persisted: &unstructured.Unstructured{}, createErr: context.DeadlineExceeded,
		beforeCreate: func(object client.Object) {
			createEnteredAt = time.Now()
			marker := object.GetAnnotations()[bpnRequestIntentAnnotation]
			if len(fixture.requests) != 1 || marker == "" {
				t.Fatal("cleanup must record ownership and request intent before Create dispatch")
			}
			intent := fixture.requests[0]
			if intent.GetName() != object.GetName() || intent.GetAnnotations()[bpnRequestIntentAnnotation] != marker {
				t.Fatal("the recorded intent must identify the dispatched request")
			}
		},
	}
	originalClient := APIClient
	APIClient = &clients.Settings{Client: requestClient}
	t.Cleanup(func() { APIClient = originalClient })
	dispatchedAt, err := fixture.createRequest(request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("creation must retain its lost response error: %v", err)
	}
	if dispatchedAt.IsZero() || dispatchedAt.After(createEnteredAt) {
		t.Fatal("a lost Create response must retain the dispatch time recorded before the API call")
	}
	if len(fixture.requests) != 1 || request.GetUID() != "" {
		t.Fatal("the lost response must leave a tracked intent without a confirmed UID")
	}
	if !fixture.restoreCleanupFaults(func(_ string, operation func() error) {
		if err := operation(); err != nil {
			t.Error(err)
		}
	}) || !requestClient.deleted || !host.restored {
		t.Fatal("cleanup must resolve ownership and cancel the request before restoring writes")
	}
}

func TestBPNUnresolvedRequestKeepsJournalWritesBlocked(t *testing.T) {
	for _, failure := range []string{"not-found", "foreign-owner", "api-error", "missing-marker", "missing-uid"} {
		t.Run(failure, func(t *testing.T) {
			request := bpnCleanupRequest()
			request.SetUID("")
			request.SetAnnotations(map[string]string{bpnRequestIntentAnnotation: "creation-intent"})
			persisted := request.DeepCopy()
			persisted.SetUID("persisted-request")
			requestClient := &bpnUncertainRequestClient{persisted: persisted}
			switch failure {
			case "not-found":
				requestClient.persisted = nil
			case "foreign-owner":
				persisted.SetAnnotations(map[string]string{bpnRequestIntentAnnotation: "foreign-intent"})
			case "api-error":
				requestClient.getErr = context.DeadlineExceeded
			case "missing-marker":
				request.SetAnnotations(nil)
			case "missing-uid":
				persisted.SetUID("")
			}
			originalClient := APIClient
			APIClient = &clients.Settings{Client: requestClient}
			t.Cleanup(func() { APIClient = originalClient })
			fixture, host := bpnPendingJournalCase(t, request)
			var failures []error
			if fixture.restoreCleanupFaults(func(_ string, operation func() error) {
				if err := operation(); err != nil {
					failures = append(failures, err)
				}
			}) || len(failures) == 0 || host.restored || requestClient.deleted {
				t.Fatal("uncertain ownership must stop cleanup without deleting CRs or restoring journal writes")
			}
			if failure == "not-found" {
				requestClient.persisted = persisted
				if err := fixture.removeCleanupRequest(request); err != nil || !requestClient.deleted {
					t.Fatalf("a later persisted request must remain tracked for normal cancellation: %v", err)
				}
			}
		})
	}
}

func TestBPNRejectedCreationDoesNotLeaveUncertainIntent(t *testing.T) {
	requestClient := &bpnUncertainRequestClient{createErr: apierrors.NewForbidden(
		schema.GroupResource{Resource: "storagebasedremediations"}, "worker-a", fmt.Errorf("creation forbidden"))}
	originalClient := APIClient
	APIClient = &clients.Settings{Client: requestClient}
	t.Cleanup(func() { APIClient = originalClient })
	fixture := &bpnFixture{ctx: t.Context()}
	if _, err := fixture.createRequest(bpnCleanupRequest()); !apierrors.IsForbidden(err) || len(fixture.requests) != 0 {
		t.Fatalf("a definitive API rejection must remove the unresolved intent: %v", err)
	}
}
