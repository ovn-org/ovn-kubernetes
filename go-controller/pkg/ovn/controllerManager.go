package ovn

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/subnetallocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	ktypes "k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

// GenericController structure is place holder for all fields shared among controllers.
type GenericController struct {
	client       clientset.Interface
	kube         kube.Interface
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
}

// ControllerManager structure is the object manages all controllers for all networks
type ControllerManager struct {
	GenericController

	netAttachDefHandler *factory.Handler
	// default wait group and stop channel, used by default network controller
	defaultWg       *sync.WaitGroup
	defaultStopChan chan struct{}

	// unique identity for controllerManager running on different ovnkube-master instance,
	// used for leader election
	identity string

	// the default controller
	defaultController *Controller

	// controller for all networks, key is netName of net-attach-def, value is *Controller
	// this map is updated either at the very beginning of ovnkube-master when initializing the default controller
	// or when net-attach-def is added/deleted. All these are serialized and no lock protection is needed
	allOvnControllers map[string]*Controller
}

func NewControllerManager(ovnClient *util.OVNClientset, identity string, wf *factory.WatchFactory,
	stopChan chan struct{}, libovsdbOvnNBClient libovsdbclient.Client, libovsdbOvnSBClient libovsdbclient.Client,
	recorder record.EventRecorder, wg *sync.WaitGroup) *ControllerManager {
	podRecorder := metrics.NewPodRecorder()
	return &ControllerManager{
		GenericController: GenericController{
			client: ovnClient.KubeClient,
			kube: &kube.Kube{
				KClient:              ovnClient.KubeClient,
				EIPClient:            ovnClient.EgressIPClient,
				EgressFirewallClient: ovnClient.EgressFirewallClient,
				CloudNetworkClient:   ovnClient.CloudNetworkClient,
			},
			watchFactory: wf,
			recorder:     recorder,
			nbClient:     libovsdbOvnNBClient,
			sbClient:     libovsdbOvnSBClient,
			podRecorder:  &podRecorder,
		},
		defaultWg:         wg,
		defaultStopChan:   stopChan,
		identity:          identity,
		allOvnControllers: make(map[string]*Controller),
	}
}

func (cm *ControllerManager) InitDefaultController(addressSetFactory addressset.AddressSetFactory) (*Controller, error) {
	defaultNetConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: ovntypes.DefaultNetworkName,
		},
		NetCidr:     config.Default.RawClusterSubnets,
		MTU:         config.Default.MTU,
		IsSecondary: false,
	}
	nadInfo := util.NewNetAttachDefInfo(defaultNetConf)
	return cm.NewOvnController(nadInfo, addressSetFactory)
}

func (cm *ControllerManager) Init() error {
	// the default network net_attach_def may not exist; we'd need to create default OVN Controller based on config.
	_, err := cm.InitDefaultController(nil)
	if err != nil {
		return err
	}

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

	// enableOVNLogicalDataPathGroups sets an OVN flag to enable logical datapath
	// groups on OVN 20.12 and later. The option is ignored if OVN doesn't
	// understand it. Logical datapath groups reduce the size of the southbound
	// database in large clusters. ovn-controllers should be upgraded to a version
	// that supports them before the option is turned on by the master.
	nbGlobal := nbdb.NBGlobal{
		Options: map[string]string{"use_logical_dp_groups": "true"},
	}
	if err := libovsdbops.UpdateNBGlobalSetOptions(cm.nbClient, &nbGlobal); err != nil {
		return fmt.Errorf("failed to set NB global option to enable logical datapath groups: %v", err)
	}

	metrics.RunTimestamp(cm.defaultStopChan, cm.sbClient, cm.nbClient)
	metrics.MonitorIPSec(cm.nbClient)
	if config.Metrics.EnableConfigDuration {
		// with k=10,
		//  for a cluster with 10 nodes, measurement of 1 in every 100 requests
		//  for a cluster with 100 nodes, measurement of 1 in every 1000 requests
		metrics.GetConfigDurationRecorder().Run(cm.nbClient, cm.kube, 10, time.Second*5, cm.defaultStopChan)
	}

	cm.podRecorder.Run(cm.sbClient, cm.defaultStopChan)

	// Start and sync the watch factory to begin listening for events
	if err := cm.watchFactory.Start(); err != nil {
		return err
	}
	return nil
}

// NewOvnController creates a new OVN controller for creating logical network
// infrastructure and policy
func (cm *ControllerManager) NewOvnController(nadInfo *util.NetAttachDefInfo,
	addressSetFactory addressset.AddressSetFactory) (*Controller, error) {
	clusterIPNet, err := config.ParseClusterSubnetEntries(nadInfo.NetCidr)
	if err != nil {
		return nil, fmt.Errorf("cluster subnet %s for network %s is invalid: %v", nadInfo.NetCidr, nadInfo.NetName, err)
	}

	stopChan := cm.defaultStopChan
	if nadInfo.IsSecondary {
		stopChan = make(chan struct{})
	}
	oc := &Controller{
		GenericController:     cm.GenericController,
		nadInfo:               nadInfo,
		stopChan:              stopChan,
		clusterSubnets:        clusterIPNet,
		masterSubnetAllocator: subnetallocator.NewSubnetAllocator(),
		logicalPortCache:      newPortCache(stopChan),
		lsManager:             lsm.NewLogicalSwitchManager(),
		loadBalancerGroupUUID: "",
		aclLoggingEnabled:     true,
		joinSwIPManager:       nil,
		retryPods:             NewRetryObjs(factory.PodType, "", nil, nil, nil),
		retryNodes:            NewRetryObjs(factory.NodeType, "", nil, nil, nil),
	}
	if !nadInfo.IsSecondary {
		svcController, svcFactory := newServiceController(cm.client, cm.nbClient, cm.recorder)
		oc.svcController = svcController
		oc.svcFactory = svcFactory
		oc.egressSvcController = newEgressServiceController(cm.client, cm.nbClient, svcFactory, cm.defaultStopChan)
		if config.HybridOverlay.Enabled {
			oc.hybridOverlaySubnetAllocator = subnetallocator.NewSubnetAllocator()
		}
		oc.eIPC = &egressIPController{
			egressIPAssignmentMutex:           &sync.Mutex{},
			podAssignmentMutex:                &sync.Mutex{},
			podAssignment:                     make(map[string]*podAssignmentState),
			pendingCloudPrivateIPConfigsMutex: &sync.Mutex{},
			pendingCloudPrivateIPConfigsOps:   make(map[string]map[string]*cloudPrivateIPConfigOp),
			allocator:                         allocator{&sync.Mutex{}, make(map[string]*egressNode)},
			nbClient:                          cm.nbClient,
			watchFactory:                      cm.watchFactory,
			egressIPTotalTimeout:              config.OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout,
			egressIPNodeHealthCheckPort:       config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort,
		}
		oc.externalGWCache = make(map[ktypes.NamespacedName]*externalRouteInfo)
		oc.exGWCacheMutex = sync.RWMutex{}

		oc.retryEgressFirewalls = NewRetryObjs(factory.EgressFirewallType, "", nil, nil, nil)
		oc.retryEgressIPs = NewRetryObjs(factory.EgressIPType, "", nil, nil, nil)
		oc.retryEgressIPNamespaces = NewRetryObjs(factory.EgressIPNamespaceType, "", nil, nil, nil)
		oc.retryEgressIPPods = NewRetryObjs(factory.EgressIPPodType, "", nil, nil, nil)
		oc.retryEgressNodes = NewRetryObjs(factory.EgressNodeType, "", nil, nil, nil)
		oc.retryCloudPrivateIPConfig = NewRetryObjs(factory.CloudPrivateIPConfigType, "", nil, nil, nil)

		if addressSetFactory == nil {
			addressSetFactory = addressset.NewOvnAddressSetFactory(cm.nbClient)
		}
		oc.addressSetFactory = addressSetFactory
		oc.sharedNetpolPortGroups = syncmap.NewSyncMap[*defaultDenyPortGroups]()
		oc.networkPolicies = syncmap.NewSyncMap[*networkPolicy]()
		oc.namespaces = make(map[string]*namespaceInfo)
		oc.namespacesMutex = sync.Mutex{}
		oc.retryNamespaces = NewRetryObjs(factory.NamespaceType, "", nil, nil, nil)
		oc.retryNetworkPolicies = NewRetryObjs(factory.PolicyType, "", nil, nil, nil)

		oc.wg = cm.defaultWg
		oc.multicastSupport = config.EnableMulticast
		cm.defaultController = oc
	} else {
		oc.wg = &sync.WaitGroup{}
		oc.multicastSupport = false // disable multicast support for now
	}
	cm.allOvnControllers[nadInfo.NetName] = oc
	return oc, nil
}

func (cm *ControllerManager) initOvnController(netattachdef *nettypes.NetworkAttachmentDefinition) (*Controller, error) {
	nadInfo, err := util.ParseNADInfo(netattachdef)
	if err != nil {
		return nil, err
	}
	klog.V(5).Infof("Add Network Attachment Definition %s/%s to nad %s", netattachdef.Namespace, netattachdef.Name, nadInfo.NetName)

	// Note that net-attach-def add/delete/update events are serialized, so we don't need locks here.
	// Check if any Controller of the same netconf.Name already exists, if so, check its conf to see if they are the same.
	oc, ok := cm.allOvnControllers[nadInfo.NetName]
	if ok {
		// for default network, the configuration comes from command configuration, do not validate
		if oc.nadInfo.IsSecondary && (oc.nadInfo.NetCidr != nadInfo.NetCidr || oc.nadInfo.MTU != nadInfo.MTU) {
			return nil, fmt.Errorf("network attachment definition %s/%s does not share the same CNI config of name %s",
				netattachdef.Namespace, netattachdef.Name, nadInfo.NetName)
		} else {
			oc.nadInfo.NetAttachDefs.Store(util.GetNadKeyName(netattachdef.Namespace, netattachdef.Name), true)
			return oc, nil
		}
	}

	oc, err = cm.NewOvnController(nadInfo, nil)
	oc.nadInfo.NetAttachDefs.Store(util.GetNadKeyName(netattachdef.Namespace, netattachdef.Name), true)
	return oc, err
}

func (cm *ControllerManager) Run() error {
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		handler, err := cm.watchFactory.AddNetworkattachmentdefinitionHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				netattachdef := obj.(*nettypes.NetworkAttachmentDefinition)
				cm.addNetworkAttachDefinition(netattachdef)
			},
			UpdateFunc: func(old, new interface{}) {},
			DeleteFunc: func(obj interface{}) {
				netattachdef := obj.(*nettypes.NetworkAttachmentDefinition)
				cm.deleteNetworkAttachDefinition(netattachdef)
			},
		}, cm.syncNetworkAttachDefinition)

		if err != nil {
			cm.netAttachDefHandler = handler
		}
		return err
	}
	return nil
}

func (cm *ControllerManager) Stop() {
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		if cm.netAttachDefHandler != nil {
			cm.watchFactory.RemovePodHandler(cm.netAttachDefHandler)
		}
		for netName, oc := range cm.allOvnControllers {
			if netName == ovntypes.DefaultNetworkName {
				continue
			}
			oc.Stop()
		}
	}
}

func (cm *ControllerManager) addNetworkAttachDefinition(netattachdef *nettypes.NetworkAttachmentDefinition) {
	klog.Infof("Add Network Attachment Definition %s/%s", netattachdef.Namespace, netattachdef.Name)
	oc, err := cm.initOvnController(netattachdef)
	if err != nil {
		// if the net-attach-def is not managed by OVN, return silently
		if err != util.ErrorAttachDefNotOvnManaged {
			klog.Errorf("Failed to add Network Attachment Definition %s/%s: %v", netattachdef.Namespace, netattachdef.Name, err)
		}
		return
	}

	// run the cluster controller to init the master
	err = oc.Start(context.TODO())
	if err != nil {
		klog.Errorf(err.Error())
	}
}

func (cm *ControllerManager) deleteNetworkAttachDefinition(netattachdef *nettypes.NetworkAttachmentDefinition) {
	klog.Infof("Delete Network Attachment Definition %s/%s", netattachdef.Namespace, netattachdef.Name)
	netconf, err := util.ParseNetConf(netattachdef)
	if err != nil {
		if err != util.ErrorAttachDefNotOvnManaged {
			klog.Error(err)
		}
		return
	}
	netName := netconf.Name
	nadName := util.GetNadKeyName(netattachdef.Namespace, netattachdef.Name)
	oc, ok := cm.allOvnControllers[netName]
	if !ok {
		klog.Errorf("Failed to find network controller for network %s", netconf.Name)
		return
	}
	_, ok = oc.nadInfo.NetAttachDefs.LoadAndDelete(nadName)
	if !ok {
		klog.Errorf("Failed to find nad %s from network controller for network %s", nadName, netconf.Name)
		return
	}
	if !netconf.IsSecondary {
		return
	}
	// check if there any net-attach-def sharing the same CNI conf name left, if yes, just return
	netAttachDefLeft := false
	oc.nadInfo.NetAttachDefs.Range(func(key, value interface{}) bool {
		netAttachDefLeft = true
		return false
	})
	if netAttachDefLeft {
		return
	}

	klog.Infof("The last Network Attachment Definition %s/%s is deleted from nad %s, delete associated logical entities",
		netattachdef.Namespace, netattachdef.Name, netconf.Name)

	oc.Stop()

	// cleanup related OVN logical entities
	var ops []libovsdb.Operation

	// first delete node logical switches
	ops, err = libovsdbops.DeleteLogicalSwitchesWithPredicateOps(cm.nbClient, ops,
		func(item *nbdb.LogicalSwitch) bool { return item.ExternalIDs["network_name"] == oc.nadInfo.NetName })
	if err != nil {
		klog.Errorf("Failed to get ops for deleting switches of network %s", oc.nadInfo.NetName)
	}

	// now delete cluster router
	ops, err = libovsdbops.DeleteLogicalRoutersWithPredicateOps(cm.nbClient, ops,
		func(item *nbdb.LogicalRouter) bool { return item.ExternalIDs["network_name"] == oc.nadInfo.NetName })
	if err != nil {
		klog.Errorf("Failed to get ops for deleting routers of network %s", oc.nadInfo.NetName)
	}

	_, err = libovsdbops.TransactAndCheck(cm.nbClient, ops)
	if err != nil {
		klog.Errorf("Failed to deleting routers/switches of network %s", oc.nadInfo.NetName)
	}

	// cleanup related OVN logical entities
	existingNodes, err := oc.watchFactory.GetNodes()
	if err != nil {
		klog.Errorf("Error in initializing/fetching subnets: %v", err)
	} else {
		// remove hostsubnet annoation for this network
		for _, node := range existingNodes {
			oc.lsManager.DeleteNode(oc.nadInfo.Prefix + node.Name)
			if noHostSubnet(node) {
				continue
			}
			_ = oc.updateNodeAnnotationWithRetry(node.Name, []*net.IPNet{})
		}
	}

	delete(cm.allOvnControllers, netconf.Name)
}

// syncNetworkAttachDefinition() delete OVN logical entities of the obsoleted netNames.
//
// Note that we'd need to initOvnController of all existing net-attach-def here, so that nad
// of all net-attach-def are known when watchPods() is called when first net-attach-def is added.
func (cm *ControllerManager) syncNetworkAttachDefinition(netattachdefs []interface{}) error {
	// Get all the existing non-default netNames
	expectedNetworks := make(map[string]bool)

	// we need to walk through all net-attach-def and add them into Controller.nadInfo.NetAttachDefs, so that when each
	// Controller is running, watchPods()->addLogicalPod()->IsNetworkOnPod() can correctly check Pods need to be plumbed
	// for the specific Controller
	for _, netattachdefIntf := range netattachdefs {
		netattachdef, ok := netattachdefIntf.(*nettypes.NetworkAttachmentDefinition)
		if !ok {
			klog.Errorf("Spurious object in syncNetworkAttachDefinition: %v", netattachdefIntf)
			continue
		}

		// ovnController.nadInfo.NetAttachDefs
		oc, err := cm.initOvnController(netattachdef)
		if err != nil {
			klog.Errorf(err.Error())
			continue
		}

		expectedNetworks[oc.nadInfo.NetName] = true
	}

	// Find all the logical node switches for the non-default networks and delete the ones that belong to the
	// obsolete networks
	p1 := func(item *nbdb.LogicalSwitch) bool {
		// Ignore external and Join switches(both legacy and current)
		_, ok := item.ExternalIDs["network_name"]
		return len(item.OtherConfig) != 0 && ok
	}
	nodeSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(cm.nbClient, p1)
	if err != nil {
		klog.Errorf("Failed to get node logical switches which have other-config set error: %v", err)
		return err
	}
	for _, nodeSwitch := range nodeSwitches {
		netName := nodeSwitch.ExternalIDs["network_name"]
		if _, ok := expectedNetworks[netName]; ok {
			// network still exists, no cleanup to do
			continue
		}
		netPrefix := util.GetSecondaryNetworkPrefix(netName)
		// items[0] is the switch name, which should be prefixed with netName
		if !strings.HasPrefix(nodeSwitch.Name, netPrefix) {
			klog.Warningf("Unexpected logical switch %s for network %s during sync", nodeSwitch.Name, netName)
			continue
		}

		err = libovsdbops.DeleteLogicalSwitch(cm.nbClient, nodeSwitch.Name)
		if err != nil {
			klog.Errorf("Failed to delete stale logical switch %s for network %s: %v", nodeSwitch.Name, netName, err)
		}
		nodeName := strings.TrimPrefix(nodeSwitch.Name, netPrefix)
		oc := &Controller{GenericController: cm.GenericController, nadInfo: &util.NetAttachDefInfo{NetNameInfo: util.NetNameInfo{NetName: netName, Prefix: netPrefix, IsSecondary: true}}}
		err = oc.updateNodeAnnotationWithRetry(nodeName, []*net.IPNet{})
		if err != nil {
			klog.Errorf("Failed to remove subnet annotation on node %s for network %s: %v", nodeName, netName, err)
		}
	}
	p2 := func(item *nbdb.LogicalRouter) bool {
		_, ok := item.ExternalIDs["network_name"]
		return item.ExternalIDs["k8s-cluster-router"] == "yes" && ok
	}
	clusterRouters, err := libovsdbops.FindLogicalRoutersWithPredicate(cm.nbClient, p2)
	if err != nil {
		klog.Errorf("Failed to get all distributed logical routers: %v", err)
		return err
	}
	for _, clusterRouter := range clusterRouters {
		netName := clusterRouter.ExternalIDs["network_name"]
		if _, ok := expectedNetworks[netName]; ok {
			// network still exists, no cleanup to do
			continue
		}

		netPrefix := util.GetSecondaryNetworkPrefix(netName)
		// items[0] is the router name, which should be prefixed with netName
		if !strings.HasPrefix(clusterRouter.Name, netPrefix) {
			klog.Warningf("Unexpected logical router %s for network %s during sync", clusterRouter.Name, netName)
			continue
		}

		err := libovsdbops.DeleteLogicalRouter(cm.nbClient, clusterRouter.Name)
		if err != nil {
			klog.Errorf("Failed to delete distributed router for network %s: %v", netName, err)
		}
	}
	return nil
}
