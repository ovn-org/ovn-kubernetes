# AdminNetworkPolicy

Kubernetes AdminNetworkPolicy documentation: https://network-policy-api.sigs.k8s.io/

Kubernetes AdminNetworkPolicy API reference: https://github.com/kubernetes-sigs/network-policy-api/blob/429a9e6ae89d411f89d5a16aba38a5d920c969ee/apis/v1alpha1/adminnetworkpolicy_types.go

NOTE: This documentation focuses on OVNK's implementation of ANP and is more for developers than for users.

NetworkPolicy API was designed mainly for namespace owners or application developers. They are thus namespace scoped. NetworkPolicy API is not suitable for administrators/network-administrators/security-operators of the cluster because of two main reasons:

* They are not cluster-scoped and hence its hard to define a network of policies that spawns across namespaces
* They cannot be created before the namespace is created (kapi server expects the namespace to be created first); thus they cannot satisfy the requirements where admins may want policies in the cluster to be in place before the workloads are even created.
* The design of NetworkPolicy API is implicit which means the deny is already set in place when we create a policy and then we are expected to do an allowList of rules in the policy. Network administrators prefer to have the power of defining what exactly to deny and allow instead of this implicit model.

Sample API:

```
apiVersion: policy.networking.k8s.io/v1alpha1
kind: AdminNetworkPolicy
metadata:
  name: pass-example
spec:
  priority: 10
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

**OVN-Implementation:**

* Each AdminNetworkPolicy CRD will have a `.spec.priority` field. The lower the number the higher the precedence. Thus 0 is the highest priority and 1000 (the largest number supported by upstream sig-network-policy-api) is the lowest priority. However the number of admin policies in a cluster are usually expected to be of a maximum of say 30-50 and not more than that based on use cases for which this API was defined for. OVNK plugin will support 100 policies max in a cluster. If anyone creates more than 100, it will not work properly. Thus supported priority values in OVNK are from 0 to 99.
* Each AdminNetworkPolicy CRD can have upto 100 ingress rules and 100 egress rules, thus 200 rules in total. The ordering of each rule is important. If the rule is at the top of the list then it has the highest precedence and if the rule is at the bottom it has the lowest prededence. Each rule translates to one ACL.
* Since we can have upto 100 policies and each one can have upto 100 gress rules (100*100), we have blocked out the range: 30,000 - 20,000 priority range for the OVN nbdb.ACL table in the `Tier1` block for ANP's implementation.
* Each AdminNetworkPolicy CRD will have a subject on which the policy is applied on - this is translated to one PortGroup on which the ACLs of each rules are attached on.
* Each gress rule can have upto 100 peers. Each rule will also create an nbdb.AddressSet which will contain the IPs of all the pods that are selected by the peer selector across all the peers of that given rule.

The PortGroup for the above AdminNetworkPolicy is:

```
_uuid               : a10e3675-5260-4e28-9462-b705b9dac862
acls                : [120082fa-5a70-4c72-9211-529766078278, 2e0f811f-e0db-41ad-b12b-4a0cf1c621ae]
external_ids        : {AdminNetworkPolicy=pass-example}
name                : a3052488126344707991
ports               : [a22a4c3a-bb65-4b22-8bc1-13e1e8899a7b, c7e4ffe3-73df-4db5-a3bc-a9649394d549]
```

The ACLs for the above AdminNetworkPolicy are:

```
_uuid               : 2e0f811f-e0db-41ad-b12b-4a0cf1c621ae
action              : drop
direction           : to-lport
external_ids        : {direction=ANPIngress, "k8s.ovn.org/id"="admin-network-policy-controller:AdminNetworkPolicy:pass-example:ANPIngress:29000", "k8s.ovn.org/name"=pass-example, "k8s.ovn.org/owner-controller"=admin-network-policy-controller, "k8s.ovn.org/owner-type"=AdminNetworkPolicy, priority="29000"}
label               : 0
log                 : false
match               : "((ip4.src == $a10282262890368313763)) && (outport == @a3052488126344707991)"
meter               : acl-logging
name                : pass-example_ANPIngress_29000
options             : {}
priority            : 29000
severity            : debug
tier                : 1
===
_uuid               : 120082fa-5a70-4c72-9211-529766078278
action              : drop
direction           : from-lport
external_ids        : {direction=ANPEgress, "k8s.ovn.org/id"="admin-network-policy-controller:AdminNetworkPolicy:pass-example:ANPEgress:29000", "k8s.ovn.org/name"=pass-example, "k8s.ovn.org/owner-controller"=admin-network-policy-controller, "k8s.ovn.org/owner-type"=AdminNetworkPolicy, priority="29000"}
label               : 0
log                 : false
match               : "((ip4.dst == $a16426961577074298037)) && (inport == @a3052488126344707991)"
meter               : acl-logging
name                : pass-example_ANPEgress_29000
options             : {apply-after-lb="true"}
priority            : 29000
severity            : debug
tier                : 1
```

The Address-Sets for the above AdminNetworkPolicy are:

```
_uuid               : df47aab4-ab2c-4530-bb08-2a90476af9a7
addresses           : ["10.244.0.5", "10.244.1.6"]
external_ids        : {direction=ANPIngress, ip-family=v4, "k8s.ovn.org/id"="admin-network-policy-controller:AdminNetworkPolicy:pass-example:ANPIngress:29000:v4", "k8s.ovn.org/name"=pass-example, "k8s.ovn.org/owner-controller"=admin-network-policy-controller, "k8s.ovn.org/owner-type"=AdminNetworkPolicy, priority="29000"}
name                : a10282262890368313763
===
_uuid               : d797804a-9401-446e-8295-1f2eebcfa80b
addresses           : ["10.244.0.5", "10.244.1.6"]
external_ids        : {direction=ANPEgress, ip-family=v4, "k8s.ovn.org/id"="admin-network-policy-controller:AdminNetworkPolicy:pass-example:ANPEgress:29000:v4", "k8s.ovn.org/name"=pass-example, "k8s.ovn.org/owner-controller"=admin-network-policy-controller, "k8s.ovn.org/owner-type"=AdminNetworkPolicy, priority="29000"}
name                : a16426961577074298037
```

NOTE: Since priority is 10, it is 29000 in the ACL world. If we had a second rule, that rule would get 28999 as its priority. There are no default deny policies for a given ANP unlike NP.

***Pass Action:***

In addition to setting `Deny` and `Allow` actions on ANP API rules, one can also use the `Pass` action for a rule. What this means is ANP controller defers the decision of packets that match the pass action rule to either the NetworkPolicy OR to the BaselineAdminNetworkPolicy defined in the cluster (if either of them match the same set of pods, then they will take effect and if not, the result will be an `Allow`). Order of precedence: AdminNetworkPolicy (Tier1) > NetworkPolicy(Tier2) > BaselineAdminNetworkPolicy (Tier3).

Sample Pass ACTION API:

```
apiVersion: policy.networking.k8s.io/v1alpha1
kind: AdminNetworkPolicy
metadata:
  name: pass-example
spec:
  priority: 10
  subject:
    namespaces:
      matchLabels:
          conformance-house: gryffindor
  ingress:
  - name: "pass-all-ingress-from-slytherin"
    action: "Pass"
    from:
    - namespaces:
        namespaceSelector:
          matchLabels:
            conformance-house: slytherin
  egress:
  - name: "pass-all-egress-to-slytherin"
    action: "Pass"
    to:
    - namespaces:
        namespaceSelector:
          matchLabels:
            conformance-house: slytherin
```

Corresponding ACLs:

```
_uuid               : 2e0f811f-e0db-41ad-b12b-4a0cf1c621ae
action              : pass
direction           : to-lport
external_ids        : {direction=ANPIngress, "k8s.ovn.org/id"="admin-network-policy-controller:AdminNetworkPolicy:pass-example:ANPIngress:29000", "k8s.ovn.org/name"=pass-example, "k8s.ovn.org/owner-controller"=admin-network-policy-controller, "k8s.ovn.org/owner-type"=AdminNetworkPolicy, priority="29000"}
label               : 0
log                 : false
match               : "((ip4.src == $a10282262890368313763)) && (outport == @a3052488126344707991)"
meter               : acl-logging
name                : pass-example_ANPIngress_29000
options             : {}
priority            : 29000
severity            : debug
tier                : 1
===
_uuid               : 120082fa-5a70-4c72-9211-529766078278
action              : pass
direction           : from-lport
external_ids        : {direction=ANPEgress, "k8s.ovn.org/id"="admin-network-policy-controller:AdminNetworkPolicy:pass-example:ANPEgress:29000", "k8s.ovn.org/name"=pass-example, "k8s.ovn.org/owner-controller"=admin-network-policy-controller, "k8s.ovn.org/owner-type"=AdminNetworkPolicy, priority="29000"}
label               : 0
log                 : false
match               : "((ip4.dst == $a16426961577074298037)) && (inport == @a3052488126344707991)"
meter               : acl-logging
name                : pass-example_ANPEgress_29000
options             : {apply-after-lb="true"}
priority            : 29000
severity            : debug
tier                : 1
```

If we now define a networkpolicy that matches the same set of subjects as our pass-example admin-network-policy, that network-policy will take effect. This is how administrators can delegate a decision making to the namespace owners in a cluster.

# BaselineAdminNetworkPolicy

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

# TODO

This section tracks the remaining work (some of these items are work-in-progress already and will be merged in future PRs) that are future items and outside the scope of the initial PR (https://github.com/ovn-org/ovn-kubernetes/pull/3659)

* Adding Northbound Support for ANP: https://github.com/kubernetes-sigs/network-policy-api/pull/117
* Adding support for sameLabels/notSameLabels: https://github.com/kubernetes-sigs/network-policy-api/pull/123
* Adding support for Named Ports: https://github.com/ovn-org/ovn-kubernetes/pull/3641 (Once the final design here is done will rebase)
* Adding support for Logging: (PR in progress locally, did not push till these main changes land)
    * Change to using ovn.acl package for bulding ACLs instead of libovsdb.ACL package: per comment https://github.com/ovn-org/ovn-kubernetes/pull/3659#discussion_r1257988920 if needed (although tssurya thinks using the libovsdbops function causes lesser abstracted and more straightforwardness)
* Scale improvements (We will only have max 100 ANP's in a cluster, so we could get away by not doing any scale changes; depends on how pod/namespace add/updates perform.)
    * Reducing ACLs on L4 (Max ACL Count: 100x200 = 20K without ports) - with ports this can go upto 100x200x100 = 200K ACLs: https://github.com/ovn-org/ovn-kubernetes/pull/3582
    * Investigating better locking (if needed after scale runs)
    * Adding support for sharing address-sets with namespaces (depends on use cases as ANPs
      are created before namespaces most of times and the adiquate support on the namespace
      controller side needs to be added) - similar to what's done for NPs
    * Adding support for sharing port-groups (depends on use cases as ANPs span
      across namespaces maybe we can combine per namespace ones with an || expression
      but need to see if its worth the effort): https://github.com/ovn-org/ovn-kubernetes/pull/2740

# Constraints

* The v1alpha1 CRDs upstream support upto 1000 priorities (`.Spec.Priority`) but OVNK only allows users to have maximum 100 ANPs in a cluster.
  This means you can create an ANP with priority between 0 and 99 - we do not support creating ANPs with higher priorities in OVNK.
  Since each ANP can have 100 ingress and egress rules, administrators must be able to express relations using 30-50 policies max from our assumptions.
  Changing this to support beyond 200 will need OVN RFEs
* It is for the best if two ANPs are not created with the same priority. The outcome is nondeterministic and this is a case we do not support. So ensure
  your policies have unique priorities
