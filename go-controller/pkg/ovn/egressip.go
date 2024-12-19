package ovn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	networkmanager "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	egresssvc "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/egressservice"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	corev1 "k8s.io/api/core/v1"
	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type egressIPAddrSetName string
type egressIPFamilyValue string
type egressIPNoReroutePolicyName string
type egressIPQoSRuleName string

const (
	NodeIPAddrSetName             egressIPAddrSetName = "node-ips"
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

// EgressIPController configures OVN to support EgressIP
type EgressIPController struct {
	// libovsdb northbound client interface
	nbClient     libovsdbclient.Client
	kube         *kube.KubeOVN
	watchFactory *factory.WatchFactory
	// event recorder used to post events to k8s
	recorder record.EventRecorder
	// podAssignmentMutex is used to ensure safe access to podAssignment.
	// Currently WatchEgressIP, WatchEgressNamespace and WatchEgressPod could
	// all access that map simultaneously, hence why this guard is needed.
	podAssignmentMutex *sync.Mutex
	// nodeUpdateMutex is used for two reasons:
	// (1) to ensure safe handling of node ip address updates. VIP addresses are
	// dynamic and might move across nodes.
	// (2) used in ensureDefaultNoRerouteQoSRules function to ensure
	// creating QoS rules is thread safe since otherwise when two nodes are added
	// at the same time by two different threads we end up creating duplicate
	// QoS rules in database due to libovsdb cache race
	nodeUpdateMutex *sync.Mutex
	// podAssignment is a cache used for keeping track of which egressIP status
	// has been setup for each pod. The key is defined by getPodKey
	podAssignment map[string]*podAssignmentState
	// logicalPortCache allows access to pod IPs for all networks
	logicalPortCache *PortCache
	// A cache that maintains all nodes in the cluster,
	// value will be true if local to this zone and false otherwise
	nodeZoneState *syncmap.SyncMap[bool]
	// networkManager used for getting network information for UDNs
	networkManager networkmanager.Interface
	// An address set factory that creates address sets
	addressSetFactory addressset.AddressSetFactory
	// Northbound database zone name to which this Controller is connected to - aka local zone
	zone string
	v4   bool
	v6   bool
	// controllerName is the name of the controller. For backward compatibility reasons, this is the default network controller name.
	controllerName string
}

func NewEIPController(
	nbClient libovsdbclient.Client,
	kube *kube.KubeOVN,
	watchFactory *factory.WatchFactory,
	recorder record.EventRecorder,
	portCache *PortCache,
	networkmanager networkmanager.Interface,
	addressSetFactor addressset.AddressSetFactory,
	v4 bool,
	v6 bool,
	zone string,
	controllerName string,
) *EgressIPController {
	e := &EgressIPController{
		nbClient:           nbClient,
		kube:               kube,
		watchFactory:       watchFactory,
		recorder:           recorder,
		podAssignmentMutex: &sync.Mutex{},
		nodeUpdateMutex:    &sync.Mutex{},
		podAssignment:      map[string]*podAssignmentState{},
		logicalPortCache:   portCache,
		nodeZoneState:      syncmap.NewSyncMap[bool](),
		controllerName:     controllerName,
		networkManager:     networkmanager,
		addressSetFactory:  addressSetFactor,
		zone:               zone,
		v4:                 v4,
		v6:                 v6,
	}
	return e
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
// NOTE: `Spec.EgressIPs“ updates for EIP object are not processed here, that is the job of cluster manager
//
//	We only care about `Spec.NamespaceSelector`, `Spec.PodSelector` and `Status` field
func (e *EgressIPController) reconcileEgressIP(old, new *egressipv1.EgressIP) (err error) {
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
					ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
					if err != nil {
						return fmt.Errorf("failed to get active network for namespace %s: %v", namespace.Name, err)
					}
					if err := e.deleteNamespaceEgressIPAssignment(ni, oldEIP.Name, oldEIP.Status.Items, namespace, oldEIP.Spec.PodSelector); err != nil {
						return fmt.Errorf("network %s: failed to delete namespace %s egress IP config: %v", ni.GetNetworkName(), namespace.Name, err)
					}
				}
				if newNamespaceSelector.Matches(namespaceLabels) && !oldNamespaceSelector.Matches(namespaceLabels) {
					ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
					if err != nil {
						return fmt.Errorf("failed to get active network for namespace %s: %v", namespace.Name, err)
					}
					if err := e.addNamespaceEgressIPAssignments(ni, newEIP.Name, newEIP.Status.Items, mark, namespace, newEIP.Spec.PodSelector); err != nil {
						return fmt.Errorf("network %s: failed to add namespace %s egress IP config: %v", ni.GetNetworkName(), namespace.Name, err)
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
						ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
						if err != nil {
							return fmt.Errorf("failed to get active network for namespace %s: %v", namespace.Name, err)
						}
						if err := e.deletePodEgressIPAssignmentsWithCleanup(ni, oldEIP.Name, oldEIP.Status.Items, pod); err != nil {
							return fmt.Errorf("network %s: failed to delete pod %s/%s egress IP config: %v", ni.GetNetworkName(), pod.Namespace, pod.Name, err)
						}
					}
					if util.PodCompleted(pod) {
						continue
					}
					if newPodSelector.Matches(podLabels) && !oldPodSelector.Matches(podLabels) {
						ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
						if err != nil {
							return fmt.Errorf("failed to get active network for namespace %s: %v", namespace.Name, err)
						}
						if err := e.addPodEgressIPAssignmentsWithLock(ni, newEIP.Name, newEIP.Status.Items, mark, pod); err != nil {
							return fmt.Errorf("network %s: failed to add pod %s/%s egress IP config: %v", ni.GetNetworkName(), pod.Namespace, pod.Name, err)
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
				ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
				if err != nil {
					return fmt.Errorf("failed to get active network for namespace %s: %v", namespace.Name, err)
				}
				if !newNamespaceSelector.Matches(namespaceLabels) && oldNamespaceSelector.Matches(namespaceLabels) {
					if err := e.deleteNamespaceEgressIPAssignment(ni, oldEIP.Name, oldEIP.Status.Items, namespace, oldEIP.Spec.PodSelector); err != nil {
						return fmt.Errorf("network %s: failed to delete namespace %s egress IP config: %v", ni.GetNetworkName(), namespace.Name, err)
					}
				}
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
							if err := e.addPodEgressIPAssignmentsWithLock(ni, newEIP.Name, newEIP.Status.Items, mark, pod); err != nil {
								return fmt.Errorf("network %s: failed to add pod %s/%s egress IP config: %v", ni.GetNetworkName(), pod.Namespace, pod.Name, err)
							}
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
							if err := e.deletePodEgressIPAssignmentsWithCleanup(ni, oldEIP.Name, oldEIP.Status.Items, pod); err != nil {
								return fmt.Errorf("network %s: failed to delete pod %s/%s egress IP config: %v", ni.GetNetworkName(), pod.Namespace, pod.Name, err)
							}
						}
						if util.PodCompleted(pod) {
							continue
						}
						if newPodSelector.Matches(podLabels) && !oldPodSelector.Matches(podLabels) {
							if err := e.addPodEgressIPAssignmentsWithLock(ni, newEIP.Name, newEIP.Status.Items, mark, pod); err != nil {
								return fmt.Errorf("network %s: failed to add pod %s/%s egress IP config: %v", ni.GetNetworkName(), pod.Namespace, pod.Name, err)
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// reconcileEgressIPNamespace reconciles the database configuration setup in nbdb
// based on received namespace objects.
// NOTE: we only care about namespace label updates
func (e *EgressIPController) reconcileEgressIPNamespace(old, new *corev1.Namespace) error {
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
	for _, egressIP := range egressIPs {
		namespaceSelector, err := metav1.LabelSelectorAsSelector(&egressIP.Spec.NamespaceSelector)
		if err != nil {
			return err
		}
		if namespaceSelector.Matches(oldLabels) && !namespaceSelector.Matches(newLabels) {
			ni, err := e.networkManager.GetActiveNetworkForNamespace(namespaceName)
			if err != nil {
				return fmt.Errorf("failed to get active network for namespace %s: %v", namespaceName, err)
			}
			if err := e.deleteNamespaceEgressIPAssignment(ni, egressIP.Name, egressIP.Status.Items, oldNamespace, egressIP.Spec.PodSelector); err != nil {
				return fmt.Errorf("network %s: failed to delete namespace %s egress IP config: %v", ni.GetNetworkName(), namespaceName, err)
			}
		}
		if !namespaceSelector.Matches(oldLabels) && namespaceSelector.Matches(newLabels) {
			mark := getEgressIPPktMark(egressIP.Name, egressIP.Annotations)
			ni, err := e.networkManager.GetActiveNetworkForNamespace(namespaceName)
			if err != nil {
				return fmt.Errorf("failed to get active network for namespace %s: %v", namespaceName, err)
			}
			if err := e.addNamespaceEgressIPAssignments(ni, egressIP.Name, egressIP.Status.Items, mark, newNamespace, egressIP.Spec.PodSelector); err != nil {
				return fmt.Errorf("network %s: failed to add namespace %s egress IP config: %v", ni.GetNetworkName(), namespaceName, err)
			}
		}
	}
	return nil
}

// reconcileEgressIPPod reconciles the database configuration setup in nbdb
// based on received pod objects.
// NOTE: we only care about pod label updates
func (e *EgressIPController) reconcileEgressIPPod(old, new *corev1.Pod) (err error) {
	oldPod, newPod := &corev1.Pod{}, &corev1.Pod{}
	namespace := &corev1.Namespace{}
	if old != nil {
		oldPod = old
		namespace, err = e.watchFactory.GetNamespace(oldPod.Namespace)
		if err != nil {
			// when the whole namespace gets removed, we can ignore the NotFound error here
			// any potential configuration will get removed in reconcileEgressIPNamespace
			if new == nil && apierrors.IsNotFound(err) {
				klog.V(5).Infof("Namespace %s no longer exists for the deleted pod: %s", oldPod.Namespace, oldPod.Name)
				return nil
			}
			return err
		}
	}
	if new != nil {
		newPod = new
		namespace, err = e.watchFactory.GetNamespace(newPod.Namespace)
		if err != nil {
			return err
		}
	}

	newPodLabels := labels.Set(newPod.Labels)
	oldPodLabels := labels.Set(oldPod.Labels)

	// If the namespace the pod belongs to does not have any labels, just return
	// it can't match any EgressIP object
	namespaceLabels := labels.Set(namespace.Labels)
	if namespaceLabels.AsSelector().Empty() {
		return nil
	}

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
	for _, egressIP := range egressIPs {
		namespaceSelector, err := metav1.LabelSelectorAsSelector(&egressIP.Spec.NamespaceSelector)
		if err != nil {
			return err
		}
		if namespaceSelector.Matches(namespaceLabels) {
			// If the namespace the pod belongs to matches this object then
			// check the if there's a podSelector defined on the EgressIP
			// object. If there is one: the user intends the EgressIP object to
			// match only a subset of pods in the namespace, and we'll have to
			// check that. If there is no podSelector: the user intends it to
			// match all pods in the namespace.
			mark := getEgressIPPktMark(egressIP.Name, egressIP.Annotations)
			podSelector, err := metav1.LabelSelectorAsSelector(&egressIP.Spec.PodSelector)
			if err != nil {
				return err
			}
			ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
			if err != nil {
				return fmt.Errorf("failed to get active network for namespace %s: %v", namespace.Name, err)
			}
			if !podSelector.Empty() {
				// Use "new" and "old" instead of "newPod" and "oldPod" to determine whether
				// pods was created or is being deleted.
				newMatches := new != nil && podSelector.Matches(newPodLabels)
				oldMatches := old != nil && podSelector.Matches(oldPodLabels)
				// If the podSelector doesn't match the pod, then continue
				// because this EgressIP intends to match other pods in that
				// namespace and not this one. Other EgressIP objects might
				// match the pod though so we need to check that.
				if !newMatches && !oldMatches {
					continue
				}
				// Check if the pod stopped matching. If the pod was deleted,
				// "new" will be nil, so this must account for that case.
				if !newMatches && oldMatches {
					if err := e.deletePodEgressIPAssignmentsWithCleanup(ni, egressIP.Name, egressIP.Status.Items, oldPod); err != nil {
						return fmt.Errorf("network %s: failed to delete pod %s/%s egress IP config: %v", ni.GetNetworkName(), oldPod.Namespace, oldPod.Name, err)
					}
					continue
				}
				// If the pod starts matching the podSelector or continues to
				// match: add the pod. The reason as to why we need to continue
				// adding it if it continues to match, as opposed to once when
				// it started matching, is because the pod might not have pod
				// IPs assigned at that point and we need to continue trying the
				// pod setup for every pod update as to make sure we process the
				// pod IP assignment.
				if err := e.addPodEgressIPAssignmentsWithLock(ni, egressIP.Name, egressIP.Status.Items, mark, newPod); err != nil {
					return fmt.Errorf("network %s: failed to add pod %s/%s egress IP config: %v", ni.GetNetworkName(), newPod.Namespace, newPod.Name, err)
				}
				continue
			}
			// If the podSelector is empty (i.e: the EgressIP object is intended
			// to match all pods in the namespace) and the pod has been deleted:
			// "new" will be nil and we need to remove the setup
			if new == nil {
				if err := e.deletePodEgressIPAssignmentsWithCleanup(ni, egressIP.Name, egressIP.Status.Items, oldPod); err != nil {
					return fmt.Errorf("network %s: failed to delete pod %s/%s egress IP config: %v", ni.GetNetworkName(), oldPod.Namespace, oldPod.Name, err)
				}
				continue
			}
			// For all else, perform a setup for the pod
			if err := e.addPodEgressIPAssignmentsWithLock(ni, egressIP.Name, egressIP.Status.Items, mark, newPod); err != nil {
				return fmt.Errorf("network %s: failed to add pod %s/%s egress IP config: %v", ni.GetNetworkName(), newPod.Namespace, newPod.Name, err)
			}
		}
	}
	return nil
}

// main reconcile functions end here and local zone controller functions begin

func (e *EgressIPController) addEgressIPAssignments(name string, statusAssignments []egressipv1.EgressIPStatusItem, mark util.EgressIPMark, namespaceSelector, podSelector metav1.LabelSelector) error {
	namespaces, err := e.watchFactory.GetNamespacesBySelector(namespaceSelector)
	if err != nil {
		return err
	}
	for _, namespace := range namespaces {
		ni, err := e.networkManager.GetActiveNetworkForNamespace(namespace.Name)
		if err != nil {
			return fmt.Errorf("failed to get active network for namespace %s: %v", namespace.Name, err)
		}
		if err := e.addNamespaceEgressIPAssignments(ni, name, statusAssignments, mark, namespace, podSelector); err != nil {
			return err
		}
	}
	return nil
}

func (e *EgressIPController) addNamespaceEgressIPAssignments(ni util.NetInfo, name string, statusAssignments []egressipv1.EgressIPStatusItem, mark util.EgressIPMark,
	namespace *kapi.Namespace, podSelector metav1.LabelSelector) error {
	var pods []*kapi.Pod
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
	for _, pod := range pods {
		if err := e.addPodEgressIPAssignmentsWithLock(ni, name, statusAssignments, mark, pod); err != nil {
			return err
		}
	}
	return nil
}

func (e *EgressIPController) addPodEgressIPAssignmentsWithLock(ni util.NetInfo, name string, statusAssignments []egressipv1.EgressIPStatusItem, mark util.EgressIPMark, pod *kapi.Pod) error {
	e.deletePreviousNetworkPodEgressIPAssignments(ni, name, statusAssignments, pod)
	e.podAssignmentMutex.Lock()
	defer e.podAssignmentMutex.Unlock()
	return e.addPodEgressIPAssignments(ni, name, statusAssignments, mark, pod)
}

// addPodEgressIPAssignments tracks the setup made for each egress IP matching
// pod w.r.t to each status. This is mainly done to avoid a lot of duplicated
// work on ovnkube-master restarts when all egress IP handlers will most likely
// match and perform the setup for the same pod and status multiple times over.
func (e *EgressIPController) addPodEgressIPAssignments(ni util.NetInfo, name string, statusAssignments []egressipv1.EgressIPStatusItem, mark util.EgressIPMark, pod *kapi.Pod) error {
	podKey := getPodKey(pod)
	// If pod is already in succeeded or failed state, return it without proceeding further.
	if util.PodCompleted(pod) {
		klog.Infof("Pod %s is already in completed state, skipping egress ip assignment", podKey)
		return nil
	}
	// If statusAssignments is empty just return, not doing this will delete the
	// external GW set up, even though there might be no egress IP set up to
	// perform.
	if len(statusAssignments) == 0 {
		return nil
	}
	// We need to proceed with add only under two conditions
	// 1) egressNode present in at least one status is local to this zone
	// (NOTE: The relation between egressIPName and nodeName is 1:1 i.e in the same object the given node will be present only in one status)
	// 2) the pod being added is local to this zone
	proceed := false
	for _, status := range statusAssignments {
		e.nodeZoneState.LockKey(status.Node)
		isLocalZoneEgressNode, loadedEgressNode := e.nodeZoneState.Load(status.Node)
		if loadedEgressNode && isLocalZoneEgressNode {
			proceed = true
			e.nodeZoneState.UnlockKey(status.Node)
			break
		}
		e.nodeZoneState.UnlockKey(status.Node)
	}
	if !proceed && !e.isPodScheduledinLocalZone(pod) {
		return nil // nothing to do if none of the status nodes are local to this master and pod is also remote
	}
	var remainingAssignments []egressipv1.EgressIPStatusItem
	nadName := ni.GetNetworkName()
	if ni.IsSecondary() {
		nadNames := ni.GetNADs()
		if len(nadNames) == 0 {
			return fmt.Errorf("expected at least one NAD name for Namespace %s", pod.Namespace)
		}
		nadName = nadNames[0] // there should only be one active network
	}
	podIPNets, err := e.getPodIPs(ni, pod, nadName)
	if err != nil {
		return fmt.Errorf("failed to get pod %s/%s IPs: %v", pod.Namespace, pod.Name, err)
	}
	if len(podIPNets) == 0 {
		return fmt.Errorf("failed to get pod ips for pod %s on network %s with NAD name %s", podKey, ni.GetNetworkName(), nadName)
	}
	podIPs := make([]net.IP, 0, len(podIPNets))
	for _, ipNet := range podIPNets {
		podIPs = append(podIPs, ipNet.IP)
	}
	podState, exists := e.podAssignment[podKey]
	if !exists {
		remainingAssignments = statusAssignments
		podState = &podAssignmentState{
			egressIPName:         name,
			egressStatuses:       egressStatuses{make(map[egressipv1.EgressIPStatusItem]string)},
			standbyEgressIPNames: sets.New[string](),
			podIPs:               podIPs,
			network:              ni,
		}
		e.podAssignment[podKey] = podState
	} else if podState.egressIPName == name || podState.egressIPName == "" {
		// We do the setup only if this egressIP object is the one serving this pod OR
		// podState.egressIPName can be empty if no re-routes were found in
		// syncPodAssignmentCache for the existing pod, we will treat this case as a new add
		for _, status := range statusAssignments {
			if exists := podState.egressStatuses.contains(status); !exists {
				remainingAssignments = append(remainingAssignments, status)
			}
		}
		podState.podIPs = podIPs
		podState.egressIPName = name
		podState.network = ni
		podState.standbyEgressIPNames.Delete(name)
	} else if podState.egressIPName != name {
		klog.Warningf("EgressIP object %s will not be configured for pod %s "+
			"since another egressIP object %s is serving it", name, podKey, podState.egressIPName)
		eIPRef := kapi.ObjectReference{
			Kind: "EgressIP",
			Name: name,
		}
		e.recorder.Eventf(
			&eIPRef,
			kapi.EventTypeWarning,
			"UndefinedRequest",
			"EgressIP object %s will not be configured for pod %s since another egressIP object %s is serving it, this is undefined", name, podKey, podState.egressIPName,
		)
		podState.standbyEgressIPNames.Insert(name)
		return nil
	}
	for _, status := range remainingAssignments {
		klog.V(2).Infof("Adding pod egress IP status: %v for EgressIP: %s and pod: %s/%s/%v", status, name, pod.Namespace, pod.Name, podIPNets)
		err = e.nodeZoneState.DoWithLock(status.Node, func(key string) error {
			if status.Node == pod.Spec.NodeName {
				// we are safe, no need to grab lock again
				if err := e.addPodEgressIPAssignment(ni, name, status, mark, pod, podIPNets); err != nil {
					return fmt.Errorf("unable to create egressip configuration for pod %s/%s/%v, err: %w", pod.Namespace, pod.Name, podIPNets, err)
				}
				podState.egressStatuses.statusMap[status] = ""
				return nil
			}
			return e.nodeZoneState.DoWithLock(pod.Spec.NodeName, func(key string) error {
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
	if e.isPodScheduledinLocalZone(pod) {
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
	e.podAssignmentMutex.Lock()
	defer e.podAssignmentMutex.Unlock()

	for _, statusToRemove := range statusesToRemove {
		processedNetworks := make(map[string]struct{})
		for podKey, podStatus := range e.podAssignment {
			if podStatus.egressIPName != name {
				// we can continue here since this pod was not managed by this EIP object
				podStatus.standbyEgressIPNames.Delete(name)
				continue
			}
			if ok := podStatus.egressStatuses.contains(statusToRemove); !ok {
				// we can continue here since this pod was not managed by this statusToRemove
				continue
			}
			podNamespace, podName := getPodNamespaceAndNameFromKey(podKey)
			ni, err := e.networkManager.GetActiveNetworkForNamespace(podNamespace)
			if err != nil {
				return fmt.Errorf("failed to get active network for namespace %s", podNamespace)
			}
			cachedNetwork := e.getNetworkFromPodAssignment(podKey)
			err = e.nodeZoneState.DoWithLock(statusToRemove.Node, func(key string) error {
				// this statusToRemove was managing at least one pod, hence let's tear down the setup for this status
				if _, ok := processedNetworks[ni.GetNetworkName()]; !ok {
					klog.V(2).Infof("Deleting pod egress IP status: %v for EgressIP: %s", statusToRemove, name)
					if err := e.deleteEgressIPStatusSetup(ni, name, statusToRemove); err != nil {
						return fmt.Errorf("failed to delete EgressIP %s status setup for network %s: %v", name, ni.GetNetworkName(), err)
					}
					if cachedNetwork != nil && util.AreNetworksCompatible(cachedNetwork, ni) {
						if err := e.deleteEgressIPStatusSetup(cachedNetwork, name, statusToRemove); err != nil {
							klog.Errorf("Failed to delete EgressIP %s status setup for network %s: %v", name, cachedNetwork.GetNetworkName(), err)
						}
					}
				}
				processedNetworks[ni.GetNetworkName()] = struct{}{}
				// this pod was managed by statusToRemove.EgressIP; we need to try and add its SNAT back towards nodeIP
				if err := e.addExternalGWPodSNAT(ni, podNamespace, podName, statusToRemove); err != nil {
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
				// NOTE(tssurya): There is no way to infer if pod was local to this zone or not,
				// so we try to nuke the IP from address-set anyways - it will be a no-op for remote pods
				if err := e.deletePodIPsFromAddressSet(ni.GetNetworkName(), e.controllerName, podStatus.podIPs...); err != nil {
					return fmt.Errorf("cannot delete egressPodIPs for the pod %s from the address set: err: %v", podKey, err)
				}
				delete(e.podAssignment, podKey)
			} else if len(podStatus.egressStatuses.statusMap) == 0 && len(podStatus.standbyEgressIPNames) > 0 {
				klog.V(2).Infof("Pod %s has standby egress IP %+v", podKey, podStatus.standbyEgressIPNames.UnsortedList())
				podStatus.egressIPName = "" // we have deleted the current egressIP that was managing the pod
				if err := e.addStandByEgressIPAssignment(ni, podKey, podStatus); err != nil {
					klog.Errorf("Adding standby egressIPs for pod %s with status %v failed: %v", podKey, podStatus, err)
					// We are not returning the error on purpose, this will be best effort without any retries because
					// retrying deleteEgressIPAssignments for original EIP because addStandByEgressIPAssignment failed is useless.
					// Since we delete statusToRemove from podstatus.egressStatuses the first time we call this function,
					// later when the operation is retried we will never continue down the loop
					// since we add SNAT to node only if this pod was managed by statusToRemove
					return nil
				}
			}
		}
	}
	return nil
}

func (e *EgressIPController) deleteNamespaceEgressIPAssignment(ni util.NetInfo, name string, statusAssignments []egressipv1.EgressIPStatusItem, namespace *kapi.Namespace, podSelector metav1.LabelSelector) error {
	var pods []*kapi.Pod
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
	for _, pod := range pods {
		if err := e.deletePodEgressIPAssignmentsWithCleanup(ni, name, statusAssignments, pod); err != nil {
			return fmt.Errorf("failed to delete EgressIP %s assignment for pod %s/%s attached to network %s: %v",
				name, pod.Namespace, pod.Name, ni.GetNetworkName(), err)
		}
	}
	return nil
}

func (e *EgressIPController) deletePodEgressIPAssignmentsWithCleanup(ni util.NetInfo, name string, statusesToRemove []egressipv1.EgressIPStatusItem, pod *kapi.Pod) error {
	e.deletePreviousNetworkPodEgressIPAssignments(ni, name, statusesToRemove, pod)
	return e.deletePodEgressIPAssignments(ni, name, statusesToRemove, pod)
}

func (e *EgressIPController) deletePodEgressIPAssignments(ni util.NetInfo, name string, statusesToRemove []egressipv1.EgressIPStatusItem, pod *kapi.Pod) error {
	e.podAssignmentMutex.Lock()
	defer e.podAssignmentMutex.Unlock()
	podKey := getPodKey(pod)
	podStatus, exists := e.podAssignment[podKey]
	if !exists {
		return nil
	} else if podStatus.egressIPName != name {
		// standby egressIP no longer matches this pod, update cache
		podStatus.standbyEgressIPNames.Delete(name)
		return nil
	}
	for _, statusToRemove := range statusesToRemove {
		if ok := podStatus.egressStatuses.contains(statusToRemove); !ok {
			// we can continue here since this pod was not managed by this statusToRemove
			continue
		}
		klog.V(2).Infof("Deleting pod egress IP status: %v for EgressIP: %s and pod: %s/%s", statusToRemove, name, pod.Name, pod.Namespace)
		err := e.nodeZoneState.DoWithLock(statusToRemove.Node, func(key string) error {
			if statusToRemove.Node == pod.Spec.NodeName {
				// we are safe, no need to grab lock again
				if err := e.deletePodEgressIPAssignment(ni, name, statusToRemove, pod); err != nil {
					return err
				}
				podStatus.egressStatuses.delete(statusToRemove)
				return nil
			}
			return e.nodeZoneState.DoWithLock(pod.Spec.NodeName, func(key string) error {
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
	// Delete the key if there are no more status assignments to keep
	// for the pod.
	if len(podStatus.egressStatuses.statusMap) == 0 {
		// pod could be managed by more than one egressIP
		// so remove the podKey from cache only if we are sure
		// there are no more egressStatuses managing this pod
		klog.V(5).Infof("Deleting pod key %s from assignment cache", podKey)
		if e.isPodScheduledinLocalZone(pod) {
			if err := e.deletePodIPsFromAddressSet(ni.GetNetworkName(), e.controllerName, podStatus.podIPs...); err != nil {
				return fmt.Errorf("cannot delete egressPodIPs for the pod %s from the address set: err: %v", podKey, err)
			}
		}
		delete(e.podAssignment, podKey)
	}
	return nil
}

// deletePreviousNetworkPodEgressIPAssignments checks if the network changed and remove any stale config on the previous network.
func (e *EgressIPController) deletePreviousNetworkPodEgressIPAssignments(ni util.NetInfo, name string, statusesToRemove []egressipv1.EgressIPStatusItem, pod *corev1.Pod) {
	cachedNetwork := e.getNetworkFromPodAssignmentWithLock(getPodKey(pod))
	if cachedNetwork != nil {
		if util.AreNetworksCompatible(cachedNetwork, ni) {
			if err := e.deletePodEgressIPAssignments(cachedNetwork, name, statusesToRemove, pod); err != nil {
				// no error is returned because high probability network is deleted
				klog.Errorf("Failed to delete EgressIP %s assignment for pod %s/%s attached to network %s: %v",
					name, pod.Namespace, pod.Name, cachedNetwork.GetNetworkName(), err)
			}
		}
	}
}

// isPodScheduledinLocalZone returns true if
//   - e.localZoneNodes map is nil or
//   - if the pod.Spec.NodeName is in the e.localZoneNodes map
//
// false otherwise.
func (e *EgressIPController) isPodScheduledinLocalZone(pod *kapi.Pod) bool {
	if !config.OVNKubernetesFeature.EnableInterconnect {
		return true
	}
	isLocalZonePod := true

	if e.nodeZoneState != nil {
		if util.PodScheduled(pod) {
			if isLocal, ok := e.nodeZoneState.Load(pod.Spec.NodeName); ok {
				isLocalZonePod = isLocal
			}
		} else {
			isLocalZonePod = false
		}
	}
	return isLocalZonePod
}

// isLocalZoneNode returns true if the node is part of the local zone.
func (e *EgressIPController) isLocalZoneNode(node *kapi.Node) bool {
	/** HACK BEGIN **/
	// TODO(tssurya): Remove this HACK a few months from now. This has been added only to
	// minimize disruption for upgrades when moving to interconnect=true.
	// We want the legacy ovnkube-master to wait for remote ovnkube-node to
	// signal it using "k8s.ovn.org/remote-zone-migrated" annotation before
	// considering a node as remote when we upgrade from "global" (1 zone IC)
	// zone to multi-zone. This is so that network disruption for the existing workloads
	// is negligible and until the point where ovnkube-node flips the switch to connect
	// to the new SBDB, it would continue talking to the legacy RAFT ovnkube-sbdb to ensure
	// OVN/OVS flows are intact.
	if e.zone == types.OvnDefaultZone {
		return !util.HasNodeMigratedZone(node)
	}
	/** HACK END **/
	return util.GetNodeZone(node) == e.zone
}

type egressIPCache struct {
	// egressIP name -> network name -> cache
	egressIPNameToPods map[string]map[string]selectedPods
	// egressLocalNodes will contain all nodes that are local
	// to this zone which are serving this egressIP object..
	// This will help sync SNATs
	egressLocalNodesCache sets.Set[string]
	// egressIP IP -> assigned node name
	egressIPIPToNodeCache map[string]string
	// node name -> network name -> redirect IPs
	egressNodeRedirectsCache nodeNetworkRedirects
	// network name -> OVN cluster router name
	networkToRouter map[string]string
	// packet mark for primary secondary networks
	// EgressIP name -> mark
	markCache map[string]string
}

type nodeNetworkRedirects struct {
	// node name -> network name -> redirect IPs
	cache map[string]map[string]redirectIPs
}

type selectedPods struct {
	// egressLocalPods will contain all the pods that
	// are local to this zone being served by thie egressIP
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

func (e *EgressIPController) syncEgressIPs(namespaces []interface{}) error {
	// This part will take of syncing stale data which we might have in OVN if
	// there's no ovnkube-master running for a while, while there are changes to
	// pods/egress IPs.
	// It will sync:
	// - Egress IPs which have been deleted while ovnkube-master was down
	// - pods/namespaces which have stopped matching on egress IPs while
	//   ovnkube-master was down
	// - create an address-set that can hold all the egressIP pods and sync the address set by
	//   resetting pods that are managed by egressIPs based on the constructed kapi cache
	// This function is called when handlers for EgressIPNamespaceType are started
	// since namespaces is the first object that egressIP feature starts watching

	// update localZones cache of eIPCZoneController
	// WatchNodes() is called before WatchEgressIPNamespaces() so the oc.localZones cache
	// will be updated whereas WatchEgressNodes() is called after WatchEgressIPNamespaces()
	// and so we must update the cache to ensure we are not stale.
	// FIXME(martinkennelly): re-enable when EIP controller is fully extracted from DNC
	//if err := e.SyncLocalNodeZonesCache(); err != nil {
	//	return fmt.Errorf("SyncLocalNodeZonesCache unable to update the local zones node cache: %v", err)
	//}
	egressIPCache, err := e.generateCacheForEgressIP()
	if err != nil {
		return fmt.Errorf("syncEgressIPs unable to generate cache for egressip: %v", err)
	}
	if err = e.syncStaleEgressReroutePolicy(egressIPCache); err != nil {
		return fmt.Errorf("syncEgressIPs unable to remove stale reroute policies: %v", err)
	}
	if err = e.syncStaleSNATRules(egressIPCache); err != nil {
		return fmt.Errorf("syncEgressIPs unable to remove stale nats: %v", err)
	}
	if err = e.syncPodAssignmentCache(egressIPCache); err != nil {
		return fmt.Errorf("syncEgressIPs unable to sync internal pod assignment cache: %v", err)
	}
	if err = e.syncStaleAddressSetIPs(egressIPCache); err != nil {
		return fmt.Errorf("syncEgressIPs unable to reset stale address IPs: %v", err)
	}
	if err = e.syncStaleGWMarkRules(egressIPCache); err != nil {
		return fmt.Errorf("syncEgressIPs unable to sync GW packet mark rules: %v", err)
	}
	return nil
}

// SyncLocalNodeZonesCache iterates over all known Nodes and stores whether it is a local or remote OVN zone.
func (e *EgressIPController) SyncLocalNodeZonesCache() error {
	nodes, err := e.watchFactory.GetNodes()
	if err != nil {
		return fmt.Errorf("unable to fetch nodes from watch factory %w", err)
	}
	for _, node := range nodes {
		// NOTE: Even at this stage, there can be race; the bnc.zone might be the nodeName
		// while the node's annotations are not yet set, so it still shows global.
		// The EgressNodeType events (which are basically all node updates) should
		// constantly update this cache as nodes get added, updated and removed
		e.nodeZoneState.LockKey(node.Name)
		e.nodeZoneState.Store(node.Name, e.isLocalZoneNode(node))
		e.nodeZoneState.UnlockKey(node.Name)
	}
	return nil
}

// getALocalZoneNodeName fetches the first local OVN zone Node. Support for multiple Nodes per OVN zone is not supported
// and neither is changing a Nodes OVN zone. This function supports said assumptions.
func (e *EgressIPController) getALocalZoneNodeName() (string, error) {
	nodeNames := e.nodeZoneState.GetKeys()
	for _, nodeName := range nodeNames {
		if isLocal, ok := e.nodeZoneState.Load(nodeName); ok && isLocal {
			return nodeName, nil
		}
	}
	return "", fmt.Errorf("failed to find a local OVN zone Node")
}

func (e *EgressIPController) syncStaleAddressSetIPs(egressIPCache egressIPCache) error {
	for _, networkPodCache := range egressIPCache.egressIPNameToPods {
		for networkName, podCache := range networkPodCache {
			dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, networkName, e.controllerName)
			as, err := e.addressSetFactory.EnsureAddressSet(dbIDs)
			if err != nil {
				return fmt.Errorf("network %s: cannot ensure that addressSet for egressIP pods %s exists %v", networkName, EgressIPServedPodsAddrSetName, err)
			}
			var allEIPServedPodIPs []net.IP
			// we only care about local zone pods for the address-set since
			// traffic from remote pods towards nodeIP won't even reach this zone
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
func (e *EgressIPController) syncStaleGWMarkRules(egressIPCache egressIPCache) error {
	// Delete all stale LRPs then add missing LRPs
	// This func assumes one node per zone. It determines if an LRP is a valid local LRP. It doesn't determine if the
	// LRP is attached to the correct GW router
	if !isEgressIPForUDNSupported() {
		return nil
	}
	for _, networkPodCache := range egressIPCache.egressIPNameToPods {
		for networkName, podCache := range networkPodCache {
			// skip GW mark rules processing for CDN because they don't exist
			if networkName == types.DefaultNetworkName {
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
				// if theres no local egress nodes, the LRP must be invalid
				if egressIPCache.egressLocalNodesCache.Len() == 0 {
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
				// FIXME: not multi node per zone aware. Doesn't try to find out if the LRP is on the correct nodes GW router
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
			// gather local node names
			localNodeNames := make([]string, 0, 1)
			allNodes := e.nodeZoneState.GetKeys()
			for _, node := range allNodes {
				if isLocal, ok := e.nodeZoneState.Load(node); ok && isLocal {
					localNodeNames = append(localNodeNames, node)
				}
			}
			invalidLRPPredicate = func(item *nbdb.LogicalRouterPolicy) bool {
				return invalidLRPUUIDs.Has(item.UUID)
			}
			for _, nodeName := range localNodeNames {
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
			for eIP, nodeName := range egressIPCache.egressIPIPToNodeCache {
				if !egressIPCache.egressLocalNodesCache.Has(nodeName) {
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
				isEIPIPv6 := utilnet.IsIPv6String(eIP)
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
// since internal cache based logic will be different for different ovnkube-controllers
// zone can think objA is active while zoneb can think objB is active if both have multiple choice options
func (e *EgressIPController) syncPodAssignmentCache(egressIPCache egressIPCache) error {
	e.podAssignmentMutex.Lock()
	defer e.podAssignmentMutex.Unlock()
	for egressIPName, networkPods := range egressIPCache.egressIPNameToPods {
		for networkName, podCache := range networkPods {
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
			routerName := ni.GetNetworkScopedClusterRouterName()
			if ni.TopologyType() == types.Layer2Topology {
				// no support for multiple Nodes per OVN zone, therefore pick the first local zone node
				localNodeName, err := e.getALocalZoneNodeName()
				if err != nil {
					return err
				}
				routerName = ni.GetNetworkScopedGWRouterName(localNodeName)
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
			// either local to zone (in which case reRoutePolicies will work) OR pods that are
			// managed by local egressIP nodes (in which case egressIPSNATs will work)
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
				podState, ok := e.podAssignment[podKey]
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

				e.podAssignment[podKey] = podState
			}
		}
	}

	return nil
}

// This function implements a portion of syncEgressIPs.
// It removes OVN logical router policies used by EgressIPs deleted while ovnkube-master was down.
// It also removes stale nexthops from router policies used by EgressIPs.
// Upon failure, it may be invoked multiple times in order to avoid a pod restart.
func (e *EgressIPController) syncStaleEgressReroutePolicy(cache egressIPCache) error {
	for _, networkCache := range cache.egressIPNameToPods {
		for networkName, data := range networkCache {
			logicalRouterPolicyStaleNexthops := []*nbdb.LogicalRouterPolicy{}
			p := func(item *nbdb.LogicalRouterPolicy) bool {
				if item.Priority != types.EgressIPReroutePriority || item.ExternalIDs[libovsdbops.NetworkKey.String()] != networkName {
					return false
				}
				egressIPName, _ := getEIPLRPObjK8MetaData(item.ExternalIDs)
				if egressIPName == "" {
					klog.Errorf("syncStaleEgressReroutePolicy found logical router policy (UUID: %s) with invalid meta data associated with network %s", item.UUID, networkName)
					return false
				}
				splitMatch := strings.Split(item.Match, " ")
				logicalIP := splitMatch[len(splitMatch)-1]
				parsedLogicalIP := net.ParseIP(logicalIP)
				egressPodIPs := sets.NewString()
				// Since LRPs are created only for pods local to this zone
				// we need to care about only those pods. Nexthop for them will
				// either be transit switch IP or join switch IP or mp0 IP.
				// FIXME: LRPs are also created for remote pods to route them
				// correctly but we do not handling cleaning for them now
				for _, podIPs := range data.egressLocalPods {
					egressPodIPs.Insert(podIPs.UnsortedList()...)
				}
				if !egressPodIPs.Has(parsedLogicalIP.String()) {
					klog.Infof("syncStaleEgressReroutePolicy will delete %s due to no nexthop or stale logical ip: %v", egressIPName, item)
					return true
				}
				// Check for stale nexthops that may exist in the logical router policy and store that in logicalRouterPolicyStaleNexthops.
				// Note: adding missing nexthop(s) to the logical router policy is done outside the scope of this function.
				staleNextHops := []string{}
				for _, nexthop := range item.Nexthops {
					nodeName, ok := cache.egressIPIPToNodeCache[parsedLogicalIP.String()]
					if ok {
						klog.Infof("syncStaleEgressReroutePolicy will delete %s due to no node assigned to logical ip: %v", egressIPName, item)
						return true
					}
					networksRedirects, ok := cache.egressNodeRedirectsCache.cache[nodeName]
					if ok {
						klog.Infof("syncStaleEgressReroutePolicy will delete %s due to no network in cache: %v", egressIPName, item)
						return true
					}
					redirects, ok := networksRedirects[networkName]
					if !ok {
						klog.Infof("syncStaleEgressReroutePolicy will delete %s due to no redirects for network in cache: %v", egressIPName, item)
						return true
					}
					//FIXME: be more specific about which is the valid next hop instead of relying on verifying if the IP is within a valid set of IPs.
					if !redirects.containsIP(nexthop) {
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
			klog.Infof("syncStaleEgressReroutePolicy will remove stale nexthops for network %s: %+v", networkName, logicalRouterPolicyStaleNexthops)
			err = libovsdbops.DeleteNextHopsFromLogicalRouterPolicies(e.nbClient, cache.networkToRouter[networkName], logicalRouterPolicyStaleNexthops...)
			if err != nil {
				return fmt.Errorf("unable to remove stale next hops from logical router policies for network %s: %v", networkName, err)
			}
		}
	}

	return nil
}

// This function implements a portion of syncEgressIPs.
// It removes OVN NAT rules used by EgressIPs deleted while ovnkube-master was down for the default cluster network only.
// Upon failure, it may be invoked multiple times in order to avoid a pod restart. For UDNs, we do not SNAT to the EgressIP
// using OVNs gateway router and in-fact we do not use OVN, but instead we add OVS flows in the external bridge to SNAT to the EgressIP. This is not managed here.
func (e *EgressIPController) syncStaleSNATRules(egressIPCache egressIPCache) error {
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
		parsedLogicalIP := net.ParseIP(item.LogicalIP).String()
		cacheEntry, exists := egressIPCache.egressIPNameToPods[egressIPName][types.DefaultNetworkName]
		egressPodIPs := sets.NewString()
		if exists {
			// since SNATs can be present either if status.Node was local to
			// the zone or pods were local to the zone, we need to check both
			for _, podIPs := range cacheEntry.egressLocalPods {
				egressPodIPs.Insert(podIPs.UnsortedList()...)
			}
			for _, podIPs := range cacheEntry.egressRemotePods {
				egressPodIPs.Insert(podIPs.UnsortedList()...)
			}
		}
		if !exists || !egressPodIPs.Has(parsedLogicalIP) {
			klog.Infof("syncStaleSNATRules will delete %s due to logical ip: %v", egressIPName, item)
			return true
		}
		ni := e.networkManager.GetNetwork(types.DefaultNetworkName)
		if ni == nil {
			klog.Errorf("syncStaleSNATRules failed to find default network in networks cache")
			return false
		}
		if node, ok := egressIPCache.egressIPIPToNodeCache[item.ExternalIP]; !ok || !cacheEntry.egressLocalPods[types.DefaultNetworkName].Has(node) ||
			item.LogicalPort == nil || *item.LogicalPort != ni.GetNetworkScopedK8sMgmtIntfName(node) {
			klog.Infof("syncStaleSNATRules will delete %s due to external ip or stale logical port: %v", egressIPName, item)
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
	// The routers length 0 check is needed because some of ovnk master restart unit tests have
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
// trips to the API server per item.
func (e *EgressIPController) generateCacheForEgressIP() (egressIPCache, error) {
	cache := egressIPCache{}
	namespaces, err := e.watchFactory.GetNamespaces()
	if err != nil {
		return cache, fmt.Errorf("failed to get all namespaces: %v", err)
	}
	nodes, err := e.watchFactory.GetNodes()
	if err != nil {
		return cache, fmt.Errorf("failed to get all nodes: %v", err)
	}
	localZoneNodes := sets.New[string]()
	nodeNames := e.nodeZoneState.GetKeys()
	for _, nodeName := range nodeNames {
		if isLocal, ok := e.nodeZoneState.Load(nodeName); ok && isLocal {
			localZoneNodes.Insert(nodeName)
		}
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
		// skip if already processed
		if _, ok := redirectCache[ni.GetNetworkName()]; ok {
			continue
		}
		redirectCache[ni.GetNetworkName()] = map[string]redirectIPs{}
		var localNodeName string
		if localZoneNodes.Len() > 0 {
			localNodeName = localZoneNodes.UnsortedList()[0]
		}
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
			if err != nil {
				// if switch port isnt created, we can assume theres nothing to sync
				if errors.Is(err, libovsdbclient.ErrNotFound) {
					continue
				}
				return cache, fmt.Errorf("failed to find management port for node %s: %v", node.Name, err)
			}
			mgmtPortAddresses := mgmtPort.GetAddresses()
			if len(mgmtPortAddresses) == 0 {
				return cache, fmt.Errorf("management switch port %s for node %s does not contain any addresses", ni.GetNetworkScopedK8sMgmtIntfName(node.Name), node.Name)
			}
			// assuming only one IP per IP family
			for _, mgmtPortAddress := range mgmtPortAddresses {
				mgmtPortAddressesStr := strings.Fields(mgmtPortAddress)
				mgmtPortIP := net.ParseIP(mgmtPortAddressesStr[1])
				if utilnet.IsIPv6(mgmtPortIP) {
					if ip := mgmtPortIP.To16(); ip != nil {
						r.v6MgtPort = ip.String()
					}
				} else {
					if ip := mgmtPortIP.To4(); ip != nil {
						r.v4MgtPort = ip.String()
					}
				}
			}

			if localZoneNodes.Has(node.Name) {
				if e.v4 {
					if gatewayRouterIP, err := e.getGatewayNextHop(ni, node.Name, false); err != nil {
						klog.V(5).Infof("Unable to retrieve gateway IP for node: %s, protocol is IPv4: err: %v", node.Name, err)
					} else {
						r.v4Gateway = gatewayRouterIP.String()
					}
				}
				if e.v6 {
					if gatewayRouterIP, err := e.getGatewayNextHop(ni, node.Name, true); err != nil {
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
	// egressLocalNodes will contain all nodes that are local
	// to this zone which are serving this egressIP object..
	// This will help sync SNATs
	egressLocalNodesCache := sets.New[string]()
	cache.egressLocalNodesCache = egressLocalNodesCache
	// egressIP name -> node name
	egressNodesCache := make(map[string]string, 0)
	cache.egressIPIPToNodeCache = egressNodesCache
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
		for _, status := range egressIP.Status.Items {
			if localZoneNodes.Has(status.Node) {
				egressLocalNodesCache.Insert(status.Node)
			}
			egressNodesCache[status.EgressIP] = status.Node
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
			_, ok := egressIPsCache[egressIP.Name][ni.GetNetworkName()]
			if ok {
				continue // aready populated
			}
			egressIPsCache[egressIP.Name][ni.GetNetworkName()] = selectedPods{
				egressLocalPods:  map[string]sets.Set[string]{},
				egressRemotePods: map[string]sets.Set[string]{},
			}
			nadName := types.DefaultNetworkName
			if ni.IsSecondary() {
				nadNames := ni.GetNADs()
				if len(nadNames) == 0 {
					klog.Errorf("Network %s: error build egress IP sync cache, expected at least one NAD name for Namespace %s", ni.GetNetworkName(), namespace.Name)
					continue
				}
				nadName = nadNames[0] // there should only be one active network
			}
			for _, pod := range pods {
				if util.PodCompleted(pod) || !util.PodScheduled(pod) || util.PodWantsHostNetwork(pod) {
					continue
				}
				if egressLocalNodesCache.Len() == 0 && !e.isPodScheduledinLocalZone(pod) {
					continue // don't process anything on master's that have nothing to do with the pod
				}
				podIPs, err := e.getPodIPs(ni, pod, nadName)
				if err != nil {
					klog.Errorf("Network %s: error build egress IP sync cache, error while trying to get pod %s/%s IPs: %v", ni.GetNetworkName(), pod.Namespace, pod.Name, err)
					continue
				}
				if len(podIPs) == 0 {
					continue
				}
				podKey := getPodKey(pod)
				if e.isPodScheduledinLocalZone(pod) {
					//
					_, ok := egressIPsCache[egressIP.Name][ni.GetNetworkName()].egressLocalPods[podKey]
					if !ok {
						egressIPsCache[egressIP.Name][ni.GetNetworkName()].egressLocalPods[podKey] = sets.New[string]()
					}
					for _, ipNet := range podIPs {
						egressIPsCache[egressIP.Name][ni.GetNetworkName()].egressLocalPods[podKey].Insert(ipNet.IP.String())
					}
				} else if egressLocalNodesCache.Len() > 0 {
					// it means this controller has at least one egressNode that is in localZone but matched pod is remote
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
	if e.isLocalZoneNode(node) {
		klog.V(5).Infof("Egress node: %s about to be initialized", node.Name)
		if config.OVNKubernetesFeature.EnableInterconnect && e.zone != types.OvnDefaultZone {
			// NOTE: EgressIP is not supported on multi-nodes-in-same-zone case
			// NOTE2: We don't want this route for all-nodes-in-same-zone (almost nonIC a.k.a single zone) case because
			// it makes no sense - all nodes are connected via the same ovn_cluster_router
			// NOTE3: When the node gets deleted we do not remove this route intentionally because
			// on IC if the node is gone, then the ovn_cluster_router is also gone along with all
			// the routes on it.
			processNetworkFn := func(ni util.NetInfo) error {
				if ni.TopologyType() == types.Layer2Topology || len(ni.Subnets()) == 0 {
					return nil
				}
				if err := libovsdbutil.CreateDefaultRouteToExternal(e.nbClient, ni.GetNetworkScopedClusterRouterName(),
					ni.GetNetworkScopedGWRouterName(node.Name), ni.Subnets()); err != nil {
					return fmt.Errorf("failed to create route to external for network %s: %v", ni.GetNetworkName(), err)
				}
				return nil
			}
			ni := e.networkManager.GetNetwork(types.DefaultNetworkName)
			if ni == nil {
				return fmt.Errorf("failed to get default network from NAD controller")
			}
			if err := processNetworkFn(ni); err != nil {
				return fmt.Errorf("failed to process default network: %v", err)
			}
			if !isEgressIPForUDNSupported() {
				return nil
			}
			if err := e.networkManager.DoWithLock(processNetworkFn); err != nil {
				return fmt.Errorf("failed to process all user defined networks route to external: %v", err)
			}
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
func (e *EgressIPController) initClusterEgressPolicies(nodes []interface{}) error {
	// Init default network
	defaultNetInfo := e.networkManager.GetNetwork(types.DefaultNetworkName)
	localNodeName, err := e.getALocalZoneNodeName()
	if err != nil {
		klog.Warningf(err.Error())
	}
	subnets := util.GetAllClusterSubnetsFromEntries(defaultNetInfo.Subnets())
	if err := InitClusterEgressPolicies(e.nbClient, e.addressSetFactory, defaultNetInfo, subnets, e.controllerName, defaultNetInfo.GetNetworkScopedClusterRouterName()); err != nil {
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
		if err = InitClusterEgressPolicies(e.nbClient, e.addressSetFactory, network, subnets, e.controllerName, routerName); err != nil {
			return fmt.Errorf("failed to initialize networks cluster logical router egress policies for network %s: %v", network.GetNetworkName(), err)
		}
		return nil
	})
}

// InitClusterEgressPolicies creates the global no reroute policies and address-sets
// required by the egressIP and egressServices features.
func InitClusterEgressPolicies(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory, ni util.NetInfo,
	clusterSubnets []*net.IPNet, controllerName, routerName string) error {
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
	dbIDs := getEgressIPAddrSetDbIDs(NodeIPAddrSetName, types.DefaultNetworkName, DefaultNetworkControllerName)
	if _, err = addressSetFactory.EnsureAddressSet(dbIDs); err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", NodeIPAddrSetName, err)
	}

	// ensure the address-set for storing egressIP pods exists
	dbIDs = getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, ni.GetNetworkName(), controllerName)
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

type statusMap map[egressipv1.EgressIPStatusItem]string

type egressStatuses struct {
	statusMap
}

func (e egressStatuses) contains(potentialStatus egressipv1.EgressIPStatusItem) bool {
	// handle the case where the all of the status fields are populated
	if _, exists := e.statusMap[potentialStatus]; exists {
		return true
	}
	return false
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
// Any failure from here will not be retried, its a corner case undefined behaviour
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
		if ni.IsSecondary() {
			mark = getEgressIPPktMark(eip.Name, eip.Annotations)
		}
		break
	}
	if eipToAssign == "" {
		klog.Infof("No standby egressIP's found for pod %s", podKey)
		return nil
	}
	// get IPs
	nadName := ni.GetNetworkName()
	if ni.IsSecondary() {
		nadNames := ni.GetNADs()
		if len(nadNames) == 0 {
			return fmt.Errorf("expected at least one NAD name for Namespace %s", pod.Namespace)
		}
		nadName = nadNames[0] // there should only be one active network
	}
	podIPNets, err := e.getPodIPs(ni, pod, nadName)
	if err != nil {
		return fmt.Errorf("failed to get pod %s/%s IPs using nad name %q: %v", pod.Namespace, pod.Name, nadName, err)
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
	e.podAssignment[podKey] = podState
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
// This function should be called with lock on nodeZoneState cache key status.Node and pod.Spec.NodeName
func (e *EgressIPController) addPodEgressIPAssignment(ni util.NetInfo, egressIPName string, status egressipv1.EgressIPStatusItem, mark util.EgressIPMark,
	pod *kapi.Pod, podIPs []*net.IPNet) (err error) {
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
	isLocalZoneEgressNode, loadedEgressNode := e.nodeZoneState.Load(status.Node)
	isLocalZonePod, loadedPodNode := e.nodeZoneState.Load(pod.Spec.NodeName)
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
	nextHopIP, err := e.getNextHop(ni, status.Node, status.EgressIP, egressIPName, isLocalZoneEgressNode, isOVNNetwork)
	if err != nil {
		return fmt.Errorf("failed to determine next hop for pod %s/%s when configuring egress IP %s"+
			" IP %s: %v", pod.Namespace, pod.Name, egressIPName, status.EgressIP, err)
	}
	if nextHopIP == "" {
		return fmt.Errorf("could not calculate the next hop for pod %s/%s when configuring egress IP %s"+
			" IP %s", pod.Namespace, pod.Name, egressIPName, status.EgressIP)
	}
	var ops []ovsdb.Operation
	if loadedEgressNode && isLocalZoneEgressNode {
		// create NATs for CDNs only
		// create LRPs with allow action (aka GW policy marks) only for L3 UDNs.
		// L2 UDNs require LRPs with reroute action with a pkt_mark option attached to GW router.
		if isOVNNetwork {
			if ni.IsDefault() {
				ops, err = e.createNATRuleOps(ni, nil, podIPs, status, egressIPName, pod.Namespace, pod.Name)
				if err != nil {
					return fmt.Errorf("unable to create NAT rule ops for status: %v, err: %v", status, err)
				}

			} else if ni.IsSecondary() && ni.TopologyType() == types.Layer3Topology {
				// not required for L2 because we always have LRPs using reroute action to pkt mark
				ops, err = e.createGWMarkPolicyOps(ni, ops, podIPs, status, mark, pod.Namespace, pod.Name, egressIPName)
				if err != nil {
					return fmt.Errorf("unable to create GW router LRP ops to packet mark pod %s/%s: %v", pod.Namespace, pod.Name, err)
				}
			}
		}
		if config.OVNKubernetesFeature.EnableInterconnect && ni.IsDefault() && !isOVNNetwork && (loadedPodNode && !isLocalZonePod) {
			// For CDNs, configure LRP with reroute action for non-local-zone pods on egress nodes to support redirect to local management port
			// when the egress IP is assigned to a host secondary interface
			routerName, err := getTopologyScopedRouterName(ni, pod.Spec.NodeName)
			if err != nil {
				return err
			}
			ops, err = e.createReroutePolicyOps(ni, ops, podIPs, status, mark, egressIPName, nextHopIP, routerName, pod.Namespace, pod.Name)
			if err != nil {
				return fmt.Errorf("unable to create logical router policy ops %v, err: %v", status, err)
			}
		}
	}

	// For L2, we always attach an LRP with reroute action to the Nodes gateway router. If the pod is remote, use the local zone Node name to generate the GW router name.
	nodeName := pod.Spec.NodeName
	if loadedEgressNode && loadedPodNode && !isLocalZonePod && isLocalZoneEgressNode && ni.IsSecondary() && ni.TopologyType() == types.Layer2Topology {
		nodeName = status.Node
	}
	routerName, err := getTopologyScopedRouterName(ni, nodeName)
	if err != nil {
		return err
	}

	// exec when node is local OR when pods are local or L2 UDN
	// don't add a reroute policy if the egress node towards which we are adding this doesn't exist
	if loadedEgressNode && loadedPodNode {
		if isLocalZonePod || (isLocalZoneEgressNode && ni.IsSecondary() && ni.TopologyType() == types.Layer2Topology) {
			ops, err = e.createReroutePolicyOps(ni, ops, podIPs, status, mark, egressIPName, nextHopIP, routerName, pod.Namespace, pod.Name)
			if err != nil {
				return fmt.Errorf("unable to create logical router policy ops, err: %v", err)
			}
		}
		if isLocalZonePod {
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
// This function should be called with lock on nodeZoneState cache key status.Node and pod.Spec.NodeName
func (e *EgressIPController) deletePodEgressIPAssignment(ni util.NetInfo, egressIPName string, status egressipv1.EgressIPStatusItem, pod *kapi.Pod) (err error) {
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
	isLocalZoneEgressNode, loadedEgressNode := e.nodeZoneState.Load(status.Node)
	isLocalZonePod, loadedPodNode := e.nodeZoneState.Load(pod.Spec.NodeName)
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
			nextHopIP, err = e.getNextHop(ni, status.Node, status.EgressIP, egressIPName, isLocalZoneEgressNode, isOVNNetwork)
			if err != nil {
				klog.Warningf("Unable to determine next hop for egress IP %s IP %s assigned to node %s: %v", egressIPName,
					status.EgressIP, status.Node, err)
			}
		}
	}
	// For L2, we always attach an LRP with reroute action to the Nodes gateway router. If the pod is remote, use the local zone Node name to generate the GW router name.
	nodeName := pod.Spec.NodeName
	if !isLocalZonePod && isLocalZoneEgressNode && ni.IsSecondary() && ni.TopologyType() == types.Layer2Topology {
		nodeName = status.Node
	}
	routerName, err := getTopologyScopedRouterName(ni, nodeName)
	if err != nil {
		return err
	}
	var ops []ovsdb.Operation
	if !loadedPodNode || isLocalZonePod { // node is deleted (we can't determine zone so we always try and nuke OR pod is local to zone)
		ops, err = e.addExternalGWPodSNATOps(ni, nil, pod.Namespace, pod.Name, status)
		if err != nil {
			return err
		}
		ops, err = e.deleteReroutePolicyOps(ni, ops, status, egressIPName, nextHopIP, routerName, pod.Namespace, pod.Name)
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			// if the gateway router join IP setup is already gone, then don't count it as error.
			klog.Warningf("Unable to delete logical router policy, err: %v", err)
		} else if err != nil {
			return fmt.Errorf("unable to delete logical router policy, err: %v", err)
		}
	}

	if loadedEgressNode && isLocalZoneEgressNode {
		if config.OVNKubernetesFeature.EnableInterconnect && ni.IsDefault() && !isOVNNetwork && (!loadedPodNode || !isLocalZonePod) { // node is deleted (we can't determine zone so we always try and nuke OR pod is remote to zone)
			// For CDNs, delete reroute for non-local-zone pods on egress nodes when the egress IP is assigned to a secondary host interface
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
		} else if ni.IsSecondary() && ni.TopologyType() == types.Layer3Topology {
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
// This function should be called with lock on nodeZoneState cache key pod.Spec.Name
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

		isLocalZonePod, loadedPodNode := e.nodeZoneState.Load(pod.Spec.NodeName)
		if pod.Spec.NodeName == status.Node && loadedPodNode && isLocalZonePod && util.PodNeedsSNAT(pod) {
			// if the pod still exists, add snats to->nodeIP (on the node where the pod exists) for these podIPs after deleting the snat to->egressIP
			// NOTE: This needs to be done only if the pod was on the same node as egressNode
			extIPs, err := getExternalIPsGR(e.watchFactory, pod.Spec.NodeName)
			if err != nil {
				return nil, err
			}
			podIPs, err := util.GetPodCIDRsWithFullMask(pod, &util.DefaultNetInfo{})
			if err != nil {
				return nil, err
			}
			ops, err = addOrUpdatePodSNATOps(e.nbClient, ni.GetNetworkScopedGWRouterName(pod.Spec.NodeName), extIPs, podIPs, "", ops)
			if err != nil {
				return nil, err
			}
			klog.V(5).Infof("Adding SNAT on %s since egress node managing %s/%s was the same: %s", pod.Spec.NodeName, pod.Namespace, pod.Name, status.Node)
		}
	}
	return ops, nil
}

// deleteExternalGWPodSNATOps creates ops for the required external GW teardown for the given pod
func (e *EgressIPController) deleteExternalGWPodSNATOps(ni util.NetInfo, ops []ovsdb.Operation, pod *kapi.Pod, podIPs []*net.IPNet, status egressipv1.EgressIPStatusItem, isOVNNetwork bool) ([]ovsdb.Operation, error) {
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
		ops, err = deletePodSNATOps(e.nbClient, ops, ni.GetNetworkScopedGWRouterName(pod.Spec.NodeName), extIPs, affectedIPs, "")
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
// For layer 2, it's the callers responsibility to ensure that the egress node is remote because a LRP should not be created
func (e *EgressIPController) getGatewayNextHop(ni util.NetInfo, nodeName string, isIPv6 bool) (net.IP, error) {
	// fetch gateway router 'router to join' port IP
	if ni.TopologyType() == types.Layer3Topology {
		return e.getRouterPortIP(types.GWRouterToJoinSwitchPrefix+ni.GetNetworkScopedGWRouterName(nodeName), isIPv6)
	}

	// If egress node is local, retrieve the external default gateway next hops from the Node L3 gateway annotation.
	// We must pick one of the next hops to add to the LRP reroute next hops to not break ECMP.
	// If an egress node is remote, retrieve the remote Nodes gateway router 'router to switch' port IP
	// from the Node annotation.
	// FIXME: remove gathering the required information from a Node annotations as this approach does not scale
	// FIXME: we do not respect multiple default gateway next hops and instead pick the first IP that matches the IP family of the EIP
	if ni.TopologyType() == types.Layer2Topology {
		node, err := e.watchFactory.GetNode(nodeName)
		if err != nil {
			return nil, fmt.Errorf("failed to retrive node %s: %w", nodeName, err)
		}
		localNode, err := e.getALocalZoneNodeName()
		if err != nil {
			return nil, err
		}
		// Node is local
		if localNode == nodeName {
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
			return util.ParseNodeGatewayRouterJoinIPv6(node, ni.GetNetworkName())
		}
		return util.ParseNodeGatewayRouterJoinIPv4(node, ni.GetNetworkName())
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
func (e *EgressIPController) getNextHop(ni util.NetInfo, egressNodeName, egressIP, egressIPName string, isLocalZoneEgressNode, isOVNNetwork bool) (string, error) {
	isEgressIPv6 := utilnet.IsIPv6String(egressIP)
	if isLocalZoneEgressNode || ni.TopologyType() == types.Layer2Topology {
		// isOVNNetwork is true when an EgressIP is "assigned" to the Nodes primary interface (breth0). Ext traffic will egress breth0.
		// is OVNNetwork is false when the EgressIP is assigned to a host secondary interface (not breth0). Ext traffic will egress this interface.
		if isOVNNetwork {
			gatewayRouterIP, err := e.getGatewayNextHop(ni, egressNodeName, isEgressIPv6)
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
			if ni.IsSecondary() {
				return "", fmt.Errorf("egress IP assigned to a host secondary interface for a user defined network (network name %s) is unsupported", ni.GetNetworkName())
			}
			return e.getLocalMgmtPortNextHop(ni, egressNodeName, egressIPName, egressIP, isEgressIPv6)
		}
	}

	if config.OVNKubernetesFeature.EnableInterconnect {
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
	return "", nil
}

// createReroutePolicyOps creates an operation that does idempotent updates of the
// LogicalRouterPolicy corresponding to the egressIP status item, according to the
// following update procedure for when EIP is hosted by a OVN network:
// - if the LogicalRouterPolicy does not exist: it adds it by creating the
// reference to it from ovn_cluster_router and specifying the array of nexthops
// to equal [gatewayRouterIP]
// - if the LogicalRouterPolicy does exist: it adds the gatewayRouterIP to the
// array of nexthops
// For EIP hosted on secondary host network, logical route policies are needed
// to redirect the pods to the appropriate management port or if interconnect is
// enabled, the appropriate transit switch port.
// This function should be called with lock on nodeZoneState cache key status.Node
func (e *EgressIPController) createReroutePolicyOps(ni util.NetInfo, ops []ovsdb.Operation, podIPNets []*net.IPNet, status egressipv1.EgressIPStatusItem,
	mark util.EgressIPMark, egressIPName, nextHopIP, routerName, podNamespace, podName string) ([]ovsdb.Operation, error) {
	isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
	ipFamily := getEIPIPFamily(isEgressIPv6)
	options := make(map[string]string)
	if ni.IsSecondary() {
		if !mark.IsAvailable() {
			return nil, fmt.Errorf("egressIP %s object must contain a mark for user defined networks", egressIPName)
		}
		addPktMarkToLRPOptions(options, mark.String())
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
// This function should be called with lock on nodeZoneState cache key status.Node
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
	mark util.EgressIPMark, podNamespace, podName, egressIPName string) ([]ovsdb.Operation, error) {
	isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
	routerName := ni.GetNetworkScopedGWRouterName(status.Node)
	options := make(map[string]string)
	if !mark.IsAvailable() {
		return nil, fmt.Errorf("egressIP object must contain a mark for user defined networks")
	}
	addPktMarkToLRPOptions(options, mark.String())
	var err error
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
// This function should be called with a lock on e.nodeZoneState.status.Node
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
		router := ni.GetNetworkScopedClusterRouterName()
		if ni.TopologyType() == types.Layer2Topology {
			nodeName, err := e.getALocalZoneNodeName()
			if err != nil {
				return err
			}
			router = ni.GetNetworkScopedGWRouterName(nodeName)
		}
		ops, err = libovsdbops.DeleteNextHopFromLogicalRouterPoliciesWithPredicateOps(e.nbClient, ops, router, policyPredNextHop, nextHopIP)
		if err != nil {
			return fmt.Errorf("error removing nexthop IP %s from egress ip %s policies on router %s: %v",
				nextHopIP, name, router, err)
		}
	} else if ops, err = e.ensureOnlyValidNextHops(ni, name, status.Node, ops); err != nil {
		return err
	}

	isLocalZoneEgressNode, loadedEgressNode := e.nodeZoneState.Load(status.Node)
	if loadedEgressNode && isLocalZoneEgressNode {
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
		} else if ni.IsSecondary() {
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

func (e *EgressIPController) ensureOnlyValidNextHops(ni util.NetInfo, name, nodeName string, ops []libovsdb.Operation) ([]libovsdb.Operation, error) {
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
	isLocalZoneEgressNode, _ := n.nodeZoneState.Load(status.Node)
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
		nextHopIP, err = n.getNextHop(ni, status.Node, status.EgressIP, name, isLocalZoneEgressNode, isOVNNetwork)
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
		if err := createLogicalRouterPolicy(nbClient, clusterRouter, match, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
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
		if err := createLogicalRouterPolicy(nbClient, clusterRouter, match, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv6 no-reroute service policies, err: %v", err)
		}
	}
	return nil
}

func (e *EgressIPController) ensureRouterPoliciesForNetwork(ni util.NetInfo) error {
	e.nodeUpdateMutex.Lock()
	defer e.nodeUpdateMutex.Unlock()
	subnetEntries := ni.Subnets()
	subnets := util.GetAllClusterSubnetsFromEntries(subnetEntries)
	if len(subnets) == 0 {
		return nil
	}
	localNode, err := e.getALocalZoneNodeName()
	if err != nil {
		return err
	}
	routerName, err := getTopologyScopedRouterName(ni, localNode)
	if err != nil {
		return err
	}
	if err := InitClusterEgressPolicies(e.nbClient, e.addressSetFactory, ni, subnets, e.controllerName, routerName); err != nil {
		return fmt.Errorf("failed to initialize networks cluster logical router egress policies for the default network: %v", err)
	}
	err = ensureDefaultNoRerouteNodePolicies(e.nbClient, e.addressSetFactory, ni.GetNetworkName(), routerName,
		e.controllerName, listers.NewNodeLister(e.watchFactory.NodeInformer().GetIndexer()), e.v4, e.v6)
	if err != nil {
		return fmt.Errorf("failed to ensure no reroute node policies for network %s: %v", ni.GetNetworkName(), err)
	}
	if config.OVNKubernetesFeature.EnableInterconnect && ni.TopologyType() == types.Layer3Topology {
		if err := libovsdbutil.CreateDefaultRouteToExternal(e.nbClient, routerName,
			ni.GetNetworkScopedGWRouterName(localNode), subnetEntries); err != nil {
			return fmt.Errorf("failed to create route to external for network %s: %v", ni.GetNetworkName(), err)
		}
	}
	return nil
}

func (e *EgressIPController) ensureSwitchPoliciesForNode(ni util.NetInfo, nodeName string) error {
	e.nodeUpdateMutex.Lock()
	defer e.nodeUpdateMutex.Unlock()
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
	if err := createLogicalRouterPolicy(nbClient, routerName, match, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
		return fmt.Errorf("unable to create no-reroute reply traffic policies, err: %v", err)
	}
	return nil
}

// createDefaultNoReroutePodPolicies ensures egress pods east<->west traffic with regular pods,
// i.e: ensuring that an egress pod can still communicate with a regular pod / service backed by regular pods
func createDefaultNoReroutePodPolicies(nbClient libovsdbclient.Client, network, controller, routerName string, v4ClusterSubnet, v6ClusterSubnet []*net.IPNet) error {
	for _, v4Subnet := range v4ClusterSubnet {
		match := fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Subnet.String(), v4Subnet.String())
		dbIDs := getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, network, controller)
		if err := createLogicalRouterPolicy(nbClient, routerName, match, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv4 no-reroute pod policies, err: %v", err)
		}
	}
	for _, v6Subnet := range v6ClusterSubnet {
		match := fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6Subnet.String(), v6Subnet.String())
		dbIDs := getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV6, network, controller)
		if err := createLogicalRouterPolicy(nbClient, routerName, match, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv6 no-reroute pod policies, err: %v", err)
		}
	}
	return nil
}

// createDefaultReRouteQoSRule builds QoS rule ops to be created on every node's switch that let's us
// mark packets that are tracked in conntrack and replies emerging from the pod.
// This mark is then matched on the reroute policies to determine if its a reply packet
// in which case we do not need to reRoute to other nodes and if its a service response it is
// not rerouted since mark will be present.
func createDefaultReRouteQoSRuleOps(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory, ops []libovsdb.Operation,
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
		if ipv4EgressIPServedPodsAS == "" {
			return nil, nil, fmt.Errorf("failed to fetch IPv4 address set %s hash names", EgressIPServedPodsAddrSetName)
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
		if ipv6EgressIPServedPodsAS == "" {
			return nil, nil, fmt.Errorf("failed to fetch IPv6 address set %s hash names", EgressIPServedPodsAddrSetName)
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
	e.nodeUpdateMutex.Lock()
	defer e.nodeUpdateMutex.Unlock()
	defaultNetInfo := e.networkManager.GetNetwork(types.DefaultNetworkName)
	var ops []libovsdb.Operation
	ops, err := e.ensureDefaultNoReRouteQosRulesForNode(defaultNetInfo, nodeName, ops)
	if err != nil {
		return fmt.Errorf("failed to process default network: %v", err)
	}
	if err = e.networkManager.DoWithLock(func(network util.NetInfo) error {
		if network.GetNetworkName() == types.DefaultNetworkName {
			return nil
		}
		ops, err = e.ensureDefaultNoReRouteQosRulesForNode(network, nodeName, ops)
		if err != nil {
			return fmt.Errorf("failed to process network %s: %v", network.GetNetworkName(), err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to ensure default no reroute QoS rules: %v", err)
	}
	if _, err := libovsdbops.TransactAndCheck(e.nbClient, ops); err != nil {
		return fmt.Errorf("unable to add EgressIP QoS to switch, err: %v", err)
	}
	return nil
}

func (e *EgressIPController) ensureDefaultNoReRouteQosRulesForNode(ni util.NetInfo, nodeName string, ops []libovsdb.Operation) ([]libovsdb.Operation, error) {
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
	e.nodeUpdateMutex.Lock()
	defer e.nodeUpdateMutex.Unlock()
	nodeLister := listers.NewNodeLister(e.watchFactory.NodeInformer().GetIndexer())
	// ensure default network is processed
	defaultNetInfo := e.networkManager.GetNetwork(types.DefaultNetworkName)
	err := ensureDefaultNoRerouteNodePolicies(e.nbClient, e.addressSetFactory, defaultNetInfo.GetNetworkName(), defaultNetInfo.GetNetworkScopedClusterRouterName(),
		e.controllerName, nodeLister, e.v4, e.v6)
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
		routerName := network.GetNetworkScopedClusterRouterName()
		if network.TopologyType() == types.Layer2Topology {
			// assume one node per zone only. Multi nodes per zone not supported.
			nodeName, err := e.getALocalZoneNodeName()
			if err != nil {
				return err
			}
			routerName = network.GetNetworkScopedGWRouterName(nodeName)
		}
		err = ensureDefaultNoRerouteNodePolicies(e.nbClient, e.addressSetFactory, network.GetNetworkName(), routerName,
			e.controllerName, nodeLister, e.v4, e.v6)
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
// All the cluster node's addresses are considered. This is to avoid race conditions after a VIP moves from one node
// to another where we might process events out of order. For the same reason this function needs to be called under
// lock.
func ensureDefaultNoRerouteNodePolicies(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory,
	network, router, controller string, nodeLister listers.NodeLister, v4, v6 bool) error {
	nodes, err := nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %v", err)
	}
	v4NodeAddrs, v6NodeAddrs, err := util.GetNodeAddresses(v4, v6, nodes...)
	if err != nil {
		return fmt.Errorf("failed to get node addresses: %v", err)
	}
	allAddresses := make([]net.IP, 0, len(v4NodeAddrs)+len(v6NodeAddrs))
	allAddresses = append(allAddresses, v4NodeAddrs...)
	allAddresses = append(allAddresses, v6NodeAddrs...)

	var as addressset.AddressSet
	// all networks reference the same node IP address set
	dbIDs := getEgressIPAddrSetDbIDs(NodeIPAddrSetName, types.DefaultNetworkName, DefaultNetworkControllerName)
	if as, err = addressSetFactory.GetAddressSet(dbIDs); err != nil {
		return fmt.Errorf("cannot ensure that addressSet %s exists %v", NodeIPAddrSetName, err)
	}

	if err = as.SetAddresses(util.StringSlice(allAddresses)); err != nil {
		return fmt.Errorf("unable to set IPs to no re-route address set %s: %w", NodeIPAddrSetName, err)
	}

	ipv4ClusterNodeIPAS, ipv6ClusterNodeIPAS := as.GetASHashNames()
	// fetch the egressIP pods address-set
	dbIDs = getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, network, controller)
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
	if len(v4NodeAddrs) > 0 {
		if ipv4EgressIPServedPodsAS == "" || ipv4EgressServiceServedPodsAS == "" || ipv4ClusterNodeIPAS == "" {
			return fmt.Errorf("address set name(s) %s not found %q %q %q", as.GetName(), ipv4EgressServiceServedPodsAS, ipv4EgressServiceServedPodsAS, ipv4ClusterNodeIPAS)
		}
		matchV4 = fmt.Sprintf(`(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s`,
			ipv4EgressIPServedPodsAS, ipv4EgressServiceServedPodsAS, ipv4ClusterNodeIPAS)
	}
	if len(v6NodeAddrs) > 0 {
		if ipv6EgressIPServedPodsAS == "" || ipv6EgressServiceServedPodsAS == "" || ipv6ClusterNodeIPAS == "" {
			return fmt.Errorf("address set hash name(s) %s not found", as.GetName())
		}
		matchV6 = fmt.Sprintf(`(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s`,
			ipv6EgressIPServedPodsAS, ipv6EgressServiceServedPodsAS, ipv6ClusterNodeIPAS)
	}
	options := map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark}
	// Create global allow policy for node traffic
	if matchV4 != "" {
		dbIDs = getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, network, controller)
		if err := createLogicalRouterPolicy(nbClient, router, matchV4, types.DefaultNoRereoutePriority, options, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv4 no-reroute node policies, err: %v", err)
		}
	}

	if matchV6 != "" {
		dbIDs = getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, network, controller)
		if err := createLogicalRouterPolicy(nbClient, router, matchV6, types.DefaultNoRereoutePriority, options, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv6 no-reroute node policies, err: %v", err)
		}
	}
	return nil
}

func (e *EgressIPController) getPodIPs(ni util.NetInfo, pod *corev1.Pod, nadName string) ([]*net.IPNet, error) {
	podIPs := make([]*net.IPNet, 0)
	getIPFromIPNetFn := func(podIPNets []*net.IPNet) []*net.IPNet {
		ipNetsCopy := make([]*net.IPNet, 0, len(podIPNets))
		for _, ipNet := range podIPNets {
			ipNetCopy := *ipNet
			ipNetsCopy = append(ipNetsCopy, &ipNetCopy)
		}
		return ipNetsCopy
	}
	if e.isPodScheduledinLocalZone(pod) {
		// Retrieve the pod's networking configuration from the
		// logicalPortCache. The reason for doing this: a) only normal network
		// pods are placed in this cache, b) once the pod is placed here we know
		// addLogicalPort has finished successfully setting up networking for
		// the pod, so we can proceed with retrieving its IP and deleting the
		// external GW configuration created in addLogicalPort for the pod.
		logicalPort, err := e.logicalPortCache.get(pod, nadName)
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
	} else { // means this is egress node's local master
		if ni.IsDefault() {
			podIPNets, err := util.GetPodCIDRsWithFullMask(pod, ni)
			if err != nil {
				return nil, fmt.Errorf("failed to get pod %s/%s IP: %v", pod.Namespace, pod.Name, err)
			}
			if len(podIPNets) == 0 {
				return nil, fmt.Errorf("failed to get pod %s/%s IPs", pod.Namespace, pod.Name)
			}
			podIPs = getIPFromIPNetFn(podIPNets)
		} else if ni.IsSecondary() {
			podIPNets := util.GetPodCIDRsWithFullMaskOfNetwork(pod, nadName)
			if len(podIPNets) == 0 {
				return nil, fmt.Errorf("failed to get pod %s/%s IPs", pod.Namespace, pod.Name)
			}
			podIPs = getIPFromIPNetFn(podIPNets)
		}
	}
	return podIPs, nil
}

func createLogicalRouterPolicy(nbClient libovsdbclient.Client, clusterRouter, match string, priority int, options map[string]string, dbIDs *libovsdbops.DbObjectIDs) error {
	lrp := nbdb.LogicalRouterPolicy{
		Priority:    priority,
		Action:      nbdb.LogicalRouterPolicyActionAllow,
		Match:       match,
		ExternalIDs: dbIDs.GetExternalIDs(),
		Options:     options,
	}
	p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
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
	router := &nbdb.LogicalRouter{
		Name: ni.GetNetworkScopedGWRouterName(status.Node),
	}
	ops, err = libovsdbops.CreateOrUpdateNATsOps(e.nbClient, ops, router, nats...)
	if err != nil {
		return nil, fmt.Errorf("unable to create snat rules, for router: %s, error: %v", router.Name, err)
	}
	return ops, nil
}

func (e *EgressIPController) deleteNATRuleOps(ni util.NetInfo, ops []ovsdb.Operation, status egressipv1.EgressIPStatusItem,
	egressIPName, podNamespace, podName string) ([]ovsdb.Operation, error) {
	var err error
	pV4 := libovsdbops.GetPredicate[*nbdb.NAT](getEgressIPNATDbIDs(egressIPName, podNamespace, podName, IPFamilyValueV4, e.controllerName), nil)
	pV6 := libovsdbops.GetPredicate[*nbdb.NAT](getEgressIPNATDbIDs(egressIPName, podNamespace, podName, IPFamilyValueV6, e.controllerName), nil)
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

// getNetworkFromPodAssignmentWithLock attempts to find a pods network from the pod assignment cache. If pod is not found
// in cache or no network set, nil network is return.
func (e *EgressIPController) getNetworkFromPodAssignmentWithLock(podKey string) util.NetInfo {
	e.podAssignmentMutex.Lock()
	defer e.podAssignmentMutex.Unlock()
	return e.getNetworkFromPodAssignment(podKey)
}

// getNetworkFromPodAssignmentWithLock attempts to find a pods network from the pod assignment cache. If pod is not found
// in cache or no network set, nil network is return.
func (e *EgressIPController) getNetworkFromPodAssignment(podKey string) util.NetInfo {
	podAssignment, ok := e.podAssignment[podKey]
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
	err = wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (done bool, err error) {

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

	var matchV4, matchV6 string
	// construct the policy match
	if v4 && ipv4UDNEnabledSvcAS != "" {
		matchV4 = fmt.Sprintf(`(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s`,
			ipv4EgressIPServedPodsAS, ipv4EgressServiceServedPodsAS, ipv4UDNEnabledSvcAS)
	}
	if v6 && ipv6UDNEnabledSvcAS != "" {
		if ipv6EgressIPServedPodsAS == "" || ipv6EgressServiceServedPodsAS == "" || ipv6UDNEnabledSvcAS == "" {
			return fmt.Errorf("address set hash name(s) %s not found", as.GetName())
		}
		matchV6 = fmt.Sprintf(`(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s`,
			ipv6EgressIPServedPodsAS, ipv6EgressServiceServedPodsAS, ipv6UDNEnabledSvcAS)
	}

	// Create global allow policy for UDN enabled service traffic
	if v4 && matchV4 != "" {
		dbIDs = getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, NoReRouteUDNPodToCDNSvc, IPFamilyValueV4, ni.GetNetworkName(), controllerName)
		if err := createLogicalRouterPolicy(nbClient, routerName, matchV4, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv4 no-rerouteUDN pod to CDN svc, err: %v", err)
		}
	}

	if v6 && matchV6 != "" {
		dbIDs = getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, NoReRouteUDNPodToCDNSvc, IPFamilyValueV6, ni.GetNetworkName(), controllerName)
		if err := createLogicalRouterPolicy(nbClient, routerName, matchV6, types.DefaultNoRereoutePriority, nil, dbIDs); err != nil {
			return fmt.Errorf("unable to create IPv6 no-reroute UDN pod to CDN svc policies, err: %v", err)
		}
	}
	return nil
}

func getPodKey(pod *kapi.Pod) string {
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
	if ni.TopologyType() == types.Layer2Topology {
		if nodeName == "" {
			return "", fmt.Errorf("node name is required to determine the Nodes gateway router name")
		}
		return ni.GetNetworkScopedGWRouterName(nodeName), nil
	}
	return ni.GetNetworkScopedClusterRouterName(), nil
}

func isEgressIPForUDNSupported() bool {
	return config.OVNKubernetesFeature.EnableInterconnect &&
		config.OVNKubernetesFeature.EnableNetworkSegmentation
}
