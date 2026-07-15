// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"net"
	"sync"
	"time"

	listers "k8s.io/client-go/listers/core/v1"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	hotypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
)

// NodeController is the node hybrid overlay controller.
// This controller is running in ovnkube-node binary. It's responsible for local
// node configuration and annotation.
type NodeController struct {
	kube      kube.Interface
	ovsClient libovsdbclient.Client
	sync.RWMutex
	nodeName  string
	initState hotypes.HybridInitState
	drMAC     net.HardwareAddr
	drIP      net.IP
	gwLRPIP   net.IP
	vxlanPort uint16
	// contains a map of pods to corresponding tunnels
	flowCache map[string]*flowCacheEntry
	flowMutex sync.Mutex
	// channel to indicate we need to update flows immediately
	flowChan            chan struct{}
	flowCacheSyncPeriod time.Duration

	nodeLister     listers.NodeLister
	localPodLister listers.PodLister
}

// newNodeController returns a new node controller that runs on linux.
// On linux there's two controller implementations. One runs from
// ovnkube-node binary and the other from HO node binary and they
// prepare the OVN and SDN nodes for the HO tunnel respectively.
// so that Add/Update/Delete events are appropriately handled.
// It initializes the node it is currently running on.
// On ovn nodes, this means:
//  1. Setting up a VXLAN gateway and hooking to the OVN gateway
//  2. Setting back annotations about its VTEP and gateway MAC address to its own object
//
// On hybrid overlay nodes, this means:
//  1. Add Hybrid Overlay DRMAC annotation to its own node object,
//  2. Remove the ovnkube pod annotations from the pods running on this node.
func newNodeController(
	kube kube.Interface,
	nodeName string,
	nodeLister listers.NodeLister,
	localPodLister listers.PodLister,
	isHONode bool,
	ovsClient libovsdbclient.Client,
) (nodeController, error) {
	if isHONode {
		return newHONodeController(kube, nodeName)
	}
	return newOVNNodeController(kube, nodeName, nodeLister, localPodLister, ovsClient)
}
