package types

import (
	kapi "k8s.io/api/core/v1"
)

const (
	// HybridOverlayAnnotationBase holds the hybrid overlay annotation base
	HybridOverlayAnnotationBase = "k8s.v1.ovn.org/hybrid-overlay-"
	// HybridOverlayHostSubnet holds the pod CIDR assigned to the node
	HybridOverlayHostSubnet = HybridOverlayAnnotationBase + "hostsubnet"
	// HybridOverlayDrMac holds the MAC address of the Distributed Router/gateway
	HybridOverlayDrMac = HybridOverlayAnnotationBase + "distributed-router-gateway-mac"

	// HybridOverlayVNI is the VNI for VXLAN tunnels between nodes/endpoints
	HybridOverlayVNI = 4097
)

// NodeHandler interface respresents the three functions that get called by the informer upon respective events
type NodeHandler interface {
	// Add is called when a new object is created
	Add(obj *kapi.Node)

	// Update is called when an object is updated, both old and new ones are passed along
	Update(oldObj *kapi.Node, newObj *kapi.Node)

	// Delete is called when an object is deleted
	Delete(obj *kapi.Node)

	// Sync is called to synchronize the full list of objects
	Sync(objs []*kapi.Node)
}
