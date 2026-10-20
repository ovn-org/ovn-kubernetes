# OVN-Kubernetes Network Topology

OVN-Kubernetes uses interconnect mode, a distributed control plane
architecture. The network topology is distributed across zones; each
node is a zone of its own.

## OVN-Kubernetes Network Topology - Distributed (Interconnect)

The interconnect architecture in OVN-K looks like this today
(each node is a zone of its own):

![ovn-kubernetes-distributed-topology](../images/distributed-network-topology.png)

On each node we have:

* node-local-switch: all the logical switch ports for the pods
created on a node are bound to this switch and it also hosts
load balancers that take care of DNAT-ing the service traffic
* ovn-cluster-router: it's responsible for routing traffic between
the node switch and the gateway router (since each zone is a single
node it behaves like a local router; there is no need for a
distributed setup)
* join-switch: connects the ovn-cluster-router to the
gateway router (since each zone is a single node it behaves like a
local switch; there is no need for a distributed setup)
* node-local-gateway-router: it's responsible for north-south
traffic routing and connects the join switch to the external
switch and it also hosts load balancers that take care of DNAT-ing
the service traffic
* node-local-external-switch: connects the gateway router to the
external bridge
* transit-switch: distributed across the nodes in the cluster and responsible
for routing traffic between the different zones.

<!-- FIXME: This page is lazily written, there is so much more to do here. -->
