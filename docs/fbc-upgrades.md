# File-Based Catalog Operator Upgrades

The shared `tier:upgrade-operator` runner handles the catalog and OLM transition
for NHC, SNR, FAR, MDR, and NMO. Operator-specific lifecycle hooks supply
the configuration and behavior checks:

1. Apply the release-provided ImageDigestMirrorSet and wait for any
   MachineConfigPool rollout.
2. Create a test-owned CatalogSource from a digest-pinned FBC image.
3. Install the released operator from `redhat-operators`.
4. Exercise operator-specific behavior and capture persistent state.
5. Switch the existing Subscription to the candidate FBC.
6. Require a new Succeeded CSV with the exact candidate version.
7. Require the live controller to run the expected digest-pinned image.
8. Verify preserved state, fresh reconciliation, and operator-specific behavior.

## Validation coverage

NHC's validations moved from `upgrade_operator.go` into
`tests/nhc-operator/tests/upgrade_lifecycle.go`; they are not deferred to a stub.
`BeforeUpgrade` creates the configuration, captures its UID and complete spec,
and exercises baseline reconciliation and remediation. `AfterUpgrade` compares
the UID and complete spec, requires a fresh pause response, restores the original
configuration, and exercises candidate remediation. The shared runner calls both
hooks around the Subscription upgrade and requires them to succeed.

SNR has concrete hooks in `tests/snr-operator/tests/upgrade_lifecycle.go`.
They customize the GA-created default config, compare its UID and complete spec
after upgrading, and require a unique config probe to reach newly rolled agent
pods. Both the controller and all agents must run the expected candidate image.
Before upgrading, all GA agents must be Ready and reflect the customized config.
After upgrading, a direct SNR remediation must reboot and recover a worker. Its
boot ID must change while its node UID remains the same. This scenario is
destructive and requires at least two Ready workers on a disposable cluster.
It does not install NHC or stop kubelet: a test-owned SNR CR triggers remediation.

FAR's default is a focused, nondestructive upgrade check on OpenShift 5.0.
It requires the new Succeeded CSV, exact candidate version/image, and the same
customized template UID and complete spec. A temporary, read-only status probe
against a documentation-only dummy IP address must produce `ValidationFailed`
at the current template generation, proving fresh candidate reconciliation.
That failure is expected, not a claim that real remediation works. The original
spec is restored and the probe result must clear. The default creates no
remediation CRs, fencing credentials, workload pods, NHC, or storage.

Real fencing is optional: set `FAR_FBC_REMEDIATION=true` on a disposable AWS
cluster with three Ready workers, a runnable `WORKLOAD_IMAGE` with `sleep`,
and CCO capable of minting fencing credentials. This mode requires a successful
read-only status probe and uses the persisted template to reboot a worker and
evict a standalone workload before and after upgrading. It creates an owned
least-privilege CredentialsRequest, keeps credentials only in Secrets, and
deletes its request during cleanup. The full FAR acceptance suite is not run.

MDR has concrete, nondestructive hooks in `tests/mdr-operator/tests/upgrade_lifecycle.go`.
They require the exact upgrade and preserve a GA-created template's UID and full
spec. MDR currently has no configurable spec fields: its `template.spec` is an
empty map, which is still compared in full rather than replaced with invented settings.
Before and after the upgrade, a fresh request copies the persisted template and
must report both `Processing=False` and `Succeeded=False` with reason
`RemediationStoppedByNHC`. Each request targets a unique, verified nonexistent node
and carries the mandatory NHC timeout safety annotation, blocking Machine deletion
while the request is active. No NHC installation, worker replacement, or storage is
needed. A new post-upgrade request UID prevents stale GA status satisfying this check;
MDR does not currently populate `observedGeneration` in these conditions.
This proves safe controller reconciliation, not functional Machine replacement.

NMO has concrete hooks in `tests/nmo-operator/tests/upgrade_lifecycle.go`.
On a disposable OpenShift 5.0 cluster with at least two Ready, schedulable workers,
it drains one non-control-plane worker using an owned `NodeMaintenance` CR.
The same CR UID and complete spec must survive the exact version/image upgrade.
A unique reason change and restoration must each produce a newer controller
`status.lastUpdate` than the API patch response: NMO has no `observedGeneration`.
The worker must remain Ready, cordoned, drain-tainted, and excluded from remediation.
Cleanup deletes only the owned CR, requires the original worker to recover and its
lease to disappear, and only then removes the operator. This is destructive
maintenance, not a reboot or a full acceptance suite. All five suites have real hooks.

## Inputs

Each suite uses variables prefixed by its operator name. For NHC:

```bash
export NHC_FBC_CATALOG_IMAGE='quay.io/example/rhwa-fbc@sha256:...'
# Required only when the catalog relies on registry mirrors:
export NHC_FBC_IDMS_PATH=/absolute/path/to/idms.yaml
export NHC_FBC_CANDIDATE_VERSION=5.8.0
export NHC_FBC_CANDIDATE_IMAGE='registry.example/operator@sha256:...'

# Optional defaults:
export NHC_FBC_CATALOG_NAME=nhc-upgrade-candidate
export NHC_FBC_CHANNEL=stable
export NHC_FBC_SKIP_CLEANUP=false
```

Use the corresponding `SNR_FBC_*`, `FAR_FBC_*`, `MDR_FBC_*`, or
`NMO_FBC_*` variables for another operator. This lets multiple suites use
different catalogs, versions, and images in the same test process.

When `<OPERATOR>_FBC_IDMS_PATH` is unset, the test uses `$SHARED_DIR/idms.yaml` if
that file exists. Otherwise it skips IDMS application, which is appropriate for
directly pullable source-built catalogs. The catalog and candidate images must
use immutable `@sha256:` pullspecs. Registry credentials must already be
present in the cluster pull secret; tests never acquire or print registry credentials.

## Run one operator

```bash
export KUBECONFIG=/absolute/path/to/kubeconfig
export ECO_TEST_FEATURES=nhc-operator
export ECO_TEST_LABELS='tier:upgrade-operator'
export WORKLOAD_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest
export ECO_REPORTS_DUMP_DIR="$(mktemp -d)"
make run-tests
```

Replace `nhc-operator` with `snr-operator`, `far-operator`, `mdr-operator`, or
`nmo-operator`. All five have operator-specific lifecycle checks.

The tests are designed for disposable clusters. Candidate CatalogSource and
OLM cleanup failures are logged as warnings. The applied IDMS remains in place
to avoid triggering a second MachineConfigPool rollout during teardown.
