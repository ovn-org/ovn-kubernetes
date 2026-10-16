# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

E2E_REPORT_DIR:=$(CURDIR)/_artifacts
JOB_NAME?="$@"

# CRD integration tests — run against a real kube-apiserver + etcd (envtest).
# setup-envtest downloads the required binaries on first run and caches them in
# ENVTEST_ASSETS_DIR.  No Kind cluster is needed.
#
# Long-term goal: tests from testscenario/ (CRD validation, CEL rules,
# defaulting) that do not require a full Kind cluster will migrate here.
# Tracking issue: https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6932
#
# Usage:
#   make test-crd
#   make test-crd ENVTEST_K8S_VERSION=1.37.0   # override k8s version
ENVTEST_ASSETS_DIR ?= $(CURDIR)/_artifacts/envtest-bin
# Keep in sync with K8S_VERSION in .github/workflows/test.yml
ENVTEST_K8S_VERSION ?= 1.36.2

.PHONY: test-crd
test-crd:
	mkdir -p $(ENVTEST_ASSETS_DIR)
	cd crd-integration && \
	KUBEBUILDER_ASSETS="$$(go tool setup-envtest \
	    use $(ENVTEST_K8S_VERSION) --bin-dir $(abspath $(ENVTEST_ASSETS_DIR)) -p path)" \
	go test -count=1 -v ./...

# Make no assumptions about network, so run all tests. If on IPv4 or
# IPv6 only network, set appropriately to skip related tests.
PLATFORM_IPV4_SUPPORT?=false
PLATFORM_IPV6_SUPPORT?=false
DUALSTACK_CONVERSION?=false

# Coredump detection settings
# Directory where kind clusters store coredumps
COREDUMP_DIR?=/tmp/kind/logs/coredumps
# Processes to skip when checking for coredumps (pipe-separated for grep)
# https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5782
# https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6299
#
# Don't remove this skip until we trust frr enough not to puke coredumps that
# often.
SKIPPED_COREDUMPS?=zebra|bgpd|mgmtd|bfdd

# Check for coredumps and fail if any are found (excluding skipped processes)
# Usage: $(call check-coredumps)
define check-coredumps
	@if [ -d "$(COREDUMP_DIR)" ]; then \
		coredumps=$$(find "$(COREDUMP_DIR)" -maxdepth 1 -type f 2>/dev/null | grep -v -E '$(SKIPPED_COREDUMPS)' || true); \
		if [ -n "$$coredumps" ]; then \
			echo "ERROR: Coredumps found:"; \
			echo "$$coredumps"; \
			exit 1; \
		fi; \
	fi
endef

.PHONY: install-kind
install-kind:
	PLATFORM_IPV4_SUPPORT=$(PLATFORM_IPV4_SUPPORT) \
	PLATFORM_IPV6_SUPPORT=$(PLATFORM_IPV6_SUPPORT) \
	./scripts/install-kind.sh

.PHONY: staged-upgrade-ovn
staged-upgrade-ovn:
	./scripts/staged-upgrade-ovn.sh

.PHONY: shard-%
shard-%:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) \
	E2E_REPORT_PREFIX=$(JOB_NAME)_ \
	PLATFORM_IPV4_SUPPORT=$(PLATFORM_IPV4_SUPPORT) \
	PLATFORM_IPV6_SUPPORT=$(PLATFORM_IPV6_SUPPORT) \
	DUALSTACK_CONVERSION=$(DUALSTACK_CONVERSION) \
	SINGLE_NODE_CLUSTER=$(SINGLE_NODE_CLUSTER) \
	./scripts/e2e-kind.sh $@ "$(WHAT)"
	$(call check-coredumps)

# control-plane: coredump check is handled by the test framework (wrappedTestFramework)
.PHONY: control-plane
control-plane:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) \
	E2E_REPORT_PREFIX=$(JOB_NAME)_ \
	PLATFORM_IPV4_SUPPORT=$(PLATFORM_IPV4_SUPPORT) \
	PLATFORM_IPV6_SUPPORT=$(PLATFORM_IPV6_SUPPORT) \
	./scripts/e2e-cp.sh "$(WHAT)"

.PHONY: conformance
conformance:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) \
	E2E_REPORT_PREFIX=$(JOB_NAME)_ \
	./scripts/conformance.sh
	$(call check-coredumps)

.PHONY: tools
tools:
	./scripts/test-ovnkube-trace.sh
	$(call check-coredumps)

.PHONY: traffic-flow-tests
traffic-flow-tests:
	TRAFFIC_FLOW_TESTS=$(TRAFFIC_FLOW_TESTS) \
	./scripts/traffic-flow-tests.sh "$(WHAT)"
	$(call check-coredumps)
