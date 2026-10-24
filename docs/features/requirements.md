# External requirements

OVN-Kubernetes has additional dependencies for the external components, here are the recommended (not necessarily minimal)
supported versions.

| OVN-Kubernetes release | OVN release | nft binary | multus | CNI spec | k8s  |
|------------------------|-------------|------------|--------|----------|------|
| master                 | 26.03       | 1.0.1+     | v4.1.3 | 1.1.0    | 1.33+ |
| 1.2                    | 25.09       | 1.0.1+     | v4.1.3 | 0.4.0    | 1.34 |
| 1.1                    | 25.03       | 1.0.1+     | v4.1.3 | 0.4.0    | 1.33 |
| 1.0                    | 24.03       | -          | v4.1.0 | 0.4.0    | 1.29 |

OVN should work with any supported OVS release, extra requirements for OVS version may be specified per-feature

- [OVN releases](https://www.ovn.org/en/releases/all_releases/)
- [OVS release process](https://docs.openvswitch.org/en/latest/internals/release-process/)

Some of the requirements are feature-specific.

## UDN

### Masquerade Subnet Sizing

When UserDefinedNetworks are in use, the `internalMasqueradeSubnet` (IPv4) and
`internalMasqueradeV6Subnet` (IPv6) must be large enough to accommodate the maximum
number of concurrent networks (`MaxNetworks = 4096`).

The masquerade IP index formula (from `go-controller/pkg/generator/udn/masquerade_ips.go`):

```text
GatewayRouter  index = 10 + networkID × 2 − 1
ManagementPort index = 10 + networkID × 2
```

For `networkID = MaxNetworks = 4096`, the highest index is `8202`. The subnet must
contain at least `8203` IPs. Max safe networkID = `floor((subnet_size - 1 - 10) / 2)`.

**IPv4 sizing:**

| IPv4 Subnet | IPs    | Max safe networkID | Safe with MaxNetworks=4096? |
|-------------|--------|-------------------|----------------------------|
| /29         | 8      | 0                 | NO — cannot fit 1 UDN       |
| /24         | 256    | 122               | NO — crashes after ~122 total creates |
| /20         | 4096   | 2042              | NO — crashes after ~2042 total creates |
| /19         | 8192   | 4090              | NO — max index 8202 > 8191  |
| **/18**     | 16384  | 8186              | **YES — minimum safe size** |
| /17         | 32768  | 16378             | YES — recommended            |

**Recommendation:** Use `/17` for IPv4 (`169.254.0.0/17`). The `/17` range
(`169.254.0.0–169.254.127.255`) is chosen to avoid overlapping with the cloud
metadata IP `169.254.169.254`.

**IPv6 sizing:**

The same formula applies to `internalMasqueradeV6Subnet`. The default `fd69::/125`
(8 IPs) is insufficient for any UDN.

| IPv6 Prefix | IPs    | Max safe networkID | Safe with MaxNetworks=4096? |
|-------------|--------|-------------------|----------------------------|
| /125        | 8      | 0                 | NO — cannot fit 1 UDN       |
| /117        | 2048   | 1019              | NO — crashes after ~1019 total creates |
| /115        | 8192   | 4090              | NO — max index 8202 > 8191  |
| **/114**    | 16384  | 8186              | **YES — minimum safe size** |
| /113        | 32768  | 16378             | YES — recommended            |

**Recommendation:** Use `/113` for IPv6. Update the `--gateway-v6-masquerade-subnet`
flag at ovnkube startup (or the equivalent operator field) and restart ovnkube-node pods.

**Note on the 4096 limit:** `MaxNetworks = 4096` is tied to OVN's transit switch
tunnel key reservation. OVN reserves a deterministic key at
`BaseTransitSwitchTunnelKey + networkID` for each network's transit switch, with
exactly 4096 slots reserved at the top of the 24-bit key space. See
[ovn-util.h](https://github.com/ovn-org/ovn/blob/cfaf849c034469502fc97149f20676dec4d76595/lib/ovn-util.h#L159-L164).

**Existing deployments with a small subnet:** If your deployment was configured with
a masquerade subnet smaller than `/18` (IPv4) or `/114` (IPv6), update it before
creating UDNs. The relevant flags are `--gateway-v4-masquerade-subnet` and
`--gateway-v6-masquerade-subnet` (passed to `ovnkube` at startup). After changing,
restart ovnkube-node pods to pick up the new values.

### Kubelet Network Probes

For kubelet network probes to work with UDN pods, the following are required:
- kernel fix 7f3287db654395f9c5ddd246325ff7889f550286: netfilter: nft_socket: make cgroupsv2 matching work with namespaces)

  - introduced in [6.11](https://cdn.kernel.org/pub/linux/kernel/v6.x/ChangeLog-6.11), backported to 
  [6.10.12](https://cdn.kernel.org/pub/linux/kernel/v6.x/ChangeLog-6.10.12),
  [6.6.53](https://cdn.kernel.org/pub/linux/kernel/v6.x/ChangeLog-6.6.53), and 
  [6.1.112](https://cdn.kernel.org/pub/linux/kernel/v6.x/ChangeLog-6.1.112)
- cgroupv2: required for kubelet probes to work with UDN pods

## BGP, EVPN, and No-Overlay (ENABLE_ROUTE_ADVERTISEMENTS)

- [Route Advertisements feature link](bgp-integration/route-advertisements.md)
- [No-overlay feature link](bgp-integration/no-overlay.md)

| OVN-Kubernetes release | frr-k8s | frr  | 
|------------------------|---------|------|
| master                 | v0.0.21 | 10.4 |
| 1.2                    | v0.0.17 | 9.1  |
| 1.1                    | v0.0.17 | 9.1  |

## OVN Observability

[Feature link](../observability/ovn-observability.md)

OVS 3.4+ and linux kernel 6.11+

## Multi-VTEP

[Feature link](multiple-networks/multi-vtep.md)

OVN version newer than v24.03.2

## OVS acceleration with Kernel datapath

[Prerequisites](hardware-offload/ovs-kernel.md#prerequisites)

- Linux Kernel 5.7.0 or above
- Open vSwitch 2.13 or above
- iproute >= 4.12

## DPU healthcheck support

[DPU healthcheck support OKEP](../okeps/okep-5674-dpu-healthcheck.md)

- multus-CNI >= v4.2.4
- containerd >= v2 or crio >= 1.32
