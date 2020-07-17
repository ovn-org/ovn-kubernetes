#!/usr/bin/env bash

set -ex

SHARD=$1

pushd $GOPATH/src/k8s.io/kubernetes/
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${HOME}/admin.conf
export MASTER_NAME=${KIND_CLUSTER_NAME}-control-plane
export NODE_NAMES=${MASTER_NAME}

groomTestList() {
	echo $(echo "${1}" | sed -e '/^\($\|#\)/d' -e 's/ /\\s/g' | tr '\n' '|' | sed -e 's/|$//')
}

SKIPPED_TESTS="
# PERFORMANCE TESTS: NOT WANTED FOR CI
Networking IPerf IPv[46]
\[Feature:PerformanceDNS\]

# FEATURES NOT AVAILABLE IN OUR CI ENVIRONMENT
\[Feature:Federation\]
should have ipv4 and ipv6 internal node ip

# TESTS THAT ASSUME KUBE-PROXY
kube-proxy
should set TCP CLOSE_WAIT timeout

# TO BE IMPLEMENTED: https://github.com/ovn-org/ovn-kubernetes/issues/819
Services.+session affinity

# TO BE IMPLEMENTED: https://github.com/ovn-org/ovn-kubernetes/issues/1116
EndpointSlices

# NOT IMPLEMENTED; SEE DISCUSSION IN https://github.com/ovn-org/ovn-kubernetes/pull/1225
named port.+\[Feature:NetworkPolicy\]

# TO BE FIXED BY https://github.com/kubernetes/kubernetes/pull/93119
GCE

# ???
\[Feature:NoSNAT\]
Services.+(ESIPP|cleanup finalizer)
configMap nameserver
ClusterDns \[Feature:Example\]
should set default value on new IngressClass
# RACE CONDITION IN TEST, SEE https://github.com/kubernetes/kubernetes/pull/90254
should prevent Ingress creation if more than 1 IngressClass marked as default
"

IPV4_ONLY_TESTS="
# Limit the IPv4 related test to IPv4 only deployments
#  See: https://github.com/leblancd/kube-v6-test
\[Feature:Networking-IPv4\]

# The following tests currently fail for IPv6 only, but should be passing.
# They will be removed as they are resolved.

# shard-n Tests
#  See: https://github.com/ovn-org/ovn-kubernetes/issues/1516
Network.+should resolve connrection reset issue

# shard-np Tests
#  See: https://github.com/ovn-org/ovn-kubernetes/issues/1517
NetworkPolicy.+should allow egress access to server in CIDR block
"
IPV6_ONLY_TESTS="
# Limit the IPv6 related tests to IPv6 only deployments
#  See: https://github.com/leblancd/kube-v6-test
\[Feature:Networking-IPv6\]
"

DUALSTACK_ONLY_TESTS="
\[Feature:.*DualStack.*\]
"

# Github CI doesn´t offer IPv6 connectivity, so always skip IPv6 only tests.
#  See: https://github.com/ovn-org/ovn-kubernetes/issues/1522
SKIPPED_TESTS=$SKIPPED_TESTS$IPV6_ONLY_TESTS

# IPv6 Only, skip any IPv4 Only Tests
if [ "$KIND_IPV4_SUPPORT" == false ] && [ "$KIND_IPV6_SUPPORT" == true ]; then
	echo "IPv6 Only"
	SKIPPED_TESTS=$SKIPPED_TESTS$IPV4_ONLY_TESTS
fi

# If not DualStack, skip DualStack tests
if [ "$KIND_IPV4_SUPPORT" == false ] || [ "$KIND_IPV6_SUPPORT" == false ]; then
	SKIPPED_TESTS=$SKIPPED_TESTS$DUALSTACK_ONLY_TESTS
fi

SKIPPED_TESTS="$(groomTestList "${SKIPPED_TESTS}")"

GINKGO_ARGS="--num-nodes=3 --ginkgo.skip=${SKIPPED_TESTS} --disable-log-dump=false --report-dir=${E2E_REPORT_DIR} --report-prefix=${E2E_REPORT_PREFIX}"

case "$SHARD" in
	shard-n-other)
		# all tests that don't have P as their sixth letter after the N, and all other tests
		GINKGO_ARGS="${GINKGO_ARGS} "'--ginkgo.focus=\[sig-network\]\s([Nn](.{6}[^Pp].*|.{0,6}$)|[^Nn].*)'
		;;
	shard-np)
		# all tests that have P as the sixth letter after the N
		GINKGO_ARGS="${GINKGO_ARGS} "'--ginkgo.focus=\[sig-network\]\s[Nn].{6}[Pp].*$'
		;;
	shard-test)
		TEST_REGEX_REPR=$(echo ${@:2} | sed 's/ /\\s/g')
		GINKGO_ARGS="${GINKGO_ARGS} "'--ginkgo.focus=\[sig-network\].*'$TEST_REGEX_REPR'.*'
		;;
	*)
		echo "unknown shard"
		exit 1
	;;
esac

e2e.test ${GINKGO_ARGS}
