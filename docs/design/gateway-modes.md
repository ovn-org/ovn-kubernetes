# Gateway Modes

OVN-Kubernetes supports two gateway modes that control how north-south
(pod ↔ external) traffic leaves the cluster at each node. The mode is
set cluster-wide via `--gateway-mode`.

## Shared Gateway Mode (`shared`)

In shared gateway mode the node's external IP address is **shared**
between the host and OVN. Pod egress traffic is routed entirely through
OVN — the Gateway Router (GR) performs SNAT to the node IP and sends
packets directly out the physical uplink via the external bridge
(br-ex). The host kernel is not in the egress data path.

This is the default mode in standard OVN-Kubernetes deployments/manifests.
At the binary CLI level, omitting `--gateway-mode` leaves gateway functionality disabled.

## Local Gateway Mode (`local`)

In local gateway mode (default network), pod egress traffic exits the OVN br-int bridge
but is delivered to the **host kernel** (via the management port, ovn-k8s-mp0) instead
of going directly to the physical NIC. The host's routing table and
iptables/nftables masquerading handle the final hop out. This gives the
host full control over outbound path selection.

Note: For some no-overlay CUDN paths, OVN network-scoped router SNAT-exemption/NAT
objects also participate, so the exact SNAT mechanism is network-type dependent.

## When to Use Which

| Consideration | Shared | Local |
|---|---|---|
| Simplicity | Simpler — fewer moving parts, no host iptables dependency for egress | More iptables/nftables rules on the host |
| Egress path control | OVN default route; BGP/Route Advertisements and MEG can steer traffic to dynamic next hops | Full control via host routing table, VRFs, policy routing; BGP/Route Advertisements also supported |
| VLAN tagging on external port | Supported (`--gateway-vlanid`) | Supported |
| Custom egress routing / VRFs | No | Yes |
| OVS offload (max throughput) | Yes — required for full OVS offload | No |

Choose **shared** when you want a straightforward setup and don't need
host-level routing control. Choose **local** when you need advanced
egress routing, VRF isolation, or EVPN integration.

## Configuration

```bash
--gateway-mode=shared   # deployment default (when enabled by tooling/manifests)
--gateway-mode=local
```

Helm:

```yaml
global:
  gatewayMode: "shared"
```

KIND:

```bash
./kind.sh --gateway-mode shared
./kind.sh --gateway-mode local
```
