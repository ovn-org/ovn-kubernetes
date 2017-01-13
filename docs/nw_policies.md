Kubernetes Network Policies Support
===================================

Support is disabled by default, and should be enabled launching the watcher
`--watch-policies` option. When launched with this option the watcher process
will monitor Kubernetes namespaces, and for each namespace will monitor
network policies.

Network policy OVN integration has been tested against Kubernetes v1.4, where
network policies are a beta API. The integration will not work with Kubernetes
v1.3, where network policies where in alpha state and implemented via
ThirdPartyResources.

The watcher process will only implement network policies on those namespaces
whose default isolation behavior is set to __DefaultDeny__. Any other value
for the namespace isolation annotation will be ignored, and the namespace
will be treated as non-isolated.

The isolation annotation for an isolated namespace should have the following
value:

```
net.beta.kubernetes.io/network-policy: |
    {
      "ingress": {
        "isolation": "DefaultDeny"
      }
    }
```

It is also worth noting that:

- The value of the property must be in JSON format. The Kubernetes API
  server expects a string and will not perform any JSON validation.
  The watcher processes will however only try to parse this string as
  JSON. Any failure in parsing will result in the namespace being
  considered non-isolated
- As the server only expects a string, this value should be passed as such
  when annotating with kubectl, and quotes must be properly escaped. More
  information are available on the [Kubernetes docs page for network
  policies](http://kubernetes.io/docs/user-guide/networkpolicies/#
  configuring-namespace-isolation-policy)

Theory of operation
--------------------

This section briefly discusses how the network policy integration for OVN
operates with regards to pod, namespace, and policy events.

Policy workflow for pod operations
-----------------------------------

Network policies are enforced via OVN ACLs. The ACLs are generated and enforced
before the pod is started. ACLs are generated only for network policies whose pod
selector matches the one being started. A distinct ACL is generated for each
rule in the network policy object. In particular:
- Port and protocols in the 'ports' clause for a policy are converted into
  `tcp.dst` and `udp.dst` expressions in the match rule;
- The IP addresses for pods matched by pod or namespace selector are place in
  a unique address set for the rule. The address set is then referenced in the
  `ip.src` expression of the match rule;
- The pod's logical port is always part of the ACL match; therefore each ACL
  always applies to a single pod.
- Every ACL has the same priority, which is anyway higher than the baseline
  'drop' ACL which is added for every pod created in isolated namespaces.

Network Policies are implemented synchronously with creation of the pod's
logical port. When the CNI plugin receives network information for a given
pod, the watcher process already programmed ACLs corresponding to network
policies for that pod.

Upon pod deletion, every ACL configured for the pod is destroyed, and the
pod's IP address removed from the policy's address sets.

Pod labels affect policy rules' from clauses. Whenever a change is detected
in pod labels, address sets for policies might need to be recalculated. The
current logic - inefficiently - recalculates all address sets every time a
change in pod labels is detected.

_Note:_ The current integration does not recalculate address sets upon
namespace events, which might affect policy rules' namespace selectors.
This is a limitation that will be addressed in the near future.

Policy workflow for namespace operations
-----------------------------------------

The workflow above applies - obviously - only if the namespace where the pod
is being created is isolated. If the pod is instead created in a
non-isolated namespace, an ACL for explicitly white listing traffic directed
to that pod is created.

Whenever namespace isolation is switched on or off, ACLs for every pod in
the namespace are recalculated. This process is however not synchronous with
the namespace operation, as that operation simply changes the value of
the namespace isolation annotation on the Kubernetes API server.

_Note_: Unfortunately Kubernetes (as of version 1.4) does not offer any
facility to assess whether policy processing has been completed or not, and
in the first case whether it completed successfully or not.


Workflow for policy operations
-------------------------------

Network policies rules are translated in 'pseudo-ACL', which are simply
in-memory data structures that contain abstract data for creating match rules
for OVN ACLs.
When a policy is created, its pod selector is analyzed to check for which pods
the policy must be enforced. Then pseudo-ACLs for those policies are
translated into actual ACLs building a match rule which also includes the OVN
logical port for the pod in the match expression.

When a network policy is deleted, all of its address sets are destroyed,
together with the ACLs for all pods that match the policy pod selector.

_Note_: As policy objects are accessed only through a namespace scope, the
watcher process maintains a distinct watcher thread for every namespace.
For this reason the logs will show a creation of a distinct network policy
watcher for every namespace in the Kubernetes cluster.

Implementation details
----------------------

- The Kubernetes API only allows for retrieving network policies within the
  scope of a given namespace. Unlike pods or services, there is no API for
  retrieving policies for every namespace. For this reason a distinct watcher
  thread is started for each namespace. This is performed by the policy
  processor thread when handling the namespace ADDED event. The same policy
  processor also destroys the policy watcher when then namespace is deleted.
- The policy processor mentioned above processes events generated from pods,
  namespace, and network policy instances and ensures they are translated in
  to the appropriate OVN ACLs. In particular, the policy processor has the
  following responsibilities:
  - ensuring traffic is white listed for pods in non-isolated namespaces
  - ensuring a default 'drop' ACL is added for each pod in an isolated
    namespace
  - translating Kubernetes network policies into a set of 'pseudo-ACLs',
    an intermediate representation for an OVN ACL, which essentially is just
    a tuple describing a rule's priority, the target ports, the source IPs,
    and the desired action (so far that would always be 'allow-related')
  - determining which policies apply to a pod and translating pseudo-ACLs
    into actual ACLs upon pod creation
  - removing all ACLs for a pod upon pod deletion
  - creating and maintaining OVN address sets for the IP addresses of pods
    that match the from clause of network policies rules
  - re-calculate ACLs upon transitions in the namespace isolation property
  - starting network policy watchers for every namespace, regardless of their
    isolation status.
- When a pod gets created, the following things happen:
  1. For each network policy check whether its pod selector matches the pod.
     If it does, or if the network policy has an empty pod selector,
     add ingress control ACLs based on the rules in that network policy object.
  2. For each network policy, verify whether this pod matches any 'from'
     clause in the policy rules. If it does match, then this pod IP address
     must be added to address sets for matching rules.
