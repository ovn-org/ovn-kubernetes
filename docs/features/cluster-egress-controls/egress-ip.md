# EgressIP

## Introduction

The Egress IP feature enables a cluster administrator to ensure that the traffic from one or more pods
in one or more namespaces has a consistent source IP address for services outside the cluster network.  
East-West traffic (including pod -> node IP) is excluded from Egress IP.  

For more info, consider looking at the following links:
- [Egress IP CRD](https://github.com/ovn-org/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/crd/egressip/v1/types.go#L47)
- [Assigning an egress IP address](https://docs.okd.io/latest/networking/ovn_kubernetes_network_provider/assigning-egress-ips-ovn.html)
- [Managing Egress IP in OpenShift 4 with OVN-Kubernetes](https://rcarrata.com/openshift/egress-ip-ovn/)

## Example

An example of EgressIP might look like this:

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: egressip-prod
spec:
  egressIPs:
    - 172.18.0.33
    - 172.18.0.44
  namespaceSelector:
    matchExpressions:
      - key: environment
        operator: NotIn
        values:
          - development
  podSelector:
    matchLabels:
      app: web
```
It specifies to use `172.18.0.33` or `172.18.0.44` egressIP for pods that are labeled with `app: web` that run in a namespace without `environment: development` label.
Both selectors use the [generic kubernetes label selectors](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors).

## Layer 3 network
Supported network configs:
- Cluster default network
- Role primary user defined networks

### EgressIP IP is assigned to the primary host interface
If the Egress IP(s) are hosted on the OVN primary network then the implementation is redirecting the POD traffic
to an egress node where it is SNATed and sent out.  

Using the example EgressIP and a matching pod attached to the cluster default network with `10.244.1.3` IP, the following logical router policies are configured in `ovn_cluster_router`:
```shell
Routing Policies
  1004 inport == "rtos-ovn-control-plane" && ip4.dst == 172.18.0.4 /* ovn-control-plane */                            reroute  10.244.0.2
  1004 inport == "rtos-ovn-worker" && ip4.dst == 172.18.0.2 /* ovn-worker */                                          reroute  10.244.1.2
  1004 inport == "rtos-ovn-worker2" && ip4.dst == 172.18.0.3 /* ovn-worker2 */                                        reroute  10.244.2.2

   102 (ip4.src == $a12749576804119081385 || ip4.src == $a16335301576733828072) && ip4.dst == $a11079093880111560446  allow    pkt_mark=1008
   102 ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16                                                           allow
   102 ip4.src == 10.244.0.0/16 && ip4.dst == 100.64.0.0/16                                                           allow

   100 ip4.src == 10.244.1.3                                                                                          reroute  100.64.0.3, 100.64.0.4
```
- Rules with `1004` priority are responsible for redirecting `pod -> local host IP` traffic.  
- Rules with `102` priority are added by OVN-Kubernetes when EgressIP feature is enabled, they ensure that east-west traffic is not using egress IPs.
- The rule with `100` priority is added for the pod matching `egressip-prod` EgressIP, and it redirects the traffic to one of the egress nodes (ECMP is used to balance the traffic between next hops).

For a pod attached to the cluster default network and once the redirected traffic reaches one of the egress nodes it gets SNATed in the gateway router:
```shell
ovn-nbctl lr-nat-list GR_ovn-worker
TYPE             GATEWAY_PORT          EXTERNAL_IP        EXTERNAL_PORT    LOGICAL_IP          EXTERNAL_MAC         LOGICAL_PORT
...
snat                                   172.18.0.33                         10.244.1.3
ovn-nbctl lr-nat-list  GR_ovn-worker2
TYPE             GATEWAY_PORT          EXTERNAL_IP        EXTERNAL_PORT    LOGICAL_IP          EXTERNAL_MAC         LOGICAL_PORT
...
snat                                   172.18.0.44                         10.244.1.3
```

For a pod attached to a role primary user defined network "network1", there is no NAT entry for the pod attached to the egress OVN gateway and
instead a logical router policy is attached to the egress nodes OVN gateway router:
```shell
sh-5.2# ovn-nbctl lr-policy-list GR_network1_ovn-worker
Routing Policies
        95             ip4.src == 10.128.1.3 && pkt.mark == 0           allow               pkt_mark=50006
```

### EgressIP IP is assigned to a secondary host interface
Note that this is unsupported for user defined networks.
Lets now imagine the Egress IP(s) mentioned previously, are not hosted by the OVN primary network and is hosted
by a secondary host network which is assigned to a standard linux interface, a redirect to the egress-able node management port IP address:
```shell
Routing Policies
  1004 inport == "rtos-ovn-control-plane" && ip4.dst == 172.18.0.4 /* ovn-control-plane */                            reroute  10.244.0.2
  1004 inport == "rtos-ovn-worker" && ip4.dst == 172.18.0.2 /* ovn-worker */                                          reroute  10.244.1.2
  1004 inport == "rtos-ovn-worker2" && ip4.dst == 172.18.0.3 /* ovn-worker2 */                                        reroute  10.244.2.2

   102 (ip4.src == $a12749576804119081385 || ip4.src == $a16335301576733828072) && ip4.dst == $a11079093880111560446  allow    pkt_mark=1008
   102 ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16                                                           allow
   102 ip4.src == 10.244.0.0/16 && ip4.dst == 100.64.0.0/16                                                           allow

   100 ip4.src == 10.244.1.3                                                                                          reroute  10.244.1.2, 10.244.2.2
```

IPTables will have the following chain in NAT table and also rules within that chain to source NAT to the correct IP address:
```shell
sh-5.2# iptables-save 
# Generated by iptables-save v1.8.7 on Tue Jul 25 13:09:39 2023
*mangle
:PREROUTING ACCEPT [14087:9430205]
:INPUT ACCEPT [13923:9397241]
:FORWARD ACCEPT [0:0]
:OUTPUT ACCEPT [11270:1030982]
...
:KUBE-POSTROUTING - [0:0]
:OVN-KUBE-EGRESS-IP-Multi-NIC - [0:0]
:OVN-KUBE-EGRESS-SVC - [0:0]
...
-A POSTROUTING -j OVN-KUBE-EGRESS-IP-MULTI-NIC
-A POSTROUTING -j OVN-KUBE-EGRESS-SVC
-A POSTROUTING -o ovn-k8s-mp0 -j OVN-KUBE-SNAT-MGMTPORT
-A POSTROUTING -m comment --comment "kubernetes postrouting rules" -j KUBE-POSTROUTING
-A KUBE-MARK-DROP -j MARK --set-xmark 0x8000/0x8000
-A KUBE-POSTROUTING -m comment --comment "kubernetes service traffic requiring SNAT" -j MASQUERADE --random-fully
...
-A OVN-KUBE-EGRESS-IP-MULTI-NIC -s 10.244.2.3/32 -o dummy -j SNAT --to-source 10.10.10.100
...
-A OVN-KUBE-EGRESS-SVC -m mark --mark 0x3f0 -m comment --comment DoNotSNAT -j RETURN
-A OVN-KUBE-SNAT-MGMTPORT -o ovn-k8s-mp0 -m comment --comment "OVN SNAT to Management Port" -j SNAT --to-source 10.244.2.2
COMMIT
...
```

IPRoute2 rules will look like the following - note rule with priority `6000` and also the table `1111`:
```shell
sh-5.2# ip rule
0:	from all lookup local
30:	from all fwmark 0x1745ec lookup 7
6000:	from 10.244.2.3 lookup 1111
32766:	from all lookup main
32767:	from all lookup default
```

And the default route in the correct table `1111`:
```shell
sh-5.2# ip route show table 1111
default dev dummy
```
Routes associated with the egress interface are copied from the main routing table to the routing table that was created to support EgressIP, as shown above.
If the interface is enslaved to a VRF device, routes are copied from the VRF interfaces associated routing table.

No NAT is required on the OVN primary network gateway router.
OVN-Kubernetes (ovnkube-node) takes care of adding a rule to the rule table with src IP of the pod and routed towards a
new routing table specifically created to route the traffic out the correct interface. IPTables rules are also altered and an entry
is created within the chain `OVN-KUBE-EGRESS-IP-Multi-NIC` for each selected pod to allow SNAT to occur when
egress-ing a particular interface. The routing table number `1111` is generated from the interface name.
Routes within the main routing table who's output interface share the same interface used for Egress IP are also cloned into the VRF 1111.

## Layer 2 network
Not supported

## Localnet
Not supported

### Pod to node IP traffic
When a cluster networked pod matched by an egress IP tries to connect to a non-local node IP it hits the following
logical router policy in `ovn_cluster_router`:
```shell
# $<all_eip_pod_ips> - address-set of all pod IPs matched by any EgressIP
# $<all_esvc_pod_ips> - address-set of all pod IPs matched by any EgressService
# $<all_node_ips> - address-set of all node IPs in the cluster
102 (ip4.src == $<all_eip_pod_ips> || ip4.src == $<all_esvc_pod_ips>) && ip4.dst == $<all_node_ips>  allow    pkt_mark=1008
```
In addition to simply allowing the `pod -> node IP` traffic so that EgressIP reroute policies 
are not matched upon, it is also marked with the 1008 mark.  
If a pod is hosted on an egressNode the traffic will first get SNATed to the egress IP, and then it will hit 
following flow on breth0 that will SNAT the traffic to local node IP:
```shell
# output truncated, 0x3f0 == 1008
priority=105,pkt_mark=0x3f0,ip,in_port=2 actions=ct(commit,zone=64000,nat(src=<NodeIP>),exec(load:0x1->NXM_NX_CT_MARK[])),output:1
``` 
This is required to make `pod -> node IP` traffic behave the same regardless of where the pod is hosted.  
Implementation details: https://github.com/ovn-org/ovn-kubernetes/commit/e2c981a42a28e6213d9daf3b4489c18dc2b84b19.

For local gateway mode, in which an Egress IP is assigned to a non-primary interface, an IP rule is added to send packets
to the main routing table at a priority higher than that of EgressIP IP rules, which are set to priority `6000`:
```shell
5999:	from all fwmark 0x3f0 lookup main
```
Note: `0x3f0` is `1008` in hexadecimal. Lower IP rule priority number indicates higher precedence versus higher IP rule priority number.

This ensures all traffic to node IPs will not be selected by EgressIP IP rules.
However, reply traffic will not have the mark `1008` and would be dropped by reverse path filtering, therefore we add
an IPTable rule to the mangle table to save and restore the `1008` mark:
```shell
sh-5.2# iptables -t mangle -L  PREROUTING
Chain PREROUTING (policy ACCEPT)
target     prot opt source               destination
CONNMARK   all  --  anywhere             anywhere             mark match 0x3f0 CONNMARK save
CONNMARK   all  --  anywhere             anywhere             mark match 0x0 CONNMARK restore
```

### Dealing with non SNATed traffic
Egress IP is often configured on a node different from the one hosting the affected pods.  
Due to the fact that ovn-controllers on different nodes apply the changes independently,
there is a chance that some pod traffic will reach the egress node before it configures the SNAT rules.
The following flows are added on breth0 to address this scenario:
```shell
# Commit connections from local pods so they are not affected by the drop rule below, this is required for ICNIv2
priority=109,ip,in_port=2,nw_src=<nodeSubnet> actions=ct(commit,zone=64000,exec(set_field:0x1->ct_mark)),output:1

# Drop non SNATed egress traffic coming from non-local pods
priority=104,ip,in_port=2,nw_src=<clusterSubnet> actions=drop

# Commit connections coming from IPs not in cluster network
priority=100,ip,in_port=2 actions=ct(commit,zone=64000,exec(set_field:0x1->ct_mark)),output:1
```

## Special considerations for Egress IPs hosted by standard linux interfaces
If you wish to assign an Egress IP to a standard linux interface (non OVS type), then the following is required:
* Link is up
* IP address must have scope universe / global
* Links and their addresses must not be removed during runtime after an egress IP is assigned to it. If you wish to remove the link, first
remove the Egress IP and then remove the address / link.
* IP forwarding must be enabled for the link

## Egress Nodes

In order to select which node(s) may be used as egress, the following label must be added to the `node` resource:

```shell
kubectl label nodes <node_name> k8s.ovn.org/egress-assignable=""
```

## Egress IP reachability

Once a node has been labeled with `k8s.ovn.org/egress-assignable`, the EgressIP operator in the leader ovnkube-master pod will periodically check if that node is
usable. EgressIPs assigned to a node that is no longer reachable will get revalidated and moved to another useable node.

Egress nodes normally have multiple IP addresses. For sake of Egress IP reachability, [the management](https://github.com/ovn-org/ovn-kubernetes/pull/2495) (aka internal SDN) addresses of the node are the ones used. In deployments of ovn-kubernetes this is known to be the `ovn-k8s-mp0` interface of a node.

Even though the periodic checking of egress nodes is hard coded to trigger [every 5 seconds](https://github.com/ovn-org/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/ovn/egressip.go#L2206), there are attributes that the user can set:

- egressIPTotalTimeout
- gRPC vs. DISCARD port

### egressIPTotalTimeout

This attribute specifies the maximum amount of time, in seconds, that the egressIP operator will wait until it declares the node unreachable. The default value is [1 second](https://github.com/ovn-org/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/config/config.go#L866).

This value can be set in the following ways:
- ovnkube binary flag: `--egressip-reachability-total-timeout=<TIMEOUT>`
- inside config specified by `--config-file` flag:
```
[ovnkubernetesfeature]
egressip-reachability-total-timeout=123
```

**Note:** Using value `0` will skip reachability. Use this to assume that egress nodes are available.

### gRPC vs. DISCARD port

Up until recently, the only method available for determining if an egress node was reachable relied on the `TCP port unreachable` icmp response from the probed node. The TCP port 9 (aka DISCARD) is the port used for that.

[Later implementation](https://github.com/ovn-org/ovn-kubernetes/pull/3100) of ovn-kubernetes is capable of leveraging secure gRPC sessions in order to probe nodes. That requires the `ovnkube node` pods to be listening on a pre-specified TCP port, in addition to configuring the `ovnkube master` pod(s).

This value can be set in the following ways:
- ovnkube binary flag: `--egressip-node-healthcheck-port=<TCP_PORT>`
- inside config specified by `--config-file` flag:
```
[ovnkubernetesfeature]
egressip-node-healthcheck-port=9107
```

**Note:** If not specifying a value, or using `0` as the `egressip-node-healthcheck-port` will make Egress IP reachability probe the egress nodes using the DISCARD port method. Unlike egressip-reachability-total-timeout, it is important that both node and master pods of ovnkube get configured with the same value!

#### Additional details on the implementation of the gRPC probing:

- If available, the session uses the [same TLS certs](https://github.com/ovn-org/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/ovn/healthcheck/egressip_healthcheck.go#L78) used by ovnkube to connect to the northbound OVSDB server. Conversely, an insecure gRPC session is used when no certs are specified.
- The [message used for probing](https://github.com/ovn-org/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/ovn/healthcheck/health.proto#L6) is the [standard service health](https://github.com/grpc/grpc/blob/master/src/proto/grpc/health/v1/health.proto) specified in gRPC.
- [Special care was taken into consideration](https://github.com/ovn-org/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/ovn/healthcheck/egressip_healthcheck.go#L193-L195) to handle cases when the gRPC session bounced for normal reasons. EgressIP implementation will not declare a node unreachable under these circumstances.

