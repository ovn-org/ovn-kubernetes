# Multi-homing
A K8s pod with more than one network interface is said to be multi-homed. The
[Network Plumbing Working Group](https://github.com/k8snetworkplumbingwg/multi-net-spec)
has put forward a [standard](https://github.com/k8snetworkplumbingwg/multi-net-spec)
describing how to specify the configurations for additional network interfaces.

There are several delegating plugins or meta-plugins (Multus, Genie)
implementing this standard.

After a pod is scheduled on a particular Kubernetes node, kubelet will invoke
the delegating plugin to prepare the pod for networking. This meta-plugin will
then invoke the CNI responsible for setting up the pod's default cluster
network, and afterwards it iterates the list of additional attachments on the
pod, invoking the corresponding delegate CNI implementing the logic to attach
the pod to that particular network.

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
  namespace: ns1
spec:
  config: |2
    {
            "cniVersion": "0.3.1",
            "name": "l3-network",
            "type": "ovn-k8s-cni-overlay",
            "topology":"layer3",
            "subnets": "10.128.0.0/16/24",
            "mtu": 1300,
            "netAttachDefName": "ns1/l3-network"
    }
```

#### Network Configuration reference
- `name` (string, required): the name of the network.
- `type` (string, required): "ovn-k8s-cni-overlay".
- `topology` (string, required): "layer3".
- `subnets` (string, required): a comma separated list of subnets. When multiple subnets
  are provided, the user will get an IP from each subnet.
- `mtu` (integer, optional): explicitly set MTU to the specified value. Defaults to the value chosen by the kernel.
- `netAttachDefName` (string, required): must match <namespace>/<net-attach-def name>
  of the surrounding object.

**NOTE**
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
  name: l2-network
  namespace: ns1
spec:
  config: |2
    {
            "cniVersion": "0.3.1",
            "name": "l2-network",
            "type": "ovn-k8s-cni-overlay",
            "topology":"layer2",
            "subnets": "10.100.200.0/24",
            "mtu": 1300,
            "netAttachDefName": "ns1/l2-network"
    }
```

#### Network Configuration reference
- `name` (string, required): the name of the network.
- `type` (string, required): "ovn-k8s-cni-overlay".
- `topology` (string, required): "layer2".
  `subnets` (string, optional): a comma separated list of subnets. When multiple subnets
  are provided, the user will get an IP from each subnet.
- `mtu` (integer, optional): explicitly set MTU to the specified value. Defaults to the value chosen by the kernel.
- `netAttachDefName` (string, required): must match <namespace>/<net-attach-def name>
  of the surrounding object.
- `excludeSubnets` (string, optional): a comma separated list of CIDRs / IPs.
  These IPs will be removed from the assignable IP pool, and never handed over
  to the pods.

**NOTE**
- when the subnets attribute is omitted, the logical switch implementing the
  network will only provide layer 2 communication, and the users must configure
  IPs for the pods. Port security will only prevent MAC spoofing.

### Switched - localnet - topology
This topology interconnects the workloads via a cluster-wide logical switch to
a physical network.

The following net-attach-def configures the attachment to a localnet secondary
network.

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: localnet-network
  namespace: ns1
spec:
  config: |2
    {
            "cniVersion": "0.3.1",
            "name": "localnet-network",
            "type": "ovn-k8s-cni-overlay",
            "topology":"localnet",
            "subnets": "202.10.130.112/28",
            "vlanID": 33,
            "mtu": 1500,
            "netAttachDefName": "ns1/localnet-network"
    }
```

Note that in order to connect to the physical network, it is expected that
ovn-bridge-mappings is configured appropriately on the chassis for this
localnet network.

#### Network Configuration reference
- `name` (string, required): the name of the network.
- `type` (string, required): "ovn-k8s-cni-overlay".
- `topology` (string, required): "layer2".
  `subnets` (string, optional): a comma separated list of subnets. When multiple subnets
  are provided, the user will get an IP from each subnet.
- `mtu` (integer, optional): explicitly set MTU to the specified value. Defaults to the value chosen by the kernel.
- `netAttachDefName` (string, required): must match <namespace>/<net-attach-def name>
  of the surrounding object.
- `excludeSubnets` (string, optional): a comma separated list of CIDRs / IPs.
  These IPs will be removed from the assignable IP pool, and never handed over
  to the pods.
- `vlanID` (integer, optional): assign VLAN tag. Defaults to none.

**NOTE**
- when the subnets attribute is omitted, the logical switch implementing the
  network will only provide layer 2 communication, and the users must configure
  IPs for the pods. Port security will only prevent MAC spoofing.

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
    k8s.v1.cni.cncf.io/networks: l3-network,l2-network
  name: tinypod
  namespace: ns1
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
