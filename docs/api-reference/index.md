---
title: API Reference Guide
hide:
  - navigation
  - toc
---

# API Reference Guide

Specifications for OVN-Kubernetes Custom Resource Definitions (CRDs).

<div class="landing-grid" markdown>

<div class="landing-card" markdown>

## Introduction

Overview of the OVN-Kubernetes API surface and CRD conventions.

[Read more](introduction.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## EgressIP

Assign stable source IPs to egress traffic from selected pods.

[Read more](egress-ip-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## EgressService

Route egress traffic through a Kubernetes Service's load balancer IP.

[Read more](egress-service-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## EgressQoS

Apply DSCP marking rules to egress traffic by namespace.

[Read more](egress-qos-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## EgressFirewall

Control which external destinations pods in a namespace can reach.

[Read more](egress-firewall-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## AdminPolicyBasedExternalRoutes

Define cluster-wide external gateway routing policies.

[Read more](admin-epbr-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## UserDefinedNetwork

Create isolated or connected tenant networks with custom topologies.

[Read more](userdefinednetwork-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## RouteAdvertisements

Advertise pod and service routes to external BGP peers.

[Read more](routeadvertisements-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## ClusterNetworkConnect

Enable controlled connectivity between isolated User Defined Networks.

[Read more](clusternetworkconnect-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## VTEP

Configure Virtual Tunnel Endpoints for external network integration.

[Read more](vtep-api-spec.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Uplink and UplinkState

Configure physical uplinks for DPU-accelerated UserDefinedNetworks.

[Read more](uplink-api-spec.md){ .landing-btn }

</div>

</div>
