/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/* NOTE: This file is loosely based on https://github.com/kubernetes/kubernetes/blob/23a4920d83559ba803629512c8a02630e5fdc0a6/pkg/proxy/topology.go.
We are picking up the parts we need to implement topology aware hints without having to vendor in the k8s code.
Technically this should live in package kube, but we are using LB config structs here and thus it made sense to keep
it in services package */

package services

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/klog/v2"
)

// topologyAwareRoutingIsEnabledForService is true for the given service if:
// * The "service.kubernetes.io/topology-aware-hints" annotation on this Service is set to "Auto"
// * All of the endpoints for this Service have a topology hint
func topologyAwareRoutingIsEnabledForService(service *v1.Service, endpointSlices []*discovery.EndpointSlice) bool {
	hintsAnnotation := service.Annotations[v1.AnnotationTopologyAwareHints]
	if hintsAnnotation != "Auto" && hintsAnnotation != "auto" {
		if hintsAnnotation != "" && hintsAnnotation != "Disabled" && hintsAnnotation != "disabled" {
			klog.Warningf("Skipping topology aware endpoint filtering since Service %s/%s has unexpected value in %s:%v", service.Namespace, service.Name, v1.AnnotationTopologyAwareHints, hintsAnnotation)
		}
		return false
	}
	for _, epSlice := range endpointSlices {
		for _, endpoint := range epSlice.Endpoints {
			if endpoint.Hints.Size() == 0 {
				klog.Warningf("Skipping topology aware endpoint filtering since one or more endpoints is missing a zone hint for service %s/%s", service.Namespace, service.Name)
				return false
			}
		}
	}
	return true
}

// topologyAwareRoutingIsEnabledForLBConfig is true for the given LB config if:
// * The node's labels include "topology.kubernetes.io/zone"
// * At least one endpoint for this Service LB config is hinted for this node's zone.
func topologyAwareRoutingIsEnabledForLBConfig(endpointsInfo util.LbEndpoints, nodeInfo nodeInfo) bool {
	zone, ok := nodeInfo.nodeLabels[v1.LabelTopologyZone]
	if !ok || zone == "" {
		klog.Warningf("Skipping topology aware endpoint filtering since node %s is missing %s label", nodeInfo.name, v1.LabelTopologyZone)
		return false
	}
	if _, ok := endpointsInfo.ZoneHints[zone]; !ok {
		return false
	}
	return true
}
