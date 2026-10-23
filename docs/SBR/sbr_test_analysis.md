# Storage-Based Remediation (SBR) Test Analysis

## Table of Contents

- [Overview](#overview)
- [Test Structure](#test-structure)
  - [Test Files](#test-files)
- [Test Cases by Category](#test-cases-by-category)
  - [1. Post-Deployment Validation Tests](#1-post-deployment-validation-tests-sbrgo)
  - [2. Negative Tests](#2-negative-tests-sbrgo)
  - [3. Remediation CR Lifecycle](#3-remediation-cr-lifecycle-remediationgo)
  - [4. SBRC Lifecycle](#4-sbrc-lifecycle-sbrc_lifecyclego)
  - [5. Watchdog Tests](#5-watchdog-tests-watchdoggo)
  - [6. Destructive Failure Scenarios](#6-destructive-failure-scenarios)
  - [7. Integration Tests](#7-integration-tests)
  - [8. Observability Tests](#8-observability-tests)
  - [9. Controller Resilience](#9-controller-resilience-controller_resiliencego)
  - [10. Upgrade Tests](#10-upgrade-tests-upgrade_operator_fbcgo)
- [Test Labels and Categorization](#test-labels-and-categorization)
- [Platform Support](#platform-support)
- [Storage Class Configuration](#storage-class-configuration)
  - [Storage Requirements by Test Category](#storage-requirements-by-test-category)
  - [CephFS-Specific Tests](#cephfs-specific-tests)
  - [Environment Variable Override](#environment-variable-override)
  - [Common StorageClass Examples](#common-storageclass-examples)
  - [Storage Class Requirements Summary](#storage-class-requirements-summary)
- [Known Test Dependencies](#known-test-dependencies)
- [Common Test Patterns](#common-test-patterns)
- [Job Run Analysis](#job-run-analysis)
- [Test Execution Flow](#test-execution-flow)
- [Recommendations](#recommendations)
- [Summary](#summary)
- [Proposed New Test Cases for Ceph RBD Block Storage](#proposed-new-test-cases-for-ceph-rbd-block-storage-rhwa-system-tests)
  - [Background](#background)
  - [Proposed Test Cases for rhwa-system-tests](#proposed-test-cases-for-rhwa-system-tests)
    - [1. Ceph RBD Multi-Writer Concurrency Test](#1-ceph-rbd-multi-writer-concurrency-test)
    - [2. Ceph RBD Block Device Inspection and Slot Verification Test](#2-ceph-rbd-block-device-inspection-and-slot-verification-test)
    - [3. Ceph RBD Persistent Fencing State Across Node Reboot](#3-ceph-rbd-persistent-fencing-state-across-node-reboot)
    - [4. Ceph RBD Heartbeat Performance Under Load](#4-ceph-rbd-heartbeat-performance-under-load)
    - [5. Ceph RBD Recovery After Transient Network Partition](#5-ceph-rbd-recovery-after-transient-network-partition)
  - [Implementation Guide for rhwa-system-tests](#implementation-guide-for-rhwa-system-tests)
  - [Success Criteria](#success-criteria)
  - [Expected Benefits](#expected-benefits)
  - [Integration with OpenShift CI](#integration-with-openshift-ci)
  - [Proposed Ceph RBD Block Storage Tests - Storage Requirements Summary](#proposed-ceph-rbd-block-storage-tests---storage-requirements-summary)

---

## Overview
Analysis of Storage-Based Remediation tests from the rhwa-system-tests repository and job run from periodic-ci-openshift-rhwa-system-tests-main-5.0-upstream-e2e-sbr-daily-aws-odf/2107320620009132032

## Test Structure

The SBR test suite is organized in the `/tests/sbr-operator/tests/` directory with the following components:

### Test Files
1. **sbr.go** - Post-deployment and validation tests
2. **remediation.go** - StorageBasedRemediation CR lifecycle tests
3. **sbrc_lifecycle.go** - StorageBasedRemediationConfig CR lifecycle tests
4. **watchdog.go** - Watchdog device accessibility tests
5. **node_hang.go** - Node hang and kernel panic recovery tests
6. **storage_loss_watchdog.go** - Total storage I/O loss tests
7. **storage_loss_write_only.go** - Write-only storage loss tests
8. **transient_storage.go** - Transient storage failure tests
9. **split_brain.go** - Split-brain scenario tests
10. **nhc_integration.go** - Node Health Check integration tests
11. **detect_only.go** - Detection-only mode tests
12. **metrics.go** - Prometheus metrics tests
13. **must_gather.go** - Must-gather diagnostic collection tests
14. **controller_resilience.go** - Controller resilience and HA tests
15. **fresh_install.go** - Fresh installation tests
16. **upgrade_operator_fbc.go** - Operator upgrade via FBC tests

## Test Cases by Category

### 1. Post-Deployment Validation Tests (sbr.go)

#### Test ID: 90163 - Watchdog Inventory Discovery
**Purpose**: Discover /dev/watchdog* devices on all cluster nodes
**Description**: 
- Creates privileged debug pods on each node
- Probes for /dev/watchdog* devices via hostPID namespace
- Builds cluster-wide watchdog device inventory
**Expected**: Successfully discover watchdog devices on nodes that have them
**Storage Class**: None (no SBRC created)

#### Test ID: 89232 - Operator Pod Running
**Purpose**: Verify Storage-Based Remediation Operator pod is running
**Description**:
- Verifies expected replica count matches running pods
- Checks pod phase is Running
- Adjusts expectations for Single Node OpenShift (SNO)
**Expected**: Operator pods reach Running state and match expected count
**Storage Class**: None (validates controller pods only)

#### Test ID: 89233 - CSV Required Annotations
**Purpose**: Verify SBR CSV has required annotations
**Description**:
- Validates ClusterServiceVersion annotations for downstream packaging
- Checks feature annotations specific to product
**Expected**: All required annotations present with correct values
**Status**: Skipped when ECO_IS_DOWNSTREAM=false
**Storage Class**: None (validates CSV metadata only)

#### Test ID: 89234 - Correct Replica Count
**Purpose**: Verify SBR controller manager has correct number of replicas
**Description**:
- Validates replica count matches expected value (2 for HA clusters)
- Verifies pods distributed across different nodes for HA
- Skipped on SNO clusters
**Expected**: 2 ready replicas on distinct nodes (non-SNO)
**Storage Class**: None (validates controller deployment only)

#### Test ID: 89235 - Non-Root Security Context
**Purpose**: Verify SBR container runs as non-root user
**Description**:
- Validates security context of controller pods
- Ensures non-root user execution
**Expected**: All containers run with non-root user
**Storage Class**: None (validates security context only)

#### Test ID: 88822 - API and OLM Naming
**Purpose**: Verify SBR uses correct API and OLM naming
**Description**:
- Validates CSV display name uses "Storage-Based Remediation" (not "SBD")
- Verifies CRD API group correctness
- Checks CRD version alignment
**Expected**: Proper naming conventions followed
**Storage Class**: None (validates API/OLM metadata only)

### 2. Negative Tests (sbr.go)

#### Test ID: 88881 - CR Validation
**Purpose**: Verify StorageBasedRemediationConfig CR validation rejects invalid field values
**Description**:
- Layer 1: Tests CRD OpenAPI schema validation at API server
- Layer 2: Tests controller-level validation
- Tests: timeout below min, timeout above max, failures below min, failures above max
- Tests non-existent StorageClass handling
**Expected**: 
- API server rejects out-of-range values
- Controller doesn't deploy DaemonSet for invalid StorageClass
**Storage Class**: 
- Layer 1: None (tests field value validation only)
- Layer 2: Uses fictional "nonexistent-storage-class" to verify controller validation

#### Test ID: 88741 - Invalid Watchdog and NodeSelector
**Purpose**: Verify SBRC controller handles invalid watchdog path and non-matching nodeSelector
**Description**:
- Tests invalid watchdog device path
- Tests nodeSelector that matches no cluster nodes
**Expected**:
- No DaemonSet created for invalid watchdog path
- DaemonSet created but 0 pods scheduled for non-matching nodeSelector
**Storage Class**: None (tests watchdog path and nodeSelector validation)

### 3. Remediation CR Lifecycle (remediation.go)

#### Test ID: 88737 - CR Lifecycle
**Purpose**: Verify StorageBasedRemediation CR lifecycle: admission, finalizer, and deletion cleanup
**Description**:
- Creates StorageBasedRemediation CR targeting a worker node
- Verifies finalizer addition by controller
- Tests CR deletion and finalizer release
- Restores node schedulability post-test
**Expected**:
- CR admitted with empty spec
- Finalizer added by controller
- CR fully removed after deletion
**Storage Class**: 
- **Auto-discovered RWX**: CephFS-based (searches for provisioner containing "cephfs")
- **Override**: Set `SBR_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) access mode
- **Note**: Requires SBRC with sharedStorageClass so agent DaemonSet runs and can add finalizers

### 4. SBRC Lifecycle (sbrc_lifecycle.go)

#### Test ID: 88734 - SBRC Lifecycle
**Purpose**: Verify StorageBasedRemediationConfig CR create, patch, multi-instance, and delete lifecycle
**Description**:
- Tests SBRC creation with RWX storage class
- Tests agent DaemonSet deployment and readiness
- Tests SBRC patching capabilities
- Tests multi-instance SBRC support
- Tests proper cleanup on deletion
**Expected**: Complete lifecycle operations succeed
**Storage Class**: 
- **Auto-discovered RWX**: CephFS-based (searches for provisioner containing "cephfs")
- **Override**: Set `SBR_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) access mode

### 5. Watchdog Tests (watchdog.go)

#### Test ID: 88878 - Watchdog Device Accessibility
**Purpose**: Verify watchdog device accessibility and softdog module availability
**Description**:
- Checks /dev/watchdog device accessibility
- Verifies softdog kernel module availability
- Tests watchdog device open/close operations
**Expected**: Watchdog devices accessible when present
**Storage Class**: None (tests watchdog device access only, uses watchdog inventory from Test 90163)

### 6. Destructive Failure Scenarios

#### Test ID: 88738 - Node Hang (node_hang.go)
**Purpose**: Node hang: kernel panic triggers reboot, NHC fences, node recovers
**Description**:
- Simulates kernel panic via sysrq trigger
- Verifies node becomes NotReady
- Validates SBR-triggered fencing
- Confirms node recovery post-reboot
**Expected**: Node successfully recovers after kernel panic and fencing
**Storage Class**: 
- **Auto-discovered RWX**: CephFS-based (searches for provisioner containing "cephfs")
- **Override**: Set `SBR_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) access mode
- **Used for**: Agent DaemonSet deployment and shared storage coordination

#### Test ID: 88880 - Total Storage I/O Loss (storage_loss_watchdog.go)
**Purpose**: Total storage I/O loss: watchdog fires, node reboots, fencing completes
**Description**:
- Simulates total storage I/O loss
- Verifies watchdog timeout triggers reboot
- Validates fencing completion
- Confirms node recovery
**Expected**: Watchdog triggers reboot, fencing completes, node recovers
**Storage Class**: 
- **Auto-discovered RWX**: CephFS-based (searches for provisioner containing "cephfs")
- **Override**: Set `SBR_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) access mode
- **CephFS Specific**: Test uses iptables rules to block CephFS ports (6789, 6800-7300)
- **Note**: Skips if StorageClass provisioner doesn't contain "ceph"

#### Test ID: 89200 - Write-Only Storage Loss (storage_loss_write_only.go)
**Purpose**: Write-only storage loss: fence-message-read path triggers self-fencing
**Description**:
- Simulates storage loss affecting write operations only
- Tests fence message read path
- Verifies self-fencing trigger
**Expected**: Write-only loss detected and self-fencing triggered
**Storage Class**: 
- **Auto-discovered RWX**: CephFS-based (searches for provisioner containing "cephfs")
- **Override**: Set `SBR_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) access mode

#### Test ID: 88877 - Split-Brain Prevention (split_brain.go)
**Purpose**: Split-brain: only the storage-isolated node is fenced; healthy witnesses are untouched
**Description**:
- Simulates network partition creating split-brain scenario
- Verifies only storage-isolated node is fenced
- Ensures healthy nodes remain operational
**Expected**: Correct identification and fencing of isolated node only
**Storage Class**: 
- **Auto-discovered RWX**: CephFS-based (searches for provisioner containing "cephfs")
- **Override**: Set `SBR_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) access mode

#### Test ID: 88735 - Transient Storage Failure (transient_storage.go)
**Purpose**: Verify transient storage failure clears without fencing
**Description**:
- Simulates brief, recoverable storage disruption
- Verifies condition clears without triggering fence
- Validates no unnecessary node disruption
**Expected**: Transient failures don't trigger fencing
**Storage Class**: 
- **Auto-discovered**: CephFS-based (searches for provisioner containing "cephfs")
- **Override**: Set `SBR_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) access mode
- **CephFS Specific**: Uses port-based iptables injection rules for CephFS (ports 6789, 6800)
- **Note**: Skips if `SBR_STORAGE_CLASS` is set but provisioner doesn't contain "cephfs"

### 7. Integration Tests

#### Test ID: 88879 - NHC Integration (nhc_integration.go)
**Purpose**: Node Health Check integration
**Description**:
- Tests integration with Node Health Check operator
- Verifies remediation triggered by NHC
- Validates coordination between operators
**Expected**: NHC correctly triggers SBR remediation
**Storage Class**: 
- **Auto-discovered RWX**: CephFS-based (searches for provisioner containing "cephfs")
- **Override**: Set `SBR_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) access mode

#### Test ID: 88876 - Detection-Only Mode (detect_only.go)
**Purpose**: Verify detect-only configuration, storage-fault suppression, mode toggle, and agent cleanup
**Description**:
- Verifies `detectOnlyMode: Enabled` is reflected in the StorageBasedRemediationConfig
- Toggles detect-only mode to Disabled and back to Enabled before any fault, verifying each time that the agent DaemonSet rolls out running pods (a toggle restarts every agent pod, and a pod restarted on the faulted node cannot remount CephFS)
- Requires no NodeHealthCheck to reference SBR directly or through escalating remediations, and waits for any terminating remediation CR for the target node to disappear before injecting the fault
- Injects a CephFS storage fault and waits for `SBRStorageUnhealthy=True`
- Verifies the node remains Ready and schedulable and no StorageBasedRemediation CR appears during the suppression window
- Restores storage connectivity
- Deletes the configuration and verifies the agent DaemonSet is garbage-collected
- Leaves remediation CRs owned by other actors untouched, including when the absence preflight fails
**Expected**: Storage failure is detected without remediation in detect-only mode, the mode toggle succeeds, and configuration deletion removes the agents
**Storage Class**: 
- **Auto-discovered RWX**: CephFS-based (searches for provisioner containing "cephfs")
- **Override**: Set `SBR_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) access mode

#### Test ID: 1933 - Detect-Only Startup With Watchdog Held Open (`detect_only.go`)
**Purpose**: Verify SBR agents remain Ready in detect-only mode while another process holds `/dev/watchdog` open
**Description**:
- Starts a privileged watchdog keepalive pod on every schedulable worker before creating the detect-only configuration
- Verifies all agent pods stay Ready with the same pod identities and restart counts during a 90-second observation window
- Verifies keepalive containers remain running and no worker reports `SBRStorageUnhealthy=True`
- Does not assert specific preflight log messages
**Expected**: Watchdog contention does not prevent agent startup or stability in detect-only mode
**Storage Class**:
- **Auto-discovered RWX**: CephFS-based (searches for provisioner containing "cephfs")
- **Override**: Set `SBR_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) access mode

### 8. Observability Tests

#### Test ID: 89202 - Prometheus Metrics (metrics.go)
**Purpose**: Verify SBR agent pods expose required Prometheus metrics
**Description**:
- Validates metrics endpoint availability
- Checks for required metrics
- Verifies metrics format
**Expected**: All required metrics exposed correctly
**Storage Class**: 
- **Auto-discovered RWX**: CephFS-based (searches for provisioner containing "cephfs")
- **Override**: Set `SBR_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) access mode
- **Note**: sharedStorageClass required to create agent DaemonSet for metrics validation

#### Test ID: 88733 - Must-Gather (must_gather.go)
**Purpose**: Verify SBR must-gather collects diagnostic data
**Description**:
- Tests must-gather diagnostic collection
- Validates collected data completeness
**Expected**: Must-gather successfully collects SBR diagnostics
**Storage Class**: None (validates must-gather collection only)

### 9. Controller Resilience (controller_resilience.go)

#### Test ID: 90306 - Controller Availability
**Purpose**: Should maintain controller availability with one worker
**Description**:
- Tests controller operation with minimal resources
- Validates continued operation on single worker
**Expected**: Controller remains available
**Storage Class**: None (tests controller resilience only)

#### Test ID: 90307 - Leadership Transfer
**Purpose**: Should transfer controller leadership when active pod is deleted
**Description**:
- Tests leader election mechanism
- Validates smooth leadership transfer
- Ensures no service disruption
**Expected**: Successful leadership transfer
**Storage Class**: None (tests leader election only)

### 10. Upgrade Tests (upgrade_operator_fbc.go)

**Purpose**: Validate operator upgrades via File-Based Catalog
**Description**:
- Tests upgrade path from previous versions
- Validates CRD schema compatibility
- Ensures existing resources continue functioning
**Expected**: Clean upgrade with no resource disruption
**Storage Class**: 
- **Auto-discovered RWX**: CephFS-based (searches for provisioner containing "cephfs")
- **Override**: Set `SBR_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) access mode
- **Note**: Required to test SBRC upgrade compatibility

## Test Labels and Categorization

### Disruption Levels
- **DisruptionNonDestructive**: Tests that don't affect cluster workloads
- **DisruptionDestructive**: Tests that may cause node reboots or workload disruption

### Tiers
- **TierSmoke**: Quick validation tests (< 5 min)
- **TierAcceptance**: Standard functional tests (5-15 min)
- **TierExtended**: Comprehensive tests (> 15 min)

### Components
- **ComponentPostDeploy**: Post-installation validation
- **ComponentController**: Controller functionality
- **ComponentRemediation**: Remediation logic
- **ComponentOLM**: OLM integration
- **ComponentAgent**: Agent functionality

### Frequency
- **FrequencyPresubmit**: Run on every PR
- **FrequencyNightly**: Run daily
- **FrequencyWeekly**: Run weekly

## Platform Support
- **PlatformAny**: All platforms
- **PlatformAWS**: AWS-specific tests
- **PlatformBaremetal**: Bare metal-specific tests

## Storage Class Configuration

### Overview
Most SBR functional tests require a StorageClass that supports **ReadWriteMany (RWX)** access mode for shared storage coordination between agent pods.

### Auto-Discovery Mechanism
The test suite uses the `discoverRWXStorageClass()` function which:
1. **First checks**: `SBR_STORAGE_CLASS` environment variable
2. **Auto-discovers**: Searches for StorageClass with provisioner containing "cephfs"
3. **Skips test**: If neither is available

### StorageClass Discovery Logic
```go
func discoverRWXStorageClass() string {
    if sbrparams.SBRStorageClass != "" {
        return sbrparams.SBRStorageClass  // From SBR_STORAGE_CLASS env var
    }
    
    // Auto-discover CephFS-based storage class
    for _, sc := range storageClasses {
        if strings.Contains(sc.Provisioner, "cephfs") {
            return sc.Name
        }
    }
    
    Skip("No CephFS StorageClass found; set SBR_STORAGE_CLASS env var to override")
}
```

### Storage Requirements by Test Category

| Test Category | Test Name | Polarion ID | Storage Required | StorageClass Type | CephFS Specific |
|---------------|-----------|-------------|------------------|-------------------|-----------------|
| Post-deployment validation | Watchdog Inventory Discovery | 90163 | ❌ No | N/A | N/A |
| Post-deployment validation | Operator Pod Running | 89232 | ❌ No | N/A | N/A |
| Post-deployment validation | CSV Required Annotations | 89233 | ❌ No | N/A | N/A |
| Post-deployment validation | Correct Replica Count | 89234 | ❌ No | N/A | N/A |
| Post-deployment validation | Non-Root Security Context | 89235 | ❌ No | N/A | N/A |
| Post-deployment validation | API and OLM Naming | 88822 | ❌ No | N/A | N/A |
| Negative validation tests | CR Validation | 88881 | ⚠️ Partial | Mock/Invalid for negative cases | No |
| Negative validation tests | Invalid Watchdog and NodeSelector | 88741 | ❌ No | N/A | N/A |
| CR Lifecycle tests | Verify StorageBasedRemediation CR lifecycle: admission, finalizer, and deletion cleanup | 88737 | ✅ Yes | RWX (CephFS auto-discovered) | No |
| SBRC Lifecycle tests | Verify StorageBasedRemediationConfig CR create, patch, multi-instance, and delete lifecycle | 88734 | ✅ Yes | RWX (CephFS auto-discovered) | No |
| Watchdog tests | Verify watchdog device accessibility and softdog module availability | 88878 | ❌ No | N/A | N/A |
| Node hang tests | Node hang: kernel panic triggers reboot, NHC fences, node recovers | 88738 | ✅ Yes | RWX (CephFS auto-discovered) | No |
| Total storage I/O loss | Total storage I/O loss: watchdog fires, node reboots, fencing completes | 88880 | ✅ Yes | RWX (CephFS required) | ✅ Yes - uses CephFS port blocking |
| Write-only storage loss | Write-only storage loss: fence-message-read path triggers self-fencing | 89200 | ✅ Yes | RWX (CephFS auto-discovered) | No |
| Split-brain prevention | Split-brain: simultaneous NHC triggers to peer-fence each other | 88877 | ✅ Yes | RWX (CephFS auto-discovered) | No |
| Transient storage failure | Verify transient storage failure clears without fencing | 88735 | ✅ Yes | RWX (CephFS required) | ✅ Yes - uses CephFS port injection |
| NHC Integration | Verify NHC integration with auto-created NHC CR | 88739 | ✅ Yes | RWX (CephFS auto-discovered) | No |
| NHC Integration | Verify NHC integration with user-provided NHC CR | 88879 | ✅ Yes | RWX (CephFS auto-discovered) | No |
| Detection-only mode | Verify detection-only mode: node state unhealthy, no fencing | 88742 | ✅ Yes | RWX (CephFS auto-discovered) | No |
| Metrics tests | Verify SBR controller metrics exposure | 88743 | ✅ Yes | RWX (CephFS auto-discovered) | No |
| Must-gather tests | Verify must-gather collects SBR resources | 88782 | ❌ No | N/A | N/A |
| Controller resilience | Verify controller resilience after pod restart | 88787 | ❌ No | N/A | N/A |
| Fresh install tests | Verify SBR fresh install from catalog source | 89228 | ❌ No | N/A | N/A |
| Upgrade tests | Verify SBR operator upgrade via FBC | 89309 | ✅ Yes | RWX (CephFS auto-discovered) | No |

### CephFS-Specific Tests
Some tests specifically require CephFS storage because they use storage disruption techniques that are CephFS-specific:

1. **Test 88880 - Total Storage I/O Loss** (`storage_loss_watchdog.go`)
   - Uses iptables rules to block CephFS ports: 6789 (monitor), 6800-7300 (OSD)
   - Skips if provisioner doesn't contain "ceph"
   - Storage disruption mechanism: Port-based network blocking

2. **Test 88735 - Transient Storage Failure** (`transient_storage.go`)
   - Uses iptables rules to block CephFS ports: 6789, 6800
   - Validates that `SBR_STORAGE_CLASS` provisioner contains "cephfs" if set
   - Storage disruption mechanism: Port-based injection for brief outage

### Environment Variable Override

Set `SBR_STORAGE_CLASS` to use a specific StorageClass:

```bash
export SBR_STORAGE_CLASS='ocs-storagecluster-cephfs'
# or for NFS (non-CephFS tests only)
export SBR_STORAGE_CLASS='nfs-storage-class'
```

**Note**: Setting `SBR_STORAGE_CLASS` to a non-CephFS StorageClass will cause CephFS-specific tests (88880, 88735) to skip.

### Common StorageClass Examples

| Platform | StorageClass Name | Provisioner | Notes |
|----------|------------------|-------------|-------|
| ODF/OCS | `ocs-storagecluster-cephfs` | `openshift-storage.cephfs.csi.ceph.com` | Recommended for OpenShift |
| Rook-Ceph | `rook-cephfs` | `rook-ceph.cephfs.csi.ceph.com` | Common in upstream Kubernetes |
| NFS | Custom | `kubernetes.io/no-provisioner` or NFS CSI | Requires manual PV creation for static provisioners |
| AWS EFS | `efs-sc` | `efs.csi.aws.com` | Works for non-CephFS tests |

### Static vs Dynamic Provisioning

**Dynamic Provisioning** (CephFS, AWS EFS CSI):
- PVCs automatically create PVs
- Tests proceed immediately after SBRC creation

**Static Provisioning** (Manual NFS):
- Requires pre-created PVs
- Test performs pre-check: verifies Available or Bound PV exists before creating SBRC
- Pre-check timeout: Configured in `sbrparams.PVCheckTimeout`
- Prevents long timeout waiting for PVC bind that will never happen

### Storage Class Requirements Summary

**Required Capabilities**:
- ✅ ReadWriteMany (RWX) access mode
- ✅ Supports multiple pods on different nodes accessing the same volume
- ✅ Sufficient IOPS for frequent heartbeat writes

**Recommended**:
- CephFS-based for full test coverage
- Dynamic provisioning for faster test execution
- Low-latency storage for reliable heartbeat mechanism

## Known Test Dependencies

1. **Storage Requirements**:
   - Tests require RWX (ReadWriteMany) capable storage class
   - Auto-discovers CephFS storage classes
   - Can be overridden via `SBR_STORAGE_CLASS` env var
   - Some tests specifically require ODF/CephFS (see Storage Class Configuration above)

2. **Cluster Requirements**:
   - Multi-node cluster for HA tests
   - Worker nodes for remediation tests
   - Privileged access for watchdog and disruption tests

3. **Timing Considerations**:
   - SBRC ready timeout: Extended for cold-start scenarios
   - PVC bind delays on fresh ODF clusters
   - Device-init job completion required before agent deployment

## Common Test Patterns

### Test Initialization
```go
BeforeAll(func() {
    // Pre-cleanup of stale resources
    // Storage class discovery
    // SBRC creation for agent deployment
    // Node selection and validation
})
```

### Test Cleanup
```go
DeferCleanup(func() {
    // CR deletion
    // Node state restoration
    // Resource cleanup
})
```

### Validation Patterns
- **Eventually**: For async operations with timeout
- **Consistently**: For stability validation over time
- **Expect + Eventually**: Standard assertion pattern

## Job Run Analysis

**Note**: The job log URL provided (`https://gcsweb-test-platform-results-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/test-platform-results/logs/periodic-ci-openshift-rhwa-system-tests-main-5.0-upstream-e2e-sbr-daily-aws-odf/2107320620009132032/artifacts/e2e-sbr-daily-aws-odf/e2e-test/build-log.txt`) requires authentication and could not be accessed directly.

### Alternative Ways to Check Results

1. **Via OpenShift CI Dashboard**:
   - Visit https://prow.ci.openshift.org/
   - Search for job: `periodic-ci-openshift-rhwa-system-tests-main-5.0-upstream-e2e-sbr-daily-aws-odf`
   - Navigate to specific run: 2107320620009132032

2. **Via PR/Issue Links**:
   - Check associated GitHub PR or issue for test results
   - Look for automated bot comments with test status

3. **Via Command Line** (if you have access):
   ```bash
   # Download artifacts using authenticated curl
   curl -H "Authorization: Bearer $TOKEN" \
     https://gcsweb-test-platform-results-ci.apps.ci.l2s4.p1.openshiftapps.com/... \
     -o build-log.txt
   ```

## Test Execution Flow

### Typical Test Execution Order

1. **Pre-flight Checks**:
   - Watchdog inventory discovery
   - Operator pod validation
   - CSV annotation validation

2. **Basic Functionality**:
   - SBRC lifecycle tests
   - SBR CR lifecycle tests
   - Negative validation tests

3. **Integration Tests**:
   - Watchdog accessibility
   - Metrics exposure
   - NHC integration

4. **Destructive Tests** (if enabled):
   - Node hang scenarios
   - Storage loss scenarios
   - Split-brain prevention
   - Transient failure handling

5. **Extended Tests**:
   - Controller resilience
   - Upgrade scenarios
   - Must-gather collection

## Recommendations

### For Test Development
1. Always use `DeferCleanup` for resource cleanup
2. Leverage `waitForSBRCReady` before testing remediation
3. Use node selection that excludes controller nodes
4. Implement proper timeout values for storage operations

### For Test Debugging
1. Check SBRC readiness diagnostics output
2. Review agent pod logs and status
3. Verify storage class and PVC state
4. Check node conditions and taints

### For CI/CD Integration
1. Separate destructive tests into dedicated jobs
2. Use appropriate labels for test filtering
3. Set realistic timeouts for storage provisioning
4. Configure proper cleanup on test failure

## Summary

The SBR test suite provides comprehensive coverage across:
- ✅ Post-deployment validation
- ✅ CR lifecycle management
- ✅ Operator resilience
- ✅ Destructive failure scenarios
- ✅ Integration with other operators
- ✅ Observability and diagnostics
- ✅ Upgrade paths

The tests follow Ginkgo BDD patterns with proper cleanup, timeout handling, and diagnostic collection, making them suitable for automated CI/CD pipelines.

---

# Proposed New Test Cases for Ceph RBD Block Storage (rhwa-system-tests)

## Background

Based on the analysis of the existing SBR tests in `rhwa-system-tests/tests/sbr-operator/tests/` and the upstream block-mode tests, the following test cases are proposed specifically for **Ceph RBD block storage** environments with ODF (OpenShift Data Foundation) installed.

### Current rhwa-system-tests Block-Mode Coverage Gap

The existing rhwa-system-tests suite currently focuses on **CephFS (filesystem-based)** storage:
- Most tests use `discoverRWXStorageClass()` which auto-discovers CephFS provisioners
- Storage disruption tests (Test 88880, 88735) use CephFS-specific port blocking
- **No dedicated Ceph RBD block storage tests exist**

### Upstream Block-Mode Test Coverage (for reference)

The upstream storage-based-remediation repository includes:
1. **Storage write check confirmation** - Validates block-mode write checks pass
2. **Portworx write check withheld** - Validates single-writer limitation detection
3. **Generic tests** - Run on both filesystem and block storage

### Target Test Infrastructure
- **Repository**: `rhwa-system-tests/tests/sbr-operator/tests/`
- **Framework**: eco-goinfra + Ginkgo v2
- **CI**: OpenShift CI (Prow)
- **Storage Backend**: ODF Ceph RBD (OpenShift Data Foundation)
- **Volume Mode**: Block (`volumeMode: Block`)
- **New Test File**: `tests/sbr-operator/tests/block_storage_rbd.go`

---

## Proposed Test Cases for rhwa-system-tests

### Test File Location
**New File**: `tests/sbr-operator/tests/block_storage_rbd.go`

### Test Suite Structure
```go
var _ = Describe(
    "SBR Block Storage — Ceph RBD",
    Ordered,
    ContinueOnFailure,
    Label(labels.OperatorSBR, labels.ComponentAgent), func() {
        // Tests go here
    })
```

---

### 1. Ceph RBD Multi-Writer Concurrency Test

**Polarion ID**: TBD (assign after test creation)  
**Labels**: `labels.OperatorSBR`, `labels.ComponentAgent`, `labels.TierAcceptance`, `labels.PlatformAny`, `labels.FrequencyNightly`  
**Priority**: High  
**Disruption**: Non-destructive

**Purpose**: Verify that Ceph RBD block storage correctly handles concurrent writes from multiple SBR agents across different nodes, validating true RWX (ReadWriteMany) block device sharing.

**Test Declaration**:
```go
It("Verify SBR agents write concurrently to Ceph RBD block device without conflicts",
    reportxml.ID("TBD"),
    Label(
        labels.OperatorSBR,
        labels.DisruptionNonDestructive,
        labels.TierAcceptance,
        labels.PlatformAny,
        labels.ComponentAgent,
        labels.FrequencyNightly,
    ), func() {
        testCephRBDConcurrentWrites()
    })
```

**Test Implementation** (eco-goinfra pattern):
```go
func testCephRBDConcurrentWrites() {
    By("Discovering Ceph RBD block storage class")
    rbdStorageClass := discoverCephRBDStorageClass()
    Expect(rbdStorageClass).ToNot(BeEmpty(), 
        "Ceph RBD storage class required; set via ODF or skip test")
    
    By("Creating StorageBasedRemediationConfig with Ceph RBD block storage")
    blockMode := "Block"
    sbrc := buildSBRC(sbrparams.SBRCBlockTestName, map[string]interface{}{
        "sharedStorageClass": rbdStorageClass,
        "sharedStorageVolumeMode": blockMode,
    })
    err := APIClient.Create(context.TODO(), sbrc)
    Expect(err).ToNot(HaveOccurred())
    
    DeferCleanup(func() {
        // Cleanup SBRC
    })
    
    By("Waiting for agent DaemonSet to deploy across all worker nodes")
    waitForSBRCReady(sbrparams.SBRCBlockTestName)
    
    By("Verifying all agents reach Ready state")
    listOptions := metav1.ListOptions{LabelSelector: "app=sbr-agent"}
    Eventually(func() error {
        agentPods, err := pod.List(APIClient, medik8sparams.OperatorNs, listOptions)
        if err != nil {
            return err
        }
        readyCount := len(helpers.FilterRunningPods(agentPods))
        expectedCount := getWorkerNodeCount()
        if readyCount < expectedCount {
            return fmt.Errorf("only %d/%d agents ready", readyCount, expectedCount)
        }
        return nil
    }, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed())
    
    By("Triggering concurrent heartbeat writes from all agents")
    // Implementation: exec into each pod, trigger write, capture timing
    
    By("Validating block device consistency across agents")
    // Implementation: compare slot data across pods
}
```

**Storage Class Discovery**:
```go
func discoverCephRBDStorageClass() string {
    if envClass := os.Getenv("SBR_RBD_STORAGE_CLASS"); envClass != "" {
        return envClass
    }
    
    scList, err := APIClient.StorageV1Interface.StorageClasses().List(
        context.TODO(), metav1.ListOptions{})
    Expect(err).ToNot(HaveOccurred())
    
    for idx := range scList.Items {
        provisioner := scList.Items[idx].Provisioner
        // ODF Ceph RBD provisioner
        if strings.Contains(provisioner, "rbd.csi.ceph.com") {
            GinkgoWriter.Printf("Auto-discovered Ceph RBD StorageClass: %s (provisioner: %s)\n",
                scList.Items[idx].Name, provisioner)
            return scList.Items[idx].Name
        }
    }
    
    Skip("No Ceph RBD StorageClass found; install ODF or set SBR_RBD_STORAGE_CLASS")
    return ""
}
```

**Why Ceph RBD Specific**:
- Ceph RBD supports true RWX for block devices via `openshift-storage.rbd.csi.ceph.com`
- Unlike Portworx which has single-writer limitation
- Critical for validating distributed consensus on block storage

**Expected Outcome**:
- ✅ All agents successfully write heartbeats concurrently
- ✅ Block device maintains data integrity
- ✅ Sequence numbers from different agents don't conflict
- ✅ No "device busy" or locking errors

**Storage Class**:
- **Auto-discovered**: Ceph RBD (searches for provisioner containing "rbd.csi.ceph.com")
- **Override**: Set `SBR_RBD_STORAGE_CLASS` env var
- **Requirement**: ReadWriteMany (RWX) with `volumeMode: Block`

---

---

### 2. Ceph RBD Block Device Inspection and Slot Verification Test

**Polarion ID**: TBD  
**Labels**: `labels.OperatorSBR`, `labels.ComponentAgent`, `labels.TierAcceptance`, `labels.PlatformAny`, `labels.FrequencyNightly`  
**Priority**: High  
**Disruption**: Non-destructive

**Purpose**: Validate SBR device inspection and debugging capabilities for Ceph RBD block-mode storage, enabling production troubleshooting on block devices.

**Test Declaration**:
```go
It("Verify SBR device inspection works on Ceph RBD block storage",
    reportxml.ID("TBD"),
    Label(
        labels.OperatorSBR,
        labels.DisruptionNonDestructive,
        labels.TierAcceptance,
        labels.PlatformAny,
        labels.ComponentAgent,
        labels.FrequencyNightly,
    ), func() {
        testCephRBDDeviceInspection()
    })
```

**Test Implementation**:
```go
func testCephRBDDeviceInspection() {
    By("Creating SBRC with Ceph RBD block storage")
    rbdStorageClass := discoverCephRBDStorageClass()
    blockMode := "Block"
    sbrc := buildSBRC(sbrparams.SBRCBlockInspectionTestName, map[string]interface{}{
        "sharedStorageClass": rbdStorageClass,
        "sharedStorageVolumeMode": blockMode,
    })
    err := APIClient.Create(context.TODO(), sbrc)
    Expect(err).ToNot(HaveOccurred())
    DeferCleanup(/* cleanup SBRC */)
    
    waitForSBRCReady(sbrparams.SBRCBlockInspectionTestName)
    
    By("Finding SBR agent pods for inspection")
    agentPods, err := pod.List(APIClient, medik8sparams.OperatorNs,
        metav1.ListOptions{LabelSelector: "app=sbr-agent"})
    Expect(err).ToNot(HaveOccurred())
    Expect(agentPods).ToNot(BeEmpty())
    
    testPod := agentPods[0]
    
    By("Executing block device inspection commands")
    // Note: These commands currently skip in upstream for block mode
    // This test validates they now work for Ceph RBD
    
    // Execute sbr-device-summary via exec
    cmd := []string{"sbr-device-summary"}
    output, execErr := testPod.ExecCommand(cmd)
    Expect(execErr).ToNot(HaveOccurred(), 
        "Block device inspection should work on Ceph RBD")
    
    By("Validating inspection output format")
    // Parse and validate output structure
    
    By("Comparing slot data across all agent pods")
    type slotSummary struct {
        PodName string
        Slots   []SlotData
    }
    summaries := []slotSummary{}
    
    for _, agentPod := range agentPods {
        // Exec and collect slot data from each pod
        // Compare NodeIDs, Types, Sequences across pods
    }
    
    By("Saving inspection artifacts")
    // Save to .tests/ or artifacts directory
}
```

**Why Important**:
- Addresses gap: rhwa-system-tests currently has no block storage inspection tests
- Block-mode debugging capabilities are essential for production troubleshooting
- Validates block storage-specific slot format and consistency

**Expected Outcome**:
- ✅ Inspection commands work for Ceph RBD block storage
- ✅ Slot summaries are consistent across all agent pods
- ✅ Debug artifacts saved for post-test analysis
- ✅ Enables production troubleshooting on ODF environments

**Storage Class**:
- **Auto-discovered**: Ceph RBD
- **Override**: Set `SBR_RBD_STORAGE_CLASS` env var
- **Requirement**: `volumeMode: Block`

---

---

### 3. Ceph RBD Persistent Fencing State Across Node Reboot

**Polarion ID**: TBD  
**Labels**: `labels.OperatorSBR`, `labels.ComponentRemediation`, `labels.TierAcceptance`, `labels.DisruptionDestructive`, `labels.FrequencyWeekly`  
**Priority**: Medium  
**Disruption**: **Destructive** (causes node reboot)

**Purpose**: Verify that fencing state persists correctly on Ceph RBD block storage across node reboots and agent restarts, ensuring no phantom re-fencing occurs.

**Test Declaration**:
```go
It("Verify fencing state persists on Ceph RBD block storage across node reboot",
    reportxml.ID("TBD"),
    Label(
        labels.OperatorSBR,
        labels.DisruptionDestructive,
        labels.TierAcceptance,
        labels.PlatformAny,
        labels.ComponentRemediation,
        labels.FrequencyWeekly,
    ), func() {
        testCephRBDPersistentFencingState()
    })
```

**Test Implementation**:
```go
func testCephRBDPersistentFencingState() {
    By("Creating SBRC with Ceph RBD block storage")
    rbdStorageClass := discoverCephRBDStorageClass()
    // Create SBRC with block mode...
    
    By("Selecting target worker node")
    targetNode := selectSchedulableWorkerNode()
    
    By("Recording original boot ID")
    originalBootID := getNodeBootID(targetNode.Name)
    
    By("Creating StorageBasedRemediation CR")
    sbrCR := buildSBR(targetNode.Name)
    err := APIClient.Create(context.TODO(), sbrCR)
    Expect(err).ToNot(HaveOccurred())
    DeferCleanup(/* cleanup SBR CR */)
    
    By("Waiting for fencing to complete")
    Eventually(func() bool {
        current := &unstructured.Unstructured{}
        // Fetch SBR CR and check FencingSucceeded condition
        return checkFencingSucceeded(current)
    }, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(BeTrue())
    
    By("Capturing block device slot state before reboot")
    slotsBeforeReboot := captureBlockDeviceSlots()
    
    By("Waiting for node to reboot")
    Eventually(func() string {
        return getNodeBootID(targetNode.Name)
    }, time.Minute*10, time.Second*30).ShouldNot(Equal(originalBootID))
    
    By("Waiting for node to become Ready after reboot")
    waitForNodeReady(targetNode.Name)
    
    By("Waiting for agent pod to restart on rebooted node")
    waitForAgentPodRunning(targetNode.Name)
    
    By("Capturing block device slot state after reboot")
    slotsAfterReboot := captureBlockDeviceSlots()
    
    By("Validating fencing state persistence")
    Expect(slotsAfterReboot).To(ContainFenceStateFrom(slotsBeforeReboot),
        "Fence state should persist across reboot")
    
    By("Verifying no duplicate fencing attempts")
    // Check logs for re-fencing attempts
}
```

**Why Ceph RBD Specific**:
- Block storage persistence is different from filesystem-based storage
- Validates that RBD block device writes are truly durable
- Ensures fence messages survive kernel crashes/reboots

**Expected Outcome**:
- ✅ Fence state persists across node reboot
- ✅ No duplicate fencing attempts
- ✅ Slot data maintains continuity
- ✅ Agent recognizes pre-existing fence state

**Storage Class**:
- **Auto-discovered**: Ceph RBD
- **Override**: Set `SBR_RBD_STORAGE_CLASS` env var

---

---

### 4. Ceph RBD Heartbeat Performance Under Load

**Polarion ID**: TBD  
**Labels**: `labels.OperatorSBR`, `labels.ComponentAgent`, `labels.TierExtended`, `labels.PlatformAny`, `labels.FrequencyWeekly`  
**Priority**: Medium  
**Disruption**: Non-destructive

**Purpose**: Validate SBR heartbeat mechanism performance and reliability on Ceph RBD block storage under sustained high-frequency writes, ensuring latency doesn't cause false-positive fencing.

**Test Declaration**:
```go
It("Verify SBR heartbeats remain stable on Ceph RBD under load",
    reportxml.ID("TBD"),
    Label(
        labels.OperatorSBR,
        labels.DisruptionNonDestructive,
        labels.TierExtended,
        labels.PlatformAny,
        labels.ComponentAgent,
        labels.FrequencyWeekly,
    ), func() {
        testCephRBDHeartbeatPerformance()
    })
```

**Test Implementation**:
```go
func testCephRBDHeartbeatPerformance() {
    By("Creating SBRC with Ceph RBD and minimum timeout")
    rbdStorageClass := discoverCephRBDStorageClass()
    sbrc := buildSBRC(sbrparams.SBRCBlockPerfTestName, map[string]interface{}{
        "sharedStorageClass": rbdStorageClass,
        "sharedStorageVolumeMode": "Block",
        "sbrTimeoutSeconds": int64(sbrparams.SBRCTimeoutSecondsMin), // 10s
    })
    err := APIClient.Create(context.TODO(), sbrc)
    Expect(err).ToNot(HaveOccurred())
    DeferCleanup(/* cleanup */)
    
    waitForSBRCReady(sbrparams.SBRCBlockPerfTestName)
    
    By("Monitoring baseline heartbeat latency for 2 minutes")
    baselineLatencies := monitorHeartbeatLatency(time.Minute * 2)
    baselineP99 := calculateP99(baselineLatencies)
    GinkgoWriter.Printf("Baseline P99 latency: %v\n", baselineP99)
    
    By("Injecting CPU load on all worker nodes")
    stressPods := injectCPULoad()
    DeferCleanup(func() { cleanupStressPods(stressPods) })
    
    By("Monitoring heartbeat latency under load for 3 minutes")
    loadedLatencies := monitorHeartbeatLatency(time.Minute * 3)
    loadedP99 := calculateP99(loadedLatencies)
    GinkgoWriter.Printf("Under-load P99 latency: %v\n", loadedP99)
    
    By("Validating latency remains acceptable")
    Expect(loadedP99).To(BeNumerically("<", 500*time.Millisecond),
        "Heartbeat P99 latency should remain under 500ms")
    
    By("Verifying no false-positive SBRStorageUnhealthy conditions")
    nodes := getAllWorkerNodes()
    for _, node := range nodes {
        Expect(hasStorageUnhealthyCondition(node)).To(BeFalse(),
            "Node %s should not have SBRStorageUnhealthy condition", node.Name)
    }
    
    By("Validating sequence number continuity")
    // Check for gaps in sequence numbers
}
```

**Why Important**:
- Block storage I/O characteristics differ from filesystem
- Ceph RBD latency can vary under load
- Need to validate SBR timeout tuning for block storage

**Expected Outcome**:
- ✅ Heartbeat writes maintain <500ms latency at p99
- ✅ No false-positive storage unhealthy conditions
- ✅ No unintended fencing under normal load
- ✅ Consistent sequence number progression

**Storage Class**:
- **Auto-discovered**: Ceph RBD
- **Override**: Set `SBR_RBD_STORAGE_CLASS` env var

---

---

### 5. Ceph RBD Recovery After Transient Network Partition

**Polarion ID**: TBD  
**Labels**: `labels.OperatorSBR`, `labels.ComponentAgent`, `labels.TierAcceptance`, `labels.DisruptionDestructive`, `labels.FrequencyNightly`  
**Priority**: High  
**Disruption**: **Destructive** (temporary network disruption)

**Purpose**: Verify SBR correctly detects and recovers from transient Ceph RBD network disruptions without triggering unnecessary fencing, distinguishing between true storage loss and temporary network glitches.

**Test Declaration**:
```go
It("Verify SBR recovers from transient Ceph RBD network partition without fencing",
    reportxml.ID("TBD"),
    Label(
        labels.OperatorSBR,
        labels.DisruptionDestructive,
        labels.TierAcceptance,
        labels.PlatformAny,
        labels.ComponentAgent,
        labels.FrequencyNightly,
    ), func() {
        testCephRBDTransientNetworkPartition()
    })
```

**Test Implementation**:
```go
func testCephRBDTransientNetworkPartition() {
    By("Creating SBRC with Ceph RBD block storage")
    rbdStorageClass := discoverCephRBDStorageClass()
    // Create SBRC...
    
    By("Selecting target worker node")
    targetNode := selectSchedulableWorkerNode()
    
    By("Creating privileged disruptor pod on target node")
    disruptorPod := createCephNetworkDisruptorPod(targetNode.Name)
    DeferCleanup(func() { cleanupDisruptorPod(disruptorPod) })
    
    By("Waiting for disruptor to block Ceph RBD traffic")
    // Disruptor pod blocks ports: 3300, 6789, 6800-7300 for 15 seconds
    time.Sleep(time.Second * 5)
    
    By("Verifying brief SBRStorageUnhealthy condition appears")
    Eventually(func() bool {
        node, _ := APIClient.CoreV1Interface.Nodes().Get(
            context.TODO(), targetNode.Name, metav1.GetOptions{})
        return hasStorageUnhealthyCondition(node)
    }, time.Second*30, time.Second*5).Should(BeTrue())
    
    By("Waiting for network to be restored")
    // Disruptor pod self-terminates after 15s
    time.Sleep(time.Second * 20)
    
    By("Verifying SBRStorageUnhealthy condition clears")
    Eventually(func() bool {
        node, _ := APIClient.CoreV1Interface.Nodes().Get(
            context.TODO(), targetNode.Name, metav1.GetOptions{})
        return hasStorageUnhealthyCondition(node)
    }, time.Minute*2, time.Second*10).Should(BeFalse())
    
    By("Verifying no fencing was triggered")
    sbrList := &unstructured.UnstructuredList{}
    sbrList.SetGroupVersionKind(/* SBR GVK */)
    err := APIClient.List(context.TODO(), sbrList, 
        client.InNamespace(medik8sparams.OperatorNs))
    Expect(err).ToNot(HaveOccurred())
    
    for _, sbr := range sbrList.Items {
        if sbr.GetName() == targetNode.Name {
            Fail("SBR CR should not exist for transient failure")
        }
    }
    
    By("Verifying heartbeat writes resumed successfully")
    // Check sequence numbers continue incrementing
}

func createCephNetworkDisruptorPod(nodeName string) *pod.Builder {
    // Create privileged pod that:
    // 1. Blocks Ceph ports with iptables
    // 2. Sleeps for 15 seconds
    // 3. Removes iptables rules
    // 4. Exits
    // Similar to existing storage disruption patterns in transient_storage.go
}
```

**Why Ceph RBD Specific**:
- Uses Ceph-specific ports (3300 for newer versions, 6789 for monitors)
- RBD kernel module behavior differs from filesystem mounts
- Network partition scenarios common in ODF/Ceph deployments

**Expected Outcome**:
- ✅ Transient disruption detected
- ✅ Condition clears after restoration
- ❌ No fencing triggered
- ✅ Normal operations resume
- ✅ Other nodes unaffected

**Storage Class**:
- **Auto-discovered**: Ceph RBD
- **Override**: Set `SBR_RBD_STORAGE_CLASS` env var
- **Requirement**: CephFS-specific for port-based disruption

---

---

## Implementation Guide for rhwa-system-tests

### File Structure

**New Test File**: `tests/sbr-operator/tests/block_storage_rbd.go`

```go
package tests

import (
    "context"
    "fmt"
    "os"
    "strings"
    "time"

    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"
    
    "github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
    "github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
    
    "github.com/medik8s/system-tests/tests/internal/labels"
    . "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
    "github.com/medik8s/system-tests/tests/internal/medik8sparams"
    "github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
    
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe(
    "SBR Block Storage — Ceph RBD",
    Ordered,
    ContinueOnFailure,
    Label(labels.OperatorSBR, labels.ComponentAgent), func() {
        
        // Tests go here
    })
```

### Helper Functions to Add

**Location**: `tests/sbr-operator/tests/block_storage_rbd_helpers.go`

```go
package tests

// discoverCephRBDStorageClass returns Ceph RBD block storage class name
func discoverCephRBDStorageClass() string {
    if envClass := os.Getenv("SBR_RBD_STORAGE_CLASS"); envClass != "" {
        return envClass
    }
    
    scList, err := APIClient.StorageV1Interface.StorageClasses().List(
        context.TODO(), metav1.ListOptions{})
    Expect(err).ToNot(HaveOccurred(), "Failed to list StorageClasses")
    
    for idx := range scList.Items {
        provisioner := scList.Items[idx].Provisioner
        // ODF Ceph RBD provisioner
        if strings.Contains(provisioner, "rbd.csi.ceph.com") {
            GinkgoWriter.Printf("Auto-discovered Ceph RBD StorageClass: %s (provisioner: %s)\n",
                scList.Items[idx].Name, provisioner)
            return scList.Items[idx].Name
        }
    }
    
    Skip("No Ceph RBD StorageClass found; install ODF or set SBR_RBD_STORAGE_CLASS")
    return ""
}

// getCephRBDMonitorPorts returns Ceph monitor ports
func getCephRBDMonitorPorts() []int {
    return []int{3300, 6789}  // v2: 3300, v1: 6789
}

// getCephRBDOSDPortRange returns Ceph OSD port range
func getCephRBDOSDPortRange() (int, int) {
    return 6800, 7300
}

// hasStorageUnhealthyCondition checks for SBRStorageUnhealthy condition
func hasStorageUnhealthyCondition(node *corev1.Node) bool {
    for _, condition := range node.Status.Conditions {
        if condition.Type == "SBRStorageUnhealthy" && 
           condition.Status == corev1.ConditionTrue {
            return true
        }
    }
    return false
}
```

### Constants to Add to sbrparams

**Location**: `tests/sbr-operator/internal/sbrparams/const.go`

```go
const (
    // Block storage test names
    SBRCBlockTestName           = "test-sbr-block-rbd"
    SBRCBlockInspectionTestName = "test-sbr-block-inspection"
    SBRCBlockPerfTestName       = "test-sbr-block-perf"
)
```

### Environment Variables

```bash
# Optional: Override auto-discovery
export SBR_RBD_STORAGE_CLASS="ocs-storagecluster-ceph-rbd"
```

### Expected ODF Storage Class

The tests expect a Ceph RBD StorageClass from ODF:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ocs-storagecluster-ceph-rbd
provisioner: openshift-storage.rbd.csi.ceph.com
parameters:
  clusterID: openshift-storage
  pool: ocs-storagecluster-cephblockpool
  imageFeatures: layering
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: Immediate
```

---

## Success Criteria

| Test Case | Polarion ID | Must Pass | Acceptable Skip Conditions |
|-----------|-------------|-----------|----------------------------|
| Ceph RBD Multi-Writer Concurrency | TBD | ✅ Yes | No Ceph RBD storage available |
| Ceph RBD Device Inspection | TBD | ✅ Yes | No Ceph RBD storage available |
| Ceph RBD Persistent Fencing State | TBD | ⚠️ Optional | Requires destructive node reboot |
| Ceph RBD Heartbeat Performance | TBD | ⚠️ Optional | Extended test, weekly frequency |
| Ceph RBD Network Partition Recovery | TBD | ✅ Yes | No Ceph RBD storage available |

**Note**: Polarion IDs should be assigned by the QE team when test cases are officially added to the test plan.

---

## Expected Benefits

1. **Fill Coverage Gap**: rhwa-system-tests currently has no dedicated Ceph RBD block storage tests
2. **ODF Validation**: Target most common OpenShift block storage deployment (ODF Ceph RBD)
3. **Production Readiness**: Validate real-world scenarios on OpenShift (network partitions, reboots, concurrent access)
4. **Debug Capability**: Enable block-mode inspection tools for production troubleshooting
5. **Performance Baseline**: Establish acceptable latency metrics for Ceph RBD block storage

---

## Integration with OpenShift CI

### Add to CI Configuration

**Location**: `.ci-operator/config/medik8s/rhwa-system-tests/medik8s-rhwa-system-tests-<branch>.yaml`

```yaml
tests:
- as: e2e-sbr-block-rbd-aws-odf
  cluster_claim:
    architecture: amd64
    cloud: aws
    owner: openshift-ci
    product: ocp
    timeout: 1h0m0s
    version: "4.18"
  steps:
    test:
    - as: install-odf
      cli: latest
      commands: |
        # Install ODF operator and create StorageSystem
        # (Reuse existing ODF installation steps from other tests)
      from: cli
      resources:
        requests:
          cpu: 100m
          memory: 200Mi
    - as: e2e-test
      cli: latest
      commands: |
        # Run only Ceph RBD block storage tests
        export SBR_RBD_STORAGE_CLASS="ocs-storagecluster-ceph-rbd"
        export GINKGO_LABEL_FILTER="labels.ComponentAgent && labels.OperatorSBR"
        make test-sbr-block-rbd
      from: src
      resources:
        requests:
          cpu: 100m
          memory: 200Mi
  timeout: 2h0m0s
```

### Makefile Target

**Location**: `Makefile`

```makefile
.PHONY: test-sbr-block-rbd
test-sbr-block-rbd: ## Run SBR Ceph RBD block storage tests
	@echo "Running SBR Ceph RBD block storage tests..."
	$(GINKGO) $(GINKGO_OPTIONS) \
		--label-filter='labels.OperatorSBR && labels.ComponentAgent' \
		--focus='Ceph RBD' \
		./tests/sbr-operator/tests/
```

---

## Proposed Ceph RBD Block Storage Tests - Storage Requirements Summary

### New Block Storage Test Requirements

| Test Category | Test Name | Polarion ID | Storage Required | StorageClass Type | Volume Mode | Disruption Level |
|---------------|-----------|-------------|------------------|-------------------|-------------|------------------|
| Block storage concurrency | Verify SBR agents write concurrently to Ceph RBD block device without conflicts | TBD | ✅ Yes | RWX Ceph RBD (auto-discovered) | Block | Non-destructive |
| Block storage inspection | Verify SBR device inspection works on Ceph RBD block storage | TBD | ✅ Yes | RWX Ceph RBD (auto-discovered) | Block | Non-destructive |
| Block storage persistence | Verify fencing state persists on Ceph RBD block storage across node reboot | TBD | ✅ Yes | RWX Ceph RBD (auto-discovered) | Block | **Destructive** (node reboot) |
| Block storage performance | Verify SBR heartbeats remain stable on Ceph RBD under load | TBD | ✅ Yes | RWX Ceph RBD (auto-discovered) | Block | Non-destructive |
| Block storage network resilience | Verify SBR recovers from transient Ceph RBD network partition without fencing | TBD | ✅ Yes | RWX Ceph RBD (auto-discovered) | Block | **Destructive** (network disruption) |

### Storage Class Auto-Discovery for Block Tests

**Primary Discovery Method**: `discoverCephRBDStorageClass()`

```go
// Auto-discovers Ceph RBD StorageClass by provisioner
for _, sc := range storageClasses {
    if strings.Contains(sc.Provisioner, "rbd.csi.ceph.com") {
        return sc.Name  // e.g., "ocs-storagecluster-ceph-rbd"
    }
}
```

**Override**: Set `SBR_RBD_STORAGE_CLASS` environment variable

```bash
export SBR_RBD_STORAGE_CLASS="ocs-storagecluster-ceph-rbd"
```

### Ceph RBD Specific Requirements

| Requirement | CephFS Tests | Ceph RBD Block Tests |
|-------------|--------------|----------------------|
| **Volume Mode** | Filesystem | **Block** |
| **Provisioner Pattern** | `*cephfs*` | `*rbd.csi.ceph.com*` |
| **Access Mode** | ReadWriteMany (RWX) | ReadWriteMany (RWX) |
| **Network Disruption Ports** | 6789, 6800-7300 (CephFS) | 3300, 6789, 6800-7300 (Ceph RBD) |
| **Storage Class Example** | `ocs-storagecluster-cephfs` | `ocs-storagecluster-ceph-rbd` |
| **Multi-attach Support** | Yes (filesystem-based) | Yes (block-based, true RWX) |
| **Inspection Commands** | Works | **New** - validates block device inspection |
| **Current Coverage** | ✅ Full coverage (15+ tests) | ❌ **Gap** - 0 dedicated tests |

### Test Labels and Frequency

| Test | Labels | Frequency | CI Job Recommendation |
|------|--------|-----------|----------------------|
| Ceph RBD Multi-Writer Concurrency | `OperatorSBR`, `ComponentAgent`, `TierAcceptance`, `DisruptionNonDestructive`, `FrequencyNightly` | Nightly | `e2e-sbr-block-rbd-aws-odf` |
| Ceph RBD Device Inspection | `OperatorSBR`, `ComponentAgent`, `TierAcceptance`, `DisruptionNonDestructive`, `FrequencyNightly` | Nightly | `e2e-sbr-block-rbd-aws-odf` |
| Ceph RBD Persistent Fencing State | `OperatorSBR`, `ComponentRemediation`, `TierAcceptance`, `DisruptionDestructive`, `FrequencyWeekly` | Weekly | `e2e-sbr-block-rbd-destructive` |
| Ceph RBD Heartbeat Performance | `OperatorSBR`, `ComponentAgent`, `TierExtended`, `DisruptionNonDestructive`, `FrequencyWeekly` | Weekly | `e2e-sbr-block-rbd-extended` |
| Ceph RBD Network Partition Recovery | `OperatorSBR`, `ComponentAgent`, `TierAcceptance`, `DisruptionDestructive`, `FrequencyNightly` | Nightly | `e2e-sbr-block-rbd-aws-odf` |

### Expected ODF StorageClass for Block Tests

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ocs-storagecluster-ceph-rbd
provisioner: openshift-storage.rbd.csi.ceph.com
parameters:
  clusterID: openshift-storage
  pool: ocs-storagecluster-cephblockpool
  imageFeatures: layering
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: Immediate
```

### Skip Conditions

All proposed Ceph RBD block storage tests will skip if:
- ❌ No Ceph RBD StorageClass found (provisioner must contain `rbd.csi.ceph.com`)
- ❌ `SBR_RBD_STORAGE_CLASS` env var points to non-RBD storage
- ❌ ODF/Ceph not installed on cluster

**Skip Message Example**:
```
No Ceph RBD StorageClass found; install ODF or set SBR_RBD_STORAGE_CLASS
```

### Coverage Comparison

| Aspect | Existing CephFS Tests | Proposed Ceph RBD Tests |
|--------|----------------------|-------------------------|
| **Volume Mode** | Filesystem | **Block** |
| **Test Count** | 15+ tests | **5 new tests** |
| **Concurrent Access** | Implicit (filesystem semantics) | **Explicit multi-writer validation** |
| **Device Inspection** | N/A (filesystem) | **Block device slot inspection** |
| **Fencing Persistence** | Covered | **Block-specific persistence validation** |
| **Performance Testing** | Not covered | **Heartbeat latency under load** |
| **Network Disruption** | ✅ Port-based (88880, 88735) | **RBD-specific port blocking** |
| **Storage Discovery** | `discoverRWXStorageClass()` (cephfs) | **`discoverCephRBDStorageClass()` (rbd)** |

### Benefits of Adding Ceph RBD Block Tests

1. **Close Coverage Gap**: rhwa-system-tests currently has 0 Ceph RBD block storage tests
2. **ODF Validation**: Most common OpenShift storage deployment uses Ceph RBD for block
3. **Block vs Filesystem**: Different I/O paths, locking semantics, and failure modes
4. **Production Parity**: Validates SBR on both CephFS (current) and Ceph RBD (new)
5. **Debug Enablement**: Block device inspection tools currently skip for block mode
