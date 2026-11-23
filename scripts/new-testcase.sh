#!/usr/bin/env bash
# Scaffold a test case document from docs/TESTCASES/TEMPLATE.md.
#
# Usage: make new-testcase JIRA=RHWA-1834
#
# Kept portable between GNU and BSD userland (Linux and macOS): no in-place
# sed, no bash 4 features.

set -euo pipefail

JIRA_INPUT="${1:-}"

if [[ -z "${JIRA_INPUT}" ]]; then
    echo "usage: make new-testcase JIRA=RHWA-1834" >&2
    exit 1
fi

# macOS ships bash 3.2, which has no ${var^^} / ${var,,}.
JIRA_UPPER=$(printf '%s' "${JIRA_INPUT}" | tr '[:lower:]' '[:upper:]')
JIRA_LOWER=$(printf '%s' "${JIRA_INPUT}" | tr '[:upper:]' '[:lower:]')

if ! printf '%s' "${JIRA_UPPER}" | grep -Eq '^RHWA-[0-9]+$'; then
    echo "error: JIRA must look like RHWA-1834, got '${JIRA_INPUT}'" >&2
    exit 1
fi

JIRA_NUMBER="${JIRA_UPPER#RHWA-}"

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
TEMPLATE="${REPO_ROOT}/docs/TESTCASES/TEMPLATE.md"
TARGET="${REPO_ROOT}/docs/TESTCASES/${JIRA_LOWER}.md"

if [[ ! -f "${TEMPLATE}" ]]; then
    echo "error: template not found at ${TEMPLATE}" >&2
    exit 1
fi

if [[ -e "${TARGET}" ]]; then
    echo "error: ${TARGET#"${REPO_ROOT}"/} already exists; edit it instead" >&2
    exit 1
fi

# Drop the "this is the skeleton" notice, which only makes sense in the template
# itself, then substitute the ticket into the RHWA-XXXX / XXXX placeholders.
sed -e '/new-testcase:strip-start/,/new-testcase:strip-end/d' \
    -e "s/RHWA-XXXX/${JIRA_UPPER}/g" \
    -e "s/rhwa-xxxx/${JIRA_LOWER}/g" \
    -e "s/\`XXXX\`/\`${JIRA_NUMBER}\`/g" \
    -e "s/reportxml\.ID(\"XXXX\")/reportxml.ID(\"${JIRA_NUMBER}\")/g" \
    "${TEMPLATE}" >"${TARGET}"

cat <<EOF
created docs/TESTCASES/${JIRA_LOWER}.md

next steps:
  1. replace the remaining example values and delete the '>' guidance blockquotes
  2. fill in sections 1-12 before writing Go
  3. implement tests/<operator>/tests/<feature>.go, keeping the By("...") strings
     identical to the step table
  4. add the suite catalog entry in tests/<operator>/README.md

${JIRA_UPPER} must be a Task or Sub-task, titled "TEST: ...", and labelled rhwa-test.
CI checks this. Verify with: make check-testcase-jira TESTCASE=${JIRA_UPPER}

requirements and review checklist: docs/TESTCASES/AGENTS.md
EOF
