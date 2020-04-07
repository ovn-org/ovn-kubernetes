#!/usr/bin/env bash

set -ex

SHARD=$1

pushd $GOPATH/src/k8s.io/kubernetes/
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${HOME}/admin.conf
export MASTER_NAME=${KIND_CLUSTER_NAME}-control-plane
export NODE_NAMES=${MASTER_NAME}

#REMOVE skip for "ingress access from updated pod" when k8s is updated to 1.18
SKIPPED_TESTS='Networking\sIPerf\sIPv[46]|\[Feature:PerformanceDNS\]|\[Feature:IPv6DualStackAlphaFeature\]|\[Feature:NoSNAT\]|Services.+(ESIPP|cleanup\sfinalizer|session\saffinity)|\[Feature:Networking-IPv6\]|\[Feature:Federation\]|configMap\snameserver|ClusterDns\s\[Feature:Example\]|kube-proxy|should\sset\sTCP\sCLOSE_WAIT\stimeout|EndpointSlices|named\sport.+\[Feature:NetworkPolicy\]|should\sallow\singress\saccess\sfrom\supdated\spod.+\[Feature:NetworkPolicy\]'
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

