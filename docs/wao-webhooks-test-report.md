# WAO admission webhooks: test run report

Date of the runs: 2026-11-14. Plan: [wao-webhooks-test-plan.md](wao-webhooks-test-plan.md).

## 1. Result

| Item | Value |
| --- | --- |
| Specs that ran | 20 (7 new WAO specs, 13 existing specs of the operator suites) |
| Passed | 20 |
| Failed | 0 |
| Remediations and maintenances that the runs started | 0 |
| Node state after the runs | 6 of 6 nodes `Ready`, none unschedulable |
| New WAO concerns | 4 (WAO issues 31 to 34). Issue 35 is the tracker of the open items |

## 2. Environment

| Item | Value |
| --- | --- |
| Cluster | OpenShift 5.0.0-rc.7 (Kubernetes 1.36), AWS, 3 control plane nodes and 3 workers, OVN-Kubernetes |
| WAO | v0.1.0, built from branch `feat/webhooks/snr` at commit `4fb904d` with `make dev-olm-deploy` (classic OLM) |
| Operand images | The tag `wao` of each operand repository in `quay.io/medik8s` |
| WAO replicas | 2 |
| System tests | Branch `wao-webhooks`, started from `wao` at `3cc98f50` |
| Start state | No WAO on the cluster. After the install, each operand was `Disabled` |

## 3. What was implemented

### 3.1 New specs (WAO suite)

| Ticket (placeholder) | Spec file | Requests | Stored objects |
| --- | --- | --- | --- |
| RHWA-9001 | `webhook_registration.go` | Reads only | None |
| RHWA-9002 | `webhook_far.go` | 23 dry-run requests | One template and one Secret with a test value, while FAR is `Disabled` |
| RHWA-9003 | `webhook_nhc.go` | 31 dry-run requests | Two SNR templates and one paused `NodeHealthCheck`, while NHC is `Disabled` |
| RHWA-9004 | `webhook_nmo.go` | 9 dry-run requests | One `NodeMaintenance`, while NMO is `Disabled` |
| RHWA-9005 | `webhook_snr.go` | 33 dry-run requests | None |
| RHWA-9006 | `webhook_resilience.go` | 21 dry-run requests | One `NetworkPolicy` for approximately 85 seconds |
| RHWA-9007 | `webhook_resilience.go` | 15 dry-run requests | None. Deletes the 2 WAO manager Pods one time |

Shared code:

- `tests/wao-operator/tests/webhook_helpers.go`: the request helpers, the warning recorder and the operand state sequence.
- `tests/wao-operator/internal/waoparams/webhooks.go`: the ten expected entries and the timeouts.
- `tests/internal/helpers/wao.go`: `EnsureWAOComponentsAvailable` and `WAOComponentAdmissionReady`, with unit tests.

### 3.2 Changes to existing specs

| Change | Files |
| --- | --- |
| WAO precondition in `BeforeAll` (enable the operand, wait for `Available`). No change to a test body | `far_negative_validation.go`, `nhc_negative_validation.go`, `nhc_escalation_validation.go`, `nmo_validation.go`, `crd_negative.go` |
| `list` grant for the WAO ServiceAccount on the test template kind of spec 74932 | `tests/nhc-operator/tests/nhc_helpers.go` |
| Label `component:webhook` added to specs 50961 and 47330 | `tests/snr-operator/tests/crd_negative.go` |

### 3.3 Tools and documents

| Item | File |
| --- | --- |
| Run script and Makefile target | `scripts/run-webhook-tests.sh`, `make run-webhook-tests` |
| Watch script | `tools/watch/webhook-watch.sh` |
| Seven test case documents | `docs/TESTCASES/rhwa-9001.md` to `rhwa-9007.md` |
| Suite catalog | `tests/wao-operator/README.md` |
| Convention correction | `docs/TESTCASES/AGENTS.md` (admission stays active while a central operand is `Disabled`) |
| Plan and this report | `docs/wao-webhooks-test-plan.md`, `docs/wao-webhooks-test-report.md` |

## 4. Runs

Each run used `make run-webhook-tests`. The selection excludes destructive specs.

| # | Command | Suites | Specs | Result | Time of the suites |
| --- | --- | --- | --- | --- | --- |
| 1 | `WEBHOOK_TARGET=far` | far, wao | 3 + 1 | Passed | 11 s + 17 s |
| 2 | `WEBHOOK_TARGET=nmo` | nmo, wao | 1 + 1 | Passed | 16 s + 21 s |
| 3 | `WEBHOOK_TARGET=snr` | snr, wao | 2 + 1 | Passed | 10 s + 27 s |
| 4 | `WEBHOOK_TARGET=nhc` | nhc, wao | 7 + 1 | Passed | 36 s + 19 s |
| 5 | `WEBHOOK_TARGET=wao` | wao | 7 | Passed | 176 s |
| 6 | `WEBHOOK_TARGET=all` | far, nhc, nmo, snr, wao | 3 + 7 + 1 + 2 + 7 = 20 | Passed | 0.7 s + 29 s + 0.5 s + 0.3 s + 176 s |

Before these runs, each new spec ran one time alone with each operand `Disabled` (the state after the install). Each passed, with one exception that section 6.1 describes.

`make fmt`, `make lint`, `make test` and `make check-testcase` pass on the branch.

### 4.1 Existing specs that ran

| Suite | Spec IDs | Result |
| --- | --- | --- |
| FAR | 66090, 71219, 71220 | Passed |
| NHC | 53769, 51626, 61591, 60863, 60862, 66838, 74932 | Passed |
| NMO | 29598 | Passed |
| SNR | 50961, 47330 | Passed |

### 4.2 Specs that did not run

| Spec | Reason |
| --- | --- |
| NHC 60865, NHC 56600 | Destructive (kubelet stop). The new spec RHWA-9003 checks the same rules with a test status |
| NMO 29630, 29632 | Destructive (worker drain). RHWA-9004 checks the rule of 29630 without a drain |
| NMO 46790 | Destructive (control plane node drain). The etcd quorum rejection has no run under WAO |
| RHWA-2040 | Not a webhook spec |
| Upgrade, CSV and SBR specs | Excluded by the task |

## 5. How admission behaved

### 5.1 Registration (RHWA-9001)

- OLM created ten configuration objects, one for each entry. Each calls `workload-availability-operator-controller-manager-service`.
- Nine entries have `Fail`. `vselfnoderemediation.kb.io` has `Ignore`. `vnodemaintenance.kb.io` has 15 seconds, the others 10.
- Each entry has a CA bundle, one rule and `sideEffects: None`.
- With the four operands `Enabled`, no operand registered a second entry for the same resources.
- `admissionReady` was `true` for FAR, NHC, NMO and SNR in each operand state.

### 5.2 Rules (RHWA-9002 to RHWA-9005)

Each rule of section 1.2 of the plan gave the same answer with the operand `Disabled` and `Enabled`.

| Operand | Rules with live evidence now | Notes |
| --- | --- | --- |
| FAR | FAR-1 to FAR-10 | The agent inventory rejected `fence_incorrect` and accepted `fence_ipmilan`. The WAO ServiceAccount read the legacy Secret and a named Secret |
| NHC | NHC-1 to NHC-11 | Two SNR templates got the annotation from the SNR defaulter of WAO. Then NHC admission admitted two steps of the SNR kind. Delete and two update rules were rejected with a test status. A `pauseRequests` update passed |
| NMO | NMO-1, NMO-2, NMO-4, NMO-3 permitted case | A `NodeMaintenance` stored while NMO was `Disabled` caused no cordon. The control plane request was admitted, so the etcd reads are permitted |
| SNR | SNR-1 to SNR-10 | The two warnings arrived in the answer. A dry-run delete of the default config changed nothing |

### 5.3 Failure policy (RHWA-9006)

With a `NetworkPolicy` that denies ingress to the WAO manager Pods:

| Kind | Answer | Time |
| --- | --- | --- |
| `FenceAgentsRemediation`, `FenceAgentsRemediationTemplate`, `NodeHealthCheck`, `SelfNodeRemediationConfig`, `SelfNodeRemediationTemplate` | Internal error, `failed calling webhook "<entry>"` | 10 s |
| `NodeMaintenance` | Internal error, `admission plugin "ValidatingAdmissionWebhook" failed to complete validation in 13s` | 13 s |
| `SelfNodeRemediation` | Admitted | 10 s |

The WAO Pods stayed `Ready`, and `admissionReady` stayed `true` (it reports the local listener).
Admission answered again less than 1 second after the policy was deleted.

### 5.4 Restart (RHWA-9007)

The Deployment replaced the 2 Pods and admission answered again in approximately 16 seconds.
The FAR image was in the node cache. The agent rule gave the correct answer after the restart.

## 6. Findings

### 6.1 Found by the tests

| # | Finding | Severity | Tracker |
| --- | --- | --- | --- |
| 1 | A `FenceAgentsRemediation` create fails when no WAO replica answers. A WAO outage thus stops the start of each FAR remediation, also with a healthy FAR manager | Decision necessary | WAO issue 31 |
| 2 | The 15 second timeout of the NMO entry is longer than the 13 seconds that the API server gives to the validating phase. The error then does not name the webhook. The first run of RHWA-9006 failed on this text. The spec now accepts the two texts | Low | WAO issue 32 |
| 3 | WAO did not report a `NodeHealthCheck` that was created while the phase was `Disabled`. The status had no retained resource and no D1 message after 120 seconds. The report appeared after an annotation change on the configuration | Medium | WAO issue 33 |
| 4 | After SNR is `Disabled` and its config is deleted, the default `SelfNodeRemediationTemplate` stays, and `retainedResources` does not list it | Low | WAO issue 34 |

### 6.2 Confirmed for existing issues

| Issue | Evidence |
| --- | --- |
| 10 | `oc auth can-i list` for the test template kind as the WAO ServiceAccount gives `no`. Spec 74932 passed with the grant that the branch adds. A run without the grant was not done |
| 15 | NHC UPDATE and DELETE rules have live evidence, with a test status |
| 23 | NMO rules 2 and 4 have live evidence. The etcd quorum rejection has none |
| 24, 30 | The `Ignore` entry admits a request when the listener does not answer, after a delay of 10 seconds for each request |

### 6.3 Open items of the tests

- The Jira tickets do not exist. RHWA-9001 to RHWA-9007 are placeholder numbers. `make check-testcase-jira` fails until they are replaced.
- The `Author` row of the documents has `jmatthews`. Check it.
- The existing NHC spec 74932 leaves the CRD `multitemplateremediationtemplates.test.medik8s.io` on the cluster. That was so before this branch.
- The branch is local. It is not pushed.

## 7. Cluster state after the work

- WAO is installed. Each operand is `Disabled`, with the phase `Disabled`.
- The namespace holds the WAO Deployment (2 of 2 ready) and the OLM bundle Pod only.
- No test object remains (`oc get nodehealthcheck,nodemaintenance` is empty; no template, no test Secret, no test `NetworkPolicy`).
- The operator and bundle images are on ttl.sh with a lifetime of 24 hours from 2026-11-14 02:07 UTC. After that time a WAO Pod restart cannot pull the image. Deploy again then.

## 8. How to run the tests again

### 8.1 Install WAO (only if it is not on the cluster)

```bash
cd <workload-availability-operator checkout, branch feat/webhooks/snr>
KUBECONFIG=/path/to/kubeconfig GOTOOLCHAIN=go1.26.5 TTL_SH_TTL=24h make dev-olm-deploy
GOTOOLCHAIN=go1.26.5 make bundle-reset     # the deploy changes bundle/ and config/manager/kustomization.yaml
```

Uninstall: `KUBECONFIG=/path/to/kubeconfig make dev-olm-undeploy`.

### 8.2 Run the webhook tests

```bash
cd <rhwa-system-tests checkout, branch wao-webhooks>
export KUBECONFIG=/path/to/kubeconfig
export WORKLOAD_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest
export ECO_TEST_VERBOSE=true              # optional: print each step
export ECO_REPORTS_DUMP_DIR=/tmp/reports  # optional: JUnit and failure dumps

make run-webhook-tests                      # all webhook tests: far, nhc, nmo, snr and wao suites
make run-webhook-tests WEBHOOK_TARGET=wao   # the seven WAO specs only
make run-webhook-tests WEBHOOK_TARGET=far   # FAR suite webhook specs and RHWA-9002
make run-webhook-tests WEBHOOK_TARGET=nhc   # NHC suite webhook specs and RHWA-9003
make run-webhook-tests WEBHOOK_TARGET=nmo   # NMO suite webhook specs and RHWA-9004
make run-webhook-tests WEBHOOK_TARGET=snr   # SNR suite webhook specs and RHWA-9005
```

One spec:

```bash
bin/ginkgo/*/ginkgo -v --focus="RHWA-9006" ./tests/wao-operator/...
```

`WEBHOOK_DESTRUCTIVE=true` adds the destructive webhook specs (kubelet stop, node drain). They were not run under WAO, and their suites have no WAO precondition.

### 8.3 Watch a run

```bash
watch -n 5 -d tools/watch/webhook-watch.sh
```

### 8.4 State that the runs leave

- The operator suite specs enable their operands and leave them enabled (repository convention). After `make run-webhook-tests`, FAR, NHC, NMO and SNR are `Enabled`.
- The WAO specs restore the operand state that they find.
- To return to the install state:

```bash
oc patch workloadavailabilityconfig cluster --type=merge -p \
  '{"spec":{"components":{"nodeHealthCheck":{"enabled":false},"selfNodeRemediation":{"enabled":false},"fenceAgentsRemediation":{"enabled":false},"nodeMaintenance":{"enabled":false}}}}'
oc -n openshift-workload-availability delete selfnoderemediationconfig --all
oc -n openshift-workload-availability delete selfnoderemediationtemplate --all
```

### 8.5 Recovery after an interrupted run

```bash
oc -n openshift-workload-availability delete networkpolicy wao-webhook-test-deny-admission --ignore-not-found
oc delete nodemaintenance wao-webhook-test-nm --ignore-not-found
oc patch nodehealthcheck wao-webhook-test-nhc --subresource=status --type=merge -p '{"status":{"unhealthyNodes":null}}'
oc delete nodehealthcheck wao-webhook-test-nhc --ignore-not-found
oc -n openshift-workload-availability delete secret fence-agents-credentials-shared -l wao-webhook-test=true
```

Delete the `NodeMaintenance` before NMO is enabled. An enabled NMO manager drains the node of a stored `NodeMaintenance`.
