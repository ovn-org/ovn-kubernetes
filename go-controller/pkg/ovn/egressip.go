// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	networkmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	egresssvc "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/egressservice"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	ovnretry "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

type egressIPAddrSetName string
type egressIPFamilyValue string
type egressIPNoReroutePolicyName string
type egressIPQoSRuleName string

const (
	NodeIPAddrSetName             egressIPAddrSetName = addresssetmanager.ClusterNodeIPsAddrSetName
	EgressIPServedPodsAddrSetName egressIPAddrSetName = "egressip-served-pods"
	// the possible values for LRP DB objects for EIPs
	IPFamilyValueV4         egressIPFamilyValue         = "ip4"
	IPFamilyValueV6         egressIPFamilyValue         = "ip6"
	IPFamilyValue           egressIPFamilyValue         = "ip" // use it when its dualstack
	ReplyTrafficNoReroute   egressIPNoReroutePolicyName = "EIP-No-Reroute-reply-traffic"
	NoReRoutePodToPod       egressIPNoReroutePolicyName = "EIP-No-Reroute-Pod-To-Pod"
	NoReRoutePodToJoin      egressIPNoReroutePolicyName = "EIP-No-Reroute-Pod-To-Join"
	NoReRoutePodToNode      egressIPNoReroutePolicyName = "EIP-No-Reroute-Pod-To-Node"
	NoReRouteUDNPodToCDNSvc egressIPNoReroutePolicyName = "EIP-No-Reroute-Pod-To-CDN-Svc"
	ReplyTrafficMark        egressIPQoSRuleName         = "EgressIP-Mark-Reply-Traffic"
	dbIDEIPNamePodDivider                               = "_"
)

func getEgressIPAddrSetDbIDs(name egressIPAddrSetName, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		// egress ip creates cluster-wide address sets with egressIpAddrSetName
		libovsdbops.ObjectNameKey: string(name),
		libovsdbops.NetworkKey:    network,
	})
}

func getEgressIPLRPReRouteDbIDs(egressIPName, podNamespace, podName string, ipFamily egressIPFamilyValue, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: fmt.Sprintf("%s%s%s/%s", egressIPName, dbIDEIPNamePodDivider, podNamespace, podName),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", types.EgressIPReroutePriority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
		libovsdbops.NetworkKey:    network,
	})
}

func getEIPLRPObjK8MetaData(externalIDs map[string]string) (string, string) {
	objMetaDataRaw := externalIDs[libovsdbops.ObjectNameKey.String()]
	if objMetaDataRaw == "" || !strings.Contains(objMetaDataRaw, "_") || !strings.Contains(objMetaDataRaw, "/") {
		return "", ""
	}
	objMetaDataSplit := strings.Split(objMetaDataRaw, "_")
	if len(objMetaDataSplit) != 2 {
		return "", ""
	}
	return objMetaDataSplit[0], objMetaDataSplit[1] // EgressIP name and "podNamespace/podName"
}

func getEgressIPLRPNoReRoutePodToJoinDbIDs(ipFamily egressIPFamilyValue, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: string(NoReRoutePodToJoin),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", types.DefaultNoRereoutePriority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
		libovsdbops.NetworkKey:    network,
	})
}

func getEgressIPLRPNoReRoutePodToPodDbIDs(ipFamily egressIPFamilyValue, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: string(NoReRoutePodToPod),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", types.DefaultNoRereoutePriority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
		libovsdbops.NetworkKey:    network,
	})
}

func getEgressIPLRPNoReRoutePodToNodeDbIDs(ipFamily egressIPFamilyValue, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: string(NoReRoutePodToNode),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", types.DefaultNoRereoutePriority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
		libovsdbops.NetworkKey:    network,
	})
}

func getEgressIPLRPNoReRouteDbIDs(priority int, uniqueName egressIPNoReroutePolicyName, ipFamily egressIPFamilyValue, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		// egress ip creates global no-reroute policies at 102 priority
		libovsdbops.ObjectNameKey: string(uniqueName),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", priority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
		libovsdbops.NetworkKey:    network,
	})
}

func getEgressIPQoSRuleDbIDs(ipFamily egressIPFamilyValue, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		// egress ip creates reply traffic marker rule at 103 priority
		libovsdbops.ObjectNameKey: string(ReplyTrafficMark),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", types.EgressIPRerouteQoSRulePriority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
		libovsdbops.NetworkKey:    network,
	})
}

func getEgressIPLRPSNATMarkDbIDs(eIPName, podNamespace, podName string, ipFamily egressIPFamilyValue, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: fmt.Sprintf("%s_%s/%s", eIPName, podNamespace, podName),
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", types.EgressIPSNATMarkPriority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
		libovsdbops.NetworkKey:    network,
	})
}

func getEgressIPNATDbIDs(eIPName, podNamespace, podName string, ipFamily egressIPFamilyValue, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.NATEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: fmt.Sprintf("%s_%s/%s", eIPName, podNamespace, podName),
		libovsdbops.IPFamilyKey:   string(ipFamily),
	})
}

func getDeprecatedEgressIPClusterNodeIPsAddrSetDbIDs() *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressIP, types.DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: addresssetmanager.ClusterNodeIPsAddrSetName,
		})
}

func cleanupDeprecatedClusterNodeIPsAddressSet(nbClient libovsdbclient.Client) error {
	return libovsdbutil.DeleteAddrSetsWithoutMatchRef(getDeprecatedEgressIPClusterNodeIPsAddrSetDbIDs(), nbClient)
}

// EgressIPController configures OVN to support EgressIP
type EgressIPController struct {
	// libovsdb northbound client interface
	nbClient     libovsdbclient.Client
	kube         *kube.KubeOVN
	watchFactory *factory.WatchFactory
	// event recorder used to post events to k8s
	recorder record.EventRecorder
	// lock serializes per-network EgressIP bring-up and teardown against live
	// EgressIP/pod/namespace handlers (handlers take RLock; bring-up/teardown take Lock).
	lock sync.RWMutex
	// used as a locking mechanism to serialize egress IP processing on a per egress IP basis
	// the order of locking should always be lock (RLock), then egressIPCache, then
	// podAssignment, then nodeState
	egressIPCache *syncmap.SyncMap[bool]
	// networkMutexes is used for three reasons:
	// (1) to ensure safe handling of node ip address updates. VIP addresses are
	// dynamic and might move across nodes.
	// (2) used in ensureDefaultNoRerouteQoSRules function to ensure
	// creating QoS rules is thread safe since otherwise when two nodes are added
	// at the same time by two different threads we end up creating duplicate
	// QoS rules in database due to libovsdb cache race
	// (3) to update nextHop during layer2 topology upgrade
	networkMutexes sync.Map
	// podAssignment is a cache used for keeping track of which egressIP status
	// has been set up for each pod. The key is defined by getPodKey
	podAssignment *syncmap.SyncMap[*podAssignmentState]
	// logicalPortCache allows access to pod IPs for all networks
	logicalPortCache *PortCache
	// nodeState tracks every observed node. Locality is derived from nodeName;
	// the keyed locks serialize node events with EgressIP programming.
	nodeState *syncmap.SyncMap[struct{}]
	// networkManager used for getting network information for UDNs
	networkManager          networkmanager.Interface
	nadReconciler           networkmanager.NADReconciler
	nadReconcilerID         uint64
	nadReconcilerRegistered bool
	// networkReconciler handles EnqueueNetwork → SyncNetworkReady (stale sync + register)
	networkReconciler controller.Reconciler
	// retryEgressIPPods allows requeuing egressIP pod processing on NAD changes
	retryEgressIPPods *ovnretry.RetryFramework
	// An address set factory that creates address sets
	addressSetFactory addressset.AddressSetFactory
	// Shared address set manager that owns cluster-wide node IP address sets.
	addressSetManager *addresssetmanager.AddressSetManager
	// nodeName is the node managed by this controller.
	nodeName string
	v4       bool
	v6       bool
	// controllerName is the name of the controller. For backward compatibility reasons, this is the default network controller name.
	controllerName string
	// registeredNetworks tracks per-network EgressIP bring-up state (pending or synced).
	// Live handlers may program a network only when it is synced. Missing means the
	// network was never enqueued or was unregistered (e.g. UDN deleted).
	registeredNetworks sync.Map
}

// networkSyncState is the EgressIP bring-up state for a network.
type networkSyncState string

const (
	// networkPending: enqueued for bring-up; stale sync not finished yet.
	networkPending networkSyncState = "pending"
	// networkSynced: stale sync finished; live handlers may program the network.
	networkSynced networkSyncState = "synced"
)

func NewEIPController(
	nbClient libovsdbclient.Client,
	kube *kube.KubeOVN,
	watchFactory *factory.WatchFactory,
	recorder record.EventRecorder,
	portCache *PortCache,
	networkmanager networkmanager.Interface,
	addressSetFactor addressset.AddressSetFactory,
	addressSetManager *addresssetmanager.AddressSetManager,
	v4 bool,
	v6 bool,
	nodeName string,
	controllerName string,
) *EgressIPController {
	e := &EgressIPController{
		nbClient:          nbClient,
		kube:              kube,
		watchFactory:      watchFactory,
		recorder:          recorder,
		egressIPCache:     syncmap.NewSyncMap[bool](),
		podAssignment:     syncmap.NewSyncMap[*podAssignmentState](),
		logicalPortCache:  portCache,
		nodeState:         syncmap.NewSyncMap[struct{}](),
		controllerName:    controllerName,
		networkManager:    networkmanager,
		addressSetFactory: addressSetFactor,
		addressSetManager: addressSetManager,
		nodeName:          nodeName,
		v4:                v4,
		v6:                v6,
	}
	nadReconcilerConfig := &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   e.syncNAD,
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	}
	e.nadReconciler = controller.NewReconciler(
		controllerName+"-egressip-nad",
		nadReconcilerConfig,
	)
	networkReconcilerConfig := &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   e.SyncNetworkReady,
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	}
	e.networkReconciler = controller.NewReconciler(
		controllerName+"-egressip-network",
		networkReconcilerConfig,
	)
	return e
}

// getNetworkMutex returns the mutex for a specific network, creating it if necessary.
// This enables per-network locking for concurrent reconciliation of different networks.
func (e *EgressIPController) getNetworkMutex(networkName string) *sync.Mutex {
	// Try to load existing mutex
	if mu, ok := e.networkMutexes.Load(networkName); ok {
		return mu.(*sync.Mutex)
	}

	// Create new mutex
	newMu := &sync.Mutex{}
	actual, _ := e.networkMutexes.LoadOrStore(networkName, newMu)
	return actual.(*sync.Mutex)
}

// lockNetwork acquires the mutex for a specific network.
func (e *EgressIPController) lockNetwork(networkName string) {
	mu := e.getNetworkMutex(networkName)
	mu.Lock()
}

// unlockNetwork releases the mutex for a specific network.
func (e *EgressIPController) unlockNetwork(networkName string) {
	mu := e.getNetworkMutex(networkName)
	mu.Unlock()
}

// main reconcile functions begin here

// reconcileEgressIP reconciles the database configuration
// setup in nbdb based on the received egressIP objects
// CASE 1: if old == nil && new != nil {add event, we do a full setup for all statuses}
// CASE 2: if old != nil && new == nil {delete event, we do a full teardown for all statuses}
// CASE 3: if old != nil && new != nil {update event,
//
//	  CASE 3.1: we calculate based on difference between old and new statuses
//	            which ones need teardown and which ones need setup
//	            this ensures there is no disruption for things that did not change
//	  CASE 3.2: Only Namespace selectors on Spec changed
//	  CASE 3.3: Only Pod Selectors on Spec changed
//	  CASE 3.4: Both Namespace && Pod Selectors on Spec changed
//	}
//
// NOTE: `Spec.EgressIPs" updates for EIP object are not processed here, that is the job of cluster manager
//
//	We only care about `Spec.NamespaceSelector`, `Spec.PodSelector` and `Status` field
func (e *EgressIPController) reconcileEgressIP(old, new *egressipv1.EgressIP) (err error) {
	e.lock.RLock()
	defer e.lock.RUnlock()

	var egressIPName string
	if old != nil {
		egressIPName = old.Name
	} else {
		egressIPName = new.Name
	}
	e.egressIPCache.LockKey(egressIPName)
	defer e.egressIPCache.UnlockKey(egressIPName)

	// CASE 1: EIP object deletion, we need to teardown database configuration for all the statuses
	if old != nil && new == nil {
		removeStatus := old.Status.Items
		if len(removeStatus) > 0 {
			if err := e.deleteEgressIPAssignments(old.Name, removeStatus); err != nil {
				return err
			}
		}
	}
	var mark util.EgressIPMark
	if new != nil {
		mark = getEgressIPPktMark(new.Name, new.Annotations)
	}

	// CASE 2: EIP object addition, we need to setup database configuration for all the statuses
	if old == nil && new != nil {
		addStatus := new.Status.Items
		if len(addStatus) > 0 {
			if err := e.addEgressIPAssignments(new.Name, addStatus, mark, new.Spec.NamespaceSelector, new.Spec.PodSelector); err != nil {
				return err
			}
		}
	}
	// CASE 3: EIP object update
	var errs []error
	if old != nil && new != nil {
		oldEIP := old
		newEIP := new
		// CASE 3.1: we need to see which statuses
		//        1) need teardown
		//        2) need setup
		//        3) need no-op
		if !reflect.DeepEqual(oldEIP.Status.Items, newEIP.Status.Items) {
			statusToRemove := make(map[string]egressipv1.EgressIPStatusItem, 0)
			statusToKeep := make(map[string]egressipv1.EgressIPStatusItem, 0)
			for _, status := range oldEIP.Status.Items {
				statusToRemove[status.EgressIP] = status
			}
			for _, status := range newEIP.Status.Items {
				statusToKeep[status.EgressIP] = status
			}
			// only delete items that were in the oldSpec but cannot be found in the newSpec
			statusToDelete := make([]egressipv1.EgressIPStatusItem, 0)
			for eIP, oldStatus := range statusToRemove {
				if newStatus, ok := statusToKeep[eIP]; ok && newStatus.Node == oldStatus.Node {
					continue
				}
				statusToDelete = append(statusToDelete, oldStatus)
			}
			if len(statusToDelete) > 0 {
				if err := e.deleteEgressIPAssignments(old.Name, statusToDelete); err != nil {
					return err
				}
			}
			// only add items that were NOT in the oldSpec but can be found in the newSpec
			statusToAdd := make([]egressipv1.EgressIPStatusItem, 0)
			for eIP, newStatus := range statusToKeep {
				if oldStatus, ok := statusToRemove[eIP]; ok && oldStatus.Node == newStatus.Node {
					continue
				}
				statusToAdd = append(statusToAdd, newStatus)
			}
			if len(statusToAdd) > 0 {
				if err := e.addEgressIPAssignments(new.Name, statusToAdd, mark, new.Spec.NamespaceSelector, new.Spec.PodSelector); err != nil {
					return err
				}
			}
		}

		oldNamespaceSelector, err := metav1.LabelSelectorAsSelector(&oldEIP.Spec.NamespaceSelector)
		if err != nil {
			return fmt.Errorf("invalid old namespaceSelector, err: %v", err)
		}
		newNamespaceSelector, err := metav1.LabelSelectorAsSelector(&newEIP.Spec.NamespaceSelector)
		if err != nil {
			return fmt.Errorf("invalid new namespaceSelector, err: %v", err)
		}
		oldPodSelector, err := metav1.LabelSelectorAsSelector(&oldEIP.Spec.PodSelector)
		if err != nil {
			return fmt.Errorf("invalid old podSelector, err: %v", err)
		}
		newPodSelector, err := metav1.LabelSelectorAsSelector(&newEIP.Spec.PodSelector)
		if err != nil {
			return fmt.Errorf("invalid new podSelector, err: %v", err)
		}
		// CASE 3.2: Only Namespace selectors on Spec changed
		// Only the namespace selector changed: remove the setup for all pods
		// matching the old and not matching the new, and add setup for the pod
		// matching the new and which didn't match the old.
		if !reflect.DeepEqual(newNamespaceSelector, oldNamespaceSelector) && reflect.DeepEqual(newPodSelector, oldPodSelector) {
			namespaces, err := e.watchFactory.GetNamespaces()
			if err != nil {
				return err
			}
			for _, namespace := range namespaces {
				namespaceLabels := labels.Set(namespace.Labels)
				if !newNamespaceSelector.Matches(namespaceLabels) && oldNamespaceSelector.Matches(namespaceLabels) {
					if cleanupErr := e.deleteNamespaceEgressIPAssignment(oldEIP.Name, namespace, oldEIP.Spec.PodSelector, true); cleanupErr != nil {
						errs = append(errs, fmt.Errorf("failed to delete namespace %s egress IP config: %w", namespace.Name, cleanupErr))
					}
				}
				if newNamespaceSelector.Matches(namespaceLabels) && !oldNamespaceSelector.Matches(namespaceLabels) {
					ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
					if err != nil {
						if util.IsInvalidPrimaryNetworkError(err) || apierrors.IsNotFound(err) {
							// InvalidPrimaryNetworkError: NAD reconciler will replay.
							// NotFound: listed namespace concurrently removed, namespace/pod handlers will catch up.
							continue
						}
						return fmt.Errorf("failed to get active network for namespace %s: %w", namespace.Name, err)
					}
					if ni == nil {
						// our node does not have this network
						continue
					}
					if err := e.addNamespaceEgressIPAssignments(ni, newEIP.Name, newEIP.Status.Items, mark, namespace, newEIP.Spec.PodSelector); err != nil {
						errs = append(errs, fmt.Errorf("network %s: failed to add namespace %s egress IP config: %v", ni.GetNetworkName(), namespace.Name, err))
					}
				}
			}
			// CASE 3.3: Only Pod Selectors on Spec changed
			// Only the pod selector changed: remove the setup for all pods
			// matching the old and not matching the new, and add setup for the pod
			// matching the new and which didn't match the old.
		} else if reflect.DeepEqual(newNamespaceSelector, oldNamespaceSelector) && !reflect.DeepEqual(newPodSelector, oldPodSelector) {
			namespaces, err := e.watchFactory.GetNamespacesBySelector(newEIP.Spec.NamespaceSelector)
			if err != nil {
				return err
			}
			for _, namespace := range namespaces {
				pods, err := e.watchFactory.GetPods(namespace.Name)
				if err != nil {
					return err
				}
				for _, pod := range pods {
					podLabels := labels.Set(pod.Labels)
					if !newPodSelector.Matches(podLabels) && oldPodSelector.Matches(podLabels) {
						if cleanupErr := e.deletePodEgressIPAssignmentsUsingCache(oldEIP.Name, pod, true); cleanupErr != nil {
							errs = append(errs, fmt.Errorf("failed to delete pod %s/%s egress IP config: %w", pod.Namespace, pod.Name, cleanupErr))
						}
					}
					if newPodSelector.Matches(podLabels) && !oldPodSelector.Matches(podLabels) {
						ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
						if err != nil {
							if util.IsInvalidPrimaryNetworkError(err) || apierrors.IsNotFound(err) {
								// InvalidPrimaryNetworkError: NAD reconciler will replay.
								// NotFound: listed namespace concurrently removed, namespace/pod handlers will catch up.
								continue
							}
							return fmt.Errorf("failed to get active network for namespace %s: %w", namespace.Name, err)
						}
						if ni == nil {
							// our node does not have this network
							continue
						}
						if err := e.addPodEgressIPAssignmentsWithLock(ni, newEIP.Name, newEIP.Status.Items, mark, pod); err != nil {
							errs = append(errs, fmt.Errorf("network %s: failed to add pod %s/%s egress IP config: %v", ni.GetNetworkName(), pod.Namespace, pod.Name, err))
						}
					}
				}
			}
			// CASE 3.4: Both Namespace && Pod Selectors on Spec changed
			// Both selectors changed: remove the setup for pods matching the
			// old ones and not matching the new ones, and add setup for all
			// matching the new ones but which didn't match the old ones.
		} else if !reflect.DeepEqual(newNamespaceSelector, oldNamespaceSelector) && !reflect.DeepEqual(newPodSelector, oldPodSelector) {
			namespaces, err := e.watchFactory.GetNamespaces()
			if err != nil {
				return err
			}
			for _, namespace := range namespaces {
				namespaceLabels := labels.Set(namespace.Labels)
				// If the namespace does not match anymore then there's no
				// reason to look at the pod selector.
				if !newNamespaceSelector.Matches(namespaceLabels) && oldNamespaceSelector.Matches(namespaceLabels) {
					if cleanupErr := e.deleteNamespaceEgressIPAssignment(oldEIP.Name, namespace, oldEIP.Spec.PodSelector, true); cleanupErr != nil {
						errs = append(errs, fmt.Errorf("failed to delete namespace %s egress IP config: %w", namespace.Name, cleanupErr))
					}
					continue
				}

				podsToAdd := []*corev1.Pod{}
				// If the namespace starts matching, look at the pods selector
				// and pods in that namespace and perform the setup for the pods
				// which match the new pod selector or if the podSelector is empty
				// then just perform the setup.
				if newNamespaceSelector.Matches(namespaceLabels) && !oldNamespaceSelector.Matches(namespaceLabels) {
					pods, err := e.watchFactory.GetPods(namespace.Name)
					if err != nil {
						return err
					}
					for _, pod := range pods {
						podLabels := labels.Set(pod.Labels)
						if newPodSelector.Matches(podLabels) {
							podsToAdd = append(podsToAdd, pod)
						}
					}
				}
				// If the namespace continues to match, look at the pods
				// selector and pods in that namespace.
				if newNamespaceSelector.Matches(namespaceLabels) && oldNamespaceSelector.Matches(namespaceLabels) {
					pods, err := e.watchFactory.GetPods(namespace.Name)
					if err != nil {
						return err
					}
					for _, pod := range pods {
						podLabels := labels.Set(pod.Labels)
						if !newPodSelector.Matches(podLabels) && oldPodSelector.Matches(podLabels) {
							if cleanupErr := e.deletePodEgressIPAssignmentsUsingCache(oldEIP.Name, pod, true); cleanupErr != nil {
								errs = append(errs, fmt.Errorf("failed to delete pod %s/%s egress IP config: %w", pod.Namespace, pod.Name, cleanupErr))
							}
						}
						if newPodSelector.Matches(podLabels) && !oldPodSelector.Matches(podLabels) {
							podsToAdd = append(podsToAdd, pod)
						}
					}
				}
				if len(podsToAdd) == 0 {
					continue
				}
				ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
				if err != nil {
					if util.IsInvalidPrimaryNetworkError(err) || apierrors.IsNotFound(err) {
						// InvalidPrimaryNetworkError: NAD reconciler will replay.
						// NotFound: listed namespace concurrently removed, namespace/pod handlers will catch up.
						continue
					}
					return fmt.Errorf("failed to get active network for namespace %s: %w", namespace.Name, err)
				}
				if ni == nil {
					// our node does not have this network
					continue
				}
				for _, pod := range podsToAdd {
					if err := e.addPodEgressIPAssignmentsWithLock(ni, newEIP.Name, newEIP.Status.Items, mark, pod); err != nil {
						errs = append(errs, fmt.Errorf("network %s: failed to add pod %s/%s egress IP config: %v", ni.GetNetworkName(), pod.Namespace, pod.Name, err))
					}
				}
			}
		}
	}
	return utilerrors.Join(errs...)
}

// reconcileEgressIPNamespace reconciles the database configuration setup in nbdb
// based on received namespace objects.
// NOTE: we only care about namespace label updates
func (e *EgressIPController) reconcileEgressIPNamespace(old, new *corev1.Namespace) error {
	e.lock.RLock()
	defer e.lock.RUnlock()

	// Same as for reconcileEgressIP: labels play nicely with empty object, not
	// nil ones.
	var namespaceName string
	oldNamespace, newNamespace := &corev1.Namespace{}, &corev1.Namespace{}
	if old != nil {
		oldNamespace = old
		namespaceName = old.Name
	}
	if new != nil {
		newNamespace = new
		namespaceName = new.Name
	}

	// If the labels have not changed, then there's no change that we care
	// about: return.
	oldLabels := labels.Set(oldNamespace.Labels)
	newLabels := labels.Set(newNamespace.Labels)
	if reflect.DeepEqual(newLabels.AsSelector(), oldLabels.AsSelector()) {
		return nil
	}

	// Iterate all EgressIPs and check if this namespace start/stops matching
	// any and add/remove the setup accordingly. Namespaces can match multiple
	// EgressIP objects (ex: users can chose to have one EgressIP object match
	// all "blue" pods in namespace A, and a second EgressIP object match all
	// "red" pods in namespace A), so continue iterating all EgressIP objects
	// before finishing.
	egressIPs, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	var errs []error
	for _, egressIP := range egressIPs {
		if err := e.egressIPCache.DoWithLock(egressIP.Name, func(key string) error {
			// get latest egressIP object after we get cache lock to serialize egress ip operations
			// between egressIP handler and EgressIPPod handler
			eIP, err := e.watchFactory.GetEgressIP(key)
			if err != nil {
				if apierrors.IsNotFound(err) {
					// we assume egress IP handler will remove egress IP from affected namespace
					return nil
				}
				return err
			}
			namespaceSelector, err := metav1.LabelSelectorAsSelector(&eIP.Spec.NamespaceSelector)
			if err != nil {
				return err
			}
			if namespaceSelector.Matches(oldLabels) && !namespaceSelector.Matches(newLabels) {
				if cleanupErr := e.deleteNamespaceEgressIPAssignment(eIP.Name, oldNamespace, eIP.Spec.PodSelector, new != nil); cleanupErr != nil {
					return fmt.Errorf("failed to delete namespace %q for egress IP %q: %w",
						namespaceName, eIP.Name, cleanupErr)
				}
			}
			if !namespaceSelector.Matches(oldLabels) && namespaceSelector.Matches(newLabels) {
				mark := getEgressIPPktMark(eIP.Name, eIP.Annotations)
				ni, err := e.networkManager.GetActiveNetworkForNamespace(namespaceName)
				if err != nil {
					if util.IsInvalidPrimaryNetworkError(err) || apierrors.IsNotFound(err) {
						// InvalidPrimaryNetworkError: NAD reconciler will replay.
						// NotFound: listed namespace concurrently removed, namespace/pod handlers will catch up.
						return nil
					}
					return fmt.Errorf("failed to get active network for namespace %s: %w", namespaceName, err)
				}
				if ni == nil {
					// our node does not have this network
					return nil
				}
				if err := e.addNamespaceEgressIPAssignments(ni, eIP.Name, eIP.Status.Items, mark, newNamespace, eIP.Spec.PodSelector); err != nil {
					return fmt.Errorf("network %s: failed to add namespace %q for egress IP %q: %w",
						ni.GetNetworkName(), namespaceName, eIP.Name, err)
				}
			}

			return nil

		}); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

// reconcileEgressIPPod reconciles the database configuration setup in nbdb
// based on received pod objects.
// NOTE: we only care about pod label updates
func (e *EgressIPController) reconcileEgressIPPod(old, new *corev1.Pod) (err error) {
	e.lock.RLock()
	defer e.lock.RUnlock()

	oldPod, newPod := &corev1.Pod{}, &corev1.Pod{}
	if old != nil {
		oldPod = old
	}
	if new != nil {
		newPod = new
	}

	newPodLabels := labels.Set(newPod.Labels)
	oldPodLabels := labels.Set(oldPod.Labels)

	// Iterate all EgressIPs and check if this pod start/stops matching any and
	// add/remove the setup accordingly. Pods should not match multiple EgressIP
	// objects: that is considered a user error and is undefined. However, in
	// such events iterate all EgressIPs and clean up as much as possible. By
	// iterating all EgressIPs we also cover the case where a pod has its labels
	// changed from matching one EgressIP to another, ex: EgressIP1 matching
	// "blue pods" and EgressIP2 matching "red pods". If a pod with a red label
	// gets changed to a blue label: we need add and remove the set up for both
	// EgressIP obejcts - since we can't be sure of which EgressIP object we
	// process first, always iterate all.
	egressIPs, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	var errs []error
	for _, egressIP := range egressIPs {
		if err := e.egressIPCache.DoWithLock(egressIP.Name, func(key string) error {
			// get latest egressIP object after we get cache lock to serialize egress ip operations
			// between egressIP handler and EgressIPNamespace handler
			eIP, err := e.watchFactory.GetEgressIP(key)
			if err != nil {
				if apierrors.IsNotFound(err) {
					// we assume egress IP handler will remove egress IP from affected pod
					return nil
				}
				return err
			}
			if new == nil {
				if cleanupErr := e.deletePodEgressIPAssignmentsUsingCache(eIP.Name, oldPod, false); cleanupErr != nil {
					return fmt.Errorf("failed to delete pod %s/%s for egress IP %q using cached assignment: %w",
						oldPod.Namespace, oldPod.Name, eIP.Name, cleanupErr)
				}
				return nil
			}

			namespace := &corev1.Namespace{}
			if old != nil {
				namespace, err = e.watchFactory.GetNamespace(oldPod.Namespace)
				if err != nil {
					return err
				}
			}

			if new != nil {
				namespace, err = e.watchFactory.GetNamespace(newPod.Namespace)
				if err != nil {
					return err
				}
			}

			// If the namespace the pod belongs to does not have any labels, just return
			// it can't match any EgressIP object
			namespaceLabels := labels.Set(namespace.Labels)
			if namespaceLabels.AsSelector().Empty() {
				return nil
			}

			namespaceSelector, err := metav1.LabelSelectorAsSelector(&eIP.Spec.NamespaceSelector)
			if err != nil {
				return err
			}
			if namespaceSelector.Matches(namespaceLabels) {
				// If the namespace the pod belongs to matches this object then
				// check if there's a podSelector defined on the EgressIP
				// object. If there is one: the user intends the EgressIP object to
				// match only a subset of pods in the namespace, and we'll have to
				// check that. If there is no podSelector: the user intends it to
				// match all pods in the namespace.
				podSelector, err := metav1.LabelSelectorAsSelector(&eIP.Spec.PodSelector)
				if err != nil {
					return err
				}
				oldMatches, newMatches := false, false
				deletePath := false
				if !podSelector.Empty() {
					// Use "new" and "old" instead of "newPod" and "oldPod" to determine whether
					// pods was created or is being deleted.
					newMatches = new != nil && podSelector.Matches(newPodLabels)
					oldMatches = old != nil && podSelector.Matches(oldPodLabels)
					// If the podSelector doesn't match the pod, then continue
					// because this EgressIP intends to match other pods in that
					// namespace and not this one. Other EgressIP objects might
					// match the pod though so we need to check that.
					if !newMatches && !oldMatches {
						return nil
					}
					// Check if the pod stopped matching.
					deletePath = !newMatches && oldMatches
				}
				if deletePath {
					if cleanupErr := e.deletePodEgressIPAssignmentsUsingCache(eIP.Name, oldPod, true); cleanupErr != nil {
						return fmt.Errorf("failed to delete pod %s/%s for egress IP %q using cached assignment: %w",
							oldPod.Namespace, oldPod.Name, eIP.Name, cleanupErr)
					}
					return nil
				}

				ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
				if err != nil && !util.IsInvalidPrimaryNetworkError(err) {
					return fmt.Errorf("failed to get active network for namespace %s: %w", namespace.Name, err)
				}
				if ni == nil {
					return nil
				}
				mark := getEgressIPPktMark(eIP.Name, eIP.Annotations)
				if !podSelector.Empty() {
					// If the pod starts matching the podSelector or continues to
					// match: add the pod. The reason as to why we need to continue
					// adding it if it continues to match, as opposed to once when
					// it started matching, is because the pod might not have pod
					// IPs assigned at that point and we need to continue trying the
					// pod setup for every pod update as to make sure we process the
					// pod IP assignment.
					if err := e.addPodEgressIPAssignmentsWithLock(ni, eIP.Name, eIP.Status.Items, mark, newPod); err != nil {
						return fmt.Errorf("network %s: failed to add pod %s/%s for egress IP %q: %w",
							ni.GetNetworkName(), newPod.Namespace, newPod.Name, eIP.Name, err)
					}
					return nil
				}
				// For all else, perform a setup for the pod
				if err := e.addPodEgressIPAssignmentsWithLock(ni, eIP.Name, eIP.Status.Items, mark, newPod); err != nil {
					return fmt.Errorf("network %s: failed to add pod %s/%s for egress IP %q: %w",
						ni.GetNetworkName(), newPod.Namespace, newPod.Name, eIP.Name, err)
				}
			}

			return nil
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

// main reconcile functions end here and local node controller functions begin

func (e *EgressIPController) addEgressIPAssignments(name string, statusAssignments []egressipv1.EgressIPStatusItem, mark util.EgressIPMark, namespaceSelector, podSelector metav1.LabelSelector) error {
	namespaces, err := e.watchFactory.GetNamespacesBySelector(namespaceSelector)
	if err != nil {
		return err
	}
	var errs []error
	for _, namespace := range namespaces {
		ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
		if err != nil {
			if util.IsInvalidPrimaryNetworkError(err) || apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("failed to get active network for namespace %s: %v", namespace.Name, err)
		}
		if ni == nil {
			continue
		}
		if err := e.addNamespaceEgressIPAssignments(ni, name, statusAssignments, mark, namespace, podSelector); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

func (e *EgressIPController) addNamespaceEgressIPAssignments(ni util.NetInfo, name string, statusAssignments []egressipv1.EgressIPStatusItem, mark util.EgressIPMark,
	namespace *corev1.Namespace, podSelector metav1.LabelSelector) error {
	var pods []*corev1.Pod
	var err error
	selector, err := metav1.LabelSelectorAsSelector(&podSelector)
	if err != nil {
		return err
	}
	if !selector.Empty() {
		pods, err = e.watchFactory.GetPodsBySelector(namespace.Name, podSelector)
		if err != nil {
			return err
		}
	} else {
		pods, err = e.watchFactory.GetPods(namespace.Name)
		if err != nil {
			return err
		}
	}
	var errs []error
	for _, pod := range pods {
		if err := e.addPodEgressIPAssignmentsWithLock(ni, name, statusAssignments, mark, pod); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

func (e *EgressIPController) addPodEgressIPAssignmentsWithLock(ni util.NetInfo, name string, statusAssignments []egressipv1.EgressIPStatusItem, mark util.EgressIPMark, pod *corev1.Pod) error {
	// Gate live programming until SyncNetworkReady has finished stale sync for
	// this network (same sequencing as the former bulk syncEgressIPs barrier).
	// Return an error so the retry framework requeues until the network is registered.
	if !e.IsNetworkRegistered(ni.GetNetworkName()) {
		return fmt.Errorf("network %s is not ready for EgressIP programming", ni.GetNetworkName())
	}

	e.podAssignment.LockKey(getPodKey(pod))
	defer e.podAssignment.UnlockKey(getPodKey(pod))
	e.deletePreviousNetworkPodEgressIPAssignments(ni, name, statusAssignments, pod, false)
	return e.addPodEgressIPAssignments(ni, name, statusAssignments, mark, pod)
}

// addPodEgressIPAssignments tracks the setup made for each egress IP matching
// pod w.r.t to each status. This is mainly done to avoid a lot of duplicated
// work on ovnkube-controller restarts when all egress IP handlers will most likely
// match and perform the setup for the same pod and status multiple times over.
// requires holding the podAssignmentMutex lock
func (e *EgressIPController) addPodEgressIPAssignments(ni util.NetInfo, name string, statusAssignments []egressipv1.EgressIPStatusItem, mark util.EgressIPMark, pod *corev1.Pod) error {
	podKey := getPodKey(pod)
	// Ignore completed pods, host networked pods, pods not scheduled
	if !util.PodNeedsSNAT(pod) {
		klog.Infof("Pod %s is not in desired state, skipping egress ip assignment", podKey)
		return nil
	}
	// If statusAssignments is empty just return, not doing this will delete the
	// external GW set up, even though there might be no egress IP set up to
	// perform.
	if len(statusAssignments) == 0 {
		return nil
	}
	var remainingAssignments, staleAssignments, reprogramAssignments []egressipv1.EgressIPStatusItem
	nadKey, err := e.getPodNADKeyForNetwork(ni, pod)
	if err != nil {
		return err
	}
	podIPNets, err := e.getPodIPs(ni, pod, nadKey)
	if err != nil {
		return fmt.Errorf("failed to get pod %s/%s IPs: %v", pod.Namespace, pod.Name, err)
	}
	if len(podIPNets) == 0 {
		return fmt.Errorf("failed to get pod ips for pod %s on network %s with NAD key %s", podKey, ni.GetNetworkName(), nadKey)
	}
	podIPs := make([]net.IP, 0, len(podIPNets))
	for _, ipNet := range podIPNets {
		podIPs = append(podIPs, ipNet.IP)
	}
	podState, exists := e.podAssignment.Load(podKey)
	if !exists {
		remainingAssignments = statusAssignments
		podState = &podAssignmentState{
			egressIPName:         name,
			egressStatuses:       egressStatuses{make(map[egressipv1.EgressIPStatusItem]string)},
			standbyEgressIPNames: sets.New[string](),
			podIPs:               podIPs,
			network:              ni,
		}
	} else if podState.egressIPName == name || podState.egressIPName == "" {
		podIPsChanged := !util.IsIPsEqual(podState.podIPs, podIPs)
		// We do the setup only if this egressIP object is the one serving this pod OR
		// podState.egressIPName can be empty if no re-routes were found in
		// syncPodAssignmentCache for the existing pod, we will treat this case as a new add
		for _, status := range statusAssignments {
			// Add the status if it's not already in the cache, or if it exists but is in pending state
			// (meaning it was populated during EIP sync and needs to be processed for the pod).
			if value, exists := podState.egressStatuses.statusMap[status]; !exists || value == egressStatusStatePending {
				remainingAssignments = append(remainingAssignments, status)
			} else if podIPsChanged {
				// A pod can be re-created with the same name but a different IP.
				// Force a delete+add for existing statuses so LRP match/NAT gets updated.
				reprogramAssignments = append(reprogramAssignments, status)
			}
			// Detect stale EIP status entries (same EgressIP reassigned to a different node)
			// and queue the outdated entry for cleanup.
			if staleStatus := podState.egressStatuses.hasStaleEIPStatus(status); staleStatus != nil {
				staleAssignments = append(staleAssignments, *staleStatus)
			}
		}
		podState.egressIPName = name
		podState.network = ni
		podState.standbyEgressIPNames.Delete(name)
	} else if podState.egressIPName != name {
		klog.Warningf("EgressIP object %s will not be configured for pod %s "+
			"since another egressIP object %s is serving it", name, podKey, podState.egressIPName)
		eIPRef := corev1.ObjectReference{
			Kind: "EgressIP",
			Name: name,
		}
		e.recorder.Eventf(
			&eIPRef,
			corev1.EventTypeWarning,
			"UndefinedRequest",
			"EgressIP object %s will not be configured for pod %s since another egressIP object %s is serving it, this is undefined", name, podKey, podState.egressIPName,
		)
		podState.standbyEgressIPNames.Insert(name)
		return nil
	}
	for _, staleStatus := range staleAssignments {
		klog.V(2).Infof("Deleting stale pod egress IP status: %v for EgressIP: %s and pod: %s/%s/%v", staleStatus, name, pod.Namespace, pod.Name, podIPNets)
		// When deleting stale status during assignment, assignStandby=true is passed, but standby promotion
		// will never occur because standbyEgressIPNames is empty (or doesn't contain this EgressIP) at this point:
		// - If podState is new, standbyEgressIPNames is initialized empty
		// - If this EIP is already active, it was removed from standbyEgressIPNames
		// - If another EIP is active, we return early and never reach this code
		// Additionally, even if statusMap becomes temporarily empty, new statuses are added immediately after.
		err = e.deletePodEgressIPAssignments(ni, name, []egressipv1.EgressIPStatusItem{staleStatus}, pod, true)
		if err != nil {
			klog.Warningf("Failed to delete stale EgressIP status %s/%v for pod %s: %v", name, staleStatus, podKey, err)
		}
		delete(podState.egressStatuses.statusMap, staleStatus)
	}
	if len(reprogramAssignments) > 0 {
		klog.V(2).Infof("Pod %s IPs changed, forcing egress IP status reprogram for statuses: %+v", podKey, reprogramAssignments)
		if err := e.deletePodEgressIPAssignments(ni, name, reprogramAssignments, pod, false); err != nil {
			return fmt.Errorf("failed to force reprogram of pod %s statuses %v for egress IP %s: %w",
				podKey, reprogramAssignments, name, err)
		}
		for _, status := range reprogramAssignments {
			delete(podState.egressStatuses.statusMap, status)
		}
		remainingAssignments = append(remainingAssignments, reprogramAssignments...)
	}
	podState.podIPs = podIPs
	// We store podState into podAssignment cache at this place for two reasons.
	// 1. When podAssignmentState is newly created.
	// 2. deletePodEgressIPAssignments might clean the podAssignment cache, make sure we add it back.
	e.podAssignment.Store(podKey, podState)
	// We need to proceed with add only under two conditions
	// 1) egressNode present in at least one status is managed by this controller
	// (NOTE: The relation between egressIPName and nodeName is 1:1 i.e in the same object the given node will be present only in one status)
	// 2) the pod being added is scheduled on this controller's node
	proceed := false
	for _, status := range statusAssignments {
		e.nodeState.LockKey(status.Node)
		isLocalEgressNode, loadedEgressNode := e.getNodeLocality(status.Node)
		if loadedEgressNode && isLocalEgressNode {
			proceed = true
			e.nodeState.UnlockKey(status.Node)
			break
		}
		e.nodeState.UnlockKey(status.Node)
	}
	if !proceed && !e.isPodScheduledOnLocalNode(pod) {
		return nil // nothing to do if none of the status nodes are local to this controller and the pod is also remote
	}
	for _, status := range remainingAssignments {
		klog.V(2).Infof("Adding pod egress IP status: %v for EgressIP: %s and pod: %s/%s/%v", status, name, pod.Namespace, pod.Name, podIPNets)
		nodesToLock := []string{status.Node, pod.Spec.NodeName}
		// Sort nodes lexicographically to ensure node locks are acquired in
		// same order, preventing deadlock situations.
		sort.Strings(nodesToLock)
		err = e.nodeState.DoWithLock(nodesToLock[0], func(_ string) error {
			if status.Node == pod.Spec.NodeName {
				// we are safe, no need to grab lock again
				if err := e.addPodEgressIPAssignment(ni, name, status, mark, pod, podIPNets); err != nil {
					return fmt.Errorf("unable to create egressip configuration for pod %s/%s/%v, err: %w", pod.Namespace, pod.Name, podIPNets, err)
				}
				podState.egressStatuses.statusMap[status] = ""
				return nil
			}
			return e.nodeState.DoWithLock(nodesToLock[1], func(_ string) error {
				// we need to grab lock again for pod's node
				if err := e.addPodEgressIPAssignment(ni, name, status, mark, pod, podIPNets); err != nil {
					return fmt.Errorf("unable to create egressip configuration for pod %s/%s/%v, err: %w", pod.Namespace, pod.Name, podIPNets, err)
				}
				podState.egressStatuses.statusMap[status] = ""
				return nil
			})
		})
		if err != nil {
			return err
		}
	}
	if e.isPodScheduledOnLocalNode(pod) {
		if err := e.addPodIPsToAddressSet(ni.GetNetworkName(), e.controllerName, podIPs...); err != nil {
			return fmt.Errorf("cannot add egressPodIPs for the pod %s/%s to the address set: err: %v", pod.Namespace, pod.Name, err)
		}
	}
	return nil
}

// deleteEgressIPAssignments performs a full egress IP setup deletion on a per
// (egress IP name - status) basis. The idea is thus to list the full content of
// the NB DB for that egress IP object and delete everything which match the
// status. We also need to update the podAssignment cache and finally re-add the
// external GW setup in case the pod still exists.
func (e *EgressIPController) deleteEgressIPAssignments(name string, statusesToRemove []egressipv1.EgressIPStatusItem) error {

	podAssignments := e.podAssignment.GetKeys()

	for _, statusToRemove := range statusesToRemove {
		processedNetworks := make(map[string]struct{})

		for _, podKey := range podAssignments {
			if err := e.podAssignment.DoWithLock(podKey, func(_ string) error {
				podStatus, exists := e.podAssignment.Load(podKey)
				if !exists {
					return nil
				}
				if podStatus.egressIPName != name {
					// we can continue here since this pod was not managed by this EIP object
					podStatus.standbyEgressIPNames.Delete(name)
					return nil
				}
				if ok := podStatus.egressStatuses.contains(statusToRemove); !ok {
					// we can continue here since this pod was not managed by this statusToRemove
					return nil
				}
				podNamespace, podName := getPodNamespaceAndNameFromKey(podKey)
				cachedNetwork := e.getNetworkFromPodAssignment(podKey)
				if cachedNetwork == nil {
					panic(fmt.Sprintf("cached network is missing for egress IP pod assignment: %q. This should never happen!", podKey))
				}

				err := e.nodeState.DoWithLock(statusToRemove.Node, func(_ string) error {
					// this statusToRemove was managing at least one pod, hence let's tear down the setup for this status
					if _, ok := processedNetworks[cachedNetwork.GetNetworkName()]; !ok {
						klog.V(2).Infof("Deleting pod egress IP status: %v for EgressIP: %s", statusToRemove, name)
						if err := e.deleteEgressIPStatusSetup(cachedNetwork, name, statusToRemove); err != nil {
							return fmt.Errorf("failed to delete EgressIP %s status setup for network %s: %v", name, cachedNetwork.GetNetworkName(), err)
						}
					}
					processedNetworks[cachedNetwork.GetNetworkName()] = struct{}{}
					// this pod was managed by statusToRemove.EgressIP; we need to try and add its SNAT back towards nodeIP
					if err := e.addExternalGWPodSNAT(cachedNetwork, podNamespace, podName, statusToRemove); err != nil {
						return err
					}
					podStatus.egressStatuses.delete(statusToRemove)
					return nil
				})
				if err != nil {
					return err
				}
				if len(podStatus.egressStatuses.statusMap) == 0 && len(podStatus.standbyEgressIPNames) == 0 {
					// pod could be managed by more than one egressIP
					// so remove the podKey from cache only if we are sure
					// there are no more egressStatuses managing this pod
					klog.V(5).Infof("Deleting pod key %s from assignment cache", podKey)
					// delete the podIP from the global egressIP address set since its no longer managed by egressIPs
					// NOTE(tssurya): There is no way to infer if the pod was scheduled on this controller's node,
					// so we try to nuke the IP from address-set anyways - it will be a no-op for remote pods
					if err := e.deletePodIPsFromAddressSet(cachedNetwork.GetNetworkName(), e.controllerName, podStatus.podIPs...); err != nil {
						return fmt.Errorf("cannot delete egressPodIPs for the pod %s from the address set: err: %v", podKey, err)
					}
					e.podAssignment.Delete(podKey)
				} else if len(podStatus.egressStatuses.statusMap) == 0 && len(podStatus.standbyEgressIPNames) > 0 {
					klog.V(2).Infof("Pod %s has standby egress IP %+v", podKey, podStatus.standbyEgressIPNames.UnsortedList())
					podStatus.egressIPName = "" // we have deleted the current egressIP that was managing the pod
					if err := e.addStandByEgressIPAssignment(cachedNetwork, podKey, podStatus); err != nil {
						klog.Errorf("Adding standby egressIPs for pod %s with status %v failed: %v", podKey, podStatus, err)
						// We are not returning the error on purpose, this will be best effort without any retries because
						// retrying deleteEgressIPAssignments for original EIP because addStandByEgressIPAssignment failed is useless.
						// Since we delete statusToRemove from podstatus.egressStatuses the first time we call this function,
						// later when the operation is retried we will never continue down the loop
						// since we add SNAT to node only if this pod was managed by statusToRemove
						return nil
					}
				}
				return nil

			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *EgressIPController) deleteNamespaceEgressIPAssignment(name string, namespace *corev1.Namespace, podSelector metav1.LabelSelector, assignStandby bool) error {
	var pods []*corev1.Pod
	var err error
	var errs []error
	selector, err := metav1.LabelSelectorAsSelector(&podSelector)
	if err != nil {
		return err
	}
	if !selector.Empty() {
		pods, err = e.watchFactory.GetPodsBySelector(namespace.Name, podSelector)
		if err != nil {
			return err
		}
	} else {
		pods, err = e.watchFactory.GetPods(namespace.Name)
		if err != nil {
			return err
		}
	}
	for _, pod := range pods {
		if cleanupErr := e.deletePodEgressIPAssignmentsUsingCache(name, pod, assignStandby); cleanupErr != nil {
			errs = append(errs, fmt.Errorf("failed to delete EgressIP %s assignment for pod %s/%s: %w",
				name, pod.Namespace, pod.Name, cleanupErr))
		}
	}
	return utilerrors.Join(errs...)
}

// deletePodEgressIPAssignmentsUsingCache removes a pod's EgressIP configuration using the network
// and statuses that were programmed for the pod. If assignStandby is true, a standby EgressIP may
// be promoted for a pod that still exists.
func (e *EgressIPController) deletePodEgressIPAssignmentsUsingCache(name string, pod *corev1.Pod, assignStandby bool) error {
	podKey := getPodKey(pod)
	e.podAssignment.LockKey(podKey)
	defer e.podAssignment.UnlockKey(podKey)

	podStatus, exists := e.podAssignment.Load(podKey)
	if !exists {
		return nil
	}
	if podStatus.network == nil {
		return fmt.Errorf("pod %s has no cached network", podKey)
	}

	var statusesToRemove []egressipv1.EgressIPStatusItem
	if podStatus.egressIPName == name {
		statusesToRemove = make([]egressipv1.EgressIPStatusItem, 0, len(podStatus.egressStatuses.statusMap))
		for status := range podStatus.egressStatuses.statusMap {
			statusesToRemove = append(statusesToRemove, status)
		}
	}

	klog.V(4).Infof("Using cached network %q and statuses for egressIP delete reconciliation of pod %s",
		podStatus.network.GetNetworkName(), podKey)
	return e.deletePodEgressIPAssignments(podStatus.network, name, statusesToRemove, pod, assignStandby)
}

// deletePodEgressIPAssignments *must* be called with podAssignment lock on pod
// If assignStandby is true, any standby EgressIP for the pod will be promoted to active.
func (e *EgressIPController) deletePodEgressIPAssignments(ni util.NetInfo, name string, statusesToRemove []egressipv1.EgressIPStatusItem, pod *corev1.Pod,
	assignStandby bool) error {
	podKey := getPodKey(pod)
	podStatus, exists := e.podAssignment.Load(podKey)
	if !exists {
		return nil
	} else if podStatus.egressIPName != name {
		// standby egressIP no longer matches this pod, update cache
		podStatus.standbyEgressIPNames.Delete(name)
	} else {
		for _, statusToRemove := range statusesToRemove {
			if ok := podStatus.egressStatuses.contains(statusToRemove); !ok {
				// we can continue here since this pod was not managed by this statusToRemove
				continue
			}
			klog.V(2).Infof("Deleting pod egress IP status: %v for EgressIP: %s and pod: %s/%s", statusToRemove, name, pod.Name, pod.Namespace)
			nodesToLock := []string{statusToRemove.Node, pod.Spec.NodeName}
			// Sort nodes lexicographically to ensure node locks are acquired in
			// same order, preventing deadlock situations.
			sort.Strings(nodesToLock)
			err := e.nodeState.DoWithLock(nodesToLock[0], func(_ string) error {
				if statusToRemove.Node == pod.Spec.NodeName {
					// we are safe, no need to grab lock again
					if err := e.deletePodEgressIPAssignment(ni, name, statusToRemove, pod); err != nil {
						return err
					}
					podStatus.egressStatuses.delete(statusToRemove)
					return nil
				}
				return e.nodeState.DoWithLock(nodesToLock[1], func(_ string) error {
					if err := e.deletePodEgressIPAssignment(ni, name, statusToRemove, pod); err != nil {
						return err
					}
					podStatus.egressStatuses.delete(statusToRemove)
					return nil
				})
			})
			if err != nil {
				return err
			}
		}
	}
	// Delete the key if there are no more status assignments to keep
	// for the pod and no other assigned standby EgressIPs.
	if len(podStatus.egressStatuses.statusMap) == 0 && len(podStatus.standbyEgressIPNames) == 0 {
		// pod could be managed by more than one egressIP
		// so remove the podKey from cache only if we are sure
		// there are no more egressStatuses managing this pod
		klog.V(5).Infof("Deleting pod key %s from assignment cache", podKey)
		if e.isPodScheduledOnLocalNode(pod) {
			if err := e.deletePodIPsFromAddressSet(ni.GetNetworkName(), e.controllerName, podStatus.podIPs...); err != nil {
				return fmt.Errorf("cannot delete egressPodIPs for the pod %s from the address set: err: %v", podKey, err)
			}
		}
		e.podAssignment.Delete(podKey)
	} else if len(podStatus.egressStatuses.statusMap) == 0 && len(podStatus.standbyEgressIPNames) > 0 && assignStandby {
		// If the pod is no longer assigned an active EgressIP, promote a standby EgressIP to active.
		// If the pod is already deleted, addStandByEgressIPAssignment is a no-op, so calling it is safe.
		// The pod's state is removed from the podAssignment cache when deletePodEgressIPAssignments is invoked
		// for the standby EgressIP.
		// This block is not executed in the following cases:
		// 1. During stale-assignment cleanup in addPodEgressIPAssignments, because podStatus.standbyEgressIPNames
		//    is empty.
		// 2. During stale-network cleanup (assignStandby=false).
		klog.V(2).Infof("Pod %s has standby egress IP %+v", podKey, podStatus.standbyEgressIPNames.UnsortedList())
		// We have deleted the current egressIP (i.e. podStatus.egressStatuses.statusMap is empty now) that was managing
		// the pod, so set podStatus.egressIPName to empty string. This allows the promotion of a standby EgressIP to active:
		// addStandByEgressIPAssignment (called below) invokes addPodEgressIPAssignments, which checks if
		// podState.egressIPName matches the new EgressIP name or is empty. When empty, it allows the standby EgressIP
		// to become the active one for this pod.
		podStatus.egressIPName = ""
		if err := e.addStandByEgressIPAssignment(ni, podKey, podStatus); err != nil {
			klog.Errorf("Adding standby egressIPs for pod %s with status %v failed: %v", podKey, podStatus, err)
			// We are not returning the error on purpose, this will be best effort without any retries because
			// retrying deletePodEgressIPAssignments for original EIP because addStandByEgressIPAssignment failed is
			// useless.
			return nil
		}
	}
	return nil
}

// deletePreviousNetworkPodEgressIPAssignments checks if the network changed and remove any stale config on the previous network.
// must be called with podAssignment lock on pod
func (e *EgressIPController) deletePreviousNetworkPodEgressIPAssignments(ni util.NetInfo, name string, statusesToRemove []egressipv1.EgressIPStatusItem,
	pod *corev1.Pod, assignStandby bool) {
	cachedNetwork := e.getNetworkFromPodAssignment(getPodKey(pod))
	if cachedNetwork != nil {
		if !util.AreNetworksCompatible(cachedNetwork, ni) {
			if err := e.deletePodEgressIPAssignments(cachedNetwork, name, statusesToRemove, pod, assignStandby); err != nil {
				// no error is returned because high probability network is deleted
				klog.Errorf("Failed to delete EgressIP %s assignment for pod %s/%s attached to network %s: %v",
					name, pod.Namespace, pod.Name, cachedNetwork.GetNetworkName(), err)
			}
		}
	}
}

// isPodScheduledOnLocalNode returns true when the pod is scheduled on the node
// managed by this controller.
func (e *EgressIPController) isPodScheduledOnLocalNode(pod *corev1.Pod) bool {
	return util.PodScheduled(pod) && pod.Spec.NodeName == e.nodeName
}

// getNodeLocality reports whether nodeName is managed by this controller and
// whether the node has been observed. Callers use the second result to avoid
// programming OVN for nodes that have not been added or were already deleted.
func (e *EgressIPController) getNodeLocality(nodeName string) (isLocal, loaded bool) {
	_, loaded = e.nodeState.Load(nodeName)
	return loaded && nodeName == e.nodeName, loaded
}

type egressIPCache struct {
	// egressIP name -> network name -> cache
	egressIPNameToPods map[string]map[string]selectedPods
	// egressIP name -> to assigned Node names
	egressIPNameToAssignedNodes map[string][]string
	// egressLocalNode is set to this controller's node when at least one EgressIP is assigned to it.
	egressLocalNode string
	// egressIP IP -> assigned node name
	egressIPIPToNodeCache map[string]string
	// egressIP name -> egress IP -> assigned node name
	egressIPToAssignedNodes map[string]map[string]string
	// node name -> network name -> redirect IPs
	egressNodeRedirectsCache nodeNetworkRedirects
	// network name -> OVN cluster router name
	networkToRouter map[string]string
	// packet mark for primary UDNs
	// EgressIP name -> mark
	markCache map[string]string
}

type nodeNetworkRedirects struct {
	// network name -> node name -> redirect IPs
	cache map[string]map[string]redirectIPs
}

type selectedPods struct {
	// egressLocalPods contains pods scheduled on this controller's node that are served by this EgressIP
	// object. This will help sync LRP & LRSR.
	egressLocalPods map[string]sets.Set[string]
	// egressRemotePods will contain all the remote pods
	// that are being served by this egressIP object
	// This will help sync SNATs.
	egressRemotePods map[string]sets.Set[string] // will be used only when multizone IC is enabled
}

// redirectIPs stores IPv4 or IPv6 next hops to support creation of OVN router logical router policies.
type redirectIPs struct {
	v4Gateway       string
	v6Gateway       string
	v4TransitSwitch string
	v6TransitSwitch string
	v4MgtPort       string
	v6MgtPort       string
}

func (r redirectIPs) containsIP(ip string) bool {
	switch ip {
	case r.v4MgtPort, r.v6MgtPort, r.v4TransitSwitch, r.v6TransitSwitch, r.v4Gateway, r.v6Gateway:
		return true
	}
	return false
}

// egressIPNetworkFilter selects which networks a stale-NB sync should process.
type egressIPNetworkFilter func(networkName string) bool

func egressIPAllNetworks(_ string) bool {
	return true
}

func (e *EgressIPController) syncEgressIPs(_ []interface{}) error {
	// Initial EgressIP informer sync for the default network.
	e.ensureNetworkPending(types.DefaultNetworkName)
	return e.SyncNetworkReady(types.DefaultNetworkName)
}

// EnqueueNetwork marks the network pending (if absent) and queues EgressIP bring-up.
func (e *EgressIPController) EnqueueNetwork(networkName string) {
	e.ensureNetworkPending(networkName)
	e.networkReconciler.Reconcile(networkName)
}

// ensureNetworkPending records that the network should be brought up. No-op if
// an entry already exists (pending or synced).
func (e *EgressIPController) ensureNetworkPending(networkName string) {
	e.registeredNetworks.LoadOrStore(networkName, networkPending)
}

func (e *EgressIPController) networkState(networkName string) (networkSyncState, bool) {
	val, ok := e.registeredNetworks.Load(networkName)
	if !ok {
		return "", false
	}
	state, ok := val.(networkSyncState)
	return state, ok
}

// IsNetworkRegistered reports whether the network finished EgressIP stale sync
// and live handlers may program it.
func (e *EgressIPController) IsNetworkRegistered(networkName string) bool {
	state, ok := e.networkState(networkName)
	return ok && state == networkSynced
}

// registerNetwork marks the network synced for live EgressIP programming.
// Caller must hold the exclusive lock after confirming the entry still exists.
func (e *EgressIPController) registerNetwork(networkName string) {
	e.registeredNetworks.Store(networkName, networkSynced)
}

// UnregisterNetwork removes the network's bring-up entry so a later enqueue can
// start fresh (e.g. after UDN stop/recreate). Takes the exclusive lock so an
// in-flight bring-up cannot mark the network synced after teardown.
func (e *EgressIPController) UnregisterNetwork(networkName string) {
	e.lock.Lock()
	defer e.lock.Unlock()
	e.registeredNetworks.Delete(networkName)
}

// SyncNetworkReady runs initial stale sync when the network is pending, then
// enqueues pods. Skips networks with no entry (never enqueued or unregistered).
// When already synced, only pod enqueue is retried.
func (e *EgressIPController) SyncNetworkReady(networkName string) error {
	if err := e.ensureNetworkRegistered(networkName); err != nil {
		return err
	}
	// Skip enqueue if never enqueued, or the entry was removed during sync.
	if _, exists := e.networkState(networkName); !exists {
		return nil
	}
	return e.enqueueEgressIPPodsForNetwork(networkName)
}

// ensureNetworkRegistered runs stale cleanup and registers the network if it is
// still pending. Serializes against live EgressIP/pod/namespace handlers and teardown.
func (e *EgressIPController) ensureNetworkRegistered(networkName string) error {
	e.lock.Lock()
	defer e.lock.Unlock()
	state, exists := e.networkState(networkName)
	if !exists {
		return nil
	}
	if state == networkSynced {
		return nil
	}
	if err := e.syncNetworkStaleEntries(networkName); err != nil {
		return fmt.Errorf("failed to sync stale EgressIP entries for network %s: %w", networkName, err)
	}
	e.registerNetwork(networkName)
	return nil
}

func (e *EgressIPController) enqueueEgressIPPodsForNetwork(networkName string) error {
	namespaces, err := e.watchFactory.GetNamespaces()
	if err != nil {
		return fmt.Errorf("failed to list namespaces while enqueueing EgressIP pods for network %s: %w", networkName, err)
	}
	for _, namespace := range namespaces {
		ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
		if err != nil {
			if apierrors.IsNotFound(err) || util.IsInvalidPrimaryNetworkError(err) {
				continue
			}
			return fmt.Errorf("failed to get active network for namespace %s while enqueueing EgressIP pods for network %s: %w",
				namespace.Name, networkName, err)
		}
		if ni == nil || ni.GetNetworkName() != networkName {
			continue
		}
		if err := e.addEgressIPPodRetriesForNamespace(namespace.Name, "network ready"); err != nil {
			return err
		}
	}
	return nil
}

// DestroyNetworkServedPodsAddressSet removes the EgressIP served-pods address set
// for a network. That set is owned by the default EgressIP controller, so
// per-network UDN address-set cleanup does not remove it.
func (e *EgressIPController) DestroyNetworkServedPodsAddressSet(networkName string) error {
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, networkName, e.controllerName)
	if err := e.addressSetFactory.DestroyAddressSet(dbIDs); err != nil {
		return fmt.Errorf("failed to destroy EgressIP served-pods address set for network %s: %w", networkName, err)
	}
	return nil
}

func (e *EgressIPController) syncNetworkStaleEntries(networkName string) error {
	include := func(name string) bool { return name == networkName }
	// Sync stale OVN state for this network (deleted EIPs, pods/namespaces that
	// stopped matching, address-set reset from the constructed kapi cache).
	// FIXME(martinkennelly): re-enable when EIP controller is fully extracted from DNC
	//if err := e.SyncNodeCache(); err != nil {
	//	return fmt.Errorf("SyncNodeCache unable to update the node cache: %v", err)
	//}
	egressIPCache, err := e.generateCacheForEgressIP(include)
	if err != nil {
		return fmt.Errorf("unable to generate cache for egressip: %v", err)
	}
	if err = e.syncStaleEgressReroutePolicy(egressIPCache, include); err != nil {
		return fmt.Errorf("unable to remove stale reroute policies: %v", err)
	}
	if err = e.syncStaleSNATRules(egressIPCache, include); err != nil {
		return fmt.Errorf("unable to remove stale nats: %v", err)
	}
	if err = e.syncPodAssignmentCache(egressIPCache, include); err != nil {
		return fmt.Errorf("unable to sync internal pod assignment cache: %v", err)
	}
	if err = e.syncStaleAddressSetIPs(egressIPCache, include); err != nil {
		return fmt.Errorf("unable to reset stale address IPs: %v", err)
	}
	if err = e.syncStaleGWMarkRules(egressIPCache, include); err != nil {
		return fmt.Errorf("unable to sync GW packet mark rules: %v", err)
	}
	return nil
}

// SyncNodeCache records all Nodes currently known to the informer cache.
func (e *EgressIPController) SyncNodeCache() error {
	nodes, err := e.watchFactory.GetNodes()
	if err != nil {
		return fmt.Errorf("unable to fetch nodes from watch factory: %w", err)
	}
	for _, node := range nodes {
		e.nodeState.LockKey(node.Name)
		e.nodeState.Store(node.Name, struct{}{})
		e.nodeState.UnlockKey(node.Name)
	}
	return nil
}

// getLocalNodeName returns the controller's node after it has been observed in
// the node state cache.
func (e *EgressIPController) getLocalNodeName() (string, error) {
	if _, ok := e.nodeState.Load(e.nodeName); ok {
		return e.nodeName, nil
	}
	return "", fmt.Errorf("failed to find local node %q in the node cache", e.nodeName)
}

func (e *EgressIPController) StartNADReconciler() (err error) {
	if err = e.StartNetworkReconciler(); err != nil {
		return err
	}
	if e.networkManager == nil || e.nadReconciler == nil {
		return nil
	}

	if !e.nadReconcilerRegistered {
		defer func() {
			if err != nil {
				e.networkManager.DeRegisterNADReconciler(e.nadReconcilerID)
				e.nadReconcilerRegistered = false
				e.nadReconcilerID = 0
			}
		}()
		id := e.networkManager.RegisterNADReconciler(e.nadReconciler)
		e.nadReconcilerID = id
		e.nadReconcilerRegistered = true
	}
	return controller.Start(e.nadReconciler)
}

// StartNetworkReconciler starts the network reconciler that runs SyncNetworkReady
// for networks enqueued via EnqueueNetwork.
func (e *EgressIPController) StartNetworkReconciler() error {
	return controller.Start(e.networkReconciler)
}

func (e *EgressIPController) StopNADReconciler() {
	e.StopNetworkReconciler()
	if e.nadReconciler == nil {
		return
	}
	if e.nadReconcilerRegistered {
		e.networkManager.DeRegisterNADReconciler(e.nadReconcilerID)
		e.nadReconcilerRegistered = false
	}
	controller.Stop(e.nadReconciler)
	e.nadReconcilerID = 0
	e.nadReconciler = nil
}

func (e *EgressIPController) StopNetworkReconciler() {
	controller.Stop(e.networkReconciler)
}

func (e *EgressIPController) syncNAD(key string) error {
	startTime := time.Now()
	klog.V(5).Infof("Egress IP NAD reconcile %s", key)
	defer func() {
		klog.V(4).Infof("Finished syncing Egress IP for NAD %s, took %v", key, time.Since(startTime))
	}()

	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Failed splitting NAD key %s: %v", key, err)
		return nil
	}

	ni := e.networkManager.GetNetInfoForNADKey(key)
	if ni == nil {
		return nil
	}
	// Only reconcile for primary network NADs. Secondary NADs are irrelevant for EgressIP.
	if !ni.IsPrimaryNetwork() {
		return nil
	}
	// Ensure egressIP pods for this namespace are retried after NAD processing so
	// we don't miss the UDN IPs if pod updates raced the NAD event.
	return e.addEgressIPPodRetriesForNamespace(namespace, "NAD change")
}

func (e *EgressIPController) addEgressIPPodRetriesForNamespace(namespace, reason string) error {
	if e.retryEgressIPPods == nil {
		return nil
	}
	pods, err := e.watchFactory.GetPods(namespace)
	if err != nil {
		return fmt.Errorf("failed to list pods for EgressIP retry in namespace %s: %w", namespace, err)
	}
	var errs []error
	for _, pod := range pods {
		pod := *pod
		if util.PodCompleted(&pod) {
			continue
		}
		if err := e.addEgressIPPodRetry(&pod, reason); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

func (e *EgressIPController) addEgressIPPodRetry(pod *corev1.Pod, reason string) error {
	if e.retryEgressIPPods == nil || pod == nil || util.PodCompleted(pod) || !util.PodNeedsSNAT(pod) {
		return nil
	}
	klog.V(5).Infof("Adding egress IP pod %s/%s for immediate retry due to %s", pod.Namespace, pod.Name, reason)
	if err := e.retryEgressIPPods.AddRetryObjWithAddNoBackoff(pod); err != nil {
		return fmt.Errorf("failed to add pod %s/%s to egressIP retry queue due to %s: %w",
			pod.Namespace, pod.Name, reason, err)
	}
	e.retryEgressIPPods.RequestRetryObjs()
	return nil
}

func (e *EgressIPController) syncStaleAddressSetIPs(egressIPCache egressIPCache, include egressIPNetworkFilter) error {
	for _, networkPodCache := range egressIPCache.egressIPNameToPods {
		for networkName, podCache := range networkPodCache {
			if !include(networkName) {
				continue
			}
			dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, networkName, e.controllerName)
			as, err := e.addressSetFactory.EnsureAddressSet(dbIDs)
			if err != nil {
				return fmt.Errorf("network %s: cannot ensure that addressSet for egressIP pods %s exists %v", networkName, EgressIPServedPodsAddrSetName, err)
			}
			var allEIPServedPodIPs []net.IP
			// we only care about pods on this controller's node for the address-set since
			// traffic from remote pods towards nodeIP won't reach this controller's node
			for _, podIPs := range podCache.egressLocalPods {
				for podIP := range podIPs {
					allEIPServedPodIPs = append(allEIPServedPodIPs, net.ParseIP(podIP))
				}
			}

			// we replace all IPs in the address-set based on eIP cache constructed from kapi
			// note that setIPs is not thread-safe
			if err = as.SetAddresses(util.StringSlice(allEIPServedPodIPs)); err != nil {
				return fmt.Errorf("network %s: cannot reset egressPodIPs in address set %v: err: %v", networkName, EgressIPServedPodsAddrSetName, err)
			}
		}
	}
	return nil
}

// syncStaleGWMarkRules removes stale or invalid LRP that packet mark. They are attached to egress nodes gateway router.
// It adds expected LRPs that packet mark. This is valid only for L3 networks.
func (e *EgressIPController) syncStaleGWMarkRules(egressIPCache egressIPCache, include egressIPNetworkFilter) error {
	// Delete all stale LRPs then add missing LRPs
	// This determines if an LRP is valid for this controller, but doesn't determine whether the
	// LRP is attached to the correct gateway router.
	if !isEgressIPForUDNSupported() {
		return nil
	}
	for _, networkPodCache := range egressIPCache.egressIPNameToPods {
		for networkName, podCache := range networkPodCache {
			// skip GW mark rules processing for CDN because they don't exist
			if networkName == types.DefaultNetworkName || !include(networkName) {
				continue
			}

			invalidLRPPredicate := func(item *nbdb.LogicalRouterPolicy) bool {
				if item.Priority != types.EgressIPSNATMarkPriority || item.Action != nbdb.LogicalRouterPolicyActionAllow {
					return false
				}
				// skip if owned by another controller
				if item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] != getNetworkControllerName(networkName) {
					return false
				}
				eIPName, podNamespaceName := getEIPLRPObjK8MetaData(item.ExternalIDs)
				if eIPName == "" || podNamespaceName == "" {
					klog.Errorf("Sync stale SNAT Mark rules for network %s unable to process logical router policy because invalid meta data", networkName)
					return true
				}
				_, exists := egressIPCache.egressIPNameToPods[eIPName]
				// if EgressIP doesn't exist, its stale
				if !exists {
					return true
				}
				// if there is no local egress node, the LRP must be invalid
				if egressIPCache.egressLocalNode == "" {
					return true
				}
				ipsLocal, okLocal := podCache.egressLocalPods[podNamespaceName]
				ipsRemote, okRemote := podCache.egressRemotePods[podNamespaceName]
				// if pod doesn't exist locally or remote, its stale
				if !okLocal && !okRemote {
					return true
				}
				var ips sets.Set[string]
				if okLocal {
					ips = ipsLocal
				}
				if okRemote {
					ips = ipsRemote
				}
				podIP := getPodIPFromEIPSNATMarkMatch(item.Match)
				if podIP == "" {
					// invalid match
					return true
				}
				if !ips.Has(podIP) {
					return true
				}
				// FIXME: This doesn't verify that the LRP is attached to the correct node's gateway router.
				pktMarkValue, ok := item.Options["pkt_mark"]
				if !ok || egressIPCache.markCache[eIPName] != "" && pktMarkValue != egressIPCache.markCache[eIPName] {
					return true
				}
				return false
			}
			invalidLRPs, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(e.nbClient, invalidLRPPredicate)
			if err != nil {
				return fmt.Errorf("network %s: unable to retrieve invalid SNAT mark logical router polices: %v", networkName, err)
			}
			if len(invalidLRPs) == 0 {
				return nil
			}
			// gather UUIDs of invalid LRPs
			invalidLRPUUIDs := sets.New[string]()
			for _, invalidLRP := range invalidLRPs {
				invalidLRPUUIDs.Insert(invalidLRP.UUID)
			}
			localNodeObserved, _ := e.getNodeLocality(e.nodeName)
			if !localNodeObserved {
				continue
			}
			invalidLRPPredicate = func(item *nbdb.LogicalRouterPolicy) bool {
				return invalidLRPUUIDs.Has(item.UUID)
			}
			ni, err := util.NewNetInfo(&ovncnitypes.NetConf{
				Topology: types.Layer3Topology,
				NetConf: cnitypes.NetConf{
					Name: networkName,
				},
			})
			if err != nil {
				return fmt.Errorf("failed to create new network %s: %v", networkName, err)
			}
			routerName := ni.GetNetworkScopedGWRouterName(e.nodeName)
			lrps, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(e.nbClient, routerName, invalidLRPPredicate)
			if err != nil {
				if errors.Is(err, libovsdbclient.ErrNotFound) {
					continue
				}
				return fmt.Errorf("network %s: failed to find gateway routers (%s) invalid logical router policies: %v", networkName, routerName, err)
			}
			if err = libovsdbops.DeleteLogicalRouterPolicies(e.nbClient, routerName, lrps...); err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
				return fmt.Errorf("network %s: failed to delete gateway routers (%s) invalid logical router policies: %v", networkName, routerName, err)
			}

		}
	}

	// ensure all LRPs to mark pods are present
	isSupportedIP := func(podIP net.IP) bool {
		isIPv6 := utilnet.IsIPv6(podIP)
		if isIPv6 && e.v6 {
			return true
		}
		if !isIPv6 && e.v4 {
			return true
		}
		return false
	}

	processPodFn := func(ops []ovsdb.Operation, eIPName, podKey, mark, routerName, networkName string, podIPs sets.Set[string], isEIPIPv6 bool) ([]ovsdb.Operation, error) {
		podNamespace, podName := getPodNamespaceAndNameFromKey(podKey)
		dbIDs := getEgressIPLRPSNATMarkDbIDs(eIPName, podNamespace, podName, getEIPIPFamily(isEIPIPv6), networkName, e.controllerName)
		for _, podIPStr := range podIPs.UnsortedList() {
			podIP := net.ParseIP(podIPStr)
			if podIP == nil || utilnet.IsIPv6(podIP) != isEIPIPv6 && !isSupportedIP(podIP) {
				continue
			}
			lrp := nbdb.LogicalRouterPolicy{
				Match:       fmt.Sprintf("%s.src == %s", getEIPIPFamily(isEIPIPv6), podIPStr),
				Priority:    types.EgressIPSNATMarkPriority,
				Action:      nbdb.LogicalRouterPolicyActionAllow,
				ExternalIDs: dbIDs.GetExternalIDs(),
				Options:     map[string]string{"pkt_mark": mark},
			}
			p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
			ops, err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(e.nbClient, ops, routerName, &lrp, p)
			if err != nil {
				return ops, fmt.Errorf("network %s: error creating logical router policy %+v create/update ops for packet marking on router %s: %v", networkName, lrp, routerName, err)
			}
		}
		return ops, nil
	}

	var ops []ovsdb.Operation
	for eIPName, networkPodCache := range egressIPCache.egressIPNameToPods {
		if egressIPCache.markCache[eIPName] == "" {
			continue
		}
		for networkName, podCache := range networkPodCache {
			if networkName == types.DefaultNetworkName || !include(networkName) {
				continue
			}
			for eIPIP, nodeName := range egressIPCache.egressIPIPToNodeCache {
				if egressIPCache.egressLocalNode != nodeName {
					continue
				}
				ni, err := util.NewNetInfo(&ovncnitypes.NetConf{
					Topology: types.Layer3Topology,
					NetConf: cnitypes.NetConf{
						Name: networkName,
					},
				})
				if err != nil {
					return fmt.Errorf("failed to create new network %s: %v", networkName, err)
				}
				routerName := ni.GetNetworkScopedGWRouterName(nodeName)
				isEIPIPv6 := utilnet.IsIPv6String(eIPIP)
				for podKey, podIPs := range podCache.egressLocalPods {
					ops, err = processPodFn(ops, eIPName, podKey, egressIPCache.markCache[eIPName], routerName, networkName, podIPs, isEIPIPv6)
					if err != nil {
						return fmt.Errorf("network %s: failed to add process local pod pod %s gateway router SNAT mark: %v", networkName, podKey, err)
					}
				}
				for podKey, podIPs := range podCache.egressRemotePods {
					ops, err = processPodFn(ops, eIPName, podKey, egressIPCache.markCache[eIPName], routerName, networkName, podIPs, isEIPIPv6)
					if err != nil {
						return fmt.Errorf("network %s: failed to add process remote pod %s gateway router SNAT mark: %v", networkName, podKey, err)
					}
				}
			}
		}
	}
	_, err := libovsdbops.TransactAndCheck(e.nbClient, ops)
	if err != nil {
		return fmt.Errorf("error transacting ops %+v: %v", ops, err)
	}

	return nil
}

// syncPodAssignmentCache rebuilds the internal pod cache used by the egressIP feature.
// We use the existing kapi and ovn-db information to populate oc.eIPC.podAssignment cache for
// all the pods that are managed by egressIPs.
// NOTE: This is done mostly to handle the corner case where one pod has more than one
// egressIP object matching it, in which case we do the ovn setup only for one of the objects.
// This corner case  of same pod matching more than one object will not work for IC deployments
// since internal cache based logic will be different for different ovnkube-controllers.
// One controller can think objA is active while another thinks objB is active if both have multiple choices.
func (e *EgressIPController) syncPodAssignmentCache(egressIPCache egressIPCache, include egressIPNetworkFilter) error {
	for egressIPName, networkPods := range egressIPCache.egressIPNameToPods {
		for networkName, podCache := range networkPods {
			if !include(networkName) {
				continue
			}
			p1 := func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Priority == types.EgressIPReroutePriority &&
					item.ExternalIDs[libovsdbops.NetworkKey.String()] == networkName &&
					item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == getNetworkControllerName(networkName) &&
					strings.HasPrefix(item.ExternalIDs[libovsdbops.ObjectNameKey.String()], egressIPName+dbIDEIPNamePodDivider)
			}
			ni := e.networkManager.GetNetwork(networkName)
			if ni == nil {
				return fmt.Errorf("failed to get active network for network name %q", networkName)
			}
			routerName, err := e.getTopologyScopedLocalRouterName(ni)
			if err != nil {
				return err
			}
			reRoutePolicies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(e.nbClient, routerName, p1)
			if err != nil {
				return fmt.Errorf("failed to retrieve a logical router polices attached to router %s: %w", routerName, err)
			}
			// not scoped by network since all NATs selected by the follow predicate select only CDN NATs
			p2 := func(item *nbdb.NAT) bool {
				return strings.HasPrefix(item.ExternalIDs[libovsdbops.ObjectNameKey.String()], egressIPName)
			}
			// NATs are only valid for CDN
			egressIPSNATs := make([]*nbdb.NAT, 0)
			if networkName == types.DefaultNetworkName {
				egressIPSNATs, err = libovsdbops.FindNATsWithPredicate(e.nbClient, p2)
				if err != nil {
					return fmt.Errorf("failed to find NATs with predicate for network %s: %v", networkName, err)
				}
			}
			// Because of how we do generateCacheForEgressIP, we will only have pods that are
			// either scheduled on this controller's node (in which case reRoutePolicies will work) OR pods
			// managed by this controller's egressIP node (in which case egressIPSNATs will work)
			egressPods := make(map[string]sets.Set[string])
			for podKey, podIPs := range podCache.egressLocalPods {
				egressPods[podKey] = podIPs
			}
			for podKey, podIPs := range podCache.egressRemotePods {
				egressPods[podKey] = podIPs
			}
			for podKey, podIPsSet := range egressPods {
				podIPs := make([]net.IP, 0, podIPsSet.Len())
				for _, podIP := range podIPsSet.UnsortedList() {
					podIPs = append(podIPs, net.ParseIP(podIP))
				}
				if err := e.podAssignment.DoWithLock(podKey, func(podKey string) error {
					podState, ok := e.podAssignment.Load(podKey)
					if !ok {
						podState = &podAssignmentState{
							egressStatuses:       egressStatuses{make(map[egressipv1.EgressIPStatusItem]string)},
							standbyEgressIPNames: sets.New[string](),
							podIPs:               podIPs,
							network:              ni,
						}
					}

					podState.standbyEgressIPNames.Insert(egressIPName)
					for _, policy := range reRoutePolicies {
						splitMatch := strings.Split(policy.Match, " ")
						if len(splitMatch) <= 0 {
							continue
						}
						logicalIP := splitMatch[len(splitMatch)-1]
						parsedLogicalIP := net.ParseIP(logicalIP)
						if parsedLogicalIP == nil {
							continue
						}

						if podIPsSet.Has(parsedLogicalIP.String()) { // should match for only one egressIP object
							podState.egressIPName = egressIPName
							podState.standbyEgressIPNames.Delete(egressIPName)
							klog.Infof("EgressIP %s is managing pod %s for network %s", egressIPName, podKey, networkName)
						}
					}
					// process SNAT only for CDN
					if networkName == types.DefaultNetworkName {
						for _, snat := range egressIPSNATs {
							if podIPsSet.Has(snat.LogicalIP) { // should match for only one egressIP object
								podState.egressIPName = egressIPName
								podState.standbyEgressIPNames.Delete(egressIPName)
								klog.Infof("EgressIP %s is managing pod %s for network %s", egressIPName, podKey, networkName)
							}
						}
					}

					// populate podState.egressStatuses with assigned node for active egressIP IPs.
					if podState.egressIPName == egressIPName {
						for egressIPIP, nodeName := range egressIPCache.egressIPToAssignedNodes[egressIPName] {
							podState.egressStatuses.statusMap[egressipv1.EgressIPStatusItem{
								EgressIP: egressIPIP, Node: nodeName}] = egressStatusStatePending
						}
					}

					e.podAssignment.Store(podKey, podState)
					return nil
				}); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// This function implements a portion of syncEgressIPs.
// It removes OVN logical router policies used by EgressIPs deleted while ovnkube-controller was down.
// It also removes stale nexthops from router policies used by EgressIPs.
// Upon failure, it may be invoked multiple times in order to avoid a pod restart.
func (e *EgressIPController) syncStaleEgressReroutePolicy(cache egressIPCache, include egressIPNetworkFilter) error {
	// limit Nodes only to egress node(s) for the EgressIP name
	limitToValidEgressNodes := func(eipName string, nodeRedirectCache map[string]redirectIPs) map[string]redirectIPs {
		filteredEgressNodesRedirectsCache := make(map[string]redirectIPs, 0)
		egressNodeNames, ok := cache.egressIPNameToAssignedNodes[eipName]
		if !ok {
			return filteredEgressNodesRedirectsCache
		}
		for _, egressNode := range egressNodeNames {
			if nodeRedirect, ok := nodeRedirectCache[egressNode]; ok {
				filteredEgressNodesRedirectsCache[egressNode] = nodeRedirect
			}
		}
		return filteredEgressNodesRedirectsCache
	}

	for eipName, networkCache := range cache.egressIPNameToPods {
		for networkName, data := range networkCache {
			if !include(networkName) {
				continue
			}
			logicalRouterPolicyStaleNexthops := []*nbdb.LogicalRouterPolicy{}
			// select LRPs scoped to the correct LRP priority, network and EIP name
			p := func(item *nbdb.LogicalRouterPolicy) bool {
				if item.Priority != types.EgressIPReroutePriority || item.ExternalIDs[libovsdbops.NetworkKey.String()] != networkName {
					return false
				}
				extractedEgressIPName, _ := getEIPLRPObjK8MetaData(item.ExternalIDs)
				if extractedEgressIPName == "" {
					klog.Errorf("syncStaleEgressReroutePolicy found logical router policy (UUID: %s) with invalid meta data associated with network %s", item.UUID, networkName)
					return true
				}
				if extractedEgressIPName != eipName {
					// remove if there's no reference to this EIP name
					_, ok := cache.egressIPNameToPods[extractedEgressIPName]
					return !ok
				}
				networkNodeRedirectCache := limitToValidEgressNodes(eipName, cache.egressNodeRedirectsCache.cache[networkName])
				if len(networkNodeRedirectCache) == 0 {
					klog.Infof("syncStaleEgressReroutePolicy deleting invalid logical router policy %q because there are no existing nodes assigned to its EgressIP %s", item.UUID, eipName)
					return true
				}
				splitMatch := strings.Split(item.Match, " ")
				podIPStr := splitMatch[len(splitMatch)-1]
				podIP := net.ParseIP(podIPStr)
				if podIP == nil {
					klog.Infof("syncStaleEgressReroutePolicy found invalid LRP with broken match with UID %q", item.UUID)
					return true
				}
				egressPodIPs := sets.NewString()
				// Since LRPs are created only for pods scheduled on this controller's node,
				// we need to care about only those pods. Nexthop for them will
				// either be transit switch IP or join switch IP or mp0 IP.
				// FIXME: LRPs are also created for remote pods to route them
				// correctly but we do not handling cleaning for them now
				for _, podIPs := range data.egressLocalPods {
					egressPodIPs.Insert(podIPs.UnsortedList()...)
				}
				if !egressPodIPs.Has(podIP.String()) {
					klog.Infof("syncStaleEgressReroutePolicy will delete %s due to no nexthop or stale logical ip: %v", extractedEgressIPName, item)
					return true
				}
				// Check for stale nexthops that may exist in the logical router policy and store that in logicalRouterPolicyStaleNexthops.
				// Note: adding missing nexthop(s) to the logical router policy is done outside the scope of this function.
				staleNextHops := []string{}
				for _, nexthop := range item.Nexthops {
					// ensure valid next hop by iterating through the node config
					var isFound bool // isFound is true, if the next hop IP is found within the set of assigned nodes
					for _, nodeRedirect := range networkNodeRedirectCache {
						if nodeRedirect.containsIP(nexthop) {
							isFound = true
							break
						}
					}
					if !isFound {
						//FIXME: be more specific about which is the valid next hop instead of relying on verifying if the IP is within a valid set of IPs.
						staleNextHops = append(staleNextHops, nexthop)
					}
				}
				if len(staleNextHops) > 0 {
					lrp := nbdb.LogicalRouterPolicy{
						UUID:     item.UUID,
						Nexthops: staleNextHops,
					}
					logicalRouterPolicyStaleNexthops = append(logicalRouterPolicyStaleNexthops, &lrp)
				}
				return false
			}

			err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(e.nbClient, cache.networkToRouter[networkName], p)
			if err != nil {
				return fmt.Errorf("error deleting stale logical router policies from router %s for network %s: %v", cache.networkToRouter[networkName], networkName, err)
			}

			// Update Logical Router Policies that have stale nexthops. Notice that we must do this separately
			// because logicalRouterPolicyStaleNexthops must be populated first
			for _, staleNextHopLogicalRouterPolicy := range logicalRouterPolicyStaleNexthops {
				klog.Infof("syncStaleEgressReroutePolicy will remove stale nexthops for LRP %q for network %s: %v",
					staleNextHopLogicalRouterPolicy.UUID, networkName, staleNextHopLogicalRouterPolicy.Nexthops)
			}
			// nothing to do if there's no stale next hops
			if len(logicalRouterPolicyStaleNexthops) == 0 {
				continue
			}
			err = libovsdbops.DeleteNextHopsFromLogicalRouterPolicies(e.nbClient, cache.networkToRouter[networkName], logicalRouterPolicyStaleNexthops...)
			if err != nil {
				return fmt.Errorf("unable to remove stale next hops from logical router policies for network %s: %v", networkName, err)
			}
		}
	}

	return nil
}

// This function implements a portion of syncEgressIPs.
// It removes OVN NAT rules used by EgressIPs deleted while ovnkube-controller was down for the default cluster network only.
// Upon failure, it may be invoked multiple times in order to avoid a pod restart. For UDNs, we do not SNAT to the EgressIP
// using OVNs gateway router and in-fact we do not use OVN, but instead we add OVS flows in the external bridge to SNAT to the EgressIP. This is not managed here.
func (e *EgressIPController) syncStaleSNATRules(egressIPCache egressIPCache, include egressIPNetworkFilter) error {
	// OVN SNATs are only programmed for the default network.
	if !include(types.DefaultNetworkName) {
		return nil
	}
	predicate := func(item *nbdb.NAT) bool {
		if item.Type != nbdb.NATTypeSNAT {
			return false
		}
		egressIPMetaData, exists := item.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		if !exists {
			return false
		}
		egressIPMeta := strings.Split(egressIPMetaData, dbIDEIPNamePodDivider)
		if len(egressIPMeta) != 2 {
			klog.Errorf("Found NAT %s with erroneous object name key %s", item.UUID, egressIPMetaData)
			return false
		}
		egressIPName := egressIPMeta[0]
		// check logical IP maps to a valid pod
		parsedPodIP := net.ParseIP(item.LogicalIP)
		if parsedPodIP == nil {
			klog.Errorf("syncStaleSNATRules found invalid logical IP for NAT with UID %q", item.UUID)
			return true
		}
		parsedPodIPStr := parsedPodIP.String()
		cacheEntry, exists := egressIPCache.egressIPNameToPods[egressIPName][types.DefaultNetworkName]
		egressPodIPs := sets.NewString()
		if exists {
			// since SNATs can be present either if status.Node is managed by this controller
			// or pods are scheduled on this controller's node, we need to check both
			for _, podIPs := range cacheEntry.egressLocalPods {
				egressPodIPs.Insert(podIPs.UnsortedList()...)
			}
			for _, podIPs := range cacheEntry.egressRemotePods {
				egressPodIPs.Insert(podIPs.UnsortedList()...)
			}
		}
		if !exists || !egressPodIPs.Has(parsedPodIPStr) {
			klog.Infof("syncStaleSNATRules will delete %s due to logical ip: %v", egressIPName, item)
			return true
		}
		ni := e.networkManager.GetNetwork(types.DefaultNetworkName)
		if ni == nil {
			klog.Errorf("syncStaleSNATRules failed to find default network in networks cache")
			return false
		}
		// check external IP maps to a valid EgressIP IP and its assigned to a Node
		node, ok := egressIPCache.egressIPIPToNodeCache[item.ExternalIP]
		if !ok {
			klog.Infof("syncStaleSNATRules found NAT %q without EIP assigned to a Node", item.UUID)
			return true
		}
		// check logical port is set and correspondes to the correct egress node
		if item.LogicalPort == nil || *item.LogicalPort != ni.GetNetworkScopedK8sMgmtIntfName(node) {
			klog.Infof("syncStaleSNATRules found NAT %q with invalid logical port", item.UUID)
			return true
		}
		return false
	}

	nats, err := libovsdbops.FindNATsWithPredicate(e.nbClient, predicate)
	if err != nil {
		return fmt.Errorf("unable to sync egress IPs err: %v", err)
	}

	if len(nats) == 0 {
		// No stale nat entries to deal with: noop.
		return nil
	}

	natIds := sets.Set[string]{}
	for _, nat := range nats {
		natIds.Insert(nat.UUID)
	}
	p := func(item *nbdb.LogicalRouter) bool {
		return natIds.HasAny(item.Nat...)
	}
	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(e.nbClient, p)
	if err != nil {
		return fmt.Errorf("unable to sync egress IPs, err: %v", err)
	}

	var errors []error
	ops := []ovsdb.Operation{}
	for _, router := range routers {
		ops, err = libovsdbops.DeleteNATsOps(e.nbClient, ops, router, nats...)
		if err != nil {
			errors = append(errors, fmt.Errorf("error deleting stale NAT from router %s: %v", router.Name, err))
			continue
		}
	}
	if len(errors) > 0 {
		return utilerrors.Join(errors...)
	}
	// The routers length 0 check is needed because some of ovnkube-controller restart unit tests have
	// router object referring to SNAT's UUID string instead of actual UUID (though it may not
	// happen in real scenario). Hence this check is needed to delete those stale SNATs as well.
	if len(routers) == 0 {
		predicate := func(item *nbdb.NAT) bool {
			return natIds.Has(item.UUID)
		}
		ops, err = libovsdbops.DeleteNATsWithPredicateOps(e.nbClient, ops, predicate)
		if err != nil {
			return fmt.Errorf("unable to delete stale SNATs err: %v", err)
		}
	}

	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	if err != nil {
		return fmt.Errorf("error deleting stale NATs: %v", err)
	}
	return nil
}

// generateCacheForEgressIP builds a cache of egressIP name -> podIPs for fast
// access when syncing egress IPs. The Egress IP setup will return a lot of
// atomic items with the same general information repeated across most (egressIP
// name, logical IP defined for that name), hence use a cache to avoid round
// trips to the API server per item. include limits which networks are walked so
// a per-network stale sync does not depend on other networks' state.
func (e *EgressIPController) generateCacheForEgressIP(include egressIPNetworkFilter) (egressIPCache, error) {
	if include == nil {
		include = egressIPAllNetworks
	}
	cache := egressIPCache{}
	namespaces, err := e.watchFactory.GetNamespaces()
	if err != nil {
		return cache, fmt.Errorf("failed to get all namespaces: %v", err)
	}
	nodes, err := e.watchFactory.GetNodes()
	if err != nil {
		return cache, fmt.Errorf("failed to get all nodes: %v", err)
	}
	_, localNodeObserved := e.nodeState.Load(e.nodeName)
	localNodeName := ""
	if localNodeObserved {
		localNodeName = e.nodeName
	}
	// network name -> node name -> redirect IPs
	redirectCache := map[string]map[string]redirectIPs{}
	cache.egressNodeRedirectsCache = nodeNetworkRedirects{redirectCache}
	cache.networkToRouter = map[string]string{}
	// build a map of networks -> nodes -> redirect IP
	for _, namespace := range namespaces {
		ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
		if err != nil {
			klog.Errorf("Failed to get active network for namespace %s, stale objects may remain: %v", namespace.Name, err)
			continue
		}
		if ni == nil {
			klog.V(5).Infof("Skipping namespace %s while building egress IP cache: network not active for this controller", namespace.Name)
			continue
		}
		if !include(ni.GetNetworkName()) {
			continue
		}
		// skip if already processed
		if _, ok := redirectCache[ni.GetNetworkName()]; ok {
			continue
		}
		redirectCache[ni.GetNetworkName()] = map[string]redirectIPs{}
		routerName, err := getTopologyScopedRouterName(ni, localNodeName)
		if err != nil {
			klog.Errorf("Failed to get network topology scoped router name for network %s attached to namespace %s, stale objects may remain: %v",
				ni.GetNetworkName(), namespace.Name, err)
			continue
		}
		cache.networkToRouter[ni.GetNetworkName()] = routerName
		for _, node := range nodes {
			r := redirectIPs{}
			mgmtPort := &nbdb.LogicalSwitchPort{Name: ni.GetNetworkScopedK8sMgmtIntfName(node.Name)}
			mgmtPort, err := libovsdbops.GetLogicalSwitchPort(e.nbClient, mgmtPort)
			// return if error is anything other than not found to allow retry
			if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
				return cache, fmt.Errorf("failed to find management port for node %s: %v", node.Name, err)
			}
			// if management port is available, gather the data. If it's not available, OVN constructs that depend on a deleted
			// management port IP will fail and be cleaned up in sync LRPs func.
			if mgmtPort != nil {
				mgmtPortAddresses := mgmtPort.GetAddresses()
				if len(mgmtPortAddresses) == 0 {
					return cache, fmt.Errorf("management switch port %s for node %s does not contain any addresses", ni.GetNetworkScopedK8sMgmtIntfName(node.Name), node.Name)
				}
				// Extract at most one IP per family; entries are "MAC IP [IP ...]"
				for _, macPlusIPs := range mgmtPortAddresses {
					parts := strings.Fields(macPlusIPs)
					if len(parts) < 2 {
						continue // no IPs
					}
					for _, ipStr := range parts[1:] {
						ip := net.ParseIP(ipStr)
						if ip == nil {
							continue
						}
						if utilnet.IsIPv6(ip) {
							if r.v6MgtPort == "" && ip.To16() != nil {
								r.v6MgtPort = ip.String()
							}
						} else {
							if r.v4MgtPort == "" && ip.To4() != nil {
								r.v4MgtPort = ip.String()
							}
						}
					}
				}
			}

			if localNodeObserved && node.Name == e.nodeName {
				if e.v4 {
					if gatewayRouterIP, err := e.getGatewayNextHop(ni, node, false); err != nil {
						klog.V(5).Infof("Unable to retrieve gateway IP for node: %s, protocol is IPv4: err: %v", node.Name, err)
					} else {
						r.v4Gateway = gatewayRouterIP.String()
					}
				}
				if e.v6 {
					if gatewayRouterIP, err := e.getGatewayNextHop(ni, node, true); err != nil {
						klog.V(5).Infof("Unable to retrieve gateway IP for node: %s, protocol is IPv6: err: %v", node.Name, err)
					} else {
						r.v6Gateway = gatewayRouterIP.String()
					}
				}
			} else {
				if e.v4 {
					nextHopIP, err := e.getTransitIP(node.Name, false)
					if err != nil {
						klog.V(5).Infof("Unable to fetch transit switch IPv4 for node %s: %v", node.Name, err)
					} else {
						r.v4TransitSwitch = nextHopIP
					}
				}
				if e.v6 {
					nextHopIP, err := e.getTransitIP(node.Name, true)
					if err != nil {
						klog.V(5).Infof("Unable to fetch transit switch IPv6 for node %s: %v", node.Name, err)
					} else {
						r.v6TransitSwitch = nextHopIP
					}
				}
			}
			redirectCache[ni.GetNetworkName()][node.Name] = r
		}
	}

	// egressIP name -> network name -> cache
	egressIPsCache := make(map[string]map[string]selectedPods)
	cache.egressIPNameToPods = egressIPsCache
	// egressIP name -> nodes where the IPs are assigned
	egressIPNameNodesCache := make(map[string][]string, 0)
	cache.egressIPNameToAssignedNodes = egressIPNameNodesCache
	// egressIP IP -> node name. Assigned node for EIP.
	egressIPIPNodeCache := make(map[string]string, 0)
	cache.egressIPIPToNodeCache = egressIPIPNodeCache
	// egressIP name -> egressIP IP -> node name.
	egressIPToAssignedNodes := make(map[string]map[string]string, 0)
	cache.egressIPToAssignedNodes = egressIPToAssignedNodes
	cache.markCache = make(map[string]string)
	egressIPs, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return cache, err
	}
	for _, egressIP := range egressIPs {
		mark, err := util.ParseEgressIPMark(egressIP.Annotations)
		if err != nil {
			klog.Errorf("Failed to parse EgressIP %s mark: %v", egressIP.Name, err)
		}
		cache.markCache[egressIP.Name] = mark.String()
		egressIPsCache[egressIP.Name] = make(map[string]selectedPods, 0)
		egressIPNameNodesCache[egressIP.Name] = make([]string, 0, len(egressIP.Status.Items))
		egressIPToAssignedNodes[egressIP.Name] = make(map[string]string, 0)
		for _, status := range egressIP.Status.Items {
			eipIP := net.ParseIP(status.EgressIP)
			if eipIP == nil {
				klog.Errorf("Failed to parse EgressIP %s IP %q from status", egressIP.Name, status.EgressIP)
				continue
			}
			egressIPIPNodeCache[eipIP.String()] = status.Node
			egressIPToAssignedNodes[egressIP.Name][eipIP.String()] = status.Node
			if localNodeObserved && status.Node == e.nodeName {
				cache.egressLocalNode = status.Node
			}
			egressIPNameNodesCache[egressIP.Name] = append(egressIPNameNodesCache[egressIP.Name], status.Node)
		}
		namespaces, err = e.watchFactory.GetNamespacesBySelector(egressIP.Spec.NamespaceSelector)
		if err != nil {
			klog.Errorf("Error building egress IP sync cache, cannot retrieve namespaces for EgressIP: %s, err: %v", egressIP.Name, err)
			continue
		}
		for _, namespace := range namespaces {
			pods, err := e.watchFactory.GetPodsBySelector(namespace.Name, egressIP.Spec.PodSelector)
			if err != nil {
				klog.Errorf("Error building egress IP sync cache, cannot retrieve pods for namespace: %s and egress IP: %s, err: %v", namespace.Name, egressIP.Name, err)
				continue
			}
			ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
			if err != nil {
				klog.Errorf("Failed to get active network for namespace %s, skipping sync: %v", namespace.Name, err)
				continue
			}
			if ni == nil {
				klog.V(5).Infof("Skipping namespace %s while building egress IP sync cache: network not active for this controller", namespace.Name)
				continue
			}
			if !include(ni.GetNetworkName()) {
				continue
			}
			_, ok := egressIPsCache[egressIP.Name][ni.GetNetworkName()]
			if ok {
				continue // aready populated
			}
			egressIPsCache[egressIP.Name][ni.GetNetworkName()] = selectedPods{
				egressLocalPods:  map[string]sets.Set[string]{},
				egressRemotePods: map[string]sets.Set[string]{},
			}
			for _, pod := range pods {
				if !util.PodNeedsSNAT(pod) {
					continue
				}
				if cache.egressLocalNode == "" && !e.isPodScheduledOnLocalNode(pod) {
					continue // don't process anything on controllers that have nothing to do with the pod
				}
				nadKey, err := e.getPodNADKeyForNetwork(ni, pod)
				if err != nil {
					klog.Errorf("Network %s: failed to resolve NAD for pod %s/%s: %v", ni.GetNetworkName(), pod.Namespace, pod.Name, err)
					continue
				}
				podIPs, err := e.getPodIPs(ni, pod, nadKey)
				if err != nil {
					klog.Errorf("Network %s: error build egress IP sync cache, error while trying to get pod %s/%s IPs: %v", ni.GetNetworkName(), pod.Namespace, pod.Name, err)
					continue
				}
				if len(podIPs) == 0 {
					continue
				}
				podKey := getPodKey(pod)
				if e.isPodScheduledOnLocalNode(pod) {
					//
					_, ok := egressIPsCache[egressIP.Name][ni.GetNetworkName()].egressLocalPods[podKey]
					if !ok {
						egressIPsCache[egressIP.Name][ni.GetNetworkName()].egressLocalPods[podKey] = sets.New[string]()
					}
					for _, ipNet := range podIPs {
						egressIPsCache[egressIP.Name][ni.GetNetworkName()].egressLocalPods[podKey].Insert(ipNet.IP.String())
					}
				} else if cache.egressLocalNode != "" {
					// it means this controller owns at least one egress node, but the matched pod is remote
					_, ok := egressIPsCache[egressIP.Name][ni.GetNetworkName()].egressRemotePods[podKey]
					if !ok {
						egressIPsCache[egressIP.Name][ni.GetNetworkName()].egressRemotePods[podKey] = sets.New[string]()
					}
					for _, ipNet := range podIPs {
						egressIPsCache[egressIP.Name][ni.GetNetworkName()].egressRemotePods[podKey].Insert(ipNet.IP.String())
					}
				}
			}
		}
	}

	return cache, nil
}

type EgressIPPatchStatus struct {
	Op    string                    `json:"op"`
	Path  string                    `json:"path"`
	Value egressipv1.EgressIPStatus `json:"value"`
}

// patchReplaceEgressIPStatus performs a replace patch operation of the egress
// IP status by replacing the status with the provided value. This allows us to
// update only the status field, without overwriting any other. This is
// important because processing egress IPs can take a while (when running on a
// public cloud and in the worst case), hence we don't want to perform a full
// object update which risks resetting the EgressIP object's fields to the state
// they had when we started processing the change.
// used for UNIT TESTING only
func (e *EgressIPController) patchReplaceEgressIPStatus(name string, statusItems []egressipv1.EgressIPStatusItem) error {
	klog.Infof("Patching status on EgressIP %s: %v", name, statusItems)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		t := []EgressIPPatchStatus{
			{
				Op:   "replace",
				Path: "/status",
				Value: egressipv1.EgressIPStatus{
					Items: statusItems,
				},
			},
		}
		op, err := json.Marshal(&t)
		if err != nil {
			return fmt.Errorf("error serializing status patch operation: %+v, err: %v", statusItems, err)
		}
		return e.kube.PatchEgressIP(name, op)
	})
}

func (e *EgressIPController) addEgressNode(node *corev1.Node) error {
	if node == nil {
		return nil
	}
	if node.Name == e.nodeName {
		klog.V(5).Infof("Egress node: %s about to be initialized", node.Name)
		// When the node gets deleted we do not remove this route intentionally because
		// if the node is gone, then the ovn_cluster_router is also gone along with all
		// the routes on it.
		ni := e.networkManager.GetNetwork(types.DefaultNetworkName)
		gatewayIPs, err := udn.GetGWRouterIPs(node, &util.DefaultNetInfo{})
		if err != nil {
			return fmt.Errorf("failed to get default network gateway router join IPs for node %q: %w", node.Name, err)
		}
		if err := libovsdbutil.CreateDefaultRouteToExternal(e.nbClient, ni.GetNetworkScopedClusterRouterName(),
			ni.GetNetworkScopedGWRouterName(node.Name), ni.Subnets(), gatewayIPs); err != nil {
			return fmt.Errorf("failed to create route to external for network %s: %v", ni.GetNetworkName(), err)
		}
	}
	return nil
}

// initClusterEgressPolicies will initialize the default allow policies for
// east<->west traffic. Egress IP is based on routing egress traffic to specific
// egress nodes, we don't want to route any other traffic however and these
// policies will take care of that. Also, we need to initialize the go-routine
// which verifies if egress nodes are ready and reachable, typically if an
// egress node experiences problems we want to move all egress IP assignment
// away from that node elsewhere so that the pods using the egress IP can
// continue to do so without any issues.
func (e *EgressIPController) initClusterEgressPolicies(_ []interface{}) error {
	// Init default network
	defaultNetInfo := e.networkManager.GetNetwork(types.DefaultNetworkName)
	localNodeName, err := e.getLocalNodeName()
	if err != nil {
		klog.Warningf("Failed to get the local node name: %v", err)
	}
	subnets := util.GetAllClusterSubnetsFromEntries(defaultNetInfo.Subnets())
	clusterNodeIPsAddrSetDbIDs, err := e.addressSetManager.EnsureClusterNodeIPsAddressSet(addresssetmanager.ClusterNodeIPsEgressIPBackRef)
	if err != nil {
		return fmt.Errorf("failed to ensure cluster node IP address set for EgressIP: %w", err)
	}
	if err := InitClusterEgressPolicies(e.nbClient, e.addressSetFactory, defaultNetInfo, subnets, e.controllerName, defaultNetInfo.GetNetworkScopedClusterRouterName(), clusterNodeIPsAddrSetDbIDs); err != nil {
		return fmt.Errorf("failed to initialize networks cluster logical router egress policies for the default network: %v", err)
	}

	return e.networkManager.DoWithLock(func(network util.NetInfo) error {
		if network.GetNetworkName() == types.DefaultNetworkName {
			return nil
		}
		subnets = util.GetAllClusterSubnetsFromEntries(network.Subnets())
		if len(subnets) == 0 {
			return nil
		}
		routerName, err := getTopologyScopedRouterName(network, localNodeName)
		if err != nil {
			return err
		}
		if err = InitClusterEgressPolicies(e.nbClient, e.addressSetFactory, network, subnets, e.controllerName, routerName, clusterNodeIPsAddrSetDbIDs); err != nil {
			return fmt.Errorf("failed to initialize networks cluster logical router egress policies for network %s: %v", network.GetNetworkName(), err)
		}
		return nil
	})
}

// InitClusterEgressPolicies creates the global no reroute policies and address-sets
// required by the egressIP and egressServices features.
func InitClusterEgressPolicies(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory, ni util.NetInfo,
	clusterSubnets []*net.IPNet, controllerName, routerName string, clusterNodeIPsAddrSetDbIDs *libovsdbops.DbObjectIDs) error {
	if len(clusterSubnets) == 0 {
		return nil
	}
	var v4ClusterSubnet, v6ClusterSubnet []*net.IPNet
	for _, subnet := range clusterSubnets {
		if utilnet.IsIPv6CIDR(subnet) {
			v6ClusterSubnet = append(v6ClusterSubnet, subnet)
		} else {
			v4ClusterSubnet = append(v4ClusterSubnet, subnet)
		}
	}
	var v4JoinSubnet, v6JoinSubnet *net.IPNet
	var err error
	if len(v4ClusterSubnet) > 0 {
		if config.Gateway.V4JoinSubnet == "" {
			return fmt.Errorf("network %s: cannot process IPv4 addresses because no IPv4 join subnet is available", ni.GetNetworkName())
		}
		_, v4JoinSubnet, err = net.ParseCIDR(config.Gateway.V4JoinSubnet)
		if err != nil {
			return fmt.Errorf("network %s: failed to parse IPv4 join subnet: %v", ni.GetNetworkName(), err)
		}
	}
	if len(v6ClusterSubnet) > 0 {
		if config.Gateway.V6JoinSubnet == "" {
			return fmt.Errorf("network %s: cannot process IPv6 addresses because no IPv6 join subnet is available", ni.GetNetworkName())
		}
		_, v6JoinSubnet, err = net.ParseCIDR(config.Gateway.V6JoinSubnet)
		if err != nil {
			return fmt.Errorf("network %s: failed to parse IPv6 join subnet: %v", ni.GetNetworkName(), err)
		}
	}
	if err = createDefaultNoReroutePodPolicies(nbClient, ni.GetNetworkName(), controllerName, routerName, v4ClusterSubnet, v6ClusterSubnet); err != nil {
		return fmt.Errorf("failed to create no reroute policies for pods on network %s: %v", ni.GetNetworkName(), err)
	}
	if err = createDefaultNoRerouteServicePolicies(nbClient, ni.GetNetworkName(), controllerName, routerName, v4ClusterSubnet, v6ClusterSubnet,
		v4JoinSubnet, v6JoinSubnet); err != nil {
		return fmt.Errorf("failed to create no reroute policies for services on network %s: %v", ni.GetNetworkName(), err)
	}
	if err = createDefaultNoRerouteReplyTrafficPolicy(nbClient, ni.GetNetworkName(), controllerName, routerName); err != nil {
		return fmt.Errorf("failed to create no reroute reply traffic policy for network %s: %v", ni.GetNetworkName(), err)
	}

	// ensure the address-set for storing nodeIPs exists
	// The address set with controller name 'default' is shared with all networks
	if clusterNodeIPsAddrSetDbIDs == nil {
		return fmt.Errorf("cluster node IP address set DB IDs are required")
	}
	if _, err = addressSetFactory.EnsureAddressSet(clusterNodeIPsAddrSetDbIDs); err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", NodeIPAddrSetName, err)
	}

	// ensure the address-set for storing egressIP pods exists
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, ni.GetNetworkName(), controllerName)
	_, err = addressSetFactory.EnsureAddressSet(dbIDs)
	if err != nil {
		return fmt.Errorf("cannot ensure that addressSet for egressIP pods %s exists for network %s: %v", EgressIPServedPodsAddrSetName, ni.GetNetworkName(), err)
	}

	// ensure the address-set for storing egressservice pod backends exists
	dbIDs = egresssvc.GetEgressServiceAddrSetDbIDs(controllerName)
	_, err = addressSetFactory.EnsureAddressSet(dbIDs)
	if err != nil {
		return fmt.Errorf("cannot ensure that addressSet for egressService pods %s exists %v", egresssvc.EgressServiceServedPodsAddrSetName, err)
	}

	if !ni.IsDefault() && isEgressIPForUDNSupported() {
		v4, v6 := len(v4ClusterSubnet) > 0, len(v6ClusterSubnet) > 0
		if err = ensureDefaultNoRerouteUDNEnabledSvcPolicies(nbClient, addressSetFactory, ni, controllerName, routerName, v4, v6); err != nil {
			return fmt.Errorf("failed to ensure no reroute for UDN enabled services for network %s: %v", ni.GetNetworkName(), err)
		}
	}
	return nil
}

// egressStatusStatePending marks entries populated during EIP sync and
// indicates they must be reconciled again for the pod.
const egressStatusStatePending = "pending"

type statusMap map[egressipv1.EgressIPStatusItem]string

type egressStatuses struct {
	// statusMap tracks per EIP status assignment for a pod.
	// Key: egressipv1.EgressIPStatusItem {EgressIP, Node}
	// Values:
	//   ""                      -> applied/reconciled
	//   egressStatusStatePending -> populated during EIP sync, pending reconcile.
	statusMap
}

func (e egressStatuses) contains(potentialStatus egressipv1.EgressIPStatusItem) bool {
	// handle the case where the all of the status fields are populated
	if _, exists := e.statusMap[potentialStatus]; exists {
		return true
	}
	return false
}

// hasStaleEIPStatus checks for stale EIP status entries already in cache.
// This addresses the race condition where an EIP is reassigned to a different node
// but the cache still contains the old assignment, leading to stale SNAT/LRP entries.
func (e egressStatuses) hasStaleEIPStatus(potentialStatus egressipv1.EgressIPStatusItem) *egressipv1.EgressIPStatusItem {
	var staleStatus *egressipv1.EgressIPStatusItem
	for status := range e.statusMap {
		if status.EgressIP == potentialStatus.EgressIP &&
			status.Node != potentialStatus.Node {
			staleStatus = &egressipv1.EgressIPStatusItem{EgressIP: status.EgressIP, Node: status.Node}
			break
		}
	}
	return staleStatus
}

func (e egressStatuses) delete(deleteStatus egressipv1.EgressIPStatusItem) {
	delete(e.statusMap, deleteStatus)
}

// podAssignmentState keeps track of which egressIP object is serving
// the related pod.
// NOTE: At a given time only one object will be configured. This is
// transparent to the user
type podAssignmentState struct {
	// the name of the egressIP object that is currently serving this pod
	egressIPName string
	// the list of egressIPs within the above egressIP object that are serving this pod

	egressStatuses

	// list of other egressIP object names that also match this pod but are on standby
	standbyEgressIPNames sets.Set[string]

	podIPs []net.IP

	// network attached to the pod
	network util.NetInfo
}

// Clone deep-copies and returns the copied podAssignmentState
func (pas *podAssignmentState) Clone() *podAssignmentState {
	clone := &podAssignmentState{
		egressIPName:         pas.egressIPName,
		standbyEgressIPNames: pas.standbyEgressIPNames.Clone(),
		podIPs:               make([]net.IP, 0, len(pas.podIPs)),
		network:              pas.network,
	}
	clone.egressStatuses = egressStatuses{make(map[egressipv1.EgressIPStatusItem]string, len(pas.egressStatuses.statusMap))}
	for k, v := range pas.statusMap {
		clone.statusMap[k] = v
	}
	clone.podIPs = append(clone.podIPs, pas.podIPs...)
	return clone
}

// addStandByEgressIPAssignment does the same setup that is done by addPodEgressIPAssignments but for
// the standby egressIP. This must always be called with a lock on podAssignmentState mutex
// This is special case function called only from deleteEgressIPAssignments, don't use this for normal setup
// Any failure from here will not be retried, it's a corner case undefined behaviour
// must be called with podAssignment lock on podKey
func (e *EgressIPController) addStandByEgressIPAssignment(ni util.NetInfo, podKey string, podStatus *podAssignmentState) error {
	podNamespace, podName := getPodNamespaceAndNameFromKey(podKey)
	pod, err := e.watchFactory.GetPod(podNamespace, podName)
	if err != nil {
		return err
	}
	eipsToAssign := podStatus.standbyEgressIPNames.UnsortedList()
	var eipToAssign string
	var eip *egressipv1.EgressIP
	var mark util.EgressIPMark
	for _, eipName := range eipsToAssign {
		eip, err = e.watchFactory.GetEgressIP(eipName)
		if err != nil {
			klog.Warningf("There seems to be a stale standby egressIP %s for pod %s "+
				"which doesn't exist: %v; removing this standby egressIP from cache...", eipName, podKey, err)
			podStatus.standbyEgressIPNames.Delete(eipName)
			continue
		}
		eipToAssign = eipName // use the first EIP we find successfully
		if ni.IsUserDefinedNetwork() {
			mark = getEgressIPPktMark(eip.Name, eip.Annotations)
		}
		break
	}
	if eipToAssign == "" {
		klog.Infof("No standby egressIP's found for pod %s", podKey)
		return nil
	}
	// get IPs
	nadKey, err := e.getPodNADKeyForNetwork(ni, pod)
	if err != nil {
		return err
	}
	podIPNets, err := e.getPodIPs(ni, pod, nadKey)
	if err != nil {
		return fmt.Errorf("failed to get pod %s/%s IPs using NAD key %q: %v", pod.Namespace, pod.Name, nadKey, err)
	}
	if len(podIPNets) == 0 {
		return fmt.Errorf("no IP(s) available for pod %s/%s on network %s", pod.Namespace, pod.Name, ni.GetNetworkName())
	}
	podIPs := make([]net.IP, 0, len(podIPNets))
	for _, podIPNet := range podIPNets {
		podIPs = append(podIPs, podIPNet.IP)
	}
	podState := &podAssignmentState{
		egressStatuses:       egressStatuses{make(map[egressipv1.EgressIPStatusItem]string)},
		standbyEgressIPNames: podStatus.standbyEgressIPNames,
		podIPs:               podIPs,
		network:              ni,
	}
	e.podAssignment.Store(podKey, podState)
	// NOTE: We let addPodEgressIPAssignments take care of setting egressIPName and egressStatuses and removing it from standBy
	err = e.addPodEgressIPAssignments(ni, eipToAssign, eip.Status.Items, mark, pod)
	if err != nil {
		return fmt.Errorf("failed to add standby pod %s/%s for network %s: %v", pod.Namespace, pod.Name, ni.GetNetworkName(), err)
	}
	return nil
}

// addPodEgressIPAssignment will program OVN with logical router policies
// (routing pod traffic to the egress node) and NAT objects on the egress node
// (SNAT-ing to the egress IP).
// This function should be called with lock on nodeState cache key status.Node and pod.Spec.NodeName
func (e *EgressIPController) addPodEgressIPAssignment(ni util.NetInfo, egressIPName string, status egressipv1.EgressIPStatusItem, mark util.EgressIPMark,
	pod *corev1.Pod, podIPs []*net.IPNet) (err error) {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			if err != nil {
				return
			}
			duration := time.Since(start)
			metrics.RecordEgressIPAssign(duration)
		}()
	}
	eIPIP := net.ParseIP(status.EgressIP)
	isLocalEgressNode, loadedEgressNode := e.getNodeLocality(status.Node)
	isLocalPod, loadedPodNode := e.getNodeLocality(pod.Spec.NodeName)
	eNode, err := e.watchFactory.GetNode(status.Node)
	if err != nil {
		return fmt.Errorf("failed to add pod %s/%s because failed to lookup node %s: %v", pod.Namespace, pod.Name,
			pod.Spec.NodeName, err)
	}
	parsedNodeEIPConfig, err := util.GetNodeEIPConfig(eNode)
	if err != nil {
		return fmt.Errorf("failed to get node %s egress IP config: %w", eNode.Name, err)
	}
	isOVNNetwork := util.IsOVNNetwork(parsedNodeEIPConfig, eIPIP)
	nextHopIP, err := e.getNextHop(ni, status.Node, status.EgressIP, egressIPName, isLocalEgressNode, isOVNNetwork)
	if err != nil {
		return fmt.Errorf("failed to determine next hop for pod %s/%s when configuring egress IP %s"+
			" IP %s: %v", pod.Namespace, pod.Name, egressIPName, status.EgressIP, err)
	}
	if nextHopIP == "" {
		return fmt.Errorf("could not calculate the next hop for pod %s/%s when configuring egress IP %s"+
			" IP %s", pod.Namespace, pod.Name, egressIPName, status.EgressIP)
	}
	policyOpsMark := ""
	if ni.IsUserDefinedNetwork() {
		if !mark.IsAvailable() {
			return fmt.Errorf("egressIP %s object must contain a mark for user defined networks", egressIPName)
		}
		policyOpsMark = mark.String()
	}
	// For default network with secondary host interface, set egress ip secondary interface mark in reroute policy
	if ni.IsDefault() && !isOVNNetwork && isLocalEgressNode {
		policyOpsMark = types.EgressIPSecondaryInterfaceMark
	}

	var ops []ovsdb.Operation
	if loadedEgressNode && isLocalEgressNode {
		// create NATs for CDNs only
		// create LRPs with allow action (aka GW policy marks) only for L3 UDNs.
		// L2 UDNs require LRPs with reroute action with a pkt_mark option attached to GW router.
		if isOVNNetwork {
			if ni.IsDefault() {
				ops, err = e.createNATRuleOps(ni, nil, podIPs, status, egressIPName, pod.Namespace, pod.Name)
				if err != nil {
					return fmt.Errorf("unable to create NAT rule ops for status: %v, err: %v", status, err)
				}

			} else if ni.IsUserDefinedNetwork() && (ni.TopologyType() == types.Layer3Topology ||
				ni.TopologyType() == types.Layer2Topology && config.Layer2UsesTransitRouter) {
				ops, err = e.createGWMarkPolicyOps(ni, ops, podIPs, status, policyOpsMark, pod.Namespace, pod.Name, egressIPName)
				if err != nil {
					return fmt.Errorf("unable to create GW router LRP ops to packet mark pod %s/%s: %v", pod.Namespace, pod.Name, err)
				}
			}
		}
		if ni.IsDefault() && !isOVNNetwork && (loadedPodNode && !isLocalPod) {
			// For CDNs, configure LRP with reroute action for remote pods on egress nodes to support redirect to local management port
			// when the egress IP is assigned to a host secondary interface
			routerName, err := getTopologyScopedRouterName(ni, pod.Spec.NodeName)
			if err != nil {
				return err
			}
			ops, err = e.createReroutePolicyOps(ni, ops, podIPs, status, policyOpsMark, egressIPName, nextHopIP, routerName, pod.Namespace, pod.Name)
			if err != nil {
				return fmt.Errorf("unable to create logical router policy ops %v, err: %v", status, err)
			}
		}
	}

	// For L2, we always attach an LRP with reroute action to the node's gateway router. If the pod is remote, use the local node name to generate the GW router name.
	nodeName := pod.Spec.NodeName
	if loadedEgressNode && loadedPodNode && !isLocalPod && isLocalEgressNode && ni.IsUserDefinedNetwork() &&
		ni.TopologyType() == types.Layer2Topology && !config.Layer2UsesTransitRouter {
		nodeName = status.Node
	}
	routerName, err := getTopologyScopedRouterName(ni, nodeName)
	if err != nil {
		return err
	}

	// exec when node is local OR when pods are local or L2 UDN
	// don't add a reroute policy if the egress node towards which we are adding this doesn't exist
	if loadedEgressNode && loadedPodNode {
		if isLocalPod || (isLocalEgressNode && ni.IsUserDefinedNetwork() && ni.TopologyType() == types.Layer2Topology) {
			ops, err = e.createReroutePolicyOps(ni, ops, podIPs, status, policyOpsMark, egressIPName, nextHopIP, routerName, pod.Namespace, pod.Name)
			if err != nil {
				return fmt.Errorf("unable to create logical router policy ops, err: %v", err)
			}
		}
		if isLocalPod {
			ops, err = e.deleteExternalGWPodSNATOps(ni, ops, pod, podIPs, status, isOVNNetwork)
			if err != nil {
				return err
			}
		}
	}
	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	return err
}

// deletePodEgressIPAssignment deletes the OVN programmed egress IP
// configuration mentioned for addPodEgressIPAssignment.
// This function should be called with lock on nodeState cache key status.Node and pod.Spec.NodeName
func (e *EgressIPController) deletePodEgressIPAssignment(ni util.NetInfo, egressIPName string, status egressipv1.EgressIPStatusItem, pod *corev1.Pod) (err error) {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			if err != nil {
				return
			}
			duration := time.Since(start)
			metrics.RecordEgressIPUnassign(duration)
		}()
	}
	isLocalEgressNode, loadedEgressNode := e.getNodeLocality(status.Node)
	isLocalPod, loadedPodNode := e.getNodeLocality(pod.Spec.NodeName)
	var nextHopIP string
	var isOVNNetwork bool
	// node may not exist - attempt to retrieve it
	eNode, err := e.watchFactory.GetNode(status.Node)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete pod %s/%s egress IP config because failed to lookup node %s obj: %v", pod.Namespace, pod.Name,
			pod.Spec.NodeName, err)
	}
	if err == nil {
		eIPIP := net.ParseIP(status.EgressIP)
		parsedEIPConfig, err := util.GetNodeEIPConfig(eNode)
		if err != nil {
			klog.Warningf("Unable to get node %s egress IP config: %v", eNode.Name, err)
		} else {
			isOVNNetwork = util.IsOVNNetwork(parsedEIPConfig, eIPIP)
			nextHopIP, err = e.getNextHop(ni, status.Node, status.EgressIP, egressIPName, isLocalEgressNode, isOVNNetwork)
			if err != nil {
				klog.Warningf("Unable to determine next hop for egress IP %s IP %s assigned to node %s: %v", egressIPName,
					status.EgressIP, status.Node, err)
			}
		}
	}
	// For L2, we always attach an LRP with reroute action to the node's gateway router. If the pod is remote, use the local node name to generate the GW router name.
	nodeName := pod.Spec.NodeName
	if !isLocalPod && isLocalEgressNode && ni.IsUserDefinedNetwork() &&
		ni.TopologyType() == types.Layer2Topology && !config.Layer2UsesTransitRouter {
		nodeName = status.Node
	}
	routerName, err := getTopologyScopedRouterName(ni, nodeName)
	if err != nil {
		return err
	}
	var ops []ovsdb.Operation
	// For CDN only, add SNATs to support external GW feature
	if ni.IsDefault() && (!loadedPodNode || isLocalPod) {
		ops, err = e.addExternalGWPodSNATOps(ni, nil, pod.Namespace, pod.Name, status)
		if err != nil {
			return err
		}
	}
	// Following cases will ensure removal of a pod LRP
	// Case 1 - node where pod is hosted is not known
	// Case 2 - pod is on the local node
	// Case 3 - the local node is the egress node and the pod is attached to layer 2. For layer 2, there is always an LRP attached to the egress node GW router.
	if !loadedPodNode || isLocalPod || (isLocalEgressNode && ni.IsUserDefinedNetwork() &&
		ni.TopologyType() == types.Layer2Topology) {
		ops, err = e.deleteReroutePolicyOps(ni, ops, status, egressIPName, nextHopIP, routerName, pod.Namespace, pod.Name)
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			// if the gateway router join IP setup is already gone, then don't count it as error.
			klog.Warningf("Unable to delete logical router policy, err: %v", err)
		} else if err != nil {
			return fmt.Errorf("unable to delete logical router policy, err: %v", err)
		}
	}

	if loadedEgressNode && isLocalEgressNode {
		if ni.IsDefault() && !isOVNNetwork && (!loadedPodNode || !isLocalPod) { // node is deleted (we can't determine locality so we always try and nuke OR pod is remote)
			// For CDNs, delete reroute for remote pods on egress nodes when the egress IP is assigned to a secondary host interface
			ops, err = e.deleteReroutePolicyOps(ni, ops, status, egressIPName, nextHopIP, routerName, pod.Namespace, pod.Name)
			if err != nil {
				return fmt.Errorf("unable to delete logical router static route ops %v, err: %v", status, err)
			}
		}
		if ni.IsDefault() {
			ops, err = e.deleteNATRuleOps(ni, ops, status, egressIPName, pod.Namespace, pod.Name)
			if err != nil {
				return fmt.Errorf("unable to delete NAT rule for status: %v, err: %v", status, err)
			}
		} else if ni.IsUserDefinedNetwork() && ni.TopologyType() == types.Layer3Topology ||
			ni.TopologyType() == types.Layer2Topology && config.Layer2UsesTransitRouter {
			ops, err = e.deleteGWMarkPolicyOps(ni, ops, status, pod.Namespace, pod.Name, egressIPName)
			if err != nil {
				return fmt.Errorf("unable to create GW router packet mark LRPs delete ops for pod %s/%s: %v", pod.Namespace, pod.Name, err)
			}
		}
	}
	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	return err
}

// addExternalGWPodSNAT performs the required external GW setup in two particular
// cases:
// - An egress IP matching pod stops matching by means of EgressIP object
// deletion
// - An egress IP matching pod stops matching by means of changed EgressIP
// selector change.
// In both cases we should re-add the external GW setup. We however need to
// guard against a third case, which is: pod deletion, for that it's enough to
// check the informer cache since on pod deletion the event handlers are
// triggered after the update to the informer cache. We should not re-add the
// external GW setup in those cases.
func (e *EgressIPController) addExternalGWPodSNAT(ni util.NetInfo, podNamespace, podName string, status egressipv1.EgressIPStatusItem) error {
	ops, err := e.addExternalGWPodSNATOps(ni, nil, podNamespace, podName, status)
	if err != nil {
		return fmt.Errorf("error creating ops for adding external gw pod snat: %+v", err)
	}
	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	if err != nil {
		return fmt.Errorf("error trasnsacting ops %+v: %v", ops, err)
	}
	return nil
}

// addExternalGWPodSNATOps returns ovsdb ops that perform the required external GW setup in two particular
// cases:
// - An egress IP matching pod stops matching by means of EgressIP object
// deletion
// - An egress IP matching pod stops matching by means of changed EgressIP
// selector change.
// In both cases we should re-add the external GW setup. We however need to
// guard against a third case, which is: pod deletion, for that it's enough to
// check the informer cache since on pod deletion the event handlers are
// triggered after the update to the informer cache. We should not re-add the
// external GW setup in those cases.
// This function should be called with the nodeState cache key pod.Spec.NodeName locked.
func (e *EgressIPController) addExternalGWPodSNATOps(ni util.NetInfo, ops []ovsdb.Operation, podNamespace, podName string, status egressipv1.EgressIPStatusItem) ([]ovsdb.Operation, error) {
	if config.Gateway.DisableSNATMultipleGWs {
		pod, err := e.watchFactory.GetPod(podNamespace, podName)
		if err != nil {
			return nil, nil // nothing to do.
		}

		if util.IsPodNetworkAdvertisedAtNode(ni, pod.Spec.NodeName) {
			// network is advertised so don't setup the SNAT
			return ops, nil
		}

		isLocalPod, loadedPodNode := e.getNodeLocality(pod.Spec.NodeName)
		if pod.Spec.NodeName == status.Node && loadedPodNode && isLocalPod && util.PodNeedsSNAT(pod) {
			// if the pod still exists, add snats to->nodeIP (on the node where the pod exists) for these podIPs after deleting the snat to->egressIP
			// NOTE: This needs to be done only if the pod was on the same node as egressNode
			extIPs, err := getExternalIPsGR(e.watchFactory, pod.Spec.NodeName)
			if err != nil {
				return nil, err
			}
			podIPs, err := util.GetPodCIDRsWithFullMask(pod, &util.DefaultNetInfo{}, nil)
			if err != nil {
				return nil, err
			}

			isNetworkAdvertised := util.IsPodNetworkAdvertisedAtNode(ni, pod.Spec.NodeName)
			var clusterNodeIPsAddrSetDbIDs *libovsdbops.DbObjectIDs
			if isNetworkAdvertised && !util.IsNoOverlaySNATExemptionNeeded(ni) {
				clusterNodeIPsAddrSetDbIDs, err = e.addressSetManager.EnsureClusterNodeIPsAddressSet(addresssetmanager.ClusterNodeIPsEgressIPBackRef)
				if err != nil {
					return nil, fmt.Errorf("failed to ensure cluster node IP address set for EgressIP: %w", err)
				}
			}
			// Handle each pod IP individually since each IP family needs its own SNAT match
			for _, podIP := range podIPs {
				ipFamily := utilnet.IPv4
				if utilnet.IsIPv6CIDR(podIP) {
					ipFamily = utilnet.IPv6
				}
				snatMatch, err := GetNetworkScopedClusterSubnetSNATMatch(e.nbClient, ni, pod.Spec.NodeName, isNetworkAdvertised, ipFamily, clusterNodeIPsAddrSetDbIDs)
				if err != nil {
					return nil, fmt.Errorf("failed to get SNAT match for node %s for network %s: %w", pod.Spec.NodeName, ni.GetNetworkName(), err)
				}
				ops, err = addOrUpdatePodSNATOps(e.nbClient, ni.GetNetworkScopedGWRouterName(pod.Spec.NodeName), extIPs, []*net.IPNet{podIP}, snatMatch, "", ops)
				if err != nil {
					return nil, err
				}
			}
			klog.V(5).Infof("Adding SNAT on %s since egress node managing %s/%s was the same: %s", pod.Spec.NodeName, pod.Namespace, pod.Name, status.Node)
		}
	}
	return ops, nil
}

// deleteExternalGWPodSNATOps creates ops for the required external GW teardown for the given pod
func (e *EgressIPController) deleteExternalGWPodSNATOps(ni util.NetInfo, ops []ovsdb.Operation, pod *corev1.Pod, podIPs []*net.IPNet, status egressipv1.EgressIPStatusItem, isOVNNetwork bool) ([]ovsdb.Operation, error) {
	if config.Gateway.DisableSNATMultipleGWs && status.Node == pod.Spec.NodeName && isOVNNetwork {
		affectedIPs := util.MatchAllIPNetFamily(utilnet.IsIPv6String(status.EgressIP), podIPs)
		if len(affectedIPs) == 0 {
			return nil, nil // noting to do.
		}
		// remove snats to->nodeIP (from the node where pod exists if that node is also serving
		// as an egress node for this pod) for these podIPs before adding the snat to->egressIP
		extIPs, err := getExternalIPsGR(e.watchFactory, pod.Spec.NodeName)
		if err != nil {
			return nil, err
		}
		ops, err = deletePodSNATOps(e.nbClient, ops, ni.GetNetworkScopedGWRouterName(pod.Spec.NodeName), extIPs, affectedIPs)
		if err != nil {
			return nil, err
		}
	} else if config.Gateway.DisableSNATMultipleGWs {
		// it means the pod host is different from the egressNode that is managing the pod
		klog.V(5).Infof("Not deleting SNAT on %s since egress node managing %s/%s is %s or Egress IP is not SNAT'd by OVN", pod.Spec.NodeName, pod.Namespace, pod.Name, status.Node)
	}
	return ops, nil
}

// getGatewayNextHop determines the next hop for a given Node considering the network topology type
// For layer 3, next hop is gateway routers 'router to join' port IP
// For layer 2 with transit router, next hop is the node's transit GR IP.
// For layer 2 without transit router, it's the callers responsibility to ensure that the egress node is remote because a LRP should not be created
func (e *EgressIPController) getGatewayNextHop(ni util.NetInfo, node *corev1.Node, isIPv6 bool) (net.IP, error) {
	if ni.TopologyType() == types.Layer3Topology {
		return e.getRouterPortIP(types.GWRouterToJoinSwitchPrefix+ni.GetNetworkScopedGWRouterName(node.Name), isIPv6)
	} else if ni.TopologyType() == types.Layer2Topology {
		if config.Layer2UsesTransitRouter {
			transitRouterInfo, err := getTransitRouterInfo(ni, node)
			if err != nil {
				return nil, err
			}
			nodeTransitIP, err := util.MatchFirstIPNetFamily(isIPv6, transitRouterInfo.gatewayRouterNets)
			if err != nil {
				return nil, fmt.Errorf("could not find transit router IP of node %v for this family %v: %v", node, isIPv6, err)
			}
			return nodeTransitIP.IP, nil
		}
		// If egress node is local, retrieve the external default gateway next hops from the Node L3 gateway annotation.
		// We must pick one of the next hops to add to the LRP reroute next hops to not break ECMP.
		// If an egress node is remote, retrieve the remote Nodes gateway router 'router to switch' port IP
		// from the Node annotation.
		// FIXME: remove gathering the required information from a Node annotations as this approach does not scale
		// FIXME: we do not respect multiple default gateway next hops and instead pick the first IP that matches the IP family of the EIP
		// use this for logging purposes in case fetching the node fails
		nodeName := node.Name
		node, err := e.watchFactory.GetNode(nodeName)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve node %s: %w", nodeName, err)
		}
		localNode, err := e.getLocalNodeName()
		if err != nil {
			return nil, err
		}
		// Node is local
		if localNode == node.Name {
			nextHopIPs, err := util.ParseNodeL3GatewayAnnotation(node)
			if err != nil {
				if util.IsAnnotationNotSetError(err) {
					// remote node may not have the annotation yet, suppress it
					return nil, types.NewSuppressedError(err)
				}
				return nil, fmt.Errorf("failed to get the node %s L3 gateway annotation: %w", node.Name, err)
			}
			if len(nextHopIPs.NextHops) == 0 {
				return nil, fmt.Errorf("l3 gateway annotation on the node %s has empty next hop IPs. Next hop is required for Layer 2 networks", node.Name)
			}
			ip, err := util.MatchFirstIPFamily(isIPv6, nextHopIPs.NextHops)
			if err != nil {
				return nil, fmt.Errorf("unable to find a next hop IP from the node %s L3 gateway annotation that is equal "+
					"to the EgressIP IP family (is IPv6: %v): %v", node.Name, isIPv6, err)
			}
			return ip, nil
		}
		// Node is remote
		// fetch Node gateway routers 'router to switch' port IP
		if isIPv6 {
			return udn.GetGWRouterIPv6(node, ni)
		}
		return udn.GetGWRouterIPv4(node, ni)
	}
	return nil, fmt.Errorf("unsupported network topology %s", ni.TopologyType())
}

// getLocalMgmtPortNextHop attempts the given networks local management port. If the information is not available because
// of management port deletion, no error will be returned.
func (e *EgressIPController) getLocalMgmtPortNextHop(ni util.NetInfo, egressNodeName, egressIPName, egressIPIP string, isIPv6 bool) (string, error) {
	mgmtPort := &nbdb.LogicalSwitchPort{Name: ni.GetNetworkScopedK8sMgmtIntfName(egressNodeName)}
	mgmtPort, err := libovsdbops.GetLogicalSwitchPort(e.nbClient, mgmtPort)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return "", fmt.Errorf("failed to get next hop IP for secondary host network and egress IP %s for node %s "+
			"because unable to get management port: %v", egressIPName, egressNodeName, err)
	} else if err != nil {
		klog.Warningf("While attempting to get next hop for Egress IP %s (%s), unable to get management switch port: %v",
			egressIPName, egressIPIP, err)
		return "", nil
	}
	allMgmtPortAddresses := mgmtPort.GetAddresses()
	if len(allMgmtPortAddresses) == 0 {
		return "", fmt.Errorf("failed to get next hop IP for secondary host network and egress IP %s for node %s"+
			"because failed to retrieve a MAC and IP address entry from management switch port %s", egressIPName, egressNodeName, mgmtPort.Name)
	}
	// select first MAC & IP(s) entry
	mgmtPortAddresses := strings.Fields(allMgmtPortAddresses[0])
	if len(mgmtPortAddresses) < 2 {
		return "", fmt.Errorf("failed to get next hop IP for secondary host network and egress IP %s for node %s"+
			"because management switch port %s does not contain expected MAC address and one or more IP addresses", egressIPName, egressNodeName, mgmtPort.Name)
	}
	// filter out the MAC address which is always the first entry within the slice
	mgmtPortAddresses = mgmtPortAddresses[1:]
	nextHopIP, err := util.MatchIPStringFamily(isIPv6, mgmtPortAddresses)
	if err != nil {
		return "", fmt.Errorf("failed to find a management port %s IP matching the IP family of the EgressIP: %v", mgmtPort.Name, err)
	}
	return nextHopIP, nil
}

func (e *EgressIPController) getRouterPortIP(portName string, wantsIPv6 bool) (net.IP, error) {
	gatewayIPs, err := libovsdbutil.GetLRPAddrs(e.nbClient, portName)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve port %s IP(s): %w", portName, err)
	}
	if gatewayIP, err := util.MatchFirstIPNetFamily(wantsIPv6, gatewayIPs); err != nil {
		return nil, fmt.Errorf("failed to find IP for port %s for IP family %v: %v", portName, wantsIPv6, err)
	} else {
		return gatewayIP.IP, nil
	}
}

// ipFamilyName returns IP family name based on the provided flag
func ipFamilyName(isIPv6 bool) string {
	if isIPv6 {
		return string(IPFamilyValueV6)
	}
	return string(IPFamilyValueV4)
}

func (e *EgressIPController) getTransitIP(nodeName string, wantsIPv6 bool) (string, error) {
	// fetch node annotation of the egress node
	node, err := e.watchFactory.GetNode(nodeName)
	if err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	nodeTransitIPs, err := util.ParseNodeTransitSwitchPortAddrs(node)
	if err != nil {
		return "", fmt.Errorf("unable to fetch transit switch IP for node %s: %w", nodeName, err)
	}
	nodeTransitIP, err := util.MatchFirstIPNetFamily(wantsIPv6, nodeTransitIPs)
	if err != nil {
		return "", fmt.Errorf("could not find transit switch IP of node %v for this family %v: %v", node, wantsIPv6, err)
	}
	return nodeTransitIP.IP.String(), nil
}

// getNextHop attempts to determine whether an egress IP should be routed through the Nodes primary network interface (isOVNetwork = true)
// or through a secondary host network (isOVNNetwork = false). If we failed to look up the information required to determine this, an error will be returned
// however if the information to determine the next hop IP doesn't exist, caller must be able to tolerate a empty next hop
// and no error returned. This means we searched successfully but could not find the information required to generate the next hop IP.
func (e *EgressIPController) getNextHop(ni util.NetInfo, egressNodeName, egressIP, egressIPName string, isLocalEgressNode, isOVNNetwork bool) (string, error) {
	isEgressIPv6 := utilnet.IsIPv6String(egressIP)
	egressNode, err := e.watchFactory.GetNode(egressNodeName)
	if err != nil {
		return "", err
	}
	if isLocalEgressNode || ni.TopologyType() == types.Layer2Topology {
		// isOVNNetwork is true when an EgressIP is "assigned" to the Nodes primary interface (breth0). Ext traffic will egress breth0.
		// is OVNNetwork is false when the EgressIP is assigned to a host secondary interface (not breth0). Ext traffic will egress this interface.
		if isOVNNetwork {
			gatewayRouterIP, err := e.getGatewayNextHop(ni, egressNode, isEgressIPv6)
			// return error only when we failed to retrieve the gateway IP. Do not return error when we can never get this IP (gw deleted)
			if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
				return "", fmt.Errorf("unable to retrieve gateway IP for node: %s, protocol is IPv6: %v, err: %w",
					egressNodeName, isEgressIPv6, err)
			} else if err != nil {
				klog.Warningf("While attempting to get next hop for Egress IP %s (%s), unable to get Node %s gateway "+
					"router IP: %v", egressIPName, egressIP, egressNodeName, err)
				return "", nil
			}
			return gatewayRouterIP.String(), nil
		} else {
			// for an egress IP assigned to a host secondary interface, next hop IP is the networks management port IP
			if ni.IsUserDefinedNetwork() {
				return "", fmt.Errorf("egress IP assigned to a host secondary interface for a user defined network (network name %s) is unsupported", ni.GetNetworkName())
			}
			return e.getLocalMgmtPortNextHop(ni, egressNodeName, egressIPName, egressIP, isEgressIPv6)
		}
	}

	nextHopIP, err := e.getTransitIP(egressNodeName, isEgressIPv6)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return "", fmt.Errorf("unable to fetch transit switch IP for node %s: %v", egressNodeName, err)
	} else if err != nil {
		klog.Warningf("While attempting to get next hop for Egress IP %s (%s), unable to get transit switch IP: %v",
			egressIPName, egressIP, err)
		return "", nil
	}
	return nextHopIP, nil
}

// createReroutePolicyOps creates an operation that does idempotent updates of the
// LogicalRouterPolicy corresponding to the egressIP status item, according to the
// following update procedure for when EIP is hosted by a OVN network:
// - if the LogicalRouterPolicy does not exist: it adds it by creating the
// reference to it from ovn_cluster_router and specifying the array of nexthops
// to equal [gatewayRouterIP]
// - if the LogicalRouterPolicy does exist: it adds the gatewayRouterIP to the
// array of nexthops
// For EIP hosted on secondary host network, logical route policies redirect
// pods to the appropriate management port or transit switch port.
// This function should be called with lock on nodeState cache key status.Node
func (e *EgressIPController) createReroutePolicyOps(ni util.NetInfo, ops []ovsdb.Operation, podIPNets []*net.IPNet, status egressipv1.EgressIPStatusItem,
	mark, egressIPName, nextHopIP, routerName, podNamespace, podName string) ([]ovsdb.Operation, error) {
	isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
	ipFamily := getEIPIPFamily(isEgressIPv6)
	options := make(map[string]string)

	if mark != "" {
		addPktMarkToLRPOptions(options, mark)
	}
	dbIDs := getEgressIPLRPReRouteDbIDs(egressIPName, podNamespace, podName, ipFamily, ni.GetNetworkName(), e.controllerName)
	p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
	// Handle all pod IPs that match the egress IP address family
	var err error
	for _, podIPNet := range util.MatchAllIPNetFamily(isEgressIPv6, podIPNets) {

		lrp := nbdb.LogicalRouterPolicy{
			Match:       fmt.Sprintf("%s.src == %s", ipFamilyName(isEgressIPv6), podIPNet.IP.String()),
			Priority:    types.EgressIPReroutePriority,
			Nexthops:    []string{nextHopIP},
			Action:      nbdb.LogicalRouterPolicyActionReroute,
			ExternalIDs: dbIDs.GetExternalIDs(),
			Options:     options,
		}
		ops, err = libovsdbops.CreateOrAddNextHopsToLogicalRouterPolicyWithPredicateOps(e.nbClient, ops, routerName, &lrp, p)
		if err != nil {
			return nil, fmt.Errorf("error creating logical router policy %+v on router %s: %v", lrp, routerName, err)
		}
	}
	return ops, nil
}

// deleteReroutePolicyOps creates an operation that does idempotent updates of the
// LogicalRouterPolicy corresponding to the egressIP object, according to the
// following update procedure:
// - if the LogicalRouterPolicy exist and has the len(nexthops) > 1: it removes
// the specified gatewayRouterIP from nexthops
// - if the LogicalRouterPolicy exist and has the len(nexthops) == 1: it removes
// the LogicalRouterPolicy completely
// if caller fails to find a next hop, we clear the LRPs for that specific Egress IP
// which will break HA momentarily
// This function should be called with lock on nodeState cache key status.Node
func (e *EgressIPController) deleteReroutePolicyOps(ni util.NetInfo, ops []ovsdb.Operation, status egressipv1.EgressIPStatusItem,
	egressIPName, nextHopIP, routerName, podNamespace, podName string) ([]ovsdb.Operation, error) {
	isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
	ipFamily := getEIPIPFamily(isEgressIPv6)
	var err error
	// Handle all pod IPs that match the egress IP address family
	dbIDs := getEgressIPLRPReRouteDbIDs(egressIPName, podNamespace, podName, ipFamily, ni.GetNetworkName(), e.controllerName)
	p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
	if nextHopIP != "" {
		ops, err = libovsdbops.DeleteNextHopFromLogicalRouterPoliciesWithPredicateOps(e.nbClient, ops, routerName, p, nextHopIP)
		if err != nil {
			return nil, fmt.Errorf("error removing nexthop IP %s from egress ip %s policies on router %s: %v",
				nextHopIP, egressIPName, routerName, err)
		}
	} else {
		klog.Errorf("Caller failed to pass next hop for EgressIP %s and IP %s. Deleting all LRPs. This will break HA momentarily",
			egressIPName, status.EgressIP)
		// since next hop was not found, delete everything to ensure no stale entries however this will break load
		// balancing between hops, but we offer no guarantees except one of the EIPs will work
		ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(e.nbClient, ops, routerName, p)
		if err != nil {
			return nil, fmt.Errorf("failed to create logical router policy delete operations on %s: %v", routerName, err)
		}
	}
	return ops, nil
}

func (e *EgressIPController) createGWMarkPolicyOps(ni util.NetInfo, ops []ovsdb.Operation, podIPNets []*net.IPNet, status egressipv1.EgressIPStatusItem,
	mark, podNamespace, podName, egressIPName string) ([]ovsdb.Operation, error) {
	isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
	routerName := ni.GetNetworkScopedGWRouterName(status.Node)
	options := make(map[string]string)
	var err error
	if mark != "" {
		addPktMarkToLRPOptions(options, mark)
	}
	ovnIPFamilyName := ipFamilyName(isEgressIPv6)
	ipFamilyValue := getEIPIPFamily(isEgressIPv6)
	dbIDs := getEgressIPLRPSNATMarkDbIDs(egressIPName, podNamespace, podName, ipFamilyValue, ni.GetNetworkName(), e.controllerName)
	// Handle all pod IPs that match the egress IP address family
	for _, podIPNet := range util.MatchAllIPNetFamily(isEgressIPv6, podIPNets) {
		lrp := nbdb.LogicalRouterPolicy{
			Match:       fmt.Sprintf("%s.src == %s && pkt.mark == 0", ovnIPFamilyName, podIPNet.IP.String()), // only add pkt mark if one already doesn't exist
			Priority:    types.EgressIPSNATMarkPriority,
			Action:      nbdb.LogicalRouterPolicyActionAllow,
			ExternalIDs: dbIDs.GetExternalIDs(),
			Options:     options,
		}
		p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(e.nbClient, ops, routerName, &lrp, p)
		if err != nil {
			return nil, fmt.Errorf("error creating logical router policy %+v create/update ops for packet marking on router %s: %v", lrp, routerName, err)
		}
	}
	return ops, nil
}

func (e *EgressIPController) deleteGWMarkPolicyOps(ni util.NetInfo, ops []ovsdb.Operation, status egressipv1.EgressIPStatusItem,
	podNamespace, podName, egressIPName string) ([]ovsdb.Operation, error) {
	isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
	routerName := ni.GetNetworkScopedGWRouterName(status.Node)
	ipFamilyValue := getEIPIPFamily(isEgressIPv6)
	dbIDs := getEgressIPLRPSNATMarkDbIDs(egressIPName, podNamespace, podName, ipFamilyValue, ni.GetNetworkName(), e.controllerName)
	p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
	var err error
	ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(e.nbClient, ops, routerName, p)
	if err != nil {
		return nil, fmt.Errorf("error creating logical router policy delete ops for packet marking on router %s: %v", routerName, err)
	}
	return ops, nil
}

func (e *EgressIPController) deleteGWMarkPolicyForStatusOps(ni util.NetInfo, ops []ovsdb.Operation, status egressipv1.EgressIPStatusItem,
	egressIPName string) ([]ovsdb.Operation, error) {
	isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
	routerName := ni.GetNetworkScopedGWRouterName(status.Node)
	ipFamilyValue := getEIPIPFamily(isEgressIPv6)
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, e.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.PriorityKey: fmt.Sprintf("%d", types.EgressIPSNATMarkPriority),
			libovsdbops.IPFamilyKey: string(ipFamilyValue),
			libovsdbops.NetworkKey:  ni.GetNetworkName(),
		})
	lrpExtIDPredicate := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](predicateIDs, nil)
	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return lrpExtIDPredicate(item) && strings.HasPrefix(item.ExternalIDs[libovsdbops.ObjectNameKey.String()], egressIPName+dbIDEIPNamePodDivider)
	}
	var err error
	ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(e.nbClient, ops, routerName, p)
	if err != nil {
		return nil, fmt.Errorf("error creating logical router policy delete ops for packet marking on router %s: %v", routerName, err)
	}
	return ops, nil
}

// deleteEgressIPStatusSetup deletes the entire set up in the NB DB for an
// EgressIPStatusItem. The set up in the NB DB gets tagged with the name of the
// EgressIP, hence lookup the LRP and NAT objects which match that as well as
// the attributes of the EgressIPStatusItem. Keep in mind: the LRP should get
// completely deleted once the remaining and last nexthop equals the
// gatewayRouterIP corresponding to the node in the EgressIPStatusItem, else
// just remove the gatewayRouterIP from the list of nexthops
// This function should be called with a lock on e.nodeState status.Node key.
func (e *EgressIPController) deleteEgressIPStatusSetup(ni util.NetInfo, name string, status egressipv1.EgressIPStatusItem) error {
	var err error
	var ops []ovsdb.Operation
	nextHopIP, err := e.attemptToGetNextHopIP(ni, name, status)
	if err != nil {
		return fmt.Errorf("failed to delete egress IP %s (%s) because unable to determine next hop: %v",
			name, status.EgressIP, err)
	}
	isIPv6 := utilnet.IsIPv6String(status.EgressIP)
	policyPredNextHop := func(item *nbdb.LogicalRouterPolicy) bool {
		hasIPNexthop := false
		for _, nexthop := range item.Nexthops {
			if nexthop == nextHopIP {
				hasIPNexthop = true
				break
			}
		}
		return item.Priority == types.EgressIPReroutePriority && hasIPNexthop &&
			item.ExternalIDs[libovsdbops.NetworkKey.String()] == ni.GetNetworkName() &&
			item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == e.controllerName &&
			item.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.EgressIPOwnerType) &&
			item.ExternalIDs[libovsdbops.IPFamilyKey.String()] == string(getEIPIPFamily(isIPv6)) &&
			strings.HasPrefix(item.ExternalIDs[libovsdbops.ObjectNameKey.String()], name+dbIDEIPNamePodDivider)
	}

	if nextHopIP != "" {
		router, err := e.getTopologyScopedLocalRouterName(ni)
		if err != nil {
			return err
		}
		ops, err = libovsdbops.DeleteNextHopFromLogicalRouterPoliciesWithPredicateOps(e.nbClient, ops, router, policyPredNextHop, nextHopIP)
		if err != nil {
			return fmt.Errorf("error removing nexthop IP %s from egress ip %s policies on router %s: %v",
				nextHopIP, name, router, err)
		}
	} else if ops, err = e.ensureOnlyValidNextHops(ni, name, status.Node, ops); err != nil {
		return err
	}

	isLocalEgressNode, loadedEgressNode := e.getNodeLocality(status.Node)
	if loadedEgressNode && isLocalEgressNode {
		if ni.IsDefault() {
			routerName := ni.GetNetworkScopedGWRouterName(status.Node)
			natPred := func(nat *nbdb.NAT) bool {
				// We should delete NATs only from the status.Node that was passed into this function
				return strings.HasPrefix(nat.ExternalIDs[libovsdbops.ObjectNameKey.String()], name+dbIDEIPNamePodDivider) &&
					nat.ExternalIP == status.EgressIP && nat.LogicalPort != nil &&
					*nat.LogicalPort == ni.GetNetworkScopedK8sMgmtIntfName(status.Node)
			}
			ops, err = libovsdbops.DeleteNATsWithPredicateOps(e.nbClient, ops, natPred)
			if err != nil {
				return fmt.Errorf("error removing egress ip %s nats on router %s: %v", name, routerName, err)
			}
		} else if ni.IsUserDefinedNetwork() {
			if ops, err = e.deleteGWMarkPolicyForStatusOps(ni, ops, status, name); err != nil {
				return fmt.Errorf("failed to delete gateway mark policy: %v", err)
			}
		}
	}

	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	if err != nil {
		return fmt.Errorf("error transacting ops %+v: %v", ops, err)
	}
	return nil
}

func (e *EgressIPController) ensureOnlyValidNextHops(ni util.NetInfo, name, nodeName string, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {
	// When no nextHopIP is found, This may happen when node object is already deleted.
	// So compare validNextHopIPs associated with current eIP.Status and Nexthops present
	// in the LogicalRouterPolicy, then delete nexthop(s) from LogicalRouterPolicy if
	// it doesn't match with nexthops derived from eIP.Status.
	policyPred := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == types.EgressIPReroutePriority &&
			strings.HasPrefix(item.ExternalIDs[libovsdbops.ObjectNameKey.String()], name+dbIDEIPNamePodDivider) &&
			item.ExternalIDs[libovsdbops.NetworkKey.String()] == ni.GetNetworkName()
	}
	routerName, err := getTopologyScopedRouterName(ni, nodeName)
	if err != nil {
		return ops, err
	}
	eIP, err := e.watchFactory.GetEgressIP(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return ops, fmt.Errorf("error retrieving EgressIP %s object for updating logical router policy nexthops, err: %w", name, err)
	} else if err != nil && apierrors.IsNotFound(err) {
		// EgressIP object is not found, so delete LRP associated with it.
		ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(e.nbClient, ops, routerName, policyPred)
		if err != nil {
			return ops, fmt.Errorf("error creating ops to remove logical router policy for EgressIP %s from router %s: %v",
				name, routerName, err)
		}
	} else {
		validNextHopIPs := make(sets.Set[string])
		for _, validStatus := range eIP.Status.Items {
			nextHopIP, err := e.attemptToGetNextHopIP(ni, name, validStatus)
			if err != nil {
				return ops, fmt.Errorf("failed to delete EgressIP %s (%s) because unable to determine next hop: %v",
					name, validStatus.EgressIP, err)
			}
			validNextHopIPs.Insert(nextHopIP)
		}

		reRoutePolicies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(e.nbClient, policyPred)
		if err != nil {
			return ops, fmt.Errorf("error finding logical router policy for EgressIP %s: %v", name, err)
		}
		if len(validNextHopIPs) == 0 {
			ops, err = libovsdbops.DeleteLogicalRouterPoliciesOps(e.nbClient, ops, routerName, reRoutePolicies...)
			if err != nil {
				return ops, fmt.Errorf("error creating ops to remove logical router policy for EgressIP %s from router %s: %v",
					name, routerName, err)
			}
			return ops, nil
		}
		for _, policy := range reRoutePolicies {
			for _, nextHop := range policy.Nexthops {
				if validNextHopIPs.Has(nextHop) {
					continue
				}
				ops, err = libovsdbops.DeleteNextHopsFromLogicalRouterPolicyOps(e.nbClient, ops, routerName, []*nbdb.LogicalRouterPolicy{policy}, nextHop)
				if err != nil {
					return ops, fmt.Errorf("error creating ops to remove stale next hop IP %s from logical router policy for EgressIP %s from router %s: %v",
						nextHop, name, routerName, err)
				}
			}
		}
	}
	return ops, nil
}

// attemptToGetNextHopIP this function attempts to retrieve nexthops associated with logical router policy for the given
// EgressIP's status. It ensures the following conditions are met.
// 1) When node is not found, then it must return empty nexthop without an error.
// 2) When EgressIP belongs to OVN network and node is local, then it must return node's gateway router IP address.
// 3) When EgressIP belongs to non OVN network and node is local, then it must return node's management port IP address.
// 4) When EgressIP belongs to remote node in interconnect zone, then it return node's transit switch IP address.
func (n *EgressIPController) attemptToGetNextHopIP(ni util.NetInfo, name string, status egressipv1.EgressIPStatusItem) (string, error) {
	isLocalEgressNode, _ := n.getNodeLocality(status.Node)
	eNode, err := n.watchFactory.GetNode(status.Node)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("unable to get node for Egress IP %s: %v", status.EgressIP, err)
	} else if err != nil {
		klog.Errorf("Node %s is not found for Egress IP %s", status.Node, status.EgressIP)
		return "", nil
	}
	var nextHopIP string
	eIPIP := net.ParseIP(status.EgressIP)
	if eIPConfig, err := util.GetNodeEIPConfig(eNode); err != nil {
		klog.Warningf("Failed to get Egress IP config from node annotation %s: %v", status.Node, err)
	} else {
		isOVNNetwork := util.IsOVNNetwork(eIPConfig, eIPIP)
		nextHopIP, err = n.getNextHop(ni, status.Node, status.EgressIP, name, isLocalEgressNode, isOVNNetwork)
		if err != nil {
			return "", err
		}
	}
	return nextHopIP, nil
}

func (e *EgressIPController) addPodIPsToAddressSet(network string, controller string, addrSetIPs ...net.IP) error {
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, network, controller)
	as, err := e.addressSetFactory.GetAddressSet(dbIDs)
	if err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", EgressIPServedPodsAddrSetName, err)
	}
	if err := as.AddAddresses(util.StringSlice(addrSetIPs)); err != nil {
		return fmt.Errorf("cannot add egressPodIPs %v from the address set %v: err: %v", addrSetIPs, EgressIPServedPodsAddrSetName, err)
	}
	return nil
}

func (e *EgressIPController) deletePodIPsFromAddressSet(network string, controller string, addrSetIPs ...net.IP) error {
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, network, controller)
	as, err := e.addressSetFactory.GetAddressSet(dbIDs)
	if err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", EgressIPServedPodsAddrSetName, err)
	}
	if err := as.DeleteAddresses(util.StringSlice(addrSetIPs)); err != nil {
		return fmt.Errorf("cannot delete egressPodIPs %v from the address set %v: err: %v", addrSetIPs, EgressIPServedPodsAddrSetName, err)
	}
	return nil
}

// createDefaultNoRerouteServicePolicies ensures service reachability from the
// host network to any service (except ETP=local) backed by egress IP matching pods
func createDefaultNoRerouteServicePolicies(nbClient libovsdbclient.Client, network, controller, clusterRouter string,
	v4ClusterSubnet, v6ClusterSubnet []*net.IPNet, v4JoinSubnet, v6JoinSubnet *net.IPNet) error {
	for _, v4Subnet := range v4ClusterSubnet {
		if v4JoinSubnet == nil {
			klog.Errorf("Creating no reroute services requires IPv4 join subnet but not found")
			continue
		}
		dbIDs := getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, network, controller)
		match := fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Subnet.String(), v4JoinSubnet.String())
		if err := createEgressIPLogicalRouterPolicy(nbClient, clusterRouter, match, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv4 no-reroute service policies, err: %v", err)
		}
	}
	for _, v6Subnet := range v6ClusterSubnet {
		if v6JoinSubnet == nil {
			klog.Errorf("Creating no reroute services requires IPv6 join subnet but not found")
			continue
		}
		dbIDs := getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV6, network, controller)
		match := fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6Subnet.String(), v6JoinSubnet.String())
		if err := createEgressIPLogicalRouterPolicy(nbClient, clusterRouter, match, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv6 no-reroute service policies, err: %v", err)
		}
	}
	return nil
}

// ensureRouterPoliciesForNetwork is the optimized version of batched ops
// It batches all operations for a single network into one transaction.
// This method is called by l2 and l3 udn controllers on `addUpdateLocalNodeEvent`.
// addressSetManager has its own internal locking for EnsureClusterNodeIPsAddressSet.
// There are no overlapping resources among networks being handled by this method.
func (e *EgressIPController) ensureRouterPoliciesForNetwork(ni util.NetInfo, node *corev1.Node) error {
	e.lockNetwork(ni.GetNetworkName())
	defer e.unlockNetwork(ni.GetNetworkName())

	subnetEntries := ni.Subnets()
	subnets := util.GetAllClusterSubnetsFromEntries(subnetEntries)
	if len(subnets) == 0 {
		return nil
	}

	localNode, err := e.getLocalNodeName()
	if err != nil {
		return err
	}

	routerName, err := getTopologyScopedRouterName(ni, localNode)
	if err != nil {
		return err
	}

	// Ensure cluster node address sets (shared cluster-wide resource)
	clusterNodeIPsAddrSetDbIDs, err := e.addressSetManager.EnsureClusterNodeIPsAddressSet(addresssetmanager.ClusterNodeIPsEgressIPBackRef)
	if err != nil {
		return fmt.Errorf("failed to ensure cluster node IP address set for EgressIP: %w", err)
	}

	// Build all operations without committing
	ops := []ovsdb.Operation{}

	// 1. Initialize cluster egress policies (batched)
	// These policies are network-scoped (shared by all nodes), not per-node
	ops, err = initClusterEgressPoliciesOps(e.nbClient, e.addressSetFactory, ni, subnets, e.controllerName, routerName, clusterNodeIPsAddrSetDbIDs, ops)
	if err != nil {
		return fmt.Errorf("failed to build cluster egress policies ops for network %s: %v", ni.GetNetworkName(), err)
	}

	clusterNodesAddressSets, err := e.addressSetFactory.GetAddressSet(clusterNodeIPsAddrSetDbIDs)
	if err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", NodeIPAddrSetName, err)
	}

	// 2. Ensure default no-reroute node policies (batched)
	ops, err = ensureDefaultNoRerouteNodePoliciesOps(e.nbClient, e.addressSetFactory, ni.GetNetworkName(), routerName,
		e.controllerName, e.v4, e.v6, clusterNodesAddressSets, ops)
	if err != nil {
		return fmt.Errorf("failed to build node policies ops for network %s: %v", ni.GetNetworkName(), err)
	}

	// Commit router and node policy operations under global per-network lock
	if _, err := libovsdbops.TransactAndCheck(e.nbClient, ops); err != nil {
		return fmt.Errorf("failed to commit batched policy operations for network %s: %v", ni.GetNetworkName(), err)
	}

	if ni.TopologyType() == types.Layer3Topology {
		gatewayIPs, err := udn.GetGWRouterIPs(node, ni)
		if err != nil {
			return fmt.Errorf("failed to get %q network gateway router join IPs for node %q, err: %w", ni.GetNetworkName(), node.Name, err)
		}
		if err := libovsdbutil.CreateDefaultRouteToExternal(e.nbClient, routerName,
			ni.GetNetworkScopedGWRouterName(localNode), subnetEntries, gatewayIPs); err != nil {
			return fmt.Errorf("failed to create route to external for network %s: %v", ni.GetNetworkName(), err)
		}
	}

	return nil
}

func (e *EgressIPController) ensureSwitchPoliciesForNode(ni util.NetInfo, nodeName string) error {
	e.lockNetwork(ni.GetNetworkName())
	defer e.unlockNetwork(ni.GetNetworkName())
	ops, err := e.ensureDefaultNoReRouteQosRulesForNode(ni, nodeName, nil)
	if err != nil {
		return fmt.Errorf("failed to ensure no reroute QoS rules for node %s and network %s: %v", nodeName, ni.GetNetworkName(), err)
	}
	if _, err = libovsdbops.TransactAndCheck(e.nbClient, ops); err != nil {
		return fmt.Errorf("unable to ensure default no reroute QoS rules for network %s, err: %v", ni.GetNetworkName(), err)
	}
	return nil
}

// createDefaultNoRerouteReplyTrafficPolicies ensures any traffic which is a response/reply from the egressIP pods
// will not be re-routed to egress-nodes. This ensures EIP can work well with ETP=local
// this policy is ipFamily neutral
func createDefaultNoRerouteReplyTrafficPolicy(nbClient libovsdbclient.Client, network, controller, routerName string) error {
	match := fmt.Sprintf("pkt.mark == %d", types.EgressIPReplyTrafficConnectionMark)
	dbIDs := getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, ReplyTrafficNoReroute, IPFamilyValue, network, controller)
	if err := createEgressIPLogicalRouterPolicy(nbClient, routerName, match, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
		return fmt.Errorf("unable to create no-reroute reply traffic policies, err: %v", err)
	}
	return nil
}

// createDefaultNoReroutePodPolicies ensures egress pods east<->west traffic with regular pods,
// i.e: ensuring that an egress pod can still communicate with a regular pod / service backed by regular pods
func createDefaultNoReroutePodPolicies(nbClient libovsdbclient.Client, network, controller, routerName string, v4ClusterSubnet, v6ClusterSubnet []*net.IPNet) error {
	for _, v4Subnet1 := range v4ClusterSubnet {
		for _, v4Subnet2 := range v4ClusterSubnet {
			match := fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Subnet1.String(), v4Subnet2.String())
			dbIDs := getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, network, controller)
			if err := createEgressIPLogicalRouterPolicy(nbClient, routerName, match, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
				return fmt.Errorf("unable to create IPv4 no-reroute pod policies, err: %v", err)
			}
		}
	}
	for _, v6Subnet1 := range v6ClusterSubnet {
		for _, v6Subnet2 := range v6ClusterSubnet {
			match := fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6Subnet1.String(), v6Subnet2.String())
			dbIDs := getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV6, network, controller)
			if err := createEgressIPLogicalRouterPolicy(nbClient, routerName, match, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
				return fmt.Errorf("unable to create IPv6 no-reroute pod policies, err: %v", err)
			}
		}
	}

	return nil
}

// createDefaultReRouteQoSRule builds QoS rule ops to be created on every node's switch that let's us
// mark packets that are tracked in conntrack and replies emerging from the pod.
// This mark is then matched on the reroute policies to determine if its a reply packet
// in which case we do not need to reRoute to other nodes and if its a service response it is
// not rerouted since mark will be present.
func createDefaultReRouteQoSRuleOps(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory, ops []ovsdb.Operation,
	network, controller string, isIPv4Mode, isIPv6Mode bool) ([]*nbdb.QoS, []ovsdb.Operation, error) {
	// fetch the egressIP pods address-set
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, network, controller)
	var as addressset.AddressSet
	var err error
	qoses := []*nbdb.QoS{}
	if as, err = addressSetFactory.GetAddressSet(dbIDs); err != nil {
		return nil, nil, fmt.Errorf("cannot ensure that addressSet %s exists %v", EgressIPServedPodsAddrSetName, err)
	}
	ipv4EgressIPServedPodsAS, ipv6EgressIPServedPodsAS := as.GetASHashNames()
	qosRule := nbdb.QoS{
		Priority:  types.EgressIPRerouteQoSRulePriority,
		Action:    map[string]int{"mark": types.EgressIPReplyTrafficConnectionMark},
		Direction: nbdb.QoSDirectionFromLport,
	}
	if isIPv4Mode {
		// if address set hash name is empty, the address set has yet to be created
		if ipv4EgressIPServedPodsAS == "" {
			return nil, nil, types.NewSuppressedError(fmt.Errorf("failed to fetch IPv4 address set %s hash names", EgressIPServedPodsAddrSetName))
		}
		qosV4Rule := qosRule
		qosV4Rule.Match = fmt.Sprintf(`ip4.src == $%s && ct.trk && ct.rpl`, ipv4EgressIPServedPodsAS)
		qosV4Rule.ExternalIDs = getEgressIPQoSRuleDbIDs(IPFamilyValueV4, network, controller).GetExternalIDs()
		ops, err = libovsdbops.CreateOrUpdateQoSesOps(nbClient, ops, &qosV4Rule)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot create v4 QoS rule ops for egressIP feature on controller %s, %v", controller, err)
		}
		qoses = append(qoses, &qosV4Rule)
	}
	if isIPv6Mode {
		// if address set hash name is empty, the address set has yet to be created
		if ipv6EgressIPServedPodsAS == "" {
			return nil, nil, types.NewSuppressedError(fmt.Errorf("failed to fetch IPv6 address set %s hash names", EgressIPServedPodsAddrSetName))
		}
		qosV6Rule := qosRule
		qosV6Rule.Match = fmt.Sprintf(`ip6.src == $%s && ct.trk && ct.rpl`, ipv6EgressIPServedPodsAS)
		qosV6Rule.ExternalIDs = getEgressIPQoSRuleDbIDs(IPFamilyValueV6, network, controller).GetExternalIDs()
		ops, err = libovsdbops.CreateOrUpdateQoSesOps(nbClient, ops, &qosV6Rule)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot create v6 QoS rule ops for egressIP feature on controller %s, %v", controller, err)
		}
		qoses = append(qoses, &qosV6Rule)
	}
	return qoses, ops, nil
}

func (e *EgressIPController) ensureDefaultNoRerouteQoSRules(nodeName string) error {
	e.lockNetwork(types.DefaultNetworkName)
	defer e.unlockNetwork(types.DefaultNetworkName)
	defaultNetInfo := e.networkManager.GetNetwork(types.DefaultNetworkName)
	var ops []ovsdb.Operation
	ops, err := e.ensureDefaultNoReRouteQosRulesForNode(defaultNetInfo, nodeName, ops)
	if err != nil {
		return fmt.Errorf("failed to process default network: %v", err)
	}
	if _, err := libovsdbops.TransactAndCheck(e.nbClient, ops); err != nil {
		return fmt.Errorf("failed to ensure default network no reroute QoS rules, err: %v", err)
	}

	if err = e.networkManager.DoWithLock(func(network util.NetInfo) error {
		if network.GetNetworkName() == types.DefaultNetworkName {
			return nil
		}
		e.lockNetwork(network.GetNetworkName())
		defer e.unlockNetwork(network.GetNetworkName())
		var networkOps []ovsdb.Operation
		networkOps, err = e.ensureDefaultNoReRouteQosRulesForNode(network, nodeName, networkOps)
		if err != nil {
			return fmt.Errorf("failed to process network %s: %v", network.GetNetworkName(), err)
		}
		if _, err := libovsdbops.TransactAndCheck(e.nbClient, networkOps); err != nil {
			return fmt.Errorf("failed to ensure network %s no reroute QoS rules, err: %v", network.GetNetworkName(), err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to ensure default no reroute QoS rules: %v", err)
	}
	return nil
}

func (e *EgressIPController) ensureDefaultNoReRouteQosRulesForNode(ni util.NetInfo, nodeName string, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {
	// since this function is called from node update event, let us check
	// libovsdb cache before trying to create insert/update ops so that it
	// doesn't cause no-op construction spams at scale (kubelet sends node
	// update events every 10seconds so we don't want to cause unnecessary
	// no-op transacts that often and lookup is less expensive)
	getQOSForFamily := func(ipFamily egressIPFamilyValue, existingQOSes []*nbdb.QoS) ([]*nbdb.QoS, error) {
		dbIDs := getEgressIPQoSRuleDbIDs(ipFamily, ni.GetNetworkName(), e.controllerName)
		p := libovsdbops.GetPredicate[*nbdb.QoS](dbIDs, nil)
		existingQoSesForIPFamily, err := libovsdbops.FindQoSesWithPredicate(e.nbClient, p)
		if err != nil {
			return nil, fmt.Errorf("failed to find QOS with predicate: %v", err)
		}
		return append(existingQOSes, existingQoSesForIPFamily...), nil
	}
	existingQoSes := make([]*nbdb.QoS, 0, 2)
	var err error
	if e.v4 {
		existingQoSes, err = getQOSForFamily(IPFamilyValueV4, existingQoSes)
		if err != nil {
			return nil, fmt.Errorf("failed to get existing IPv4 QOS rules: %v", err)
		}
	}
	if e.v6 {
		existingQoSes, err = getQOSForFamily(IPFamilyValueV6, existingQoSes)
		if err != nil {
			return nil, fmt.Errorf("failed to get existing IPv6 QOS rules: %v", err)
		}
	}
	qosExists := false
	if e.v4 && e.v6 && len(existingQoSes) == 2 {
		// no need to create QoS Rule ops; already exists; dualstack
		qosExists = true
	}
	if len(existingQoSes) == 1 && ((e.v4 && !e.v6) || (e.v6 && !e.v4)) {
		// no need to create QoS Rule ops; already exists; single stack
		qosExists = true
	}
	if !qosExists {
		existingQoSes, ops, err = createDefaultReRouteQoSRuleOps(e.nbClient, e.addressSetFactory, ops, ni.GetNetworkName(), e.controllerName, e.v4, e.v6)
		if err != nil {
			return nil, fmt.Errorf("cannot create QoS rule ops: %v", err)
		}
	}
	if len(existingQoSes) > 0 {
		nodeSwitchName := ni.GetNetworkScopedSwitchName(nodeName)
		if qosExists {
			// check if these rules were already added to the existing switch or not
			addQoSToSwitch := false
			nodeSwitch, err := libovsdbops.GetLogicalSwitch(e.nbClient, &nbdb.LogicalSwitch{Name: nodeSwitchName})
			if err != nil {
				return nil, fmt.Errorf("cannot fetch switch for node %s: %v", nodeSwitchName, err)
			}
			for _, qos := range existingQoSes {
				if slices.Contains(nodeSwitch.QOSRules, qos.UUID) {
					continue
				} else { // rule doesn't exist on switch; we should update switch
					addQoSToSwitch = true
					break
				}
			}
			if !addQoSToSwitch {
				return ops, nil
			}
		}
		ops, err = libovsdbops.AddQoSesToLogicalSwitchOps(e.nbClient, ops, nodeSwitchName, existingQoSes...)
		if err != nil {
			return nil, fmt.Errorf("failed to add QoS to logical switch %s: %v", nodeSwitchName, err)
		}
	}
	return ops, nil
}

func (e *EgressIPController) ensureDefaultNoRerouteNodePolicies() error {
	clusterNodeIPsAddrSetDbIDs, err := e.addressSetManager.EnsureClusterNodeIPsAddressSet(addresssetmanager.ClusterNodeIPsEgressIPBackRef)
	if err != nil {
		return fmt.Errorf("failed to ensure cluster node IP address set for EgressIP: %w", err)
	}
	clusterNodesAddressSets, err := e.addressSetFactory.GetAddressSet(clusterNodeIPsAddrSetDbIDs)
	if err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", NodeIPAddrSetName, err)
	}

	// ensure default network is processed
	e.lockNetwork(types.DefaultNetworkName)
	defer e.unlockNetwork(types.DefaultNetworkName)

	defaultNetInfo := e.networkManager.GetNetwork(types.DefaultNetworkName)
	err = ensureDefaultNoRerouteNodePolicies(e.nbClient, e.addressSetFactory, defaultNetInfo.GetNetworkName(), defaultNetInfo.GetNetworkScopedClusterRouterName(), e.controllerName, clusterNodesAddressSets, e.v4, e.v6)
	if err != nil {
		return fmt.Errorf("failed to ensure default no reroute policies for nodes for default network: %v", err)
	}

	if !isEgressIPForUDNSupported() {
		return nil
	}
	if err = e.networkManager.DoWithLock(func(network util.NetInfo) error {
		if network.GetNetworkName() == types.DefaultNetworkName {
			return nil
		}
		e.lockNetwork(network.GetNetworkName())
		defer e.unlockNetwork(network.GetNetworkName())

		routerName, err := e.getTopologyScopedLocalRouterName(network)
		if err != nil {
			return err
		}

		err = ensureDefaultNoRerouteNodePolicies(e.nbClient, e.addressSetFactory, network.GetNetworkName(), routerName,
			e.controllerName, clusterNodesAddressSets, e.v4, e.v6)
		if err != nil {
			return fmt.Errorf("failed to ensure default no reroute policies for nodes for network %s: %v", network.GetNetworkName(), err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// ensureDefaultNoRerouteNodePolicies ensures egress pods east<->west traffic with hostNetwork pods,
// i.e: ensuring that an egress pod can still communicate with a hostNetwork pod / service backed by hostNetwork pods
// without using egressIPs.
// sample: 101 ip4.src == $a12749576804119081385 && ip4.dst == $a11079093880111560446 allow pkt_mark=1008
// All node addresses are listed here to decide which IP family policies are required.
func ensureDefaultNoRerouteNodePolicies(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory,
	network, router, controller string, clusterNodesAddressSets addressset.AddressSet, v4, v6 bool) error {
	ipv4ClusterNodeIPAS, ipv6ClusterNodeIPAS := clusterNodesAddressSets.GetASHashNames()

	var as addressset.AddressSet
	var err error
	// fetch the egressIP pods address-set
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, network, controller)
	if as, err = addressSetFactory.GetAddressSet(dbIDs); err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", EgressIPServedPodsAddrSetName, err)
	}
	ipv4EgressIPServedPodsAS, ipv6EgressIPServedPodsAS := as.GetASHashNames()

	// fetch the egressService pods address-set
	dbIDs = egresssvc.GetEgressServiceAddrSetDbIDs(controller)
	if as, err = addressSetFactory.GetAddressSet(dbIDs); err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", egresssvc.EgressServiceServedPodsAddrSetName, err)
	}
	ipv4EgressServiceServedPodsAS, ipv6EgressServiceServedPodsAS := as.GetASHashNames()

	var matchV4, matchV6 string
	// construct the policy match
	if v4 {
		// if address set hash name is empty, the address set has yet to be created
		if ipv4EgressIPServedPodsAS == "" || ipv4EgressServiceServedPodsAS == "" || ipv4ClusterNodeIPAS == "" {
			return types.NewSuppressedError(fmt.Errorf("address set name(s) %s not found %q %q %q", as.GetName(), ipv4EgressServiceServedPodsAS, ipv4EgressServiceServedPodsAS, ipv4ClusterNodeIPAS))
		}
		matchV4 = fmt.Sprintf(`(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s`,
			ipv4EgressIPServedPodsAS, ipv4EgressServiceServedPodsAS, ipv4ClusterNodeIPAS)
	}
	if v6 {
		// if address set hash name is empty, the address set has yet to be created
		if ipv6EgressIPServedPodsAS == "" || ipv6EgressServiceServedPodsAS == "" || ipv6ClusterNodeIPAS == "" {
			return types.NewSuppressedError(fmt.Errorf("address set hash name(s) %s not found", as.GetName()))
		}
		matchV6 = fmt.Sprintf(`(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s`,
			ipv6EgressIPServedPodsAS, ipv6EgressServiceServedPodsAS, ipv6ClusterNodeIPAS)
	}
	options := map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark}
	// Create global allow policy for node traffic
	if matchV4 != "" {
		dbIDs = getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, network, controller)
		if err := createEgressIPLogicalRouterPolicy(nbClient, router, matchV4, types.DefaultNoRereoutePriority, options, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv4 no-reroute node policies, err: %v", err)
		}
	}

	if matchV6 != "" {
		dbIDs = getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, network, controller)
		if err := createEgressIPLogicalRouterPolicy(nbClient, router, matchV6, types.DefaultNoRereoutePriority, options, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv6 no-reroute node policies, err: %v", err)
		}
	}
	return nil
}

func (e *EgressIPController) getPodNADKeyForNetwork(ni util.NetInfo, pod *corev1.Pod) (string, error) {
	if !ni.IsUserDefinedNetwork() {
		return ni.GetNetworkName(), nil
	}
	nadKeys, err := util.PodNADKeys(pod, ni, e.networkManager.GetNetworkNameForNADKey)
	if err != nil {
		return "", err
	}
	if len(nadKeys) == 0 {
		return "", fmt.Errorf("expected at least one NAD key for pod %s/%s on network %s", pod.Namespace, pod.Name, ni.GetNetworkName())
	}
	if len(nadKeys) > 1 {
		return "", fmt.Errorf("expected one NAD key for pod %s/%s on network %s, got %d", pod.Namespace, pod.Name, ni.GetNetworkName(), len(nadKeys))
	}
	return nadKeys[0], nil
}

func (e *EgressIPController) getPodIPs(ni util.NetInfo, pod *corev1.Pod, nadKey string) ([]*net.IPNet, error) {
	podIPs := make([]*net.IPNet, 0)
	getIPFromIPNetFn := func(podIPNets []*net.IPNet) []*net.IPNet {
		ipNetsCopy := make([]*net.IPNet, 0, len(podIPNets))
		for _, ipNet := range podIPNets {
			ipNetCopy := *ipNet
			ipNetsCopy = append(ipNetsCopy, &ipNetCopy)
		}
		return ipNetsCopy
	}
	if e.isPodScheduledOnLocalNode(pod) {
		// Retrieve the pod's networking configuration from the
		// logicalPortCache. The reason for doing this: a) only normal network
		// pods are placed in this cache, b) once the pod is placed here we know
		// addLogicalPort has finished successfully setting up networking for
		// the pod, so we can proceed with retrieving its IP and deleting the
		// external GW configuration created in addLogicalPort for the pod.
		logicalPort, err := e.logicalPortCache.get(pod, nadKey)
		if err != nil {
			return nil, nil
		}
		// Since the logical switch port cache removes entries only 60 seconds
		// after deletion, its possible that when pod is recreated with the same name
		// within the 60seconds timer, stale info gets used to create SNATs and reroutes
		// for the eip pods. Checking if the expiry is set for the port or not can indicate
		// if the port is scheduled for deletion.
		if !logicalPort.expires.IsZero() {
			klog.Warningf("Stale LSP %s for pod %s/%s found in cache refetching",
				logicalPort.name, pod.Namespace, pod.Name)
			return nil, nil
		}
		podIPs = getIPFromIPNetFn(logicalPort.ips)
	} else { // means this is the egress node's local controller
		if ni.IsDefault() {
			podIPNets, err := util.GetPodCIDRsWithFullMask(pod, ni, nil)
			if err != nil {
				return nil, fmt.Errorf("failed to get pod %s/%s IP: %v", pod.Namespace, pod.Name, err)
			}
			if len(podIPNets) == 0 {
				return nil, fmt.Errorf("failed to get pod %s/%s IPs", pod.Namespace, pod.Name)
			}
			podIPs = getIPFromIPNetFn(podIPNets)
		} else if ni.IsUserDefinedNetwork() {
			podIPNets := util.GetPodCIDRsWithFullMaskOfNetwork(pod, nadKey)
			if len(podIPNets) == 0 {
				return nil, fmt.Errorf("failed to get pod %s/%s IPs", pod.Namespace, pod.Name)
			}
			podIPs = getIPFromIPNetFn(podIPNets)
		}
	}
	return podIPs, nil
}

func createEgressIPLogicalRouterPolicy(nbClient libovsdbclient.Client, clusterRouter, match string, priority int, options map[string]string, dbIDs *libovsdbops.DbObjectIDs) error {
	lrp := nbdb.LogicalRouterPolicy{
		Priority:    priority,
		Action:      nbdb.LogicalRouterPolicyActionAllow,
		Match:       match,
		ExternalIDs: dbIDs.GetExternalIDs(),
		Options:     options,
	}
	p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
	externalIDs := dbIDs.GetExternalIDs()
	objectType, ok := externalIDs[libovsdbops.ObjectNameKey.String()]
	if !ok {
		return fmt.Errorf("failed to find Name key for DB ID (%q)", dbIDs.String())
	}
	// if multiple cluster subnets, we will retrieve multiple LRPs if using a predicate generated from DB IDs alone,
	// therefore filter by match and options also only for pod to pod and pod to join LRPs.
	if objectType == string(NoReRoutePodToJoin) || objectType == string(NoReRoutePodToPod) {
		p = libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == match
		})
	}

	err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(nbClient, clusterRouter, &lrp, p)
	if err != nil {
		return fmt.Errorf("error creating logical router policy %+v on router %s: %v", lrp, clusterRouter, err)
	}
	return nil
}

func (e *EgressIPController) buildSNATFromEgressIPStatus(ni util.NetInfo, podIP net.IP, status egressipv1.EgressIPStatusItem, egressIPName, podNamespace, podName string) (*nbdb.NAT, error) {
	logicalIP := &net.IPNet{
		IP:   podIP,
		Mask: util.GetIPFullMask(podIP),
	}
	ipFamily := IPFamilyValueV4
	if utilnet.IsIPv6CIDR(logicalIP) {
		ipFamily = IPFamilyValueV6
	}
	externalIP := net.ParseIP(status.EgressIP)
	logicalPort := ni.GetNetworkScopedK8sMgmtIntfName(status.Node)
	externalIds := getEgressIPNATDbIDs(egressIPName, podNamespace, podName, ipFamily, e.controllerName).GetExternalIDs()
	nat := libovsdbops.BuildSNAT(&externalIP, logicalIP, logicalPort, externalIds)
	return nat, nil
}

func (e *EgressIPController) createNATRuleOps(ni util.NetInfo, ops []ovsdb.Operation, podIPs []*net.IPNet, status egressipv1.EgressIPStatusItem,
	egressIPName, podNamespace, podName string) ([]ovsdb.Operation, error) {
	nats := make([]*nbdb.NAT, 0, len(podIPs))
	var nat *nbdb.NAT
	var err error
	for _, podIP := range podIPs {
		if (utilnet.IsIPv6String(status.EgressIP) && utilnet.IsIPv6(podIP.IP)) || (!utilnet.IsIPv6String(status.EgressIP) && !utilnet.IsIPv6(podIP.IP)) {
			nat, err = e.buildSNATFromEgressIPStatus(ni, podIP.IP, status, egressIPName, podNamespace, podName)
			if err != nil {
				return nil, err
			}
			nats = append(nats, nat)
		}
	}
	gwRouter := &nbdb.LogicalRouter{
		Name: ni.GetNetworkScopedGWRouterName(status.Node),
	}
	ops, err = libovsdbops.CreateOrUpdateNATsOps(e.nbClient, ops, gwRouter, nats...)
	if err != nil {
		return nil, fmt.Errorf("unable to create snat rules, for router: %s, error: %v", gwRouter.Name, err)
	}
	return ops, nil
}

func (e *EgressIPController) deleteNATRuleOps(ni util.NetInfo, ops []ovsdb.Operation, status egressipv1.EgressIPStatusItem,
	egressIPName, podNamespace, podName string) ([]ovsdb.Operation, error) {
	var err error
	filterByIP := func(n *nbdb.NAT) bool {
		return n.ExternalIP == status.EgressIP
	}
	pV4 := libovsdbops.GetPredicate[*nbdb.NAT](getEgressIPNATDbIDs(egressIPName, podNamespace, podName, IPFamilyValueV4, e.controllerName), filterByIP)
	pV6 := libovsdbops.GetPredicate[*nbdb.NAT](getEgressIPNATDbIDs(egressIPName, podNamespace, podName, IPFamilyValueV6, e.controllerName), filterByIP)
	router := &nbdb.LogicalRouter{
		Name: ni.GetNetworkScopedGWRouterName(status.Node),
	}
	ops, err = libovsdbops.DeleteNATsWithPredicateOps(e.nbClient, ops, pV4)
	if err != nil {
		return nil, fmt.Errorf("unable to remove SNAT IPv4 rules for router: %s, error: %v", router.Name, err)
	}
	ops, err = libovsdbops.DeleteNATsWithPredicateOps(e.nbClient, ops, pV6)
	if err != nil {
		return nil, fmt.Errorf("unable to remove SNAT IPv6 rules for router: %s, error: %v", router.Name, err)
	}
	return ops, nil
}

// getNetworkFromPodAssignment attempts to find a pods network from the pod assignment cache. If pod is not found
// in cache or no network set, nil network is return.
// must be called with podAssignment lock on podKey
func (e *EgressIPController) getNetworkFromPodAssignment(podKey string) util.NetInfo {
	podAssignment, ok := e.podAssignment.Load(podKey)
	if !ok {
		return nil
	}
	return podAssignment.network
}

func ensureDefaultNoRerouteUDNEnabledSvcPolicies(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory,
	ni util.NetInfo, controllerName, routerName string, v4, v6 bool) error {
	var err error
	var as addressset.AddressSet
	// fetch the egressIP pods address-set
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, ni.GetNetworkName(), controllerName)
	if as, err = addressSetFactory.GetAddressSet(dbIDs); err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", EgressIPServedPodsAddrSetName, err)
	}
	ipv4EgressIPServedPodsAS, ipv6EgressIPServedPodsAS := as.GetASHashNames()

	// fetch the egressService pods address-set
	dbIDs = egresssvc.GetEgressServiceAddrSetDbIDs(controllerName)
	if as, err = addressSetFactory.GetAddressSet(dbIDs); err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", egresssvc.EgressServiceServedPodsAddrSetName, err)
	}
	ipv4EgressServiceServedPodsAS, ipv6EgressServiceServedPodsAS := as.GetASHashNames()

	dbIDs = udnenabledsvc.GetAddressSetDBIDs()
	var ipv4UDNEnabledSvcAS, ipv6UDNEnabledSvcAS string
	// address set maybe not created immediately
	err = wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 5*time.Second, true, func(_ context.Context) (done bool, err error) {

		as, err := addressSetFactory.GetAddressSet(dbIDs)
		if err != nil {
			klog.V(5).Infof("Failed to get UDN enabled service address set, retrying: %v", err)
			return false, nil
		}
		ipv4UDNEnabledSvcAS, ipv6UDNEnabledSvcAS = as.GetASHashNames()
		if ipv4UDNEnabledSvcAS == "" && ipv6UDNEnabledSvcAS == "" { // only one IP family is required
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("failed to retrieve UDN enabled service address set from NB DB: %v", err)
	}
	// if address set hash name is empty, the address set has yet to be created
	if (v4 && ipv4UDNEnabledSvcAS == "") || (v6 && ipv6UDNEnabledSvcAS == "") {
		return types.NewSuppressedError(fmt.Errorf("failed to retrieve UDN enabled service address set"))
	}
	var matchV4, matchV6 string
	// construct the policy match
	if v4 {
		matchV4 = fmt.Sprintf(`(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s`,
			ipv4EgressIPServedPodsAS, ipv4EgressServiceServedPodsAS, ipv4UDNEnabledSvcAS)
	}
	if v6 {
		if ipv6EgressIPServedPodsAS == "" || ipv6EgressServiceServedPodsAS == "" || ipv6UDNEnabledSvcAS == "" {
			return fmt.Errorf("address set hash name(s) %s not found", as.GetName())
		}
		matchV6 = fmt.Sprintf(`(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s`,
			ipv6EgressIPServedPodsAS, ipv6EgressServiceServedPodsAS, ipv6UDNEnabledSvcAS)
	}

	// Create global allow policy for UDN enabled service traffic
	if v4 && matchV4 != "" {
		dbIDs = getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, NoReRouteUDNPodToCDNSvc, IPFamilyValueV4, ni.GetNetworkName(), controllerName)
		if err := createEgressIPLogicalRouterPolicy(nbClient, routerName, matchV4, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv4 no-rerouteUDN pod to CDN svc, err: %v", err)
		}
	}

	if v6 && matchV6 != "" {
		dbIDs = getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, NoReRouteUDNPodToCDNSvc, IPFamilyValueV6, ni.GetNetworkName(), controllerName)
		if err := createEgressIPLogicalRouterPolicy(nbClient, routerName, matchV6, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv6 no-reroute UDN pod to CDN svc policies, err: %v", err)
		}
	}
	return nil
}

func getPodKey(pod *corev1.Pod) string {
	return fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
}

func getPodNamespaceAndNameFromKey(podKey string) (string, string) {
	parts := strings.Split(podKey, "_")
	return parts[0], parts[1]
}

func getEgressIPPktMark(eipName string, annotations map[string]string) util.EgressIPMark {
	var err error
	var mark util.EgressIPMark
	if isEgressIPForUDNSupported() && util.IsEgressIPMarkSet(annotations) {
		mark, err = util.ParseEgressIPMark(annotations)
		if err != nil {
			klog.Errorf("Failed to get EgressIP %s packet mark from annotations: %v", eipName, err)
		}
	}
	return mark
}

func getPodIPFromEIPSNATMarkMatch(match string) string {
	//format ${IP family}.src == ${pod IP}
	if match == "" {
		return ""
	}
	matchSplit := strings.Split(match, " ")
	if len(matchSplit) != 3 {
		return ""
	}
	return matchSplit[2]
}

func getEIPIPFamily(isIPv6 bool) egressIPFamilyValue {
	if isIPv6 {
		return IPFamilyValueV6
	}
	return IPFamilyValueV4
}

func addPktMarkToLRPOptions(options map[string]string, mark string) {
	options["pkt_mark"] = mark
}

// getTopologyScopedRouterName returns the router name that we attach polices to support EgressIP depending on network topology
// For Layer 3, we return the network scoped OVN "cluster router" name. For layer 2, we return a Nodes network scoped OVN gateway router name.
func getTopologyScopedRouterName(ni util.NetInfo, nodeName string) (string, error) {
	if ni.TopologyType() == types.Layer2Topology && !config.Layer2UsesTransitRouter {
		if nodeName == "" {
			return "", fmt.Errorf("node name is required to determine the Nodes gateway router name")
		}
		return ni.GetNetworkScopedGWRouterName(nodeName), nil
	}
	return ni.GetNetworkScopedClusterRouterName(), nil
}

func (e *EgressIPController) getTopologyScopedLocalRouterName(ni util.NetInfo) (string, error) {
	routerName := ni.GetNetworkScopedClusterRouterName()
	if ni.TopologyType() == types.Layer2Topology && !config.Layer2UsesTransitRouter {
		// Layer 2 without a transit router uses this controller's node-scoped gateway router.
		localNodeName, err := e.getLocalNodeName()
		if err != nil {
			return "", err
		}
		routerName = ni.GetNetworkScopedGWRouterName(localNodeName)
	}
	return routerName, nil
}

func isEgressIPForUDNSupported() bool {
	return util.IsNetworkSegmentationSupportEnabled()
}
