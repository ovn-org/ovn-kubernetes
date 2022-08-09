package ovn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	hocontroller "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	svccontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/unidling"
	corev1listers "k8s.io/client-go/listers/core/v1"

	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/subnetallocator"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	utilnet "k8s.io/utils/net"

	egressqoslisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/listers/egressqos/v1"
	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"

	"k8s.io/klog/v2"
)

const (
	egressFirewallDNSDefaultDuration time.Duration = 30 * time.Minute
)

// ACL logging severity levels
type ACLLoggingLevels struct {
	Allow string `json:"allow,omitempty"`
	Deny  string `json:"deny,omitempty"`
}

// namespaceInfo contains information related to a Namespace. Use oc.getNamespaceLocked()
// or oc.waitForNamespaceLocked() to get a locked namespaceInfo for a Namespace, and call
// nsInfo.Unlock() on it when you are done with it. (No code outside of the code that
// manages the oc.namespaces map is ever allowed to hold an unlocked namespaceInfo.)
type namespaceInfo struct {
	sync.RWMutex

	// addressSet is an address set object that holds the IP addresses
	// of all pods in the namespace.
	addressSet addressset.AddressSet

	// map from NetworkPolicy name to networkPolicy. You must hold the
	// namespaceInfo's mutex to add/delete/lookup policies, but must hold the
	// networkPolicy's mutex (and not necessarily the namespaceInfo's) to work with
	// the policy itself.
	networkPolicies map[string]*networkPolicy

	// routingExternalGWs is a slice of net.IP containing the values parsed from
	// annotation k8s.ovn.org/routing-external-gws
	routingExternalGWs gatewayInfo

	// routingExternalPodGWs contains a map of all pods serving as exgws as well as their
	// exgw IPs
	// key is <namespace>_<pod name>
	routingExternalPodGWs map[string]gatewayInfo

	multicastEnabled bool

	// If not empty, then it has to be set to a logging a severity level, e.g. "notice", "alert", etc
	aclLogging ACLLoggingLevels

	// Per-namespace port group default deny UUIDs
	portGroupIngressDenyName string // Port group Name for ingress deny rule
	portGroupEgressDenyName  string // Port group Name for egress deny rule
}

// Controller structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints)
type Controller struct {
	client       clientset.Interface
	kube         kube.Interface
	watchFactory *factory.WatchFactory
	stopChan     <-chan struct{}

	// FIXME DUAL-STACK -  Make IP Allocators more dual-stack friendly
	masterSubnetAllocator *subnetallocator.SubnetAllocator

	hoMaster *hocontroller.MasterController

	SCTPSupport bool

	// For TCP, UDP, and SCTP type traffic, cache OVN load-balancers used for the
	// cluster's east-west traffic.
	loadbalancerClusterCache map[kapi.Protocol]string

	// A cache of all logical switches seen by the watcher and their subnets
	lsManager *lsm.LogicalSwitchManager

	// A cache of all logical ports known to the controller
	logicalPortCache *portCache

	// Info about known namespaces. You must use oc.getNamespaceLocked() or
	// oc.waitForNamespaceLocked() to read this map, and oc.createNamespaceLocked()
	// or oc.deleteNamespaceLocked() to modify it. namespacesMutex is only held
	// from inside those functions.
	namespaces      map[string]*namespaceInfo
	namespacesMutex sync.Mutex

	externalGWCache map[ktypes.NamespacedName]*externalRouteInfo
	exGWCacheMutex  sync.RWMutex

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

	// An address set factory that creates address sets
	addressSetFactory addressset.AddressSetFactory

	// For each logical port, the number of network policies that want
	// to add an ingress deny rule.
	lspIngressDenyCache map[string]int

	// For each logical port, the number of network policies that want
	// to add an egress deny rule.
	lspEgressDenyCache map[string]int

	// A mutex for lspIngressDenyCache and lspEgressDenyCache
	lspMutex *sync.Mutex

	// Supports multicast?
	multicastSupport bool

	// Cluster wide Load_Balancer_Group UUID.
	loadBalancerGroupUUID string

	// Cluster-wide gateway router default Control Plane Protection (COPP) UUID
	defaultGatewayCOPPUUID string

	// Controller used for programming OVN for egress IP
	eIPC egressIPController

	// Controller used to handle services
	svcController *svccontroller.Controller
	// svcFactory used to handle service related events
	svcFactory informers.SharedInformerFactory

	egressFirewallDNS *EgressDNS

	// Is ACL logging enabled while configuring meters?
	aclLoggingEnabled bool

	joinSwIPManager *lsm.JoinSwitchIPManager

	// event recorder used to post events to k8s
	recorder record.EventRecorder

	// libovsdb northbound client interface
	nbClient libovsdbclient.Client

	// libovsdb southbound client interface
	sbClient libovsdbclient.Client

	// v4HostSubnetsUsed keeps track of number of v4 subnets currently assigned to nodes
	v4HostSubnetsUsed float64

	// v6HostSubnetsUsed keeps track of number of v6 subnets currently assigned to nodes
	v6HostSubnetsUsed float64

	// Objects for pods that need to be retried
	retryPods *RetryObjs

	// Objects for network policies that need to be retried
	retryNetworkPolicies *RetryObjs

	// Objects for egress firewall that need to be retried
	retryEgressFirewalls *RetryObjs

	// Objects for egress IP that need to be retried
	retryEgressIPs *RetryObjs
	// Objects for egress IP Namespaces that need to be retried
	retryEgressIPNamespaces *RetryObjs
	// Objects for egress IP Pods that need to be retried
	retryEgressIPPods *RetryObjs
	// Objects for Egress nodes that need to be retried
	retryEgressNodes *RetryObjs
	// EgressIP Node-specific syncMap used by egressip node event handler
	addEgressNodeFailed sync.Map
	// Objects for nodes that need to be retried
	retryNodes *RetryObjs
	// Objects for Cloud private IP config that need to be retried
	retryCloudPrivateIPConfig *RetryObjs
	// Node-specific syncMap used by node event handler
	gatewaysFailed              sync.Map
	mgmtPortFailed              sync.Map
	addNodeFailed               sync.Map
	nodeClusterRouterPortFailed sync.Map

	podRecorder metrics.PodRecorder
}

const (
	// TCP is the constant string for the string "TCP"
	TCP = "TCP"

	// UDP is the constant string for the string "UDP"
	UDP = "UDP"

	// SCTP is the constant string for the string "SCTP"
	SCTP = "SCTP"
)

func GetIPFullMask(ip string) string {
	const (
		// IPv4FullMask is the maximum prefix mask for an IPv4 address
		IPv4FullMask = "/32"
		// IPv6FullMask is the maxiumum prefix mask for an IPv6 address
		IPv6FullMask = "/128"
	)

	if utilnet.IsIPv6(net.ParseIP(ip)) {
		return IPv6FullMask
	}
	return IPv4FullMask
}

// getPodNamespacedName returns <namespace>_<podname> for the provided pod
func getPodNamespacedName(pod *kapi.Pod) string {
	return util.GetLogicalPortName(pod.Namespace, pod.Name)
}

// NewOvnController creates a new OVN controller for creating logical network
// infrastructure and policy
func NewOvnController(ovnClient *util.OVNClientset, wf *factory.WatchFactory, stopChan <-chan struct{}, addressSetFactory addressset.AddressSetFactory,
	libovsdbOvnNBClient libovsdbclient.Client, libovsdbOvnSBClient libovsdbclient.Client,
	recorder record.EventRecorder) *Controller {
	if addressSetFactory == nil {
		addressSetFactory = addressset.NewOvnAddressSetFactory(libovsdbOvnNBClient)
	}
	svcController, svcFactory := newServiceController(ovnClient.KubeClient, libovsdbOvnNBClient, recorder)
	return &Controller{
		client: ovnClient.KubeClient,
		kube: &kube.Kube{
			KClient:              ovnClient.KubeClient,
			EIPClient:            ovnClient.EgressIPClient,
			EgressFirewallClient: ovnClient.EgressFirewallClient,
			CloudNetworkClient:   ovnClient.CloudNetworkClient,
		},
		watchFactory:          wf,
		stopChan:              stopChan,
		masterSubnetAllocator: subnetallocator.NewSubnetAllocator(),
		lsManager:             lsm.NewLogicalSwitchManager(),
		logicalPortCache:      newPortCache(stopChan),
		namespaces:            make(map[string]*namespaceInfo),
		namespacesMutex:       sync.Mutex{},
		externalGWCache:       make(map[ktypes.NamespacedName]*externalRouteInfo),
		exGWCacheMutex:        sync.RWMutex{},
		addressSetFactory:     addressSetFactory,
		lspIngressDenyCache:   make(map[string]int),
		lspEgressDenyCache:    make(map[string]int),
		lspMutex:              &sync.Mutex{},
		eIPC: egressIPController{
			egressIPAssignmentMutex:           &sync.Mutex{},
			podAssignmentMutex:                &sync.Mutex{},
			podAssignment:                     make(map[string]*podAssignmentState),
			pendingCloudPrivateIPConfigsMutex: &sync.Mutex{},
			pendingCloudPrivateIPConfigsOps:   make(map[string]map[string]*cloudPrivateIPConfigOp),
			allocator:                         allocator{&sync.Mutex{}, make(map[string]*egressNode)},
			nbClient:                          libovsdbOvnNBClient,
			watchFactory:                      wf,
			egressIPTotalTimeout:              config.OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout,
		},
		loadbalancerClusterCache:  make(map[kapi.Protocol]string),
		multicastSupport:          config.EnableMulticast,
		loadBalancerGroupUUID:     "",
		aclLoggingEnabled:         true,
		joinSwIPManager:           nil,
		retryPods:                 NewRetryObjs(factory.PodType, "", nil, nil, nil),
		retryNetworkPolicies:      NewRetryObjs(factory.PolicyType, "", nil, nil, nil),
		retryNodes:                NewRetryObjs(factory.NodeType, "", nil, nil, nil),
		retryEgressFirewalls:      NewRetryObjs(factory.EgressFirewallType, "", nil, nil, nil),
		retryEgressIPs:            NewRetryObjs(factory.EgressIPType, "", nil, nil, nil),
		retryEgressIPNamespaces:   NewRetryObjs(factory.EgressIPNamespaceType, "", nil, nil, nil),
		retryEgressIPPods:         NewRetryObjs(factory.EgressIPPodType, "", nil, nil, nil),
		retryEgressNodes:          NewRetryObjs(factory.EgressNodeType, "", nil, nil, nil),
		retryCloudPrivateIPConfig: NewRetryObjs(factory.CloudPrivateIPConfigType, "", nil, nil, nil),
		recorder:                  recorder,
		nbClient:                  libovsdbOvnNBClient,
		sbClient:                  libovsdbOvnSBClient,
		svcController:             svcController,
		svcFactory:                svcFactory,
		podRecorder:               metrics.NewPodRecorder(),
	}
}

// Run starts the actual watching.
func (oc *Controller) Run(ctx context.Context, wg *sync.WaitGroup) error {
	// Start and sync the watch factory to begin listening for events
	if err := oc.watchFactory.Start(); err != nil {
		return err
	}

	oc.syncPeriodic()
	klog.Infof("Starting all the Watchers...")
	start := time.Now()

	// Sync external gateway routes. External gateway may be set in namespaces
	// or via pods. So execute an individual sync method at startup
	oc.cleanExGwECMPRoutes()

	// WatchNamespaces() should be started first because it has no other
	// dependencies, and WatchNodes() depends on it
	if err := oc.WatchNamespaces(); err != nil {
		return err
	}

	// WatchNodes must be started next because it creates the node switch
	// which most other watches depend on.
	// https://github.com/ovn-org/ovn-kubernetes/pull/859
	if err := oc.WatchNodes(); err != nil {
		return err
	}

	// Start service watch factory and sync services
	oc.svcFactory.Start(oc.stopChan)

	// Services should be started after nodes to prevent LB churn
	if err := oc.StartServiceController(wg, true); err != nil {
		return err
	}

	if err := oc.WatchPods(); err != nil {
		return err
	}

	// WatchNetworkPolicy depends on WatchPods and WatchNamespaces
	if err := oc.WatchNetworkPolicy(); err != nil {
		return err
	}
	// Clean up stale L4 network policies
	oc.CleanStaleNetworkPolicy()
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
		if err := oc.WatchEgressIPNamespaces(); err != nil {
			return err
		}
		if err := oc.WatchEgressIPPods(); err != nil {
			return err
		}
		if err := oc.WatchEgressNodes(); err != nil {
			return err
		}
		if err := oc.WatchEgressIP(); err != nil {
			return err
		}
		if util.PlatformTypeIsEgressIPCloudProvider() {
			if err := oc.WatchCloudPrivateIPConfig(); err != nil {
				return err
			}
		}
		if config.OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout == 0 {
			klog.V(2).Infof("EgressIP node reachability check disabled")
		}
	}

	if config.OVNKubernetesFeature.EnableEgressFirewall {
		var err error
		oc.egressFirewallDNS, err = NewEgressDNS(oc.addressSetFactory, oc.stopChan)
		if err != nil {
			return err
		}
		oc.egressFirewallDNS.Run(egressFirewallDNSDefaultDuration)
		err = oc.WatchEgressFirewall()
		if err != nil {
			return err
		}
	}

	if config.OVNKubernetesFeature.EnableEgressQoS {
		oc.initEgressQoSController(
			oc.watchFactory.EgressQoSInformer(),
			oc.watchFactory.PodCoreInformer(),
			oc.watchFactory.NodeCoreInformer())
		wg.Add(1)
		go func() {
			defer wg.Done()
			oc.runEgressQoSController(1, oc.stopChan)
		}()
	}

	klog.Infof("Completing all the Watchers took %v", time.Since(start))

	if config.Kubernetes.OVNEmptyLbEvents {
		klog.Infof("Starting unidling controller")
		unidlingController, err := unidling.NewController(
			oc.recorder,
			oc.watchFactory.ServiceInformer(),
			oc.sbClient,
		)
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			unidlingController.Run(oc.stopChan)
		}()
	}

	if oc.hoMaster != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			oc.hoMaster.Run(oc.stopChan)
		}()
	}

	// Final step to cleanup after resource handlers have synced
	err := oc.ovnTopologyCleanup()
	if err != nil {
		klog.Errorf("Failed to cleanup OVN topology to version %d: %v", ovntypes.OvnCurrentTopologyVersion, err)
		return err
	}

	// Master is fully running and resource handlers have synced, update Topology version in OVN and the ConfigMap
	if err := oc.reportTopologyVersion(ctx); err != nil {
		klog.Errorf("Failed to report topology version: %v", err)
		return err
	}

	return nil
}

// syncPeriodic adds a goroutine that periodically does some work
// right now there is only one ticker registered
// for syncNodesPeriodic which deletes chassis records from the sbdb
// every 5 minutes
func (oc *Controller) syncPeriodic() {
	go func() {
		nodeSyncTicker := time.NewTicker(5 * time.Minute)
		defer nodeSyncTicker.Stop()
		for {
			select {
			case <-nodeSyncTicker.C:
				oc.syncNodesPeriodic()
			case <-oc.stopChan:
				return
			}
		}
	}()
}

func (oc *Controller) recordPodEvent(addErr error, pod *kapi.Pod) {
	podRef, err := ref.GetReference(scheme.Scheme, pod)
	if err != nil {
		klog.Errorf("Couldn't get a reference to pod %s/%s to post an event: '%v'",
			pod.Namespace, pod.Name, err)
	} else {
		klog.V(5).Infof("Posting a %s event for Pod %s/%s", kapi.EventTypeWarning, pod.Namespace, pod.Name)
		oc.recorder.Eventf(podRef, kapi.EventTypeWarning, "ErrorAddingLogicalPort", addErr.Error())
	}
}

func exGatewayAnnotationsChanged(oldPod, newPod *kapi.Pod) bool {
	return oldPod.Annotations[util.RoutingNamespaceAnnotation] != newPod.Annotations[util.RoutingNamespaceAnnotation] ||
		oldPod.Annotations[util.RoutingNetworkAnnotation] != newPod.Annotations[util.RoutingNetworkAnnotation] ||
		oldPod.Annotations[util.BfdAnnotation] != newPod.Annotations[util.BfdAnnotation]
}

func networkStatusAnnotationsChanged(oldPod, newPod *kapi.Pod) bool {
	return oldPod.Annotations[nettypes.NetworkStatusAnnot] != newPod.Annotations[nettypes.NetworkStatusAnnot]
}

// ensurePod tries to set up a pod. It returns nil on success and error on failure; failure
// indicates the pod set up should be retried later.
func (oc *Controller) ensurePod(oldPod, pod *kapi.Pod, addPort bool) error {
	// Try unscheduled pods later
	if !util.PodScheduled(pod) {
		return nil
	}

	if oldPod != nil && (exGatewayAnnotationsChanged(oldPod, pod) || networkStatusAnnotationsChanged(oldPod, pod)) {
		// No matter if a pod is ovn networked, or host networked, we still need to check for exgw
		// annotations. If the pod is ovn networked and is in update reschedule, addLogicalPort will take
		// care of updating the exgw updates
		if err := oc.deletePodExternalGW(oldPod); err != nil {
			return fmt.Errorf("ensurePod failed %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	if util.PodWantsNetwork(pod) && addPort {
		if err := oc.addLogicalPort(pod); err != nil {
			return fmt.Errorf("addLogicalPort failed for %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	} else {
		// either pod is host-networked or its an update for a normal pod (addPort=false case)
		if oldPod == nil || exGatewayAnnotationsChanged(oldPod, pod) || networkStatusAnnotationsChanged(oldPod, pod) {
			if err := oc.addPodExternalGW(pod); err != nil {
				return fmt.Errorf("addPodExternalGW failed for %s/%s: %w", pod.Namespace, pod.Name, err)
			}
		}
	}

	return nil
}

// removePod tried to tear down a pod. It returns nil on success and error on failure;
// failure indicates the pod tear down should be retried later.
func (oc *Controller) removePod(pod *kapi.Pod, portInfo *lpInfo) error {
	if !util.PodWantsNetwork(pod) {
		if err := oc.deletePodExternalGW(pod); err != nil {
			return fmt.Errorf("unable to delete external gateway routes for pod %s: %w",
				getPodNamespacedName(pod), err)
		}
		return nil
	}
	if err := oc.deleteLogicalPort(pod, portInfo); err != nil {
		return fmt.Errorf("deleteLogicalPort failed for pod %s: %w",
			getPodNamespacedName(pod), err)
	}
	return nil
}

// WatchPods starts the watching of the Pod resource and calls back the appropriate handler logic
func (oc *Controller) WatchPods() error {
	_, err := oc.WatchResource(oc.retryPods)
	return err
}

// now that we have added all the OVN ACLs with optimization, it is time to remove the stale OVN
// ACL entries from the database
func (oc *Controller) CleanStaleNetworkPolicy() {
	start := time.Now()
	defer func() {
		klog.V(5).Infof("Completed cleaning up stale OVN ACLs in %v", time.Since(start))
	}()

	klog.V(5).Infof("Cleaning up stale OVN ACLs that are left behind after L4 Port consolidation")
	// want ACLs that don't have l4fused key and have l4Match set (but not to None)
	pACL := func(item *nbdb.ACL) bool {
		if _, ok := item.ExternalIDs[l4MatchFusedExtIdKey]; !ok {
			if val, ok := item.ExternalIDs[l4MatchACLExtIdKey]; ok {
				if val != noneMatch {
					return true
				}
			}
		}
		return false
	}

	staleACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, pACL)
	if err != nil {
		klog.Warningf("Failed to retrieve stale OVN ACL entries that were not optimized " +
			"for L4 Ports consolidation: %v, err")
		return
	}
	// it could be that delete all the acls in one go might fail for various reasons,
	// so lets try to delete one at a time so that we can remove as many stale acls
	// as possible.
	klog.V(5).Infof("Number of stale ACLS to be cleaned is %d", len(staleACLs))
	for _, staleACL := range staleACLs {
		staleACL := staleACL
		nsName := staleACL.ExternalIDs[namespaceACLExtIdKey]
		policyName := staleACL.ExternalIDs[policyACLExtIdKey]
		pgName := fmt.Sprintf("%s_%s", nsName, policyName)
		pgName = hashedPortGroup(pgName)
		aclDesc := fmt.Sprintf("stale ACL %s/%s/%s in port group %s", staleACL.UUID, nsName, policyName, pgName)
		klog.V(5).Infof("About to delete %s", aclDesc)
		ops, err := libovsdbops.DeleteACLsFromPortGroupOps(oc.nbClient, nil, pgName, staleACL)
		if err != nil {
			klog.Warningf("Failed to get ops to delete %s: %v", aclDesc, err)
			continue
		}
		_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
		if err != nil {
			klog.Warningf("Failed to delete %s: %v", aclDesc, err)
		}
	}
}

// WatchNetworkPolicy starts the watching of the network policy resource and calls
// back the appropriate handler logic
func (oc *Controller) WatchNetworkPolicy() error {
	_, err := oc.WatchResource(oc.retryNetworkPolicies)
	return err
}

// WatchEgressFirewall starts the watching of egressfirewall resource and calls
// back the appropriate handler logic
func (oc *Controller) WatchEgressFirewall() error {
	_, err := oc.WatchResource(oc.retryEgressFirewalls)
	return err
}

// WatchEgressNodes starts the watching of egress assignable nodes and calls
// back the appropriate handler logic.
func (oc *Controller) WatchEgressNodes() error {
	_, err := oc.WatchResource(oc.retryEgressNodes)
	return err
}

// WatchCloudPrivateIPConfig starts the watching of cloudprivateipconfigs
// resource and calls back the appropriate handler logic.
func (oc *Controller) WatchCloudPrivateIPConfig() error {
	_, err := oc.WatchResource(oc.retryCloudPrivateIPConfig)
	return err
}

// WatchEgressIP starts the watching of egressip resource and calls back the
// appropriate handler logic. It also initiates the other dedicated resource
// handlers for egress IP setup: namespaces, pods.
func (oc *Controller) WatchEgressIP() error {
	_, err := oc.WatchResource(oc.retryEgressIPs)
	return err
}

func (oc *Controller) WatchEgressIPNamespaces() error {
	_, err := oc.WatchResource(oc.retryEgressIPNamespaces)
	return err
}

func (oc *Controller) WatchEgressIPPods() error {
	_, err := oc.WatchResource(oc.retryEgressIPPods)
	return err
}

// WatchNamespaces starts the watching of namespace resource and calls
// back the appropriate handler logic
func (oc *Controller) WatchNamespaces() error {
	start := time.Now()
	_, err := oc.watchFactory.AddNamespaceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ns := obj.(*kapi.Namespace)
			oc.AddNamespace(ns)
		},
		UpdateFunc: func(old, newer interface{}) {
			oldNs, newNs := old.(*kapi.Namespace), newer.(*kapi.Namespace)
			oc.updateNamespace(oldNs, newNs)
		},
		DeleteFunc: func(obj interface{}) {
			ns := obj.(*kapi.Namespace)
			oc.deleteNamespace(ns)
		},
	}, oc.syncNamespaces)
	klog.Infof("Bootstrapping existing namespaces and cleaning stale namespaces took %v", time.Since(start))
	if err != nil {
		klog.Errorf("Failed to watch namespaces err: %v", err)
		return err
	}
	return nil
}

// syncNodeGateway ensures a node's gateway router is configured
func (oc *Controller) syncNodeGateway(node *kapi.Node, hostSubnets []*net.IPNet) error {
	l3GatewayConfig, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil {
		return err
	}

	if hostSubnets == nil {
		hostSubnets, err = util.ParseNodeHostSubnetAnnotation(node)
		if err != nil {
			return err
		}
	}

	if l3GatewayConfig.Mode == config.GatewayModeDisabled {
		if err := oc.gatewayCleanup(node.Name); err != nil {
			return fmt.Errorf("error cleaning up gateway for node %s: %v", node.Name, err)
		}
		if err := oc.joinSwIPManager.ReleaseJoinLRPIPs(node.Name); err != nil {
			return err
		}
	} else if hostSubnets != nil {
		var hostAddrs sets.String
		if config.Gateway.Mode == config.GatewayModeShared {
			hostAddrs, err = util.ParseNodeHostAddresses(node)
			if err != nil && !util.IsAnnotationNotSetError(err) {
				return fmt.Errorf("failed to get host addresses for node: %s: %v", node.Name, err)
			}
		}
		if err := oc.syncGatewayLogicalNetwork(node, l3GatewayConfig, hostSubnets, hostAddrs); err != nil {
			return fmt.Errorf("error creating gateway for node %s: %v", node.Name, err)
		}
	}
	return nil
}

// WatchNodes starts the watching of node resource and calls
// back the appropriate handler logic
func (oc *Controller) WatchNodes() error {
	_, err := oc.WatchResource(oc.retryNodes)
	return err
}

// GetNetworkPolicyACLLogging retrieves ACL deny policy logging setting for the Namespace
func (oc *Controller) GetNetworkPolicyACLLogging(ns string) *ACLLoggingLevels {
	nsInfo, nsUnlock := oc.getNamespaceLocked(ns, true)
	if nsInfo == nil {
		return &ACLLoggingLevels{
			Allow: "",
			Deny:  "",
		}
	}
	defer nsUnlock()
	return &nsInfo.aclLogging
}

// Verify if controller can support ACL logging and validate annotation
func (oc *Controller) aclLoggingCanEnable(annotation string, nsInfo *namespaceInfo) bool {
	if !oc.aclLoggingEnabled || annotation == "" {
		nsInfo.aclLogging.Deny = ""
		nsInfo.aclLogging.Allow = ""
		return false
	}
	var aclLevels ACLLoggingLevels
	err := json.Unmarshal([]byte(annotation), &aclLevels)
	if err != nil {
		return false
	}

	// Using newDenyLoggingLevel and newAllowLoggingLevel allows resetting nsinfo state.
	// This is important if a user sets either the allow level or the deny level flag to an
	// invalid value or after they remove either the allow or the deny annotation.
	// If either of the 2 (allow or deny logging level) is set with a valid level, return true.
	newDenyLoggingLevel := ""
	newAllowLoggingLevel := ""
	okCnt := 0
	for _, s := range []string{"alert", "warning", "notice", "info", "debug"} {
		if s == aclLevels.Deny {
			newDenyLoggingLevel = aclLevels.Deny
			okCnt++
		}
		if s == aclLevels.Allow {
			newAllowLoggingLevel = aclLevels.Allow
			okCnt++
		}
	}
	nsInfo.aclLogging.Deny = newDenyLoggingLevel
	nsInfo.aclLogging.Allow = newAllowLoggingLevel
	return okCnt > 0
}

// gatewayChanged() compares old annotations to new and returns true if something has changed.
func gatewayChanged(oldNode, newNode *kapi.Node) bool {
	oldL3GatewayConfig, _ := util.ParseNodeL3GatewayAnnotation(oldNode)
	l3GatewayConfig, _ := util.ParseNodeL3GatewayAnnotation(newNode)
	return !reflect.DeepEqual(oldL3GatewayConfig, l3GatewayConfig)
}

// hostAddressesChanged compares old annotations to new and returns true if the something has changed.
func hostAddressesChanged(oldNode, newNode *kapi.Node) bool {
	oldAddrs, _ := util.ParseNodeHostAddresses(oldNode)
	Addrs, _ := util.ParseNodeHostAddresses(newNode)
	return !oldAddrs.Equal(Addrs)
}

// macAddressChanged() compares old annotations to new and returns true if something has changed.
func macAddressChanged(oldNode, node *kapi.Node) bool {
	oldMacAddress, _ := util.ParseNodeManagementPortMACAddress(oldNode)
	macAddress, _ := util.ParseNodeManagementPortMACAddress(node)
	return !bytes.Equal(oldMacAddress, macAddress)
}

func nodeSubnetChanged(oldNode, node *kapi.Node) bool {
	oldSubnets, _ := util.ParseNodeHostSubnetAnnotation(oldNode)
	newSubnets, _ := util.ParseNodeHostSubnetAnnotation(node)
	return !reflect.DeepEqual(oldSubnets, newSubnets)
}

func nodeChassisChanged(oldNode, node *kapi.Node) bool {
	oldChassis, _ := util.ParseNodeChassisIDAnnotation(oldNode)
	newChassis, _ := util.ParseNodeChassisIDAnnotation(node)
	return oldChassis != newChassis
}

// noHostSubnet() compares the no-hostsubenet-nodes flag with node labels to see if the node is manageing its
// own network.
func noHostSubnet(node *kapi.Node) bool {
	if config.Kubernetes.NoHostSubnetNodes == nil {
		return false
	}

	nodeSelector, _ := metav1.LabelSelectorAsSelector(config.Kubernetes.NoHostSubnetNodes)
	return nodeSelector.Matches(labels.Set(node.Labels))
}

// shouldUpdate() determines if the ovn-kubernetes plugin should update the state of the node.
// ovn-kube should not perform an update if it does not assign a hostsubnet, or if you want to change
// whether or not ovn-kubernetes assigns a hostsubnet
func shouldUpdate(node, oldNode *kapi.Node) (bool, error) {
	newNoHostSubnet := noHostSubnet(node)
	oldNoHostSubnet := noHostSubnet(oldNode)

	if oldNoHostSubnet && newNoHostSubnet {
		return false, nil
	} else if oldNoHostSubnet && !newNoHostSubnet {
		return false, fmt.Errorf("error updating node %s, cannot remove assigned hostsubnet, please delete node and recreate.", node.Name)
	} else if !oldNoHostSubnet && newNoHostSubnet {
		return false, fmt.Errorf("error updating node %s, cannot assign a hostsubnet to already created node, please delete node and recreate.", node.Name)
	}

	return true, nil
}

func newServiceController(client clientset.Interface, nbClient libovsdbclient.Client, recorder record.EventRecorder) (*svccontroller.Controller, informers.SharedInformerFactory) {
	// Create our own informers to start compartmentalizing the code
	// filter server side the things we don't care about
	noProxyName, err := labels.NewRequirement("service.kubernetes.io/service-proxy-name", selection.DoesNotExist, nil)
	if err != nil {
		panic(err)
	}

	noHeadlessEndpoints, err := labels.NewRequirement(kapi.IsHeadlessService, selection.DoesNotExist, nil)
	if err != nil {
		panic(err)
	}

	labelSelector := labels.NewSelector()
	labelSelector = labelSelector.Add(*noProxyName, *noHeadlessEndpoints)

	svcFactory := informers.NewSharedInformerFactoryWithOptions(client, 0,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector.String()
		}))

	controller := svccontroller.NewController(
		client,
		nbClient,
		svcFactory.Core().V1().Services(),
		svcFactory.Discovery().V1().EndpointSlices(),
		svcFactory.Core().V1().Nodes(),
		recorder,
	)

	return controller, svcFactory
}

func (oc *Controller) StartServiceController(wg *sync.WaitGroup, runRepair bool) error {
	klog.Infof("Starting OVN Service Controller: Using Endpoint Slices")
	wg.Add(1)
	go func() {
		defer wg.Done()
		useLBGroups := oc.loadBalancerGroupUUID != ""
		// use 5 workers like most of the kubernetes controllers in the
		// kubernetes controller-manager
		err := oc.svcController.Run(5, oc.stopChan, runRepair, useLBGroups)
		if err != nil {
			klog.Errorf("Error running OVN Kubernetes Services controller: %v", err)
		}
	}()
	return nil
}
