# NetworkPolicy

[Kubernetes NetworkPolicy documentation](https://kubernetes.io/docs/concepts/services-networking/network-policies)

[Kubernetes NetworkPolicy API reference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.19/#networkpolicy-v1-networking-k8s-io)

By default, the network traffic from and to K8s pods is not restricted in any way. Using NetworkPolicy is a way to enforce network isolation of selected pods. When a pod is selected by a NetworkPolicy allowed traffic is specified by the `Ingress` and `Egress` sections.  

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

Every new NetworkPolicy creates a port group named `FOO_bar` where `FOO` is the policy's Namespace and `bar` is the policy's name.  All pods that the policy's `podSelector` selects are added to the port group.

Additionally, two global deny PortGroups are also used, specifially: `IngressDefaultDeny` and `EgressDefaultDeny`.  Any pod selected by a NetworkPolicy in any Namespace is added to these PortGroups.

subset of `ovn-nbctl find port-group`
```
    _uuid               : 1deeac49-e87e-4e05-9324-beb8ef0dcef4
    acls                : [3f864884-cdb7-44be-a60e-e4f743afc9d0, f174fcf1-a7c2-496d-9b96-136aaccc014f]
    external_ids        : {name=ingressDefaultDeny}
    name                : ingressDefaultDeny
    ports               : [ce1bc4e5-0309-463f-9fd1-80f6e487e2d4]

    _uuid               : 5249b7a2-36bf-4246-98ea-13d5c5e17c68
    acls                : [36ed4154-71ab-4b8f-8119-5c4cc92708d9, c6663e29-99d0-4410-a2ff-f694a896a035]
    external_ids        : {name=egressDefaultDeny}
    name                : egressDefaultDeny
    ports               : []

```

Two ACLs (four total) are added to each PortGroup:

1. a drop policy with `priority=1000` and `direction=to-lport`

subset of `ovn-nbctl find ACL` 
```
   _uuid               : f174fcf1-a7c2-496d-9b96-136aaccc014f
    action              : drop
    direction           : to-lport
    external_ids        : {default-deny-policy-type=Ingress}
    log                 : false
    match               : "outport == @ingressDefaultDeny"
    meter               : []
    name                : []
    priority            : 1000
    severity            : []


```

2. an allow policy for ARP traffic with `priority=1001`, `direction=to-lport`, and `match=arp`

subset of `ovn-nbctl find ACL` 
```
_uuid               : 3f864884-cdb7-44be-a60e-e4f743afc9d0
action              : allow
direction           : to-lport
external_ids        : {default-deny-policy-type=Ingress}
log                 : false
match               : "outport == @ingressDefaultDeny && arp"
meter               : []
name                : []
priority            : 1001
severity            : []

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

          The Namespaces from which to allow traffic, uses matchLabels to select much like the [`spec.Podselector` field](#applying-the-network-policy-to-specific-pods-using-specpodselector)
    
    - `spec.Ingress.from.podSelector` 

        The pods from which to allow traffic, matches the same as described [above](#applying-the-network-policy-to-specific-pods-using-specpodselector)
    
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

        The Namespaces allowed to receive traffic, uses matchLabels to select much like the [`spec.Podselector` field](#applying-the-network-policy-to-specific-pods-using-specpodselector)
    
    - `spec.Egress.to.podSelector` 

        The pods allowed to receive traffic, uses matchLabels to select much like described [`spec.Podselector` field](#applying-the-network-policy-to-specific-pods-using-specpodselector)
    
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
  NAMESPACE            NAME                                        READY   STATUS    RESTARTS   AGE     IP           NODE                NOMINATED NODE   READINESS GATES   LABELS
  default              client1                                     1/1     Running   0          5m5s    10.244.2.5   ovn-worker          <none>           <none>            app=demo
  default              client2                                     1/1     Running   0          4m59s   10.244.1.4   ovn-worker2         <none>           <none>            <none>
  demo                 server                                      1/1     Running   0          42s     10.244.2.6   ovn-worker          <none>           <none>            <none>
  ```

  ```
  [astoycos@localhost demo]$ kubectl get namespace --show-labels
  NAME                 STATUS   AGE   LABELS
  default              Active   94m   ns=default
  demo                 Active   66m   <none>
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
            ns: default
        podSelector:
          matchLabels:
            app: demo
  ```

  after applying this Network policy in Namespace `demo` (`oc create -n demo -f policy.yaml`) only the pod `client1` can reach the `server` pod 

  NOTE: in the above definition there is only a single `from` element allowing connections from Pods labeled `app=demo` in Namespaces with the label `app=demo`

  if the from section was applied as follows 

  ```
  - from:
      - namespaceSelector:
          matchLabels:
            ns: demo
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
  edb290cf-b250-4699-b102-7acbb6300dc9 (default_client1)
  c754a19d-1e8c-4415-99b9-66fdcdaed196 (demo_server)
  24c789a2-fc4b-42a5-bb16-5b1c19490b50 (k8s-ovn-worker)
  484b2004-a5c1-447c-b857-eb8e524a73f3 (kube-system_coredns-f9fd979d6-qp6xd)
  45163af0-08c2-4d42-9fc7-7b0ddc935bd8 (local-path-storage_local-path-provisioner-78776bfc44-lgzdf)
  0bb378f1-4e89-46a8-a455-7a891f64c7c8 (stor-ovn-worker)
  ```
  **ovn-worker2**

  ```
  [root@ovn-control-plane ~]# ovn-nbctl lsp-list ovn-worker2
  d5030b96-1163-4ed0-90f8-41b3831d2a0b (default_client2)
  4da8fb64-0a43-4d76-a7ba-18941c078862 (k8s-ovn-worker2)
  37f58eeb-8c1a-437b-8b21-bcc1337b2e3f (kube-system_coredns-f9fd979d6-628xd)
  65019041-51b2-4599-913d-7f01c8eaa394 (stor-ovn-worker2)
  ```

  **Port Groups** 

  ```
  [root@ovn-control-plane ~]# ovn-nbctl find port-group
  _uuid               : 2b74086c-9986-4f4d-8c97-3388625230e9
  acls                : []
  external_ids        : {name=clusterPortGroup}
  name                : clusterPortGroup
  ports               : [24c789a2-fc4b-42a5-bb16-5b1c19490b50, 4da8fb64-0a43-4d76-a7ba-18941c078862, e556e329-d624-473a-8827-f022c17a8f60]

  _uuid               : a132ecce-dbca-4989-87f7-96e2f0b62a2c
  acls                : [b4e57f83-8b8f-4b37-b5e5-1f82704c49c4]
  external_ids        : {name=demo_allow-from-client}
  name                : a13757631697825269621
  ports               : [c754a19d-1e8c-4415-99b9-66fdcdaed196]

  _uuid               : a32d9dda-d7fb-4ae8-b6a9-3af17d62aa7f
  acls                : [510d797c-6302-4171-8a08-eeaab67063f4, f9079cce-29aa-4d1b-b36b-ca39933ad4e6]
  external_ids        : {name=ingressDefaultDeny}
  name                : ingressDefaultDeny
  ports               : [c754a19d-1e8c-4415-99b9-66fdcdaed196]

  _uuid               : 896a80ff-46f7-4837-a105-7b52cee0c625
  acls                : [660b10ea-0f2e-49cb-b620-ca4218e87ac6, 9bb634ff-cb69-44b6-a64d-09147cf337b5]
  external_ids        : {name=egressDefaultDeny}
  name                : egressDefaultDeny
  ports               : []
  ```

  Notice that the port corresponding to the pod `server` is included in the `ingressDefaultDeny` port group 

  To bypass the ingress default deny and allow traffic from pod `client1` in Namespace `demo` as specificed in the network policy, an address set is created containing the ip address for the pod `client1`  

  subset of `ovn-nbctl find address_set` 
  ```

  _uuid               : 7dc68ee9-9628-4a6a-83f0-a92bfa0970c6
  addresses           : ["10.244.2.5"]
  external_ids        : {name=demo.allow-from-client.ingress.0_v4}
  name                : a14783882619065065142

  ```

  Finally we can see the ingress ACL that allows traffic to the `server` pod by allowing `ip4.src` traffic **FROM** the address's in the address set `a14783882619065065142` **TO** the port group `@a13757631697825269621` which contains the port `c754a19d-1e8c-4415-99b9-66fdcdaed196` (corresponding to the `server`'s logical port)

  subset of `ovn-nbctl find ACL`
  ```

  _uuid               : b4e57f83-8b8f-4b37-b5e5-1f82704c49c4
  action              : allow-related
  direction           : to-lport
  external_ids        : {Ingress_num="0", ipblock_cidr="false", l4Match=None, namespace=demo, policy=allow-from-client, policy_type=Ingress}
  log                 : false
  match               : "ip4.src == {$a14783882619065065142} && outport == @a13757631697825269621"
  meter               : []
  name                : []
  priority            : 1001
  severity            : []

  ```

TODO: Add more examples(good for first PRs), specifically replicate above scenario by matching on the pod's network(`ip_block`) rather than the pod itself 




