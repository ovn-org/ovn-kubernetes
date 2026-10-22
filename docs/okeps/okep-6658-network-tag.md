# OKEP-6658: NetworkTag - Shared Workload Identity with EVPN

* Issue: [#6658](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6658)

## Problem Statement

Kubernetes workloads on EVPN-enabled clusters carry rich label metadata, but that
metadata never leaves the OVN/OVS datapath. Physical network devices (ToR switches,
firewalls) that receive EVPN RT-2 MAC/IP Advertisement routes have no way to identify
which workload a MAC/IP binding belongs to, making label-based ACL enforcement at the
fabric layer impossible without Kubernetes-aware hardware.

## Goals

- Introduce a `NetworkTag` cluster-scoped CRD that maps pod label selectors to BGP
  extended community values, scoped to a specific UDN.
- Advertise matched extended communities on EVPN RT-2 routes for pods in the referenced
  EVPN MAC-VRF UDN, additively when a pod matches multiple tags.
- Enable downstream physical network devices to enforce ACLs based on workload identity
  without requiring those devices to speak Kubernetes APIs.
- Keep the feature a no-op when disabled via feature flag — zero change to existing EVPN
  behaviour.

## Non-Goals

- Enforcement of network policy within the OVN/OVS datapath — that is handled by
  existing `NetworkPolicy`, `AdminNetworkPolicy`, etc.
- Defining or distributing the mapping between community values and ACL policy to physical
  devices — that is the operator's responsibility via their existing fabric management
  tooling.
- Support for non-EVPN networks or pods not attached to a MAC-VRF UDN.
- Consuming extended communities from inbound RT-2 routes to populate NetworkPolicy
  address sets (see Future Goals).
- Automatic tag value assignment — operators manage the tag namespace per UDN.
- Enforcing uniqueness of policy intent across tags (e.g. two tags with overlapping
  selectors within the same UDN is permitted).

## Introduction

EVPN (RFC 7432) RT-2 routes carry MAC/IP bindings between VTEP peers. Each route can
carry multiple BGP Extended Communities (RFC 4360) alongside the mandatory Route Target.
OVN-Kubernetes already generates RT-2 routes for every pod in a MAC-VRF UDN via static
FDB and neighbor entries that FRR reads from the kernel.

BGP Extended Communities are 8-byte values: a 2-byte type field and a 6-byte value. For
this feature the primary community type is the **Color Extended Community** (RFC 5512,
type `0x030B`), which carries a 4-byte opaque value and is widely supported in FRR and
physical switching hardware. The design is extensible to other community types (e.g. BGP
Large Communities) via a structured discriminated union in the CRD API.

FRR applies extended communities to outbound routes via **route-maps**. The OVN-Kubernetes
`routeadvertisements` controller already generates per-node `FRRConfiguration` objects
containing a `rawConfig` block with the EVPN router configuration. The `NetworkTag`
feature adds a parallel `FRRConfiguration` per node that defines:

1. **Per-tag prefix-lists** — one `ip prefix-list` and one `ipv6 prefix-list` per tag,
   containing the IPs of matched pods on that node.
2. **A single outbound route-map** (`OVNK-NT-OUT`) — one entry per (tag, address-family)
   pair, matching the corresponding prefix-list and setting the extended community.

The `routeadvertisements` controller chains into this route-map via FRR's `call`
directive at the tail of its own EVPN outbound route-map, and only does so when the
`NetworkTag` feature flag is enabled.

## User-Stories/Use-Cases

### Story 1: Fabric-enforced workload segmentation

As a **network operator** running Kubernetes workloads on an EVPN fabric, I want RT-2
routes for pods to carry a numeric tag derived from their Kubernetes labels, so that my
ToR switches can enforce ACLs between workload tiers (e.g. web, app, database) using
their existing community-based policy mechanisms — without any Kubernetes API access on
the switch.

### Story 2: Multi-tag composition

As an **operator** with overlapping policy dimensions (e.g. environment and tier), I want
a single pod to carry multiple extended communities simultaneously — one per matching
`NetworkTag` — so I can compose policy at the fabric layer without a combinatorial
explosion of tags.

## Proposed Solution

### API Details

#### `NetworkTag` CRD (cluster-scoped)

```yaml
apiVersion: k8s.ovn.org/v1
kind: NetworkTag
metadata:
  name: web-tier
spec:
  # networkRef references the ClusterUserDefinedNetwork this tag applies to.
  # The referenced network must have an EVPN MAC-VRF configured.
  # Community value uniqueness is enforced per networkRef, not cluster-wide,
  # since different tenants on different VRFs may legitimately reuse the same
  # community values.
  networkRef:
    name: my-cudn
  # namespaceSelector restricts which namespaces' pods are evaluated.
  # If absent, all namespaces participating in the referenced network are evaluated.
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: production
  # selector matches pods within the selected namespaces.
  selector:
    matchLabels:
      app: nginx
      tier: web
  # community defines the BGP extended community to set on RT-2 routes
  # for matched pods. Exactly one community type must be specified.
  community:
    # color sets a Color Extended Community (RFC 5512). Value is a
    # 4-byte unsigned integer (0–4294967295).
    color: 42
status:
  conditions:
  - type: Ready
    status: "True"
    reason: ReconcileSucceeded
    message: ""
  # matchedPods is the total number of pods currently matched across
  # the cluster (limited to pods in the referenced EVPN MAC-VRF UDN).
  matchedPods: 3
```

**Community type discriminated union** — exactly one of the following fields may be set:

| Field | Community Type | RFC | FRR syntax | Value constraints |
|---|---|---|---|---|
| `color` | Color Extended Community | RFC 5512 | `set extcommunity color <value>` | uint32 (0–4294967295) |
| `largeCommunity` | BGP Large Community | RFC 8092 | `set large-community <ga>:<ld1>:<ld2>` | Three uint32 fields: `globalAdmin`, `localData1`, `localData2` |

**Validation:**

- Webhook enforces uniqueness per `(networkRef, community type, community value)` — two
  `NetworkTag` CRs referencing the same UDN may not encode the same community value.
  `NetworkTag` CRs referencing different UDNs (different VRFs) may freely reuse the same
  community values, mirroring how community semantics are VRF-scoped at the network layer.
  For `color`, uniqueness is on the uint32 value within the UDN. For `largeCommunity`,
  uniqueness is on the full `(globalAdmin, localData1, localData2)` triple within the UDN.
- `networkRef.name` must reference an existing `ClusterUserDefinedNetwork` with an EVPN
  MAC-VRF configured. Webhook rejects tags referencing non-EVPN or non-existent networks.
- `selector` must be a valid `metav1.LabelSelector`.
- `namespaceSelector` must be a valid `metav1.LabelSelector` (optional).
- CEL rules validate that exactly one community type field is set.

**Feature flag:** `--enable-network-tag` (gcfg: `enable-network-tag`). Requires
`--enable-evpn`. The helper `util.IsNetworkTagEnabled()` returns
`IsEVPNEnabled() && config.OVNKubernetesFeature.EnableNetworkTag`.

#### Changes to existing APIs

No changes to existing CRDs. The `routeadvertisements` controller gains a read-only
dependency on `NetworkTag` CRs (list/watch) to determine whether to emit the `call
OVNK-NT-OUT` directive. No fields are added to `RouteAdvertisements`,
`FRRConfiguration`, `VTEP`, or `UserDefinedNetwork`.

### Implementation Details

#### Controller: `networktag` (cluster manager)

A new controller in `go-controller/pkg/clustermanager/networktag/` following the same
pattern as `routeadvertisements`. It watches:

- `NetworkTag` CRs (list/watch, cluster-scoped)
- Pods (list/watch, all namespaces, filtered to local node via `spec.nodeName` field
  selector per informer shard)
- Nodes
- `ClusterUserDefinedNetwork` CRs (to validate `networkRef` and resolve MAC-VRF VNI)

On each reconcile it computes, per node, per UDN:

```
{networkRef → {tag → {v4IPs: [10.0.10.5, ...], v6IPs: [fd00::5, ...]}}}
```

Only pods attached to the `networkRef` CUDN's MAC-VRF are included. Two `NetworkTag` CRs
referencing different CUDNs but using the same community value produce separate
prefix-lists with no conflict, since the RT-2 routes for each CUDN carry different Route
Targets and are imported into separate VRFs on the ToR.
Pods matching multiple `NetworkTag` selectors within the same CUDN contribute their IPs
to each matching tag.

It writes one `FRRConfiguration` per node (named `ovnk-nt-<node-hash>`) containing
the `rawConfig` block described below. When no `NetworkTag` CRs exist, it still emits
the object with a minimal catch-all permit route-map to guarantee `OVNK-NT-OUT` is always
defined when the feature flag is enabled (prevents FRR denying routes due to a missing
`call` target).

#### Generated rawConfig structure

```
# Level 1: prefix-lists — change when pods join/leave a tag
ip prefix-list ovnk-nt-42-v4 permit 10.0.10.5/32
ip prefix-list ovnk-nt-42-v4 permit 10.0.10.7/32
ipv6 prefix-list ovnk-nt-42-v6 permit fd00::5/128
ipv6 prefix-list ovnk-nt-42-v6 permit fd00::7/128

ip prefix-list ovnk-nt-100-v4 permit 10.0.10.9/32
ipv6 prefix-list ovnk-nt-100-v6 permit fd00::9/128

# Level 2: route-map — changes only when NetworkTag CRs are added/removed
route-map OVNK-NT-OUT permit 84         # tag 42 × 2
 match ip address prefix-list ovnk-nt-42-v4
 set extcommunity color 42
route-map OVNK-NT-OUT permit 85         # tag 42 × 2 + 1
 match ipv6 address prefix-list ovnk-nt-42-v6
 set extcommunity color 42
route-map OVNK-NT-OUT permit 200        # tag 100 × 2
 match ip address prefix-list ovnk-nt-100-v4
 set extcommunity color 100
route-map OVNK-NT-OUT permit 201        # tag 100 × 2 + 1
 match ipv6 address prefix-list ovnk-nt-100-v6
 set extcommunity color 100
route-map OVNK-NT-OUT permit 65534     # catch-all: unmatched pods pass through
```

Route-map sequence numbers are deterministic: v4 seq = `tag × 2`, v6 seq = `tag × 2 + 1`.
The catch-all uses fixed seq `65534`. This ensures stable config across reconciles and
avoids spurious FRR reconfiguration.

The full rawConfig is regenerated from scratch on every reconcile (same approach as
`routeadvertisements`). The generation is split into two composable functions:

- `generateNTPrefixLists(tags, podsByTag)` — changes on pod add/remove events
- `generateNTRouteMap(tags)` — changes only on NetworkTag add/remove events

#### Integration with `routeadvertisements` controller

Currently `routeadvertisements` does not apply any outbound route-map to EVPN neighbours
in the `l2vpn evpn` address-family — neighbours are simply activated. When
`IsNetworkTagEnabled()` is true, `genDefaultVRFEVPNSection` in `rawconfig.go` introduces
a named outbound route-map `OVNK-EVPN-OUT` for each EVPN neighbour and defines it with
a single catch-all entry that chains into the `NetworkTag` route-map:

```
router bgp 65000
 address-family l2vpn evpn
  neighbor X.X.X.X activate
  neighbor X.X.X.X allowas-in origin
  neighbor X.X.X.X route-map OVNK-EVPN-OUT out   ← new when flag enabled
  advertise-all-vni
 exit-address-family
exit
!
route-map OVNK-EVPN-OUT permit 9999              ← new when flag enabled
 call OVNK-NT-OUT
```

The `OVNK-EVPN-OUT` route-map contains only the `call` entry — all permit/deny logic
lives in `OVNK-NT-OUT`, owned by the `networktag` controller. Because `OVNK-NT-OUT`
always contains at least a catch-all permit (see Lifecycle section above), adding the
`call` is safe even before any `NetworkTag` CRs exist.

The `networktag` controller's `FRRConfiguration` is assigned `rawConfigPriority - 1`
(priority 9 vs the routeadvertisements priority of 10). Since the two configs define
non-overlapping FRR directives, priority only affects merge ordering, not correctness.

#### Lifecycle and consistency

- **Pod label changes:** the `networktag` controller watches pods and re-reconciles on
  any label change for pods in EVPN MAC-VRF UDNs. FRR re-advertises affected RT-2 routes
  with updated communities via a soft outbound reconfiguration.
- **KubeVirt live migration:** when a VM migrates, the source node's FDB/neighbor entries
  are withdrawn before the target's are programmed. There is a brief window where the
  target's `FRRConfiguration` may not yet carry the pod IP in its prefix-lists. This
  eventual consistency gap is accepted — FRR sends a corrective UPDATE within one reconcile
  cycle, consistent with how MAC mobility works elsewhere in the EVPN stack.
- **NetworkTag deletion:** the controller removes the tag's prefix-list entries and
  route-map seq entries from all per-node `FRRConfiguration` objects. The catch-all
  permit ensures no connectivity disruption during the cleanup.
- **Gateway mode:** this feature is only applicable in local gateway mode (`lgw`), as
  EVPN requires it. No shared gateway (`sgw`) considerations apply.

#### Directory layout

```
go-controller/pkg/clustermanager/networktag/
  controller.go          # main reconcile loop
  rawconfig.go           # generateNTPrefixLists, generateNTRouteMap
  controller_test.go
  suite_test.go

go-controller/pkg/crd/networktag/v1/
  types.go               # NetworkTag, NetworkTagSpec, NetworkTagStatus, Community types
  doc.go
  register.go
  zz_generated.deepcopy.go
  apis/applyconfiguration/...

go-controller/pkg/factory/factory.go    # add NetworkTag informer (when flag enabled)
go-controller/pkg/config/config.go      # add EnableNetworkTag field
go-controller/pkg/util/multi_network.go # add IsNetworkTagEnabled()
go-controller/pkg/clustermanager/clustermanager.go  # start networktag controller
go-controller/pkg/clustermanager/routeadvertisements/rawconfig.go  # call OVNK-NT-OUT
```

### Testing Details

- **Unit tests:** selector evaluation against pod labels; `generateNTPrefixLists` and
  `generateNTRouteMap` for single tag, multiple tags, tag deletion, empty tag set,
  dual-stack pods; deterministic seq number generation; webhook uniqueness validation.
- **E2E tests:** deploy a MAC-VRF EVPN cluster; create `NetworkTag` CRs; verify via
  `show bgp l2vpn evpn` that RT-2 routes for matched pods carry the expected extended
  community; verify unmatched pods carry no community; verify multiple matching tags
  produce additive communities; verify tag deletion removes the community; verify
  KubeVirt VM RT-2 carries the correct community post live-migration.
- **API tests:** webhook rejects duplicate (type, value) pairs; webhook rejects missing
  community field; webhook rejects multiple community type fields set simultaneously.
- **Scale tests:** 100 `NetworkTag` CRs × 1000 matched pods per node; measure reconcile
  latency and FRR config size; verify no spurious BGP UPDATEs on unrelated pod events.
- **Cross-feature tests:** interaction with KubeVirt live migration (RT-2 community
  continuity post-migration); dual-stack pods (both v4 and v6 RT-2 carry communities);
  feature flag disabled (zero FRR config change, no `call` directive emitted).

### Documentation Details

- New feature doc: `docs/features/bgp-integration/network-tag.md` covering the workflow
  (create `NetworkTag` → verify RT-2 communities → configure ToR ACLs), CRD reference,
  and troubleshooting steps (FRR `show bgp l2vpn evpn detail` to inspect communities).
- Update `docs/features/bgp-integration/evpn.md` to cross-reference the NetworkTag feature.
- `mkdocs.yml` entry under Enhancement Proposals and under the BGP integration features
  section.

## Future Goals

### Inbound: RT-2 communities as NetworkPolicy peers

The outbound direction defined in this OKEP — advertising pod label membership as
extended communities on RT-2 routes — is one half of a symmetric identity propagation
model. The other half is **inbound**: consuming extended communities from RT-2 routes
learned from the fabric and using them to influence NetworkPolicy enforcement within
the cluster.

The concept: a remote non-Kubernetes workload (bare metal server, VM on another platform,
pod in a separate cluster sharing the same EVPN fabric) advertises an RT-2 route carrying
`color: 42`. OVN-Kubernetes learns that binding and automatically includes the remote IP
in the OVN address set that NetworkPolicy already uses for pods matching the `NetworkTag`
selector associated with `color: 42`. No NetworkPolicy API changes are required — the
remote endpoint simply appears in the existing peer address set.

This requires two pieces of work not in scope for v1:

1. **RIB access** — OVN-Kubernetes has no mechanism to read learned RT-2 routes from
   FRR's BGP RIB, including their extended communities. The kernel ARP/neighbor table
   carries the MAC/IP bindings but not the BGP communities. Options include polling
   FRR's JSON RIB via `vtysh -c "show bgp l2vpn evpn json"` from the EVPN node
   controller, or a future frr-k8s API that exposes learned routes as Kubernetes CRs
   (the cleaner long-term path).

2. **`AddressSetManager` extension** — the existing `AddressSetManager` calls
   `SetAddresses()` (full replace) on every pod reconcile, which would wipe any
   externally contributed IPs. A new `AddExternalIPs(selector, ips)` extension point
   is needed so that fabric-learned IPs survive pod reconcile cycles and are included
   in the `SetAddresses()` call alongside pod IPs. The `NetworkTag` selector↔community
   mapping provides the bridge between a received community value and the correct
   address set key.

When both pieces are available, the full symmetric model becomes possible: operators
define a single `NetworkTag` CR that simultaneously drives outbound community
advertisement for local pods *and* populates the matching address set with remote
endpoints advertising the same community — enabling bidirectional fabric-aware policy
without any changes to Kubernetes NetworkPolicy.

## Risks, Known Limitations and Mitigations

- **Label churn causing BGP instability:** rapid pod label changes trigger FRR
  soft-reconfiguration. Mitigation: the controller's work queue rate-limiter provides
  natural debouncing; each reconcile is per-node, so churn on one node does not affect
  others.
- **Route-map seq number exhaustion:** seq = `tag × 2` allows tags up to 32766 before
  colliding with the catch-all at `65534`. For `color` communities (uint32 value space)
  tags above 32766 would require a different seq scheme. Mitigation: document the limit;
  revisit seq numbering if large tag spaces are needed in practice.
- **`OVNK-NT-OUT` not yet present at startup:** mitigated by the `networktag` controller
  always emitting the catch-all route-map as soon as the feature flag is enabled,
  regardless of whether any `NetworkTag` CRs exist.
- **Long-term rawConfig dependency:** route-map configuration is currently expressed as
  opaque FRR config strings. When frr-k8s introduces a native route-map API, this
  implementation should be migrated to use it.

## OVN-Kubernetes Version Skew

Planned for introduction in **v1.4.0**. The feature is entirely additive and
feature-flag gated (`--enable-network-tag`). No existing API or datapath is changed.
During a rolling upgrade, nodes running older ovnkube-node binaries continue to function
normally — the `networktag` controller only generates `FRRConfiguration` objects, which
are applied by the FRR-k8s operator independently of ovnkube-node version.

## Backwards Compatibility

- **New CRD only:** `NetworkTag` is a net-new cluster-scoped CRD. No existing CRD fields
  are modified.
- **Feature-flag gated:** when `--enable-network-tag` is absent or false, no
  `NetworkTag` informer is registered, no `OVNK-NT-OUT` route-map is emitted, and no
  `call` directive is added to the EVPN outbound route-map. Existing EVPN behaviour is
  identical to today.
- **E2E tests:** all existing EVPN E2E tests continue to pass unchanged. New E2E tests
  are added in a separate suite gated on the feature flag.

## Alternatives

### Raw label strings encoded as opaque extended communities

Pod labels are arbitrary UTF-8 strings (key up to 253 chars, value up to 63 chars).
Extended community value fields are 6 bytes. Encoding raw strings is impossible without
truncation or hashing. Hashing produces collisions and is not human-readable at the ToR.
Rejected in favour of operator-assigned integers.

### Single tag per pod (strict, no additive matching)

A pod may only match one `NetworkTag`; conflicts are rejected at admission. Rejected
because it forces operators to create a combinatorial tag space for multi-dimensional
policy (e.g. env × tier), and because most fabric hardware can match on multiple BGP
communities simultaneously.

### Encoding labels as RT (Route Target) communities

Route Target communities are consumed by FRR and VTEP peers for VRF import/export
decisions. Reusing the RT type for workload identity would interfere with EVPN VRF
membership signalling. Rejected.

### Cluster-wide community uniqueness

An earlier design enforced community value uniqueness cluster-wide. This is incorrect
from a networking standpoint: community semantics are VRF-scoped at the fabric layer.
Tenant A using color:42 in VRF blue and tenant B using color:42 in VRF red is perfectly
valid — the ToR enforces ACLs per-VRF and the two bindings are completely isolated.
Uniqueness is therefore enforced per `(networkRef, community value)` i.e. per UDN, which
maps 1:1 to a VRF.

### Node-level controller generating rawConfig

The pod-watching and per-node FRRConfiguration generation pattern is already established
in the cluster manager (`routeadvertisements` controller). Duplicating it in ovnkube-node
would mean every node independently evaluates all `NetworkTag` selectors against its local
pods, requiring the `NetworkTag` CRD to be watched N times (once per node). The cluster
manager approach centralises policy resolution and is consistent with the existing
architecture.

## References

- [RFC 4360 — BGP Extended Communities Attribute](https://www.rfc-editor.org/rfc/rfc4360)
- [RFC 5512 — The BGP Encapsulation SAFI and Tunnel Encapsulation Attribute (Color community)](https://www.rfc-editor.org/rfc/rfc5512)
- [RFC 7432 — BGP MPLS-Based Ethernet VPN (EVPN)](https://www.rfc-editor.org/rfc/rfc7432)
- [RFC 8092 — BGP Large Communities Attribute](https://www.rfc-editor.org/rfc/rfc8092)
- [OKEP-5088: EVPN Support](okep-5088-evpn.md)
- [EVPN Feature Documentation](../features/bgp-integration/evpn.md)
- [GitHub Issue #6658](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6658)
