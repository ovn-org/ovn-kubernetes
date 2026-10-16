# OKEP-23694: Egress Gateway

* Tracking issue: [CNF-23694](https://redhat.atlassian.net/browse/CNF-23694)
* Upstream tracking: TODO

## Problem Statement

Cluster administrators need to route pod north-south egress traffic through
designated nodes that provide required external connectivity, such as BGP
sessions and allowlisted node IPs. Ordinary egress through workload nodes does
not enforce these required paths and source addresses.

## Goals

- Enable administrators to route all pod egress traffic (north-south) through
  designated gateway nodes cluster-wide.
- SNAT each flow to the IP of the forwarding gateway node's outgoing interface.
- Distribute pod egress flows across healthy gateway nodes using ECMP.
- Add or remove gateway nodes from ECMP forwarding based on health checks.
- Preserve EgressService and EgressIP priority and behavior.

## Non-Goals

- Provide a floating virtual SNAT IP that moves between gateway nodes.
- Preserve established connections across gateway membership changes.
- Select individual workloads to use or bypass egress-gateway routing.
- Select different gateway nodes for each UDN.
- Replace EgressIP or EgressService.

## Introduction

Centralized egress ensures pod traffic exits through administrator-designated
gateway nodes with the required upstream connectivity and approved source IPs.
The feature is enabled cluster-wide; when disabled, pod egress retains its
existing routing behavior.

## User-Stories/Use-Cases

As a cluster administrator, I want pod egress traffic not handled by
EgressService or EgressIP to leave through designated gateway nodes so that
external systems see approved node IP addresses and traffic is subject to
centralized controls.

As a cluster administrator, I want pod egress flows distributed across healthy
gateway nodes to use their combined forwarding capacity.

As a cluster administrator, I want new connections to use healthy gateways
after a gateway failure, with applications reconnecting disrupted flows.

## Proposed Solution

Introduce an optional, cluster-wide Egress Gateway configuration. When enabled,
pod north-south traffic is distributed using ECMP across the healthy
nodes marked by the cluster administrator with the fixed
`k8s.ovn.org/egress-gateway` node label.

The default network and primary UDNs share this gateway-node selection. Each
network has its own forwarding, SNAT, and connection-tracking state. Secondary
pod network attachments are outside this proposal's scope. The intended IP
family support is IPv4, IPv6, and dual-stack; datapath validation is pending.

### API Details

This proposal does not introduce a new CRD. Add the boolean
`--enable-egress-gateway` OVN-Kubernetes configuration option. It defaults to
`false`.

Administrators configure the feature in this order:

1. Mark one or more gateway candidates with the fixed
   `k8s.ovn.org/egress-gateway` label.

   ```bash
   kubectl label node <gateway-node>... k8s.ovn.org/egress-gateway=
   ```

2. Enable Egress Gateway in the deployment configuration. For Helm:

   ```yaml
   global:
     enableEgressGateway: true
   ```

The chart will pass `--enable-egress-gateway` to components running cluster
manager, OVN network controllers, and node gateway logic. Initial enablement
without any labeled gateway candidates is invalid. 

Cluster manager owns the `k8s.ovn.org/egress-gateway-ready` node annotation.
It writes `"true"` for healthy gateway nodes and `"false"` for unhealthy ones.
OVN controllers include a node in ECMP only when it has the gateway label and
the annotation is `"true"`.

### Implementation Details

- Cluster manager maintains the healthy gateway set using the existing
  EgressIP readiness and reachability checks. It publishes gateway eligibility
  through the node annotation. The probe server must also run when Egress
  Gateway is enabled.
- OVN controllers health checks gateway nodes and update ECMP route policies
  with lower priority from EgressService and EgressIP policies.
- Node components configure host SNAT (all pod network) for Egress Gateway
  traffic in local gateway mode. OVN controllers configure gateway-router SNAT
  in shared gateway mode. Both use the outgoing-interface IP that is the default
  behavior.


#### Packet Path in LocalGateway

- Pod `10.128.1.10` runs on source node A `192.0.2.10`.
- Gateway node B `192.0.2.20` uses outgoing interface `eth1` (`198.51.100.20`).
- External server: `203.0.113.80:443`.
- Cluster pod subnet: `10.128.0.0/14`.

```text
# Pod sends a TCP SYN.
pod/eth0 Out IP 10.128.1.10.40000 > 203.0.113.80.443: Flags [S]

  [OVN egress policy selection: pod, service, machine, and link-local networks destinations excluded first]
  Priority 101: EgressService LoadBalancerIP -> reroute to the service's selected node.
  Priority 100: EgressIP -> reroute to a node holding an assigned EgressIP.
  Priority 90: Egress Gateways -> ECMP reroute to healthy gateway 192.0.2.20.
source-node/br-int Out IP 10.128.1.10.40000 > 203.0.113.80.443: Flags [S]

  [Geneve tunnel: 192.0.2.10 -> 192.0.2.20]
gateway-node/br-int In IP 10.128.1.10.40000 > 203.0.113.80.443: Flags [S]

# OVS delivers the packet to Linux.
gateway-node/ovn-k8s-mp0 In IP 10.128.1.10.40000 > 203.0.113.80.443: Flags [S]

  [Existing Linux routing]
  Route to 203.0.113.80 through outgoing interface eth1.

  [Extend Masquerade rule installed before ECMP admission to accept full pod network]
  Existing POSTROUTING priority 101 runs after EgressService/EgressIP SNAT.
  ip saddr 10.128.0.0/14 masquerade

  SOURCE CHANGES:
    10.128.1.10:40000 -> 198.51.100.20:40000

# Packet leaves the gateway, SNATed, contrack records the translation.
gateway-node/eth1 Out IP 198.51.100.20.40000 > 203.0.113.80.443: Flags [S]

# External server replies.
gateway-node/eth1 In IP 203.0.113.80.443 > 198.51.100.20.40000: Flags [S.]

  [Linux conntrack/NAT: reverse the recorded SNAT in PREROUTING]
  DESTINATION CHANGES:
    198.51.100.20:40000 -> 10.128.1.10:40000
  No separate reply SNAT rule is needed.

  [Linux routing]
  Route the restored pod destination through its management port.

gateway-node/ovn-k8s-mp0 Out IP 203.0.113.80.443 > 10.128.1.10.40000: Flags [S.] mark: 0x0

  [Geneve tunnel: 192.0.2.20 -> 192.0.2.10]

pod/eth0 In IP 203.0.113.80.443 > 10.128.1.10.40000: Flags [S.] mark: 0x0
```

### Testing Details

E2E tests cover both local and shared gateway modes on the default network,
with IPv4, IPv6, and dual-stack:

- Verify egress uses multiple healthy gateways and each outgoing interface's
  IP for SNAT, including secondary NICs. Established flows must keep their
  gateway while ECMP membership is unchanged.
- Add and remove gateway labels, then fail and restore health checks. Verify
  new flows use the updated healthy gateway set after convergence.
- Verify eligible egress is dropped when all gateways are unavailable and
  resumes when a gateway recovers.
- Verify pod-to-pod, ClusterIP, and node-IP traffic is unchanged, and
  NetworkPolicy and EgressFirewall restrictions still apply.
- Verify EgressIP and Egress Gateway on the same node use EgressIP SNAT for
  selected traffic and gateway SNAT otherwise. Verify EgressService
  `LoadBalancerIP` SNAT takes precedence over EgressIP, and EgressService
  `Network` routing and SNAT are unchanged.
- Restart controllers with healthy gateways and with none available; verify
  forwarding and drop behavior is preserved.
- Disable the feature; verify ordinary egress resumes and feature rules are
  removed. Keep existing E2E coverage with the feature disabled.

Scale testing will vary gateway, pod, and flow counts, measuring OVN
policy growth, health-check load, and convergence after membership changes.
Target sizes and acceptable convergence times remain to be agreed.

### Documentation Details

Add user documentation to `docs/features/cluster-egress-controls/egress-gateway.md`
covering configuration, gateway-node requirements, UDN setup, traffic
exclusions, local/shared packet paths, failover, and disabling the feature.
Document precedence and coexistence with EgressIP and EgressService, with
troubleshooting steps for no healthy gateways and incomplete network setup.

## Risks, Known Limitations and Mitigations

### Rebalancing when ECMP set changes

Flows remain assigned to their selected gateway while ECMP membership is
unchanged. Adding, removing, or recovering a gateway may remap established
flows, including flows using healthy gateways. Remapped connections may break
because their SNAT source address changes. Preserving established connections
across membership changes is outside this proposal's scope.

### Gateway Failover

> **Note:** This provides loose gateway failover, not end-to-end high
> availability. It does not preserve connections assigned to a failed gateway
> or detect failures of the external interface, host routing, or upstream path.

Cluster manager uses the existing EgressIP health-check mechanism to determine
which candidates are eligible. A candidate must be `NodeReady` and reachable
through its management IP. Cluster manager checks reachability every five
seconds by using the configured EgressIP TCP or gRPC health check. Failover is
therefore expected on a seconds timescale, not a subsecond timescale, and
includes failure detection, controller reconciliation, and OVN convergence.

Recovered gateways are marked ready and rejoin each network's ECMP set once
that network's gateway datapath is ready.

### Gateway node sizing

Gateway nodes aggregate egress traffic from across the cluster and need
sufficient CPU, memory, and NIC bandwidth to handle the expected load. Prefer
dedicated nodes to avoid resource contention with application workloads. These
nodes must participate in OVN but they do no really need to host k8s workloads.

## Backwards Compatibility / Toggle Feature

The feature is opt-in. Clusters with Egress Gateway disabled keep their
existing routing and SNAT behavior. Existing EgressIP and EgressService APIs
retain their semantics and test coverage. Enabling the feature changes source
addresses and paths for egress traffic, so applications must reconnect flows
disrupted by the transition.

## Alternatives

- Gateway appliances: add devices in between OCP and PE.

## EgressIP and EgressService Interaction

Egress Gateway overlaps with EgressIP and EgressService in:

- Node selection, readiness, and health checks.
- Traffic steering and rule reconciliation.
- SNAT configuration and failure handling.

Further alignment could introduce a common gateway node label and a single
egress configuration option that enables the features together, instead of
independent feature toggles. Each feature would retain its traffic-selection
and SNAT behavior. A unified configuration would simplify deployment,
documentation, and testing by reducing the supported configuration
combinations.

## References

- [CNF-23694](https://redhat.atlassian.net/browse/CNF-23694)
- [VMware NSX-T gateway NAT](https://techdocs.broadcom.com/us/en/vmware-cis/nsx/nsxt-dc/3-2/administration-guide/network-address-translation.html):
  centralized egress SNAT on Tier-0/Tier-1 gateways in active-standby mode.
- [Dynamic UDN Node Allocation](okep-5552-dynamic-udn-node-allocation.md)
- [EgressIP Node Selector](okep-6800-egressip-node-selector.md)
- [Per-destination EgressIP proposal](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6228)

## Items to Be Investigated

- Investigate Egress Gateway compatibility with UDNs.
