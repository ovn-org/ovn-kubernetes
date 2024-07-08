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
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/observability"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

// NetworkControllerManager structure is the object manages all controllers for all networks
type NetworkControllerManager struct {
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

	stopChan chan struct{}
	wg       *sync.WaitGroup

	defaultNetworkController nad.BaseNetworkController

	// net-attach-def controller handle net-attach-def and create/delete network controllers
	nadController *nad.NetAttachDefinitionController
}

func (cm *NetworkControllerManager) NewNetworkController(nInfo util.NetInfo) (nad.NetworkController, error) {
	cnci, err := cm.newCommonNetworkControllerInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to create network controller info %w", err)
	}
	topoType := nInfo.TopologyType()
	switch topoType {
	case ovntypes.Layer3Topology:
		return ovn.NewSecondaryLayer3NetworkController(cnci, nInfo, cm.nadController)
	case ovntypes.Layer2Topology:
		return ovn.NewSecondaryLayer2NetworkController(cnci, nInfo, cm.nadController)
	case ovntypes.LocalnetTopology:
		return ovn.NewSecondaryLocalnetNetworkController(cnci, nInfo, cm.nadController), nil
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// newDummyNetworkController creates a dummy network controller used to clean up specific network
func (cm *NetworkControllerManager) newDummyNetworkController(topoType, netName string) (nad.NetworkController, error) {
	cnci, err := cm.newCommonNetworkControllerInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to create network controller info %w", err)
	}
	netInfo, _ := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: types.NetConf{Name: netName}, Topology: topoType})
	switch topoType {
	case ovntypes.Layer3Topology:
		return ovn.NewSecondaryLayer3NetworkController(cnci, netInfo, cm.nadController)
	case ovntypes.Layer2Topology:
		return ovn.NewSecondaryLayer2NetworkController(cnci, netInfo, cm.nadController)
	case ovntypes.LocalnetTopology:
		return ovn.NewSecondaryLocalnetNetworkController(cnci, netInfo, cm.nadController), nil
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// Find all the OVN logical switches/routers for the secondary networks
func findAllSecondaryNetworkLogicalEntities(nbClient libovsdbclient.Client) ([]*nbdb.LogicalSwitch,
	[]*nbdb.LogicalRouter, error) {

	belongsToSecondaryNetwork := func(externalIDs map[string]string) bool {
		_, hasNetworkExternalID := externalIDs[ovntypes.NetworkExternalID]
		networkRole, hasNetworkRoleExternalID := externalIDs[ovntypes.NetworkRoleExternalID]
		return hasNetworkExternalID && hasNetworkRoleExternalID && networkRole == ovntypes.NetworkRoleSecondary
	}

	p1 := func(item *nbdb.LogicalSwitch) bool {
		return belongsToSecondaryNetwork(item.ExternalIDs)
	}
	nodeSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(nbClient, p1)
	if err != nil {
		klog.Errorf("Failed to get all logical switches of secondary network error: %v", err)
		return nil, nil, err
	}
	p2 := func(item *nbdb.LogicalRouter) bool {
		return belongsToSecondaryNetwork(item.ExternalIDs)
	}
	clusterRouters, err := libovsdbops.FindLogicalRoutersWithPredicate(nbClient, p2)
	if err != nil {
		klog.Errorf("Failed to get all distributed logical routers: %v", err)
		return nil, nil, err
	}
	return nodeSwitches, clusterRouters, nil
}

func (cm *NetworkControllerManager) CleanupDeletedNetworks(validNetworks ...util.BasicNetInfo) error {
	existingNetworksMap := map[string]string{}
	for _, network := range validNetworks {
		existingNetworksMap[network.GetNetworkName()] = network.TopologyType()
	}

	// Get all the existing secondary networks and its logical entities
	switches, routers, err := findAllSecondaryNetworkLogicalEntities(cm.nbClient)
	if err != nil {
		return err
	}

	staleNetworkControllers := map[string]nad.NetworkController{}
	for _, ls := range switches {
		netName := ls.ExternalIDs[ovntypes.NetworkExternalID]
		// TopologyExternalID always co-exists with NetworkExternalID
		topoType := ls.ExternalIDs[ovntypes.TopologyExternalID]
		if existingNetworksMap[netName] == topoType {
			// network still exists, no cleanup to do
			continue
		}
		// Create dummy network controllers to clean up logical entities
		klog.V(5).Infof("Found stale %s network %s", topoType, netName)
		if oc, err := cm.newDummyNetworkController(topoType, netName); err == nil {
			staleNetworkControllers[netName] = oc
			continue
		}
	}
	for _, lr := range routers {
		netName := lr.ExternalIDs[ovntypes.NetworkExternalID]
		// TopologyExternalID always co-exists with NetworkExternalID
		topoType := lr.ExternalIDs[ovntypes.TopologyExternalID]
		if existingNetworksMap[netName] == topoType {
			// network still exists, no cleanup to do
			continue
		}
		// Create dummy network controllers to clean up logical entities
		klog.V(5).Infof("Found stale %s network %s", topoType, netName)
		if oc, err := cm.newDummyNetworkController(topoType, netName); err == nil {
			staleNetworkControllers[netName] = oc
			continue
		}
	}

	for netName, oc := range staleNetworkControllers {
		klog.Infof("Cleanup entities for stale network %s", netName)
		err = oc.Cleanup()
		if err != nil {
			klog.Errorf("Failed to delete stale OVN logical entities for network %s: %v", netName, err)
		}
	}
	return nil
}

// NewNetworkControllerManager creates a new ovnkube controller manager to manage all the controller for all networks
func NewNetworkControllerManager(ovnClient *util.OVNClientset, wf *factory.WatchFactory,
	libovsdbOvnNBClient libovsdbclient.Client, libovsdbOvnSBClient libovsdbclient.Client,
	recorder record.EventRecorder, wg *sync.WaitGroup) (*NetworkControllerManager, error) {
	podRecorder := metrics.NewPodRecorder()

	cm := &NetworkControllerManager{
		client: ovnClient.KubeClient,
		kube: &kube.KubeOVN{
			Kube:                 kube.Kube{KClient: ovnClient.KubeClient},
			ANPClient:            ovnClient.ANPClient,
			EIPClient:            ovnClient.EgressIPClient,
			EgressFirewallClient: ovnClient.EgressFirewallClient,
			CloudNetworkClient:   ovnClient.CloudNetworkClient,
			EgressServiceClient:  ovnClient.EgressServiceClient,
			APBRouteClient:       ovnClient.AdminPolicyRouteClient,
			EgressQoSClient:      ovnClient.EgressQoSClient,
			IPAMClaimsClient:     ovnClient.IPAMClaimsClient,
			NetworkQoSClient:     ovnClient.NetworkQoSClient,
		},
		stopChan:     make(chan struct{}),
		watchFactory: wf,
		recorder:     recorder,
		nbClient:     libovsdbOvnNBClient,
		sbClient:     libovsdbOvnSBClient,
		podRecorder:  &podRecorder,

		wg:               wg,
		multicastSupport: config.EnableMulticast,
	}

	var err error
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.nadController, err = nad.NewNetAttachDefinitionController("network-controller-manager", cm, wf, nil)
		if err != nil {
			return nil, err
		}
	}
	return cm, nil
}

func (cm *NetworkControllerManager) configureSCTPSupport() error {
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

func (cm *NetworkControllerManager) configureSvcTemplateSupport() {
	if !config.OVNKubernetesFeature.EnableServiceTemplateSupport {
		cm.svcTemplateSupport = false
	} else if _, _, err := util.RunOVNNbctl("--columns=_uuid", "list", "Chassis_Template_Var"); err != nil {
		klog.Warningf("Version of OVN in use does not support Chassis_Template_Var. " +
			"Disabling Templates Support")
		cm.svcTemplateSupport = false
	} else {
		cm.svcTemplateSupport = true
	}
}

func (cm *NetworkControllerManager) configureMetrics(stopChan <-chan struct{}) {
	metrics.RegisterOVNKubeControllerPerformance(cm.nbClient)
	metrics.RegisterOVNKubeControllerFunctional(stopChan)
	metrics.RunTimestamp(stopChan, cm.sbClient, cm.nbClient)
	metrics.MonitorIPSec(cm.nbClient)
}

func (cm *NetworkControllerManager) createACLLoggingMeter() error {
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

// newCommonNetworkControllerInfo creates and returns the common networkController info
func (cm *NetworkControllerManager) newCommonNetworkControllerInfo() (*ovn.CommonNetworkControllerInfo, error) {
	return ovn.NewCommonNetworkControllerInfo(cm.client, cm.kube, cm.watchFactory, cm.recorder, cm.nbClient,
		cm.sbClient, cm.podRecorder, cm.SCTPSupport, cm.multicastSupport, cm.svcTemplateSupport)
}

// initDefaultNetworkController creates the controller for default network
func (cm *NetworkControllerManager) initDefaultNetworkController(nadController *nad.NetAttachDefinitionController,
	observManager *observability.Manager) error {
	cnci, err := cm.newCommonNetworkControllerInfo()
	if err != nil {
		return fmt.Errorf("failed to create common network controller info: %w", err)
	}
	defaultController, err := ovn.NewDefaultNetworkController(cnci, nadController, observManager)
	if err != nil {
		return err
	}
	// Make sure we only set defaultNetworkController in case of no error,
	// otherwise we would initialize the interface with a nil implementation
	// which is not the same as nil interface.
	cm.defaultNetworkController = defaultController
	return nil
}

// Start the ovnkube controller
func (cm *NetworkControllerManager) Start(ctx context.Context) error {
	klog.Info("Starting the ovnkube controller")

	// Make sure that the ovnkube-controller zone matches with the Northbound db zone.
	// Wait for 300s before giving up
	maxTimeout := 300 * time.Second
	klog.Infof("Waiting up to %s for NBDB zone to match: %s", maxTimeout, config.Default.Zone)
	start := time.Now()
	var zone string
	var err1 error
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, maxTimeout, true, func(ctx context.Context) (bool, error) {
		zone, err1 = libovsdbutil.GetNBZone(cm.nbClient)
		if err1 != nil {
			return false, nil
		}
		if config.Default.Zone != zone {
			err1 = fmt.Errorf("config zone %s different from NBDB zone %s", config.Default.Zone, zone)
			return false, nil
		}
		return true, nil
	})

	if err != nil {
		return fmt.Errorf("failed to start default ovnkube-controller - OVN NBDB zone %s does not match the configured zone %q: errors: %v, %v",
			zone, config.Default.Zone, err, err1)
	}
	klog.Infof("NBDB zone sync took: %s", time.Since(start))

	err = cm.watchFactory.Start()
	if err != nil {
		return err
	}

	// Wait for one node to have the zone we want to manage, otherwise there is no point in configuring NBDB.
	// Really this covers a use case where a node is going from local -> remote, but has not yet annotated itself.
	// In this case ovnkube-controller on this remote node will treat the node as remote, and then once the annotation
	// appears will convert it to local, which may or may not clean up DB resources correctly.
	klog.Infof("Waiting up to %s for a node to have %q zone", maxTimeout, config.Default.Zone)
	start = time.Now()
	err = wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, maxTimeout, true, func(ctx context.Context) (bool, error) {
		nodes, err := cm.watchFactory.GetNodes()
		if err != nil {
			klog.Errorf("Unable to get nodes from informer while waiting for node zone sync")
			return false, nil
		}
		if len(nodes) == 0 {
			klog.Infof("No nodes in cluster: waiting for a node to have %q zone is not needed", config.Default.Zone)
			return true, nil
		}
		for _, node := range nodes {
			if util.GetNodeZone(node) == config.Default.Zone {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("failed to start default network controller - while waiting for any node to have zone: %q, error: %v",
			config.Default.Zone, err)
	}
	klog.Infof("Waiting for node in zone sync took: %s", time.Since(start))

	cm.configureMetrics(cm.stopChan)

	err = cm.configureSCTPSupport()
	if err != nil {
		return err
	}

	cm.configureSvcTemplateSupport()

	err = cm.createACLLoggingMeter()
	if err != nil {
		return nil
	}

	if config.Metrics.EnableConfigDuration {
		// with k=10,
		//  for a cluster with 10 nodes, measurement of 1 in every 100 requests
		//  for a cluster with 100 nodes, measurement of 1 in every 1000 requests
		metrics.GetConfigDurationRecorder().Run(cm.nbClient, cm.kube, 10, time.Second*5, cm.stopChan)
	}
	cm.podRecorder.Run(cm.sbClient, cm.stopChan)
	// nadController is nil if multi-network is disabled
	if cm.nadController != nil {
		if err = cm.nadController.Start(); err != nil {
			return fmt.Errorf("failed to start NAD Controller :%v", err)
		}
	}

	var observabilityManager *observability.Manager
	if config.OVNKubernetesFeature.EnableObservability {
		observabilityManager = observability.NewManager(cm.nbClient)
		if err = observabilityManager.Init(); err != nil {
			return fmt.Errorf("failed to init observability manager: %w", err)
		}
	} else {
		err = observability.Cleanup(cm.nbClient)
		if err != nil {
			klog.Warningf("Observability cleanup failed, expected if not all Samples ware deleted yet: %v", err)
		}
	}
	err = cm.initDefaultNetworkController(cm.nadController, observabilityManager)
	if err != nil {
		return fmt.Errorf("failed to init default network controller: %v", err)
	}
	err = cm.defaultNetworkController.Start(ctx)
	if err != nil {
		return fmt.Errorf("failed to start default network controller: %v", err)
	}

	return nil
}

// Stop gracefully stops all managed controllers
func (cm *NetworkControllerManager) Stop() {
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
