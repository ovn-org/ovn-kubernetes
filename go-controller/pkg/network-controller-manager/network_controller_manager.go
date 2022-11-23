package networkControllerManager

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

// networkControllerManager structure is the object manages all controllers for all networks
type networkControllerManager struct {
	client       clientset.Interface
	kube         *kube.KubeOVN
	watchFactory *factory.WatchFactory
	podRecorder  *metrics.PodRecorder
	// event recorder used to post events to k8s
	recorder record.EventRecorder
	// libovsdb northbound client interface
	nbClient libovsdbclient.Client
	// libovsdb southbound client interface
	sbClient libovsdbclient.Client
	// has SCTP support
	SCTPSupport bool
	// Supports multicast?
	multicastSupport bool
	// Supports OVN Template Load Balancers?
	svcTemplateSupport bool
	// Is ACL logging enabled while configuring meters?
	aclLoggingEnabled bool

	stopChan chan struct{}
	wg       *sync.WaitGroup

	// unique identity for controllerManager running on different ovnkube-master instance,
	// used for leader election
	identity string

	defaultNetworkController nad.BaseNetworkController

	// net-attach-def controller handle net-attach-def and create/delete network controllers
	nadController *nad.NetAttachDefinitionController
}

func (cm *networkControllerManager) NewNetworkController(nInfo util.NetInfo,
	netConfInfo util.NetConfInfo) (nad.NetworkController, error) {
	cnci, err := cm.newCommonNetworkControllerInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to create network controller info %w", err)
	}
	topoType := netConfInfo.TopologyType()
	switch topoType {
	case ovntypes.Layer3Topology:
		return ovn.NewSecondaryLayer3NetworkController(cnci, nInfo, netConfInfo), nil
	case ovntypes.Layer2Topology:
		return ovn.NewSecondaryLayer2NetworkController(cnci, nInfo, netConfInfo), nil
	case ovntypes.LocalnetTopology:
		return ovn.NewSecondaryLocalnetNetworkController(cnci, nInfo, netConfInfo), nil
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// newDummyNetworkController creates a dummy network controller used to clean up specific network
func (cm *networkControllerManager) newDummyNetworkController(topoType, netName string) (nad.NetworkController, error) {
	cnci, err := cm.newCommonNetworkControllerInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to create network controller info %w", err)
	}
	netInfo := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: types.NetConf{Name: netName}, Topology: topoType})
	switch topoType {
	case ovntypes.Layer3Topology:
		return ovn.NewSecondaryLayer3NetworkController(cnci, netInfo, &util.Layer3NetConfInfo{}), nil
	case ovntypes.Layer2Topology:
		return ovn.NewSecondaryLayer2NetworkController(cnci, netInfo, &util.Layer2NetConfInfo{}), nil
	case ovntypes.LocalnetTopology:
		return ovn.NewSecondaryLocalnetNetworkController(cnci, netInfo, &util.LocalnetNetConfInfo{}), nil
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// Find all the OVN logical switches/routers for the secondary networks
func findAllSecondaryNetworkLogicalEntities(nbClient libovsdbclient.Client) ([]*nbdb.LogicalSwitch,
	[]*nbdb.LogicalRouter, error) {
	p1 := func(item *nbdb.LogicalSwitch) bool {
		_, ok := item.ExternalIDs[ovntypes.NetworkExternalID]
		return ok
	}
	nodeSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(nbClient, p1)
	if err != nil {
		klog.Errorf("Failed to get all logical switches of secondary network error: %v", err)
		return nil, nil, err
	}
	p2 := func(item *nbdb.LogicalRouter) bool {
		_, ok := item.ExternalIDs[ovntypes.NetworkExternalID]
		return ok
	}
	clusterRouters, err := libovsdbops.FindLogicalRoutersWithPredicate(nbClient, p2)
	if err != nil {
		klog.Errorf("Failed to get all distributed logical routers: %v", err)
		return nil, nil, err
	}
	return nodeSwitches, clusterRouters, nil
}

func (cm *networkControllerManager) CleanupDeletedNetworks(allControllers []nad.NetworkController) error {
	existingNetworksMap := map[string]struct{}{}
	for _, oc := range allControllers {
		existingNetworksMap[oc.GetNetworkName()] = struct{}{}
	}

	// Get all the existing secondary networks and its logical entities
	switches, routers, err := findAllSecondaryNetworkLogicalEntities(cm.nbClient)
	if err != nil {
		return err
	}

	staleNetworkControllers := map[string]nad.NetworkController{}
	for _, ls := range switches {
		netName := ls.ExternalIDs[ovntypes.NetworkExternalID]
		if _, ok := existingNetworksMap[netName]; ok {
			// network still exists, no cleanup to do
			continue
		}
		// TopologyExternalID always co-exists with NetworkExternalID
		topoType := ls.ExternalIDs[ovntypes.TopologyExternalID]
		// Create dummy network controllers to clean up logical entities
		klog.V(5).Infof("Found stale %s network %s", topoType, netName)
		if oc, err := cm.newDummyNetworkController(topoType, netName); err == nil {
			staleNetworkControllers[netName] = oc
			continue
		}
	}
	for _, lr := range routers {
		netName := lr.ExternalIDs[ovntypes.NetworkExternalID]
		if _, ok := existingNetworksMap[netName]; ok {
			// network still exists, no cleanup to do
			continue
		}
		// TopologyExternalID always co-exists with NetworkExternalID
		topoType := lr.ExternalIDs[ovntypes.TopologyExternalID]
		// Create dummy network controllers to clean up logical entities
		klog.V(5).Infof("Found stale %s network %s", topoType, netName)
		if oc, err := cm.newDummyNetworkController(topoType, netName); err == nil {
			staleNetworkControllers[netName] = oc
			continue
		}
	}

	for netName, oc := range staleNetworkControllers {
		klog.Infof("Cleanup entities for stale network %s", netName)
		err = oc.Cleanup(netName)
		if err != nil {
			klog.Errorf("Failed to delete stale OVN logical entities for network %s: %v", netName, err)
		}
	}
	return nil
}

// NewNetworkControllerManager creates a new OVN controller manager to manage all the controller for all networks
func NewNetworkControllerManager(ovnClient *util.OVNClientset, identity string, wf *factory.WatchFactory,
	libovsdbOvnNBClient libovsdbclient.Client, libovsdbOvnSBClient libovsdbclient.Client,
	recorder record.EventRecorder, wg *sync.WaitGroup) (*networkControllerManager, error) {
	podRecorder := metrics.NewPodRecorder()

	cm := &networkControllerManager{
		client: ovnClient.KubeClient,
		kube: &kube.KubeOVN{
			Kube:                 kube.Kube{KClient: ovnClient.KubeClient},
			EIPClient:            ovnClient.EgressIPClient,
			EgressFirewallClient: ovnClient.EgressFirewallClient,
			CloudNetworkClient:   ovnClient.CloudNetworkClient,
		},
		stopChan:     make(chan struct{}),
		watchFactory: wf,
		recorder:     recorder,
		nbClient:     libovsdbOvnNBClient,
		sbClient:     libovsdbOvnSBClient,
		podRecorder:  &podRecorder,

		wg:       wg,
		identity: identity,
	}

	var err error
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.nadController, err = nad.NewNetAttachDefinitionController("network-controller-manager", cm, ovnClient.NetworkAttchDefClient, cm.recorder)
		if err != nil {
			return nil, err
		}
	}
	return cm, nil
}

func (cm *networkControllerManager) configureSCTPSupport() error {
	hasSCTPSupport, err := util.DetectSCTPSupport()
	if err != nil {
		return err
	}

	if !hasSCTPSupport {
		klog.Warningf("SCTP unsupported by this version of OVN. Kubernetes service creation with SCTP will not work ")
	} else {
		klog.Info("SCTP support detected in OVN")
	}
	cm.SCTPSupport = hasSCTPSupport
	return nil
}

func (cm *networkControllerManager) configureMulticastSupport() {
	cm.multicastSupport = config.EnableMulticast
	if cm.multicastSupport {
		if _, _, err := util.RunOVNSbctl("--columns=_uuid", "list", "IGMP_Group"); err != nil {
			klog.Warningf("Multicast support enabled, however version of OVN in use does not support IGMP Group. " +
				"Disabling Multicast Support")
			cm.multicastSupport = false
		}
	}
}

func (cm *networkControllerManager) configureSvcTemplateSupport() {
	if _, _, err := util.RunOVNNbctl("--columns=_uuid", "list", "Chassis_Template_Var"); err != nil {
		klog.Warningf("Version of OVN in use does not support Chassis_Template_Var. " +
			"Disabling Templates Support")
		cm.svcTemplateSupport = false
	} else {
		cm.svcTemplateSupport = true
	}
}

// enableOVNLogicalDataPathGroups sets an OVN flag to enable logical datapath
// groups on OVN 20.12 and later. The option is ignored if OVN doesn't
// understand it. Logical datapath groups reduce the size of the southbound
// database in large clusters. ovn-controllers should be upgraded to a version
// that supports them before the option is turned on by the master.
func (cm *networkControllerManager) enableOVNLogicalDataPathGroups() error {
	nbGlobal := nbdb.NBGlobal{
		Options: map[string]string{"use_logical_dp_groups": "true"},
	}
	if err := libovsdbops.UpdateNBGlobalSetOptions(cm.nbClient, &nbGlobal); err != nil {
		return fmt.Errorf("failed to set NB global option to enable logical datapath groups: %v", err)
	}
	return nil
}

func (cm *networkControllerManager) configureMetrics(stopChan <-chan struct{}) {
	metrics.RegisterMasterPerformance(cm.nbClient)
	metrics.RegisterMasterFunctional()
	metrics.RunTimestamp(stopChan, cm.sbClient, cm.nbClient)
	metrics.MonitorIPSec(cm.nbClient)
}

func (cm *networkControllerManager) createACLLoggingMeter() error {
	band := &nbdb.MeterBand{
		Action: ovntypes.MeterAction,
		Rate:   config.Logging.ACLLoggingRateLimit,
	}
	ops, err := libovsdbops.CreateMeterBandOps(cm.nbClient, nil, band)
	if err != nil {
		return fmt.Errorf("can't create meter band %v: %v", band, err)
	}

	meterFairness := true
	meter := &nbdb.Meter{
		Name: ovntypes.OvnACLLoggingMeter,
		Fair: &meterFairness,
		Unit: ovntypes.PacketsPerSecond,
	}
	ops, err = libovsdbops.CreateOrUpdateMeterOps(cm.nbClient, ops, meter, []*nbdb.MeterBand{band},
		&meter.Bands, &meter.Fair, &meter.Unit)
	if err != nil {
		return fmt.Errorf("can't create meter %v: %v", meter, err)
	}

	_, err = libovsdbops.TransactAndCheck(cm.nbClient, ops)
	if err != nil {
		return fmt.Errorf("can't transact ACL logging meter: %v", err)
	}

	return nil
}

func (cm *networkControllerManager) enableACLLoggingSupport() {
	cm.aclLoggingEnabled = true
	if err := cm.createACLLoggingMeter(); err != nil {
		klog.Warningf("ACL logging support enabled, however acl-logging meter could not be created: %v. "+
			"Disabling ACL logging support", err)
		cm.aclLoggingEnabled = false
	}
}

// newCommonNetworkControllerInfo creates and returns the common networkController info
func (cm *networkControllerManager) newCommonNetworkControllerInfo() (*ovn.CommonNetworkControllerInfo, error) {
	return ovn.NewCommonNetworkControllerInfo(cm.client, cm.kube, cm.watchFactory, cm.recorder, cm.nbClient,
		cm.sbClient, cm.podRecorder, cm.SCTPSupport, cm.multicastSupport, cm.svcTemplateSupport, cm.aclLoggingEnabled)
}

// initDefaultNetworkController creates the controller for default network
func (cm *networkControllerManager) initDefaultNetworkController() error {
	defaultController, err := ovn.NewDefaultNetworkController(cm.newCommonNetworkControllerInfo())
	if err != nil {
		return err
	}
	// Make sure we only set defaultNetworkController in case of no error,
	// otherwise we would initialize the interface with a nil implementation
	// which is not the same as nil interface.
	cm.defaultNetworkController = defaultController
	return nil
}

// Start the network controller manager
func (cm *networkControllerManager) Start(ctx context.Context) error {
	klog.Info("Starting the network controller manager")
	cm.configureMetrics(cm.stopChan)

	err := cm.configureSCTPSupport()
	if err != nil {
		return err
	}

	cm.configureMulticastSupport()
	cm.configureSvcTemplateSupport()
	cm.enableACLLoggingSupport()

	err = cm.enableOVNLogicalDataPathGroups()
	if err != nil {
		return err
	}

	if config.Metrics.EnableConfigDuration {
		// with k=10,
		//  for a cluster with 10 nodes, measurement of 1 in every 100 requests
		//  for a cluster with 100 nodes, measurement of 1 in every 1000 requests
		metrics.GetConfigDurationRecorder().Run(cm.nbClient, cm.kube, 10, time.Second*5, cm.stopChan)
	}
	cm.podRecorder.Run(cm.sbClient, cm.stopChan)

	err = cm.watchFactory.Start()
	if err != nil {
		return err
	}

	err = cm.initDefaultNetworkController()
	if err != nil {
		return fmt.Errorf("failed to init default network controller: %v", err)
	}
	err = cm.defaultNetworkController.Start(ctx)
	if err != nil {
		return fmt.Errorf("failed to start default network controller: %v", err)
	}

	// nadController is nil if multi-network is disabled
	if cm.nadController != nil {
		return cm.nadController.Start()
	}

	return nil
}

// Stop gracefully stops all managed controllers
func (cm *networkControllerManager) Stop() {
	// stop metric recorders
	close(cm.stopChan)

	// stop the default network controller
	if cm.defaultNetworkController != nil {
		cm.defaultNetworkController.Stop()
	}

	// stop the NAD controller
	if cm.nadController != nil {
		cm.nadController.Stop()
	}
}
