# OKEP-6788: Per-node (C)UDN render state for cluster-manager consumers

* Issue: [#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)
* Related: [#6414](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414) — **(C)UDN only:** deprecate high-churn **`NodesSelected`** on dynamic UDN; other resources in that issue are out of scope here.

## Problem Statement

With **dynamic UDN allocation** enabled ([OKEP-5552](okep-5552-dynamic-udn-node-allocation.md)), a (C)UDN is rendered only on nodes where it is **active** (pods or EgressIPs present). Cluster-manager controllers—especially **route advertisements (RA)**—must know **which nodes have a network active and when OVN programming is complete** before advertising routes or tearing them down.

Today this is only partially visible on the primary `UserDefinedNetwork` / `ClusterUserDefinedNetwork` object:

* **`NodesSelected`** patches the primary CR whenever the active-node **count** changes. That creates **high churn** on a single object as pods move between nodes ([#6414](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414) motivation for the UDN line item). It also exposes only an **aggregate count**, not per-node **rendered** state.
* **Route advertisements** under dynamic allocation still infer readiness from **proxies** instead of render completion:
  * **Legacy layer-2:** tunnel-ID node annotations (allocation ≠ rendered; see TODO in `go-controller/pkg/clustermanager/routeadvertisements/controller.go`).
  * **Layer-2 transit-router:** network manager **active** on the node (`NodeHasNetwork`)—active ≠ rendered ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)).
  * **Layer-3:** host subnet **annotations** gate when prefixes are gathered; they do not report OVN programming complete on the node.

Cluster-manager controllers cannot consume Prometheus as a control-plane API. They need a **Kubernetes-API object** with clear per-node **active**, **rendered**, and **teardown** semantics.

## Goals

* Introduce a **sparse per-node status CRD** per **(network, node)** for **dynamic UDN only** (`EnableDynamicUDNAllocation`).
* Publish **`Active`** (network referenced on the node) and **`Rendered`** (OVN logical topology programmed on the node) conditions that other controllers can **watch/list**.
* **Deprecate** `NodesSelected` on the primary (C)UDN and stop patching it once the per-node status CRD is available.
* Let RA and future consumers stop relying on tunnel-ID annotations and aggregated `NodesSelected` counts.
* Wire cluster-manager consumers to the **active (network, node)** set the network manager already derives from pod and EgressIP placement (same membership that drives `NodesSelected` today), exposed per node instead of as a churny aggregate on the primary CR.

## Non-Goals

* **Static UDNs** — would scale as nodes × UDNs with little benefit over today's annotation-based signals.
* **Other [#6414](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414) resources** (EgressFirewall, ANP/BANP, EgressQoS, NetworkQoS, APBExternalRoute) — per-node status removal and operator visibility for those CRDs are not part of this OKEP.
* **Prometheus metrics or cluster-manager rollup** as the control-plane signal for render state (metrics are for operators; controllers need kapi).
* Replacing spec-driven or low-churn (C)UDN conditions on the primary object (`NetworkCreated`, `NetworkAllocationSucceeded` on UDN/CUDN; `TransportAccepted`, `UplinksReady` on **CUDN only**). Only **`NodesSelected`** (dynamic UDN, high churn) is deprecated here.

## Introduction

Dynamic UDN allocation ([OKEP-5552](okep-5552-dynamic-udn-node-allocation.md)) limits which nodes render a given UDN. The network manager already tracks when a network becomes **active** or **inactive** on a node (pods / EgressIPs). What is missing is a **durable, watchable per-node record** that also exposes **render completion** after `ovnkube-controller` programs OVN on that node.

One per-node status CRD design satisfies **both** [#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788) (render/teardown for controllers) and the **(C)UDN-only** [#6414](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414) goal of ending **`NodesSelected`** churn on the primary object:

| | Today: `NodesSelected` on primary (C)UDN | Target: `NetworkRenderState` (this OKEP) |
|---|---|---|
| **Who writes** | Cluster-manager patches **one** aggregate on the primary CR | Per-node `ovnkube-controller` owns **one object** per **(network, node)** |
| **Who reads** | Humans / coarse count; RA uses separate proxies | **Controllers** list/watch per-node **Active** and **Rendered** |
| **Cardinality** | Repeated patches on a **single** CR as active-node count changes | **Sparse** objects only for active (node, network) pairs |
| **Signal** | Active-node **count** only | Active + **rendered** lifecycle + teardown |
| **Relevant nodes** | Implicit in count | **Active** nodes for that network (pods / EgressIPs) |

The selected approach follows the **UplinkState** pattern ([PR #6555](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6555)): a cluster-scoped per-node status CRD named `<resource>.<node>` that the node component owns and cluster-manager watches.

### (C)UDN status on the primary object today

These conditions stay on the primary `UserDefinedNetwork` / `ClusterUserDefinedNetwork` and are **not** replaced by this OKEP:

| Condition | Scope | Typical writer | Node-churn etcd risk |
|-----------|-------|----------------|----------------------|
| `NetworkCreated` | UDN/CUDN | UDN controller | **None** on the happy path |
| `TransportAccepted` | **CUDN only** | UDN controller | **None** on routine node churn |
| `UplinksReady` | **CUDN only** | Uplink controller | **Low** — deduped success message |
| `NetworkAllocationSucceeded` | UDN/CUDN | `NetworkClusterController` | **Very low** — deduped; errors go to Events |
| `NodesSelected` | dynamic UDN only | `NetworkClusterController` | **High** — patches on active-node **count** change; **deprecated** by this OKEP |

## User-Stories/Use-Cases

Story 1: Advertise routes only when a network is rendered on a node

As a **cluster-manager route-advertisements controller**, I want a **Kubernetes-API object** per (network, node) that reports **active** and **rendered** state, **so that** I can advertise or withdraw routes without heuristics such as tunnel-ID annotations ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)).

Story 2: Observe per-node UDN placement without parsing primary CR churn

As a **platform engineer**, when dynamic UDN allocation moves workloads between nodes, I want **`kubectl get`** (or a controller watch) to show **which nodes** currently host a network and whether rendering finished, **so that** I do not rely on aggregated `NodesSelected` counts on the primary CR.

Story 3: Future controller consumers

As a **cluster-manager developer**, I want a **stable, typed per-node status API** for dynamic UDN render state, **so that** new features can gate behaviour on “network programmed on node N” without adding more patches to the primary (C)UDN.

## Proposed Solution

### High-level direction

1. Add a cluster-scoped **`NetworkRenderState`** CRD (name TBD during implementation) — one object per **(network, node)** when the network is active (or imminently active) on that node.
2. **Per-node `ovnkube-controller`** creates, updates, and deletes these objects across the active → rendered → teardown lifecycle.
3. **Cluster-manager consumers** (RA first) **list/watch** `NetworkRenderState` instead of `NodesSelected` and tunnel-ID annotations.
4. **Stop writing** `NodesSelected` on the primary (C)UDN once migration is complete.

**Scope:** **dynamic UDN only** (`EnableDynamicUDNAllocation`). Static UDNs do **not** get per-node status objects.

### API Details

#### CRD sketch (name/API finalized with [#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788))

| Field | Value |
|-------|--------|
| **Kind** | `NetworkRenderState` (proposed) |
| **API group** | `k8s.ovn.org/v1alpha1` (proposed) |
| **Scope** | Cluster |
| **Naming** | `<network>.<node>` (same pattern as `UplinkState`, e.g. `tenant-blue.worker-1`) |
| **Labels** | `k8s.ovn.org/network`, `k8s.ovn.org/node` for list/watch |

**Spec** (immutable identity):

```yaml
spec:
  networkName: tenant-blue   # internal network name
  nodeName: worker-1
```

**Status** (`status.conditions[]`):

| Condition | Meaning |
|-----------|---------|
| **`Active`** | Network is **active** on this node (pods or EgressIPs present)—replaces `NodesSelected` per-node semantics |
| **`Rendered`** | OVN logical topology for this network is **programmed** on this node ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)) |

Example (healthy):

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkRenderState
metadata:
  name: tenant-blue.worker-1
  labels:
    k8s.ovn.org/network: tenant-blue
    k8s.ovn.org/node: worker-1
spec:
  networkName: tenant-blue
  nodeName: worker-1
status:
  conditions:
    - type: Active
      status: "True"
      reason: NetworkReferenced
      message: Network is active on this node (pods or egress IPs present)
    - type: Rendered
      status: "True"
      reason: RenderSucceeded
      message: OVN logical network topology is programmed on this node
```

### Lifecycle

1. First pod / EgressIP on `(node, network)` → **create** `NetworkRenderState` (`Active=True`, `Rendered=False`).
2. Per-node `ovnkube-controller` finishes OVN programming → patch `Rendered=True` (RA may advertise).
3. **No pods and no EgressIPs** remain on `(node, network)` + grace period ([`--udn-deletion-grace-period`](okep-5552-dynamic-udn-node-allocation.md)) → teardown → `Rendered=False` → **delete** `NetworkRenderState` (do not delete while either reference type is still present).

### Active node membership

Dynamic UDN already tracks **which nodes are active** for a network (pod and EgressIP placement). That membership is what today drives **`NodesSelected`** patches on the primary (C)UDN and what RA approximates with tunnel-ID annotations for legacy layer-2.

For this OKEP:

* **Create** a `NetworkRenderState` when a network becomes active (or imminently active) on a node; **delete** it after teardown when neither pods nor EgressIPs reference the network on that node (lifecycle above).
* **Consumers** (RA first) derive the relevant **(network, node)** set from **list/watch** of `NetworkRenderState` (and labels), not from aggregated counts on the primary CR.
* **Implementation** wires existing network-manager active/inactive notifications to the per-node writer and to RA—reusing the same placement truth as `updateDynamicUDNStatus`, with per-node objects instead of patching the primary CR on every count change.

### Writers / readers

* **Writer:** `ovnkube-controller` on the node that renders the network owns status patches (`fieldManager` = node name).
* **Readers:** cluster-manager (route advertisements and future consumers) **watch/list** `NetworkRenderState`; stop using tunnel-id annotations and `NodesSelected` on the primary CR.

**Deprecation:** `NodesSelected` on the primary `UserDefinedNetwork` / `ClusterUserDefinedNetwork` is **deprecated** when `NetworkRenderState` ships; `updateDynamicUDNStatus` stops patching the primary CR. Per-node active membership is derived by listing objects (`kubectl get networkrenderstates -l k8s.ovn.org/network=…`).

**Cardinality:** bounded by **active (node, network) pairs**, not nodes × all UDNs. Objects are created only when a network is active (or imminently active) on a node; **garbage collection (GC)** on teardown and node/network delete.

### Implementation Details

| Area | Current pattern | Target |
|------|-----------------|--------|
| Active / render signal | `NodesSelected` + annotation heuristics (tunnel ID, subnets) | **`NetworkRenderState`** per (node, network); RA watches per-node status ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)) |

Implementation steps:

* Define CRD (`NetworkRenderState` or name agreed in [#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)): schema, RBAC, codegen, Helm/OLM manifests.
* **Per-node `ovnkube-controller`**: create/update/delete per-node status CRD objects on active/render/teardown transitions; own `Active` and `Rendered` conditions; align create/delete with pod **and** EgressIP presence on `(node, network)`.
* Wire **active-node membership** from network manager / dynamic UDN placement into the writer and RA (same source as today's `NodesSelected` logic, per-node API).
* Remove `updateDynamicUDNStatus` / `NodesSelected` patches on primary (C)UDN.
* **RA controller**: list/watch `NetworkRenderState`; require `Active=True` **and** `Rendered=True` before advertising on **all dynamic-UDN topologies** (layer-2 legacy, layer-2 transit-router, layer-3). Replace **readiness** proxies (tunnel-ID, active-only, “wait for subnet annotation” as a stand-in for rendered). Node subnet annotations may still supply **prefix data** for BGP after readiness is established.
* **GC**: delete objects on node delete, network delete, and after teardown grace.
* **E2E**: per-node status object count tracks active pairs; primary CR does not patch on pod placement churn.

*No difference between local gateway (lgw) and shared gateway (sgw)* for this control-plane API—the render signal is owned by `ovnkube-controller` on the node regardless of gateway mode.

### Testing Details

* **Unit tests:** `NetworkRenderState` lifecycle (create → rendered → delete) and RA consumer logic without tunnel-id annotations.
* **Unit tests:** no `NodesSelected` patch on primary (C)UDN when per-node status is enabled.
* **E2E:** dynamic UDN pod placement churn; RA advertises only after `Rendered=True`; object count tracks active (node, network) pairs.
* **Unit / E2E:** `NetworkRenderState` is **not** deleted while an EgressIP still references the network on the node after the last pod leaves (and vice versa).
* **Scale:** node churn does not increase primary (C)UDN status patch rate; per-node object count stays sparse.

### Documentation Details

* CRD reference: schema, naming, labels, condition semantics.
* Lifecycle diagram: active → rendered → teardown → GC.
* Migration from `NodesSelected` and tunnel-ID heuristics.
* RA behaviour change: required conditions before route advertisement (all dynamic-UDN topologies).
* Which (C)UDN primary conditions remain unchanged (only `NodesSelected` deprecated).
* Boundary with [#6414](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414): this OKEP does not define metrics or status changes for non-(C)UDN resources listed there.

## Risks, Known Limitations and Mitigations

* **Risk:** Per-node status object count for dynamic UDN.  
  **Mitigation:** Sparse creation only for active (node, network) pairs; GC on teardown; **not** used for static UDN.

* **Risk:** RA advertises before render completes if consumers ignore `Rendered`.  
  **Mitigation:** Document and enforce `Active=True` **and** `Rendered=True` in RA; unit/E2E coverage.

* **Risk:** Stale objects after node loss or forced deletes.  
  **Mitigation:** GC on node delete, network delete, and grace-period expiry; owner references or finalizers TBD in implementation.

* **Limitation:** Adds a new CRD, RBAC, and watch fan-out.  
  **Mitigation:** Bounded cardinality; list/watch by label; follow UplinkState operational patterns.

## OVN-Kubernetes Version Skew

To be set during implementation (target minor release TBD). Likely spans multiple PRs: CRD + node writer first; RA consumer; `NodesSelected` deprecation.

## Backwards Compatibility

* **`NodesSelected`** deprecated on primary (C)UDN when per-node status ships; consumers switch to `NetworkRenderState`.
* **(C)UDN** (`NetworkCreated`, `NetworkAllocationSucceeded` on UDN/CUDN; `TransportAccepted`, `UplinksReady` on CUDN only): **no breaking change** to those conditions.
* **Downgrade:** document mixed-version behaviour if older components still patch `NodesSelected` or lack the CRD writer.
* **E2E:** prefer new tests for per-node status; avoid breaking existing dynamic UDN tests that validate current API until migration is complete.

## Alternatives

### 1. Keep `NodesSelected` on the primary (C)UDN

| Pros | Cons |
|------|------|
| No new CRD | High churn on primary CR as active-node count changes |
| Simple for human `kubectl describe` | Cannot express per-node **rendered** state |
| | Other controllers still need annotation heuristics for L2 |

**Rejected** — does not solve [#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788) and worsens etcd churn.

### 2. Prometheus metrics only

| Pros | Cons |
|------|------|
| No new CRD | Controllers cannot watch metrics as a reliable control-plane API |
| Good for operators | Does not replace tunnel-ID / `NodesSelected` for RA |

**Rejected** for controller consumption — metrics suit operators on other resources ([#6414](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414)); they do not replace tunnel-ID / `NodesSelected` / render semantics for RA ([#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)).

### 3. Tunnel-ID / node annotation heuristics (status quo for L2)

| Pros | Cons |
|------|------|
| Already implemented | Opaque; couples RA to allocation internals |
| No schema change | Wrong signal for transit-router topology (no tunnel allocation) |

**Rejected** — explicit TODO in RA controller; not a stable API.

### 4. Per-node status CRD (selected)

Publish state on a dedicated CRD with naming `<network>.<node>`. The per-node `ovnkube-controller` owns one object; cluster-manager watches it.

| Pros | Cons |
|------|------|
| Kubernetes-native; **other controllers can watch it** | Extra CRD, RBAC, discovery, lifecycle |
| Clear writer ownership (one field manager per object) | Watch fan-out grows with active (node, network) pairs |
| Avoids SSA shard fights on the primary CR | Still etcd writes (sparse, not per-node-event on primary CR) |
| Bounded cardinality for dynamic UDN only | |

**Prior art:**

* **OVN-Kubernetes UplinkState** ([PR #6555](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6555)) — cluster-scoped per-node status named `<uplink>.<node>`. `ovnkube-node` publishes it; other controllers watch it.
* **kubernetes-nmstate `NodeNetworkConfigurationEnactment`** — one object per **(node, policy)**, named `<node>.<policy>`.
* **OpenShift `MachineConfigNode`** — per-node status; `MachineConfigPool` aggregates from those objects.

**Selected** for dynamic UDN `NetworkRenderState` (low-cardinality **network × node**; replaces `NodesSelected` and supplies `Rendered`).

### 5. Errors-only per-node status on primary CR

Patch the primary (C)UDN only on failure, or maintain a coarse aggregate condition.

**Rejected** for render state — RA must see the **happy path** (`Rendered=True`) before advertising; errors-only does not provide positive per-node confirmation.

### 6. Lease or ConfigMap per-node status

| Pros | Cons |
|------|------|
| Avoids patching primary CR `.status` | Non-standard; poor UX vs CRD conditions |
| TTL on Leases can GC stale entries | Not idiomatic for typed controller input |

**Rejected** in favor of a typed per-node status CRD.

## References

* [#6788 - Per-node status that a (C)UDN is rendered, for cluster-manager consumers](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)
* [OKEP-5552: Dynamic UDN Node Allocation](okep-5552-dynamic-udn-node-allocation.md)
* UDN status: `go-controller/pkg/clustermanager/network_cluster_controller.go` (`updateNetworkStatus`, `updateDynamicUDNStatus`)
* Route advertisements (per-node network render status TODO): `go-controller/pkg/clustermanager/routeadvertisements/controller.go`
* OVN-Kubernetes UplinkState: [PR #6555](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6555)
* Kubernetes API conventions on conditions: [api-conventions.md](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)
