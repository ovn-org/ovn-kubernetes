# AGENTS.md — clustermanager

Cluster-scoped orchestration for OVN-Kubernetes. See
[`go-controller/pkg/AGENTS.md`](../AGENTS.md) for the package directory
overview and [`go-controller/AGENTS.md`](../../AGENTS.md) for component
architecture, codegen, and code conventions.

## Purpose

The `clustermanager` package implements the **ovnkube-cluster-manager**
component. It runs once per cluster (leader-elected) and handles
cluster-wide allocation and coordination tasks that must not be
duplicated across nodes:

- Per-node subnet, tunnel-ID, and network-ID allocation (L3 networks).
- Cluster-wide pod IP allocation and annotation (L2 networks).
- Zone and node ID assignment for multi-zone interconnect.
- User Defined Network (UDN) lifecycle — reconciling
  `UserDefinedNetwork`/`ClusterUserDefinedNetwork` CRDs into
  `NetworkAttachmentDefinition` resources with allocated subnets.
- Cluster Network Connect (CNC) — subnet and tunnel-key allocation for
  connecting isolated UDNs.
- EgressIP node assignment and reachability tracking.
- EgressService host selection.
- Route advertisement, managed BGP, no-overlay validation, and VTEP
  controllers.
- DNS name resolver alignment with EgressFirewall DNS usage.
- EndpointSlice mirroring for primary UDN namespaces.
- Status aggregation across zones.

The cluster manager interacts with the Kubernetes API only — it has no
direct access to OVN NB/SB databases.

## Design Decisions

- **Why cluster-wide allocation is centralized** — Subnet, tunnel-ID,
  and network-ID allocation must be conflict-free across all nodes. A
  leader-elected singleton avoids split-brain allocation races that
  would occur if each node allocated independently.

- **Why L2 IPAM lives here, but L3 IPAM lives in ovnkube-controller** —
  L3 networks carve per-node subnets, so pod IPAM is node-local (handled
  by ovnkube-controller's logical switch manager). L2/localnet networks
  have a single flat address space with no per-node partitioning, so a
  central allocator is required to prevent IP conflicts.

- **Why no OVN DB access** — The cluster manager only needs to set K8s
  annotations and status. Keeping it decoupled from OVN DB means it can
  run anywhere (including without OVN connectivity), reduces blast
  radius, and simplifies leader election since there is no DB state to
  reconcile on failover.

- **Per-network controller pattern** — One `networkClusterController` is
  created per network (default + each UDN). This allows per-network
  lifecycle, independent allocation pools, and clean teardown without
  cross-network interference.

- **Why status aggregation is centralized (and its scale cost)** — In
  multi-zone deployments, each zone's ovnkube-controller reports partial
  status. The cluster manager aggregates these per-zone reports into a
  single coherent status, since it is the only component with a
  cluster-wide view. However, this per-zone status aggregation pattern
  has scale implications: each zone update triggers an API write to the
  status object, and `managedFields` entries grow with the number of
  nodes. Newer features are moving away from this pattern — for example,
  OKEP-5552 (Dynamic UDN Node Allocation) chose K8s events over
  per-node status to avoid scale issues, and OKEP-5088 (EVPN) rejected
  per-node status in VTEP objects for the same reason. The long-term
  direction is to avoid per-zone/per-node status aggregation where
  possible.

- **NADs are an implementation detail, not a user API** — Users create
  `UserDefinedNetwork` or `ClusterUserDefinedNetwork` CRDs. The cluster
  manager's UDN controller renders the corresponding
  `NetworkAttachmentDefinition` (NAD) from the CRD spec and creates it
  in the appropriate namespace. ovnkube-controller and ovnkube-node
  only **watch** NADs via `pkg/networkmanager`'s `nadController` to
  discover network definitions and start/stop per-network controllers.
  If a NAD is deleted externally, the cluster manager re-creates it.
  This separation means the NAD is an internal handoff mechanism
  between the cluster manager (producer) and the zone components
  (consumers), not something users should create or modify directly.

- **CRDs over annotations for newer APIs** — Early OVN-Kubernetes
  features relied on node/pod annotations and NADs for configuration.
  NADs have no schema validation, limited RBAC controls, and no status
  subresource. Newer features use typed CRDs instead:
  `UserDefinedNetwork` replaced direct NAD creation for tenants,
  `UplinkState` models per-node gateway state to avoid annotation
  sprawl, and `RouteAdvertisements` express BGP intent declaratively.
  The direction is CRDs for any user-facing or multi-writer API
  surface.

- **Interconnect mode and why zones exist** — The original
  architecture used a single centralized OVN Northbound database and
  RAFT for HA, which suffered from split-brain issues and poor
  scaling. Interconnect mode replaced this with per-zone OVN NB/SB
  databases (one zone per node by default). Each zone's
  ovnkube-controller manages only its local database, and OVN
  Interconnect handles cross-zone traffic via geneve tunnels. The
  cluster manager exists partly to coordinate across zones (allocations,
  status) since individual zone controllers only see their local DB.

- **Designed to be lightweight** — Because the cluster manager is a
  centralized singleton, it must stay lightweight to avoid becoming a
  bottleneck or a scalability concern. It deliberately avoids OVN DB
  access, heavy reconciliation loops, and per-pod processing wherever
  possible. Its work is primarily cluster-scoped allocations (subnets,
  IDs), CRD reconciliation, and status aggregation — all of which
  operate on coarse-grained objects (nodes, networks) rather than
  fine-grained ones (pods, ports). The exception is L2 IPAM, which
  requires per-pod IP allocation centrally because L2 networks have no
  per-node subnet partitioning. Any new feature that would add per-pod
  or high-frequency work should go into the per-zone
  ovnkube-controller instead.

## Key Types

| Type | Description |
|------|-------------|
| `ClusterManager` | Top-level orchestrator. Owns all sub-controllers. `Start()` brings up the watch factory, network manager, and feature controllers in order; `Stop()` tears them down. |
| `networkClusterController` | Per-network allocator/reconciler. Manages node subnet allocation (L3), cluster-wide pod IP allocation (L2), tunnel-ID allocation, IPAM claims, and per-network status reporting. One instance per network. |
| `zoneClusterController` | Assigns unique node IDs and transit-switch-port addresses for zone interconnect. |
| `egressIPClusterController` | Tracks EgressIP assignments and node reachability (gRPC health checks). Programs node annotations, not OVN DB. |
| `userDefinedNetworkClusterManager` | Creates and deletes `networkClusterController` instances for UDN networks. |
| `NetworkStatusReporter` | Callback type used to report subsystem conditions on UDN status. |

## Lifecycle

```text
NewClusterManager()
  → allocates tunnelKeysAllocator, networkManager, statusManager
  → creates defaultNetClusterController, zoneClusterController
  → conditionally creates feature controllers (EgressIP, EgressService,
    UDN, NetworkConnect, RouteAdvertisements, ManagedBGP, NoOverlay, VTEP,
    EndpointSliceMirror, DNSNameResolver)

Start(ctx)
  → wf.Start()                        — start watch factory
  → networkManager.Start()            — NAD detection and controller lifecycle
  → nodeController.Start()            — shared node reconciliation
  → defaultNetClusterController.Start — default network allocations
  → zoneClusterController.Start       — zone/node ID assignment
  → feature controllers .Start()      — EgressIP, EgressService,
                                        EndpointSliceMirror, StatusManager,
                                        DNSNameResolver, UDN, NetworkConnect,
                                        ManagedBGP, RouteAdvertisements,
                                        NoOverlay, VTEP

Stop()
  → tears down all controllers in reverse order
```

## Sub-packages

| Sub-package | Description |
|-------------|-------------|
| `node/` | Node-level subnet and tunnel-ID allocation primitives (`NodeAllocator`). Used by `networkClusterController` to carve per-node subnets from the cluster CIDR. |
| `pod/` | Pod IP allocation and annotation for L2/localnet networks (`PodAllocator`). Coordinates with IPAM claims for persistent IPs. |
| `status_manager/` | Aggregates per-zone status for network resources. Cleans up stale `managedFields` entries from old zone controllers. |
| `status_manager/zone_tracker/` | Tracks which zones exist based on node annotations. |
| `userdefinednetwork/` | Reconciles `UserDefinedNetwork` and `ClusterUserDefinedNetwork` CRDs. Renders `NetworkAttachmentDefinition` manifests, manages EVPN VID/VNI reservations, and validates transport/topology constraints. |
| `userdefinednetwork/notifier/` | Fanout notifier for UDN status updates to dependent controllers. |
| `userdefinednetwork/template/` | NAD manifest rendering from UDN specs. |
| `networkconnect/` | Reconciles `ClusterNetworkConnect` CRDs — allocates subnets and tunnel keys, creates NADs, and manages namespace-driven reconciliation. |
| `routeadvertisements/` | Reconciles `RouteAdvertisements` CRDs with FRR config, EgressIP, NAD, node, and namespace triggers. |
| `managedbgp/` | Creates managed no-overlay BGP topology resources (FRR configs, RouteAdvertisements). |
| `nooverlay/` | Validates that no-overlay transport configuration is backed by valid RouteAdvertisements state. |
| `vtep/` | Validates and finalizes VTEP CRs for EVPN, checks EVPN dependencies. |
| `endpointslicemirror/` | Mirrors default-network EndpointSlices into primary UDN namespaces. |
| `dnsnameresolver/` | Keeps `DNSNameResolver` resources aligned to EgressFirewall DNS usage. |
| `egressservice/` | Cluster-level EgressService host selection and reachability bookkeeping. |

## Testing

Unit tests use Ginkgo suites and live alongside the code. Most
sub-packages have their own `*_suite_test.go` and focused unit tests
for allocators and reconcilers. Root-level tests cover the full
`ClusterManager` startup and `networkClusterController` reconciliation.
