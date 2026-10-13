# EVPN

## Introduction

The EVPN feature extends OVN-Kubernetes BGP support to carry primary cluster
user-defined network (P-CUDN) traffic as EVPN VPNs using VXLAN. It allows
exposing P-CUDNs externally via a VPN to other entities inside or outside the
cluster, providing an industry-standardized way to achieve network segmentation
between sites.

EVPN provides two types of VPNs:

- **MAC-VRF**: stretches a Layer 2 segment across the EVPN fabric, enabling
  east/west bridged traffic as well as VM live migration.
- **IP-VRF**: provides Layer 3 routing across the EVPN fabric, enabling routed
  north/south traffic.

When EVPN is enabled for a network, VXLAN replaces Geneve as the overlay,
eliminating the double-encapsulation overhead that occurs when running Geneve
inside an EVPN fabric. FRR acts as the BGP EVPN control plane and integrates
with Linux netdevs to provide the data plane.

> [!NOTE]
> For purposes of this documentation, the external, physical network of the
> cluster which a user administers will be called the "provider network".

## Prerequisites

- [Route Advertisements](./route-advertisements.md) feature enabled and
  understood.
- [FRR-k8s](https://github.com/metallb/frr-k8s) deployed.
- FRR 10+ (required for Single VXLAN Device mode).
- Local gateway mode.

Always check the dependencies on the [Requirements page](../requirements.md).

## User-Stories/Use-Cases

- As a user, I want to connect my Kubernetes cluster to VMs or physical hosts on
  an external EVPN fabric, preserving network isolation between tenants.
- As a user, I want to migrate VMs from my external network onto the Kubernetes
  platform preserving IP and MAC address reachability.
- As a user, my data center already uses EVPN. I want to eliminate double
  encapsulation (VXLAN + Geneve) and integrate natively with my networking
  fabric.
- As a user, I want to create overlapping IP address space UDNs and connect
  them to different external networks while preserving isolation.

## Workflow Description

A typical EVPN setup follows these steps:

1. Configure BGP peering via FRR-K8s `FRRConfiguration`.
2. Create a `VTEP` CR.
3. Create a `RouteAdvertisements` CR selecting the CUDN.
4. Create a P-CUDN with `transport: EVPN`, referencing the VTEP from
   step 2.

### Step 1: Configure BGP peering

Deploy an `FRRConfiguration` that establishes BGP peering with the provider
network. This defines the underlay router and neighbors that EVPN will use:

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  labels:
    use-for-advertisements: evpn
  name: evpn-peering
  namespace: frr-k8s-system
spec:
  nodeSelector: {}
  bgp:
    routers:
    - asn: 65000
      neighbors:
      - address: 192.168.122.12
        asn: 65001
```

The label (e.g. `use-for-advertisements: evpn`) is used by the
`RouteAdvertisements` CR in step 3 to select this configuration.

### Step 2: Create a VTEP

Create a `VTEP` CR to define the VXLAN Tunnel Endpoint IPs. The CIDRs specify
the address range from which VTEP IPs are allocated (managed) or discovered
(unmanaged). In unmanaged mode, the VTEP IP is assumed to already be configured
as a primary address on a dedicated device (e.g. a dummy interface) on each
node. As a special case of unmanaged mode, node subnet CIDRs can be used, in
which case the node IPs themselves serve as VTEP IPs.

```yaml
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: evpn-vtep
spec:
  cidrs:
  - 100.64.0.0/24
  mode: Unmanaged
```

> [!NOTE]
> Managed VTEP mode is not yet implemented. Currently only unmanaged mode is
> supported.

> [!NOTE]
> Using the node IP as VTEP IP should be avoided. Assigning the VTEP IP to a
> device without a physical link carrier (e.g. a dummy interface) keeps the
> address up when a physical link goes down, enabling EVPN multihoming and
> mass withdrawal failover.

### Step 3: Create RouteAdvertisements

Create a `RouteAdvertisements` CR that selects the EVPN CUDNs and references
the `FRRConfiguration` from step 1:

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: evpn-blue
spec:
  targetVRF: auto
  advertisements:
  - PodNetwork
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      use-for-advertisements: evpn
  networkSelectors:
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          evpn: enabled
```

The `networkSelector` matches CUDNs by label, so any CUDN with the
`evpn: enabled` label will have its routes advertised. The selected
`FRRConfiguration` must contain at least a default VRF router — the controller
extracts its neighbors to enable EVPN for them. For IP-VRF networks, if a
VRF-specific router matching the CUDN name exists, the IP-VRF EVPN router is
derived from it; otherwise it is derived from the underlay router. See
[FRR Configuration](#frr-configuration) for examples of both cases.

### Step 4: Create CUDN with EVPN transport

Create a P-CUDN with `transport: EVPN`, referencing the VTEP from step 2. The
`evpn` section configures the MAC-VRF and/or IP-VRF with a VNI (must be unique
across all EVPN configurations) and an optional route target (auto-derived to
`<ASN>:<VNI>` if omitted). The EVPN configuration varies depending on the
desired topology:

#### Layer 2 CUDN with MAC-VRF

Stretches the Layer 2 UDN across the EVPN fabric as a MAC-VRF. Pods on
different nodes and external entities can communicate at Layer 2:

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: blue
  labels:
    evpn: enabled
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: udn-test
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets:
      - 10.0.10.0/24
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
```

#### Layer 2 CUDN with MAC-VRF + IP-VRF

Adding an IP-VRF on top of the MAC-VRF enables Layer 3 routing for the same
network. This allows pods to reach external routed destinations via the IP-VRF
while maintaining Layer 2 reachability via the MAC-VRF:

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: blue
  labels:
    evpn: enabled
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: udn-test
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets:
      - 10.0.10.0/24
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      macVRF:
        vni: 100
        routeTarget: "65000:100"
      ipVRF:
        vni: 101
        routeTarget: "65000:101"
```

#### Layer 3 CUDN with IP-VRF

A Layer 3 UDN uses pure routing via the IP-VRF. Each node has its own Layer 2
domain and inter-node communication uses EVPN Type 5 routes:

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: red
  labels:
    evpn: enabled
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: udn-test
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.0.20.0/16
        hostSubnet: 24
    transport: EVPN
    evpn:
      vtep: evpn-vtep
      ipVRF:
        vni: 200
```

## Implementation Details

### API

The implementation of EVPN introduces the following OVN-Kubernetes API changes:

- **[VTEP](../../api-reference/vtep-api-spec.md)**: new cluster-scoped CRD
  defining VXLAN Tunnel Endpoint IPs for EVPN.
- **[ClusterUserDefinedNetwork](../../api-reference/userdefinednetwork-api-spec.md)**:
  new `transport: EVPN` option with `evpn` configuration section for MAC-VRF
  and IP-VRF settings.

The existing [RouteAdvertisements](../../api-reference/routeadvertisements-api-spec.md)
CRD is not changed but is required to select EVPN CUDNs for route
advertisement.

### Node Configuration

#### SVD Architecture

OVN-Kubernetes uses FRR's Single VXLAN Device (SVD) mode. A single Linux bridge
and VXLAN device are created per VTEP, shared by all EVPN networks using that
VTEP. VLANs within the bridge segment the traffic, and each VLAN is mapped to a
VNI on the VXLAN device. This is more scalable than the alternative Multiple
VXLAN Device (MVD) mode which requires a separate bridge and VXLAN per network.

The tradeoff is that VLANs are limited to 4094 per bridge, so at most 4094
MAC-VRF + IP-VRF combinations can be configured per VTEP. VLAN IDs (VIDs) are
allocated cluster-wide by the UDN controller to ensure consistent mapping across
nodes.

#### Devices

When a VTEP CR exists and a CUDN references it, ovnkube-node creates the
following Linux devices. The examples below assume a VTEP named `evpn-vtep`, a
Layer 2 CUDN named `blue` with MAC-VRF VNI 100 (VLAN 12) and IP-VRF VNI 101
(VLAN 11), and a VTEP IP of 100.64.0.1:

```bash
# VTEP IP assignment to a dummy device (managed mode only)
ip link add evlo-evpn-vtep type dummy
ip addr add 100.64.0.1/32 dev evlo-evpn-vtep

# SVD bridge + VXLAN setup (one per VTEP, shared by all networks)
ip link add evbr-evpn-vtep type bridge vlan_filtering 1 vlan_default_pvid 0
ip link set evbr-evpn-vtep addrgenmode none
ip link set evbr-evpn-vtep address aa:bb:cc:00:00:64
ip link add evx4-evpn-vtep type vxlan dstport 4789 local 100.64.0.1 nolearning external vnifilter
ip link set evx4-evpn-vtep addrgenmode none master evbr-evpn-vtep
ip link set evx4-evpn-vtep address aa:bb:cc:00:00:64
ip link set evbr-evpn-vtep up
ip link set evx4-evpn-vtep up
bridge link set dev evx4-evpn-vtep vlan_tunnel on neigh_suppress on learning off

# IP-VRF: map VLAN 11 <-> VNI 101
bridge vlan add dev evbr-evpn-vtep vid 11 self
bridge vlan add dev evx4-evpn-vtep vid 11
bridge vni add dev evx4-evpn-vtep vni 101
bridge vlan add dev evx4-evpn-vtep vid 11 tunnel_info id 101

# IP-VRF SVI
ip link add svl3-blue link evbr-evpn-vtep type vlan id 11
ip link set svl3-blue address aa:bb:cc:00:00:64 addrgenmode none

# Bind to the UDN VRF
ip link set svl3-blue master blue
ip link set svl3-blue up

# MAC-VRF: map VLAN 12 <-> VNI 100
bridge vlan add dev evbr-evpn-vtep vid 12 self
bridge vlan add dev evx4-evpn-vtep vid 12
bridge vni add dev evx4-evpn-vtep vni 100
bridge vlan add dev evx4-evpn-vtep vid 12 tunnel_info id 100

# MAC-VRF SVI
ip link add svl2-blue link evbr-evpn-vtep type vlan id 12
ip link set svl2-blue address aa:bb:cc:00:00:64 addrgenmode none
ip link set svl2-blue master blue
ip link set svl2-blue up

# Connect OVS to the Linux Bridge
ovs-vsctl add-port br-int ovl2-blue -- set interface ovl2-blue type=internal
ip link set ovl2-blue master evbr-evpn-vtep
bridge vlan add dev ovl2-blue vid 12 pvid untagged
ip link set ovl2-blue up

# Per pod: static FDB and neighbor entries
# (example pod with MAC 0a:58:0a:00:0a:05 and IP 10.0.10.5)
bridge fdb add 0a:58:0a:00:0a:05 dev ovl2-blue vlan 12 master static
ip neigh add 10.0.10.5 lladdr 0a:58:0a:00:0a:05 dev svl2-blue nud permanent
```

For a **Layer 3 IP-VRF only** configuration, the MAC-VRF section (VLAN 12
mapping, L2 SVI, OVS port, and per-pod entries) is omitted. For a **MAC-VRF
only** configuration, the IP-VRF section (VLAN 11 mapping and L3 SVI) is
omitted.

> [!NOTE]
> Device names are derived from the VTEP or CUDN name. When the resulting name
> exceeds 15 characters (the Linux interface name limit), a hash-based fallback
> is used: e.g. `evbr.a3f2b1c9` instead of `evbr-my-long-vtep-name`.

#### L2 SVI

For MAC-VRFs, a Layer 2 SVI (`svl2-{name}`) is created on the bridge and
attached to the UDN VRF. This SVI is not used as a gateway and does not have an
IP address configured; the management port (`ovn-k8s-mpx`) is used instead for
routing. However, FRR depends on the SVI as an IP domain anchor with a neighbor
table from which it generates Type 2 routes with IP and installs entries from
learned Type 2 routes with IP for ARP suppression.

#### Static FDB and Neighbor Entries

For each pod in a MAC-VRF enabled UDN, ovnkube-node creates:

- A **static FDB entry** on the OVS bridge port for the pod's MAC address.
- A **static neighbor entry** on the L2 SVI for each pod IP.

Both entries are needed for FRR to generate Type 2 EVPN routes with IP
information. Creating the FDB entry statically also avoids depending on
bootstrap traffic that would otherwise be needed to populate the entry through
MAC learning.

These entries are cleaned up when the pod is deleted. For KubeVirt live
migration, the source pod's entries are removed when the migration target
becomes ready, triggering FRR to withdraw the Type 2 routes from the source node
and advertise them from the target. During the short window where both source and
target coexist, BGP MAC mobility handles the situation: the target node advertises
the same MAC/IP with a higher sequence number, causing peers to prefer the new
path.

#### VTEP IP

In **managed mode**, ovnkube-node reads the allocated VTEP IP from the
`k8s.ovn.org/vteps` annotation and configures it on a dedicated per-VTEP dummy
device (e.g. `evlo-evpn-vtep`).

In **unmanaged mode**, the VTEP IP is assumed to already be configured as a
primary address on a dedicated device (e.g. a dummy interface) on the node.
ovnkube-node discovers IPs that fall within the VTEP CIDRs and annotates the
node with `k8s.ovn.org/vteps`. Keepalived VIPs and secondary addresses are
filtered out.

### OVN Configuration

When EVPN is enabled for a network, ovnkube-controller configures OVN with the
following differences from a standard overlay network:

**No interconnect resources**: transit switches, transit ports and remote ports
are not created. The `hasInterconnectTransport()` method returns false for EVPN
networks; VXLAN via the EVPN fabric replaces Geneve for east/west traffic.

**MACVRF logical switch port**: for MAC-VRF networks, a logical switch port
with `addresses: ["unknown"]` is created on the worker switch. This port is
bound to the OVS internal port (`ovl2-{name}`) that connects the logical switch
to the EVPN bridge, where it is added on the MAC-VRF allocated VLAN. Traffic
destined to unknown MACs (i.e. MACs on remote nodes or external entities) is
forwarded through this port into the EVPN fabric.

**Gateway ARP/NS ACLs**: ACLs are added to drop ARP requests and IPv6 Neighbor
Solicitations targeting the OVN gateway IP on the MACVRF port. This prevents
external entities from resolving the OVN gateway address through the EVPN
fabric, since the gateway is only reachable locally through the management port.

#### Multicast

EVPN MAC-VRF networks configure the OVN logical switch with the following
multicast settings:

- `mcast_snoop=true`: IGMP snooping is enabled so OVN forwards multicast only
  to pods that have joined the group.
- `mcast_flood_unregistered=true`: unregistered multicast is flooded, including
  towards the MACVRF port and thus into the EVPN fabric.
- `mcast_querier=false`: the IGMP querier is disabled; querying is handled by
  the EVPN fabric or external entities.

The MACVRF port is added to the cluster router port group so that the
AllowInterNode multicast ACL overrides the default deny and permits multicast
traffic to and from the EVPN fabric. Type 3 EVPN routes announce VPN/VTEP
membership and trigger ingress replication to flood multicast packets to the
right VTEPs across the fabric. The Linux bridge floods the multicast to the OVS
internal port, and OVN then relies on IGMP snooping to limit delivery to
registered pods.

> [!NOTE]
> Multicast is only supported for MAC-VRFs. IP-VRF multicast is not supported
> because the kernel multicast routing is not configured.

### Cluster Manager

#### VTEP IP Management

In **managed mode**, ovnkube-cluster-manager allocates one VTEP IP per node from
the VTEP CIDRs and annotates the node with `k8s.ovn.org/vteps`. IPs are
allocated sequentially across CIDRs in order.

In **unmanaged mode**, the annotation is written by ovnkube-node (see
[VTEP IP](#vtep-ip) under Node Configuration).

In both modes, the annotation has the following format:

```json
{"evpn-vtep": {"ips": ["100.64.0.1"]}}
```

ovnkube-cluster-manager reads this annotation to validate all nodes have VTEP
IPs and sets the VTEP status accordingly. The annotation is also used by the
RouteAdvertisements controller to advertise the VTEP IP via the underlay BGP
session (see [FRR Configuration](#frr-configuration) below).

#### FRR Configuration

The RouteAdvertisements controller generates FRR configuration for EVPN. The
configuration has two main sections:

**Global EVPN section**: activates EVPN neighbors and advertises all VNIs:

```text
router bgp 65000
 address-family l2vpn evpn
  neighbor 192.168.122.12 activate
  neighbor 192.168.122.12 allowas-in origin
  advertise-all-vni
  vni 100
   route-target import 65000:100
   route-target export 65000:100
  exit-vni
 exit-address-family
exit
```

FRR automatically detects VNIs via netlink through `advertise-all-vni`. The
explicit `vni` section is only added when a route target is configured, to
override FRR's auto-generated route target.

The `allowas-in origin` directive is applied unconditionally for each neighbor
to handle eBGP peers sharing the same ASN.

**Per-VRF EVPN section** (for IP-VRFs): maps the VRF to a VNI and advertises
unicast routes. The `advertise ipv4 unicast` and `advertise ipv6 unicast`
directives are included dynamically based on the IP families present in the
CUDN's subnets:

```text
vrf blue
 vni 101
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  advertise ipv6 unicast
  route-target import 65000:101
  route-target export 65000:101
 exit-address-family
exit
```

The controller also reads the `k8s.ovn.org/vteps` node annotation to configure
the underlay router to advertise the node's VTEP IP and accept the VTEP CIDRs,
ensuring VXLAN reachability between VTEPs.

#### Example: MAC-VRF + IP-VRF derived from underlay

> [!NOTE]
> The generated `FRRConfiguration` in these examples uses `rawConfig` to carry
> the EVPN configuration. This is an implementation detail that may change once
> an official FRR-K8s API for EVPN is available.

Given this source `FRRConfiguration` with a single default VRF router:

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  labels:
    use-for-advertisements: evpn
  name: evpn-peering
  namespace: frr-k8s-system
spec:
  nodeSelector: {}
  bgp:
    routers:
    - asn: 65000
      neighbors:
      - address: 192.168.122.12
        asn: 65001
```

And the Layer 2 CUDN `blue` with subnet `10.0.10.0/24`, MAC-VRF VNI 100,
IP-VRF VNI 101, and a VTEP with CIDR `100.64.0.0/24` (node allocated IP
`100.64.0.1`), the controller generates a per-node `FRRConfiguration` similar
to:

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: ovnk-generated-xxxxx
  namespace: frr-k8s-system
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: worker-1
  bgp:
    routers:
    - asn: 65000
      neighbors:
      - address: 192.168.122.12
        asn: 65001
        toAdvertise:
          allowed:
            mode: filtered
            prefixes:
            - 100.64.0.1/32
        toReceive:
          allowed:
            mode: filtered
            prefixes:
            - 100.64.0.0/24
      prefixes:
      - 100.64.0.1/32
    - asn: 65000
      vrf: blue
      prefixes:
      - 10.0.10.0/24
  rawConfig: |
    router bgp 65000
     address-family l2vpn evpn
      neighbor 192.168.122.12 activate
      neighbor 192.168.122.12 allowas-in origin
      advertise-all-vni
     exit-address-family
    exit
    !
    vrf blue
     vni 101
    exit-vrf
    !
    router bgp 65000 vrf blue
     address-family l2vpn evpn
      advertise ipv4 unicast
     exit-address-family
    exit
    !
```

The underlay router advertises the node's VTEP IP (`100.64.0.1/32`) and accepts
the full VTEP CIDR (`100.64.0.0/24`), so that VTEP-to-VTEP VXLAN reachability
is established via the underlay BGP session.

Notice that the VRF router for `blue` was created automatically with the ASN
inherited from the underlay router, since no VRF-specific router was provided
in the source `FRRConfiguration`.

#### Example: IP-VRF with explicit VRF router

If the source `FRRConfiguration` includes a router whose VRF matches the CUDN
name, that router is used for the IP-VRF EVPN section instead of deriving one
from the underlay.

Given this source `FRRConfiguration` with an explicit `red` VRF router:

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  labels:
    use-for-advertisements: evpn
  name: evpn-peering
  namespace: frr-k8s-system
spec:
  nodeSelector: {}
  bgp:
    routers:
    - asn: 65000
      neighbors:
      - address: 192.168.122.12
        asn: 65001
    - asn: 65100
      vrf: red
```

And the Layer 3 CUDN `red` with subnet `10.0.20.0/16`, IP-VRF VNI 200, and
the same VTEP as before, the controller generates:

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: ovnk-generated-xxxxx
  namespace: frr-k8s-system
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: worker-1
  bgp:
    routers:
    - asn: 65000
      neighbors:
      - address: 192.168.122.12
        asn: 65001
        toAdvertise:
          allowed:
            mode: filtered
            prefixes:
            - 100.64.0.1/32
        toReceive:
          allowed:
            mode: filtered
            prefixes:
            - 100.64.0.0/24
      prefixes:
      - 100.64.0.1/32
    - asn: 65100
      vrf: red
      prefixes:
      - 10.0.20.0/24
  rawConfig: |
    router bgp 65000
     address-family l2vpn evpn
      neighbor 192.168.122.12 activate
      neighbor 192.168.122.12 allowas-in origin
      advertise-all-vni
     exit-address-family
    exit
    !
    vrf red
     vni 200
    exit-vrf
    !
    router bgp 65100 vrf red
     address-family l2vpn evpn
      advertise ipv4 unicast
     exit-address-family
    exit
    !
```

Notice that the VRF EVPN section uses ASN 65100 from the explicit VRF router
instead of inheriting 65000 from the underlay.

## Troubleshooting

### Check VTEP status

Verify the VTEP CR is accepted. `Accepted: True` with reason `Allocated` means
all nodes have a valid VTEP IP:

```shell
❯ kubectl get vtep
NAME         ACCEPTED   REASON
evpn-vtep    True       Allocated
```

If `Accepted` is `False`, check node annotations to see which nodes are missing
a VTEP IP:

```shell
❯ kubectl get node worker-1 -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/vteps}'
{"evpn-vtep":{"ips":["100.64.0.1"]}}
```

An empty or missing annotation means ovnkube-node has not yet discovered or
been assigned a VTEP IP for that node.

### Check RouteAdvertisements status

Verify the RouteAdvertisements CR is accepted:

```shell
❯ kubectl get routeadvertisements evpn-blue
NAME        ACCEPTED   REASON
evpn-blue   True       Accepted
```

If `Accepted` is `False`, check that the `frrConfigurationSelector` matches an
existing `FRRConfiguration` and that it contains at least a default VRF router
with neighbors. Also check the condition message: the controller rejects a
selected network whose EVPN VNI is outside 1-16777215 or whose route target is
not of the form `ASN:NN`, `A.B.C.D:NN` or `*:NN`, and deletes the
`FRRConfiguration` objects generated for that `RouteAdvertisements` before
input validation existed.

### Check CUDN status

Verify the CUDN is accepted and the referenced VTEP exists:

```shell
❯ kubectl get clusteruserdefinednetwork blue -o \
    custom-columns='NAME:.metadata.name,ACCEPTED:.status.conditions[?(@.type=="clusteruserdefinednetwork")].status,REASON:.status.conditions[?(@.type=="clusteruserdefinednetwork")].reason'
NAME   ACCEPTED   REASON
blue   True       EVPNTransportAccepted
```

If `ACCEPTED` is not `True`, check that the referenced VTEP CR exists and is
itself accepted.

### Verify node devices

Check that the EVPN bridge, VXLAN, SVIs, and OVS port are created. A healthy
setup for a VTEP named `evpn-vtep` and CUDN `blue` should show:

```shell
❯ ip link show type bridge
...
42: evbr-evpn-vtep: <BROADCAST,MULTICAST,UP> mtu 1500 ...
...

❯ ip link show type vxlan
...
43: evx4-evpn-vtep: <BROADCAST,MULTICAST,UP> mtu 1450 ...
...

❯ ip link show type vlan
...
44: svl3-blue@evbr-evpn-vtep: <BROADCAST,MULTICAST,UP> ...
45: svl2-blue@evbr-evpn-vtep: <BROADCAST,MULTICAST,UP> ...
...
```

Verify VLAN-to-VNI mappings are present for all VNIs:

```shell
❯ bridge vni show
port              vlan-range    vni-range
...
evx4-evpn-vtep    11            101
                  12            100
...

❯ bridge vlan show dev evx4-evpn-vtep
port              vlan-range
...
evx4-evpn-vtep    11
                  12
...
```

Check the OVS access port is present on the MAC-VRF VLAN:

```shell
❯ bridge vlan show
...
port              vlan-range
ovl2-blue         12 PVID Egress Untagged
...
```

Check static FDB entries for pods. Each pod's MAC should appear with the
MAC-VRF VLAN:

```shell
❯ bridge fdb show
0a:58:0a:00:0a:05 dev ovl2-blue vlan 12 master evbr-evpn-vtep static
```

Check static neighbor entries on the L2 SVI. Each pod IP should appear as
permanent:

```shell
❯ ip neigh show
...
10.0.10.5 dev svl2-blue lladdr 0a:58:0a:00:0a:05 PERMANENT
...
```

### Verify FRR EVPN state

Check EVPN routes. You should see Type 2 (MAC/IP), Type 3 (multicast), and
Type 5 (IP prefix) routes depending on your configuration:

```shell
❯ kubectl exec -ti -n frr-k8s-system <frr-pod> -c frr -- vtysh -c "show bgp l2vpn evpn"
BGP table version is 5, local router ID is 100.64.0.1
   Network          Next Hop       Metric LocPrf Weight Path
...
Route Distinguisher: 100.64.0.1:100
*> [2]:[0]:[48]:[0a:58:0a:00:0a:05]:[32]:[10.0.10.5]
                    100.64.0.1          0         32768 ?
*> [3]:[0]:[32]:[100.64.0.1]
                    100.64.0.1          0         32768 ?
...
Route Distinguisher: 100.64.0.1:101
*> [5]:[0]:[24]:[10.0.10.0]
                    100.64.0.1          0         32768 ?
...
```

Check VNI status. All configured VNIs should appear with the correct VxLAN
interface and number of MACs/neighbors:

```shell
❯ kubectl exec -ti -n frr-k8s-system <frr-pod> -c frr -- vtysh -c "show evpn vni"
VNI        Type VxLAN IF         # MACs   # ARPs   # Remote VTEPs  Tenant VRF  VLAN  BRIDGE
100        L2   evx4-evpn-vtep   1        1        1               blue        12    evbr-evpn-vtep
101        L3   evx4-evpn-vtep   0        0        n/a             blue        11    evbr-evpn-vtep
```

Check learned MACs per VNI. Local pod MACs and remote MACs learned from the
fabric should be listed:

```shell
❯ kubectl exec -ti -n frr-k8s-system <frr-pod> -c frr -- vtysh -c "show evpn mac vni 100"
MAC               Type   Flags Intf/Remote ES/VTEP   VLAN  Seq #'s
0a:58:0a:00:0a:05 local        ovl2-blue              12    0/0
0a:58:0a:00:0a:06 remote       100.64.0.2                   0/0
```

Check ARP cache per VNI. This shows IP-to-MAC bindings from Type 2 routes:

```shell
❯ kubectl exec -ti -n frr-k8s-system <frr-pod> -c frr -- vtysh -c "show evpn arp-cache vni 100"
Neighbor        Type   Flags State    MAC               Remote ES/VTEP          Seq #'s
10.0.10.5       local        active   0a:58:0a:00:0a:05                         0/0
10.0.10.6       remote       active   0a:58:0a:00:0a:06 100.64.0.2             0/0
```

Also refer to the [Route Advertisements troubleshooting
section](./route-advertisements.md#troubleshooting) for general BGP
troubleshooting steps.

## Feature Compatibility

| Feature                                | Support                                                  |
|----------------------------------------|----------------------------------------------------------|
| Egress Firewall                        | Full                                                     |
| Egress QoS                             | Full                                                     |
| Network QoS                            | Full                                                     |
| Network Policy / ANP                   | Full                                                     |
| Services (Cluster IP)                  | Full                                                     |
| Services (NodePort, External IP, LB)   | Limited (see [Known Limitations](#known-limitations))    |
| KubeVirt Live Migration                | Full                                                     |
| Multicast (MAC-VRF)                    | Full                                                     |
| Multicast (IP-VRF)                     | Not supported                                            |
| Multiple External Gateways (MEG)       | Not supported                                            |
| Egress IP                              | Not supported                                            |
| Egress Service                         | Not supported                                            |
| IPSec                                  | Not supported                                            |

## Known Limitations

- Only supported in local gateway mode. Supporting shared gateway mode is a
  future goal.
- FRR 9+ is required for SVD mode.
- Maximum 4094 MAC-VRF + IP-VRF combinations per VTEP due to the VLAN limit in
  SVD mode.
- VXLAN destination port defaults to 4789 and customization is not supported.
- CUDN names must be under 16 characters for a predictable VRF name.
- Services (NodePort, External IP, LoadBalancer) require that node, external,
  and LoadBalancer IPs are routed on the EVPN fabric to the appropriate cluster
  nodes. OVN-Kubernetes does not handle this advertisement, but service traffic
  works normally once it reaches the nodes.

## References

- [FRR EVPN Configuration Guide](https://docs.frrouting.org/en/latest/evpn.html)
- [FRR-k8s](https://github.com/metallb/frr-k8s)
- [OKEP-5088: EVPN Support](../../okeps/okep-5088-evpn.md)
- [Route Advertisements](./route-advertisements.md)
