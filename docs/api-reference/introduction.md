# OVN-Kubernetes API Reference Guide

OVN-Kubernetes implements core Kubernetes Networking
features as well as advanced features using Custom
Resource Definitions.

## Core Kubernetes features API Reference

This section contains the API Reference guide for core
Kubernetes APIs that OVN-Kubernetes watches

* [Nodes](https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/node-v1/)
* [Namespaces](https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/namespace-v1/)
* [Pods](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/)
* [Services](https://kubernetes.io/docs/reference/kubernetes-api/service-resources/service-v1/)
* [Endpoints](https://kubernetes.io/docs/reference/kubernetes-api/service-resources/endpoints-v1/)
* [EndpointSlices](https://kubernetes.io/docs/reference/kubernetes-api/service-resources/endpoint-slice-v1/)
* [NetworkPolicies](https://kubernetes.io/docs/reference/kubernetes-api/policy-resources/network-policy-v1/)

## Extended Kubernetes features API Reference

This section contains the API Reference guide for extended
Kubernetes ecosystem APIs that OVN-Kubernetes watches and implements

* [AdminNetworkPolicies](https://network-policy-api.sigs.k8s.io/reference/spec/#policy.networking.k8s.io%2fv1alpha1.AdminNetworkPolicy) - shipped by SIG-Network-Policy-API
* [BaselineAdminNetworkPolicies](https://network-policy-api.sigs.k8s.io/reference/spec/#policy.networking.k8s.io%2fv1alpha1.BaselineAdminNetworkPolicy) - shipped by SIG-Network-Policy-API
* [NetworkAttachmentDefinitions](https://github.com/k8snetworkplumbingwg/network-attachment-definition-client/blob/master/pkg/apis/k8s.cni.cncf.io/v1/types.go) - shipped by Kubernetes-Network-Plumbing-WG
* [MultiNetworkPolicies](https://github.com/k8snetworkplumbingwg/multi-networkpolicy/tree/master/pkg/apis/k8s.cni.cncf.io) - shipped by Kubernetes-Network-Plumbing-WG

## OVN-Kubernetes features API Reference

This section contains the API Reference guide for features
designed and implemented by OVN-Kubernetes

* [EgressIP](egress-ip-api-spec.md)
* [EgressService](egress-service-api-spec.md)
* [EgressQoS](egress-qos-api-spec.md)
* [EgressFirewall](egress-firewall-api-spec.md)
* [AdminPolicyBasedExternalRoutes](admin-epbr-api-spec.md)
* [UserDefinedNetwork](userdefinednetwork-api-spec.md)
* [Uplink and UplinkState](uplink-api-spec.md)
* [RouteAdvertisements](routeadvertisements-api-spec.md)
* [VTEP](vtep-api-spec.md)
* [ClusterNetworkConnect](clusternetworkconnect-api-spec.md)

## Updating API reference docs
Update the API reference docs by running the following make target:
```bash
$ make -C docs/ generate-api-reference
```
