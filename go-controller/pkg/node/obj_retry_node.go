// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"fmt"
	"net"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	cache "k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type nodeEventHandler struct {
	retry.DefaultEventHandler

	objType  reflect.Type
	nc       *DefaultNodeNetworkController
	syncFunc func([]interface{}) error
}

func (h *nodeEventHandler) FilterOutResource(_ interface{}) bool {
	return false
}

// newRetryFrameworkNodeWithParameters builds and returns a retry framework for the input resource
// type and assigns all ovnk-node-specific function attributes in the returned struct;
// these functions will then be called by the retry logic in the retry package when
// WatchResource() is called.
// newRetryFrameworkNodeWithParameters takes as input a resource type (required)
// and the following optional parameters: a namespace and a label filter for the
// shared informer and a sync function to process all objects of this type at startup.
// In order to create a retry framework for most resource types, newRetryFrameworkNode is
// to be preferred, as it calls newRetryFrameworkNodeWithParameters with all optional parameters unset.
func (nc *DefaultNodeNetworkController) newRetryFrameworkNodeWithParameters(
	objectType reflect.Type,
	syncFunc func([]interface{}) error) *retry.RetryFramework {

	resourceHandler := &retry.ResourceHandler{
		HasUpdateFunc:          hasResourceAnUpdateFunc(objectType),
		NeedsUpdateDuringRetry: needsUpdateDuringRetry(objectType),
		ObjType:                objectType,
		EventHandler: &nodeEventHandler{
			objType:  objectType,
			nc:       nc,
			syncFunc: syncFunc,
		},
	}

	r := retry.NewRetryFramework("Default/NodeController", nc.stopChan, nc.wg, nc.watchFactory.(*factory.WatchFactory), resourceHandler)

	return r
}

// newRetryFrameworkNode takes as input a resource type and returns a retry framework
// as defined for that type. This constructor is used for resources (1) that do not need
// any namespace or label filtering in their shared informer, (2) whose sync function
// is assigned statically based on the resource type.
func (nc *DefaultNodeNetworkController) newRetryFrameworkNode(objectType reflect.Type) *retry.RetryFramework {
	return nc.newRetryFrameworkNodeWithParameters(objectType, nil)
}

// hasResourceAnUpdateFunc returns true if the given resource type has a dedicated update function.
// It returns false if, upon an update event on this resource type, we instead need to first delete the old
// object and then add the new one.
func hasResourceAnUpdateFunc(objType reflect.Type) bool {
	switch objType {
	case factory.EndpointSliceForStaleConntrackRemovalType,
		factory.NodeType:
		return true
	}
	return false
}

// Given an object type, needsUpdateDuringRetry returns true if the object needs to invoke update during iterate retry.
func needsUpdateDuringRetry(objType reflect.Type) bool {
	switch objType {
	case factory.EndpointSliceForStaleConntrackRemovalType,
		factory.NodeType:
		return true
	}
	return false
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *nodeEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	// switch based on type
	switch h.objType {
	case factory.EndpointSliceForStaleConntrackRemovalType:
		// always run update code
		return false, nil

	case factory.NodeType:
		node1, ok := obj1.(*corev1.Node)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *kapi.Node", obj1)
		}
		node2, ok := obj2.(*corev1.Node)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *kapi.Node", obj2)
		}
		return reflect.DeepEqual(node1.Status.Addresses, node2.Status.Addresses) && reflect.DeepEqual(node1.Annotations, node2.Annotations), nil

	default:
		return false, fmt.Errorf("no object comparison for type %s", h.objType)
	}

}

// Given an object key and its type, GetResourceFromInformerCache returns the latest state of the object
// from the informers cache.
func (h *nodeEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var namespace, name string
	var err error

	namespace, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %v", key, err)
	}

	switch h.objType {

	case factory.EndpointSliceForStaleConntrackRemovalType:
		obj, err = h.nc.watchFactory.GetEndpointSlice(namespace, name)

	case factory.NodeType:
		obj, err = h.nc.watchFactory.GetNode(name)

	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			h.objType)
	}

	return obj, err
}

// Given a *RetryFramework instance, an object to add and a boolean specifying if
// the function was executed from iterateRetryResources, AddResource adds the
// specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
func (h *nodeEventHandler) AddResource(obj interface{}, _ bool) error {
	switch h.objType {
	case factory.EndpointSliceForStaleConntrackRemovalType:
		// Handle add events to detect 0→N endpoint transitions and flush stale conntrack entries
		epSlice := obj.(*discovery.EndpointSlice)
		if err := h.nc.reconcileConntrackUponEndpointSliceEvents(nil, epSlice); err != nil {
			return fmt.Errorf("failed to reconcile conntrack for added EndpointSlice %s/%s: %w", epSlice.Namespace, epSlice.Name, err)
		}
		return nil

	case factory.NodeType:
		node := obj.(*corev1.Node)
		// if it's our node that is changing, then nothing to do as we dont add our own IP to the nftables rules
		if node.Name == h.nc.name {
			if config.IsModeDPUHost() || config.IsModeFull() {
				if util.NodeDontSNATSubnetAnnotationExist(node) {
					err := managementport.UpdateNoSNATSubnetsSets(node, util.ParseNodeDontSNATSubnetsList)
					if err != nil {
						return fmt.Errorf("error updating no snat subnets sets: %w", err)
					}
				}

				// Sync nftables sets for no-overlay SNAT exemption in LGW mode.
				// In SGW mode, OVN address sets are used instead.
				if config.Default.Transport == types.NetworkTransportNoOverlay && config.NoOverlay.OutboundSNAT == types.NoOverlaySNATEnabled && config.Gateway.Mode == config.GatewayModeLocal {
					hostAddrs, err := util.GetNodeHostAddrs(node)
					if err != nil {
						return fmt.Errorf("failed to get host addresses for node %s: %w", node.Name, err)
					}
					if err := syncNoOverlaySNATExemptNFTSets(hostAddrs); err != nil {
						return fmt.Errorf("failed to sync no-overlay SNAT exemption nftables sets: %w", err)
					}
				}
			}

			return nil
		}
		return h.nc.addOrUpdateNode(node)

	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}
}

// Given a *RetryFramework instance, an old and a new object, UpdateResource updates
// the specified object in the cluster to its version in newObj according to its type
// and returns the error, if any, yielded during the object update. The inRetryCache
// boolean argument is to indicate if the given resource is in the retryCache or not.
func (h *nodeEventHandler) UpdateResource(oldObj, newObj interface{}, _ bool) error {
	switch h.objType {
	case factory.EndpointSliceForStaleConntrackRemovalType:
		oldEndpointSlice := oldObj.(*discovery.EndpointSlice)
		newEndpointSlice := newObj.(*discovery.EndpointSlice)
		return h.nc.reconcileConntrackUponEndpointSliceEvents(
			oldEndpointSlice, newEndpointSlice)

	case factory.NodeType:
		oldNode := oldObj.(*corev1.Node)
		newNode := newObj.(*corev1.Node)

		// if it's our node that is changing, then nothing to do as we dont add our own IP to the nftables rules
		if newNode.Name == h.nc.name {
			if (config.IsModeDPUHost() || config.IsModeFull()) && !reflect.DeepEqual(oldNode.Annotations, newNode.Annotations) {
				// if node's dont SNAT subnet annotation changed sync nftables
				if util.NodeDontSNATSubnetAnnotationChanged(oldNode, newNode) {
					err := managementport.UpdateNoSNATSubnetsSets(newNode, util.ParseNodeDontSNATSubnetsList)
					if err != nil {
						return fmt.Errorf("error updating no snat subnets sets: %w", err)
					}
				}

				// Sync nftables sets for no-overlay SNAT exemption in LGW mode if host addresses annotation changed.
				// In SGW mode, OVN address sets are used instead.
				if config.Default.Transport == types.NetworkTransportNoOverlay && config.NoOverlay.OutboundSNAT == types.NoOverlaySNATEnabled && config.Gateway.Mode == config.GatewayModeLocal {
					if util.NodeHostCIDRsAnnotationChanged(oldNode, newNode) {
						hostAddrs, err := util.GetNodeHostAddrs(newNode)
						if err != nil {
							return fmt.Errorf("failed to get host addresses for node %s: %w", newNode.Name, err)
						}
						if err := syncNoOverlaySNATExemptNFTSets(hostAddrs); err != nil {
							return fmt.Errorf("failed to sync no-overlay SNAT exemption nftables sets: %w", err)
						}
					}
				}
			}

			// On the DPU, the host (DPUHost) publishes its primary interface address via the
			// primary-dpu-host-addr annotation. When it changes we must rebuild
			// node-primary-ifaddr and l3-gateway-config from it: only the DPU holds the
			// OVS/OVN state (chassis, bridge, MAC, ofport) required to author l3-gateway-config,
			// so the host cannot do it. We piggyback on this existing local-node watch rather
			// than registering a separate node informer handler.
			if config.OvnKubeNode.Mode == types.NodeModeDPU && util.NodePrimaryDPUHostAddrAnnotationChanged(oldNode, newNode) {
				if gw, ok := h.nc.Gateway.(*gateway); ok && gw.nodeIPManager != nil {
					if err := gw.nodeIPManager.updateGatewayAddressAnnotations(); err != nil {
						return fmt.Errorf("failed to update gateway annotations after primary-dpu-host-addr change for node %s: %w", newNode.Name, err)
					}
				}
			}
			return nil
		}

		if (config.IsModeDPUHost() || config.IsModeFull()) && util.NodeHostCIDRsAnnotationChanged(oldNode, newNode) {
			// remote node that is changing
			// Use GetNodeAddresses to get new node IPs
			newIPsv4, newIPsv6, err := util.GetNodeAddresses(config.IPv4Mode, config.IPv6Mode, newNode)
			if err != nil {
				return fmt.Errorf("failed to get addresses for new node %q: %w", newNode.Name, err)
			}

			ipsToKeep := map[string]bool{}
			for _, nodeIP := range newIPsv4 {
				ipsToKeep[nodeIP.String()] = true
			}
			for _, nodeIP := range newIPsv6 {
				ipsToKeep[nodeIP.String()] = true
			}

			// Use GetNodeAddresses to get old node IPs
			oldIPsv4, oldIPsv6, err := util.GetNodeAddresses(config.IPv4Mode, config.IPv6Mode, oldNode)
			if err != nil {
				return fmt.Errorf("failed to get addresses for old node %q: %w", oldNode.Name, err)
			}

			ipsToRemove := make([]net.IP, 0)
			for _, nodeIP := range oldIPsv4 {
				if _, exists := ipsToKeep[nodeIP.String()]; !exists {
					ipsToRemove = append(ipsToRemove, nodeIP)
				}
			}
			for _, nodeIP := range oldIPsv6 {
				if _, exists := ipsToKeep[nodeIP.String()]; !exists {
					ipsToRemove = append(ipsToRemove, nodeIP)
				}
			}

			if err := removePMTUDNodeNFTRules(ipsToRemove); err != nil {
				return fmt.Errorf("error removing node %q stale NFT rules during update: %w", oldNode.Name, err)
			}
		}
		return h.nc.addOrUpdateNode(newNode)

	default:
		return fmt.Errorf("no update function for object type %s", h.objType)
	}
}

// Given a *RetryFramework instance, an object and optionally a cachedObj, DeleteResource
// deletes the object from the cluster according to the delete logic of its resource type.
// cachedObj is the internal cache entry for this object, used for now for pods and network
// policies.
func (h *nodeEventHandler) DeleteResource(obj, _ interface{}) error {
	switch h.objType {
	case factory.EndpointSliceForStaleConntrackRemovalType:
		endpointslice := obj.(*discovery.EndpointSlice)
		return h.nc.reconcileConntrackUponEndpointSliceEvents(endpointslice, nil)

	case factory.NodeType:
		h.nc.deleteNode(obj.(*corev1.Node))
		if config.IsModeDPUHost() || config.IsModeFull() {
			_ = managementport.UpdateNoSNATSubnetsSets(obj.(*corev1.Node), func(_ *corev1.Node) ([]string, error) {
				return []string{}, nil
			})
		}

		return nil

	default:
		return fmt.Errorf("no delete function for object type %s", h.objType)
	}
}

func (h *nodeEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {

		switch h.objType {
		case factory.EndpointSliceForStaleConntrackRemovalType:
			// Use syncEndpointSlices to process initial sync without triggering
			// false 0→N transitions that would flush conntrack on every restart
			syncFunc = h.nc.syncEndpointSlices
		case factory.NodeType:
			syncFunc = h.nc.syncNodes

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}
