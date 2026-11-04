# AGENTS.md — Test Case Requirements

Requirements and expectations for a test case in this repository. Read this
before writing or modifying a test case document in `docs/TESTCASES/` or a
Ginkgo spec under `tests/<operator>/tests/`.

This file is scoped to test cases. Repository-wide conventions — suite layout,
initialization chain, label taxonomy, commit message format — are in the root
[AGENTS.md](../../AGENTS.md). Behavioral guidelines for agents are in
[.agents/SKILLS.md](../../.agents/SKILLS.md). The directory workflow and status
values are in [README.md](README.md).

## The Jira ticket

Every test case is tracked by a Jira ticket in the
[RHWA project](https://issues.redhat.com/browse/RHWA), and that ticket must be:

| Requirement | Value                                                          |
| ----------- | ---------------------------------------------------------------- |
| Issue type  | `Task` — not a Bug, Story, Epic, or sub-task                    |
| Title       | starts with `TEST:`, e.g. `TEST: Ceph RBD persistent fencing state` |
| Label       | `rhwa-test`                                                     |

Create the ticket before generating the document; `rhwa-<jira-id>.md` is named
after it. The three requirements are enforced in CI by
`make check-testcase-jira`, which reads the ticket from Jira and fails the
"Check Test Case" workflow when any of them is unmet, so a document whose
ticket is a Bug, lacks the `TEST:` prefix, or is missing the label will not
merge. Fix the ticket rather than the check.

Jira is the system of record for new tests; the optional Polarion row in
section 1 exists only for tests ported from an old Polarion plan.

## What a test case is

A test case is **two artifacts that ship in the same pull request**:

1. A document, `docs/TESTCASES/rhwa-<jira-id>.md`, generated from the
   [TEMPLATE.md](TEMPLATE.md) skeleton with `make new-testcase JIRA=RHWA-<id>`.
   Always use that target — humans and agents alike. Do not hand-copy the
   skeleton, write a document from memory, or reproduce the section list from
   this file: the target is what guarantees every document starts from the
   current skeleton with all thirteen sections intact. [EXAMPLE.md](EXAMPLE.md)
   is a finished document to read for calibration — never a file to copy, and
   never a source of content for your own test.
2. One or more Ginkgo specs under `tests/<operator>/tests/<feature>.go`.

A spec without a document, or a document whose steps no longer match its spec,
is incomplete work. Do not open a PR with only one of the two.

A third artifact, the entry in `tests/<operator>/README.md`, is updated in the
same PR (see section 13). That per-operator README remains the scannable catalog
(one entry per test); the document in `docs/TESTCASES/` is the full
specification behind that entry.

Throughout this file and the template, `<operator>` is the suite directory name
(`nhc-operator`, `sbr-operator`), and `<op>` is its short form used in package
names (`nhcparams`, `sbrparams`).

This applies to tests added from now on. Specs that predate this directory have
no document and do not need one backfilled; when you substantially rework one,
adding its document is welcome but is not a condition of the change.

## Required sections

Every document keeps all thirteen sections of the template, in order, including
the ones that do not apply — write `None` rather than deleting a heading, so
reviewers can tell "not applicable" from "not considered".

| # | Section                   | Must contain                                                                   |
| - | ------------------------- | ------------------------------------------------------------------------------ |
| 1 | Metadata                  | Jira link (a `Task` titled `TEST: …`, labelled `rhwa-test`), `reportxml.ID`, legacy Polarion link (OPTIONAL — `None` for anything not ported), operator versions, suite, spec file, status |
| 2 | Summary                   | One or two sentences in plain language                                         |
| 3 | Scope                     | In scope **and** an explicit out-of-scope list                                 |
| 4 | Environment               | Topology, platform, storage, namespace, destructive flag, every env var read   |
| 5 | Preconditions             | Conditions asserted in `BeforeAll`/`BeforeSuite`, not assumed                                |
| 6 | Test steps                | One row per `By("…")`, in order, with expected result                          |
| 7 | Pass criteria             | What a passing run proves, including that cleanup finished                     |
| 8 | Failure / known issues    | Open bugs, their effect, and the gate used — or `None`                         |
| 9 | Cleanup                   | What `DeferCleanup` and `AfterAll` each remove, and the recovery steps         |
| 10 | Labels                   | The exact `Label(…)` block from the spec                                       |
| 11 | Running it standalone    | A copy-pasteable `ginkgo` command, plus the `tools/watch` command for following the run                        |
| 12 | Implementation sketch    | The spec skeleton, or a link to the merged spec once implemented               |
| 13 | Documentation to update  | Suite `README.md` entry, the `tools/watch` catalog when a watch script is added or changed, and any guide that changes |

## Expectations for the document

- **Scope boundaries are explicit.** Name the adjacent behaviour this test does
  *not* prove and the spec that covers it. Unstated scope is the most common
  review comment on a new test case.
- **Steps mirror the spec.** The step table and the `By("…")` strings use the
  same wording in the same order. When you change one, change the other.
- **Pass criteria are observable.** "The operator behaves correctly" is not a
  pass criterion; "exactly one `SelfNodeRemediation` named after the target node
  exists within `nhcparams.RemediationCompletionTimeout`" is.
- **Environment is complete and minimal.** List only what the test reads. For a
  new env var, give its name (`<OP>_<THING>`), default, and the behaviour when
  unset — prefer `Skip` with an actionable message over a failure.
- **Destructive is declared.** If the test reboots, fences, cordons, stops a
  kubelet, or injects a network fault, say so in section 4 and label the spec
  `labels.DisruptionDestructive`.
- **Give the person running it something to watch.** Specs here are routinely run
  one at a time against a live cluster, and the state that matters lives on the
  cluster rather than in the Ginkgo log — which reports a step only once it has
  finished, and by then the evidence has often already healed. This is not just a
  destructive-test concern: an operator rolling out, a CR moving through its
  conditions, pods churning during an upgrade are all easier to follow live than
  to reconstruct afterwards, and a run that is going wrong is obvious minutes
  before Ginkgo says so. Give the runner a read-only snapshot script under
  [`tools/watch`](../../tools/watch): reuse an existing one where it fits, add a
  new one where it does not. Document the exact `watch -n 5 -d …` invocation in
  section 11, say what each block shows, and name the values that must change and
  the ones that must not — record the baseline (boot IDs, for a reboot test)
  before the run, because that is what the assertion actually compares against.
  Add any new script to the catalog in
  [`tools/watch/README.md`](../../tools/watch/README.md).
- **Watch tools run on Fedora Linux and on a MacBook.** People here run specs
  from both, so a tool that only works on one is a tool half the team cannot
  use. In practice: no `python3` (macOS ships none without the Xcode command
  line tools) — `oc -o custom-columns` and `awk` cover most of what a snapshot
  needs; stay within POSIX `awk`, since macOS has BSD awk and Fedora has gawk;
  target bash 3.2, the system bash on macOS, so no associative arrays,
  `mapfile`, or `${var^^}`; and avoid GNU-only flags on `sed`, `date` and
  `head`. macOS has no `watch` at all, so a tool meant for `watch -n 5 -d`
  needs a self-refreshing fallback — see `--loop` in
  [`snr-watch.sh`](../../tools/watch/snr-watch.sh), which also marks what
  changed since the previous tick. Run it on both before merging; the failure
  mode is silent, wrong columns rather than an error.
- **Absolute dates and versions.** Pin operator versions (`SBR v0.3.0`), not
  "latest". Write dates as `2026-10-14`, not "last week".

## Expectations for the spec

These are enforced in review and, where noted, by `make lint`.

- Every `It`/`DescribeTable` calls `reportxml.ID("…")` with the ID recorded in
  section 1 — the numeric part of the Jira key for a new test, or the existing
  `OCP-xxxxx` number for a test ported from a Polarion plan.
- Every step is a `By("description")` block.
- `Eventually`/`Consistently` with an explicit timeout and poll interval from
  `<op>params` or `medik8sparams`. Never `time.Sleep`.
- Labels come from `tests/internal/labels` — never a bare string literal.
  Operator, tier, and component are mandatory: CI selects specs with
  `ECO_TEST_LABELS` expressions such as `operator:nhc && tier:smoke`, and a spec
  missing its operator label is silently skipped by those runs. Set frequency,
  disruption, platform, and topology whenever the test is constrained on that
  axis, and keep them consistent with section 4 of the document. Older specs
  predate this rule and do not all satisfy it; new ones must.
- Assertion messages state what *must* hold, naming the resource:
  `"Node %q must reach %s=True after CephFS port injection"`.
- Gomega assertions live in test files only, never in `internal/` packages.
- Preconditions delete stale resources from a previous run **and wait for them to
  be fully gone** before recreating. This is the main source of flakes here.
- Cleanup is idempotent, tolerates `IsNotFound`, and never fails the suite on a
  missing resource — log with `GinkgoWriter.Printf` instead. `DeferCleanup`
  removes what the `It` created; `AfterAll` removes what `BeforeAll` created.
- Recovery (uncordon, restart kubelet, flush iptables, delete injector pods) runs
  even when the spec failed mid-way.
- A spec that exercises an unfixed product bug is gated behind an env var and
  `Skip`s with a message naming the bug and the variable. Never merge a spec
  known to fail on a current build without a gate.
- `make fmt && make lint && make test` pass before pushing.

## Operands under the workload-availability-operator

The medik8s operators are being consolidated into a single operator,
[workload-availability-operator](https://github.com/medik8s/workload-availability-operator)
(WAO). FAR, MDR, NHC, NMO, SNR and SBR are no longer installed individually;
each is an **operand** that WAO installs when it is enabled. See
[medik8s/workload-availability-operator#55](https://github.com/medik8s/workload-availability-operator/pull/55)
for the design, and the [wao-testing wiki](https://github.com/medik8s/.github/wiki/wao-testing)
for current test notes.

**This work happens on the `wao` branch only.** It was forked from `main` and is
merged back once the single-operator layout ships. Branch from `wao` and open
the PR against `wao` for anything in this section; a change that is not about
WAO still goes to `main`. See
[Project Conventions for Agents](../../AGENTS.md#project-conventions-for-agents).

**The goal is to keep the tests as written.** The behaviour under test does not
change: the operand is the same controller running in the same namespace
(`medik8sparams.OperatorNs`, `openshift-workload-availability`), reconciling the
same CRs. Only the *pre-test* functions change — what used to assume "the
operator is installed" must now make sure the operand is **enabled**. Do not
rewrite the body of an `It`, re-scope a test, or change its assertions as part of
a WAO migration; if a spec genuinely cannot work under WAO, say so in section 8
of its document rather than quietly reshaping it.

### What the cluster looks like now

| Before | Under WAO |
| ------ | --------- |
| One CSV per operator, each installed on demand | One CSV, `waoparams.CSVNamePattern` (`workload-availability-operator`) |
| Operator CRDs arrive with their operator | OLM installs **every** operand CRD and RBAC when WAO is installed, regardless of what is enabled |
| Operator Deployment present ⇒ operator usable | WAO's own manager is the only Deployment running until an operand is enabled |
| — | WAO creates a cluster-scoped singleton `WorkloadAvailabilityConfig` named `cluster` on bootstrap, with **every operand disabled** |

Enabling an operand (`spec.components.<key>.enabled: true`) makes WAO deploy that
operand's runtime manifests — Deployment, Service, ConfigMaps, webhooks.
Disabling removes them. The operand keys, phases and the helpers for reading and
patching the singleton live in
[`medik8sparams/wao.go`](../../tests/internal/medik8sparams/wao.go) and
[`helpers/wao.go`](../../tests/internal/helpers/wao.go); per-operand Deployment
names and timeouts are in
[`waoparams`](../../tests/wao-operator/internal/waoparams).

### The precondition contract

A suite's `BeforeAll`/`BeforeSuite` ensures its operands are enabled and running
before the first `It`. In order:

1. Read the singleton with `helpers.GetWAOConfig`.
2. For each operand the spec uses, check `helpers.WAOComponentEnabled`. If it is
   already enabled and `Available`, change nothing — the precondition must be
   idempotent and must not patch a cluster that is already correct.
3. Otherwise enable it with `helpers.EnableWAOComponents(ctx, APIClient,
   medik8sparams.WAOComponent<OP>)` — one patch naming only the operands the
   suite needs.
4. `Eventually` on `helpers.WAOComponentPhase` reaching
   `medik8sparams.WAOPhaseAvailable`, bounded by
   `waoparams.OperandAvailableTimeout` and `waoparams.DefaultPollInterval`.
   Never `time.Sleep` after a patch.
5. Keep the existing readiness check the spec already had — `deployment.Pull(…)`
   plus `IsReady`. It is still the externally observable assertion, and it is
   what tells you WAO reported `Available` for a manager that really came up.

`Available` already accounts for webhook admission readiness
(`status.components[].admissionReady`, which waits on the service CA bundle), so
once an operand is `Available` its CRs can be created without a further wait.
While an operand is disabled its webhooks are gone, so CRs are admitted
unvalidated — a spec that asserts a webhook rejection is meaningless unless its
operand is enabled first.

Both layouts exist during the transition. Branch on the singleton, not on an env
var or a cluster name: if the `WorkloadAvailabilityConfig` is `IsNotFound`, the
cluster is a pre-WAO deployment — fall back to the per-operator check the spec
already had rather than failing. Specs then run unchanged on either cluster.

### Do not

- **Do not treat a CRD as proof an operand is running.** Every operand CRD is
  installed with WAO and outlives any disable, so `CustomResourceDefinition`
  presence asserts nothing. Assert `status.components[].phase` and the manager
  Deployment.
- **Do not look up a per-operator CSV.** There is one CSV now. A spec that reads
  a per-operator CSV name, version, or annotation needs real rework — treat it
  as its own change with its own document update, not a precondition tweak.
- **Do not enable `medik8sparams.WAOAllComponents` from a suite precondition.**
  Enable only what the spec uses. Extra operands change what else is reconciling
  on the cluster and make a failure harder to read. The operand-lifecycle spec
  under `tests/wao-operator` is the only one that owns all six.
- **Do not disable operands in cleanup.** Enabling is an installation step, not
  state the test created, so `DeferCleanup`/`AfterAll` leave it enabled: a
  disable is slow, can land in `DisableIncomplete` with
  `status.components[].retainedResources` still on the cluster, and strands the
  next suite. The exception is a spec whose subject *is* enable/disable, which
  restores exactly what it found.
- **Do not assume enabling creates operand CRs.** Enabling starts the manager
  only. `NodeHealthCheck`, `StorageBasedRemediationConfig` and `NodeMaintenance`
  are still created by the spec.

### Cross-operand dependencies

A spec usually needs more than its own operand: an NHC remediation test needs
the remediator operand (SNR, FAR or MDR) enabled too, and an SBR test needs SBR
plus whatever triggers it. Enable every operand the spec actually exercises,
name each one in section 5 of the document, and name the remediator in section 3
so the scope boundary stays explicit.

### What this changes in the document

| Section | Add |
| ------- | --- |
| 1 Metadata | The WAO version alongside the operand version (`WAO v0.1.0`, `NHC v0.9.0`) |
| 3 Scope | Whether the test proves anything about WAO itself — for most tests, explicitly out of scope |
| 4 Environment | That the cluster has WAO installed, and the operand keys the test enables |
| 5 Preconditions | Each operand enabled and the phase waited on, as an asserted precondition |
| 7 Pass criteria | Nothing — "the operand is enabled" is a precondition, not something the test proves |
| 9 Cleanup | That operands are left enabled, and why |
| 11 Running it standalone | `tools/watch/wao-watch.sh` when the run turns an operand on, alongside the operand's own watch script |

## Porting a test from Python (ocp-edge-auto)

Read the full Python implementation before writing Go — the test method body,
every helper it calls, and the teardown. The Polarion plan says *what* to verify;
the Python code shows *how*, and the mechanism matters. Record any deviation from
the Python approach, with its reason, in section 3 of the document. See
"Migrating Tests from Python" in the root [AGENTS.md](../../AGENTS.md).

A ported test keeps its existing `OCP-xxxxx` number in `reportxml.ID` so results
stay comparable with historical runs; record the Polarion link in the optional
legacy row of section 1 and the Jira ticket tracking the port in the Jira row.
The Polarion link is a convenience for tracing a test back to its old plan — if
the work item is gone or you cannot find it, write `None` and move on.

## Review checklist

- [ ] Document and spec in the same PR; all thirteen sections present
- [ ] Jira ticket is a `Task`, titled `TEST: …`, labelled `rhwa-test`
- [ ] Jira and `reportxml.ID` agree with the spec
- [ ] Out-of-scope list is explicit
- [ ] Step table matches the `By("…")` strings one to one
- [ ] Pass criteria are observable and include cleanup
- [ ] Every env var documented with its default and unset behaviour
- [ ] Destructive tests declared and labelled
- [ ] Labels use `tests/internal/labels` constants; operator, tier, and
      component set, plus every other axis the test is constrained on
- [ ] No `time.Sleep`; timeouts come from `<op>params`/`medik8sparams`
- [ ] Preconditions clean stale state and wait for removal
- [ ] Preconditions enable every WAO operand the spec uses — and only those —
      wait for phase `Available`, are idempotent when it is already enabled, and
      fall back to the pre-WAO check when the `WorkloadAvailabilityConfig`
      singleton is absent
- [ ] Operands are left enabled by cleanup; no CRD-presence or per-operator-CSV
      check is used to prove an operand is running
- [ ] Cleanup idempotent and `IsNotFound`-tolerant
- [ ] Known-bug specs gated behind an env var with a `Skip` message
- [ ] `tests/<operator>/README.md` catalog entry added
- [ ] Section 11 documents a `tools/watch` command for following the run, and any
      new watch script is read-only, portable to Fedora and macOS, and listed in
      the `tools/watch` catalog
- [ ] `make fmt && make lint && make test` pass
