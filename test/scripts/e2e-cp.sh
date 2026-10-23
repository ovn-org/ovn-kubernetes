#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0


set -ex

# setting this env prevents ginkgo e2e from trying to run provider setup
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${KUBECONFIG:-${HOME}/ovn.conf}

# Skip tests which are not IPv6 ready yet (see description of https://github.com/ovn-kubernetes/ovn-kubernetes/pull/2276)
# (Note that netflow v5 is IPv4 only)
# NOTE: Some of these tests that check connectivity to internet cannot be run.
#       See https://github.com/actions/runner-images/issues/668#issuecomment-1480921915 for details
# There were some past efforts to re-enable some of these skipped tests, but that never happened and they are
# still failing v6 lane: https://github.com/ovn-kubernetes/ovn-kubernetes/pull/2505,
# https://github.com/ovn-kubernetes/ovn-kubernetes/pull/2524, https://github.com/ovn-kubernetes/ovn-kubernetes/pull/2287; so
# going to skip them again.
# TODO: Fix metalLB integration with KIND on IPV6 in LGW mode and enable those service tests.See
# https://github.com/ovn-kubernetes/ovn-kubernetes/issues/4131 for details.
# TODO: Fix EIP tests. See https://github.com/ovn-kubernetes/ovn-kubernetes/issues/4130 for details.
# TODO: Fix MTU tests. See https://github.com/ovn-kubernetes/ovn-kubernetes/issues/4160 for details.
IPV6_SKIPPED_TESTS="Should be allowed by externalip services|\
should provide connection to external host by DNS name from a pod|\
should provide Internet connection continuously when ovnkube-node pod is killed|\
should provide Internet connection continuously when pod running leader instance of ovnkube-control-plane is killed|\
should provide Internet connection continuously when all pods are killed on node running leader instance of ovnkube-control-plane|\
should provide Internet connection continuously when all ovnkube-control-plane pods are killed|\
Should validate flow data of br-int is sent to an external gateway with netflow v5|\
should be able to receive multicast IGMP query|\
test node readiness according to its defaults interface MTU size|\
Pod to pod TCP with low MTU|\
queries to the hostNetworked server pod on another node shall work for TCP|\
queries to the hostNetworked server pod on another node shall work for UDP|\
ipv4 pod"

SKIPPED_TESTS=""
skip() {
  if [ "$SKIPPED_TESTS" != "" ]; then
  	SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+=$*
}

LABELED_TESTS=""
skip_label() {
  if [ "$LABELED_TESTS" != "" ]; then
  	LABELED_TESTS+=" && "
  fi
  LABELED_TESTS+="!($*)"
}

require_label() {
  if [ "$LABELED_TESTS" != "" ]; then
  	LABELED_TESTS+=" && "
  fi
  LABELED_TESTS+="$*"
}

if [ "$PLATFORM_IPV4_SUPPORT" == true ]; then
  if  [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
	  # No support for these features in dual-stack yet
	   skip "hybrid.overlay"
  else
	  # Skip sflow in IPv4 since it's a long test (~5 minutes)
	  # We're validating netflow v5 with an ipv4 cluster, sflow with an ipv6 cluster
	  skip "Should validate flow data of br-int is sent to an external gateway with sflow|ipv6 pod"
  fi
fi

if [ "$PLATFORM_IPV4_SUPPORT" == false ]; then
  skip "\[IPv4\]"
fi

if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
  # No support for these tests in IPv6 mode yet
  skip $IPV6_SKIPPED_TESTS
fi

if [ "$OVN_DISABLE_SNAT_MULTIPLE_GWS" == false ]; then
  skip "e2e multiple external gateway stale conntrack entry deletion validation"
fi

if [ "$OVN_GATEWAY_MODE" == "shared" ]; then
  # See https://github.com/ovn-kubernetes/ovn-kubernetes/issues/4138 for details
  skip "Should ensure load balancer service|LGW"
fi

if [ "$OVN_GATEWAY_MODE" == "local" ]; then
  # See https://github.com/ovn-kubernetes/ovn-kubernetes/labels/ci-ipv6 for details
  if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    skip "Should be allowed by nodeport services"
    skip "Should successfully create then remove a static pod"
    skip "Should validate connectivity from a pod to a non-node host address on same node"
    skip "Should validate connectivity within a namespace of pods on separate nodes"
    skip "Services"
  fi
fi

# skipping the egress ip legacy health check test because it requires two
# sequenced rollouts of OVN-Kubernetes control plane components that take a lot
# of time.
skip "disabling egress nodes impeding Legacy health check"

if [ "$ENABLE_MULTI_NET" != "true" ]; then
  skip "Multi Homing"
fi

if [ "$OVN_NETWORK_QOS_ENABLE" != "true" ]; then
  skip "e2e NetworkQoS validation"
fi

if [ "${OVN_ENABLE_STATUS_METRICS}" != "true" ]; then
  skip "Status metrics mode"
fi

# Only run Node IP/MAC address migration tests if they are explicitly requested
IP_MIGRATION_TESTS="Node IP and MAC address migration"
if [[ "${WHAT}" != "${IP_MIGRATION_TESTS}"* ]]; then
  skip "Node IP and MAC address migration"
fi

# Only run external gateway tests if they are explicitly requested
EXTERNAL_GATEWAY_TESTS="External Gateway"
if [[ "${WHAT}" != "${EXTERNAL_GATEWAY_TESTS}"* ]]; then
  skip "External Gateway"
fi

# Only run kubevirt virtual machines tests if they are explicitly requested
KV_LIVE_MIGRATION_TESTS="Kubevirt Virtual Machines"
if [[ "${WHAT}" != "${KV_LIVE_MIGRATION_TESTS}"* ]]; then
  skip $KV_LIVE_MIGRATION_TESTS
fi

# Only run Localnet DHCP IPAM tests if they are explicitly requested: they
# need the dhcp CNI plugin binary on the nodes (KIND_INSTALL_PLUGINS) and
# NetworkQoS enabled. Contains-match, not prefix: the requesting lane carries
# this suite as a focus alternation ("Kubevirt Virtual Machines|Localnet DHCP
# IPAM"), so the suite name is not at the start of WHAT.
DHCP_IPAM_TESTS="Localnet DHCP IPAM"
if [[ "${WHAT}" != *"${DHCP_IPAM_TESTS}"* ]]; then
  skip_label "Feature:DHCPIPAM"
fi

# Select all Uplink tests by feature label. These tests span network
# segmentation and route advertisement suites, with individual specs deciding
# whether to use or skip DPU-specific behavior.
# Only run network segmentation tests if they are explicitly requested. Route
# advertisement lanes are the exception: allow tests in the intersection of
# network segmentation and route advertisements when both features are enabled.
UPLINK_TESTS="Uplink"
DPU_TESTS="DPU"
NETWORK_SEGMENTATION_TESTS="Network Segmentation"
if [[ "${WHAT}" = "${UPLINK_TESTS}" ]]; then
  require_label "Feature:Uplink"
  shift # don't "focus" on Uplink since we filter by label
elif [[ "${WHAT}" = "${DPU_TESTS}" ]]; then
  # Feature:DPU specs need the dpu-simulator layout and the Uplink e2e environment,
  # and disrupt a host node, so the dpu-sim no-overlay lane runs them after Uplink.
  require_label "Feature:DPU"
  shift # don't "focus" on DPU since we filter by label
elif [[ "${WHAT}" = "${NETWORK_SEGMENTATION_TESTS}"* ]]; then
  require_label "Feature:NetworkSegmentation"
  shift # don't "focus" on Network Segmentation since we filter by label
elif [[ "${WHAT}" != "${NETWORK_SEGMENTATION_TESTS}"* ]]; then
  if [ "$ENABLE_NETWORK_SEGMENTATION" == true ] && [ "$ENABLE_ROUTE_ADVERTISEMENTS" == true ]; then
    skip_label "Feature:NetworkSegmentation && !(Feature:RouteAdvertisements)"
  else
    skip_label "Feature:NetworkSegmentation"
  fi
fi

# Network Segmentation suite selection uses a feature label and removes the
# suite prefix from the positional arguments. Keep any text after that prefix
# as a narrower Ginkgo focus.
if [[ "${WHAT}" == "${NETWORK_SEGMENTATION_TESTS} "* ]]; then
  set -- "${WHAT#"${NETWORK_SEGMENTATION_TESTS} "}"
fi

# Only run cluster network connect tests if they are explicitly requested
# To conserve CI resources, we run these tests as part of the network segmentation tests
CLUSTER_NETWORK_CONNECT_TESTS="ClusterNetworkConnect"
if [[ "${WHAT}" != "${CLUSTER_NETWORK_CONNECT_TESTS}"* ]]; then
  skip $CLUSTER_NETWORK_CONNECT_TESTS
fi

SERIAL_LABEL="Serial"
if [[ "${WHAT}" = "$SERIAL_LABEL" ]]; then
  require_label "$SERIAL_LABEL"
  shift # don't "focus" on Serial since we filter by label
fi

if [ "$ENABLE_UPLINK" != true ]; then
  skip_label "Feature:Uplink"
fi

if [ "$ENABLE_EVPN" != true ]; then
  skip_label "Feature:EVPN"
fi

if [ "$ENABLE_NO_OVERLAY" != true ]; then
  skip_label "Feature:NoOverlay"
fi

# RouteAdvertisements tests over dynamically allocated UDNs require a cluster
# with both route advertisements and dynamic UDN allocation enabled.
if [ "$ENABLE_ROUTE_ADVERTISEMENTS" != true ] || [ "$DYNAMIC_UDN_ALLOCATION" != true ]; then
  skip_label "Feature:RouteAdvertisementsDynamicUDN"
fi

# The "BGP: For BGP configured networks" tests (VRF-Lite and EVPN CUDNs) only
# run in local gateway mode, while dynamic UDN allocation is only covered in
# shared gateway mode lanes, so the two never overlap in CI. Skip them
# explicitly on dynamic UDN clusters regardless: they set up and expect
# per-network topology on every node, which does not hold with dynamic UDN
# allocation (okep-5552).
if [ "$DYNAMIC_UDN_ALLOCATION" == true ]; then
  skip "BGP: For BGP configured networks"
fi

if [ "$ENABLE_ROUTE_ADVERTISEMENTS" != true ]; then
  skip_label "Feature:RouteAdvertisements"
else
  if [ "$ADVERTISE_DEFAULT_NETWORK" = true ]; then
    # Some test don't work when the default network is advertised, either because
    # the configuration that the test excercises does not make sense for an advertised network, or
    # there is some bug or functional gap
    # call out case by case

    # pod reached from default network through secondary interface, asymetric, configuration does not make sense
    # TODO: perhaps the secondary network attached pods should not be attached to default network
    skip "Multi Homing A single pod with an OVN-K secondary network attached to a localnet network mapped to external primary interface bridge can be reached by a client pod in the default network on the same node"
    skip "Multi Homing A single pod with an OVN-K secondary network attached to a localnet network mapped to external primary interface bridge can be reached by a client pod in the default network on a different node"
    if [ "$PLATFORM_IPV6_SUPPORT" == true ] && [ "$PLATFORM_IPV4_SUPPORT" == false ]; then
      # Skip all Multi Homing tests in BGP IPv6 only mode
      # TODO: The tests are doing weird static ipv4, ipv6, dualstack specific ginkgo entries instead of relying on
      # cluster family type to make a dynamic determination. These tests need to be refactored to be family-friendly
      # instead of assuming only single stack v4 or dualstack lanes exist.
      # https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5569
      skip "Multi Homing"
    fi
    if [ "$PLATFORM_IPV4_SUPPORT" == true ] && [ "$PLATFORM_IPV6_SUPPORT" == false ]; then
      # Skip IPv6/dual-stack multihoming secondary network tests in IPv4-only clusters.
      skip "Multi Homing.*L3 - routed - secondary network with IPv6 subnet"
      skip "Multi Homing.*L3 - routed - secondary network with a dual stack configuration"
    fi
    # these tests require metallb but the configuration we do for it is not compatible with the configuration we do to advertise the default network
    # TODO: consolidate configuration
    skip "Load Balancer Service Tests with MetalLB"
    skip "EgressService"

    # tests that specifically expect the node SNAT to happen
    # TODO: expect the pod IP where it makes sense
    skip "e2e egress firewall policy validation with external containers"
    skip "e2e egress IP validation on network of type Cluster Default \[OVN network\] Using different methods to disable a node's availability for egress Should validate the egress IP functionality against remote hosts"
    skip "e2e egress IP validation on network of type Cluster Default \[OVN network\] Should validate the egress IP SNAT functionality against host-networked pods"
    skip "e2e egress IP validation on network of type Cluster Default Should validate egress IP logic when one pod is managed by more than one egressIP object"
    skip "e2e egress IP validation on network of type Cluster Default Should re-assign egress IPs when node readiness / reachability goes down/up"
    skip "Pod to external server PMTUD when a client ovnk pod targeting an external server is created when tests are run towards the agnhost echo server queries to the hostNetworked server pod on another node shall work for UDP"
    skip "e2e egress IP validation on network of type Cluster Default Should handle EIP reassignment correctly on namespace and pod label updates, and EIP object updates"

    # https://issues.redhat.com/browse/OCPBUGS-55028
    skip "e2e egress IP validation on network of type Cluster Default \[secondary-host-eip\]"


    # https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5240
    skip "e2e control plane test node readiness according to its defaults interface MTU size should get node not ready with a too small MTU"

    # buggy tests that don't work in dual stack mode
    skip "Service Hairpin SNAT Should ensure service hairpin traffic is NOT SNATed to hairpin masquerade IP; GR LB"
    skip "Services when a nodePort service targeting a pod with hostNetwork:false is created when tests are run towards the agnhost echo service queries to the nodePort service shall work for TCP"
    skip "Services when a nodePort service targeting a pod with hostNetwork:true is created when tests are run towards the agnhost echo service queries to the nodePort service shall work for TCP"
    skip "Services when a nodePort service targeting a pod with hostNetwork:false is created when tests are run towards the agnhost echo service queries to the nodePort service shall work for UDP"
    skip "Services when a nodePort service targeting a pod with hostNetwork:true is created when tests are run towards the agnhost echo service queries to the nodePort service shall work for UDP"
  fi
fi

# if we set PARALLEL=true, skip serial test
if [ "${PARALLEL:-false}" = "true" ]; then
  export GINKGO_PARALLEL=y
  export GINKGO_PARALLEL_NODES=10
  skip_label "$SERIAL_LABEL"
fi

if [ "$ENABLE_NO_OVERLAY" == true ]; then
  # No-overlay mode uses underlying network infrastructure directly.
  # Overlay-dependent features are not supported.
  skip_label "Feature:Multicast"
  skip_label "Feature:EgressIP"
  skip_label "Feature:EgressService"
  # This test validates MTU reduction behavior specific to overlay mode (1500->1400).
  # In no-overlay mode, pods use the full underlying network MTU without reduction.
  skip "blocking ICMP needs frag"
  # This test validates MTU reduction due to Geneve encapsulation overhead (1400->1342).
  # In no-overlay mode, there is no encapsulation and thus no MTU overhead.
  skip "Pod to pod TCP with low MTU"
fi

# setting these is required to make RuntimeClass tests work ... :/
export KUBE_CONTAINER_RUNTIME=remote
export KUBE_CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock
export KUBE_CONTAINER_RUNTIME_NAME=containerd
export NUM_NODES=2

FOCUS=$(echo "${@:1}" | sed 's/ /\\s/g')

# Ginkgo test timeout needs to be lower than both github's timeout and go test
# timeout to be able to get proper Ginkgo output when it happens.
TEST_TIMEOUT=${TEST_TIMEOUT:-180}
GO_TEST_TIMEOUT=$((TEST_TIMEOUT + 5))

pushd e2e

go mod download

if [ "$ENABLE_EVPN" = true ] && [[ "${WHAT}" != "${KV_LIVE_MIGRATION_TESTS}"* ]]; then
  # EVPN tests are parallel-safe (unique per-test resource names, randomized
  # subnets). Use the ginkgo CLI so that -procs=3 spawns 3 coordinated worker
  # processes — one per DescribeTable entry.  go test cannot drive Ginkgo
  # parallelism directly; only the ginkgo binary can start the sync server and
  # fan out the worker processes.
  ginkgo run \
        -procs=3 \
        -v \
        --focus="${FOCUS:-.}" \
        --fail-on-empty \
        --timeout="${TEST_TIMEOUT}m" \
        --flake-attempts="${FLAKE_ATTEMPTS:-2}" \
        --skip="${SKIPPED_TESTS}" \
        ${LABELED_TESTS:+--label-filter="${LABELED_TESTS}"} \
        --junit-report="${E2E_REPORT_DIR}/junit_${E2E_REPORT_PREFIX}report.xml" \
        . \
        -- \
        -provider skeleton \
        -kubeconfig "${KUBECONFIG}" \
        ${NUM_NODES:+"--num-nodes=${NUM_NODES}"} \
        ${E2E_REPORT_DIR:+"--report-dir=${E2E_REPORT_DIR}"}
else
  go test -test.timeout ${GO_TEST_TIMEOUT}m -v . \
          -ginkgo.v \
          -ginkgo.focus ${FOCUS:-.} \
          -ginkgo.fail-on-empty \
          -ginkgo.timeout ${TEST_TIMEOUT}m \
          -ginkgo.flake-attempts ${FLAKE_ATTEMPTS:-2} \
          -ginkgo.skip="${SKIPPED_TESTS}" \
          ${LABELED_TESTS:+-ginkgo.label-filter="${LABELED_TESTS}"} \
          -ginkgo.junit-report=${E2E_REPORT_DIR}/junit_${E2E_REPORT_PREFIX}report.xml \
          -provider skeleton \
          -kubeconfig ${KUBECONFIG} \
          ${NUM_NODES:+"--num-nodes=${NUM_NODES}"} \
          ${E2E_REPORT_DIR:+"--report-dir=${E2E_REPORT_DIR}"}
fi
popd
