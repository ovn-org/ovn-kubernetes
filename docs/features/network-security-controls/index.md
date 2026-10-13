---
title: Network Security Controls
hide:
  - toc
---

# Network Security Controls

Network policies and firewall rules for controlling traffic in OVN-Kubernetes.

**[AdminNetworkPolicy](admin-network-policy.md)**

Cluster-scoped network policies that take precedence over namespace-level NetworkPolicy.

**[NetworkPolicy](network-policy.md)**

Standard Kubernetes NetworkPolicy enforcement via OVN ACLs.

**[EgressFirewall](egress-firewall.md)**

Restrict which external destinations pods in a namespace can reach.

**[DNS Name Resolution](dns-name-resolution.md)**

DNS-based matching for EgressFirewall rules.
