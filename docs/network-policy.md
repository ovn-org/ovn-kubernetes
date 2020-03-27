# NetworkPolicy
Kubernetes NetworkPolicy documentation: https://kubernetes.io/docs/concepts/services-networking/network-policies/#behavior-of-to-and-from-selectors

Each NetworkPolicy object consists of three sections:

1. `podSelector`: a label selector that determines which pods the NetworkPolicy applies to
2. `ingress`: determines which entities ingress traffic can be received from by the pods selected by the NetworkPolicy
3. `egress`: determines which entities egress traffic can be sent to by pods selected by the NetworkPolicy

## Unicast default-deny
When a pod is selected by one or more NetworkPolicies it becomes isolated and all unicast ingress and egress traffic is blocked. Two global PortGroups are used to facilitate this behavior: `ingressDefaultDeny` and `egressDefaultDeny`.  Any pod selected by a NetworkPolicy in any namespace is added to these PortGroups.

Two ACLs (four total) are added to each PortGroup:

1. a drop policy with `priority=1000` and `direction=to-lport`
2. an allow policy for ARP traffic with `priority=1001`, `direction=to-lport`, and `match=arp`

## Multicast
All multicast traffic is blocked by default, and must be enabled on a per-Namespace basis using an annotation on that Namespace.

### default-deny
Multicast traffic is blocked by the `mcastPortGroupDeny` PortGroup, which all pods are added to. Two ACLs are added to the PortGroup:

1. a drop policy with `priority=1011`, `direction=from-lport`, and `match=ip4.mcast`
2. a drop policy with `priority=1011`, `direction=to-lport`, and `match=ip4.mcast`

#### allow
To allow multicast traffic for a given Namespace, a PortGroup is created for that namespace with the name `mcastPortGroup-FOO` where FOO is the Namespace's name. Each pod created in a Namespace is added to this PortGroup.

Two ACLs are added to the Namespace's multicast allow port group;

1. an allow policy with `priority=1012`, `direction=from-lport`, and `match=ip4.mcast`
2. an allow policy with `priority=1012`, `direction=to-lport`, and `match=ip4.mcast`

## Namespace AddressSet
Each Namespace creates an AddressSet to which the IPs of all pods in that Namespace are added. This is used in NetworkPolicy `ingress` and `egress` sections to implement the namespace selector.

## `podSelector`
The `podSelector` is a label selector, which can be either a list of labels (`app=nginx`) or a match expression. Only pods in the same namespace as the NetworkPolicy can be selected by it. The end result is a list of zero or more pods to which this NetworkPolicy's `ingress` and `egress` sections will be applied.

By default pods may send to an recipient and receive from any source. When a pod is selected by one or more NetworkPolicies it becomes isolated and all traffic is blocked. NetworkPolicy may then be used to allow received traffic from sources determined by the `ingress` section and/or send traffic to recipients specified by the `egress` section.

### NetworkPolicy PortGroup
Each NetworkPolicy creates a port group named `FOO_bar` where `FOO` is the policy's Namespace and `bar` is the policy's name.  All pods that the policy's `podSelector` selects are added to the port group.

## `ingress` and `egress` sections
These sections contain a list of ingress or egress "peers" (the `from` section for `ingress` and the `to` section for `egress`) and a list of IP ports/protocols (the `ports` section) to or from which traffic should be allowed. Each list element of each section is logically OR-ed with other elements.

### `from`/`to` sections
Peers are defined by:

1. `namespaceSelector`: a label selector matching all pods in zero or more Namespaces
2. `podSelector`: a label selector matching zero or more pods in the same namespace as the NetworkPolicy
3. `namespaceSelector` and `podSelector`: when both are present in an element, selects only pods matching the `podSelector` from namespaces matching the `namespaceSelector`
4. `ipBlock`: an IP network in CIDR notation, with optional exceptions

Each element in the `from` or `to` list results in an AddressSet containing the IP addresses of all peer pods. As pods are created, updated, or deleted each AddressSet is updated to add the new pod if it matches the selectors, or to remove the pod if it used to match selectors but no longer does. Namespace label changes may also result in AddressSet updates to add or remove pods if the namespace now matches or no longer matches the `namespaceSelector`.

### `ports` section
Each `ingress` and `egress` section contains a list of IP ports and protocols. These define which specific traffic should be allowed (or if no ports are specified all traffic) from the peers defined by the `from`/`to` sections.

### Generated ACLs
If there is an `ipBlock` peer specified, any exceptions are added as `drop` ACLs to the policy's PortGroup with `priorit=1010`.

#### Generated ACLs without `ports`
If an `ipBlock` is specified, an ACL is added to the policy's PortGroup with `priority=1001` that allows traffic to or from the list of CIDRs in the `ipBlock`, and matches

#### Generated ACLs with `ports`


