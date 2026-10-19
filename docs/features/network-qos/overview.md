# Network QoS

## Introduction

To enable NetworkQoS, we will use Differentiated Services Code Point (DSCP) which allows us to classify packets by setting a 6-bit field in the IP header, effectively marking the priority of a given packet relative to other packets as "Critical", "High Priority", "Best Effort" and so on.

## Problem Statement
The workloads running in Kubernetes using OVN-Kubernetes as a networking backend might have different requirements in handling network traffic. For example, a video streaming application needs low latency and jitter, whereas a storage application can tolerate packet loss. Hence NetworkQoS is essential in meeting these SLAs to provide better service quality.

Workload traffic in a Kubernetes cluster can be either east-west (pod-to-pod) or north-south (pod-to-external), and bandwidth is finite. NetworkQoS must ensure high-priority applications get the necessary markings to help prevent network congestion.

## Solution

The `NetworkQoS` CRD lets users specify a DSCP value for packets originating from pods in a given namespace heading to a specified Namespace Selector, Pod Selector, CIDR, Protocol and Port. It also supports metering for the packets by specifying bandwidth parameters `rate` and/or `burst`.
The CRD is namespaced, with multiple resources allowed per namespace.
The resources are watched by ovn-k, which configures OVN's [QoS Table](https://man7.org/linux/man-pages/man5/ovn-nb.5.html#NetworkQoS_TABLE).
The `NetworkQoS` also has `status` field which is populated by ovn-k which helps users to identify whether NetworkQoS rules are configured correctly in OVN or not.

## Sources
- [OKEP-4380: Network QoS Support](../../okeps/okep-4380-network-qos.md)
