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
- Source NAT (SNAT) each flow to the IP of the forwarding gateway node's
  outgoing interface. This proposal uses SNAT as the general term, including
  masquerading.
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
network maintains separate forwarding, SNAT, and connection-tracking state.
Secondary pod network attachments are outside this proposal's scope.
IPv4, IPv6, and dual-stack support require datapath validation.

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
manager, OVN network controllers, and node gateway logic. If no gateway is
ready for a network, it uses its ordinary egress routing and SNAT.

Cluster manager owns the `k8s.ovn.org/egress-gateway-ready` node annotation.
It writes `"true"` for healthy gateway nodes and `"false"` for unhealthy ones.
OVN controllers include a node in ECMP only when it has the gateway label and
the annotation is `"true"`.

### Implementation Details

- Cluster manager maintains the healthy gateway set using the existing
  EgressIP readiness and reachability checks. It publishes node health
  through the node annotation. The probe server must also run when Egress
  Gateway is enabled.
- OVN controllers consume gateway health results and update ECMP route policies
  with lower priority than EgressService and EgressIP policies.
- Node components configure Linux masquerade for Egress Gateway traffic from
  all pod networks in local gateway mode. OVN controllers configure
  gateway-router SNAT with an explicit external IP in shared gateway mode.
  Both use the outgoing-interface IP. Linux masquerade follows interface IP
  changes automatically; OVN SNAT requires the controller to update the
  external IP when the interface IP changes.

#### Packet Path in LocalGateway

- Pod `10.128.1.10` runs on source node A `192.0.2.10`.
- Gateway node B `192.0.2.20` uses outgoing interface `eth1` (`198.51.100.20`).
- External server: `203.0.113.80:443`.
- Cluster pod subnet: `10.128.0.0/14`.

```text
# Pod sends a TCP SYN.
pod/eth0 Out IP 10.128.1.10.40000 > 203.0.113.80.443: Flags [S]

  [OVN cluster-router policy selection: pod, service, machine, and link-local destinations excluded first]
  Priority 101: EgressService LoadBalancerIP -> reroute to the service's selected node.
  Priority 100: EgressIP -> reroute to a node holding an assigned EgressIP.
  Priority 90: Egress Gateway -> ECMP reroute to healthy node B's management-port IP.
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

# Packet leaves the gateway, SNATed; conntrack records the translation.
gateway-node/eth1 Out IP 198.51.100.20.40000 > 203.0.113.80.443: Flags [S]

# External server replies.
gateway-node/eth1 In IP 203.0.113.80.443 > 198.51.100.20.40000: Flags [S.]

  [Linux conntrack/NAT: reverse the recorded SNAT in PREROUTING]
  DESTINATION CHANGES:
    198.51.100.20:40000 -> 10.128.1.10:40000
  No separate reply SNAT rule is needed.

  [Linux routing]
  Route the restored pod destination using the existing global podCIDR
  route (10.128.0.0/14) through its management port.

gateway-node/ovn-k8s-mp0 Out IP 203.0.113.80.443 > 10.128.1.10.40000: Flags [S.] mark: 0x0

  [Geneve tunnel: 192.0.2.20 -> 192.0.2.10]

pod/eth0 In IP 203.0.113.80.443 > 10.128.1.10.40000: Flags [S.] mark: 0x0
```

### Testing Details

E2E tests cover both local and shared gateway modes on the default network
and primary UDNs, with IPv4, IPv6, and dual-stack:

- Verify egress uses multiple healthy gateways and each outgoing interface's
  IP for SNAT, including secondary NICs. Established flows must keep their
  gateway while ECMP membership is unchanged.
- Add and remove gateway labels, then fail and restore health checks. Verify
  new flows use the updated healthy gateway set after convergence.
- Verify ordinary egress routing and SNAT when no gateways are available, and
  gateway routing resumes when a gateway recovers.
- Verify pod-to-pod, ClusterIP, and node-IP traffic is unchanged, and
  NetworkPolicy and EgressFirewall restrictions still apply.
- Verify EgressIP and Egress Gateway on the same node use EgressIP SNAT for
  selected traffic and gateway SNAT otherwise. Verify EgressService
  `LoadBalancerIP` SNAT takes precedence over EgressIP, and EgressService
  `Network` routing and SNAT are unchanged.
- Restart controllers with healthy gateways and with none available; verify
  forwarding and fallback behavior is preserved.
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

- Gateway appliances: add devices between the Kubernetes bare-metal cluster
  and the Provider Edge routers.

### API alternatives

This OKEP proposes a cluster-wide feature flag instead of a new CR. A separate
CR could confuse users about when to use Egress Gateway versus EgressIP.
Cluster-wide configuration also simplifies implementation.

An alternative is a cluster-scoped `EgressGateway` CR. It
combines namespace and gateway-node selection.

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressGateway
metadata:
  name: cluster-egress
spec:
  from:
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: <test-namespace>
  gatewayNodeSelector:
    matchLabels:
      node-role.kubernetes.io/egress-gateway: ""
```

### Implement the feature using EgressIP

If pod selection is required, another approach would extend EgressIP to route
traffic through an egress node without specifying an EgressIP address. The
node would masquerade traffic to its outgoing-interface IP, following
`ip route get <destination>` in local gateway mode.
```
apiVersion: k8s.ovn.org/v1
  kind: EgressIP
  metadata:
    name: cluster-egress
  spec:
    egressIPs:
      - auto (or missing the egressIP)
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: <test-namespace>
    podSelector:
      matchLabels:
        app: example
```

This alternative was rejected to keep EgressIP and Egress Gateway separate
from the user's perspective, although they might share implementation.

### Implement the feature using MEG (Multiple External Gateways)

The egress-gateway feature could be implemented by extending MEG.

This approach uses the following MEG CR and host-networked gateway pods:

```
apiVersion: k8s.ovn.org/v1
  kind: AdminPolicyBasedExternalRoute
  metadata:
    name: via-gateway-pod
  spec:
    from:
      namespaceSelector:
        matchLabels:
          egress-gateway: "true"
          kubernetes.io/metadata.name: meg-client
    nextHops:
      dynamic:
      - bfdEnabled: false
        namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: egress-gateways
        networkAttachmentName: ""
        podSelector:
          matchLabels:
            app: egress-gateway
  status:
    status: Success

  apiVersion: apps/v1
  kind: DaemonSet
  metadata:
    name: egress-gateway
    namespace: egress-gateways
  spec:
    selector:
      matchLabels:
        app: egress-gateway
    template:
      metadata:
        labels:
          app: egress-gateway
     # Deploy host-networked gateway pods to add MASQUERADE rules for all podCIDRs.
     # Install custom return routes.
```

Earlier experiment: intended path with a custom return route on the gateway.
This is not the current MASQUERADE-only DaemonSet configuration.

```text
MEG with host-networked gateway pods
  # Client podIP:         10.131.1.6       (meg-client on mno-worker-0)
  # Client nodeIP:        10.10.30.233     (mno-worker-0)
  # Selected nodeIP:      10.10.30.230     (mno-master-0)
  # External gatewayIP:   10.10.30.1       (ToR)
  # Destination serverIP: 200.200.200.200 

meg-client/eth0 Out IP 10.131.1.6.43102 > 200.200.200.200.443: Flags [S] (podIP.43102 > serverIP.443)
    ECMP: GR_mno-worker-0 selects 10.10.30.230 (master-0 nodeIP).
mno-worker-0/br-ex Out IP 10.131.1.6.43102 > 200.200.200.200.443: Flags [S] (podIP.43102 > serverIP.443)
    Underlay
mno-master-0/br-ex In IP 10.131.1.6.43102 > 200.200.200.200.443: Flags [S] (podIP.43102 > serverIP.443)
    MASQUERADE: source 10.131.1.6 (podIP) → 10.10.30.230 (nodeIP).  # MASQUERADE rule added by gateway pod
mno-master-0/br-ex Out IP 10.10.30.230.43102 > 200.200.200.200.443: Flags [S] (nodeIP.43102 > serverIP.443)
ToR In IP 10.10.30.230.43102 > 200.200.200.200.443: Flags [S] (nodeIP.43102 > serverIP.443)
    Outside → server → reply.
ToR Out IP 200.200.200.200.443 > 10.10.30.230.43102: Flags [S.] (serverIP.443 > nodeIP.43102)
mno-master-0/br-ex In IP 200.200.200.200.443 > 10.10.30.230.43102: Flags [S.] (serverIP.443 > nodeIP.43102)
    Reverse NAT: destination 10.10.30.230 (nodeIP) → 10.131.1.6 (podIP).
    Custom return route required: via 10.10.30.233 (worker nodeIP), through br-ex; bypasses master mp0.
mno-master-0/br-ex Out IP 200.200.200.200.443 > 10.131.1.6.43102: Flags [S.] (serverIP.443 > podIP.43102)
    Underlay
mno-worker-0/br-ex In IP 200.200.200.200.443 > 10.131.1.6.43102: Flags [S.] (serverIP.443 > podIP.43102)
OVN → meg-client/eth0 In IP 200.200.200.200.443 > 10.131.1.6.43102: Flags [S.] (serverIP.443 > podIP.43102)
```

MEG gateway pods support custom health checks, resource requests, and reuse
of the existing selection mechanism. This OKEP rejects this approach because
it sends podIPs over the underlay and requires custom return routes to bypass
the existing pod-network route through the management port.

References: [MEG implementation](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/3462)

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
