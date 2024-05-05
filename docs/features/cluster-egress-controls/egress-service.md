# Egress Service

## Introduction

The Egress Service feature enables the egress traffic of pods backing a LoadBalancer service to use a different network than the main one and/or their source IP to be the Service's ingress IP.
This is useful for external systems that communicate with applications running on the Kubernetes cluster through a LoadBalancer service and expect that the source IP of egress traffic originating from the pods backing the service is identical to the destination IP they use to reach them - i.e the LoadBalancer's ingress IP.
In addition, this allows to separate the egress traffic of specified LoadBalancer services by different networks (VRFs).

By introducing a new CRD `EgressService`, users could request that egress packets originating from all of the pods that are endpoints of a LoadBalancer service would use a different network than the main one and/or their source IP will be the Service's ingress IP.
The CRD is namespace scoped. The name of the EgressService corresponds to the name of a LoadBalancer Service that should be affected by this functionality. Note the mapping of EgressService to Kubernetes Service is 1to1.
The feature is supported fully by "Local" gateway mode and almost entirely by "Shared" gateway mode (it does not support [Network without LoadBalancer SNAT](#network-without-loadbalancer-snat). In any case the affected traffic will be that which is coming from a pod to a destination outside of the cluster - meaning pod-pod / pod-service / pod-node traffic will not be affected.

Announcing the service externally (for ingress traffic) is handled by a LoadBalancer provider (like MetalLB) and not by OVN-Kubernetes as explained later.

## Details - Modifying the source IP of the egress packets

Only SNATing a pod's IP to the LoadBalancer service ingress IP that it is backing is problematic, as usually the ingress IP is exposed via multiple nodes by the LoadBalancer provider. This means we can't just add an SNAT to the regular traffic flow of a pod before it exits its node because we don't have a guarantee that the reply will come back to the pod's node (where the traffic originated).
An external client usually has multiple paths to reach the LoadBalancer ingress IP and could reply to a node that is not the pod's node - in that case the other node does not have the proper CONNTRACK entries to send the reply back to the pod and the traffic is lost.
For that reason, we need to make sure that all traffic for the service's pods (ingress/egress) is handled by a single node so the right CONNTRACK entries are always matched and the traffic is not lost.

The egress part is handled by OVN-Kubernetes, which chooses a node that acts as the point of ingress/egress, and steers the relevant pods' egress traffic to its mgmt port, by using logical router policies on the `ovn_cluster_router`.
When that traffic reaches the node's mgmt port it will use its routing table and iptables before heading out.
Because of that, it takes care of adding the necessary iptables rules on the selected node to SNAT traffic exiting from these pods to the service's ingress IP.

These goals are achieved by introducing a new resource `EgressService` for users to create alongside LoadBalancer services with the following fields:
- `sourceIPBy`: Determines the source IP of egress traffic originating from the pods backing the Service.
When "LoadBalancerIP" the source IP is set to the Service's LoadBalancer ingress IP.
When "Network" the source IP is set according to the interface of the Network, leveraging the masquerade rules that are already in place. Typically these rules specify SNAT to the IP of the outgoing interface, which means the packet will typically leave with the IP of the node.

`nodeSelector`: Allows limiting the nodes that can be selected to handle the service's traffic when sourceIPBy: "LoadBalancerIP".
When present only a node whose labels match the specified selectors can be selected for handling the service's traffic as explained earlier.
When the field is not specified any node in the cluster can be chosen to manage the service's traffic.
In addition, if the service's `ExternalTrafficPolicy` is set to `Local` an additional constraint is added that only a node that has an endpoint can be selected - this is important as otherwise new ingress traffic will not work properly if there are no local endpoints on the host to forward to. This also means that when "ETP=Local" only endpoints local to the selected host will be used for ingress traffic and other endpoints will not be used.

- `network`: The network which this service should send egress and corresponding ingress replies to.
This is typically implemented as VRF mapping, representing a numeric id or string name of a routing table which by omission uses the default host routing.

When a node is selected to handle the service's traffic both the status of the relevant `EgressService` is updated with `host: <node_name>` (which is consumed by `ovnkube-node`) and the node is labeled with `egress-service.k8s.ovn.org/<svc-namespace>-<svc-name>: ""`, which can be consumed by a LoadBalancer provider to handle the ingress part.

Similarly to the EgressIP feature, once a node is selected it is checked for readiness (TCP/gRPC) to serve traffic every x seconds.
If a node fails the health check, its allocated services move to another node by removing the `egress-service.k8s.ovn.org/<svc-namespace>-<svc-name>: ""` label from it, removing the logical router policies from the cluster router, resetting the status of the relevant `EgressServices` and requeuing them - causing a new node to be selected for the services.
If the node becomes not ready or its labels no longer match the service's selectors the same re-election process happens.

The ingress part is handled by a LoadBalancer provider, such as MetalLB, that needs to select the right node (and only it) for announcing the LoadBalancer service (ingress traffic) according to the `egress-service.k8s.ovn.org/<svc-namespace>-<svc-name>: ""` label set by OVN-Kubernetes.
A full example with MetalLB is detailed in [Usage Example](#usage-example).

Just to be clear, OVN-Kubernetes does not care which component advertises the LoadBalancer service or checks if it does it correctly - it is the user's responsibility to make sure ingress traffic arrives only to the node with the `egress-service.k8s.ovn.org/<svc-namespace>-<svc-name>: ""` label.

Assuming an Egress Service has `172.19.0.100` as its ingress IP and `ovn-worker` selected to handle all of its traffic, the egress traffic flow of an endpoint pod with the ip `10.244.1.6` on `ovn-worker2` towards an external destination (172.19.0.5) will look like:
```none
                     ┌────────────────────┐
                     │                    │
                     │external destination│
                     │    172.19.0.5      │
                     │                    │
                     └───▲────────────────┘
                         │
     5. packet reaches   │                      2. router policy rereoutes it
        the external     │                         to ovn-worker's mgmt port
        destination with │                      ┌──────────────────┐
        src ip:          │                  ┌───┤ovn cluster router│
        172.19.0.100     │                  │   └───────────▲──────┘
                         │                  │               │
                         │                  │               │1. packet to 172.19.0.5
                      ┌──┴───┐        ┌─────▼┐              │   heads to the cluster router
                   ┌──┘ eth1 └──┐  ┌──┘ mgmt └──┐           │   as usual
                   │ 172.19.0.2 │  │ 10.244.0.2 │           │
                   ├─────▲──────┴──┴─────┬──────┤           │   ┌────────────────┐
4. an iptables rule│     │   ovn-worker  │3.    │           │   │  ovn-worker2   │
   that SNATs to   │     │               │      │           │   │                │
   the service's ip│     │               │      │           │   │                │
   is hit          │     │  ┌────────┐   │      │           │   │ ┌────────────┐ │
                   │     │4.│routes +│   │      │           └───┼─┤    pod     │ │
                   │     └──┤iptables◄───┘      │               │ │ 10.244.1.6 │ │
                   │        └────────┘          │               │ └────────────┘ │
                   │                            │               │                │
                   └────────────────────────────┘               └────────────────┘
                3. from the mgmt port it hits ovn-worker's
                   routes and iptables rules
```
Notice how the packet exits `ovn-worker`'s eth1 and not breth0, as the packet goes through the host's routing table regardless of the gateway mode.

When `sourceIPBy: "Network"` is set, `ovnkube-master` does not need to create any logical router policies because the egress packets of each pod would exit through the pod's node but will set the status field of the resource with `host: ALL` as decribed later.

### Network
The `EgressService` supports a `network` field to specify to which network the egress traffic of the service should be steered to.
When it is specified the relevant `ovnkube-nodes` take care of creating ip rules on their host - either the node which matches `Status.Host` or all of the nodes when `Status.Host` is "ALL".
Assuming an `EgressService` has `Network: blue`, a ClusterIP of 10.96.135.5 and its endpoints are 10.244.0.3 and 10.244.1.6 the following will be created on the host:

```none
$ ip rule list
5000:	from 10.96.135.5 lookup blue
5000:	from 10.244.0.3 lookup blue
5000:	from 10.244.1.6 lookup blue
```

This makes the egress traffic of endpoints of an EgressService to be routed via the "blue" routing table.
An ip rule is also created for the ClusterIP of the service which is needed in order for the ingress reply traffic (reply to an external client calling the service) to use the correct table - this is because the packet flow of contacting a LoadBalancer service goes:
`lb ip -> node -> enter ovn with ClusterIP -> exit ovn with ClusterIP -> exit node with lb ip`
so we need to make sure that packets from ClusterIPs are marked before being routed in order for them to hit the relevant ip rule in time.

### Network without LoadBalancer SNAT
As mentioned earlier, it is possible to use the "Network" capability without SNATing the traffic to the service's ingress IP. This is done by creating an EgressService with the `Network` field specified and `sourceIPBy: "Network"`.

An EgressService with `sourceIPBy: "Network"` does not need to have a host selected, as the traffic will exit each node with the IP of the interface corresponding to the "Network" by leveraging the masquerade rules that are already in place.

This works only on clusters running on "Local" gateway mode, because on "Shared" gateway mode the ip rules created by the controller are ignored (like all the node's routing stack).

When `sourceIPBy: "Network"`, `ovnkube-master` does not need to create any logical router policies as the egress packets of each pod would exit through the pod's node.
However, `ovnkube-master` will set the status field of the resource with `host: ALL` to designate that no reroute logical router policies exist for the service, "instructing" all of the `ovnkube-nodes` to handle the resource's `Network` field without creating SNAT iptables rules.

When `ovnkube-node` detects that the host of an EgressService is `ALL`, only the endpoints local to the node will have an ip rule created, and no SNAT iptables rules will be created.

It is the user's responsibility to make sure that the pods backing an EgressService without SNAT run only on nodes that have the required "Network", as no additional steering (lrps) will take place by OVN and pods running on nodes without a correct "Network" will misbehave.


## Changes in OVN northbound database and iptables

The feature is implemented by reacting to events from `EgressServices`, `Services`, `EndpointSlices` and `Nodes` changes -
updating OVN's northbound database `Logical_Router_Policy` objects to steer the traffic to the selected node and creating iptables SNAT rules in its `OVN-KUBE-EGRESS-SVC` chain, which is called by the POSTROUTING chain of its nat table.

We'll see how the related objects are changed once a LoadBalancer is requested to act as an "Egress Service" by creating a corresponding `EgressService` named after it in a Dual-Stack kind cluster.

We start with a clean cluster:
```
$ kubectl get nodes
NAME                STATUS   ROLES
ovn-control-plane   Ready    control-plane
ovn-worker          Ready    worker
ovn-worker2         Ready    worker
```

```
$ kubectl describe svc demo-svc
Name:                     demo-svc
Namespace:                default
Type:                     LoadBalancer
LoadBalancer Ingress:     5.5.5.5, 5555:5555:5555:5555:5555:5555:5555:5555
Endpoints:                10.244.0.5:8080,10.244.2.7:8080
                          fd00:10:244:1::5,fd00:10:244:3::7
```

```
$ ovn-nbctl lr-policy-list ovn_cluster_router
Routing Policies
      1004 inport == "rtos-ovn-control-plane" && ip4.dst == 172.18.0.3 /* ovn-control-plane */         reroute                10.244.2.2
      1004 inport == "rtos-ovn-control-plane" && ip6.dst == fc00:f853:ccd:e793::3 /* ovn-control-plane */         reroute          fd00:10:244:3::2
      1004 inport == "rtos-ovn-worker" && ip4.dst == 172.18.0.4 /* ovn-worker */         reroute                10.244.0.2
      1004 inport == "rtos-ovn-worker" && ip6.dst == fc00:f853:ccd:e793::4 /* ovn-worker */         reroute          fd00:10:244:1::2
      1004 inport == "rtos-ovn-worker2" && ip4.dst == 172.18.0.2 /* ovn-worker2 */         reroute                10.244.1.2
      1004 inport == "rtos-ovn-worker2" && ip6.dst == fc00:f853:ccd:e793::2 /* ovn-worker2 */         reroute          fd00:10:244:2::2
       102 ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16           allow
       102 ip4.src == 10.244.0.0/16 && ip4.dst == 100.64.0.0/16           allow
       102 ip4.src == 10.244.0.0/16 && ip4.dst == 172.18.0.2/32           allow
       102 ip4.src == 10.244.0.0/16 && ip4.dst == 172.18.0.3/32           allow
       102 ip4.src == 10.244.0.0/16 && ip4.dst == 172.18.0.4/32           allow
       102 ip6.src == fd00:10:244::/48 && ip6.dst == fc00:f853:ccd:e793::2/128           allow
       102 ip6.src == fd00:10:244::/48 && ip6.dst == fc00:f853:ccd:e793::3/128           allow
       102 ip6.src == fd00:10:244::/48 && ip6.dst == fc00:f853:ccd:e793::4/128           allow
       102 ip6.src == fd00:10:244::/48 && ip6.dst == fd00:10:244::/48           allow
       102 ip6.src == fd00:10:244::/48 && ip6.dst == fd98::/64           allow

```

At this point nothing related to Egress Services is in place. It is worth noting that the "allow" policies (102's) that make sure east-west traffic is not affected for EgressIPs are present here as well - if the EgressIP feature is enabled it takes care of creating them, otherwise the "Egress Service" feature does (sharing the same logic), as we do not want Egress Services to change the behavior of east-west traffic.
Also, the policies created (seen later) for an Egress Service use a higher priority than the EgressIP ones, which means that if a pod belongs to both an EgressIP and an Egress Service the service's ingress IP will be used for the SNAT.

We now request that our service "demo-svc" will act as an "Egress Service" by creating a corresponding `EgressService`, with the constraint that only a node with the `"node-role.kubernetes.io/worker": ""` label can be selected to handle its traffic:
```
$ cat egress-service.yaml
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: demo-svc
  namespace: default
spec:
  sourceIPBy: "LoadBalancerIP"
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/worker: ""

$ kubectl apply -f egress-service.yaml
egressservice.k8s.ovn.org/demo-svc created
```

Once the `EgressService` is created a node is selected to handle all of its traffic (ingress/egress) as described earlier.
The `EgressService` status is updated with its name, logical router policies are created on ovn_cluster_router to steer the endpoints' traffic to its mgmt port, SNAT rules are created in its iptables and it is labeled as the node in charge of the service's traffic:

The status points to `ovn-worker2`, meaning it was selected to handle the service's traffic:
```
$ kubectl describe egressservice demo-svc
Name:         demo-svc
Namespace:    default
Spec:
    Source IP By:                        LoadBalancerIP
    Node Selector:
      Match Labels:
        node-role.kubernetes.io/worker:  ""
Status:
  Host:  ovn-worker2
```

A logical router policy is created for each endpoint to steer its egress traffic towards `ovn-worker2`'s mgmt port:
```
$ ovn-nbctl lr-policy-list ovn_cluster_router
Routing Policies
       <truncated 1004's and 102's>
       101                              ip4.src == 10.244.0.5         reroute                10.244.1.2
       101                              ip4.src == 10.244.2.7         reroute                10.244.1.2
       101                        ip6.src == fd00:10:244:1::5         reroute          fd00:10:244:2::2
       101                        ip6.src == fd00:10:244:3::7         reroute          fd00:10:244:2::2
```

An SNAT rule to the service's ingress IP is created for each endpoint:
```
$ hostname
ovn-worker2

$ iptables-save
*nat
...
-A POSTROUTING -j OVN-KUBE-EGRESS-SVC
-A OVN-KUBE-EGRESS-SVC -s 10.244.0.5/32 -m comment --comment "default/demo-svc" -j SNAT --to-source 5.5.5.5
-A OVN-KUBE-EGRESS-SVC -s 10.244.2.7/32 -m comment --comment "default/demo-svc" -j SNAT --to-source 5.5.5.5
...

$ ip6tables-save
...
*nat
-A POSTROUTING -j OVN-KUBE-EGRESS-SVC
-A OVN-KUBE-EGRESS-SVC -s fd00:10:244:3::7/128 -m comment --comment "default/demo-svc" -j SNAT --to-source 5555:5555:5555:5555:5555:5555:5555:5555
-A OVN-KUBE-EGRESS-SVC -s fd00:10:244:1::5/128 -m comment --comment "default/demo-svc" -j SNAT --to-source 5555:5555:5555:5555:5555:5555:5555:5555
...
```

`ovn-worker2` is the only node holding the `egress-service.k8s.ovn.org/default-demo-svc=""` label:
```
$ kubectl get nodes -l egress-service.k8s.ovn.org/default-demo-svc=""
NAME          STATUS   ROLES
ovn-worker2   Ready    worker
```

When the endpoints of the service change, the logical router policies and iptables rules are changed accordingly.

We will now simulate a failover of the service when a node fails its health check.
By stopping `ovn-worker2`'s container we see that all of the resources "jump" to `ovn-worker`, as it is the only node left matching the `nodeSelector`:

```
$ docker stop ovn-worker2
ovn-worker2
```

The status now points to `ovn-worker`:
```
$ kubectl describe egressservice demo-svc
Name:         demo-svc
Namespace:    default
Spec:
    Source IP By:                        LoadBalancerIP
    Node Selector:
      Match Labels:
        node-role.kubernetes.io/worker:  ""
Status:
  Host:  ovn-worker
```

The reroute destination changed to `ovn-worker`'s mgmt port (10.244.1.2 -> 10.244.0.2, fd00:10:244:2::2 -> fd00:10:244:1::2):
```
$ ovn-nbctl lr-policy-list ovn_cluster_router
Routing Policies
       <truncated 1004's and 102's>
       101                              ip4.src == 10.244.0.5         reroute                10.244.0.2
       101                              ip4.src == 10.244.2.7         reroute                10.244.0.2
       101                        ip6.src == fd00:10:244:1::5         reroute          fd00:10:244:1::2
       101                        ip6.src == fd00:10:244:3::7         reroute          fd00:10:244:1::2
```

The iptables rules were created on `ovn-worker`:
```
$ hostname
ovn-worker

$ iptables-save
*nat
...
-A POSTROUTING -j OVN-KUBE-EGRESS-SVC
-A OVN-KUBE-EGRESS-SVC -s 10.244.0.5/32 -m comment --comment "default/demo-svc" -j SNAT --to-source 5.5.5.5
-A OVN-KUBE-EGRESS-SVC -s 10.244.2.7/32 -m comment --comment "default/demo-svc" -j SNAT --to-source 5.5.5.5
...

$ ip6tables-save
...
*nat
-A POSTROUTING -j OVN-KUBE-EGRESS-SVC
-A OVN-KUBE-EGRESS-SVC -s fd00:10:244:3::7/128 -m comment --comment "default/demo-svc" -j SNAT --to-source 5555:5555:5555:5555:5555:5555:5555:5555
-A OVN-KUBE-EGRESS-SVC -s fd00:10:244:1::5/128 -m comment --comment "default/demo-svc" -j SNAT --to-source 5555:5555:5555:5555:5555:5555:5555:5555
...
```

The label moved to `ovn-worker`:
```
$ kubectl get nodes -l egress-service.k8s.ovn.org/default-demo-svc=""
NAME         STATUS   ROLES
ovn-worker   Ready    worker
```

Finally, deleting the `EgressService` resource resets the cluster to the point we started from:
```
$ kubectl delete egressservice demo-svc
egressservice.k8s.ovn.org "demo-svc" deleted
```

```
$ ovn-nbctl lr-policy-list ovn_cluster_router
Routing Policies
      1004 inport == "rtos-ovn-control-plane" && ip4.dst == 172.18.0.3 /* ovn-control-plane */         reroute                10.244.2.2
      1004 inport == "rtos-ovn-control-plane" && ip6.dst == fc00:f853:ccd:e793::3 /* ovn-control-plane */         reroute          fd00:10:244:3::2
      1004 inport == "rtos-ovn-worker" && ip4.dst == 172.18.0.4 /* ovn-worker */         reroute                10.244.0.2
      1004 inport == "rtos-ovn-worker" && ip6.dst == fc00:f853:ccd:e793::4 /* ovn-worker */         reroute          fd00:10:244:1::2
      1004 inport == "rtos-ovn-worker2" && ip4.dst == 172.18.0.2 /* ovn-worker2 */         reroute                10.244.1.2
      1004 inport == "rtos-ovn-worker2" && ip6.dst == fc00:f853:ccd:e793::2 /* ovn-worker2 */         reroute          fd00:10:244:2::2
       102 ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16           allow
       102 ip4.src == 10.244.0.0/16 && ip4.dst == 100.64.0.0/16           allow
       102 ip4.src == 10.244.0.0/16 && ip4.dst == 172.18.0.2/32           allow
       102 ip4.src == 10.244.0.0/16 && ip4.dst == 172.18.0.3/32           allow
       102 ip4.src == 10.244.0.0/16 && ip4.dst == 172.18.0.4/32           allow
       102 ip6.src == fd00:10:244::/48 && ip6.dst == fc00:f853:ccd:e793::2/128           allow
       102 ip6.src == fd00:10:244::/48 && ip6.dst == fc00:f853:ccd:e793::3/128           allow
       102 ip6.src == fd00:10:244::/48 && ip6.dst == fc00:f853:ccd:e793::4/128           allow
       102 ip6.src == fd00:10:244::/48 && ip6.dst == fd00:10:244::/48           allow
       102 ip6.src == fd00:10:244::/48 && ip6.dst == fd98::/64           allow

```

```
$ hostname
ovn-worker

$ iptables-save | grep EGRESS
:OVN-KUBE-EGRESS-SVC - [0:0]
-A POSTROUTING -j OVN-KUBE-EGRESS-SVC

$ ip6tables-save | grep EGRESS
:OVN-KUBE-EGRESS-SVC - [0:0]
-A POSTROUTING -j OVN-KUBE-EGRESS-SVC
```

```
$ kubectl get nodes -l egress-service.k8s.ovn.org/default-demo-svc=""
No resources found
```

### TBD: Dealing with non SNATed traffic
The host of an Egress Service is often in charge of pods (endpoints) that run in different nodes.  
Due to the fact that ovn-controllers on different nodes apply the changes independently, there is
a chance that some pod traffic will reach the host before it configures the relevant SNAT iptables rules.
In that timeframe, the egress traffic from these pods will exit the host with their ip instead of the LB's ingress ip, and the it will not be able to return properly because an external client is not aware of a pod's inner ip.

This is currently a known issue for EgressService because we can't leverage the same as [EgressIP](egress-ip.md#dealing-with-non-snated-traffic) currently does by setting a flow on breth0 - the flow won't be hit because the traffic "exits" OVN when using EgressService (= doesn't hit the host's breth0) as opposed to how EgressIP "keeps everything" inside OVN.

## Usage Example

While the user does not need to know all of the details of how "Egress Services" work, they need to know that in order for a service to work properly the access to it from outside the cluster (ingress traffic) has to go only through the node labeled with the `egress-service.k8s.ovn.org/<svc-namespace>-<svc-name>: ""` label - i.e the node designated by OVN-Kubernetes to handle all of the service's traffic.
As mentioned earlier, OVN-Kubernetes does not care which component advertises the LoadBalancer service or checks if it does it correctly.

Here we look at an example of "Egress Services" using MetalLB to advertise the LoadBalancer service externally.
A user of MetalLB can follow these steps to create a LoadBalancer service whose endpoints exit the cluster with its ingress IP.
We already assume MetalLB's `BGPPeers` are configured and the sessions are established.

1. Create the IPAddressPool with the desired IP for the service. It makes sense to set `autoAssign: false` so it is not taken by another service by mistake - our service will request that pool explicitly. 
```yaml
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: example-pool
  namespace: metallb-system
spec:
  addresses:
  - 172.19.0.100/32
  autoAssign: false
```

2. Create the LoadBalancer service and the corresponding EgressService. We create the service with the `metallb.universe.tf/address-pool` annotation to explicitly request its IP to be from the `example-pool` and the EgressService with a `nodeSelector` so that the traffic exits from a node that matches these selectors.
```yaml
apiVersion: v1
kind: Service
metadata:
  name: example-service
  namespace: some-namespace
  annotations:
    metallb.universe.tf/address-pool: example-pool
spec:
  selector:
    app: example
  ports:
    - name: http
      protocol: TCP
      port: 8080
      targetPort: 8080
  type: LoadBalancer
---
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: example-service
  namespace: some-namespace
spec:
  sourceIPBy: "LoadBalancerIP"
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/worker: ""
```

3. Advertise the service from the node in charge of the service's traffic. So far the service is "broken" - it is not reachable from outside the cluster and if the pods try to send traffic outside it would probably not come back as it is SNATed to an IP which is unknown.
We create the advertisements targeting only the node that is in charge of the service's traffic using the `nodeSelector` field, relying on ovn-k to label the node properly.
```yaml
apiVersion: metallb.io/v1beta1
kind: BGPAdvertisement
metadata:
  name: example-bgp-adv
  namespace: metallb-system
spec:
  ipAddressPools:
  - example-pool
  nodeSelector:
  - matchLabels:
      egress-service.k8s.ovn.org/some-namespace-example-service: ""
```
While possible to create more advertisements resources for the `example-pool`, it is the user's responsibility to make sure that the pool is advertised only by advertisements targeting the node holding the `egress-service.k8s.ovn.org/<svc-namespace>-<svc-name>: ""` label - otherwise the traffic of the service will be broken.
