package mustgather

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const inventoryDirPermissions = 0o755

// writeTree materializes relPaths as empty files under root, creating parents.
func writeTree(t *testing.T, root string, relPaths ...string) {
	t.Helper()

	for _, relPath := range relPaths {
		full := filepath.Join(root, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(full), inventoryDirPermissions); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(full, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCRDDefinitionPath(t *testing.T) {
	got := CRDDefinitionPath("nodehealthchecks.remediation.medik8s.io")
	want := "cluster-scoped-resources/apiextensions.k8s.io/customresourcedefinitions/" +
		"nodehealthchecks.remediation.medik8s.io.yaml"

	if got != want {
		t.Errorf("CRDDefinitionPath = %q, want %q", got, want)
	}
}

func TestNodePath(t *testing.T) {
	if got, want := NodePath("worker-0"), "cluster-scoped-resources/core/nodes/worker-0.yaml"; got != want {
		t.Errorf("NodePath = %q, want %q", got, want)
	}
}

// TestInstancePath pins the two layouts `oc adm inspect` writes custom
// resources to. Both were verified against its output on a live cluster.
func TestInstancePath(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		instance Instance
		want     string
	}{
		{
			name: "cluster scoped",
			instance: Instance{
				Group:         "remediation.medik8s.io",
				Plural:        "nodehealthchecks",
				Name:          "collection-test-nhc",
				ClusterScoped: true,
			},
			want: "cluster-scoped-resources/remediation.medik8s.io/nodehealthchecks/collection-test-nhc.yaml",
		},
		{
			name: "namespaced",
			instance: Instance{
				Group:     "self-node-remediation.medik8s.io",
				Plural:    "selfnoderemediationconfigs",
				Namespace: "openshift-workload-availability",
				Name:      "self-node-remediation-config",
			},
			want: "namespaces/openshift-workload-availability/self-node-remediation.medik8s.io/" +
				"selfnoderemediationconfigs/self-node-remediation-config.yaml",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := InstancePath(testCase.instance); got != testCase.want {
				t.Errorf("InstancePath = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestInventoryExpectedPaths(t *testing.T) {
	inventory := Inventory{
		CRDs: []CRD{
			{
				Name: "nodehealthchecks.remediation.medik8s.io", Group: "remediation.medik8s.io",
				Plural: "nodehealthchecks", Version: "v1alpha1", ClusterScoped: true,
			},
		},
		Instances: []Instance{
			{Group: "remediation.medik8s.io", Plural: "nodehealthchecks", Name: "nhc", ClusterScoped: true},
		},
	}
	got := inventory.ExpectedPaths()
	want := []string{
		"cluster-scoped-resources/apiextensions.k8s.io/customresourcedefinitions/" +
			"nodehealthchecks.remediation.medik8s.io.yaml",
		"cluster-scoped-resources/remediation.medik8s.io/nodehealthchecks/nhc.yaml",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpectedPaths = %v, want %v", got, want)
	}
}

func TestInventoryInstancesOfAndSummary(t *testing.T) {
	inventory := Inventory{
		CRDs: []CRD{
			{
				Name:  "selfnoderemediations.self-node-remediation.medik8s.io",
				Group: "self-node-remediation.medik8s.io", Plural: "selfnoderemediations",
			},
			{
				Name:  "nodehealthchecks.remediation.medik8s.io",
				Group: "remediation.medik8s.io", Plural: "nodehealthchecks", ClusterScoped: true,
			},
		},
		Instances: []Instance{
			{
				Group: "self-node-remediation.medik8s.io", Plural: "selfnoderemediations",
				Namespace: "openshift-workload-availability", Name: "worker-0",
			},
		},
	}
	matched := inventory.InstancesOf("selfnoderemediations.self-node-remediation.medik8s.io")
	if len(matched) != 1 || matched[0].Name != "worker-0" {
		t.Errorf("InstancesOf returned %v, want one instance named worker-0", matched)
	}

	if got := inventory.InstancesOf("nodehealthchecks.remediation.medik8s.io"); got != nil {
		t.Errorf("InstancesOf for a CRD with no instances = %v, want nil", got)
	}

	summary := inventory.Summary()
	if !strings.Contains(summary, "1 instance(s)") || !strings.Contains(summary, "0 instance(s)") {
		t.Errorf("Summary did not report both counts:\n%s", summary)
	}
}

// TestInventoryInstanceString covers the log rendering of both scopes.
func TestInventoryInstanceString(t *testing.T) {
	clusterScoped := Instance{
		Group: "remediation.medik8s.io", Plural: "nodehealthchecks",
		Name: "nhc", ClusterScoped: true,
	}
	if got, want := clusterScoped.String(), "nodehealthchecks.remediation.medik8s.io/nhc"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}

	namespaced := Instance{
		Group: "self-node-remediation.medik8s.io", Plural: "selfnoderemediations",
		Namespace: "openshift-workload-availability", Name: "worker-0",
	}
	want := "openshift-workload-availability/selfnoderemediations.self-node-remediation.medik8s.io/worker-0"

	if got := namespaced.String(); got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

// TestAcronymForGroup pins the short name of every medik8s operator, and the
// fallback a group from a future operator would get.
func TestAcronymForGroup(t *testing.T) {
	for _, testCase := range []struct{ group, want string }{
		{"fence-agents-remediation.medik8s.io", "far"},
		{"machine-deletion-remediation.medik8s.io", "mdr"},
		{"remediation.medik8s.io", "nhc"},
		{"nodemaintenance.medik8s.io", "nmo"},
		{"self-node-remediation.medik8s.io", "snr"},
		{"storage-based-remediation.medik8s.io", "sbr"},
		// Unmapped groups fall back to initials, or to the whole leading
		// label when it is a single word.
		{"some-new-operator.medik8s.io", "sno"},
		{"newthing.medik8s.io", "newthing"},
	} {
		t.Run(testCase.group, func(t *testing.T) {
			if got := acronymForGroup(testCase.group); got != testCase.want {
				t.Errorf("acronymForGroup(%q) = %q, want %q", testCase.group, got, testCase.want)
			}
		})
	}
}

func TestInventoryOperatorAcronyms(t *testing.T) {
	inventory := Inventory{
		// Every CRD is discovered, but only some have instances. The acronyms
		// must follow the instances: naming an archive after installed CRDs
		// would give the same name on every run.
		CRDs: []CRD{
			{Name: "fenceagentsremediations.fence-agents-remediation.medik8s.io",
				Group: "fence-agents-remediation.medik8s.io", Plural: "fenceagentsremediations"},
			{Name: "selfnoderemediations.self-node-remediation.medik8s.io",
				Group: "self-node-remediation.medik8s.io", Plural: "selfnoderemediations"},
		},
		Instances: []Instance{
			// Two instances of one operator must collapse to one acronym...
			{Group: "self-node-remediation.medik8s.io", Plural: "selfnoderemediations", Name: "worker-0"},
			{Group: "self-node-remediation.medik8s.io", Plural: "selfnoderemediationconfigs", Name: "config"},
			// ...and the result must be sorted, not insertion-ordered.
			{Group: "nodemaintenance.medik8s.io", Plural: "nodemaintenances", Name: "nm", ClusterScoped: true},
		},
	}
	got := inventory.OperatorAcronyms()
	if want := []string{"nmo", "snr"}; !reflect.DeepEqual(got, want) {
		t.Errorf("OperatorAcronyms = %v, want %v", got, want)
	}

	if got := (Inventory{}).OperatorAcronyms(); got != nil {
		t.Errorf("OperatorAcronyms with no instances = %v, want nil", got)
	}
}

func TestGatherRoot(t *testing.T) {
	t.Run("nested image directory", func(t *testing.T) {
		destDir := t.TempDir()
		writeTree(t, destDir, "quay-io-medik8s-must-gather-sha256-abc/cluster-scoped-resources/core/nodes/w0.yaml")
		root, err := GatherRoot(destDir)
		if err != nil {
			t.Fatalf("GatherRoot returned %v", err)
		}

		if want := filepath.Join(destDir, "quay-io-medik8s-must-gather-sha256-abc"); root != want {
			t.Errorf("GatherRoot = %q, want %q", root, want)
		}
	})

	t.Run("tree written directly into dest dir", func(t *testing.T) {
		destDir := t.TempDir()
		writeTree(t, destDir, "namespaces/openshift-workload-availability/x.yaml")
		root, err := GatherRoot(destDir)
		if err != nil {
			t.Fatalf("GatherRoot returned %v", err)
		}

		if root != destDir {
			t.Errorf("GatherRoot = %q, want %q", root, destDir)
		}
	})

	t.Run("empty destination", func(t *testing.T) {
		if _, err := GatherRoot(t.TempDir()); err == nil {
			t.Error("GatherRoot on an empty destination returned no error")
		}
	})

	// A second image directory means two gathers wrote into one destination;
	// picking either would validate an arbitrary one of them.
	t.Run("two output directories", func(t *testing.T) {
		destDir := t.TempDir()
		writeTree(t, destDir,
			"image-a/cluster-scoped-resources/core/nodes/w0.yaml",
			"image-b/cluster-scoped-resources/core/nodes/w0.yaml")

		if _, err := GatherRoot(destDir); err == nil {
			t.Error("GatherRoot with two output directories returned no error")
		}
	})

	t.Run("unreadable destination", func(t *testing.T) {
		if _, err := GatherRoot(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
			t.Error("GatherRoot on a missing destination returned no error")
		}
	})
}

func TestMissingPaths(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "cluster-scoped-resources/core/nodes/worker-0.yaml")
	got := MissingPaths(root, []string{
		"cluster-scoped-resources/core/nodes/worker-0.yaml",
		"cluster-scoped-resources/core/nodes/worker-1.yaml",
	})
	want := []string{"cluster-scoped-resources/core/nodes/worker-1.yaml"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("MissingPaths = %v, want %v", got, want)
	}

	if got := MissingPaths(root, nil); got != nil {
		t.Errorf("MissingPaths with no expectations = %v, want nil", got)
	}
}

func TestMissingNames(t *testing.T) {
	actual := []string{"a", "b"}

	if got := MissingNames(actual, []string{"a", "b"}); got != nil {
		t.Errorf("MissingNames with everything present = %v, want nil", got)
	}

	got := MissingNames(actual, []string{"a", "c", "d"})
	if want := []string{"c", "d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("MissingNames = %v, want %v", got, want)
	}
}
