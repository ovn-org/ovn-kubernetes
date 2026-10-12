# OKEP-6641: CoreDNS Plugin for User Defined Networks

* Issue: [#6641](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6641)

## Problem Statement

The standard CoreDNS `kubernetes` plugin resolves only default-network pod IPs
(from `pod.Status.PodIP`). Pods on a User Defined Network (UDN) or Cluster
User Defined Network (CUDN) also carry UDN-scoped IPs in the
`k8s.ovn.org/pod-networks` annotation, but these are invisible to CoreDNS.
As a result, a pod on a primary UDN cannot resolve peer pods by their UDN IP,
so DNS-driven traffic is forced onto the default network instead of staying
within the UDN fabric.

A secondary problem is isolation: CoreDNS cannot distinguish DNS queries
originating from a UDN pod from those sent by a default-network pod, because
OVN-K SNATs all service-bound traffic from UDN pods to a per-node join-subnet
IP before it reaches CoreDNS. Without knowing the true source, the plugin
cannot enforce network-level DNS isolation.

## Goals

- Serve A / AAAA records for every UDN IP a pod carries (primary and
  secondary networks)
- Serve PTR records for UDN IPs in the `in-addr.arpa` / `ip6.arpa` reverse
  zones
- Serve hostname + subdomain records (`pod.spec.hostname` /
  `pod.spec.subdomain`) for UDN pods
- Implement server-side search-path completion (CoreDNS `autopath`) so that
  pods on a UDN can resolve short service names without knowing the full FQDN
  or requiring `resolv.conf` changes
- Include directly-connected peer networks (via `ClusterNetworkConnect`) in
  the autopath search order
- Enforce **network-isolation by default**: restrict DNS resolution to a pod's
  own network and its direct CNC peers, returning NXDOMAIN for cross-network
  queries. An opt-out directive (`disable-network-isolation`) is provided for
  environments that require fully transparent DNS
- Recover the pod's true UDN IP at CoreDNS despite OVN-K SNAT, using a TC
  BPF program that injects an EDNS0 Client Subnet (ECS) option into DNS
  queries before they leave the pod
- Define the service DNS format ready for a future implementation once UDN
  service support lands

## Future Goals

- Service IP DNS resolution (spec'd here, implementation blocked on the
  UDN service load-balancer work)

## Non-Goals

- Modifying pod `resolv.conf` at CNI time
- Upstreaming this plugin to the CoreDNS mainline repository

## Introduction

OVN-Kubernetes supports User Defined Networks (UDN), which allow workloads to
be placed on isolated L2/L3 networks separate from the cluster default network.
A pod on a primary UDN uses its UDN IP as its principal address; the default
network IP is either absent or locked to infrastructure use only. Cluster User
Defined Networks (CUDN) extend the same concept across multiple namespaces via
a label selector.

Networks can also be linked together using `ClusterNetworkConnect` (CNC),
which creates a shared transit subnet between selected UDNs / CUDNs and
enables pod-to-pod and service traffic across network boundaries.

Despite this rich networking model, DNS remains entirely default-network-aware.
The CoreDNS `kubernetes` plugin reads `pod.Status.PodIP` and Endpoints only.
It has no knowledge of the `k8s.ovn.org/pod-networks` annotation where OVN-K
records every per-network IP a pod holds. The result is that:

- `<udn-ip-dashed>.<namespace>.pod.cluster.local` returns `NXDOMAIN`
- Short-name service resolution from a UDN pod searches only
  `<namespace>.svc.cluster.local`, missing any UDN-scoped service zones
- Reverse (PTR) lookups for UDN IPs fail

This OKEP introduces an **external CoreDNS plugin** (`ovn-kubernetes`) that
sits ahead of the `kubernetes` plugin in the CoreDNS chain and fills all three
gaps, together with a TC BPF program embedded in `ovnkube-node` that solves
the SNAT-identity problem.

## User-Stories/Use-Cases

**Story 1: Pod-to-pod discovery within a UDN**

As a developer running microservices on a primary UDN, I want to query a peer
pod's UDN IP by its DNS name so that my service-mesh or health-check traffic
stays on the UDN fabric and never touches the default network.

**Story 2: Short-name service resolution from a UDN pod**

As a developer, I want to query `my-service` from inside my pod and have DNS
automatically search my UDN's service zone (`my-net.foo.svc.cluster.local`)
first, so that I get the UDN-scoped ClusterIP without writing a full FQDN in
my application config.

**Story 3: DNS across connected networks**

As a cluster administrator who has connected UDN `frontend-net` to CUDN
`backend-net` via `ClusterNetworkConnect`, I want pods on `frontend-net` to
resolve short service names from `backend-net` automatically so that
application developers do not need to hard-code cross-network FQDNs.

**Story 4: Reverse DNS for UDN IPs**

As an operator debugging connectivity, I want `dig -x <udn-ip>` to return the
pod's UDN-scoped DNS name so that I can identify the pod without cross-
referencing IP-to-pod mappings manually.

**Story 5: Per-UDN DNS isolation**

As a cluster administrator, I want DNS queries from pods on one UDN to be
unable to resolve names belonging to pods on a different, unconnected UDN, so
that network isolation is enforced at the DNS layer and not only at the data
plane.

## Proposed Solution

Two cooperating components:

### 1. CoreDNS plugin (`plugin/coredns-ovn-kubernetes/`)

An external CoreDNS plugin that:

1. Watches Pods, `UserDefinedNetwork`, `ClusterUserDefinedNetwork`, and
   `ClusterNetworkConnect` objects via shared informers
2. Builds an in-memory index of UDN IPs → pod, keyed from the
   `k8s.ovn.org/pod-networks` annotation
3. Answers A / AAAA queries for the UDN-scoped pod DNS name formats defined
   below, falling through to the `kubernetes` plugin for everything else
4. Answers PTR queries for IPs in UDN subnets
5. Implements the CoreDNS `AutoPather` interface so that the `autopath` plugin
   can return a UDN-aware search path based on the source IP of each query
6. Reads the EDNS0 Client Subnet (ECS) option injected by the BPF program
   (component 2) to learn the pod's real UDN IP, enabling per-UDN isolation
   even after OVN-K SNAT

The plugin lives at `plugin/coredns-ovn-kubernetes/` in this repository as a
top-level Go module, linked to `go-controller` via a `go.work` workspace file.

### 2. TC BPF ECS injector (`go-controller/pkg/node/ecs/`)

When `--enable-udn-edns` is set on `ovnkube-node`, a BPF program is compiled
into the binary and loaded once at startup. For each UDN pod created on the
node, the CNI path automatically attaches the program as a **TCX ingress**
hook on the host-side veth peer of that pod's primary UDN interface.

The hook fires on every outgoing DNS query (UDP destination port 53) before
OVN-K's join-subnet SNAT runs, when `ip->saddr` still holds the pod's real
UDN IP. It injects an EDNS0 Client Subnet option (RFC 7871, option code 8)
carrying that IP as a `/32` host prefix. CoreDNS reads the ECS option and uses
it as the authoritative source identity for isolation decisions.

The BPF program uses:
- `bpf_skb_change_tail` to extend the packet
- `bpf_skb_store_bytes` to write the ECS bytes (direct pointer writes do not
  survive the OVS/veth pipeline)
- `bpf_l4_csum_replace` with `size=2` for all checksum updates (using `size=4`
  inverts the one's-complement fold and breaks subsequent `size=2` calls)
- TCX multi-program attachment (`AttachTCXIngress`) rather than legacy `tc`
  qdisc ownership, avoiding conflicts with other programs on the same interface

### API Details

#### DNS name formats

UDN and CUDN records use distinct formats reflecting their respective scopes.

**Namespace-scoped UDN — pod IP:**
```
<dashed-ip>.<udn-name>.<namespace>.pod.cluster.local
```

**Cluster-scoped CUDN — pod IP:**
```
<dashed-ip>.<cudn-name>.pod.cluster.local
```

`<udn-name>` / `<cudn-name>` is the `.metadata.name` of the
`UserDefinedNetwork` or `ClusterUserDefinedNetwork` object. For UDNs, the
namespace is included in the label to ensure that two namespace-scoped UDNs
with the same name in different namespaces always produce distinct records,
even if their subnet CIDRs overlap.

The label ordering — `<udn-name>.<namespace>` rather than
`<namespace>.<udn-name>` — mirrors the existing Kubernetes DNS hierarchy where
more-general scopes sit closer to the zone apex:

```
Standard:  10-0-0-5    . default         . pod . cluster.local
UDN:       10-128-1-5  . my-net.default  . pod . cluster.local
CUDN:      10-201-2-7  . blue-net        . pod . cluster.local
```

**Hostname + subdomain (UDN):**
```
<hostname>.<subdomain>.<udn-name>.<namespace>.svc.cluster.local
```

**Hostname + subdomain (CUDN):**
```
<hostname>.<subdomain>.<cudn-name>.svc.cluster.local
```

**Service records (UDN) — spec only, implementation deferred:**
```
<service-name>.<udn-name>.<namespace>.svc.cluster.local
```

**Service records (CUDN) — spec only, implementation deferred:**
```
<service-name>.<cudn-name>.svc.cluster.local
```

**PTR records** point back to the scoped forward name:
```
5.1.128.10.in-addr.arpa.  PTR  10-128-1-5.my-net.foo.pod.cluster.local.
7.2.201.10.in-addr.arpa.  PTR  10-201-2-7.blue-net.pod.cluster.local.
```

#### Corefile configuration

The plugin is zero-configuration beyond zone specification; all data is
auto-discovered from the Kubernetes API.

```corefile
cluster.local {
    ovn-kubernetes
    kubernetes cluster.local in-addr.arpa ip6.arpa {
        pods insecure
        fallthrough in-addr.arpa ip6.arpa
        ttl 30
    }
    cache 30 {
        disable success cluster.local
        disable denial cluster.local
    }
    loop
    reload
    loadbalance
}

. {
    errors
    health { lameduck 5s }
    ready
    forward . /etc/resolv.conf { max_concurrent 1000 }
    cache 30
    reload
    loadbalance
}
```

`ovn-kubernetes` must appear before `kubernetes` in each stanza. The `ready`
endpoint gates traffic until the plugin's informer caches have synced;
the sync runs in the background so CoreDNS opens its listener immediately
and marks itself not-ready until the sync completes.

Network isolation is **on by default**. To opt out:

```corefile
cluster.local {
    ovn-kubernetes {
        disable-network-isolation
    }
    ...
}
```

#### `--enable-udn-edns` node flag

Setting this flag on `ovnkube-node` enables the BPF ECS injector. On startup,
`default_node_network_controller` loads the BPF objects once. When a UDN pod
is created via CNI, `helper_linux.go` automatically attaches the BPF program
to the host-side veth of that pod's primary UDN interface.

Enabling this flag is required for full per-UDN DNS isolation. Without it, DNS
isolation degrades to per-node granularity (all UDN pods on the same node are
indistinguishable at CoreDNS because OVN-K SNATs them to the same join-subnet
IP).

In `kind.sh`:
```bash
contrib/kind.sh -mne -nse -nce -uedns
```

#### Autopath search path

For a pod in namespace `foo` on primary UDN `my-net` (namespace-scoped) and
secondary CUDN `blue-net` (cluster-scoped), connected to CUDN `backend-net`
via `ClusterNetworkConnect`:

```
my-net.foo.svc.cluster.local    # primary UDN — scoped label
blue-net.svc.cluster.local      # secondary CUDN — cluster-scoped
backend-net.svc.cluster.local   # connected peer (direct, not transitive)
foo.svc.cluster.local           # standard Kubernetes namespace fallback
svc.cluster.local
cluster.local
""
```

Autopath peer expansion is **one hop only** — mirrors actual network
reachability.

### Implementation Status

| Phase | Description | Status |
|---|---|---|
| 1 | A / AAAA records for UDN pod IPs | ✅ Implemented |
| 2 | PTR records for UDN IPs | ✅ Implemented |
| 3 | Autopath / short-name resolution | ✅ Implemented |
| 4 | Hostname + subdomain records | ✅ Implemented |
| BPF | `--enable-udn-edns` TC BPF ECS injector | ✅ Implemented |
| 5 | Service records | ⏳ Blocked on UDN load-balancer OKEP |

### Testing Details

**Unit tests:**
- Fake `client-go` clientset with synthetic Pods, UDN, CUDN, CNC objects
- Table-driven tests for A, AAAA, PTR query types
- Autopath search-path construction covering primary, secondary, and
  connected-network cases
- Edge cases: UDN name > 63 characters (skip + warn), same-name UDNs in
  different namespaces with overlapping subnets, pod with no UDN annotation

**E2E tests on KIND (`contrib/kind.sh -mne -nse -nce -uedns`):**
- UDN pod resolves its own pod IP by UDN-scoped DNS name → `NOERROR`
- Default-network pod resolves UDN pod name → `NXDOMAIN` (isolation)
- `dig -x <udn-ip>` returns the scoped forward name (PTR)
- Short-name service resolution from a UDN pod resolves via autopath
- Pod with `hostname` + `subdomain` resolves via UDN hostname format

**Scale testing**: 1000 pods across 50 UDNs; measure CoreDNS memory overhead
and P99 query latency vs baseline (no plugin).

### Documentation Details

- New `docs/dns/udn-dns.md` page covering the DNS name formats, Corefile
  setup, `--enable-udn-edns`, and autopath behaviour, with worked examples
- Update `docs/user-defined-networks.md` to link to the DNS page
- `plugin/coredns-ovn-kubernetes/README.md` (plugin-level reference)
- This OKEP added to `mkdocs.yml` under the OKEPs nav section

## Risks, Known Limitations and Mitigations

**UDN names longer than 63 characters** — Kubernetes allows object names up to
253 characters; a single DNS label is capped at 63. The plugin skips and logs
a warning for any UDN/CUDN whose name exceeds 63 characters. Mitigation: add
`+kubebuilder:validation:MaxLength=63` to the UDN and CUDN CRD
`.metadata.name` field as a follow-up change.

**autopath source-IP race condition** — The existing `kubernetes` plugin
documents the same race: if a pod is deleted and its IP is immediately
re-assigned to a new pod in a different namespace/network before the informer
cache updates, autopath may return the wrong search path for one or two
queries. Mitigation: short cache TTLs (default 5 s); the window is bounded by
the informer resync period.

**No subnet overlap validation across namespace-scoped UDNs** — OVN-K does not
currently prevent `foo/my-net` and `bar/my-net` from sharing the same subnet
CIDR, which could produce pods with identical IPs on different networks.
Mitigation: the scoped label format (`<udn-name>.<namespace>`) ensures DNS
records are always distinct. PTR lookups remain unambiguous because the
plugin indexes pods by IP and full NAD key (`namespace/nad-name`).

**BPF requires kernel 5.8+ for `bpf_skb_change_tail` and kernel 6.6+ for TCX
links** — Both are available in all supported RHEL/Fedora/Ubuntu LTS releases
at the time of writing. On kernels older than 6.6, a `cls_bpf` fallback is
conceptually possible but not implemented; it carries risks (accidental ejection
of other programs sharing the same qdisc priority, exclusive qdisc ownership
conflicts with Cilium and similar). A safe fallback would need to create the
clsact qdisc only if absent and pick a known handle/priority that does not
conflict with other consumers — similar to the approach taken by NetObserv.

**ECS option stripped by intermediate resolvers** — EDNS0 ECS is widely
respected but could be stripped by a recursive resolver sitting between the pod
and CoreDNS. In practice, UDN pods reach CoreDNS directly (cluster-internal
DNS), so no intermediate resolver is involved.

## DNS Isolation

The plugin enforces **DNS-level isolation by default**, restricting resolution
to the querying pod's own network and its direct CNC peers. This mirrors the
actual data-plane topology and prevents IP enumeration across isolated networks.

### Source identity recovery via ECS

DNS queries from UDN pods are SNAT'd by OVN-K to the node's join-subnet IP
(e.g. `100.64.0.2`) before reaching CoreDNS. All UDN pods on the same node
appear identical. The BPF ECS injector solves this:

1. The `--enable-udn-edns` flag causes `ovnkube-node` to load a TC BPF
   program and attach it as **TCX ingress** on the host-side veth of each UDN
   pod's primary UDN interface at pod-creation time (via the CNI path)
2. The BPF program intercepts DNS queries before the SNAT runs, when
   `ip->saddr` is still the pod's real UDN IP (e.g. `10.200.1.4`)
3. It injects an EDNS0 Client Subnet option carrying that IP as a `/32` prefix
4. CoreDNS reads the ECS option in `extractSourceIP()` and uses the ECS
   address as the authoritative source identity for isolation decisions
5. The ECS option is stripped from the response before it is returned to the
   pod, so applications never see it

The plugin falls back to `state.W.RemoteAddr()` (the join-subnet IP) when no
ECS option is present, which gives per-node granularity rather than per-pod.

### Isolation rules

| Querying pod | Target network | Default result |
|---|---|---|
| Default network | Any UDN/CUDN | **NXDOMAIN** (blocked) |
| UDN A | UDN A (same network) | **NOERROR** |
| UDN A | UDN B (CNC-connected to A) | **NOERROR** (mirrors data-plane path) |
| UDN A | UDN C (no CNC to A) | **NXDOMAIN** |
| CUDN X | CUDN X (any namespace) | **NOERROR** (same cluster-scoped network) |
| CUDN X | UDN A (no CNC) | **NXDOMAIN** |

### Why NXDOMAIN rather than REFUSED

RFC 5452 and common resolver implementations handle REFUSED inconsistently:
some treat it as fatal, others as transient. NXDOMAIN is universally
understood as a definitive negative. The trade-off is that NXDOMAIN is
strictly a lie (the record exists), but operators who need accurate error
classification can detect blocked queries by correlating CoreDNS metrics or
logs.

### Why default-network pods are blocked

Although default-network pods cannot reach UDN pod IPs via the data plane
(OVN-K flows deny the traffic), they could otherwise use DNS to enumerate IPs
on isolated networks. Blocking them at the DNS layer prevents this information
leak.

### Why only direct CNC peers, not transitive

CNC explicitly models which networks can communicate. Allowing transitive peers
(A→B→C ⟹ A can resolve C) would be surprising and inconsistent with the
data-plane model, where A→C traffic is not permitted unless an explicit CNC
object selects both A and C.

## OVN-Kubernetes Version Skew

Target: the release cycle immediately following the UDN service load-balancer
work (lb-okep), so that Phase 5 (service records) can land in the same release
as the feature it depends on. Phases 1–4 and the BPF injector can land
independently in the preceding release.

## Backwards Compatibility

The plugin is purely additive:

- The `kubernetes` plugin's behaviour is unchanged; it remains in the chain
  and continues to serve all default-network records
- Clusters without any UDN/CUDN objects see no difference: the plugin falls
  through on every query
- The new DNS name formats do not conflict with any existing record served by
  the `kubernetes` plugin (UDN IPs are not in `pod.Status.PodIP`)
- Enabling the plugin requires a Corefile update and a custom CoreDNS build;
  neither change affects clusters that do not opt in
- `--enable-udn-edns` is opt-in and has no effect on default-network pods or
  on clusters without UDN configured
- BPF attachment failure is non-fatal: ovnkube-node logs a warning and
  continues; DNS still works, just without per-UDN isolation granularity

## Alternatives

**Extend the `kubernetes` plugin upstream** — Would require upstreaming
OVN-Kubernetes-specific annotation parsing into a project-agnostic plugin.
Rejected: high upstream coordination cost, tight coupling to OVN-K
implementation details.

**Per-UDN zone with a separate CoreDNS instance (sidecar)** — A second
CoreDNS deployment with stub-zone forwarding from the primary. Rejected:
doubles operational overhead, adds a network hop for every UDN DNS query, and
requires dynamic Corefile regeneration as UDNs are created/deleted.

**Modify pod `resolv.conf` at CNI time** — OVN-K CNI would inject UDN search
domains into the pod's `/etc/resolv.conf` at attach time. Rejected: OVN-K CNI
currently writes nothing to `resolv.conf`; adding this would be complex and
fragile (resolv.conf is bind-mounted at pod start by the kubelet).

**OVN-K masquerade IPs for DNS isolation** — OVN-K assigns per-UDN masquerade
IPs in `169.254.0.0/17` on the transit-router path. Using these for service
traffic (including DNS) would provide per-UDN identity at CoreDNS without BPF.
Rejected for this OKEP: requires a larger OVN-K data-plane change; the BPF
approach is self-contained and deployable today.

**BPF on pod-side veth (`ovn-udn1` TC egress, inside pod netns)** — Would
require entering the pod's network namespace at attach time. Rejected: the
host-side veth peer (`b049782ae4d8_N`) is visible in the host netns, requires
no netns entry, and TCX ingress on the host side fires at the same point in the
packet lifetime (before OVN-K SNAT) because the veth crossing is transparent.

**TC qdisc ownership instead of TCX** — Legacy `tc` filter attachment assumes
exclusive ownership of the clsact qdisc, which conflicts with other programs
(e.g. Cilium, OVN-K's own TC offload filters). TCX uses the kernel's
multi-program chain, composing correctly with any other TCX program on the
same interface.

## References

- [UDN design doc](../design/user-defined-network.md)
- [Connecting UDNs OKEP](okep-5224-connecting-udns/okep-5224-connecting-udns.md)
- [CoreDNS external plugin documentation](https://coredns.io/explugins/)
- [CoreDNS `kubernetes` plugin](https://github.com/coredns/coredns/tree/master/plugin/kubernetes)
- [CoreDNS `autopath` plugin](https://github.com/coredns/coredns/tree/master/plugin/autopath)
- [RFC 7871 — EDNS0 Client Subnet](https://datatracker.ietf.org/doc/html/rfc7871)
- lb-okep (UDN service load-balancer, prerequisite for Phase 5)
