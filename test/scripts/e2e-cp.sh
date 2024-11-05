#!/usr/bin/env bash

set -ex

# setting this env prevents ginkgo e2e from trying to run provider setup
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${KUBECONFIG:-${HOME}/ovn.conf}

# Skip tests which are not IPv6 ready yet (see description of https://github.com/ovn-org/ovn-kubernetes/pull/2276)
# (Note that netflow v5 is IPv4 only)
# NOTE: Some of these tests that check connectivity to internet cannot be run.
#       See https://github.com/actions/runner-images/issues/668#issuecomment-1480921915 for details
# There were some past efforts to re-enable some of these skipped tests, but that never happened and they are
# still failing v6 lane: https://github.com/ovn-org/ovn-kubernetes/pull/2505,
# https://github.com/ovn-org/ovn-kubernetes/pull/2524, https://github.com/ovn-org/ovn-kubernetes/pull/2287; so
# going to skip them again.
# TODO: Fix metalLB integration with KIND on IPV6 in LGW mode and enable those service tests.See
# https://github.com/ovn-org/ovn-kubernetes/issues/4131 for details.
# TODO: Fix EIP tests. See https://github.com/ovn-org/ovn-kubernetes/issues/4130 for details.
# TODO: Fix MTU tests. See https://github.com/ovn-org/ovn-kubernetes/issues/4160 for details.
IPV6_SKIPPED_TESTS="Should be allowed by externalip services|\
should provide connection to external host by DNS name from a pod|\
should provide Internet connection continuously when ovnkube-node pod is killed|\
should provide Internet connection continuously when pod running master instance of ovnkube-control-plane is killed|\
should provide Internet connection continuously when all pods are killed on node running master instance of ovnkube-control-plane|\
should provide Internet connection continuously when all ovnkube-control-plane pods are killed|\
Should validate flow data of br-int is sent to an external gateway with netflow v5|\
can retrieve multicast IGMP query|\
test node readiness according to its defaults interface MTU size|\
Pod to pod TCP with low MTU|\
queries to the hostNetworked server pod on another node shall work for TCP|\
queries to the hostNetworked server pod on another node shall work for UDP|\
ipv4 pod"

SKIPPED_TESTS=""

if [ "$KIND_IPV4_SUPPORT" == true ]; then
    if  [ "$KIND_IPV6_SUPPORT" == true ]; then
	# No support for these features in dual-stack yet
	SKIPPED_TESTS="hybrid.overlay"
    else
	# Skip sflow in IPv4 since it's a long test (~5 minutes)
	# We're validating netflow v5 with an ipv4 cluster, sflow with an ipv6 cluster
	SKIPPED_TESTS="Should validate flow data of br-int is sent to an external gateway with sflow|ipv6 pod"
    fi
fi

if [ "$KIND_IPV4_SUPPORT" == false ]; then
  SKIPPED_TESTS+="\[IPv4\]"
fi

if [ "$OVN_HA" == false ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
  	SKIPPED_TESTS+="|"
  fi
  # No support for these features in no-ha mode yet
  # TODO streamline the db delete tests
  SKIPPED_TESTS+="recovering from deleting db files while maintaining connectivity|\
Should validate connectivity before and after deleting all the db-pods at once in HA mode"
else 
  if [ "$SKIPPED_TESTS" != "" ]; then
  	SKIPPED_TESTS+="|"
  fi

  SKIPPED_TESTS+="Should validate connectivity before and after deleting all the db-pods at once in Non-HA mode|\
  e2e br-int NetFlow export validation"
fi

if [ "$KIND_IPV6_SUPPORT" == true ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
  	SKIPPED_TESTS+="|"
  fi
  # No support for these tests in IPv6 mode yet
  SKIPPED_TESTS+=$IPV6_SKIPPED_TESTS
fi

if [ "$OVN_DISABLE_SNAT_MULTIPLE_GWS" == false ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
    SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+="e2e multiple external gateway stale conntrack entry deletion validation"
fi

if [ "$OVN_GATEWAY_MODE" == "shared" ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
    SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+="Should ensure load balancer service|LGW"
  # See https://github.com/ovn-org/ovn-kubernetes/issues/4138 for details
fi

if [ "$OVN_GATEWAY_MODE" == "local" ]; then
  # See https://github.com/ovn-org/ovn-kubernetes/labels/ci-ipv6 for details:
  if [ "$KIND_IPV6_SUPPORT" == true ]; then
    if [ "$SKIPPED_TESTS" != "" ]; then
        SKIPPED_TESTS+="|"
    fi
    SKIPPED_TESTS+="Should be allowed by nodeport services|\
Should successfully create then remove a static pod|\
Should validate connectivity from a pod to a non-node host address on same node|\
Should validate connectivity within a namespace of pods on separate nodes|\
Services"
  fi
fi

# skipping the egress ip legacy health check test because it requires two
# sequenced rollouts of both ovnkube-node and ovnkube-master that take a lot of
# time.
SKIPPED_TESTS+="${SKIPPED_TESTS:+|}disabling egress nodes impeding Legacy health check"

if [ "$ENABLE_MULTI_NET" != "true" ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
    SKIPPED_TESTS+="|"
  fi
  # Network QoS feature depends on multi-homing to exercise secondary NAD
  SKIPPED_TESTS+="Multi Homing|\
e2e NetworkQoS validation"
fi

# Only run Node IP/MAC address migration tests if they are explicitly requested
IP_MIGRATION_TESTS="Node IP and MAC address migration"
if [[ "${WHAT}" != "${IP_MIGRATION_TESTS}"* ]]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
	SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+="Node IP and MAC address migration"
fi

# Only run Multi node zones interconnect tests if they are explicitly requested
MULTI_NODE_ZONES_TESTS="Multi node zones interconnect"
if [[ "${WHAT}" != "${MULTI_NODE_ZONES_TESTS}"* ]]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
	SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+="Multi node zones interconnect"
fi

# Only run external gateway tests if they are explicitly requested
EXTERNAL_GATEWAY_TESTS="External Gateway"
if [[ "${WHAT}" != "${EXTERNAL_GATEWAY_TESTS}"* ]]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
	SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+="External Gateway"
fi

# Only run kubevirt virtual machines tests if they are explicitly requested
KV_LIVE_MIGRATION_TESTS="Kubevirt Virtual Machines"
if [[ "${WHAT}" != "${KV_LIVE_MIGRATION_TESTS}"* ]]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
	SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+=$KV_LIVE_MIGRATION_TESTS
fi

# Only run network segmentation tests if they are explicitly requested
NETWORK_SEGMENTATION_TESTS="Network Segmentation"
if [[ "${WHAT}" != "${NETWORK_SEGMENTATION_TESTS}"* ]]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
	SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+=$NETWORK_SEGMENTATION_TESTS
fi

# setting these is required to make RuntimeClass tests work ... :/
export KUBE_CONTAINER_RUNTIME=remote
export KUBE_CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock
export KUBE_CONTAINER_RUNTIME_NAME=containerd
export NUM_NODES=2

FOCUS=$(echo ${@:1} | sed 's/ /\\s/g')

pushd e2e

go mod download
go test -test.timeout 180m -v . \
        -ginkgo.v \
        -ginkgo.focus ${FOCUS:-.} \
        -ginkgo.timeout 3h \
        -ginkgo.flake-attempts ${FLAKE_ATTEMPTS:-2} \
        -ginkgo.skip="${SKIPPED_TESTS}" \
        -ginkgo.junit-report=${E2E_REPORT_DIR}/junit_${E2E_REPORT_PREFIX}report.xml \
        -provider skeleton \
        -kubeconfig ${KUBECONFIG} \
        ${NUM_NODES:+"--num-nodes=${NUM_NODES}"} \
        ${E2E_REPORT_DIR:+"--report-dir=${E2E_REPORT_DIR}"}
popd
