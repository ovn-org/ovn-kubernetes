# OKEP-6227: EgressTrafficClass — Selective EgressIP

* Issue: [#6227](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6227)

## Problem Statement

The existing EgressIP feature applies to all egress traffic from a matched pod. An EgressIP object may list several addresses (ECMP among assigned next hops), but there is no way to choose a different EgressIP — and thus a different source IP or egress interface — based on destination CIDR or L4 protocol/port.

## Goals

* Allow a cluster administrator to define destinations (destination CIDRs with optional L4 protocol/port filtering) via a new `EgressTrafficClass` CRD.
* Allow an `EgressIP` to select `EgressTrafficClass` resources via a `trafficSelector` label selector, routing only matching traffic through that egress IP.
* Support L4-level filtering: route traffic based on IP protocol (TCP/UDP/SCTP) and optional destination and/or source port or port range, using an NPEP-187-style `protocols` list so additional protocols can be added later without a breaking API change.
* Support multiple `EgressIP` objects with `trafficSelector` serving the same pod, each handling different destination networks or protocols.
* Support coexistence of `trafficSelector` EgressIPs on secondary host interfaces with a non-`trafficSelector` default EgressIP on the OVN network (Story 2).
* Work with EgressIPs assigned to both OVN primary network interfaces and secondary host interfaces.
* Support both local and shared OVN gateway modes.
* Support EgressIPs on user-defined networks (UDNs) for OVN-network EgressIPs (SNAT on the gateway router).

## Non-Goals

* **Protocol-specific** ICMP (or other non-port protocol) matching: the `protocols` list is shaped so ICMP (type/code) or other non-port protocols can be added as a structured match later; this OKEP only defines TCP, UDP, and SCTP entries. This does not exclude ICMP traffic from CIDR-only destinations — a destination with no `protocols` matches all traffic to the CIDR, ICMP included (see `Destination.Protocols` in API Details); only *selectively matching on* ICMP (the way `protocols` lets you select on TCP/UDP/SCTP) is out of scope.
* Cross-feature consumption of `EgressTrafficClass` by NetworkQoS, ClusterNetworkPolicy, or other APIs in this OKEP. The type is a named **destination match** (CIDR + optional L4), not a policy: it has no allow/deny/drop, no rule order, and no ingress/`from` match. A later OKEP may select it from other **egress** consumers (for example a ClusterNetworkPolicy Allow rule or NetworkQoS dest matching); action and direction stay on those APIs. This OKEP's only consumer is EgressIP. A generic `TrafficClass` or ingress reuse would need `sources` (or `ingress`/`egress` subtrees) and is out of scope.
* Catch-all EgressIP on a secondary host interface coexisting with a `trafficSelector` EgressIP on the OVN network for the same pod. Both LRPs reroute to the same transit switch; the packet arrives on the assigned node's management port; the catch-all `6001: from <podIP>` IP rule then steals dests that should have reached gateway-router SNAT for the OVN EgressIP. This OKEP does not add dest-exclusion host IP rules to undo that.

## Future Goals

* Secondary host interface EgressIPs on UDNs, including with `trafficSelector`, once base secondary EgressIP on UDN exists. Reuse this OKEP's host path (per-destination IP rules and dest-scoped iptables SNAT) inside the UDN routing domain (VRF), rather than a second match model. A likely vehicle is Uplink-backed CUDN gateways ([OKEP-6019](okep-6019-vrf-lite-shared-gateway-external-bridges.md)), which already lists secondary EgressIP through `Uplink` as a future consumer.

## Introduction

In telecommunications and carrier deployments, a single pod often needs to reach multiple external networks (for example OAM, signaling, and user plane). Each network must see traffic leave an **egress node** interface with an **EgressIP** source address (not the pod IP, and not the node IP). Local-gateway masquerade to node IPs is not enough: those addresses are per-node, move when the pod or assignment moves, and are not the HA EgressIP that external ACLs allowlist. Secondary-host EgressIP can already steer via host routes, but if a pod matches more than one EgressIP the winner is undefined — operators cannot pin which EgressIP (hence which egress node interface and source IP) applies by destination CIDR or L4. Different services on the same network (e.g. SIP on UDP 5060 vs RTP on UDP 16384–32767) may also need different EgressIPs.

EgressTrafficClass solves this by introducing destination-based egress routing with optional L4 filtering. A new `EgressTrafficClass` CRD defines destinations — destination CIDRs with optional protocol/port filters — and a new `trafficSelector` field on `EgressIP` selects which `EgressTrafficClass` resources apply. A `trafficSelector` EgressIP only handles matching destinations. Non-matching traffic is not handled by that EgressIP: it uses a coexisting non-`trafficSelector` default EgressIP if one is assigned to the pod (Story 2), otherwise normal OVN egress (typically the node IP). Omitting `trafficSelector` is unchanged EgressIP behavior: all of the pod's egress uses that EgressIP.

### Behavioral change: multi-EgressIP pod support

Prior to this OKEP, having multiple `EgressIP` objects selecting the same pod was considered **undefined behavior**. The controller would arbitrarily pick one EgressIP as the "primary" and put the rest on standby. There was no guarantee about which EgressIP would serve the pod, and failover between them was non-deterministic.

This OKEP changes that behavior to be **deterministic** when `trafficSelector` is used:

- Multiple `EgressIP` objects with different `trafficSelector` values can coexist on the same pod, each handling traffic to different destination networks.
- A non-`trafficSelector` default EgressIP on the OVN network can coexist with `trafficSelector` EgressIPs on secondary host interfaces, acting as a catch-all for traffic not matching any destination. The reverse mix (catch-all on a secondary host NIC, `trafficSelector` on OVN) is a Non-Goal.
- Priority-based LRP evaluation (100 for `trafficSelector`, 99 for every non-`trafficSelector` EgressIP) ensures dest/L4 matches win over catch-all. On upgrade, existing catch-all EIP LRPs are rewritten from 100 to 99; `trafficSelector` LRPs use 100. Relative order vs EgressService (101), no-reroute (102), and EIP QoS (103) is unchanged.
- Without `trafficSelector`, multiple EgressIPs selecting the same pod remain undefined. Standalone EgressIP still SNAT/reroutes all pod egress; only the LRP priority changes (100 → 99).

### Example topology

```text
                        ┌───────────────────────────────────────────┐
                        │              Egress Node                  │
                        │                                           │
Pod ──OVN──►            │  ┌─ eth-mgmt ──► Mgmt Network             │
  10.244.1.3            │  │   (192.168.150.101)  (192.168.250.0/24)│
                        │  │                                        │
         reroute ──────►│──┤                                        │
                        │  │                                        │
                        │  └─ eth-sig  ──► Signaling Network        │
                        │      (192.168.200.101)  (192.168.251.0/24)│
                        └───────────────────────────────────────────┘
```

## User-Stories/Use-Cases

**Story 1: Per-destination egress IP for multi-network workloads**

As a cluster administrator, I want pods in a telco namespace to use different source IP addresses depending on which backend network they are reaching, so that each network sees traffic from the correct interface and IP range.

**Story 2: Default egress IP with destination-specific overrides**

As a cluster administrator, I want a default EgressIP on the OVN network that handles general internet traffic, combined with destination-specific EgressIPs on secondary host interfaces for particular backend networks, so that pods have predictable source IPs for all egress traffic without requiring per-destination configuration for every possible destination.

**Story 3: Per-protocol/port egress routing on the same network**

As a cluster administrator, I want SIP signaling traffic (UDP destination 5060) to the OAM network to use one egress IP on a dedicated interface, while SSH management traffic (TCP destination 22) to the same network uses a different egress IP, so each service type has the correct source IP and interface.

**Story 4: Local RTP port range as source-port match**

As a cluster administrator, I want media traffic from a pod that binds a local RTP port range (UDP source 16384–32767) to use a dedicated egress IP, even when remote RTP destination ports are unpredictable, so media and other traffic to the same destination CIDR can take different EgressIPs.

## Proposed Solution

### API Details

#### EgressTrafficClass CRD

A new cluster-scoped custom resource that defines destination matches (CIDR + optional L4). The name is **egress-scoped**, not EgressIP-specific: other egress features could select the same named dest sets later. It is not a policy object (no action, no order, no direction wrapper) and not an ingress classifier (`spec.destinations` is a `to:` list). This OKEP only defines the EgressIP consumer.

```go
// +genclient
// +genclient:nonNamespaced
// +kubebuilder:resource:shortName=etc,scope=Cluster
// +kubebuilder:subresource:status
type EgressTrafficClass struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Spec              EgressTrafficClassSpec   `json:"spec"`
    Status            EgressTrafficClassStatus `json:"status,omitempty"` // NEW
}

type EgressTrafficClassSpec struct {
    // Destinations defines destination networks with optional L4 protocol/port filtering.
    // +kubebuilder:validation:MaxItems=25
    // +kubebuilder:validation:MinItems=1
    Destinations []Destination `json:"destinations"`
}

// EgressTrafficClassStatus surfaces whether this object is currently
// referenced, so operators don't have to grep EgressIP trafficSelector
// labels cluster-wide before deleting it (see "Deleting a referenced
// EgressTrafficClass" below).
type EgressTrafficClassStatus struct {
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Destination matches traffic to a destination CIDR, optionally narrowed by
// an NPEP-187-style protocols list (nested protocol keys, destinationPort
// and/or sourcePort).
type Destination struct {
    // CIDR is the destination network in IPv4 or IPv6 CIDR notation.
    // isCIDR() also accepts host-and-mask values (k8s.io/kubernetes#134224);
    // require the canonical network address.
    // +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self) == cidr(self).masked()",message="CIDR must be a valid network address"
    CIDR string `json:"cidr"`

    // Protocols is an optional list of L4 matches. When empty or omitted, all
    // protocols and ports to the CIDR match. When set, traffic must match at
    // least one entry (OR semantics). Exactly one protocol field must be set
    // per entry so unknown future protocols decode as {} for older clients
    // (NPEP-187 option 3).
    // +optional
    Protocols []ProtocolMatch `json:"protocols,omitempty"`
}

// ProtocolMatch is a one-of union. This OKEP supports TCP, UDP, and SCTP. A future
// OKEP can add ICMP (type/code) or other protocol structs without renaming
// this field from protocols to ports.
// +kubebuilder:validation:XValidation:rule="(has(self.tcp)?1:0)+(has(self.udp)?1:0)+(has(self.sctp)?1:0)==1",message="exactly one of tcp, udp, or sctp must be set"
type ProtocolMatch struct {
    TCP  *PortProtocol `json:"tcp,omitempty"`
    UDP  *PortProtocol `json:"udp,omitempty"`
    SCTP *PortProtocol `json:"sctp,omitempty"`
}

type PortProtocol struct {
    // DestinationPort matches the remote port. Optional.
    // +optional
    DestinationPort *Port `json:"destinationPort,omitempty"`

    // SourcePort matches the local (pod) port. Optional. Used when the
    // workload binds a well-known local port or port range (e.g. RTP).
    // +optional
    SourcePort *Port `json:"sourcePort,omitempty"`
}

// Port is exactly one of Number or Range.
// +kubebuilder:validation:XValidation:rule="(has(self.number)?1:0)+(has(self.range)?1:0)==1",message="exactly one of number or range must be set"
type Port struct {
    // +optional
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=65535
    Number *int32 `json:"number,omitempty"`

    // +optional
    Range *PortRange `json:"range,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.end >= self.start",message="end must be >= start"
type PortRange struct {
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=65535
    Start int32 `json:"start"`
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=65535
    End int32 `json:"end"`
}
```

**Status**: One condition, **`InUse`**, reports whether this `EgressTrafficClass` is currently matched by at least one `EgressIP.spec.trafficSelector`:

```yaml
status:
  conditions:
  - type: InUse
    status: "True"
    reason: ReferencedByEgressIP
    message: "Matched by trafficSelector of 1 EgressIP(s)"
```

```yaml
status:
  conditions:
  - type: InUse
    status: "False"
    reason: NotReferenced
    message: "Not matched by any EgressIP trafficSelector"
```

`TrafficConflict` is intentionally not surfaced here: a conflict is a property of a *pair* of `EgressIP` objects assigned to the same pod, not of the `EgressTrafficClass` itself — the same class can be conflict-free for one pod and conflicting for another, so it stays on `EgressIP` only. Computing `InUse` requires the `EgressTrafficClass` informer to also watch `EgressIP.spec.trafficSelector`/label changes, not just react by re-queueing matching EgressIPs.

Key points:
- Cluster-scoped (not namespaced).
- Labels on the object are used for selection by `EgressIP.spec.trafficSelector`.
- CIDRs are validated at admission time via CEL (`isCIDR(self) && cidr(self) == cidr(self).masked()`), so host-and-mask values like `192.168.1.1/24` are rejected ([kubernetes#134224](https://github.com/kubernetes/kubernetes/issues/134224)).
- Maximum 25 destinations per resource; at least one required.
- `protocols` follows [NPEP-187](https://network-policy-api.sigs.k8s.io/npeps/npep-187-ports-and-protocols/) option 3: nested `tcp` / `udp` / `sctp` keys and optional `destinationPort` / `sourcePort` (`number` or `range`). OR across list entries; AND within an entry when both ports are set. Exactly one protocol key per entry.
- A destination with no `protocols` matches all traffic to the CIDR.
- Protocol-only match (e.g. all TCP) omits both port fields, or uses `destinationPort.range` covering 1–65535.
- When both `destinationPort` and `sourcePort` are set, a packet must match both.
- ICMP is reserved for a later OKEP.
- **Overlapping destinations** are not rejected at `EgressTrafficClass` admission (label selectors do not say which pods will share two CRs):

| Where | Allowed? |
|--------|----------|
| Two dests in one `EgressTrafficClass` | Yes (redundant). |
| Two `EgressTrafficClass` resources selected by the **same** EgressIP | Yes (same SNAT). |
| Two `EgressTrafficClass` resources / EgressIPs, **different** pods | Yes. |
| Two `trafficSelector` EgressIPs, **same** pod, overlapping traffic | No correct routing. Detected at assignment as `TrafficConflict` (see below). |

#### EgressIP TrafficSelector field

A new optional pointer field on `EgressIP.spec`:

```go
type EgressIPSpec struct {
    EgressIPs         []string              `json:"egressIPs"`
    NamespaceSelector metav1.LabelSelector  `json:"namespaceSelector"`
    PodSelector       metav1.LabelSelector  `json:"podSelector"`
    // +kubebuilder:validation:XValidation:rule="(has(self.matchLabels) && size(self.matchLabels) > 0) || (has(self.matchExpressions) && size(self.matchExpressions) > 0)",message="trafficSelector must specify matchLabels or matchExpressions"
    TrafficSelector   *metav1.LabelSelector `json:"trafficSelector,omitempty"` // NEW
}
```

The field is a pointer to distinguish:
- **nil** (field absent): no traffic selector — all of the pod's egress uses this EgressIP (existing behavior).
- **set**: must include `matchLabels` and/or `matchExpressions` with at least one entry (CEL). Matches only `EgressTrafficClass` resources whose labels satisfy the selector. `trafficSelector: {}` is rejected; matching every `EgressTrafficClass` in the cluster is not supported.

When `trafficSelector` is set (non-nil), the EgressIP only handles traffic defined in matching `EgressTrafficClass` resources.

**When no `EgressTrafficClass` matches the `trafficSelector`**: This is the same fall-through mechanism described in the Introduction, just for the degenerate case where the resolved destination set is empty — every packet is "non-matching," so every packet falls through to a coexisting default EgressIP or normal OVN egress, exactly as a single non-matching destination would. It is not a blackhole: the reroute LRP simply matches no traffic, so the EgressIP stops participating in routing for this pod (nothing it would otherwise reroute is dropped — traffic takes whatever path it would take if this EgressIP didn't exist). A warning is logged and a Kubernetes event is emitted on the EgressIP object:

```text
Warning  NoTrafficClass  TrafficSelector is set but no matching EgressTrafficClass
                          resources were found; traffic will not be routed via
                          this EgressIP until matching EgressTrafficClass resources
                          are created
```

This is by design: no matching egress traffic classes means no traffic should use this EgressIP. Operators should create the required `EgressTrafficClass` resources or verify the `trafficSelector` labels.

**Deleting a referenced `EgressTrafficClass`**: Selection is via label selector, not a name reference, so "referenced" is computed from labels rather than a fixed link — an `EgressTrafficClass` can stop matching an EgressIP just by a label edit, with no delete involved. A finalizer would only guard the delete path, not relabeling, and would block `kubectl delete` on an object with stale or orphaned labels if the owning controller is down. This OKEP does not add a finalizer: deleting (or un-matching) an `EgressTrafficClass` drops its destinations from every EgressIP that selected it, the same as the no-match case above (`TrafficSelectorResolved` moves to `False` once no classes remain, or the LRP match narrows if other classes still match). Operators should check the `InUse` condition (see `EgressTrafficClassStatus` above) before deleting an `EgressTrafficClass`, rather than grepping EgressIP `trafficSelector` labels cluster-wide.

#### EgressIP Status Conditions

Two new conditions are added to the EgressIP status to improve observability:

```go
type EgressIPStatus struct {
    Items      []EgressIPStatusItem `json:"items"`
    Conditions []metav1.Condition   `json:"conditions,omitempty"` // NEW
}
```

**`TrafficSelectorResolved`**: Set to `True` when at least one destination **actually resolved** (has a programmed LRP/NAT) — not merely when a matching `EgressTrafficClass` exists. Set to `False` for any of three distinct reasons, so operators get the right fix for each:
- `NoMatchingEgressTrafficClass`: `trafficSelector` matched zero `EgressTrafficClass` resources. Fix: create/relabel the class.
- `AllDestinationsUnsupported`: `trafficSelector` matched one or more `EgressTrafficClass` resources, but every destination across all of them had an unsupported `protocols` entry (see **Fail closed on unrecognized `protocols` entries**) — a matching class exists, yet zero destinations actually resolved. Fix: upgrade ovnkube-controller to a version that understands the protocol field in use, or avoid that field until it does.
- `FeatureGateDisabled`: see below.

```yaml
conditions:
- type: TrafficSelectorResolved
  status: "True"
  reason: DestinationsFound
  message: "Resolved 2 destinations from 1 EgressTrafficClass resource(s)"
```

```yaml
conditions:
- type: TrafficSelectorResolved
  status: "False"
  reason: NoMatchingEgressTrafficClass
  message: "No EgressTrafficClass resources match trafficSelector"
```

```yaml
conditions:
- type: TrafficSelectorResolved
  status: "False"
  reason: AllDestinationsUnsupported
  message: "1 EgressTrafficClass resource(s) matched, but all destinations had unsupported protocols entries"
```

```yaml
conditions:
- type: TrafficSelectorResolved
  status: "False"
  reason: FeatureGateDisabled
  message: "trafficSelector is set but --enable-egress-traffic-class is disabled"
```

**`TrafficConflict`**: Set to `True` when two or more `trafficSelector` EgressIPs targeting the same pod have **overlapping traffic** — overlapping destination CIDRs **and** overlapping L4 matches. Disjoint L4 on the same CIDR is not a conflict (e.g. UDP dest 5060 vs TCP dest 22 — different protocol; or UDP dest 5060 vs UDP dest 22, both with no source port set — same protocol but disjoint *explicit* destination ports). An unset port field is a wildcard that matches any value on that side, so a dest-port-only entry always overlaps a source-port-only entry of the same protocol on the same CIDR — there is no disjoint case between those two shapes. Overlap inside one `EgressTrafficClass`, or across `EgressTrafficClass` resources selected by the same EgressIP, is not a `TrafficConflict`.

Overlap rules:
- CIDR-only destination (no `protocols`) overlaps any other destination whose CIDR intersects that CIDR, including L4-narrowed ones, because the CIDR-only match accepts all protocols/ports.
- Two L4 destinations conflict only if their CIDRs intersect **and** at least one protocol entry pair intersects: same protocol, overlapping destination ports (unset dest port matches all dest ports), and overlapping source ports (unset source port matches all source ports).
- The controller performs this analysis during pod assignment and surfaces the conflicting peer EgressIP and matchers in the condition message.

**Fail closed, not detect-and-install**: Admission-time rejection is not possible — whether two destinations actually overlap for a given pod depends on which EgressIPs' pod/namespace selectors intersect, which cannot be known from either `EgressTrafficClass` or `EgressIP` object in isolation. Deterministic precedence by CIDR specificity (most-specific wins) also does not fully resolve ties: two `trafficSelector` EgressIPs with identical CIDRs and overlapping L4 have equal specificity. Instead, conflict detection at pod assignment gates programming, with a **stable tiebreaker** rather than informer/reconciliation processing order — order is not guaranteed stable across controller restarts, leader re-elections, or cache resyncs, and a winner that depends on it could flip which EgressIP serves a pod's traffic with no configuration change. The winner is instead determined from immutable object metadata, recomputed the same way regardless of processing order: the `EgressIP` with the **older `metadata.creationTimestamp`** wins (keeps its LRP and NAT); `metadata.name` (lexicographic) breaks a tie on identical timestamps. The losing EgressIP is **not** programmed (no LRP, no NAT) and is set to `TrafficConflict=True` until the conflict is resolved (by adjusting destinations/protocols on one side, or removing the overlap). This reuses the existing non-`trafficSelector` multi-EgressIP "one active, rest standby" mechanism rather than adding a new admission-time check or OVN-side tie-break, with a stable rule for which side is "active."

```yaml
# Set on the unprogrammed EgressIP (newer metadata.creationTimestamp, the
# losing side of the stable tiebreaker); eip-signaling (older, already active
# for this pod) keeps TrafficSelectorResolved=True and its LRP/NAT.
conditions:
- type: TrafficConflict
  status: "True"
  reason: OverlappingTraffic
  message: "Destination 192.168.250.0/24 UDP/5060 overlaps with EgressIP eip-signaling for pod selector app=telco-workload; not programmed"
```

#### Example resources

```yaml
# CIDR-only destination: all traffic to OAM network
apiVersion: k8s.ovn.org/v1
kind: EgressTrafficClass
metadata:
  name: mgmt-traffic
  labels:
    traffic-group: mgmt
spec:
  destinations:
  - cidr: "192.168.250.0/24"
---
# L4-filtered destinations: SIP signaling and RTP media on the same network
apiVersion: k8s.ovn.org/v1
kind: EgressTrafficClass
metadata:
  name: signaling-traffic
  labels:
    traffic-group: signaling
spec:
  destinations:
  - cidr: "192.168.250.0/24"
    protocols:
    - udp:
        destinationPort:
          number: 5060
    - udp:
        destinationPort:
          range:
            start: 8000
            end: 8010
  - cidr: "192.168.250.0/24"
    protocols:
    - tcp:
        destinationPort:
          number: 22
---
# Source-port match: local RTP bind range (remote dest ports unknown)
apiVersion: k8s.ovn.org/v1
kind: EgressTrafficClass
metadata:
  name: rtp-media
  labels:
    traffic-group: media
spec:
  destinations:
  - cidr: "192.168.250.0/24"
    protocols:
    - udp:
        sourcePort:
          range:
            start: 16384
            end: 32767
---
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: eip-mgmt
spec:
  egressIPs:
  - 192.168.150.101
  namespaceSelector:
    matchLabels:
      env: production
  podSelector:
    matchLabels:
      app: telco-workload
  trafficSelector:
    matchLabels:
      traffic-group: mgmt
```

### Feature Gate

The feature is gated by `--enable-egress-traffic-class` (config: `enable-egress-traffic-class`). When enabled, it implicitly enables `--enable-egress-ip` since EgressTrafficClass extends EgressIP. When disabled (default), the EgressTrafficClass informer is not registered and no destination-filtered LRPs or NATs are created — but a non-nil `trafficSelector` on an EgressIP is **not** treated as absent. Under the pointer semantics above, nil means catch-all; silently treating a disabled-but-set `trafficSelector` as nil would widen that EgressIP from "only matching destinations" to "all of the pod's egress," misrouting non-matching traffic through the wrong source IP or interface. Instead, same as the `NoTrafficClass` case: no LRP or NAT is programmed for that EgressIP at all (fail closed, not fail open), `TrafficSelectorResolved` is set to `False` with `reason: FeatureGateDisabled`, and a warning event is emitted — distinct from `NoMatchingEgressTrafficClass` so operators can tell "feature not enabled" apart from "no CRs match."

### Implementation Details

The feature is implemented in OVN-Kubernetes across:

1. **ovnkube-controller** (`pkg/ovn/egressip.go`): LRPs, NAT, and address sets in the OVN NB database.
2. **ovnkube-node** (`pkg/node/controllers/egressip/egressip.go`): Linux IP rules, routing tables, and iptables SNAT on the egress node.

EgressIP node assignment in ovnkube-cluster-manager is unchanged.

#### ovnkube-controller changes

**Destination parsing**: Destinations from all matching `EgressTrafficClass` resources are parsed into structured matches (CIDR, protocol, destination and/or source port/range). These are split into two groups:
- **CIDR-only destinations** (no `protocols`): Added to an OVN address set.
- **L4-filtered destinations**: Converted to inline OVN match conditions (one clause per `protocols` entry).

**Fail closed on unrecognized `protocols` entries**: Admission CEL requires exactly one of `tcp`/`udp`/`sctp` per entry for *this* schema version, so a genuinely empty or multi-set entry is rejected before it reaches the controller. But a future OKEP will add more protocols (e.g. ICMP) to `ProtocolMatch`, and CEL can only validate against fields that exist in the schema version it was written for — it cannot protect against version skew between the object's schema and an older ovnkube-controller binary. If a pod's `EgressTrafficClass` uses a protocol field an older binary's generated `ProtocolMatch` type doesn't have, that field is silently dropped on JSON decode (`encoding/json` behavior, same as the `EgressIPSpec.TrafficSelector` skew case above), and the entry decodes as `ProtocolMatch{}` — all of TCP/UDP/SCTP nil. `ParseDestinations` treats that as **unsupported, not unfiltered**: it does not fall back to "no restriction" (which would silently widen that destination to all protocols/ports — fail-open, a scoping bug), and does not fail the whole `EgressTrafficClass`. Instead it skips that one entry, programs no LRP/SNAT clause for it, and logs a warning event on the EgressIP (same pattern as `NoTrafficClass`) naming the affected `EgressTrafficClass` and entry index, so the gap is visible rather than silent. `TrafficSelectorResolved` itself stays based on whether at least one destination resolved overall — this does not introduce a new condition state for a partial, per-entry skip.

**Destination networks address set**: For each EgressIP with `trafficSelector`, an OVN address set is created containing the CIDRs from CIDR-only destinations. L4-filtered destinations cannot be represented in address sets and are handled via inline match conditions. The address set is updated when `EgressTrafficClass` resources are created, updated, or deleted.

**Destination-filtered reroute LRP**: The reroute LRP match combines address set matching with inline L4 conditions:

```text
# CIDR-only (priority 100):
ip4.src == 10.244.1.3 && ip4.dst == $<dest-nets-addr-set>    reroute    10.244.2.2

# L4-filtered (priority 100):
ip4.src == 10.244.1.3 && (ip4.dst == 10.0.0.0/8 && tcp && tcp.dst == 5060)    reroute    10.244.2.2

# L4-filtered with source port (priority 100):
ip4.src == 10.244.1.3 && (ip4.dst == 10.0.0.0/8 && udp && udp.src == 16384..32767)    reroute    10.244.2.2

# Mixed CIDR-only + L4 (priority 100):
ip4.src == 10.244.1.3 && (ip4.dst == $<addrset> || (ip4.dst == 10.0.0.0/8 && tcp && tcp.dst == 5060))    reroute    10.244.2.2

# This EgressIP has no trafficSelector field set (not a feature-disabled
# case — the feature is enabled, this particular object just doesn't use
# it), coexisting as default (priority 99):
ip4.src == 10.244.1.3    reroute    100.64.0.3
```

Port ranges use OVN range syntax: `tcp.dst == 5060..5062`, `udp.src == 16384..32767`.

**Destination-filtered NAT**: When the EgressIP is on the OVN network and has `trafficSelector`, the SNAT rule on the gateway router must match the same traffic as the reroute LRP — destination CIDR for CIDR-only destinations, plus protocol/port for L4-filtered destinations — so multiple EgressIPs on the same pod select the correct SNAT IP. A filtered (matched) SNAT row and an unfiltered default row can both structurally apply to the same packet from the same pod on the same gateway router (same `logical_ip`); disambiguation between them is **not** a number this OKEP picks — it is how OVN northd computes the actual logical-flow priority for a NAT row, confirmed against `lrouter_nat_get_priority()` in `northd/northd.c` (feeds `S_ROUTER_OUT_SNAT` flow priority directly):
- If `NAT.match` is **non-empty** (any filtered row), the real flow priority is `300 + NAT.Priority` (`NAT_PRIORITY_MATCH_OFFSET = 300`).
- If `NAT.match` is **empty** (the default/unfiltered row, same as every pre-this-OKEP EgressIP SNAT row), `NAT.Priority` is **not consulted at all** — the real flow priority is instead `prefix_len(logical_ip) + 1` (longer/more specific CIDR wins), optionally `+128` in certain distributed-gateway-port cases.

So for filtered rows, `NAT.Priority` genuinely matters and this OKEP sets it explicitly (100, consistent with the LRP scheme, though any single explicit value works equally well here since filtered rows' `match` clauses are already guaranteed non-overlapping by `TrafficConflict`). For the **default** row, setting `NAT.Priority` would be a no-op — northd never reads it for a match-less row, so this OKEP does not set it there and does not claim a specific value matters. Filtered-over-default precedence holds not because of any number comparison, but because a match-having row's flow priority (≥ 300ish) structurally exceeds any match-less row's prefix-length-derived priority (at most ~33 for an IPv4 pod `/32`, ~129–257 for IPv6 with the `+128` distributed case) — the `+300` offset always dominates:

```text
# CIDR-only trafficSelector (match non-empty, NAT.Priority=100 -> real flow priority 400):
snat    172.18.0.200    10.244.1.3    match="ip4.dst == $<dest-nets-addr-set>"    priority=100

# L4-filtered trafficSelector (example, match non-empty, NAT.Priority=100 -> real flow priority 400):
snat    172.18.0.201    10.244.1.3    match="ip4.dst == 192.168.250.0/24 && udp && udp.dst == 5060"    priority=100
snat    172.18.0.202    10.244.1.3    match="ip4.dst == 192.168.250.0/24 && udp && udp.src == 16384..32767"    priority=100

# EgressIP without a trafficSelector field set (default, no dest match; NAT.Priority is
# unset/ignored here — real flow priority comes from logical_ip's /32 prefix length instead):
snat    172.18.0.100    10.244.1.3
```

**EgressIP coexistence**: Multiple EgressIPs can serve the same pod when at least one has `trafficSelector`. `trafficSelector` LRPs use priority 100; every non-`trafficSelector` EgressIP uses 99 (standalone or as a Story 2 default), so dest/L4 matches are evaluated first. Existing catch-all LRPs at 100 are rewritten to 99 on upgrade (see Backwards Compatibility).

| EgressIP | TrafficSelector | Interface | LRP Priority | IP Rule Priority | Role |
|----------|----------------|-----------|-------------|-----------------|------|
| eip-mgmt | yes | secondary host | 100 | 6000 (per-dst) | Handles traffic to 192.168.250.0/24 |
| eip-signaling | yes | secondary host | 100 | 6000 (per-dst+L4) | Handles UDP 5060 to 192.168.250.0/24 |
| eip-default | no | OVN network | 99 | n/a (GR SNAT) | Handles all other traffic |

**Reconciliation**: `EgressTrafficClass` changes trigger re-reconciliation of all EgressIPs with matching `trafficSelector`. When only `destinations` change (labels unchanged), the set of matching EgressIPs is unchanged too, so a single reconcile with the new object is enough. When labels change, the update handler reconciles the **union** of EgressIPs matching the old labels and EgressIPs matching the new labels — old-labels-only matches lose this class's destinations (cleanup), new-labels-only matches gain them (install). Processing only one side of a label change would miss one of those two directions entirely: looking only at old labels on a non-matching→matching transition finds no affected EgressIP and never installs the destinations; looking only at new labels on a matching→non-matching transition never finds the EgressIP that needs cleanup. TrafficSelector changes on an existing EgressIP (CASE 3.0) trigger a full teardown and rebuild of assignments.

#### ovnkube-node changes

**Per-destination IP rules with L4 filtering**: Instead of a single catch-all IP rule per pod, one IP rule per destination (and per `protocols` entry when L4 is set) is created at priority 6000. When protocol/port fields are set, the IP rule includes `IPProto`, `Dport`, and `Sport` fields:

```text
# CIDR-only destination:
6000: from 10.244.1.3 to 192.168.250.0/24 lookup 1026

# L4-filtered destinations:
6000: from 10.244.1.3 to 192.168.250.0/24 ipproto tcp dport 5060 lookup 1026
6000: from 10.244.1.3 to 192.168.250.0/24 ipproto udp dport 5060 lookup 1026
6000: from 10.244.1.3 to 192.168.250.0/24 ipproto udp dport 8000-8010 lookup 1026
6000: from 10.244.1.3 to 192.168.250.0/24 ipproto udp sport 16384-32767 lookup 1026
```

Non-`trafficSelector` catch-all rules on a **secondary host** EgressIP use priority 6001 so destination-specific rules are evaluated first:

```text
6001: from 10.244.1.3 lookup 1028
```

Do not combine this catch-all with a `trafficSelector` EgressIP on the OVN network for the same pod (Non-Goal). A default EgressIP on the OVN network does not install a host catch-all IP rule.

**Destination-specific routing table**: The default route is replaced with destination-specific routes, preserving the gateway from the original default route. If a specific route for the destination CIDR already exists on the interface (e.g., configured by the network administrator), it is preserved with its gateway rather than replaced. Non-matching traffic falls through to the main routing table:

```text
# With trafficSelector:
192.168.250.0/24 via 192.168.150.1 dev eth-mgmt
192.168.150.0/24 dev eth-mgmt proto kernel scope link

# EgressIP without a trafficSelector field set (standard, pre-existing route):
default via 192.168.150.1 dev eth-mgmt
192.168.150.0/24 dev eth-mgmt proto kernel scope link
```

Routes remain L3-only — L4 filtering is at the IP rule level.

**IP rule comparison**: The `ruleKey()` function extends `netlink.Rule.String()` with `IPProto`, `Dport`, and `Sport` fields for use as set keys. Without this, rules differing only by protocol/port would appear identical to the rule manager and repair path, causing deduplication issues.

**Traffic-matched iptables SNAT**: For secondary host interface EgressIPs with `trafficSelector`, each iptables SNAT rule must be scoped to the same traffic as the corresponding IP rule / destination. Matching only on source IP and output interface is insufficient when two EgressIPs select the same pod and the same egress interface: each EgressIP has its own address, so two rules of the form `-s <podIP> -o <iface> -j SNAT --to-source <egressIP>` both match all pod traffic out that interface and the first rule wins for every destination.

```text
# Incorrect (ambiguous when multiple EIPs share pod + iface):
-s 10.244.1.3/32 -o eth-mgmt -j SNAT --to-source 192.168.150.101
-s 10.244.1.3/32 -o eth-mgmt -j SNAT --to-source 192.168.150.102

# Correct — CIDR-only destination:
-s 10.244.1.3/32 -d 192.168.250.0/24 -o eth-mgmt -j SNAT --to-source 192.168.150.101
-s 10.244.1.3/32 -d 192.168.251.0/24 -o eth-mgmt -j SNAT --to-source 192.168.150.102

# Correct — L4-filtered destination (protocol/port match the Destination protocols entry):
-s 10.244.1.3/32 -d 192.168.250.0/24 -p udp --dport 5060 -o eth-mgmt -j SNAT --to-source 192.168.150.101
-s 10.244.1.3/32 -d 192.168.250.0/24 -p udp --sport 16384:32767 -o eth-mgmt -j SNAT --to-source 192.168.150.103
```

IP rules still decide which routing table (and thus which interface) is used; destination/L4-conditioned SNAT decides which EgressIP address is applied once the packet egresses that interface. Non-`trafficSelector` EgressIPs keep the existing `-s <podIP> -o <iface>` SNAT form.

#### EgressIP failover with `trafficSelector`

A plain EgressIP's failover is O(1): one LRP + one NAT torn down on the old node and recreated on the new node (secondary-host case: one IP rule + default route + one unscoped SNAT rule). A `trafficSelector` EgressIP's failover is **O(N)** in the number of destinations (up to 25 per `EgressTrafficClass`, times however many `protocols` entries split a destination by L4):

- **OVN side**: the destination-filtered reroute LRP's nexthop and (for OVN-network EgressIPs) each dest/L4-scoped NAT rule must be updated to point at the new node's gateway router.
- **Node side** (secondary-host EgressIPs): every per-destination IP rule, every route in the per-destination routing table, and every dest/L4-scoped iptables SNAT rule must be deleted on the old node and recreated on the new node — N of each, not one.

The failover window is proportionally longer than plain EgressIP. On the node side, while a destination's IP rule/route/SNAT triplet has been deleted on the old node and not yet recreated on the new node, that destination is **not** matched by any rule — the kernel policy-routing lookup falls through to the main routing table, same mechanism (and same "not a blackhole" contract) as the non-matching-traffic case described earlier, not a packet drop: traffic to that destination briefly uses the node IP instead of the EgressIP. On the OVN side, the transient state during the reroute LRP nexthop update and NAT rule recreation is less precisely characterized here — it depends on operation ordering within the batched NB transaction — and should be verified empirically once implemented rather than asserted. Mitigation: ovnkube-controller issues OVN NB changes as a single batched transaction rather than N sequential operations; ovnkube-node should do the same for IP rules/routes/iptables via the existing rule/route manager batching rather than N sequential netlink/iptables calls. This OKEP does not change the per-destination design to avoid this cost (that would mean one shared construct per EgressIP instead of per destination, losing the per-destination teardown granularity this feature needs); it is tracked as a scale/latency risk below, to be measured once implemented.

**EgressTrafficClass informer**: ovnkube-node watches `EgressTrafficClass` resources and re-queues affected EgressIPs, using the same old∪new-labels reconciliation as ovnkube-controller above — a `destinations`-only change re-queues using the new object alone (set of matching EgressIPs is unchanged); a label change re-queues EgressIPs matching either the old or the new labels, so both the "started matching" and "stopped matching" directions are handled.

#### Differences between LGW and SGW modes

The feature works identically in both local and shared gateway modes. The destination-filtered LRP match, address sets, NAT rules, and node-side constructs are gateway-mode-agnostic.

**Interaction with no-overlay/BGP UDN outbound SNAT ([OKEP-5259](okep-5259-no-overlay.md))**: when `outboundSNAT` is enabled for a no-overlay/BGP UDN, the UDN controller programs its own fallback SNAT row on the same router — `logical_ip` the pod **subnet** (e.g. a `/24`), `external_ip` the node or masquerade IP, `match=""` (empty), destination condition expressed via `exempted_ext_ips` (local gateway) or `allowed_ext_ips` (shared gateway) instead of `match` (OKEP-5259 examples print this row's NB `priority` column as `0`, but — per the mechanism verified above — that printed value is a red herring: `match` is empty on this row too, so northd never consults it there either). Confirmed directly against `northd/northd.c` (`lrouter_nat_get_priority()`):
- This OKEP's **filtered** EgressIP rows (`match` non-empty) get real flow priority `300 + NAT.Priority` (≥ 300ish), which structurally outranks any match-less row regardless of that row's `logical_ip` prefix. They win over the no-overlay fallback the same way they win over this OKEP's own default row — the `+300` match offset, not a priority-number comparison with whatever the fallback row's `priority` field happens to print as.
- This OKEP's **default** EgressIP row (`match` empty, `logical_ip` the pod's specific `/32` or `/128`) and the no-overlay fallback row (`match` empty, `logical_ip` the whole pod **subnet**) are both match-less, so both get the *same* `prefix_len(logical_ip) + 1` treatment northd uses to disambiguate match-less rows — and the default row's pod-specific, longer prefix (e.g. `/32` → priority 33) is strictly more specific than the fallback's subnet-level, shorter prefix (e.g. `/24` → priority 25), so the default EgressIP row wins, consistent with "the more specific destination wins" and with how OVN-Kubernetes already documents this exact longest-prefix-wins behavior elsewhere (see the pre-existing `AddHostCIDRPolicy` comment referenced in Backwards Compatibility).

This holds in both gateway modes (local gateway's `exempted_ext_ips`+`ct-commit-all` and shared gateway's `allowed_ext_ips` variants of the fallback row are both `match`-empty per OKEP-5259, so the same reasoning applies to both) and only matters when `outboundSNAT` is enabled for a no-overlay/BGP UDN; most UDN deployments do not have this fallback row at all.

### Testing Details

#### Unit Tests

**Destination parsing** (`pkg/util/egressip/destination_test.go`):
- `ParseDestinations`: CIDR-only, TCP/UDP/SCTP with `destinationPort.number`, `destinationPort.range`, `sourcePort.number`, `sourcePort.range`, combined dest+src, invalid CIDR, more than one protocol key, empty protocols, multiple destinations
- `ParseDestinations`, version-skew simulation: a `ProtocolMatch` decoded with TCP/UDP/SCTP all nil (simulating an older binary's type decoding an unknown future protocol field) is skipped — not treated as unfiltered — and does not fail the rest of the `EgressTrafficClass`
- `TrafficSelectorResolved`, all-unsupported-class case: `trafficSelector` matches an `EgressTrafficClass` whose every destination has an unsupported `protocols` entry — asserts `status: False`/`reason: AllDestinationsUnsupported`, distinct from both `DestinationsFound` (which would wrongly claim routing) and `NoMatchingEgressTrafficClass` (which would wrongly imply no class matched)
- `UniqueCIDRs`: deduplication across destinations with different L4 filters
- `SplitByL4`: separation of CIDR-only vs L4-filtered destinations
- Port overlap helpers used by `TrafficConflict`
- `HasL4Filter` and `ProtocolName` helpers

**ovnkube-controller tests** (`pkg/ovn/egressip_test.go`):
- `hasTrafficSelector`: nil vs set (empty `{}` is rejected at admission)
- Feature gate disabled with `trafficSelector` set: no LRP/NAT programmed (not treated as nil/catch-all), `TrafficSelectorResolved=False`/`reason: FeatureGateDisabled`
- `egressStatuses.containsAny`: empty, matching, non-matching
- `buildL4MatchClauses`: single dest port, dest port range, source port, source port range, combined dest+src, multiple `protocols` entries, IP family filtering, mixed CIDR-only and L4
- `buildPortMatch`: `number` and `range` notation for dest and source
- LRP match parsing: standard match, destination-filtered match, too-short match, IPv6
- NAT match: IPv4/IPv6 with and without destination match
- NAT priority: filtered (`trafficSelector`, `match` non-empty) rows set `Priority: 100`; the default (no-`trafficSelector`, `match` empty) row does not set `Priority` (northd never consults it for a match-less row — verified against `lrouter_nat_get_priority()` in `northd/northd.c`). Filtered rows win over the default row via northd's `300 + NAT.Priority` match-offset, not via a 100-vs-99 number comparison
- NAT priority vs. no-overlay/BGP UDN outbound SNAT: this OKEP's default EgressIP row (`match` empty, pod-specific `/32` `logical_ip`) outranks the no-overlay fallback row (`match` empty, pod-subnet `logical_ip`, per OKEP-5259) via northd's longest-prefix-wins rule for match-less rows, not via any `Priority` value comparison; this OKEP's filtered rows outrank both via the `+300` match offset
- EgressIP reroute priority: non-trafficSelector uses 99, trafficSelector uses 100
- `TrafficConflict`: CIDR-only vs CIDR-only, CIDR-only vs L4, different protocols on the same CIDR (no conflict), same protocol with disjoint explicit destination ports (no conflict), a dest-port-only entry vs a source-port-only entry of the same protocol (conflict — the unset side is a wildcard, there is no disjoint case between these two shapes), overlapping explicit dest or source ports (conflict); conflicting EgressIP left unprogrammed (no LRP/NAT) while the already-active one is unaffected
- Stable tiebreaker: given two conflicting EgressIPs, the winner (older `metadata.creationTimestamp`, `metadata.name` on a tie) is the same regardless of the order the two are processed in — re-running the conflict check with the inputs reversed, or after a simulated restart/resync, does not flip which EgressIP is programmed

**ovnkube-node tests** (`pkg/node/controllers/egressip/egressip_test.go`):
- TrafficSelector EgressIP with single destination
- TrafficSelector EgressIP with multiple destinations (per-destination dest-scoped SNAT)
- Two TrafficSelector EgressIPs on the same secondary interface for the same pod: each SNAT rule includes distinct `-d` (and L4) matches and the correct `--to-source`
- Non-TrafficSelector EgressIP with lower priority catch-all rule and unscoped `-s/-o` SNAT
- `ruleKey`: IPProto/Dport/Sport inclusion, differentiation of same-CIDR rules by protocol/port
- `replaceDefaultRouteWithDestNetworks`: default route removal, gateway preservation, existing specific route preservation, IPv4/IPv6 filtering, empty destinations
- `hasRouteForDst`: matching CIDR, different CIDR, same IP different mask, empty routes
- `containsRoute`: matching, non-matching, empty
- `getRoutesForOtherEIPsOnLink`: co-located routes, empty when alone

#### E2E Tests

Fifteen E2E test cases: thirteen tagged `[secondary-host-eip] [traffic-selector]` (cases 1–13); case 14, tagged `[udn] [local-gateway]`, is a standalone priority-migration regression test unrelated to `trafficSelector`; case 15, tagged `[traffic-selector]` only (no secondary-host EgressIP involved), directly tests `trafficSelector` NAT-priority behavior on the OVN network (see below for both):

1. **Matching and non-matching traffic**: Verifies matching destination traffic uses EgressIP, non-matching uses node IP, and EgressTrafficClass deletion causes traffic to that destination to fall through to the node IP (same mechanism as non-matching traffic — the destination network in this test only accepts the EgressIP's source IP, so falling through to the node IP breaks connectivity to it specifically, not a general connectivity failure).
2. **Multiple TrafficSelector EgressIPs**: Two secondary networks, two EgressTrafficClass CRs, two EgressIPs; verifies each destination uses the correct EgressIP.
3. **Same secondary interface, two EgressIPs**: Two EgressIPs on the same host NIC for the same pod with different destinations; verifies each destination is SNATed to the correct EgressIP (not the first-matching unscoped rule).
4. **Coexistence**: TrafficSelector EgressIP on secondary host network (priority 100) + non-TrafficSelector default EgressIP on OVN network (priority 99); verifies both work independently.
5. **EgressTrafficClass destinations update**: Changes destinations on a live EgressTrafficClass, verifies traffic transitions from non-matching to matching.
6. **EgressTrafficClass label change, both directions**: (a) Changes labels so a previously non-matching EgressTrafficClass starts matching the trafficSelector, verifies its destinations are installed (not missed by only looking at the old, non-matching labels). (b) Changes labels so a previously matching EgressTrafficClass stops matching, verifies its destinations are cleaned up (not missed by only looking at the new, non-matching labels).
7. **TrafficSelector added to existing EgressIP**: Starts with standard EgressIP, adds trafficSelector, verifies transition from catch-all to destination-filtered routing.
8. **L4 filtering — matching dest port**: TCP traffic to matching destination port uses EgressIP.
9. **L4 filtering — non-matching dest port**: Traffic to same CIDR on non-matching destination port does NOT use EgressIP.
10. **L4 filtering — mixed destinations**: CIDR-only + L4-filtered destinations in same EgressTrafficClass; CIDR-only routes all traffic.
11. **L4 filtering — dest port range**: `destinationPort.range` covering the external container's port.
12. **L4 filtering — source port range**: UDP from a bound local port in `sourcePort.range` uses EgressIP; traffic from a different local port to the same CIDR does not.
13. **`trafficSelector` destination overlapping a Service IP**: A `trafficSelector` EgressIP with a destination CIDR covering a ClusterIP and a LoadBalancer/ExternalIP service in the cluster; verifies Service traffic is unaffected (still load-balanced to a backend pod, not rerouted via the EgressIP) while other traffic in the same CIDR still uses the EgressIP.
14. **L2 UDN local-gateway without transit router, catch-all EgressIP + host-CIDR policy** (tagged `[udn] [local-gateway]`, not `[traffic-selector]` — a regression test for the priority migration in Backwards Compatibility, not a feature test): On an L2 primary UDN in local-gateway mode with no transit router, a pod selected by a catch-all (non-`trafficSelector`) EgressIP sends traffic to another node's primary/host IP (the `UDNHostCIDRPolicyPriority` match); verifies the EgressIP reroute wins deterministically, consistent with today's pre-OKEP behavior for every EgressIP on this topology.
15. **Filtered + default SNAT precedence, both on OVN network** (tagged `[traffic-selector]`, not `[secondary-host-eip]` — both EgressIPs are on the OVN network, no secondary-host EgressIP is involved): A `trafficSelector` EgressIP and a non-`trafficSelector` default EgressIP both assigned to OVN-network interfaces for the same pod — unlike case 4, both program NAT rows on the *same* gateway router, so this exercises filtered-vs-default NAT disambiguation directly. Verifies traffic to the `trafficSelector`'s destinations is SNATed to the filtered EgressIP's address (the `match`+`Priority: 100` row), and all other traffic is SNATed to the default EgressIP's address (the match-less row, which wins by virtue of being match-less, not by any `Priority` value) — an explicit assertion on the selected SNAT IP for both paths, not just that both "work."

These fifteen tests are **planned**, not yet implemented or run — this OKEP has no implementation or CI evidence yet. Once implemented, all fifteen are expected to pass on both local and shared gateway modes, and to skip when the EgressTrafficClass feature gate is not enabled.

#### Cross-feature interaction testing

`trafficSelector` EgressIP reroute LRPs take over priority 100 (`EgressIPReroutePriority`), the same numeric priority all EgressIP reroutes use today; non-`trafficSelector` EgressIPs move to 99 (see Backwards Compatibility). Neither change affects where EgressIP reroute sits relative to other features in the OVN pipeline — EgressFirewall/NetworkPolicy ACLs and Service load-balancing are at the logical switch stage regardless of router-level LRP priority, and EgressService (101) is above both 100 and 99 either way — so existing precedence is unchanged:

- **EgressFirewall / NetworkPolicy egress**: Both are ACLs evaluated at the logical switch stage, before the packet reaches the logical router's EgressIP reroute LRP. A deny ACL drops the packet before any EgressIP reroute (`trafficSelector` or not) is evaluated — deny always wins, same as today.
- **Services (ClusterIP)**: ClusterIP traffic is resolved via OVN load-balancer DNAT at the switch stage, also before the router-level reroute LRP. A `trafficSelector` destination CIDR overlapping the service CIDR is a no-op for real ClusterIPs — only unbacked addresses in that range would ever reach the reroute LRP.
- **EgressService**: Its reroute LRP priority (`EgressSVCReroutePriority` = 101) is above both `trafficSelector` (100) and default (99) EgressIP reroute priorities, so EgressService already takes precedence; this OKEP does not change that ordering.
- **LoadBalancer / ExternalIP hairpin**: Precedence for a pod egressing toward a Service's external/LoadBalancer IP is covered by E2E case 13 above rather than asserted here, since the exact pipeline stage for that path needs empirical verification rather than a documentation-only claim.

#### Scale Considerations

`trafficSelector` matches `EgressTrafficClass` resources by label selector, and one EgressIP can match more than one `EgressTrafficClass`. Each class is capped at 25 destinations (`MaxItems=25`), but **there is no cap on how many classes one EgressIP can match**, so the resolved destination count per EgressIP is `25 × (number of classes matched)`, not 25. There is also **no cap on `Protocols` entries per destination** — unlike `Destinations`, `Protocols` has no `MaxItems`, so a single destination can fan out into an arbitrary number of L4-filtered rules/NAT entries (the failover section above already accounts for this per-destination fanout; this table did not). Admission-time rejection of either over-broad total isn't possible for the same reason overlapping destinations aren't rejected at admission (see **Overlapping destinations** in API Details, above): at the time either the `EgressIP` or the `EgressTrafficClass` is admitted, the other side of the match isn't known yet. This OKEP leaves both uncapped and documents them as an accepted scale risk (see Risks) rather than adding a `Protocols` `MaxItems` or reconcile-time enforcement.

Scaling dimensions, with formulas rather than measured numbers, applied to a representative deployment (50 nodes, 500 pods, 20 `trafficSelector` EgressIPs, 5 `EgressTrafficClass` @ 25 destinations each, assuming 1 `protocols` entry per destination as a baseline — the real number is unbounded per destination):

| Dimension | Formula | At the representative scale |
|---|---|---|
| OVN address sets | 1 per `trafficSelector` EgressIP with ≥1 CIDR-only destination | ≤ 20 |
| Resolved destinations per EgressIP | 25 × (classes matched by that EgressIP's selector) | up to 125 if one EgressIP matches all 5 classes |
| Rules/NAT entries per destination | 1 if CIDR-only, else `Protocols` entries for that destination (unbounded) | ≥ 1, no upper bound |
| `EgressTrafficClass` update fan-out | re-reconciles EgressIPs whose `trafficSelector` currently matches it — O(matching EgressIPs), not O(all EgressIPs in the cluster) | ≤ 20 |
| IP rules/node (secondary-host case) | resolved destinations × rules-per-destination × pods assigned to that EgressIP on that node | ≥ 125 per pod at baseline, unbounded with multiple `protocols` entries per destination |

One real, cited OVN limitation applies to the address-set dimension: `ovn-controller` does not do incremental processing when multiple address sets share members, causing full OVS flow recompute on member churn ([OCPBUGS-31385](https://issues.redhat.com/browse/OCPBUGS-31385), documented for AdminNetworkPolicy peer address sets). This OKEP's address sets are lower-churn than typical NetworkPolicy/ANP peer address sets — membership changes only on `EgressTrafficClass` spec edits by an admin, not on every pod add/delete — so the churn-triggered recompute risk is present but less frequent than for pod-selector-backed address sets.

What is **not** asserted here without benchmarking: actual `ovn-controller`/northd recompute time or kernel policy-routing lookup cost at these counts. That needs measuring once implemented, not guessing.

### Documentation Details

Feature documentation will be added at `docs/features/cluster-egress-controls/egress-traffic-class.md` covering:
- Workflow with YAML examples (basic, advanced, L4 filtering)
- Implementation details with traffic flow diagrams
- ovnkube-controller and ovnkube-node construct descriptions
- Troubleshooting guide
- Best practices, design notes, known limitations

## Risks, Known Limitations and Mitigations

- **Overlapping traffic**: If two EgressIPs with different `trafficSelector` match overlapping traffic for the same pod (intersecting CIDRs and intersecting L4, or a CIDR-only destination vs any intersecting CIDR), the one with the newer `metadata.creationTimestamp` (name as tiebreaker) is left unprogrammed (fail closed) rather than installed alongside the other with undefined OVN tie-break order — a stable rule based on immutable object metadata, not informer/reconciliation processing order, so the winner cannot flip across a controller restart or failover. Mitigation: the `TrafficConflict` status condition identifies the unprogrammed EgressIP and the conflicting peer; operators resolve by adjusting destinations or protocols. Disjoint L4 on the same CIDR is not treated as a conflict.
- **UDN secondary host interface**: Not in this OKEP (see Future Goals). Dest-scoped host IP rules and iptables SNAT are additive to today's unscoped secondary-host EgressIP programming and should not block a later UDN/VRF or Uplink-backed secondary EgressIP design.
- **Port range limits**: Port ranges in OVN match expressions use range syntax (`tcp.dst == 5060..5062`).
- **Unscoped secondary-host SNAT**: Matching iptables SNAT only on `-s <podIP> -o <iface>` is incorrect when multiple `trafficSelector` EgressIPs share a pod and secondary host interface. Mitigation: scope each SNAT rule to the destination’s CIDR and L4 fields (see **Traffic-matched iptables SNAT** above). Non-`trafficSelector` EgressIPs retain the existing unscoped form.
- **Catch-all on secondary host + `trafficSelector` on OVN**: Unsupported in this OKEP (see Non-Goals). The catch-all `6001: from <podIP>` IP rule on the assigned node intercepts packets that arrived via the transit switch / management port before gateway-router SNAT for the OVN EgressIP. The supported Story 2 mix is the reverse: `trafficSelector` on secondary host, default on OVN (no host catch-all IP rule).
- **Unbounded resolved destinations, and unbounded `Protocols` per destination**: `trafficSelector` can match multiple `EgressTrafficClass` resources, each capped at 25 destinations, but there is no cap on the number of classes matched; separately, `Protocols` itself has no `MaxItems`, so a single destination can fan out into an arbitrary number of rules/NAT entries. Both multiply the OVN/node constructs scaling with them (see **Scale Considerations**) and are uncapped. Admission can't reject either (neither object knows the other side of the match at admission time, same as destination overlap). Accepted as a scale risk rather than adding reconcile-time enforcement or a `Protocols` `MaxItems`; operators should keep `trafficSelector` scoped to a small, deliberate set of classes, and keep `protocols` entries per destination small.
- **L2 UDN local-gateway without transit router**: In that topology only, EgressIP reroute LRPs are programmed on the gateway router instead of the cluster/transit router, the same router `UDNHostCIDRPolicyPriority` uses. `UDNHostCIDRPolicyPriority` moves from 99 to 98 (see **Upgrade Sequence** above) so both catch-all (99) and `trafficSelector` (100) EgressIP reroute LRPs stay above it, preserving the existing invariant that EgressIP wins over the host-CIDR policy uniformly for every EgressIP variant. L2 with a transit router does not program that host-CIDR LRP at all, so no exception is needed there.
- **Failover scales with destination count**: Unlike plain EgressIP's O(1) failover, a `trafficSelector` EgressIP's failover is O(N) in destinations (see **EgressIP failover with `trafficSelector`** above) — N IP rule/route/NAT deletions on the old node plus N creations on the new node, instead of one of each. The failover window is proportionally longer; on the node side, each destination falls through to the main routing table (node IP) individually until its rule is recreated on the new node — not a blackhole, same contract as non-matching traffic — while the OVN-side transient behavior during the window needs empirical verification. Mitigation: batch OVN NB transactions and node-side rule/route manager operations rather than issuing N sequential calls; actual failover latency at the 25-destination cap should be measured once implemented.

## OVN-Kubernetes Version Skew

This feature is proposed for introduction in v1.5.0 (the PR's current milestone). The `EgressTrafficClass` CRD and `trafficSelector` field are additive. Existing EgressIP objects without `trafficSelector` keep the same SNAT/reroute behavior; their cluster-router (or L2-no-transit GR) reroute LRPs move from priority 100 to 99.

## Backwards Compatibility

- The `trafficSelector` field on `EgressIP` is optional (pointer, nil by default). Omitting it is still catch-all EgressIP for that object.
- The `EgressTrafficClass` CRD is new and has no impact on existing resources.

### Upgrade Sequence

**OVN NB (ovnkube-controller), LRP priority 100 → 99**: Today `EgressIPReroutePriority` is 100 for every EgressIP reroute LRP. This OKEP keeps 100 for `trafficSelector` LRPs and moves all other EgressIP reroute LRPs to 99, so dest/L4 matches win. On upgrade, ovnkube-controller rewrites existing EIP reroute LRPs from 100 to 99; repair treats both priorities as EIP during the transition: find leftover 100, recreate at 99, delete stale 100. Relative to EgressService (101), no-reroute (102), and EIP QoS (103), catch-all EIP is still lower, so those interactions do not change.

**`UDNHostCIDRPolicyPriority` 99 → 98, for L2 UDN local-gateway without transit router**: in every other topology, EgressIP reroute LRPs are programmed on the cluster or transit router, and `UDNHostCIDRPolicyPriority` is either not programmed at all or lives on a different router, so there is no overlap. In this one topology, `getTopologyScopedLocalZoneRouterName` resolves to the **gateway router** for EgressIP reroute too — the same router `AddHostCIDRPolicy` programs `UDNHostCIDRPolicyPriority` on — so catch-all EgressIP (99) would otherwise collide with it (undefined OVN match ordering at equal priority). `UDNHostCIDRPolicyPriority` is a single global constant consumed only by `AddHostCIDRPolicy`, itself only invoked in this one topology, so lowering it to 98 needs no topology-aware conditional in the EgressIP controller: both catch-all (99) and `trafficSelector` (100) EgressIP reroute LRPs stay above it, preserving the existing invariant that EgressIP wins over the host-CIDR policy uniformly for every EgressIP variant, matching today's pre-OKEP behavior where `EgressIPReroutePriority` is uniformly 100 and already wins over `UDNHostCIDRPolicyPriority` (99). On upgrade, the existing priority-99 host-CIDR LRP on that GR is rewritten to 98, the same repair pattern as the other priority migrations in this section.

**ovnkube-node, IP rule priority 6000 → 6001**: Today `pkg/node/controllers/egressip/egressip.go` has a single hardcoded `rulePriority = 6000` constant, and `repairNode` lists/reconciles IP rules with `filterRuleByPriority(rulePriority)` — it only ever looks at priority 6000, there is no existing dual-priority scheme. This OKEP adds per-destination rules at 6000 and moves the non-`trafficSelector` catch-all rule to 6001, so `repairNode`'s filter must change to scan **both** 6000 and 6001, not just 6000, or it will treat the other priority's rules as unmanaged and never clean them up.

Walking through the mixed-version window and rollback:

1. **Rolling upgrade, node assignment unrelated to `trafficSelector`**: EgressIP-to-node assignment (cluster-manager) does not depend on `trafficSelector`, so a `trafficSelector` EgressIP can be assigned to, or already resident on, a node whose ovnkube-node has not yet been upgraded. The old binary's generated `EgressIPSpec` Go type has no `TrafficSelector` field, so the JSON field is silently dropped on decode (standard `encoding/json` behavior) — old code never sees it and treats the EgressIP exactly like a plain catch-all one, installing its single rule at (old) priority 6000 for the whole pod. Result: during the rollout window, that pod's traffic is **over-permissive** (everything routed via the EgressIP, not just the intended destinations) on nodes not yet upgraded — not broken, but not yet scoped either. This self-corrects once that node's ovnkube-node is upgraded and reconciles.
2. **Rolling upgrade, ovnkube-controller version skew (OVN NB side)**: the same Go-type decode-skew applies symmetrically to ovnkube-controller, which owns OVN NB LRP/NAT programming — a zone whose ovnkube-controller hasn't been upgraded yet also has an `EgressIPSpec` type with no `TrafficSelector` field, decodes a `trafficSelector` EgressIP as plain catch-all, and programs a catch-all reroute LRP and catch-all NAT for it at that old binary's own (pre-this-OKEP) `EgressIPReroutePriority`. This is the OVN-side equivalent of point 1's over-permissive window, not fail-closed — fail-closed isn't achievable for an already-running old binary, since it would require logic (recognizing and rejecting an unknown field) that the old code was never built with. It self-corrects the same way: once that zone's ovnkube-controller is upgraded, it re-reads the object with `TrafficSelector` present and reconciles to the correct destination-filtered LRP/NAT. There is no code-enforceable ordering between upgrading ovnkube-controller and ovnkube-node — each component's skew window self-corrects independently on its own upgrade; operators should upgrade both promptly to minimize the combined window, but neither has to go first.
3. **New code on the node, repair of old 6000-only state**: new `repairNode` scans both 6000 and 6001, finds the single old-style 6000 catch-all rule, and replaces it with the correct 6001 catch-all plus any 6000 per-destination rules.
4. **Rollback**: if the node is downgraded back to old code, old `repairNode`'s filter only matches priority 6000 — it cannot see 6001 rules at all, so any leftover 6001 catch-all rule from the new code is invisible to it and is **not** cleaned up. Old code then recreates its own catch-all rule at 6000 (functionally correct for old code's purposes), leaving the stale 6001 rule dangling until the node is upgraded again and new code's repair removes it. This is a harmless but real leftover-state gap worth noting, not a correctness break — both rules point traffic out the same interface.

## Alternatives

1. **Per-pod secondary network attachments**: Instead of destination-based routing, each pod could attach to multiple networks directly. This requires application-level awareness and significantly complicates pod networking.
2. **Adding SNAT to EgressFirewall**: EgressFirewall today only has `Allow`/`Deny` ACL rules with optional port matching — no source-IP-rewrite concept. Giving it one would mean adding a new rule action type (and its own destination-match struct, effectively duplicating this OKEP's `EgressTrafficClass`) to a CRD whose job today is traffic filtering, not traffic steering, plus bolting on EgressIP's node assignment and HA semantics that EgressFirewall has none of. Rejected as architecturally equivalent to this OKEP's design but grafted onto the wrong feature.
3. **Destination CIDRs inline on EgressIP**: Instead of a separate `EgressTrafficClass` CRD, destinations could be specified directly on the `EgressIP` spec. This was rejected because it doesn't allow reuse of destination definitions across multiple EgressIPs and makes the EgressIP spec overly complex.

## References

- [EgressIP documentation](../features/cluster-egress-controls/egress-ip.md)
- [GitHub Issue #6227](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6227)
- [NPEP-187: More protocols support](https://network-policy-api.sigs.k8s.io/npeps/npep-187-ports-and-protocols/)
- [OKEP-6019: VRF-Lite with Shared Gateway Mode using Uplinks](okep-6019-vrf-lite-shared-gateway-external-bridges.md)
- [kubernetes#134224: CEL `isCIDR` accepts unmasked values](https://github.com/kubernetes/kubernetes/issues/134224)
