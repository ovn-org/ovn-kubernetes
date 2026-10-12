# AGENTS.md — ovn

Core OVN control-plane for OVN-Kubernetes. See
[`go-controller/pkg/AGENTS.md`](../AGENTS.md) for the package directory
overview and [`go-controller/AGENTS.md`](../../AGENTS.md) for component
architecture, codegen, and code conventions.

## Purpose

The `ovn` package implements the **ovnkube-controller** component. It
runs on every node and programs OVN Northbound/Southbound databases via
libovsdb to create the logical networking infrastructure for pods,
services, network policies, egress features, and User Defined Networks:

- Logical switches per node, logical router ports, and distributed
  gateway routers.
- Pod logical switch ports and IPAM (via logical switch manager).
- Kubernetes Services as OVN load balancers (via service controller).
- NetworkPolicy enforcement as OVN ACLs and port groups.
- AdminNetworkPolicy / BaselineAdminNetworkPolicy (ANP/BANP).
- EgressIP — OVN logical router policies, NATs, and reroute rules.
- EgressFirewall — OVN ACLs with DNS name resolution support.
- EgressQoS — DSCP marking via OVN QoS rules.
- EgressService — zone-side OVN programming for egress service routing.
- NetworkQoS — per-flow QoS policies.
- Gateway topology — shared/local gateway router setup per node, join
  switch, COPP, load balancer groups.
- Zone interconnect — transit switches, remote zone chassis handling,
  cross-zone route and port management.
- User Defined Network controllers — L2, L3, and localnet topology
  variants.
- Cluster Network Connect — OVN-side topology (router ports, routes,
  LB attachments) for connecting UDNs.
- Route import — host route import for no-overlay/BGP integration.
- Hybrid overlay support for mixed Windows/Linux clusters.
- Address set management for network policies, EgressIP, and
  EgressFirewall.

ovnkube-controller has direct access to OVN NB/SB databases via
libovsdb. This is the only component that programs OVN directly.

## Design Decisions

- **Why only ovnkube-controller programs OVN DB** — Centralizing all
  OVN NB/SB writes in one component avoids conflicting DB transactions,
  simplifies external-ID ownership tracking, and makes it possible to
  reason about DB state from a single codebase. The cluster manager and
  node controller communicate intent via K8s annotations, which
  ovnkube-controller translates into OVN constructs.

- **Historical evolution: centralized master to per-node controller** —
  ovnkube-controller originally ran as `ovnkube-master` on
  control-plane nodes only (centralized architecture). It was moved to
  run on every node so each node programs its own OVN logical switch
  locally, reducing latency and enabling zone interconnect. The code
  still has a `master.go` file and some naming artifacts from this
  transition. ovnkube-controller and ovnkube-node could logically be a
  single process but remain separate for this historical reason.

- **Controller inheritance hierarchy** —
  `CommonNetworkControllerInfo` (shared clients) →
  `BaseNetworkController` (shared per-network logic) →
  `DefaultNetworkController` or `BaseUserDefinedNetworkController` →
  `Layer2/Layer3/LocalnetUserDefinedNetworkController`. This avoids
  duplicating pod, namespace, network policy, and node handling across
  topology types while allowing topology-specific overrides.

- **Shared node controller** — A single `NodeController` (from
  `pkg/controllers/node/`) reconciles node events and fans out to all
  registered network controllers. This avoids N independent node watches
  (one per network) and ensures consistent node state across networks.
  Network controllers register via `RegisterNodeHandler()`.

- **Startup ordering matters** — The `run()` sequence (namespace → node
  → service → pod → policy → egress) is carefully ordered: node switches
  must exist before pods can be placed, services must exist before pods
  to prevent LB churn, and namespace address sets must exist before
  network policies reference them. Changing this order causes race
  conditions.

- **Address set deduplication** — The `addresssetmanager` deduplicates
  OVN address sets across network policies that select the same pods.
  Without this, N policies selecting the same namespace would create N
  identical address sets, wasting OVN DB space and increasing flow
  computation time.

- **Event-driven vs level-driven controllers** — Older controllers
  (EgressIP, pods, namespaces, network policies) use the event-driven
  `pkg/retry` framework, which wraps informer Add/Update/Delete
  handlers and retries on failure. This approach is fragile: if an
  event is missed or a handler fails to fully reconcile, the state can
  remain inconsistent until the next event arrives. Newer controllers
  use level-driven workqueue-based reconciliation — some via the
  `pkg/controller` framework (EgressFirewall, NetworkConnect,
  NetworkQoS, addresssetmanager, DNS name resolver), others via their
  own workqueue implementations (ANP, services). In both cases the
  reconcile function reads current state from the informer cache
  rather than relying on event deltas. Level-driven is the preferred
  direction for all new controllers — it eliminates the class of bugs
  where a missed event causes permanent drift.

- **Annotations as the inter-component communication bus** —
  Components do not share database access. Instead, K8s annotations on
  nodes and pods serve as the state-passing channel: ovnkube-node
  writes host-discovered network config (encap IPs, management port
  details, gateway config) into node annotations; ovnkube-controller
  reads them and programs OVN accordingly. Similarly, pod network
  annotations carry IP assignments from IPAM to the CNI path. This
  decoupling allows components to restart independently and simplifies
  the trust model — but the annotation-based approach has known scale
  limits (annotation size, API server write load) that newer features
  are working to reduce.

- **OVN topology varies by network type** — Each network type creates
  a different set of OVN logical objects. The default network uses
  bare names; UDNs prefix every object with `<netname>_` (where `-`
  and `/` in the network name are replaced with `.`).

  Two traffic directions define the topology:
  - **East-west** (pod-to-pod within the cluster): flows through node
    logical switches and the distributed cluster router.
  - **North-south** (pod-to-external or external-to-pod): flows
    through the gateway router (GR), which connects to the cluster
    router via a join switch, and exits through an external switch
    tied to the physical network. With User Defined Networks, each
    network needs its own join subnet to avoid route ambiguity when
    pods have multiple interfaces.

  Secondary networks only carry east-west traffic — they have no
  north-south path (no GR, no join switch, no external switch).
  Only the default network and **primary** UDNs get the full
  north-south gateway stack.

  Topologies per network type:
  - **Default network**: distributed cluster router
    (`ovn_cluster_router`) + per-node logical switch (`<node>`) +
    per-node gateway router (`GR_<node>`) + join switch (`join`)
    connecting GRs to the cluster router + external switch
    (`ext_<node>`). Key ports follow a prefix convention:
    `rtos-`/`stor-` (router↔switch), `rtoj-`/`jtor-` (GR↔join),
    `rtoe-`/`etor-` (GR↔external).
  - **L3 UDN (primary)**: same shape as default but all names are
    scoped — e.g. `<net>_ovn_cluster_router`, `<net>_<node>` (node
    switch), `GR_<net>_<node>`, `<net>_join`, `ext_<net>_<node>`.
  - **L3 UDN (secondary)**: cluster router and per-node switches
    only (east-west). No GR, join switch, or external switch.
  - **L2 UDN (primary)**: a single flat logical switch
    (`<net>_ovn_layer2_switch`) shared by all nodes — no per-node
    switches. A transit router (`<net>_transit_router`) replaces the
    classic cluster-router + join-switch pattern: GRs peer directly
    with the transit router via `rtotr-`/`trtor-` ports.
  - **L2 UDN (secondary)**: the flat switch only — no router at all.
  - **Localnet UDN (always secondary)**: a single logical switch
    (`<net>_ovn_localnet_switch`) with a `localnet`-type port
    (`<net>_ovn_localnet_port`) mapped to a physical network via
    `ovn-bridge-mappings`. No router, no gateway stack, no per-node
    switches — traffic exits directly to the physical network.
  - **No-overlay (default network or primary L3 UDNs)**: the same
    OVN logical objects are created (cluster router, per-node
    switches, GRs, join switch for primary) but Geneve encapsulation
    is disabled and the transit switch is not created. The GR routes
    only this node's local pod subnet(s) to the cluster router
    (instead of all cluster subnets in overlay mode). For remote pod
    subnets, the `routeimport` controller subscribes to netlink
    events, reads BGP-learned routes from the network's kernel VRF
    (installed by FRR), and copies them as static routes into the
    GR — so the GR forwards off-node pod traffic out the physical
    network via BGP next-hops.
  - **EVPN (primary L2/L3 UDNs)**: the local OVN topology stays —
    L3 keeps its cluster router, per-node switches, GRs, and join
    switch; L2 keeps its flat switch, transit router, and GRs. What
    is removed is the transit switch and its cross-zone ports (the
    Geneve interconnect layer). Instead, east-west traffic is
    VXLAN-encapsulated by Linux bridge + VTEP devices that FRR
    configures on each node. The OVN logical switch connects to the
    Linux bridge via an internal port, bridging OVN flows into the
    EVPN fabric. This eliminates double encapsulation (Geneve inside
    VXLAN) and integrates natively with data center spine-leaf EVPN
    fabrics. Currently local gateway mode only.
  - **Cluster Network Connect (primary UDNs only)**: when a
    `ClusterNetworkConnect` CR selects multiple primary UDNs, a
    connect router (`connect_router_<cnc>`) is created to bridge
    their otherwise-isolated topologies. Each selected network's
    cluster/transit router gets a port peered into the connect
    router, with static routes enabling cross-network traffic.

- **Port groups and address sets for network policy** — NetworkPolicy
  ACLs target OVN port groups (sets of selected pod logical switch
  ports) rather than creating per-port ACLs, which would cause O(pods)
  ACL duplication. Peer selectors (ingress `from` / egress `to`) are
  realized as OVN address sets that dynamically track pod/namespace
  membership. This combination keeps the number of OVN ACLs
  proportional to the number of policies, not pods.

- **External IDs for OVN DB ownership tracking** — Every OVN NB/SB
  object created by ovnkube-controller is tagged with `external_ids`
  keys (controller name, network name, node name, etc.). These tags
  enable safe cleanup of stale objects on restart, cross-controller
  isolation (e.g. preventing KubeVirt cleanup from deleting non-KubeVirt
  resources), and upgrade-safe reconciliation when external-ID schemas
  evolve.

- **Per-topology UDN controllers (and their scale cost)** — L2, L3,
  and localnet topologies have fundamentally different OVN constructs
  (L2 has no router, L3 has per-node subnets and a distributed router,
  localnet has bridge mappings). Separate controller types allow clean
  topology-specific logic without conditional branches throughout a
  single controller. However, spinning up a full controller instance
  per network has significant scale implications: each controller
  carries its own informer watches, retry state, and reconciliation
  loops, so resource consumption grows linearly with the number of
  networks. At hundreds of UDNs this becomes a CPU/memory bottleneck. The
  long-term direction is to consolidate into a single network
  controller that plumbs all networks, eliminating per-network
  overhead.

- **Stale-object cleanup on restart** — When ovnkube-controller
  restarts, networks may have been deleted while it was down. The
  controller manager reconstructs "dummy" network controllers from
  external IDs found in the OVN DB to clean up orphaned objects
  (logical switches, ports, ACLs) before starting live controllers.
  This reconstruction-based cleanup ensures no stale OVN objects
  survive restarts, even if the K8s API no longer has a record of the
  deleted network.

- **Startup zone-gate** — The controller manager waits until the OVN
  NB database's zone name matches the expected zone and at least one
  local node exists before starting controllers. This prevents
  wrong-zone programming and avoids the hazard of misclassifying local
  vs remote resources during rolling upgrades or zone migrations.

## Key Types

| Type | Description |
|------|-------------|
| `CommonNetworkControllerInfo` | Shared fields for all network controllers: K8s client, KubeOVN, watch factory, NB/SB clients, zone, pod recorder, event recorder. |
| `BaseNetworkController` | Per-network base struct. Provides logical switch manager, port cache, address set factory, namespace tracking, network policy state, retry frameworks, zone interconnect handler, observability, and shared node reconciler. Embedded by all concrete controllers. |
| `DefaultNetworkController` | Default-network orchestrator. Adds service controller, EgressIP controller, EgressQoS, ANP, egress firewall, egress service, APB external route, network connect, DNS name resolver, zone chassis handler, and load balancer group management. |
| `BaseUserDefinedNetworkController` | Base for UDN controllers. Adds topology-specific join switch and external switch handling. |
| `Layer2UserDefinedNetworkController` | UDN controller for L2 (layer2) topology — flat broadcast domain. |
| `Layer3UserDefinedNetworkController` | UDN controller for L3 (layer3) topology — per-node subnets with distributed routing. |
| `LocalnetUserDefinedNetworkController` | UDN controller for localnet topology — direct physical network connectivity. |
| `EgressIPController` | Manages EgressIP assignments — logical router policies, NATs, reroute rules, and node reachability. Shared across default and UDN networks via NAD reconciler. |
| `GatewayManager` | Per-node gateway topology builder — creates/updates gateway routers, join switch ports, SNAT rules, and load balancer attachments. |
| `PortCache` | Thread-safe cache for logical switch port info, shared across network controllers. |

## Startup Sequence

`DefaultNetworkController.Start()` calls `syncDb()` → `init()` →
`run()`. The `run()` method starts watches in a carefully ordered
sequence:

```text
run()
  1. WatchNamespaces        — no dependencies, node startup depends on it
  2. startNodeReconciliation — creates node switches (most watches depend on this)
  3. StartServiceController — after nodes, to prevent LB churn
  4. WatchPods              — after nodes and services
  5. ANP controller         — if enabled
  6. WatchNetworkPolicy     — depends on pods and namespaces
  7. EgressIP watches       — pods → namespaces → nodes → EgressIP objects
  8. EgressFirewall          — with DNS name resolver
  9. EgressQoS controller
  10. EgressService controller
  11. APBExternalRoute controller
  12. NetworkQoS controller
  13. NetworkConnect controller
  14. Unidling controller    — if OVNEmptyLbEvents enabled
```

## Sub-packages

### Feature Controllers

| Sub-package | Description |
|-------------|-------------|
| `controller/services/` | Service load-balancer controller. Watches Services and EndpointSlices, programs OVN load balancers. Shared across default and UDN networks via multi-network registration. |
| `controller/admin_network_policy/` | ANP/BANP controller. Watches AdminNetworkPolicy, namespace, pod, and node events. Programs OVN ACLs and address sets with cluster-scoped priority ordering. |
| `controller/egressfirewall/` | EgressFirewall controller. Converts EgressFirewall CRs into OVN ACLs, integrates with DNS name resolution for FQDN rules. |
| `controller/egressservice/` | Zone-side EgressService OVN programming — logical router policies and status updates. |
| `controller/network_qos/` | NetworkQoS controller with per-object queues. Programs OVN QoS rules, NAD-aware for UDN support. |
| `controller/networkconnect/` | OVN-side Cluster Network Connect topology builder — connects UDN routers via transit ports, routes, and LB attachments. |
| `controller/apbroute/` | AdminPolicyBasedExternalRoute controller. Programs OVN logical router static routes for external gateway ECMP routing. |
| `controller/apbroute/gateway_info/` | Gateway info cache for APB external routes. |
| `controller/udnenabledsvc/` | Filters and tracks services enabled for UDN networks. |
| `controller/unidling/` | Service unidling controller — watches OVN SB for empty-LB events, annotates services to trigger scale-up. |

### Core Infrastructure

| Sub-package | Description |
|-------------|-------------|
| `address_set/` | OVN address set interface, factory, and OVN-backed implementation. Address sets are the building blocks for ACL match expressions in network policies and egress features. |
| `addresssetmanager/` | Higher-level shared address set manager. Selector-driven, deduplicates address sets across network policies with identical pod/namespace selectors. |
| `logical_switch_manager/` | Logical switch and subnet IPAM management. Tracks per-node switches, allocates pod IPs from node subnets, and manages switch lifecycle. |
| `topology/` | Gateway topology factory helpers — encapsulates shared vs. local gateway router creation patterns. |
| `gateway/` | Gateway cleanup utilities. |
| `gatewayrouter/` | Gateway router configuration helpers. |
| `routeimport/` | Host route import manager interface and controller for no-overlay/BGP integration. Imports host-learned routes into OVN logical routers. |
| `zone_interconnect/` | Zone interconnect handler — manages transit switches, remote zone port bindings, cross-zone static routes, and chassis entries for multi-zone deployments. |
| `dns_name_resolver/` | DNS name resolution for EgressFirewall FQDN rules. Two implementations: `EgressDNS` (built-in resolver) and `ExternalEgressDNS` (using DNSNameResolver CRDs). |
| `healthcheck/` | EgressIP gRPC health server and client for node reachability checks. |
| `port_cache` | Logical port info cache (part of root package, not a sub-package). |

### Migration / Sync

| Sub-package | Description |
|-------------|-------------|
| `external_ids_syncer/logical_router_policy/` | One-time syncer that updates legacy LRP external IDs to current format on startup. |
| `external_ids_syncer/nat/` | One-time syncer that updates legacy NAT external IDs to current format on startup. |

## Testing

Unit tests use Ginkgo suites alongside the code. Coverage is broad:
root-level integration-style controller tests, plus focused tests in
nearly every sub-package (services, network connect, ANP, egress
firewall, route import, zone interconnect, address sets, logical switch
manager, etc.). A fake libovsdb client (`pkg/testing/libovsdb/`) is
used extensively for OVN DB interactions in tests.
