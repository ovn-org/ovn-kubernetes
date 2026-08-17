# OKEP-6815: Add NetworkQoS support for ipamless localnet networks

* Issue: [#6815](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6815)

> **Status of this document.** This revision intentionally scopes the problem
> space only: motivation, use cases, requirements, goals, future goals, and
> non-goals. The `Proposed Solution`, `Implementation Details`, `Testing
> Details`, `Risks`, `Version Skew`, `Backwards Compatibility`, and
> `Alternatives` sections are placeholders to be filled in a subsequent
> iteration once the community aligns on the problem framing below.

## Problem Statement

NetworkQoS ([OKEP-4380](okep-4380-network-qos.md)) delivers DSCP marking and
bandwidth policing by building OVN QoS match expressions from pod IP addresses
read from the `k8s.ovn.org/pod-networks` annotation.

On secondary **localnet** networks with IPAM disabled (**ipamless**) - the
topology commonly used by KubeVirt / OpenShift Virtualization VMs whose IP
addresses are managed statically or by an external DHCP server - pods have no
OVN-Kubernetes-managed IPs, so `getPodAddresses()` returns nothing and the
controller silently skips them. As a result NetworkQoS is non-functional on
these networks, and there is today no supported way to apply network QoS
(workload-tier prioritization, backup-traffic deprioritization, or bandwidth
capping) to VMs running on them.

The root cause is that NetworkQoS matches the **source** of a packet by IP
address. That IP dependency is what breaks on ipamless networks. It is also, on
every network, the one part of source matching that a guest can influence
(source-IP spoofing, backstopped only by port security). The fix chosen here
removes the IP dependency from source matching entirely - and does so uniformly,
not just where it is strictly forced.

Ipamless **layer2** networks share the same underlying limitation, but this
proposal is scoped to **localnet**; see [Non-Goals](#non-goals) and
[Future Goals](#future-goals) for how layer2 is treated.

## Goals

- Make NetworkQoS functional on secondary ipamless **localnet** networks.
- Support, on ipamless networks, the QoS capabilities the target use cases
  require, **without requiring OVN-Kubernetes to discover or manage pod IP
  addresses**:
  - source pod/VM selection by label (`podSelector`);
  - DSCP marking;
  - bandwidth policing (`rate` / `burst`);
  - protocol + port classification (TCP/UDP/SCTP + port);
  - destination matching by IP range (`ipBlock` / CIDR);
  - priority ordering across and within NetworkQoS objects.

## Non-Goals

- **Ipamless layer2 topology, as a committed deliverable.** This proposal
  targets localnet. The enabling mechanism is expected to apply to ipamless
  layer2 as well; **if layer2 support comes essentially for free** (little or no
  additional work beyond the localnet implementation), it will be folded into
  this effort opportunistically. **If layer2 requires meaningful additional
  work**, it will be pursued as a **separate enhancement** rather than expanding
  this one (see [Future Goals](#future-goals)).
- **Destination selection by `podSelector` / `namespaceSelector` on ipamless
  networks.** Destination matching on ipamless networks is limited to
  `ipBlock` (IP / CIDR). The target use cases in this OKEP do not require
  selecting *destination* pods by label (see Requirements and User-Stories).
  Supporting labeled-pod destinations on ipamless networks is a
  [Future Goal](#future-goals): it depends on resolving destination pod
  identity, which under OVN Interconnect is topology-dependent (and, on
  localnet specifically, not reliably resolvable across nodes).
- **NetworkQoS on layer3 ipamless topology.** Layer3 uses per-node logical
  switches connected by a logical router; layer3 also exists primarily for
  routed multi-subnet deployments, which implies IPAM.
- **IP discovery or reporting for ipamless networks** (DHCP snooping, KubeVirt
  VMI status watching, ARP/NDP learning, IP-claim CRDs, or similar). This OKEP
  does not add any mechanism for OVN-Kubernetes to learn the guest's IP.
- **Changes to the NetworkQoS CRD API / schema.** Users create the same
  `NetworkQoS` resources with the same fields; only controller-internal
  behavior changes.
- **Ingress-direction QoS and traffic shaping.** NetworkQoS is egress-only and
  *polices* (drops excess) rather than *shapes* (queues) - an existing product
  constraint, not introduced here.
- **Minimum bandwidth guarantees / reservations.** Not supported by OVN.
- **Arbitration against other node traffic (host/OCP, CSI, other networks).**
  NetworkQoS governs only the selected localnet UDN's own egress. It does not
  prioritize that traffic over - or protect it from - other traffic sharing the
  node's uplink. The two mechanisms it offers are: **throttling** the UDN's
  traffic (bandwidth policing, a rate cap on that traffic alone), and **DSCP
  marking**, which has no local forwarding effect and is only acted upon by the
  physical fabric. Neither is a cross-class scheduler, and there is no
  minimum-bandwidth guarantee relative to other traffic.

## Future Goals

- **Ipamless layer2 support**, if it is not delivered opportunistically as part
  of this proposal. Should enabling layer2 require more than trivial additional
  work beyond the localnet implementation, it will be tracked and delivered as a
  separate enhancement that reuses this work.
- **Destination `podSelector` / `namespaceSelector` on ipamless networks** -
  "apply QoS to traffic *toward* a labeled set of pods." This is the principal
  capability deferred by this OKEP. It becomes relevant only if a future
  requirement needs to select destination pods by label rather than by address
  range. Because destination matching is built from IP address sets (not port
  identity), delivering it comes down to getting the pod IP into the
  `k8s.ovn.org/pod-networks` annotation so the existing IP-based path applies -
  e.g. KubeVirt static-IP propagation, or the DHCP IPAM path of
  [OKEP-6224](okep-6224-dhcp-ipam-localnet.md).
- **Convergence with DHCP IPAM ([OKEP-6224](okep-6224-dhcp-ipam-localnet.md)).**
  DHCP-mode networks have subnets configured (for the DHCP pool), so
  `DoesNetworkRequireIPAM()` returns true and they already follow the standard
  IP-based path. A future optimization could unify the ipamless and DHCP paths.

## Introduction

### How NetworkQoS works, and its IP dependency

The NetworkQoS CRD (`k8s.ovn.org/v1alpha1`) is namespace-scoped and defines:

- **`podSelector`** - selects source pods by label (empty = all pods in the
  namespace).
- **`networkSelectors`** - restricts the rule to specific networks (default,
  UDN, NAD).
- **`priority`** (0–100) - resolves conflicts when multiple NetworkQoS objects
  match the same packet; higher value wins.
- **`egress`** (ordered list, up to 20 rules) - each rule specifies `dscp`
  (0–63), an optional `bandwidth` (`rate` in kbps, `burst` in kilobits;
  policing, not shaping), and an optional `classifier` matching by destination
  (`ipBlock`, `podSelector`, `namespaceSelector`) and/or by `ports` (protocol +
  port). Later rules in the list take higher precedence.

### Why ipamless secondary localnet is the target topology

Enterprises migrating VM workloads to OpenShift Virtualization need the same
network QoS controls they had on traditional hypervisors (e.g. VMware ESX):
differentiating priority between workload tiers, between application and
infrastructure traffic, and capping bandwidth for specific traffic classes.

These VMs commonly attach to **secondary ipamless localnet UDNs** - the guest
OS or an external DHCP server manages addressing, not OVN-Kubernetes.

A natural question is why these workloads do not simply adopt DHCP IPAM
([OKEP-6224](okep-6224-dhcp-ipam-localnet.md)), which would place them on the
standard IP-based path (where destination `podSelector` also works). The target
population is precisely the set that cannot: VMs with truly static, externally
managed addresses - for these, no OVN-managed IP ever exists, so an
IP-independent matching path is the only option. Workloads that *can* use
DHCP IPAM should prefer it (see [Future Goals](#future-goals) on convergence).

This proposal targets localnet; ipamless layer2 networks share the same
limitation and may benefit from the same fix, but layer2 is not a committed
deliverable here (see [Non-Goals](#non-goals) and
[Future Goals](#future-goals)).

On localnet there is **no Geneve tunnel**: traffic egresses directly onto the
physical network, so a DSCP value stamped on the IP header is immediately
visible to the fabric with no inner/outer-header concerns. This is favorable
for fabric-level prioritization - provided the physical network is configured
to honor DSCP and map it to hardware queues.

The diagram below shows the egress traffic path for a KubeVirt VM on a secondary
ipamless localnet UDN, and where each QoS action is enforced. Everything left of
the enforcement boundary is under OVN-Kubernetes' control on the *sending* node;
everything right of it is the physical fabric, which OVN-Kubernetes never sees
again (the basis for the source-only matching argument in the next section).

```text
           sending node (OVN-Kubernetes control)          │  physical fabric
                                                          │  (out of OVN-K control)
 ┌───────────┐   ┌───────────────────────────────────────┐│
 │ KubeVirt  │   │        secondary localnet UDN         ││
 │   VM      │   │  (ipamless: MAC only, no OVN-managed  ││
 │ (source   │ ─>│   pod IP; label-selected source)      ││
 │  pod,     │   │                                       ││
 │  label:   │   │   logical switch port ── zone-local   ││
 │  tier=..) │   │   on the sending node, so the source  ││
 └───────────┘   │   is always identifiable (no pod IP)  ││
                 └───────────────┬───────────────────────┘│
                                 ▼                        │
                 ┌───────────────────────────────────────┐│
                 │        OVN/OVS QoS (to-lport)         ││
                 │  • source matching   (by port/label)  ││
                 │  • dst matching      (ipBlock/CIDR)   ││
                 │  • bandwidth policing (rate/burst)    ││
                 │  • DSCP marking      (stamp IP hdr)   ││
                 └───────────────┬───────────────────────┘│
                                 ▼                        │
                 ┌───────────────────────────────────────┐│      ┌─────────────┐
                 │   OVS bridge → physical NIC           ││      │ QoS-aware   │
                 │   (localnet: NO Geneve tunnel;        ││----▶ │ switches    │
                 │    DSCP on outer IP hdr, fabric-      ││ DSCP │ honor DSCP, │
                 │    visible immediately)               ││ on   │ map to HW   │
                 └───────────────────────────────────────┘│ wire │ queues      │
                                                          │      └─────────────┘
                 <------- enforcement boundary ---------->│
```

Because there is no tunnel, the DSCP value stamped by OVN/OVS QoS travels on the
same IP header the fabric reads - no inner/outer-header translation is needed for
the marking to reach QoS-aware physical switches.

### Scope of destination matching in this proposal

Destination matching in this OKEP is limited to **IP range (`ipBlock` / CIDR)**.
Source selection remains fully label-driven; classification by protocol/port is
unchanged; DSCP, bandwidth, and priority are unchanged. Labeled-pod destination
selection on ipamless networks is deferred to [Future Goals](#future-goals).

This is a deliberate scoping decision, not merely an implementation shortcut.
The reasons it is the right initial scope:

1. **On localnet, source is the only thing we can reliably match and enforce
   on.** OVN-Kubernetes enforces QoS at the sending node's OVN/OVS datapath. On
   localnet there is no tunnel and no cluster-managed path to the destination:
   the moment a packet egresses the node onto the physical fabric it leaves
   OVN-Kubernetes' control and visibility entirely - it can be routed, NAT'd,
   re-marked, or dropped, and OVN-Kubernetes will never see it again. The source
   pod, by contrast, is right there on the local switch and always identifiable.
   Destination pod *identity* is out of reach on ipamless networks for the same
   root reason the source would be without this proposal's enabling work: there
   is no OVN-managed IP in the pod annotation. (Destination matching is built
   from an IP address set - `ip4.dst == $dest_as`, evaluated against the packet
   header - so wherever pod IPs exist it already works cluster-wide regardless
   of OVN Interconnect zone; it is not gated on port identity or zone-locality.
   The blocker on ipamless networks is purely the missing IP, not topology.) On
   top of that, much QoS-relevant traffic terminates off-cluster (external
   services, appliances, hosts on the segment) where there is no destination pod
   to select at all. `ipBlock`, in contrast, is evaluated locally against the packet
   header before egress - no destination-side cooperation or resolution
   required - so it is not a lossy substitute for a working feature; it is the
   honest, complete story for what can be matched correctly today.

2. **The use cases are source-side; the destination refinements are address
   ranges.** Workload-tier differentiation (production vs. staging), per-class
   bandwidth capping, and DSCP marking for fabric prioritization are all decided
   by *who is sending* (selected by label). Destination matching appears only as
   a secondary refinement (e.g. "deprioritize traffic *to* the backup subnet"),
   and in every such case the destination is stable, fixed-address
   infrastructure - backup servers, storage/NFS targets, monitoring collectors.
   That is the canonical `ipBlock` case ("traffic to `10.20.0.0/16`"), not a
   churning label-selected pod set. Fabric-level enforcement reinforces this:
   once a packet is marked, the physical fabric queues it by that marking
   regardless of destination.

3. **It aligns with an already-accepted platform constraint.** MultiNetworkPolicy
   already restricts ipamless networks to `ipBlock` peers, and `ipBlock` is a
   long-established, widely-used selector in Kubernetes NetworkPolicy. Scoping
   destination matching to IP/CIDR follows an existing platform norm rather than
   introducing a novel limitation.

4. **There is a clear escape hatch, so the door is not permanently closed.** A
   user who genuinely needs label-based destination selection can get pod IPs
   into the `k8s.ovn.org/pod-networks` annotation - via DHCP IPAM
   ([OKEP-6224](okep-6224-dhcp-ipam-localnet.md)) or static-IP propagation -
   which moves the network onto the standard IP-based path where destination
   `podSelector` already works. This capability is deferred
   ([Future Goals](#future-goals)), not foreclosed.

## User-Stories/Use-Cases

### Definition of personas

- **Cluster admin** - creates and manages secondary networks and cluster-wide
  policies.
- **Namespace admin** - deploys workloads and manages per-namespace QoS
  policies within a namespace.
- **VM operator** - runs virtual machines via KubeVirt on secondary networks.

### Story 1: Workload-tier differentiation (production over staging)

**As a** cluster admin,
**I want** production VM workloads to receive higher network priority than
staging workloads, even when both share the same ipamless localnet subnet,
**so that** production traffic is forwarded preferentially by the physical
fabric.

**Example:** Production and staging VMs run on the same secondary localnet UDN,
labeled `tier: production` and `tier: staging`. The admin creates two
`NetworkQoS` objects selecting each tier by `podSelector`, marking production
with a high-priority DSCP class (e.g. EF / 46) and staging with a lower class
(e.g. AF11 / 10), and uses `priority` to resolve overlaps. This is entirely a
**source-side** decision. Without ipamless support these objects are silently
ignored.

### Story 2: Intra-VM backup-traffic deprioritization

**As a** namespace admin,
**I want** in-guest backup traffic from a VM to be marked at lower priority and
optionally rate-capped, relative to the VM's regular application traffic,
**so that** bulk backup transfers do not degrade application performance on the
shared NIC.

**Example:** VMs run an in-guest backup agent that sends bulk traffic to a
backup server on a known port and/or a stable address range. Within a single
`NetworkQoS` object, an earlier egress rule marks general traffic at a high
DSCP, and a later, higher-precedence rule classifies backup traffic (by
protocol/port and/or the backup server's `ipBlock`) with a low DSCP and a
bandwidth cap. Because later rules take higher precedence, the specific backup
rule overrides the general high-DSCP rule for backup packets. The backup
destination is stable infrastructure - the canonical `ipBlock` case - not a
churning set of label-selected pods.

### Story 3: Bandwidth capping per workload class

**As a** namespace admin,
**I want** to cap egress bandwidth for a specific class of VMs (selected by
label) on an ipamless localnet network,
**so that** a batch class cannot exceed a fixed egress rate and monopolize the
shared NIC, leaving headroom for latency-sensitive workloads - even when IP
addresses are managed outside Kubernetes.

**Example:** Batch-processing VMs (`workload: batch`) are capped via a
`NetworkQoS` `bandwidth` rate, leaving the remaining capacity to
latency-sensitive VMs. This is source- and/or port-scoped, not
destination-pod-scoped.

### Story 4: DSCP marking for fabric-level prioritization on tunnel-free localnet

**As a** cluster admin,
**I want** egress traffic from selected VMs on an ipamless localnet network to
carry a DSCP marking,
**so that** QoS-aware physical switches place the traffic in the appropriate
hardware queue - without OVN-Kubernetes needing to know the VMs' IP addresses.

**Example:** VMs labeled `priority: gold` have egress traffic marked DSCP 46.
Because localnet has no tunnel, the marking is visible on the physical
interface directly (verifiable with `tcpdump` on the OVS bridge port). This
`tcpdump` check verifies only that OVN-Kubernetes applied the mark; whether the
fabric honors it (queues accordingly, or re-marks/clears it at a trust boundary)
is a separate, out-of-scope prerequisite not validated by this OKEP.

## Requirements

Derived from the user stories above and the driving user requests tracked in
the upstream enhancement issue
([#6815](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6815)):

- The solution must work on **secondary OVN localnet networks with
statically managed IPs (ipamless)**. Ipamless **layer2** is not a committed
target: it will be reused opportunistically if the localnet mechanism applies
with little or no additional work, otherwise pursued as a separate
enhancement.
- Destination matching on ipamless networks must support **IP range
(`ipBlock` / CIDR)** classification. Destination selection by `podSelector` /
`namespaceSelector` is deliberately out of scope for this proposal (see
[Non-Goals](#non-goals) and [Future Goals](#future-goals)), because `ipBlock`
is sufficient for the target use cases.

## Proposed Solution

*To be detailed in a subsequent iteration.*

### API Details

*To be detailed in a subsequent iteration. No NetworkQoS CRD API changes are
anticipated (R7).*

### Implementation Details

*To be detailed in a subsequent iteration.*

### Testing Details

*To be detailed in a subsequent iteration.*

### Documentation Details

*To be detailed in a subsequent iteration. Must include adding this OKEP to
`mkdocs.yml` under the OKEPs navigation section.*

## Risks, Known Limitations and Mitigations

*To be detailed in a subsequent iteration.*

## OVN-Kubernetes Version Skew

*To be detailed in a subsequent iteration.*

## Backwards Compatibility

*To be detailed in a subsequent iteration. The intended direction is strictly
additive: NetworkQoS on ipamless networks is non-functional today, so enabling
it does not change any existing behavior; IPAM-enabled networks are unaffected.*

## Alternatives

*To be detailed in a subsequent iteration.* The comparison must evaluate the
candidate IP-independent matching approaches against an IP-in-annotation
approach, and must call out the **loss of destination `podSelector` /
`namespaceSelector` matching** as a key drawback of the IP-independent
approaches (they can express "QoS toward `10.20.0.0/16`" but not "QoS toward
pods labeled `role: backup-target`" without manual CIDR bookkeeping).

## Deferred / Open Questions

- **Discovery mechanism for unsupported/degraded configs on ipamless networks**

  For better user experience, a `NetworkQoS` config relying on a capability
  unavailable on an ipamless network (notably destination `podSelector` /
  `namespaceSelector`) must not degrade silently - but deliberately does not fix
  *how* the user is told. This is an open design question for the Proposed
  Solution, with (at least) two options:

  1. **Emit a Kubernetes event per reconcile** - low-effort; events are not part
     of the `NetworkQoS` API surface, so this is arguably the cleaner option
     (controller-internal behavior). Downsides: events are ephemeral and
     noisy on a hot reconcile loop, and easy to miss after the fact.
  2. **Surface a condition on the `NetworkQoS` object's status** - persistent and
     queryable. Although the `NetworkQos` CRD struct already exposes
     `Status.Conditions` (no OpenAPI/schema diff needed), introducing a *new
     condition type* with defined semantics is an **additive API change** - an
     externally observable contract that must be documented and maintained.

## References

- [OKEP-4380: Network QoS Support](okep-4380-network-qos.md) - existing
  NetworkQoS implementation.
- [OKEP-6224: DHCP IPAM Support for Localnet Networks](okep-6224-dhcp-ipam-localnet.md)
  - DHCP-based IP delivery (related, not a dependency).
- [OVN NB Schema - QoS Table](https://www.ovn.org/support/dist-docs/ovn-nb.5.html)
  - QoS match expression syntax.
- Upstream tracking:
  [ovn-kubernetes#6815](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6815)
  - enhancement tracking issue for this OKEP, where the driving user requests
  and discussion are recorded.
