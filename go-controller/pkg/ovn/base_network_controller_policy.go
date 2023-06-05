package ovn

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrorsutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type netpolDefaultDenyACLType string

const (
	// netpolDefaultDenyACLType is used to distinguish default deny and arp allow acls create for the same port group
	defaultDenyACL netpolDefaultDenyACLType = "defaultDeny"
	arpAllowACL    netpolDefaultDenyACLType = "arpAllow"

	// arpAllowPolicyMatch is the match used when creating default allow ARP ACLs for a namespace
	arpAllowPolicyMatch   = "(arp || nd)"
	allowHairpinningACLID = "allow-hairpinning"
	// ovnStatelessNetPolAnnotationName is an annotation on K8s Network Policy resource to specify that all
	// the resulting OVN ACLs must be created as stateless
	ovnStatelessNetPolAnnotationName = "k8s.ovn.org/acl-stateless"
)

// defaultDenyPortGroups is a shared object and should be used by only 1 thread at a time
type defaultDenyPortGroups struct {
	// portName: map[portName]sets.String(policyNames)
	// store policies that are using every port in the map
	// these maps should be atomically updated with db operations
	// if adding a port to db for a policy fails, map shouldn't be changed
	ingressPortToPolicies map[string]sets.Set[string]
	egressPortToPolicies  map[string]sets.Set[string]
	// policies is a map of policies that use this port group
	// policy keys must be unique, and it can be retrieved with (np *networkPolicy) getKey()
	policies map[string]bool
}

// addPortsForPolicy adds port-policy association for default deny port groups and
// returns lists of new ports to add to the default deny port groups.
// If port should be added to ingress and/or egress default deny port group depends on policy spec.
func (sharedPGs *defaultDenyPortGroups) addPortsForPolicy(np *networkPolicy,
	portNamesToUUIDs map[string]string) (ingressDenyPorts, egressDenyPorts []string) {
	ingressDenyPorts = []string{}
	egressDenyPorts = []string{}

	if np.isIngress {
		for portName, portUUID := range portNamesToUUIDs {
			// if this is the first NP referencing this pod, then we
			// need to add it to the port group.
			if sharedPGs.ingressPortToPolicies[portName].Len() == 0 {
				ingressDenyPorts = append(ingressDenyPorts, portUUID)
				sharedPGs.ingressPortToPolicies[portName] = sets.Set[string]{}
			}
			// increment the reference count.
			sharedPGs.ingressPortToPolicies[portName].Insert(np.getKey())
		}
	}
	if np.isEgress {
		for portName, portUUID := range portNamesToUUIDs {
			if sharedPGs.egressPortToPolicies[portName].Len() == 0 {
				// again, reference count is 0, so add to port
				egressDenyPorts = append(egressDenyPorts, portUUID)
				sharedPGs.egressPortToPolicies[portName] = sets.Set[string]{}
			}
			// bump reference count
			sharedPGs.egressPortToPolicies[portName].Insert(np.getKey())
		}
	}
	return
}

// deletePortsForPolicy deletes port-policy association for default deny port groups,
// and returns lists of port UUIDs to delete from the default deny port groups.
// If port should be deleted from ingress and/or egress default deny port group depends on policy spec.
func (sharedPGs *defaultDenyPortGroups) deletePortsForPolicy(np *networkPolicy,
	portNamesToUUIDs map[string]string) (ingressDenyPorts, egressDenyPorts []string) {
	ingressDenyPorts = []string{}
	egressDenyPorts = []string{}

	if np.isIngress {
		for portName, portUUID := range portNamesToUUIDs {
			// Delete and Len can be used for zero-value nil set
			sharedPGs.ingressPortToPolicies[portName].Delete(np.getKey())
			if sharedPGs.ingressPortToPolicies[portName].Len() == 0 {
				ingressDenyPorts = append(ingressDenyPorts, portUUID)
				delete(sharedPGs.ingressPortToPolicies, portName)
			}
		}
	}
	if np.isEgress {
		for portName, portUUID := range portNamesToUUIDs {
			sharedPGs.egressPortToPolicies[portName].Delete(np.getKey())
			if sharedPGs.egressPortToPolicies[portName].Len() == 0 {
				egressDenyPorts = append(egressDenyPorts, portUUID)
				delete(sharedPGs.egressPortToPolicies, portName)
			}
		}
	}
	return
}

type networkPolicy struct {
	// For now networkPolicy has
	// 3 types of global events (those use bnc.networkPolicies to get networkPolicy object)
	// 1. Create network policy - create networkPolicy resources,
	// enable local events, and Update namespace loglevel event
	// 2. Update namespace loglevel - update ACLs for defaultDenyPortGroups and portGroup
	// 3. Delete network policy - disable local events, and Update namespace loglevel event,
	// send deletion signal to already running event handlers, delete resources
	//
	// 2 types of local events (those use the same networkPolicy object there were created for):
	// 1. localPod events - update portGroup, defaultDenyPortGroups and localPods
	// 2. peerNamespace events - add/delete gressPolicy address set, update ACLs for portGroup
	//
	// Delete network policy conflict with all other handlers, therefore we need to make sure it only runs
	// when no other handlers are executing, and that no other handlers will try to work with networkPolicy after
	// Delete network policy was called. This can be done with RWLock, if Delete network policy takes Write lock
	// and sets deleted field to true, and all other handlers take RLock and return immediately if deleted is true.
	// Create network Policy can also take Write lock while it is creating required resources.
	//
	// ACL updates are handled separately for different fields: namespace event will affect ACL.Log and ACL.Severity,
	// while peerNamespace events will affect ACL.Match. Therefore, no additional locking is required.
	//
	// We also need to make sure handlers of the same type can be executed in parallel, if this is not true, every
	// event handler can have it own additional lock to sync handlers of the same type.
	//
	// Allowed order of locking is bnc.networkPolicies key Lock -> networkPolicy.Lock
	// Don't take RLock from the same goroutine twice, it can lead to deadlock.
	sync.RWMutex

	name            string
	namespace       string
	ingressPolicies []*gressPolicy
	egressPolicies  []*gressPolicy
	isIngress       bool
	isEgress        bool

	// network policy owns only 1 local pod handler
	localPodHandler *factory.Handler
	// peer namespace handlers
	nsHandlerList []*factory.Handler
	// peerAddressSets stores PodSelectorAddressSet keys for peers that this network policy was successfully added to.
	// Required for cleanup.
	peerAddressSets []string

	// localPods is a map of pods affected by this policy.
	// It is used to update defaultDeny port group port counters, when deleting network policy.
	// Port should only be added here if it was successfully added to default deny port group,
	// and local port group in db.
	// localPods may be updated by multiple pod handlers at the same time,
	// therefore it uses a sync map to handle simultaneous access.
	// map of portName(string): portUUID(string)
	localPods sync.Map

	portGroupName string
	// this is a signal for related event handlers that they are/should be stopped.
	// it will be set to true before any networkPolicy infrastructure is deleted,
	// therefore every handler can either do its work and be sure all required resources are there,
	// or this value will be set to true and handler can't proceed.
	// Use networkPolicy.RLock to read this field and hold it for the whole event handling.
	deleted bool
}

func newNetworkPolicy(policy *knet.NetworkPolicy) *networkPolicy {
	policyTypeIngress, policyTypeEgress := getPolicyType(policy)
	np := &networkPolicy{
		name:            policy.Name,
		namespace:       policy.Namespace,
		ingressPolicies: make([]*gressPolicy, 0),
		egressPolicies:  make([]*gressPolicy, 0),
		isIngress:       policyTypeIngress,
		isEgress:        policyTypeEgress,
		nsHandlerList:   make([]*factory.Handler, 0),
		localPods:       sync.Map{},
	}
	return np
}

// NetpolNamespaceHandler controller namespace-network policy dependency for namespace ACLLoggingLevels
type NetpolNamespaceHandler interface {
	RegisterNetpolHandler(updateHandler func(namespace string, aclLogging *ACLLoggingLevels, relatedNPKeys map[string]bool) error)
	GetNamespaceACLLogging(ns string) *ACLLoggingLevels
	SubscribeToNamespaceUpdates(namespace string, npKey string, initialSync func(aclLogging *ACLLoggingLevels) error) error
	UnsubscribeFromNamespaceUpdates(namespace, npKey string)
}

type NetpolRequirements interface {
	AddConfigDurationRecord(kind, namespace, name string) ([]ovsdb.Operation, func(), time.Time, error)
	isPodScheduledinLocalZone(pod *kapi.Pod) bool
	podExpectedInLogicalCache(pod *kapi.Pod) bool
	getPodNADNames(pod *kapi.Pod) []string
	GetLogicalPortName(pod *kapi.Pod, nadName string) string
	logicalPortCacheGet(pod *kapi.Pod, nadName string) (*lpInfo, error)
	newNetpolRetryFramework(objectType reflect.Type, syncFunc func([]interface{}) error, extraParameters interface{}) *retry.RetryFramework

	// future addressSet controller's functions
	EnsurePodSelectorAddressSet(podSelector, namespaceSelector *metav1.LabelSelector,
		namespace, backRef string) (addrSetKey, psAddrSetHashV4, psAddrSetHashV6 string, err error)
	DeletePodSelectorAddressSet(addrSetKey, backRef string) error

	NetpolNamespaceHandler
}

// NetworkPolicyController provides an interface to handle network policy events: AddNetworkPolicy and DeleteNetworkPolicy.
// It also creates new watchFactories for pod and namespace types, therefore you need to make sure to watch these
// objects.
type NetworkPolicyController struct {
	// network policies map, key should be retrieved with getPolicyKey(policy *knet.NetworkPolicy).
	// network policies that failed to be created will also be added here, and can be retried or cleaned up later.
	// network policy is only deleted from this map after successful cleanup.
	// Allowed order of locking is networkPolicies key Lock -> networkPolicy.Lock
	networkPolicies *syncmap.SyncMap[*networkPolicy]

	// map of existing shared port groups for network policies
	// port group exists in the db if and only if port group key is present in this map
	// key is namespace
	// allowed locking order is networkPolicy.Lock -> sharedNetpolPortGroups key Lock
	// make sure to keep this order to avoid deadlocks
	sharedNetpolPortGroups *syncmap.SyncMap[*defaultDenyPortGroups]

	addressSetFactory addressset.AddressSetFactory
	nbClient          libovsdbclient.Client
	watchFactory      *factory.WatchFactory

	controllerName string
	enableMetrics  bool
	netInfo        util.NetInfo
	stopChan       chan struct{}
	wg             *sync.WaitGroup

	externalInterface NetpolRequirements
}

func NewNetworkPolicyController(addressSetFactory addressset.AddressSetFactory, nbClient libovsdbclient.Client,
	watchFactory *factory.WatchFactory, controllerName string, enableMetrics bool, netInfo util.NetInfo,
	stopChan chan struct{}, wg *sync.WaitGroup, netpolReqs NetpolRequirements) *NetworkPolicyController {
	c := &NetworkPolicyController{
		networkPolicies:        syncmap.NewSyncMap[*networkPolicy](),
		sharedNetpolPortGroups: syncmap.NewSyncMap[*defaultDenyPortGroups](),
		addressSetFactory:      addressSetFactory,
		nbClient:               nbClient,
		watchFactory:           watchFactory,
		controllerName:         controllerName,
		enableMetrics:          enableMetrics,
		netInfo:                netInfo,
		stopChan:               stopChan,
		wg:                     wg,
		externalInterface:      netpolReqs,
	}
	c.externalInterface.RegisterNetpolHandler(c.handleNetPolNamespaceUpdate)
	return c
}

// SyncNetworkPoliciesCommon syncs logical entities associated with existing network policies.
// It serves both networkpolicies (for default network) and multi-networkpolicies (for secondary networks)
func (c *NetworkPolicyController) SyncNetworkPoliciesCommon(expectedPolicies map[string]map[string]bool) error {
	// find network policies that don't exist in k8s anymore, but still present in the dbs, and cleanup.
	// Peer address sets and network policy's port groups (together with acls) will be cleaned up.
	// Delete port groups with acls first, since address sets may be referenced in these acls, and
	// cause SyntaxError in ovn-controller, if address sets deleted first, but acls still reference them.

	// cleanup port groups
	// netpol-owned port groups first
	stalePGNames := sets.Set[string]{}
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetworkPolicy, c.controllerName, nil)
	p := libovsdbops.GetPredicate[*nbdb.PortGroup](predicateIDs, func(item *nbdb.PortGroup) bool {
		namespace, policyName, err := parsePGPolicyKey(item.ExternalIDs[libovsdbops.ObjectNameKey.String()])
		if err != nil {
			klog.Errorf("Failed to sync stale network policy %v: port group IDs parsing failed: %w",
				item.ExternalIDs[libovsdbops.ObjectNameKey.String()], err)
			return false
		}
		if !expectedPolicies[namespace][policyName] {
			// policy doesn't exist on k8s, cleanup
			stalePGNames.Insert(item.Name)
		}
		return false
	})
	_, err := libovsdbops.FindPortGroupsWithPredicate(c.nbClient, p)
	if err != nil {
		return fmt.Errorf("cannot find NetworkPolicy port groups: %v", err)
	}

	// default deny port groups
	predicateIDs = libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetpolNamespace, c.controllerName, nil)
	p = libovsdbops.GetPredicate[*nbdb.PortGroup](predicateIDs, func(item *nbdb.PortGroup) bool {
		namespace := item.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		if _, ok := expectedPolicies[namespace]; !ok {
			// no policies in that namespace are found, delete default deny port group
			stalePGNames.Insert(item.Name)
		}
		return false
	})
	_, err = libovsdbops.FindPortGroupsWithPredicate(c.nbClient, p)
	if err != nil {
		return fmt.Errorf("cannot find default deny NetworkPolicy port groups: %v", err)
	}

	if len(stalePGNames) > 0 {
		err = libovsdbops.DeletePortGroups(c.nbClient, sets.List[string](stalePGNames)...)
		if err != nil {
			return fmt.Errorf("error removing stale port groups %v: %v", stalePGNames, err)
		}
		klog.Infof("Network policy sync cleaned up %d stale port groups", len(stalePGNames))
	}

	return nil
}

func (c *NetworkPolicyController) getDefaultDenyPolicyACLIDs(ns string, aclDir aclDirection,
	defaultACLType netpolDefaultDenyACLType) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNamespace, c.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ns,
			// in the same namespace there can be 2 default deny port groups, egress and ingress,
			// every port group has default deny and arp allow acl.
			libovsdbops.PolicyDirectionKey: string(aclDir),
			libovsdbops.TypeKey:            string(defaultACLType),
		})
}

func (c *NetworkPolicyController) getDefaultDenyPolicyPortGroupIDs(ns string, aclDir aclDirection) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetpolNamespace, c.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ns,
			// in the same namespace there can be 2 default deny port groups, egress and ingress,
			// every port group has default deny and arp allow acl.
			libovsdbops.PolicyDirectionKey: string(aclDir),
		})
}

func (c *NetworkPolicyController) defaultDenyPortGroupName(namespace string, aclDir aclDirection) string {
	return libovsdbops.GetPortGroupName(c.getDefaultDenyPolicyPortGroupIDs(namespace, aclDir))
}

func (c *NetworkPolicyController) buildDenyACLs(namespace, pgName string, aclLogging *ACLLoggingLevels,
	aclDir aclDirection) (denyACL, allowACL *nbdb.ACL) {
	denyMatch := getACLMatch(pgName, "", aclDir)
	allowMatch := getACLMatch(pgName, arpAllowPolicyMatch, aclDir)
	aclPipeline := aclDirectionToACLPipeline(aclDir)

	denyACL = BuildACL(c.getDefaultDenyPolicyACLIDs(namespace, aclDir, defaultDenyACL),
		types.DefaultDenyPriority, denyMatch, nbdb.ACLActionDrop, aclLogging, aclPipeline)
	allowACL = BuildACL(c.getDefaultDenyPolicyACLIDs(namespace, aclDir, arpAllowACL),
		types.DefaultAllowPriority, allowMatch, nbdb.ACLActionAllow, nil, aclPipeline)
	return
}

func (c *NetworkPolicyController) addPolicyToDefaultPortGroups(np *networkPolicy, aclLogging *ACLLoggingLevels) error {
	return c.sharedNetpolPortGroups.DoWithLock(np.namespace, func(pgKey string) error {
		sharedPGs, loaded := c.sharedNetpolPortGroups.LoadOrStore(pgKey, &defaultDenyPortGroups{
			ingressPortToPolicies: map[string]sets.Set[string]{},
			egressPortToPolicies:  map[string]sets.Set[string]{},
			policies:              map[string]bool{},
		})
		if !loaded {
			// create port groups with acls
			err := c.createDefaultDenyPGAndACLs(np.namespace, np.name, aclLogging)
			if err != nil {
				c.sharedNetpolPortGroups.Delete(pgKey)
				return fmt.Errorf("failed to create default deny port groups: %v", err)
			}
		}
		sharedPGs.policies[np.getKey()] = true
		return nil
	})
}

func (c *NetworkPolicyController) delPolicyFromDefaultPortGroups(np *networkPolicy) error {
	return c.sharedNetpolPortGroups.DoWithLock(np.namespace, func(pgKey string) error {
		sharedPGs, found := c.sharedNetpolPortGroups.Load(pgKey)
		if !found {
			return nil
		}
		delete(sharedPGs.policies, np.getKey())
		if len(sharedPGs.policies) == 0 {
			// last policy was deleted, delete port group
			err := c.deleteDefaultDenyPGAndACLs(np.namespace)
			if err != nil {
				return fmt.Errorf("failed to delete defaul deny port group: %v", err)
			}
			c.sharedNetpolPortGroups.Delete(pgKey)
		}
		return nil
	})
}

// createDefaultDenyPGAndACLs creates the default port groups and acls for a namespace
// must be called with defaultDenyPortGroups lock
func (c *NetworkPolicyController) createDefaultDenyPGAndACLs(namespace, policy string, aclLogging *ACLLoggingLevels) error {
	ingressPGIDs := c.getDefaultDenyPolicyPortGroupIDs(namespace, aclIngress)
	ingressPGName := libovsdbops.GetPortGroupName(ingressPGIDs)
	ingressDenyACL, ingressAllowACL := c.buildDenyACLs(namespace, ingressPGName, aclLogging, aclIngress)
	egressPGIDs := c.getDefaultDenyPolicyPortGroupIDs(namespace, aclEgress)
	egressPGName := libovsdbops.GetPortGroupName(egressPGIDs)
	egressDenyACL, egressAllowACL := c.buildDenyACLs(namespace, egressPGName, aclLogging, aclEgress)
	ops, err := libovsdbops.CreateOrUpdateACLsOps(c.nbClient, nil, ingressDenyACL, ingressAllowACL, egressDenyACL, egressAllowACL)
	if err != nil {
		return err
	}

	ingressPG := libovsdbops.BuildPortGroup(ingressPGIDs, nil, []*nbdb.ACL{ingressDenyACL, ingressAllowACL})
	egressPG := libovsdbops.BuildPortGroup(egressPGIDs, nil, []*nbdb.ACL{egressDenyACL, egressAllowACL})
	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(c.nbClient, ops, ingressPG, egressPG)
	if err != nil {
		return err
	}

	recordOps, txOkCallBack, _, err := c.externalInterface.AddConfigDurationRecord("networkpolicy", namespace, policy)
	if err != nil {
		klog.Errorf("Failed to record config duration: %v", err)
	}
	ops = append(ops, recordOps...)
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return err
	}
	txOkCallBack()

	return nil
}

// deleteDefaultDenyPGAndACLs deletes the default port groups and acls for a namespace
// must be called with defaultDenyPortGroups lock
func (c *NetworkPolicyController) deleteDefaultDenyPGAndACLs(namespace string) error {
	ingressPGName := c.defaultDenyPortGroupName(namespace, aclIngress)
	egressPGName := c.defaultDenyPortGroupName(namespace, aclEgress)

	ops, err := libovsdbops.DeletePortGroupsOps(c.nbClient, nil, ingressPGName, egressPGName)
	if err != nil {
		return err
	}
	// No need to delete ACLs, since they will be garbage collected with deleted port groups
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to transact deleteDefaultDenyPGAndACLs: %v", err)
	}

	return nil
}

// must be called with namespace lock
func (c *NetworkPolicyController) updateACLLoggingForPolicy(np *networkPolicy, aclLogging *ACLLoggingLevels) error {
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}

	// Predicate for given network policy ACLs
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetworkPolicy, c.controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: getACLPolicyKey(np.namespace, np.name),
	})
	p := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	return UpdateACLLoggingWithPredicate(c.nbClient, p, aclLogging)
}

func (c *NetworkPolicyController) updateACLLoggingForDefaultACLs(ns string, aclLogging *ACLLoggingLevels) error {
	return c.sharedNetpolPortGroups.DoWithLock(ns, func(pgKey string) error {
		_, loaded := c.sharedNetpolPortGroups.Load(pgKey)
		if !loaded {
			// shared port group doesn't exist, nothing to update
			return nil
		}
		predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNamespace, c.controllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.ObjectNameKey: ns,
				libovsdbops.TypeKey:       string(defaultDenyACL),
			})
		p := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
		defaultDenyACLs, err := libovsdbops.FindACLsWithPredicate(c.nbClient, p)
		if err != nil {
			return fmt.Errorf("failed to find netpol default deny acls for namespace %s: %v", ns, err)
		}
		if err := UpdateACLLogging(c.nbClient, defaultDenyACLs, aclLogging); err != nil {
			return fmt.Errorf("unable to update ACL logging for namespace %s: %w", ns, err)
		}
		return nil
	})
}

// handleNetPolNamespaceUpdate should update all network policies related to given namespace.
// Must be called with namespace Lock, should be retriable
func (c *NetworkPolicyController) handleNetPolNamespaceUpdate(namespace string, aclLogging *ACLLoggingLevels, relatedNetworkPolicies map[string]bool) error {
	// update shared port group ACLs
	if err := c.updateACLLoggingForDefaultACLs(namespace, aclLogging); err != nil {
		return fmt.Errorf("failed to update default deny ACLs for namespace %s: %v", namespace, err)
	}
	// now update network policy specific ACLs
	klog.V(5).Infof("Setting network policy ACLs for ns: %s", namespace)
	for npKey := range relatedNetworkPolicies {
		err := c.networkPolicies.DoWithLock(npKey, func(key string) error {
			np, found := c.networkPolicies.Load(npKey)
			if !found {
				klog.Errorf("Netpol was deleted from cache, but not from namespace related objects")
				return nil
			}
			return c.updateACLLoggingForPolicy(np, aclLogging)
		})
		if err != nil {
			return fmt.Errorf("unable to update ACL for network policy %s: %v", npKey, err)
		}
		klog.Infof("ACL for network policy: %s, updated to new log level: %s", npKey, aclLogging.Allow)
	}
	return nil
}

// getPolicyType returns whether the policy is of type ingress and/or egress
func getPolicyType(policy *knet.NetworkPolicy) (bool, bool) {
	var policyTypeIngress bool
	var policyTypeEgress bool

	for _, policyType := range policy.Spec.PolicyTypes {
		if policyType == knet.PolicyTypeIngress {
			policyTypeIngress = true
		} else if policyType == knet.PolicyTypeEgress {
			policyTypeEgress = true
		}
	}

	return policyTypeIngress, policyTypeEgress
}

// getNewLocalPolicyPorts will find and return port info for every given pod obj, that is not found in
// np.localPods.
// if there are problems with fetching port info from logicalPortCache, error will be added to returned error array.
func (c *NetworkPolicyController) getNewLocalPolicyPorts(np *networkPolicy,
	objs ...interface{}) (policyPortsToUUIDs map[string]string, policyPortUUIDs []string, errs []error) {

	policyPortUUIDs = []string{}
	policyPortsToUUIDs = map[string]string{}
	errs = []error{}

	for _, obj := range objs {
		pod := obj.(*kapi.Pod)
		if pod.Spec.NodeName == "" {
			// pod is not yet scheduled, will receive update event for it
			continue
		}

		if !c.externalInterface.isPodScheduledinLocalZone(pod) {
			continue
		}

		// Skip pods that will never be present in logicalPortCache,
		// e.g. hostNetwork pods, overlay node pods, or completed pods
		if !c.externalInterface.podExpectedInLogicalCache(pod) {
			continue
		}

		nadNames := c.externalInterface.getPodNADNames(pod)
		for _, nadName := range nadNames {
			logicalPortName := c.externalInterface.GetLogicalPortName(pod, nadName)
			if _, ok := np.localPods.Load(logicalPortName); ok {
				// port is already added for this policy
				continue
			}

			// Return error for retry if
			// 1. getting pod LSP from the cache fails,
			// 2. the gotten LSP is scheduled for removal (stateful-sets).
			portInfo, err := c.externalInterface.logicalPortCacheGet(pod, nadName)
			if err != nil {
				klog.Warningf("Failed to get get LSP for pod %s/%s NAD %s for networkPolicy %s, err: %v",
					pod.Namespace, pod.Name, nadName, np.name, err)
				errs = append(errs, fmt.Errorf("unable to get port info for pod %s/%s NAD %s", pod.Namespace, pod.Name, nadName))
				continue
			}

			// Add pod to errObjs if LSP is scheduled for deletion
			if !portInfo.expires.IsZero() {
				klog.Warningf("Stale LSP %s for network policy %s found in cache",
					portInfo.name, np.name)
				errs = append(errs, fmt.Errorf("unable to get port info for pod %s/%s NAD %s", pod.Namespace, pod.Name, nadName))
				continue
			}

			// LSP get succeeded and LSP is up to fresh
			klog.V(5).Infof("Fresh LSP %s for network policy %s found in cache",
				portInfo.name, np.name)

			policyPortUUIDs = append(policyPortUUIDs, portInfo.uuid)
			policyPortsToUUIDs[portInfo.name] = portInfo.uuid
		}
	}

	return
}

// getExistingLocalPolicyPorts will find and return port info for every given pod obj, that is present in np.localPods.
func (c *NetworkPolicyController) getExistingLocalPolicyPorts(np *networkPolicy,
	objs ...interface{}) (policyPortsToUUIDs map[string]string, policyPortUUIDs []string) {
	klog.Infof("Processing NetworkPolicy %s/%s to delete %d local pods...", np.namespace, np.name, len(objs))

	policyPortUUIDs = []string{}
	policyPortsToUUIDs = map[string]string{}
	for _, obj := range objs {
		pod := obj.(*kapi.Pod)

		nadNames := c.externalInterface.getPodNADNames(pod)
		for _, nadName := range nadNames {
			logicalPortName := c.externalInterface.GetLogicalPortName(pod, nadName)
			loadedPortUUID, ok := np.localPods.Load(logicalPortName)
			if !ok {
				// port is already deleted for this policy
				continue
			}
			portUUID := loadedPortUUID.(string)

			policyPortsToUUIDs[logicalPortName] = portUUID
			policyPortUUIDs = append(policyPortUUIDs, portUUID)
		}
	}
	return
}

// denyPGAddPorts adds ports to default deny port groups.
// It also can take existing ops e.g. to add port to network policy port group and transact it.
// It only adds new ports that do not already exist in the deny port groups.
func (c *NetworkPolicyController) denyPGAddPorts(np *networkPolicy, portNamesToUUIDs map[string]string, ops []ovsdb.Operation) error {
	var err error
	ingressDenyPGName := c.defaultDenyPortGroupName(np.namespace, aclIngress)
	egressDenyPGName := c.defaultDenyPortGroupName(np.namespace, aclEgress)

	pgKey := np.namespace
	// this lock guarantees that sharedPortGroup counters will be updated atomically
	// with adding port to port group in db.
	c.sharedNetpolPortGroups.LockKey(pgKey)
	pgLocked := true
	defer func() {
		if pgLocked {
			c.sharedNetpolPortGroups.UnlockKey(pgKey)
		}
	}()
	sharedPGs, ok := c.sharedNetpolPortGroups.Load(pgKey)
	if !ok {
		// Port group doesn't exist
		return fmt.Errorf("port groups for ns %s don't exist", np.namespace)
	}

	ingressDenyPorts, egressDenyPorts := sharedPGs.addPortsForPolicy(np, portNamesToUUIDs)
	// counters were updated, update back to initial values on error
	defer func() {
		if err != nil {
			sharedPGs.deletePortsForPolicy(np, portNamesToUUIDs)
		}
	}()

	if len(ingressDenyPorts) != 0 || len(egressDenyPorts) != 0 {
		// db changes required
		ops, err = libovsdbops.AddPortsToPortGroupOps(c.nbClient, ops, ingressDenyPGName, ingressDenyPorts...)
		if err != nil {
			return fmt.Errorf("unable to get add ports to %s port group ops: %v", ingressDenyPGName, err)
		}

		ops, err = libovsdbops.AddPortsToPortGroupOps(c.nbClient, ops, egressDenyPGName, egressDenyPorts...)
		if err != nil {
			return fmt.Errorf("unable to get add ports to %s port group ops: %v", egressDenyPGName, err)
		}
	} else {
		// shared pg was updated and doesn't require db changes, no need to hold the lock
		c.sharedNetpolPortGroups.UnlockKey(pgKey)
		pgLocked = false
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return fmt.Errorf("unable to transact add ports to default deny port groups: %v", err)
	}
	return nil
}

// denyPGDeletePorts deletes ports from default deny port groups.
// Set useLocalPods = true, when deleting networkPolicy to remove all its ports from defaultDeny port groups.
// It also can take existing ops e.g. to delete ports from network policy port group and transact it.
func (c *NetworkPolicyController) denyPGDeletePorts(np *networkPolicy, portNamesToUUIDs map[string]string, useLocalPods bool,
	ops []ovsdb.Operation) error {
	var err error
	if useLocalPods {
		portNamesToUUIDs = map[string]string{}
		np.localPods.Range(func(key, value interface{}) bool {
			portNamesToUUIDs[key.(string)] = value.(string)
			return true
		})
	}
	if len(portNamesToUUIDs) != 0 {
		ingressDenyPGName := c.defaultDenyPortGroupName(np.namespace, aclIngress)
		egressDenyPGName := c.defaultDenyPortGroupName(np.namespace, aclEgress)

		pgKey := np.namespace
		// this lock guarantees that sharedPortGroup counters will be updated atomically
		// with adding port to port group in db.
		c.sharedNetpolPortGroups.LockKey(pgKey)
		pgLocked := true
		defer func() {
			if pgLocked {
				c.sharedNetpolPortGroups.UnlockKey(pgKey)
			}
		}()
		sharedPGs, ok := c.sharedNetpolPortGroups.Load(pgKey)
		if !ok {
			// Port group doesn't exist, nothing to clean up
			klog.Infof("Skip delete ports from default deny port group: port group doesn't exist")
		} else {
			ingressDenyPorts, egressDenyPorts := sharedPGs.deletePortsForPolicy(np, portNamesToUUIDs)
			// counters were updated, update back to initial values on error
			defer func() {
				if err != nil {
					sharedPGs.addPortsForPolicy(np, portNamesToUUIDs)
				}
			}()

			if len(ingressDenyPorts) != 0 || len(egressDenyPorts) != 0 {
				// db changes required
				ops, err = libovsdbops.DeletePortsFromPortGroupOps(c.nbClient, ops, ingressDenyPGName, ingressDenyPorts...)
				if err != nil {
					return fmt.Errorf("unable to get del ports from %s port group ops: %v", ingressDenyPGName, err)
				}

				ops, err = libovsdbops.DeletePortsFromPortGroupOps(c.nbClient, ops, egressDenyPGName, egressDenyPorts...)
				if err != nil {
					return fmt.Errorf("unable to get del ports from %s port group ops: %v", egressDenyPGName, err)
				}
			} else {
				// shared pg was updated and doesn't require db changes, no need to hold the lock
				c.sharedNetpolPortGroups.UnlockKey(pgKey)
				pgLocked = false
			}
		}
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return fmt.Errorf("unable to transact del ports from default deny port groups: %v", err)
	}

	return nil
}

// handleLocalPodSelectorAddFunc adds a new pod to an existing NetworkPolicy, should be retriable.
func (c *NetworkPolicyController) handleLocalPodSelectorAddFunc(np *networkPolicy, objs ...interface{}) error {
	if c.enableMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolLocalPodEvent("add", duration)
		}()
	}
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}
	// get info for new pods that are not listed in np.localPods
	portNamesToUUIDs, policyPortUUIDs, errs := c.getNewLocalPolicyPorts(np, objs...)
	// for multiple objects, try to update the ones that were fetched successfully
	// return error for errPods in the end
	if len(portNamesToUUIDs) > 0 {
		var err error
		// add pods to policy port group
		var ops []ovsdb.Operation
		if !PortGroupHasPorts(c.nbClient, np.portGroupName, policyPortUUIDs) {
			ops, err = libovsdbops.AddPortsToPortGroupOps(c.nbClient, nil, np.portGroupName, policyPortUUIDs...)
			if err != nil {
				return fmt.Errorf("unable to get ops to add new pod to policy port group %s: %v", np.portGroupName, err)
			}
		}
		// add pods to default deny port group
		// make sure to only pass newly added pods
		// ops will be transacted by denyPGAddPorts
		if err = c.denyPGAddPorts(np, portNamesToUUIDs, ops); err != nil {
			return fmt.Errorf("unable to add new pod to default deny port group: %v", err)
		}
		// all operations were successful, update np.localPods
		for portName, portUUID := range portNamesToUUIDs {
			np.localPods.Store(portName, portUUID)
		}
	}

	if len(errs) > 0 {
		return kerrorsutil.NewAggregate(errs)
	}
	return nil
}

// handleLocalPodSelectorDelFunc handles delete event for local pod, should be retriable
func (c *NetworkPolicyController) handleLocalPodSelectorDelFunc(np *networkPolicy, objs ...interface{}) error {
	if c.enableMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolLocalPodEvent("delete", duration)
		}()
	}
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}

	portNamesToUUIDs, policyPortUUIDs := c.getExistingLocalPolicyPorts(np, objs...)

	if len(portNamesToUUIDs) > 0 {
		var err error
		// del pods from policy port group
		var ops []ovsdb.Operation
		ops, err = libovsdbops.DeletePortsFromPortGroupOps(c.nbClient, nil, np.portGroupName, policyPortUUIDs...)
		if err != nil {
			return fmt.Errorf("unable to get ops to add new pod to policy port group: %v", err)
		}
		// delete pods from default deny port group
		if err = c.denyPGDeletePorts(np, portNamesToUUIDs, false, ops); err != nil {
			return fmt.Errorf("unable to add new pod to default deny port group: %v", err)
		}
		// all operations were successful, update np.localPods
		for portName := range portNamesToUUIDs {
			np.localPods.Delete(portName)
		}
	}

	return nil
}

// This function starts a watcher for local pods. Sync function and add event for every existing pod
// will be executed sequentially first, and an error will be returned if something fails.
// LocalPodSelectorType uses handleLocalPodSelectorAddFunc on Add and Update,
// and handleLocalPodSelectorDelFunc on Delete.
func (c *NetworkPolicyController) addLocalPodHandler(policy *knet.NetworkPolicy, np *networkPolicy) error {
	// NetworkPolicy is validated by the apiserver
	sel, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
	if err != nil {
		klog.Errorf("Could not set up watcher for local pods: %v", err)
		return err
	}

	// Add all local pods in a syncFunction to minimize db ops.
	syncFunc := func(objs []interface{}) error {
		// ignore returned error, since any pod that wasn't properly handled will be retried individually.
		_ = c.handleLocalPodSelectorAddFunc(np, objs...)
		return nil
	}
	retryLocalPods := c.externalInterface.newNetpolRetryFramework(
		factory.LocalPodSelectorType,
		syncFunc,
		&NetworkPolicyExtraParameters{
			np: np,
		})

	podHandler, err := retryLocalPods.WatchResourceFiltered(policy.Namespace, sel)
	if err != nil {
		klog.Errorf("WatchResource failed for addLocalPodHandler: %v", err)
		return err
	}

	np.localPodHandler = podHandler
	return nil
}

func (c *NetworkPolicyController) getNetworkPolicyPortGroupDbIDs(namespace, name string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetworkPolicy, c.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: fmt.Sprintf("%s_%s", namespace, name),
		})
}

func parsePGPolicyKey(pgPolicyKey string) (string, string, error) {
	s := strings.Split(pgPolicyKey, "_")
	if len(s) != 2 {
		return "", "", fmt.Errorf("failed to parse network policy port group key %s, "+
			"expected format <policyNamespace>_<policyName>", pgPolicyKey)
	}
	return s[0], s[1], nil
}

func (c *NetworkPolicyController) getNetworkPolicyPGName(namespace, name string) string {
	return libovsdbops.GetPortGroupName(c.getNetworkPolicyPortGroupDbIDs(namespace, name))
}

type policyHandler struct {
	gress             *gressPolicy
	namespaceSelector *metav1.LabelSelector
}

// createNetworkPolicy creates a network policy, should be retriable.
// If network policy with given key exists, it will try to clean it up first, and return an error if it fails.
// No need to log network policy key here, because caller of createNetworkPolicy should prepend error message with
// that information.
func (c *NetworkPolicyController) createNetworkPolicy(policy *knet.NetworkPolicy, aclLogging *ACLLoggingLevels) (*networkPolicy, error) {
	// To avoid existing connections disruption, make sure to apply allow ACLs before applying deny ACLs.
	// This requires to start peer handlers before local pod handlers.
	// 1. Cleanup old policy if it failed to be created
	// 2. Build gress policies, create addressSets for peers
	// 3. Add policy to default deny port group.
	// 4. Build policy ACLs and port group. All the local pods that this policy
	// selects will be eventually added to this port group.
	// Pods are not added to default deny port groups yet, this is just a preparation step
	// 5. Unlock networkPolicy before starting pod handlers to avoid deadlock
	// since pod handlers take np.RLock
	// 6. Start peer handlers to update all allow rules first
	// 7. Start local pod handlers, that will update networkPolicy and default deny port groups with selected pods.

	npKey := getPolicyKey(policy)
	var np *networkPolicy
	var policyHandlers []*policyHandler

	// network policy will be annotated with this
	// annotation -- [ "k8s.ovn.org/acl-stateless": "true"] for the ingress/egress
	// policies to be added as stateless OVN ACL's.
	// if the above annotation is not present or set to false in network policy,
	// then corresponding egress/ingress policies will be added as stateful OVN ACL's.
	var statelessNetPol bool
	if config.OVNKubernetesFeature.EnableStatelessNetPol {
		// look for stateless annotation if the statlessNetPol feature flag is enabled
		val, ok := policy.Annotations[ovnStatelessNetPolAnnotationName]
		if ok && val == "true" {
			statelessNetPol = true
		}
	}

	err := c.networkPolicies.DoWithLock(npKey, func(npKey string) error {
		oldNP, found := c.networkPolicies.Load(npKey)
		if found {
			// 1. Cleanup old policy if it failed to be created
			if cleanupErr := c.cleanupNetworkPolicy(oldNP); cleanupErr != nil {
				return fmt.Errorf("cleanup for retrying network policy create failed: %v", cleanupErr)
			}
		}
		np, found = c.networkPolicies.LoadOrStore(npKey, newNetworkPolicy(policy))
		if found {
			// that should never happen, because successful cleanup will delete np from bnc.networkPolicies
			return fmt.Errorf("network policy is found in the system, "+
				"while it should've been cleaned up, obj: %+v", np)
		}
		np.Lock()
		npLocked := true
		// we unlock np in the middle of this function, use npLocked to track if it was already unlocked explicitly
		defer func() {
			if npLocked {
				np.Unlock()
			}
		}()
		// no need to check np.deleted, since the object has just been created
		// now we have a new np stored in bnc.networkPolicies
		var err error

		if aclLogging.Deny != "" || aclLogging.Allow != "" {
			klog.Infof("ACL logging for network policy %s in namespace %s set to deny=%s, allow=%s",
				policy.Name, policy.Namespace, aclLogging.Deny, aclLogging.Allow)
		}

		// 2. Build gress policies, create addressSets for peers

		// Consider both ingress and egress rules of the policy regardless of this
		// policy type. A pod is isolated as long as as it is selected by any
		// namespace policy. Since we don't process all namespace policies on a
		// given policy update that might change the isolation status of a selected
		// pod, we have created the allow ACLs derived from the policy rules in case
		// the selected pods become isolated in the future even if that is not their
		// current status.

		// Go through each ingress rule.  For each ingress rule, create an
		// addressSet for the peer pods.
		for i, ingressJSON := range policy.Spec.Ingress {
			klog.V(5).Infof("Network policy ingress is %+v", ingressJSON)

			ingress := newGressPolicy(knet.PolicyTypeIngress, i, policy.Namespace, policy.Name, c.controllerName, statelessNetPol, c.netInfo)
			// append ingress policy to be able to cleanup created address set
			// see cleanupNetworkPolicy for details
			np.ingressPolicies = append(np.ingressPolicies, ingress)

			// Each ingress rule can have multiple ports to which we allow traffic.
			for _, portJSON := range ingressJSON.Ports {
				ingress.addPortPolicy(&portJSON)
			}

			for _, fromJSON := range ingressJSON.From {
				handler, err := c.setupGressPolicy(np, ingress, fromJSON)
				if err != nil {
					return err
				}
				if handler != nil {
					policyHandlers = append(policyHandlers, handler)
				}
			}
		}

		// Go through each egress rule.  For each egress rule, create an
		// addressSet for the peer pods.
		for i, egressJSON := range policy.Spec.Egress {
			klog.V(5).Infof("Network policy egress is %+v", egressJSON)

			egress := newGressPolicy(knet.PolicyTypeEgress, i, policy.Namespace, policy.Name, c.controllerName, statelessNetPol, c.netInfo)
			// append ingress policy to be able to cleanup created address set
			// see cleanupNetworkPolicy for details
			np.egressPolicies = append(np.egressPolicies, egress)

			// Each egress rule can have multiple ports to which we allow traffic.
			for _, portJSON := range egressJSON.Ports {
				egress.addPortPolicy(&portJSON)
			}

			for _, toJSON := range egressJSON.To {
				handler, err := c.setupGressPolicy(np, egress, toJSON)
				if err != nil {
					return err
				}
				if handler != nil {
					policyHandlers = append(policyHandlers, handler)
				}
			}
		}
		klog.Infof("Policy %s added to peer address sets %v", npKey, np.peerAddressSets)

		// 3. Add policy to default deny port group
		// Pods are not added to default deny port groups yet, this is just a preparation step
		err = c.addPolicyToDefaultPortGroups(np, aclLogging)
		if err != nil {
			return err
		}

		// 4. Build policy ACLs and port group. All the local pods that this policy
		// selects will be eventually added to this port group.

		pgDbIDs := c.getNetworkPolicyPortGroupDbIDs(policy.Namespace, policy.Name)
		np.portGroupName = libovsdbops.GetPortGroupName(pgDbIDs)
		ops := []ovsdb.Operation{}

		acls := c.buildNetworkPolicyACLs(np, aclLogging)
		ops, err = libovsdbops.CreateOrUpdateACLsOps(c.nbClient, ops, acls...)
		if err != nil {
			return fmt.Errorf("failed to create ACL ops: %v", err)
		}

		pg := libovsdbops.BuildPortGroup(pgDbIDs, nil, acls)
		ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(c.nbClient, ops, pg)
		if err != nil {
			return fmt.Errorf("failed to create ops to add port to a port group: %v", err)
		}

		var recordOps []ovsdb.Operation
		var txOkCallBack func()
		recordOps, txOkCallBack, _, err = c.externalInterface.AddConfigDurationRecord("networkpolicy", policy.Namespace, policy.Name)
		if err != nil {
			klog.Errorf("Failed to record config duration: %v", err)
		}
		ops = append(ops, recordOps...)

		_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to run ovsdb txn to add ports to port group: %v", err)
		}
		txOkCallBack()

		// 5. Unlock network policy before starting pod handlers to avoid deadlock,
		// since pod handlers take np.RLock
		np.Unlock()
		npLocked = false

		// 6. Start peer handlers to update all allow rules first
		for _, handler := range policyHandlers {
			// For each peer namespace selector, we create a watcher that
			// populates ingress.peerAddressSets
			err = c.addPeerNamespaceHandler(handler.namespaceSelector, handler.gress, np)
			if err != nil {
				return fmt.Errorf("failed to start peer handler: %v", err)
			}
		}

		// 7. Start local pod handlers, that will update networkPolicy and default deny port groups with selected pods.
		err = c.addLocalPodHandler(policy, np)
		if err != nil {
			return fmt.Errorf("failed to start local pod handler: %v", err)
		}

		return nil
	})
	return np, err
}

func (c *NetworkPolicyController) setupGressPolicy(np *networkPolicy, gp *gressPolicy,
	peer knet.NetworkPolicyPeer) (*policyHandler, error) {
	// Add IPBlock to ingress network policy
	if peer.IPBlock != nil {
		gp.addIPBlock(peer.IPBlock)
		return nil, nil
	}
	if peer.PodSelector == nil && peer.NamespaceSelector == nil {
		// undefined behaviour
		klog.Errorf("setupGressPolicy failed: all fields unset")
		return nil, nil
	}
	gp.hasPeerSelector = true

	podSelector := peer.PodSelector
	if podSelector == nil {
		// nil pod selector is equivalent to empty pod selector, which selects all
		podSelector = &metav1.LabelSelector{}
	}
	podSel, _ := metav1.LabelSelectorAsSelector(podSelector)
	nsSel, _ := metav1.LabelSelectorAsSelector(peer.NamespaceSelector)

	if podSel.Empty() && (peer.NamespaceSelector == nil || !nsSel.Empty()) {
		// namespace-based filtering
		if peer.NamespaceSelector == nil {
			// nil namespace selector means same namespace
			_, err := gp.addNamespaceAddressSet(np.namespace, c.addressSetFactory)
			if err != nil {
				return nil, fmt.Errorf("failed to add namespace address set for gress policy: %w", err)
			}
		} else if !nsSel.Empty() {
			// namespace selector, use namespace address sets
			handler := &policyHandler{
				gress:             gp,
				namespaceSelector: peer.NamespaceSelector,
			}
			return handler, nil
		}
	} else {
		// use podSelector address set
		// np.namespace will be used when fromJSON.NamespaceSelector = nil
		asKey, ipv4as, ipv6as, err := c.externalInterface.EnsurePodSelectorAddressSet(
			podSelector, peer.NamespaceSelector, np.namespace, np.getKeyWithKind())
		// even if GetPodSelectorAddressSet failed, add key for future cleanup or retry.
		np.peerAddressSets = append(np.peerAddressSets, asKey)
		if err != nil {
			return nil, fmt.Errorf("failed to ensure pod selector address set %s: %v", asKey, err)
		}
		gp.addPeerAddressSets(ipv4as, ipv6as)
	}
	return nil, nil
}

// AddNetworkPolicy creates and applies OVN ACLs to pod logical switch
// ports from Kubernetes NetworkPolicy objects using OVN Port Groups
// if AddNetworkPolicy fails, create or delete operation can be retried
func (c *NetworkPolicyController) AddNetworkPolicy(policy *knet.NetworkPolicy) error {
	klog.Infof("Adding network policy %s for network %s", getPolicyKey(policy), c.controllerName)
	if c.enableMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolEvent("add", duration)
		}()
	}

	// To not hold nsLock for the whole process on network policy creation, we do the following:
	// 1. save required namespace information to use for netpol create
	// 2. create network policy without ns Lock
	// 3. subscribe to namespace events with ns Lock

	// 1. save required namespace information to use for netpol create,
	npKey := getPolicyKey(policy)
	aclLogging := c.externalInterface.GetNamespaceACLLogging(policy.Namespace)

	// 2. create network policy without ns Lock, cleanup on failure
	var np *networkPolicy
	var err error

	np, err = c.createNetworkPolicy(policy, aclLogging)
	defer func() {
		if err != nil {
			klog.Infof("Create network policy %s failed, try to cleanup", npKey)
			// try to cleanup network policy straight away
			// it will be retried later with add/delete network policy handlers if it fails
			cleanupErr := c.networkPolicies.DoWithLock(npKey, func(npKey string) error {
				np, ok := c.networkPolicies.Load(npKey)
				if !ok {
					klog.Infof("Deleting policy %s that is already deleted", npKey)
					return nil
				}
				return c.cleanupNetworkPolicy(np)
			})
			if cleanupErr != nil {
				klog.Infof("Cleanup for failed create network policy %s returned an error: %v",
					npKey, cleanupErr)
			}
		}
	}()
	if err != nil {
		return fmt.Errorf("failed to create Network Policy %s: %v", npKey, err)
	}
	klog.Infof("Create network policy %s resources completed, update namespace loglevel", npKey)

	// 3. subscribe to namespace updates
	nsSync := func(latestACLLogging *ACLLoggingLevels) error {
		// check if namespace information related to network policy has changed,
		// network policy only reacts to namespace update ACL log level.
		// Run handleNetPolNamespaceUpdate sequence, but only for 1 newly added policy.
		if latestACLLogging.Deny != aclLogging.Deny {
			if err := c.updateACLLoggingForDefaultACLs(np.namespace, latestACLLogging); err != nil {
				return fmt.Errorf("network policy %s failed to be created: update default deny ACLs failed: %v", npKey, err)
			} else {
				klog.Infof("Policy %s: ACL logging setting updated to deny=%s allow=%s",
					npKey, latestACLLogging.Deny, latestACLLogging.Allow)
			}
		}
		if latestACLLogging.Allow != aclLogging.Allow {
			if err := c.updateACLLoggingForPolicy(np, latestACLLogging); err != nil {
				return fmt.Errorf("network policy %s failed to be created: update policy ACLs failed: %v", npKey, err)
			} else {
				klog.Infof("Policy %s: ACL logging setting updated to deny=%s allow=%s",
					npKey, latestACLLogging.Deny, latestACLLogging.Allow)
			}
		}
		return nil
	}

	err = c.externalInterface.SubscribeToNamespaceUpdates(np.namespace, npKey, nsSync)

	return err
}

// buildNetworkPolicyACLs builds the ACLS associated with the 'gress policies
// of the provided network policy.
func (c *NetworkPolicyController) buildNetworkPolicyACLs(np *networkPolicy, aclLogging *ACLLoggingLevels) []*nbdb.ACL {
	acls := []*nbdb.ACL{}
	for _, gp := range np.ingressPolicies {
		acl := gp.buildLocalPodACLs(np.portGroupName, aclLogging)
		acls = append(acls, acl...)
	}
	for _, gp := range np.egressPolicies {
		acl := gp.buildLocalPodACLs(np.portGroupName, aclLogging)
		acls = append(acls, acl...)
	}

	return acls
}

// DeleteNetworkPolicy removes a network policy
// It only uses Namespace and Name from given network policy
func (c *NetworkPolicyController) DeleteNetworkPolicy(policy *knet.NetworkPolicy) error {
	npKey := getPolicyKey(policy)
	klog.Infof("Deleting network policy %s", npKey)
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolEvent("delete", duration)
		}()
	}
	// First lock and update namespace
	c.externalInterface.UnsubscribeFromNamespaceUpdates(policy.Namespace, npKey)
	// Next cleanup network policy
	err := c.networkPolicies.DoWithLock(npKey, func(npKey string) error {
		np, ok := c.networkPolicies.Load(npKey)
		if !ok {
			klog.Infof("Deleting policy %s that is already deleted", npKey)
			return nil
		}
		if err := c.cleanupNetworkPolicy(np); err != nil {
			return fmt.Errorf("deleting policy %s failed: %v", npKey, err)
		}
		return nil
	})
	return err
}

// cleanupNetworkPolicy should be retriable
// It takes and releases networkPolicy lock.
// It updates bnc.networkPolicies on success, should be called with bnc.networkPolicies key locked.
// No need to log network policy key here, because caller of cleanupNetworkPolicy should prepend error message with
// that information.
func (c *NetworkPolicyController) cleanupNetworkPolicy(np *networkPolicy) error {
	npKey := np.getKey()
	klog.Infof("Cleaning up network policy %s", npKey)
	np.Lock()
	defer np.Unlock()

	// signal to local pod/peer handlers to ignore new events
	np.deleted = true

	// stop handlers, retriable
	c.shutdownHandlers(np)
	var err error

	// delete from peer address set
	for i, asKey := range np.peerAddressSets {
		if err := c.externalInterface.DeletePodSelectorAddressSet(asKey, np.getKeyWithKind()); err != nil {
			// remove deleted address sets from the list
			np.peerAddressSets = np.peerAddressSets[i:]
			return fmt.Errorf("failed to delete network policy from peer address set %s: %v", asKey, err)
		}
	}
	np.peerAddressSets = nil

	// Delete the port group, idempotent
	ops, err := libovsdbops.DeletePortGroupsOps(c.nbClient, nil, np.portGroupName)
	if err != nil {
		return fmt.Errorf("failed to get delete network policy port group %s ops: %v", np.portGroupName, err)
	}
	recordOps, txOkCallBack, _, err := c.externalInterface.AddConfigDurationRecord("networkpolicy", np.namespace, np.name)
	if err != nil {
		klog.Errorf("Failed to record config duration: %v", err)
	}
	ops = append(ops, recordOps...)

	err = c.denyPGDeletePorts(np, nil, true, ops)
	if err != nil {
		return fmt.Errorf("unable to delete ports from defaultDeny port group: %v", err)
	}
	// transaction was successful, exec callback
	txOkCallBack()
	// cleanup local pods, since they were deleted from port groups
	np.localPods = sync.Map{}

	err = c.delPolicyFromDefaultPortGroups(np)
	if err != nil {
		return fmt.Errorf("unable to delete policy from default deny port groups: %v", err)
	}

	// finally, delete netpol from existing networkPolicies
	// this is the signal that cleanup was successful
	c.networkPolicies.Delete(npKey)
	return nil
}

type NetworkPolicyExtraParameters struct {
	np *networkPolicy
	gp *gressPolicy
}

func (c *NetworkPolicyController) handlePeerNamespaceSelectorAdd(np *networkPolicy, gp *gressPolicy, objs ...interface{}) error {
	if c.enableMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolPeerNamespaceEvent("add", duration)
		}()
	}
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}
	updated := false
	var errors []error
	for _, obj := range objs {
		namespace := obj.(*kapi.Namespace)
		// addNamespaceAddressSet is safe for concurrent use, doesn't require additional synchronization
		nsUpdated, err := gp.addNamespaceAddressSet(namespace.Name, c.addressSetFactory)
		if err != nil {
			errors = append(errors, err)
		} else if nsUpdated {
			updated = true
		}
	}
	if updated {
		err := c.peerNamespaceUpdate(np, gp)
		if err != nil {
			errors = append(errors, err)
		}
	}
	return kerrorsutil.NewAggregate(errors)

}

func (c *NetworkPolicyController) handlePeerNamespaceSelectorDel(np *networkPolicy, gp *gressPolicy, objs ...interface{}) error {
	if c.enableMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolPeerNamespaceEvent("delete", duration)
		}()
	}
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}
	updated := false
	for _, obj := range objs {
		namespace := obj.(*kapi.Namespace)
		// delNamespaceAddressSet is safe for concurrent use, doesn't require additional synchronization
		if gp.delNamespaceAddressSet(namespace.Name) {
			updated = true
		}
	}
	// unlock networkPolicy, before calling peerNamespaceUpdate
	if updated {
		return c.peerNamespaceUpdate(np, gp)
	}
	return nil
}

// must be called with np.Lock
func (c *NetworkPolicyController) peerNamespaceUpdate(np *networkPolicy, gp *gressPolicy) error {
	// buildLocalPodACLs is safe for concurrent use, see function comment for details
	// we don't care about logLevels, since this function only updated Match
	acls := gp.buildLocalPodACLs(np.portGroupName, &ACLLoggingLevels{})
	ops, err := libovsdbops.UpdateACLsMatchOps(c.nbClient, nil, acls...)
	if err != nil {
		return err
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	return err
}

// addPeerNamespaceHandler starts a watcher for PeerNamespaceSelectorType.
// Sync function and Add event for every existing namespace will be executed sequentially first, and an error will be
// returned if something fails.
// PeerNamespaceSelectorType uses handlePeerNamespaceSelectorAdd on Add,
// and handlePeerNamespaceSelectorDel on Delete.
func (c *NetworkPolicyController) addPeerNamespaceHandler(
	namespaceSelector *metav1.LabelSelector,
	gress *gressPolicy, np *networkPolicy) error {

	// NetworkPolicy is validated by the apiserver; this can't fail.
	sel, _ := metav1.LabelSelectorAsSelector(namespaceSelector)

	// start watching namespaces selected by the namespace selector
	syncFunc := func(objs []interface{}) error {
		// ignore returned error, since any namespace that wasn't properly handled will be retried individually.
		_ = c.handlePeerNamespaceSelectorAdd(np, gress, objs...)
		return nil
	}
	retryPeerNamespaces := c.externalInterface.newNetpolRetryFramework(
		factory.PeerNamespaceSelectorType,
		syncFunc,
		&NetworkPolicyExtraParameters{gp: gress, np: np},
	)

	namespaceHandler, err := retryPeerNamespaces.WatchResourceFiltered("", sel)
	if err != nil {
		klog.Errorf("WatchResource failed for addPeerNamespaceHandler: %v", err)
		return err
	}

	np.nsHandlerList = append(np.nsHandlerList, namespaceHandler)
	return nil
}

func (c *NetworkPolicyController) shutdownHandlers(np *networkPolicy) {
	if np.localPodHandler != nil {
		c.watchFactory.RemovePodHandler(np.localPodHandler)
		np.localPodHandler = nil
	}
	for _, handler := range np.nsHandlerList {
		c.watchFactory.RemoveNamespaceHandler(handler)
	}
	np.nsHandlerList = make([]*factory.Handler, 0)
}

// The following 2 functions should return the same key for network policy based on k8s on internal networkPolicy object
func getPolicyKey(policy *knet.NetworkPolicy) string {
	return fmt.Sprintf("%v/%v", policy.Namespace, policy.Name)
}

func (np *networkPolicy) getKey() string {
	return fmt.Sprintf("%v/%v", np.namespace, np.name)
}

func (np *networkPolicy) getKeyWithKind() string {
	return fmt.Sprintf("%v/%v/%v", "NetworkPolicy", np.namespace, np.name)
}

// PortGroupHasPorts returns true if a port group contains all given ports
func PortGroupHasPorts(nbClient libovsdbclient.Client, pgName string, portUUIDs []string) bool {
	pg := &nbdb.PortGroup{
		Name: pgName,
	}
	pg, err := libovsdbops.GetPortGroup(nbClient, pg)
	if err != nil {
		return false
	}

	return sets.NewString(pg.Ports...).HasAll(portUUIDs...)
}

// getStaleNetpolAddrSetDbIDs returns the ids for address sets that were owned by network policy before we
// switched to shared address sets with PodSelectorAddressSet. Should only be used for sync and testing.
func getStaleNetpolAddrSetDbIDs(policyNamespace, policyName, policyType, idx, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNetworkPolicy, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: policyNamespace + "_" + policyName,
		// direction and idx uniquely identify address set (= gress policy rule)
		libovsdbops.PolicyDirectionKey: strings.ToLower(policyType),
		libovsdbops.GressIdxKey:        idx,
	})
}
