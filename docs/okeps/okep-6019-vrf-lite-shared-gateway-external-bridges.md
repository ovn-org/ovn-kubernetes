# OKEP-6019: VRF-Lite with Shared Gateway Mode using Uplinks

* Issue: [#6019](https://github.com/ovn-org/ovn-kubernetes/issues/6019)

## Networking Glossary

| Term           | Definition                                                                                 |
|----------------|--------------------------------------------------------------------------------------------|
| **VRF-Lite**   | Linux host VRF separation where interfaces are attached to specific VRFs without MPLS.     |
| **Shared GW**  | OVN and host share an external OVS bridge for north/south traffic.                         |
| **CUDN**       | `ClusterUserDefinedNetwork`, cluster-scoped primary or secondary user-defined network API. |
| **MEG**        | Multiple External Gateways feature for pod egress steering using additional gateway(s).    |
| **Uplink**     | A named, cluster-scoped connectivity target that CUDNs can use for external traffic.       |
| **OVSBridge**  | An OVS bridge-backed uplink type. This is the only uplink type in this OKEP.               |

## Problem Statement

OVN-Kubernetes support for VRF-Lite is limited to local gateway mode today. VRF-Lite also requires manual host
preparation, including interface-to-VRF attachment. This is operationally expensive and error-prone in heterogeneous node
environments.

OVN-Kubernetes currently assumes one default external bridge and optionally one extra bridge via
`config.Gateway.EgressGWInterface`. This model is not sufficient for shared gateway mode VRF-Lite, where different CUDNs
may need to use different pre-provisioned OVS bridges and physical uplinks.

The goal of this OKEP is not to make OVN-Kubernetes a general host bridge provisioning system. Bridge creation, bridge
port membership, IP addressing, default routes, MTU, and VLAN setup remain external preparation tasks, for example via
NMState or other node configuration tooling. OVN-Kubernetes consumes that prepared state, wires it into OVN and VRF-Lite,
and publishes typed per-node gateway state for CUDN reconciliation.

## Goals

* Support VRF-Lite with shared gateway mode by allowing primary Layer2 and Layer3 CUDNs to reference a list of named
  `Uplink` objects, limited to one item in this OKEP.
* Introduce a generic `Uplink` API that can be extended later beyond OVS bridges.
* Support one `OVSBridge` node config type in this OKEP, where the backing OVS bridge is pre-created by the
  administrator.
* Allow each `Uplink` to describe different host-side gateway interfaces for different node groups through
  `spec.nodeConfigs`, while resolving to at most one link per node.
* Store per-node discovery, validation, and gateway data in a typed `UplinkState` resource.
* Use `UplinkState` as the typed gateway configuration source for this feature instead of adding CUDN entries to the node
  `k8s.ovn.org/l3-gateway-config` annotation.
* Configure OVN bridge mappings automatically for CUDNs that select a `Uplink`.
* Attach the deployment-specific gateway interface to the matching CUDN VRF only when matching `RouteAdvertisements`
  resolves the CUDN to a per-CUDN VRF. In a non-DPU deployment without hardware offload, this is the OVS bridge internal
  interface. With SmartNIC/offload, this is the accelerated VF/PF representor used as the host gateway interface. In a DPU
  deployment, the DPU-Host attaches the host PF to the host-side CUDN VRF, while the DPU side attaches the resolved bridge
  `LOCAL` interface to the DPU-local route-import VRF. When the CUDN uses the default routing domain, these interfaces
  remain in the default VRF.
* Preserve existing behavior for the default gateway bridge and current MEG deployments when the new API is not used.
* Support DPU deployments by splitting `UplinkState` discovery between the DPU and DPU-Host while keeping OVS programming
  on the DPU side.
* Support service traffic through the CUDN's resolved OVS bridge by programming the relevant per-UDN service flows there.

## Non-Goals

* Configuring IP addresses, routes, MTU, or VLAN tags on the selected host interface or resolved bridge. OVN-Kubernetes
  only consumes existing VLAN tags from OVS and applies the corresponding OVN gateway configuration.
* Replacing NMState or other node network provisioning tools.
* Replacing FRR/FRR-K8S APIs or changing BGP peering semantics.
* Collapsing multiple CUDNs into the same non-default VRF for Uplink-backed VRF-Lite isolation. CUDNs may intentionally
  share the default VRF/routing domain when `RouteAdvertisements targetVRF: default` is used; that is not VRF isolation.

## Future Goals

* Support `Uplink` in local gateway mode, including any required `Interface` uplink type or host-routing integration.
* Support `Uplink` with EVPN transport after shared gateway EVPN support exists and the relationship between Uplink
  bridge selection, VTEPs, and EVPN underlay selection is defined.
* Allow localnet CUDNs to reference `Uplink`, so OVN-Kubernetes can configure bridge mappings automatically instead of
  requiring the user to coordinate `physicalNetworkName` and bridge mappings separately.
* Allow a CUDN to reference multiple `Uplink` objects for ECMP, multi-homing, or reachability to additional external
  subnets.
* Consider OVN-Kubernetes-owned bridge lifecycle in a follow-up proposal if there is clear demand.
* Evaluate consumers beyond VRF-Lite CUDNs, such as secondary EgressIP, EgressService, or ICNI, without expanding this
  OKEP's initial scope.
* Converge legacy `EgressGWInterface` and default gateway bridge behavior into a more general uplink model.
* Evaluate migrating the default shared gateway bridge and legacy MEG bridge gateway data from the
  `k8s.ovn.org/l3-gateway-config` node annotation into typed `UplinkState`-style status, so default and non-default
  gateway bridges eventually use the same gateway data model.

## Introduction

The [BGP enhancement](./okep-5296-bgp.md) introduced RouteAdvertisements and VRF-aware route export/import with FRR. It
also introduced VRF-Lite use cases for CUDNs, but those use cases rely on manual host configuration and are currently
scoped to local gateway mode in the documentation.

OVN-Kubernetes node gateway behavior today assumes:

* One default external bridge (`br-ex` / `breth0`) that OVN-Kubernetes can create automatically.
* One optional secondary bridge selected by `config.Gateway.EgressGWInterface`, primarily for MEG related paths on the
  cluster default network (CDN); MEG is not supported for CUDNs.

This works for default networking and limited dual-uplink use cases, but not for shared gateway VRF-Lite environments
where multiple tenant networks may need distinct uplinks while keeping the shared gateway datapath and DPU offload
advantages.

In DPU deployments, OVS runs on the DPU rather than the DPU-Host. OVN-Kubernetes therefore needs a typed way for the DPU
and DPU-Host components to exchange gateway data for a selected uplink without relying on host-local PCI identifiers or
extending the node `l3-gateway-config` annotation with CUDN-specific entries.

## User Stories

### Story 1: Tenant VRFs on different pre-provisioned bridges

As an admin, I want to create CUDNs `blue` and `red` that use different already-created OVS bridges, `br-blue` and
`br-red`, on my nodes. I create `Uplink` CRs named `blue` and `red`, each with an `OVSBridge` node config that points to
the host-side gateway interface backed by the appropriate bridge for the matching node group. I then configure each CUDN
with `spec.uplinks`.

OVN-Kubernetes validates the host interface and resolves the backing bridge on each relevant node, configures the CUDN
localnet bridge mapping, attaches the selected host interface to the CUDN VRF when `RouteAdvertisements` resolves the
CUDN to per-CUDN VRF-Lite isolation, and publishes `UplinkState` with the gateway data needed by the CUDN controller and
RouteAdvertisements.

### Story 2: Split default network and CUDN external traffic

As an admin, I want the cluster default network to keep using the existing default gateway bridge (`br-ex` / `breth0`),
while a primary CUDN uses a different pre-created OVS bridge. I create a `Uplink` for the host-side gateway interface
backed by that bridge and configure the CUDN with `spec.uplinks`. OVN-Kubernetes programs the CUDN external path on the
resolved bridge while leaving the default network bridge behavior unchanged.

Without BGP, CUDN egress follows normal shared gateway behavior and is SNAT'ed to the selected host interface or gateway
IP when applicable. This OKEP does not add a general non-BGP ingress-to-pod routing mechanism.

With BGP, `RouteAdvertisements` determines whether the selected uplink participates in the default routing domain or in a
per-CUDN VRF-Lite routing domain. OVN-Kubernetes updates the selected uplink's VRF membership and the UDN VRF route
programming from that effective routing-domain decision.

## Proposed Solution

Introduce a new cluster-scoped `Uplink` CRD and add an `uplinks` list to CUDN. This OKEP limits the CUDN list to one
item, but the list shape leaves room for future multi-uplink routing without another CUDN API shape change.

At a high level:

1. The admin prepares the OVS bridges and bridge ports on each node using existing host configuration tooling.
2. The admin creates a `Uplink` whose node configs map node groups to host-side gateway interface names.
3. The admin creates a primary Layer2 or Layer3 CUDN with `spec.uplinks: [<uplink-name>]`.
4. OVN-Kubernetes validates the selected host interface, resolves the backing OVS bridge on each relevant node, and
   publishes one `UplinkState` per `Uplink`/node.
5. OVN-Kubernetes configures OVN bridge mappings and the CUDN shared-gateway external path using `UplinkState`.
6. OVN-Kubernetes derives the CUDN's effective routing domain from matching `RouteAdvertisements`. When the CUDN resolves
   to a per-CUDN VRF, OVN-Kubernetes attaches the selected host interface to that VRF. When the CUDN resolves to the
   default VRF, or no matching `RouteAdvertisements` selects it, the selected host interface remains in the default VRF.
7. Existing RouteAdvertisements behavior remains; `targetVRF: auto` can now work with shared gateway mode through the
   selected uplink and `UplinkState`.

### Workflow Description

1. Admin creates one or more pre-existing OVS bridges on the nodes.
2. Admin creates a `Uplink` CR with one or more `OVSBridge` node configs.
3. Admin creates a primary Layer2 or Layer3 CUDN with `spec.uplinks` referencing the `Uplink`.
4. For VRF-Lite route advertisement, admin configures FRR peering as done today.
5. For VRF-Lite route advertisement, admin creates `RouteAdvertisements` with `targetVRF: auto` selecting the CUDN.
6. OVN-Kubernetes validates host interface and bridge state, updates `UplinkState`, configures bridge mappings, and
   attaches the selected host interface to the CUDN VRF only when matching `RouteAdvertisements` resolves the CUDN to a
   per-CUDN VRF on nodes where the CUDN is active.
7. When BGP is configured, routes are advertised and received as today; dataplane uses shared gateway bridge flows and
   UDN VRF routing semantics.

Host interface and bridge validation means OVN-Kubernetes checks that the configured host interface exists, resolves to
one backing OVS bridge, the relevant link states are up, and the resolved bridge or gateway interface has the IP address
data OVN-Kubernetes needs for gateway configuration. In Full mode, resolution means the host interface is attached to the
local OVS bridge. In DPU mode, resolution means the host-side interface can be matched to the DPU-side representor and
bridge. OVN-Kubernetes discovers this information from existing node state; it does not configure these admin-provisioned
properties through the `Uplink` API.

## API Details

### New CRD: Uplink

An `Uplink` represents a physical network path out of the node. Different `Uplink` types can describe different ways to
model that path. In this OKEP, the only supported type is `OVSBridge`, which may represent a network path across host,
DPU, or SmartNIC boundaries. The `Uplink` gives OVN-Kubernetes a named path that it can connect the logical OVN topology
to.

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: Uplink
metadata:
  name: blue-underlay
spec:
  nodeConfigs:
  - type: OVSBridge
    nodeSelector:
      matchLabels:
        rack: rack-a
    hostInterfaceName: pf0hpf
  - type: OVSBridge
    nodeSelector:
      matchLabels:
        rack: rack-b
    hostInterfaceName: pf1hpf
  - type: OVSBridge
    nodeSelector:
      matchLabels:
        offload: "false"
    hostInterfaceName: br-blue
status:
```

The `Uplink` name is the stable API reference used by CUDNs. It does not need to match the host interface name or the
resolved OVS bridge name on any node.

#### UplinkSpec

* `nodeConfigs` (required): list of node-scoped uplink configs. Each item describes the single link this `Uplink` uses on
  nodes selected by that item.
* `nodeConfigs[*].type` (required): uplink type and union discriminator. This OKEP supports only `OVSBridge`.
* `nodeConfigs[*].nodeSelector` (required): nodes where this config applies. An empty selector matches all nodes.
* `nodeConfigs[*].hostInterfaceName` (required): host-visible Linux interface name that carries the gateway L3
  identity for this uplink on nodes selected by this config.

`hostInterfaceName` has the same API meaning in all modes: it is the host-visible Linux interface carrying the uplink
gateway L3 identity on the node. What differs by deployment mode is how OVN-Kubernetes resolves that interface to the OVS
bridge used for bridge mappings and OpenFlow. In Full mode, the interface is expected to be attached directly to the local
pre-existing OVS bridge, typically the bridge `LOCAL` interface or a host-side internal port. In DPU mode, the interface
is the DPU-Host-side gateway interface; OVN-Kubernetes uses its MAC and representor peer relationship to discover the
DPU-side representor and OVS bridge. DPU-side representor names are not part of the `Uplink` API or published in
`UplinkState`. Different interface names on different node groups are represented with multiple `nodeConfigs` and
`nodeSelector`; CIDR-based interface selection is out of scope for this OKEP.

`nodeConfigs` is a per-node selection table, not a request to use multiple links from one `Uplink`. For any given node,
one `Uplink` resolves to at most one host-side gateway interface and one backing OVS bridge.

This OKEP does not define OVSBridge-specific spec fields. `type: OVSBridge` means OVN-Kubernetes resolves
`hostInterfaceName` to a pre-existing OVS bridge and reports the resolved bridge name in `UplinkState.status.ovsBridge`.

The `Uplink` object intentionally does not describe the OVS bridge name, bridge ports, physical NICs, IP addresses, MTU,
default gateways, or VLANs. Those are properties of the pre-provisioned node network. OVN-Kubernetes discovers the subset
of that state needed for OVN gateway configuration and reports it through `UplinkState`.

#### Uplink validation

Static API-shape rules are enforced by CRD schema/kubebuilder validation and CEL. Rules that depend on live node labels
or node-local interface/OVS state are enforced by controllers and reported through `Uplink` and `UplinkState` conditions.

* `spec.nodeConfigs` has `MinItems=1` and `MaxItems=64`.
* `nodeConfigs[*].type` must be `OVSBridge`.
* `nodeConfigs[*].hostInterfaceName` must be a valid Linux interface name. Existence, resolution to exactly one
  backing OVS bridge, link state, IP address data on the resolved bridge or gateway interface, MAC address,
  route/default-gateway data when present, and any OVS VLAN tag are discovered or validated by the node reconcilers
  because they depend on node-local state. OVN-Kubernetes does not configure these admin-provisioned properties through
  the `Uplink` API.
* For a given node, at most one node config in a `Uplink` may apply in this OKEP. Because `nodeSelector` overlap depends
  on live node labels, this cannot be fully enforced by CRD schema validation. The Uplink controller resolves selected
  node configs per node; if more than one applies to a node, the `Uplink` reports `Ready=False` and affected
  `UplinkState` objects report `Resolved=False` until the overlap is resolved.
* If no node config applies to a node where a CUDN referencing the `Uplink` is active, cluster-manager reports
  `UplinkNotFoundForNode` in CUDN status and no gateway configuration is generated for that node.
* `Uplink.spec.nodeConfigs` is mutable. Updating a node config can temporarily degrade CUDNs that reference the `Uplink`
  while node state is rediscovered.

#### Uplink conditions

`Uplink.status.conditions` is owned by the ovnkube-cluster-manager Uplink controller and contains aggregate state only.
The controller derives this state from `Uplink`, Nodes, and the per-node `UplinkState` objects:

* `Ready`: `True` when at least one node is selected and every selected node has a usable matching `UplinkState`; `False`
  when no nodes are selected, at least one selected node has an error or required missing state in its corresponding
  `UplinkState`, or current node labels make the `nodeConfigs` selection ambiguous.

`Uplink.status.conditions[Ready]` aggregates only the node-local `Resolved` condition from each `UplinkState`.
It does not aggregate `UplinkState.status.conditions[GatewayReady]`, because that condition describes gateway programming
for the CUDNs active on one Uplink/node pair and is reflected in the corresponding CUDN `UplinksReady` conditions.

The aggregate `Ready` condition uses positive polarity and the following reasons:

* `Ready`: at least one node is selected and all selected nodes have matching `UplinkState` objects with
  `Resolved=True`.
* `InvalidSpec`: the controller cannot resolve the `Uplink` spec.
* `NodeSelectorOverlap`: one or more nodes are selected by multiple `nodeConfigs` entries.
* `NoMatchingNodes`: `Ready=False` because no nodes match any `nodeConfigs` entry.
* `MissingUplinkState`: one or more selected nodes do not have a matching `UplinkState`.
* `UplinkStateNotResolved`: one or more matching `UplinkState` objects have `Resolved=False` or do not match the
  selected node config.
* `UplinkStateIdentityError`: an `UplinkState` spec does not match the expected Uplink/node identity.
* `UplinkTerminating`: the `Uplink` is being deleted and cannot accept new CUDN references.

For node-scoped failures, the condition message includes the number of affected nodes out of the total selected nodes and
a bounded sample of node names and underlying `UplinkState` reasons. The aggregate reason is selected deterministically
when failures have different causes. This distinguishes partial availability from a failure affecting every selected
node without creating a negative-polarity condition.

Because `nodeConfigs[*].nodeSelector` depends on node labels, `Uplink.status` can change without `Uplink.spec` changing.
This is expected. For example, labeling a new node into a node config creates or updates that node's `UplinkState`; if
the host interface is missing or cannot be resolved to an OVS bridge on that node,
`Uplink.status.conditions[Ready]` becomes false.

When one or more CUDNs reference an `Uplink`, ovnkube-cluster-manager keeps a finalizer on the `Uplink` so it cannot be
deleted while still selected by a CUDN.

`Uplink.status.conditions[Ready]` is aggregate, operator-facing health for the `Uplink` object. It is not consumed
directly as CUDN readiness, because two Dynamic UDN-backed CUDNs can reference the same `Uplink` while being active on
different node sets. CUDN uplink readiness is derived by cluster-manager from the referenced `Uplink` spec and the
`UplinkState` objects relevant to that specific CUDN/UDN active node set. This OKEP allows only one referenced `Uplink`,
but the status logic should be structured around the list. Healthy node-local Uplink paths remain usable when aggregate
`Ready=False`; for example, a Dynamic CUDN active only on healthy nodes can still report `UplinksReady=True`.

### New CRD: UplinkState

Per-node discovery and gateway state is stored in a separate cluster-scoped `UplinkState` resource instead of a large
list in `Uplink.status` or a node annotation. This avoids many nodes contending on the same `Uplink` object and keeps
node-owned gateway data in a typed API.

`UplinkState` is keyed by `Uplink` and node. A typical object name is `<uplink-name>.<node-name>` when that fits within
Kubernetes object name limits. If the combined name would be too long, OVN-Kubernetes uses a deterministic
DNS-subdomain-safe name built from truncated name prefixes plus a stable hash of the full `<uplink-name>/<node-name>`
pair. Controllers must treat `spec.uplinkName` and `spec.nodeName` as the canonical identity, not parse identity from the
object name. These fields are immutable and selectable so controllers and users can query by Uplink and node without
relying on generated names, labels, or annotations. On `UplinkState` delete events, controllers should derive the
affected `Uplink` from `spec.uplinkName` when available. If that identity source is not present in the delete tombstone,
the controller may fall back to reconciling all `Uplink` objects. Users do not create or update this resource;
OVN-Kubernetes exclusively owns it.

#### UplinkState spec

The spec contains only the immutable identity of the state object:

* `uplinkName` (required, immutable): `Uplink` this state belongs to.
* `nodeName` (required, immutable): node this state belongs to.

Example field selector queries:

```bash
kubectl get uplinkstates --field-selector spec.uplinkName=blue-underlay
kubectl get uplinkstates --field-selector spec.uplinkName=blue-underlay,spec.nodeName=ovn-worker-a
```

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: UplinkState
metadata:
  name: blue-underlay.ovn-worker-a
spec:
  uplinkName: blue-underlay
  nodeName: ovn-worker-a
status:
  type: OVSBridge
  hostInterfaceName: pf0hpf
  ovsBridge:
    name: br-blue
  macAddress: 02:42:c0:00:02:06
  ipAddresses:
  - 192.0.2.6/24
  defaultGateways:
  - 192.0.2.1
  conditions:
  - type: Resolved
    status: "True"
    reason: Resolved
  - type: GatewayReady
    status: "True"
    reason: GatewayConfigured
```

#### UplinkState status

`UplinkState` intentionally does not use a separate Kubernetes status subresource. OVN-Kubernetes creates the object
with its immutable identity spec and initial status in one API operation, then updates the status fields with server-side
apply. This avoids a second API operation during creation while preserving the immutable identity needed for
field-selector based watches and queries.

The top-level status fields describe generic resolved L3 gateway data that OVN-Kubernetes needs regardless of the uplink
type: selected host interface, MAC address, IP addresses, default gateways, resolution, and aggregate node-local gateway
programming state. Type-specific resolved data belongs under the corresponding union field. For this OKEP, only
`ovsBridge.name` is OVS bridge-specific. Future uplink types should add their own type-specific status blocks rather than
moving the common gateway fields under `ovsBridge`.

* `type`: resolved uplink type. This OKEP supports only `OVSBridge`.
* `hostInterfaceName`: host-visible Linux interface selected by the `Uplink` node config. This field has the same meaning
  in all modes: the interface carrying the host-side gateway L3 identity. In Full mode, OVN-Kubernetes resolves it to a
  local OVS bridge. In DPU mode, OVN-Kubernetes uses it as the DPU-Host-side handoff for discovering the DPU-side
  representor and bridge. DPU-side representor names are not exposed in `UplinkState`.
* `ovsBridge` (required when `type` is `OVSBridge`): resolved OVS bridge data.
* `ovsBridge.name`: resolved OVS bridge used for CUDN bridge mappings and OpenFlow on this node. In Full mode this is
  resolved locally from `hostInterfaceName`. In DPU mode this is resolved and published by the DPU-side reconciler after
  matching the host interface MAC to the DPU-side representor and bridge.
* `macAddress`: MAC address used for the OVN gateway interface on this bridge. In DPU deployments, the DPU-Host side
  publishes the selected host interface MAC here so the DPU can find the corresponding representor without publishing PF
  IDs or host PCI addresses.
* `ipAddresses`: host-side shared gateway IP addresses discovered from the selected host interface.
* `defaultGateways`: default route next-hop IPs discovered from host routing for the selected host interface, when
  present. Discovery reads the host interface's routes from the routing table they live in: the VRF's table when the
  interface is enslaved to a VRF, the main table otherwise. This maps to `next-hops` in the existing `l3-gateway-config`
  shape when internal compatibility requires it. If no default route exists, this field can be empty; the Uplink still
  resolves, no condition degrades, and egress can still work for destinations covered by BGP-learned routes imported by
  OVN-Kubernetes. Platform monitoring can alert on an empty field where a default gateway is expected. Host route
  changes generate netlink events, which the node's route manager already subscribes to; Uplink discovery registers
  for them and rediscovers the node's Uplinks when a default route changes in any routing table, once the change has
  settled, publishing added gateways and withdrawing removed ones. The OVN gateway router's default routes follow the
  published list, withdrawn next hops included. A change limited to the default gateways updates the routes of an
  active network without rebuilding its Uplink gateway. While an addressed IP family lacks a default gateway,
  discovery also polls the host routes at a fixed interval as a fallback. With VRF-Lite (`targetVRF: auto`),
  the selected host interface's existing static and DHCP routes, including defaults, are preserved in the CUDN VRF
  and discovery reads them there. Discovery selects the lowest-metric
  defaults per IP family through the selected interface and, among their next hops, keeps only the ones with the
  highest weight, since neither `UplinkState` nor OVN static routes can express weights. It publishes the distinct
  gateways in deterministic order, up to 256 total across both families per node and Uplink.
  More than 256 fails discovery with `GatewayInfoUnavailable` and retries, rather than publishing a subset. The API
  bound accommodates 128 next hops per family; see the [feature documentation](../features/user-defined-networks/uplinks.md)
  for its rationale. Polling stops once each addressed family has a gateway; route events drive later rediscovery
  ([#6784](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6784)).

In DPU deployments, the DPU-Host initializes the top-level status with host-side L3 data. The DPU side patches DPU-local
fields on the same object, including `ovsBridge.name` and DPU-side validation state.

Gateway mode is derived from OVN-Kubernetes configuration rather than
stored per `UplinkState`. `UplinkState` stores only reusable per-node host gateway data and is not serialized into
`k8s.ovn.org/l3-gateway-config` for CUDNs.

#### UplinkState conditions

`UplinkState.status.conditions` reports independent node-local conditions, and each condition has a single writer.
The Uplink discovery reconciler owns `Resolved` (published by the DPU side in split DPU mode), the DPU-Host discovery
reconciler owns `HostDataReady` (split DPU mode only), the node-level UplinkState gateway status controller owns
`GatewayReady`, and its DPU-Host counterpart owns `HostGatewayReady` (split DPU mode only);
a gateway programming failure does not overwrite successful Uplink resolution. Individual CUDN gateway reconcilers do
not write `GatewayReady` directly.

The `Resolved` condition reports host interface, gateway data, and OVS bridge discovery. Reasons describe the current
discovery state:

* `Resolved`: bridge discovery and gateway data discovery are complete for this `Uplink`/node.
* `HostInterfaceNotFound`: the selected `hostInterfaceName` does not exist on the node or DPU-Host.
* `BridgeNotFound`: the selected host interface cannot be resolved to a backing OVS bridge.
* `BridgeUplinkNotFound`: the bridge exists, but OVN-Kubernetes cannot resolve its physical uplink port.
* `BridgeInvalid`: the bridge exists but has an unsupported or ambiguous layout.
* `DefaultGatewayBridgeUnsupported`: the resolved bridge is the node's default shared gateway bridge, which cannot be
  selected as an `Uplink` in this OKEP.
* `InvalidHostInterface`: the selected host interface resolves to an unsupported bridge role, such as the bridge's
  physical uplink port instead of its host gateway interface, or the interface has no usable MAC address (missing,
  all-zero or multicast) on the node or DPU-Host.
* `GatewayInfoUnavailable`: OVN-Kubernetes cannot discover the gateway MAC/IP data needed for OVN configuration.
* `WaitingForDPUHost`: DPU-side reconciliation is waiting for the DPU-Host to publish complete host-side L3 data. The
  DPU-Host-side cause is reported on the `HostDataReady` condition.
* `NodeSelectorOverlap`: more than one `Uplink.spec.nodeConfigs` entry applies to this node, so OVN-Kubernetes cannot
  select a single node config for this `Uplink`/node pair.

When discovery is complete, `Resolved=True` with reason `Resolved`. For the other reasons above, `Resolved=False`.
If the `Uplink` no longer selects the node, the Uplink discovery reconciler deletes the corresponding `UplinkState`
instead of retaining stale state with `Resolved=False`.

In split DPU mode the `HostDataReady` condition reports host interface data discovery on the DPU-Host:
`HostDataReady=True` with reason `HostDataDiscovered` once discovery succeeds (which guarantees a usable MAC and an
address for each enabled IP family, so the DPU side can consume the data), and `HostDataReady=False` with a host-side
discovery reason (`HostInterfaceNotFound`, `InvalidHostInterface`, `GatewayInfoUnavailable`, `NodeSelectorOverlap`)
otherwise. Full mode does not publish this condition.

In split DPU mode the `HostGatewayReady` condition reports host-side gateway programming on the DPU-Host, mirroring
`GatewayReady` on the DPU: the attachment of the selected host interface to the host-side CUDN VRF, including the
migration of its pre-existing routes into the VRF table. `HostGatewayReady=True` with reason `GatewayConfigured` once
the reported host-side programming has succeeded, and `HostGatewayReady=False` with the same reasons `GatewayReady`
uses (e.g. `UplinkVRFAttachmentFailed` when the interface cannot be attached or its routes cannot be migrated)
otherwise. Full mode does not publish this condition: the node reports everything through `GatewayReady`.

The `GatewayReady` condition reports aggregate results from CUDN gateway programming on the resolved Uplink path on
this node. The node-level status controller is the sole writer of this condition and batches result updates. Each CUDN
controller owns and serializes its own gateway reconciliation.

When a CUDN becomes active on an Uplink that is already in use, OVN-Kubernetes reuses the existing OVS bridge and gateway
interface. It adds that CUDN's bridge mapping, waits for `ovn-controller` to realize the corresponding physical patch
port, adds the per-network bridge configuration, and reconciles the required gateway OpenFlow. With no reported CUDNs,
`GatewayReady=True` with reason `NoActiveCUDNs`; cluster-manager treats that reason as not ready for an active CUDN until
its first result arrives. If other CUDNs already report success on the same Uplink and node, adding another CUDN does
not temporarily make the shared condition false. Precise in-progress readiness for each CUDN would require per-CUDN,
per-node status.

The condition uses the following reasons:

* `GatewayConfigured`: required bridge mappings, physical patch ports, per-network bridge configuration, OpenFlow, and
  VRF attachment programming succeeded for the CUDNs that reported against the current UplinkState inputs.
* `NoActiveCUDNs`: no active CUDN gateway has reported programming on this Uplink and node.
* `UplinkVRFAttachmentFailed`: OVN-Kubernetes could not attach or detach the resolved gateway interface for the effective
  routing domain required by the active CUDN set.
* `UplinkBridgeMappingFailed`: OVN-Kubernetes could not reconcile a CUDN bridge mapping or discover a corresponding
  physical patch port on the resolved OVS bridge.
* `UplinkGatewayProgrammingFailed`: OVN-Kubernetes could not reconcile the per-network bridge configuration or gateway
  OpenFlow for a reported active CUDN.
* `UplinkConfigurationConflict`: multiple CUDNs active on this node require incompatible use of the same resolved
  gateway interface, such as attaching it to distinct per-CUDN VRFs.

For failure states, `GatewayReady=False` while `Resolved` can remain `True`. Its message contains the number of affected
reported CUDNs and a bounded sample of CUDN names and underlying reasons; full reconciliation errors remain available
in node logs. Because the programmed gateway is shared state, cluster-manager maps the aggregate non-ready state of
`GatewayReady`, and of `HostGatewayReady` where present, into `UplinksReady=False` for every active CUDN using that
Uplink/node pair. When the reported configuration succeeds again, `GatewayReady=True` with reason
`GatewayConfigured`. Dynamic CUDNs using the same Uplink on disjoint nodes
have independent `GatewayReady` conditions in their node-scoped `UplinkState` objects.

VLAN configuration is not part of the `Uplink` or `UplinkState` API. When the selected host interface in Full mode, or
the resolved DPU-side representor or bridge-local gateway interface in DPU mode, has an OVS VLAN tag, ovnkube-node or
ovnkube-node-dpu detects that tag from OVS and configures the OVN gateway router external port to use the same VLAN
behavior. The user is responsible for provisioning the OVS bridge with the correct tagged host-facing interface. In DPU
deployments, the DPU-side representor is discovered locally and is not published in `UplinkState`.

MTU is also not part of the `Uplink` or `UplinkState` API. The user must provision a consistent MTU on the physical
uplink, OVS bridge, and host gateway interface. In DPU deployments, this includes the host-side gateway interface and the
DPU-side representor path that backs the resolved bridge. `Uplink` does not provide a way to raise a CUDN above
OVN-Kubernetes' effective network MTU.

### CUDN CRD extension

Add an optional `uplinks` field to CUDN spec.

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: blue
spec:
  namespaceSelector:
    matchLabels:
      tenant: blue
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.20.0.0/16
  uplinks:
  - br-blue
```

#### CUDN validation and status

* `spec.uplinks` is optional. CUDNs that do not set it behave exactly as they do today.
* `spec.uplinks[*]` is a direct reference to one `Uplink` by name.
* `spec.uplinks` has `MaxItems=1` in this OKEP.
* `spec.uplinks` is valid for primary Layer2 and Layer3 CUDNs only when OVN-Kubernetes is configured for shared gateway
  mode in this OKEP.
  * CEL rule:
    `!has(self.uplinks) || size(self.uplinks) == 0 || ((self.network.topology == 'Layer2' && has(self.network.layer2) && self.network.layer2.role == 'Primary') || (self.network.topology == 'Layer3' && has(self.network.layer3) && self.network.layer3.role == 'Primary'))`
* `spec.uplinks` is not valid when the CUDN sets `spec.network.transport: EVPN`.
  * CEL rule:
    `!has(self.uplinks) || size(self.uplinks) == 0 || !has(self.network.transport) || self.network.transport != 'EVPN'`
* `spec.uplinks` is not valid for localnet topology in this OKEP. Localnet support is a future goal.
* `spec.uplinks` is immutable after CUDN creation.
  * CEL rule on `spec.uplinks`: `self == oldSelf`
* If OVN-Kubernetes is configured for local gateway mode and a CUDN sets `spec.uplinks`, set a CUDN condition with reason
  `UplinkUnsupportedGatewayMode`.
* If a CUDN sets both `spec.uplinks` and `spec.network.transport: EVPN`, set a CUDN condition with reason
  `UplinkUnsupportedTransport`.
* If a referenced `Uplink` does not exist, set a CUDN condition with reason `UplinkNotFound`.
* If more than one `Uplink.spec.nodeConfigs` item applies to a selected node, set reason `UplinkOverlapOnNode`.
* If no `Uplink.spec.nodeConfigs` item applies to a node where the CUDN/UDN is active, set reason
  `UplinkNotFoundForNode`.
* If the matching `UplinkState` does not have `Resolved=True` for a node where the CUDN/UDN is active, set reason
  `UplinkNotResolvedForNode`.
* If no active CUDN gateway has reported programming on an Uplink and node, publish `GatewayReady=True` with reason
  `NoActiveCUDNs`; an active CUDN treats that reason as not ready until its first result arrives.
* If OVN-Kubernetes cannot attach or detach the selected host interface according to the CUDN's effective routing domain,
  set reason `UplinkVRFAttachmentFailed`.
* If OVN-Kubernetes cannot configure a required bridge mapping or discover a corresponding physical patch port on the
  resolved OVS bridge, set reason `UplinkBridgeMappingFailed`.
* If OVN-Kubernetes cannot reconcile the required per-network bridge configuration or gateway OpenFlow, set reason
  `UplinkGatewayProgrammingFailed`.
* For Dynamic UDN, if a node is selected by both the CUDN and `Uplink` but the UDN is not active on that node,
  OVN-Kubernetes delays Uplink reconciliation until the UDN becomes active. If the UDN later becomes inactive on that
  node, OVN-Kubernetes cleans up OVN-Kubernetes-owned per-CUDN artifacts for that node and leaves the admin-owned OVS
  bridge and host network configuration unchanged.

These reasons are surfaced through a dedicated CUDN condition rather than overloading `NetworkReady` semantics:

* `type: UplinksReady`
* `status: True` when the CUDN does not set `spec.uplinks`, or when all required uplinks for every active node for the
  CUDN/UDN have usable referenced Uplink state and the node-level aggregate gateway condition reports success rather
  than `NoActiveCUDNs`.
* `status: False` when one or more required uplinks are missing, unsupported, unresolved on an active node, have no
  reported active CUDN gateways, or have a reported programming failure. The condition uses the reasons listed above,
  including
  `UplinkUnsupportedGatewayMode`, `UplinkUnsupportedTransport`, `UplinkNotFound`, `UplinkOverlapOnNode`,
  `UplinkNotFoundForNode`, `UplinkNotResolvedForNode`, `NoActiveCUDNs`,
  `UplinkVRFAttachmentFailed`, `UplinkBridgeMappingFailed`, `UplinkGatewayProgrammingFailed`, and
  `UplinkConfigurationConflict`.

`UplinksReady` is aggregate readiness for the CUDN's full `spec.uplinks` selection. This OKEP limits that list to one
item. Future multi-uplink work may add keyed per-uplink status if detailed per-uplink reporting is needed.

When multiple active nodes fail uplink readiness for the same CUDN, `UplinksReady` remains a single aggregate condition.
The condition reason is selected deterministically from the failing reasons. The message contains a bounded summary, such
as the number of affected active nodes and a small sample of node names. It must not list every failed node in large
clusters. Full per-node details remain in the corresponding `UplinkState` objects, which can be queried with field
selectors on `spec.uplinkName` and `spec.nodeName`.

`UplinksReady=True` for CUDNs without `spec.uplinks` keeps the condition usable as a component of any future aggregate
CUDN readiness condition without requiring consumers to special-case the absence of an uplink.

`spec.uplinks` is modeled as a list now to preserve room for future multi-uplink routing. For example, a future CUDN
could select three Uplinks. If `RouteAdvertisements targetVRF: auto` selects that CUDN, OVN-Kubernetes would attach all
three selected host interfaces to the CUDN VRF. If the node already had kernel routes with two equal-cost default routes
through the first two uplinks and a more specific subnet route through the third uplink, traffic selection would follow
those routes. With RouteAdvertisements and BGP, `targetVRF: auto` could import and export routes only in the CUDN VRF:
two peers could advertise ECMP paths to one network while a third peer advertises a different reachable subnet. This
routing policy belongs to host routing and RouteAdvertisements/FRR-K8S configuration, not to the `Uplink` object.
`targetVRF: default` would intentionally leave those interfaces in the shared default routing table and can include routes
from unrelated default-network or CDN uplinks, so it is not the isolated multi-uplink CUDN case this future expansion is
meant to cover. A mode where OVN-Kubernetes installs routes in the absence of BGP or pre-existing node routes would
require a separate managed routing/provisioning API and is out of scope.

The CUDN status controller runs in ovnkube-cluster-manager. It watches the referenced `Uplink` objects, Nodes, and
`UplinkState` objects, then derives the CUDN's external uplink readiness from the node-local state relevant to that CUDN.
It does not treat aggregate `Uplink.status.conditions[Ready]` as a direct CUDN readiness input, and it does not rely on
ovnkube-node directly updating CUDN status. This OKEP allows only one referenced `Uplink`.

For node-local checks, the CUDN status controller filters `UplinkState` by the CUDN/UDN's relevant node set. For Dynamic
UDN, nodes where the CUDN and `Uplink` both select the node but the UDN is not active do not block CUDN readiness and do
not produce `UplinkNotFoundForNode` or `UplinkNotResolvedForNode`.

#### Bridge sharing

Multiple CUDNs can reference the same `Uplink`. Sharing is evaluated against the CUDNs that are active on each node, so
Dynamic UDN-backed CUDNs with disjoint active node sets can use the same `Uplink` even when each CUDN uses its own VRF.
Whether multiple CUDNs can use the same resolved OVS bridge concurrently on one node depends on the routing domain:

* `RouteAdvertisements targetVRF: auto`: OVN-Kubernetes resolves a distinct CUDN VRF name for each selected CUDN. Sharing
  the same effective OVS bridge between multiple CUDNs active on the same node is not supported for this per-CUDN
  VRF-Lite isolation mode. Dynamic CUDNs active on different nodes are supported because each node attaches its local
  gateway interface to only one CUDN VRF.
* `RouteAdvertisements targetVRF: default`: selected CUDNs intentionally share the default routing domain. In this mode,
  multiple CUDNs may share the same effective OVS bridge. This is not VRF isolation, and overlapping CUDN subnets remain
  user misconfiguration.
* No RouteAdvertisements/BGP: selected CUDNs use normal shared gateway egress behavior on the resolved bridge. Egress
  routing follows the default gateways discovered for that node's selected `Uplink` and published in
  `UplinkState.status.defaultGateways`; pod traffic is SNAT'ed to the selected host interface or gateway IP when
  applicable. For bridge-sharing conflict detection, this is treated like shared/default routing-domain use, so multiple
  CUDNs may share the same effective OVS bridge. This is not VRF-Lite route isolation.
* Explicit non-default `targetVRF`: assigning multiple CUDNs to the same custom non-default VRF is not supported by this
  OKEP.

Unsupported same-node sharing is treated as a symmetric invalid configuration. The node's `UplinkState` reports
`GatewayReady=False` with reason `UplinkConfigurationConflict`, and all active CUDNs in the unsupported conflict set
report `UplinksReady=False` with the same reason. Aggregate `Uplink.status.conditions[Ready]` can remain `True` because
Uplink discovery succeeded. OVN-Kubernetes does not use first-writer-wins ordering or preserve a previous CUDN assignment
as the supported winner. If a new CUDN creates an unsupported conflict with an already-programmed CUDN, OVN-Kubernetes
does not program new conflicting uplink artifacts while the conflict exists. It also does not proactively tear down
previously programmed dataplane for the existing CUDN solely because another CUDN introduced a conflict. Existing traffic
may continue until normal reconciliation or deletion removes the previously programmed artifacts, but the conflict is
reported as unsupported and users should not rely on that dataplane behavior. Admin-owned OVS bridge and host network
configuration is left unchanged.

### RouteAdvertisements interaction

No new RouteAdvertisements API is required.

`Uplink` selection is not limited to CUDNs using BGP or `RouteAdvertisements`. A CUDN may select a `Uplink` without
configuring BGP. In shared gateway mode, egress traffic uses the resolved OVS bridge and follows normal shared gateway
behavior, including SNAT to the selected host interface or gateway IP when applicable. This OKEP does not add a general
non-BGP ingress-to-pod routing mechanism.

OVN-Kubernetes derives whether an Uplink-backed CUDN needs VRF attachment from the `RouteAdvertisements` that select that
CUDN and node:

* `targetVRF: auto`: the CUDN resolves to its per-network VRF. OVN-Kubernetes attaches the selected host interface to that
  VRF in Full/DPU-Host mode. In DPU mode, OVN-Kubernetes creates the DPU-local route-import VRF and attaches the resolved
  bridge `LOCAL` interface to it.
* `targetVRF: default`: the CUDN uses the default routing domain. OVN-Kubernetes leaves the selected host interface and
  DPU bridge `LOCAL` interface in the default VRF.
* No matching `RouteAdvertisements`: the CUDN uses the default routing domain for Uplink purposes. OVN-Kubernetes leaves
  the selected host interface and DPU bridge `LOCAL` interface in the default VRF.

This decision is level driven. Changes to `RouteAdvertisements`, selected FRR configuration, CUDN labels, node labels, or
network activation can move an Uplink-backed CUDN between the default routing domain and per-CUDN VRF-Lite isolation.
OVN-Kubernetes manages the selected Uplink interface's VRF membership and preserves its static and DHCP routes across
VRF attachment. Bridge, IP, MTU, and VLAN configuration remain the administrator's responsibility.

For VRF-Lite, existing default routes through the selected host interface are preserved in the CUDN VRF. The OVN
gateway router uses the discovered default gateways alongside imported BGP routes for pod egress.

Expected usage with VRF-Lite + shared gateway:

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: blue-ra
spec:
  targetVRF: auto
  advertisements:
  - PodNetwork
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      routeAdvertisements: vpn-blue
  networkSelectors:
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          k8s.ovn.org/metadata.name: blue
```

In DPU deployments, `targetVRF: auto` still resolves to the CUDN VRF name. Since the DPU does not naturally have the
host-side CUDN VRF, ovnkube on the DPU creates a DPU-local route-import VRF with that name only for this per-CUDN
VRF-Lite case. FRR peers in that VRF and installs learned routes into the VRF routing table; OVN-Kubernetes route import
then reflects those routes into the CUDN gateway router. This DPU-local VRF is separate from the DPU-Host VRF and
nftables state. For `targetVRF: default` or CUDNs without matching `RouteAdvertisements`, the DPU bridge `LOCAL` interface
remains in the default VRF and no DPU-local CUDN route-import VRF is created for that CUDN.

In DPU deployments, FRR peering and route import happen on the DPU. Host traffic to off-subnet destinations needs a
suitable route in the host CUDN VRF, such as a preserved static or default route. A host BGP speaker can also learn
routes from FRR on the DPU; configuring it is outside this proposal. Pod traffic follows the discovered default
gateways and imported BGP routes in the OVN gateway router on the DPU.

The DPU-local FRR peering IP is not stored in `UplinkState`. For `targetVRF: auto`, it is expected to be configured on the
resolved OVS bridge's `LOCAL` interface as local DPU state. OVNKube on the DPU enslaves that `LOCAL` interface to the
DPU-local route-import VRF and uses that interface for FRR peering. The advertised next-hop for pod routes remains the
host shared gateway IP from `UplinkState.status.ipAddresses`.

For DPU deployments, generated FRR configuration for the host node is applied to the corresponding DPU FRR instance. The
RouteAdvertisements controller uses `UplinkState.status.ipAddresses` as the source for the host shared gateway next-hop
when generating next-hop override configuration. If the matching `UplinkState` does not yet have host shared gateway IPs,
RouteAdvertisements reconciliation waits and reports pending/not-ready status rather than generating incomplete FRR
configuration.

For Uplink-backed networks using route import, OVN-Kubernetes also scopes imported BGP routes to the selected uplink
bridge. RouteImport resolves the Linux link index from the matching `UplinkState.status.ovsBridge.name` and imports only
BGP routes whose kernel route output link matches that bridge. This prevents routes learned or leaked through the default
bridge or another uplink from being installed into a CUDN gateway router that is attached only to the selected uplink
bridge. If the matching `UplinkState` is missing, does not have `Resolved=True`, or has no resolved bridge, RouteImport
treats the route set as pending for that network rather than importing routes from another interface.

## Implementation Details

### Controller changes

#### Uplink controller (cluster manager)

* Watches `Uplink`, `UplinkState`, Nodes, and relevant CUDNs.
* Relies on CRD schema and CEL for static `Uplink` API-shape validation.
* Resolves `Uplink.spec.nodeConfigs` against live Nodes and reports controller-detected configuration problems, including
  multiple node configs matching the same node and CUDN active nodes that have no matching node config.
* Aggregates `UplinkState.status.conditions[Resolved]` into `Uplink.status.conditions[Ready]`. For node-scoped failures,
  the condition message includes a bounded count and sample of affected nodes and underlying reasons such as
  `HostInterfaceNotFound`, `BridgeNotFound`, `BridgeUplinkNotFound`, `BridgeInvalid`,
  `DefaultGatewayBridgeUnsupported`, `InvalidHostInterface`, `GatewayInfoUnavailable`, `WaitingForDPUHost`, and
  `NodeSelectorOverlap`.
* Derives CUDN reason `UplinkOverlapOnNode` from affected `UplinkState` objects with reason `NodeSelectorOverlap`.
* Resolves nodes selected by a referencing active CUDN but not selected by any `Uplink.spec.nodeConfigs` entry into CUDN
  reason `UplinkNotFoundForNode`.
* Maintains a finalizer on `Uplink` objects referenced by one or more CUDNs. The finalizer is removed only after no CUDNs
  reference the `Uplink`.
* Aggregates per-node state without storing per-node details in `Uplink.status`.
* Reflects uplink readiness into CUDN status using both `Resolved` and gateway-programming `GatewayReady` from
  active-node-scoped `UplinkState` objects, not from aggregate `Uplink.status.conditions[Ready]`.

#### Node Uplink discovery reconciler (OVNKube-Node Full mode)

The Uplink discovery reconciler runs for each `Uplink` that selects the local node, independently of whether a CUDN
currently references or is active on that node. It owns the local `UplinkState` lifecycle and its `Resolved`
condition:

1. Resolve the node config that applies to this node. If more than one `Uplink.spec.nodeConfigs` entry applies, create or
   update this node's `UplinkState` with `Resolved=False` and reason `NodeSelectorOverlap`.
2. Validate that `nodeConfigs[*].hostInterfaceName` exists and carries the expected host-side gateway L3 identity.
3. Resolve the OVS bridge that contains the host interface and validate the bridge layout.
4. Discover the host interface MAC address, IP addresses, default gateways, and any OVS VLAN tag on that interface from
   existing host and OVS state.
5. Create or update this node's `UplinkState`, including the canonical identity in `spec.uplinkName` and
   `spec.nodeName`, and `hostInterfaceName`, `ovsBridge.name`, gateway data, and `Resolved` condition in status.

If no node config applies after an `Uplink` or Node label change, the discovery reconciler deletes the local
`UplinkState` directly. It does not retain the object to report that the node is no longer selected.

The discovery reconciler should requeue affected Uplinks when local host or OVS state changes. Netlink subscriptions
cover interface, address, route, and link-state changes; OVSDB or equivalent OVS change notifications cover bridge, port,
VLAN tag, and bridge-layout changes. Normal retry handling remains required for transient failures, with a periodic
resync as a fallback so admin-side network changes are eventually reflected even if an event is missed.

#### Node Uplink gateway reconciliation (OVNKube-Node Full mode)

Each active CUDN gateway controller watches its referenced local `UplinkState` and owns its dataplane reconciliation.
The controller serializes UplinkState events, RouteAdvertisements updates, initial setup, and cleanup for that CUDN.
Shared components such as bridge and OpenFlow configuration provide their own cross-CUDN synchronization.

For each reconciliation, the CUDN gateway controller:

1. Reads one `UplinkState` snapshot, requires `Resolved=True`, and resolves its host interface, bridge, MAC, addresses,
   gateways, and host function.
2. Compares those consumed inputs with the successfully programmed fingerprint. An unchanged fingerprint preserves the
   dataplane; a changed or invalid configuration withdraws the old programming before replacement or retry.
3. Attaches the gateway interface to the CUDN VRF when required, or leaves it in the default VRF for shared/default
   routing-domain use. OVN-Kubernetes does not create/delete the admin bridge, remove its admin ports, move its IP
   addresses, or alter admin-owned VLAN/MTU configuration.
4. Adds the CUDN bridge mapping, waits for its physical patch port, adds its per-network bridge configuration, and
   reconciles gateway OpenFlow and route-advertisement-dependent state.
5. Reports one success or failure result with the UplinkState UID observed before the operation and the input
   fingerprint on which the operation was based.

The UplinkState gateway status controller is the sole writer of `GatewayReady` (or `HostGatewayReady` on the DPU-Host).
It accepts a result only if the current informer object has the reported UID and the same discovery-owned inputs, then
batches accepted per-CUDN results into the shared condition. This prevents an operation for a deleted, recreated, or
superseded UplinkState from publishing a late result. It does not publish an intermediate Pending condition.

The status controller does not own `Resolved` or serialize dataplane operations. UplinkState changes requeue each
affected CUDN directly; RouteAdvertisements changes requeue that CUDN's gateway reconciliation. Those changes can
require attaching or detaching the selected Uplink interface, adding or removing the normal default route from the UDN
VRF, updating service route steering, and refreshing route-import filtering.

#### DPU reconciliation split

DPU deployments run ovnkube on both the DPU-Host and the DPU. OVS runs on the DPU on behalf of the host, so the Uplink
reconciler is split across the two components.

DPU-Host reconciler:

1. Watch `Uplink` and the matching `UplinkState` for the host node.
2. Resolve the referenced `Uplink` and the node config that applies to the host node.
3. Find `nodeConfigs[*].hostInterfaceName` on the DPU-Host and discover its MAC address, IP addresses, and default
   gateways.
4. Determine the CUDN's effective routing domain from matching `RouteAdvertisements`. Attach the selected host interface
   to the host-side CUDN VRF only for per-CUDN VRF-Lite isolation; otherwise leave it in the default VRF.
5. Report the aggregate host-side gateway programming, including the VRF attachment and the migration of the interface's
   pre-existing routes, through the `HostGatewayReady` condition.
6. Update this node's `UplinkState.status` with `hostInterfaceName`, `macAddress`, `ipAddresses`, `defaultGateways`, and
   the `HostDataReady` condition. The DPU-Host does not write the `Resolved` condition; the DPU side owns it.

DPU-side reconciler:

1. Watch the matching `UplinkState` for the host node it represents.
2. Wait for the DPU-Host side to publish `status.macAddress`.
3. Detect the DPU-side host gateway/PF representor by matching the host interface MAC to the existing representor peer
   relationship. The bridge is pre-provisioned, so the representor must already be attached to an OVS bridge on the DPU.
4. Resolve the OVS bridge that contains the representor, validate the bridge layout, and publish `status.ovsBridge.name`
   plus the `Resolved` condition in `UplinkState`. While host data is missing or incomplete, publish `Resolved=False`
   with reason `WaitingForDPUHost`.
5. Create or reconcile the DPU-local route-import VRF for the CUDN only when the CUDN resolves to per-CUDN VRF-Lite
   isolation. This VRF uses the same name that `RouteAdvertisements targetVRF: auto` resolves to for the CUDN.
6. For per-CUDN VRF-Lite isolation, use the resolved bridge's `LOCAL` interface as the DPU-local FRR peering interface
   and enslave that `LOCAL` interface to the DPU-local route-import VRF. Any DPU-local FRR peering IP is expected to be
   configured on that interface and is not published in `UplinkState`. For default routing-domain use, leave the `LOCAL`
   interface in the default VRF.
7. Detect any OVS VLAN tag on the DPU-side host gateway representor or bridge-local gateway interface and configure the
   OVN gateway router external port to use the same VLAN behavior.
8. Reconcile the OVS bridge mapping, DPU-local route-import VRF state when applicable, gateway interface data, and
   OpenFlow on the DPU side using host shared gateway data from `UplinkState.status`.

Although both DPU-Host and DPU-side reconcilers update the same `UplinkState`, updates are dependency ordered and low
frequency. The DPU side waits for the DPU-Host to publish host-side gateway data, especially `status.macAddress`, before
resolving and publishing DPU-local bridge data such as `status.ovsBridge.name`. Implementations should use status patches
or server-side apply field ownership for the fields each side owns, and each status condition has a single writer
(`HostDataReady` and `HostGatewayReady` on the DPU-Host, `Resolved` and `GatewayReady` on the DPU), so normal retry-on-conflict handling is
sufficient and the object cannot become a hot write target.

This avoids publishing PF IDs, host PCI addresses, or DPU-local bridge names in the `Uplink` spec. The DPU-Host publishes
the host interface MAC as the handoff point, and the DPU side uses the existing representor peer relationship to discover
the DPU-local representor and OVS bridge.

For gateway interfaces backed by VF, SF, or PF representors, OVN-Kubernetes continues to use the dynamic representor
derivation mechanisms used today rather than adding representor names to the `Uplink` API. Full mode derives the
representor from device details, and DPU mode uses existing switchdev/DPU representor discovery.

#### CUDN/UDN controller integration (OVNKube-Controller)

* When configuring the CUDN topology in OVN with shared gateway mode, the UDN controller reads the matching `UplinkState`
  and derives the gateway router configuration from that typed status.
* If existing OVN-Kubernetes internals require an `L3GatewayConfig`-like structure, the implementation should build it in
  memory from `UplinkState`, `Uplink`, node, and CUDN context rather than serializing CUDN gateway data through a node
  annotation.
* The CUDN remains a primary UDN. The external connectivity path uses OVN localnet plumbing for the CUDN's external
  logical switch port. OVN-Kubernetes configures that localnet port to use the normalized CUDN network name, and
  ovnkube-node configures the matching per-node OVS bridge mapping:

  ```text
  <normalized-cudn-network-name>:<resolved-ovs-bridge>
  ```

  This mapping is the join point between OVN's logical localnet port and the `Uplink` selected on that node. In DPU
  deployments, the same logical localnet network name is used, but the bridge mapping is reconciled by the DPU-side
  component because OVS runs on the DPU.

### Shared gateway datapath updates

The current openflow manager models one default bridge plus one optional secondary egress bridge. Each bridge is
represented by a `BridgeConfiguration`, and `BridgeConfiguration` already carries per-network state in its
`netConfig map[string]*BridgeUDNConfiguration`. This enhancement reuses that model but makes the non-default bridge set
dynamic and explicitly records which CUDN network is assigned to which resolved `Uplink` on the node.

Required updates:

* Keep the default bridge as the existing fixed `BridgeConfiguration`.
* Replace the single optional `externalGatewayBridge` field with a map of resolved uplink OVS bridges to
  `BridgeConfiguration` objects.
* Replace the single secondary-bridge flow cache with per-uplink-bridge flow caches keyed by the resolved bridge.
* Add a node-local assignment cache that maps each CUDN network name to the selected resolved `Uplink` and OVS bridge.
  The assignment cache carries the CUDN/Uplink relationship used for conflict detection, cleanup, bridge mappings, and
  status; `BridgeConfiguration` remains the per-OVS-bridge object.
* When a CUDN selects a `Uplink` on the node, create or reuse that bridge's `BridgeConfiguration` and add that CUDN's
  `BridgeUDNConfiguration` only to the resolved bridge.
* After the active CUDN assignment set changes, use the bridge's aggregate readiness check across every assigned
  `BridgeUDNConfiguration`. Keep `GatewayReady=False` until all corresponding patch ports are present and flow
  reconciliation for the complete set succeeds.
* A `BridgeConfiguration` may contain multiple concurrently active CUDN network configs only for supported
  shared-routing-domain cases such as `targetVRF: default`. For per-CUDN VRF-Lite isolation or explicit non-default VRF
  targets, reject another CUDN active on the same node from assigning the same resolved bridge with
  `UplinkConfigurationConflict`. Dynamic CUDNs using the bridge on disjoint nodes do not coexist in one node's
  `BridgeConfiguration` and are supported.
* Generate and apply ingress/egress flows for each active uplink bridge using the CUDN network configurations assigned to
  that bridge.
* Preserve existing default bridge flow behavior for clusters not using `Uplink`.

### Gateway configuration source

The existing `k8s.ovn.org/l3-gateway-config` node annotation remains the source of truth for the default network gateway
and existing legacy consumers. This enhancement does not add CUDN or non-default uplink entries to that annotation.

For CUDNs using `Uplink`, OVN-Kubernetes uses `UplinkState` as the typed gateway configuration source. The status object
carries the host interface, resolved OVS bridge, MAC, IP addresses, and default gateways needed to configure the CUDN
gateway router. OVN interface identifiers are derived from `UplinkState`, node, and CUDN context. This avoids
duplicating the same data in both a CRD status object and a node annotation, and avoids stale dual-source-of-truth
behavior.

In DPU deployments, `UplinkState.status.ipAddresses` is also the source for the host shared gateway IP used as the
advertised BGP next-hop. The DPU-local FRR peering IP is expected on the resolved bridge's `LOCAL` interface and is not
included in `UplinkState`.

Compatibility requirement:

* Existing `default` key behavior in `k8s.ovn.org/l3-gateway-config` remains unchanged.
* Existing single egress-gateway fields remain supported.
* New CUDN uplink gateway data is not serialized into node annotations.
* Internal code that still expects an `L3GatewayConfig` shape must derive that structure from `UplinkState`, `Uplink`,
  node, and CUDN context for this feature.

## Feature Compatibility

### Local gateway mode

`Uplink` is not supported when OVN-Kubernetes is configured for local gateway mode in this OKEP. If a CUDN sets
`spec.uplinks` while the cluster uses local gateway mode, OVN-Kubernetes reports `UplinkUnsupportedGatewayMode` and does
not generate Uplink-backed gateway configuration for that CUDN.

Local gateway VRF-Lite continues to work as documented by the BGP OKEP for CUDNs that do not set `spec.uplinks`; the user
remains responsible for attaching the relevant IP interface to the matching CUDN VRF. Extending `Uplink` to automate that
local gateway interface selection and VRF attachment is a future goal.

### EVPN

`Uplink` is not supported for CUDNs using EVPN transport in this OKEP. EVPN is a distinct external connectivity model and
is currently scoped to local gateway mode, while this OKEP defines Uplink behavior for shared gateway mode. If a CUDN sets
both `spec.uplinks` and `spec.network.transport: EVPN`, OVN-Kubernetes reports `UplinkUnsupportedTransport` and does not
generate Uplink-backed gateway configuration for that CUDN.

Shared gateway EVPN support and any future mapping between `Uplink`, EVPN VTEPs, and EVPN underlay selection should be
covered by a separate proposal.

### Egress IP

Egress IP behavior for CUDNs should continue to follow existing shared gateway behavior. Supporting additional consumers
such as secondary EgressIP through `Uplink` is a future goal and is not part of this OKEP's initial scope.

### Egress Service

Egress Service is not currently supported with CUDN. Future behavior can use the same Uplink bridge selection model once
CUDN support exists.

### MEG

MEG remains supported only for CDN and remains outside CUDN scope.

### Service access

#### Ingress traffic into the selected uplink

Service flows must be configured on the OVS bridge selected by the CUDN on that node. The flow programming is per-CUDN
and per-bridge: a CUDN bound to one uplink bridge gets its service ingress and return-path flows on that bridge, while a
different CUDN bound to a different uplink bridge gets its own flows on its resolved bridge. Traffic entering the
resolved bridge uplink will function like shared gateway does today for services, with traffic forwarded to the patch
port on the gateway router of the respective CUDN. When service traffic enters the selected OVS bridge from that
bridge's uplink port and the service belongs to a CUDN mapped to that bridge, bridge OpenFlow steers the packet directly
to the CUDN gateway router patch port.

Traffic that enters the selected uplink destined for a non-CUDN service like the Kubernetes API is sent to the local host
path and then handled through existing UDN service behavior.

#### Ingress traffic from other host interfaces

For CUDNs that use the default routing domain (`targetVRF: default` or no matching `RouteAdvertisements`), service traffic
may enter the node through another default-VRF host interface, including the normal shared gateway bridge. NodePort,
ExternalIP, or LoadBalancer traffic is DNAT'ed to the service cluster IP as it is today. The existing service packet mark
and IP rule path selects the UDN VRF for the forward lookup, and that UDN VRF service CIDR route must use the selected
uplink gateway interface. That route gets the post-DNAT packet onto the selected OVS bridge. OpenFlow on that bridge then
uses the preserved service mark to dispatch the packet to the CUDN's patch port and gateway router. The selected uplink
bridge must therefore have the same per-UDN service handling, masquerade, and return-path flows that the default shared
gateway bridge has for UDN services.

In the default routing-domain case, reply traffic does not need to be marked back into the CUDN VRF for routing. The
forward path uses the UDN VRF lookup to reach the selected bridge, but after the reply returns from OVN through that
bridge and the service/masquerade conntrack state restores the host-side tuple, the final host routing lookup can use the
default routing domain where the relevant routes are intentionally shared.

For CUDNs using per-CUDN VRF-Lite isolation (`targetVRF: auto`), service ingress is intentionally scoped to interfaces and
routes in that CUDN VRF. Traffic that enters a different VRF is not implicitly steered into the CUDN VRF by `spec.uplinks`.
If the ingress interface is part of the same CUDN VRF, service DNAT and the VRF-local service route can steer traffic to
the selected uplink bridge, and the reply remains in that CUDN VRF because the selected uplink interface is enslaved to
it.

Because service reply traffic can re-enter the kernel through the selected uplink bridge while reverse-path lookup points
at another route, OVN-Kubernetes must configure reverse-path filtering for the involved bridge/interface path consistently
with existing shared gateway service handling.

If a future requirement needs stricter service isolation based on ingress interface or uplink, that should be handled by a
separate policy design rather than being implicit in `spec.uplinks`.

#### Pod service access from a CUDN using Uplink

Pods should still be able to access services on their CUDN internally. The selected uplink bridge must install the same
per-UDN masquerade and service fallback flows used by shared gateway mode today, using the CUDN's own masquerade IPs and
packet marks to keep networks identified.

Pods trying to access exposed Kubernetes services like the Kubernetes API service should still work through the existing
UDN-enabled default service path. This requires the selected uplink bridge to have the relevant per-UDN service flows and
the static FDB/MAC programming needed to send host-bound masqueraded traffic to the correct LOCAL or DPU host-representor
path.

## Failure and Cleanup Behavior

### Uplink

* If the selected host interface is missing, or the resolved OVS bridge is missing or malformed on a node, the matching
  `UplinkState` reports `Resolved=False`. Retries continue while unresolved: discovery reports the failure in the
  `UplinkState` and returns an error, and its controller retries with rate-limited backoff and unbounded attempts,
  re-polling admin-owned host and OVS state, which generates no Kubernetes watch events. Once a side reports success,
  its published data is refreshed on the next Kubernetes-triggered reconcile, with one exception: while the discovered
  `status.defaultGateways` is missing a default gateway for one of the host interface's address families, the
  netlink-discovering side publishes success and schedules its own delayed rediscovery, so a default route added later
  on the host is picked up without a Kubernetes event.
* Per-node failures are reported on the matching `UplinkState`; `Uplink.status.conditions` reports aggregate health only.
* If an admin deletes or changes the OVS bridge, OVN-Kubernetes does not recreate or repair the bridge. While discovery
  is unresolved, it updates status and retries validation. A converged `Resolved=True` status is not re-validated until
  the next Kubernetes event or ovnkube-node restart triggers a reconcile, except while `status.defaultGateways` is
  missing a default gateway for one of the host interface's address families, when discovery keeps re-polling the host
  routes on a fixed interval.
* If an admin requests deletion of a `Uplink` while one or more CUDNs reference it, OVN-Kubernetes keeps the `Uplink`
  finalizer and deletion remains pending. This prevents accidental disruption of active CUDNs. Since `spec.uplinks` is
  immutable in this OKEP, clearing the reference requires deleting or recreating the CUDN without that `Uplink`. Once no
  CUDNs reference the `Uplink`, OVN-Kubernetes removes the finalizer and deletion can complete.
* In Full mode, cleanup removes OVN-Kubernetes-owned CUDN VRF membership from the selected host interface when such
  membership was created, and removes OVN-Kubernetes-owned bridge mappings and OpenFlow for the CUDN. OVN-Kubernetes does
  not delete the OVS bridge, remove bridge ports, or restore admin-owned IP/VLAN/MTU/route state.
* In DPU-Host mode, cleanup removes OVN-Kubernetes-owned CUDN VRF membership from the selected host interface for the
  uplink when such membership was created. DPU-side cleanup is handled separately by the DPU-side reconciler.
* In DPU mode, cleanup removes OVN-Kubernetes-owned state only: generated FRR configuration for the per-CUDN VRF when
  applicable, RouteImport-owned static routes from the CUDN gateway router, the resolved bridge `LOCAL` interface's
  membership in the DPU-local route-import VRF when such membership was created, the DPU-local route-import VRF itself
  when no longer needed, CUDN bridge mappings, and CUDN OpenFlow. OVN-Kubernetes does not delete the OVS bridge, remove
  bridge ports, or remove admin-owned DPU-local peering IP/VLAN state.

### CUDN Uplinks

* If an uplink for a node does not exist, the CUDN reports error status for that node, but OVN-Kubernetes does not halt
  configuration for all other nodes.
* If a CUDN references a non-existent `Uplink`, CUDN reconciliation reports `UplinkNotFound` until the `Uplink` is
  created or the CUDN is recreated with a valid reference.
* If a `Uplink` becomes valid later for a node, OVNKube-Controller and OVNKube-Node UDN reconciliation should succeed.

## Testing Details

* Unit tests:
  * `Uplink` API validation for required node configs, supported types, host interface names, and selector shape.
  * selector resolution and per-node overlap handling.
  * `Uplink` finalizer behavior when CUDNs reference or stop referencing an `Uplink`.
  * `UplinkState` status ownership and aggregation into `Uplink.status.conditions`.
  * independent `UplinkState` resolution (`Resolved`) and gateway-programming (`GatewayReady`) condition ownership and
    CUDN status propagation.
  * `GatewayReady=True/NoActiveCUDNs` keeping the first active CUDN not ready until its gateway result is reported.
  * current UID and UplinkState input checks rejecting late results from deleted, recreated, or superseded objects.
  * aggregate gateway failures for mapping, patch-port, per-network bridge configuration, OpenFlow, and VRF operations.
  * CUDN validation for `spec.uplinks`.
  * CUDN external gateway data is derived from `UplinkState` without mutating the
    `k8s.ovn.org/l3-gateway-config` node annotation.
  * rejection/not-ready status when multiple active CUDNs select the same effective OVS bridge on a node for unsupported
    distinct non-default VRF use.
  * rejection of the node's default shared gateway bridge with `DefaultGatewayBridgeUnsupported`.
  * local gateway mode rejecting `spec.uplinks` with `UplinkUnsupportedGatewayMode`.
  * EVPN transport rejecting `spec.uplinks` with `UplinkUnsupportedTransport`.
  * localnet topology rejecting `spec.uplinks` in this OKEP.
* Node integration tests:
  * host interface validation and pre-existing OVS bridge resolution.
  * missing or ambiguous bridge layout status.
  * host interface MAC address, IP address, default gateway, resolved OVS bridge, and OVS VLAN tag discovery from existing
    node state.
  * full-mode RouteAdvertisements-driven VRF attachment, detachment, and cleanup for the selected host interface.
  * RouteAdvertisements-driven UDN VRF default-route add/remove behavior for Uplink-backed networks.
  * bridge mapping creation and cleanup for the normalized CUDN network name, including waiting for the corresponding
    physical patch port before reporting aggregate gateway readiness.
  * DPU-Host publishing of selected host interface MAC, IPs, and default gateways.
  * DPU-side host gateway representor detection from the DPU-Host-published MAC.
  * DPU-side publishing of resolved OVS bridge name into `UplinkState`.
  * DPU-Host RouteAdvertisements-driven VRF attachment, detachment, and cleanup for the selected host interface.
  * DPU-side route-import VRF creation using the CUDN VRF name for `targetVRF: auto`.
  * DPU-side route-import VRF enslaving and detachment of the resolved bridge's `LOCAL` interface for `targetVRF: auto`.
  * DPU-side cleanup for route-import VRF state.
  * DPU-side OVS VLAN tag discovery from existing bridge gateway ports.
  * RouteImport filtering of BGP routes by the selected uplink bridge link index.
* E2E tests:
  * shared gateway VRF-Lite with two CUDNs on two distinct pre-created uplink bridges.
  * heterogeneous nodes using different `Uplink.spec.nodeConfigs` for the same `Uplink`.
  * service access from a CUDN on a Uplink, including verification that per-UDN service flows are programmed on the
    resolved bridge and that CUDN services plus UDN-enabled default services such as the Kubernetes API are reachable.
  * default-routing-domain service ingress where NodePort or LoadBalancer traffic enters through the normal shared gateway
    interface and reaches a service backed by an Uplink-backed CUDN through the selected uplink bridge.
  * DPU shared gateway VRF-Lite with FRR peering in the DPU-local CUDN VRF and advertised next-hop set to the host shared
    gateway IP.
  * route advertisement + ingress/egress datapath verification.
  * Dynamic CUDNs using one `targetVRF: auto` Uplink on disjoint active node sets, followed by same-node activation that
    reports `UplinkConfigurationConflict` without moving the already attached gateway interface.
* Cross-feature coverage:
  * MEG coexistence with legacy `EgressGWInterface`.
  * default bridge behavior unchanged when no CUDN uses `spec.uplinks`.
  * local gateway mode unchanged behavior for CUDNs without `spec.uplinks`.

## Documentation Details

* Add a user-facing guide for preparing OVS bridges and referencing the host-side gateway interface with `Uplink`.
* Document that bridge provisioning is external to OVN-Kubernetes; NMState is one valid way to prepare bridges.
* Update BGP/RouteAdvertisements docs to include VRF-Lite shared gateway workflows.
* Add troubleshooting docs for `Uplink` and `UplinkState` conditions.

## Risks, Known Limitations and Mitigations

* Pre-provisioned bridge dependency.
  * Mitigation: explicitly document the boundary and include examples using existing node configuration tooling such as
    NMState.
* Incorrect or ambiguous bridge layout.
  * Mitigation: deterministic validation, node-local `UplinkState` conditions, and E2E coverage for malformed bridge
    layouts.
* Incorrect default route behavior if the selected host interface has no usable default route.
  * Mitigation: publish discovered `defaultGateways`; allow operation with BGP-learned/imported routes when no default
    route is present.
* Increased complexity from dynamic multi-bridge flow programming.
  * Mitigation: incremental implementation and focused scale/perf testing.
* Service access breaks if per-UDN service flows, service CIDR routing, reverse-path filtering, or static FDB/MAC entries
  are only programmed for the default bridge.
  * Mitigation: program UDN service, masquerade, route-steering, reverse-path filtering, and return-path flows on each
    CUDN's selected uplink bridge and add E2E coverage for CUDN service access, cross-interface default-VRF service
    ingress, and UDN-enabled default services.
* Dynamic UDN first-pod latency can increase when the first pod activates a CUDN that selects a `Uplink`.
  * Mitigation: keep bridge validation and bridge mapping reconciliation incremental and idempotent. Since bridge
    lifecycle is external, OVN-Kubernetes does not perform bridge creation or IP migration on first pod startup.
* Legacy field/API overlap (`EgressGWInterface` vs `Uplink`).
  * Mitigation: additive rollout and clear precedence documentation.
* Hot status object contention in large clusters.
  * Mitigation: store per-node state in one `UplinkState` object per uplink/node pair and keep `Uplink.status` limited to
    aggregate conditions.
* DPU representor or peer MAC discovery fails.
  * Mitigation: report a clear per-node condition. The DPU-Host interface must have a discoverable peer representor that
    is attached to a pre-provisioned OVS bridge on the DPU.
* DPU route import cannot map BGP routes to the CUDN for `targetVRF: auto`.
  * Mitigation: create a DPU-local route-import VRF using the CUDN VRF name and have RouteAdvertisements target that VRF
    for shared gateway VRF-Lite mode. For default routing-domain use, do not create a CUDN route-import VRF on the DPU.
* Multiple CUDNs active on the same node select the same uplink bridge for unsupported routing-domain combinations.
  * Mitigation: allow sharing only for supported shared-routing-domain cases such as `targetVRF: default`; reject or
    report not-ready mappings that would place distinct non-default CUDN VRFs on the same effective bridge. Dynamic CUDNs
    with disjoint active node sets remain supported.

Known limitations:

* A CUDN can reference only one `Uplink` in this OKEP.
* One effective OVS bridge per node per CUDN.
* Only `OVSBridge` uplink node configs are supported.
* One `Uplink` can resolve to only one host-side gateway interface and one backing OVS bridge per node.
* The selected host interface must exist on selected nodes and resolve to a pre-created OVS bridge that is correctly
  connected to the external network.
* The node's default shared gateway bridge cannot also be selected as an `Uplink` in this OKEP.
* Local gateway mode cannot use `spec.uplinks` in this OKEP.
* EVPN transport cannot use `spec.uplinks` in this OKEP.
* Localnet topology cannot use `spec.uplinks` in this OKEP.
* Concurrent same-node Uplink sharing for distinct per-CUDN VRFs is not supported. Dynamic CUDNs with disjoint active node
  sets may share an `Uplink`; multiple CUDNs active on the same node may share an effective OVS bridge only when using a
  supported shared routing domain such as the default VRF. Overlapping subnets remain user misconfiguration.

## OVN-Kubernetes Version Skew

Planned introduction: 1.4.

## Backwards Compatibility

* Clusters not using `Uplink` have no behavior change.
* Existing default bridge and `EgressGWInterface` behavior remains supported.
* New CUDN fields are additive and optional.
* The `k8s.ovn.org/l3-gateway-config` annotation is not extended for this feature.

## Alternatives

* Keep all uplink handling outside OVN-Kubernetes.
  * Rejected as the only model because CUDN, RouteAdvertisements, DPU route-import VRFs, bridge mappings, and gateway
    router configuration need a typed contract. Manually prepared networking with tools such as NMState remains the
    expected way to create the bridge itself.
* Use only node annotations for gateway data.
  * Rejected: annotations are an untyped, shared surface and would extend the existing `l3-gateway-config` annotation
    into a multi-network source of truth.
* Put Uplink selection and lifecycle into RouteAdvertisements only.
  * Rejected: the selected external path belongs to network connectivity intent on the CUDN. RouteAdvertisements decides
    whether that CUDN uses the default routing domain or per-CUDN VRF-Lite isolation for BGP route exchange.
* Keep the API named `ExternalBridge`.
  * Rejected: the bridge is an implementation detail of the first uplink type. `Uplink` keeps the API extensible for
    future non-bridge types.

## References

* [OKEP-5296: OVN-Kubernetes BGP Integration](./okep-5296-bgp.md)
* [OKEP-5088: EVPN Support](./okep-5088-evpn.md)
* [OKEP-5259: No Overlay Support](./okep-5259-no-overlay.md)
