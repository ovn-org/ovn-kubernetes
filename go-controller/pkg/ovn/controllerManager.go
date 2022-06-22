package ovn

import (
	"fmt"
	"sync"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
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

func (cm *ControllerManager) Run() error {
	// add the net-attach-def watcher handler,
	// for each Controller created, call oc.Start()
	return nil
}

func (cm *ControllerManager) Stop() {
	// remove the net-attach-def watch handler
	// and for each Controller of secondary network, call oc.Stop()
}
