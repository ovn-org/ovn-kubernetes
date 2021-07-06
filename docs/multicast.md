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

### Multicast group membership
As indicated in the [introduction secion](#introduction), multicast filtering is
achieved by dynamic group control management. The manager of these filtering
lists is the switch that is configured to act as an IGMP querier - on OVN-K's
case, the node's logical switches.

For these membership requests to be allowed in the network, each node's logical
switch's must declare themselves "multicast queriers", by using the
`other_config:mcast_querier=true` option.

The `other_config:mcast_snoop=true` is also used so the logical switches can
passively snoop IGMP query / report / leave packets transferred between the
multicast hosts and switches to compute group membership.

Without these two options, multicast traffic would be treated as broadcast
traffic, which forwards packets to all ports on the network.

Please refer to the following snippet featuring the node's logical swithes
of a cluster with one control plane node, and two workers, to see these
options in use:
```
# control plane node
_uuid               : 09d7a498-8885-4b66-9c50-da2286579382
acls                : [ef11235e-54f7-4f7b-a19a-d6d935c836a2]
dns_records         : []
external_ids        : {}
forwarding_groups   : []
load_balancer       : [78e40b60-458c-43b7-b1d8-344ca85c8b08, 84b6c4a7-3ec4-46d4-8611-23267a2e1d83, 9ab56c1a-00f1-4d3d-bea0-e3a2679bce11, bafdde0a-
f303-41e2-98eb-4e149bc75c91, ce697ebf-2059-48fa-a47c-20b901a18395, ebbb52b8-ba28-4550-b03f-0865e6c7162b]
name                : ovn-control-plane
other_config        : {mcast_eth_src="0a:58:0a:f4:00:01", mcast_ip4_src="10.244.0.1", mcast_querier="true", mcast_snoop="true", subnet="10.244.0.0/24"}
ports               : [08273d87-50f9-461f-9421-4df0360a624b, 3ab00f31-a2dc-4257-8882-5e73913c55d3, 3d4ce3c5-407f-4fa2-bbb6-fe9107d9ddc2]
qos_rules           : []

# ovn-worker
_uuid               : cb85ea2b-309b-43fa-8fc8-db97353e872c
acls                : [8d9f3900-1cc2-433b-8a1b-c9536eac8575]
dns_records         : []
external_ids        : {}
forwarding_groups   : []
load_balancer       : [17cb0b5e-840c-4aad-abc5-d0dd9d67a1e7, 2a84133f-0675-4f40-bc47-78608886b848, 38455c9c-5c03-4777-8f35-3d71b3af0487, 84b6c4a7-3ec4-46d4-8611-23267a2e1d83, 9ab56c1a-00f1-4d3d-bea0-e3a2679bce11, ebbb52b8-ba28-4550-b03f-0865e6c7162b]
name                : ovn-worker
other_config        : {mcast_eth_src="0a:58:0a:f4:02:01", mcast_ip4_src="10.244.2.1", mcast_querier="true", mcast_snoop="true", subnet="10.244.2.0/24"}
ports               : [199d3b11-3eba-41b6-947b-22db7f9ee529, 4202a225-83b4-46ff-ba87-624af78d2b16, 84cf40d2-7dc2-477c-aff8-bdf2e0fc60d7, b7415c11-2840-4abb-8965-3abf073aa26b, c716aa8a-7178-4d52-9b7f-174c22084e4e, c7d9ef31-4ec6-450e-8299-cc4d715ce9ea, dfa10e32-5613-4297-b635-75f4db26f4e7, ef13e922-a097-460f-94a2-e1f08b16fbeb, fd8e6b71-ea0d-4a44-a18b-824aa5ff6bc0]
qos_rules           : []

# ovn-worker 2
_uuid               : b9255452-3e7a-4b84-ab64-11154432eb08
acls                : [208907e0-55b1-4b6d-a1d8-985148ba6b29]
dns_records         : []
external_ids        : {}
forwarding_groups   : []
load_balancer       : [32091c8e-394d-46a8-a65e-10b0f1d4aecc, 36f5827e-e027-4740-a908-d6e277b5ad55, 535c0e56-9762-4890-931b-50c4062aa673, 84b6c4a7-3ec4-46d4-8611-23267a2e1d83, 9ab56c1a-00f1-4d3d-bea0-e3a2679bce11, ebbb52b8-ba28-4550-b03f-0865e6c7162b]
name                : ovn-worker2
other_config        : {mcast_eth_src="0a:58:0a:f4:01:01", mcast_ip4_src="10.244.1.1", mcast_querier="true", mcast_snoop="true", subnet="10.244.1.0/24"}
ports               : [20e826fb-0298-4f45-8621-5af6fe7c3bd1, 49938955-f3b8-4363-ab40-acd826cd977c, 5fefc0c6-e651-48a9-aa0d-f86197b93267, 7323dbf3-f8a8-4858-8a00-5b7987e97648, 79463ad7-a50d-4a05-8e15-d695713f6bb3, dddf772b-6925-4fa7-9ea5-1e4fd1267cb3, f584c3b2-5370-45c6-912b-56be00168c98]
qos_rules           : []
```

**Note:** it is important to note that the source IP / MAC address of the
multicast queries sent by the logical switch are the addresses of the
logical router ports connecting each node's switch to the cluster's router,
as can be seen below.

```
# ovn-control-plane logical router port
_uuid               : e3e21af2-0ec5-4993-bb0c-e052b3f3eeb7
enabled             : []
external_ids        : {}
gateway_chassis     : []
ha_chassis_group    : []
ipv6_prefix         : []
ipv6_ra_configs     : {}
mac                 : "0a:58:0a:f4:00:01"
name                : rtos-ovn-control-plane
networks            : ["10.244.0.1/24"]
options             : {}
peer                : []

# ovn-worker logical router port
_uuid               : 28be35a4-26cf-4daf-b922-c6aa5cecf58b
enabled             : []
external_ids        : {}
gateway_chassis     : []
ha_chassis_group    : []
ipv6_prefix         : []
ipv6_ra_configs     : {}
mac                 : "0a:58:0a:f4:02:01"
name                : rtos-ovn-worker
networks            : ["10.244.2.1/24"]
options             : {}
peer                : []

# ovn-worker2 logical router port
_uuid               : 6bcbca4e-572f-4109-a71e-862292f463b2
enabled             : []
external_ids        : {}
gateway_chassis     : []
ha_chassis_group    : []
ipv6_prefix         : []
ipv6_ra_configs     : {}
mac                 : "0a:58:0a:f4:01:01"
name                : rtos-ovn-worker2
networks            : ["10.244.1.1/24"]
options             : {}
peer                : []

```

Finally, to enable the multicast group to span across multiple OVN nodes, we
need to enable multicast relay on the cluster router. That is done via the
`option:mcast_relay=true` option. Please check below for a real OVN-K deployment
example:

```
_uuid               : acb0f1ab-2b40-463b-b5ce-fbdd183f0f44
enabled             : []
external_ids        : {k8s-cluster-router=yes, k8s-ovn-topo-version="4"}
load_balancer       : []
name                : ovn_cluster_router
nat                 : [8002667a-f4a1-4cbb-8830-9286f3636791, a74f337f-6a8d-4cd6-a32d-df49560aa224, c38d2424-0e13-40a8-a67f-3317b9bdbbdd]
options             : {mcast_relay="true"}
policies            : [1ace3e52-8be3-49fa-86ae-e910ffbe4dd3, 1afd7fa9-c32d-4240-84dd-da48115e95c8, 2274b1f0-e8c3-495d-9c9d-a5f14f517920, 24f00c8a-7df5-4041-986b-5be79a605807, 2d7df894-ea13-4819-9614-04c814f34c94, 3a20641e-29f3-4577-aaa5-12a5ae8a56fa, 521edd9a-bc5d-4ca7-bf13-7435c5f1fbce, 8d5ee2c5-65b3-478a-a034-a6624231b6ec, b024d223-56eb-462e-9e27-1b8b4ffcec08, b4999288-7af0-4b1f-bcd7-94974562e5b0, b4ac21ed-530f-45ac-bff4-fc09fcfedd25, d4c434df-9ddd-47ab-8016-062f42d3102b, ec342a8c-a39a-4ea5-ae13-b39d17760a4e]
ports               : [28be35a4-26cf-4daf-b922-c6aa5cecf58b, 3f5b669e-6c6c-46b0-a029-c6198d47706d, 6bcbca4e-572f-4109-a71e-862292f463b2, e3e21af2-0ec5-4993-bb0c-e052b3f3eeb7, f39f5210-8ec6-4d0b-89ef-8397599cc8cf]
static_routes       : [3d9a8a37-368a-43ca-9c62-80cdae843b77, 53cfa8f0-a10e-45aa-9a9f-8e9b4910315b, 6f992b50-5c52-4caf-a146-ba5ca45d7d6a, ae5f8b78-3253-47b1-818d-13f07f42dd48, b65fcc82-1015-40dd-99f4-5b98e7514fe0, de705ce6-3a28-42ac-b3bb-fdba55b020a5]
```

## Sources
- [PR introducing multicast into OVN-K](https://github.com/ovn-org/ovn-kubernetes/pull/885)
- [Dumitru Ceara's presentation about IGMP snooping / relay](https://www.youtube.com/watch?v=1BdLzyGHgTY)

