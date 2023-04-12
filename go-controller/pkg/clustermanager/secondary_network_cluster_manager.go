package clustermanager

import (
	"fmt"

	"github.com/containernetworking/cni/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// Maximum secondary network IDs that can be generated. An arbitrary value is chosen.
	maxSecondaryNetworkIDs = 4096
)

// secondaryNetworkClusterManager object manages the multi net-attach-def controllers.
// It implements networkAttachDefController.NetworkControllerManager and can be used
// by NetAttachDefinitionController to add and delete NADs.
type secondaryNetworkClusterManager struct {
	// net-attach-def controller handle net-attach-def and create/delete network controllers
	nadController *nad.NetAttachDefinitionController
	ovnClient     *util.OVNClusterManagerClientset
	watchFactory  *factory.WatchFactory
	// networkIDAllocator is used to allocate a unique ID for each secondary layer3 network
	networkIDAllocator *idAllocator
}

func newSecondaryNetworkClusterManager(ovnClient *util.OVNClusterManagerClientset,
	wf *factory.WatchFactory, recorder record.EventRecorder) (*secondaryNetworkClusterManager, error) {
	klog.Infof("Creating secondary network cluster manager")
	networkIDAllocator, err := NewIDAllocator("NetworkIDs", maxSecondaryNetworkIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to create an IdAllocator for the secondary network ids, err: %v", err)
	}

	// Reserve the id 0 for the default network.
	if err := networkIDAllocator.reserveID("default", defaultNetworkID); err != nil {
		return nil, fmt.Errorf("idAllocator failed to reserve defaultNetworkID %d", defaultNetworkID)
	}
	sncm := &secondaryNetworkClusterManager{
		ovnClient:          ovnClient,
		watchFactory:       wf,
		networkIDAllocator: networkIDAllocator,
	}

	sncm.nadController, err = nad.NewNetAttachDefinitionController(
		"cluster-manager", sncm, ovnClient.NetworkAttchDefClient, recorder)
	if err != nil {
		return nil, err
	}
	return sncm, nil
}

// Start the secondary layer3 controller, handles all events and creates all
// needed logical entities
func (sncm *secondaryNetworkClusterManager) Start() error {
	klog.Infof("Starting secondary network cluster manager")

	// Reserve the network ids in the id allocator for the existing secondary layer3 networks.
	nodes, err := sncm.watchFactory.GetNodes()
	if err != nil {
		return fmt.Errorf("error getting the nodes from the watch factory : err - %v", err)
	}

	for _, n := range nodes {
		networkIdsMap, err := util.GetNodeNetworkIDsAnnotationNetworkIDs(n)
		if err == nil {
			for networkName, id := range networkIdsMap {
				// Reserver the id for the network name. We can safely
				// ignore any errors if there are duplicate ids or if
				// two networks have the same id. We will resync the node
				// annotations correctly when the network controller
				// is created.
				_ = sncm.networkIDAllocator.reserveID(networkName, id)
			}
		}
	}
	return sncm.nadController.Start()
}

func (sncm *secondaryNetworkClusterManager) Stop() {
	klog.Infof("Stopping secondary network cluster manager")
	sncm.nadController.Stop()
}

// NewNetworkController implements the networkAttachDefController.NetworkControllerManager
// interface function.  This function is called by the net-attach-def controller when
// a layer2 or layer3 secondary network is created.  Layer2 type is not handled here.
func (sncm *secondaryNetworkClusterManager) NewNetworkController(nInfo util.NetInfo) (nad.NetworkController, error) {
	topoType := nInfo.TopologyType()
	if topoType == ovntypes.Layer3Topology {
		networkId, err := sncm.networkIDAllocator.allocateID(nInfo.GetNetworkName())
		if err != nil {
			return nil, fmt.Errorf("failed to create NetworkController for secondary layer3 network %s : %w", nInfo.GetNetworkName(), err)
		}

		sncc := newNetworkClusterController(nInfo.GetNetworkName(), networkId, nInfo.Subnets(),
			sncm.ovnClient, sncm.watchFactory, false, nInfo)
		return sncc, nil
	}

	// Secondary network cluster manager doesn't manage other topology types
	return nil, nad.ErrNetworkControllerTopologyNotManaged
}

// CleanupDeletedNetworks implements the networkAttachDefController.NetworkControllerManager
// interface function.
func (sncm *secondaryNetworkClusterManager) CleanupDeletedNetworks(allControllers []nad.NetworkController) error {
	existingNetworksMap := map[string]struct{}{}
	for _, oc := range allControllers {
		existingNetworksMap[oc.GetNetworkName()] = struct{}{}
	}

	staleNetworkControllers := map[string]nad.NetworkController{}
	existingNodes, err := sncm.watchFactory.GetNodes()
	if err != nil {
		return err
	}

	for _, node := range existingNodes {
		nodeNetworks, err := util.GetNodeSubnetAnnotationNetworkNames(node)
		if err != nil {
			continue
		}

		for i := range nodeNetworks {
			netName := nodeNetworks[i]
			if netName == ovntypes.DefaultNetworkName {
				continue
			}

			if _, ok := existingNetworksMap[netName]; ok {
				// network still exists, no cleanup to do
				continue
			}

			if _, ok := staleNetworkControllers[netName]; ok {
				// dummy controller already created for the stale network
				continue
			}

			oc := sncm.newDummyLayer3NetworkController(netName)
			staleNetworkControllers[netName] = oc
		}
	}

	for netName, oc := range staleNetworkControllers {
		klog.Infof("Cleanup subnet annotation for stale network %s", netName)
		err = oc.Cleanup(netName)
		if err != nil {
			klog.Errorf("Failed to delete stale subnet annotation for network %s: %v", netName, err)
		}
	}
	return nil
}

// newDummyNetworkController creates a dummy network controller used to clean up specific network
func (sncm *secondaryNetworkClusterManager) newDummyLayer3NetworkController(netName string) nad.NetworkController {
	netInfo, _ := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: types.NetConf{Name: netName}, Topology: ovntypes.Layer3Topology})
	return newNetworkClusterController(netInfo.GetNetworkName(), util.InvalidNetworkID, nil, sncm.ovnClient, sncm.watchFactory,
		false, netInfo)
}
