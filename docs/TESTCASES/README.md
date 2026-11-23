# Test Case Documents

Every system test added to this repository is described by a test case document
in this directory before — or alongside — its Ginkgo spec. The document is the
contract a reviewer reads: what is verified, on which cluster, and what counts as
a pass. The per-operator `tests/<operator>/README.md` remains the scannable
catalog (one entry per test); the document here is the full specification behind
that entry. Specs that predate this directory have no document; none is
backfilled.

The normative rules live in [AGENTS.md](AGENTS.md) and apply to humans and coding
agents alike. This file explains the directory and the workflow.

## Contents

| File                       | Purpose                                                       |
| -------------------------- | ------------------------------------------------------------- |
| [AGENTS.md](AGENTS.md)     | Requirements and expectations for a test case                 |
| [TEMPLATE.md](TEMPLATE.md) | Empty skeleton; `make new-testcase` copies it                 |
| [EXAMPLE.md](EXAMPLE.md)   | A finished document, as a worked example to read              |
| `rhwa-<jira-id>.md`        | One document per Jira ticket                                  |

## Naming

- One file per Jira ticket: `rhwa-<jira-id>.md`, lowercase (`rhwa-1834.md`).
  Write the key uppercase in prose and links (`RHWA-1834`).
- A ticket covering several specs stays in one document, with one
  "Test steps"/"Pass criteria" pair per spec.
- `TEMPLATE.md` and `EXAMPLE.md` are never real test cases, and the `RHWA-XXXX`
  in the skeleton is not a ticket. Leave both alone except to improve the
  template or the example itself.

## Workflow

1. **File the Jira ticket.** It must be a `Task` or `Sub-task`, titled
   `TEST: <what the test proves>`, and labelled `rhwa-test`. CI checks all three with
   `make check-testcase-jira` and fails the PR when one is missing. You can
   check a ticket before opening the PR with
   `make check-testcase-jira TESTCASE=RHWA-1834`.
2. **Generate the document.** `make new-testcase JIRA=RHWA-1834` copies the
   `TEMPLATE.md` skeleton to `rhwa-1834.md` with the ticket substituted. Read
   [EXAMPLE.md](EXAMPLE.md) first to see the level of detail expected, then
   replace every `<…>` placeholder and delete the `>` guidance blockquotes.
3. **Fill in sections 1–12 before writing Go.** Scope, environment, preconditions,
   numbered steps, pass criteria, and the implementation sketch are design
   decisions, not documentation of code that already exists. Getting them
   reviewed first is cheaper than rewriting a spec.
4. **Implement the spec** under `tests/<operator>/tests/<feature>.go`, keeping the
   `By("…")` strings identical to the step table.
5. **Update the suite catalog.** Add the test to `tests/<operator>/README.md` with
   its Jira link, requirements, standalone command, and pass criteria. Keep that
   entry short — the document in this directory is the detailed description, and
   the catalog is the index that points at it.
6. **Set the document status** to `implemented` and open the PR with the document
   and the spec in the same change.

## Status values

| Status           | Meaning                                                              |
| ---------------- | ---------------------------------------------------------------------- |
| `draft`          | Document under review; no spec merged yet                             |
| `implemented`    | Spec merged and passing in CI                                         |
| `blocked-by-bug` | Spec merged but gated behind an env var because the product bug is open |

## Reviewing a test case

A reviewer should be able to read the document next to the spec and match them
line for line. The checklist is in [AGENTS.md](AGENTS.md#review-checklist).

## Related

- [AGENTS.md](../../AGENTS.md) — repository-wide conventions, suite layout, label
  taxonomy, commit message format
- [docs/README.md](../README.md) — cross-cutting guides
- [docs/fbc-upgrades.md](../fbc-upgrades.md) — shared FBC upgrade contract
- [docs/SBR/sbr_test_analysis.md](../SBR/sbr_test_analysis.md) — SBR coverage
  analysis and proposed tests; new proposals from it become documents here
- [medik8s CONTRIBUTING.md](https://github.com/medik8s/.github/blob/main/CONTRIBUTING.md)
  — DCO sign-off, fork workflow, `/lgtm` + `/approve` from two different OWNERS
