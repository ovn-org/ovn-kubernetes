# OVN-Kubernetes API Reference Guide

OVN-Kubernetes implements core Kubernetes Networking
features as well as advanced features using Custom
Resource Definitions.

## Core Kubernetes features API Reference

This section contains the API Reference guide for core
Kubernetes APIs that is watched by OVN-Kubernetes

* [Nodes](https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/node-v1/)
* [Namespaces](https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/namespace-v1/)
* [Pods](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/)
* [Services](https://kubernetes.io/docs/reference/kubernetes-api/service-resources/service-v1/)
* [Endpoints](https://kubernetes.io/docs/reference/kubernetes-api/service-resources/endpoints-v1/)
* [EndpointSlices](https://kubernetes.io/docs/reference/kubernetes-api/service-resources/endpoint-slice-v1/)
* [NetworkPolicies](https://kubernetes.io/docs/reference/kubernetes-api/policy-resources/network-policy-v1/)

## Extended Kubernetes features API Reference

This section contains the API Reference guide for extended
Kubernetes ecosystem APIs that is watched and implemented by OVN-Kubernetes

* [AdminNetworkPolicies](https://network-policy-api.sigs.k8s.io/reference/spec/#policy.networking.k8s.io%2fv1alpha1.AdminNetworkPolicy) - shipped by SIG-Network-Policy-API
* [BaselineAdminNetworkPolicies](https://network-policy-api.sigs.k8s.io/reference/spec/#policy.networking.k8s.io%2fv1alpha1.BaselineAdminNetworkPolicy) - shipped by SIG-Network-Policy-API
* [NetworkAttachmentDefinitions](https://github.com/k8snetworkplumbingwg/network-attachment-definition-client/blob/master/pkg/apis/k8s.cni.cncf.io/v1/types.go) - shipped by Kubernetes-Network-Plumbing-WG
* [MultiNetworkPolicies](https://github.com/k8snetworkplumbingwg/multi-networkpolicy/tree/master/pkg/apis/k8s.cni.cncf.io) - shipped by Kubernetes-Network-Plumbing-WG

## OVN-Kubernetes features API Reference

This section contains the API Reference guide for features
designed and implemented by OVN-Kubernetes

* [EgressIP](https://ovn-kubernetes.io/api-reference/egress-ip-api-spec/)
* [EgressService](https://ovn-kubernetes.io/api-reference/egress-service-api-spec/)
* [EgressQoS](https://ovn-kubernetes.io/api-reference/egress-qos-api-spec/)
* [EgressFirewall](https://ovn-kubernetes.io/api-reference/egress-firewall-api-spec/)
* [AdminPolicyBasedExternalRoutes](https://ovn-kubernetes.io/api-reference/admin-epbr-api-spec/)
* [UserDefinedNetwork](https://ovn-kubernetes.io/api-reference/userdefinednetwork-api-spec/)
