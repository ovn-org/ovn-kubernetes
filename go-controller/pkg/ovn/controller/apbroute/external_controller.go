package apbroute

import (
	"fmt"
	"strings"
	"sync"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedroutelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

type gatewayInfoList []*gatewayInfo

func (g gatewayInfoList) String() string {

	s := strings.Builder{}
	for _, item := range g {
		s.WriteString(fmt.Sprintf("%s, ", item.gws))
	}
	return s.String()
}

func (g gatewayInfoList) HasIP(ip string) bool {
	for _, i := range g {
		if i.gws.Has(ip) {
			return true
		}
	}
	return false
}

func (g gatewayInfoList) Insert(items ...*gatewayInfo) gatewayInfoList {
	for _, item := range items {
		for _, ip := range item.gws.UnsortedList() {
			if g.HasIP(ip) {
				klog.Warningf("Duplicated static IP %s", ip)
				continue
			}
			g = append(g, item)
		}
	}
	return g
}
func (g gatewayInfoList) Delete(item *gatewayInfo) gatewayInfoList {
	for index, i := range g {
		if i.gws.Equal(item.gws) {
			g = append(g[:index], g[index+1:]...)
		}
	}
	return g
}

func (g gatewayInfoList) Len() int {
	return len(g)
}

func (g gatewayInfoList) Less(i, j int) bool { return lessGatewayIPs(g[i], g[j]) }
func (g gatewayInfoList) Swap(i, j int)      { g[i], g[j] = g[j], g[i] }

func lessGatewayIPs(l, r *gatewayInfo) bool {

	for lip := range l.gws {
		for rip := range r.gws {
			if lip > rip {
				return false
			}
		}
	}
	return true
}

type gatewayInfo struct {
	gws        sets.Set[string]
	bfdEnabled bool
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
	labelSelector   *metav1.LabelSelector
	staticGateways  gatewayInfoList
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
	namespaceInfoCache      map[string]*namespaceInfo
	namespaceInfoCacheMutex *sync.Mutex
	namespaceLockMap        map[string]*sync.Mutex
	namespaceLockMapMutex   *sync.Mutex

	// cache that captures the current route policies, with their processed information
	routePolicyCache      map[string]*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute
	routePolicyCacheMutex *sync.Mutex
	netClient             networkClient
}

func newExternalPolicyManager(
	stopCh <-chan struct{},
	podLister corev1listers.PodLister,
	namespaceLister corev1listers.NamespaceLister,
	routeLister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister,
	netClient networkClient) *externalPolicyManager {

	m := externalPolicyManager{
		stopCh:                  stopCh,
		routeLister:             routeLister,
		podLister:               podLister,
		namespaceLister:         namespaceLister,
		namespaceInfoCache:      make(map[string]*namespaceInfo),
		namespaceInfoCacheMutex: &sync.Mutex{},
		namespaceLockMap:        make(map[string]*sync.Mutex),
		namespaceLockMapMutex:   &sync.Mutex{},
		routePolicyCache:        make(map[string]*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute),
		routePolicyCacheMutex:   &sync.Mutex{},
		netClient:               netClient,
	}

	return &m
}

func (m *externalPolicyManager) getRoutePolicyFromCache(policyName string) (*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, bool) {
	m.routePolicyCacheMutex.Lock()
	defer m.routePolicyCacheMutex.Unlock()
	p, found := m.routePolicyCache[policyName]
	return p, found
}

func (m *externalPolicyManager) updateRoutePolicyInCache(policy *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) {
	m.routePolicyCacheMutex.Lock()
	defer m.routePolicyCacheMutex.Unlock()
	m.routePolicyCache[policy.Name] = policy
}

func (m *externalPolicyManager) deleteRoutePolicyFromCache(policyName string) {
	m.routePolicyCacheMutex.Lock()
	defer m.routePolicyCacheMutex.Unlock()
	delete(m.routePolicyCache, policyName)
}

// GetDynamicGatewayIPsForTargetNamespace is called by the annotation logic to identify if a namespace is managed by an CR.
// Since the call can occur outside the lifecycle of the controller, it cannot rely on the namespace info cache object to have been populated.
// Therefore it has to go through all policies until it identifies one that targets the namespace and retrieve the gateway IPs.
// these IPs are used by the annotation logic to determine which ones to remove from the north bound DB (the ones not included in the list),
// and the ones to keep (the ones that match both the annotation and the CR).
// This logic ensures that both CR and annotations can coexist without duplicating gateway IPs.
func (m *externalPolicyManager) getDynamicGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
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
		targetNs, err := m.listNamespacesBySelector(p.labelSelector)
		if err != nil {
			klog.Errorf("Failed to process namespace selector for Admin Policy Based External Route %s:%v", routePolicy.Name, err)
			return nil, err
		}
		for _, ns := range targetNs {
			if ns.Name == namespaceName {
				// only collect the dynamic gateways
				for _, gwInfo := range p.dynamicGateways {
					policyGWIPs = policyGWIPs.Union(gwInfo.gws)
				}
			}
		}
	}
	return policyGWIPs, nil
}

// GetStaticGatewayIPsForTargetNamespace is called by the annotation logic to identify if a namespace is managed by an CR.
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
		targetNs, err := m.listNamespacesBySelector(p.labelSelector)
		if err != nil {
			klog.Errorf("Failed to process namespace selector for Admin Policy Based External Route %s:%v", routePolicy.Name, err)
			return nil, err
		}
		for _, ns := range targetNs {
			if ns.Name == namespaceName {
				// only collect the dynamic gateways
				for _, gwInfo := range p.staticGateways {
					policyGWIPs.Insert(gwInfo.gws.UnsortedList()...)
				}
			}
		}
	}
	return policyGWIPs, nil
}
