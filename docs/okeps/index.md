---
title: Enhancement Proposals
hide:
  - toc
---

# Enhancement Proposals

OVN-Kubernetes Enhancement Proposals (OKEPs) describe design and implementation plans for new features.

**[Localnet API](okep-5085-localnet-api.md)**

Native API for connecting pods directly to external physical networks.

**[Network QoS](okep-4380-network-qos.md)**

DSCP marking and traffic shaping at the pod level.

**[User Defined Networks](okep-5193-user-defined-networks.md)**

Tenant network isolation and custom network topologies.

**[Preconfigured UDN Addresses](okep-5233-preconfigured-udn-addresses.md)**

Static IP address assignment for User Defined Networks.

**[BGP](okep-5296-bgp.md)**

Advertising pod and service routes to external BGP routers.

**[Layer2 Transit Router](okep-5094-layer2-transit-router.md)**

Layer 2 transit switch architecture for cross-node connectivity.

**[MCP for Troubleshooting](okep-5494-ovn-kubernetes-mcp-server.md)**

Model Context Protocol server for AI-assisted cluster debugging.

**[Dynamic UDN Node Allocation](okep-5552-dynamic-udn-node-allocation.md)**

On-demand subnet allocation for UDN nodes as pods are scheduled.

**[Connecting User Defined Networks](okep-5224-connecting-udns/okep-5224-connecting-udns.md)**

Controlled inter-UDN connectivity via ClusterNetworkConnect.

**[Connecting UDNs: Discarded Alternatives](okep-5224-connecting-udns/discarded-alternatives.md)**

Discarded network topology proposals for connecting Layer3-type Cluster User Defined Networks (CUDNs).

**[No-Overlay Mode](okep-5259-no-overlay.md)**

Direct pod routing using BGP-learned routes without encapsulation.

**[EVPN](okep-5088-evpn.md)**

Ethernet VPN integration for scalable multi-tenant networking.

**[DPU Healthcheck](okep-5674-dpu-healthcheck.md)**

Health monitoring for DPU-offloaded networking components.

**[Extend UDN to Support Multiple Cluster Subnets](okep-5377-extend-udn-to-support-multiple-cluster-subnets-in-layer3-topology.md)**

Multiple cluster subnets in Layer3 topology for primary UDN/CUDN.

**[VRF-Lite Shared Gateway Mode](okep-6019-vrf-lite-shared-gateway-external-bridges.md)**

VRF-Lite shared gateway mode with external bridge uplinks.

**[Disable MAC Spoof Protection on Secondary Networks](okep-3926-disable-port-security.md)**

Allow disabling port security on secondary network interfaces.

**[OVN Observability API](okep-5212-ovnobserv-api.md)**

API for OVN observability sampling and tracing.

**[DHCP IPAM for Localnet](okep-6224-dhcp-ipam-localnet.md)**

DHCP-based IPAM for localnet secondary networks.
