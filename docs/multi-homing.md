# Multi-homing
A K8s pod with more than one network interface is said to be multi-homed. The
[Network Plumbing Working Group](https://github.com/k8snetworkplumbingwg/multi-net-spec)
has put forward a [standard](https://github.com/k8snetworkplumbingwg/multi-net-spec)
describing how to specify the configurations for additional network interfaces.

There are several delegating plugins or meta-plugins (Multus, Genie) implementing this standard.

After a pod is scheduled on a particular Kubernetes node, kubelet will invoke
the delegating plugin to prepare the pod for networking. This meta-plugin will
then invoke the CNI responsible for setting up the pod's default cluster
network, and afterwards it iterates the list of additional attachments on the
pod (yes, these plugins **must** be able to access the Kubernetes API...),
invoking the corresponding delegate CNI implementing the logic to attach the pod
to that particular network.

## Configuring secondary networks
To allow pods to have multiple network interfaces, the user must provide the
configurations specifying how to connect to these networks; these
configurations are defined in a CRD named `NetworkAttachmentDefinition`.

Below you will find example attachment configurations for each of the current
topologies OVN-K allows for secondary networks.

**NOTE**: currently, all the secondary networks **only** allow for east/west
traffic.

### Routed - layer 3 - topology
This topology is a simplification of the topology for the cluster default
network - but without egress.

There is a logical switch per node - each with a different subnet - and a
router interconnecting all the logical switches.

The following net-attach-def configures the attachment to a routed secondary
network.

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: l3-network
  namespace: left-one
spec:
  config: |2
    {
            "cniVersion": "0.3.1",
            "name": "l3-network",
            "type": "ovn-k8s-cni-overlay",
            "topology":"layer3",
            "subnets": "10.128.0.0/16/24",
            "mtu": 1300,
            "netAttachDefName": "left-one/l3-network"
    }
```

**NOTE**
- the `netAttachDefName` parameter **must** be in <namespace>/<net-attach-def name> format
- the `subnets` attribute is a comma separated list of subnets.
- the `subnets` attribute indicates both the subnet across the cluster, and per node.
  The example above means you have a /16 subnet for the network, but each **node** has
  a /24 subnet.

### Switched - layer 2 - topology
This topology interconnects the workloads via a cluster-wide logical switch.

The following net-attach-def configures the attachment to a layer 2 secondary
network.

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: not-so-fast-network
  namespace: left-one
spec:
  config: |2
    {
            "cniVersion": "0.3.1",
            "name": "not-so-fast-network",
            "type": "ovn-k8s-cni-overlay",
            "topology":"layer2",
            "subnets": "10.128.0.0/24",
            "mtu": 1300,
            "netAttachDefName": "left-one/not-so-fast-network"
    }
```

## Pod configuration
The user must specify the secondary network attachments via the
`k8s.v1.cni.cncf.io/networks` annotation.

The following example provisions a pod with two secondary attachments, one for
each of the attachment configurations presented in
[Configuring secondary networks](#configuring-secondary-networks).

```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    k8s.v1.cni.cncf.io/networks: l3-network,not-so-fast-network
  name: tinypod
  namespace: left-one
spec:
  containers:
  - args:
    - pause
    image: k8s.gcr.io/e2e-test-images/agnhost:2.36
    imagePullPolicy: IfNotPresent
    name: agnhost-container
```
## Limitations
OVN-K currently does **not** support:
- the same attachment configured multiple times in the same pod - i.e.
  `k8s.v1.cni.cncf.io/networks: l3-network,l3-network` is invalid.
- updates to the network selection elements lists - i.e. `k8s.v1.cni.cncf.io/networks` annotation
