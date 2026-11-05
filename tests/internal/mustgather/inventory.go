package mustgather

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// CRDNameFilter is the substring the upstream collection script selects CRDs
	// with (`oc get crds ... | grep 'medik8s'`). The expected set is derived with
	// the same rule the script collects by, so a CRD added by a future operator
	// release is covered without editing this repository.
	CRDNameFilter = "medik8s"

	clusterScopedDir = "cluster-scoped-resources"
	namespacesDir    = "namespaces"

	// crdDefinitionDir is where `oc adm inspect crd/<name>` writes a definition.
	crdDefinitionDir = clusterScopedDir + "/apiextensions.k8s.io/customresourcedefinitions"

	// nodeDir is where `oc adm inspect nodes` writes node YAML.
	nodeDir = clusterScopedDir + "/core/nodes"

	// VersionFile is written by the collection script itself rather than by
	// `oc adm inspect`, so its presence proves the script ran to completion.
	VersionFile = "version"
)

// CRD is a custom resource definition discovered on the cluster, reduced to the
// fields the collected layout depends on.
type CRD struct {
	// Name is the full `plural.group` CRD name, which is also the base name
	// `oc adm inspect` writes the definition to.
	Name string
	// Group, Plural and Version address the resource through the dynamic client.
	Group   string
	Plural  string
	Version string
	// ClusterScoped selects which of the two output trees instances land in.
	ClusterScoped bool
}

// GroupVersionResource addresses the CRD's custom resources.
func (crd CRD) GroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    crd.Group,
		Version:  crd.Version,
		Resource: crd.Plural,
	}
}

// Instance identifies one live custom resource of a discovered CRD.
type Instance struct {
	Group         string
	Plural        string
	Namespace     string
	Name          string
	ClusterScoped bool
}

// String renders an instance the way `oc get` addresses it, for log output.
func (instance Instance) String() string {
	if instance.ClusterScoped {
		return fmt.Sprintf("%s.%s/%s", instance.Plural, instance.Group, instance.Name)
	}

	return fmt.Sprintf("%s/%s.%s/%s", instance.Namespace, instance.Plural, instance.Group, instance.Name)
}

// Inventory is the set of medik8s CRDs and their live custom resources at the
// moment the snapshot was taken. It is the expected result the collected tree
// is compared against.
type Inventory struct {
	CRDs      []CRD
	Instances []Instance
}

// SnapshotInventory discovers every CRD whose name contains nameFilter and
// lists all of their instances across all namespaces. Take the snapshot
// immediately before running the collection: anything created afterwards is
// legitimately absent from the output.
func SnapshotInventory(
	ctx context.Context, apiClient runtimeclient.Client, dynamicClient dynamic.Interface, nameFilter string,
) (Inventory, error) {
	crds, err := DiscoverCRDs(ctx, apiClient, nameFilter)
	if err != nil {
		return Inventory{}, err
	}

	inventory := Inventory{CRDs: crds}

	for _, crd := range crds {
		instances, listErr := ListInstances(ctx, dynamicClient, crd)
		if listErr != nil {
			return Inventory{}, listErr
		}

		inventory.Instances = append(inventory.Instances, instances...)
	}

	return inventory, nil
}

// DiscoverCRDs returns the CRDs whose name contains nameFilter, sorted by name.
func DiscoverCRDs(ctx context.Context, apiClient runtimeclient.Client, nameFilter string) ([]CRD, error) {
	crdList := &apiextensionsv1.CustomResourceDefinitionList{}
	if err := apiClient.List(ctx, crdList); err != nil {
		return nil, fmt.Errorf("listing CRDs: %w", err)
	}

	discovered := make([]CRD, 0, len(crdList.Items))

	for crdIdx := range crdList.Items {
		definition := &crdList.Items[crdIdx]
		if !strings.Contains(definition.Name, nameFilter) {
			continue
		}

		version, versionErr := servedVersion(definition)
		if versionErr != nil {
			return nil, versionErr
		}

		discovered = append(discovered, CRD{
			Name:          definition.Name,
			Group:         definition.Spec.Group,
			Plural:        definition.Spec.Names.Plural,
			Version:       version,
			ClusterScoped: definition.Spec.Scope == apiextensionsv1.ClusterScoped,
		})
	}

	sort.Slice(discovered, func(lhs, rhs int) bool { return discovered[lhs].Name < discovered[rhs].Name })

	return discovered, nil
}

// servedVersion picks the version to list instances through: the storage
// version when one is marked, otherwise the first served version.
func servedVersion(definition *apiextensionsv1.CustomResourceDefinition) (string, error) {
	fallback := ""

	for versionIdx := range definition.Spec.Versions {
		version := &definition.Spec.Versions[versionIdx]
		if !version.Served {
			continue
		}

		if version.Storage {
			return version.Name, nil
		}

		if fallback == "" {
			fallback = version.Name
		}
	}

	if fallback == "" {
		return "", fmt.Errorf("CRD %s has no served version", definition.Name)
	}

	return fallback, nil
}

// ListInstances returns every custom resource of crd across all namespaces.
func ListInstances(ctx context.Context, dynamicClient dynamic.Interface, crd CRD) ([]Instance, error) {
	list, err := dynamicClient.Resource(crd.GroupVersionResource()).
		Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing instances of %s: %w", crd.Name, err)
	}

	instances := make([]Instance, 0, len(list.Items))

	for itemIdx := range list.Items {
		item := &list.Items[itemIdx]
		instances = append(instances, Instance{
			Group:         crd.Group,
			Plural:        crd.Plural,
			Namespace:     item.GetNamespace(),
			Name:          item.GetName(),
			ClusterScoped: crd.ClusterScoped,
		})
	}

	return instances, nil
}

// ExpectedPaths returns every path, relative to the gather root, that the
// collection must produce for this inventory: one definition per CRD and one
// YAML per live instance.
func (inv Inventory) ExpectedPaths() []string {
	expected := make([]string, 0, len(inv.CRDs)+len(inv.Instances))
	for _, crd := range inv.CRDs {
		expected = append(expected, CRDDefinitionPath(crd.Name))
	}

	for _, instance := range inv.Instances {
		expected = append(expected, InstancePath(instance))
	}

	sort.Strings(expected)

	return expected
}

// operatorAcronyms maps each medik8s API group to the short name its operator
// is known by, so an archive can be named after what is actually in it.
var operatorAcronyms = map[string]string{
	"fence-agents-remediation.medik8s.io":     "far",
	"machine-deletion-remediation.medik8s.io": "mdr",
	"remediation.medik8s.io":                  "nhc",
	"nodemaintenance.medik8s.io":              "nmo",
	"self-node-remediation.medik8s.io":        "snr",
	"storage-based-remediation.medik8s.io":    "sbr",
}

// OperatorAcronyms returns the sorted, deduplicated short names of the
// operators that have at least one live custom resource in the inventory.
//
// Instances, not CRDs: every installed operator's CRD definition is collected
// on every run, so naming an archive after those would produce the same name
// every time and say nothing. The operators whose resources are actually in
// the output differ from spec to spec, which is what makes the name useful.
func (inv Inventory) OperatorAcronyms() []string {
	seen := make(map[string]bool, len(inv.Instances))

	var acronyms []string

	for _, instance := range inv.Instances {
		acronym := acronymForGroup(instance.Group)
		if acronym == "" || seen[acronym] {
			continue
		}

		seen[acronym] = true
		acronyms = append(acronyms, acronym)
	}

	sort.Strings(acronyms)

	return acronyms
}

// acronymForGroup returns the operator short name for an API group. A group
// added by a future operator is not in the map, so it falls back to the
// initials of its leading label ("fence-agents-remediation" -> "far"), or to
// that label whole when it is a single word.
func acronymForGroup(group string) string {
	if acronym, known := operatorAcronyms[group]; known {
		return acronym
	}

	label := group
	if dot := strings.Index(label, "."); dot > 0 {
		label = label[:dot]
	}

	words := strings.Split(label, "-")
	if len(words) < 2 {
		return label
	}

	initials := make([]byte, 0, len(words))

	for _, word := range words {
		if word != "" {
			initials = append(initials, word[0])
		}
	}

	return string(initials)
}

// CRDNames returns the discovered CRD names, sorted.
func (inv Inventory) CRDNames() []string {
	names := make([]string, 0, len(inv.CRDs))
	for _, crd := range inv.CRDs {
		names = append(names, crd.Name)
	}

	sort.Strings(names)

	return names
}

// InstancesOf returns the live instances belonging to the named CRD
// (`plural.group`), so a spec can assert on one operator's resources.
func (inv Inventory) InstancesOf(crdName string) []Instance {
	var matched []Instance

	for _, instance := range inv.Instances {
		if instance.Plural+"."+instance.Group == crdName {
			matched = append(matched, instance)
		}
	}

	return matched
}

// Summary renders one line per CRD with its instance count, for the run log.
func (inv Inventory) Summary() string {
	counts := make(map[string]int, len(inv.CRDs))
	for _, instance := range inv.Instances {
		counts[instance.Plural+"."+instance.Group]++
	}

	lines := make([]string, 0, len(inv.CRDs))
	for _, crd := range inv.CRDs {
		lines = append(lines, fmt.Sprintf("  %-70s %d instance(s)", crd.Name, counts[crd.Name]))
	}

	return strings.Join(lines, "\n")
}

// CRDDefinitionPath returns the path, relative to the gather root, that
// `oc adm inspect crd/<name>` writes a CRD definition to.
func CRDDefinitionPath(crdName string) string {
	return path.Join(crdDefinitionDir, crdName+".yaml")
}

// NodePath returns the path `oc adm inspect nodes` writes a node's YAML to.
func NodePath(nodeName string) string {
	return path.Join(nodeDir, nodeName+".yaml")
}

// InstancePath returns the path a custom resource is written to. Cluster-scoped
// resources land under cluster-scoped-resources/<group>/<plural>/ and namespaced
// ones under namespaces/<namespace>/<group>/<plural>/ -- the two layouts
// `oc adm inspect` uses, confirmed against its output on a live cluster.
func InstancePath(instance Instance) string {
	if instance.ClusterScoped {
		return path.Join(clusterScopedDir, instance.Group, instance.Plural, instance.Name+".yaml")
	}

	return path.Join(namespacesDir, instance.Namespace, instance.Group, instance.Plural, instance.Name+".yaml")
}

// GatherRoot returns the directory under destDir holding the collected tree.
// `oc adm must-gather` nests its output one level down, in a directory named
// after the image it ran (quay-io-medik8s-must-gather-sha256-<digest>), so
// every relative path resolves against that rather than against destDir.
func GatherRoot(destDir string) (string, error) {
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return "", fmt.Errorf("reading must-gather destination %s: %w", destDir, err)
	}

	var roots []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		candidate := filepath.Join(destDir, entry.Name())
		if isGatherRoot(candidate) {
			roots = append(roots, candidate)
		}
	}

	switch len(roots) {
	case 1:
		return roots[0], nil
	case 0:
		// A gather that wrote straight into --dest-dir is still usable; only an
		// empty destination is a failure.
		if isGatherRoot(destDir) {
			return destDir, nil
		}

		return "", fmt.Errorf("no must-gather output found under %s", destDir)
	default:
		return "", fmt.Errorf("found %d must-gather output directories under %s: %v",
			len(roots), destDir, roots)
	}
}

// isGatherRoot reports whether dir holds either of the two trees
// `oc adm inspect` creates.
func isGatherRoot(dir string) bool {
	for _, marker := range []string{clusterScopedDir, namespacesDir} {
		if info, err := os.Stat(filepath.Join(dir, marker)); err == nil && info.IsDir() {
			return true
		}
	}

	return false
}

// MissingPaths returns the expected paths that are absent under root, preserving
// the given order. An empty result means the collection covered the whole
// inventory.
func MissingPaths(root string, expected []string) []string {
	var missing []string

	for _, relPath := range expected {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relPath))); err != nil {
			missing = append(missing, relPath)
		}
	}

	return missing
}

// MissingNames returns the entries of required that are absent from actual.
// The required list is the checked-in floor: without it a cluster missing an
// operator would shrink the cluster-derived expectation and pass vacuously.
func MissingNames(actual, required []string) []string {
	present := make(map[string]bool, len(actual))
	for _, name := range actual {
		present[name] = true
	}

	var missing []string

	for _, name := range required {
		if !present[name] {
			missing = append(missing, name)
		}
	}

	return missing
}
