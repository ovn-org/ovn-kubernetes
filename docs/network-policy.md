# NetworkPolicy

Kubernetes NetworkPolicy documentation: https://kubernetes.io/docs/concepts/services-networking/network-policies

Kubernetes NetworkPolicy API reference: https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.19/#networkpolicy-v1-networking-k8s-io

By default the network traffic from and to K8s pods is not restricted in any way. Using NetworkPolicy is a way to enforce network isolation of selected pods. When a pod is selected by a NetworkPolicy allowed traffic is specified by the `Ingress` and `Egress` sections.  

Each NetworkPolicy object consists of four sections:

1. `podSelector`: a label selector that determines which pods the NetworkPolicy applies to
2. `policyTypes`: determines which policy types are included, if none are selected then `Ingress` will always be set and `Egress` will be set if any `Egress` rules are applied 
3. `Ingress rules`: determines the sources that pods selected by the ingress rule can receive traffic from 
4. `Egress rules`: determines the sinks that pods selected by the egress rule can send trafic to 

# NetworkPolicy features 

These are described in order and are additive 

## **Unicast default-deny**

When a pod is selected by one or more NetworkPolicies, the `policyTypes` is set to both `Ingress` and `Egress`, and if no rules are specified it becomes isolated and all unicast ingress and egress traffic is blocked for pods in the same namespce as the NetworkPolicy. 

```
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-all
spec:
  podSelector: {}
  policyTypes:
  - Ingress
  - Egress
```

If only ingress traffic to all pods in a namespace needs to be blocked the following can be used 

```
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-all
spec:
  podSelector: {}
  policyTypes:
  - Ingress
```

And finally if only Egress traffic from all pods in a Namespace needs to be blocked 

```
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-all
spec:
  podSelector: {}
  policyTypes:
  - Egress
```
**OVN-Implementation:**

Every new NetworkPolicy creates a port group named `<namespace>_<policyName>`.  All pods that the policy's `podSelector` selects are added to the port group.

Additionally, two deny PortGroups are created per-namespace, specifically: `<hashedNamespace>_IngressDefaultDeny` and `<hashedNamespace>_EgressDefaultDeny`.  Any pod selected by any NetworkPolicy in a Namespace is added to these PortGroups.

subset of `ovn-nbctl find port-group`
```
_uuid               : bfb3a7a9-fb30-4b94-afc5-8c28345cb8f4
acls                : [10f1baf9-2b4f-4947-9597-8e7159932977, 25614d28-0138-40c4-b331-2098a29042b9]
external_ids        : {name=a16982411286042166782_egressDefaultDeny}
name                : a16982411286042166782_egressDefaultDeny
ports               : []

_uuid               : dba665da-fb88-407c-837c-8b754ded2415
acls                : [825f2270-3873-4478-aecb-8f28f49a6b92, 843fa1b7-8d59-4b2c-9a92-a4e7113492bf]
external_ids        : {name=a16982411286042166782_ingressDefaultDeny}
name                : a16982411286042166782_ingressDefaultDeny
ports               : []
```

Two ACLs (four total) are added to each DefaultDeny PortGroup:

1. a drop policy with `priority=9100`, `direction=to-lport`, and `name=<namespace>_<e/in>gressDefaultDeny`

subset of `ovn-nbctl find ACL` 
```
_uuid               : 825f2270-3873-4478-aecb-8f28f49a6b92
action              : drop
direction           : to-lport
external_ids        : {default-deny-policy-type=Ingress}
label               : 0
log                 : false
match               : "outport == @a16982411286042166782_ingressDefaultDeny"
meter               : acl-logging
name                : default_ingressDefaultDeny
options             : {}
priority            : 9100
severity            : info
```

2. an allow policy for ARP traffic with `priority=9101`, `direction=to-lport`, `match=arp`, and `name=<namespace>_ARPallowPolicy`

subset of `ovn-nbctl find ACL` 
```
_uuid               : 843fa1b7-8d59-4b2c-9a92-a4e7113492bf
action              : allow
direction           : to-lport
external_ids        : {default-deny-policy-type=Ingress}
label               : 0
log                 : false
match               : "outport == @a16982411286042166782_ingressDefaultDeny && (arp || nd)"
meter               : acl-logging
name                : default_ARPallowPolicy
options             : {}
priority            : 9101
severity            : info
```

## **Applying the network policy to specific pods using `spec.podSelector`**

In some cases only certain pods in a Namespace may need to be selected by a NetworkPolicy. To handle this feature the `spec.podSelector` field can be used as follows 

The `spec.podSelector` is a label selector, which can be either a list of labels (`app=nginx`) or a match expression. Only pods in the same Namespace as the NetworkPolicy can be selected by it. The end result is a list of zero or more pods to which this NetworkPolicy's `Ingress` and `Egress` sections will be applied.

For example, to block all traffic to and from a pod labeled with `app=demo` the following can be used 

```
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-all
spec:
  podSelector:
    matchLabels:
          app: demo
  policyTypes:
  - Ingress
  - Egress
```

## **Applying Ingress and Egress Rules using `spec.Ingress` and `spec.Egress`** 

In some cases we need to explicilty define what sources and sinks a pod is allowed to communicate with, to handle this feature the `spec.Ingress` and `spec.Egress` fields of a NeworkPolicy can be used 

These sections contain a list of ingress or egress "peers" (the `from` section for `ingress` and the `to` section for `egress`) and a list of IP ports/protocols (the `ports` section) to or from which traffic should be allowed. Each list element of each section is logically OR-ed with other elements.

Each `from`/`to` section can contain the following selectors 

1. `namespaceSelector`: a label selector matching all pods in zero or more Namespaces
2. `podSelector`: a label selector matching zero or more pods in the same Namespace as the NetworkPolicy
3. `namespaceSelector` and `podSelector`: when both are present in an element, selects only pods matching the `podSelector` from Namespaces matching the `namespaceSelector`
4. `ipBlock`: an IP network in CIDR notation that can be either internal or external, with optional exceptions

### `spec.ingress`

Rules defined in `spec.Ingress` can match on two main sections, 1.`spec.Ingress.from` and 2.`spec.Ingress.ports`

1. `spec.Ingress.from` 

    Specifies FROM what sources a network policy will allow traffic 

    It contains three selectors which are described further below 

    - `spec.Ingress.from.ipBlock` 
        
        The ip addresses from which to allow traffic, contains fields `spec.Ingress.from.ipBlock.cidr` to specify which ip address are allowed and 
        `spec.Ingress.from.ipBlock.except` to specifiy which address's are not allowed 
    
    - `spec.Ingress.from.namespaceSelector` 

        The Namespaces from which to allow traffic, uses matchLabels to select much like the [`spec.Podselector` field](#**Applying-the-network-policy-to-specific-pods-using-`spec.podSelector`**)
    
    - `spec.Ingress.from.podSelector` 

        The pods from which to allow traffic, matches the same as described [above](#**Applying-the-network-policy-to-specific-pods-using-`spec.podSelector`**)
    
2. `spec.Ingress.ports`
    
    The ports that the `Ingress` rule matches on, contains fields `spec.Ingress.ports.port` which can be either numerical or named, if set all port names and numbers will be matched, and `spec.Ingress.ports.protocol` matches to the protocol of the provided port 



### `spec.egress` 

Rules defined in `spec.Egress` can match on two main sections, 1.`spec.Egress.to` and 2.`spec.Egress.ports`

1. `spec.Egress.to` 

    specifies TO what destinations a network policy will allow a pod to send traffic 

    It contains three selectors which are described further below 

    - `spec.Egress.to.ipBlock` 
        
        The ip addresses which a pod can send traffic to, contains fields `spec.Egress.from.ipBlock.cidr` to specify which ip address are allowed and 
        `spec.Egress.from.ipBlock.except` to specifiy which address's are not allowed
    
    - `spec.Egress.to.namespaceSelector` 

        The Namespaces allowed to receive traffic, uses matchLabels to select much like the [`spec.Podselector` field](#**Applying-the-network-policy-to-specific-pods-using-`spec.podSelector`**)
    
    - `spec.Egress.to.podSelector` 

        The pods allowed to receive traffic, uses matchLabels to select much like described [`spec.Podselector` field](#**Applying-the-network-policy-to-specific-pods-using-`spec.podSelector`**)
    
2. `spec.Egress.ports`

    The ports that the `Egress` rule matches on, contains fields `spec.Egress.ports.port` which can be either numerical or named, if set all port names and numbers will be matched, and `spec.Egress.ports.protocol` matches to the protocol of the provided port 

### `spec.ingress` and `spec.egress` OVN implementation 

  Each Namespace creates an AddressSet to which the IPs of all pods in that Namespace are added. This is used in NetworkPolicy `Ingress` and `Egress` sections to implement the Namespace selector.

  Each element in the `from` or `to` list results in an AddressSet containing the IP addresses of all peer pods(i.e all pods touched by this policy) As pods are created, updated, or deleted each AddressSet is updated to add the new pod if it matches the selectors, or to remove the pod if it used to match selectors but no longer does. Namespace label changes may also result in AddressSet updates to add or remove pods if the Namespace now matches or no longer matches the `namespaceSelector`.
  
  If an `ipBlock` is specified, an ACL with the label `ipblock_cidr="false"` is added to the policy's PortGroup with `priority=1001` that allows traffic to or from the list of CIDRs in the `ipBlock`, any exceptions are added as `drop` ACLs to the policy's PortGroup with `priority=1010`.

  **Examples:** 

  Given two pods in Namespace `default` called  `client1` and `client2` , and one pod in Mamespace `demo`, called `server` lets make a network policy that allows ingress traffic to the server from `client1` but bocks traffic from `client2` 

  Notice the pod `client1` and Namespace `default` are labeled with `app=demo`

```
[astoycos@localhost demo]$ kubectl get pods -o wide --show-labels --all-namespaces
NAMESPACE            NAME                                        READY   STATUS    RESTARTS   AGE   IP           NODE                NOMINATED NODE   READINESS GATES   LABELS
default              client1                                     1/1     Running   0          24s   10.244.1.6   ovn-worker          <none>           <none>            app=demo
default              client2                                     1/1     Running   0          24s   10.244.0.7   ovn-worker2         <none>           <none>            <none>
demo                 server                                      1/1     Running   0          24s   10.244.1.7   ovn-worker          <none>           <none>            <none>
```

```
[astoycos@localhost demo]$ kubectl get namespace --show-labels
NAME                 STATUS   AGE   LABELS
default              Active   40m   kubernetes.io/metadata.name=default
demo                 Active   10m   kubernetes.io/metadata.name=demo
```

  Before applying the following network policy both pods `client1` and `client2` can reach the `server` pod

``` 
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
 name: allow-from-client
spec:
 podSelector: {}
 policyTypes:
 - Ingress
 ingress:
 - from:
   - namespaceSelector:
       matchLabels:
         kubernetes.io/metadata.name: default
     podSelector:
       matchLabels:
         app: demo
```

  after applying this Network policy in Namespace `demo` only the pod `client1` can reach the `server` pod 

  NOTE: in the above definition there is only a single `from` element allowing connections from Pods labeled `app=demo` in Namespaces with the label `app=demo`

  if the from section was applied as follows 

```
- from:
   - namespaceSelector:
       matchLabels:
         kubernetes.io/metadata.name: demo
   - podSelector:
       matchLabels:
         app: demo
``` 
  Then there would be two elements in the `from` array which allows connections from Pods labeled `app=demo` **OR** and Pod from Namespaces with the label `app=demo`

  Now let's have a look at some of the OVN resources that are created along with this Network Policy 

  For all worker nodes we can see the UUIDs of the logical ports corresponding to the pods `client1`, `client2`, and `server` 

  **ovn-worker**

```
[root@ovn-control-plane ~]# ovn-nbctl lsp-list ovn-worker
97ebaf37-aa18-4f0c-aeb2-e2a22deb5222 (default_client1)
9f15cc4a-a6ff-47d8-97ae-ae3d1a229fc1 (demo_server)
```
  **ovn-worker2**

```
[root@ovn-control-plane ~]# ovn-nbctl lsp-list ovn-worker2
5114f0d8-3e2b-452a-a860-1bcc3f049107 (default_client2)
```

  **Port Groups** 

```
[root@ovn-control-plane ~]# ovn-nbctl find port-group
_uuid               : 4d41cdb1-3b42-4f6a-bc12-df9929f1807c
acls                : [6a4f872b-2878-42f0-8f43-141d75c36ea5, 8529af47-44c3-4cd5-b141-8c5ec05c9e8d]
external_ids        : {name=a11953709441258804118_ingressDefaultDeny}
name                : a11953709441258804118_ingressDefaultDeny
ports               : [9f15cc4a-a6ff-47d8-97ae-ae3d1a229fc1]

_uuid               : bd02a185-3c0d-4859-8667-056e9abe973a
acls                : [3a66eef8-295d-4927-95cf-202f4ac73fab]
external_ids        : {name=demo_allow-from-client}
name                : a13757631697825269621
ports               : [9f15cc4a-a6ff-47d8-97ae-ae3d1a229fc1]

_uuid               : d28d207f-3599-4493-a63e-fd6b9ae1d58c
acls                : [172625a3-fed8-4075-8d79-b1c79114bf56, 4bad07cf-9878-4dd9-8b1b-840a28f25db1]
external_ids        : {name=a11953709441258804118_egressDefaultDeny}
name                : a11953709441258804118_egressDefaultDeny
ports               : []

```

  Notice that the port corresponding to the pod `server` is included in the `ingressDefaultDeny` port group 

  To bypass the ingress default deny and allow traffic from pod `client1` in Namespace `demo` as specificed in the network policy, an address set is created containing the ip address for the pod `client1`  

  subset of `ovn-nbctl find address_set` 
```
_uuid               : 70fca3fd-4b38-4c09-b39c-8c1ac345f9ca
addresses           : ["10.244.1.6"]
external_ids        : {name=demo.allow-from-client.ingress.0_v4}
name                : a14783882619065065142
```

  Finally we can see the ingress ACL that allows traffic to the `server` pod by allowing `ip4.src` traffic **FROM** the address's in the address set `a14783882619065065142` **TO** the port group `@a13757631697825269621` which contains the port `c754a19d-1e8c-4415-99b9-66fdcdaed196` (corresponding to the `server`'s logical port)

  subset of `ovn-nbctl find ACL`
```
_uuid               : 3a66eef8-295d-4927-95cf-202f4ac73fab
action              : allow-related
direction           : to-lport
external_ids        : {Ingress_num="0", ipblock_cidr="false", l4Match=None, namespace=demo, policy=allow-from-client, policy_type=Ingress}
label               : 0
log                 : false
match               : "ip4.src == {$a14783882619065065142} && outport == @a13757631697825269621"
meter               : acl-logging
name                : demo_allow-from-client_0
options             : {}
priority            : 9101
severity            : info
```

TODO: Add more examples(good for first PRs), specifically replicate above scenario by matching on the pod's network(`ip_block`) rather than the pod itself 




