package clustermanager

import (
	"fmt"

	"github.com/containernetworking/cni/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// Maximum secondary network IDs that can be generated. An arbitrary value is chosen.
	maxSecondaryNetworkIDs = 4096
)

// secondaryNetworkClusterManager object manages the multi net-attach-def controllers.
// It implements networkmanager.ControllerManager interface and can be used
// by network manager to create and delete network controllers.
type secondaryNetworkClusterManager struct {
	// networkManager creates and deletes network controllers
	networkManager networkmanager.Interface
	ovnClient      *util.OVNClusterManagerClientset
	watchFactory   *factory.WatchFactory
	// networkIDAllocator is used to allocate a unique ID for each secondary layer3 network
	networkIDAllocator id.Allocator

	// event recorder used to post events to k8s
	recorder record.EventRecorder

	errorReporter NetworkStatusReporter
}

func newSecondaryNetworkClusterManager(
	ovnClient *util.OVNClusterManagerClientset,
	wf *factory.WatchFactory,
	networkManager networkmanager.Interface,
	recorder record.EventRecorder,
) (*secondaryNetworkClusterManager, error) {
	klog.Infof("Creating secondary network cluster manager")
	networkIDAllocator := id.NewIDAllocator("NetworkIDs", maxSecondaryNetworkIDs)
	// Reserve the id 0 for the default network.
	if err := networkIDAllocator.ReserveID(ovntypes.DefaultNetworkName, defaultNetworkID); err != nil {
		return nil, fmt.Errorf("idAllocator failed to reserve defaultNetworkID %d", defaultNetworkID)
	}
	sncm := &secondaryNetworkClusterManager{
		ovnClient:          ovnClient,
		watchFactory:       wf,
		networkIDAllocator: networkIDAllocator,
		networkManager:     networkManager,
		recorder:           recorder,
	}
	return sncm, nil
}

func (sncm *secondaryNetworkClusterManager) SetNetworkStatusReporter(errorReporter NetworkStatusReporter) {
	sncm.errorReporter = errorReporter
}

// Start the secondary network controller, handles all events and creates all
// needed logical entities
func (sncm *secondaryNetworkClusterManager) Start() error {
	klog.Infof("Starting secondary network cluster manager")
	return sncm.init()
}

func (sncm *secondaryNetworkClusterManager) init() error {
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
				_ = sncm.networkIDAllocator.ReserveID(networkName, id)
			}
		}
	}

	return nil
}

func (sncm *secondaryNetworkClusterManager) Stop() {
	klog.Infof("Stopping secondary network cluster manager")
}

func (sncm *secondaryNetworkClusterManager) GetDefaultNetworkController() networkmanager.ReconcilableNetworkController {
	return nil
}

// NewNetworkController implements the networkmanager.ControllerManager
// interface called by network manager to create or delete a network controller.
func (sncm *secondaryNetworkClusterManager) NewNetworkController(nInfo util.NetInfo) (networkmanager.NetworkController, error) {
	if !sncm.isTopologyManaged(nInfo) {
		return nil, networkmanager.ErrNetworkControllerTopologyNotManaged
	}

	klog.Infof("Creating new network controller for network %s of topology %s", nInfo.GetNetworkName(), nInfo.TopologyType())

	namedIDAllocator := sncm.networkIDAllocator.ForName(nInfo.GetNetworkName())
	sncc := newNetworkClusterController(
		namedIDAllocator,
		nInfo,
		sncm.ovnClient,
		sncm.watchFactory.ShallowClone(),
		sncm.recorder,
		sncm.networkManager,
		sncm.errorReporter,
	)
	return sncc, nil
}

func (sncm *secondaryNetworkClusterManager) isTopologyManaged(nInfo util.NetInfo) bool {
	switch nInfo.TopologyType() {
	case ovntypes.Layer3Topology:
		// we need to allocate subnets to each node regardless of configuration
		return true
	case ovntypes.Layer2Topology:
		// for IC, pod IPs and tunnel IDs need to be allocated
		// in non IC config, this is done from ovnkube-master network controller
		return config.OVNKubernetesFeature.EnableInterconnect
	case ovntypes.LocalnetTopology:
		// for IC, pod IPs need to be allocated
		// in non IC config, this is done from ovnkube-master network controller
		return config.OVNKubernetesFeature.EnableInterconnect && len(nInfo.Subnets()) > 0
	}
	return false
}

// CleanupStaleNetworks cleans of stale data from the OVN database
// corresponding to networks not included in validNetworks, which are considered
// stale.
func (sncm *secondaryNetworkClusterManager) CleanupStaleNetworks(validNetworks ...util.NetInfo) error {
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

			oc, err := sncm.newDummyLayer3NetworkController(netName)
			if err != nil {
				klog.Errorf("Failed to delete stale subnet annotation for network %s: %v", netName, err)
				continue
			}
			staleNetworkControllers[netName] = oc
		}
	}

	for netName, oc := range staleNetworkControllers {
		klog.Infof("Cleanup subnet annotation for stale network %s", netName)
		err = oc.Cleanup()
		if err != nil {
			klog.Errorf("Failed to delete stale subnet annotation for network %s: %v", netName, err)
		}
	}
	return nil
}

// newDummyNetworkController creates a dummy network controller used to clean up specific network
func (sncm *secondaryNetworkClusterManager) newDummyLayer3NetworkController(netName string) (networkmanager.NetworkController, error) {
	netInfo, _ := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: types.NetConf{Name: netName}, Topology: ovntypes.Layer3Topology})
	namedIDAllocator := sncm.networkIDAllocator.ForName(netInfo.GetNetworkName())
	nc := newNetworkClusterController(
		namedIDAllocator,
		netInfo,
		sncm.ovnClient,
		sncm.watchFactory,
		sncm.recorder,
		sncm.networkManager,
		nil,
	)
	err := nc.init()
	return nc, err
}
