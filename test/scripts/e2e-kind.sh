#!/usr/bin/env bash

set -ex

SHARD=$1

pushd $GOPATH/src/k8s.io/kubernetes/
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${HOME}/admin.conf
export MASTER_NAME=${KIND_CLUSTER_NAME}-control-plane
export NODE_NAMES=${MASTER_NAME}

SKIPPED_TESTS="
# PERFORMANCE TESTS: NOT WANTED FOR CI
Networking IPerf IPv[46]
\[Feature:PerformanceDNS\]

# FEATURES NOT AVAILABLE IN OUR CI ENVIRONMENT
\[Feature:Networking-IPv6\]
\[Feature:Federation\]

# TESTS THAT ASSUME KUBE-PROXY
kube-proxy
should set TCP CLOSE_WAIT timeout

# TO BE IMPLEMENTED: https://github.com/ovn-org/ovn-kubernetes/issues/1142
\[Feature:IPv6DualStackAlphaFeature\]

# TO BE IMPLEMENTED: https://github.com/ovn-org/ovn-kubernetes/issues/819
Services.+session affinity

# TO BE IMPLEMENTED: https://github.com/ovn-org/ovn-kubernetes/issues/1116
EndpointSlices

# REMOVE when k8s is updated to 1.18
should allow ingress access from updated pod.+\[Feature:NetworkPolicy\]

# NOT IMPLEMENTED; SEE DISCUSSION IN https://github.com/ovn-org/ovn-kubernetes/pull/1225
named port.+\[Feature:NetworkPolicy\]

# ???
\[Feature:NoSNAT\]
Services.+(ESIPP|cleanup finalizer)
configMap nameserver
ClusterDns \[Feature:Example\]
"

SKIPPED_TESTS=$(echo "${SKIPPED_TESTS}" | sed -e '/^\($\|#\)/d' -e 's/ /\\s/g' | tr '\n' '|' | sed -e 's/|$//')

GINKGO_ARGS="--num-nodes=3 --ginkgo.skip=${SKIPPED_TESTS} --disable-log-dump=false"

case "$SHARD" in
	shard-n)
		# all tests that don't have P as their sixth letter after the N
		GINKGO_ARGS="${GINKGO_ARGS} "'--ginkgo.focus=\[sig-network\]\s[Nn](.{6}[^Pp].*|.{0,6}$)'
		;;
	shard-np)
		# all tests that have P as the sixth letter after the N
		GINKGO_ARGS="${GINKGO_ARGS} "'--ginkgo.focus=\[sig-network\]\s[Nn].{6}[Pp].*$'
		;;
	shard-s)
		GINKGO_ARGS="${GINKGO_ARGS} "'--ginkgo.focus=\[sig-network\]\s[Ss].*'
		;;
	shard-other)
		GINKGO_ARGS="${GINKGO_ARGS} "'--ginkgo.focus=\[sig-network\]\s[^NnSs].*'
		;;
	*)
		echo "unknown shard"
		exit 1
	;;
esac

e2e.test ${GINKGO_ARGS}

