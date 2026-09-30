# OKEP-7040: BGP NodeSelector for PodNetwork RouteAdvertisements

* Issue: [#7040](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/7040)

## Problem Statement

The `RouteAdvertisements` CRD currently requires `nodeSelector` to select all
nodes when `PodNetwork` is included in `advertisements`. This prevents users
from designating a subset of nodes as BGP gateway nodes for pod network route
advertisements. Users need the ability to restrict BGP peering to specific
gateway nodes while maintaining full pod reachability from the external network.

## Goals

* Allow `RouteAdvertisements.spec.nodeSelector` to select a subset of **BGP gateway** nodes when
  `PodNetwork` is advertised, for the **default cluster network** and for
  **primary Layer 3** Cluster User Defined Networks (CUDNs). Layer 2 UDNs are
  **rejected** with non-empty `nodeSelector` (see scope matrix for rationale).
* Gateway nodes advertise pod subnets for **all** nodes so pods on **non-gateway** nodes remain
  reachable from the external provider network.
* Support **ingress** (external → pod) through gateway nodes in **shared gateway (SGW)** and
  **local gateway (LGW)** modes with **overlay (Geneve)** transport for all networks. EVPN
  (VXLAN) transport is not supported with non-empty `nodeSelector` (see scope matrix for details).
* Provide ECMP load-balancing and high availability across multiple gateway
  nodes for ingress traffic.
* **Support IPv4, IPv6, and dual-stack networks** for ingress forwarding paths,
  BGP prefix advertisements, and OVS flows.
* Preserve `EgressIP` `nodeSelector` expected behavior when combined with
  `PodNetwork` `nodeSelector` selection.
* **Two independent selectors control BGP behavior**:
  `FRRConfiguration.spec.nodeSelector` selects **BGP peers** (which nodes establish
  BGP sessions with external routers), while `RouteAdvertisements.spec.nodeSelector`
  selects **advertisement gateway nodes** (whose subnets should be made reachable
  externally). These two selectors may differ (see "Two Selectors, Two Concerns").
  Non-gateway nodes do not require BGP peering — only nodes in both selectors
  (gateway nodes) advertise pod network routes.
* Preserve today's behavior when `nodeSelector` is empty.

## Non-Goals

* **Egress control / egress rerouting** — Steering non-gateway node traffic
  through gateway nodes via OVN logical router policies, transit switch nexthops,
  or any other datapath mechanism for reaching BGP-learned external destinations.
  Non-gateway nodes egress directly via their node's default gateway. If a
  destination is only reachable via BGP-learned routes, non-gateway nodes cannot
  reach it — operators should enable BGP on that node or schedule the workload on
  a gateway node. This is a separate feature orthogonal to selective BGP peering.
* Support for no-overlay transport with **non-empty** `nodeSelector` for
  `PodNetwork` — NoOverlay mode has no tunnel overlay to forward traffic between
  nodes. If only gateway nodes advertise pod subnets (non-empty selector),
  external traffic for pods on non-gateway nodes would arrive at gateway nodes
  with no way to forward it to the actual pod location.
* Replacing or modifying the existing EgressIP reroute mechanism.
* Automatic scheduling of workloads on gateway nodes — users may schedule pods
  on any node.
* **FRR daemonset scheduling / scale-down** — Managing which nodes run FRR
  containers (FRR-k8s pods). `FRRConfiguration.spec.nodeSelector` controls which
  nodes peer with external BGP routers, not which nodes run FRR containers. FRR
  containers may continue to run on all nodes as a daemonset (existing behavior),
  with only selected nodes establishing external BGP sessions. Idle FRR containers
  cost little; the value of selective BGP peering is in reducing BGP session count
  and external router table size, not in unscheduling pods. Controlling FRR
  daemonset scheduling is a separate concern.

## Introduction

The original BGP OKEP ([OKEP-5296](okep-5296-bgp.md)) listed “Allow selecting
only a subset of nodes to advertise BGP” as a future goal. This OKEP fulfills
that goal for `PodNetwork` advertisements.

In many datacenter deployments, only a subset of nodes are connected to BGP
peers on the provider network. These “gateway nodes” act as the entry points for
north/south ingress traffic. Today, the `RouteAdvertisements` CRD enforces that
`PodNetwork` must be advertised on all nodes, which prevents this deployment
pattern.

When only gateway nodes peer with external BGP routers and advertise pod
subnets, an **ingress forwarding challenge** arises: external traffic for a pod
on a non-gateway node arrives at a gateway node via BGP/ECMP. The gateway node
must forward it to the correct node through the tunnel overlay (Geneve).

This OKEP addresses the ingress path, enabling gateway nodes to advertise all
pod subnets and forward external traffic to pods on any node. The implementation
requires both control plane changes (RouteAdvertisements controller generating
advertisements for all nodes' subnets) and data plane changes (breth0 OVS flows
on gateway nodes for remote subnet forwarding).

**Egress control** (steering non-gateway node traffic through gateway nodes to
reach BGP-learned destinations) is **explicitly not in scope** for this OKEP.
Non-gateway nodes **behave normally**:

* Egress traffic exits via the node's default gateway
* No BGP-learned routes are available on non-gateway nodes
* If the destination is reachable via the default gateway (standard internet
  routing), it works
* If the destination is **only** reachable via BGP-learned routes, non-gateway
  nodes cannot reach it

**The solution**: enable BGP on that node, or schedule the workload on a gateway
node. Egress rerouting through gateway nodes is a separate, generic cluster
capability orthogonal to BGP peering (see
[#7037](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/7037)). The design
philosophy: if you don't want BGP on a node, that's fine — but that node won't
learn BGP routes. The straightforward solution is to enable BGP peering on nodes
that need to reach BGP-learned destinations, not to implement complex egress
rerouting infrastructure.

### What Exists Today vs What This OKEP Adds

**Selective BGP peering already works** via `FRRConfiguration.spec.nodeSelector`
— this is the existing, documented mechanism for controlling which nodes establish
BGP sessions with external routers:

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: bgp-peering
  namespace: openshift-frr-k8s
spec:
  bgp:
    routers:
    - asn: 64512
      neighbors:
      - address: 172.18.0.5
        asn: 64513
  nodeSelector:
    matchLabels:
      bgp-gateway: "true"
```

Only nodes with `bgp-gateway: "true"` peer with external routers. This mechanism
is documented in the [OCP BGP routing
guide](https://docs.redhat.com/en/documentation/openshift_container_platform/4.20/html/advanced_networking/bgp-routing)
and is used in production today.

**What was missing** (and what this OKEP adds):

1. **`RouteAdvertisements.spec.nodeSelector` was blocked for `PodNetwork`** — the
   CRD validation rejected non-empty `nodeSelector` when `PodNetwork` was
   advertised. This forced all nodes to advertise pod subnets, preventing the
   selective gateway pattern.

2. **Optimized route advertisements for selective peering**:
   * **Layer 3 networks** (default and Layer 3 UDNs): Gateway nodes currently
     advertise only their own per-node host subnet (e.g., `/24` in typical
     configurations). When `nodeSelector` selects a subset of nodes, external
     traffic for pods on non-gateway nodes has no route. This OKEP adds **cluster
     supernet advertisement** (e.g., `/16` in typical configurations) from gateway
     nodes — longest prefix match gives direct routing to gateway nodes (host
     subnet wins), fallback via gateway for non-gateway nodes (supernet catches the
     rest). Prefix lengths are **derived from the network's configured cluster CIDR
     and host subnet allocation**, not hardcoded.
   * **Layer 2 networks** (Layer 2 UDNs): **Rejected** with non-empty
     `nodeSelector`. Layer 2 uses a single flat subnet shared by all nodes, so
     selective advertisement cannot meaningfully make only some nodes' pods
     reachable. Use empty `nodeSelector` (all nodes) for Layer 2 UDNs.

### Two Selectors, Two Concerns

OVN-Kubernetes BGP integration uses **two independent selectors** that control
different aspects of BGP behavior:

1. **`FRRConfiguration.spec.nodeSelector`** — Selects **BGP peers**: which nodes
   establish BGP sessions with external routers ("from which nodes do we
   advertise"). This selector determines which nodes have FRR BGP peering
   configured and establish BGP sessions with provider network routers. This is
   the existing mechanism documented in the [OCP BGP routing
   guide](https://docs.redhat.com/en/documentation/openshift_container_platform/4.20/html/advanced_networking/bgp-routing).
   Note: This controls BGP peering configuration, not FRR daemonset scheduling —
   FRR containers may run on all nodes, but only selected nodes will peer
   externally.

2. **`RouteAdvertisements.spec.nodeSelector`** — Selects **advertisement gateway
   nodes**: which nodes' subnets should be made reachable externally ("which
   nodes' subnets do we advertise"). This OKEP relaxes the existing constraint
   that requires this selector to be empty (select all nodes) when `PodNetwork`
   is advertised.

These two selectors address different concerns and may select different sets of
nodes. This OKEP focuses on enabling **non-empty**
`RouteAdvertisements.spec.nodeSelector` for `PodNetwork`.

**Terminology**:
- **Gateway node** (or **BGP gateway node**): A node that is selected by BOTH
  `FRRConfiguration.spec.nodeSelector` AND `RouteAdvertisements.spec.nodeSelector`.
  These nodes establish BGP sessions with external routers AND advertise pod
  network routes. This is the **intersection** of the two selectors. Only gateway
  nodes actually advertise pod network routes to external routers.

- **Non-gateway node**: A node NOT in the intersection (not a gateway node).
  This includes:
  - Nodes in NEITHER selector (typical non-gateway: no BGP role at all)
  - Nodes in FRRConfiguration only (has BGP sessions but doesn't advertise pod
    routes because RA doesn't select it)
  - Nodes in RouteAdvertisements only (RA wants to advertise its subnet, but it
    can't because it has no BGP sessions - the gateway nodes advertise supernet
    covering it instead)

- **BGP peering node**: A node selected by `FRRConfiguration.spec.nodeSelector`
  (has BGP sessions). This may or may not be a gateway node depending on whether
  RouteAdvertisements also selects it.

- **RA-selected node**: A node selected by `RouteAdvertisements.spec.nodeSelector`.
  This may or may not be a gateway node depending on whether FRRConfiguration
  also selects it.

For ingress routing purposes, only **gateway nodes** (intersection of both
selectors) actually advertise pod network routes.

**Common deployment pattern** (recommended): Both selectors match the same set of
nodes. For example, both select worker-1 and worker-2:
- Worker-1 and worker-2 are **gateway nodes** (in BOTH selectors)
- Worker-3 and all other nodes are **non-gateway nodes** (in NEITHER selector)

In this pattern: gateway node = BGP peering node = RA-selected node (the sets are
identical). Gateway nodes peer with external routers AND advertise pod network
routes. Non-gateway nodes have no BGP role.

**Advanced scenarios** where the two selectors diverge are also supported:

1. **RouteAdvertisements selects MORE nodes than FRRConfiguration**:
   - Example: FRR selects worker-1, RA selects worker-1 + worker-2
   - **Worker-1**: gateway node (in BOTH selectors) - advertises pod routes
   - **Worker-2**: non-gateway node (RA-selected but no BGP sessions) - cannot
     actually advertise; gateway nodes advertise supernet covering it instead
   - **Worker-3**: non-gateway node (in NEITHER)
   - Gateway nodes (worker-1) advertise cluster supernet covering ALL nodes. Uses
     themselves as next-hop, so traffic to worker-2 arrives at worker-1 then
     forwards via overlay.

2. **FRRConfiguration selects MORE nodes than RouteAdvertisements**:
   - Example: FRR selects worker-1 + worker-2, RA selects worker-1
   - **Worker-1**: gateway node (in BOTH selectors) - advertises pod routes
   - **Worker-2**: non-gateway node (BGP peering but not RA-selected) - has BGP
     sessions but doesn't advertise pod network routes
   - **Worker-3**: non-gateway node (in NEITHER)
   - Gateway nodes (worker-1) advertise cluster supernet covering ALL nodes
     including worker-2. Worker-2 can use BGP-learned routes for egress but
     doesn't advertise pod routes.

## User-Stories/Use-Cases

### Story 1: Dedicated BGP gateway nodes

As a cluster admin, I want to designate specific nodes as BGP gateways so that
only those nodes peer with my provider network's BGP routers, while pods on any
node in the cluster remain reachable from the external network.

For example: In a 10-node cluster, only 2 nodes are connected to the spine
switches via BGP. These 2 gateway nodes advertise all pod subnets, and external
traffic enters the cluster through them.

### Story 2: High availability with multiple gateways

As a cluster admin, I want to select multiple gateway nodes so that if one
gateway fails, traffic is automatically rerouted to the remaining gateways via
ECMP, with optional BFD for fast failover detection.

### Story 3: UDN pod network with gateway nodes

As a cluster admin, I want to advertise a Layer 3 CUDN's pod subnets through
designated gateway nodes, with each UDN's routes advertised in its corresponding
VRF.

### Story 4: Multiple provider BGP peers for the same network

As a cluster admin, I want to peer different gateway nodes with different
provider BGP routers for the same network, using multiple `RouteAdvertisements`
objects. Each `RouteAdvertisements` selects a different (or overlapping) set of
gateway nodes via `nodeSelector` and uses a distinct `frrConfigurationSelector`
to peer with different external routers. FRR-k8s merges the configurations on
each node, and the combined gateway set provides ECMP across all peers.

For example: In a multi-ToR (Top-of-Rack) deployment, `RouteAdvertisements` RA1
selects nodes in rack A (peering with ToR-A) and RA2 selects nodes in rack B
(peering with ToR-B). Both advertise the default network's pod subnets, providing
redundant paths to the external network.

### Story 5: EgressIP compatibility

As a cluster admin, I use EgressIP for source IP control on egress traffic while
also using `RouteAdvertisements` with a `nodeSelector` for `PodNetwork`. EgressIP
continues to function independently - pods selected by EgressIP egress with the
configured EgressIP source address through EgressIP-selected nodes as normal.

### Story 6: Dual-stack network support

As a cluster admin, I run a dual-stack Kubernetes cluster with both IPv4 and IPv6
pod networks. I want to advertise both IPv4 and IPv6 pod subnets through the same
set of gateway nodes, so that external clients can reach pods via either IPv4 or
IPv6 addresses. Gateway nodes should advertise both IP families simultaneously:
their own host subnets (derived from node allocation) plus cluster supernets
(derived from cluster CIDR configuration) for both IP families.

For example: A dual-stack cluster with pod networks `10.244.0.0/16` (IPv4) and
`fd01::/48` (IPv6), allocating host subnets as `/24` (IPv4) and `/64` (IPv6).
Gateway nodes advertise their own `/24` and `/64` subnets in both families plus
the cluster `/16` (IPv4) and `/48` (IPv6) supernets, enabling external clients to
reach any pod via either protocol.

### Story 7: Selector mismatch scenarios

As a cluster admin, I configure `FRRConfiguration.spec.nodeSelector` and
`RouteAdvertisements.spec.nodeSelector` to select different sets of nodes to
support advanced deployment patterns:

* **RA selects MORE nodes than FRRConfiguration**: I want subnets from nodes A, B,
  and C advertised, but only node C has a BGP session with external routers.
  Node C is the gateway node (in both selectors) - it advertises the cluster
  supernet covering subnets for A, B, and C with itself as next-hop, so external
  traffic to pods on A or B arrives at C and is forwarded via overlay to the
  destination node. This enables centralized BGP peering for a larger set of
  nodes.

* **FRRConfiguration selects MORE nodes than RA**: Nodes A, B, and C have BGP
  sessions, but RA only selects node A's subnet for advertisement. Nodes B and C
  peer with external routers but do not advertise pod network routes - they may
  advertise other route types (e.g., LoadBalancer services) or learn routes from
  external peers for egress traffic. This enables selective pod network
  advertisement while maintaining BGP peering for other purposes.

## Proposed Solution

### API Details

#### Remove the CEL validation rule

Remove the existing CEL validation on `RouteAdvertisementsSpec` that blocks
`nodeSelector` with `PodNetwork`:

```
// REMOVE:
// +kubebuilder:validation:XValidation:rule="(!has(self.nodeSelector.matchLabels) && !has(self.nodeSelector.matchExpressions)) || !('PodNetwork' in self.advertisements)"
// message="If 'PodNetwork' is selected for advertisement, a 'nodeSelector' can't be specified as it needs to be advertised on all nodes"
```

No new API fields are required. The existing `nodeSelector` field gains
additional semantics when used with `PodNetwork`.

#### Runtime validation

A controller-level validation rejects **non-empty** `nodeSelector` with
`PodNetwork` when any selected network uses `transport: NoOverlay`:

```
"non-empty nodeSelector is not supported for PodNetwork when selected networks
use NoOverlay transport"
```

This validation only applies when `nodeSelector` is non-empty (selects a subset
of nodes). When `nodeSelector` is empty (selects all nodes), NoOverlay networks
can continue to advertise `PodNetwork` as they do today. This is enforced at the
controller level (not CEL) because it requires cross-resource validation — the
transport is determined from the selected network's configuration, not from the
`RouteAdvertisements` spec itself.

* If `nodeSelector` matches **zero** ready nodes:
  * Status condition: `Degraded` with reason `NoGatewayNodes`
  * Message: "nodeSelector matches no ready nodes; at least one gateway node is
    required for PodNetwork advertisement"
  * No `FRRConfiguration` objects are generated in this state
  * Status clears when at least one matching gateway node becomes ready
* If a selected gateway node has no matching `FRRConfiguration` (per
  `frrConfigurationSelector`), surface an error in `RouteAdvertisements`
  status for that node.
* If no ready node is selected by both the applicable
  `FRRConfiguration.spec.nodeSelector` and
  `RouteAdvertisements.spec.nodeSelector`, report `Degraded` with reason
  `NoGatewayNodes` and generate no gateway `FRRConfiguration` objects.
* **Multiple `RouteAdvertisements` may now select the same network** (this relaxes
  the OKEP-5296 conflict rule). Each `RouteAdvertisements` generates its own per-node
  `FRRConfiguration` objects (labeled per-RA). FRR-K8s merges them only when their
  router and neighbor settings are compatible; conflicting settings (such as different
  ASNs for the same router in the same VRF) prevent the merged configuration from
  applying. **When a merge conflict occurs**:
  * FRR-K8s retains the previous valid configuration for the affected router and does
    not apply the conflicting change
  * The `RouteAdvertisements` controller detects the conflict via `FRRNodeState` status
    and reports it in the **affected `RouteAdvertisements` status** with:
    * Status condition: `Degraded`
    * Reason: `FRRConfigConflict`
    * Message: "FRR configuration conflict on node <node-name>: <conflict-type>.
      Another RouteAdvertisements may have incompatible settings. Check
      FRRNodeState/<node-name> for details."
  * Operators must resolve the conflict by making `FRRConfiguration` settings compatible
    (e.g., using the same ASN across all RAs selecting overlapping nodes for the same
    network)
  
  When multiple `RouteAdvertisements` select the same network with different (or
  overlapping) `nodeSelector` values and **compatible** FRR settings, the effective
  gateway set is the **union** of all selected nodes. Operators may intentionally
  configure overlapping gateway sets when peering with different provider BGP
  routers via distinct `frrConfigurationSelector` values, provided the FRR router
  settings are compatible.

### Scope matrix

| Network | Topology | Transport | non-empty `nodeSelector` + `PodNetwork` |
|---------|----------|-----------|------------------------------------------|
| Default cluster network | L3 | Geneve (overlay) | **Supported** |
| Default cluster network | — | NoOverlay | **Rejected** |
| Primary L3 CUDN | Layer 3 | Overlay | **Supported** |
| Primary L3 CUDN | Layer 3 | NoOverlay | **Rejected** |
| Primary L3 CUDN | Layer 3 | EVPN | **Rejected** |
| Primary L2 CUDN | Layer 2 | Overlay | **Rejected** |
| Primary L2 CUDN | Layer 2 | EVPN | **Rejected** |
| Primary L2 CUDN | Layer 2 | NoOverlay | **Rejected** |

**Notes**:
- When `nodeSelector` is **empty** (selects all nodes), the existing behavior is
  preserved for all network types and transports, including Layer 2 and EVPN.
- **Layer 2 networks are rejected** because Layer 2
  networks use a single flat subnet shared across all nodes. When gateway nodes
  advertise the cluster subnet (e.g., `10.150.0.0/24`), they make ALL pods in that
  subnet reachable externally, regardless of which nodes the `nodeSelector` selects.
  This creates ambiguity: the `nodeSelector` cannot control which nodes' pods are
  reachable (as it does for Layer 3), making the feature confusing. Use Layer 3
  networks (default or Layer 3 UDNs) for selective gateway node patterns, or use
  empty `nodeSelector` (all nodes) for Layer 2 UDNs.
- **EVPN is rejected** with non-empty `nodeSelector` because EVPN requires all
  nodes to participate in the VXLAN underlay and internal iBGP full-mesh peering.
  While `FRRConfiguration.spec.nodeSelector` could control which nodes establish
  external BGP sessions with provider routers, all nodes must still run FRR and
  maintain full EVPN infrastructure (VTEPs, VXLAN devices, Linux bridge, internal
  iBGP sessions). This eliminates the primary value proposition of selective
  `nodeSelector` (reducing FRR overhead on non-gateway nodes). Use overlay
  (Geneve) transport for gateway-node deployments.

**IP family support**: All "Supported" configurations work with **IPv4-only,
IPv6-only, and dual-stack (IPv4+IPv6)** networks. For dual-stack networks:
- Gateway nodes advertise both IPv4 and IPv6 prefixes to BGP peers.
- Ingress flows (breth0 OVS flows) are created for both IPv4 and IPv6 remote subnets.

#### VRF-Lite considerations

VRF-Lite uses Geneve overlay for east-west traffic, so the standard overlay
gateway pattern applies. Only gateway nodes need provider network interfaces and
provider VRF routing table entries. Non-gateway nodes forward traffic via Geneve
tunnels to gateway nodes — encapsulated traffic does not violate VRF isolation.

### Implementation Details

#### Control Plane: RouteAdvertisements Controller

**File:** `go-controller/pkg/clustermanager/routeadvertisements/controller.go`

When `nodeSelector` is non-empty and `PodNetwork` is in `advertisements`:

1. Remove the runtime guard at `controller.go:628-630` that rejects non-empty
   `nodeSelector` with `PodNetwork`.
2. **Remove cross-RA same-network conflict detection** and **define
   `FRRConfiguration` lifecycle**: Multiple `RouteAdvertisements` may now select
   the same network. Each RA generates independently labeled `FRRConfiguration`
   objects; FRR-k8s merges them on each node.
   
   **FRRConfiguration object lifecycle**:
   * `frrConfigurationSelector` continues to select the **source**
     `FRRConfiguration` objects (templates that define BGP router and neighbor
     settings).
   * For each RA/source-FRRConfiguration/gateway-node combination, the controller
     **clones** the selected source configuration and generates a per-node
     `FRRConfiguration` object with:
     * Generated name: Concatenate `<ra-name>`, `<source-name>`, and `<node-name>`
       with `-` separators. If the result exceeds 253 characters, truncate
       deterministically and append a collision-resistant hash (e.g., first 8
       characters of SHA256 of the full tuple).
     * Label: `routeadvertisements.k8s.ovn.org/owner: <owner-hash>` where
       `<owner-hash>` is the full `<ra-name>` if it fits within 63 characters,
       otherwise a deterministic truncation (e.g., first 54 chars) plus a
       collision-resistant hash suffix (e.g., 8-char SHA256 prefix).
     * Annotation: `routeadvertisements.k8s.ovn.org/owner-name: <ra-name>` to
       preserve the full RouteAdvertisements name.
     * Annotation: `routeadvertisements.k8s.ovn.org/source-key:
       <ra-name>/<source-name>/<node-name>` to uniquely identify the
       RA/source/node tuple (used for reconciliation).
     * `nodeSelector` matching only the specific gateway node
     * Prefixes injected from the advertised network (pod subnets for Layer 3,
       cluster subnet for Layer 2)
   * **Reconciliation**: On each reconcile loop:
     * List existing generated `FRRConfiguration` objects by label
       `routeadvertisements.k8s.ovn.org/owner` (matches the hashed owner value)
     * Filter by annotation `owner-name` to ensure exact RA match (handles hash
       collisions)
     * Match retained objects by annotation `source-key` (the full tuple)
     * Update the object if prefixes or settings changed
     * Delete duplicates (multiple objects with same `source-key`) or obsolete
       objects (node no longer selected, **node is no longer `Ready`**, source
       config no longer selected, or RA deleted)
   * **Node readiness handling**:
     * When a selected gateway node becomes **`NotReady`**: Delete its
       `FRRConfiguration` object. BGP sessions from that node are withdrawn. The
       node network controller cleans up OVS flows for remote subnets on that
       node. Ingress traffic fails over to remaining Ready gateway nodes via BGP
       ECMP.
     * When a gateway node becomes **`Ready`** again: Recreate its
       `FRRConfiguration` object with the current prefix set. The node network
       controller reinstalls OVS flows for remote subnets. BGP sessions are
       re-established and routes re-advertised. Ingress traffic resumes to this
       gateway via ECMP.
   * This ensures multiple `RouteAdvertisements` selecting the same network each
     maintain their own set of per-node `FRRConfiguration` objects without
     conflict, and only Ready nodes participate in BGP gateway duties.
3. **Monitor FRRNodeState for merge conflicts**: Watch `FRRNodeState` status for
   nodes selected by this RA. If FRR-k8s reports a configuration conflict (e.g.,
   incompatible ASNs, router ID mismatch), update the `RouteAdvertisements`
   status with:
   * Status condition: `Degraded` with reason `FRRConfigConflict`
   * Message: "FRR configuration conflict on node <node-name>: <conflict-type>.
     Another `RouteAdvertisements` may have incompatible settings. Check
     `FRRNodeState`/<node-name> for details."
   * The previous valid FRR configuration remains active; prefixes from the
     conflicting RA are not advertised on the affected node.
4. **Add Layer 2 network topology validation** (only when `nodeSelector` is non-empty):
   If `nodeSelector` selects a subset of nodes (non-empty) and any selected
   network is a **Layer 2** network (primary Layer 2 CUDN), reject the
   configuration and update status:
   * Status condition: `Degraded` with reason `InvalidTopology`
   * Message: "non-empty nodeSelector is not supported for PodNetwork when
     selected networks use Layer 2 topology. Layer 2 networks use a single flat
     subnet shared by all nodes; selective advertisement cannot meaningfully make
     only some nodes' pods reachable. Use empty nodeSelector (all nodes) for Layer
     2 networks."
   * This validation does **not** apply when `nodeSelector` is empty (selects all
     nodes), preserving existing behavior for Layer 2 networks.
   * **Network type detection**: Determine if a network is Layer 2 by checking the
     UserDefinedNetwork resource's topology field or NAD annotation indicating
     Layer 2 mode.
5. **Add the no-overlay validation** (only when `nodeSelector` is non-empty):
   If `nodeSelector` selects a subset of nodes (non-empty) and any selected
   network has `transport: NoOverlay`, reject the configuration and update status:
   * Status condition: `Degraded` with reason `InvalidTransport`
   * Message: "non-empty nodeSelector is not supported for PodNetwork when
     selected networks use NoOverlay transport"
   * This validation does **not** apply when `nodeSelector` is empty (selects all
     nodes), preserving existing behavior for NoOverlay networks.
6. **Add EVPN transport validation** (only when `nodeSelector` is non-empty):
   If `nodeSelector` selects a subset of nodes (non-empty) and any selected
   network has `transport: EVPN`, reject the configuration and update status:
   * Status condition: `Degraded` with reason `InvalidTransport`
   * Message: "non-empty nodeSelector is not supported for PodNetwork when
     selected networks use EVPN transport (all nodes must participate in EVPN
     fabric)"
   * This validation does **not** apply when `nodeSelector` is empty (selects all
     nodes), preserving existing behavior for EVPN networks.
7. **EgressIP compatibility validation and handling**: When `PodNetwork` is
   advertised with a non-empty `nodeSelector`, watch `EgressIP` objects and
   monitor both the **eligible node pool** (`EgressIP.spec.egressNodeSelector`)
   and **current assignments** (`EgressIP.status.items[].node`):
   
   **Contract**: EgressIP assignments outside the gateway union are **allowed but
   reported as degraded**. The RouteAdvertisements controller detects the
   incompatibility and reports it via status condition but does **not prevent**
   the assignment. This is a **detect-and-warn** approach. Traffic from pods
   using an EgressIP assigned to a non-gateway node **will black-hole** if the
   destination requires BGP-learned routes (because the non-gateway EgressIP node
   lacks those routes).
   
   **Validation logic**:
   * **Check eligible pool**: If `EgressIP.spec.egressNodeSelector` selects any
     nodes outside the gateway union, report status `Degraded`. This check applies
     regardless of current assignment - even if currently assigned to a gateway
     node, the eligible pool containing non-gateway nodes creates risk of future
     black-hole on reassignment.
   * **Check current assignments**: If `EgressIP.status.items[].node` is assigned
     to a node outside the gateway union, report status `Degraded`.
   * **Watch reassignments**: On EgressIP failover, re-evaluate both checks. Only
     clear `Degraded` condition when BOTH conditions are satisfied: eligible pool
     contains only gateway nodes AND current assignment is to a gateway node (or
     no assignment yet).
   
   **Status reporting**:
   * Status condition: `Degraded` with reason `EgressIPNodeConflict`
   * Message varies based on which check failed:
     * If current assignment is to non-gateway: "EgressIP <egressip-name> is
       assigned to node <node-name> which is not in the BGP gateway union for
       network <network-name>. Traffic from pods using this EgressIP cannot reach
       BGP-learned destinations and ingress to the EgressIP address will fail.
       Assign the EgressIP to a gateway node or update
       EgressIP.spec.egressNodeSelector to select only gateway nodes."
     * If eligible pool contains non-gateway nodes: "EgressIP <egressip-name> has
       egressNodeSelector that includes nodes outside the BGP gateway union
       (<list-of-non-gateway-nodes>). This creates risk of black-hole if EgressIP
       reassigns to a non-gateway node. Update EgressIP.spec.egressNodeSelector to
       select only gateway nodes."
   
   **EgressIP /32 or /128 advertisement** (only if `EgressIP` is in
   `spec.advertisements`):
   * If an EgressIP is assigned to a **gateway node** AND the RouteAdvertisements
     includes `EgressIP` in its `spec.advertisements` list AND the EgressIP's
     namespace matches the selected network, that node's generated
     `FRRConfiguration` includes the EgressIP /32 (IPv4) or /128 (IPv6) route for
     direct routing to the EgressIP-assigned node.
   * If `EgressIP` is **not in `spec.advertisements`**, no EgressIP /32 or /128
     routes are advertised, even if EgressIPs are assigned to gateway nodes.
   * If an EgressIP is assigned to a **non-gateway node** (and `EgressIP` is in
     advertisements), the EgressIP /32 or /128 is **not advertised** (no
     FRRConfiguration exists for non-gateway nodes). **Ingress traffic to the
     EgressIP address will fail** because there is no BGP route for it (the pod
     network cluster supernet does not cover EgressIP addresses, which are separate
     IPs). Egress from pods using this EgressIP will black-hole if the destination
     requires BGP-learned routes.
   * On failover/reassignment: When EgressIP assignment moves from node A to node
     B, node A's FRRConfiguration is updated to withdraw the /32 or /128, and node
     B's FRRConfiguration (if B is a gateway and `EgressIP` is in advertisements)
     is updated to add the /32 or /128.
8. **Change route generation (optimized advertisements)** for **Layer 3 networks
   only** (Layer 2 networks are rejected with non-empty `nodeSelector`):
   
   * **Layer 3 networks** (default cluster network and primary Layer 3 UDNs): Each
     selected gateway node advertises:
     * Its **own host subnet**: Derived from the node's allocated subnet within the
       network. For the default network, read from node annotation
       `k8s.ovn.org/node-subnets` (e.g., `10.244.2.0/24` for IPv4, `fd01:0:0:3::/64`
       for IPv6). For UDNs, read from the network's per-node subnet allocation.
       Prefix length is determined by the network's host subnet configuration, **not
       hardcoded** (e.g., could be /23 instead of /24 for IPv4).
     * The **cluster supernet**: Derived from the network's cluster CIDR
       configuration. For the default network, read from
       `spec.clusterNetwork[].cidr` in the Network CRD or cluster configuration
       (e.g., `10.244.0.0/16` for IPv4, `fd01::/48` for IPv6). For UDNs, read from
       the UserDefinedNetwork resource's `spec.subnets` field. Prefix length is the
       network's configured cluster CIDR, **not hardcoded** (e.g., could be /14
       instead of /16 for IPv4).
     * External routers use **longest prefix match**: the node's specific host
       subnet wins for direct routing to gateway nodes; the cluster supernet catches
       traffic for non-gateway nodes and routes it to any gateway node (which then
       forwards via Geneve overlay)
     * This makes **all pods reachable externally**, regardless of whether they're
       on gateway or non-gateway nodes
     * **For UDNs with VRF**: advertisements go into the network's configured VRF
       (via `targetVRF: auto` or explicit VRF name)
   
   **Prefix derivation logic**:
   * **Host subnet prefix**: Extract from node's allocated subnet for the network
     (node annotation or UDN subnet allocation). Preserve the original prefix length.
   * **Cluster supernet prefix**: Extract from the network's cluster CIDR
     configuration. Preserve the original prefix length.
   * **Example**: If cluster CIDR is `10.128.0.0/14` with host subnets allocated as
     `/23`, gateway nodes advertise their own `/23` + cluster `/14` supernet, NOT
     hardcoded `/24` and `/16`.
9. On **node add** (Layer 3 networks only):
   * Allocate host subnet (IPv4 and/or IPv6). If the new node is a gateway node
     **and is Ready**, it advertises its own host subnet + cluster supernet (using
     derived prefix lengths from network configuration). If it's a non-gateway node,
     no advertisement changes are needed (cluster supernet already covers it). Add
     br-ex remote flows for the new node's subnet on all gateway nodes.
10. On **node delete** (Layer 3 networks only):
   * If a gateway node is deleted, its host subnet and cluster supernet
     advertisements are withdrawn. If a non-gateway node is deleted, no
     advertisement changes (still covered by supernet). Remove br-ex flows for that
     subnet on remaining gateway nodes.
11. On **node readiness transitions** (Ready ↔ NotReady):
   * Handled by reconciliation (step 2 above): When a selected gateway becomes
     `NotReady`, its `FRRConfiguration` is deleted and BGP sessions are withdrawn.
     When it becomes `Ready` again, its `FRRConfiguration` is recreated and BGP
     sessions are re-established. This ensures only Ready gateway nodes participate
     in ingress traffic handling.

#### Handling Selector Mismatch Scenarios

The RouteAdvertisements controller must handle cases where
`FRRConfiguration.spec.nodeSelector` and `RouteAdvertisements.spec.nodeSelector`
select different sets of nodes:

**Scenario 1: RA selects MORE nodes than FRRConfiguration** (Story 7)
* **Example**: FRRConfiguration selects worker-1 only (worker-1 peers with external
  BGP routers). RA selects worker-1 AND worker-2 (RA wants both nodes' subnets
  advertised).
* **Controller logic**:
  1. Identify **BGP peering nodes**: Nodes selected by **any** FRRConfiguration
     referenced by this RA's `frrConfigurationSelector`. These nodes establish BGP
     sessions with external routers.
  2. Identify **RA-selected nodes**: Nodes selected by RA's `nodeSelector`. These
     are the nodes whose subnets should be made reachable externally.
  3. **BGP peering nodes (advertisement gateways) advertise cluster supernet**
     (derived from configured cluster CIDR, e.g., /16 for IPv4, /48 for IPv6)
     covering ALL nodes (both RA-selected and non-RA-selected nodes).
  4. Each BGP peering node advertises its own host subnet + cluster supernet.
  5. **Next-hop**: BGP peering nodes use themselves as next-hop for all advertised
     prefixes (including the supernet).
* **Result**: Worker-1 (which peers with external BGP routers) advertises the
  cluster supernet covering worker-2. External traffic to worker-2 arrives at
  worker-1 (via supernet route), then worker-1 forwards via Geneve overlay to
  worker-2.
* **RA-selected non-peering nodes** (worker-2 in this example): Their subnets are
  covered by the cluster supernet advertised by BGP peering nodes. No special
  handling needed - the supernet makes them reachable.

**Scenario 2: FRRConfiguration selects MORE nodes than RA**
* **Example**: FRRConfiguration selects worker-1 AND worker-2 (both peer with
  external BGP routers). RA selects worker-1 only (RA wants only worker-1 as
  advertisement gateway).
* **Controller logic**:
  1. Identify BGP peering nodes (both worker-1 and worker-2 have BGP sessions).
  2. Identify RA-selected nodes (worker-1 only - should be advertisement gateway).
  3. Generate FRRConfiguration objects only for **BGP peering nodes that are also
     selected by RA's nodeSelector** (worker-1 in this example).
  4. Worker-2 has a BGP session (from base FRRConfiguration) but does **not**
     receive a generated FRRConfiguration from this RA, so it does not advertise
     pod network routes.
  5. Worker-1 advertises its own subnet + cluster supernet (covering worker-2
     and worker-3).
* **Result**: Worker-2 peers with external routers (FRRConfiguration control) but
  does not advertise pod network routes (not selected by RA). Worker-1 advertises
  the supernet, making all pods (including those on worker-2) reachable via
  worker-1.

**Key principle**: Nodes that peer with external BGP routers
(FRRConfiguration-selected) and are also selected as advertisement gateways
(RouteAdvertisements-selected) always advertise the **cluster supernet**
regardless of selector mismatch. This ensures all pods are reachable externally,
even if some nodes don't have BGP sessions (not in FRRConfiguration) or aren't
selected as advertisement targets (not in RouteAdvertisements).

#### Dual-Stack Support

Dual-stack clusters (IPv4 + IPv6 simultaneously) are fully supported. The
controller advertises both IP families in parallel (Story 6):

* **RouteAdvertisements controller**:
  * When generating FRRConfiguration objects for gateway nodes, include prefixes
    for **both IPv4 and IPv6** networks:
    * IPv4: Gateway's own host subnet (derived from node allocation, e.g., `/24`)
      + cluster supernet (derived from cluster CIDR, e.g., `/16`)
    * IPv6: Gateway's own host subnet (derived from node allocation, e.g., `/64`)
      + cluster supernet (derived from cluster CIDR, e.g., `/48`)
  * Both families are advertised to their respective BGP neighbors (IPv4
    neighbor receives IPv4 prefixes, IPv6 neighbor receives IPv6 prefixes).
  * Prefix lengths are determined by the network configuration, not hardcoded.
* **Node network controller**:
  * Install breth0 OVS flows for **both IPv4 and IPv6** remote subnets on gateway
    nodes (Shared Gateway Mode and Local Gateway Mode).
  * Install host kernel routes for **both IPv4 and IPv6** CUDN subnets (Local
    Gateway Mode only).
* **Data plane**:
  * Ingress traffic for IPv4 pod addresses follows the IPv4 path (BGP → gateway
    node → overlay → pod).
  * Ingress traffic for IPv6 pod addresses follows the IPv6 path (BGP → gateway
    node → overlay → pod).
  * Both families use the same overlay mechanism (Geneve tunnels).
* **SNAT behavior**:
  * When PodNetwork is advertised via BGP, SNAT is disabled cluster-wide for
    **both IPv4 and IPv6** (via NAD annotation `k8s.ovn.org/snat-per-pod-disable:
    "true"`).
  * Non-gateway nodes egress with pod IP for both IPv4 and IPv6 traffic.

**IPv6-only clusters** are also supported - the same logic applies but only IPv6
prefixes are advertised (gateway's own host subnet + cluster supernet, with prefix
lengths derived from the IPv6 network configuration).

#### Generated FRRConfiguration Examples

The generated per-node `FRRConfiguration` for a gateway node follows the **optimized
advertisement pattern**. **For dual-stack networks**, both IPv4 and IPv6 prefixes are
advertised to separate BGP neighbors (one IPv4 neighbor, one IPv6 neighbor), enabling
full dual-stack pod reachability:

```yaml
# Example 1: DUAL-STACK Layer 3 - Default Cluster Network - gateway node ovn-worker
# (Same pattern applies to primary Layer 3 UDNs, but with UDN-specific subnets and VRF)
# Cluster: 4 nodes (ovn-worker, ovn-worker2 are gateways; ovn-worker3, ovn-control-plane are non-gateways)
# Network: IPv4 10.244.0.0/16, IPv6 fd01::/48
#
# NOTE: Prefix lengths (/24, /16, /64, /48) in this example are typical defaults.
# Actual prefix lengths are DERIVED from the network's configured cluster CIDR and
# host subnet allocation, not hardcoded (e.g., could be /23 and /14 for IPv4).
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: ovn-worker
  bgp:
    routers:
      - asn: 64512
        prefixes:
          # DUAL-STACK: Gateway node's own subnets (both IP families):
          - 10.244.2.0/24           # ovn-worker's own IPv4 subnet (/24)
          - fd01:0:0:3::/64         # ovn-worker's own IPv6 subnet (/64)
          
          # DUAL-STACK: Cluster supernets (fallback for non-gateway nodes):
          - 10.244.0.0/16           # IPv4 cluster supernet (catches all non-gateway IPv4 traffic)
          - fd01::/48               # IPv6 cluster supernet (catches all non-gateway IPv6 traffic)
          
        neighbors:
          # IPv4 BGP neighbor:
          - asn: 64513
            address: 192.168.1.1
            toAdvertise:
              allowed:
                prefixes:
                  - 10.244.2.0/24   # Own /24 (longest prefix match → direct routing)
                  - 10.244.0.0/16   # Supernet /16 (fallback for non-gateway nodes)
                  
          # IPv6 BGP neighbor:
          - asn: 64513
            address: fd99::1
            toAdvertise:
              allowed:
                prefixes:
                  - fd01:0:0:3::/64 # Own /64 (longest prefix match → direct routing)
                  - fd01::/48       # Supernet /48 (fallback for non-gateway nodes)

# Note: Layer 2 networks with non-empty nodeSelector are REJECTED (see scope matrix).
# Layer 2 UDNs only work with empty nodeSelector (all nodes advertise cluster subnet).
```

#### BGP Advertisement Topology Diagram

The following diagram illustrates how gateway nodes advertise both their own subnets
and the cluster supernet to external BGP routers, enabling longest-prefix-match routing
for direct gateway access and fallback routing for non-gateway nodes:

```
BGP ADVERTISEMENT TOPOLOGY (IPv4 shown; IPv6 follows same pattern):

NOTE: This example uses /24 host subnets and /16 cluster supernet (typical defaults).
      Actual prefix lengths are derived from the network's configured cluster CIDR
      and host subnet allocation, not hardcoded.

Cluster subnets (example):
  - Cluster supernet: 10.244.0.0/16 (covers all nodes)
  - ovn-worker (gateway):   10.244.2.0/24
  - ovn-worker2 (gateway):  10.244.1.0/24
  - ovn-worker3 (non-gw):   10.244.0.0/24
  - ovn-control-plane:      10.244.3.0/24

                    ┌─────────────────────────┐
                    │  External BGP Router    │
                    │  Routing table:         │
                    │  10.244.2.0/24 → GW1    │ ← Longest prefix match (gateway node)
                    │  10.244.0.0/16 → GW1    │ ← Supernet fallback (all nodes)
                    │  10.244.1.0/24 → GW2    │ ← Longest prefix match (gateway node)
                    │  10.244.0.0/16 → GW2    │ ← Supernet fallback (all nodes)
                    └─────────────────────────┘
                           ▲           ▲
                           │BGP        │BGP
                           │session    │session
                  ┌────────┘           └────────┐
                  │                             │
        ┌─────────────────┐           ┌─────────────────┐
        │ ovn-worker (GW1)│           │ovn-worker2 (GW2)│
        │ 10.244.2.0/24   │           │ 10.244.1.0/24   │
        │                 │           │                 │
        │ Advertises:     │           │ Advertises:     │
        │ • 10.244.2.0/24 │           │ • 10.244.1.0/24 │
        │ • 10.244.0.0/16 │           │ • 10.244.0.0/16 │
        └─────────────────┘           └─────────────────┘
                  │                             │
                  │  Geneve overlay             │
                  └─────────────┬───────────────┘
                                │
                  ┌─────────────┴───────────────┐
                  │                             │
        ┌─────────────────┐           ┌─────────────────┐
        │ ovn-worker3     │           │ovn-control-plane│
        │ 10.244.0.0/24   │           │ 10.244.3.0/24   │
        │ (non-gateway)   │           │ (non-gateway)   │
        │                 │           │                 │
        │ No BGP session  │           │ No BGP session  │
        │ Not advertised  │           │ Not advertised  │
        │ directly, but   │           │ directly, but   │
        │ covered by      │           │ covered by      │
        │ supernet /16    │           │ supernet /16    │
        └─────────────────┘           └─────────────────┘

Traffic to 10.244.0.5 (pod on ovn-worker3, non-gateway):
  1. External router receives request for 10.244.0.5
  2. Longest prefix match: no /24 route, use /16 supernet
  3. ECMP selects GW1 or GW2 (e.g., GW1)
  4. Traffic arrives at ovn-worker (GW1)
  5. GW1 forwards via Geneve tunnel to ovn-worker3
  6. Pod sends its reply through its normal egress path via the pod node's
     default gateway. The reply retains the pod IP as its source and has the
     external client's IP as its destination. This proposal does not guarantee
     that the reply traverses the ingress gateway.

Traffic to 10.244.2.5 (pod on ovn-worker, gateway):
  1. External router receives request for 10.244.2.5
  2. Longest prefix match: /24 route wins (10.244.2.0/24 → GW1)
  3. Traffic arrives directly at ovn-worker (GW1)
  4. No overlay hop needed (local delivery)
```

#### Node Network Controller (Existing Implementation)

**File:** `go-controller/pkg/node/*_node_network_controller.go`

**Note:** This section documents existing behavior, not new requirements. The
current node network controller implementation already installs the necessary
host routes for primary CUDNs in Local Gateway Mode. This OKEP does not require
changes to route installation; it only requires adding breth0 OVS flows for
remote subnets on gateway nodes. **Layer 2 examples are included for
infrastructure context only; Layer 2 with non-empty `nodeSelector` is rejected
(see scope matrix).**

**Existing Local Gateway Mode host routes for primary CUDNs:**

The `UserDefinedNodeNetworkController` (via its `UserDefinedNetworkGateway`
component) already installs host kernel routes on **all nodes** (both gateway and
non-gateway) to direct CUDN pod traffic into the OVN management port:

* **Layer 3 primary CUDN** (e.g., `blue-network` with network ID 100, pod subnet `10.100.0.0/16`):
  * Route: `10.100.0.0/16 via <mp-blue-gw> dev ovn-k8s-mp100`
  * IPv6: `fd02::/48 via <mp-blue-gw-v6> dev ovn-k8s-mp100`
  * Linux device: `ovn-k8s-mp<network-ID>` (network ID 100 → `ovn-k8s-mp100`)
  * Next-hop: management port gateway IP (first IP in management port subnet)
  * Controller: `BaseSecondaryLayer3UserDefinedNetworkController` (control-plane)
  * Route installer: `UserDefinedNetworkGateway.computeRoutesForUDN()`  * VRF table: network-specific VRF routing table

* **Layer 2 primary CUDN** (e.g., `red-network` with network ID 101, cluster subnet `10.150.0.0/24`)  
  **⚠️ NOTE: This describes EXISTING behavior with EMPTY `nodeSelector` only.  
  Layer 2 with NON-EMPTY `nodeSelector` is REJECTED (see scope matrix).**
  * **Connected route** (kernel-managed): `10.150.0.0/24 dev ovn-k8s-mp101 scope link`
  * IPv6 connected route: `fd03::/64 dev ovn-k8s-mp101 scope link`
  * Linux device: `ovn-k8s-mp<network-ID>` (network ID 101 → `ovn-k8s-mp101`)
  * Route type: Kernel automatically creates connected routes when the management
    port interface is assigned an IP address in the cluster subnet
  * Controller: `BaseSecondaryLayer2UserDefinedNetworkController` (control-plane)
  * Route installer: No explicit via route from `UserDefinedNetworkGateway.computeRoutesForUDN()`;
    kernel creates connected route automatically
  * Note: Layer 2 uses **cluster-wide subnet**, not per-node subnets

These existing routes (explicit via routes for Layer 3, connected routes for
Layer 2) enable breth0 OVS flows (`actions=LOCAL`) to deliver traffic to the
host kernel, which then forwards via the management port into the OVN pipeline.

#### Data Plane: Ingress (External → Pod on non-gateway node)

External traffic for a remote pod arrives at the gateway node. The gateway node
must forward it to the correct node via the tunnel overlay. **For dual-stack
networks, flows are created for both IPv4 and IPv6 subnets.**

##### Shared Gateway Mode (SGW)

Traffic enters breth0 on the gateway node. OVS flows must steer traffic for
**all** advertised pod subnets into the OVN pipeline via the patch port, not
just the gateway node's own subnet.

```
# Existing (own subnet):
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.2.0/24 actions=output:2
cookie=0xdeff105, priority=300,ip6,in_port=1,ipv6_dst=fd01:0:0:3::/64 actions=output:2

# New (remote subnets) - IPv4:
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.0.0/24 actions=output:2
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.1.0/24 actions=output:2
cookie=0xdeff105, priority=300,ip,in_port=1,nw_dst=10.244.3.0/24 actions=output:2

# New (remote subnets) - IPv6:
cookie=0xdeff105, priority=300,ip6,in_port=1,ipv6_dst=fd01:0:0:1::/64 actions=output:2
cookie=0xdeff105, priority=300,ip6,in_port=1,ipv6_dst=fd01:0:0:2::/64 actions=output:2
cookie=0xdeff105, priority=300,ip6,in_port=1,ipv6_dst=fd01:0:0:4::/64 actions=output:2
```

Once inside OVN, the network's router (`ovn_cluster_router` for default network,
or `ovn_cluster_router_<network>` for Layer 3 UDN) has routes for all node
subnets (both IPv4 and IPv6) and forwards via Geneve tunnel to the correct node.

**Note:** Layer 2 UDNs (`GR_<node>_<network>` router) are **not supported** with
non-empty `nodeSelector` (rejected in scope matrix). They only work with empty
`nodeSelector` (all nodes), in which case all nodes advertise the cluster subnet
and there's no selective gateway pattern.

##### Local Gateway Mode (LGW)

Traffic enters breth0 and is sent to the host kernel via `actions=LOCAL`. The
host already has aggregate routes (`10.244.0.0/16 via <mp0-gw> dev ovn-k8s-mp0`
for IPv4 and `fd01::/48 via <mp0-gw-v6> dev ovn-k8s-mp0` for IPv6) that cover
all pod subnets. Only the breth0 OVS flows need to be added for remote subnets:

```
# Existing (own subnet):
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.2.0/24 actions=LOCAL
cookie=0xdeff105, priority=300,ip6,in_port=eth0,ipv6_dst=fd01:0:0:3::/64 actions=LOCAL

# New (remote subnets) - IPv4:
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.0.0/24 actions=LOCAL
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.1.0/24 actions=LOCAL
cookie=0xdeff105, priority=300,ip,in_port=eth0,nw_dst=10.244.3.0/24 actions=LOCAL

# New (remote subnets) - IPv6:
cookie=0xdeff105, priority=300,ip6,in_port=eth0,ipv6_dst=fd01:0:0:1::/64 actions=LOCAL
cookie=0xdeff105, priority=300,ip6,in_port=eth0,ipv6_dst=fd01:0:0:2::/64 actions=LOCAL
cookie=0xdeff105, priority=300,ip6,in_port=eth0,ipv6_dst=fd01:0:0:4::/64 actions=LOCAL
```

Traffic then follows: `breth0 → LOCAL → host kernel → 10.244.0.0/16 via mp0 (or
fd01::/48 via mp0 for IPv6) → OVN network router (ovn_cluster_router for default
network, ovn_cluster_router_<network> for Layer 3 UDN) → Geneve tunnel → remote
node → pod`.

**Note:** Layer 2 UDNs use `GR_<node>_<network>` routers but are **not supported**
with non-empty `nodeSelector`. They only work with empty `nodeSelector`.

**For primary CUDNs in LGW mode**, the `UserDefinedNodeNetworkController`
(via its `UserDefinedNetworkGateway` component) installs network-specific host routes:

* **Layer 3 primary CUDN** (e.g., `blue-network` with network ID 100, subnet `10.100.0.0/16`):
  * Each node: route for all pod subnets via network-scoped management port
  * Node controller: `UserDefinedNodeNetworkController`  * Route installer: `UserDefinedNetworkGateway.computeRoutesForUDN()`
  * Route: `10.100.0.0/16 via <mp-blue-gw> dev ovn-k8s-mp100`
  * IPv6: `fd02::/48 via <mp-blue-gw-v6> dev ovn-k8s-mp100`
  * Linux device name: `ovn-k8s-mp<network-ID>` (network ID 100 → `ovn-k8s-mp100`)
  * Installed in the network's VRF routing table

* **Layer 2 primary CUDN** (e.g., `red-network` with network ID 101, cluster subnet `10.150.0.0/24`):
  * Each node: **connected route** (kernel-managed, not explicit via route)
  * Node controller: `UserDefinedNodeNetworkController`  * Route installer: Linux kernel (automatic when interface gets IP in cluster subnet)
  * Connected route: `10.150.0.0/24 dev ovn-k8s-mp101 scope link`
  * IPv6 connected route: `fd03::/64 dev ovn-k8s-mp101 scope link`
  * Linux device name: `ovn-k8s-mp<network-ID>` (network ID 101 → `ovn-k8s-mp101`)
  * Note: Layer 2 does not get an explicit via route from `computeRoutesForUDN()`;
    the kernel's connected route is sufficient because the management port has an
    IP address in the cluster subnet

Gateway nodes add breth0 OVS flows for remote subnets (both default network and
CUDN subnets) as described in the SGW section.

#### ECMP and Reply Path Behavior

When multiple gateway nodes advertise the same cluster supernet, external routers
use **ECMP (Equal-Cost Multi-Path)** routing to distribute ingress traffic across
all available gateways.

**OVN configuration**:
* `ecmp_symmetric_reply` is **NOT set** (OVN default behavior)
* Reply path follows normal pod egress behavior (see below)

**Ingress and reply flow behavior**:

1. **Ingress (external → pod on non-gateway node)**:
   - External router selects a gateway node via ECMP (e.g., worker-1)
   - Gateway node receives traffic on breth0, forwards via OVN pipeline and Geneve
     tunnel to pod's node
   - Pod receives packet

2. **Reply path (pod → external)**:
   - The pod sends its reply through its normal egress path via the pod node's
     default gateway. The reply retains the pod IP as its source and has the
     external client's IP as its destination. This proposal does not guarantee
     that the reply traverses the ingress gateway.

**Path asymmetry**:
* Ingress (request): external → gateway-1 → pod
* Egress (reply): pod → pod's node default gateway → external
* The reply does NOT route back through gateway-1 unless the pod's node happens
  to be gateway-1 or the pod's node's default gateway routes to gateway-1

**Implications for stateful external infrastructure**:
* External firewalls/routers with stateful tracking may see asymmetric flows
* Request arrives via gateway-1 node (ingress path), reply exits via pod's node's
  default gateway (egress path, potentially a different physical node)
* Reply has pod IP as source, not the gateway node IP
* Operators deploying with stateful external infrastructure should either:
  1. Configure external firewalls to allow asymmetric flows for the advertised
     pod network supernet, or
  2. Use empty `nodeSelector` (all nodes as gateways) to minimize asymmetry

**Multiple ECMP paths**:
* Different ingress connections are distributed across gateways by external ECMP
* Reply paths use the pod's node's default gateway (independent of ingress gateway)

#### SNAT Behavior

When a network is advertised via BGP, SNAT is already conditionally disabled
(as documented in the existing BGP implementation). This remains unchanged —
pod traffic exits with the pod IP as source, which is the expected behavior for
advertised subnets.

#### Traffic Flow Summary

For a 4-node dual-stack cluster where ovn-worker and ovn-worker2 are gateway nodes:

```
INGRESS (external → pod on ovn-worker3 on default network):

  external
    │
    ▼
  gateway node (ovn-worker)
    breth0 → flow matches 10.244.0.0/24 (IPv4) or fd01:0:0:1::/64 (IPv6)
    │
    SGW: → patch port → OVN pipeline
    LGW: → LOCAL → host kernel → mp0 → OVN pipeline
    │
    ▼
  ovn_cluster_router (for default network or Layer 3 UDN:
                      ovn_cluster_router_<network>)
    → Geneve tunnel → ovn-worker3 → pod

Note: Layer 2 UDNs (GR_<node>_<network>) are NOT supported with non-empty
      nodeSelector (see scope matrix).

EGRESS (pod on ovn-worker3 on default network → external destination):

  pod on ovn-worker3
    │
    ▼
  Normal pod egress path via node's default gateway
  (no special BGP gateway rerouting - if destination requires BGP-learned routes,
   non-gateway nodes cannot reach it; use BGP on that node or schedule workload
   on a gateway node)
```

#### Transport Support Matrix

**Note:** This table applies to **Layer 3 networks only**. Layer 2 networks are
**rejected** with non-empty `nodeSelector` (see scope matrix).

| Transport        | NodeSelector + PodNetwork (Layer 3 only) | Reason                                          |
|------------------|------------------------------------------|-------------------------------------------------|
| Overlay (Geneve) | Supported                                | Geneve tunnels provide forwarding path; non-gateway nodes do not need FRR |
| EVPN (VXLAN)     | Blocked                                  | All nodes must run FRR for internal iBGP full-mesh regardless of nodeSelector |
| NoOverlay        | Blocked                                  | No tunnel backplane for remote pod forwarding   |

### SGW vs LGW Differences

| Aspect                         | SGW                            | LGW                                                                                           |
|--------------------------------|--------------------------------|-----------------------------------------------------------------------------------------------|
| Ingress breth0 flows           | `actions=output:<patch-port>`  | `actions=LOCAL`                                                                               |
| Host routes for remote subnets | Not needed (stays in OVS/OVN)  | Already covered by aggregate routes (IPv4: `10.244.0.0/16 via mp0`, IPv6: `fd01::/48 via mp0`) |
| SNAT handling                  | Conditional SNAT on GR         | Conditional SNAT on GR + host nftables (both IPv4 and IPv6)                                  |

#### EgressIP compatibility

* EgressIP continues to function independently alongside selective BGP peering.
* EgressIP nodes should be included in the BGP gateway `nodeSelector` to ensure
  they have BGP-learned routes for reaching external destinations after SNAT. The
  RouteAdvertisements controller validates this and reports a `Degraded` status
  if an EgressIP node is outside the gateway union.

### Testing Details

#### Unit Tests

* **RouteAdvertisements controller**: Update `controller_test.go`:
  * Remove `"fails to reconcile pod network if node selector is not empty"` test.
  * Add test: **Layer 3 networks** — when `nodeSelector` is non-empty, gateway
    nodes generate FRRConfigurations with:
    * Their **own** host subnet (e.g., `10.244.2.0/24` for IPv4, `/64` for IPv6)
    * The **cluster supernet** (e.g., `10.244.0.0/16` for IPv4, `/48` for IPv6)
    * This differs from empty `nodeSelector` where each node advertises only its
      own subnet
  * Add test: **Layer 2 networks** — `nodeSelector` + `PodNetwork` is **rejected**
    with status condition `Degraded` and reason `InvalidTopology`.
  * Add test: non-gateway nodes are not included in generated
    FRRConfigurations.
  * Add test: reject `nodeSelector` + `PodNetwork` with `NoOverlay` transport.
  * Add test: reject `nodeSelector` + `PodNetwork` with `EVPN` transport.
  * Add test: `nodeSelector` matching zero ready nodes:
    * Reports status condition `Degraded` with reason `NoGatewayNodes`
    * Does not generate `FRRConfiguration` objects
    * Status clears when a matching gateway node becomes ready
  * Add test: **multiple `RouteAdvertisements` selecting the same network**
    with different `nodeSelector` values generate independent
    `FRRConfiguration` objects (labeled per-RA).
  * Add test: multiple `RouteAdvertisements` selecting the same network with
    overlapping `nodeSelector` values merge correctly without conflict when FRR
    settings are compatible.
  * Add test: **FRR configuration conflict detection** — when multiple
    `RouteAdvertisements` select overlapping nodes with incompatible FRR
    settings (e.g., different ASNs for the same router in same VRF), verify:
    * Controller detects conflict via `FRRNodeState`
    * Affected `RouteAdvertisements` status reports:
      * Condition: `Degraded`
      * Reason: `FRRConfigConflict`
      * Message identifying the node and conflict type
    * Previous valid FRR configuration is retained on the affected node
    * Prefixes from the conflicting RA are not advertised
  * Add test: **dual-stack networks** generate both IPv4 and IPv6
    `FRRConfiguration` prefixes for all nodes on gateway nodes.

* **networkmanager**: Test `syncRouteAdvertisements` / `podAdvertisements`
  when only a subset of nodes are BGP gateways, including dual-stack networks.

* **OVN network controller**: Test that breth0 flows are created for remote pod
  subnets on gateway nodes, including both IPv4 and IPv6 flows for dual-stack
  networks.

#### E2E Tests

* Deploy a Kind cluster with 4 nodes, designate 2 as gateway nodes via labels.
* Create `RouteAdvertisements` with `nodeSelector` matching the 2 gateway nodes.
* Verify:
  * External traffic reaches pods on non-gateway nodes via gateway nodes (ingress path).
  * **Reply path (asymmetric)**: For ingress-initiated traffic (external → pod on
    non-gateway node), the pod sends its reply through its normal egress path via
    the pod node's default gateway. The reply retains the pod IP as its source and
    has the external client's IP as its destination. This proposal does not
    guarantee that the reply traverses the ingress gateway.
  * **Pod-initiated egress**: For pod-initiated traffic (pod on non-gateway node →
    external), verify traffic exits directly via the pod's node's default gateway,
    NOT through a BGP gateway node. Both ingress-replies and pod-initiated traffic
    use the same egress path (pod's node's default gateway).
  * Failover works when one gateway node is taken down (ingress continues via remaining gateway).
  * Both SGW and LGW modes.
* **Multiple RouteAdvertisements for same network**:
  * Create two `RouteAdvertisements` selecting the same network with different
    `nodeSelector` values (e.g., RA1 selects worker-1, RA2 selects worker-2).
  * Verify both gateway nodes advertise the optimized prefix set:
    * **Layer 3**: Each gateway advertises its own subnet + cluster supernet
  * Verify ingress traffic load-balances across both gateways (via BGP ECMP).
  * Take down one gateway; verify ingress traffic fails over to the remaining gateway.
* **FRR configuration conflict handling**:
  * Create RA1 selecting worker-1 with ASN 64512
  * Create RA2 selecting worker-1 (overlapping node) with ASN 64513 (incompatible)
  * Verify:
    * FRR-k8s detects the conflict and retains the first valid configuration (RA1)
    * RA2 status reports `Degraded` condition with message identifying worker-1
      and the ASN conflict
    * Prefixes from RA1 are advertised; prefixes from RA2 are not advertised on
      worker-1
    * Correct RA2 to use ASN 64512; verify status becomes healthy and both RAs'
      prefixes are advertised
* **Dual-stack networks**:
  * Deploy a dual-stack Kind cluster with IPv4+IPv6 pod subnets.
  * Create `RouteAdvertisements` with gateway `nodeSelector`.
  * Verify:
    * Gateway nodes advertise **both IPv4 and IPv6** prefixes to BGP peers
      (inspect `FRRConfiguration` and BGP RIB):
      * Each gateway advertises its own IPv4 /24 + IPv4 /16 supernet,
        AND its own IPv6 /64 + IPv6 /48 supernet
    * **IPv4 ingress**: External IPv4 traffic reaches IPv4 pod addresses on
      non-gateway nodes via gateway nodes (test via curl/ping).
    * **IPv6 ingress**: External IPv6 traffic reaches IPv6 pod addresses on
      non-gateway nodes via gateway nodes.
    * OVS flows exist for both IPv4 and IPv6 remote subnets on gateway nodes
      (SGW and LGW modes).
    * **Failover works for both IP families**: Take down one gateway node,
      verify both IPv4 and IPv6 ingress traffic fail over to the remaining gateway.
* **IPv6-only networks**:
  * Deploy an IPv6-only Kind cluster.
  * Verify gateway advertisements, ingress, and failover work correctly for
    IPv6-only traffic.
* **Gateway readiness transitions (Ready ↔ NotReady)**:
  * Deploy a Kind cluster with 2 gateway nodes (worker-1, worker-2).
  * Create `RouteAdvertisements` with `nodeSelector` selecting both gateways.
  * Deploy pods on non-gateway nodes.
  * Verify ingress traffic reaches pods via gateway nodes (both Ready).
  * **Transition worker-1 to NotReady** (e.g., cordon + drain, or stop kubelet):
    * Verify worker-1's `FRRConfiguration` is **deleted** by RouteAdvertisements
      controller.
    * Verify worker-1's BGP sessions are withdrawn (inspect BGP RIB on external
      peer).
    * Verify ingress traffic fails over to worker-2 (remaining Ready gateway).
  * **Transition worker-1 back to Ready** (uncordon, or restart kubelet):
    * Verify worker-1's `FRRConfiguration` is **recreated** by
      RouteAdvertisements controller.
    * Verify worker-1 re-establishes BGP sessions and re-advertises routes
      (inspect BGP RIB).
    * Verify ingress traffic resumes load-balancing across both gateways via
      ECMP.
  * Run this test in both Shared Gateway Mode (SGW) and Local Gateway Mode (LGW).
* **Primary Layer 3 UDN with gateway nodeSelector**:
  * Deploy a Kind cluster with a primary Layer 3 CUDN.
  * Create `RouteAdvertisements` selecting the UDN with gateway `nodeSelector`.
  * Verify:
    * Gateway nodes advertise their own UDN subnet + UDN cluster supernet to BGP
      peers (in the correct VRF if `targetVRF: auto`).
    * External traffic reaches UDN pods on non-gateway nodes via gateway nodes
      (ingress path routed via supernet → gateway → overlay tunnel).
* **Layer 2 UDN rejection test**:
  * Verify that attempting to create `RouteAdvertisements` with `PodNetwork` +
    non-empty `nodeSelector` for a Layer 2 UDN is **rejected** with status
    condition `Degraded` and reason `InvalidTopology`.
* **Selector mismatch: RA selects MORE nodes than FRRConfiguration** (Story 7):
  * Deploy a 3-node cluster (worker-1, worker-2, worker-3).
  * Example subnets: worker-1: 10.244.1.0/24, worker-2: 10.244.2.0/24, worker-3:
    10.244.3.0/24, cluster supernet: 10.244.0.0/16.
  * Create `FRRConfiguration` with `nodeSelector` selecting only worker-1
    (worker-1 establishes BGP peering with external routers).
  * Create `RouteAdvertisements` with `nodeSelector` selecting worker-1 AND
    worker-2 (RA wants both nodes' subnets advertised, but only worker-1 has a
    BGP session).
  * Verify:
    * Worker-1 advertises **its own host subnet (10.244.1.0/24) AND the cluster
      supernet (10.244.0.0/16)** to BGP peers (inspect `FRRConfiguration` and BGP
      RIB). The cluster supernet covers all nodes including worker-2 and worker-3.
    * Worker-1 is set as **next-hop** for both advertised prefixes (its own /24
      and the /16 supernet).
    * **Ingress to pod on worker-1 (10.244.1.x)**: External traffic uses longest
      prefix match (10.244.1.0/24), reaches pod directly via worker-1 (no extra
      overlay hop).
    * **Ingress to pod on worker-2 (10.244.2.x)**: External traffic uses longest
      prefix match - no specific /24 route for worker-2, so uses cluster supernet
      (10.244.0.0/16). Traffic arrives at worker-1 (the advertisement gateway -
      the node advertising routes), worker-1 forwards via Geneve tunnel to worker-2
      (extra overlay hop, but works).
    * **Ingress to pod on worker-3 (10.244.3.x)**: Traffic also uses cluster
      supernet (10.244.0.0/16), arrives at worker-1, forwarded via overlay to
      worker-3. Worker-3's pods are reachable even though worker-3 is not selected
      by RA, because the supernet covers it.
* **Selector mismatch: FRRConfiguration selects MORE nodes than RA**:
  * Deploy a 3-node cluster (worker-1, worker-2, worker-3).
  * Create `FRRConfiguration` with `nodeSelector` selecting worker-1 AND worker-2
    (both nodes have BGP sessions).
  * Create `RouteAdvertisements` with `nodeSelector` selecting only worker-1 (RA
    wants only worker-1's subnet advertised).
  * Verify:
    * Worker-1 advertises its own subnet + cluster supernet to BGP peers.
    * Worker-2 has a BGP session with external routers but does **not advertise
      pod network routes** (inspect BGP RIB - no routes from worker-2).
    * **Ingress to pod on worker-1**: External traffic reaches pod on worker-1
      (either via direct /24 route or via supernet through worker-1).
    * **Ingress to pod on worker-2**: External traffic arrives via cluster
      supernet route to worker-1, worker-1 forwards via Geneve tunnel to worker-2.
    * **Ingress to pod on worker-3**: Same as worker-2 - via supernet to
      worker-1, then overlay to worker-3.
    * Worker-2 can still learn routes from external BGP peers for egress
      (optional test).

#### CRD Integration Tests

* Verify the updated CEL validation allows `nodeSelector` with `PodNetwork`.
* Verify no regression in existing validation rules.

#### Scale Testing

* Many nodes with few gateways: BGP prefix count, FRR reconcile latency, and
  breth0 flow count as nodes scale.

#### Cross-Feature Testing

* **EgressIP compatibility**:
  * Deploy a Kind cluster with EgressIP enabled.
  * Create `RouteAdvertisements` with `advertisements: [PodNetwork, EgressIP]` and
    gateway `nodeSelector` selecting nodes worker-1 and worker-2 as gateways.
  * **Test EgressIP on gateway node**:
    * Create `EgressIP` object assigning pod traffic to worker-1 (a gateway node).
    * Verify EgressIP functions correctly - traffic egresses with the EgressIP
      source address.
    * If destination requires BGP-learned routes, verify EgressIP node (worker-1,
      a gateway node) can reach it.
  * **Test EgressIP on non-gateway node (detect-and-warn scenario)**:
    * Create `EgressIP` object with `egressNodeSelector` that includes worker-3
      (NOT a gateway node). Wait for EgressIP to assign to worker-3.
    * Verify `RouteAdvertisements` status reports `Degraded` condition with reason
      `EgressIPNodeConflict` and message identifying worker-3 as outside the
      gateway union.
    * Verify the EgressIP /32 or /128 is **not advertised** via BGP (inspect BGP
      RIB on external peer - no /32 for this EgressIP address).
    * If destination requires BGP-learned routes, verify pod with this EgressIP
      **cannot** reach it (traffic is black-holed because worker-3 lacks BGP
      routes).
    * Verify ingress traffic to the EgressIP address **fails** (no BGP route for
      the EgressIP address because it's not advertised and not covered by pod
      network cluster supernet).
  * **Test EgressIP reassignment (gateway → non-gateway)**:
    * Create `EgressIP` object with `egressNodeSelector` selecting both worker-1
      (gateway) and worker-3 (non-gateway). Initial assignment is to worker-1.
    * Verify worker-1's `FRRConfiguration` includes the EgressIP /32 or /128.
    * Verify `RouteAdvertisements` status reports `Degraded` with reason
      `EgressIPNodeConflict` because the eligible pool includes worker-3
      (non-gateway), even though current assignment is to worker-1 (gateway). The
      Degraded status warns that reassignment to worker-3 will cause black-holes.
    * **Trigger reassignment** to worker-3 (e.g., drain worker-1, or delete and
      recreate EgressIP with selector matching only worker-3).
    * Verify worker-1's `FRRConfiguration` is updated to **withdraw** the EgressIP
      /32 or /128 (inspect BGP RIB - route withdrawn).
    * Verify `RouteAdvertisements` status reports `Degraded` with reason
      `EgressIPNodeConflict` (worker-3 is non-gateway).
    * Verify egress traffic from pods with this EgressIP now black-holes to
      BGP-learned destinations.
  * **Test EgressIP reassignment (non-gateway → gateway)**:
    * Start with EgressIP assigned to worker-3 (non-gateway, status Degraded).
    * Reassign to worker-1 (gateway) by updating `egressNodeSelector` to select
      **only** worker-1 (remove worker-3 from eligible pool).
    * Verify worker-1's `FRRConfiguration` is updated to **add** the EgressIP /32
      or /128.
    * Verify `RouteAdvertisements` status clears `Degraded` condition (becomes
      healthy) because BOTH conditions are now satisfied: eligible pool contains
      only gateway nodes (worker-1) AND current assignment is to a gateway node
      (worker-1).
    * Verify egress traffic from pods with this EgressIP can now reach BGP-learned
      destinations.

### Documentation Details

This OKEP requires updating the feature documentation to officially document and
support the selective BGP peering pattern:

* **Update `docs/features/bgp-integration/route-advertisements.md`**:
  * Add a **"Selective BGP Gateway Nodes"** section documenting the deployment
    pattern as officially supported (previously worked but was undocumented).
  * Include complete examples showing:
    * `FRRConfiguration` with `nodeSelector` (existing mechanism for selecting
      which nodes peer)
    * `RouteAdvertisements` with `nodeSelector` (new - selecting which nodes'
      subnets to advertise)
    * Combined example showing both selectors together (recommended pattern)
  * Document the **optimized advertisement pattern**:
    * **Layer 3** (default network and Layer 3 UDNs): Gateway nodes advertise their
      own allocated host subnet + configured cluster CIDR supernet (e.g., /24 + /16
      for IPv4, /64 + /48 for IPv6 in typical configurations)
  * Add **traffic flow diagrams** showing:
    * Ingress path: external → gateway node → overlay tunnel → pod on non-gateway
      node
    * Longest prefix match routing behavior (specific host subnet vs fallback
      cluster supernet)
    * Dual-stack traffic flow (both IPv4 and IPv6 paths)
  * Document restrictions:
    * Layer 2 networks not supported with non-empty `nodeSelector` (reason:
      `InvalidTopology`)
    * NoOverlay not supported with non-empty `nodeSelector` (reason:
      `InvalidTransport`)
    * EVPN not supported with non-empty `nodeSelector` (reason: `InvalidTransport`)
  * Add troubleshooting guide for common issues (e.g., EgressIP node mismatch,
    FRR configuration conflicts, Layer 2 topology rejection)

* **Update `RouteAdvertisements` API reference documentation**:
  * Update CEL validation description to reflect relaxed `nodeSelector` constraint
  * Document status conditions: `Degraded` with reasons `NoGatewayNodes`,
    `FRRConfigConflict`, `EgressIPNodeConflict`, `InvalidTransport`,
    `InvalidTopology`

* **Add diagrams to `docs/features/bgp-integration/`**:
  * Architecture diagram: gateway nodes with selective peering
  * Traffic flow diagram: ingress path through gateway nodes with ECMP
  * Advertisement diagram: showing supernet + specific subnet advertisements

* **Add `mkdocs.yml` entry** for this OKEP in the `okeps/` navigation section.

## Risks, Known Limitations and Mitigations

* **Gateway bottleneck for ingress**: All north-south ingress traffic flows
  through gateway nodes. Users should select multiple gateway nodes for ECMP and
  monitor gateway node capacity. Egress traffic from pods exits directly via each
  node's default gateway (no gateway rerouting).
* **Asymmetric ingress vs egress paths**: For pods on non-gateway nodes, there are
  two distinct traffic patterns:
  
  * **Ingress-initiated traffic** (external → pod → reply): Traffic enters via a
    BGP gateway node (selected by external ECMP), which forwards to the pod's node
    via Geneve tunnel. **Replies to this ingress traffic follow the normal pod
    egress path** via the pod's node's default gateway (see "ECMP and Reply Path
    Behavior" in Implementation Details). The reply does NOT necessarily route back
    through the same gateway that received the request - it follows the pod's node's
    default egress path with pod IP as source (SNAT disabled).
  
  * **Pod-initiated egress traffic** (pod → external): Traffic exits directly from
    the pod's node via the node's default gateway, bypassing BGP gateway nodes
    entirely. **Egress uses pod IP as source** (SNAT is disabled). Reply traffic
    from the external router will be routed to a gateway node (via BGP supernet
    advertisement), and the gateway forwards it back to the pod via overlay tunnel.
  
  * **Path asymmetry summary**: Both ingress-replies and pod-initiated egress use
    the same path - pod's node's default gateway. Ingress arrives via gateway
    nodes, but egress (including replies) leaves via the pod's node's default
    gateway. From the external network's perspective: ingress traffic is sent to
    gateway nodes (via BGP routes), but replies/egress arrive from pod nodes (via
    pod's node's default gateway).
  * This path asymmetry means:
    * Pods on non-gateway nodes **cannot reach destinations that require
      BGP-learned routes** (only gateway nodes have these routes). For destinations
      reachable via the node's default gateway (standard internet routing), egress
      works with pod IP as source.
    * External networks with stateful firewalls/connection tracking must be
      configured to allow asymmetric flows for the advertised pod network supernet.
      Ingress and egress may use different paths.
    * Operators should enable BGP on nodes that need to reach BGP-learned
      destinations, or schedule workloads requiring BGP-learned routes on gateway
      nodes.
* **NoOverlay not supported**: The feature requires a tunnel overlay for
  cross-node forwarding. NoOverlay deployments must continue to advertise from
  all nodes.
* **Interaction with EgressIP**: When both EgressIP and BGP gateway `nodeSelector`
  are configured, **EgressIP nodes should be in the BGP gateway union** to avoid
  traffic black-holes. If an EgressIP is assigned to a non-gateway node, that node
  will not have BGP-learned routes, and egress traffic requiring those routes will
  be black-holed after SNAT. The RouteAdvertisements controller **detects and
  reports** this condition via a `Degraded` status condition with reason
  `EgressIPNodeConflict` but does **not prevent** the assignment (detect-and-warn
  approach). The controller monitors both the eligible node pool
  (`EgressIP.spec.egressNodeSelector`) and current assignments
  (`EgressIP.status.items[].node`), updating status on reassignments. When
  `EgressIP` is in `spec.advertisements` and an EgressIP is assigned to a gateway
  node, the EgressIP /32 or /128 is advertised via that node's `FRRConfiguration`;
  when assigned to a non-gateway node or when `EgressIP` is not in
  `spec.advertisements`, the /32 or /128 is not advertised. Operators must ensure
  EgressIP nodes are in the gateway union when advertising PodNetwork with
  selective nodeSelector, or accept
  degraded egress behavior for pods using EgressIP.
* **FRR configuration conflicts**: When multiple `RouteAdvertisements` select
  overlapping nodes with incompatible FRR settings (e.g., different ASNs for the
  same router in the same VRF), FRR-k8s retains the previous valid configuration
  and rejects the conflicting change. The affected `RouteAdvertisements` reports
  a `Degraded` status condition identifying the node and conflict type. Operators
  must ensure FRR router settings are compatible across all `RouteAdvertisements`
  selecting overlapping nodes for the same network. Compatible settings allow
  intentional multi-ToR deployments where different RAs peer with different
  provider routers.
* **Empty gateway selection**: If `nodeSelector` matches no ready nodes,
  `RouteAdvertisements` reports status condition `Degraded` with reason
  `NoGatewayNodes` and does not generate gateway `FRRConfiguration` objects
  until at least one gateway node is available.
* **EVPN transport with selective nodeSelector is rejected**: EVPN requires
  **all nodes to participate in the EVPN fabric** (VTEPs, VXLAN devices, Linux
  bridge, internal iBGP full-mesh peering) regardless of `nodeSelector`. While
  `FRRConfiguration.spec.nodeSelector` could control which nodes establish
  external BGP sessions with provider routers, all nodes would still need to run
  FRR for internal iBGP peering. This eliminates the primary value proposition
  of selective `nodeSelector` — eliminating FRR on non-gateway nodes. With
  overlay (Geneve) transport, non-gateway nodes do not need FRR at all. EVPN with
  non-empty `nodeSelector` is rejected; use overlay (Geneve) transport for
  gateway-node deployments. EVPN continues to work when `nodeSelector` is empty
  (selects all nodes).
* **SNAT behavior is uniform across all nodes**: When `PodNetwork` is advertised
  via BGP, SNAT is disabled cluster-wide (as documented in existing BGP
  implementation) via the NAD annotation. This applies to **all** nodes,
  including non-gateway nodes. Pod traffic exits with the pod IP as source on all
  nodes. For non-gateway nodes, reply traffic will arrive at a gateway node (via
  BGP routing) with the pod IP as destination, and the gateway forwards it to the
  pod's node via overlay tunnel. This is the expected and required behavior for
  advertised subnets — SNAT cannot be selectively enabled on non-gateway nodes
  because their pod IPs are included in the advertised supernet.
* **Layer 2 UDN /32 or /128 host route scalability** (applies to Layer 2 with
  **empty** `nodeSelector` only): Gateway nodes advertise individual /32 (IPv4)
  or /128 (IPv6) host routes for each pod on that gateway. During concurrent VM
  live migrations, multiple /32 or /128 routes may be added and withdrawn
  simultaneously. This creates BGP update churn but is bounded by the number of
  VMs on gateway nodes. Mitigation: BGP route dampening (if supported by external
  routers) and rate-limiting VM migrations. For large-scale VM deployments,
  monitor BGP update rates and external router capacity.
* **Cloud route limits**: Some cloud providers limit the number of routes per BGP
  session (e.g., AWS BGP Cloud Connector limits to 100 routes). The optimized
  advertisement model may exceed these limits in large clusters:
  * **Layer 3**: Each gateway advertises its own allocated host subnet + configured
    cluster supernet (2 routes per IP family per gateway, e.g., /24 + /16 for IPv4)
  * **Layer 2** (with empty `nodeSelector`): Each gateway advertises cluster
    subnet + /32 or /128 for each local pod (1 + N routes per IP family, where N
    = pods on that gateway)
  
  For cloud deployments with route limits, operators should either: (1) use Layer
  3 networks (minimal route count), (2) limit the number of pods on gateway nodes
  in Layer 2 deployments, or (3) use route aggregation if the cloud connector
  supports it. Cloud route limits are a cloud provider constraint, not an
  OVN-Kubernetes limitation.

## OVN-Kubernetes Version Skew

No target release has been scheduled for this feature. The OKEP must be accepted
and implementation completed before a release milestone is assigned.

## Backwards Compatibility

* **API**: The `nodeSelector` field already exists. Removing the CEL validation
  rule is a relaxation — previously invalid configurations become valid. No
  existing valid configurations are affected.
* **Same-network conflict rule relaxed**: OKEP-5296 prohibited multiple
  `RouteAdvertisements` from selecting the same network. This proposal **relaxes
  that restriction**: multiple `RouteAdvertisements` may now select the same
  network (each generates independently labeled `FRRConfiguration` objects that
  FRR-k8s merges). This is a compatibility expansion — configurations that were
  previously rejected are now allowed; no existing valid configurations break.
* **Existing behavior** (varies by network topology):
  * **Layer 3 networks** (default cluster network, primary Layer 3 UDNs): When
    `nodeSelector` is empty (selects all nodes), the behavior is identical to
    today — each node advertises only its own host subnet. The new "advertise
    all nodes' subnets" behavior only activates when `nodeSelector` is non-empty.
  * **Layer 2 networks** (primary Layer 2 UDNs): Only supported with **empty**
    `nodeSelector` (all nodes). Non-empty `nodeSelector` is **rejected** for
    Layer 2 (see scope matrix). When `nodeSelector` is empty, all nodes advertise
    the cluster-wide network subnet (unchanged behavior).
* **E2E tests**: Existing BGP/EVPN E2E tests should continue to pass without
  modification. New E2E tests will be added for the gateway node pattern.

## Alternatives

### Alternative 1: Gateway nodes advertise only their own subnets

Each gateway node advertises only its own host subnet. Pods on non-gateway nodes
are not reachable from the external network. Users must schedule
externally-reachable workloads on gateway nodes.

**Rejected because**: Users need the flexibility to schedule workloads on any
node while maintaining external reachability through gateway nodes.

### Alternative 2: Gateway nodes advertise aggregate CIDR

Instead of individual host subnets, gateway nodes advertise the parent network
CIDR (e.g., `10.244.0.0/16`). This is simpler but provides coarse-grained
routing.

**Rejected because**: Advertising individual host subnets provides more precise
routing information to external peers and is consistent with existing behavior.

## References

* [OKEP-5296: OVN-Kubernetes BGP Integration](okep-5296-bgp.md) — original BGP
  OKEP listing node selection as a future goal.
* [GitHub issue #7040](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/7040)
* [Route Advertisements documentation](../features/bgp-integration/route-advertisements.md)
* [FRR-k8s](https://github.com/metallb/frr-k8s)
