# Config Variables

Let us look at the supported configuration variables by OVN-Kubernetes

## Default Config

## Gateway Config

### Disable Forwarding Config

OVN-Kubernetes configures packet forwarding as follows:

  - If IPv4 is enabled, it sets the `net.ipv4.conf.[IFNAME].forwarding` sysctl to `1` on
    the OVN-Kubernetes management port and bridge interfaces, allowing IPv4 packets to be
    forwarded to and from those interfaces specifically.

  - If IPv6 is enabled, and you are using a sufficiently new kernel (6.17+), it sets the
    `net.ipv6.conf.[IFNAME].force_forwarding` sysctl to `1` on the OVN-Kubernetes
    management port and bridge interfaces, allowing IPv6 packets to be forwarded to and
    from those interfaces specifically.

The result is that packet forwarding is only enabled on OVN-Kubernetes's own interfaces
(unless the administrator set `net.ipv4.ip_forward` and/or `net.ipv6.conf.all.forwarding`
themselves to enable global forwarding)

Older (pre-6.17) kernels did not support per-interface IPv6 forwarding, so if you are
running on an older host with IPv6 enabled, OVN-Kubernetes has to set
`net.ipv6.conf.all.forwarding` to `1`, enabling IPv6 packets to be forwarded to and from
_all_ interfaces. The [IP sysctl documentation] recommends that if you only want IPv6
forwarding on specific interfaces in this case, you should use iptables rules to block it
on other interfaces. OVN-Kubernetes provides the `disable-forwarding` config/command-line
option to do this:

  - If `disable-forwarding` is `true` (and IPv6 is enabled, and the kernel does not
    support per-interface IPv6 forwarding):

      - OVN-Kubernetes sets the default policy of the ip6tables `FORWARD` chain to
        `DROP`, blocking the effect of the global forwarding sysctls.

      - To ensure that OVN-Kubernetes's own IPv6 traffic is still forwarded, it adds
        specific `ACCEPT` rules to the `FORWARD` chain to allow forwarding traffic to
        or from IPv6 Pod and Service networks, and to or from the IPv6 "masquerade IP".

      - This fixes IPv6 forwarding to work effectively the same as IPv4 forwarding: it is
        only allowed on OVN-Kubernetes's own interfaces.

  - In all other cases (`disable-forwarding` is `false`, or the cluster is single-stack
    IPv4, or the kernel supports per-interface IPv6 forwarding):

      - OVN-Kubernetes does not take any action other than resetting the default policy of
        the `FORWARD` chain back to `ACCEPT` if it appears that OVN-Kubernetes itself had
        previously set it to `DROP`.

Note that setting `disable-forwarding` has no effect on IPv4 traffic, and has no effect on
nodes with newer kernels.

Note that this is always done via iptables, not nftables, to better preserve compatibility
with other components. If ovn-kubernetes were to create its own `hook forward; policy
drop` table in nftables, there would be no way for other components to add `accept` rules
that would override it. But if all components use the iptables `FORWARD` chain, then they
can all coordinate on accept/drop there.

[IP sysctl documentation]: https://www.kernel.org/doc/Documentation/networking/ip-sysctl.txt

### VLAN Config

OVN-Kubernetes supports using VLAN tagging for underlay connectivity. To enable VLAN tagging, specify the `vlan-id`
gateway configuration option with the desired VLAN tag. This tag will be used for traffic ingressing and egressing
OVN as well as the host. When `vlan-id` is configured:

* OVN will be configured to accept the VLAN tag specified by `vlan-id`
* The external gateway bridge will be configured to add/strip the VLAN tag specified by `vlan-id` for packets destined to
and coming from the host
* The physical interface attached to the external gateway bridge will act as a VLAN trunk

Note, it is not required to configure a VLAN sub-interface (802.1q interface) on the host, OVN-Kubernetes will automatically
handle VLAN tagging in the OVS external bridge. It is also supported to have additional interfaces attached to the external
gateway bridge that use different VLAN tags than VLANID. These interfaces will operate in their own VLAN, and share the
physical interface as a trunk.

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
flag on a KinD cluster. This allows users to be able to design native isolation between
their tenant namespaces by coupling all namespaces that belong to the same
tenant under the same secondary network and then making this network the primary network
for the pod. Each network is isolated and cannot talk to other user
defined network. Check out the feature docs for more information on how to segment your
cluster on a network level.

NOTE: This feature only works if `--enable-multi-network` is
also enabled since it leverages the secondary networks feature.

## HA Config

`ovnkube-cluster-manager` runs in the `ovnkube-control-plane` Deployment. The
Helm chart pins it to control-plane-labeled nodes
(`node-role.kubernetes.io/control-plane`) and enforces one pod per node via
pod anti-affinity, but defaults to a single replica.

For HA, set `replicas` on the `ovnkube-control-plane` chart to match the
number of control-plane nodes:

    helm install ovn-kubernetes . ... --set ovnkube-control-plane.replicas=3

Cluster-manager uses Kubernetes lease-based leader election — only one replica
is active at any time; the rest stand by and take over on failure.

## Hybrid Overlay Config

## Cluster Manager Config
