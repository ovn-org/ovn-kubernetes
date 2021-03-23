
# Host -> Services using OpenFlow with shared gateway bridge

## Background

In order to allow host to Kubernetes service access packets originating from the host must go into OVN in order to hit load balancers and be routed to the appropriate destination. Typically when accessing a services backed by an OVN networked pod, a single interface can be used (ovn-k8s-mp0) in order to get into the local worker switch, hit the load balancer, and reach the pod endpoint. However, when a service is backed by host networked pods, the behavior becomes more complex. For example, if the host access a service, where the backing endpoint is the host itself, then the packet must hairpin back to the host after being load balanced. Due to Reverse Path Filtering (RPF) in Linux, the packet cannot return to ovn-k8s-mp0, and thus we use the Distributed Gateway Port (DGP) via ovn-k8s-gw0 to get the packet back to the host.

## Introduction

The DGP solution functionally works, but adds complexity to the OVN-Kubernetes network architecture and creates an additional path into the host which is undesirable for Smart NIC deployments. A new implementation, leverages OpenFlow on the shared gateway bridge to handle all host to service traffic, and eliminates the need for DGP. This feature is only supported with shared gateway mode.

Accessing services from the host via the shared gateway bridge means that traffic from the host enters the shared gateway bridge and is then forwarded into the Gateway Router (GR). The complexity with this solution revolves around the fact that the host and the GR both share the same node IP address. Due to this fact, the host and OVN can never know the other is using the same IP address and thus requires masquerading done inside of the shared gateway bridge. Additionally, we want to avoid the shared gateway having to act like a router and managing control plane functions like ARP. This would add a bunch of overhead to managing the shared gateway bridge and greatly increase the cost of this implementation. 

In order to avoid ARP, both the OVN GR and the host think that the service CIDR is an externally routed network. This means that the host uses its default route when it wants to talk to the service CIDR, and routes the packet towards its default gateway on the shared gateway bridge. By doing this the destination MAC address will be the default gateway MAC and can be manipulated to act as masquerade MAC to the host. As the host goes to send traffic to the default gateway destined to the service CIDR, OpenFlow rules in br-ex hijack the packet, modify it, and redirect it to OVN GR. Similarly the reply packets from OVN GR are modified and masqueraded, before being sent back to the host.

For Host to pod access (and vice versa) the management port (ovn-k8s-mp0) is still used.

## New Load Balancer Behavior

Load balancers with OVN are placed either on a router or worker switch. Previously, the "node port" load balancers (created on a per node basis) were applied to the GR and the worker switch, while singleton cluster wide load balancers for Cluster IP services were applied across all worker switches. With the new implementation, Cluster IP service traffic is destined for GR from the host and therefore the GR requires load balancers that can handle Cluster IP. In addition, the load balancer must not have the node's IP address as and endpoint. This is due to the fact that on a GR the node IP address is used. Therefore if a load balancer on the GR were to DNAT to its own node IP, the packet would be dropped.

To solve this problem, an additional load balancer is added with this implementation. The purpose of this new load balancer is to accommodate host endpoints. If endpoints are added for a service that contain a host endpoint, that VIP is moved to the new load balancer. Additionally, if one of those endpoints contain this node's IP address, it is replaced with the host's special masqueraded IP (IPv4: 169.254.169.2, IPv6: fd69::2).

## Use Cases

The following sections go over each potential traffic path originating from Host to Service. Note Host -> node port or external IP services are DNAT'ed in iptables to the cluster IP address before being sent out. Therefore the behavior of any host to service is essentially the same, forwarded towards the Cluster IP via the shared gateway bridge.

For all of the following use cases, follow this topology:

```text
          host (ovn-worker, 172.18.0.3) 
           |
eth0----|breth0| ------ 172.18.0.3 OVN GR 100.64.0.4 --- join switch --- ovn_cluster_router --- 10.244.1.3 pod

```

The service used in the following use cases is:
```text
[trozet@trozet contrib]$ kubectl get svc web-service
NAME          TYPE       CLUSTER-IP     EXTERNAL-IP   PORT(S)                       AGE
web-service   NodePort   10.96.146.87   <none>        80:30015/TCP,9999:32326/UDP   4m54s

```

OVN masquerade IPs are 169.254.169.1, and fd69::1

Another worker node exists, ovn-worker2 at 172.18.0.4.

### OpenFlow Flows

With the new implementation comes new OpenFlow rules in the shared gateway bridge. The following flows are added and used:
```text
 cookie=0xdeff105, duration=793.273s, table=0, n_packets=0, n_bytes=0, priority=500,ip,in_port="patch-breth0_ov",nw_src=172.18.0.3,nw_dst=169.254.169.2 actions=ct(commit,table=4,zone=64001,nat(dst=172.18.0.3))
 cookie=0xdeff105, duration=793.273s, table=0, n_packets=0, n_bytes=0, priority=500,ip,in_port=LOCAL,nw_dst=169.254.169.1 actions=ct(table=5,zone=64002,nat)
 cookie=0xdeff105, duration=793.273s, table=0, n_packets=0, n_bytes=0, priority=500,ip,in_port=LOCAL,nw_dst=10.96.0.0/16 actions=ct(commit,table=2,zone=64001,nat(src=169.254.169.2))
 cookie=0xdeff105, duration=793.273s, table=0, n_packets=0, n_bytes=0, priority=500,ip,in_port="patch-breth0_ov",nw_src=10.96.0.0/16,nw_dst=169.254.169.2 actions=ct(table=3,zone=64001,nat)

 cookie=0xdeff105, duration=5.507s, table=2, n_packets=0, n_bytes=0, actions=mod_dl_dst:02:42:ac:12:00:03,output:"patch-breth0_ov"
 cookie=0xdeff105, duration=5.507s, table=3, n_packets=0, n_bytes=0, actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],mod_dl_dst:02:42:ac:12:00:03,LOCAL
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
2. The host routes this packet towards the default gateway 172.18.0.1's MAC address via shared gateway bridge breth0.
3. Flows in breth0 hijack the packet, SNAT to the host's masquerade IP (169.254.169.2) and send it to OF table 2:
```text
cookie=0xdeff105, duration=1136.261s, table=0, n_packets=12, n_bytes=884, priority=500,ip,in_port=LOCAL,nw_dst=10.96.0.0/16 actions=ct(commit,table=2,zone=64001,nat(src=169.254.169.2))
```
4. In table 2, the destination MAC address is modified to be the MAC of the OVN GR. Note, although the source MAC is the host's, OVN does not care so this is left unmodified.
```text
cookie=0xdeff105, duration=1.486s, table=2, n_packets=12, n_bytes=884, actions=mod_dl_dst:02:42:ac:12:00:03,output:"patch-breth0_ov"
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

The reply packet simply uses same reverse path and packets are unNAT'ed on their way back towards breth0 and eventually the LOCAL host port. The OVN GR will think it is routing towards the default gateway and set the dest MAC to be the MAC of 172.18.0.1 In OpenFlow, the return packet will hit this first flow in the shared gateway bridge:
```text
cookie=0xdeff105, duration=1136.261s, table=0, n_packets=11, n_bytes=1670, priority=500,ip,in_port="patch-breth0_ov",nw_src=10.96.0.0/16,nw_dst=169.254.169.2 actions=ct(table=3,zone=64001,nat)
```
This flow will unDNAT the packet, and send to table 3:
```text
cookie=0xdeff105, duration=1.486s, table=3, n_packets=11, n_bytes=1670, actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],mod_dl_dst:02:42:ac:12:00:03,LOCAL
```
This flow will move the dest MAC (default gw MAC) to be the source, and set the new dest MAC to be the MAC of the host. This ensures the Linux host thinks it is still talking to the default gateway. The packet is then delivered to the host.


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
7. As this packet comes back into breth0 it is treated like any other normal packet going from OVN to the external network. The packet is Conntracked in zone 64000 and sent out of the eth0 interface:
```text
cookie=0xdeff105, duration=2182.669s, table=0, n_packets=393581, n_bytes=592594193, priority=50,ip,in_port=eth0 actions=ct(table=1,zone=64000)
```
CT Entry:
```text
tcp      6 116 TIME_WAIT src=172.18.0.3 dst=172.18.0.4 sport=55978 dport=80 src=172.18.0.4 dst=172.18.0.3 sport=80 dport=55978 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 zone=64000 use=1
```

#### Reply

When the reply packet comes back into ovn-worker via the eth0 interface, the packet will be forwarded back into OVN GR (due to a CT match in zone 64000). OVN GR will then handle unSNAT, unDNAT and send the packet back towards breth0 where the packet will be handled the same way as it was in the previous example.

### Host -> Service -> Host Endpoint on Same Node

This is the most complicated use case. Unlike the previous examples, multiple sessions have to be established and masqueraded between the host and OVN in order to trick the host into thinking it has two external connections that are not the same. This avoids issues with RPF as well as short-circuiting where the host would skip sending traffic back to OVN because it knows the traffic destination is local to itself. In this example a host network pod is used as the backend endpoint on ovn-worker:
```text
[trozet@trozet contrib]$ kubectl get ep web-service
NAME          ENDPOINTS                       AGE
web-service   172.18.0.3:80,172.18.0.3:9999   42m
```
Like the previous example, Steps 1-4 are the same. Continuing with Step 5:

5. OVN GR receives the packet, DNAT's to ovn-worker's endpoint IP. However, the endpoint IP in this load balancer is **not** the node IP. It has been replaced by the load balancer behavior with the host masquerade IP (169.254.169.2):
CT Entry:
```text
tcp      6 117 TIME_WAIT src=169.254.169.2 dst=10.96.146.87 sport=33316 dport=80 src=169.254.169.2 dst=169.254.169.2 sport=80 dport=33316 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 zone=16 use=1

```
6. OVN GR hairpins the packet back towards breth0 while SNAT'ing to the GR IP 172.18.0.3, and forwarding towards 169.254.169.2:
CT Entry:
```text
tcp      6 117 TIME_WAIT src=169.254.169.2 dst=169.254.169.2 sport=33316 dport=80 src=169.254.169.2 dst=172.18.0.3 sport=80 dport=33316 [ASSURED] mark=0 secctx=system_u:object_r:unlabeled_t:s0 use=1
```
7. The packet is received in br-eth0, where it hits this flow:
```text
cookie=0xdeff105, duration=2877.527s, table=0, n_packets=11, n_bytes=810, priority=500,ip,in_port="patch-breth0_ov",nw_src=172.18.0.3,nw_dst=169.254.169.2 actions=ct(commit,table=4,zone=64001,nat(dst=172.18.0.3))
```
This flow detects the packet is destined for the masqueraded host IP, DNATs it accordingly while sending to table 4.
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
cookie=0xdeff105, duration=5.520s, table=2, n_packets=47, n_bytes=4314, actions=mod_dl_dst:02:42:ac:12:00:03,output:"patch-breth0_ov"
```
Table 2 will modify the MAC accordingly and sent it back into OVN GR. OVN GR will then unSNAT, unDNAT and send the packet back to breth0 in the same fashion as the previous example.

