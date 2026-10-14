---
title: Infrastructure Security Controls
hide:
  - toc
---

# Infrastructure Security Controls

Node-level security mechanisms for OVN-Kubernetes infrastructure.

## Controller-owned Node annotations

The `k8s.ovn.org/node-subnets` annotation controls pod address allocation and
networking configuration across the cluster. An admission policy permits only
`ovnkube-cluster-manager` to add, change, or remove it. This protection does not
depend on the optional NodeIdentity service.

**[NodeIdentity](node-identity.md)**

Cryptographic node identity verification to prevent unauthorized nodes from joining the network.
