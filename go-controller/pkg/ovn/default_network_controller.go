package ovn

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressqoslisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/listers/egressqos/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	apbroutecontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	egresssvc "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/egressservice"
	svccontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/unidling"
	aclsyncer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/external_ids_syncer/acl"
	addrsetsyncer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/external_ids_syncer/address_set"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	zoneic "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const DefaultNetworkControllerName = "default-network-controller"

// DefaultNetworkController structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints) for default l3 network
type DefaultNetworkController struct {
	BaseNetworkController

	// For TCP, UDP, and SCTP type traffic, cache OVN load-balancers used for the
	// cluster's east-west traffic.
	loadbalancerClusterCache map[kapi.Protocol]string

	externalGWCache map[ktypes.NamespacedName]*apbroutecontroller.ExternalRouteInfo
	exGWCacheMutex  *sync.RWMutex

	// egressFirewalls is a map of namespaces and the egressFirewall attached to it
	egressFirewalls sync.Map

	// EgressQoS
	egressQoSLister egressqoslisters.EgressQoSLister
	egressQoSSynced cache.InformerSynced
	egressQoSQueue  workqueue.RateLimitingInterface
	egressQoSCache  sync.Map

	egressQoSPodLister corev1listers.PodLister
	egressQoSPodSynced cache.InformerSynced
	egressQoSPodQueue  workqueue.RateLimitingInterface

	egressQoSNodeLister corev1listers.NodeLister
	egressQoSNodeSynced cache.InformerSynced
	egressQoSNodeQueue  workqueue.RateLimitingInterface

	// Cluster wide Load_Balancer_Group UUID.
	// Includes all node switches and node gateway routers.
	clusterLoadBalancerGroupUUID string

	// Cluster wide switch Load_Balancer_Group UUID.
	// Includes all node switches.
	switchLoadBalancerGroupUUID string

	// Cluster wide router Load_Balancer_Group UUID.
	// Includes all node gateway routers.
	routerLoadBalancerGroupUUID string

	// Cluster-wide router default Control Plane Protection (COPP) UUID
	defaultCOPPUUID string

	// Controller used for programming OVN for egress IP
	eIPC egressIPZoneController

	// Controller used to handle services
	svcController *svccontroller.Controller
	// Controller used to handle egress services
	egressSvcController *egresssvc.Controller

	// Controller used to handle the admin policy based external route resources
	apbExternalRouteController *apbroutecontroller.ExternalGatewayMasterController
	// svcFactory used to handle service related events
	svcFactory informers.SharedInformerFactory

	egressFirewallDNS *EgressDNS

	// retry framework for egress firewall
	retryEgressFirewalls *retry.RetryFramework

	// retry framework for egress IP
	retryEgressIPs *retry.RetryFramework
	// retry framework for egress IP Namespaces
	retryEgressIPNamespaces *retry.RetryFramework
	// retry framework for egress IP Pods
	retryEgressIPPods *retry.RetryFramework
	// retry framework for Egress nodes
	retryEgressNodes *retry.RetryFramework
	// retry framework for Egress Firewall Nodes
	retryEgressFwNodes *retry.RetryFramework

	// Node-specific syncMaps used by node event handler
	gatewaysFailed              sync.Map
	mgmtPortFailed              sync.Map
	addNodeFailed               sync.Map
	nodeClusterRouterPortFailed sync.Map
	hybridOverlayFailed         sync.Map
	syncZoneICFailed            sync.Map

	// variable to determine if all pods present on the node during startup have been processed
	// updated atomically
	allInitialPodsProcessed uint32

	// IP addresses of OVN Cluster logical router port ("GwRouterToJoinSwitchPrefix + OVNClusterRouter")
	// connecting to the join switch
	ovnClusterLRPToJoinIfAddrs []*net.IPNet

	// zoneICHandler creates the interconnect resources for local nodes and remote nodes.
	// Interconnect resources are Transit switch and logical ports connecting this transit switch
	// to the cluster router. Please see zone_interconnect/interconnect_handler.go for more details.
	zoneICHandler *zoneic.ZoneInterconnectHandler
	// zoneChassisHandler handles the local node and remote nodes in creating or updating the chassis entries in the OVN Southbound DB.
	// Please see zone_interconnect/chassis_handler.go for more details.
	zoneChassisHandler *zoneic.ZoneChassisHandler
}

// NewDefaultNetworkController creates a new OVN controller for creating logical network
// infrastructure and policy for default l3 network
func NewDefaultNetworkController(cnci *CommonNetworkControllerInfo, err error) (*DefaultNetworkController, error) {
	stopChan := make(chan struct{})
	wg := &sync.WaitGroup{}
	return newDefaultNetworkControllerCommon(cnci, stopChan, wg, nil)
}

func newDefaultNetworkControllerCommon(cnci *CommonNetworkControllerInfo,
	defaultStopChan chan struct{}, defaultWg *sync.WaitGroup,
	addressSetFactory addressset.AddressSetFactory) (*DefaultNetworkController, error) {

	if addressSetFactory == nil {
		addressSetFactory = addressset.NewOvnAddressSetFactory(cnci.nbClient, config.IPv4Mode, config.IPv6Mode)
	}
	svcController, svcFactory, err := newServiceController(cnci.client, cnci.nbClient, cnci.recorder)
	if err != nil {
		return nil, fmt.Errorf("unable to create new service controller while creating new default network controller: %w", err)
	}

	var zoneICHandler *zoneic.ZoneInterconnectHandler
	var zoneChassisHandler *zoneic.ZoneChassisHandler
	if config.OVNKubernetesFeature.EnableInterconnect {
		zoneICHandler = zoneic.NewZoneInterconnectHandler(&util.DefaultNetInfo{}, cnci.nbClient, cnci.sbClient)
		zoneChassisHandler = zoneic.NewZoneChassisHandler(cnci.sbClient)
	}
	apbExternalRouteController, err := apbroutecontroller.NewExternalMasterController(
		DefaultNetworkControllerName,
		cnci.client,
		cnci.kube.APBRouteClient,
		defaultStopChan,
		cnci.watchFactory.PodCoreInformer(),
		cnci.watchFactory.NamespaceInformer(),
		cnci.watchFactory.NodeCoreInformer().Lister(),
		cnci.nbClient,
		addressSetFactory,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create new admin policy based external route controller while creating new default network controller :%w", err)
	}

	oc := &DefaultNetworkController{
		BaseNetworkController: BaseNetworkController{
			CommonNetworkControllerInfo: *cnci,
			controllerName:              DefaultNetworkControllerName,
			NetInfo:                     &util.DefaultNetInfo{},
			lsManager:                   lsm.NewLogicalSwitchManager(),
			logicalPortCache:            newPortCache(defaultStopChan),
			namespaces:                  make(map[string]*namespaceInfo),
			namespacesMutex:             sync.Mutex{},
			addressSetFactory:           addressSetFactory,
			networkPolicies:             syncmap.NewSyncMap[*networkPolicy](),
			sharedNetpolPortGroups:      syncmap.NewSyncMap[*defaultDenyPortGroups](),
			podSelectorAddressSets:      syncmap.NewSyncMap[*PodSelectorAddressSet](),
			stopChan:                    defaultStopChan,
			wg:                          defaultWg,
			localZoneNodes:              &sync.Map{},
		},
		externalGWCache: apbExternalRouteController.ExternalGWCache,
		exGWCacheMutex:  apbExternalRouteController.ExGWCacheMutex,
		eIPC: egressIPZoneController{
			nodeIPUpdateMutex:  &sync.Mutex{},
			podAssignmentMutex: &sync.Mutex{},
			podAssignment:      make(map[string]*podAssignmentState),
			nbClient:           cnci.nbClient,
			watchFactory:       cnci.watchFactory,
			nodeZoneState:      syncmap.NewSyncMap[bool](),
		},
		loadbalancerClusterCache:     make(map[kapi.Protocol]string),
		clusterLoadBalancerGroupUUID: "",
		switchLoadBalancerGroupUUID:  "",
		routerLoadBalancerGroupUUID:  "",
		svcController:                svcController,
		svcFactory:                   svcFactory,
		zoneICHandler:                zoneICHandler,
		zoneChassisHandler:           zoneChassisHandler,
		apbExternalRouteController:   apbExternalRouteController,
	}

	// Allocate IPs for logical router port "GwRouterToJoinSwitchPrefix + OVNClusterRouter". This should always
	// allocate the first IPs in the join switch subnets.
	gwLRPIfAddrs, err := oc.getOVNClusterRouterPortToJoinSwitchIfAddrs()
	if err != nil {
		return nil, fmt.Errorf("failed to allocate join switch IP address connected to %s: %v", types.OVNClusterRouter, err)
	}

	oc.ovnClusterLRPToJoinIfAddrs = gwLRPIfAddrs

	oc.initRetryFramework()
	return oc, nil
}

func (oc *DefaultNetworkController) initRetryFramework() {
	// Init the retry framework for pods, namespaces, nodes, network policies, egress firewalls,
	// egress IP (and dependent namespaces, pods, nodes), cloud private ip config.
	oc.retryPods = oc.newRetryFramework(factory.PodType)
	oc.retryNodes = oc.newRetryFramework(factory.NodeType)
	oc.retryEgressFirewalls = oc.newRetryFramework(factory.EgressFirewallType)
	oc.retryEgressIPs = oc.newRetryFramework(factory.EgressIPType)
	oc.retryEgressIPNamespaces = oc.newRetryFramework(factory.EgressIPNamespaceType)
	oc.retryEgressIPPods = oc.newRetryFramework(factory.EgressIPPodType)
	oc.retryEgressNodes = oc.newRetryFramework(factory.EgressNodeType)
	oc.retryEgressFwNodes = oc.newRetryFramework(factory.EgressFwNodeType)
	oc.retryNamespaces = oc.newRetryFramework(factory.NamespaceType)
	oc.retryNetworkPolicies = oc.newRetryFramework(factory.PolicyType)
}

// newRetryFramework builds and returns a retry framework for the input resource
// type and assigns all ovnk-master-specific function attributes in the returned struct;
// these functions will then be called by the retry logic in the retry package when
// WatchResource() is called.
func (oc *DefaultNetworkController) newRetryFramework(
	objectType reflect.Type) *retry.RetryFramework {
	eventHandler := &defaultNetworkControllerEventHandler{
		baseHandler:     baseNetworkControllerEventHandler{},
		objType:         objectType,
		watchFactory:    oc.watchFactory,
		oc:              oc,
		extraParameters: nil, // in use by network policy dynamic watchers
		syncFunc:        nil,
	}
	resourceHandler := &retry.ResourceHandler{
		HasUpdateFunc:          hasResourceAnUpdateFunc(objectType),
		NeedsUpdateDuringRetry: needsUpdateDuringRetry(objectType),
		ObjType:                objectType,
		EventHandler:           eventHandler,
	}
	r := retry.NewRetryFramework(
		oc.stopChan,
		oc.wg,
		oc.watchFactory,
		resourceHandler,
	)
	return r
}

func (oc *DefaultNetworkController) syncAddressSetsAndAcls() error {
	// sync address sets, only required for network controller, since any old objects in the db without
	// Owner set are owned by the default network controller.
	addrSetSyncer := addrsetsyncer.NewAddressSetSyncer(oc.nbClient, oc.controllerName)
	err := addrSetSyncer.SyncAddressSets()
	if err != nil {
		return fmt.Errorf("failed to sync address sets on controller init: %v", err)
	}

	existingNodes, err := oc.kube.GetNodes()
	if err != nil {
		return fmt.Errorf("failed to get existing nodes: %w", err)
	}
	aclSyncer := aclsyncer.NewACLSyncer(oc.nbClient, oc.controllerName)
	err = aclSyncer.SyncACLs(existingNodes)
	if err != nil {
		return fmt.Errorf("failed to sync acls on controller init: %v", err)
	}

	// sync shared resources
	// pod selector address sets
	err = oc.cleanupPodSelectorAddressSets()
	if err != nil {
		return fmt.Errorf("cleaning up stale pod selector address sets for network %v failed : %w", oc.GetNetworkName(), err)
	}
	return nil
}

// Start starts the default controller; handles all events and creates all needed logical entities
func (oc *DefaultNetworkController) Start(ctx context.Context) error {
	klog.Infof("Starting the default network controller")

	err := oc.syncAddressSetsAndAcls()
	if err != nil {
		return err
	}
	if err = oc.Init(); err != nil {
		return err
	}

	return oc.Run(ctx)
}

// Stop gracefully stops the controller
func (oc *DefaultNetworkController) Stop() {
	close(oc.stopChan)
	oc.wg.Wait()
}

// Init runs a subnet IPAM and a controller that watches arrival/departure
// of nodes in the cluster
// On an addition to the cluster (node create), a new subnet is created for it that will translate
// to creation of a logical switch (done by the node, but could be created here at the master process too)
// Upon deletion of a node, the switch will be deleted
//
// TODO: Verify that the cluster was not already called with a different global subnet
//
//	If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func (oc *DefaultNetworkController) Init() error {
	existingNodes, err := oc.kube.GetNodes()
	if err != nil {
		klog.Errorf("Error in fetching nodes: %v", err)
		return err
	}
	klog.V(5).Infof("Existing number of nodes: %d", len(existingNodes.Items))
	err = oc.upgradeOVNTopology(existingNodes)
	if err != nil {
		klog.Errorf("Failed to upgrade OVN topology to version %d: %v", ovntypes.OvnCurrentTopologyVersion, err)
		return err
	}

	// FIXME: When https://github.com/ovn-org/libovsdb/issues/235 is fixed,
	// use IsTableSupported(nbdb.LoadBalancerGroup).
	if _, _, err := util.RunOVNNbctl("--columns=_uuid", "list", "Load_Balancer_Group"); err != nil {
		klog.Warningf("Load Balancer Group support enabled, however version of OVN in use does not support Load Balancer Groups.")
	} else {
		loadBalancerGroup := nbdb.LoadBalancerGroup{
			Name: ovntypes.ClusterLBGroupName,
		}
		err := libovsdbops.CreateOrUpdateLoadBalancerGroup(oc.nbClient, &loadBalancerGroup)
		if err != nil {
			klog.Errorf("Error creating cluster-wide load balancer group %s: %v", ovntypes.ClusterLBGroupName, err)
			return err
		}
		oc.clusterLoadBalancerGroupUUID = loadBalancerGroup.UUID

		loadBalancerGroup = nbdb.LoadBalancerGroup{
			Name: ovntypes.ClusterSwitchLBGroupName,
		}
		err = libovsdbops.CreateOrUpdateLoadBalancerGroup(oc.nbClient, &loadBalancerGroup)
		if err != nil {
			klog.Errorf("Error creating cluster-wide switch load balancer group %s: %v", ovntypes.ClusterSwitchLBGroupName, err)
			return err
		}
		oc.switchLoadBalancerGroupUUID = loadBalancerGroup.UUID

		loadBalancerGroup = nbdb.LoadBalancerGroup{
			Name: ovntypes.ClusterRouterLBGroupName,
		}
		err = libovsdbops.CreateOrUpdateLoadBalancerGroup(oc.nbClient, &loadBalancerGroup)
		if err != nil {
			klog.Errorf("Error creating cluster-wide router load balancer group %s: %v", ovntypes.ClusterRouterLBGroupName, err)
			return err
		}
		oc.routerLoadBalancerGroupUUID = loadBalancerGroup.UUID
	}

	nodeNames := []string{}
	for _, node := range existingNodes.Items {
		nodeNames = append(nodeNames, node.Name)
	}
	if err := oc.SetupMaster(nodeNames); err != nil {
		klog.Errorf("Failed to setup master (%v)", err)
		return err
	}

	return nil
}

// Run starts the actual watching.
func (oc *DefaultNetworkController) Run(ctx context.Context) error {
	oc.syncPeriodic()
	klog.Infof("Starting all the Watchers...")
	start := time.Now()

	// WatchNamespaces() should be started first because it has no other
	// dependencies, and WatchNodes() depends on it
	if err := WithSyncDurationMetric("namespace", oc.WatchNamespaces); err != nil {
		return err
	}

	// WatchNodes must be started next because it creates the node switch
	// which most other watches depend on.
	// https://github.com/ovn-org/ovn-kubernetes/pull/859
	if err := WithSyncDurationMetric("node", oc.WatchNodes); err != nil {
		return err
	}

	startSvc := time.Now()
	// Start service watch factory and sync services
	oc.svcFactory.Start(oc.stopChan)

	// Services should be started after nodes to prevent LB churn
	err := oc.StartServiceController(oc.wg, true)
	endSvc := time.Since(startSvc)
	metrics.MetricMasterSyncDuration.WithLabelValues("service").Set(endSvc.Seconds())
	if err != nil {
		return err
	}

	if err := WithSyncDurationMetric("pod", oc.WatchPods); err != nil {
		return err
	}

	// WatchNetworkPolicy depends on WatchPods and WatchNamespaces
	if err := WithSyncDurationMetric("network policy", oc.WatchNetworkPolicy); err != nil {
		return err
	}

	if config.OVNKubernetesFeature.EnableEgressIP {
		// This is probably the best starting order for all egress IP handlers.
		// WatchEgressIPNamespaces and WatchEgressIPPods only use the informer
		// cache to retrieve the egress IPs when determining if namespace/pods
		// match. It is thus better if we initialize them first and allow
		// WatchEgressNodes / WatchEgressIP to initialize after. Those handlers
		// might change the assignments of the existing objects. If we do the
		// inverse and start WatchEgressIPNamespaces / WatchEgressIPPod last, we
		// risk performing a bunch of modifications on the EgressIP objects when
		// we restart and then have these handlers act on stale data when they
		// sync.
		if err := WithSyncDurationMetric("egress ip namespace", oc.WatchEgressIPNamespaces); err != nil {
			return err
		}
		if err := WithSyncDurationMetric("egress ip pod", oc.WatchEgressIPPods); err != nil {
			return err
		}
		if err := WithSyncDurationMetric("egress node", oc.WatchEgressNodes); err != nil {
			return err
		}
		if err := WithSyncDurationMetric("egress ip", oc.WatchEgressIP); err != nil {
			return err
		}
		if config.OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout == 0 {
			klog.V(2).Infof("EgressIP node reachability check disabled")
		} else if config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort != 0 {
			klog.Infof("EgressIP node reachability enabled and using gRPC port %d",
				config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort)
		}
	}

	if config.OVNKubernetesFeature.EnableEgressFirewall {
		var err error
		oc.egressFirewallDNS, err = NewEgressDNS(oc.addressSetFactory, oc.controllerName, oc.stopChan)
		if err != nil {
			return err
		}
		oc.egressFirewallDNS.Run(egressFirewallDNSDefaultDuration)
		err = WithSyncDurationMetric("egress firewall", oc.WatchEgressFirewall)
		if err != nil {
			return err
		}
		err = oc.WatchEgressFwNodes()
		if err != nil {
			return err
		}
	}

	if config.OVNKubernetesFeature.EnableEgressQoS {
		err := oc.initEgressQoSController(
			oc.watchFactory.EgressQoSInformer(),
			oc.watchFactory.PodCoreInformer(),
			oc.watchFactory.NodeCoreInformer())
		if err != nil {
			return err
		}
		oc.wg.Add(1)
		go func() {
			defer oc.wg.Done()
			oc.runEgressQoSController(1, oc.stopChan)
		}()
	}

	if config.OVNKubernetesFeature.EnableEgressService {
		c, err := oc.InitEgressServiceZoneController()
		if err != nil {
			return fmt.Errorf("unable to create new egress service controller while creating new default network controller: %w", err)
		}
		oc.egressSvcController = c
		oc.wg.Add(1)
		go func() {
			defer oc.wg.Done()
			oc.egressSvcController.Run(1)
		}()
	}

	oc.wg.Add(1)
	go func() {
		defer oc.wg.Done()
		oc.apbExternalRouteController.Run(1)
	}()

	end := time.Since(start)
	klog.Infof("Completing all the Watchers took %v", end)
	metrics.MetricMasterSyncDuration.WithLabelValues("all watchers").Set(end.Seconds())

	if config.Kubernetes.OVNEmptyLbEvents {
		klog.Infof("Starting unidling controllers")
		unidlingController, err := unidling.NewController(
			oc.recorder,
			oc.watchFactory.ServiceInformer(),
			oc.sbClient,
		)
		if err != nil {
			return err
		}
		oc.wg.Add(1)
		go func() {
			defer oc.wg.Done()
			unidlingController.Run(oc.stopChan)
		}()

		_, err = unidling.NewUnidledAtController(oc.kube, oc.watchFactory.ServiceInformer())
		if err != nil {
			return err
		}
	}

	// Master is fully running and resource handlers have synced, update Topology version in OVN and the ConfigMap
	if err := oc.reportTopologyVersion(ctx); err != nil {
		klog.Errorf("Failed to report topology version: %v", err)
		return err
	}

	return nil
}

func WithSyncDurationMetric(resourceName string, f func() error) error {
	start := time.Now()
	defer func() {
		end := time.Since(start)
		metrics.MetricMasterSyncDuration.WithLabelValues(resourceName).Set(end.Seconds())
	}()
	return f()
}

func WithSyncDurationMetricNoError(resourceName string, f func()) {
	start := time.Now()
	defer func() {
		end := time.Since(start)
		metrics.MetricMasterSyncDuration.WithLabelValues(resourceName).Set(end.Seconds())
	}()
	f()
}

type defaultNetworkControllerEventHandler struct {
	baseHandler     baseNetworkControllerEventHandler
	watchFactory    *factory.WatchFactory
	objType         reflect.Type
	oc              *DefaultNetworkController
	extraParameters interface{}
	syncFunc        func([]interface{}) error
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *defaultNetworkControllerEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	return h.baseHandler.areResourcesEqual(h.objType, obj1, obj2)
}

// GetInternalCacheEntry returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (h *defaultNetworkControllerEventHandler) GetInternalCacheEntry(obj interface{}) interface{} {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		return h.oc.getPortInfo(pod)
	default:
		return nil
	}
}

// GetResourceFromInformerCache returns the latest state of the object, given an object key and its type.
// from the informers cache.
func (h *defaultNetworkControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	return h.baseHandler.getResourceFromInformerCache(h.objType, h.watchFactory, key)
}

// RecordAddEvent records the add event on this given object.
func (h *defaultNetworkControllerEventHandler) RecordAddEvent(obj interface{}) {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		klog.V(5).Infof("Recording add event on pod %s/%s", pod.Namespace, pod.Name)
		h.oc.podRecorder.AddPod(pod.UID)
		metrics.GetConfigDurationRecorder().Start("pod", pod.Namespace, pod.Name)
	case factory.PolicyType:
		np := obj.(*knet.NetworkPolicy)
		klog.V(5).Infof("Recording add event on network policy %s/%s", np.Namespace, np.Name)
		metrics.GetConfigDurationRecorder().Start("networkpolicy", np.Namespace, np.Name)
	}
}

// RecordUpdateEvent records the update event on this given object.
func (h *defaultNetworkControllerEventHandler) RecordUpdateEvent(obj interface{}) {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		klog.V(5).Infof("Recording update event on pod %s/%s", pod.Namespace, pod.Name)
		metrics.GetConfigDurationRecorder().Start("pod", pod.Namespace, pod.Name)
	case factory.PolicyType:
		np := obj.(*knet.NetworkPolicy)
		klog.V(5).Infof("Recording update event on network policy %s/%s", np.Namespace, np.Name)
		metrics.GetConfigDurationRecorder().Start("networkpolicy", np.Namespace, np.Name)
	}
}

// RecordDeleteEvent records the delete event on this given object.
func (h *defaultNetworkControllerEventHandler) RecordDeleteEvent(obj interface{}) {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		klog.V(5).Infof("Recording delete event on pod %s/%s", pod.Namespace, pod.Name)
		h.oc.podRecorder.CleanPod(pod.UID)
		metrics.GetConfigDurationRecorder().Start("pod", pod.Namespace, pod.Name)
	case factory.PolicyType:
		np := obj.(*knet.NetworkPolicy)
		klog.V(5).Infof("Recording delete event on network policy %s/%s", np.Namespace, np.Name)
		metrics.GetConfigDurationRecorder().Start("networkpolicy", np.Namespace, np.Name)
	}
}

// RecordSuccessEvent records the success event on this given object.
func (h *defaultNetworkControllerEventHandler) RecordSuccessEvent(obj interface{}) {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		klog.V(5).Infof("Recording success event on pod %s/%s", pod.Namespace, pod.Name)
		metrics.GetConfigDurationRecorder().End("pod", pod.Namespace, pod.Name)
	case factory.PolicyType:
		np := obj.(*knet.NetworkPolicy)
		klog.V(5).Infof("Recording success event on network policy %s/%s", np.Namespace, np.Name)
		metrics.GetConfigDurationRecorder().End("networkpolicy", np.Namespace, np.Name)
	}
}

// RecordErrorEvent records an error event on the given object.
// Only used for pods now.
func (h *defaultNetworkControllerEventHandler) RecordErrorEvent(obj interface{}, reason string, err error) {
	switch h.objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		klog.V(5).Infof("Recording error event on pod %s/%s", pod.Namespace, pod.Name)
		h.oc.recordPodEvent(reason, err, pod)
	case factory.NodeType:
		node := obj.(*kapi.Node)
		klog.V(5).Infof("Recording error event for node %s", node.Name)
		h.oc.recordNodeEvent(reason, err, node)
	}
}

// IsResourceScheduled returns true if the given object has been scheduled.
// Only applied to pods for now. Returns true for all other types.
func (h *defaultNetworkControllerEventHandler) IsResourceScheduled(obj interface{}) bool {
	return h.baseHandler.isResourceScheduled(h.objType, obj)
}

// AddResource adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
// Given an object to add and a boolean specifying if the function was executed from iterateRetryResources
func (h *defaultNetworkControllerEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	var err error

	switch h.objType {
	case factory.PodType:
		pod, ok := obj.(*kapi.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T object to *knet.Pod", obj)
		}
		return h.oc.ensurePod(nil, pod, true)

	case factory.PolicyType:
		np, ok := obj.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *knet.NetworkPolicy", obj)
		}

		if err = h.oc.addNetworkPolicy(np); err != nil {
			klog.Infof("Network Policy add failed for %s/%s, will try again later: %v",
				np.Namespace, np.Name, err)
			return err
		}

	case factory.NodeType:
		node, ok := obj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *kapi.Node", obj)
		}
		if h.oc.isLocalZoneNode(node) {
			var nodeParams *nodeSyncs
			if fromRetryLoop {
				_, nodeSync := h.oc.addNodeFailed.Load(node.Name)
				_, clusterRtrSync := h.oc.nodeClusterRouterPortFailed.Load(node.Name)
				_, mgmtSync := h.oc.mgmtPortFailed.Load(node.Name)
				_, gwSync := h.oc.gatewaysFailed.Load(node.Name)
				_, hoSync := h.oc.hybridOverlayFailed.Load(node.Name)
				_, zoneICSync := h.oc.syncZoneICFailed.Load(node.Name)
				nodeParams = &nodeSyncs{
					nodeSync,
					clusterRtrSync,
					mgmtSync,
					gwSync,
					hoSync,
					zoneICSync}
			} else {
				nodeParams = &nodeSyncs{true, true, true, true, config.HybridOverlay.Enabled, config.OVNKubernetesFeature.EnableInterconnect}
			}

			if err = h.oc.addUpdateLocalNodeEvent(node, nodeParams); err != nil {
				klog.Infof("Node add failed for %s, will try again later: %v",
					node.Name, err)
				return err
			}
		} else {
			if err = h.oc.addUpdateRemoteNodeEvent(node, config.OVNKubernetesFeature.EnableInterconnect); err != nil {
				return err
			}
		}

	case factory.EgressFirewallType:
		var err error
		egressFirewall := obj.(*egressfirewall.EgressFirewall).DeepCopy()
		if err = h.oc.addEgressFirewall(egressFirewall); err != nil {
			egressFirewall.Status.Status = egressFirewallAddError
		} else {
			egressFirewall.Status.Status = egressFirewallAppliedCorrectly
			metrics.UpdateEgressFirewallRuleCount(float64(len(egressFirewall.Spec.Egress)))
			metrics.IncrementEgressFirewallCount()
		}
		if statusErr := h.oc.updateEgressFirewallStatusWithRetry(egressFirewall); statusErr != nil {
			klog.Errorf("Failed to update egress firewall status %s, error: %v",
				getEgressFirewallNamespacedName(egressFirewall), statusErr)
		}
		return err

	case factory.EgressIPType:
		eIP := obj.(*egressipv1.EgressIP)
		return h.oc.reconcileEgressIP(nil, eIP)

	case factory.EgressIPNamespaceType:
		namespace := obj.(*kapi.Namespace)
		return h.oc.reconcileEgressIPNamespace(nil, namespace)

	case factory.EgressIPPodType:
		pod := obj.(*kapi.Pod)
		return h.oc.reconcileEgressIPPod(nil, pod)

	case factory.EgressNodeType:
		node := obj.(*kapi.Node)
		// Update node in zone cache; value will be true if node is local
		// to this zone and false if its not
		h.oc.eIPC.nodeZoneState.LockKey(node.Name)
		h.oc.eIPC.nodeZoneState.Store(node.Name, h.oc.isLocalZoneNode(node))
		h.oc.eIPC.nodeZoneState.UnlockKey(node.Name)
		// add the nodeIP to the default LRP (102 priority) destination address-set
		err := h.oc.ensureDefaultNoRerouteNodePolicies()
		if err != nil {
			return err
		}
		// add the GARP configuration for all the new nodes we get
		// since we use the "exclude-lb-vips-from-garp": "true"
		// we shouldn't have scale issues
		// NOTE: Adding GARP needs to be done only during node add
		// It is a one time operation and doesn't need to be done during
		// node updates. It needs to be done only for nodes local to this zone
		return h.oc.addEgressNode(node)

	case factory.EgressFwNodeType:
		node := obj.(*kapi.Node)
		if err = h.oc.updateEgressFirewallForNode(nil, node); err != nil {
			klog.Infof("Node add failed during egress firewall eval for node: %s, will try again later: %v",
				node.Name, err)
			return err
		}

	case factory.NamespaceType:
		ns, ok := obj.(*kapi.Namespace)
		if !ok {
			return fmt.Errorf("could not cast %T object to *kapi.Namespace", obj)
		}
		return h.oc.AddNamespace(ns)

	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}

	return nil
}

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (h *defaultNetworkControllerEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	switch h.objType {
	case factory.PodType:
		oldPod := oldObj.(*kapi.Pod)
		newPod := newObj.(*kapi.Pod)

		return h.oc.ensurePod(oldPod, newPod, inRetryCache || util.PodScheduled(oldPod) != util.PodScheduled(newPod))

	case factory.NodeType:
		newNode, ok := newObj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast newObj of type %T to *kapi.Node", newObj)
		}
		oldNode, ok := oldObj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast oldObj of type %T to *kapi.Node", oldObj)
		}

		// +--------------------+-------------------+-------------------------------------------------+
		// |    oldNode         |      newNode      |       Action                                    |
		// |--------------------+-------------------+-------------------------------------------------+
		// |                    |                   |     Node is remote.                             |
		// |    local           |      remote       |     Call addUpdateRemoteNodeEvent()             |
		// |                    |                   |                                                 |
		// |--------------------+-------------------+-------------------------------------------------+
		// |                    |                   |     Node is local                               |
		// |    local           |      local        |     Call addUpdateLocalNodeEvent()              |
		// |                    |                   |                                                 |
		// |--------------------+-------------------+-------------------------------------------------+
		// |                    |                   |     Node is local                               |
		// |    remote          |      local        |     Call addUpdateLocalNodeEvent(full sync)     |
		// |                    |                   |                                                 |
		// |--------------------+-------------------+-------------------------------------------------+
		// |                    |                   |     Node is remote                              |
		// |    remote          |      remote       |     Call addUpdateRemoteNodeEvent()             |
		// |                    |                   |                                                 |
		// |--------------------+-------------------+-------------------------------------------------+
		if h.oc.isLocalZoneNode(newNode) {
			var nodeSyncsParam *nodeSyncs
			if h.oc.isLocalZoneNode(oldNode) {
				// determine what actually changed in this update
				_, nodeSync := h.oc.addNodeFailed.Load(newNode.Name)
				_, failed := h.oc.nodeClusterRouterPortFailed.Load(newNode.Name)
				clusterRtrSync := failed || nodeChassisChanged(oldNode, newNode) || nodeSubnetChanged(oldNode, newNode)
				_, failed = h.oc.mgmtPortFailed.Load(newNode.Name)
				mgmtSync := failed || macAddressChanged(oldNode, newNode) || nodeSubnetChanged(oldNode, newNode)
				_, failed = h.oc.gatewaysFailed.Load(newNode.Name)
				gwSync := (failed || gatewayChanged(oldNode, newNode) ||
					nodeSubnetChanged(oldNode, newNode) || hostAddressesChanged(oldNode, newNode) ||
					nodeGatewayMTUSupportChanged(oldNode, newNode))
				_, hoSync := h.oc.hybridOverlayFailed.Load(newNode.Name)
				_, syncZoneIC := h.oc.syncZoneICFailed.Load(newNode.Name)
				nodeSyncsParam = &nodeSyncs{
					nodeSync,
					clusterRtrSync,
					mgmtSync,
					gwSync,
					hoSync,
					syncZoneIC}
			} else {
				klog.Infof("Node %s moved from the remote zone %s to local zone.",
					newNode.Name, util.GetNodeZone(oldNode), util.GetNodeZone(newNode))
				// The node is now a local zone node.  Trigger a full node sync.
				nodeSyncsParam = &nodeSyncs{true, true, true, true, true, config.OVNKubernetesFeature.EnableInterconnect}
			}

			return h.oc.addUpdateLocalNodeEvent(newNode, nodeSyncsParam)
		} else {
			_, syncZoneIC := h.oc.syncZoneICFailed.Load(newNode.Name)
			// Check if the node moved from local zone to remote zone and if so syncZoneIC should be set to true
			syncZoneIC = syncZoneIC || h.oc.isLocalZoneNode(oldNode)
			return h.oc.addUpdateRemoteNodeEvent(newNode, syncZoneIC)
		}

	case factory.EgressIPType:
		oldEIP := oldObj.(*egressipv1.EgressIP)
		newEIP := newObj.(*egressipv1.EgressIP)
		return h.oc.reconcileEgressIP(oldEIP, newEIP)

	case factory.EgressIPNamespaceType:
		oldNamespace := oldObj.(*kapi.Namespace)
		newNamespace := newObj.(*kapi.Namespace)
		return h.oc.reconcileEgressIPNamespace(oldNamespace, newNamespace)

	case factory.EgressIPPodType:
		oldPod := oldObj.(*kapi.Pod)
		newPod := newObj.(*kapi.Pod)
		return h.oc.reconcileEgressIPPod(oldPod, newPod)

	case factory.EgressNodeType:
		oldNode := oldObj.(*kapi.Node)
		newNode := newObj.(*kapi.Node)
		// Update node in zone cache; value will be true if node is local
		// to this zone and false if its not
		h.oc.eIPC.nodeZoneState.LockKey(newNode.Name)
		h.oc.eIPC.nodeZoneState.Store(newNode.Name, h.oc.isLocalZoneNode(newNode))
		h.oc.eIPC.nodeZoneState.UnlockKey(newNode.Name)
		// update the nodeIP in the defalt-reRoute (102 priority) destination address-set
		if util.NodeHostAddressesAnnotationChanged(oldNode, newNode) {
			klog.Infof("Egress IP detected IP address change for node %s. Updating no re-route policies", newNode.Name)
			err := h.oc.ensureDefaultNoRerouteNodePolicies()
			if err != nil {
				return err
			}
		}
		return nil

	case factory.EgressFwNodeType:
		oldNode := oldObj.(*kapi.Node)
		newNode := newObj.(*kapi.Node)
		return h.oc.updateEgressFirewallForNode(oldNode, newNode)

	case factory.NamespaceType:
		oldNs, newNs := oldObj.(*kapi.Namespace), newObj.(*kapi.Namespace)
		return h.oc.updateNamespace(oldNs, newNs)
	}
	return fmt.Errorf("no update function for object type %s", h.objType)
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *defaultNetworkControllerEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	switch h.objType {
	case factory.PodType:
		var portInfo *lpInfo
		pod := obj.(*kapi.Pod)

		if cachedObj != nil {
			portInfo = cachedObj.(*lpInfo)
		}
		return h.oc.removePod(pod, portInfo)

	case factory.PolicyType:
		knp, ok := obj.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.NetworkPolicy", obj)
		}
		return h.oc.deleteNetworkPolicy(knp)

	case factory.NodeType:
		node, ok := obj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.Node", obj)
		}
		return h.oc.deleteNodeEvent(node)

	case factory.EgressFirewallType:
		egressFirewall := obj.(*egressfirewall.EgressFirewall)
		if err := h.oc.deleteEgressFirewall(egressFirewall); err != nil {
			return err
		}
		metrics.UpdateEgressFirewallRuleCount(float64(-len(egressFirewall.Spec.Egress)))
		metrics.DecrementEgressFirewallCount()
		return nil

	case factory.EgressIPType:
		eIP := obj.(*egressipv1.EgressIP)
		return h.oc.reconcileEgressIP(eIP, nil)

	case factory.EgressIPNamespaceType:
		namespace := obj.(*kapi.Namespace)
		return h.oc.reconcileEgressIPNamespace(namespace, nil)

	case factory.EgressIPPodType:
		pod := obj.(*kapi.Pod)
		return h.oc.reconcileEgressIPPod(pod, nil)

	case factory.EgressNodeType:
		node := obj.(*kapi.Node)
		// remove the GARP setup for the node
		if err := h.oc.deleteEgressNode(node); err != nil {
			return err
		}
		// remove the IPs from the destination address-set of the default LRP (102)
		err := h.oc.ensureDefaultNoRerouteNodePolicies()
		if err != nil {
			return err
		}
		// Update node in zone cache; remove the node key since node has been deleted.
		h.oc.eIPC.nodeZoneState.LockKey(node.Name)
		h.oc.eIPC.nodeZoneState.Delete(node.Name)
		h.oc.eIPC.nodeZoneState.UnlockKey(node.Name)
		return nil

	case factory.EgressFwNodeType:
		node, ok := obj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.Node", obj)
		}
		return h.oc.updateEgressFirewallForNode(node, nil)

	case factory.NamespaceType:
		ns := obj.(*kapi.Namespace)
		return h.oc.deleteNamespace(ns)

	default:
		return fmt.Errorf("object type %s not supported", h.objType)
	}
}

func (h *defaultNetworkControllerEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.PodType:
			syncFunc = h.oc.syncPods

		case factory.PolicyType:
			syncFunc = h.oc.syncNetworkPolicies

		case factory.NodeType:
			syncFunc = h.oc.syncNodes

		case factory.EgressFirewallType:
			syncFunc = h.oc.syncEgressFirewall

		case factory.EgressIPNamespaceType:
			syncFunc = h.oc.syncEgressIPs

		case factory.EgressNodeType:
			syncFunc = h.oc.initClusterEgressPolicies

		case factory.EgressFwNodeType:
			syncFunc = nil

		case factory.EgressIPPodType,
			factory.EgressIPType:
			syncFunc = nil

		case factory.NamespaceType:
			syncFunc = h.oc.syncNamespaces

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

// IsObjectInTerminalState returns true if the given object is a in terminal state.
// This is used now for pods that are either in a PodSucceeded or in a PodFailed state.
func (h *defaultNetworkControllerEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	return h.baseHandler.isObjectInTerminalState(h.objType, obj)
}
