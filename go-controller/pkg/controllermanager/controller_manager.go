// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controllermanager

import (
	"context"
	"fmt"
	"sync"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics/recorders"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/observability"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	svccontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/routeimport"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// ControllerManager structure is the object manages all controllers
type ControllerManager struct {
	nodeName     string
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
	// Supports multicast?
	multicastSupport bool
	// Supports OVN Template Load Balancers?
	svcTemplateSupport bool

	stopChan                 chan struct{}
	wg                       *sync.WaitGroup
	portCache                *ovn.PortCache
	defaultNetworkController networkmanager.BaseNetworkController

	// networkManager creates and deletes network controllers
	networkManager     networkmanager.Controller
	routeImportManager routeimport.Controller
	nodeController     *nodecontroller.NodeController
	serviceController  *svccontroller.Controller

	// eIPController programs OVN to support EgressIP
	eIPController *ovn.EgressIPController

	addressSetManager *addresssetmanager.AddressSetManager
}

func (cm *ControllerManager) NewNetworkController(nInfo util.NetInfo) (networkmanager.NetworkController, error) {
	// Pass a shallow clone of the watch factory, this allows multiplexing
	// informers for user-defined networks.
	cnci := cm.newCommonNetworkControllerInfo(cm.watchFactory.ShallowClone())
	topoType := nInfo.TopologyType()
	switch topoType {
	case ovntypes.Layer3Topology:
		oc, err := ovn.NewLayer3UserDefinedNetworkController(cnci, nInfo, cm.networkManager.Interface(), cm.routeImportManager,
			cm.eIPController, cm.portCache, cm.addressSetManager, cm.nodeController, cm.serviceController)
		if err != nil {
			return nil, err
		}
		return oc, nil
	case ovntypes.Layer2Topology:
		oc, err := ovn.NewLayer2UserDefinedNetworkController(cnci, nInfo, cm.networkManager.Interface(), cm.routeImportManager,
			cm.portCache, cm.eIPController, cm.addressSetManager, cm.nodeController, cm.serviceController)
		if err != nil {
			return nil, err
		}
		return oc, nil
	case ovntypes.LocalnetTopology:
		oc := ovn.NewLocalnetUserDefinedNetworkController(cnci, nInfo, cm.networkManager.Interface(), cm.addressSetManager,
			cm.nodeController)
		return oc, nil
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// newDummyNetworkController creates a dummy network controller used to clean up specific network.
// role is the NetworkRoleExternalID from stale OVN entities (e.g. "primary" or "secondary") so that
// the dummy's netInfo.IsPrimaryNetwork() is correct for Layer2 gateway cleanup.
func (cm *ControllerManager) newDummyNetworkController(topoType, netName, role string) (networkmanager.NetworkController, error) {
	// Pass a shallow clone of the watch factory, this allows multiplexing
	// informers for user-defined Networks.
	cnci := cm.newCommonNetworkControllerInfo(cm.watchFactory.ShallowClone())
	netInfo, _ := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: cnitypes.NetConf{Name: netName}, Topology: topoType, Role: role})
	switch topoType {
	case ovntypes.Layer3Topology:
		return ovn.NewLayer3UserDefinedNetworkController(cnci, netInfo, cm.networkManager.Interface(), cm.routeImportManager,
			cm.eIPController, cm.portCache, cm.addressSetManager, nil, nil)
	case ovntypes.Layer2Topology:
		return ovn.NewLayer2UserDefinedNetworkController(cnci, netInfo, cm.networkManager.Interface(), cm.routeImportManager,
			cm.portCache, cm.eIPController, cm.addressSetManager, nil, nil)
	case ovntypes.LocalnetTopology:
		return ovn.NewLocalnetUserDefinedNetworkController(cnci, netInfo, cm.networkManager.Interface(), cm.addressSetManager, nil), nil
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// findAllUserDefinedNetworkLogicalEntities returns all OVN logical switches and
// routers that belong to user-defined networks (primary or secondary). Same
// predicate as original: entities have NetworkExternalID and NetworkRoleExternalID
// (TopologyExternalID always co-exists with NetworkExternalID per CleanupStaleNetworks).
// Caller reads role and topoType from entity ExternalIDs for dummy controller creation.
// Used on controller restart to remove stale entities for deleted UDNs.
func findAllUserDefinedNetworkLogicalEntities(nbClient libovsdbclient.Client) ([]*nbdb.LogicalSwitch,
	[]*nbdb.LogicalRouter, error) {

	belongsToUserDefinedNetwork := func(externalIDs map[string]string) bool {
		_, hasNetworkExternalID := externalIDs[ovntypes.NetworkExternalID]
		_, hasNetworkRoleExternalID := externalIDs[ovntypes.NetworkRoleExternalID]
		return hasNetworkExternalID && hasNetworkRoleExternalID
	}

	p1 := func(item *nbdb.LogicalSwitch) bool {
		return belongsToUserDefinedNetwork(item.ExternalIDs)
	}
	switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(nbClient, p1)
	if err != nil {
		klog.Errorf("Failed to get all logical switches of user-defined networks: %v", err)
		return nil, nil, err
	}
	p2 := func(item *nbdb.LogicalRouter) bool {
		return belongsToUserDefinedNetwork(item.ExternalIDs)
	}
	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(nbClient, p2)
	if err != nil {
		klog.Errorf("Failed to get all logical routers of user-defined networks: %v", err)
		return nil, nil, err
	}
	return switches, routers, nil
}

func (cm *ControllerManager) GetDefaultNetworkController() networkmanager.ReconcilableNetworkController {
	return cm.defaultNetworkController
}

func (cm *ControllerManager) CleanupStaleNetworks(validNetworks ...util.NetInfo) error {
	existingNetworksMap := map[string]string{}
	validNetworksSubnets := sets.New[string]()
	for _, network := range validNetworks {
		existingNetworksMap[network.GetNetworkName()] = network.TopologyType()
		for _, subnet := range network.Subnets() {
			validNetworksSubnets.Insert(subnet.CIDR.String())
		}
	}

	// Get all the existing user-defined network logical entities (primary and secondary).
	// For a given network, all switches/routers have the same role external ID (primary or secondary).
	switches, routers, err := findAllUserDefinedNetworkLogicalEntities(cm.nbClient)
	if err != nil {
		return err
	}

	staleNetworkControllers := map[string]networkmanager.NetworkController{}
	for _, ls := range switches {
		netName := ls.ExternalIDs[ovntypes.NetworkExternalID]
		// TopologyExternalID always co-exists with NetworkExternalID
		topoType := ls.ExternalIDs[ovntypes.TopologyExternalID]
		if existingNetworksMap[netName] == topoType {
			// network still exists, no cleanup to do
			continue
		}
		role := ls.ExternalIDs[ovntypes.NetworkRoleExternalID]
		if _, ok := staleNetworkControllers[netName]; ok {
			// already have a dummy controller for this network (from an earlier entity)
			continue
		}
		// Create dummy network controllers to clean up logical entities
		klog.V(5).Infof("Found stale %s network %s", topoType, netName)
		if oc, err := cm.newDummyNetworkController(topoType, netName, role); err == nil {
			staleNetworkControllers[netName] = oc
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
		role := lr.ExternalIDs[ovntypes.NetworkRoleExternalID]
		if _, ok := staleNetworkControllers[netName]; ok {
			// already have a dummy controller for this network (from an earlier entity)
			continue
		}
		// Create dummy network controllers to clean up logical entities
		klog.V(5).Infof("Found stale %s network %s", topoType, netName)
		if oc, err := cm.newDummyNetworkController(topoType, netName, role); err == nil {
			staleNetworkControllers[netName] = oc
		}
	}

	for netName, oc := range staleNetworkControllers {
		klog.Infof("Cleanup entities for stale network %s", netName)
		err = oc.Cleanup()
		if err != nil {
			klog.Errorf("Failed to delete stale OVN logical entities for network %s: %v", netName, err)
		}
	}

	// Remove stale subnets from the advertised networks address set used for isolation.
	// Network reconciliation will take care of removing the subnets for existing networks that are no longer
	// advertised.
	if err := ovn.CleanupStaleAdvertisedNetworkSubnets(cm.nbClient, validNetworksSubnets); err != nil {
		return fmt.Errorf("failed to cleanup stale advertised subnets: %w", err)
	}
	return nil
}

// NewControllerManager creates a new ovnkube controller manager to manage all the controller for all networks
func NewControllerManager(nodeName string, ovnClient *util.OVNClientset, wf *factory.WatchFactory,
	libovsdbOvnNBClient libovsdbclient.Client, libovsdbOvnSBClient libovsdbclient.Client,
	recorder record.EventRecorder, wg *sync.WaitGroup) (*ControllerManager, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("ovnkube-controller node name is required")
	}

	podRecorder := metrics.NewPodRecorder()

	stopCh := make(chan struct{})
	cm := &ControllerManager{
		nodeName: nodeName,
		client:   ovnClient.KubeClient,
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
			NADClient:            ovnClient.NetworkAttchDefClient,
		},
		stopChan:         stopCh,
		watchFactory:     wf,
		recorder:         recorder,
		nbClient:         libovsdbOvnNBClient,
		sbClient:         libovsdbOvnSBClient,
		podRecorder:      &podRecorder,
		portCache:        ovn.NewPortCache(stopCh),
		wg:               wg,
		multicastSupport: config.EnableMulticast,
	}
	var err error

	cm.networkManager = networkmanager.Default()
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.networkManager, err = networkmanager.NewForNode(cm.nodeName, cm, wf)
		if err != nil {
			return nil, err
		}
	}
	cm.nodeController = nodecontroller.NewNodeController(cm.watchFactory, cm.networkManager.Interface(), cm.nodeName)

	if util.IsRouteAdvertisementsEnabled() {
		if util.IsUplinkEnabled() {
			cm.routeImportManager = routeimport.New(cm.nodeName, cm.nbClient, wf.UplinkStateInformer().Lister())
		} else {
			cm.routeImportManager = routeimport.New(cm.nodeName, cm.nbClient, nil)
		}
	}
	cm.addressSetManager = addresssetmanager.NewAddressSetManager(cm.watchFactory.PodCoreInformer(),
		cm.watchFactory.NamespaceInformer(), cm.watchFactory.NodeCoreInformer(), cm.nbClient, cm.networkManager.Interface().GetNetworkNameForNADKey)

	return cm, nil
}

func (cm *ControllerManager) configureSvcTemplateSupport() {
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

func (cm *ControllerManager) configureMetrics(stopChan <-chan struct{}) {
	metrics.RegisterOVNKubeControllerPerformance(cm.nbClient)
	metrics.RegisterOVNKubeControllerFunctional(stopChan)
	metrics.RunTimestamp(stopChan, cm.sbClient, cm.nbClient)
	metrics.MonitorIPSec(cm.nbClient)
}

func (cm *ControllerManager) createACLLoggingMeter() error {
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
func (cm *ControllerManager) newCommonNetworkControllerInfo(wf *factory.WatchFactory) *ovn.CommonNetworkControllerInfo {
	return ovn.NewCommonNetworkControllerInfo(cm.client, cm.kube, wf, cm.recorder, cm.nbClient,
		cm.sbClient, cm.podRecorder, cm.multicastSupport, cm.svcTemplateSupport, cm.nodeName)
}

// initDefaultNetworkController creates the controller for default network
func (cm *ControllerManager) initDefaultNetworkController(observManager *observability.Manager) error {
	cnci := cm.newCommonNetworkControllerInfo(cm.watchFactory)
	defaultController, err := ovn.NewDefaultNetworkController(cnci, observManager, cm.networkManager.Interface(), cm.routeImportManager, cm.eIPController, cm.portCache, cm.addressSetManager, cm.nodeController)
	if err != nil {
		return err
	}
	cm.serviceController = defaultController.ServiceController()
	// Make sure we only set defaultNetworkController in case of no error,
	// otherwise we would initialize the interface with a nil implementation
	// which is not the same as nil interface.
	cm.defaultNetworkController = defaultController
	return nil
}

// Start the ovnkube controller
func (cm *ControllerManager) Start(ctx context.Context) error {
	klog.Info("Starting the ovnkube controller")
	config.Layer2UsesTransitRouter = true

	// Configure metrics early so workqueue provider is set before any workqueues are created
	cm.configureMetrics(cm.stopChan)

	err := cm.watchFactory.Start()
	if err != nil {
		return err
	}

	if err = cm.setUDNLayer2NodeUsesTransitRouter(); err != nil {
		return err
	}

	cm.configureSvcTemplateSupport()

	err = cm.createACLLoggingMeter()
	if err != nil {
		return fmt.Errorf("failed to create acl logging meter: %w", err)
	}

	if config.Metrics.EnableConfigDuration {
		// with k=10,
		//  for a cluster with 10 nodes, measurement of 1 in every 100 requests
		//  for a cluster with 100 nodes, measurement of 1 in every 1000 requests
		recorders.GetConfigDurationRecorder().Run(cm.nbClient, cm.watchFactory, 10, time.Second*5, cm.stopChan)
	}
	cm.podRecorder.Run(cm.sbClient, cm.stopChan)

	if err := cm.addressSetManager.Start(); err != nil {
		return fmt.Errorf("failed to start address set manager: %w", err)
	}

	if config.OVNKubernetesFeature.EnableEgressIP {
		cm.eIPController = ovn.NewEIPController(cm.nbClient, cm.kube, cm.watchFactory, cm.recorder, cm.portCache, cm.networkManager.Interface(),
			addressset.NewOvnAddressSetFactory(cm.nbClient, config.IPv4Mode, config.IPv6Mode), cm.addressSetManager,
			config.IPv4Mode, config.IPv6Mode, cm.nodeName, ovntypes.DefaultNetworkControllerName)
		// FIXME(martinkennelly): remove when EIP controller is fully extracted from DNC and started here. Ensure SyncNodeCache is re-enabled in EIP controller.
		if err = cm.eIPController.SyncNodeCache(); err != nil {
			klog.Warningf("Failed to sync EgressIP controller node cache: %v", err)
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

	if util.IsNetworkSegmentationSupportEnabled() {
		addressSetFactory := addressset.NewOvnAddressSetFactory(cm.nbClient, config.IPv4Mode, config.IPv6Mode)
		go func() {
			if err := udnenabledsvc.NewController(cm.nbClient, addressSetFactory, cm.watchFactory.ServiceCoreInformer(),
				config.Default.UDNAllowedDefaultServices).Run(cm.stopChan); err != nil {
				klog.Errorf("UDN enabled service controller failed: %v", err)
			}
		}()
	}

	err = cm.initDefaultNetworkController(observabilityManager)
	if err != nil {
		return fmt.Errorf("failed to init default network controller: %v", err)
	}

	// The advertised-network isolation port group is consumed whenever pod
	// networks are routed over the underlay: for networks advertised through
	// the RouteAdvertisements feature, but also for no-overlay networks,
	// which are supported without it (the default network via config,
	// CUDNs via network segmentation).
	if util.IsRouteAdvertisementsEnabled() ||
		config.Default.Transport == ovntypes.NetworkTransportNoOverlay ||
		util.IsNetworkSegmentationSupportEnabled() {
		if err := ovn.ConfigureAdvertisedNetworkIsolation(cm.nbClient); err != nil {
			return fmt.Errorf("failed to initialize advertised network isolation: %w", err)
		}
	}

	if cm.networkManager != nil {
		if cm.nodeController != nil {
			if err = cm.nodeController.Start(); err != nil {
				return fmt.Errorf("failed to start UDN node topology controller: %v", err)
			}
		}
		if err = cm.networkManager.Start(); err != nil {
			return fmt.Errorf("failed to start NAD Controller :%v", err)
		}
	}

	if cm.routeImportManager != nil {
		err = cm.routeImportManager.Start()
		if err != nil {
			return fmt.Errorf("failed to start route import: %v", err)
		}
	}

	err = cm.defaultNetworkController.Start(ctx)
	if err != nil {
		return fmt.Errorf("failed to start default network controller: %v", err)
	}

	return nil
}

// Stop gracefully stops all managed controllers
func (cm *ControllerManager) Stop() {
	// stop metric recorders
	close(cm.stopChan)

	// stop the default network controller
	if cm.defaultNetworkController != nil {
		cm.defaultNetworkController.Stop()
	}

	// stop the NAD controller
	if cm.networkManager != nil {
		if cm.nodeController != nil {
			cm.nodeController.Stop()
		}
		cm.networkManager.Stop()
	}

	if cm.routeImportManager != nil {
		cm.routeImportManager.Stop()
	}

	if cm.addressSetManager != nil {
		cm.addressSetManager.Stop()
	}
}

func (cm *ControllerManager) Reconcile(_ string, _, _ util.NetInfo) error {
	return nil
}

func (cm *ControllerManager) setUDNLayer2NodeUsesTransitRouter() error {
	// Older peers still use this annotation to determine how to connect to this node.
	node, err := cm.watchFactory.GetNode(cm.nodeName)
	if err != nil {
		return fmt.Errorf("unable to get controller node %s from informer while annotating layer2 topology: %w", cm.nodeName, err)
	}
	if util.UDNLayer2NodeUsesTransitRouter(node) {
		return nil
	}
	if err := cm.kube.SetAnnotationsOnNode(cm.nodeName, map[string]interface{}{
		util.Layer2TopologyVersion: util.TransitRouterTopoVersion}); err != nil {
		return fmt.Errorf("failed to set annotation %s on node %s: %w", util.Layer2TopologyVersion, cm.nodeName, err)
	}
	return nil
}
