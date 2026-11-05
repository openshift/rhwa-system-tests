# Medik8s Collection Tests

Cross-operator tests for the RHWA diagnostic collection image
([medik8s/must-gather](https://github.com/medik8s/must-gather)). They configure
every RHWA operator, run `oc adm must-gather`, and compare the collected tree
against an inventory read from the cluster.

The per-operator suites each assert on their own slice of a must-gather. This
suite asks the question none of them can: *did the collection miss anything?*

## How the expected result is built

The collection script does not hardcode a resource list. It selects by name:

```bash
MEDIK8S_CRDS=$(oc get crds ... | grep 'medik8s')   # crd/<name>, the definitions
MEDIK8S_CRS=$(oc get crds  ... | grep 'medik8s')   # the resources, every instance
oc adm inspect --all-namespaces ...                # + nodes, machinehealthchecks, operator namespaces
```

The suite derives its expectation with the same rule
(`mustgather.CRDNameFilter`), so a CRD added by a future operator release is
covered without editing this repository. Two things keep that from passing
vacuously:

- `collectionparams.RequiredCRDNames` is a checked-in floor. A cluster missing
  an operator fails rather than expecting less.
- The suite creates a custom resource for every CRD it safely can, so the
  comparison runs against a populated cluster rather than an empty one.

Each discovered CRD must have a definition file, and each live custom resource
must have its own YAML, at the path `oc adm inspect` writes it to:

| Resource | Path, relative to the gather root |
| -------- | --------------------------------- |
| CRD definition | `cluster-scoped-resources/apiextensions.k8s.io/customresourcedefinitions/<plural.group>.yaml` |
| Cluster-scoped CR | `cluster-scoped-resources/<group>/<plural>/<name>.yaml` |
| Namespaced CR | `namespaces/<namespace>/<group>/<plural>/<name>.yaml` |
| Node | `cluster-scoped-resources/core/nodes/<name>.yaml` |

## Tests

| Test | Label | What it covers |
| ---- | ----- | -------------- |
| collects every medik8s CRD definition and custom resource | `disruption:nondestructive`, `tier:acceptance` | Creates a template for SNR, FAR, MDR and SBR plus a `NodeHealthCheck`, then requires every discovered CRD and CR, every node, and the operator namespace's workloads and pod logs to be collected |
| collects every StorageBasedRemediation resource | `operator:sbr`, `disruption:nondestructive`, `tier:acceptance` | Creates an SBR config, template and remediation together, so all three SBR CRDs are populated in one collection. The config omits `sharedStorageClass`, which is what keeps it inert — the controller owns it but creates no agent DaemonSet and no PVC, both asserted |
| collects a live NodeMaintenance custom resource | `disruption:destructive`, `tier:resiliency` | Cordons and drains a worker through a `NodeMaintenance` CR and requires it in the collection |
| collects a live SelfNodeRemediation custom resource | `disruption:destructive`, `tier:resiliency` | Reboots a worker through a `SelfNodeRemediation` CR and gathers while the remediation is live |

### Coverage per operator

All six operators are exercised. Every CRD's **definition** is always required
to be collected; the column below is about whether a live **instance** of it
exists when the collection runs, which is the stronger check.

| Operator | CRD | Live instance | Created by |
| -------- | --- | ------------- | ---------- |
| FAR | `fenceagentsremediationtemplates` | yes | spec 1 fixture |
| FAR | `fenceagentsremediations` | no | — fences a node |
| MDR | `machinedeletionremediationtemplates` | yes | spec 1 fixture |
| MDR | `machinedeletionremediations` | yes | spec 1 fixture, named after an absent node |
| NHC | `nodehealthchecks` | yes | spec 1 fixture |
| NMO | `nodemaintenances` | yes | spec 3 (destructive) |
| SNR | `selfnoderemediationtemplates` | yes | spec 1 fixture |
| SNR | `selfnoderemediationconfigs` | yes | the operator's own default config |
| SNR | `selfnoderemediations` | yes | spec 4 (destructive) |
| SBR | `storagebasedremediationtemplates` | yes | spec 1 fixture |
| SBR | `storagebasedremediationconfigs` | yes | spec 2 fixture, no shared storage so it stays inert |
| SBR | `storagebasedremediations` | yes | spec 1 and spec 2 fixtures, absent node |

Eleven of the twelve CRDs have a live instance — every one except
`fenceagentsremediations`. Spec 1 covers five operators, spec 2 completes SBR
by adding its config, and specs 3 and 4 add the two CRDs that cannot exist
without disrupting a node.

### What is not covered

### The absent-node fixtures

`machinedeletionremediations` and `storagebasedremediations` are covered by
the two fixtures whose *name* is load-bearing. Both operators identify the
node to remediate by the CR's own name, so both fixtures are named after
`rhwa-collection-absent-node`, a node that does not exist. Pointed at a real
node, MDR would delete its `Machine` and SBR would storage-fence it.

One assertion guards both: `BeforeAll` proves no node carries that name before
either is created, and any read error other than `NotFound` fails too —
absence that cannot be read is not absence.

The two behave differently once created, and the difference matters:

- **MDR is reconciled.** The controller picks the CR up, finds no such node,
  and records `RemediationCannotStartNodeNotFound` on its `Processing`
  condition without adding a finalizer. The suite asserts that reason, so the
  collected resource is a genuinely reconciled one and nothing was deleted.
- **SBR is not reconciled.** Its remediation controller only runs while a
  `StorageBasedRemediationConfig` exists, and the suite only creates this
  fixture when none does — so the CR is stored and never acted on: no
  finalizer, no taint, deletes in milliseconds. It is a real instance of its
  CRD, which is all the collection has to find, but it proves nothing about
  SBR's behaviour. Where a cluster *does* have a config, the fixture is
  skipped rather than risk fencing a node to prove a file path exists, and
  the run records a `skipped-fixture` report entry saying so.

### What is not covered

`fenceagentsremediations` has no instance in any spec: FAR needs working BMC
credentials, and the controller taints the node before it attempts to fence,
so even a CR that can never succeed leaves state behind to clean up.

Its CRD *definition* is still required to be collected.

`storagebasedremediationconfigs` **is** covered, by spec 2. The trick is that
its config omits `sharedStorageClass`: the controller reconciles the object
and takes ownership of it, but creates neither the agent DaemonSet nor a PVC,
so nothing lands on a real node. The spec asserts both — the finalizer, to
show the controller really took it up, and the unchanged DaemonSet and PVC
lists, to show it stayed inert. On a cluster that already has a config of its
own, spec 2 skips rather than add a second one.

## Running it

```bash
export KUBECONFIG=/absolute/path/to/kubeconfig
export ECO_TEST_FEATURES=medik8s-collection
export WORKLOAD_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest
export ARTIFACT_DIR=/tmp/rhwa-collection-artifacts
make run-tests
```

Non-destructive only:

```bash
export KUBECONFIG=/absolute/path/to/kubeconfig
export WORKLOAD_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest
ginkgo -v --label-filter='disruption:nondestructive' ./tests/medik8s-collection/...
```

Archives land in `/tmp/rhwa-collection-artifacts` with no further setup, and
each run prints the full path. `ARTIFACT_DIR` overrides that; `make run-tests`
sets it for you via `ECO_REPORTS_DUMP_DIR`. The directory is created if it
does not exist.

`MUST_GATHER_IMAGE` overrides the collection image, for a mirrored reference on
a disconnected cluster. It defaults to `quay.io/medik8s/must-gather:latest`.

## Artifacts

Every collection is published as one gzipped tar, so a job leaves behind the
diagnostics it gathered and not just a verdict:

```text
rhwa-<operator acronyms>-must-gather-<UTC timestamp>.tar.gz
```

The acronyms name the operators whose custom resources are actually in that
collection, sorted, so the file says what it holds without being opened. They
differ per spec, which is also what tells the three archives apart:

```text
rhwa-far-mdr-nhc-sbr-snr-must-gather-20261029-222900.tar.gz   spec 1
rhwa-sbr-snr-must-gather-20261029-225626.tar.gz               spec 2
rhwa-nmo-snr-must-gather-20261029-223145.tar.gz               spec 3
rhwa-snr-must-gather-20261029-224012.tar.gz                   spec 4
```

They follow live custom resources, not installed CRDs: every operator's CRD
definition is collected on every run, so naming by those would give the same
name every time. A CRD added by a future operator falls back to the initials
of its API group.

They go to `ARTIFACT_DIR`, falling back to `ECO_REPORTS_DUMP_DIR`, falling
back to `/tmp/rhwa-collection-artifacts`. A collection is always kept — there
is no case where it is silently dropped.

Archives accumulate. Nothing in the suite removes the artifact directory or
prunes what earlier runs put there — each collection only adds a file, and the
UTC timestamp in the name keeps runs from colliding. That is deliberate: it
lets a failing collection be compared against the last good one, and it means
a later suite never destroys an earlier suite's evidence. Pruning is a manual
decision.


Each archive holds the collected tree and the `oc-adm-must-gather.log` under a
single top-level directory named after the archive, so extracting one never
scatters files. A collection is roughly 5.5 MB raw and 0.5 MB compressed —
hence publishing the archive rather than the raw tree.

Archiving runs before the gather's own error is asserted, so a failed or
partial collection is still published. An archiving failure is reported as a
`must-gather-archive-failed` report entry and never fails the spec.

## Which image actually ran

Every collection logs its image provenance, since "the test passed" means
little without knowing which build produced the output:

- the requested reference and whether it came from `MUST_GATHER_IMAGE` or the
  default;
- the digest that reference resolves to, from `oc image info` — the default is
  a floating `:latest`, so the digest is what makes a run reproducible;
- the `plug-in image`, `namespace/… created` and `pod … created` lines from
  `oc adm must-gather`'s own output;
- the image each container of the gather pod **actually pulled**, from
  `status.containerStatuses[].imageID`, with the pod and node names.

The last one is the only place an `ImageDigestMirrorSet` redirect shows up, and
the report calls it out when the pulled registry differs from the requested
one. It is captured by a watcher running alongside the collection:
`oc adm must-gather` deletes its namespace the moment it finishes, so there is
nothing left to query afterwards. A missed observation is reported, not fatal.

```text
must-gather image provenance:
  requested image: quay.io/medik8s/must-gather:latest
  pod openshift-must-gather-knmvf/must-gather-7z7rn on node master-2
    container "gather"
      requested: quay.io/medik8s/must-gather:latest
      pulled:    quay.io/medik8s/must-gather@sha256:9d3dcb2e…
```

Watch a run with [`tools/watch/collection-watch.sh`](../../tools/watch/collection-watch.sh).
