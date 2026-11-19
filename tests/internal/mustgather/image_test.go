package mustgather

import (
	"reflect"
	"strings"
	"testing"
)

func TestRegistryOf(t *testing.T) {
	for _, testCase := range []struct {
		reference string
		want      string
	}{
		{"quay.io/medik8s/must-gather:latest", "quay.io"},
		{"quay.io/medik8s/must-gather@sha256:abc", "quay.io"},
		{"docker://quay.io/medik8s/must-gather@sha256:abc", "quay.io"},
		{"mirror.example.com:5000/medik8s/must-gather@sha256:abc", "mirror.example.com:5000"},
		{"localhost/medik8s/must-gather:latest", "localhost"},
		// CRI-O emits a bare digest for an image resolved from the local store.
		{"sha256:abc", ""},
		// No host component: the first segment is a repository path.
		{"library/busybox", ""},
		{"", ""},
	} {
		t.Run(testCase.reference, func(t *testing.T) {
			if got := registryOf(testCase.reference); got != testCase.want {
				t.Errorf("registryOf(%q) = %q, want %q", testCase.reference, got, testCase.want)
			}
		})
	}
}

// TestContainerImageMirrored covers the case the report exists for: a pull
// redirected to a mirror by an ImageDigestMirrorSet.
func TestContainerImageMirrored(t *testing.T) {
	mirrored := ContainerImage{
		Image:   "quay.io/medik8s/must-gather:latest",
		ImageID: "mirror.example.com:5000/medik8s/must-gather@sha256:abc",
	}
	if !mirrored.Mirrored() {
		t.Error("Mirrored() = false for an image pulled from a different registry")
	}

	direct := ContainerImage{
		Image:   "quay.io/medik8s/must-gather:latest",
		ImageID: "quay.io/medik8s/must-gather@sha256:abc",
	}
	if direct.Mirrored() {
		t.Error("Mirrored() = true for an image pulled from the requested registry")
	}

	// A bare digest has no registry to compare, so it must not be reported as
	// mirrored -- that would be a false alarm on every such pod.
	bare := ContainerImage{
		Image:   "quay.io/medik8s/must-gather:latest",
		ImageID: "sha256:abc",
	}
	if bare.Mirrored() {
		t.Error("Mirrored() = true for a bare digest imageID")
	}
}

func TestGatherPodImagesString(t *testing.T) {
	pod := GatherPodImages{
		Namespace: "openshift-must-gather-abcde",
		Pod:       "must-gather-xyz",
		Node:      "worker-0",
		Containers: []ContainerImage{
			{
				Name:    "gather",
				Image:   "quay.io/medik8s/must-gather:latest",
				ImageID: "mirror.example.com:5000/medik8s/must-gather@sha256:abc",
			},
		},
	}
	rendered := pod.String()
	for _, want := range []string{
		"openshift-must-gather-abcde/must-gather-xyz",
		"worker-0",
		`container "gather"`,
		"requested: quay.io/medik8s/must-gather:latest",
		"pulled:    mirror.example.com:5000/medik8s/must-gather@sha256:abc",
		"an image mirror is in effect",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("String() does not contain %q:\n%s", want, rendered)
		}
	}
}

func TestFormatGatherPodImages(t *testing.T) {
	t.Run("nothing observed", func(t *testing.T) {
		rendered := FormatGatherPodImages("quay.io/medik8s/must-gather:latest", nil)
		if !strings.Contains(rendered, "requested image: quay.io/medik8s/must-gather:latest") {
			t.Errorf("report omits the requested image:\n%s", rendered)
		}

		if !strings.Contains(rendered, "no must-gather pod was observed") {
			t.Errorf("report does not explain the missing observation:\n%s", rendered)
		}
	})

	t.Run("pod observed", func(t *testing.T) {
		rendered := FormatGatherPodImages("quay.io/medik8s/must-gather:latest",
			[]GatherPodImages{{
				Namespace:  "openshift-must-gather-abcde",
				Pod:        "must-gather-xyz",
				Node:       "worker-0",
				Containers: []ContainerImage{{Name: "gather", Image: "img", ImageID: "img@sha256:abc"}},
			}})
		if !strings.Contains(rendered, "must-gather-xyz") {
			t.Errorf("report omits the pod:\n%s", rendered)
		}
	})
}

func TestPulledImages(t *testing.T) {
	// Both containers of a gather pod run the same image, so the report must
	// collapse them rather than listing the same digest twice.
	observed := []GatherPodImages{{
		Containers: []ContainerImage{
			{Name: "copy", ImageID: "quay.io/medik8s/must-gather@sha256:abc"},
			{Name: "gather", ImageID: "quay.io/medik8s/must-gather@sha256:abc"},
		},
	}}
	got := PulledImages(observed)
	if want := []string{"quay.io/medik8s/must-gather@sha256:abc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PulledImages = %v, want %v", got, want)
	}

	if got := PulledImages(nil); got != nil {
		t.Errorf("PulledImages(nil) = %v, want nil", got)
	}
}

func TestImageLinesFromOutput(t *testing.T) {
	output := strings.Join([]string{
		"[must-gather      ] OUT Using must-gather plug-in image: quay.io/medik8s/must-gather:latest",
		"ClusterVersion: Stable at \"5.0.0-rc.7\"",
		"[must-gather      ] OUT namespace/openshift-must-gather-gbksh created",
		"[must-gather      ] OUT pod for plug-in image quay.io/medik8s/must-gather:latest created",
		"[must-gather-42j7j] OUT namespaces/openshift-workload-availability/",
	}, "\n")
	got := ImageLinesFromOutput(output)
	want := []string{
		"[must-gather      ] OUT Using must-gather plug-in image: quay.io/medik8s/must-gather:latest",
		"[must-gather      ] OUT namespace/openshift-must-gather-gbksh created",
		"[must-gather      ] OUT pod for plug-in image quay.io/medik8s/must-gather:latest created",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ImageLinesFromOutput =\n%v\nwant\n%v", got, want)
	}

	if got := ImageLinesFromOutput(""); got != nil {
		t.Errorf("ImageLinesFromOutput(\"\") = %v, want nil", got)
	}
}
