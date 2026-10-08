# SBR Test Documentation

This directory contains detailed design documentation for Storage-Based Remediation (SBR) tests.

## Available Test Designs

### [Ceph RBD Heartbeat Performance Under Load](ceph-rbd-heartbeat-performance-under-load-test-design.md)

**Status**: ✅ Implemented  
**Test Files**:
- `tests/sbr-operator/tests/block_storage_rbd_performance.go` - Main test implementation
- `tests/sbr-operator/tests/block_storage_rbd_performance_load.go` - Load generation helpers
- `tests/sbr-operator/tests/block_storage_rbd_performance_metrics_simple.go` - Metrics collection
- `tests/sbr-operator/tests/block_storage_rbd_helpers.go` - Ceph RBD storage helpers

**Purpose**: Validates SBR heartbeat mechanism performance on Ceph RBD block storage under sustained system load, ensuring no false-positive fencing occurs.

**Key Features**:
- 5-phase test: Setup → Baseline → Load → Recovery → Validation
- CPU, memory, and I/O load injection via stress-ng
- Prometheus metrics scraping from agent pods
- Statistical analysis (p50, p95, p99 latency)
- JSON performance report generation
- Sequence number gap detection

**Running the Test**:
```bash
# Set Ceph RBD StorageClass (optional - auto-discovered)
export SBR_RBD_STORAGE_CLASS="ocs-storagecluster-ceph-rbd"

# Run the performance test
ginkgo -v \
  --focus="Ceph RBD.*heartbeat.*performance" \
  --timeout=30m \
  ./tests/sbr-operator/tests/

# Check artifacts
ls -la ${ARTIFACT_DIR}/sbr-heartbeat-performance-*.json
```

**Requirements**:
- OpenShift cluster with ODF (Ceph RBD)
- At least 2 worker nodes
- Ceph RBD StorageClass with `volumeMode: Block`
- ~15 minutes runtime

**Expected Results**:
- Baseline P99 latency < 100ms
- Under-load P99 latency < 500ms
- No SBRStorageUnhealthy false positives
- Error rate < 1%
- Sequence gaps < 5

## Test Design Process

1. **Analysis**: Review existing test coverage and identify gaps
2. **Design**: Create detailed design document (markdown)
3. **Implementation**: Implement test according to design
4. **Validation**: Run test in CI and verify results
5. **Documentation**: Update README with usage instructions

## Related Documentation

- [Main SBR Test Analysis](file:///home/whayutin/RHWA/CI/SBR/sbr_test_analysis.md)
- [SBR Upstream Repository](https://github.com/medik8s/storage-based-remediation)
- [ODF Documentation](https://access.redhat.com/documentation/en-us/red_hat_openshift_data_foundation/)
