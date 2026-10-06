package medik8sparams

import (
	"os"
	"time"
)

const (
	// Label represents medik8s label that can be used for test cases selection.
	Label = "medik8s"
	// OperatorNs custom namespace of medik8s operators.
	OperatorNs = "openshift-workload-availability"
	// DefaultTimeout represents the default timeout.
	DefaultTimeout = 300 * time.Second
)

// WorkloadImage is the container image used for test workload pods.
// Set via WORKLOAD_IMAGE env var. In Prow CI this is written to
// SHARED_DIR/workload_image by the medik8s-lib step and exported by the
// e2e-test commands block. For local runs you can either:
//
//	export WORKLOAD_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest
//	export WORKLOAD_IMAGE=local  # Uses default registry.access.redhat.com/ubi9/ubi-minimal:latest
var WorkloadImage = func() string {
	img := os.Getenv("WORKLOAD_IMAGE")
	if img == "" {
		panic("WORKLOAD_IMAGE env var is required but not set. " +
			"In Prow CI this is exported from SHARED_DIR/workload_image by the e2e-test commands block. " +
			"For local runs: export WORKLOAD_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest or export WORKLOAD_IMAGE=local")
	}

	// Allow "local" as a shorthand for the default image
	if img == "local" {
		return "registry.access.redhat.com/ubi9/ubi-minimal:latest"
	}

	return img
}()
