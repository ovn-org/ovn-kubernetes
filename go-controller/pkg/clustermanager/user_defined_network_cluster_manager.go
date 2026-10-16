// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	cnitypes "github.com/containernetworking/cni/pkg/types"

	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/node"
	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// userDefinedNetworkClusterManager object manages the multi net-attach-def controllers.
// It implements networkmanager.ControllerManager interface and can be used
// by network manager to create and delete network controllers.
type userDefinedNetworkClusterManager struct {
	// networkManager creates and deletes network controllers
	networkManager networkmanager.Interface
	ovnClient      *util.OVNClusterManagerClientset
	watchFactory   *factory.WatchFactory

	// event recorder used to post events to k8s
	recorder record.EventRecorder

	errorReporter  NetworkStatusReporter
	nodeReconciler *nodecontroller.NodeController

	// nodeAnnotationBatcher batches node annotation updates across networks
	nodeAnnotationBatcher *node.NodeAnnotationBatcher
}

func newUserDefinedNetworkClusterManager(
	ovnClient *util.OVNClusterManagerClientset,
	wf *factory.WatchFactory,
	networkManager networkmanager.Interface,
	recorder record.EventRecorder,
	nodeReconciler *nodecontroller.NodeController,
	batcher *node.NodeAnnotationBatcher,
) (*userDefinedNetworkClusterManager, error) {
	klog.Infof("Creating user-defined network cluster manager")
	sncm := &userDefinedNetworkClusterManager{
		ovnClient:             ovnClient,
		watchFactory:          wf,
		networkManager:        networkManager,
		recorder:              recorder,
		nodeReconciler:        nodeReconciler,
		nodeAnnotationBatcher: batcher,
	}
	return sncm, nil
}

func (sncm *userDefinedNetworkClusterManager) SetNetworkStatusReporter(errorReporter NetworkStatusReporter) {
	sncm.errorReporter = errorReporter
}

func (sncm *userDefinedNetworkClusterManager) GetDefaultNetworkController() networkmanager.ReconcilableNetworkController {
	return nil
}

// NewNetworkController implements the networkmanager.ControllerManager
// interface called by network manager to create or delete a network controller.
func (sncm *userDefinedNetworkClusterManager) NewNetworkController(nInfo util.NetInfo) (networkmanager.NetworkController, error) {
	if !sncm.isTopologyManaged(nInfo) {
		return nil, networkmanager.ErrNetworkControllerTopologyNotManaged
	}

	klog.Infof("Creating new network controller for network %s of topology %s", nInfo.GetNetworkName(), nInfo.TopologyType())

	sncc := newNetworkClusterController(
		nInfo,
		sncm.ovnClient,
		sncm.watchFactory.ShallowClone(),
		sncm.recorder,
		sncm.networkManager,
		sncm.errorReporter,
		sncm.nodeReconciler,
		sncm.nodeAnnotationBatcher,
	)
	return sncc, nil
}

func (sncm *userDefinedNetworkClusterManager) isTopologyManaged(nInfo util.NetInfo) bool {
	switch nInfo.TopologyType() {
	case ovntypes.Layer3Topology:
		// we need to allocate subnets to each node regardless of configuration
		return true
	case ovntypes.Layer2Topology:
		// pod IPs and tunnel IDs are allocated by cluster-manager
		return true
	case ovntypes.LocalnetTopology:
		// pod IPs are allocated by cluster-manager if there is IPAM
		return len(nInfo.Subnets()) > 0
	}
	return false
}

// CleanupStaleNetworks cleans up stale node annotations (node-subnets, network-ids,
// tunnel IDs) for networks not included in validNetworks.
// Stale network names are discovered from per-network annotation sources:
//   - k8s.ovn.org/node-subnets (covers L3 UDNs)
//   - k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids (covers L2 primary UDNs with IC)
func (sncm *userDefinedNetworkClusterManager) CleanupStaleNetworks(validNetworks ...util.NetInfo) error {
	existingNetworksMap := map[string]struct{}{}
	for _, network := range validNetworks {
		existingNetworksMap[network.GetNetworkName()] = struct{}{}
	}

	staleNetworkControllers := map[string]networkmanager.NetworkController{}
	existingNodes, err := sncm.watchFactory.GetNodes()
	if err != nil {
		return err
	}

	for _, node := range existingNodes {
		// node-subnets covers L3 UDNs
		nodeNetworks, err := util.GetNodeSubnetAnnotationNetworkNames(node)
		if err != nil {
			nodeNetworks = nil
		}
		for _, netName := range nodeNetworks {
			if netName == ovntypes.DefaultNetworkName {
				continue
			}
			if _, ok := existingNetworksMap[netName]; ok {
				continue
			}
			if _, ok := staleNetworkControllers[netName]; ok {
				continue
			}
			oc, err := sncm.newDummyNetworkController(ovntypes.Layer3Topology, netName)
			if err != nil {
				klog.Errorf("Failed to create dummy controller for stale network %s: %v", netName, err)
				continue
			}
			staleNetworkControllers[netName] = oc
		}

		// network-ids covers L2 secondary UDNs (and others)
		networkIDsMap, err := util.GetNodeNetworkIDsAnnotationNetworkIDs(node)
		if err != nil {
			networkIDsMap = nil
		}
		for netName := range networkIDsMap {
			if netName == ovntypes.DefaultNetworkName {
				continue
			}
			if _, ok := existingNetworksMap[netName]; ok {
				continue
			}
			if _, ok := staleNetworkControllers[netName]; ok {
				continue
			}
			oc, err := sncm.newDummyNetworkController(ovntypes.Layer2Topology, netName)
			if err != nil {
				klog.Errorf("Failed to create dummy controller for stale network %s: %v", netName, err)
				continue
			}
			staleNetworkControllers[netName] = oc
		}

		// tunnel IDs cover L2 primary UDNs with IC
		tunnelNetworks, err := util.GetNodeUDNLayer2TunnelIDAnnotationNetworkNames(node)
		if err != nil {
			tunnelNetworks = nil
		}
		for _, netName := range tunnelNetworks {
			if netName == ovntypes.DefaultNetworkName {
				continue
			}
			if _, ok := existingNetworksMap[netName]; ok {
				continue
			}
			if _, ok := staleNetworkControllers[netName]; ok {
				continue
			}
			oc, err := sncm.newDummyNetworkController(ovntypes.Layer2Topology, netName)
			if err != nil {
				klog.Errorf("Failed to create dummy controller for stale network %s: %v", netName, err)
				continue
			}
			staleNetworkControllers[netName] = oc
		}
	}

	for netName, oc := range staleNetworkControllers {
		klog.Infof("Cleanup stale network %s", netName)
		err = oc.Cleanup()
		if err != nil {
			klog.Errorf("Failed to clean up stale network %s: %v", netName, err)
		}
	}
	return nil
}

// newDummyNetworkController creates a minimal network controller used only to
// clean up stale node annotations for the given network. It skips the full
// init() path and only sets up what Cleanup() requires: a nodeAllocator.
func (sncm *userDefinedNetworkClusterManager) newDummyNetworkController(topoType, netName string) (networkmanager.NetworkController, error) {
	netInfo, _ := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: cnitypes.NetConf{Name: netName}, Topology: topoType})
	nc := newNetworkClusterController(
		netInfo,
		sncm.ovnClient,
		sncm.watchFactory,
		sncm.recorder,
		sncm.networkManager,
		nil,
		sncm.nodeReconciler,
		sncm.nodeAnnotationBatcher,
	)
	if nc.hasNodeAllocation() {
		nc.nodeAllocator = node.NewNodeAllocator(ovntypes.InvalidID, nc.GetNetInfo(), nc.watchFactory.NodeCoreInformer().Lister(), nc.kube, nc.nodeClient, nil, sncm.nodeAnnotationBatcher)
	}
	return nc, nil
}
