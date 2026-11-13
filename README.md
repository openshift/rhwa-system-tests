medik8s System Tests
=======
# system-tests

## Overview
The [system-tests](https://github.com/medik8s/system-tests) repository contains E2E tests for [medik8s](https://www.medik8s.io/) operators: FAR, MDR, NHC, NMO, and SNR.
The project is based on golang+[ginkgo](https://onsi.github.io/ginkgo) framework. It was forked from [eco-gotests](https://github.com/rh-ecosystem-edge/eco-gotests).

### Project requirements
* golang v1.26.x
* ginkgo v2.x

## Prerequisites
The test framework is designed to test a pre-installed OCP cluster which meets the following requirements:

### Mandatory setup requirements:
* OCP cluster installed with version >=4.13
* Operator namespace with Pod Security Admission (PSA) labels (manual installs only):

  All medik8s operators deploy into `openshift-workload-availability` and require privileged pod security
  because remediation operators run pods with host access and elevated capabilities. If you install
  operators manually (not via Prow CI), create the namespace and apply PSA labels before creating
  subscriptions:

  ```bash
  kubectl create namespace openshift-workload-availability
  kubectl label --overwrite ns openshift-workload-availability \
    security.openshift.io/scc.podSecurityLabelSync=false \
    pod-security.kubernetes.io/enforce=privileged \
    pod-security.kubernetes.io/audit=privileged \
    pod-security.kubernetes.io/warn=privileged
  ```

  > **Note:** In Prow CI, the `medik8s-operator-subscribe` step applies these labels automatically.
  > The `enforce=privileged` label is required (validated by FAR tests); the remaining labels
  > prevent SCC/PSA conflicts and match Prow CI behavior.

### Supported setups:
* Regular cluster 3 master nodes (VMs or BMs) 2 workers (VMs or BMs)
* Single Node Cluster (VM or BM)

### General environment variables
#### Mandatory:
* `KUBECONFIG` - Path to kubeconfig file. Default: empty
* `WORKLOAD_IMAGE` - Container image for test workload pods (used by destructive tests to verify pod eviction after remediation). In Prow CI this is set automatically by the `medik8s-lib` step. For local runs:
  ```bash
  export WORKLOAD_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest
  ```
#### Optional:
* `ECO_IS_DOWNSTREAM` - Whether operators under test are expected to carry product/downstream
  OLM packaging (CSV feature annotations, etc.). Defaults to `true`. Set to `false`
  for upstream Quay `main` bundles (e.g. `5.0-upstream` Prow jobs) so those packaging
  checks are skipped.
* Logging with glog

We use glog library for logging in the project. In order to enable verbose logging the following needs to be done:

1. Make sure to import inittool in your go script, per this example:

<sup>
    import (
      . "github.com/medik8s/system-tests/tests/internal/inittools"
    )
</sup>

2. Need to export the following SHELL variable:
> export ECO_VERBOSE_LEVEL=100

##### Notes:

  1. The value for the variable has to be >= 100.
  2. The variable can simply be exported in the shell where you run your automation.
  3. The go file you work on has to be in a directory under the `tests/` directory for being able to import inittools.
  4. Importing inittool also initializes the apiclient and it's available via "APIClient" variable.

* Collect logs from cluster with reporter

We use k8reporter library for collecting resource from cluster in case of test failure.
In order to enable k8reporter the following needs to be done:

1. Export DUMP_FAILED_TESTS and set it to true. Use example below
> export ECO_DUMP_FAILED_TESTS=true

2. Specify absolute path for logs directory like it appears below. By default /tmp/reports directory is used.
> export ECO_REPORTS_DUMP_DIR=/tmp/logs_directory

* Generation XML reports

We use reportxml library for generating compatible xml reports. 
The reporter is enabled by default and stores reports under REPORTS_DUMP_DIR directory.
In oder to disable reporterxml the following needs to be done:
> export ECO_ENABLE_REPORT=false


<!-- TODO Update this section with optional env vars for each test suite -->

## How to run

The test-runner [script](scripts/test-runner.sh) is the recommended way for executing tests.

Parameters for the script are controlled by the following environment variables:
- `ECO_TEST_FEATURES`: list of features to be tested ("all" will include all tests). All subdirectories under tests that match a feature will be included (internal directories are excluded) - _required_
- `ECO_TEST_LABELS`: ginkgo query passed to the label-filter option for including/excluding tests - _optional_ 
- `ECO_TEST_FOCUS`: text passed to ginkgo's `--focus` option for running a single test (or any tests whose name matches) - _optional_
- `ECO_VERBOSE_SCRIPT`: prints verbose script information when executing the script - _optional_
- `ECO_TEST_VERBOSE`: executes ginkgo with verbose test output - _optional_
- `ECO_TEST_TRACE`: includes full stack trace from ginkgo tests when a failure occurs - _optional_
- `ECO_TEST_TIMEOUT`: value passed to ginkgo's `-timeout` option, controlling the maximum time the full test run may take. Default: `24h` - _optional_

It is recommended to execute the runner script through the `make run-tests` make target.

### Local SSH bastion

Use this setup when the test runner cannot reach node InternalIP addresses directly.
It creates a bastion Pod on a control-plane node and forwards local port `2222` through the OpenShift API.
It does not need a LoadBalancer or an external VM.

Prerequisites:

- Log in to the target cluster with `oc login` before setup.
- Your cluster account must be able to create an SCC and grant its use to the bastion ServiceAccount.
- Install `oc`, `jq`, Bash, OpenSSH, and Make on the local computer.
- Supply an unencrypted private key that permits SSH as `core` on the nodes.
  The nodes must permit passwordless `sudo` for this account.
- Provide a Ready Linux control-plane node with the `node-role.kubernetes.io/master` label.
  That node must be able to pull the [CI bastion image](https://github.com/eparis/ssh-bastion/blob/master/deploy/deployment.yaml), `quay.io/eparis/ssh:latest`.
- Leave local port `2222` available.

From this repository, run the following command in Bash or Zsh:

```bash
bastion_env=$(make -s setup-local-bastion SSH_KEY=/absolute/path/to/private-key) && eval "$bastion_env"
```

Quote the `SSH_KEY` value if its path contains spaces.
Make cannot change its parent shell. The `eval` command loads the settings only after setup succeeds.
Do not replace this command with an unconditional `eval` of Make output.

The script creates local files under `/tmp/medik8s-local-bastion-$(id -u)`.
It copies the key to `cluster-profile/ssh-privatekey` with mode `0600`.
It saves the current cluster configuration in a separate `kubeconfig` file with mode `0600`.
The cluster receives the public key and a separate bastion host key. The node private key stays local.

The command sets `KUBECONFIG`, `CLUSTER_PROFILE_DIR`, and `SHARED_DIR` in the current shell.
It adds a local SSH wrapper to `PATH` so the existing test helper can use the forwarded port.
It does not edit your existing SSH configuration.
Run `make run-tests` from the same shell with the usual workload and test selection settings.

Repeated setup runs reuse the namespace, host key, and active port-forward supervisor.
The supervisor restarts `oc port-forward` one second after it exits. Setup checks SSH plus passwordless `sudo` on every Ready Linux node.
The dedicated `system-tests-local-bastion` SCC permits `SYS_CHROOT` and `AUDIT_WRITE`, which the SSH server requires.

The supervisor runs in the background. Its PID and log are in `port-forward.pid` and `port-forward.log`.
The log records forward exits and restart attempts. Connection failures remain visible to tests during reconnection.
Keep the computer awake and the cluster API accessible during tests.

To remove this setup, stop all tests first. Check the saved process from the configured shell:

```bash
bastion_state="/tmp/medik8s-local-bastion-$(id -u)"
bastion_pid=$(cat "$bastion_state/port-forward.pid")
ps -ww -p "$bastion_pid" -o command=
```

Confirm that the command runs this repository's `scripts/local-bastion-forward.sh` for the bastion.
For an older setup, confirm that it runs `oc port-forward` for `test-ssh-bastion`.
If that process still exists, stop it before removing the resources. The supervisor also stops its active child:

```bash
kill "$bastion_pid"
```

Then remove the cluster resources and local files:

```bash
oc --kubeconfig="$bastion_state/kubeconfig" delete namespace test-ssh-bastion --ignore-not-found
oc --kubeconfig="$bastion_state/kubeconfig" delete scc system-tests-local-bastion --ignore-not-found
rm -rf "$bastion_state"
```

### Test runner examples

Example:
```
$ export KUBECONFIG=/path/to/kubeconfig
$ export WORKLOAD_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest
$ export ECO_TEST_FEATURES="far-operator"
$ export ECO_TEST_LABELS='far'
$ make run-tests                    
Executing test-runner script
scripts/test-runner.sh
ginkgo -timeout=24h --keep-going --require-suite -r --label-filter="far" ./tests/far-operator
```

To run a single test, set `ECO_TEST_FOCUS` to (a substring of) the test's name, e.g. the `"should remediate a worker node after kubelet stop"` test from the SNR (Self Node Remediation) suite:

```
$ export KUBECONFIG=/path/to/kubeconfig
$ export WORKLOAD_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest
$ export ECO_TEST_FEATURES="snr-operator"
$ export ECO_TEST_FOCUS="should remediate a worker node after kubelet stop"
$ make run-tests
Executing test-runner script
scripts/test-runner.sh
ginkgo -timeout=24h --keep-going --require-suite -r --focus="should remediate a worker node after kubelet stop" ./tests/snr-operator
```

When focusing on a single test, it's also useful to shorten `ECO_TEST_TIMEOUT` from the 24h default:

```
$ export ECO_TEST_TIMEOUT=30m
$ make run-tests
Executing test-runner script
scripts/test-runner.sh
ginkgo -timeout=30m --keep-going --require-suite -r --focus="should remediate a worker node after kubelet stop" ./tests/snr-operator
```
## DAST Scanning

Runtime misconfiguration scanning of running RHWA operator pods using the
[Trivy](https://trivy.dev) Kubernetes scanner. This detects Kubernetes security
misconfigurations (pod security context, privilege escalation, read-only root filesystem, etc.)
in the live cluster — it is not endpoint fuzzing or API probing.

Background and full history: [DAST Scanning for RHWA Operators](https://docs.google.com/document/d/1pp4VR3jKGu_irgVqry8vRqJzWukjuX9Wmiia1L9ttOs).
For CI-integrated runs (prow), see [eco-gotests](https://github.com/openshift-kni/eco-gotests/pull/320).

### Prerequisites

- `KUBECONFIG` set and pointing at a cluster with RHWA operators deployed in `openshift-workload-availability`
- `podman` available locally
- `kubectl` available locally

### Run DAST

Scan all RHWA operators in the `openshift-workload-availability` namespace:

```bash
export KUBECONFIG=/path/to/kubeconfig
make run-dast
```

To scan a different namespace or pin a specific Trivy version:

```bash
make run-dast DAST_NAMESPACE=my-custom-namespace
make run-dast TRIVY_IMAGE=docker.io/aquasec/trivy:0.76.1
```

> **Note:** The scan targets a namespace, not a specific operator. For per-operator isolation,
> install each operator in its own namespace and pass that namespace via `DAST_NAMESPACE`.
> The default `openshift-workload-availability` is a shared namespace used when all RHWA
> operators are installed together (e.g. via OLM in Prow CI).

### Results

Scan output goes to stdout. Pass/fail is reported inline: a finding at HIGH or CRITICAL severity
causes a non-zero exit. No results are stored to disk — redirect stdout to capture them:

```bash
set -o pipefail; make -s run-dast | tee dast-results.json
```

`make -s` suppresses Make's recipe echo so only Trivy's JSON stdout is captured. Trivy progress
and diagnostics go to stderr and remain on the terminal. `set -o pipefail` ensures a non-zero
exit from `make run-dast` (findings found) propagates through the pipeline.

# How to contribute

The project follows the medik8s organization guidelines: the
[Contributing Guide](https://github.com/medik8s/.github/blob/main/CONTRIBUTING.md)
(DCO sign-off with `git commit -s`, forking workflow, `/lgtm` + `/approve` from two
different OWNERS), the
[Code of Conduct](https://github.com/medik8s/.github/blob/main/CODE_OF_CONDUCT.md),
and the [Security Policy](https://github.com/medik8s/.github/blob/main/SECURITY.md).

A new test ships as two artifacts in the same PR: a test case document under
[`docs/TESTCASES/`](docs/TESTCASES/README.md) — generate it with
`make new-testcase JIRA=RHWA-<id>` — and its Ginkgo spec. The requirements for both
are in [docs/TESTCASES/AGENTS.md](docs/TESTCASES/AGENTS.md).

The project uses a development method - forking workflow
### The following is a step-by-step example of forking workflow:
1) A developer [forks](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/working-with-forks/fork-a-repo)
   the [system-tests](https://github.com/medik8s/system-tests) project
2) A new local feature branch is created
3) A developer makes changes on the new branch.
4) New commits are created for the changes.
5) The branch gets pushed to developer's own server-side copy.
6) Changes are tested.
7) A developer opens a pull request(`PR`) from the new branch to
   the [system-tests](https://github.com/medik8s/system-tests).
8) The pull request gets approved from at least 2 reviewers for merge and is merged into
   the [system-tests](https://github.com/medik8s/system-tests).

# Project structure
    .
    ├── docs                               # Cross-cutting guides and test workflows
    │   └── TESTCASES                      # Per-ticket test case documents
    ├── images                             # Container images artifacts
    ├── scripts                            # Makefile scripts
    ├── tests                              # Test cases directory
    │   ├── internal                       # Common packages used across framework
    │   │   ├── config                     # Common config struct
    │   │   ├── medik8sconfig              # Medik8s-specific configuration
    │   │   ├── medik8sinittools           # Medik8s test initialization
    │   │   └── medik8sparams              # Medik8s shared constants
    │   ├── far-operator                   # Fence Agents Remediation tests
    │   ├── mdr-operator                   # Machine Deletion Remediation tests
    │   ├── nhc-operator                   # Node Health Check tests
    │   ├── nmo-operator                   # Node Maintenance Operator tests
    │   ├── sbr-operator                   # Storage-Based Remediation tests
    │   └── snr-operator                   # Self Node Remediation tests
    └── vendor                             # Dependencies folder
### Code conventions
#### Lint
Push requested are tested in a pipeline with golangci-lint. It is advised to add [Golangci-lint integration](https://golangci-lint.run/usage/integrations/) to your development editor. It's recommended to run `make lint` before uploading a PR.

Run `make fmt` after Go changes to apply gofumpt and goimports using the golangci-lint version pinned in `scripts/golangci-lint.sh`. The existing `make lint` check enforces this formatting along with `wsl_v5` and `nlreturn`. Do not add or remove cosmetic blank lines in unrelated code; gofumpt does not remove every optional blank line between statements.

`make lint` also runs `make lint-blanklines`, a Go checker in `internal/tools/blanklines` that rejects blank lines between consecutive assignments, calls, and increments/decrements. Keep straight-line code compact, regardless of whether the statements share a variable. Separators remain allowed at intervening comments (including trailing comments), actual Ginkgo `By` steps, control-flow statements, returns, and statements containing inline callbacks. Use a comment or named test step to mark a separate logical phase. `wsl_v5` checks error-check adjacency and block-edge whitespace, while `nlreturn` checks return spacing; conflicting statement-grouping rules are disabled. Generated files are excluded; ordinary Go files and test files selected by the current build settings are checked. Failures report the physical file and blank-line number and fail local lint and the existing CI lint job. Run `make fmt` to apply gofumpt/goimports and remove prohibited blank lines, `make lint-blanklines` for a read-only repository check, or `go run ./internal/tools/blanklines ./path/to/package/...` for selected packages. The checker does not execute test suites or require cluster credentials.

#### Commit Message Guidelines
There are two main components of a Git commit message: the title or summary, and the description. The commit message title is limited to 72 characters, and the description has no character limit.

Commit title format has two parts: 
1. Team name: Example - "cnf network" or "hw-accel" 
2. Short summary of code changes: Example - "added deployment test".

If a PR changes multiple team's directories or common infrastructure code, then instead of the team name, simply add "infra". Follow similar naming rules when adding changes to README (readme:) or github ci (ci:) files.

Commit message examples:
```text
infra: func defineNetwork moved to global net package"
readme: added commit message convention"
ci: added new deployment job
cnf network: added set func to cluster pkg
```

Please notice: Don't use internal test IDs and capital letters in commit message.

Commit description explains a changes in details if it's needed.

#### Functions format
If the function's arguments fit in a single line - use the following format:
```go
func Function(argInt1, argInt2 int, argString1, argString2 string) {
    ...
}
```

If the function's arguments don't fit in a single line - use the following format:
```go
func Function(
    argInt1 int,
    argInt2 int,
    argInt3 int,
    argInt4 int,
    argString1 string,
    argString2 string,
    argString3 string,
    argString4 string) {
    ...
}
```
One more acceptable format example:
```go
func Function(
    argInt1, argInt2 int, argString1, argString2 string, argSlice1, argSlice2 []string) {
	
}
```

### Common issues:
* If the automated commit check fails - make sure to pull/rebase the latest change and have a successful execution of 'make lint' locally first.


### Update eco-goinfra modules - How to:
1. In the left pane locate the "Eco-GoInfra Module Bump" action here: https://github.com/medik8s/system-tests/actions
2. Click on "Run workflow" in the right pane and run the workflow against the main branch (should take a few minutes to complete)
3. Click on the last executed workflow, then click on the "Label created PR" job and expand the "Label PR based on CI result" step
4. Copy the link to the created pull request and paste it in your browser.
5. Merge the pull request after passing the automated checks on it

#### Note: To start using the new package for the first time:
1. Add it to the import section of your test
2. Run `go mod vendor`
