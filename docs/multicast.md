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
The feature is gated by config flag. In order to create a KIND cluster with
multicast feature enabled, use the `--multicast-enabled` option with KIND.

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
- 2 ACLs (ingress/egress) dropping all multicast traffic - on all switches (via clusterPortGroup)
- 2 ACLs (ingress/egress) allowing all multicast traffic - on clusterRouterPortGroup 
(that allows multicast between pods that reside on different nodes, see 
https://github.com/ovn-org/ovn-kubernetes/commit/3864f2b6463392ae2d80c18d06bd46ec44e639f9 for more details)


These ACLs Matches look like:

```
# deny all multicast match
"(ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))"

# allow clusterPortGroup match ingress
"outport == @clusterRtrPortGroup && (ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))"
# allow clusterPortGroup match egress
"inport == @clusterRtrPortGroup && (ip4.mcast || mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))"
```

Then, for each annotated(`k8s.ovn.org/multicast-enabled=true`) namespace, two
ACLs with higher priority are provisioned; in the following example we show ACLs
that apply to the `default` namespace.

```
# egress direction
match               : "inport == @a16982411286042166782 && ip4.mcast"

# ingress direction
match               : "outport == @a16982411286042166782 && (igmp || (ip4.src == $a5154718082306775057 && ip4.mcast))"
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
## IPv6 considerations
There are some changes when the cluster is configured to also assign IPv6
addresses to the pods, starting with the `allow` ACLs, which now also account
for the IPv6 addresses:

```
_uuid               : 67ed1d4d-c81e-4553-a232-f0448798462e
action              : allow
direction           : to-lport
external_ids        : {default-deny-policy-type=Ingress}
log                 : false
match               : "outport == @a16982411286042166782 && ((igmp || (ip4.src == $a5154718082306775057 && ip4.mcast)) || (mldv1 || mldv2 || (ip6.src == $a5154715883283518635 && (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1))))"
meter               : acl-logging
name                : default_MulticastAllowIngress
priority            : 1012
severity            : info

_uuid               : bda7c475-613e-4dcf-8e88-024ef70b030d
action              : allow
direction           : from-lport
external_ids        : {default-deny-policy-type=Egress}
log                 : false
match               : "inport == @a16982411286042166782 && (ip4.mcast || (mldv1 || mldv2 || (ip6.dst[120..127] == 0xff && ip6.dst[116] == 1)))"
meter               : acl-logging
name                : default_MulticastAllowEgress
priority            : 1012
severity            : info
```

Please note that in fact there is an address set **per IP family per namespace**
; this means the `default`  namespaces has two distinct address sets: one for
IPv4, another for IPv6:

```
_uuid               : 2b48e408-5058-470b-8c0f-59d839d6a80f
addresses           : ["10.244.2.9"]
external_ids        : {name=default_v4}
name                : a5154718082306775057

_uuid               : 37076cff-9560-47c2-bbaa-312ff5e1a114
addresses           : ["fd00:10:244:3::9"]
external_ids        : {name=default_v6}
name                : a5154715883283518635
```

Finally, it is also important to refer we need to specify the `mcast_ip6_src`
option on each node's logical switch, also using the IP address of each node's
logical router port:

```
_uuid               : e1fd9e60-831a-4ca9-ad4c-6a5bdfc018f8
acls                : [0403fbb5-6633-46f1-8481-b329c1ccd916, f49c2de7-5f9d-42bf-a2dc-5f10b53a8347]
dns_records         : []
external_ids        : {}
forwarding_groups   : []
load_balancer       : [142ed4bf-1422-4e9a-a69f-9d07aa67abb8, 6df77979-f829-465f-9617-abc479945261, a9395376-9e96-4b1e-ac2d-8b03947db6cc, bd8a8f3a-4872-49d2-8d14-cd83c48ca265, f05994f7-3f21-457e-aa2b-896a86da319e, fa55f0a8-e3af-4467-8444-a54acfffef6d]
name                : ovn-control-plane
other_config        : {ipv6_prefix="fd00:10:244:1::", mcast_eth_src="0a:58:0a:f4:00:01", mcast_ip4_src="10.244.0.1", mcast_ip6_src="fe80::858:aff:fef4:1", mcast_querier="true", mcast_snoop="true", subnet="10.244.0.0/24"}
ports               : [abea774f-d989-402a-a2af-07c066b130db, c6c6468f-6459-4072-8c98-e02bf08fb377, ecceb049-8afd-40ea-bd6d-77eab32c3302]
qos_rules           : []

_uuid               : 6b7de40a-618a-4d5a-b8b8-58e1f964ee71
acls                : [42018b5b-31e2-4733-bcde-c6ea470836f2, d980fb54-3178-468a-981b-b43e5efa1d48]
dns_records         : []
external_ids        : {}
forwarding_groups   : []
load_balancer       : [109b7d66-2a59-4945-b067-1e51548a6f30, 6727e44b-6256-47b7-8146-0dc9561ce270, 6df77979-f829-465f-9617-abc479945261, 7e59e9b1-a2db-4005-97f3-c1b99131126f, a9395376-9e96-4b1e-ac2d-8b03947db6cc, bd8a8f3a-4872-49d2-8d14-cd83c48ca265]
name                : ovn-worker
other_config        : {ipv6_prefix="fd00:10:244:2::", mcast_eth_src="0a:58:0a:f4:01:01", mcast_ip4_src="10.244.1.1", mcast_ip6_src="fe80::858:aff:fef4:101", mcast_querier="true", mcast_snoop="true", subnet="10.244.1.0/24"}
ports               : [0a9f14e7-94f0-49f2-b5df-acb123ace867, 53251da6-1c21-4e81-97f6-a9b41c64d71a, 6b5deecb-7536-4eeb-a865-2b8ca17be1fc, 8d56ad80-e45c-43b7-9f20-91d15bfc8836, a3252b54-7c2d-48e0-b2b2-36d121ca7292, cc0c791d-c6ee-4381-b8a4-91462039bfd1, f261f485-b974-4448-9751-62e883d677f2, fe77f44c-51ee-45f7-a7a2-4e03862803de]
qos_rules           : []

_uuid               : 3b4e3a69-8439-4124-a028-cb2851d80da6
acls                : [607abd66-a12a-4387-8b3f-66c4ba4cb9a8, 9963f94c-cde0-41a1-b0cc-c3e6f5800e67]
dns_records         : []
external_ids        : {}
forwarding_groups   : []
load_balancer       : [26a7f6a7-9a5c-4023-ac55-10a860237a6d, 2fa35fce-a63a-4185-ae49-b9c09709fdea, 6df77979-f829-465f-9617-abc479945261, a9395376-9e96-4b1e-ac2d-8b03947db6cc, bd8a8f3a-4872-49d2-8d14-cd83c48ca265, c3de2c11-8cd3-4d6f-9373-7a1b54e1366a]
name                : ovn-worker2
other_config        : {ipv6_prefix="fd00:10:244:3::", mcast_eth_src="0a:58:0a:f4:02:01", mcast_ip4_src="10.244.2.1", mcast_ip6_src="fe80::858:aff:fef4:201", mcast_querier="true", mcast_snoop="true", subnet="10.244.2.0/24"}
ports               : [1b40d04a-26d2-4881-ac55-6c2a5fa679a9, 35316f83-79c3-4a23-9fdd-04cefd963f54, 39c240ef-3303-4fa4-8b5c-b860d69d7c11, 44ddb1ab-56c3-4665-8f1e-02505553a950, 728221da-fe1f-41cc-90ff-9e2a8eb8fb6d, ac22f113-7e40-4657-ad7e-4444a39bfd45, bd115b86-bdc1-4427-9cc7-0ece2d9269c6, f2a89d6a-b6c8-491c-b2b1-398d37c5aa4c]
qos_rules           : []
```

## Sources
- [PR introducing multicast into OVN-K](https://github.com/ovn-org/ovn-kubernetes/pull/885)
- [PR introducing IPv6 multicast support into OVN-K](https://github.com/ovn-org/ovn-kubernetes/pull/1705)
- [Dumitru Ceara's presentation about IGMP snooping / relay](https://www.youtube.com/watch?v=1BdLzyGHgTY)

