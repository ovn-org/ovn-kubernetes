# EgressQoS

## Introduction

The EgressQoS feature enables marking pods egress traffic with a valid QoS Differentiated Services Code Point (DSCP) value.
The QoS markings will be consumed and acted upon by network appliances outside of the Kubernetes cluster
to optimize traffic flow throughout their networks.

The EgressQoS resource is namespaced-scoped and allows specifying a set of QoS rules - each has a DSCP value, an optional
destination CIDR (dstCIDR) and an optional PodSelector (podSelector).
A rule applies its DSCP marking to traffic coming from pods whose labels match the podSelector heading to the dstCIDR.
A namespace supports having only one EgressQoS resource named `default` (other EgressQoSes will be ignored).

## Example

```yaml
kind: EgressQoS
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: default
spec:
  egress:
  - dscp: 30
    dstCIDR: 1.2.3.0/24
  - dscp: 42
    podSelector:
      matchLabels:
        app: example
  - dscp: 28
```

This example marks the packets originating from pods in the `default` namespace in the following way:
* All traffic heading to an address that belongs to 1.2.3.0/24 is marked with DSCP 30.
* Egress traffic from pods labeled `app: example` is marked with DSCP 42.
* All egress traffic is marked with DSCP 28.

The priority of a rule is determined by its placement in the egress array.
An earlier rule is processed before a later rule. In this example, if the rules are reversed
all traffic originating from pods in the `default` namespace is marked with DSCP 28 - regardless of
its destination or pods labels.
Because of that specific rules should always come before general ones in that array.

## Changes in OVN northbound database

EgressQoS is implemented by reacting to events from `EgressQoSes`, `Pods` and `Nodes` changes -
updating OVN's northbound database `QoS`, `Address_Set` and `Logical_Switch` objects.
The code is implemented under `pkg/ovn/egressqos.go` and most of the logic for the events sits under
the `syncEgressQoS`, `syncEgressQoSPod`, `syncEgressQoSNode` functions.

We'll use the example YAML above and see how the related objects are changed in OVN's northbound database
in a Dual-Stack kind cluster.

```
$ kubectl get nodes
NAME                STATUS   ROLES               
ovn-control-plane   Ready    control-plane,master
ovn-worker          Ready    <none>              
ovn-worker2         Ready    <none>              
```

```
$ kubectl get pods -o=custom-columns=Name:.metadata.name,IPs:.status.podIPs,LABELS:.metadata.labels
Name           IPs                                             LABELS
no-labels      [map[ip:10.244.1.3] map[ip:fd00:10:244:2::3]]   <none>
with-labels1   [map[ip:10.244.2.3] map[ip:fd00:10:244:3::3]]   map[app:example]
```

```
$ kubectl get egressqoses
No resources found in default namespace.
```

At this point there are no QoS objects in the northbound database.
We create the example EgressQoS:
```
$ kubectl get egressqoses
NAME      AGE
default   6s
```

And the following is created in the northbound database (`SyncEgressQoS`):
```
# QoS

_uuid               : 14b923a1-d7b0-42b8-a3d7-6a5028b09ae2
action              : {dscp=30}
bandwidth           : {}
direction           : to-lport
external_ids        : {EgressQoS=default}
match               : "(ip4.dst == 1.2.3.0/24) && (ip4.src == $a5154718082306775057 || ip6.src == $a5154715883283518635)"
priority            : 1000

_uuid               : 820a011d-0eda-43b7-994d-46a55620c4bf
action              : {dscp=42}
bandwidth           : {}
direction           : to-lport
external_ids        : {EgressQoS=default}
match               : "(ip4.dst == 0.0.0.0/0 || ip6.dst == ::/0) && (ip4.src == $a10759091379580272948 || ip6.src == $a10759093578603529370)"
priority            : 999

_uuid               : 1e35ea19-3353-4cbc-a1f5-7ea5bf831d67
action              : {dscp=28}
bandwidth           : {}
direction           : to-lport
external_ids        : {EgressQoS=default}
match               : "(ip4.dst == 0.0.0.0/0 || ip6.dst == ::/0) && (ip4.src == $a5154718082306775057 || ip6.src == $a5154715883283518635)"
priority            : 998
```

A QoS object is created for each rule specified in the EgressQoS, all attached to all of the nodes logical switches:
```
# Logical_Switch

name                : ovn-worker
qos_rules           : [14b923a1-d7b0-42b8-a3d7-6a5028b09ae2, 1e35ea19-3353-4cbc-a1f5-7ea5bf831d67, 820a011d-0eda-43b7-994d-46a55620c4bf]

name                : ovn-worker2
qos_rules           : [14b923a1-d7b0-42b8-a3d7-6a5028b09ae2, 1e35ea19-3353-4cbc-a1f5-7ea5bf831d67, 820a011d-0eda-43b7-994d-46a55620c4bf]

name                : ovn-control-plane
qos_rules           : [14b923a1-d7b0-42b8-a3d7-6a5028b09ae2, 1e35ea19-3353-4cbc-a1f5-7ea5bf831d67, 820a011d-0eda-43b7-994d-46a55620c4bf]
```

When a new node is added to the cluster we attach all of the `QoS` objects that belong to EgressQoSes
to its logical switch as well (`SyncEgressQoSNode`).

Notice that QoS objects that belong to rules **without** a `podSelector` reference the same address sets for
their src match - which are the namespace's address sets:
```
# Address_Set

_uuid               : 7cb04096-7107-4a35-ae34-be4b0e0c584e
addresses           : ["10.244.1.3", "10.244.2.3"]
external_ids        : {name=default_v4}
name                : a5154718082306775057

_uuid               : 467a93c9-9c76-40bf-8318-e91ee7f6f010
addresses           : ["fd00:10:244:2::3", "fd00:10:244:3::3"]
external_ids        : {name=default_v6}
name                : a5154715883283518635
```
We only use these address sets without editing them.

For each rule that does have a `podSelector` an address set is created, adding the pods that match to it.
Its name (external_ids) follows the pattern `egress-qos-pods-<rule-namespace>-<rule-priority>`,
rule-priority is `1000 - rule's index in the array` which matches the QoS object's priority that relates to that rule.
```
# Address_Set

_uuid               : 74b8df1e-5a2d-4fa4-a5e5-57bdea61d0c5
addresses           : ["fd00:10:244:3::3"]
external_ids        : {name=egress-qos-pods-default-999_v6}
name                : a10759093578603529370

_uuid               : a718588e-0a9b-480e-9adb-2738e524b82d
addresses           : ["10.244.2.3"]
external_ids        : {name=egress-qos-pods-default-999_v4}
name                : a10759091379580272948
```

When a new pod that matches a rule's `podSelector` is created in the namespace we add its IPs to the
relevant address set (`SyncEgressQoSPod`):
```
$ kubectl get pods -o=custom-columns=Name:.metadata.name,IPs:.status.podIPs,LABELS:.metadata.labels
Name           IPs                                             LABELS
no-labels      [map[ip:10.244.1.3] map[ip:fd00:10:244:2::3]]   <none>
with-labels1   [map[ip:10.244.2.3] map[ip:fd00:10:244:3::3]]   map[app:example]
with-labels2   [map[ip:10.244.2.4] map[ip:fd00:10:244:3::4]]   map[app:example]
```

```
# Address_Set

_uuid               : 74b8df1e-5a2d-4fa4-a5e5-57bdea61d0c5
addresses           : ["fd00:10:244:3::3", "fd00:10:244:3::4"]
external_ids        : {name=egress-qos-pods-default-999_v6}
name                : a10759093578603529370
--
_uuid               : a718588e-0a9b-480e-9adb-2738e524b82d
addresses           : ["10.244.2.3", "10.244.2.4"]
external_ids        : {name=egress-qos-pods-default-999_v4}
name                : a10759091379580272948
```

When a pod is deleted or its labels change and do not longer match the rule's `podSelector`
its IPs are deleted from the relevant address set:
```
$ kubectl get pods -o=custom-columns=Name:.metadata.name,IPs:.status.podIPs,LABELS:.metadata.labels
Name           IPs                                             LABELS
no-labels      [map[ip:10.244.1.3] map[ip:fd00:10:244:2::3]]   <none>
with-labels2   [map[ip:10.244.2.4] map[ip:fd00:10:244:3::4]]   map[app:not-the-example]
```

```
# Address_Set

_uuid               : 74b8df1e-5a2d-4fa4-a5e5-57bdea61d0c5
addresses           : []
external_ids        : {name=egress-qos-pods-default-999_v6}
name                : a10759093578603529370
--
_uuid               : a718588e-0a9b-480e-9adb-2738e524b82d
addresses           : []
external_ids        : {name=egress-qos-pods-default-999_v4}
name                : a10759091379580272948
```

When an EgressQoS is updated - we recreate the QoS and Address Set objects, detach the old QoSes from the logical switches
and attach the new ones:

```yaml
kind: EgressQoS
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: default
spec:
  egress:
  - dscp: 48
    podSelector:
      matchLabels:
        app: updated-example
  - dscp: 28
```

```
$ kubectl get pods -o=custom-columns=Name:.metadata.name,IPs:.status.podIPs,LABELS:.metadata.labels
Name                  IPs                                             LABELS
no-labels             [map[ip:10.244.1.3] map[ip:fd00:10:244:2::3]]   <none>
with-labels2          [map[ip:10.244.2.4] map[ip:fd00:10:244:3::4]]   map[app:not-the-example]
with-updated-labels   [map[ip:10.244.1.4] map[ip:fd00:10:244:2::4]]   map[app:updated-example]
```

```
# QoS

_uuid               : cf84322a-0b9e-4aef-97bb-4dcd13f0e73d
action              : {dscp=48}
bandwidth           : {}
direction           : to-lport
external_ids        : {EgressQoS=default}
match               : "(ip4.dst == 0.0.0.0/0 || ip6.dst == ::/0) && (ip4.src == $a17475935309050627288 || ip6.src == $a17475937508073883710)"
priority            : 1000

_uuid               : 8bf07e6b-ca0a-4d76-a206-b7ccb5e948d8
action              : {dscp=28}
bandwidth           : {}
direction           : to-lport
external_ids        : {EgressQoS=default}
match               : "(ip4.dst == 0.0.0.0/0 || ip6.dst == ::/0) && (ip4.src == $a5154718082306775057 || ip6.src == $a5154715883283518635)"
priority            : 999
```

```
# Logical_Switch

name                : ovn-worker
qos_rules           : [8bf07e6b-ca0a-4d76-a206-b7ccb5e948d8, cf84322a-0b9e-4aef-97bb-4dcd13f0e73d]

name                : ovn-worker2
qos_rules           : [8bf07e6b-ca0a-4d76-a206-b7ccb5e948d8, cf84322a-0b9e-4aef-97bb-4dcd13f0e73d]

name                : ovn-control-plane
qos_rules           : [8bf07e6b-ca0a-4d76-a206-b7ccb5e948d8, cf84322a-0b9e-4aef-97bb-4dcd13f0e73d]
```

```
# Address_Set

_uuid               : a27cf559-0981-487f-a0ca-5a355da89cba
addresses           : ["10.244.1.4"]
external_ids        : {name=egress-qos-pods-default-1000_v4}
name                : a17475935309050627288

_uuid               : 93cdc1fd-995c-498b-88a1-4992af93c630
addresses           : ["fd00:10:244:2::4"]
external_ids        : {name=egress-qos-pods-default-1000_v6}
name                : a17475937508073883710

_uuid               : 7cb04096-7107-4a35-ae34-be4b0e0c584e
addresses           : ["10.244.1.3", "10.244.1.4", "10.244.2.4"]
external_ids        : {name=default_v4}
name                : a5154718082306775057

_uuid               : 467a93c9-9c76-40bf-8318-e91ee7f6f010
addresses           : ["fd00:10:244:2::3", "fd00:10:244:2::4", "fd00:10:244:3::4"]
external_ids        : {name=default_v6}
name                : a5154715883283518635
```
