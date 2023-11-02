package apbroute

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	adminpolicybasedrouteinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/informers/externalversions/adminpolicybasedroute/v1"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// Admin Policy Based Route services

type ExternalGatewayMasterController struct {
	apbRoutePolicyClient adminpolicybasedrouteclient.Interface

	mgr                      *externalPolicyManager
	nbClient                 *northBoundClient
	ExternalGWRouteInfoCache *ExternalGatewayRouteInfoCache
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
	processedError error) error {
	if gwIPs == nil {
		// policy doesn't exist anymore, nothing to do
		return nil
	}

	resultErr := retry.RetryOnConflict(util.OvnConflictBackoff, func() error {
		routePolicy, err := c.apbRoutePolicyClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), policyName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// policy doesn't exist, no need to update status
				return nil
			}
			return err
		}

		updateStatus(routePolicy, strings.Join(sets.List(gwIPs), ","), processedError)

		_, err = c.apbRoutePolicyClient.K8sV1().AdminPolicyBasedExternalRoutes().UpdateStatus(context.TODO(), routePolicy, metav1.UpdateOptions{})
		if !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update AdminPolicyBasedExternalRoutes %s status: %v", policyName, resultErr)
	}
	return nil
}

func (c *ExternalGatewayMasterController) GetDynamicGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	return c.mgr.getDynamicGatewayIPsForTargetNamespace(namespaceName)
}

func (c *ExternalGatewayMasterController) GetStaticGatewayIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	return c.mgr.getStaticGatewayIPsForTargetNamespace(namespaceName)
}

func updateStatus(route *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, gwIPs string, err error) {
	route.Status.LastTransitionTime = metav1.Time{Time: time.Now()}
	if err != nil {
		route.Status.Status = adminpolicybasedrouteapi.FailStatus
		route.Status.Messages = append(route.Status.Messages, fmt.Sprintf("Failed to apply policy: %v", err.Error()))
		return
	}
	route.Status.Status = adminpolicybasedrouteapi.SuccessStatus
	route.Status.Messages = append(route.Status.Messages, fmt.Sprintf("Configured external gateway IPs: %s", gwIPs))
	klog.V(4).Infof("Updating Admin Policy Based External Route %s with Status: %s, Message: %s", route.Name, route.Status.Status, route.Status.Messages[len(route.Status.Messages)-1])
}

// AddHybridRoutePolicyForPod exposes the function addHybridRoutePolicyForPod
func (c *ExternalGatewayMasterController) AddHybridRoutePolicyForPod(podIP net.IP, node string) error {
	return c.nbClient.addHybridRoutePolicyForPod(podIP, node)
}

// DelHybridRoutePolicyForPod exposes the function delHybridRoutePolicyForPod
func (c *ExternalGatewayMasterController) DelHybridRoutePolicyForPod(podIP net.IP, node string) error {
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
