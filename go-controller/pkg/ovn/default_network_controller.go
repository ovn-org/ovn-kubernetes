package ovn

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressqoslisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/listers/egressqos/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/observability"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	anpcontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/admin_network_policy"
	apbroutecontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	egresssvc "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/egressservice"
	svccontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/unidling"
	dnsnameresolver "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/dns_name_resolver"
	aclsyncer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/external_ids_syncer/acl"
	addrsetsyncer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/external_ids_syncer/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/external_ids_syncer/port_group"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/topology"
	zoneic "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/wait"
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

	externalGatewayRouteInfo *apbroutecontroller.ExternalGatewayRouteInfoCache

	// egressFirewalls is a map of namespaces and the egressFirewall attached to it
	egressFirewalls sync.Map

	// EgressQoS
	egressQoSLister egressqoslisters.EgressQoSLister
	egressQoSSynced cache.InformerSynced
	egressQoSQueue  workqueue.TypedRateLimitingInterface[string]
	egressQoSCache  sync.Map

	egressQoSPodLister corev1listers.PodLister
	egressQoSPodSynced cache.InformerSynced
	egressQoSPodQueue  workqueue.TypedRateLimitingInterface[string]

	egressQoSNodeLister corev1listers.NodeLister
	egressQoSNodeSynced cache.InformerSynced
	egressQoSNodeQueue  workqueue.TypedRateLimitingInterface[string]

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

	// Controller in charge of services
	svcController *svccontroller.Controller

	// Controller used for programming OVN for egress IP
	eIPC egressIPZoneController

	// Controller used to handle egress services
	egressSvcController *egresssvc.Controller
	// Controller used for programming OVN for Admin Network Policy
	anpController *anpcontroller.Controller

	// Controller used to handle the admin policy based external route resources
	apbExternalRouteController *apbroutecontroller.ExternalGatewayMasterController

	// dnsNameResolver is used for resolving the IP addresses of DNS names
	// used in egress firewall rules
	dnsNameResolver  dnsnameresolver.DNSNameResolver
	efNodeController controller.Controller

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

	// Node-specific syncMaps used by node event handler
	gatewaysFailed              sync.Map
	mgmtPortFailed              sync.Map
	addNodeFailed               sync.Map
	nodeClusterRouterPortFailed sync.Map
	hybridOverlayFailed         sync.Map
	syncZoneICFailed            sync.Map
	syncHostNetAddrSetFailed    sync.Map

	// variable to determine if all pods present on the node during startup have been processed
	// updated atomically
	allInitialPodsProcessed uint32

	// zoneChassisHandler handles the local node and remote nodes in creating or updating the chassis entries in the OVN Southbound DB.
	// Please see zone_interconnect/chassis_handler.go for more details.
	zoneChassisHandler *zoneic.ZoneChassisHandler

	gatewayTopologyFactory *topology.GatewayTopologyFactory
}

// NewDefaultNetworkController creates a new OVN controller for creating logical network
// infrastructure and policy for default l3 network
func NewDefaultNetworkController(cnci *CommonNetworkControllerInfo, nadController *nad.NetAttachDefinitionController,
	observManager *observability.Manager) (*DefaultNetworkController, error) {
	stopChan := make(chan struct{})
	wg := &sync.WaitGroup{}
	return newDefaultNetworkControllerCommon(cnci, stopChan, wg, nil, nadController, observManager)
}

func newDefaultNetworkControllerCommon(cnci *CommonNetworkControllerInfo,
	defaultStopChan chan struct{}, defaultWg *sync.WaitGroup,
	addressSetFactory addressset.AddressSetFactory, nadController *nad.NetAttachDefinitionController,
	observManager *observability.Manager) (*DefaultNetworkController, error) {

	if addressSetFactory == nil {
		addressSetFactory = addressset.NewOvnAddressSetFactory(cnci.nbClient, config.IPv4Mode, config.IPv6Mode)
	}

	svcController, err := svccontroller.NewController(
		cnci.client, cnci.nbClient,
		cnci.watchFactory.ServiceCoreInformer(),
		cnci.watchFactory.EndpointSliceCoreInformer(),
		cnci.watchFactory.NodeCoreInformer(),
		nadController,
		cnci.recorder,
		&util.DefaultNetInfo{},
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create new service controller while creating new default network controller: %w", err)
	}

	var zoneICHandler *zoneic.ZoneInterconnectHandler
	var zoneChassisHandler *zoneic.ZoneChassisHandler
	if config.OVNKubernetesFeature.EnableInterconnect {
		zoneICHandler = zoneic.NewZoneInterconnectHandler(&util.DefaultNetInfo{}, cnci.nbClient, cnci.sbClient, cnci.watchFactory)
		zoneChassisHandler = zoneic.NewZoneChassisHandler(cnci.sbClient)
	}
	apbExternalRouteController, err := apbroutecontroller.NewExternalMasterController(
		cnci.kube.APBRouteClient,
		defaultStopChan,
		cnci.watchFactory.PodCoreInformer(),
		cnci.watchFactory.NamespaceInformer(),
		cnci.watchFactory.APBRouteInformer(),
		cnci.watchFactory.NodeCoreInformer().Lister(),
		cnci.nbClient,
		addressSetFactory,
		DefaultNetworkControllerName,
		cnci.zone,
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
			zoneICHandler:               zoneICHandler,
			cancelableCtx:               util.NewCancelableContext(),
			observManager:               observManager,
			nadController:               nadController,
		},
		externalGatewayRouteInfo: apbExternalRouteController.ExternalGWRouteInfoCache,
		eIPC: egressIPZoneController{
			NetInfo:            &util.DefaultNetInfo{},
			nodeUpdateMutex:    &sync.Mutex{},
			podAssignmentMutex: &sync.Mutex{},
			podAssignment:      make(map[string]*podAssignmentState),
			nbClient:           cnci.nbClient,
			watchFactory:       cnci.watchFactory,
			nodeZoneState:      syncmap.NewSyncMap[bool](),
		},
		loadbalancerClusterCache:   make(map[kapi.Protocol]string),
		zoneChassisHandler:         zoneChassisHandler,
		apbExternalRouteController: apbExternalRouteController,
		svcController:              svcController,
		gatewayTopologyFactory:     topology.NewGatewayTopologyFactory(cnci.nbClient),
	}
	// Allocate IPs for logical router port "GwRouterToJoinSwitchPrefix + OVNClusterRouter". This should always
	// allocate the first IPs in the join switch subnets.
	gwLRPIfAddrs, err := oc.getOVNClusterRouterPortToJoinSwitchIfAddrs()
	if err != nil {
		return nil, fmt.Errorf("failed to allocate join switch IP address connected to %s: %v", ovntypes.OVNClusterRouter, err)
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

func (oc *DefaultNetworkController) syncDb() error {
	// sync address sets and ACLs, only required for network controller, since any old objects in the db without
	// Owner set are owned by the default network controller.
	// The order of syncs is important, since the next syncer may rely on the data updated by the previous one.
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

	// port groups should be synced only once across all controllers (as port groups were used by secondary network
	// controllers before dbIDs, but SyncPortGroups knows how to get this info from the old ExternalIDs, that is also
	// why it doesn't have controllerName as an argument).
	// Do it here since DefaultNetworkController is always created, and this sync has dependencies with the other syncs
	// in this function. It uses acl.ExternalIDs[libovsdbops.ObjectNameKey.String()] to fetch namespace name for a
	// referenced port group (thus, SyncACLs should be called before).
	portGroupSyncer := port_group.NewPortGroupSyncer(oc.nbClient)
	if err = portGroupSyncer.SyncPortGroups(); err != nil {
		return fmt.Errorf("failed to sync port groups on controller init: %v", err)
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

	err := oc.syncDb()
	if err != nil {
		return err
	}
	if err = oc.Init(ctx); err != nil {
		return err
	}

	return oc.Run(ctx)
}

// Stop gracefully stops the controller
func (oc *DefaultNetworkController) Stop() {
	if oc.dnsNameResolver != nil {
		oc.dnsNameResolver.Shutdown()
	}
	if oc.efNodeController != nil {
		controller.Stop(oc.efNodeController)
	}

	close(oc.stopChan)
	oc.cancelableCtx.Cancel()
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
func (oc *DefaultNetworkController) Init(ctx context.Context) error {
	existingNodes, err := oc.kube.GetNodes()
	if err != nil {
		klog.Errorf("Error in fetching nodes: %v", err)
		return err
	}
	klog.V(5).Infof("Existing number of nodes: %d", len(existingNodes))

	// FIXME: When https://github.com/ovn-org/libovsdb/issues/235 is fixed,
	// use IsTableSupported(nbdb.LoadBalancerGroup).
	if _, _, err := util.RunOVNNbctl("--columns=_uuid", "list", "Load_Balancer_Group"); err != nil {
		klog.Warningf("Load Balancer Group support enabled, however version of OVN in use does not support Load Balancer Groups.")
	} else {
		clusterLBGroupUUID, switchLBGroupUUID, routerLBGroupUUID, err := initLoadBalancerGroups(oc.nbClient, oc.NetInfo)
		if err != nil {
			return err
		}
		oc.clusterLoadBalancerGroupUUID = clusterLBGroupUUID
		oc.switchLoadBalancerGroupUUID = switchLBGroupUUID
		oc.routerLoadBalancerGroupUUID = routerLBGroupUUID
	}

	networkID := util.InvalidNetworkID
	nodeNames := []string{}
	for _, node := range existingNodes {
		node := *node
		nodeNames = append(nodeNames, node.Name)

		if config.OVNKubernetesFeature.EnableInterconnect && networkID == util.InvalidNetworkID {
			// get networkID from any node in the cluster
			networkID, _ = util.ParseNetworkIDAnnotation(&node, oc.zoneICHandler.GetNetworkName())
		}
	}
	if err := oc.SetupMaster(nodeNames); err != nil {
		klog.Errorf("Failed to setup master (%v)", err)
		return err
	}
	// Sync external gateway routes. External gateway are set via Admin Policy Based External Route CRs.
	// So execute an individual sync method at startup to cleanup any difference
	klog.V(4).Info("Cleaning External Gateway ECMP routes")
	if err := WithSyncDurationMetric("external gateway routes", oc.apbExternalRouteController.Repair); err != nil {
		return err
	}

	return nil
}

// Run starts the actual watching.
func (oc *DefaultNetworkController) Run(ctx context.Context) error {
	oc.syncPeriodic()
	klog.Info("Starting all the Watchers...")
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
	// Services should be started after nodes to prevent LB churn
	err := oc.StartServiceController(oc.wg, true)
	endSvc := time.Since(startSvc)
	metrics.MetricOVNKubeControllerSyncDuration.WithLabelValues("service").Set(endSvc.Seconds())
	if err != nil {
		return err
	}

	if err := WithSyncDurationMetric("pod", oc.WatchPods); err != nil {
		return err
	}

	if config.OVNKubernetesFeature.EnableAdminNetworkPolicy {
		err := oc.newANPController()
		if err != nil {
			return fmt.Errorf("unable to create admin network policy controller, err: %v", err)
		}
		oc.wg.Add(1)
		go func() {
			defer oc.wg.Done()
			// Until we have scale issues in future let's spawn only one thread
			oc.anpController.Run(1, oc.stopChan)
		}()
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
	}

	if config.OVNKubernetesFeature.EnableEgressFirewall {
		var err error
		// If DNSNameResolver is enabled, then initialize dnsNameResolver to ExternalEgressDNS
		// for maintaining the address sets corresponding to the DNS names and start watching
		// DNSNameResolver resources. Otherwise initialize dnsNameResolver to EgressDNS.
		if config.OVNKubernetesFeature.EnableDNSNameResolver {
			oc.dnsNameResolver, err = dnsnameresolver.NewExternalEgressDNS(oc.addressSetFactory, oc.controllerName, true,
				oc.watchFactory.DNSNameResolverInformer().Informer(), oc.watchFactory.EgressFirewallInformer().Lister())
		} else {
			oc.dnsNameResolver, err = dnsnameresolver.NewEgressDNS(oc.addressSetFactory, oc.controllerName, oc.stopChan, egressFirewallDNSDefaultDuration)
		}
		if err != nil {
			return err
		}
		err = oc.dnsNameResolver.Run()
		if err != nil {
			return err
		}
		err = WithSyncDurationMetric("egress firewall", oc.WatchEgressFirewall)
		if err != nil {
			return err
		}
		oc.efNodeController = oc.newEFNodeController(oc.watchFactory.NodeCoreInformer())
		err = controller.Start(oc.efNodeController)
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
		if err = oc.runEgressQoSController(oc.wg, 1, oc.stopChan); err != nil {
			return err
		}
	}

	if config.OVNKubernetesFeature.EnableEgressService {
		c, err := oc.InitEgressServiceZoneController()
		if err != nil {
			return fmt.Errorf("unable to create new egress service controller while creating new default network controller: %w", err)
		}
		oc.egressSvcController = c
		if err = oc.egressSvcController.Run(oc.wg, 1); err != nil {
			return err
		}
	}

	if config.OVNKubernetesFeature.EnableMultiExternalGateway {
		if err = oc.apbExternalRouteController.Run(oc.wg, 1); err != nil {
			return err
		}
		// If interconnect is enabled and it is a multi-zone setup, then we flush conntrack
		// on ovnkube-controller side and not on ovnkube-node side, since they are run in the
		// same process. TODO(tssurya): In upstream ovnk, its possible to run these as different processes
		// in which case this flushing feature is not supported.
		if config.OVNKubernetesFeature.EnableInterconnect && oc.zone != ovntypes.OvnDefaultZone {
			// every minute cleanup stale conntrack entries if any
			go wait.Until(func() {
				oc.checkAndDeleteStaleConntrackEntries()
			}, time.Minute*1, oc.stopChan)
		}
	}

	if config.OVNKubernetesFeature.EnableNetworkQoS {
		err := oc.newNetworkQoSController()
		if err != nil {
			return fmt.Errorf("unable to create network qos controller, err: %w", err)
		}
		oc.wg.Add(1)
		go func() {
			defer oc.wg.Done()
			// Until we have scale issues in future let's spawn only one thread
			oc.nqosController.Run(1, oc.stopChan)
		}()
	}

	end := time.Since(start)
	klog.Infof("Completing all the Watchers took %v", end)
	metrics.MetricOVNKubeControllerSyncDuration.WithLabelValues("all watchers").Set(end.Seconds())

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
	}

	metrics.RunOVNKubeFeatureDBObjectsMetricsUpdater(oc.nbClient, oc.controllerName, 30*time.Second, oc.stopChan)

	return nil
}

func WithSyncDurationMetric(resourceName string, f func() error) error {
	start := time.Now()
	defer func() {
		end := time.Since(start)
		metrics.MetricOVNKubeControllerSyncDuration.WithLabelValues(resourceName).Set(end.Seconds())
	}()
	return f()
}

func WithSyncDurationMetricNoError(resourceName string, f func()) {
	start := time.Now()
	defer func() {
		end := time.Since(start)
		metrics.MetricOVNKubeControllerSyncDuration.WithLabelValues(resourceName).Set(end.Seconds())
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

	case factory.NodeType:
		node, ok := obj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *kapi.Node", obj)
		}
		if config.HybridOverlay.Enabled {
			if util.NoHostSubnet(node) {
				return h.oc.addUpdateHoNodeEvent(node)
			} else {
				// clean possible remainings for a node that is used to be a HO node
				if err := h.oc.deleteHoNodeEvent(node); err != nil {
					return err
				}
			}
		}
		var aggregatedErrors []error
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
				aggregatedErrors = append(aggregatedErrors, err)
			}
		} else {
			if err = h.oc.addUpdateRemoteNodeEvent(node, config.OVNKubernetesFeature.EnableInterconnect); err != nil {
				aggregatedErrors = append(aggregatedErrors, err)
			}
		}
		if err = h.oc.addIPToHostNetworkNamespaceAddrSet(node); err != nil {
			klog.Errorf("Failed to add node IPs to %s address_set: %v", config.Kubernetes.HostNetworkNamespace, err)
			h.oc.syncHostNetAddrSetFailed.Store(node.Name, true)
			aggregatedErrors = append(aggregatedErrors, err)
		}
		return utilerrors.Join(aggregatedErrors...)

	case factory.EgressFirewallType:
		egressFirewall := obj.(*egressfirewall.EgressFirewall).DeepCopy()
		err := h.oc.addEgressFirewall(egressFirewall)
		if statusErr := h.oc.setEgressFirewallStatus(egressFirewall, err); statusErr != nil {
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
		// add the 103 qos rule to new node's switch
		// NOTE: We don't need to remove this on node delete since entire node switch will get cleaned up
		if h.oc.isLocalZoneNode(node) {
			if err := h.oc.ensureDefaultNoRerouteQoSRules(node.Name); err != nil {
				return err
			}
		}
		// add the nodeIP to the default LRP (102 priority) destination address-set
		err := h.oc.ensureDefaultNoRerouteNodePolicies()
		if err != nil {
			return err
		}
		// Add routing specific to Egress IP NOTE: GARP configuration that
		// Egress IP depends on is added from the gateway reconciliation logic
		return h.oc.addEgressNode(node)

	case factory.NamespaceType:
		ns, ok := obj.(*kapi.Namespace)
		if !ok {
			return fmt.Errorf("could not cast %T object to *kapi.Namespace", obj)
		}
		return h.oc.AddNamespace(ns)

	default:
		return h.oc.AddResourceCommon(h.objType, obj)
	}
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
		var switchToOvnNode bool
		if config.HybridOverlay.Enabled {
			if util.NoHostSubnet(newNode) && !util.NoHostSubnet(oldNode) {
				klog.Infof("Node %s has been updated to be a remote/unmanaged hybrid overlay node", newNode.Name)
				return h.oc.addUpdateHoNodeEvent(newNode)
			} else if !util.NoHostSubnet(newNode) && util.NoHostSubnet(oldNode) {
				klog.Infof("Node %s has been updated to be an ovn-kubernetes managed node", newNode.Name)
				if err := h.oc.deleteHoNodeEvent(newNode); err != nil {
					return err
				}
				switchToOvnNode = true
			}
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
		newNodeIsLocalZoneNode := h.oc.isLocalZoneNode(newNode)
		zoneClusterChanged := h.oc.nodeZoneClusterChanged(oldNode, newNode, newNodeIsLocalZoneNode, types.DefaultNetworkName)
		nodeSubnetChanged := nodeSubnetChanged(oldNode, newNode, types.DefaultNetworkName)
		var aggregatedErrors []error
		if newNodeIsLocalZoneNode {
			var nodeSyncsParam *nodeSyncs
			if h.oc.isLocalZoneNode(oldNode) {
				// determine what actually changed in this update
				_, nodeSync := h.oc.addNodeFailed.Load(newNode.Name)
				_, failed := h.oc.nodeClusterRouterPortFailed.Load(newNode.Name)
				clusterRtrSync := failed || nodeChassisChanged(oldNode, newNode) || nodeSubnetChanged
				_, failed = h.oc.mgmtPortFailed.Load(newNode.Name)
				mgmtSync := failed || macAddressChanged(oldNode, newNode, types.DefaultNetworkName) || nodeSubnetChanged
				_, failed = h.oc.gatewaysFailed.Load(newNode.Name)
				gwSync := (failed || gatewayChanged(oldNode, newNode) ||
					nodeSubnetChanged || hostCIDRsChanged(oldNode, newNode) ||
					nodeGatewayMTUSupportChanged(oldNode, newNode))
				_, hoSync := h.oc.hybridOverlayFailed.Load(newNode.Name)
				_, syncZoneIC := h.oc.syncZoneICFailed.Load(newNode.Name)
				syncZoneIC = syncZoneIC || zoneClusterChanged || primaryAddrChanged(oldNode, newNode)
				nodeSyncsParam = &nodeSyncs{
					nodeSync,
					clusterRtrSync,
					mgmtSync,
					gwSync,
					hoSync,
					syncZoneIC}
			} else {
				klog.Infof("Node %s moved from the remote zone %s to local zone %s.",
					newNode.Name, util.GetNodeZone(oldNode), util.GetNodeZone(newNode))
				// The node is now a local zone node.  Trigger a full node sync.
				nodeSyncsParam = &nodeSyncs{true, true, true, true, true, config.OVNKubernetesFeature.EnableInterconnect}
			}

			if err := h.oc.addUpdateLocalNodeEvent(newNode, nodeSyncsParam); err != nil {
				aggregatedErrors = append(aggregatedErrors, err)
			}
		} else {
			_, syncZoneIC := h.oc.syncZoneICFailed.Load(newNode.Name)

			// Check if the node moved from local zone to remote zone and if so syncZoneIC should be set to true.
			// Also check if node subnet changed, so static routes are properly set
			// Also check if the node is used to be a hybrid overlay node
			syncZoneIC = syncZoneIC || h.oc.isLocalZoneNode(oldNode) || nodeSubnetChanged || zoneClusterChanged || primaryAddrChanged(oldNode, newNode) || switchToOvnNode
			if syncZoneIC {
				klog.Infof("Node %s in remote zone %s needs interconnect zone sync up. Zone cluster changed: %v",
					newNode.Name, util.GetNodeZone(newNode), zoneClusterChanged)
			}
			if err := h.oc.addUpdateRemoteNodeEvent(newNode, syncZoneIC); err != nil {
				aggregatedErrors = append(aggregatedErrors, err)
			}
		}
		_, syncHostNetAddrSet := h.oc.syncHostNetAddrSetFailed.Load(newNode.Name)
		if syncHostNetAddrSet {
			if err := h.oc.addIPToHostNetworkNamespaceAddrSet(newNode); err != nil {
				klog.Errorf("Failed to add node IPs to %s address_set: %v", config.Kubernetes.HostNetworkNamespace, err)
				aggregatedErrors = append(aggregatedErrors, err)
			} else {
				h.oc.syncHostNetAddrSetFailed.Delete(newNode.Name)
			}
		}
		return utilerrors.Join(aggregatedErrors...)

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
		// try to add the 103 qos rule to new node's switch if it doesn't exist
		// The reason we call this from update is because in case the add node event
		// did not succeed and we got an update node event which overrides the add event
		// and removes the add event from retry cache, we'd need to ensure the qos rule exists
		// NOTE: We don't need to remove this on node delete since entire node switch will get cleaned up
		if h.oc.isLocalZoneNode(newNode) {
			if err := h.oc.ensureDefaultNoRerouteQoSRules(newNode.Name); err != nil {
				return err
			}
		}
		// update the nodeIP in the defalt-reRoute (102 priority) destination address-set
		if util.NodeHostCIDRsAnnotationChanged(oldNode, newNode) {
			klog.Infof("Egress IP detected IP address change for node %s. Updating no re-route policies", newNode.Name)
			err := h.oc.ensureDefaultNoRerouteNodePolicies()
			if err != nil {
				return err
			}
		}
		return nil

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

	case factory.NamespaceType:
		ns := obj.(*kapi.Namespace)
		return h.oc.deleteNamespace(ns)

	default:
		return h.oc.DeleteResourceCommon(h.objType, obj)
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
