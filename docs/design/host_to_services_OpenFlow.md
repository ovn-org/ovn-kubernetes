
# Host -> Services using OpenFlow with shared gateway bridge

## Background

In order to allow host to Kubernetes service access packets originating from the host must go into OVN in order to hit
load balancers and be routed to the appropriate destination. Typically when accessing a services backed by an OVN
networked pod, a single interface can be used (ovn-k8s-mp0) in order to get into the local worker switch, hit the load
balancer, and reach the pod endpoint. However, when a service is backed by host networked pods, the behavior becomes
more complex. For example, if the host access a service, where the backing endpoint is the host itself, then the packet
must hairpin back to the host after being load balanced. There are additional complexities to consider, such as when an
host network endpoint is using a secondary IP on a NIC. To be able to solve all of the potential use cases for service
traffic, OpenFlow is leveraged with OVN-Kubernetse programmed flows in order to steer service traffic.

## Introduction

OpenFlow on the OVS gateway bridge to handle all host to service traffic and this methodology is used for both gateway
modes (local and shared). However, the paths that local and shared take may be slightly different as local gateway mode
requires that all service traffic uses host's routing stack as a next hop before leaving the node.

Accessing services from the host via the shared gateway bridge means that traffic from the host enters the shared 
gateway bridge and is then forwarded into the Gateway Router (GR). The complexity with this solution revolves around 
the fact that the host and the GR both share the same node IP address. Due to this fact, the host and OVN can never know
the other is using the same IP address and thus requires masquerading done inside of the shared gateway bridge. 
Additionally, we want to avoid the shared gateway having to act like a router and managing control plane functions like
ARP. This would add a bunch of overhead to managing the shared gateway bridge and greatly increase the cost of this implementation. 

In order to avoid ARP, both the OVN GR and the host think that the service CIDR is an externally routed network. In other
words this both OVN and the host think that to reach the service CIDR they need to route their packet to some next hop
external to the node. In the past OVN-Kubernetes has leveraged the default gateway as that next hop, routing all service traffic
towards that default gateway. However, this behavior relied on the default gateway existing and being able to ARP for its
MAC address. In the current implementation this dependency on the gateway has been removed, and a secondary network is
configured on OVN and the host with a fake next hop in that secondary network. The default secondary networks for IPv4 and IPv6
are:
 - `169.254.169.0/29`
 - `fd69::/125`

OVN is assigned 169.254.169.1 and fd69::1, while the Host uses 169.254.169.2 and fd69::2. The next hop address used is
169.254.169.4 and fd69::4.

This subnet is only used for services communication, and service access from the host will be routed via:
```text
10.96.0.0/16 via 169.254.169.4 dev breth0 mtu 1400
```

By doing this the destination MAC address will be the next hop MAC and can be manipulated to act as masquerade MAC to the host. 
As the host goes to send traffic to the next hop, OpenFlow rules in br-ex hijack the packet, modify it, and redirect it 
to the OVN GR. Similarly the reply packets from OVN GR are modified and masqueraded, before being sent back to the host.

Since the next hop is not a real address it cannot perform ARP response, so static ARP entries are added to the host
and OVN GR.

For Host to pod access (and vice versa) the management port (ovn-k8s-mp0) is still used.

## New Load Balancer Behavior

Load balancers with OVN are placed either on a router or worker switch. Previously, the "node port" load balancers (created on a per node basis) were applied to the GR and the worker switch, while singleton cluster wide load balancers for Cluster IP services were applied across all worker switches. With the new implementation, Cluster IP service traffic is destined for GR from the host and therefore the GR requires load balancers that can handle Cluster IP. In addition, the load balancer must not have the node's IP address as and endpoint. This is due to the fact that on a GR the node IP address is used. Therefore if a load balancer on the GR were to DNAT to its own node IP, the packet would be dropped.

To solve this problem, an additional load balancer is added with this implementation. The purpose of this new load balancer is to accommodate host endpoints. If endpoints are added for a service that contain a host endpoint, that VIP is moved to the new load balancer. Additionally, if one of those endpoints contain this node's IP address, it is replaced with the host's special masqueraded IP (IPv4: 169.254.169.2, IPv6: fd69::2).

## Use Cases

The following sections go over each potential traffic path originating from Host to Service. Note Host -> node port or external IP services are DNAT'ed in iptables to the cluster IP address before being sent out. Therefore the behavior of any host to service is essentially the same, forwarded towards the Cluster IP via the shared gateway bridge.

For all of the following use cases, follow this topology:

```text
          host (ovn-worker, 172.18.0.3, 169.254.169.2) 
           |
eth0----|breth0| ------ 172.18.0.3   OVN GR 100.64.0.4 --- join switch --- ovn_cluster_router --- 10.244.1.3 pod
                        169.254.169.1

```

The service used in the following use cases is:
```text
[trozet@trozet contrib]$ kubectl get svc web-service
NAME          TYPE       CLUSTER-IP     EXTERNAL-IP   PORT(S)                       AGE
web-service   NodePort   10.96.146.87   <none>        80:30015/TCP,9999:32326/UDP   4m54s

```

OVN masquerade IPs are 169.254.169.1, and fd69::1

Another worker node exists, ovn-worker2 at 172.18.0.4.

### OVN masquerade addresses
- IPv4
  - 169.254.169.1: OVN masquerade address
  - 169.254.169.2: host masquerade address
  - 169.254.169.3: local ETP masquerade address (not used for shared GW, kept for parity)
  - 169.254.169.4: dummy next-hop masquerade address
- IPv6
  - fd69::1: OVN masquerade address
  - fd69::2: host masquerade address
  - fd69::3: local ETP masquerade address (not used for shared GW, kept for parity)
  - fd69::4: dummy next-hop masquerade address

### Configuration Details
#### Host
As previously mentioned the host is configured with the `169.254.169.2` and `fd69::2` addresses. These are configured
on the shared gateway bridge interface (typically br-ex or breth0) in the kernel. Additionally a route for service traffic
is added as well as a static ARP/IPv6 neighbor entry.

The services will now be reached from the nodes via this dummy next hop masquerade IP (i.e. `169.254.169.4`).

Additionally, to enable the hairpin scenario, we need to provision a route specifying traffic originating from the host
(acting as an host networked endpoint reply) must be routed to the OVN masquerade address (i.e. `169.254.169.1`) using
the source IP address of the real host IP.
This route looks like: `169.254.169.1/32 dev breth0 mtu 1400 src <node IP address>`

#### OVN
In OVN we do not explicitly configure the `169.254.169.1` and `fd69::1` addresses on the GR interface. OVN is only able
to have a single primary IP on the interface. Instead, we simply add a MAC Binding (ARP entry equivalent) for the next
hop address, as well as a route for the secondary subnets to the next hop. This is all that is required in order to allow
OVN to route the packet towards the fake next hop.

The SBDB MAC binding will look like this (one per node):
```text
_uuid               : f244faba-19ad-4b08-bffe-d0b52b270410
datapath            : 7cc30875-0c68-4ecc-8193-a9fe3abb6cd5
ip                  : "169.254.169.4"
logical_port        : rtoe-GR_ovn-worker
mac                 : "0a:58:a9:fe:a9:04"
timestamp           : 0
```

Each OVN GR will then have a route table that looks like this:
```text
[root@ovn-control-plane ~]# ovn-nbctl lr-route-list GR_ovn-worker
IPv4 Routes
Route Table <main>:
         169.254.169.0/29             169.254.169.4 dst-ip rtoe-GR_ovn-worker
            10.244.0.0/16                100.64.0.1 dst-ip
                0.0.0.0/0                172.18.0.1 dst-ip rtoe-GR_ovn-worker

```

Notice 169.254.169.0 route via 169.254.169.4 works, even though OVN does not have an IP on that subnet:
```text
[root@ovn-control-plane ~]# ovn-nbctl show GR_ovn-worker
router 7c1323d5-f388-449f-94cb-51216194c606 (GR_ovn-worker)
    port rtoj-GR_ovn-worker
        mac: "0a:58:64:40:00:03"
        networks: ["100.64.0.3/16"]
    port rtoe-GR_ovn-worker
        mac: "02:42:ac:12:00:02"
        networks: ["172.18.0.2/16"]
    nat 98e32e9b-e8f1-413c-881d-cfdfd5a02d43
        external ip: "172.18.0.3"
        logical ip: "10.244.0.0/16"
        type: "snat"

```

This works because the output port `rtoe_GR_ovn-worker` is configured on the route. OVN will simply lookup the MAC binding
for 169.254.169.4, and then forward it out the rtoe_GR_ovn-worker interface, while SNAT'ing the source IP of the packet to
its primary address of 1721.8.0.3.

#### OVS
The gateway bridge flows are managed by OVN-Kubernetes. Priority 500 flows are added which are specifically there to handle
service traffic for this design. These flows handle masquerading between the host and OVN, while flows in later tables
take care of rewriting MAC addresses.

### OpenFlow Flows

With the new implementation comes new OpenFlow rules in the shared gateway bridge. The following flows are added and used:
```text
 cookie=0xdeff105, duration=793.273s, table=0, n_packets=0, n_bytes=0, priority=500,ip,in_port="patch-breth0_ov",nw_src=172.18.0.3,nw_dst=169.254.169.2 actions=ct(commit,table=4,zone=64001,nat(dst=172.18.0.3))
 cookie=0xdeff105, duration=793.273s, table=0, n_packets=0, n_bytes=0, priority=500,ip,in_port=LOCAL,nw_dst=169.254.169.1 actions=ct(table=5,zone=64002,nat)
 cookie=0xdeff105, duration=793.273s, table=0, n_packets=0, n_bytes=0, priority=500,ip,in_port=LOCAL,nw_dst=10.96.0.0/16 actions=ct(commit,table=2,zone=64001,nat(src=169.254.169.2))
 cookie=0xdeff105, duration=793.273s, table=0, n_packets=0, n_bytes=0, priority=500,ip,in_port="patch-breth0_ov",nw_src=10.96.0.0/16,nw_dst=169.254.169.2 actions=ct(table=3,zone=64001,nat)

 cookie=0xdeff105, duration=5.507s, table=2, n_packets=0, n_bytes=0, actions=set_field:02:42:ac:12:00:03->eth_dst,output:"patch-breth0_ov"
 cookie=0xdeff105, duration=5.507s, table=3, n_packets=0, n_bytes=0, actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],set_field:02:42:ac:12:00:03->eth_dst,LOCAL
 cookie=0xdeff105, duration=793.273s, table=4, n_packets=0, n_bytes=0, ip actions=ct(commit,table=3,zone=64002,nat(src=169.254.169.1))
 cookie=0xdeff105, duration=793.273s, table=5, n_packets=0, n_bytes=0, ip actions=ct(commit,table=2,zone=64001,nat)

```

How these flows are used will be explained in more detail in the following sections. The main thing to remember for now is that the shared gateway bridge will use Conntrack zones 64001 and 64002 to handle the new masquerading functionality.

### Host -> Service -> OVN Pod

This is the most simple case where a host wants to reach a service backed by an OVN Networked pod. For this example the service contains a single endpoint as an ovn networked pod on ovn-worker node:
```text
[trozet@trozet contrib]$ kubectl get ep web-service
NAME          ENDPOINTS                       AGE
web-service   10.244.1.3:80,10.244.1.3:9999   6m13s
```

The general flow is:

1. TCP Packet is sent via the host (ovn-worker) to an service:
CT Entry:
```text
tcp      6 114 TIME_WAIT src=172.18.0.3 dst=10.96.146.87 sport=47108 dport=80 src=10.96.146.87 dst=172.18.0.3 sport=80 dport=47108 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 use=1

```
2. The host routes this packet towards the next hop's (169.254.169.4) MAC address via shared gateway bridge breth0.
3. Flows in breth0 hijack the packet, SNAT to the host's masquerade IP (169.254.169.2) and send it to OF table 2:
```text
cookie=0xdeff105, duration=1136.261s, table=0, n_packets=12, n_bytes=884, priority=500,ip,in_port=LOCAL,nw_dst=10.96.0.0/16 actions=ct(commit,table=2,zone=64001,nat(src=169.254.169.2))
```
4. In table 2, the destination MAC address is modified to be the MAC of the OVN GR. Note, although the source MAC is the host's, OVN does not care so this is left unmodified.
```text
cookie=0xdeff105, duration=1.486s, table=2, n_packets=12, n_bytes=884, actions=set_field:02:42:ac:12:00:03->eth_dst,output:"patch-breth0_ov"
```
CT Entry:
```text
tcp      6 114 TIME_WAIT src=172.18.0.3 dst=10.96.146.87 sport=47108 dport=80 src=10.96.146.87 dst=169.254.169.2 sport=80 dport=47108 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 zone=64001 use=1
```
5. OVN GR receives the packet, DNAT's to pod endpoint IP:
CT Entry:
```text
tcp      6 114 TIME_WAIT src=169.254.169.2 dst=10.96.146.87 sport=47108 dport=80 src=10.244.1.3 dst=169.254.169.2 sport=80 dport=47108 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 zone=16 use=1
```
6. OVN GR SNAT's to the join switch IP and sends the packet towards the pod:
CT Entry:
```text
tcp      6 114 TIME_WAIT src=169.254.169.2 dst=10.244.1.3 sport=47108 dport=80 src=10.244.1.3 dst=100.64.0.4 sport=80 dport=47108 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 use=1
```

#### Reply

The reply packet simply uses same reverse path and packets are unNAT'ed on their way back towards breth0 and eventually 
the LOCAL host port. The OVN GR will think it is routing towards the next hop and set the dest MAC to be the MAC of
169.254.169.4. In OpenFlow, the return packet will hit this first flow in the shared gateway bridge:
```text
cookie=0xdeff105, duration=1136.261s, table=0, n_packets=11, n_bytes=1670, priority=500,ip,in_port="patch-breth0_ov",nw_src=10.96.0.0/16,nw_dst=169.254.169.2 actions=ct(table=3,zone=64001,nat)
```
This flow will unDNAT the packet, and send to table 3:
```text
cookie=0xdeff105, duration=1.486s, table=3, n_packets=11, n_bytes=1670, actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],set_field:02:42:ac:12:00:03->eth_dst,LOCAL
```
This flow will move the dest MAC (next hop MAC) to be the source, and set the new dest MAC to be the MAC of the host.
This ensures the Linux host thinks it is still talking to the external next hop. The packet is then delivered to the host.


### Host -> Service -> Host Endpoint on Different Node

This example becomes slightly more complex as the packet must be hairpinned by the GR and then sent out of the node. In this example the backing service endpoint will be a pod running on the other worker node (ovn-worker2) at 172.18.0.4:
```text
[trozet@trozet contrib]$ kubectl get ep web-service
NAME          ENDPOINTS                       AGE
web-service   172.18.0.4:80,172.18.0.4:9999   32m

```

Steps 1 through 4 in this example are the same as the previous use case.

5. OVN GR receives the packet, DNAT's to ovn-worker2's endpoint IP:
CT Entry:
```text
tcp      6 116 TIME_WAIT src=169.254.169.2 dst=10.96.146.87 sport=55978 dport=80 src=172.18.0.4 dst=169.254.169.2 sport=80 dport=55978 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 zone=16 use=1
```
6. OVN GR hairpins the packet back towards breth0 while SNAT'ing to the GR IP 172.18.0.3, and forwarding towards 172.18.0.4:
```text
tcp      6 116 TIME_WAIT src=172.18.0.3 dst=172.18.0.4 sport=55978 dport=80 src=172.18.0.4 dst=172.18.0.3 sport=80 dport=55978 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 zone=64000 use=1
```
7. As this packet comes back into breth0 it is treated like any other normal packet going from OVN to the external network.
The packet is Conntracked in zone 64000, marked with 1, and sent out of the eth0 interface:
```text
cookie=0xdeff105, duration=15820.270s, table=0, n_packets=0, n_bytes=0, priority=100,ip,in_port="patch-breth0_ov" actions=ct(commit,zone=64000,exec(load:0x1->NXM_NX_CT_MARK[])),output:eth0
```
CT Entry:
```text
tcp      6 116 TIME_WAIT src=172.18.0.3 dst=172.18.0.4 sport=55978 dport=80 src=172.18.0.4 dst=172.18.0.3 sport=80 dport=55978 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 zone=64000 use=1
```

#### Reply

When the reply packet comes back into ovn-worker via the eth0 interface, the packet will be forwarded back into OVN GR 
due to a CT match in zone 64000 with a marking of 1:
```text
cookie=0xdeff105, duration=15820.270s, table=0, n_packets=5442, n_bytes=4579489, priority=50,ip,in_port=eth0 actions=ct(table=1,zone=64000)
cookie=0xdeff105, duration=15820.270s, table=1, n_packets=0, n_bytes=0, priority=100,ct_state=+est+trk,ct_mark=0x1,ip actions=output:"patch-breth0_ov"
cookie=0xdeff105, duration=15820.270s, table=1, n_packets=0, n_bytes=0, priority=100,ct_state=+rel+trk,ct_mark=0x1,ip actions=output:"patch-breth0_ov"
```

OVN GR will then handle unSNAT, unDNAT and send the packet back towards breth0 where
the packet will be handled the same way as it was in the previous example.

### Host -> Service -> Host Endpoint on Same Node

This is the most complicated use case. Unlike the previous examples, multiple sessions have to be established and masqueraded
between the host and OVN in order to trick the host into thinking it has two external connections that are not the same.
This avoids issues with RPF as well as short-circuiting where the host would skip sending traffic back to OVN because it
knows the traffic destination is local to itself. In this example a host network pod is used as the backend endpoint on ovn-worker:
```text
[trozet@trozet contrib]$ kubectl get ep web-service
NAME          ENDPOINTS                       AGE
web-service   172.18.0.3:80,172.18.0.3:9999   42m
```

There is another manifestation of this case. Sometimes, an endpoint contains a *secondary* interface on a node. This can happen when endpoints are manually managed (especially in the case of api-server trickery). Thus, the endpoints look like
```text
$ kubectl get ep kubernetes
NAME          ENDPOINTS                       AGE
kubernetes    172.20.0.2:4443                  42m
```

where `172.20.0.2` is an "extra" address, maybe even on a different interface (e.g. `lo`).

Like the previous example, Steps 1-4 are the same. Continuing with Step 5   :

5. OVN GR receives the packet, DNAT's to ovn-worker's endpoint IP. However, if the endpoint IP is the node's physical IP, then it is replaced in the OVN load-balancer backends with the host masquerade IP (169.254.169.2):
CT Entry:
```text
tcp      6 117 TIME_WAIT src=169.254.169.2 dst=10.96.146.87 sport=33316 dport=80 src=169.254.169.2 dst=169.254.169.2 sport=80 dport=33316 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 zone=16 use=1

```
6. OVN GR hairpins the packet back towards breth0 while SNAT'ing to the GR IP 172.18.0.3, and forwarding towards 169.254.169.2:
CT Entry:
```text
tcp      6 117 TIME_WAIT src=169.254.169.2 dst=169.254.169.2 sport=33316 dport=80 src=169.254.169.2 dst=172.18.0.3 sport=80 dport=33316 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 use=1
```
7. The packet is received in br-eth0, where it hits one of two flows, depending on if the destination is the masquerade IP or a "secondary" IP:
```text
cookie=0xdeff105, duration=2877.527s, table=0, n_packets=11, n_bytes=810, priority=500,ip,in_port="patch-breth0_ov",nw_src=172.18.0.3,nw_dst=169.254.169.2 actions=ct(commit,table=4,zone=64001,nat(dst=172.18.0.3)) #primary case
cookie=0xdeff105, duration=2877.527s, table=0, n_packets=11, n_bytes=810, priority=500,ip,in_port="patch-breth0_ov",nw_src=172.18.0.3,nw_dst=127.20.0.2 actions=ct(commit,table=4,zone=64001) # secondary case
```
This flow detects the packet is destined for the masqueraded host IP, DNATs it if necessary back to the host, and sends to table 4.
CT Entry:
```text
tcp      6 117 TIME_WAIT src=172.18.0.3 dst=169.254.169.2 sport=33316 dport=80 src=172.18.0.3 dst=172.18.0.3 sport=80 dport=33316 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 zone=64001 use=1
```
8. In table 4, the packet hits the following flow:
```text
cookie=0xdeff105, duration=2877.527s, table=4, n_packets=11, n_bytes=810, ip actions=ct(commit,table=3,zone=64002,nat(src=169.254.169.1))
```
Here, the packet is SNAT'ed to the special OVN masquerade IP (169.254.169.1) in order to obfuscate the node IP from the host as a source address.
CT Entry:
```text
tcp      6 117 TIME_WAIT src=172.18.0.3 dst=172.18.0.3 sport=33316 dport=80 src=172.18.0.3 dst=169.254.169.1 sport=80 dport=33316 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 zone=64002 use=1

```
9. The packet then enters table 3, where the MAC addresses are modified and sent to the LOCAL host.

The trick here is that the host thinks it has two different connections, which are really part of the same single connection:
1. 172.18.0.3-> 10.96.x.x
2. 169.254.169.1 -> 172.18.0.3

#### Reply
Reply traffic comes back into breth0 where via seeing a response to 169.254.169.1, we know this is a reply packet and thus hits an OpenFlow rule:
```text
cookie=0xdeff105, duration=2877.527s, table=0, n_packets=12, n_bytes=1736, priority=500,ip,in_port=LOCAL,nw_dst=169.254.169.1 actions=ct(table=5,zone=64002,nat)
```
This flow will unSNAT in zone 64002, and then send the packet to table 5. In table 5 it will hit this flow:
```text
cookie=0xdeff105, duration=2877.527s, table=5, n_packets=12, n_bytes=1736, ip actions=ct(commit,table=2,zone=64001,nat)
```
Here the packet will be unDNAT'ed and sent towards table 2:
```text
cookie=0xdeff105, duration=5.520s, table=2, n_packets=47, n_bytes=4314, actions=set_field:02:42:ac:12:00:03->eth_dst,output:"patch-breth0_ov"
```
Table 2 will modify the MAC accordingly and sent it back into OVN GR. OVN GR will then unSNAT, unDNAT and send the packet back to breth0 in the same fashion as the previous example.

