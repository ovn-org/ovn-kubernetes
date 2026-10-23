# OKEP-6414: Remove per-node status updates for resources

* Issue: [#6414](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414)

## Problem Statement

Several OVN-Kubernetes features today write Kubernetes API objects’ **status** subresources frequently in response to **node lifecycle** and **scale** events (adds/deletes/churn). Even when these writes use **server-side apply (SSA)**, they still generate sustained **kube-apiserver** traffic and **etcd** revision growth. Under rapid node scale-out or scale-in, that churn can contribute to large etcd databases (including retained history until compaction) and operational risk if etcd approaches size limits.

The churn comes from two places:

1. **Per-node `ovnkube-controller` instances** patch status with SSA `fieldManager` set to the **node name** when that node reconciles the CRDs listed in [Goals](#goals) (for example EgressFirewall, AdminNetworkPolicy).
2. **Cluster-manager** rolls those shards up into cluster-scoped summary fields via **`StatusManager`** (for example EgressFirewall `status.status`, ANP `Ready-In-Zone-<node>` conditions on the primary CR).

## Goals

* **Stop high-churn per-node status patches** on CRDs used for **success/error reporting** where no other controller consumes the per-node outcome as control-plane input:
  * **AdminPolicyBasedExternalRoute**
  * **EgressFirewall**
  * **AdminNetworkPolicy** and **BaselineAdminNetworkPolicy**
  * **NetworkQoS**
  * **EgressQoS**
* Provide a way to keep a **coarse success/failure summary** on the primary CR (for `kubectl` / GitOps) without per-node shards on that object.

## Future Goals

* **Deprecate legacy per-node status** (`EnableStatusMetrics=false` path) for the CRDs listed in Goals, after giving users time to move from CR status to metrics and after metrics mode has proven out. Revisit deprecation then—whether, when, and how (for example changing the flag default, or removing the legacy path) is **out of scope for this OKEP** and can be decided later. As part of that revisit, also consider whether **Network Observability (NetObserv)** (or similar) can offer a better debugging path for these features so that presence or absence of per-node CR status matters much less operationally.

## Non-Goals

* Using **Kubernetes Events** as a replacement for per-node status (events also add kube-apiserver traffic).
* Replacing etcd compaction, quota, or apiserver request tuning—this OKEP targets **OVN-Kubernetes-originated** status churn tied to node lifecycle on the CRDs listed above.
* **Coalescing or rate-limiting** per-node status updates instead of removing them—that still leaves etcd growth and watch traffic.
* Adopting a **dense** per-(node, resource) status CRD model (one status object per node per EgressFirewall, etc.).
* **Dynamic UDN** per-node render/active state, `NodesSelected` deprecation, or route-advertisement render signalling—see [#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788).
* Changing spec-driven or low-churn (C)UDN status conditions on the primary object: `NetworkCreated`, `NetworkAllocationSucceeded` on UDN/CUDN; `TransportAccepted`, `UplinksReady` on **CUDN only**.

## Introduction

OVN-Kubernetes propagates per-node sync outcomes into CRD status in several ways:

1. **Per-node `ovnkube-controller`** applies status with **`FieldManager` set to the node name** (EgressFirewall, EgressQoS, NetworkQoS, AdminPolicyBasedExternalRoute, Admin and Baseline Admin Network Policy). ANP/BANP use `Ready-In-Zone-<node>` condition types—one condition per node.
2. **Cluster-manager `StatusManager`** watches those objects, aggregates per-node shards (`messages[]` or `Ready-In-Zone-<node>`) into cluster-scoped summary fields, and reconciles when the set of OVN-managed nodes changes (`zone_tracker` when a node gains or loses host subnet—not on every node heartbeat).

At large node counts, ordinary cluster maintenance (rolling upgrades, autoscaling) creates bursts of **ApplyStatus** / **UpdateStatus** traffic across many instances of those CRDs. SSA helps with field-level merging but does **not** eliminate etcd writes or watch traffic.

These features only need a **success/error** reporting channel for operators. This enhancement offers an **opt-in** path that stops using CRD `.status` as a **high-churn, per-node** channel when `EnableStatusMetrics` is **on**. Operators then debug through **metrics**; a **low-frequency** summary on the primary object is derived from Prometheus when a scrape stack is available. With the flag **off** (the default), today's per-node status and `StatusManager` rollup are unchanged.

## User-Stories/Use-Cases

Story 1: Operate large clusters without apiserver/etcd overload from per-node status patches

As a **platform engineer** running a cluster with thousands of nodes and frequent autoscaling, I want to **enable metrics mode** so **OVN-Kubernetes stops patching `.status` on the CRDs listed in Goals on every node add/delete**, **so that** the control plane remains stable and etcd stays within safe operating bounds.

Story 2: Debug node-specific sync failures without scraping managedFields

As a **network or support engineer**, when an in-scope CR (for example an EgressFirewall) is unhealthy on **specific nodes**, I want **clear metrics and dashboards** (by node and feature) **so that** I can narrow incidents quickly without scraping per-node managedFields on many objects.

Story 3: Keep legacy status by default or when metrics are not an option

As a **platform engineer** who has not opted into metrics mode (or cannot run Prometheus yet), I want **`EnableStatusMetrics=false` (the default)** so today's per-node status shards and `StatusManager` rollup continue **so that** existing tooling that reads `.status` works unchanged.

Story 4: A coarse kapi summary without per-node shards

As a **GitOps engineer**, I want a **single summary field** on the primary CR (updated infrequently from metrics) **so that** I can gate deployments on “Is there any failure?” without per-node detail on the object.

## Proposed Solution

### High-level direction

1. **Metrics instead of per-node status (opt-in)**  
   When `EnableStatusMetrics` is **on**, per-node `ovnkube-controller` instances **stop** `ApplyStatus` / `UpdateStatus` for routine **success/error** outcomes on AdminPolicyBasedExternalRoute, EgressFirewall, Admin and Baseline Admin Network Policy, NetworkQoS, and EgressQoS. Cluster-manager **stops** `StatusManager` rollup driven by per-node shards on the primary CR. Per-node detail is exported as **Prometheus metrics**.

2. **Feature flag (legacy by default)**  
   A single flag (`EnableStatusMetrics`, default **`false`**) controls this behaviour cluster-wide:
   * **`false` (default):** legacy mode—today's per-node status patches and `StatusManager` rollup unchanged.
   * **`true`:** metrics mode—no per-node SSA shards on the CRDs listed in Goals; CM summary from Prometheus when available (below).

3. **CM summary via Prometheus**  
   When metrics mode is on and Prometheus is **available**, cluster-manager **periodically queries Prometheus** and patches a **single** aggregated summary on each primary CR **only when the queried outcome changes**. This is **not** driven by node informer events. The rollup is **skipped** when Prometheus is not configured or not reachable (see [Metrics availability](#metrics-availability)). When the flag is **off**, rollup uses today's `StatusManager` path. See [CM Prometheus rollup](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-aggregated-cr-summary) in Alternatives.

4. **Cleanup of legacy shards**  
   One-time or startup cleanup of stale **managedFields** / per-node SSA entries may still be needed when switching to metrics mode; that is not ongoing per-node status reporting.

### Feature flag

| Flag | Default | When `false` | When `true` |
|------|---------|--------------|-------------|
| `EnableStatusMetrics` | `false` | Per-node controllers write legacy status; CM `StatusManager` rollup **as today** | Per-node controllers export metrics; **no** per-node SSA shards on the CRDs listed in Goals; CM runs Prometheus rollup when metrics are available |

The flag applies to the CRDs listed in Goals only. It does **not** change (C)UDN status on the primary object. Deprecation of the legacy (`false`) path is a [Future Goal](#future-goals), not part of this OKEP's deliverables.

### Metrics

Introduce (and document) Prometheus metrics from `ovnkube-controller` on each node.

#### Metric shape

* **One metric family per CRD type:** The metric name encodes the resource; labels identify the instance and node. This keeps each metric easier to query and document.
* **Type:** labeled **gauge** (or counter for transition events) per sync outcome — `1` = success, `0` = failure.
* **Labels (bounded):** `node`, `name` (Kubernetes object name), and `namespace` on **namespaced** CRDs only. The **`node` label is bounded by cluster size** ([Prometheus label guidance](https://prometheus.io/docs/practices/naming/#labels)). Do **not** use unbounded labels (full error text, stack traces).
* **Per-node per-object series are expected** — that replaces today's per-node status shards. Cardinality is `O(nodes × resource instances)`, which is acceptable for Prometheus when labels are bounded, unlike etcd SSA shards on a shared CR.
* **Pre-aggregated gauges** (optional, exporter- or recording-rule–based): for example `ovnkube_egressfirewall_failing_nodes` — simplifies CM rollup to `count > 0` without scanning all node series each interval.
* **Deleted nodes:** when a node is removed, its per-node series may linger in Prometheus for a time after scraping stops. Operator PromQL and dashboards can still list that `node` label until series go stale; cross-check against current cluster nodes. The CR summary ignores removed nodes via the live relevant-node set in [CM rollup](#cm-rollup-prometheus-scrape).

**Proposed metric families (per CRD in Goals):**

| CRD | Metric name (gauge) | Labels (besides `node`) |
|-----|---------------------|-------------------------|
| EgressFirewall | `ovnkube_egressfirewall_sync_succeeded` | `namespace`, `name` |
| AdminNetworkPolicy | `ovnkube_adminnetworkpolicy_sync_succeeded` | `name` (cluster-scoped) |
| BaselineAdminNetworkPolicy | `ovnkube_baselineadminnetworkpolicy_sync_succeeded` | `name` (cluster-scoped) |
| EgressQoS | `ovnkube_egressqos_sync_succeeded` | `namespace`, `name` |
| NetworkQoS | `ovnkube_networkqos_sync_succeeded` | `namespace`, `name` |
| AdminPolicyBasedExternalRoute | `ovnkube_adminpolicybasedexternalroute_sync_succeeded` | `name` (cluster-scoped) |

Each removed status field must map to a documented row in this table before operators enable metrics mode.

#### Per-node controller obligations

On each `ovnkube-controller` instance, for every CRD in Goals:

1. **After reconcile** — set or update the gauge for `(resource, node)` to `1` (success) or `0` (failure).
2. **On resource delete** — remove the corresponding label set (for example `DeleteLabelValues` on the vec) so metrics do not leak after the object is gone.
3. **On node shutdown** — scraping stops; stale series are handled by [CM rollup](#cm-rollup-prometheus-scrape) (relevant-node set ignores deleted nodes).

Cluster-manager does not scrape per-node exporters directly; it queries Prometheus after federation/scrape as today.

#### Example: EgressFirewall

**Metric (proposed name):**

```text
ovnkube_egressfirewall_sync_succeeded{namespace="prod", name="default-deny", node="worker-2"} 0
ovnkube_egressfirewall_sync_succeeded{namespace="prod", name="default-deny", node="worker-1"} 1
```

**Operator debug workflow:**

1. `kubectl get egressfirewall default-deny -n prod` — read coarse summary on the CR (from CM rollup): `status.status: Failed` or equivalent.
2. PromQL — find failing nodes:

   ```promql
   ovnkube_egressfirewall_sync_succeeded{namespace="prod", name="default-deny"} == 0
   ```

3. Inspect logs on the failing node(s) — metrics expose **which node** and **success/failure**, not the full error string (same as today's pattern of checking node logs after reading status).

**Recording rule (dashboards):**

```promql
sum by (namespace, name) (ovnkube_egressfirewall_sync_succeeded == 0)
```

### Metrics availability

Cluster-manager needs a central way to decide whether Prometheus-backed rollup can run:

* **Configuration:** Prometheus query URL (or reuse an existing observability config knob).
* **Health:** periodic probe (for example `up` or a canary query). Cache **available / unavailable** with a TTL; do not block per-node reconcile on probe failure.
* **Behaviour:**
  * Metrics mode **on** + Prometheus **available** → per-node metrics exported; CM runs periodic rollup and patches summary when the aggregate changes.
  * Metrics mode **on** + Prometheus **unavailable** → per-node metrics still exported; CM **does not** patch summary (log at low verbosity); operators use scrape targets or wait for Prometheus.
  * Metrics mode **off** → legacy status path; Prometheus availability is irrelevant.

The CM summary is **eventually consistent** (scrape + query interval + lag) and is a **cache** of metrics, not the source of truth for per-node debug.

### CM rollup (Prometheus scrape)

When `EnableStatusMetrics` is **true** and Prometheus is **available**:

1. Per-node `ovnkube-controller` instances write outcomes to **metrics only** (no per-node shards on the primary CR).
2. Cluster-manager runs a **timer** (interval TBD, for example 30–60s) per resource type or a shared worker.
3. For each resource instance, CM determines the **relevant node set** from the **live** cluster view (not from Prometheus label values). See [Relevant node set](#relevant-node-set) below.
4. CM evaluates sync health **only for nodes in the relevant set** (for example PromQL with a `node` matcher built from that set, or an equivalent filter when interpreting query results). Series for other `node` label values are ignored. Use the resource’s `ovnkube_<crd>_sync_succeeded` metric from [Metric shape](#metric-shape). Pre-aggregated gauges must use the same relevant-node semantics.
5. Derive an **aggregate outcome** for that object:
   * **Success** — every relevant node has `sync_succeeded == 1`.
   * **Failure** — any relevant node has `sync_succeeded == 0`.
   * **Incomplete** — the relevant set is empty, or at least one relevant node has no series yet. Treat as unknown: do not patch a success outcome (same rule as legacy `StatusManager` `applyEmptyOrFailed` for `status.status` resources).
6. When the aggregate outcome **differs** from what is already on the primary CR, CM patches **only the coarse summary** (not per-node shards):
   * **EgressFirewall**, **EgressQoS**, **NetworkQoS**, **AdminPolicyBasedExternalRoute** — `status.status`, using the same success/failure values today’s `StatusManager` writes (message strings for the first three; `Success` / `Fail` for APBExternalRoute).
   * **AdminNetworkPolicy**, **BaselineAdminNetworkPolicy** — a **single aggregate condition** (for example `Ready`) instead of per-node `Ready-In-Zone-<node>` conditions.
   * On **failure**, when PromQL returns failing nodes, CM *will include up to **five** of those names (sorted lexicographically) in the existing summary text or condition message, so admins can find logs without PromQL. Prometheus remains the source of truth for the full node list.
7. CM is the **sole writer** of those summary fields (`fieldManager: cluster-manager`). Patch only when the derived outcome changes; do not run this path on node informer events.

When Prometheus is **unavailable** with metrics mode on, skip steps 3–7; per-node metrics are still exported ([Metrics availability](#metrics-availability)).

When the flag is **false**, steps 1–7 are skipped; today's `StatusManager` informer + per-node shard aggregation applies.

Do **not** implement rollup as “reconcile `StatusManager` on every node informer event” in metrics mode.

#### Relevant node set

| CRD | Relevant nodes (high level) | Notes |
|-----|----------------------------|--------|
| **EgressFirewall** | If the namespace **primary network is the cluster default**: all OVN-managed nodes with a host subnet (`zone_tracker`). If the namespace **primary network is a UDN**: nodes where `NodeHasNetwork(node, network)` is true for that UDN (same logic as `egressFirewallManager.getRelevantZones` today). | With **dynamic UDN**, active/rendered membership shrinks to nodes where the UDN is rendered; align with network manager (future: [#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788) `NetworkRenderState`). |
| **EgressQoS**, **AdminPolicyBasedExternalRoute** | All `zone_tracker` nodes | Implemented on the **cluster default network** controller only. |
| **AdminNetworkPolicy**, **BaselineAdminNetworkPolicy** | All `zone_tracker` nodes | Per-node programming targets **default-network** pod IPs; dynamic UDN does not shrink this set. |
| **NetworkQoS** | All `zone_tracker` nodes today; **target:** nodes where the **NetworkQoS object's network** is rendered on the node (per-network controller), matching dynamic UDN active nodes when segmentation is enabled. | Implementation should mirror EgressFirewall-style narrowing where the controller is network-scoped. |

**Node deletion:** when a node is removed from the relevant set, rollup **must not** count it even if Prometheus still returns series for that `node` label (stale samples until scrape stops or series age out).

**Cluster default network vs UDN:** the cluster default network is always rendered on every OVN node. **Dynamic UDN** only gates **UDN/CUDN** networks; it does not change the relevant-node set for default-network-only CRDs above.

### API Details

* When metrics mode is on, stop writing per-node shards on the CRDs listed in Goals; coarse summary field remains (from CM rollup when Prometheus is available).
* **No new CRDs** for per-node success/error of EgressFirewall, EgressQoS, NetworkQoS, ANP/BANP, or APBExternalRoute.

### Implementation Details

| Area | Current pattern | Target (`EnableStatusMetrics=true`) | Target (`EnableStatusMetrics=false`, default) |
|------|-----------------|---------------------------------------------|--------------------------------------|
| Admin Policy Based External Route | Per-node SSA `messages[]` + `StatusManager` rollup | Metrics; CM summary from Prometheus when available | Unchanged |
| Egress Firewall | Per-node SSA `messages[]` + `StatusManager` rollup | Metrics; CM summary from Prometheus when available | Unchanged |
| Admin and Baseline Admin Network Policy | Per-node `Ready-In-Zone-<node>` + node-delete cleanup | Metrics; CM summary from Prometheus when available | Unchanged |
| Egress QoS | Per-node `Ready-In-Zone-<node>` + `StatusManager` rollup | Metrics; CM summary from Prometheus when available | Unchanged |
| Network QoS | Per-node `Ready-In-Zone-<node>` + `StatusManager` rollup | Metrics; CM summary from Prometheus when available | Unchanged |

Implementation steps:

* Add **`EnableStatusMetrics`** flag, default **`false`** (legacy behaviour).
* Audit all `ApplyStatus` / `UpdateStatus` call paths for the CRDs above in per-node `ovnkube-controller` and `StatusManager`.
* When flag is **on:** gate out per-node SSA status writes; register metrics with documented names/labels; bypass `StatusManager` shard rollup; implement CM rollup worker.
* When flag is **off:** preserve today's code paths exactly.
* Implement **[metrics availability](#metrics-availability)** helper in cluster-manager.
* Wire CM rollup **relevant-node set** per [Relevant node set](#relevant-node-set) (reuse `egressFirewallManager.getRelevantZones` for EgressFirewall; `zone_tracker` for default-network CRDs; network-scoped logic for NetworkQoS).
* Implement CM rollup steps 5–7 (aggregate outcome, summary patch, and up to five failing node names in the summary text when that set is known) per resource type.
* Map every former status signal to a [documented metric family](#metric-shape) before enabling metrics mode.
* Ensure per-node controllers follow [Per-node controller obligations](#per-node-controller-obligations) (set on reconcile, delete label values on object delete).
* One-time **managedFields** / stale shard cleanup when switching to metrics mode.
* Provide **Grafana** dashboard examples under `docs/` or the observability guide.
* Document migration for anyone enabling metrics mode who previously parsed status, conditions, or managedFields.

### Testing Details

* **Unit tests:** with flag **on**, per-node reconcile does **not** patch `.status` on the CRDs listed in Goals; metrics increment correctly; gauge label values removed on resource delete.
* **Unit tests:** with flag **off**, existing status behaviour unchanged (regression).
* **Unit tests:** metrics availability probe (available / unavailable / recovery).
* **Unit tests:** CM rollup patches summary only when PromQL result changes; no patch when Prometheus unavailable; correct relevant-node set per resource; **node delete** with stale Prometheus series for the removed `node` label does not keep that node in the summary denominator; failing node names in the summary text capped at five and sorted.
* **Unit tests:** EgressFirewall rollup on a **primary-UDN** namespace uses `NodeHasNetwork` (not full `zone_tracker`) when dynamic UDN is enabled.
* **E2E (baseline, metrics mode on):** with `EnableStatusMetrics=true`, create an in-scope resource on the **cluster default network** (for example **EgressFirewall**). Assert per-node controllers do **not** write per-node status shards (`messages[]` / `Ready-In-Zone-<node>` / node-named managedFields), and that CM updates the coarse summary from metrics when Prometheus is available (success or failure with up to five failing node names in the summary text). Default (`EnableStatusMetrics=false`) behaviour is covered by unit tests; do not require a separate e2e for the flag-off path.
* **E2E (dynamic UDN relevant set):** with **dynamic UDN** enabled, a namespace on a primary CUDN with **EgressFirewall** and pods on a subset of nodes — CM summary and metrics reflect only **rendered** nodes (inactive UDN nodes are not in the rollup denominator).

#### Scale validation (merge criteria)

Scale reduction on the apiserver/etcd is the main motivation for this OKEP. Scale validation is **not** node churn or object count in isolation: each run combines **high node churn** (many OVN nodes; rolling add/remove or autoscale) with a **high instance count** of the CRD under test—the mix that produced per-node status shard storms under legacy mode.

Repeat that **combined workload for each CRD type** in [Goals](#goals)—AdminPolicyBasedExternalRoute, EgressFirewall, AdminNetworkPolicy, BaselineAdminNetworkPolicy, EgressQoS, and NetworkQoS—or a documented subset if a feature is disabled in the test cluster. Use many object instances where the API allows; for **BaselineAdminNetworkPolicy** (cluster singleton), stress comes from many nodes reconciling the one object during churn. Compare `EnableStatusMetrics=true` vs `false`.

For each run, measure:

* **`.status` patch rate** and **etcd revision growth / apiserver write rate** on the objects under test during node events.
* **CPU usage** of the **kube-apiserver** and **etcd** (compare metrics mode on vs legacy under the same workload).

**Success (thresholds TBD):** with metrics mode on, materially lower patch rate than legacy; remaining churn dominated by timer-bound CM summary patches, not per-node shards; apiserver and etcd CPU should not regress versus legacy (expect lower under status storms); no regression in functional e2e.

Exact node and object counts, thresholds, and whether tests run in CI or as a recorded manual benchmark are TBD in implementation. Record results **per CRD type** before recommending metrics mode for production scale.

### Documentation Details

* Feature flag: name, default (`false` = legacy), how to enable metrics mode.
* Metrics reference: [metric families table](#metric-shape), EgressFirewall example, debug workflow, example PromQL for rollup, [per-node controller obligations](#per-node-controller-obligations).
* Metrics availability: config knobs, behaviour when Prometheus is absent.
* Primary CR summary: [CM rollup](#cm-rollup-prometheus-scrape) algorithm, staleness bounds, failing node names in the summary text when available; metrics are the fresher source for per-node detail.
* CM rollup: [relevant node set](#relevant-node-set) per CRD; boundary with [#6788](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788) for UDN render state.
* [Scale validation](#scale-validation-merge-criteria) plan and recorded results.
* Migration guide from per-node status shards and managedFields.

## Risks, Known Limitations and Mitigations

* **Limitation:** With metrics mode on (`EnableStatusMetrics=true`), the CRDs listed in Goals lose per-node detail on `.status`.  
  **Mitigation:** Metrics and dashboards; CM summary when Prometheus is available; leave the flag off (default) for legacy status.

* **Limitation:** CM summary requires Prometheus when metrics mode is on.  
  **Mitigation:** Central availability check; skip rollup gracefully; document direct scrape for debug.

* **Limitation:** CM summary is eventually consistent (scrape + query interval).  
  **Mitigation:** Document max staleness; metrics remain source of truth for per-node debug.

* **Risk:** Operators grep `.status`, conditions, or managedFields for per-node health after enabling metrics mode.  
  **Mitigation:** Release notes, migration guide, metric name stability; default remains legacy until operators opt in.

* **Risk:** High-cardinality metrics from unbounded labels.  
  **Mitigation:** Bounded label enums; no error strings in labels; recording rules for dashboards.

* **Risk:** Enabling metrics mode before metrics are available leaves a visibility gap.  
  **Mitigation:** Checklist mapping each status field to a metric before flipping `EnableStatusMetrics` to `true`.

## OVN-Kubernetes Version Skew

To be set during implementation (target minor release TBD). Likely spans multiple PRs: feature flag and metrics first; CM rollup.

## Backwards Compatibility

* **Default `EnableStatusMetrics=false`** preserves today's per-node status writes and `StatusManager` rollup for the listed CRDs.
* **`EnableStatusMetrics=true`** changes behaviour (no per-node shards; metrics + Prometheus summary when available).
* **(C)UDN** primary status: **no breaking change** from this OKEP.
* **Downgrade:** document mixed-version behaviour if some components run with metrics mode on and others still write per-node shards.

## Alternatives

**Selected combination:**

* **Option 1** (metrics) for per-node success/error, behind an **opt-in feature flag** (default **off** = legacy status).
* **Option 4** (Prometheus periodic query → low-frequency CM summary) when metrics mode is on and Prometheus is available.

### 1. Metrics-only (selected for per-node success/error)

Per-node `ovnkube-controller` instances **stop** per-node SSA status writes when the feature flag is **on**. Components export Prometheus metrics for per-node outcomes. Cluster-manager does not patch per-node shards on the primary CR.

| Pros | Cons |
|------|------|
| Eliminates status patch storms on node churn, so etcd does not grow from retained status revisions during scale-out or scale-in | No per-node signal on the CRD in metrics mode; `kubectl` cannot show per-node sync health |
| No new CRDs or managedFields shard cleanup on the primary object | **Requires Prometheus** (or compatible scrape/query stack) for per-node **debug** visibility when metrics mode is on |
| Fits high-cardinality data (node × resource) better than etcd | GitOps/CI that only watches CRD `.status` must adopt metrics, dashboards, or [CM Prometheus rollup](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-aggregated-cr-summary) when metrics mode is on |
| Matches CNI/datapath practice of keeping apply detail off high-churn CR objects (see prior art) | Metric cardinality must be designed carefully to avoid a different kind of scale problem |
| **Feature flag** defaults to legacy; operators opt in when ready | Other controllers cannot watch metrics as a reliable control-plane API |

**Prior art** (named systems that keep high-churn apply/health detail **off** the API object and use metrics or local debug instead):

* **Cilium** — CiliumEndpoint extra status PATCHes were disabled by default because they bottlenecked the apiserver ([cilium#10490](https://github.com/cilium/cilium/pull/10490)), then the `--endpoint-status` path was removed ([cilium#30761](https://github.com/cilium/cilium/pull/30761)). Users are directed to agent metrics (`cilium_endpoint_state`, `cilium_policy`, `cilium_controllers_failing`) and `cilium-dbg`.
* **kube-proxy** — `Service` has no per-node status. Datapath sync health is `/metrics` (`kubeproxy_sync_proxy_rules_*`) and healthz.
* **Kubernetes NetworkPolicy** — SIG-Network added a status subresource then **withdrew** it ([KEP-2943](https://github.com/kubernetes/enhancements/issues/2943), [kubernetes#115843](https://github.com/kubernetes/kubernetes/pull/115843)). Policies have no `.status`; CNIs do not write per-node apply results onto the policy.

These examples support **not** putting high-churn dataplane or apply detail on API objects. They do **not** mean Kubernetes treats Prometheus as a general replacement for `.status`: [API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md) still put conditions on the object so other components can consume them without resource-specific knowledge.

### 2. Separate per-node status CRD (per node and resource)

Publish sync or placement state on a dedicated CRD with a stable naming scheme (for example `<node_name>.<resource_name>` or `<resource_name>.<node_name>`). The per-node `ovnkube-controller` owns one object; cluster-manager watches it instead of patching the primary CR.

| Pros | Cons |
|------|------|
| Kubernetes-native visibility; `kubectl get` works per node; **other controllers can watch it** | **Object count** scales as `nodes × resources` if applied naively to every EgressFirewall, EgressQoS, etc. |
| Clear writer ownership (one field manager per per-node status CRD object) | Watch fan-out and list cost grow with cluster size |
| Avoids SSA shard fights on the primary CR’s `.status` | Extra CRDs, RBAC, discovery, and lifecycle (garbage collection when node or policy is deleted) |
| Can be constrained to **sparse** creation (only on failure, or only for networks that render on a node)—see [Assume-success or errors-only status on the primary CR](#3-assume-success-or-errors-only-status-on-the-primary-cr) | Still etcd writes; dense success reporting for every namespaced policy or static UDN recreates the churn problem |

**Prior art:**

* **OVN-Kubernetes UplinkState** ([PR #6555](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6555)) — cluster-scoped per-node status CRD named like `<uplink>.<node>` (for example `br-blue.node-a`). `ovnkube-node` publishes it; other controllers watch it.
* **kubernetes-nmstate `NodeNetworkConfigurationEnactment`** — one object per **(node, policy)**, named `<node>.<policy>`, carrying apply status for a cluster-scoped `NodeNetworkConfigurationPolicy`. Closest analog to “per-node status CRD for a policy on a node.” (`NodeNetworkState` is a different object: one per node for observed NIC state, not per policy.)
* **OpenShift `SriovNetworkNodeState`** — one object per node, named after the node, for discovered SR-IOV hardware and sync status.
* **OpenShift `MachineConfigNode`** — per-node status CRD; `MachineConfigPool` aggregates counts from those objects rather than stuffing every node into the pool spec object.
* **Kubernetes Node conditions** — per-node health lives on the `Node` object, not on every namespaced workload.

**Rejected** for the CRDs in Goals—a per-(node, resource) object trades patch churn for object cardinality.

### 3. Assume-success or errors-only status on the primary CR

Do not report success per node. Variants include:

* **(a)** Condition stays `True` until any node fails; only patch when the failure set changes.
* **(b)** Patch `.status` only when `handlerErr != nil`; never write on successful node sync.
* **(c)** Errors-only on a per-node status CRD (combine with [separate per-node status CRD](#2-separate-per-node-status-crd-per-node-and-resource)).

| Pros | Cons |
|------|------|
| Large reduction in writes on the happy path (node additions are often silent) | **Ambiguity:** `True` vs “not yet reconciled” vs “all nodes synced” |
| Simple for operators who only care about “Is there any failure?” | Stale failure state if a node is deleted without cleanup |
| (b) avoids success-path etcd traffic entirely | Per-node SSA shards may still need cleanup; does not by itself stop cluster-manager from reconciling on every node event |
| (c) keeps primary CR clean | Failure path still writes; dense failure storms during incidents |

**Prior art:**

* **Kubernetes NetworkPolicy** (after KEP-2943 withdrawal) — no status field; the object is assumed applied. Closest to “assume success.”
* **CiliumEndpoint** extra status default-off / removed — operators assume the endpoint is healthy unless metrics or `cilium-dbg` say otherwise.
* **cert-manager `Certificate` `Ready`** and **Gateway API `Accepted` / `Programmed`** — **not** errors-only. They always maintain coarse conditions on the **primary** object (positive polarity is expected to be set). That is a **low-cardinality summary on the CR**, closer to variant (a) or [CM Prometheus rollup](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-aggregated-cr-summary) than to sparse per-node error writes.

**Not selected** as the sole per-node solution: success-path silence does not give operators positive confirmation per node, and (a)/(b) still need care so cluster-manager does not patch on every node event. Variant (a) overlaps the **allowed** low-frequency [CM Prometheus rollup](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-aggregated-cr-summary). Variant (c) overlaps [separate per-node status CRD](#2-separate-per-node-status-crd-per-node-and-resource) if objects are created only on failure.

### 4. Cluster-manager writes low-frequency aggregated status (selected for aggregated CR summary)

Hybrid: per-node `ovnkube-controller` instances emit metrics ([Metrics-only](#1-metrics-only-selected-for-per-node-successerror)) and stop per-node SSA shards on the primary CR. Cluster-manager periodically patches a **single** summary on the primary CR when a **Prometheus query** says the aggregate changed.

This OKEP selects **Prometheus scrape into cluster-manager** as the rollup transport when metrics mode is on and Prometheus is available (see [CM rollup (Prometheus scrape)](#cm-rollup-prometheus-scrape)). When metrics mode is on, cluster-manager **runs** this rollup on a timer and **skips** patching the summary only when Prometheus is unavailable (see [Metrics availability](#metrics-availability)); per-node debug still uses metrics endpoints directly. When **`EnableStatusMetrics=false`**, today's **`StatusManager`** path (reading per-node shards from the primary CR) applies instead—no Prometheus rollup.

| Transport | Selected? | Notes |
|-----------|-----------|-------|
| **Prometheus scrape into CM** | **Yes** | Periodic PromQL; patch only on diff; requires [metrics availability](#metrics-availability); documented staleness |
| **Per-node status CRD + informer** | No (for aggregated CR summary) | Durable but adds CRDs; not selected for in-scope CR summaries |
| **RPC/HTTP push** | No | Fragile without retry/HA |

| Pros | Cons |
|------|------|
| Restores a **kapi-level** summary for `kubectl` / GitOps; other controllers and humans can read the object | Still writes to etcd (much lower rate than today's per-node-event rollup, but not zero) |
| Decouples per-node churn from per-node status patches | Requires Prometheus for rollup when metrics mode is on; summary can **lag** live metrics (scrape + query interval) until the next query |
| Can be tuned (periodic PromQL interval; patch only when the aggregate changes) | Implementation complexity versus [Metrics-only](#1-metrics-only-selected-for-per-node-successerror) with no summary on the CR; uncommon scrape-then-status pattern |
| Distinct from today's "patch on every node event" rollup; reuses [Metrics-only](#1-metrics-only-selected-for-per-node-successerror) metrics—no second node→CM transport; single CM writer on the summary field | No per-node detail on primary CR; compared with metrics-only alone, a delayed kapi summary is still better than none |

**Prior art:**

* No widely used Kubernetes controller was found that **scrapes Prometheus** and then writes CRD `.status`. The common direction is the reverse (kube-state-metrics reads objects and **exports** metrics). This OKEP **selects** this unusual scrape-then-status path for a coarse summary when metrics mode is on and Prometheus is available; staleness must be documented.
* **OpenShift `MachineConfigPool`** aggregates `updatedMachineCount` / `degradedMachineCount` from per-node `MachineConfigNode` (or Node) objects—kapi per-node status CRDs plus a low-frequency rollup, not Prometheus. That is the [separate per-node status CRD](#2-separate-per-node-status-crd-per-node-and-resource) + summary model; it is a reasonable analog but **not** the selected transport for aggregated CR summaries here (would reintroduce dense per-node status CRDs for every in-scope CR).

**Selected** when metrics mode is on and Prometheus is available. Putting operational truth on the primary object is desirable. Do **not** implement it as “reconcile `StatusManager` on every node informer event.”

### 5. Cluster-manager aggregated status only (drop per-node SSA, no new input)

Per-node `ovnkube-controller` instances stop patching; cluster-manager alone rolls up using today's `StatusManager` reading shards on the primary CR, still driven by **node informers**.

| Pros | Cons |
|------|------|
| Single writer on `.status`; simpler managedFields | **Broken once per-node SSA stops:** no shard source on the primary CR |
| Stops per-node SSA patches from `ovnkube-controller` | Rollup still patches on every node event if driven by informers, so etcd revision growth remains |

**Rejected.** This is today's cluster-manager path without per-node SSA shards, but with no replacement input once shards are gone. A change-driven or periodic summary fed by metrics ([CM Prometheus rollup](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-aggregated-cr-summary)) is the version that remains viable when metrics mode is on—or operators leave `EnableStatusMetrics=false` (the default) for legacy behaviour.

### 6. Rate-limit or coalesce status patches

Batch or throttle `ApplyStatus` calls instead of removing them.

| Pros | Cons |
|------|------|
| Smaller implementation change than full removal | Still writes to etcd; delays visibility; harder to reason about “last known good” state |
| Reduces peak apiserver QPS | Does not bound total revision growth under sustained churn |

**Rejected.** Coalescing or throttling reduces peak apiserver QPS but still writes to etcd on node churn, delays visibility, and does not bound revision growth under sustained autoscaling—the problem this OKEP targets. [Metrics-only](#1-metrics-only-selected-for-per-node-successerror) with [CM Prometheus rollup](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-aggregated-cr-summary) addresses the root cause instead.

### 7. Lease or ConfigMap per-node status

Store per-node sync detail in Leases or ConfigMaps keyed by node/resource.

| Pros | Cons |
|------|------|
| Avoids patching primary CR `.status` | Non-standard; poor UX vs CRD conditions |
| TTL on Leases can garbage-collect stale entries | Still apiserver objects and watches; not idiomatic for sync health |

**Rejected** in favor of [Metrics-only](#1-metrics-only-selected-for-per-node-successerror) for per-node success/error on the CRDs listed in Goals.

## References

* [#6414 - Remove per node status update for resources](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6414)
* [#6788 - Per-node status that a (C)UDN is rendered, for cluster-manager consumers](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6788)
* OVN-Kubernetes cluster-manager status manager: `go-controller/pkg/clustermanager/status_manager/status_manager.go`
* Cluster-manager node tracking (`zone_tracker`): `go-controller/pkg/clustermanager/status_manager/zone_tracker/zone_tracker.go`
* Kubernetes API conventions on conditions: [api-conventions.md](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)
* [Option 1](#1-metrics-only-selected-for-per-node-successerror): [Cilium endpoint-status removal](https://github.com/cilium/cilium/pull/30761); [Cilium CEP status disabled by default](https://github.com/cilium/cilium/pull/10490); kube-proxy `/metrics`; [KEP-2943 NetworkPolicy status withdrawn](https://github.com/kubernetes/enhancements/issues/2943)
* [Option 2](#2-separate-per-node-status-crd-per-node-and-resource): OVN-Kubernetes UplinkState ([PR #6555](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6555)); [kubernetes-nmstate enactments](https://nmstate.github.io/kubernetes-nmstate/user-guide/102-configuration.html); [SriovNetworkNodeState](https://github.com/openshift/sriov-network-operator/blob/main/doc/api/node-state-api.md); OpenShift [MachineConfigNode](https://github.com/openshift/machine-config-operator/pull/4012)
* [Option 3](#3-assume-success-or-errors-only-status-on-the-primary-cr): NetworkPolicy (no status); cert-manager `Certificate` `Ready`; Gateway API `Accepted` / `Programmed` ([GEP-1364](https://gateway-api.sigs.k8s.io/geps/gep-1364/))
* [Option 4](#4-cluster-manager-writes-low-frequency-aggregated-status-selected-for-aggregated-cr-summary): MachineConfigPool rollup from per-node objects as the informer analog; Prometheus scrape-then-status as chosen summary transport
* Prometheus label cardinality: [naming conventions — Labels](https://prometheus.io/docs/practices/naming/#labels)
