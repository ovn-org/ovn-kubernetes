# Multicast

## Introduction
IP multicast enables data to be delivered to multiple IP addresses
simultaneously.
Multicast can distribute data one-to-many or many-to-many. For this to happen,
the 'receivers' join a multicast group, and the sender(s) send data to it.
In other words, multicast filtering is achieved by dynamic group control
management.

The multicast group membership is implemented with IGMP. For details, check RFCs
[1112](https://datatracker.ietf.org/doc/html/rfc1112)
and [2236](https://datatracker.ietf.org/doc/html/rfc2236).

## Configuring multicast on the cluster

### Enabling multicast per namespace
The multicast traffic between pods in the cluster is blocked by default; it can
be enabled **per namespace** - but it **cannot** be enabled cluster wide.

To enable multicast support on a given namespace, you need to annotate the
namespace:

```bash
$ kubectl annotate namespace <namespace name> \
    k8s.ovn.org/multicast-enabled=true
```
## Changes in OVN northbound database
In this section we will be seeing plenty of OVN north entities; all of it
consists of an example with a single pod:

```
# only list the pod name + IPs of the pod (in the default namespace)
$ kubectl get pods -o=custom-columns=Name:.metadata.name,IP:.status.podIPs
Name                                 IP
virt-launcher-vmi-masquerade-thr9j   [map[ip:10.244.1.8]]
```

The implementation of IPv4 multicast for ovn-kubernetes relies on:
- an ACL dropping all egress multicast traffic - on all pods
- an ACL dropping all ingress multicast traffic - on all pods

These ACLs look like:

```
# ingress direction
_uuid               : d5024a91-12c8-49ea-9f11-fb4bb3878613
action              : drop
direction           : to-lport
external_ids        : {default-deny-policy-type=Ingress}
log                 : false
match               : "(ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))"
meter               : acl-logging
name                : "97faee09-ae44-4b4e-9bd3-6df88116b25a_DefaultDenyMulticastIngres"
priority            : 1011
severity            : info

# egress direction
_uuid               : 150b3f92-9cbc-482d-b5c6-1037be2ca255
action              : drop
direction           : from-lport
external_ids        : {default-deny-policy-type=Egress}
log                 : false
match               : "(ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))"
meter               : acl-logging
name                : "97faee09-ae44-4b4e-9bd3-6df88116b25a_DefaultDenyMulticastEgress"
priority            : 1011
severity            : info
```

Then, for each annotated(`k8s.ovn.org/multicast-enabled=true`) namespace, two
ACLs with higher priority are provisioned; in the following example we show ACLs
that apply to the `default` namespace.

```
# egress direction
_uuid               : f086c9b7-fa61-4a91-b545-f228f6cf954b
action              : allow
direction           : from-lport
external_ids        : {default-deny-policy-type=Egress}
log                 : false
match               : "inport == @a16982411286042166782 && ip4.mcast"
meter               : acl-logging
name                : default_MulticastAllowEgress
priority            : 1012
severity            : info

# ingress direction
_uuid               : b930b6ea-5b16-4eb1-b962-6b3e9273d0a0
action              : allow
direction           : to-lport
external_ids        : {default-deny-policy-type=Ingress}
log                 : false
match               : "outport == @a16982411286042166782 && (igmp || (ip4.src == $a5154718082306775057 && ip4.mcast))"
meter               : acl-logging
name                : default_MulticastAllowIngress
priority            : 1012
severity            : info
```

As can be seen in the match condition of the ACLs above, the former ACL allows
egress traffic for all multicast traffic whose originating ports belong to
the namespace, whereas the latter allows ingress multicast traffic for ports
belonging to the namespace. This last match also assures that traffic
originating by pods in the same namespace are allowed.

Both these ACLs require a port group to keep track of all ports within the
namespace - `@a16982411286042166782` - while the `ingress` ACL also requires
the namespace's `address set` to be up to date. Both these tables can be seen
below:

```
# port group encoding the `default` namespace
_uuid               : dde5bbb0-0b1d-4dab-b100-0b710f46fc28
acls                : [b930b6ea-5b16-4eb1-b962-6b3e9273d0a0, f086c9b7-fa61-4a91-b545-f228f6cf954b]
external_ids        : {name=default}
name                : a16982411286042166782
ports               : [5fefc0c6-e651-48a9-aa0d-f86197b93267]

# address set encoding the `default` namespace
_uuid               : 1349957f-68e5-4f5a-af26-01ec58d96f6b
addresses           : ["10.244.1.8"]
external_ids        : {name=default_v4}
name                : a5154718082306775057
```

**Note:** notice the IP address on the address set encoding the default
namespace matches the IP address of the pod listed on our example.

For completeness, let's also take a look at the port referenced in the port
group:

```
_uuid               : 5fefc0c6-e651-48a9-aa0d-f86197b93267
addresses           : ["0a:58:0a:f4:01:08 10.244.1.8"]
dhcpv4_options      : []
dhcpv6_options      : []
dynamic_addresses   : []
enabled             : []
external_ids        : {namespace=default, pod="true"}
ha_chassis_group    : []
name                : default_virt-launcher-vmi-masquerade-thr9j
options             : {requested-chassis=ovn-worker2}
parent_name         : []
port_security       : ["0a:58:0a:f4:01:08 10.244.1.8"]
tag                 : []
tag_request         : []
type                : ""
up                  : true
```
As you can see, this is the OVN logical switch port assigned to the pod running
on the namespace we have annotated.

## Sources
- [PR introducing multicast into OVN-K](https://github.com/ovn-org/ovn-kubernetes/pull/885)

