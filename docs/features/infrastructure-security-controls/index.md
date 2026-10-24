---
title: Infrastructure Security Controls
hide:
  - toc
---

# Infrastructure Security Controls

Security mechanisms for OVN-Kubernetes infrastructure.

**[Security](security.md)**

Protection of OVN-Kubernetes-owned annotations and pod network allocations.

## Controller-owned Node annotations

An admission policy unconditionally protects these controller-owned node
annotations, permitting only `ovnkube-cluster-manager` to add, change, or remove them:

- `k8s.ovn.org/node-subnets`: pod subnet allocations.
- `k8s.ovn.org/node-id`: node IDs used to derive network resources.
- `k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids`: per-network router port tunnel IDs.

When hybrid overlay is enabled, the policy also protects
`k8s.ovn.org/hybrid-overlay-node-subnet`, allowing configured additional writers.
Other controller-owned annotations are outside this policy's scope.

This protection covers node creation and updates through both `nodes` and
`nodes/status`, and does not depend on the optional NodeIdentity service.
The webhook's `--extra-allowed-user` setting does not grant permission to
change these annotations. Custom deployments must include the admission policy
and its binding. Existing annotation values are not revalidated when unchanged;
allocation validation and recovery are handled separately by controllers.

Recreating a Node from saved YAML containing protected OVN annotations may be
rejected by admission. Before creating the Node, omit controller-owned OVN
annotations, including those listed above, and let the controllers regenerate
them. When NodeIdentity is enabled, its webhook also restricts other OVN node
annotations; normally omit those from the saved YAML as well.

**[NodeIdentity](node-identity.md)**

Cryptographic node identity verification to prevent unauthorized nodes from joining the network.
