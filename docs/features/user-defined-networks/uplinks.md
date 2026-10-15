# Uplinks for User Defined Networks

## Introduction

An `Uplink` is a cluster-scoped API that lets a
`ClusterUserDefinedNetwork` use a named external network path instead of the
cluster default gateway bridge. This is useful when different tenant networks
need different north-south paths, different Linux VRFs, or different BGP
peering interfaces.

An `Uplink` does not create or manage the external OVS bridge. The bridge and
its physical uplink must already exist on the selected nodes. OVN-Kubernetes
discovers the bridge, records node-local state in `UplinkState`, configures OVN
bridge mappings, and programs the gateway and service flows needed by the CUDN.

## Enabling the Feature

The Uplink feature is opt-in and disabled by default. It is a sub-feature of
network segmentation and is only active when both network segmentation and the
Uplink feature flag are enabled.

Enable it by passing `--enable-uplink` to the ovnkube components (this requires
`--enable-network-segmentation`, which in turn requires
`--enable-multi-network`). Depending on how the cluster is deployed:

* Helm: set all three global values:

```yaml
  global:
    enableMultiNetwork: true
    enableNetworkSegmentation: true
    enableUplink: true
```

* kind: pass `--multi-network-enable` (`-mne`), `--network-segmentation-enable`
  (`-nse`), and `--uplink-enable` (`-ue`) to `contrib/kind.sh`

A `ClusterUserDefinedNetwork` keeps its `spec.uplinks` field regardless of the
feature flag, but the reference is only acted upon when the feature is enabled.
When the feature is disabled, a `ClusterUserDefinedNetwork` that sets
`spec.uplinks` is **rejected** rather than silently ignored: the CUDN's
`NetworkCreated` condition is set to `False` with reason
`NetworkAttachmentDefinitionSyncError` and the message
`spec.uplinks is set but the Uplink feature is disabled, ignoring this CUDN`, and no
`NetworkAttachmentDefinition` is created for it. Enabling network segmentation
alone is not sufficient — `--enable-uplink` must also be set.

## Supported Scope

This feature currently supports:

* `ClusterUserDefinedNetwork` only.
* Primary `Layer2` and primary `Layer3` CUDNs.
* One Uplink name in `ClusterUserDefinedNetwork.spec.uplinks`.
* `Uplink.spec.nodeConfigs[].type: OVSBridge`.
* Shared gateway mode.
* Regular nodes and DPU deployments.

This feature does not currently support:

* Namespace-scoped `UserDefinedNetwork`.
* Secondary networks.
* `Localnet` topology.
* Local gateway mode.
* EVPN transport.
* Creating or configuring the external OVS bridge.
* Reusing the cluster default shared gateway bridge as an Uplink.
* More than one Uplink per CUDN.

## API Overview

`Uplink` selects node groups and names the host-visible interface used for each
selected node:

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: Uplink
metadata:
  name: tenant-blue
spec:
  nodeConfigs:
  - type: OVSBridge
    nodeSelector:
      matchLabels:
        example.com/tenant-blue-uplink: "true"
    hostInterfaceName: ovsbr1
```

`hostInterfaceName` is the host-side Linux interface that carries the gateway
L3 identity for the uplink:

* On a regular non-accelerated node, this is normally the OVS bridge internal
  interface. The physical NIC must already be a port on that bridge.
* On a SmartNIC accelerated node, this is the VF/SF netdevice that carries the
  gateway IPs. Its representor must already be a port on the OVS bridge.
* On a DPU-host node, this is the host-visible interface connected to the DPU
  uplink path. OVN-Kubernetes uses it to discover the host-side L3 state.
* On the DPU side, OVN-Kubernetes resolves the DPU-local OVS bridge from the
  host-side state. The DPU-local interface name is not specified in the API.

Each selected node must match at most one `nodeConfig` in a single `Uplink`.
If multiple `nodeConfigs` select the same node, the `Uplink` is not ready and a
CUDN that needs that node reports `UplinksReady=False`.

A CUDN references the Uplink by name:

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: blue
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: blue
  uplinks:
  - tenant-blue
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.200.0.0/16
        hostSubnet: 24
```

## Regular Node Deployments

For non-DPU deployments, the administrator provisions the OVS bridge before the
`Uplink` is used. For non-accelerated nodes, the bridge must have:

* An internal interface named by `hostInterfaceName`.
* One physical uplink port that reaches the external network.
* The expected IP addresses on the internal interface. Default gateways are
  optional: an uplink without them is valid, and discovery polls for missing
  gateways (see the default gateway discussion under DPU
  Deployments, which applies to regular deployments as well).
* Link state up on the bridge, internal interface, and physical uplink.
* MTU configured consistently on the physical NIC, bridge, and bridge ports.

For SmartNIC accelerated nodes, `hostInterfaceName` is the VF/SF netdevice with
the gateway IPs. Its representor must be a port on the OVS bridge. OVN-Kubernetes
discovers the representor from `hostInterfaceName`, then discovers the OVS
bridge from the representor.

OVN-Kubernetes discovers the bridge from `hostInterfaceName` or from its
representor. It rejects the configuration if the interface is the physical
uplink port itself, if the bridge cannot be resolved, if the bridge is `br-int`,
or if the bridge has no discoverable physical uplink.

After discovery succeeds, ovnkube-node updates the per-node `UplinkState` with
the resolved bridge name, MAC address, IP addresses, and default gateways.
OVN-Kubernetes then uses that state to build the CUDN gateway configuration.

## DPU Deployments

DPU deployments split Uplink discovery across the host and DPU components:

* The DPU-host ovnkube-node reads `hostInterfaceName`, discovers the host-side
  MAC address, IP addresses, and default gateways, and writes that data to
  `UplinkState`. When the host interface is an SR-IOV function, it also
  writes the PF index — and, for a VF, the VF index — to
  `status.hostFunction`.
* The DPU ovnkube-node reads the same `UplinkState` and finds the DPU-local
  representor and OVS bridge: with `status.hostFunction` it resolves the PF
  or VF representor directly from the published indices, and a representor
  resolved this way must peer with the published host MAC, the uplink's
  identity, whenever that peer MAC is readable. Without
  `status.hostFunction`, and when the published indices do not resolve, it
  scans bridges for the representor whose host-side peer has the published
  MAC address. Index resolution covers PF indices 0 and 1 on the default
  controller; other layouts resolve through the MAC scan. It then validates
  the bridge and writes `status.ovsBridge.name`.
* Once both sides have published the required data, the `UplinkState` becomes
  `Resolved=True`.

The user does not provide the DPU-local OVS bridge name in the `Uplink` API.
The DPU side derives it from the DPU-local OVS bridge state. The bridge must
already be provisioned with the host representor or bridge port that connects
to the host uplink path.

The same MTU requirement applies in DPU deployments. The host-side interface,
DPU representor, DPU OVS bridge, and physical uplink must be configured
consistently by the platform.

Host-originated traffic towards the uplink (host services, node processes)
follows the host routing tables. On-link destinations are reachable with no
extra configuration; only when such traffic must reach destinations beyond
the uplink subnet does it need a suitable host route, such as a specific
static route or a default route on the host interface.
Alternatively, a BGP speaker on the host can learn those routes from FRR on
the DPU; setting that up is outside the scope of OVN-Kubernetes.
Pod-to-uplink traffic is forwarded by the OVN gateway router on the DPU
without a host routing lookup. The gateway router uses the discovered default
gateways and, for advertised networks, imported FRR-learned routes. Programming
the discovered defaults still depends on host route discovery; the packets
themselves do not traverse host routing. An `UplinkState` with an empty
`status.defaultGateways` reports that no default gateway route was discovered
on the host interface; discovery keeps re-polling the host routes (in the
interface's VRF routing table when it is enslaved to a VRF) while an addressed
IP family lacks a gateway, and publishes newly discovered default gateways.
Discovery selects the lowest-metric defaults per IP family through the selected
host interface, including embedded multipath next hops. When those next hops
carry unequal weights, only the heaviest ones are kept: neither `UplinkState`
nor the programmed routes can express weights, so the lighter paths are omitted
rather than promoted to equal-cost paths. It publishes the distinct gateways in
deterministic order, up to 256 total across both IP families per node and
Uplink. Each network on the uplink installs the published gateways for its
configured IP families as default routes on its OVN gateway router.

With VRF-Lite (`targetVRF: auto`), OVN-Kubernetes moves the selected host
interface into the CUDN VRF and preserves its existing static and DHCP routes,
including default routes. Discovery reads those defaults from the CUDN VRF
and publishes the gateways for use by the OVN gateway router.

An uplink without a default gateway is valid, so no condition degrades when
one is missing. Platform monitoring can alert on an empty
`status.defaultGateways` where a default gateway is expected.

Polling stops once every addressed family has a gateway; later changes need
another reconciliation trigger ([#6784](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6784)).
The API limit of 256 gateways accommodates 128 per family, twice
[Cumulus Linux's default of 64 BGP multipaths](https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-518/Layer-3/Border-Gateway-Protocol-BGP/Optional-BGP-Configuration/),
and discovery reports `GatewayInfoUnavailable` if the limit is exceeded.

The two traffic paths toward the uplink:

```mermaid
flowchart TB
    host["host process<br/>(off-subnet destination)"] -->|"host routing tables:<br/>static or BGP-learned routes"| uplink["physical uplink"]
    pod["pod"] -->|OVN datapath| gr["OVN gateway router (DPU)"]
    gr -->|"discovered default gateways and<br/>FRR-learned routes (advertised networks)"| uplink
```

Default gateway discovery for a VRF-Lite Uplink:

```mermaid
flowchart LR
    poll["discovery reads the interface's<br/>routes in the CUDN VRF"] -->|no default route| empty["status.defaultGateways: []<br/>no condition degrades"]
    empty -->|re-poll| poll
    poll -->|default route present| pub["status.defaultGateways<br/>published"]
    pub --> gr["default routes on the<br/>OVN gateway router"]
```

## UplinkState and Conditions

`UplinkState` is a cluster-scoped, per-node state object. Its name is derived
from the Uplink name and node name. It is owned by the `Uplink`.

The required, immutable `spec.uplinkName` and `spec.nodeName` fields are the
canonical identity. `UplinkState` does not use a status subresource, allowing
OVN-Kubernetes to create the identity and initial status in one API operation.

Example:

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: UplinkState
metadata:
  name: tenant-blue.worker-a
spec:
  uplinkName: tenant-blue
  nodeName: worker-a
status:
  type: OVSBridge
  hostInterfaceName: ovsbr1
  ovsBridge:
    name: ovsbr1
  macAddress: 02:42:ac:1c:00:02
  ipAddresses:
  - 172.28.0.2/16
  defaultGateways:
  - 172.28.0.1
  conditions:
  - type: Resolved
    status: "True"
    reason: Resolved
  - type: GatewayReady
    status: "True"
    reason: GatewayConfigured
```

The `Resolved` condition reports node-local host interface and OVS bridge
discovery. The `GatewayReady` condition reports aggregate results from CUDN
gateway programming on this Uplink and node. This includes bridge mappings,
physical patch ports, per-network bridge configuration, OpenFlow, and VRF
attachment. With no reported CUDNs, `GatewayReady=True` with reason
`NoActiveCUDNs`. A successful CUDN result changes the reason to
`GatewayConfigured`; a reported programming failure changes the condition to
`False` with a reason that identifies the failing operation. A gateway
programming failure does not change discovery `Resolved`; the CUDN-specific
`UplinksReady` condition reflects both states. In split DPU mode the host-side
share of gateway programming, the VRF attachment of the Uplink gateway
interface on the DPU-Host, is reported separately on the `HostGatewayReady`
condition, and the CUDN `UplinksReady` condition requires both.

The `Uplink` object reports aggregate status:

* `Ready=True` means every selected node has successfully discovered a
  matching `UplinkState` with `Resolved=True`.
* `Ready=False` means one or more selected nodes are missing or have failed
  Uplink discovery, or the `Uplink` spec selects nodes incorrectly.

For node-scoped failures, the `Ready` message reports the number of affected
nodes out of all selected nodes and a bounded sample of node names and
underlying `UplinkState` reasons. `GatewayReady` is not part of this aggregate;
it is evaluated for the active nodes of each CUDN.

`UplinkState` supports field selection by Uplink name and node name:

```bash
$ kubectl get uplinkstate --field-selector spec.uplinkName=tenant-blue
NAME                   UPLINK        NODE       RESOLVED
tenant-blue.worker-a   tenant-blue   worker-a   True
tenant-blue.worker-b   tenant-blue   worker-b   True

$ kubectl get uplinkstate --field-selector spec.uplinkName=tenant-blue,spec.nodeName=worker-a -o yaml
apiVersion: v1
items:
- apiVersion: k8s.ovn.org/v1alpha1
  kind: UplinkState
  metadata:
    name: tenant-blue.worker-a
  spec:
    uplinkName: tenant-blue
    nodeName: worker-a
  status:
    type: OVSBridge
    hostInterfaceName: ovsbr1
    ovsBridge:
      name: ovsbr1
    conditions:
    - type: Resolved
      status: "True"
      reason: Resolved
    - type: GatewayReady
      status: "True"
      reason: GatewayConfigured
kind: List
metadata:
  resourceVersion: ""
```

When one or more CUDNs reference an `Uplink`, OVN-Kubernetes keeps a finalizer
on the `Uplink` so it cannot be deleted while still selected by a CUDN.

`UplinkState` objects are exclusively owned by OVN-Kubernetes and recover from
out-of-band deletion: if an `UplinkState` is deleted directly (for example with
`kubectl delete`), ovnkube-node recreates it without a restart, discovery
republishes `Resolved=True`, and each active CUDN reports its gateway result
against the recreated object's UID and configuration. The CUDN returns to
`UplinksReady=True` after that report. An `UplinkState` is not recreated when
its `Uplink` is terminating (its `UplinkState` objects are deleted on purpose
during teardown), no longer exists, or no longer selects the node.

The OVS bridge an `Uplink` selects is administrator-owned and may be deleted
while a CUDN is still active on it. ovnkube-node keeps running: discovery
reports `Resolved=False`, the active CUDNs tear down their gateway programming
on that node and report `GatewayReady=False`, and the OpenFlow manager stops
syncing flows to the missing bridge until the CUDNs are gone from it. Once the
bridge is recreated and resolved again, the CUDNs are reprogrammed through the
usual configuration-change path.

The CUDN reports a CUDN-specific `UplinksReady` condition. This condition is
computed from the active nodes for that CUDN, not directly from aggregate
`Uplink.status`. This matters with Dynamic UDN, where two CUDNs can reference
the same `Uplink` but be active on different nodes.

For a CUDN that does not set `spec.uplinks`, `UplinksReady=True` and existing
gateway behavior is preserved.

### Gateway Lifecycle and Configuration Changes

OVN-Kubernetes keeps gateway programming for active CUDNs synchronized with
the resolved Uplink configuration. If the selected host interface, OVS bridge,
MAC address, IP addresses, or default gateways change, each active CUDN is
reprogrammed and reports its result for that exact UplinkState object and input
configuration. Results from a deleted, recreated, or subsequently changed
UplinkState are discarded. A failed current reconfiguration is visible through
`GatewayReady=False` and is retried.

`GatewayReady` is an Uplink/node aggregate rather than per-CUDN status. When no
CUDN has reported programming, `NoActiveCUDNs` keeps a newly active CUDN's
`UplinksReady` condition false until its first result arrives. If other CUDNs
already report successful programming on the same Uplink and node, adding one
more CUDN does not temporarily change the shared condition to false. A later
failure from that CUDN changes the aggregate condition to false. Precise
in-progress readiness for each CUDN would require per-CUDN, per-node status.

If an Uplink stops selecting a node, ovnkube-node withdraws the active CUDN
gateway programming before deleting the node's `UplinkState`. Selecting the
node again creates a fresh lifecycle and programs the gateways from newly
resolved state. By contrast, recreating an `UplinkState` with an unchanged
resolved configuration restores its readiness condition without disrupting
working gateway programming.

## Dynamic UDN

With Dynamic UDN enabled, OVN-Kubernetes delays Uplink gateway programming on a
node until the CUDN becomes active on that node. Inactive nodes do not block the
CUDN `UplinksReady` condition.

If a node later becomes inactive for the CUDN, OVN-Kubernetes does not
immediately remove the Uplink bridge configuration. The normal Dynamic UDN
inactive-network cleanup behavior applies to the CUDN network state.

See [Dynamic UDN Node Allocation](dynamic-udn.md) for the node activity model.

## Route Advertisements and VRFs

The Uplink feature works with RouteAdvertisements. The selected
RouteAdvertisements determine whether the Uplink bridge stays in the default
VRF or is attached to the CUDN VRF.

When a CUDN using an Uplink is advertised through the default VRF,
OVN-Kubernetes leaves the Uplink bridge in the default VRF. Routes learned by
FRR in the default VRF can be used for the CUDN, and pod egress uses the Uplink
gateway path.

When a CUDN using an Uplink is advertised with `targetVRF: auto`,
OVN-Kubernetes treats the Uplink as the VRF-Lite path for that CUDN. The Uplink
bridge interface is enslaved to the CUDN Linux VRF on regular nodes. In DPU
deployments, the DPU-local bridge interface is attached to the CUDN VRF on the
DPU side. FRR peers in that VRF, and ovnkube reflects the routes learned in
that VRF into the OVN gateway routers for the CUDN.

On enslavement and release, OVN-Kubernetes migrates the interface's eligible
unicast routes into and out of the VRF routing table. This includes the
kernel-protocol prefix routes of `noprefixroute` addresses (for example on
NetworkManager-managed interfaces) and excludes routes that OVN-Kubernetes or
routing daemons such as FRR program per routing domain themselves. The
migration is one-shot: an agent that keeps reconciling its routes only into
the main table, such as NetworkManager on a lease renewal, leaves the migrated
copy stale until the next enslavement.

```text
             OVN-Kubernetes           kernel              main-table agent
 enslave     capture main routes ---> purges iface routes
             restore into VRF
 (running)                                                updates main only:
                                                          VRF copy now stale
 release     capture VRF routes ----> purges iface routes
             restore into main
 re-enslave  capture main routes ---> purges iface routes
             restore into VRF: copy fresh again
```

Multiple Dynamic CUDNs may use the same Uplink with `targetVRF: auto` when
their active node sets do not overlap. If more than one such CUDN becomes
active on the same node, the Uplink bridge cannot be attached to both CUDN
VRFs and OVN-Kubernetes reports `UplinkConfigurationConflict`.

For every primary CUDN, OVN-Kubernetes publishes the name of the Linux VRF
device it creates on every node where the network is present in the CUDN
`status.vrfName` field. Consumers that must name the VRF explicitly, such as
FRRConfiguration authors filling in the routers `vrf` field, should read this
value instead of deriving it: the derivation rule (the CUDN name when it fits
within the kernel's 15-character interface name limit, an ID-derived name
otherwise) is an internal implementation detail that may change.

Example RouteAdvertisements for VRF-Lite:

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: blue
spec:
  targetVRF: auto
  networkSelectors:
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          advertise: blue
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      network: blue
  advertisements:
  - PodNetwork
```

The Uplink object does not decide which routes exist. Routes come from the
kernel routing table and from RouteAdvertisements and FRR configuration.

## Services

OVN-Kubernetes programs service flows on the external bridge associated with
the CUDN. For a CUDN that uses an Uplink, per-UDN service flows are programmed
on the resolved Uplink bridge rather than only on the cluster default gateway
bridge.

For default-VRF Uplinks, NodePort traffic can enter from the cluster default
gateway path or from the Uplink path and still reach services backed by pods on
the Uplink CUDN.

For VRF-Lite Uplinks, service traffic is expected to enter through an interface
that belongs to the same VRF as the CUDN. Traffic that enters from an unrelated
VRF is not expected to reach services for that CUDN.

## Troubleshooting

Check the aggregate Uplink status:

```bash
kubectl get uplink tenant-blue -o yaml
```

Check the per-node discovery state:

```bash
kubectl get uplinkstate --field-selector spec.uplinkName=tenant-blue -o wide
kubectl get uplinkstate tenant-blue.worker-a -o yaml
```

Check the CUDN condition:

```bash
kubectl get clusteruserdefinednetwork blue -o yaml
```

Common problems:

* `Uplink` with `Ready` condition status `False` and reason
  `MissingUplinkState`: the selected node has not created or published its
  `UplinkState`.
* `UplinkState` with `Resolved` condition status `False` and reason
  `HostInterfaceNotFound`: the selected `hostInterfaceName` does not exist on
  the node.
* `UplinkState` with `Resolved` condition status `False` and reason
  `BridgeNotFound`: OVN-Kubernetes could not map `hostInterfaceName` to an OVS
  bridge.
* `UplinkState` with `Resolved` condition status `False` and reason
  `BridgeUplinkNotFound`: the resolved OVS bridge does not have a discoverable
  physical uplink port.
* `UplinkState` with `Resolved` condition status `False` and reason
  `BridgeInvalid`: the selected bridge is not a valid external bridge, for
  example `br-int`.
* `UplinkState` with `Resolved` condition status `False` and reason
  `DefaultGatewayBridgeUnsupported`: the selected bridge is the cluster
  default shared gateway bridge, which cannot currently be reused as an
  Uplink.
* `UplinkState` with `Resolved` condition status `False` and reason
  `WaitingForDPUHost` (split DPU mode only): the DPU side is waiting for the
  DPU-Host to publish complete host interface data. Check the `HostDataReady`
  condition on the same `UplinkState` for the DPU-Host-side cause.
* `UplinkState` with `HostDataReady` condition status `False` (split DPU mode
  only): host interface data discovery failed on the DPU-Host; the reason
  carries the cause, for example `HostInterfaceNotFound` when the selected
  `hostInterfaceName` does not exist on the DPU-Host, or
  `GatewayInfoUnavailable` when the interface has no IP addresses yet.
* `UplinkState` with `GatewayReady` condition status `True` and reason
  `NoActiveCUDNs`: no active CUDN gateway has reported programming on this
  Uplink and node. An active CUDN waits for its first result before reporting
  `UplinksReady=True`.
* `UplinkState` with `GatewayReady` condition status `False` and reason
  `UplinkBridgeMappingFailed`: OVN-Kubernetes could not configure the CUDN
  bridge mappings or discover a physical patch port on the resolved OVS bridge.
* `UplinkState` with `GatewayReady` condition status `False` and reason
  `UplinkVRFAttachmentFailed`: OVN-Kubernetes could not attach or detach the
  Uplink gateway interface for the CUDN routing domain.
* `UplinkState` with `GatewayReady` condition status `False` and reason
  `UplinkGatewayProgrammingFailed`: OVN-Kubernetes could not reconcile the
  per-network bridge configuration or gateway OpenFlow for the active CUDNs.
* `UplinkState` with `GatewayReady` condition status `False` and reason
  `UplinkConfigurationConflict`: the resolved Uplink bridge is already
  attached to a different VRF on this node.
* `UplinkState` with `HostGatewayReady` condition status `False` (split DPU
  mode only): host-side gateway programming failed on the DPU-Host, for
  example reason `UplinkVRFAttachmentFailed` when the Uplink gateway
  interface could not be attached to or detached from the CUDN routing
  domain on the DPU-Host.
* CUDN with `UplinksReady` condition status `False` and reason
  `UplinkNotResolvedForNode`: at least one active node for the CUDN does not have
  a matching `UplinkState` with `Resolved=True`.
* CUDN with `UplinksReady` condition status `False` and reason
  `UplinkNotFoundForNode`: the referenced Uplink does not select an active node
  for the CUDN.
* CUDN with `UplinksReady` condition status `False` and reason
  `UplinkOverlapOnNode`: multiple `nodeConfigs` in the referenced Uplink select
  the same active node.
* CUDN with `UplinksReady` condition status `False` and reason
  `UplinkConfigurationConflict`: the CUDN is part of an unsupported same-node
  Uplink bridge sharing configuration.
* CUDN with `UplinksReady` condition status `False` and reason
  `UplinkUnsupportedGatewayMode`: the cluster is not running shared gateway
  mode.
* CUDN with `UplinksReady` condition status `False` and reason
  `UplinkUnsupportedTransport`: the CUDN uses EVPN transport, which is not
  supported with Uplinks.

In split DPU mode each `UplinkState` condition has a single writer: the
DPU-Host publishes the host interface data and the `HostDataReady` and
`HostGatewayReady` conditions, while ovnkube-node on the DPU publishes
`Resolved` and `GatewayReady`. Bridge reasons (`BridgeNotFound`,
`BridgeUplinkNotFound`) therefore always describe OVS on the DPU, and
`HostDataReady`/`HostGatewayReady` reasons always describe the DPU-Host.

In DPU deployments, FRR peering and route import happen on the DPU:
BGP-learned routes are not propagated to the DPU-Host's CUDN VRF.
Host-originated traffic toward destinations beyond the host interface's
subnet therefore requires a suitable host route: a static route, a default
route preserved into the CUDN VRF across enslavement, or a route learned by a
separately configured host BGP speaker. On-link host traffic uses the connected
subnet route. Pod traffic uses the DPU gateway router, whose routes come from
the published default gateways and, for advertised networks, imported BGP
routes.

```text
             DPU                                DPU-Host
 +---------------------------+      +----------------------------+
 | FRR --- BGP peering       |      | CUDN VRF                   |
 |          |                |      |  - no BGP-learned routes   |
 |          v                |      |  - preserved default       |
 | CUDN route-import VRF     |      |    gateway route           |
 |          | imported       |      |    (or host-side FRR)      |
 |          v                |      +-------------+--------------+
 | CUDN gateway router (OVN) |                    |
 +-------------+-------------+                    | host-originated
               | pod traffic                      | traffic
               v                                  v
             Uplink <-----------------------------+
```

## References

* [User Defined Networks](user-defined-networks.md)
* [Dynamic UDN Node Allocation](dynamic-udn.md)
* [Route Advertisements](../bgp-integration/route-advertisements.md)
* [DPU support](../hardware-offload/dpu-support.md)
