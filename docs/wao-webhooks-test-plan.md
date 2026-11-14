# WAO admission webhooks: live-cluster test plan

Date: 2026-11-14. Branch: `wao-webhooks`, started from `wao`.
WAO source under test: branch `feat/webhooks/snr` of the workload-availability-operator (WAO).

This document records four things:

1. The admission webhooks that WAO serves, and each rule that they apply.
2. The webhook tests that this repository has now, and how they apply to WAO.
3. The gaps, and the new tests that close them.
4. The conventions for the test case documents, the labels and the run commands.

The run results are in [wao-webhooks-test-report.md](wao-webhooks-test-report.md).

## 1. What changed in WAO

Before WAO, each operator served its own admission webhooks from its manager Pod.
On the WAO branch `feat/webhooks/snr`, the WAO manager serves the webhooks of four operands: FAR, NHC, NMO and SNR.
This plan uses the term "central admission" for that design.

| Property | Standalone operator | WAO central admission |
| --- | --- | --- |
| Process that answers | Operator manager Pod | WAO manager Pod (2 replicas) |
| Service in `clientConfig` | Operator webhook Service | `workload-availability-operator-controller-manager-service` |
| Registration owner | The CSV of each operator | The WAO CSV (`webhookdefinitions`) |
| Present while the operand is `Disabled` | Not applicable | Yes. The entries stay for the life of WAO |
| Client for cluster reads | Manager cache | A dedicated client without a cache |
| FAR agent check | `os.Stat` in the FAR image | A name list that an init container collects from the FAR image |
| NHC template list permission | Aggregated ClusterRole | Fixed `list` permission on the four template kinds of the WAO bundle |

SBR admission is not central. The SBR operand serves its own webhooks. MDR has no webhooks. Both are out of scope.

### 1.1 Webhook entries

WAO registers ten entries. OLM creates one configuration object for each entry, with a generated name suffix.

| # | Entry | Type | Resource | Operations | `failurePolicy` | `timeoutSeconds` |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | `mfenceagentsremediation.kb.io` | Mutating | `fenceagentsremediations` | CREATE, UPDATE | Fail | 10 |
| 2 | `mfenceagentsremediationtemplate.kb.io` | Mutating | `fenceagentsremediationtemplates` | CREATE, UPDATE | Fail | 10 |
| 3 | `mselfnoderemediationtemplate.kb.io` | Mutating | `selfnoderemediationtemplates` | CREATE, UPDATE | Fail | 10 |
| 4 | `vfenceagentsremediation.kb.io` | Validating | `fenceagentsremediations` | CREATE, UPDATE | Fail | 10 |
| 5 | `vfenceagentsremediationtemplate.kb.io` | Validating | `fenceagentsremediationtemplates` | CREATE, UPDATE | Fail | 10 |
| 6 | `vnodehealthcheck.kb.io` | Validating | `nodehealthchecks` | CREATE, UPDATE, DELETE | Fail | 10 |
| 7 | `vnodemaintenance.kb.io` | Validating | `nodemaintenances` | CREATE, UPDATE | Fail | 15 |
| 8 | `vselfnoderemediation.kb.io` | Validating | `selfnoderemediations` | CREATE, UPDATE | Ignore | 10 |
| 9 | `vselfnoderemediationconfig.kb.io` | Validating | `selfnoderemediationconfigs` | CREATE, UPDATE, DELETE | Fail | 10 |
| 10 | `vselfnoderemediationtemplate.kb.io` | Validating | `selfnoderemediationtemplates` | CREATE, UPDATE | Fail | 10 |

### 1.2 Rules of each handler

The "Method" column gives the least disruptive method that can show the rule on a live cluster.

- **Dry-run**: a server-side dry-run request. The API server calls the webhooks and stores nothing.
- **Stored, Disabled**: a real object, created while the operand is `Disabled`. No controller acts on the object, so no node changes.
- **Destructive**: the rule needs a real maintenance or a real remediation.

FAR (`FenceAgentsRemediation` and `FenceAgentsRemediationTemplate`):

| ID | Rule | Method |
| --- | --- | --- |
| FAR-1 | A valid object is admitted | Dry-run |
| FAR-2 | `--action` accepts only an empty value, `reboot` or `off` | Dry-run |
| FAR-3 | The agent must be in the agent inventory of the FAR image | Dry-run |
| FAR-4 | Shared parameters only, without a `{{.NodeName}}` template, are rejected | Dry-run |
| FAR-5 | A named Secret that does not exist is rejected | Dry-run |
| FAR-6 | A parameter in the object and in the Secret is rejected | Dry-run with a test Secret |
| FAR-7 | The template defaulter adds the annotation `multiple-templates-support: "true"` when it is absent, and keeps an existing value | Dry-run |
| FAR-8 | CREATE sets the legacy Secret name when the legacy Secret exists | Dry-run with a test Secret |
| FAR-9 | A template UPDATE that removes the legacy Secret name fails while the legacy Secret exists | Stored template, dry-run update |
| FAR-10 | A template UPDATE applies the same rules as CREATE | Stored template, dry-run update |

NHC (`NodeHealthCheck`):

| ID | Rule | Method |
| --- | --- | --- |
| NHC-1 | A valid object is admitted (this includes the control plane topology check) | Dry-run |
| NHC-2 | Exactly one of `minHealthy` and `maxUnhealthy`, not negative | Dry-run |
| NHC-3 | `maxUnhealthy` is not permitted with `MachineDeletionRemediationTemplate` | Dry-run |
| NHC-4 | The selector is mandatory and must parse | Dry-run |
| NHC-5 | Exactly one of `remediationTemplate` and `escalatingRemediations` | Dry-run |
| NHC-6 | Escalation `order` values are unique | Dry-run |
| NHC-7 | Escalation `timeout` is 1 minute or more | Dry-run |
| NHC-8 | A duplicate template kind is rejected, unless each template of the kind has the annotation | Dry-run, with two stored SNR templates for the permitted case |
| NHC-9 | UPDATE of the selector, the template or the escalation is rejected during a remediation | Stored, Disabled, with a test status |
| NHC-10 | DELETE is rejected during a remediation | Stored, Disabled, with a test status |
| NHC-11 | Other UPDATE requests pass during a remediation (`pauseRequests`) | Stored, Disabled, with a test status |

NMO (`NodeMaintenance`):

| ID | Rule | Method |
| --- | --- | --- |
| NMO-1 | The node must exist | Dry-run |
| NMO-2 | No second `NodeMaintenance` for the same node | Stored, Disabled |
| NMO-3 | A control plane node is admitted only when etcd quorum permits | Dry-run for the permitted case. The rejection is destructive |
| NMO-4 | UPDATE of `spec.nodeName` is rejected | Stored, Disabled |

SNR (`SelfNodeRemediationConfig`, `SelfNodeRemediationTemplate`, `SelfNodeRemediation`):

| ID | Rule | Method |
| --- | --- | --- |
| SNR-1 | The config name must be `self-node-remediation-config` | Dry-run |
| SNR-2 | The config must be in the WAO namespace | Dry-run |
| SNR-3 | Each duration has a minimum value | Dry-run |
| SNR-4 | Custom tolerations: valid operator, valid effect, no value with `Exists` | Dry-run |
| SNR-5 | Custom node selector requirements: valid operator and values | Dry-run |
| SNR-6 | An unsafe `peerRequestTimeout` gives a warning, not a rejection | Dry-run |
| SNR-7 | DELETE of the default config gives a warning | Dry-run delete, SNR `Enabled` |
| SNR-8 | UPDATE of the config applies rules SNR-3 to SNR-6 | Dry-run update, SNR `Enabled` |
| SNR-9 | The template defaulter adds the annotation when it is absent, and keeps an existing value | Dry-run |
| SNR-10 | The `OutOfServiceTaint` strategy is admitted on Kubernetes 1.26 or later (template and remediation) | Dry-run |
| SNR-11 | The `SelfNodeRemediation` entry has `Ignore`: a request is admitted when the listener does not answer | Dry-run with a blocked listener |

## 2. Webhook tests that exist now

The `wao` branch has one WAO spec (RHWA-2040, operand lifecycle). It does not test admission.
The operator suites contain the webhook tests. All of them were written for the standalone operators.

### 2.1 Specs with the label `component:webhook`

`ginkgo --dry-run --label-filter='component:webhook' ./tests` selects 15 specs.

| ID | Suite and file | What it checks | Rule | Answer comes from | Disruption |
| --- | --- | --- | --- | --- | --- |
| 66090 | FAR `far_negative_validation.go` | FAR CR with `--action=status` is rejected | FAR-2 | Webhook | None |
| 71219 | FAR `far_negative_validation.go` | FAR CR with an agent that is not installed, and with a name without `fence_` | FAR-3 | Webhook, and CRD schema for the prefix | None |
| 71220 | FAR `far_negative_validation.go` | The same two checks for the template | FAR-3 | Webhook, and CRD schema for the prefix | None |
| 53769 | NHC `nhc_negative_validation.go` | A second NHC with the same name is rejected | None | API server (`AlreadyExists`) | None |
| 51626 | NHC `nhc_negative_validation.go` | Negative and text values for `minHealthy` and `duration` | None | CRD schema | None |
| 61591 | NHC `nhc_negative_validation.go` | An empty selector is rejected | NHC-4 | Webhook | None |
| 60863 | NHC `nhc_escalation_validation.go` | Missing `order` (schema), duplicate `order` (webhook), large `order` values admitted | NHC-6 | CRD schema and webhook | None |
| 60862 | NHC `nhc_escalation_validation.go` | Missing `timeout` (schema), `timeout` of 30s (webhook) | NHC-7 | CRD schema and webhook | None |
| 66838 | NHC `nhc_escalation_validation.go` | Two steps of the same test template kind are rejected | NHC-8 | Webhook | None |
| 74932 | NHC `nhc_escalation_validation.go` | Two steps of the same kind are admitted when each template has the annotation | NHC-8 | Webhook | None |
| 60865 | NHC `nhc_escalation_validation.go` | An escalation change is rejected during a remediation | NHC-9 | Webhook | Stops the kubelet of a worker |
| 29598 | NMO `nmo_validation.go` | A `NodeMaintenance` for a node that does not exist is rejected | NMO-1 | Webhook | None |
| 29632 | NMO `nmo_collision.go` | A second `NodeMaintenance` with the same name is rejected | None | API server (`AlreadyExists`) | Drains a worker |
| 29630 | NMO `nmo_collision.go` | A second `NodeMaintenance` for the same node is rejected | NMO-2 | Webhook | Drains a worker |
| 46790 | NMO `nmo_control_plane_quorum.go` | A second control plane maintenance is rejected for etcd quorum | NMO-3 | Webhook | Drains a control plane node |

### 2.2 Specs that depend on a webhook but do not have the label

| ID | Suite and file | What it checks | Rule | Note |
| --- | --- | --- | --- | --- |
| 50961 | SNR `crd_negative.go` | A config with a different name is rejected | SNR-1 | Label is `component:controller` |
| 47330 | SNR `crd_negative.go` | Text durations (schema) and durations below the minimum (webhook) | SNR-3 | Label is `component:controller` |
| 60877, 60822 | SNR `unsupported_strategy.go` | The `NodeDeletion` strategy is rejected | None | CRD schema only. No webhook |
| 56600 | NHC `nhc_remediation_trigger.go` | Selector change and delete are rejected during a remediation | NHC-9, NHC-10 | Destructive. No `component:webhook` label |
| 52834 | NMO `nmo_validation.go` | An integer `reason` and a malformed name | None | CRD schema and API server |

Other specs use a webhook only as a guard (for example a delete that retries until NHC admission permits it).
They do not assert a webhook rule, so this plan does not list them.

### 2.3 Rules with no test in this repository

Before this plan, these rules had no system test:

- FAR-1, FAR-4, FAR-5, FAR-6, FAR-7, FAR-8, FAR-9, FAR-10.
- NHC-2 (the webhook part), NHC-3, NHC-5, NHC-11, and the invalid-selector part of NHC-4.
- NMO-3 permitted case as an explicit check, NMO-4.
- SNR-2, SNR-4, SNR-5, SNR-6, SNR-7, SNR-8, SNR-9, SNR-10, SNR-11.
- Each mutating entry. No spec reads a defaulted field.
- Each UPDATE path, except NHC-9 (destructive).

## 3. How the existing tests apply to WAO

### 3.1 Applicable without a change to the test body

The test bodies of the 11 non-destructive specs in section 2.1, and of 50961 and 47330, apply to WAO as written.
They send a request and read the answer of the API server. They do not read which process answers.
The error texts are the same, because WAO uses the validators of the operators.

### 3.2 Changes that are necessary

| # | Change | Reason | Status in this branch |
| --- | --- | --- | --- |
| C1 | Add the WAO precondition to the `BeforeAll` of each webhook spec: enable the operand, then wait for `Available` | Each `BeforeAll` reads the operand Deployment. With a `Disabled` operand the Deployment does not exist, so the spec fails before the first request | Done for the five files in section 3.3 |
| C2 | Put the timeout and the poll interval for C1 in `medik8sparams` | `waoparams` is in an `internal` directory of the WAO suite. Go does not permit an import from a different suite | Done |
| C3 | 74932: give the `list` permission on the test template kind to the WAO ServiceAccount also | The test gives the permission to the NHC ServiceAccount. Under WAO, the WAO manager does the list (WAO decision D2, WAO issue 10) | Done |
| C4 | Add `component:webhook` to 50961 and 47330 | Without the label, a label selection of webhook tests does not include SNR | Done |
| C5 | Correct `docs/TESTCASES/AGENTS.md`: admission stays active while a central operand is `Disabled` | The text says that the webhooks are gone and that CRs are admitted without validation. That is not correct for FAR, NHC, NMO and SNR | Done |
| C6 | `waitForFARWebhookReady` is not necessary under WAO, but it is correct | `Available` includes `admissionReady`. The probe does no harm | No change |

### 3.3 Files that get the precondition (C1)

| File | Operands that the precondition enables |
| --- | --- |
| `tests/far-operator/tests/far_negative_validation.go` | FAR |
| `tests/nhc-operator/tests/nhc_negative_validation.go` | NHC, SNR |
| `tests/nhc-operator/tests/nhc_escalation_validation.go` (two containers) | NHC, SNR |
| `tests/nmo-operator/tests/nmo_validation.go` | NMO |
| `tests/snr-operator/tests/crd_negative.go` | SNR |

The precondition is `helpers.EnsureWAOComponentsAvailable`. It follows the contract in `docs/TESTCASES/AGENTS.md`:

1. It reads the singleton. If the singleton is absent, it does nothing (a cluster without WAO).
2. It changes nothing when each named operand is enabled and `Available`.
3. If not, it enables the named operands with one patch and waits for `Available`.

The destructive specs (60865, 29632, 29630, 46790) and the suites that are not about webhooks do not get the precondition in this branch.
That work belongs to the general WAO migration.

### 3.4 What the existing tests cannot show for WAO

The existing tests cannot show these WAO properties, because a standalone operator does not have them:

- The entries point at the WAO Service and no operand registers a second entry for the same resource.
- Admission answers while the operand is `Disabled`.
- Admission answers the same after the change from `Disabled` to `Enabled`.
- `status.components[].admissionReady`.
- The behavior of each `failurePolicy` when no WAO replica answers.
- Admission after a restart of the WAO Pods (this includes the FAR agent inventory).
- The permissions of the WAO ServiceAccount for admission reads (Secrets, Nodes, etcd PodDisruptionBudget, templates).

## 4. Gaps and new tests

### 4.1 New test cases

Each new spec is in `tests/wao-operator/tests/`. Each has one test case document.

| Ticket (placeholder) | Spec file | Title | Rules and WAO properties |
| --- | --- | --- | --- |
| RHWA-9001 | `webhook_registration.go` | WAO registers the ten admission entries and routes them to its Service | Section 1.1, `admissionReady`, endpoints, CA bundle, one entry for each resource |
| RHWA-9002 | `webhook_far.go` | WAO serves FAR admission while FAR is Disabled and Enabled | FAR-1 to FAR-10 |
| RHWA-9003 | `webhook_nhc.go` | WAO serves NHC admission while NHC is Disabled and Enabled | NHC-1 to NHC-11 |
| RHWA-9004 | `webhook_nmo.go` | WAO serves NMO admission while NMO is Disabled and Enabled | NMO-1, NMO-2, NMO-3 (permitted case), NMO-4 |
| RHWA-9005 | `webhook_snr.go` | WAO serves SNR admission while SNR is Disabled and Enabled | SNR-1 to SNR-10 |
| RHWA-9006 | `webhook_resilience.go` | Each entry obeys its failurePolicy when the WAO listener does not answer | SNR-11, and `Fail` for the other six kinds |
| RHWA-9007 | `webhook_resilience.go` | Admission returns after a restart of each WAO manager Pod | Restart, FAR agent inventory, `admissionReady` |

The ticket numbers are placeholders. See section 6.

### 4.2 Design of the operand specs (RHWA-9002 to RHWA-9005)

Each operand spec has the same sequence:

1. Record the state of the operand. Disable the operand if it is enabled. Wait until the manager Deployment is gone.
2. Check `admissionReady`.
3. Send the dry-run requests for each rule. Each request has one `By` step.
4. Do the steps that need a stored object (section 1.2, "Stored, Disabled"). Remove each stored object and wait until it is gone.
5. Enable the operand. Wait for `Available` and for the manager Deployment.
6. Send the dry-run requests again. Do the steps that need the operand (SNR-7, SNR-8).
7. Set the operand to the state of step 1.

These specs change `spec.components`, which the convention permits only for a spec whose subject is the enable or disable state.
That condition is met: the subject is admission in each of the two states.

Safety rules for the stored objects:

- A `NodeHealthCheck` is stored only while NHC is `Disabled`. It has a selector that matches no node, and a pause request.
- A `NodeMaintenance` is stored only while NMO is `Disabled`. The spec does not enable NMO before the object is gone. If the delete fails, the spec stops and keeps NMO `Disabled`.
- A `SelfNodeRemediationConfig` is not stored by the spec. The SNR manager creates the default config when SNR is enabled.
- The specs store no remediation request (`FenceAgentsRemediation`, `SelfNodeRemediation`). Those requests are dry-run only.
- Templates and one Secret with test values are stored. They cause no action.
- Each stored object has a name that starts with `wao-webhook-test`. A spec deletes an object with such a name from an earlier run, and waits until it is gone.
- The legacy FAR Secret has a fixed product name. A spec deletes it only if it has the label of the spec. If not, the spec stops and does not change the Secret.

### 4.3 Design of the failure policy spec (RHWA-9006)

The spec adds one `NetworkPolicy` that denies ingress to the WAO manager Pods.
The API server then gets no answer from the listener. The WAO Pods stay `Ready`, because the kubelet probe comes from the same node.
The spec does not change an object that OLM owns.

- For the six kinds with `Fail`, a dry-run create must fail with `failed calling webhook`.
- For `SelfNodeRemediation` (`Ignore`), a dry-run create must be admitted.
- The spec removes the policy and waits until admission answers again.

While the policy exists (approximately 2 minutes), no person and no controller can create or change FAR, NHC, NMO or SNR objects, except `SelfNodeRemediation`.
Thus the spec has the label `Serial`.

### 4.4 Gaps that this plan does not close

| Gap | Reason | Tracker |
| --- | --- | --- |
| NMO-3 rejection (etcd quorum) under WAO | Needs a control plane node in maintenance. Spec 46790 covers it, but its suite has no WAO precondition | WAO issue 23 |
| NHC-9 and NHC-10 with a real remediation under WAO | Destructive. The new spec uses a test status in place of a real remediation | WAO issue 15 |
| SBR admission | Not central in WAO | None |
| TLS profile of the listener | No observable assertion is defined | WAO issue 18 |
| Certificate rotation of the webhook serving certificate | Needs a long run or a forced rotation of an OLM object | New issue, see the report |
| Upgrade from the six operators, and WAO upgrade | Out of scope for this task | None |
| OLM v1 install | The cluster uses classic OLM | None |
| Admission under load, and latency | No requirement is defined | None |

## 5. Selection of webhook tests

### 5.1 Labels

Each new spec has these labels: `operator:wao`, `component:webhook`, `tier:acceptance`, `disruption:nondestructive`, `platform:any`, `frequency:weekly`.
An operand spec has the operator label of its operand also (for example `operator:far`).
The two resilience specs have `tier:resiliency` in place of `tier:acceptance`.

Thus one label expression selects the WAO spec and the operator suite specs of one operand:

```text
component:webhook && operator:far && !disruption:destructive
```

### 5.2 Run script

`scripts/run-webhook-tests.sh` sets `ECO_TEST_FEATURES` and `ECO_TEST_LABELS`, and then starts `scripts/test-runner.sh`.
The Makefile target is `run-webhook-tests`.

| Command | Suites | Label expression |
| --- | --- | --- |
| `make run-webhook-tests` | far, nhc, nmo, snr, wao | `component:webhook && !disruption:destructive` |
| `make run-webhook-tests WEBHOOK_TARGET=wao` | wao | `component:webhook && operator:wao` |
| `make run-webhook-tests WEBHOOK_TARGET=far` | far, wao | `component:webhook && operator:far && !disruption:destructive` |
| `make run-webhook-tests WEBHOOK_TARGET=nhc` | nhc, wao | `component:webhook && operator:nhc && !disruption:destructive` |
| `make run-webhook-tests WEBHOOK_TARGET=nmo` | nmo, wao | `component:webhook && operator:nmo && !disruption:destructive` |
| `make run-webhook-tests WEBHOOK_TARGET=snr` | snr, wao | `component:webhook && operator:snr && !disruption:destructive` |

`WEBHOOK_DESTRUCTIVE=true` removes `!disruption:destructive` from the expression.

## 6. Test case documents

The conventions are in [TESTCASES/AGENTS.md](TESTCASES/AGENTS.md) and [TESTCASES/README.md](TESTCASES/README.md):

- One document for each Jira ticket, with the name `docs/TESTCASES/rhwa-<id>.md`.
- `make new-testcase JIRA=RHWA-<id>` creates the document from the template. A copy by hand is not permitted.
- The ticket is a `Task` with a title that starts with `TEST:` and with the label `rhwa-test`.
- Each `It` calls `reportxml.ID` with the number of the ticket.
- The document keeps all 13 sections. The step table has one row for each `By` text.
- The suite `README.md` gets a catalog entry.
- Section 11 of the document gives a `tools/watch` command.

**Open item: the Jira tickets do not exist.** The session that wrote this plan had no access to Jira.
The seven documents use the placeholder numbers RHWA-9001 to RHWA-9007.
`make check-testcase` passes with these numbers. `make check-testcase-jira` fails until a person does these steps:

1. Create seven tickets with the titles of section 4.1 (prefix `TEST:`, type `Task`, label `rhwa-test`).
2. Rename each document, and replace the number in the document, in `reportxml.ID`, in the `It` text and in the suite README.

For the watch command, the documents use `tools/watch/wao-watch.sh`. It shows the `DESIRED`, `PHASE` and `ADMIT` values of each operand.
A new script, `tools/watch/webhook-watch.sh`, shows the webhook entries, the Service endpoints and the test objects.

## 7. Run plan for the test cluster

Cluster: a disposable OpenShift cluster with classic OLM, 3 control plane nodes and 3 workers.

1. Install WAO from `feat/webhooks/snr` with `make dev-olm-deploy`. Each operand is `Disabled` after the install.
2. Run the WAO webhook specs: `make run-webhook-tests WEBHOOK_TARGET=wao`.
3. Run the non-destructive webhook specs of the operator suites, one operand at a time.
4. Run the full selection one time: `make run-webhook-tests`.
5. Do not run upgrade specs, CSV specs, SBR specs or destructive specs.
6. Record the results, and open a WAO issue for each new concern.

Operands that the runs enable: FAR, NHC, NMO and SNR. The SNR operand starts an agent Pod on each node.
The runs start no remediation and no maintenance.
