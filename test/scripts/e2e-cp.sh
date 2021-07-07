#!/usr/bin/env bash

set -ex

# setting this env prevents ginkgo e2e from trying to run provider setup
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${HOME}/admin.conf

# Skip tests which are not IPv6 ready yet (see description of https://github.com/ovn-org/ovn-kubernetes/pull/2276)
IPV6_SKIPPED_TESTS="Should be allowed by externalip services|\
should provide connection to external host by DNS name from a pod|\
should provide Internet connection continuously when master is killed|\
should provide Internet connection continuously when ovn-k8s pod is killed|\
Should validate connectivity from a pod to a non-node host address on same node|\
Should validate connectivity to an external gateway\'s loopback address via a pod with external gateway annotations enabled|\
Should validate connectivity to multiple external gateways for an ECMP scenario|\
Should validate connectivity without vxlan before and after updating the namespace annotation to a new external gateway|\
Should validate ICMP connectivity to an external gateway\'s loopback address via a pod with external gateway annotations enabled|\
Should validate ICMP connectivity to multiple external gateways for an ECMP scenario|\
Should validate ingress connectivity from an external gateway|\
Should validate NetFlow data of br-int is sent to an external gateway|\
Should validate TCP/UDP connectivity to an external gateway\'s loopback address via a pod with external gateway annotations enabled|\
Should validate TCP/UDP connectivity to multiple external gateways for a UDP / TCP scenario|\
Should validate the egress firewall policy functionality against remote hosts|\
Should validate the egress IP functionality against remote hosts|\
recovering from deleting db files while maintain connectivity"

SKIPPED_TESTS=""
if [ "$KIND_IPV4_SUPPORT" == true ] && [ "$KIND_IPV6_SUPPORT" == true ]; then
    # No support for these features in dual-stack yet
    SKIPPED_TESTS="hybrid.overlay|external.gateway"
fi

if [ "$OVN_HA" == false ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
  	SKIPPED_TESTS+="|"
  fi
  # No support for these features in no-ha mode yet
  # TODO streamline the db delete tests
  SKIPPED_TESTS+="recovering from deleting db files while maintain connectivity|\
Should validate connectivity before and after deleting all the db-pods at once in HA mode"
else 
  if [ "$SKIPPED_TESTS" != "" ]; then
  	SKIPPED_TESTS+="|"
  fi

  SKIPPED_TESTS+="Should validate connectivity before and after deleting all the db-pods at once in Non-HA mode"
fi

if [ "$KIND_IPV6_SUPPORT" == true ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
  	SKIPPED_TESTS+="|"
  fi
  # No support for these tests in IPv6 mode yet
  SKIPPED_TESTS+=$IPV6_SKIPPED_TESTS
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
        ${CONTAINER_RUNTIME:+"--container-runtime=${CONTAINER_RUNTIME}"} \
        ${NUM_NODES:+"--num-nodes=${NUM_NODES}"} \
        ${E2E_REPORT_DIR:+"--report-dir=${E2E_REPORT_DIR}"} \
        ${E2E_REPORT_PREFIX:+"--report-prefix=${E2E_REPORT_PREFIX}"}
popd
