# Network QoS

## Introduction

To enable NetworkQoS, we will use Differentiated Services Code Point (DSCP) which allows us to classify packets by setting a 6-bit field in the IP header, effectively marking the priority of a given packet relative to other packets as "Critical", "High Priority", "Best Effort" and so on.

## Problem Statement
The workloads running in Kubernetes using OVN-Kubernetes as a networking backend might have different requirements in handling network traffic. For example video streaming application needs low latency and jitter whereas storage application can tolerate with packet loss. Hence NetworkQoS is essential in meeting these SLAs to provide better service quality.

The workload taffic can be either east west (pod to pod traffic) or north south traffic (pod to external traffic) types in a Kubernetes cluster which is limited by finite bandwidth. So NetworkQoS must ensure high priority applications get the necessary NetworkQoS marking so that it can prevent network conjestion.

## Proposed Solution

By introducing a new CRD `NetworkQoS`, users could specify a DSCP value for packets originating from pods on a given namespace heading to a specified Namespace Selector, Pod Selector, CIDR, Protocol and Port. This also supports metering for the packets by specifying bandwidth parameters `rate` and/or `burst`.
The CRD will be Namespaced, with multiple resources allowed per namespace.
The resources will be watched by ovn-k, which in turn will configure OVN's [QoS Table](https://man7.org/linux/man-pages/man5/ovn-nb.5.html#NetworkQoS_TABLE).
The `NetworkQoS` also has `status` field which is populated by ovn-k which helps users to identify whether NetworkQoS rules are configured correctly in OVN or not.

## Sources
- [OKEP-4380: Network QoS Support](https://github.com/pperiyasamy/ovn-kubernetes/blob/qos-okep/docs/okeps/okep-4380-network-qos.md)
