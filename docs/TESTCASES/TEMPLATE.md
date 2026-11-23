# RHWA-XXXX — <one line: the behaviour this test proves>
<!-- new-testcase:strip-start -->

> **This is the skeleton.** Do not copy it by hand and do not fill it in here —
> run `make new-testcase JIRA=RHWA-<id>`, which writes
> `docs/TESTCASES/rhwa-<id>.md` with the ticket substituted and this notice
> removed. [EXAMPLE.md](EXAMPLE.md) is a finished document to read first;
> [AGENTS.md](AGENTS.md) holds the rules and the review checklist.
<!-- new-testcase:strip-end -->

> Lines in `> blockquote` are guidance. Replace every `<…>` placeholder, then
> delete the guidance before opening the PR. Keep all thirteen headings, in this
> order, even when a section does not apply — write `None` under it instead, so a
> reviewer can tell "not applicable" from "not considered".
>
> The rules and the checklist this will be reviewed against are in
> [AGENTS.md](AGENTS.md); [EXAMPLE.md](EXAMPLE.md) is a finished document showing
> the level of detail each section wants.

## 1. Metadata

| Field                        | Value                                                   |
| ---------------------------- | ------------------------------------------------------- |
| Jira                         | [RHWA-XXXX](https://issues.redhat.com/browse/RHWA-XXXX) |
| `reportxml.ID`               | `XXXX`                                                  |
| Polarion (legacy) — OPTIONAL | None                                                    |
| Operators                    | `<operator and pinned version, e.g. NHC v0.9.0>`        |
| Suite                        | `tests/<operator>`                                      |
| Spec file                    | `tests/<operator>/tests/<feature>.go`                   |
| Author                       | `<your-kerberos>`                                       |
| Status                       | draft                                                   |

> The Jira ticket must have type **`Task`** or **`Sub-task`**.
> Its title must start with **`TEST:`**. It must carry the label **`rhwa-test`**.
> CI reads the ticket and fails the "Check Test Case" workflow when any
> requirement is unmet. Correct the ticket if a requirement is unmet.
>
> `<operator>` is the suite directory name — `nhc-operator`, not `nhc`.
> `reportxml.ID` is the numeric part of the Jira key and must match the
> `reportxml.ID("…")` argument in the spec. Every `It`/`DescribeTable` needs one.
>
> "Polarion (legacy)" is **optional** — a nice-to-have, never a reason to hold
> up a test case. Jira is the system of record. Leave it `None` unless this test
> was ported from a Polarion plan that already has an `OCP-xxxxx` ID; then link
> the work item here and keep that ID in `reportxml.ID` so historical runs stay
> comparable. Keep the row itself even when it is `None`.
>
> Status tracks this document's own lifecycle. The skeleton starts at `draft`;
> it is one of exactly three values:
>
> - `draft` — document under review, no spec merged yet. This is the state it is
>   written and reviewed in, before the Go code exists.
> - `implemented` — spec merged and passing in CI. Set this before opening the
>   PR: the document and the spec land in the same change, so a merged document
>   is rarely still a draft.
> - `blocked-by-bug` — spec merged, but gated behind an env var and `Skip`ping
>   because the product bug named in section 8 is still open. Section 8 must not
>   say `None` when the status is this.

## 2. Summary

`<One or two sentences, in plain language, describing the behaviour under test.>`

## 3. Scope

- **In scope**: `<what this test proves>`
- **Out of scope**: `<the adjacent behaviour it does not prove, and the spec that
  covers it instead>`

> Unstated scope is the most common review comment on a new test case.

## 4. Environment

| Requirement  | Value                                                          |
| ------------ | -------------------------------------------------------------- |
| Cluster      | `<e.g. OCP ≥ 4.13, multi-node (≥ 2 schedulable workers)>`      |
| Topology     | `<topology:… label, or any>`                                   |
| Platform     | `<platform:… label>`                                           |
| Storage      | `<storage class and volume mode, or none>`                     |
| Network      | `<connected, disconnected, or either>`                         |
| Namespace    | `openshift-workload-availability` (`medik8sparams.OperatorNs`) |
| Destructive? | `<yes — what it does to the cluster / no>`                     |

Environment variables:

```bash
export KUBECONFIG=/absolute/path/to/kubeconfig
export ECO_TEST_FEATURES=<operator>
export ECO_TEST_LABELS='<label filter expression>'
```

> List *only* the variables this test reads. For a new variable, give its name
> (`<OP>_<THING>`), its default, and the behaviour when unset — `Skip` with an
> actionable message is preferred over a failure.

## 5. Preconditions

1. `<condition asserted in BeforeAll>`
2. `<…>`

> Assert these in `BeforeAll`, do not assume them. A stale resource from a
> previous run is the main source of flakes here: delete it, then wait for it to
> be fully gone before recreating.

## 6. Test steps

| # | Step (`By` text)   | Expected result        |
| - | ------------------ | ---------------------- |
| 1 | `<By("…") string>` | `<observable outcome>` |
| 2 | `<By("…") string>` | `<observable outcome>` |

> One row per `By("…")` block, in order, using the same wording as the spec. A
> reviewer should be able to read this table next to the spec and match them line
> for line.

## 7. Pass criteria

- Exactly one spec with `reportxml.ID("XXXX")` runs and passes.
- `<the observable conditions that must hold>`
- Cleanup finishes: `<what must be gone, and the state the cluster returns to>`

> "The operator behaves correctly" is not a pass criterion. Name the resource,
> the condition, and the timeout constant.

## 8. Failure / known issues

None.

> If the test exercises an unfixed product bug, replace `None` with a table of
> bug, effect, and the env var gate used, and make the spec `Skip` with a message
> naming both. Never merge a spec known to fail on a current build without a gate.

## 9. Cleanup

- `DeferCleanup` removes `<what the It created>`.
- `AfterAll` removes `<what BeforeAll created>`, tolerating `IsNotFound`.
- Recovery (`<uncordon, restart kubelet, flush iptables, …>`) runs even when the
  spec failed mid-way.

> Cleanup must be idempotent and must never fail the suite on a missing resource;
> log a warning with `GinkgoWriter.Printf` instead.

## 10. Labels

```go
Label(
    labels.Operator<OP>,
    labels.Tier<TIER>,
    labels.Component<COMPONENT>,
    labels.StabilityCandidate,
)
```

> Operator, tier, and component are mandatory — CI selects specs with
> `ECO_TEST_LABELS` expressions such as `operator:nhc && tier:smoke`, and a spec
> missing its operator label is silently skipped by those runs. Every **new**
> default-lane spec must also include `labels.StabilityCandidate`.
> Do not add it for upgrade / interop /
> disconnected-only workflows. Add frequency, disruption, platform, and topology
> whenever the test is constrained on that axis, and keep them consistent with
> section 4. Constants only, from `tests/internal/labels` — never a bare string
> literal.

## 11. Running it standalone

```bash
ginkgo -v --label-filter="<expression>" \
    --focus="<unique substring of the It text>" ./tests/<operator>/...
```

## 12. Implementation sketch

```go
It("RHWA-XXXX: <It text>",
    reportxml.ID("XXXX"),
    Label(
        labels.Operator<OP>,
        labels.Tier<TIER>,
        labels.Component<COMPONENT>,
        labels.StabilityCandidate,
    ), func() {
        DeferCleanup(func() {
            By("DeferCleanup: <recovery>")
        })

        By("<step 1>")

        Eventually(func() error {
            return <check>
        }, <op>params.<Timeout>, <op>params.DefaultPollInterval).Should(Succeed(),
            "<what must hold, naming the resource>")
    })
```

> Once the spec merges, replace this sketch with a link to it.

## 13. Documentation to update

- `tests/<operator>/README.md` — add the test to the suite's numbered
  catalog with its Jira link, operators, cluster/storage requirements, standalone
  command, and pass criteria.
- `docs/README.md` — only when adding a new cross-cutting guide.
