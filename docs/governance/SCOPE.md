# OVN-Kubernetes Project Scope

This document defines what OVN-Kubernetes is, what it is not, and provides a
rubric for evaluating whether an enhancement proposal (OKEP) is in scope.

The primary audience is OKEP authors, maintainers reviewing proposals, and AI
agents generating or triaging enhancement ideas.

## What OVN-Kubernetes Is

OVN-Kubernetes is the **full networking stack for Kubernetes clusters**, built
on top of [Open Virtual Network (OVN)](https://www.ovn.org/) and
[Open vSwitch (OVS)](https://www.openvswitch.org/) as its core data plane.

It is more than a CNI plugin. The CNI interface is the entry point, but the
real work happens in the controllers that translate Kubernetes API intent into
OVN logical constructs, OVS flows, Linux networking configuration, and
adjacent systems such as FRR (for BGP/EVPN).

### What that includes today

Examples, this list is not exhaustive.

| Domain | Examples |
|--------|---------|
| **Pod & VM networking** | Pod-to-pod connectivity, KubeVirt VM networking (pods under the hood) |
| **Service load balancing** | ClusterIP, NodePort, LoadBalancer, ExternalName via OVN LBs |
| **Network policy** | NetworkPolicy, AdminNetworkPolicy, EgressFirewall |
| **Egress control** | EgressIP, EgressService, EgressQoS, EgressGateway |
| **User-defined networks** | UDN, CUDN, multi-homing, secondary networks, multi-network policies |
| **Routing & overlay** | BGP (via FRR), EVPN, no-overlay, OVN Interconnect, Localnet |
| **Hardware acceleration** | DPU/SmartNIC offload, OVS hardware acceleration |
| **QoS & traffic shaping** | NetworkQoS, bandwidth reservation |
| **Multicast** | IGMP snooping and relay |
| **Hybrid environments** | Hybrid overlay (Windows/Linux), local & shared gateway modes |
| **Observability & tooling** | Metrics, tracing, and tooling that surfaces data-path state (e.g., MCP server) |
| **Scheduler integration** | Topology hints, DRA for network resources, capacity/oversubscription feedback |

> [!NOTE]
> This list is not exhaustive

### What that explicitly excludes

| Domain | Reason |
|--------|--------|
| **L7 / application-layer networking** | Service mesh, Gateway API HTTPRoute, gRPC load balancing, TLS termination — these require a userspace proxy in the data path, which is outside OVN-Kubernetes's scope |
| **eBPF-only datapaths that bypass OVS** | OVS is a core dependency; replacing it undermines the project's identity and hardware-offload story. Could be considered in-scope in future if OVS/OVN provide support for eBPF hook attachment |
| **Cilium/Calico CRD compatibility shims** | Upstream Kubernetes NetworkPolicy is the right place to standardise; compat shims belong in the originating project |
| **General-purpose monitoring infrastructure** | Prometheus exporters or dashboards with no OVN-K8s-OVS-OVN-specific data belong elsewhere |
| **K8s scheduler plugins** | OVN-Kubernetes is a *resource provider* to the scheduler, not a scheduler component |
| **Application-level concerns** | DNS policy, service discovery, pod identity — these are not networking-layer responsibilities |

## The Scope Rubric

When evaluating whether a proposal belongs in OVN-Kubernetes, apply the
following five filters **in order**. A proposal that fails any filter is out of
scope.

### Filter 1 — Data Path Test

> **Does this proposal affect how packets reach or leave a pod or VM?**

If a proposal does not touch the data path — either directly (programming OVN
logical constructs, OVS flows, Linux routes, FRR config) or indirectly (exposing
data-path state, reserving data-path resources) — it does not belong in
OVN-Kubernetes.

**Passing examples:**

- A BGP route advertisement feature that informs how pod CIDRs are reachable
- An MCP server that exposes OVN flow state for a specific pod
- Scheduler hints derived from OVN-observed node network oversubscription

**Failing examples:**

- A generic log aggregation tool with no OVN-specific data
- A K8s scheduler plugin that makes placement decisions unrelated to network state
- A UI dashboard that pulls data from a generic metrics endpoint

### Filter 2 — OVN-Native or Data-Plane-Bound Test

> **Can OVN/OVS (or Linux networking under OVN-Kubernetes control) implement this
> without a userspace proxy in the data path?**

OVN-Kubernetes operates at L4 and below. The moment a feature requires reading
above the transport header — HTTP headers, TLS handshakes, gRPC framing — it
needs a userspace proxy and is out of scope.

The practical test: *if you need Envoy, Istio, or any sidecar in the data path
to make this work, it is out of scope.*

**Passing examples:**

- Weighted load balancing implemented via OVN LB backends
- Session affinity using OVN connection tracking
- ECMP or DSR implemented via OVN router policy
- Health checking via BFD or OVS port probing

**Failing examples:**

- HTTP header-based routing (requires L7 proxy)
- TLS termination or SNI-based routing
- Circuit breaking / retry logic (application-layer semantics)

### Filter 3 — Community Benefit Test

> **Does this feature benefit more than one adopter, or serve any standard
> Kubernetes deployment?**

Features that are useful only in a single vendor's deployment model belong in
that vendor's downstream layer or fork. This applies even if the feature is
technically sound and gated behind a flag.

A proposal may be accepted for a single primary adopter **only if** it:

1. Is plausibly useful to other adopters within a reasonable timeframe, **and**
2. Is isolated behind a feature flag, **and**
3. The proposing team commits to long-term maintenance of that code path

When in doubt, ask: *"Would a cluster operator running vanilla Kubernetes on
bare metal find this useful?"*

### Filter 4 — Upstream Responsibility Test

> **If this belongs in OVN, OVS, FRR, or the Kubernetes API, does the proposal
> include a plan to contribute it there?**

OVN-Kubernetes should not be the permanent home for capabilities that belong in
its upstream dependencies. If a proposal implements something that is a gap in
OVN, OVS, FRR, or the Kubernetes API, it must include:

1. A reference to an upstream issue or contribution effort, **and**
2. A deprecation / removal plan for the shim once upstream ships it

Proposals that create indefinite shims with no upstream path are rejected.

**Exception:** If the upstream project has formally declined the feature, or if
the feature is genuinely Kubernetes-orchestration-specific and has no place
upstream, the shim may be permanent — but this must be explicitly stated and
justified.

### Filter 5 — Identity / Depth Test

> **Does this feature deepen OVN-Kubernetes, or merely attach to it?**

This is the hardest filter to apply mechanically, but the most important one
for preventing slow scope creep over time.

Ask: *"If OVN-Kubernetes were replaced by a different CNI implementation, could
this feature move with it — or does it only exist because OVN-Kubernetes happens
to be here?"*

- If the feature has no meaningful dependency on OVN's logical model, OVS's
  flow programming, or the specific data-path architecture of OVN-Kubernetes,
  it is *attaching* to the project, not deepening it. → **NACK**

- If the feature exploits OVN's logical topology, or fills a gap that only a
  CNI-layer implementation can fill, it is deepening the project. → **Proceed**

OKEP authors must include a **"Why OVN-Kubernetes?"** section that answers this
question directly. Acceptable answers:

- *"Only OVN's logical model allows us to implement X efficiently at scale"*
- *"This is a Kubernetes networking gap that must be filled at the CNI layer"*
- *"This integrates data-path state from OVN-Kubernetes with [scheduler / DRA /
  hardware] in a way that requires tight coupling to our control plane"*

Unacceptable answers:

- *"We're already in this repo, so it made sense to add it here"*
- *"It's networking-adjacent"*
- *"Another CNI does this"*

## Feature Matrix Requirement

Every OKEP must address (or explicitly defer, with justification) its behaviour
across the following compatibility matrix:

| Dimension | Options |
|-----------|---------|
| **Gateway mode** | Local gateway (lgw), Shared gateway (sgw) |
| **IP family** | IPv4, IPv6, Dual-stack |
| **Cluster topology** | Single-zone, OVN Interconnect multi-zone |
| **Network scope** | Default cluster network, User-Defined Networks (UDN/CUDN) |
| **Hardware acceleration** | Software OVS, DPU/SmartNIC offload |
| **Workload type** | Standard pods, KubeVirt VMs |

**Silent gaps are a NACK.** An OKEP that simply does not mention how a feature
behaves in IPv6 or Interconnect mode is not acceptable. An OKEP that explicitly
states *"DPU offload is not supported in v1 — tracked in issue #XXXX"* is fine.

## Quick Reference Card

```
Is it on the pod/VM data path (directly or as state/resource)?
  NO  → Out of scope

Does it require a userspace proxy (L7)?
  YES → Out of scope

Does it benefit more than one adopter (or any standard K8s cluster)?
  NO  → Out of scope (unless isolated, flag-gated, and proposer-maintained)

If it reimplements an upstream capability, does it have an upstream plan?
  NO  → Out of scope

Does it deepen OVN-Kubernetes (not just attach to it)?
  NO  → Out of scope

Does the OKEP cover the full feature matrix or explicitly document gaps?
  NO  → NACK (not out of scope, but incomplete)
```

## Worked Examples

| Proposal | Verdict | Reasoning |
|----------|---------|-----------|
| BGP route advertisements for pod CIDRs | ✅ In scope | Touches data path; FRR is glue for OVN-learned routes; community-wide |
| EVPN support | ✅ In scope | Data-path extension via FRR; deepens routing story |
| UDN membership via DRA | ✅ In scope | Network resource provisioning at pod scheduling time; tight CNI coupling |
| Topology / latency hints to scheduler | ✅ In scope | OVN-Kubernetes as resource provider; exposes data-path state upward |
| MCP server for OVN flow state | ✅ In scope | Surfaces data-path state; passes identity test |
| Localnet API | ✅ In scope | Data-path feature; OVN logical port attachment to physical networks |
| Gateway API HTTPRoute | ❌ Out of scope | L7; requires userspace proxy |
| Service mesh / sidecar injection | ❌ Out of scope | L7; application layer |
| Cilium NetworkPolicy CRD adapter | ❌ Out of scope | Upstream K8s NetworkPolicy is the standard; not our compat problem |
| eBPF-only NetworkPolicy (no OVS) | ❌ Out of scope | Bypasses OVS; undermines hardware-offload story |
| Generic Prometheus exporter | ❌ Out of scope | No OVN-specific data; does not deepen the project |
| General-purpose BGP daemon | ❌ Out of scope | Not wired to pod/VM data path |

## Where This Document Lives

This document lives at `docs/governance/SCOPE.md` and is referenced
from the OKEP template and the Contributing Guide.

Amendments to this document follow the same process as amendments to
`GOVERNANCE.md`: a pull request with at least two maintainer approvals.
