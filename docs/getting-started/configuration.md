# Config Variables

Let us look at the supported configuration variables by OVN-Kubernetes

## Default Config

## Gateway Config

### Disable Forwarding Config

OVN-Kubernetes allows to enable or disable IP forwarding for all traffic on OVN-Kubernetes managed interfaces (such as br-ex).
By default forwarding is enabled and this allows host to forward traffic across OVN-Kubernetes managed interfaces.
If forwarding is disabled then Kubernetes related traffic is still forwarded appropriately, but other IP traffic will not be routed by cluster nodes.

IP forwarding is implemented at cluster node level by modifying both iptables `FORWARD` chain and IP forwarding `sysctl` parameters. 

#### IPv4

If forwarding is enabled(default) and it is desired to allow forwarding for traffic on unmanaged ovn-kubernetes interfaces, then system administrators need to set the following sysctl parameters on the desired interfaces or globally.
OVN-Kubernetes already sets sysctl forwarding for interfaces it manages, such as the ovn-k8s-mp0 interface and the shared gateway bridge interface. An operator can be built to manage forwarding sysctl parameters based on forwarding mode.
No extra iptables rules are added by OVN-Kubernetes to FORWARD chain while using this IP forwarding mode.

```
net.ipv4.ip_forward=1
```
IP forwarding can be disabled either by setting `disable-forwarding` command line option to `true` while starting ovnkube or by setting `disable-forwarding` to `true` in config file. If forwarding is disabled the default policy for the [FORWARD iptables chain is set as DROP](#forwarding-rules) and system administrators can add use-case specific ACCEPT rules.

When IP forwarding is disabled, following sysctl parameters are modified by OVN-Kubernetes to allow forwarding Kubernetes related traffic on OVN-Kubernetes managed bridge interfaces and management port interface.

```
net.ipv4.conf.br-ex.forwarding=1
net.ipv4.conf.ovn-k8s-mp0.forwarding = 1
```

#### IPv6

IP forwarding works differently for IPv6:
```
/proc/sys/net/ipv6/* Variables:

conf/all/forwarding - BOOLEAN
  Enable global IPv6 forwarding between all interfaces.
  IPv4 and IPv6 work differently here; e.g. netfilter must be used
  to control which interfaces may forward packets and which not.

...

conf/interface/*:

forwarding - INTEGER
	Configure interface-specific Host/Router behaviour.

	Note: It is recommended to have the same setting on all
	interfaces; mixed router/host scenarios are rather uncommon.

	Possible values are:
		0 Forwarding disabled
		1 Forwarding enabled

	FALSE (0):

	By default, Host behaviour is assumed.  This means:

	1. IsRouter flag is not set in Neighbour Advertisements.
	2. If accept_ra is TRUE (default), transmit Router
	   Solicitations.
	3. If accept_ra is TRUE (default), accept Router
	   Advertisements (and do autoconfiguration).
	4. If accept_redirects is TRUE (default), accept Redirects.

	TRUE (1):

	If local forwarding is enabled, Router behaviour is assumed.
	This means exactly the reverse from the above:

	1. IsRouter flag is set in Neighbour Advertisements.
	2. Router Solicitations are not sent unless accept_ra is 2.
	3. Router Advertisements are ignored unless accept_ra is 2.
	4. Redirects are ignored.

	Default: 0 (disabled) if global forwarding is disabled (default),
		 otherwise 1 (enabled).
```
https://www.kernel.org/doc/Documentation/networking/ip-sysctl.txt

It is not possible to configure the IPv6 forwarding per interface by setting the 
`net.ipv6.conf.IFNAME.forwarding` sysctl (it just configures the interface-specific Host/Router 
behaviour). \
Instead, the opposite approach is required where the global forwarding is always enabled and 
the traffic is restricted through iptables.

#### Forwarding rules

When the `disable-forwarding` parameter is configured specific iptables rules are added to
the FORWARD chain to forward clusterNetwork and serviceNetwork traffic to their intended destinations.
Additionally, the default policy for the FORWARD chain is set as `DROP`. Otherwise, the policy
defaults to `ACCEPT` and no custom rules are added. This behavior is the same for both IPv6 and IPv4
networks:

```
# In IPv4 with disable-forwarding=true the FORWARD policy is set to DROP
Chain FORWARD (policy DROP 0 packets, 0 bytes)
 pkts bytes target     prot opt in     out     source               destination         
    0     0 ACCEPT     0    --  *      *       0.0.0.0/0            169.254.169.1       
    0     0 ACCEPT     0    --  *      *       169.254.169.1        0.0.0.0/0           
    0     0 ACCEPT     0    --  *      *       0.0.0.0/0            10.96.0.0/16        
    0     0 ACCEPT     0    --  *      *       10.96.0.0/16         0.0.0.0/0           
    0     0 ACCEPT     0    --  *      *       0.0.0.0/0            10.244.0.0/16       
    0     0 ACCEPT     0    --  *      *       10.244.0.0/16        0.0.0.0/0           
    0     0 ACCEPT     0    --  ovn-k8s-mp0 *       0.0.0.0/0            0.0.0.0/0           
    0     0 ACCEPT     0    --  *      ovn-k8s-mp0  0.0.0.0/0            0.0.0.0/0
    
# In IPv6 with disable-forwarding=true the FORWARD policy is set to DROP
Chain FORWARD (policy DROP 0 packets, 0 bytes)
 pkts bytes target     prot opt in     out     source               destination         
    0     0 ACCEPT     0    --  *      *       ::/0                 fd69::1             
    0     0 ACCEPT     0    --  *      *       fd69::1              ::/0                
    0     0 ACCEPT     0    --  *      *       ::/0                 fd00:10:96::/112    
    0     0 ACCEPT     0    --  *      *       fd00:10:96::/112     ::/0                
    0     0 ACCEPT     0    --  *      *       ::/0                 fd00:10:244::/48    
    0     0 ACCEPT     0    --  *      *       fd00:10:244::/48     ::/0                
    0     0 ACCEPT     0    --  ovn-k8s-mp0 *       ::/0                 ::/0                
    0     0 ACCEPT     0    --  *      ovn-k8s-mp0  ::/0                 ::/0          
```

## Logging Config

## Monitoring Config

## IPFIX Config

## CNI Config

## Kubernetes Config

## Metrics Config

## OVN-Kubernetes Feature Config

### Enable Multiple Networks

Users can create pods with multiple interfaces such that each interface is hooked to
a separate network thereby enabling multiple networks for a given pod;
a.k.a multi-homing. All networks that are created as additions to the primary
default Kubernetes network are fondly called `secondary networks`. This feature
can be enabled by using the `--enable-multi-network` flag on OVN-Kubernetes clusters.


### Enable Network Segmentation

Users can enable the network-segmentation feature using `--enable-network-segmentation`
flag on a KIND cluster. This allows users to be able to design native isolation between
their tenant namespaces by coupling all namespaces that belong to the same
tenant under the same secondary network and then making this network the primary network
for the pod. Each network is isolated and cannot talk to other user
defined network. Check out the feature docs for more information on how to segment your
cluster on a network level.

NOTE: This feature only works if `--enable-multi-network` is
also enabled since it leverages the secondary networks feature.

## HA Config

## OVN Auth Config

## Hybrid Overlay Config

## Cluster Manager Config