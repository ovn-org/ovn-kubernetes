E2E_REPORT_DIR:=$(CURDIR)/_artifacts
JOB_NAME?="$@"

# Make no assumptions about network, so run all tests. If on IPv4 or
# IPv6 only network, set appropriately to skip related tests.
KIND_IPV4_SUPPORT?=false
KIND_IPV6_SUPPORT?=false
DUALSTACK_CONVERSION?=false

.PHONY: install-kind
install-kind:
	KIND_IPV4_SUPPORT=$(KIND_IPV4_SUPPORT) \
	KIND_IPV6_SUPPORT=$(KIND_IPV6_SUPPORT) \
	./scripts/install-kind.sh

.PHONY : upgrade-ovn
upgrade-ovn:
	./scripts/upgrade-ovn.sh

.PHONY: shard-%
shard-%:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) \
	E2E_REPORT_PREFIX=$(JOB_NAME)_ \
	KIND_IPV4_SUPPORT=$(KIND_IPV4_SUPPORT) \
	KIND_IPV6_SUPPORT=$(KIND_IPV6_SUPPORT) \
	DUALSTACK_CONVERSION=$(DUALSTACK_CONVERSION) \
	SINGLE_NODE_CLUSTER=$(SINGLE_NODE_CLUSTER) \
	./scripts/e2e-kind.sh $@ $(WHAT)

.PHONY: control-plane
control-plane:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) \
	E2E_REPORT_PREFIX=$(JOB_NAME)_ \
	KIND_IPV4_SUPPORT=$(KIND_IPV4_SUPPORT) \
	KIND_IPV6_SUPPORT=$(KIND_IPV6_SUPPORT) \
	OVN_HA=$(OVN_HA) \
	./scripts/e2e-cp.sh $(WHAT)

.PHONY: conformance
conformance:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) \
	E2E_REPORT_PREFIX=$(JOB_NAME)_ \
	./scripts/conformance.sh

.PHONY: tools
tools:
	./scripts/test-ovnkube-trace.sh

.PHONY: traffic-flow-tests
traffic-flow-tests:
	TRAFFIC_FLOW_TESTS=$(TRAFFIC_FLOW_TESTS) \
	./scripts/traffic-flow-tests.sh $(WHAT)
