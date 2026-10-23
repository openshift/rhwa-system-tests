
# CI uses a non-writable home dir, make sure .cache is writable
ifeq ("${HOME}", "/")
HOME=/tmp
endif

BASE_IMG ?= system-tests
BASE_TAG ?= latest

## Tool Versions
GINKGO_VERSION ?= $(shell grep 'github.com/onsi/ginkgo/v2 ' go.mod | awk '{print $$2}')

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
GINKGO_DIR ?= $(LOCALBIN)/ginkgo
GINKGO = $(GINKGO_DIR)/$(GINKGO_VERSION)/ginkgo

GO_PACKAGES=$(shell go list ./...)
.PHONY: lint \
        lint-blanklines \
        fmt \
        deps-download \
        deps-update \
        new-testcase \
        check-testcase \
        check-testcase-jira \
        vet
vet:
	go vet ${GO_PACKAGES}

# JIRA: the ticket the test case belongs to, e.g. make new-testcase JIRA=RHWA-1834
JIRA ?=
new-testcase:
	scripts/new-testcase.sh $(JIRA)

# Structural check of docs/TESTCASES/; pass files to check only those.
# e.g. make check-testcase TESTCASE=docs/TESTCASES/rhwa-1834.md
TESTCASE ?=
check-testcase:
	scripts/check-testcase.sh $(TESTCASE)

# Checks each test case's Jira ticket is a Task titled "TEST: ..." and labelled
# rhwa-test. Needs network; pass a ticket or document to check only that one.
# e.g. make check-testcase-jira TESTCASE=RHWA-1834
check-testcase-jira:
	scripts/check-testcase-jira.sh $(TESTCASE)

lint: lint-blanklines
	@echo "Running go lint"
	scripts/golangci-lint.sh

lint-blanklines:
	go run ./internal/tools/blanklines ./...

fmt:
	scripts/golangci-lint.sh fmt
	go run ./internal/tools/blanklines --fix ./...

deps-download:
	go mod download
	go mod verify

deps-update:
	go mod tidy

# ECO_GOINFRA_BRANCH: optional branch to sync from (e.g. release-4.20). Empty = default branch.
ECO_GOINFRA_BRANCH ?=
sync-eco-goinfra:
ifneq ($(ECO_GOINFRA_BRANCH),)
	go get github.com/rh-ecosystem-edge/eco-goinfra@$(ECO_GOINFRA_BRANCH)
else
	go get github.com/rh-ecosystem-edge/eco-goinfra
endif
	go mod tidy

.PHONY: ginkgo
ginkgo: $(LOCALBIN) ## Download ginkgo locally if necessary.
	$(call go-install-tool,$(GINKGO),$(GINKGO_DIR),github.com/onsi/ginkgo/v2/ginkgo@${GINKGO_VERSION})

# go-install-tool will delete old package $2, then 'go install' any package $3 to $1.
define go-install-tool
@[ -f $(1) ] || { \
	set -e; \
	rm -rf $(2); \
	TMP_DIR=$$(mktemp -d); \
	cd $$TMP_DIR; \
	go mod init tmp; \
	BIN_DIR=$$(dirname $(1)); \
	mkdir -p $$BIN_DIR; \
	echo "Downloading $(3)"; \
	GOBIN=$$BIN_DIR GOFLAGS='' go install $(3); \
	rm -rf $$TMP_DIR; \
}
endef

build-docker-image:
	@echo "Building docker image"
	podman build -t "${BASE_IMG}:${BASE_TAG}" -f Dockerfile

install: deps-download ginkgo
	@echo "Installing needed dependencies"

run-tests: ginkgo
	@echo "Executing test-runner script"
	GINKGO=$(GINKGO) scripts/test-runner.sh

run-internal-pkg-unit-tests:
	@echo "Executing internal package unit tests"
	UNIT_TEST=true WORKLOAD_IMAGE=unused go test -v ./tests/internal/...

# Note: To add more unit tests for more packages, add corresponding targets here
test: run-internal-pkg-unit-tests run-nhc-upgrade-unit-tests run-sbr-upgrade-unit-tests run-snr-upgrade-unit-tests run-far-upgrade-unit-tests run-mdr-upgrade-unit-tests run-nmo-upgrade-unit-tests run-blanklines-unit-tests

.PHONY: run-blanklines-unit-tests
run-blanklines-unit-tests:
	go test ./internal/tools/blanklines

.PHONY: run-nhc-upgrade-unit-tests
run-nhc-upgrade-unit-tests:
	UNIT_TEST=true WORKLOAD_IMAGE=unused go test ./tests/nhc-operator/internal/...

.PHONY: run-sbr-upgrade-unit-tests
run-sbr-upgrade-unit-tests:
	UNIT_TEST=true WORKLOAD_IMAGE=unused go test ./tests/sbr-operator/internal/...

.PHONY: run-snr-upgrade-unit-tests
run-snr-upgrade-unit-tests:
	UNIT_TEST=true WORKLOAD_IMAGE=unused go test ./tests/snr-operator/internal/...

.PHONY: run-far-upgrade-unit-tests
run-far-upgrade-unit-tests:
	UNIT_TEST=true WORKLOAD_IMAGE=unused go test ./tests/far-operator/internal/...

.PHONY: run-mdr-upgrade-unit-tests
run-mdr-upgrade-unit-tests:
	UNIT_TEST=true WORKLOAD_IMAGE=unused go test ./tests/mdr-operator/internal/...

.PHONY: run-nmo-upgrade-unit-tests
run-nmo-upgrade-unit-tests:
	UNIT_TEST=true WORKLOAD_IMAGE=unused go test ./tests/nmo-operator/internal/...

coverage-html: test
	go tool cover -html cover.out
