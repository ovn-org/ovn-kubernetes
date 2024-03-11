# External IP and LoadBalancer Ingress

OVN Kubernetes implements both External IPs and LoadBalancer Ingress IPs (`service.Status.LoadBalancer.Ingress`) in the form of OVN load balancers. These OVN load balancers live on all of the Kubernetes nodes and are thus highly available and ready for load sharing. It is the administrator's responsibility to route traffic to the Kubernetes nodes for both of these VIP types. 

In an environment where External IPs and LoadBalancer Ingress VIPs happen to be part of the nodes' subnets, administrators might expect that OVN Kubernetes answer to ARP requests to these VIPs. However, this is not the case. The administrator is responsible for routing packets to the Kubernetes nodes and they cannot rely on the network plugin for this to happen.

#### External IP

The Kubernetes documentation states that External IPs are to be handled by the cluster administrator and are not the responsibility of the network plugin. See the following quote from the kubernetes documentation:
~~~
External IPs

If there are external IPs that route to one or more cluster nodes, Kubernetes Services can be exposed on those externalIPs. Traffic that ingresses into the cluster with the external IP (as destination IP), on the Service port, will be routed to one of the Service endpoints. externalIPs are not managed by Kubernetes and are the responsibility of the cluster administrator.
~~~
> Source: [https://kubernetes.io/docs/concepts/services-networking/service/#external-ips](https://kubernetes.io/docs/concepts/services-networking/service/#external-ips)

#### LoadBalancer Ingress (service.Status.LoadBalancer.Ingress)

For a service's Status LoadBalancer Ingress field `service.Status.LoadBalancer.Ingress`, the aforementioned statement applies in exactly the same manner. Both External IP and `service.Status.LoadBalancer.Ingress` should behave the same from the network plugin's behavior, and it is the administrator's responsibility to get traffic for the VIPs into the cluster. 

#### Implementation details

OVN Kubernetes exposes External IPs and `service.Status.LoadBalancer.Ingress` VIPs as OVN load balancers on every node in the cluster. However, OVN Kubernetes will not answer to ARP requests to these VIP types, even if they reside on a node local subnet. This is because otherwise, every node in the cluster would answer with its own ARP reply to the same ARP request, leading to potential issues with stateful network flows that are tracked by conntrack. See the discussion in [https://github.com/ovn-org/ovn-kubernetes/issues/2407](https://github.com/ovn-org/ovn-kubernetes/issues/2407) for further details.

In fact, OVN Kubernetes implements explicit bypass rules for ARP requests to these VIP types on the external bridge (`br-ex` or `breth0` in most deployments). Any ARP request to such an IP that comes in from the physical port will bypass the OVN dataplane and it will be sent to host's networking stack on purpose. If an ARP reponse to a VIP is expeced, make sure the VIP is added to the host's networking stack.

For implementation details, see: 
* [https://github.com/ovn-org/ovn-kubernetes/blob/00925a6c64f57f03b2918eb48ff589c3417ddaa9/go-controller/pkg/node/gateway_shared_intf.go#L336](https://github.com/ovn-org/ovn-kubernetes/blob/00925a6c64f57f03b2918eb48ff589c3417ddaa9/go-controller/pkg/node/gateway_shared_intf.go#L336)
* [https://github.com/ovn-org/ovn-kubernetes/blob/00925a6c64f57f03b2918eb48ff589c3417ddaa9/go-controller/pkg/node/gateway_shared_intf.go#L344](https://github.com/ovn-org/ovn-kubernetes/blob/00925a6c64f57f03b2918eb48ff589c3417ddaa9/go-controller/pkg/node/gateway_shared_intf.go#L344)
* [https://github.com/ovn-org/ovn-kubernetes/pull/2540](https://github.com/ovn-org/ovn-kubernetes/pull/2540)
* [https://github.com/ovn-org/ovn-kubernetes/pull/2394](https://github.com/ovn-org/ovn-kubernetes/pull/2394)

#### Guidance for administrators

This absence of ARP replies from OVN Kubernetes means that administrators must take extra actions to make External IPs and LoadBalancer Ingress VIPs work, even when these VIPs reside on one of the node local subnets.

For External IPs, administrators can either assign the External IP to one of the nodes' Linux networking stacks if the External IP falls into one of the node's subnets. In this case, ARP requests to the External IP will be answered with ARP replies by the node that was assigned the External IP. For example, an admin could run `ip address add <externalIP>/32 dev lo` to make this work, assuming that `arp_ignore` is at its default setting of `0` and thus the Linux networking stack uses the default [weak host model](https://en.wikipedia.org/wiki/Host_model) for ARP replies. An alternative could be to point one or multiple static routes for the External IP to one or several of the Kubernetes nodes. 

For LoadBalancer Ingress VIPs, an administrator will either use a tool such as MetalLB L2 mode. Or, they can configure ECMP load-sharing. ECMP load-sharing can be implemented via static routes which point to all Kubernetes nodes or via BGP route injection (e.g., MetalLB's BGP mode).
