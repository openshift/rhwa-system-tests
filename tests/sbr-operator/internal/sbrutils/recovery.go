package sbrutils

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SetRecoveryPause changes only this recovery's pause request on the original NHC.
func SetRecoveryPause(
	ctx context.Context, api client.Client, nhc *unstructured.Unstructured, token string, paused bool,
) error {
	if nhc.GetUID() == "" || token == "" {
		return fmt.Errorf("recovery requires an NHC UID and pause token")
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := nhc.DeepCopy()
		if err := api.Get(ctx, client.ObjectKeyFromObject(nhc), current); err != nil {
			return err
		}
		if current.GetUID() != nhc.GetUID() {
			return fmt.Errorf("NodeHealthCheck %s was replaced", nhc.GetName())
		}
		requests, _, err := unstructured.NestedStringSlice(current.Object, "spec", "pauseRequests")
		if err != nil {
			return err
		}
		requests = slices.DeleteFunc(requests, func(request string) bool { return request == token })
		if paused {
			requests = append(requests, token)
		}
		if err := unstructured.SetNestedStringSlice(current.Object, requests, "spec", "pauseRequests"); err != nil {
			return err
		}

		return api.Update(ctx, current)
	})
}

// RecoveryPaused requires the original NHC to have acknowledged this pause request.
func RecoveryPaused(nhc *unstructured.Unstructured, uid types.UID, token string) error {
	requests, _, err := unstructured.NestedStringSlice(nhc.Object, "spec", "pauseRequests")
	if err != nil {
		return err
	}
	phase, _, _ := unstructured.NestedString(nhc.Object, "status", "phase")
	if uid == "" || token == "" || nhc.GetUID() != uid || nhc.GetDeletionTimestamp() != nil ||
		!slices.Contains(requests, token) || phase != "Paused" {
		return fmt.Errorf("NodeHealthCheck %s has not acknowledged this recovery pause (phase=%s)", nhc.GetName(), phase)
	}

	return nil
}
