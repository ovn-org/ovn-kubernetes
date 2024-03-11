# KubeVirt virtual machine bridge binding pod network seamless live migration


## Introduction

At kubevirt every VM is executed inside a "virt-launcher" pod; how the "virt-launcher" pod net interface is "bound" to the VM is specified by the network binding at the VM spec, ovn-kubernetes support live migration of the KubeVirt network bridge binding. 

The KubeVirt bridge binding adds  a bridge and a tap device and connects the existing pod interface as a port of the aforementioned bridge. When IPAM is configured on the pod interface, a DHCP server is started to advertise the pod's IP to DHCP aware VMs.

KubeVirt doesn't replace anything. It adds a bridge and a tap device, and connects the existing pod interface as a port of the aforementioned bridge.

Benefit of the bridge binding is that is able to expose the pod IP to the VM as expected by most users also pod network bridge binding live migration is implemented by ovn-kubernetes by doing the following:
- Do IP assignemnt with ovn-k but skip the CNI part that configure it at pod's netns veth.
- Do not expect the IP to be on the pod netns.
- Add the ability at ovn-k controllers to migrate pod's IP from node to node.


## Implementation 

To implement live migration ovn-kubernetes do the following:
- Send DHCP replies advertising the allocated IP address to the guest VM (via OVN-Kubernetes DHCP options configured for the logical switch ports).
- A point to point routing is used so one node's subnet IP can be routed from different node
- The VM's gateway IP and MAC are independent of the node they are running on using proxy arp

**Point to point routing:**

When the VM is running at a node that does not "own" it's IP address - e.g. after a live migration - 
after a live migration, do point to point routing with a policy for non 
interconnect and a cluster wide static route for interconnected zones, it will
route outbound traffic and a static route to route inboud.
By doing this, the VM can live migrate to different node and keep previous 
addresses (IP / MAC), thus preserving n/s and e/w communication, and ensuring traffic
goes over the node where the VM is running. The latter reduces inter node communication.

If the VM is going back to the node that "owns" the ip, those static routes and 
policies should be reconciled (deleted).

```text
       ┌───────────────────────────┐
       │     ovn_cluster_router    │
┌───────────────────────────┐   ┌────────────────────────────┐
│ static route (ingress all)│───│ policy  (egress n/s)       │
│ prefix: 10.244.0.8        │   │ match: ip4.src==10.244.0.8 │
│ nexthop: 10.244.0.8       │   │ action: reroute            │
│ output-port: rtos-node1   │   │ nexthop: 10.64.0.2         │
│ policy: dst-ip            │   │                            │  
└───────────────────────────┘   └────────────────────────────┘ 
- static route: ingress traffic to VM's IP via VM's node port
- policy: egress n/s traffic from VM's IP via gateway router ip
```

When ovn-kubernetes is deployed with multiple ovn zones interconected 
an extra static route is added to the local zone (zone where vm is running) to
route egress n/s traffic to the gw router. Note that for interconnect the
previous policy is not needed, at remote zones a static route is address to 
enroute VM egress traffic over the transit switch port address.

If the transit switch port address is 10.64.0.3 and gw router is 10.64.0.2, 
those below are the static route added to the topology when running with 
multiple zones:

```text
Local zone:
       ┌───────────────────────────┐
       │     ovn_cluster_router    │
┌───────────────────────────┐   ┌───────────────────────────┐
│ static route (egress n/s) │───│ static route (ingress all)│
│ prefix: 10.244.0.0/16     │   │ prefix: 10.244.0.8        │
│ nexthop: 10.64.0.2        │   │ nexthop: 10.244.0.8       │
│ policy: src-ip            │   │ output-port: rtos-node1   │
└───────────────────────────┘   │ policy: dst-ip            │
                                └───────────────────────────┘
Remote zone:
       ┌───────────────────────────┐
       │     ovn_cluster_router    │
┌───────────────────────────┐      │
│ static route (ingress all)│──────┘
│ prefix: 10.244.0.8        │
│ nexthop: 10.64.0.3        │
│ policy: dst-ip            │
└───────────────────────────┘
```


**Nodes logical switch ports:**

To have a consistent gateway at VMs (keep ip and mac after live migration) 
the "arp_proxy" feature is used and it need to be activated at 
the logical switch port of type router connects node's logical switch to 
ovn_cluster_router logical router.

The "arp_proxy" LSP option will include the MAC to answer ARPs with, and the
link local ipv4 and ipv6 to answer for and the cluster wide pod CIDR to answer
to pod subnets when the node switch do not have the live migrated ip. The
flows from arp_proxy has less priority than the ones from the node logical 
switch so ARP flows are not overriden.

```text
    ┌────────────────────┐   ┌────────────────────┐
    │logical switch node1│   │logical switch node2│
    └────────────────────┘   └────────────────────┘
┌────────────────────┐  │     │   ┌─────────────────────┐
│ lsp stor-node1     │──┘     └───│ lsp stor-node2      │
│ options:           │            │ options:            │
│  arp_proxy:        │            │   arp_proxy:        │
│   0a:58:0a:f3:00:00│            │    0a:58:0a:f3:00:00│
│   169.254.1.1      │            │    169.254.1.1      │
│   10.244.0.0/16    │            │    10.244.0.0/16    │
└────────────────────┘            └─────────────────────┘
```

**VMs logical switch ports:**

The logical switch port for new VMs will use ovn-k ipam to reserve an IP address,
and when live-migrating the VM, the LSP address will be re-used.

CNI must avoid setting the IP address on the migration destination pod, but ovn-k controllers should 
preserve the IP allocation, that is done when ovn-k detects that the pod is
live migratable.

Also the DHCP options will be configured to deliver the address to the VMs

```text
    ┌──────────────────────┐   ┌────────────────────┐
    │logical switch node1  │   │logical switch node2│
    └──────────────────────┘   └────────────────────┘
┌──────────────────────┐  │     │   ┌──────────────────────┐      
│ lsp ns-virt-launcher1│──┘     └───│ lsp ns-virt-launcher2│     
│ dhcpv4_options: 1234 │            │ dhcpv4_options: 1234 │
│ address:             │            │ address:             │    
│  0a:58:0a:f4:00:01   │            │  0a:58:0a:f4:00:01   │
│  10.244.0.8          │            │  10.244.0.8          │
└──────────────────────┘            └──────────────────────┘

┌─────────────────────────────────┐
│ dhcp-options 1234               │
│   lease_time: 3500              │
│   router: 169.254.1.1           │
│   dns_server: [kubedns]         │
│   server_id: 169.254.1.1        │
│   server_mac: c0:ff:ee:00:00:01 │
└─────────────────────────────────┘
```

**Virt-launcher pod address:**
The CNI will not set an address at virt-launcher pod netns, that address is
assigned to the VM with the DHCP options from the LSP, this allows to use 
kubevirt bridge binding with pod networking and still do live migration.

#### IPAM

The point to point routing feature allows an address to be running at a node different from
the one "owning" the subnet the address is coming from. This will happen
after VM live migration.

One scenario for live migration is to shut down the node the VMs were migrated
from, this means that the IPAM node subnet should go back to the pool but since
the migrated VMs contains IPs from it those IPs should reserved in case the 
subnet is assigned to a new node.

On that case before assigning the subnet to the node the VMs ips need to be 
reserved so they don't get assigned to new pods

Another scenario is ovn-kubernetes pods restarting after live migration, on 
that case ovnkube-master should discover to what IP pool the VM belongs.


#### Detecting migratable VMs and changing point to point routes

To detect that a pod is migratable KubevirtVm the annotation `kubevirt.io/allow-pod-bridge-network-live-migration`
has to be present and also the label `vm.kubevirt.io/name=vm1`, on that case
the point to point routes will be created after live migration and also a 
DHCPOptions will be configured to serve the ip configuration to the VM

During live migration there are two virt-launcher pods with different names for the same VM (source and target). The ovn-k pod controller uses `CreationTimestamp` and `kubevirt.io/vm=vm1`
to differentiate between them; it then watches the target pod `kubevirt.io/nodeName` and `kubevirt.io/migration-target-start-timestamp` annotation and label, with the following intent:
- The `kubevirt.io/nodeName` is set after the VM finishes live migrating or when it becomes ready.
- The `kubevirt.io/migration-target-start-timestamp` is set when live migration has not finished but migration-target pod is ready to receive traffic (this happens at post-copy live migration, where migration is taking too long).

The point to point routing cleanup (remove of static routes and policies) will be done at this cases:
- VM is deleted, all the routing related to the VM is removed at all the ovn zones.
- VM is live migrated back to the node that owns its IP, all the routing related to the VM is removed at all the ovn zones.
- ovn-kubernetes controllers are restarted, stale routing is removed.
