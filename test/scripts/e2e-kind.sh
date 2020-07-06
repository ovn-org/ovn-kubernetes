#!/usr/bin/env bash

set -ex

SHARD=$1

pushd $GOPATH/src/k8s.io/kubernetes/
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
GCE

# TESTS THAT ASSUME KUBE-PROXY
kube-proxy
should set TCP CLOSE_WAIT timeout

# TO BE IMPLEMENTED: https://github.com/ovn-org/ovn-kubernetes/issues/1142
\[Feature:IPv6DualStackAlphaFeature\]

# TO BE IMPLEMENTED: https://github.com/ovn-org/ovn-kubernetes/issues/819
Services.+session affinity

# TO BE IMPLEMENTED: https://github.com/ovn-org/ovn-kubernetes/issues/1116
EndpointSlices

# NOT IMPLEMENTED; SEE DISCUSSION IN https://github.com/ovn-org/ovn-kubernetes/pull/1225
named port.+\[Feature:NetworkPolicy\]

# ???
\[Feature:NoSNAT\]
Services.+(ESIPP|cleanup finalizer)
configMap nameserver
ClusterDns \[Feature:Example\]
should set default value on new IngressClass
# RACE CONDITION IN TEST, SEE https://github.com/kubernetes/kubernetes/pull/90254
should prevent Ingress creation if more than 1 IngressClass marked as default
"

SKIPPED_TESTS=$(echo "${SKIPPED_TESTS}" | sed -e '/^\($\|#\)/d' -e 's/ /\\s/g' | tr '\n' '|' | sed -e 's/|$//')

# if we set PARALLEL=true, skip serial test
if [ "${PARALLEL:-true}" = "true" ]; then
  export GINKGO_PARALLEL=y
  export GINKGO_PARALLEL_NODES=10
  SKIPPED_TESTS="${SKIPPED_TESTS}|\\[Serial\\]"
fi

case "$SHARD" in
	shard-network)
		FOCUS="\\[sig-network\\]"
		;;
	shard-conformance)
		FOCUS="\\[Conformance\\]"
		;;
	shard-test)
		FOCUS=$(echo ${@:2} | sed 's/ /\\s/g')
		;;
	*)
		echo "unknown shard"
		exit 1
	;;
esac

# setting this env prevents ginkgo e2e from trying to run provider setup
export KUBERNETES_CONFORMANCE_TEST='y'
# setting these is required to make RuntimeClass tests work ... :/
export KUBE_CONTAINER_RUNTIME=remote
export KUBE_CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock
export KUBE_CONTAINER_RUNTIME_NAME=containerd
NUM_NODES=2
./hack/ginkgo-e2e.sh \
'--provider=skeleton' "--num-nodes=${NUM_NODES}" \
"--ginkgo.focus=${FOCUS}" "--ginkgo.skip=${SKIPPED_TESTS}" \
"--report-dir=${E2E_REPORT_DIR}" '--disable-log-dump=true'
