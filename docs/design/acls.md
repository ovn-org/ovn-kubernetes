# ACLs

## Introduction

ovn-k uses ACLs to implement multiple features, this doc is intended to list all of them, explaining their IDs, 
priorities, and dependencies.

OVN has 3 independent sets of ACLs, based on direction and pipeline stage, which are applied in the following order: 
1. `direction="from-lport"` 
2. `direction="from-lport", options={"apply-after-lb": "true"}`
3. `direction="to-lport"`

The priorities between these stages are independent!

OVN will apply the `from-lport` ACLs in two stages. ACLs without `apply-after-lb` set, will be applied before the 
load balancer stage, and ACLs with this option set will be applied after the load balancer stage. `to-lport` are always 
applied after the load balancer stage.

For now, ovn-k doesn't use `direction="from-lport"` ACLs, since most of the time we need to apply ACLs after loadbalancing.
Here is the current order in which ACLs for different objects are applied (check the rest of the doc for details)

### `direction="from-lport", options={"apply-after-lb": "true"}`

1. egress multicast, allow priority = `1012`,  deny priority = `1011`
2. egress network policy, default deny priority = `1000`, allow priority = `1001`

### `direction="to-lport"`

1. egress firewall, priorities = `2000`-`10000` (egress firewall is applied on this stage to be independently applied
after egress network policy)
2. ingress multicast, allow priority = `1012`,  deny priority = `1011`
3. ingress network policy, default deny priority = `1000`, allow priority = `1001`

## Egress Firewall

Egress Firewall creates 1 ACL for every specified rule, with `ExternalIDs["k8s.ovn.org/owner-type"]=EgressFirewall`
e.g. given object:

```yaml
kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: default
spec:
  egress:
    - type: Allow
      to:
        dnsName: www.openvswitch.org
    - type: Allow
      to:
        cidrSelector: 1.2.3.0/24
      ports:
        - protocol: UDP
          port: 55
    - type: Deny
      to:
        cidrSelector: 0.0.0.0/0
```

will create 3 ACLs:

```
action              : allow
direction           : to-lport
external_ids        : {
    "k8s.ovn.org/id"="default-network-controller:EgressFirewall:default:10000", 
    "k8s.ovn.org/name"=default, 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=EgressFirewall, 
    rule-idx="0"
}
label               : 0
log                 : false
match               : "(ip4.dst == $a5272457940446250407) && ip4.src == $a4322231855293774466"
meter               : acl-logging
name                : "EF:default:10000"
options             : {}
priority            : 10000
severity            : []

action              : allow
direction           : to-lport
external_ids        : {
    "k8s.ovn.org/id"="default-network-controller:EgressFirewall:default:9999", 
    "k8s.ovn.org/name"=default, 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=EgressFirewall, 
    rule-idx="1"
}
label               : 0
log                 : false
match               : "(ip4.dst == 1.2.3.0/24) && ip4.src == $a4322231855293774466 && ((udp && ( udp.dst == 55 )))"
meter               : acl-logging
name                : "EF:default:9999"
options             : {}
priority            : 9999
severity            : []

action              : drop
direction           : to-lport
external_ids        : {
    "k8s.ovn.org/id"="default-network-controller:EgressFirewall:default:9998", 
    "k8s.ovn.org/name"=default, 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=EgressFirewall, 
    rule-idx="2"
}
label               : 0
log                 : false
match               : "(ip4.dst == 0.0.0.0/0 && ip4.dst != 10.244.0.0/16) && ip4.src == $a4322231855293774466"
meter               : acl-logging
name                : "EF:default:9998"
options             : {}
priority            : 9998
severity            : []
```

Egress firewall should be applied after egress network policy independently, to make sure that connection that are
allowed by network policy, but denied by egress firewall will be dropped.

## Multicast

For more details about Multicast in ovn see [Multicast docs](../features/multicast.md)
Multicast creates 2 types of ACLs: global default ACLs with `ExternalIDs["k8s.ovn.org/owner-type"]=MulticastCluster`
and per-namespace ACLs with `ExternalIDs["owner_object_type"]="MulticastNS"`. 

When multicast is enabled for ovn-k, you will find 4 default global ACLs:
- 2 ACLs (ingress/egress) dropping all multicast traffic - on all switches (via clusterPortGroup)
- 2 ACLs (ingress/egress) allowing all multicast traffic - on clusterRouterPortGroup
  (that allows multicast between pods that reside on different nodes, see
  https://github.com/ovn-org/ovn-kubernetes/commit/3864f2b6463392ae2d80c18d06bd46ec44e639f9 for more details)

```
action              : allow
direction           : from-lport
external_ids        : {
  direction=Egress, 
  "k8s.ovn.org/id"="default-network-controller:MulticastCluster:AllowInterNode:Ingress", 
  "k8s.ovn.org/owner-controller"=default-network-controller, 
  "k8s.ovn.org/owner-type"=MulticastCluster, 
  type=AllowInterNode
}
label               : 0
log                 : false
match               : "inport == @clusterRtrPortGroup && (ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))"
meter               : acl-logging
name                : []
options             : {apply-after-lb="true"}
priority            : 1012
severity            : []

action              : allow
direction           : to-lport
external_ids        : {
  direction=Ingress, 
  "k8s.ovn.org/id"="default-network-controller:MulticastCluster:AllowInterNode:Ingress", 
  "k8s.ovn.org/owner-controller"=default-network-controller, 
  "k8s.ovn.org/owner-type"=MulticastCluster, 
  type=AllowInterNode
}
label               : 0
log                 : false
match               : "outport == @clusterRtrPortGroup && (ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))"
meter               : acl-logging
name                : []
options             : {}
priority            : 1012
severity            : []

action              : drop
direction           : from-lport
external_ids        : {
  direction=Egress, 
  "k8s.ovn.org/id"="default-network-controller:MulticastCluster:DefaultDeny:Egress", 
  "k8s.ovn.org/owner-controller"=default-network-controller, 
  "k8s.ovn.org/owner-type"=MulticastCluster, 
  type=DefaultDeny
}
label               : 0
log                 : false
match               : "(ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))"
meter               : acl-logging
name                : []
options             : {apply-after-lb="true"}
priority            : 1011
severity            : []

action              : drop
direction           : to-lport
external_ids        : {
  direction=Ingress, 
  "k8s.ovn.org/id"="default-network-controller:MulticastCluster:DefaultDeny:Egress", 
  "k8s.ovn.org/owner-controller"=default-network-controller, 
  "k8s.ovn.org/owner-type"=MulticastCluster, 
  type=DefaultDeny
}
label               : 0
log                 : false
match               : "(ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))"
meter               : acl-logging
name                : []
options             : {}
priority            : 1011
severity            : []

```

For every namespace with enabled multicast, there are 2 more ACLs, e.g. for default namespace:
```
action              : allow
direction           : to-lport
external_ids        : {
    "k8s.ovn.org/id"="default-network-controller:MulticastNS:default:Allow_Ingress", 
    "k8s.ovn.org/name"=default, 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=MulticastNS, 
    direction=Ingress
}
label               : 0
log                 : false
match               : "outport == @a16982411286042166782 && (igmp || (ip4.src == $a4322231855293774466 && ip4.mcast))"
meter               : acl-logging
name                : []
options             : {}
priority            : 1012
severity            : []

action              : allow
direction           : from-lport
external_ids        : {
    "k8s.ovn.org/id"="default-network-controller:MulticastNS:default:Allow_Egress", 
    "k8s.ovn.org/name"=default, 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=MulticastNS, 
    tdirection=Egress
}
label               : 0
log                 : false
match               : "inport == @a16982411286042166782 && ip4.mcast"
meter               : acl-logging
name                : []
options             : {apply-after-lb="true"}
priority            : 1012
severity            : []
```

## Network Policy

Every node has 1 ACL to allow traffic from that node's management port IP with `ExternalIDs["k8s.ovn.org/owner-type"]=NetpolNode`, like

```
action              : allow-related
direction           : to-lport
external_ids        : {
    ip="10.244.2.2", 
    "k8s.ovn.org/id"="default-network-controller:NetpolNode:ovn-worker:10.244.2.2", 
    "k8s.ovn.org/name"=ovn-worker, 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=NetpolNode
}
label               : 0
log                 : false
match               : "ip4.src==10.244.2.2"
meter               : acl-logging
name                : []
options             : {}
priority            : 1001
severity            : []

```

There are also 2 Default Allow ACLs for the hairpinned traffic (pod->svc->same pod) with `ExternalIDs["k8s.ovn.org/owner-type"]=NetpolDefault`,
which match on the special V4OVNServiceHairpinMasqueradeIP and V6OVNServiceHairpinMasqueradeIP addresses.
These IPs are assigned as src IP to the hairpinned packets.

```
action              : allow-related
direction           : to-lport
external_ids        : {
    direction=Ingress, 
    "k8s.ovn.org/id"="default-network-controller:NetpolDefault:allow-hairpinning:Ingress", 
    "k8s.ovn.org/name"=allow-hairpinning, 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=NetpolDefault
}
label               : 0
log                 : false
match               : "ip4.src == 169.254.169.5"
meter               : acl-logging
name                : []
options             : {}
priority            : 1001
severity            : []

action              : allow-related
direction           : from-lport
external_ids        : {
    direction=Egress, 
    "k8s.ovn.org/id"="default-network-controller:NetpolDefault:allow-hairpinning:Egress", 
    "k8s.ovn.org/name"=allow-hairpinning, 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=NetpolDefault
}
label               : 0
log                 : false
match               : "ip4.src == 169.254.169.5"
meter               : acl-logging
name                : []
options             : {apply-after-lb="true"}
priority            : 1001
severity            : []
```

Network Policy creates default deny ACLs for every namespace that has at least 1 network policy with 
`ExternalIDs["k8s.ovn.org/owner-type"]=NetpolNamespace`.
There are 2 ACL types (`defaultDeny` and `arpAllow`) for every policy direction (Ingress/Egress) e.g. for `default` namespace,

Egress:
```
action              : allow
direction           : from-lport
external_ids        : {
    direction=Egress, 
    "k8s.ovn.org/id"="default-network-controller:NetpolNamespace:default:Egress:arpAllow", 
    "k8s.ovn.org/name"=default, 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=NetpolNamespace, 
    type=arpAllow
}
label               : 0
log                 : false
match               : "inport == @a16982411286042166782_egressDefaultDeny && (arp || nd)"
meter               : acl-logging
name                : "NP:default:Egress"
options             : {apply-after-lb="true"}
priority            : 1001
severity            : []

action              : drop
direction           : from-lport
external_ids        : {
    direction=Egress, 
    "k8s.ovn.org/id"="default-network-controller:NetpolNamespace:default:Egress:defaultDeny", 
    "k8s.ovn.org/name"=default, 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=NetpolNamespace, 
    type=defaultDeny
}
label               : 0
log                 : false
match               : "inport == @a16982411286042166782_egressDefaultDeny"
meter               : acl-logging
name                : "NP:default:Egress"
options             : {apply-after-lb="true"}
priority            : 1000
severity            : []
```

Ingress:
```
_uuid               : a6ffa9d4-e811-4aaf-9505-87cc0b2f442a
action              : allow
direction           : to-lport
external_ids        : {
    direction=Ingress, 
    "k8s.ovn.org/id"="default-network-controller:NetpolNamespace:default:Ingress:arpAllow", 
    "k8s.ovn.org/name"=default, 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=NetpolNamespace, 
    type=arpAllow
}
label               : 0
log                 : false
match               : "outport == @a16982411286042166782_ingressDefaultDeny && (arp || nd)"
meter               : acl-logging
name                : "NP:default:Ingress"
options             : {}
priority            : 1001
severity            : []

action              : drop
direction           : to-lport
external_ids        : {
    direction=Ingress, 
    "k8s.ovn.org/id"="default-network-controller:NetpolNamespace:default:Ingress:defaultDeny", 
    "k8s.ovn.org/name"=default, 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=NetpolNamespace, 
    type=defaultDeny
}
label               : 0
log                 : false
match               : "outport == @a16982411286042166782_ingressDefaultDeny"
meter               : acl-logging
name                : "NP:default:Ingress"
options             : {}
priority            : 1000
severity            : []

```

There are also ACLs owned by every network policy object with `ExternalIDs["k8s.ovn.org/owner-type"]=NetworkPolicy`, e.g.
for the following object

```
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
 name: test-policy
 namespace: default
spec:
 podSelector: {}
 policyTypes:
 - Ingress
 - Egress
 ingress:
 - from:
   - namespaceSelector:
       matchLabels:
         kubernetes.io/metadata.name: default
     podSelector:
       matchLabels:
         app: demo
 egress:
  - to:
    - ipBlock:
        cidr: 10.244.1.5/32
```

2 ACLs will be created:

```
action              : allow-related
direction           : from-lport
external_ids        : {
    direction=egress, 
    gress-index="0", 
    ip-block-index="0", 
    "k8s.ovn.org/id"="default-network-controller:NetworkPolicy:default:test-policy:egress:0:-1:0", 
    "k8s.ovn.org/name"="default:test-policy", 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=NetworkPolicy, 
    port-policy-index="-1"
}
label               : 0
log                 : false
match               : "ip4.dst == 10.244.1.5/32 && inport == @a2653181086423119552"
meter               : acl-logging
name                : "NP:default:test-policy:egress:0:-1:0"
options             : {apply-after-lb="true"}
priority            : 1001
severity            : []

action              : allow-related
direction           : to-lport
external_ids        : {
    direction=ingress, 
    gress-index="0", 
    ip-block-index="-1", 
    "k8s.ovn.org/id"="default-network-controller:NetworkPolicy:default:test-policy:ingress:0:-1:-1", 
    "k8s.ovn.org/name"="default:test-policy", 
    "k8s.ovn.org/owner-controller"=default-network-controller, 
    "k8s.ovn.org/owner-type"=NetworkPolicy, 
    port-policy-index="-1"
}
label               : 0
log                 : false
match               : "(ip4.src == {$a3733136965153973077} || (ip4.src == 169.254.169.5 && ip4.dst == {$a3733136965153973077})) && outport == @a2653181086423119552"
meter               : acl-logging
name                : "NP:default:test-policy:ingress:0:-1:-1"
options             : {}
priority            : 1001
severity            : []

```

`"k8s.ovn.org/name"` is the `<namespace>:<name>` of network policy object, `gress-index` is the index of gress policy in
the `NetworkPolicy.Spec.[In/E]gress`, check `gress_policy.go:getNetpolACLDbIDs` for more details on the rest of the fields.

