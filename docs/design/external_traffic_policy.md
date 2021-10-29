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

In this section I will explain some relevant traffic flows when a service's `externalTrafficPolicy` is `local`.  For
these examples we will be using a Nodeport service, but the flow is generally the same for ExternalIP and Loadbalancer
type services.

## Ingress Traffic

This section will cover the networking entities hit when traffic ingresses a cluster via a service to either host
networked pods or cluster networked pods

### External Source -> Service -> OVN pod

This case is the same as normal shared gateway traffic ingress, meaning the externally sourced traffic is routed into
OVN via flows on breth0, except in this case the new local load balancer is hit on the GR, which ensures the ip of the
client is preserved  by the time it gets to the destination Pod.

```text
          host (ovn-worker, 172.18.0.3) 
           |
eth0--->|breth0| -----> 172.18.0.3 OVN GR 100.64.0.4 --> join switch --> ovn_cluster_router --> 10.244.1.3 pod

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

This case is similar to normal host -> Service -> OVN Pod except the SRC address of the Traffic will be that of the
management port `ovn-k8s-mp0` rather than the main address attatched to `eth0` on the host.

### Host -> Service -> Host

Again when the backend is a host networked pod we shortcircuit OVN to avoid SNAT and use iptables rules on the host
to DNAT directly to the correct host endpoint.

```
-A OVN-KUBE-NODEPORT -p tcp -m addrtype --dst-type LOCAL -m tcp --dport 32126 -j DNAT --to-destination <nodeIP>:<targetIP>
```

## Intra Cluster traffic

For all service traffic that stays in the overlay the flows will remain the same for `externaltrafficpolicy:local`.

## Sources
- https://www.asykim.com/blog/deep-dive-into-kubernetes-external-traffic-policies
