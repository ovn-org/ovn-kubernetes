#!/usr/bin/env bash

set -ex

# setting this env prevents ginkgo e2e from trying to run provider setup
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${HOME}/ovn.conf

# Skip tests which are not IPv6 ready yet (see description of https://github.com/ovn-org/ovn-kubernetes/pull/2276)
# (Note that netflow v5 is IPv4 only)
IPV6_SKIPPED_TESTS="Should be allowed by externalip services|\
should provide connection to external host by DNS name from a pod|\
Should validate flow data of br-int is sent to an external gateway with netflow v5|\
test tainting a node according to its defaults interface MTU size|\
ipv4 pod"

SKIPPED_TESTS=""

if [ "$KIND_IPV4_SUPPORT" == true ]; then
    if  [ "$KIND_IPV6_SUPPORT" == true ]; then
	# No support for these features in dual-stack yet
	SKIPPED_TESTS="hybrid.overlay|external.gateway"
    else
	# Skip sflow in IPv4 since it's a long test (~5 minutes)
	# We're validating netflow v5 with an ipv4 cluster, sflow with an ipv6 cluster
	SKIPPED_TESTS="Should validate flow data of br-int is sent to an external gateway with sflow|ipv6 pod"
    fi
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
fi

# skipping the egress ip legacy health check test because it requires two
# sequenced rollouts of both ovnkube-node and ovnkube-master that take a lot of
# time.
SKIPPED_TESTS+="${SKIPPED_TESTS:+|}disabling egress nodes impeding Legacy health check"

if [ "$ENABLE_MULTI_NET" != "true" ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
    SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+="Multi Homing"
fi

# Only run Node IP address migration tests if they are explicitly requested
IP_MIGRATION_TESTS="Node IP address migration"
if [ "${WHAT}" != "${IP_MIGRATION_TESTS}" ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
	SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+="Node IP address migration"
fi

# Only run Multi node zones interconnect tests if they are explicitly requested
MULTI_NODE_ZONES_TESTS="Multi node zones interconnect"
if [ "${WHAT}" != "${MULTI_NODE_ZONES_TESTS}" ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
	SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+="Multi node zones interconnect"
fi

# Only run external gateway tests if they are explicitly requested
EXTERNAL_GATEWAY_TESTS="External Gateway"
if [ "${WHAT}" != "${EXTERNAL_GATEWAY_TESTS}" ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
	SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+="External Gateway"
fi

# Only run kubevirt virtual machines tests if they are explicitly requested
KV_LIVE_MIGRATION_TESTS="Kubevirt Virtual Machines"
if [ "${WHAT}" != "${KV_LIVE_MIGRATION_TESTS}" ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
	SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+=$KV_LIVE_MIGRATION_TESTS
fi

# setting these is required to make RuntimeClass tests work ... :/
export KUBE_CONTAINER_RUNTIME=remote
export KUBE_CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock
export KUBE_CONTAINER_RUNTIME_NAME=containerd
export NUM_NODES=2

FOCUS=$(echo ${@:1} | sed 's/ /\\s/g')

pushd e2e

go mod download
go test -timeout=0 -v . \
        -ginkgo.v \
        -ginkgo.focus ${FOCUS:-.} \
        -ginkgo.flakeAttempts ${FLAKE_ATTEMPTS:-2} \
        -ginkgo.skip="${SKIPPED_TESTS}" \
        -provider skeleton \
        -kubeconfig ${KUBECONFIG} \
        ${NUM_NODES:+"--num-nodes=${NUM_NODES}"} \
        ${E2E_REPORT_DIR:+"--report-dir=${E2E_REPORT_DIR}"} \
        ${E2E_REPORT_PREFIX:+"--report-prefix=${E2E_REPORT_PREFIX}"}
popd
