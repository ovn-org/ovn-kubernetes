## BaselineAdminNetworkPolicy

## Introduction

## Motivation

## How to enable this feature on an OVN-Kubernetes cluster

## Workflow Description

## Implementation Details

## Troubleshooting

## Future Items

Kubernetes AdminNetworkPolicy API reference: https://github.com/kubernetes-sigs/network-policy-api/blob/429a9e6ae89d411f89d5a16aba38a5d920c969ee/apis/v1alpha1/baseline_adminnetworkpolicy_types.go

Since we can delegate decisions from administrators to namespace owners, what if namespace owners don't have policies in place for the same set of subjects? Admins in such cases might want to keep a default set of guardrails in the cluster. Thus we allow one BANP to be created in the cluster with the name `default`. The rules in `default` BANP are created in Tier3.

Sample API:

```
apiVersion: policy.networking.k8s.io/v1alpha1
kind: BaselineAdminNetworkPolicy
metadata:
  name: default
spec:
  subject:
    namespaces:
      matchLabels:
          conformance-house: gryffindor
  ingress:
  - name: "deny-all-ingress-from-slytherin"
    action: "Deny"
    from:
    - namespaces:
        namespaceSelector:
          matchLabels:
            conformance-house: slytherin
  egress:
  - name: "deny-all-egress-to-slytherin"
    action: "Deny"
    to:
    - namespaces:
        namespaceSelector:
          matchLabels:
            conformance-house: slytherin
```

* BANP doesn't have any priority field, since we can have only one in the cluster
* We keep nbdb.ACL's priority range 1750 - 1649 range reserved for BANP rules in the cluster.
* Rest of the implementation details for ANP is applicable to BANP as well.

Corresponding ACLs:

```
_uuid               : 436b5a0f-9616-42b5-865d-489ec1d42666
action              : drop
direction           : to-lport
external_ids        : {direction=BANPIngress, "k8s.ovn.org/id"="admin-network-policy-controller:BaselineAdminNetworkPolicy:default:BANPIngress:1750", "k8s.ovn.org/name"=default, "k8s.ovn.org/owner-controller"=admin-network-policy-controller, "k8s.ovn.org/owner-type"=BaselineAdminNetworkPolicy, priority="1750"}
label               : 0
log                 : false
match               : "((ip4.src == $a2535546904205657311)) && (outport == @a16982411286042166782)"
meter               : acl-logging
name                : default_BANPIngress_1750
options             : {}
priority            : 1750
severity            : debug
tier                : 3
===
_uuid               : d42cb240-fac1-4429-a1bc-02efddda69cf
action              : drop
direction           : from-lport
external_ids        : {direction=BANPEgress, "k8s.ovn.org/id"="admin-network-policy-controller:BaselineAdminNetworkPolicy:default:BANPEgress:1750", "k8s.ovn.org/name"=default, "k8s.ovn.org/owner-controller"=admin-network-policy-controller, "k8s.ovn.org/owner-type"=BaselineAdminNetworkPolicy, priority="1750"}
label               : 0
log                 : false
match               : "((ip4.dst == $a6430502402203365)) && (inport == @a16982411286042166782)"
meter               : acl-logging
name                : default_BANPEgress_1750
options             : {apply-after-lb="true"}
priority            : 1750
severity            : debug
tier                : 3
```

The Address-Set for the above BaselineAdminNetworkPolicy is:

```
_uuid               : a3ac7d6b-9185-4099-84f8-92d28460b4c3
addresses           : ["10.244.0.5", "10.244.1.6"]
external_ids        : {direction=BANPIngress, ip-family=v4, "k8s.ovn.org/id"="admin-network-policy-controller:BaselineAdminNetworkPolicy:default:BANPIngress:1750:v4", "k8s.ovn.org/name"=default, "k8s.ovn.org/owner-controller"=admin-network-policy-controller, "k8s.ovn.org/owner-type"=BaselineAdminNetworkPolicy, priority="1750"}
name                : a2535546904205657311
===
_uuid               : b3e08332-e6bb-4f4e-bbc4-c36f717f149a
addresses           : ["10.244.0.5", "10.244.1.6"]
external_ids        : {direction=BANPEgress, ip-family=v4, "k8s.ovn.org/id"="admin-network-policy-controller:BaselineAdminNetworkPolicy:default:BANPEgress:1750:v4", "k8s.ovn.org/name"=default, "k8s.ovn.org/owner-controller"=admin-network-policy-controller, "k8s.ovn.org/owner-type"=BaselineAdminNetworkPolicy, priority="1750"}
name                : a6430502402203365
```

The PortGroup for the above AdminNetworkPolicy is:

```
_uuid               : 9ec16567-6f51-49fb-aedb-40c477ad470d
acls                : [436b5a0f-9616-42b5-865d-489ec1d42666, d42cb240-fac1-4429-a1bc-02efddda69cf]
external_ids        : {BaselineAdminNetworkPolicy=default}
name                : a16982411286042166782
ports               : [a22a4c3a-bb65-4b22-8bc1-13e1e8899a7b, c7e4ffe3-73df-4db5-a3bc-a9649394d549]
```