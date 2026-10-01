# Documentation

This directory contains cross-cutting architecture, shared testing workflows, and release/pipeline guides for the [system-tests](https://github.com/medik8s/system-tests) repository.

## Scope

- **Cross-cutting guides (`docs/`)**: Multi-operator workflows, release contracts, cluster preparation, and shared end-to-end testing procedures live here.
- **Suite-specific documentation (`tests/<operator>/README.md`)**: Individual operator test catalogs, Polarion test case mappings, and operator-local prerequisites live directly within each test suite directory:
  - [FAR Operator Tests](../tests/far-operator/README.md)
  - [MDR Operator Tests](../tests/mdr-operator/README.md)
  - [NHC Operator Tests](../tests/nhc-operator/README.md)
  - [NMO Operator Tests](../tests/nmo-operator/README.md)
  - [SBR Operator Tests](../tests/sbr-operator/README.md)
  - [SNR Operator Tests](../tests/snr-operator/README.md)

## Guides

- [File-Based Catalog (FBC) Operator Upgrades](fbc-upgrades.md) (`tier:upgrade-operator`): Common release upgrade contract, environment inputs, and execution for all six operators.
