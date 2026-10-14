# Route Advertisements

## Introduction

The Route Advertisements feature introduces BGP as a supported routing protocol
with OVN-Kubernetes enabling the integration into different BGP user
environments. The extent of the Route Advertisements feature and corresponding
API allows importing routes from BGP peers on the provider network into OVN pod
networks as well as exporting pod network and egress IP routes to BGP peers on
the provider network. Both default pod network as well as primary Layer 3 and
Layer 2 cluster-user-defined networks (CUDNs) are supported.

> [!NOTE]
> For purposes of this documentation, the external, physical network of the
> cluster which a user administers will be called the “provider network”.

## Prerequisites

- [FRR-k8s](https://github.com/metallb/frr-k8s)

Always check the dependencies on the [Requirements page](../requirements.md)

## Motivation

There are multiple driving factors which necessitate integrating BGP into
OVN-Kubernetes:

- Importing Routes from the Provider Network: Today there is no API for a user
to be able to configure routes into OVN. In order for a user to change how
egress traffic is routed, the user leverages local gateway mode. This mode
forces traffic to hop through the Linux networking stack, and there a user can
configure routes inside the host to control egress routing. This manual
configuration would need to be performed and maintained across nodes and VRFs
within each node.

- Exporting Routes into the Provider Network: There exists a need for provider
networks to learn routes directly to pods today in Kubernetes. One such use case
is integration with 3rd party load balancers, where they terminate a load
balancer and then send packets directly to cluster nodes with the destination IP
address being the pod IP itself. Today these load balancers rely on custom
operators to detect which node a pod is scheduled to and then add routes into
its load balancer to send the packet to the right node. By integrating BGP and
advertising the pod subnets/addresses directly on the provider network, load
balancers and other entities on the network would be able to reach the pod IPs
directly.

Additionally, integrating BGP support paves the way for other BGP based features
that might be implemented in the future, like:

- EVPN support to extend pod network isolation outside the cluster.
- No overlay mode to avoid the Geneve overhead.

## User-Stories/Use-Cases

- As a user, I want to be able to leverage my existing BGP network to dynamically
  learn routes to pods in my Kubernetes cluster.
- As a user, rather than having to maintain routes manually in each Kubernetes
  node, as well as being constrained to using local gateway mode for respecting
  user-defined routes; I want to use BGP so that I can dynamically advertise
  egress routes for the Kubernetes pod traffic in either gateway mode.
- As an egress IP user, I want to use a pure routing implementation to handle
  advertising egress IP movement across nodes.
- As a user, I want to extend CUDN isolation to the provider network over a
  VRF-Lite type of VPN where I can restrict traffic of the CUDN to an interface
  attached to the VRF associated with the CUDN.

> [!NOTE]
> The [isolation](#cudn-isolation) between different pod networks is unaffected
> by this feature.

## How to enable this feature on an OVN-Kubernetes cluster?

The `route-advertisements` feature must be enabled in the OVN-Kubernetes
configuration. Please use the `Feature Config` option
`enable-route-advertisements` under `OVNKubernetesFeatureConfig` config to
enable it.

## Local kind demo

A local kind cluster can enable route advertisements with
`contrib/kind.sh -mne -rae`.
For FRR-k8s development, apply the patches in
`contrib/frr-k8s/patches` to an FRR-k8s checkout and run the patched
`hack/demo/demo.sh` script from that checkout.

Without arguments, the demo configures the main BGP session only. It discovers
the kind node `InternalIP` addresses and the external FRR peer address for each
available IP family. In a dual-stack cluster, the rendered FRR and Kubernetes
configuration includes both IPv4 and IPv6 peers.

To add a second BGP session over a VLAN, pass the positional arguments in this
order:

```shell
$ cd hack/demo && ./demo.sh <second-session-interface> <second-session-vrf> [<second-session-vlan> [<ipv4-sed> [<ipv6-sed>]]]
```

- `<second-session-interface>`: VLAN-capable host interface used as the VLAN
  parent. If omitted, no second BGP session is configured.
- `<second-session-vrf>`: VRF used by the node-side FRR-k8s session. The demo
  creates the external FRR peer VRF as `<second-session-vrf>-ext`.
- `<second-session-vlan>`: VLAN ID for the second session. Default: `221`.
- `<ipv4-sed>`: sed expression applied to the main IPv4 BGP session IPs to
  derive the second-session IPv4 addresses. Default: `s|111|221|g`.
- `<ipv6-sed>`: sed expression applied to the main IPv6 BGP session IPs to
  derive the second-session IPv6 addresses. Default: `s|c956|ca56|g`.

Because the values are positional, pass the VLAN ID explicitly when overriding
only the sed expressions. Second-session mode is supported only with host
networking; it also requires passwordless `sudo` when the demo is not run as
root, and a VLAN-capable interface connected to the second-session network.

## User-facing API Changes

A new OVN-Kubernetes API is introduced for this feature:
[`RouteAdvertisements`](../../api-reference/routeadvertisements-api-spec.md).

## Workflow Description

OVN-Kubernetes integrates with FRR-k8s to provide BGP support and it must be
deployed before enabling the `route-advertisements` feature.

Once deployed, an initial FRR-k8s configuration must be done using its
`FRRConfiguration` API which serves, among others, three purposes:

- Configure BGP peering.
- Configure route import.
- Serve as a template to the `FRRConfiguration` instances that OVN-Kubernetes
  generates.

Finally, route export is configured through `RouteAdvertisements` instances.
Each `RouteAdvertisements` instance allows to select which pod networks to
export routes for. It also allows to select which `FRRConfiguration` instances
to use as template, and as a consequence, provides the flexibility to export
routes in a different number of ways including: which BGP peers to export to,
the use of iBGP or eBGP, etc.

### Import routes into the default pod network

The following example represents an initial FRR-k8s configuration that
configures FRR-k8s to have all the nodes establish a BGP peering session and
receive routes in the `172.20.0.0/16` subnet:

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  labels:
    use-for-advertisements: default
  name: receive-filtered
  namespace: frr-k8s-system
spec:
  nodeSelector: {}
  bgp:
    routers:
    - asn: 64512
      neighbors:
      - address: 192.168.111.3
        asn: 64512
        disableMP: true
        toReceive:
          allowed:
            mode: filtered
            prefixes:
            - prefix: 172.20.0.0/16
```

This will result in the routes being installed in the main (default VRF) routing
table on the nodes and used by the pod egress traffic in local gateway mode. As
long as the `route-advertisements` feature is enabled, OVN-Kubernetes will
synchronize the BGP routes from the default VRF to the default OVN pod network
gateway router and hence used for the egress traffic of the pods on that network
in shared gateway mode.

> [!NOTE]
> For two BGP routers to establish a peering session and exchange routes, their
> configurations must be mutually aligned: the `neighbor` configuration in the
> previous example must correspond to the remote BGP router's configuration
> (router ID, AS number, accept routes, etc...), and vice versa.

> [!NOTE]
> A neighbor can be defined by IP `address` or, for unnumbered BGP peering, by
> node `interface`; one of the two must be specified. A neighbor defined by
> address peers over a session of that address family and is only advertised
> prefixes of the same family. FRR establishes an unnumbered session over an
> IPv4 address derived from a /30 or /31 on the interface or, failing that,
> over the peer's IPv6 link-local address; in either case the session can
> carry prefixes of both IP families (RFC 8950), so OVN-Kubernetes advertises
> prefixes of both families to interface-defined neighbors.

### Import routes from the default VRF into a CUDN

Assuming we have a CUDN:

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: extranet
  labels:
    advertise: "true"
spec:
  namespaceSelector:
    matchLabels:
      network: extranet
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: "22.100.0.0/16"
        hostSubnet: 24
```

After routes have been imported to the default VRF as in the previous example,
a typical scenario is to import those routes from the default VRF to a CUDN as
well. This can be achieved with:

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  labels:
    use-for-advertisements: default
  name: import-extranet
  namespace: frr-k8s-system
spec:
  nodeSelector: {}
  bgp:
    routers:
    - asn: 64512
      imports:
      - vrf: default
      vrf: extranet
```

This will result in the routes being installed in the extranet VRF associated to
the CUDN of the same name. If `route-advertisements` feature is enabled,
OVN-Kubernetes will synchronize the BGP routes installed on a VRF to the OVN
gateway router of the associated CUDN and hence will be used for the egress
traffic of the pods on that network.

> [!NOTE]
> For CUDN names of 15 characters or fewer, the corresponding network VRF has
> the same name. For longer CUDN names, the VRF name is derived from the network
> ID. Read the actual VRF name from the CUDN `status.vrfName` field instead of
> deriving it.

> [!NOTE]
> If you export routes for a CUDN over the default VRF as detailed on the next
> sections, installed BGP routes in the default VRF are imported to the CUDN
> automatically and this configuration is not necessary.

### Export routes to the default pod network

Assuming the `FRRConfiguration` examples that have been used previously, this
example would advertise routes to the default pod network and its egress IPs:

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: default
spec:
  targetVRF: default
  advertisements:
  - PodNetwork
  - EgressIP
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      use-for-advertisements: default
  networkSelectors:
  - networkSelectionType: DefaultNetwork
```

This would advertise routes for the pod network to the BGP peers as defined on
the selected `FRRConfiguration` instances; and make the necessary changes to
correctly handle N/S traffic directly addressing IPs of that network.

Currently, when the `advertisements` field includes `PodNetwork`, you must
select all nodes with `nodeSelector`. However, if you are only advertising
egress IPs, you can limit advertisements to egress IPs assigned to the selected
nodes:

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: default-egressip
spec:
  advertisements:
  - EgressIP
  nodeSelector: 
    matchLabels:
      egress-nodes: bgp
  frrConfigurationSelector:
    matchLabels:
      use-for-advertisements: default
  networkSelectors:
  - networkSelectionType: DefaultNetwork
```

> [!NOTE]
> Egress IPs will be advertised over the selected BGP sessions
> regardless of whether they are assigned to the same interface those sessions
> are established over or not, probably making the advertisements ineffective if
> they are not the same.

### Export routes to a CUDN over the default VRF

Similarly, routes to pods on a CUDN can be advertised over the default VRF:

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: default-cudn
spec:
  targetVRF: default
  advertisements:
  - PodNetwork
  - EgressIP
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      use-for-advertisements: default
  networkSelectors:
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          advertise: true
```

Note that this configuration also results in the BGP installed routes of the
default VRF to be imported to the CUDN VRF.

Multiple types of network selectors can be specified making it possible to merge
the previous two examples into one:

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: default-all
spec:
  targetVRF: default
  advertisements:
  - PodNetwork
  - EgressIP
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      use-for-advertisements: default
  networkSelectors:
  - networkSelectionType: DefaultNetwork
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          advertise: true
```

### Import and export routes to a CUDN over the network VRF (VRF-Lite)

It is also possible to import and export routes to a CUDN over a BGP session
established over that network's VRF without involving the default VRF at all.

To import, we define the proper `FRRConfiguration` first. This example is
similar to how routes are imported for the default pod network with the
exception that the BGP peering session is configured to happen over the CUDN VRF
`extranet`:

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  labels:
    use-for-advertisements: extranet
  name: receive-filtered-extranet
  namespace: frr-k8s-system
spec:
  nodeSelector: {}
  bgp:
    routers:
    - asn: 64512
      neighbors:
      - address: 192.168.221.3
        asn: 64512
        disableMP: true
        toReceive:
          allowed:
            mode: filtered
            prefixes:
            - prefix: 172.20.0.0/16
      vrf: extranet
```

Then we define the `RouteAdvertisements` to export:

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: extranet
spec:
  targetVRF: auto
  advertisements:
  - PodNetwork
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      use-for-advertisements: extranet
  networkSelectors:
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          advertise: true
```

`targetVRF` value `auto` is a magic helper value that tells OVN-Kubernetes to
advertise each network over that network's VRF.

When a CUDN is advertised only over its own VRF, OVN-Kubernetes interprets this
as an explicit intention to isolate the network to that VRF and takes additional
measures to ensure that no network traffic is leaked externally over the default
VRF. This configuration is referred to as `VRF-Lite`. An external provider edge
BGP router could map this isolated traffic to an EVPN achieving a similar use
case as if EVPN were to be supported directly.

> [!NOTE]
> For the BGP session to be actually established over that network's VRF, at
> least one interface with proper IP configuration needs to be attached to the
> network's VRF. The CUDN egress traffic matching the learned routes will be
> routed through that interface. OVN-Kubernetes does not manage this interface
> nor its attachment to the network's VRF.

> [!NOTE]
> This configuration is only supported in local gateway mode.
> Additionally, this configuration does not support the advertisement of egress
> IPs.

## CUDN isolation

User defined networks are isolated by default. In other words, users on CUDN A
cannot access pods on CUDN B via their internal pod or service addresses. When
advertising CUDNs via BGP on the same VRF (typically the default VRF), the
behavior of inter-CUDN isolation is preserved: from the perspective of a CUDN,
traffic addressing the subnet of a different CUDN will be considered N/S traffic
and will egress the cluster towards the provider network; and if the provider
network is able to route it back to the cluster by virtue of learned BGP routes,
the traffic will still be dropped to upkeep the CUDN isolation promise.

OVN-Kubernetes relaxes the default advertised UDN isolation behavior when the
configuration flag `advertised-udn-isolation-mode` is set to `loose`. In this
configuration, traffic addressing the subnet of a different CUDN will egress the
cluster towards the provider network as before but, if routed back towards the
cluster, connectivity will be allowed in this case.

## Implementation Details

### Overview

```mermaid
flowchart TD
    S@{shape: sm-circ}

    S-->|User configures|T0

    subgraph T0
    J0@{shape: f-circ}
    F0(FRRConfiguration)
    R0(RouteAdvertisements)
    C0(CUDNs)
    J0-->|to configure BGP peering and route import|F0
    J0-->|to export routes|R0
    J0-->|to add networks|C0
    end

    R0-->|ovn-kubernetes configures|J1

    subgraph T1
    J1@{shape: f-circ}
    F1(FRRConfiguration)
    O1(OVN Networks)
    H1(Host Networks)
    J1-->F1
    J1-->O1
    J1-->H1
    end

    F0-->J2
    F1-->J2

    subgraph T2
    J2@{shape: f-circ}
    F2{/etc/frr.conf}
    J2 -->|FRR-k8s configures|F2
    end

    F2-->T3
    
    subgraph T3
    J3@{shape: f-circ}
    E3@{shape: framed-circle}
    F31(FRR advertises exported routes)
    F32(FRR installs imported routes in host)
    J3-->F31-->E3
    J3-->F32-->E3
    end

    T3-->T4

    subgraph T4
    J4@{shape: f-circ}
    E4@{shape: framed-circle}
    O4(ovn-kubernetes copies installed routes to OVN)
    J4-->O4-->E4
    end
```

The flowchart above gives an idea on what happens on different convergence
timelines:

- T0: Initially a user configures CUDN networks, sets up BGP peering and route
  import with `FRRConfiguration` instances and route export with
  `RouteAdvertisements` instances.
- T1: OVN-Kubernetes reacts to the configured `RouteAdvertisements` and
  generates the appropriate `FRRConfiguration` instances to export the selected
  networks. OVN-Kubernetes then reconfigures those networks in both OVN and the
  host stack so that they operate correctly when advertised.
- T2: FRR-k8s merges all the `FRRConfiguration` instances and configures its
  internal FRR daemons.
- T3: FRR daemons export, import and install routes accordingly.
- T4: OVN-Kubernetes copies installed routes to the appropriate OVN networks.

### RouteAdvertisements controller

The `RouteAdvertisements` controller reacts to `RouteAdvertisements` instances and
generates the corresponding `FRRConfiguration` instances to export routes for the
selected networks. It also annotates the NetworkAttachmentDefinition instances
for the selected networks to instruct the OVN and host network controllers on
each node to reconfigure the network.

#### FRRConfiguration instances generated by OVN-Kubernetes

When `RouteAdvertisements` instances are configured, OVN-Kubernetes generates
additional `FRRConfiguration` instances in order for the selected network
prefixes to be advertised, using the following logic:

- For each pair combination of selected network and selected node; and for each
  selected `FRRConfiguration` to be used as template:
    - If the `FRRConfiguration` does not apply to the node, it is discarded.
    - If a router defined in that `FRRConfiguration` does not apply to the
      target VRF, it is discarded.
    - An `FRRConfiguration` instance is generated that contains all routers that
      were not discarded with the following modifications:
        - If advertising pod network:
            - Router `prefixes` and neighbors `toAdvertise` `prefixes` set to:
                - the network host subnet for default network or layer 3
                  topologies.
                - the network subnet for layer 2 topologies.
            - Neighbors “toReceive” cleared defaulting to `filtered` mode with
              no prefixes.
            - If `targetVRF` and network VRF are different and `targetVRF` is
              not “auto”, routes are imported reciprocally across both VRFs:
                - An import from the network VRF.
                - An additional router on network VRF to import from target VRF.
        - If advertising egress IPs: for each egress IP, if the egress IP
          selects a namespace served by the selected network and it is assigned
          to the selected node, the egress IP is added to “prefixes” and
          neighbors “toAdvertise”.

This is an example of an `FRRConfiguration` instance generated for a node from
previous `RouteAdvertisements` examples when a CUDN is advertised over the
default VRF:

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  annotations:
    k8s.ovn.org/route-advertisements: extranet/receive-filtered/master-1.ostest.test.metalkube.org
  labels:
    k8s.ovn.org/route-advertisements: extranet
  name: ovnk-generated-vl8gk
  namespace: frr-k8s-system
spec:
  bgp:
    routers:
    - asn: 64512
      imports:
      - vrf: extranet
      neighbors:
      - address: 192.168.111.3
        asn: 64512
        disableMP: true
        toAdvertise:
          allowed:
            mode: filtered
            prefixes:
            - 22.100.2.0/24
        toReceive:
          allowed:
            mode: filtered
      prefixes:
      - 22.100.2.0/24
    - asn: 64512
      imports:
      - vrf: default
      vrf: extranet
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: master-1.ostest.test.metalkube.org
```

This example `FRRConfiguration` instance applies to one of the nodes but you
would see similar `FRRConfiguration` instances for the other selected nodes. In
summary, the instance is instructing FRR-k8s to advertise the `22.100.2.0/24`
prefix, which is the one assigned to pods hosted on that node for that network,
over the session established towards the BGP peer `192.168.111.3` as instructed
by the selected `FRRConfiguration` instances used as a template to generate this
one.

From this example, it is relevant to highlight a couple of things:

- When a CUDN is advertised over the default VRF, received routes on the default
  VRF will also be imported to the VRF associated with the CUDN and become
  available for use to that CUDN.
- A previously mentioned, this generated configuration only deals with the
  advertisement of routes. Route reception must be configured manually as
  detailed in previous sections. Particularly, cluster advertised routes are not
  configured to be received by other cluster nodes as that would be problematic
  for the intra-cluster connectivity.

> [!NOTE]
> `FRRConfiguration` instances generated in this manner by
> OVN-Kubernetes can't become selected by `RouteAdvertisements`.

### OVN Network controllers: impacts in OVN configuration

OVN Network controllers on each node react to annotations on the
NetworkAttachmentDefinition, processing the applicable `RouteAdvertisements`
instances for the network and gathering information on how the network is being
advertised.

#### OVN SNAT behavior with BGP Advertisement

Usually N/S egress traffic from a pod is SNATed to the node IP. This does not
happen when the network is advertised. In that case the traffic egresses the
cluster with the pod IP as source. In shared gateway mode this is handled with a
conditional SNAT on the gateway routers OVN configuration for the network which
ensures that E/W egress traffic (right now, only pod-to-node traffic) continues
to be SNATed. 

```shell
❯ kubectl exec -n ovn-kubernetes ovnkube-node-vkmkt -c ovnkube-controller -- ovn-nbctl list nat
...
_uuid               : 7855a3a5-412c-4083-963c-b11aa80b7784
allowed_ext_ips     : []
exempted_ext_ips    : []
external_ids        : {}
external_ip         : "172.18.0.2"
external_mac        : []
external_port_range : "32768-60999"
gateway_port        : []
logical_ip          : "10.244.1.3"
logical_port        : []
match               : "ip4.dst == $a712973235162149816" # added condition matching E/W traffic when advertised
options             : {stateless="false"}
priority            : 0
type                : snat

...

_uuid               : 7be1b70b-88c7-4482-85ff-487663be9eda
addresses           : ["172.18.0.2", "172.18.0.3", "172.18.0.4", "172.19.0.2", "172.19.0.3", "172.19.0.4"]
external_ids        : {ip-family=v4, "k8s.ovn.org/id"="default-network-controller:EgressIP:node-ips:v4:default", "k8s.ovn.org/name"=node-ips, "k8s.ovn.org/owner-controller"=default-network-controller, "k8s.ovn.org/owner-type"=EgressIP, network=default}
name                : a712973235162149816
...
```

For CUDNs in local gateway mode, this is handled on a similar way with a
conditional SNAT to the network's masquerade IP which would then finally be
SNATed to the node IP on the host.

```shell
❯ kubectl exec -n ovn-kubernetes ovnkube-node-vkmkt -c ovnkube-controller -- ovn-nbctl list nat
...
_uuid               : 61b26442-fa08-4aa8-b326-97afb71edab1
allowed_ext_ips     : []
exempted_ext_ips    : []
external_ids        : {"k8s.ovn.org/network"=cluster_udn_udn-l2, "k8s.ovn.org/topology"=layer2}
external_ip         : "169.254.0.11"
external_mac        : []
external_port_range : "32768-60999"
gateway_port        : []
logical_ip          : "22.100.0.0/16"
logical_port        : []
match               : "ip4.dst == $a712973235162149816"
options             : {stateless="false"}
priority            : 0
type                : snat
...
```

Egress IP SNAT is unaffected.

#### Route import

When BGP routes get installed in a node's routing table, OVN-Kubernetes
synchronizes them to the gateway router of the corresponding OVN network making
them available for egress in shared gateway mode.

```shell
❯ kubectl exec -n ovn-kubernetes ovnkube-node-vkmkt -c ovnkube-controller -- ovn-nbctl lr-route-list 076a4cba-c680-4fa3-ae2f-1ce7e0a1e153
IPv4 Routes
Route Table <main>:
           169.254.0.0/17               169.254.0.4 dst-ip rtoe-GR_ovn-worker2
            10.244.0.0/16                100.64.0.1 dst-ip
            172.26.0.0/16                172.18.0.5 dst-ip rtoe-GR_ovn-worker2  # learned route synced from host VRF
                0.0.0.0/0                172.18.0.1 dst-ip rtoe-GR_ovn-worker2
```

### Host network controllers: impacts on host networking stack

#### Ingress OVS flows

Flows are added to handle the ingress of N/S traffic addressing IPs of the
advertised pod networks. This traffic is forwarded to the corresponding patch
port of the network and is then handled by OVN with no extra changes required in
shared gateway mode.

```shell
❯ kubectl exec -n ovn-kubernetes ovnkube-node-q76br -c ovnkube-controller -- ovs-ofctl dump-flows breth0
...
 # flows forwarding pod networks to the corresponding patch ports
 cookie=0xdeff105, duration=445.802s, table=0, n_packets=0, n_bytes=0, idle_age=445, priority=300,ip,in_port=1,nw_dst=10.244.0.0/24 actions=output:2
 cookie=0xdeff105, duration=300.323s, table=0, n_packets=0, n_bytes=0, idle_age=300, priority=300,ip,in_port=1,nw_dst=22.100.0.0/16 actions=output:3
```

In local gateway mode, the traffic is forwarded to the host networking stack
from where it is routed to the network management port.

```shell
❯ kubectl exec -n ovn-kubernetes ovnkube-node-vkmkt -c ovnkube-controller -- ovs-ofctl dump-flows breth0
 ...
 # flows forwarding pod networks to host
 cookie=0xdeff105, duration=57.620s, table=0, n_packets=0, n_bytes=0, idle_age=57, priority=300,ip,in_port=1,nw_dst=22.100.0.0/16 actions=LOCAL
 cookie=0xdeff105, duration=9589.541s, table=0, n_packets=0, n_bytes=0, idle_age=9706, priority=300,ip,in_port=1,nw_dst=10.244.1.0/24 actions=LOCAL
 ...

❯ kubectl exec -n ovn-kubernetes ovnkube-node-vkmkt -c ovnkube-controller -- ip route
...
# routing to the default pod network management port
10.244.0.0/16 via 10.244.1.1 dev ovn-k8s-mp0 
10.244.1.0/24 dev ovn-k8s-mp0 proto kernel scope link src 10.244.1.2
...

❯ kubectl exec -n ovn-kubernetes ovnkube-node-vkmkt -c ovnkube-controller -- ip rule
...
# for a CUDN, an ip rule takes care of routing on the correct VRF
2000: from all to 22.100.0.0/16 lookup 1010
...

❯ kubectl exec -n ovn-kubernetes ovnkube-node-vkmkt -c ovnkube-controller -- ip r show table 1010
...
# also routing to the CUDN management port
22.100.0.0/16 dev ovn-k8s-mp1 proto kernel scope link src 22.100.0.2 
...
```

#### Host SNAT behavior with BGP Advertisement

In the same way that was done for the OVN configuration, the host networking
stack configuration is updated to inhibit the SNAT for N/S traffic.

```shell
❯ kubectl exec -n ovn-kubernetes ovnkube-node-vkmkt -c ovnkube-controller -- nft list ruleset
...
  set remote-node-ips-v4 {
    type ipv4_addr
    comment "Block egress ICMP needs frag to remote Kubernetes nodes"
    elements = { 172.18.0.3, 172.18.0.4,
           172.19.0.2, 172.19.0.4 }
  }
...
  chain ovn-kube-pod-subnet-masq {
    # ip daddr condition added if default pod network advertised
    ip saddr 10.244.1.0/24 ip daddr @remote-node-ips-v4 masquerade # ip daddr condition if advertised
  }
...
```

#### VRF-Lite isolation

To ensure isolation in VRF-Lite configurations, the default route pointing to
the default VRF gateway present on the network's VRF is inhibited. Thus only BGP
installed routes will be used for N/S traffic.

```shell
❯ kubectl exec -n ovn-kubernetes ovnkube-node-vkmkt -c ovnkube-controller -- ip r show table 1010
# default match unreachable
unreachable default metric 4278198272 
...
# installed route going through interface attached to VRF
172.26.0.0/16 nhid 28 via 172.19.0.5 dev eth1 proto bgp metric 20 
```

#### CUDN isolation

To ensure CUDN isolation in local gateway mode filtering rules are added to the host configuration

```shell
❯ kubectl exec -n ovn-kubernetes ovnkube-node-vkmkt -c ovnkube-controller -- nft list ruleset
...
  set advertised-udn-subnets-v4 {
    type ipv4_addr
    flags interval
    comment "advertised UDN V4 subnets"
    elements = { 22.100.0.0/16 comment "cluster_udn_udn-l2" }
  }
...
  chain udn-bgp-drop {
          comment "Drop traffic generated locally towards advertised UDN subnets"
          type filter hook output priority filter; policy accept;
          ct state new ip daddr @advertised-udn-subnets-v4 counter packets 0 bytes 0 drop
          ct state new ip6 daddr @advertised-udn-subnets-v6 counter packets 0 bytes 0 drop
  }
...
```

These rules are inhibited if OVN-Kubernetes is configured in "loose advertised
UDN isolation mode".

## Troubleshooting

### Troubleshooting RouteAdvertisements

Check `RouteAdvertisement` status for configuration errors:

```shell
❯ kubectl get ra
NAME       STATUS
default    Accepted
extranet   Not Accepted: configuration pending: no networks selected
```

The controller validates every value it interpolates into the generated raw
FRR configuration before generating it. A `RouteAdvertisements` is reported as
`Not Accepted` and no `FRRConfiguration` is generated for it when:

- `spec.targetVRF` is set to something other than `auto` that is not a valid
  Linux interface name (at most 15 bytes, no whitespace, `/` or `:`)
- a selected network has an EVPN VNI outside 1-16777215, a route target that is
  not of the form `ASN:NN`, `A.B.C.D:NN` or `*:NN`, or a VRF name that is not
  a valid interface name
- a neighbor of a selected `FRRConfiguration` has an address that is not an IP
  address, or an interface that is not a valid interface name

The condition message names the offending field, value and object.
`FRRConfiguration` objects generated for that `RouteAdvertisements` before
input validation existed are deleted, so a value rejected after an upgrade
does not stay in the configuration the nodes run; once none remain, the
`RouteAdvertisements` is removed from the annotations of the selected networks
and they stop being advertised. These checks apply to hand-written
`NetworkAttachmentDefinition` objects too, which are not covered by the
`ClusterUserDefinedNetwork` CRD validation.

Check that `FRRConfiguration` have been generated as expected:

```shell
❯ kubectl get frrconfiguration -n frr-k8s-system
NAME                   AGE
ovnk-generated-66plb   14m
ovnk-generated-fxncs   13m
ovnk-generated-grdfg   14m
ovnk-generated-qhz9b   14m
ovnk-generated-sgphk   13m
ovnk-generated-vtwpv   13m
receive-all            14m
```

Expected `FRRConfiguration` are:
- Any manual configuration done to import routes
- MetalLB generated FRRConfiguration if in use
- One of ovnk-generated-XXXXX configuration per RouteAdvertisement and selected FRRConfiguration/Node combination

### Troubleshooting FRR-K8s

FRR-K8s merges all FRRConfiguration into a single FRR configuration for each
node. The status of generating that configuration and applying it to FRR daemon
running on each node is relayed through `FRRNodeStates`:

```shell
❯ kubectl get -n frr-k8s-system frrnodestates
NAME                AGE
ovn-control-plane   16m
ovn-worker          16m
ovn-worker2         16m

$ oc describe -n openshift-frr-k8s frrnodestates worker-0.ostest.test.metalkube.org 
Name:         worker-0.ostest.test.metalkube.org
Namespace:    
Labels:       <none>
Annotations:  <none>
API Version:  frrk8s.metallb.io/v1beta1
Kind:         FRRNodeState
Metadata:
  Creation Timestamp:  2025-09-10T11:29:44Z
  Generation:          1
  Resource Version:    52036
  UID:                 34f67799-9642-40a3-a378-67ca3ad5dfd2
Spec:
Status:
  Last Conversion Result:  success # whether FRRConfiguration merge and conversion to FRR config was successful 
  Last Reload Result:      success # whether resulting FRR config was applied correctly
  Running Config:
    # the FRR running config is displayed here
...
```

FRR-K8s provides metrics:

```text
  Namespace = "frrk8s"
  Subsystem = "bgp"

  SessionUp = metric{
    Name: "session_up",
    Help: "BGP session state (1 is up, 0 is down)",
  }

  UpdatesSent = metric{
    Name: "updates_total",
    Help: "Number of BGP UPDATE messages sent",
  }

  Prefixes = metric{
    Name: "announced_prefixes_total",
    Help: "Number of prefixes currently being advertised on the BGP session",
  }

  ReceivedPrefixes = metric{
    Name: "received_prefixes_total",
    Help: "Number of prefixes currently being received on the BGP session",
  }
```

### Troubleshooting FRR

FRR is deployed by FRR-K8s as a daemonset and runs on every node:

```shell
❯ kubectl get pods -n frr-k8s-system -o wide
NAME                                     READY   STATUS    RESTARTS   AGE   IP           NODE                NOMINATED NODE   READINESS GATES
frr-k8s-daemon-5cqbq                     6/6     Running   0          22m   172.18.0.4   ovn-worker2         <none>           <none>
frr-k8s-daemon-6hmzb                     6/6     Running   0          22m   172.18.0.3   ovn-worker          <none>           <none>
frr-k8s-daemon-gsmml                     6/6     Running   0          22m   172.18.0.2   ovn-control-plane   <none>           <none>
...
```

Different aspects of the running daemons can be checked through the `vtysh` CLI.
Some examples are:

- The running configuration:

```shell
$ kubectl exec -ti -n frr-k8s-system frr-k8s-daemon-5cqbq -c frr -- vtysh -c "show running-conf"
Building configuration...

Current configuration:
!
frr version 8.5.3
frr defaults traditional
hostname ovn-worker2
log file /etc/frr/frr.log informational
log timestamp precision 3
no ip forwarding
service integrated-vtysh-config
!
router bgp 64512
 no bgp ebgp-requires-policy
 no bgp hard-administrative-reset
...
```

- The BGP session states:

```shell
❯ kubectl exec -ti -n frr-k8s-system frr-k8s-daemon-5cqbq -c frr -- vtysh -c "show bgp neighbor 172.18.0.5"
BGP neighbor is 172.18.0.5, remote AS 64512, local AS 64512, internal link
  …
Hostname: 78d5a0f1d3cd
  BGP version 4, remote router ID 172.18.0.5, local router ID 172.18.0.4
  BGP state = Established, up for 00:01:29
  ...
    Last reset 00:03:30,  Peer closed the session
...
```

- The actual routes exchanged through BGP:

```shell
❯ kubectl exec -ti -n frr-k8s-system frr-k8s-daemon-5cqbq -c frr -- vtysh -c "show bgp ipv4"
BGP table version is 2, local router ID is 172.18.0.4, vrf id 0
Default local pref 100, local AS 64512
Status codes:  s suppressed, d damped, h history, * valid, > best, = multipath,
               i internal, r RIB-failure, S Stale, R Removed
Nexthop codes: @NNN nexthop's vrf id, < announce-nh-self
Origin codes:  i - IGP, e - EGP, ? - incomplete
RPKI validation codes: V valid, I invalid, N Not found

    Network          Next Hop            Metric LocPrf Weight Path
 *> 10.244.0.0/24    0.0.0.0                  0         32768 i
 *> 22.100.0.0/16    0.0.0.0                  0         32768 i
 *>i172.26.0.0/16    172.18.0.5               0    100      0 i


Displayed  2 routes and 2 total paths
```

- Routes installed on the host and their origin:

```shell
❯ kubectl exec -ti -n frr-k8s-system frr-k8s-daemon-5cqbq -c frr -- vtysh -c "show ip route"
Codes: K - kernel route, C - connected, S - static, R - RIP,
       O - OSPF, I - IS-IS, B - BGP, E - EIGRP, N - NHRP,
       T - Table, v - VNC, V - VNC-Direct, F - PBR,
       f - OpenFabric,
       > - selected route, * - FIB route, q - queued, r - rejected, b - backup
       t - trapped, o - offload failure

...
B>* 172.26.0.0/16 [200/0] via 172.18.0.5, breth0, weight 1, 00:41:11
...
```

Most of these commands have variations to check the same information specific to
a VRF:

```shell
❯ kubectl exec -ti -n frr-k8s-system frr-k8s-daemon-gv76r -c frr -- vtysh -c "show ip route vrf udn-l2"
Codes: K - kernel route, C - connected, S - static, R - RIP,
       O - OSPF, I - IS-IS, B - BGP, E - EIGRP, N - NHRP,
       T - Table, v - VNC, V - VNC-Direct, A - Babel, F - PBR,
       f - OpenFabric,
       > - selected route, * - FIB route, q - queued, r - rejected, b - backup
       t - trapped, o - offload failure

VRF udn-l2:
...
B>* 172.26.0.0/16 [200/0] via 172.18.0.5, breth0 (vrf default), weight 1, 01:39:55
...
```

### Troubleshooting dataplane

FRR applies its configuration to the host networking stack in the form of
routes. Thus standard tooling can be used for dataplane troubleshooting:
connectivity checks, tcpdump, ovn-trace, ovs-trace, ...

## Best Practices

TBD

## Future Items

- No overlay support

## Related Features

- [EVPN](./evpn.md)

## Known Limitations

- The `route-advertisements` feature is only supported in inter-connect mode.
- Advertised CUDNs must have a name of length under 16 characters to use a
  homonym and predictable VRF name.
- Pod network IPs must be advertised from all nodes. As such, a
  `RouteAdvertisements` instance including `PodNetwork` as `advertisements` type
  must select all nodes with its `nodeSelector`.
- VRF-Lite configurations are only supported in local gateway mode.
- Egress IP advertisements are not supported for Layer 2 CUDNs or in VRF-Lite
  configurations.
- Egress IPs will be advertised over the selected BGP sessions regardless of
  whether they are assigned to the same interface as those sessions are
  established over or not, probably making the advertisements ineffective if
  they are not the same.

## References

- [FRR-k8s](https://github.com/metallb/frr-k8s)
- [FRR](https://frrouting.org/)
