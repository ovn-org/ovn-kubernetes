# EgressIP

## Introduction

The Egress IP feature enables a cluster administrator to ensure that the traffic from one or more pods
in one or more namespaces has a consistent source IP address for services outside the cluster network.  
East-West traffic (including pod -> node IP) is excluded from Egress IP.  

For more info, consider looking at the following links:
- [Egress IP CRD](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/crd/egressip/v1/types.go#L47)
- [Assigning an egress IP address](https://docs.okd.io/latest/networking/ovn_kubernetes_network_provider/assigning-egress-ips-ovn.html)
- [Managing Egress IP in OpenShift 4 with OVN-Kubernetes](https://rcarrata.com/openshift/egress-ip-ovn/)

Always check the dependencies on the [Requirements page](../requirements.md)

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

## Egress Nodes

An egress node is a cluster node that a cluster administrator designates for
OVN-Kubernetes to host EgressIP addresses. When an EgressIP is assigned to an
egress node, the node adds the IP to its external interface, responds to
ARP/NDP for it, and SNATs matching pod traffic to that IP before it leaves
the cluster.

Nodes are made eligible for egress assignment by labeling them with
`k8s.ovn.org/egress-assignable`:

```shell
kubectl label nodes <node_name> k8s.ovn.org/egress-assignable=""
```

This label serves as the cluster-wide default pool. Each EgressIP object controls
which nodes from that pool (or from any other labeled set) it may use via the
`egressNodeSelector` field — see below.

### Node Requirements

The `k8s.ovn.org/host-cidrs` annotation is automatically set during node bootstrap and
contains the node's host network CIDR(s). A node can be missing this annotation if it is
stuck in `NodeStatusNeverUpdated`, was orphaned by the cluster autoscaler, or was never
properly bootstrapped.

Such a node is handled in two places:

* **Conflict detection** skips the node. The annotation is the only source of the node's
  host addresses, so without it there is no data to check the requested egress IP against.
  Skipping means the check cannot prove the address is free on that node — it does not mean
  the node has no conflicting address.
* **Node assignability** excludes the node. A node whose `host-cidrs` annotation is missing,
  cannot be parsed, or contains no addresses is marked not assignable even when it carries
  the `k8s.ovn.org/egress-assignable` label, and any EgressIPs already assigned to it are
  released so they can move to an eligible node.

Together these prevent a single misconfigured or orphaned node from failing EgressIP
assignment cluster-wide, while keeping it out of the pool of assignment candidates.

**Important:** Conflict protection is not enforced for a node in this state. Restore the
node's `host-cidrs` annotation, or remove the stale node from the cluster, before relying
on conflict detection to catch an egress IP that collides with a host address.

**Troubleshooting:** If an EgressIP is not assigned to an expected node, verify the node
has the `k8s.ovn.org/host-cidrs` annotation set:

```bash
kubectl get node <node-name> -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/host-cidrs}'
```

### Egress Node Selection with egressNodeSelector

The `egressNodeSelector` field on an EgressIP object controls exactly which nodes
are eligible for that object's IPs. It is an optional field with a CRD-level
default: when omitted, the API server automatically sets it to match nodes with
`k8s.ovn.org/egress-assignable`, preserving legacy label behavior. When set
explicitly, the custom selector replaces that default for this object — the
`egress-assignable` label is not required in addition.

**Use cases**
- **Multi-tenant clusters**: Different tenants or workloads use separate node
  pools. Assign tenant A egress IPs only to nodes in pool A.
- **Compliance**: Route egress traffic from sensitive workloads through specific
  nodes with enhanced monitoring or security controls.
- **Cost optimization**: Restrict high-bandwidth workloads to cost-optimized
  node types.

**Example**

`matchLabels` and `matchExpressions` are ANDed — a node must satisfy all
conditions to be eligible:

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: egressip-finance
spec:
  egressIPs:
    - 192.0.2.10
  namespaceSelector:
    matchLabels:
      team: finance
  podSelector: {}
  egressNodeSelector:
    matchLabels:
      node-pool: finance
    matchExpressions:
      - key: accelerator
        operator: Exists
```

Only nodes with `node-pool: finance` **and** the `accelerator` label present
regardless of its value present eligible.

**Gotchas & Best Practices**

- **Label changes have a race window**: When `egressNodeSelector` or the node
  labels it references are changed, there is a brief window where the cached
  node state may be stale, potentially causing EgressIP reassignment churn.
  Plan node label schemes upfront and perform any changes during a scheduled
  maintenance window.
- **Empty selector `{}` targets every node**: It matches all cluster nodes —
  **including control-plane/master nodes** — expanding both the eligible
  assignment pool and the health-check probe set to the entire cluster. Unless
  that is intentional, avoid it: egress IPs could be placed on control-plane
  nodes, and large clusters pay the cost of probing every node.
- **Selector does not reserve nodes**: Matched nodes are not exclusively
  reserved for this EgressIP object. Other EgressIP objects can still be
  assigned to the same nodes.
- **Control-plane only**: Selector matching is purely a control-plane
  operation. The datapath is identical regardless of which selector is used,
  and works the same across default cluster network, UDN, and dynamic UDN.

## Egress IP reachability

The EgressIP controller in `ovnkube-cluster-manager` periodically checks if egress-candidate nodes are reachable. EgressIPs assigned to a node that is no longer reachable will get revalidated and moved to another usable node.

The probe target set is dynamically maintained as the union of nodes matching any EgressIP object's `egressNodeSelector`. This is a behavior change from the legacy model, where every node carrying the `k8s.ovn.org/egress-assignable` label was probed: now a node is health-checked only while at least one EgressIP selects it, and if no EgressIP selects a node it is not probed at all. The set is re-evaluated immediately when an EgressIP is created or its `egressNodeSelector` changes, so a newly eligible and reachable node can be activated without waiting for the next periodic tick.

Egress nodes normally have multiple IP addresses. For sake of Egress IP reachability, [the management](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/2495) (aka internal SDN) addresses of the node are the ones used. In deployments of ovn-kubernetes this is known to be the `ovn-k8s-mp0` interface of a node.

Even though the periodic checking of egress nodes is hard coded to trigger [every 5 seconds](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/ovn/egressip.go#L2206), there are attributes that the user can set:

- egressIPTotalTimeout
- gRPC vs. DISCARD port

### egressIPTotalTimeout

This attribute specifies the maximum amount of time, in seconds, that the egressIP operator will wait until it declares the node unreachable. The default value is [1 second](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/config/config.go#L866).

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

[Later implementation](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/3100) of ovn-kubernetes is capable of leveraging secure gRPC sessions in order to probe nodes. That requires the `ovnkube node` pods to be listening on a pre-specified TCP port, in addition to configuring the `ovnkube master` pod(s).

This value can be set in the following ways:
- ovnkube binary flag: `--egressip-node-healthcheck-port=<TCP_PORT>`
- inside config specified by `--config-file` flag:
```
[ovnkubernetesfeature]
egressip-node-healthcheck-port=9107
```

**Note:** If not specifying a value, or using `0` as the `egressip-node-healthcheck-port` will make Egress IP reachability probe the egress nodes using the DISCARD port method. Unlike egressip-reachability-total-timeout, it is important that both node and master pods of ovnkube get configured with the same value!

#### Additional details on the implementation of the gRPC probing:

- If configured, the session uses the certificates from the `[egressip-healthcheck-tls]` configuration section. The historical `--nb-client-*` flags and `[ovnnorth]` configuration keys remain accepted for compatibility. An insecure gRPC session is used when no certificates are specified.
- The [message used for probing](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/ovn/healthcheck/health.proto#L6) is the [standard service health](https://github.com/grpc/grpc/blob/master/src/proto/grpc/health/v1/health.proto) specified in gRPC.
- [Special care was taken into consideration](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/ovn/healthcheck/egressip_healthcheck.go#L193-L195) to handle cases when the gRPC session bounced for normal reasons. EgressIP implementation will not declare a node unreachable under these circumstances.

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

North-south next-hop selection then uses the Gateway Router routing table
(typically its default route toward the node default gateway on `br-ex`).
Static routes that exist in the host's **main routing table** for the
**primary node network** (the interface configured as the node's primary
network, usually `br-ex`) are not consulted.

For example, if the node default gateway is `192.168.1.1` on the primary node
network and the host main table also contains `192.168.2.0/24 via 192.168.1.2`,
EgressIP traffic sourced from an address on that primary interface still leaves
via `192.168.1.1`. It does not follow the host static route via `192.168.1.2`.

**Note:** this applies in both shared gateway mode and local gateway mode.
Primary-network EgressIP still follows the shared gateway path through the
Gateway Router rather than the host kernel, so those main-table routes
never apply. This is a known design limitation.

### EgressIP IP is assigned to a secondary host interface
Note that this is unsupported for user defined networks.
Lets now imagine the Egress IP(s) mentioned previously, are not hosted by the OVN primary network and is hosted
by a secondary host network which is assigned to a standard linux interface, in addition to the policy we saw previously to redirect to the egress node,
another policy is added to the egress node itself redirecting the egressIP traffic to the secondary host interface via the 
node management port IP address:
```shell
Routing Policies
  1004 inport == "rtos-ovn-worker" && ip4.dst == 172.18.0.121 /* ovn-worker */                                      reroute  10.244.1.2

  102 (ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741   allow    pkt_mark=1008
  102 ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16                                                          allow
  102 ip4.src == 10.244.0.0/16 && ip4.dst == 100.64.0.0/16                                                          allow
  102                                     pkt.mark == 42                                                            allow

  100 ip4.src == 10.244.0.13                                                                                        reroute  10.244.1.2  pkt_mark=1009
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
Implementation details: https://github.com/ovn-kubernetes/ovn-kubernetes/commit/e2c981a42a28e6213d9daf3b4489c18dc2b84b19.

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
# Output traffic from local pods so they are not affected by the drop rule below, this is required for ICNIv2 and advertised UDNs
priority=104,ip,in_port=2,nw_src=<nodeSubnet> actions=output:eth0

# Drop non SNATed egress traffic coming from non-local pods
priority=103,ip,in_port=2,nw_src=<clusterSubnet> actions=drop

# Commit connections coming from IPs not in cluster network
priority=100,ip,in_port=2 actions=ct(commit,zone=64000,exec(set_field:0x1->ct_mark)),output:1
```

In case of an egress IP assigned to a secondary host interface, a pkt_mark=1009 is added to the reroute policy in the egress node:
```shell
[root@ovn-worker ~]# ovn-nbctl lr-policy-list ovn_cluster_router
Routing Policies
      1004 inport == "rtos-ovn-worker" && ip4.dst == 172.18.0.121 /* ovn-worker */         reroute                10.244.1.2
       102 (ip4.src == $a8519615025667110816 || ip4.src == $a13607449821398607916) && ip4.dst == $a1042611113178530741           allow               pkt_mark=1008
       102 ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16           allow
       102 ip4.src == 10.244.0.0/16 && ip4.dst == 100.64.0.0/16           allow
       102                                     pkt.mark == 42           allow
       100                             ip4.src == 10.244.0.13         reroute                10.244.1.2               pkt_mark=1009
```

And in each node an nftable chain is added in postrouting hook with priority `srcnat+1` 
with a set of rules to drop marked (notice, 1009 == 0x000003f1) traffic from any pod CIDR subnet:
```shell
table inet ovn-kubernetes {
	chain egress-ip-sec-filter {
		comment "Egress IP on secondary interfaces filtering"
		type filter hook postrouting priority srcnat + 1; policy accept;
		ip saddr 10.244.0.0/16 meta mark 0x000003f1 drop comment "Drop egress IP pod traffic from 10.244.0.0/16"
	}
}
```

This guarantees that while differences might occur in reconciliation time, traffic would not
leak the egress secondary interface with the pod IP as source address (i.e., when the reroute LRP is applied before the SNAT).

## Special considerations for Egress IPs hosted by standard linux interfaces
If you wish to assign an Egress IP to a standard linux interface (non OVS type), then the following is required:
* Link is up
* IP address must have scope universe / global
* Links and their addresses must not be removed during runtime after an egress IP is assigned to it. If you wish to remove the link, first
remove the Egress IP and then remove the address / link.
* IP forwarding must be enabled for the link

## Known Limitations

- Layer 2 networks and localnet are not supported.
- **Note:** EgressIP on the **primary node network** (the interface configured as
  the node's network, typically `br-ex`) does not honor static routes that exist
  in the host's **main routing table** for that network. Next-hop selection uses
  the OVN Gateway Router default route. This applies in both shared gateway mode
  and local gateway mode. Local gateway mode does not send this traffic through
  the host kernel, so those main-table routes still do not apply. This is a
  known design limitation.
