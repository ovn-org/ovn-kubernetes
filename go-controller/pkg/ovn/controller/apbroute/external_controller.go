package apbroute

import (
	"fmt"
	"strings"
	"sync"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedroutelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

type gatewayInfoList []*gatewayInfo

func (g gatewayInfoList) String() string {
	ret := []string{}
	for _, i := range g {
		ret = append(ret, i.String())
	}
	return strings.Join(ret, ", ")
}

// HasBFDEnabled returns the BFD value for the given IP stored in the gatewayInfoList when found.
// It also returns a boolean that indicates if the IP was found and an error
// An IP can only have one BFD value.
func (g gatewayInfoList) HasBFDEnabled(ip string) (bool, bool) {
	for _, i := range g {
		if i.Gateways.Has(ip) {
			return i.BFDEnabled, true
		}
	}
	return false, false
}

// HasIP returns true if the given IP is found in one of the gatewayInfo elements stored in the gatewayInfoList
func (g gatewayInfoList) HasIP(ip string) bool {
	for _, i := range g {
		if i.Gateways.Has(ip) {
			return true
		}
	}
	return false
}

// Insert adds a slice of gatewayInfo's to the gatewayInfolist and ensures that no IP duplicates are inserted. It returns a new
// gatewayInfoList containing the added gatewayInfo elements and set containing the duplicated IPs found during the Insert operation.
func (g gatewayInfoList) Insert(items ...*gatewayInfo) (gatewayInfoList, sets.Set[string], error) {
	ret := append(gatewayInfoList{}, g...)
	duplicated := sets.New[string]()
	for _, item := range items {
		gws := sets.Set[string]{}
		for _, ip := range item.Gateways.UnsortedList() {
			bfd, found := ret.HasBFDEnabled(ip)
			if found {
				if bfd != item.BFDEnabled {
					return nil, nil, fmt.Errorf("attempting to insert duplicated IP %s with different BFD states: enabled/disabled", ip)
				}
				duplicated = duplicated.Insert(ip)
				continue
			}
			gws.Insert(ip)
		}
		if len(gws) > 0 {
			ret = append(ret, newGatewayInfo(gws, item.BFDEnabled))
		}
	}
	return ret, duplicated, nil
}
func (g gatewayInfoList) Delete(item *gatewayInfo) gatewayInfoList {
	ret := gatewayInfoList{}
	for _, i := range g {
		if !i.Equal(item) {
			ret, _, _ = ret.Insert(i)
		}
	}
	return ret
}

func (g gatewayInfoList) Len() int {
	return len(g)
}

type gatewayInfo struct {
	Gateways   *syncSet
	BFDEnabled bool
}

func (g *gatewayInfo) String() string {
	return fmt.Sprintf("BFDEnabled: %t, Gateways: %+v", g.BFDEnabled, g.Gateways.items)
}

func newGatewayInfo(items sets.Set[string], bfdEnabled bool) *gatewayInfo {
	return &gatewayInfo{Gateways: &syncSet{items: items, mux: &sync.Mutex{}}, BFDEnabled: bfdEnabled}
}

type syncSet struct {
	items sets.Set[string]
	mux   *sync.Mutex
}

// Equal compares two gatewayInfo instances and returns true if all the gateway IPs are equal, regardless of the order, as well as the BFDEnabled field value.
func (g *gatewayInfo) Equal(g2 *gatewayInfo) bool {
	return g.BFDEnabled == g2.BFDEnabled && len(g.Gateways.Difference(g2.Gateways.items.Clone())) == 0
}

func (g *syncSet) Has(ip string) bool {
	g.mux.Lock()
	defer g.mux.Unlock()
	return g.items.Has(ip)

}
func (g *syncSet) UnsortedList() []string {
	g.mux.Lock()
	defer g.mux.Unlock()
	return g.items.Clone().UnsortedList()
}

func (g *syncSet) Delete(items ...string) {
	g.mux.Lock()
	defer g.mux.Unlock()
	g.items = g.items.Delete(items...)
}

func (g *syncSet) Insert(items ...string) {
	g.mux.Lock()
	defer g.mux.Unlock()
	g.items = g.items.Insert(items...)
}

func (g *syncSet) Difference(items sets.Set[string]) sets.Set[string] {
	g.mux.Lock()
	defer g.mux.Unlock()
	return g.items.Difference(items)
}

func (g syncSet) Len() int {
	g.mux.Lock()
	defer g.mux.Unlock()
	return g.items.Len()
}

type namespaceInfo struct {
	Policies        sets.Set[string]
	StaticGateways  gatewayInfoList
	DynamicGateways map[ktypes.NamespacedName]*gatewayInfo
}

func newNamespaceInfo() *namespaceInfo {
	return &namespaceInfo{
		Policies:        sets.New[string](),
		DynamicGateways: make(map[ktypes.NamespacedName]*gatewayInfo),
		StaticGateways:  gatewayInfoList{},
	}
}

type routeInfo struct {
	policy            *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute
	markedForDeletion bool
}

type ExternalRouteInfo struct {
	sync.Mutex
	Deleted bool
	PodName ktypes.NamespacedName
	// PodExternalRoutes is a cache keeping the LR routes added to the GRs when
	// external gateways are used. The first map key is the podIP (src-ip of the route),
	// the second the GW IP (next hop), and the third the GR name
	PodExternalRoutes map[string]map[string]string
}

// This structure contains the processed information of a policy.
// This information is then used to update the network components (North Bound DB, conntrack) by applying the IPs here to each of the target namespaces defined in the from field.
type routePolicy struct {
	// targetNamespacesSelector contains the namespace selector defined in the from field in the policy.
	targetNamespacesSelector *metav1.LabelSelector
	// staticGateways contains the processed list of IPs and BFD information defined in the staticHop slice in the policy.
	staticGateways gatewayInfoList
	// dynamicGateways contains the processed list of IPs and BFD information defined in the dynamicHop slice in the policy.
	// the IP and BFD information of each pod gateway is stored in a map where the key is of type NamespacedName with the namespace and podName as values
	// and the value is the gatewayInfo, which contains a set of IPs and the flag to determine if the BFD protocol is to be enabled for this IP
	dynamicGateways map[ktypes.NamespacedName]*gatewayInfo
}

type externalPolicyManager struct {
	stopCh <-chan struct{}
	// route policies
	routeLister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister
	// Pods
	podLister corev1listers.PodLister
	// Namespaces
	namespaceLister corev1listers.NamespaceLister
	// cache for set of policies impacting a given namespace
	namespaceInfoSyncCache *syncmap.SyncMap[*namespaceInfo]
	routePolicySyncCache   *syncmap.SyncMap[*routeInfo]
	// networkClient is an interface that exposes add and delete GW IPs. There are 2 structs that implement this contract: one to interface with the north bound DB and another one for the conntrack.
	// the north bound is used by the master controller to add and delete the logical static routes, whilst the conntrack is used by the node controller to ensure that the ECMP entries are removed
	// when a gateway IP is no longer an egress access point.
	netClient networkClient
}

func newExternalPolicyManager(
	stopCh <-chan struct{},
	podLister corev1listers.PodLister,
	namespaceLister corev1listers.NamespaceLister,
	routeLister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister,
	netClient networkClient) *externalPolicyManager {

	m := externalPolicyManager{
		stopCh:                 stopCh,
		routeLister:            routeLister,
		podLister:              podLister,
		namespaceLister:        namespaceLister,
		namespaceInfoSyncCache: syncmap.NewSyncMap[*namespaceInfo](),
		routePolicySyncCache:   syncmap.NewSyncMap[*routeInfo](),
		netClient:              netClient,
	}

	return &m
}

// getRoutePolicyFromCache retrieves the cached value of the policy if it exists in the cache, as well as locking the key in case it exists.
func (m *externalPolicyManager) getRoutePolicyFromCache(policyName string) (*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, bool, bool) {
	var (
		policy                   *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute
		found, markedForDeletion bool
	)
	_ = m.routePolicySyncCache.DoWithLock(policyName, func(policyName string) error {
		ri, f := m.routePolicySyncCache.Load(policyName)
		if !f {
			return nil
		}
		found = f
		markedForDeletion = ri.markedForDeletion
		policy = ri.policy.DeepCopy()
		return nil
	})
	return policy, found, markedForDeletion
}

func (m *externalPolicyManager) storeRoutePolicyInCache(policyInfo *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) error {
	return m.routePolicySyncCache.DoWithLock(policyInfo.Name, func(policyName string) error {
		ri, found := m.routePolicySyncCache.Load(policyName)
		if !found {
			m.routePolicySyncCache.LoadOrStore(policyName, &routeInfo{policy: policyInfo})
			return nil
		}
		if ri.markedForDeletion {
			return fmt.Errorf("attempting to store policy %s that is in the process of being deleted", policyInfo.Name)
		}
		ri.policy = policyInfo
		return nil
	})
}

func (m *externalPolicyManager) deleteRoutePolicyFromCache(policyName string) error {
	return m.routePolicySyncCache.DoWithLock(policyName, func(policyName string) error {
		ri, found := m.routePolicySyncCache.Load(policyName)
		if found && !ri.markedForDeletion {
			return fmt.Errorf("attempting to delete route policy %s from cache before it has been marked for deletion", policyName)
		}
		m.routePolicySyncCache.Delete(policyName)
		return nil
	})
}

// getAndMarkRoutePolicyForDeletionInCache flags a route policy for deletion and returns its cached value. This mark is used as a flag for other routines that attempt to retrieve the policy
// while processing pods or namespaces related to the given policy.
func (m *externalPolicyManager) getAndMarkRoutePolicyForDeletionInCache(policyName string) (adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, bool) {
	var (
		exists      bool
		routePolicy adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute
	)
	_ = m.routePolicySyncCache.DoWithLock(policyName, func(policyName string) error {
		ri, found := m.routePolicySyncCache.Load(policyName)
		if !found {
			return nil
		}
		ri.markedForDeletion = true
		exists = true
		routePolicy = *ri.policy
		return nil
	})
	return routePolicy, exists
}

func (m *externalPolicyManager) getNamespaceInfoFromCache(namespaceName string) (*namespaceInfo, bool) {
	m.namespaceInfoSyncCache.LockKey(namespaceName)
	nsInfo, ok := m.namespaceInfoSyncCache.Load(namespaceName)
	if !ok {
		m.namespaceInfoSyncCache.UnlockKey(namespaceName)
		return nil, false
	}
	return nsInfo, true
}

func (m *externalPolicyManager) getAllNamespacesNamesInCache() []string {
	return m.namespaceInfoSyncCache.GetKeys()
}

func (m *externalPolicyManager) unlockNamespaceInfoCache(namespaceName string) {
	m.namespaceInfoSyncCache.UnlockKey(namespaceName)
}

func (m *externalPolicyManager) newNamespaceInfoInCache(namespaceName string) *namespaceInfo {
	m.namespaceInfoSyncCache.LockKey(namespaceName)
	nsInfo, _ := m.namespaceInfoSyncCache.LoadOrStore(namespaceName, newNamespaceInfo())
	return nsInfo
}

func (m *externalPolicyManager) listNamespaceInfoCache() []string {
	return m.namespaceInfoSyncCache.GetKeys()
}

func (m *externalPolicyManager) getAllRoutePolicies() ([]*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, error) {
	var (
		routePolicies []*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute
		err           error
	)

	routePolicies, err = m.routeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list Admin Policy Based External Routes:%v", err)
		return nil, err
	}
	return routePolicies, nil
}

// getDynamicGatewayIPsForTargetNamespace is called by the annotation logic to identify if a namespace is managed by an CR.
// Since the call can occur outside the lifecycle of the controller, it cannot rely on the namespace info cache object to have been populated.
// Therefore it has to go through all policies until it identifies one that targets the namespace and retrieve the gateway IPs.
// these IPs are used by the annotation logic to determine which ones to remove from the north bound DB (the ones not included in the list),
// and the ones to keep (the ones that match both the annotation and the CR).
// This logic ensures that both CR and annotations can coexist without duplicating gateway IPs.
func (m *externalPolicyManager) getDynamicGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	policyGWIPs := sets.New[string]()

	routePolicies, err := m.getAllRoutePolicies()
	if err != nil {
		return nil, err
	}
	for _, routePolicy := range routePolicies {
		p, err := m.processExternalRoutePolicy(routePolicy)
		if err != nil {
			klog.Errorf("Failed to process Admin Policy Based External Route %s: %v", routePolicy.Name, err)
			return nil, err
		}
		targetNs, err := m.listNamespacesBySelector(p.targetNamespacesSelector)
		if err != nil {
			klog.Errorf("Failed to process namespace selector for Admin Policy Based External Route %s:%v", routePolicy.Name, err)
			return nil, err
		}
		for _, ns := range targetNs {
			if ns.Name == namespaceName {
				// only collect the dynamic gateways
				for _, gwInfo := range p.dynamicGateways {
					policyGWIPs = policyGWIPs.Insert(gwInfo.Gateways.UnsortedList()...)
				}
			}
		}
	}
	return policyGWIPs, nil
}

// getStaticGatewayIPsForTargetNamespace is called by the annotation logic to identify if a namespace is managed by an CR.
// Since the call can occur outside the lifecycle of the controller, it cannot rely on the namespace info cache object to have been populated.
// Therefore it has to go through all policies until it identifies one that targets the namespace and retrieve the gateway IPs.
// these IPs are used by the annotation logic to determine which ones to remove from the north bound DB (the ones not included in the list),
// and the ones to keep (the ones that match both the annotation and the CR).
// This logic ensures that both CR and annotations can coexist without duplicating gateway IPs.
func (m *externalPolicyManager) getStaticGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	policyGWIPs := sets.New[string]()

	routePolicies, err := m.routeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list Admin Policy Based External Routes:%v", err)
		return nil, err
	}
	for _, routePolicy := range routePolicies {
		p, err := m.processExternalRoutePolicy(routePolicy)
		if err != nil {
			klog.Errorf("Failed to process Admin Policy Based External Route %s: %v", routePolicy.Name, err)
			return nil, err
		}
		targetNs, err := m.listNamespacesBySelector(p.targetNamespacesSelector)
		if err != nil {
			klog.Errorf("Failed to process namespace selector for Admin Policy Based External Route %s:%v", routePolicy.Name, err)
			return nil, err
		}
		for _, ns := range targetNs {
			if ns.Name == namespaceName {
				// only collect the static gateways
				for _, gwInfo := range p.staticGateways {
					policyGWIPs.Insert(gwInfo.Gateways.UnsortedList()...)
				}
			}
		}
	}
	return policyGWIPs, nil
}
