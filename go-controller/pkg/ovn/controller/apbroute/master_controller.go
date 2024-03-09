package apbroute

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/applyconfiguration/adminpolicybasedroute/v1"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	adminpolicybasedrouteinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/informers/externalversions/adminpolicybasedroute/v1"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// Admin Policy Based Route services

type ExternalGatewayMasterController struct {
	apbRoutePolicyClient adminpolicybasedrouteclient.Interface

	mgr                      *externalPolicyManager
	nbClient                 *northBoundClient
	ExternalGWRouteInfoCache *ExternalGatewayRouteInfoCache

	zoneID string
}

func NewExternalMasterController(
	apbRoutePolicyClient adminpolicybasedrouteclient.Interface,
	stopCh <-chan struct{},
	podInformer coreinformers.PodInformer,
	namespaceInformer coreinformers.NamespaceInformer,
	apbRouteInformer adminpolicybasedrouteinformer.AdminPolicyBasedExternalRouteInformer,
	nodeLister corev1listers.NodeLister,
	nbClient libovsdbclient.Client,
	addressSetFactory addressset.AddressSetFactory,
	controllerName string,
	zoneID string,
) (*ExternalGatewayMasterController, error) {

	externalGWRouteInfo := NewExternalGatewayRouteInfoCache()
	zone, err := libovsdbutil.GetNBZone(nbClient)
	if err != nil {
		return nil, err
	}
	nbCli := &northBoundClient{
		routeLister:              apbRouteInformer.Lister(),
		nodeLister:               nodeLister,
		podLister:                podInformer.Lister(),
		nbClient:                 nbClient,
		addressSetFactory:        addressSetFactory,
		controllerName:           controllerName,
		zone:                     zone,
		externalGatewayRouteInfo: externalGWRouteInfo,
	}

	c := &ExternalGatewayMasterController{
		apbRoutePolicyClient:     apbRoutePolicyClient,
		ExternalGWRouteInfoCache: externalGWRouteInfo,
		nbClient:                 nbCli,
		zoneID:                   zoneID,
	}
	c.mgr = newExternalPolicyManager(
		stopCh,
		podInformer,
		namespaceInformer,
		apbRouteInformer,
		nbCli,
		c.updateStatusAPBExternalRoute,
	)
	return c, nil
}

func (c *ExternalGatewayMasterController) Run(wg *sync.WaitGroup, threadiness int) error {
	klog.V(4).Info("Starting Admin Policy Based Route Controller")

	return c.mgr.Run(wg, threadiness)
}

func (c *ExternalGatewayMasterController) GetAdminPolicyBasedExternalRouteIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	gwIPs, err := c.mgr.getDynamicGatewayIPsForTargetNamespace(namespaceName)
	if err != nil {
		return nil, err
	}
	tmpIPs, err := c.mgr.getStaticGatewayIPsForTargetNamespace(namespaceName)
	if err != nil {
		return nil, err
	}

	return gwIPs.Union(tmpIPs), nil
}

// updateStatusAPBExternalRoute updates the CR with the current status of the CR instance, including errors captured while processing the CR during its lifetime
func (c *ExternalGatewayMasterController) updateStatusAPBExternalRoute(policyName string, gwIPs sets.Set[string],
	syncError error) error {
	if gwIPs == nil {
		// policy doesn't exist anymore, nothing to do
		return nil
	}

	// get object from the informer cache. No need to get the object from the kube-apiserver, since patch
	// doesn't check resource versions, and no one else can update status message owned by current zone.
	routePolicy, err := c.mgr.routeLister.Get(policyName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// policy doesn't exist, no need to update status
			return nil
		}
		return err
	}
	newMsg := fmt.Sprintf("configured external gateway IPs: %s", strings.Join(sets.List(gwIPs), ","))
	if syncError != nil {
		newMsg = fmt.Sprintf("%s %s: %v", c.zoneID, types.APBRouteErrorMsg, syncError.Error())
	}
	newMsg = types.GetZoneStatus(c.zoneID, newMsg)
	needsUpdate := true
	for _, message := range routePolicy.Status.Messages {
		if message == newMsg {
			// found previous status
			needsUpdate = false
			break
		}
	}
	if !needsUpdate {
		return nil
	}

	applyOptions := metav1.ApplyOptions{
		Force:        true,
		FieldManager: c.zoneID,
	}
	applyObj := adminpolicybasedrouteapply.AdminPolicyBasedExternalRoute(policyName).
		WithStatus(adminpolicybasedrouteapply.AdminPolicyBasedRouteStatus().
			WithMessages(newMsg).
			WithLastTransitionTime(metav1.Now()))
	_, err = c.apbRoutePolicyClient.K8sV1().AdminPolicyBasedExternalRoutes().ApplyStatus(context.TODO(), applyObj, applyOptions)

	if err != nil {
		return err
	}
	return nil
}

func (c *ExternalGatewayMasterController) GetDynamicGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	return c.mgr.getDynamicGatewayIPsForTargetNamespace(namespaceName)
}

func (c *ExternalGatewayMasterController) GetStaticGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	return c.mgr.getStaticGatewayIPsForTargetNamespace(namespaceName)
}

// AddHybridRoutePolicyForPod exposes the function addHybridRoutePolicyForPod
func (c *ExternalGatewayMasterController) AddHybridRoutePolicyForPod(podIP, node string) error {
	return c.nbClient.addHybridRoutePolicyForPod(podIP, node)
}

// DelHybridRoutePolicyForPod exposes the function delHybridRoutePolicyForPod
func (c *ExternalGatewayMasterController) DelHybridRoutePolicyForPod(podIP, node string) error {
	return c.nbClient.delHybridRoutePolicyForPod(podIP, node)
}

// DelAllHybridRoutePolicies exposes the function delAllHybridRoutePolicies
func (c *ExternalGatewayMasterController) DelAllHybridRoutePolicies() error {
	return c.nbClient.delAllHybridRoutePolicies()
}

// DelAllLegacyHybridRoutePolicies exposes the function delAllLegacyHybridRoutePolicies
func (c *ExternalGatewayMasterController) DelAllLegacyHybridRoutePolicies() error {
	return c.nbClient.delAllLegacyHybridRoutePolicies()
}

// DeletePodSNAT exposes the function deletePodSNAT
func (c *ExternalGatewayMasterController) DeletePodSNAT(nodeName string, extIPs, podIPNets []*net.IPNet) error {
	return c.nbClient.deletePodSNAT(nodeName, extIPs, podIPNets)
}

func (c *ExternalGatewayMasterController) GetAPBRoutePolicyStatus(policyName string) (*adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus, error) {
	pol, err := c.mgr.routeLister.Get(policyName)
	if err != nil {
		return nil, err
	}
	return &pol.Status, nil
}
