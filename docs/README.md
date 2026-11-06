# Documentation

This directory contains cross-cutting architecture, shared testing workflows, and release/pipeline guides for the [system-tests](https://github.com/medik8s/system-tests) repository.

## Scope

- **Cross-cutting guides (`docs/`)**: Multi-operator workflows, release contracts, cluster preparation, and shared end-to-end testing procedures live here.
- **Per-test documentation (`docs/TESTCASES/`)**: One document per Jira ticket describing scope, environment, steps, and pass criteria for tests added from now on.
- **Suite-specific documentation (`tests/<operator>/README.md`)**: Individual operator test catalogs, test case ID mappings, and operator-local prerequisites live directly within each test suite directory:
  - [FAR Operator Tests](../tests/far-operator/README.md)
  - [MDR Operator Tests](../tests/mdr-operator/README.md)
  - [NHC Operator Tests](../tests/nhc-operator/README.md)
  - [NMO Operator Tests](../tests/nmo-operator/README.md)
  - [SBR Operator Tests](../tests/sbr-operator/README.md)
  - [SNR Operator Tests](../tests/snr-operator/README.md)

## Guides

- [File-Based Catalog (FBC) Operator Upgrades](fbc-upgrades.md) (`tier:upgrade-operator`): Common release upgrade contract, environment inputs, and execution for standalone operator upgrade suites.
- [SBR Test Analysis](SBR/sbr_test_analysis.md): SBR coverage analysis, storage class requirements, and proposed tests.

## Test Case Documents

[`docs/TESTCASES/`](TESTCASES/README.md) holds per-ticket test case documents written
before the Go spec. Start one with `make new-testcase JIRA=RHWA-<id>`.

- [Test case requirements](TESTCASES/AGENTS.md): required sections, spec conventions, review checklist.
- [Directory guide](TESTCASES/README.md): naming, authoring workflow, status values.
- [Worked example](TESTCASES/EXAMPLE.md): a finished document, for calibration.

## Contributing

This repository follows the medik8s organization guidelines: the
[Contributing Guide](https://github.com/medik8s/.github/blob/main/CONTRIBUTING.md)
(DCO sign-off, fork workflow, `/lgtm` + `/approve` from two different OWNERS),
the [Code of Conduct](https://github.com/medik8s/.github/blob/main/CODE_OF_CONDUCT.md),
and the [Security Policy](https://github.com/medik8s/.github/blob/main/SECURITY.md).
Repository-specific conventions are in [AGENTS.md](../AGENTS.md).
