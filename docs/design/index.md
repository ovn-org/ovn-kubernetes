---
title: Overview
hide:
  - navigation
  - toc
---

# Overview

Architecture, design decisions, and traffic flows in OVN-Kubernetes.

<div class="landing-grid" markdown>

<div class="landing-card" markdown>

## Architecture

Components, pods, and containers that make up an OVN-Kubernetes deployment.

[Read more](architecture.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Network Topology

Logical switches, routers, and how they map to the physical cluster.

[Read more](topology.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Gateway Modes

Local vs shared gateway modes and how they affect traffic paths.

[Read more](gateway-modes.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Traffic Flows

End-to-end packet paths for pod-to-pod, pod-to-service, and external traffic.

[Read more](traffic-flows.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Pod Creation Workflow

What happens in OVN when a new pod is scheduled on a node.

[Read more](pod-creation-workflow.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Service Creation Workflow

How Kubernetes Services are translated into OVN load balancers.

[Read more](service-creation-workflow.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Service Traffic Policy

Internal and external traffic policy behavior for OVN-backed services.

[Read more](service-traffic-policy.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Host To NodePort Hairpin

Traffic flow when a node accesses its own NodePort service.

[Read more](host-to-node-port-hairpin-trafficflow.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## ExternalIPs / LoadBalancerIngress

How external IPs and LoadBalancer ingress addresses are handled.

[Read more](external-ip-and-loadbalancer-ingress.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## External Bridge Flows

OpenFlow rules on the breth0 bridge and how traffic is steered.

[Read more](bridge-flows.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Masquerade IPs

How masquerade IPs are used for return traffic and SNAT in OVN-Kubernetes.

[Read more](masquerade-ips.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## ACLs

How OVN ACLs implement network policies, admin policies, and egress firewalls.

[Read more](acls.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## EgressIP Design

Source IP control for egress traffic — architecture and OVN implementation details.

[Read more](egressip.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Gateway Accelerated Interface

Configure accelerated gateway interfaces for hardware-offloaded external traffic.

[Read more](gateway-accelerated-interface-configuration.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## KubeVirt VM Live Migration

Networking considerations when live-migrating VMs under KubeVirt.

[Read more](../features/live-migration.md){ .landing-btn }

</div>

</div>
