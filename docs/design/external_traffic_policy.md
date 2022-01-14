# K8's Services' ExternalTraffic Policy Implementation

## Background

For [Kubernetes Services](https://kubernetes.io/docs/concepts/services-networking/service/) of type Nodeport or
Loadbalancer a user can set the `service.spec.externalTrafficPolicy` field to either `cluster` or `local` to denote
whether or not external traffic is routed to cluster-wide or node-local endpoints. The default value for the
`externalTrafficPolicy` field is `cluster`. In this configuration in ingress traffic is equally disributed across all
backends and the original client IP address is lost due to SNAT. If set to `local` then the client
source IP is propagated through the service and to the destination, while service traffic arriving at nodes without
local endpoints is dropped.

Setting an `ExternalTrafficPolicy` to `Local` is only allowed for Services of type `NodePort` or `LoadBalancer`. The
APIServer enforces this requirement.

## Implementing `externalTrafficPolicy` In OVN-Kubernetes

To properly implement this feature for all relevant traffic flows, required changing how OVN, Iptables rules, and
Physical OVS flows are updated and managed in OVN-Kubernetes

## OVN Load_Balancer configuration

Normally, each service in Kubernetes has a corresponding single Load_Balancer row created in OVN. This LB is attached
to all node switches and gateway routers (GWRs). ExternalTrafficPolicy creates multiple LBs, however.

Specifically, different load balancers are attached to switches versus routers. The node switch LBs handle traffic from pods,
whereas the gateway router LBs handle external traffic.

Thus, an additional LB is created with the `skip_snat="true"` option and is applied to the GatewayRouters
and Worker switches. It is needed to override the `lb_force_snat_ip=router_ip` option that is on all the Gateway Routers,
which allows ingress traffic to arrive at OVN managed endpoints with the original client IP.

All externally-accessible vips (NodePort, ExternalIPs, LoadBalancer Status IPs) for services with `externalTrafficPolicy:local`
will reside on this loadbalancer. The loadbalancer backends may be empty, depending on whether there are pods local
to that node.

## Handling Flows between the overlay and underlay

In this section we will look at some relevant traffic flows when a service's `externalTrafficPolicy` is `local`.  For
these examples we will be using a Nodeport service, but the flow is generally the same for ExternalIP and Loadbalancer
type services.

## Ingress Traffic

This section will cover the networking entities hit when traffic ingresses a cluster via a service to either host
networked pods or cluster networked pods. If its host networked pods, then the traffic flow is the same on both gateway modes. If its cluster networked pods, they will be different for each mode.

### External Source -> Service -> OVN pod

This case is the same as normal shared gateway traffic ingress, meaning the externally sourced traffic is routed into
OVN via flows on breth0, except in this case the new local load balancer is hit on the GR, which ensures the ip of the
client is preserved  by the time it gets to the destination Pod.

```text
          host (ovn-worker, 172.18.0.3) 
           |
eth0--->|breth0| -----> 172.18.0.3 OVN GR 100.64.0.4 --> join switch --> ovn_cluster_router --> 10.244.1.3 pod

```

#### **Local Gateway Mode**

The implementation of this case differs for local gateway from that for shared gateway. In local gateway if the above path is used, response traffic would be assymmetric since the default route for pod egress traffic is via `ovn-k8s-mp0`.

In local gateway mode, rather than sending the traffic from breth0 into OVN via gateway router, we use flows on breth0 to send it into the host.

```text
          host (ovn-worker, 172.18.0.3) ---- 172.18.0.3 LOCAL(host) -- iptables -- ovn-k8s-mp0 -- node-local-switch -- 10.244.1.3 pod
           ^
           ^
           |
eth0--->|breth0|

```

1. Match on the incoming traffic via it's nodePort, send it to `table=6`:

```
 cookie=0xb4e7084fbba8bb8a, duration=100.388s, table=0, n_packets=6, n_bytes=484, idle_age=9, priority=110,tcp,in_port=1,tp_dst=30820 actions=ct(commit,table=6,zone=64003)
```

2. Send it out to LOCAL ovs port on breth0 and traffic is delivered to the host:

```
 cookie=0xe745ecf105, duration=119.391s, table=6, n_packets=6, n_bytes=484, idle_age=28, priority=110 actions=LOCAL
```

3. In the host, we have an IPtable rule in the PREROUTING chain that DNATs this packet matched on nodePort to a masqueradeIP (169.254.169.3) used specially for this traffic flow.

```
[1:60] -A OVN-KUBE-NODEPORT -p tcp -m addrtype --dst-type LOCAL -m tcp --dport 31787 -j DNAT --to-destination 169.254.169.3:31787
```

4. The special masquerade route in the host sends this packet into OVN via the management port.

```
169.254.169.3 via 10.244.0.1 dev ovn-k8s-mp0 
```

5. Since by default, all traffic into `ovn-k8s-mp0` gets SNAT-ed, we add an IPtable rule to `OVN-KUBE-SNAT-MGMTPORT` chain to ensure it doesn't get SNAT-ed to preserve its source-ip.

```
[1:60] -A OVN-KUBE-SNAT-MGMTPORT -p tcp -m tcp --dport 31787 -j RETURN
```

6. Traffic enters the node local switch on the worker node and hits the load-balancer where we add a new vip for this masqueradeIP to DNAT it correctly to the local backends. Note that this vip will translate only to the backends that are local to that worker node and hence traffic will be rejected if there is no local endpoint thus respecting ETP=local type traffic rules.

The switch load-balancer on a node with local endpoints will look like this:

```
_uuid               : b3201caf-3089-4462-b96e-1406fd7c4256
external_ids        : {"k8s.ovn.org/kind"=Service, "k8s.ovn.org/owner"="default/example-service-1"}
health_check        : []
ip_port_mappings    : {}
name                : "Service_default/example-service-1_TCP_node_switch_ovn-worker2"
options             : {event="false", reject="true", skip_snat="false"}
protocol            : tcp
selection_fields    : []
vips                : {"169.254.169.3:30820"="10.244.1.4:8080,10.244.1.5:8080", "172.18.0.4:30820"="10.244.1.4:8080,10.244.1.5:8080"}
```

The switch load-balancer on a node without local endpoints will look like this:
```
_uuid               : 42d75e10-5598-4197-a6f2-1a37094bee13
external_ids        : {"k8s.ovn.org/kind"=Service, "k8s.ovn.org/owner"="default/example-service-1"}
health_check        : []
ip_port_mappings    : {}
name                : "Service_default/example-service-1_TCP_node_switch_ovn-worker"
options             : {event="false", reject="true", skip_snat="false"}
protocol            : tcp
selection_fields    : []
vips                : {"169.254.169.3:30820"="", "172.18.0.3:30820"="10.244.1.4:8080,10.244.1.5:8080"}
```

Response traffic will follow the same path (backend->node switch->mp0->host->breth0).

7. Return traffic gets matched on src port being that of the nodePort and is sent to `table=7`

```
 cookie=0xb4e7084fbba8bb8a, duration=190.099s, table=0, n_packets=4, n_bytes=408, idle_age=99, priority=110,tcp,in_port=LOCAL,tp_src=30820 actions=ct(table=7,zone=64003)
```

8. Send the traffic back out breth0 back to the external source in `table=7`

```
 cookie=0xe745ecf105, duration=210.560s, table=7, n_packets=4, n_bytes=408, priority=110 actions=output:eth0
```

The conntrack state looks like this:
```
[NEW] tcp      6 120 SYN_SENT src=172.18.0.1 dst=172.18.0.4 sport=30366 dport=30820 [UNREPLIED] src=172.18.0.4 dst=172.18.0.1 sport=30820 dport=30366 zone=64003
[NEW] tcp      6 120 SYN_SENT src=172.18.0.1 dst=172.18.0.4 sport=30366 dport=30820 [UNREPLIED] src=169.254.169.3 dst=172.18.0.1 sport=30820 dport=30366
[NEW] tcp      6 120 SYN_SENT src=172.18.0.1 dst=169.254.169.3 sport=30366 dport=30820 [UNREPLIED] src=10.244.1.4 dst=172.18.0.1 sport=8080 dport=30366 zone=9
[NEW] tcp      6 120 SYN_SENT src=172.18.0.1 dst=10.244.1.4 sport=30366 dport=8080 [UNREPLIED] src=10.244.1.4 dst=172.18.0.1 sport=8080 dport=30366 zone=14
[UPDATE] tcp   6 60 SYN_RECV src=172.18.0.1 dst=172.18.0.4 sport=30366 dport=30820 src=169.254.169.3 dst=172.18.0.1 sport=30820 dport=30366
[UPDATE] tcp   6 432000 ESTABLISHED src=172.18.0.1 dst=172.18.0.4 sport=30366 dport=30820 src=169.254.169.3 dst=172.18.0.1 sport=30820 dport=30366 [ASSURED]
[UPDATE] tcp   6 120 FIN_WAIT src=172.18.0.1 dst=172.18.0.4 sport=30366 dport=30820 src=169.254.169.3 dst=172.18.0.1 sport=30820 dport=30366 [ASSURED]
[UPDATE] tcp   6 30 LAST_ACK src=172.18.0.1 dst=172.18.0.4 sport=30366 dport=30820 src=169.254.169.3 dst=172.18.0.1 sport=30820 dport=30366 [ASSURED]
[UPDATE] tcp   6 120 TIME_WAIT src=172.18.0.1 dst=172.18.0.4 sport=30366 dport=30820 src=169.254.169.3 dst=172.18.0.1 sport=30820 dport=30366 [ASSURED]
```


### External Source -> Service -> Host Networked pod

This Scenario is a bit different, specifically traffic now needs to be directed from an external source to service and
then to the host itself(a host networked pod)

In this flow, rather than going from breth0 into OVN we shortcircuit the path with physical flows on breth0

```text
          host (ovn-worker, 172.18.0.3) 
           ^
           ^
           |
eth0--->|breth0| ---- 172.18.0.3 OVN GR 100.64.0.4 -- join switch -- ovn_cluster_router -- 10.244.1.3 pod

```

1. Match on the incoming traffic via it's nodePort, DNAT directly to the host networked endpoint, and send to `table=6`

```
cookie=0x790ba3355d0c209b, duration=153.288s, table=0, n_packets=18, n_bytes=1468, idle_age=100, priority=100,tcp,in_port=1,tp_dst=<nodePort> actions=ct(commit,table=6,zone=64003,nat(dst=<nodeIP>:<targetIP>))
```

2. Send out the LOCAL ovs port on breth0, and traffic is delivered to host netwoked pod

```
cookie=0x790ba3355d0c209b, duration=113.033s, table=6, n_packets=18, n_bytes=1468, priority=100 actions=LOCAL
```

3. Return traffic from the host networked pod to external source is matched in `table=7` based on the src_ip of the return
   traffic being equal to `<targetIP>`, and un-Nat back to `<nodeIP>:<NodePort>`

```
 cookie=0x790ba3355d0c209b, duration=501.037s, table=0, n_packets=12, n_bytes=1259, idle_age=448, priority=100,tcp,in_port=LOCAL,tp_src=<targetIP> actions=ct(commit,table=7,zone=64003,nat)
```

4. Send the traffic back out breth0 back to the external source in `table=7`

```
cookie=0x790ba3355d0c209b, duration=501.037s, table=7, n_packets=12, n_bytes=1259, idle_age=448, priority=100 actions=output:1
```

## Host Traffic

This section will cover the networking entities hit when traffic travels from a cluster host via a service to either host
networked pods or cluster networked pods

### Host -> Service -> OVN Pod

This case is similar to steps 3-6 on `External -> Service -> OVN Pod` traffic senario we saw above for local gateway. The traffic will flow from host->PRE-ROUTING iptable rule DNAT towards `169.254.169.3`, which gets routed into `ovn-k8s-mp0` and hits the load balancer on the node-local-switch preserving sourceIP.

### Host -> Service -> Host

Again when the backend is a host networked pod we shortcircuit OVN to avoid SNAT and use iptables rules on the host
to DNAT directly to the correct host endpoint.

```
[0:0] -A OVN-KUBE-NODEPORT -p tcp -m addrtype --dst-type LOCAL -m tcp --dport 30940 -j REDIRECT --to-ports 8080
```

## Intra Cluster traffic

For all service traffic that stays in the overlay the flows will remain the same for `externaltrafficpolicy:local`. If the traffic crosses over to the underlay then its not guaranteed to be the same. See https://bugzilla.redhat.com/show_bug.cgi?id=2027270.

## Sources
- https://www.asykim.com/blog/deep-dive-into-kubernetes-external-traffic-policies
