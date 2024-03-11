# Hybrid Overlay

## Introduction

The hybrid overlay feature creates VXLAN tunnels to nodes in the cluster
that have been excluded from the ovn-kubernetes overlay using the
`no-hostsubnet-nodes` config option.

These tunnels allow pods on ovn-kubernetes nodes to communicate directly with
other pods on nodes that do not run ovn-kubernetes.

## Requirements

The feature can be enabled at runtime, but requires that the VXLAN UDP port (4789)
be accessible on every node in the cluster. The cluster administrator is
responsible for ensuring this port is open on all nodes.

Hybrid overlay uses the third IP address on every node's logical switch as the
gateway for traffic to hybrid overlay nodes. If the feature is enabled after
the cluster has been installed, and a pod on the node is using the address,
that pod will no longer work correctly until it has been killed and restarted.
This is not handled automatically.

It is recommended the hybrid overlay feature be enabled at cluster install time.
