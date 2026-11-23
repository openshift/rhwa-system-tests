#!/usr/bin/env bash
# Checks the Jira ticket behind each test case document in docs/TESTCASES/.
#
# Usage:
#   scripts/check-testcase-jira.sh [RHWA-1234 | docs/TESTCASES/rhwa-1234.md ...]
#
# With no arguments it checks the ticket of every docs/TESTCASES/rhwa-*.md.
#
# A test case ticket must be:
#   - issue type Task or Sub-task
#   - titled "TEST: <what the test proves>"
#   - labelled rhwa-test
#
# Red Hat Jira serves these fields anonymously, so no credentials are needed.
# Set JIRA_TOKEN for a bearer token, or JIRA_USER + JIRA_TOKEN for basic auth,
# if that ever changes. JIRA_BASE_URL overrides the instance.
#
# A ticket that exists and breaks the convention is an error. Jira being
# unreachable, rate-limiting, or demanding credentials is a warning: a CI
# outage should not block a pull request.
#
# Kept portable between GNU and BSD userland (Linux and macOS): no bash 4
# features, no GNU-only flags.

set -uo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)

JIRA_BASE_URL="${JIRA_BASE_URL:-https://redhat.atlassian.net}"
JIRA_TOKEN="${JIRA_TOKEN:-}"
JIRA_USER="${JIRA_USER:-}"

REQUIRED_PREFIX="TEST:"
REQUIRED_LABEL="rhwa-test"

errors=0
warnings=0

report() {
    level="$1"
    file="$2"
    message="$3"

    if [ -n "${GITHUB_ACTIONS:-}" ]; then
        printf '::%s file=%s::%s\n' "${level}" "${file}" "${message}"
    else
        printf '%s: %s: %s\n' "${level}" "${file}" "${message}"
    fi
}

err() {
    report error "$1" "$2"
    errors=$((errors + 1))
}

warn() {
    report warning "$1" "$2"
    warnings=$((warnings + 1))
}

# Body of the issue request, with the HTTP status on the last line.
fetch_issue() {
    key="$1"
    url="${JIRA_BASE_URL}/rest/api/2/issue/${key}?fields=issuetype,summary,labels"

    if [ -n "${JIRA_USER}" ] && [ -n "${JIRA_TOKEN}" ]; then
        curl -sS -L -m 30 -w '\n%{http_code}' -u "${JIRA_USER}:${JIRA_TOKEN}" "${url}" 2>/dev/null
    elif [ -n "${JIRA_TOKEN}" ]; then
        curl -sS -L -m 30 -w '\n%{http_code}' -H "Authorization: Bearer ${JIRA_TOKEN}" "${url}" 2>/dev/null
    else
        curl -sS -L -m 30 -w '\n%{http_code}' "${url}" 2>/dev/null
    fi
}

check_ticket() {
    key="$1"
    file="$2"

    response=$(fetch_issue "${key}")
    status=$(printf '%s' "${response}" | tail -1)
    body=$(printf '%s' "${response}" | sed '$d')

    case "${status}" in
    200) ;;
    404)
        err "${file}" "Jira ${key} does not exist (404 from ${JIRA_BASE_URL})"

        return
        ;;
    401 | 403)
        warn "${file}" "Jira ${key} needs credentials (HTTP ${status}); set JIRA_TOKEN to check it"

        return
        ;;
    "" | 000)
        warn "${file}" "could not reach ${JIRA_BASE_URL} to check ${key}; skipping the ticket check"

        return
        ;;
    *)
        warn "${file}" "unexpected HTTP ${status} fetching ${key}; skipping the ticket check"

        return
        ;;
    esac

    issue_type=$(printf '%s' "${body}" | jq -r '.fields.issuetype.name // ""')
    summary=$(printf '%s' "${body}" | jq -r '.fields.summary // ""')
    labels=$(printf '%s' "${body}" | jq -r '(.fields.labels // []) | join(", ")')

    case "${issue_type}" in
    Task | Sub-task) ;;
    *)
        err "${file}" "Jira ${key} has type '${issue_type}'. A test case ticket must be a Task or Sub-task"
        ;;
    esac

    case "${summary}" in
    "${REQUIRED_PREFIX}"*) ;;
    *)
        err "${file}" "Jira ${key} is titled '${summary}'; the title must start with '${REQUIRED_PREFIX}'"
        ;;
    esac

    if ! printf '%s' "${body}" | jq -e --arg want "${REQUIRED_LABEL}" \
        'any((.fields.labels // [])[]; . == $want)' >/dev/null; then
        if [ -z "${labels}" ]; then
            err "${file}" "Jira ${key} has no labels; it must be labelled '${REQUIRED_LABEL}'"
        else
            err "${file}" "Jira ${key} is labelled '${labels}'; it must also carry '${REQUIRED_LABEL}'"
        fi
    fi
}

cd "${REPO_ROOT}" || exit 1

if ! command -v curl >/dev/null 2>&1; then
    printf 'warning: curl not found; skipping the Jira ticket check\n'

    exit 0
fi

if ! command -v jq >/dev/null 2>&1; then
    printf 'warning: jq not found; skipping the Jira ticket check\n'

    exit 0
fi

targets=""
if [ "$#" -gt 0 ]; then
    targets="$*"
else
    for candidate in docs/TESTCASES/rhwa-*.md; do
        [ -e "${candidate}" ] || continue
        targets="${targets} ${candidate}"
    done
fi

if [ -z "${targets}" ]; then
    printf 'no test case documents to check\n'

    exit 0
fi

checked=0
for target in ${targets}; do
    # Accept either a document path or a bare ticket key.
    case "${target}" in
    *.md)
        file="${target}"
        key=$(basename "${target}" .md | tr '[:lower:]' '[:upper:]')
        ;;
    *)
        key=$(printf '%s' "${target}" | tr '[:lower:]' '[:upper:]')
        file="docs/TESTCASES/$(printf '%s' "${key}" | tr '[:upper:]' '[:lower:]').md"
        [ -e "${file}" ] || file="${key}"
        ;;
    esac

    if printf '%s' "${key}" | grep -Eqv '^RHWA-[0-9]+$'; then
        err "${file}" "cannot derive a Jira key from '${target}'"

        continue
    fi

    checked=$((checked + 1))
    check_ticket "${key}" "${file}"
done

printf '\nchecked %d Jira ticket(s): %d error(s), %d warning(s)\n' "${checked}" "${errors}" "${warnings}"

if [ "${errors}" -gt 0 ]; then
    printf 'a test case ticket must be a Task or Sub-task, titled "%s ...", and labelled %s\n' \
        "${REQUIRED_PREFIX}" "${REQUIRED_LABEL}"
    printf 'requirements: docs/TESTCASES/AGENTS.md\n'

    exit 1
fi

exit 0
