package apbroute

import (
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/klog/v2"

	adminpolicybasedrouteinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/informers/externalversions/adminpolicybasedroute/v1"
)

// Admin Policy Based Route Node controller

type ExternalGatewayNodeController struct {
	stopCh <-chan struct{}

	mgr *externalPolicyManager
}

func NewExternalNodeController(
	podInformer coreinformers.PodInformer,
	namespaceInformer coreinformers.NamespaceInformer,
	apbRouteInformer adminpolicybasedrouteinformer.AdminPolicyBasedExternalRouteInformer,
	stopCh <-chan struct{},
) (*ExternalGatewayNodeController, error) {

	c := &ExternalGatewayNodeController{
		stopCh: stopCh,
		mgr: newExternalPolicyManager(
			stopCh,
			podInformer,
			namespaceInformer,
			apbRouteInformer,
			&conntrackClient{podLister: podInformer.Lister()},
			nil),
	}

	return c, nil
}

func (c *ExternalGatewayNodeController) Run(wg *sync.WaitGroup, threadiness int) error {
	klog.V(4).Info("Starting Admin Policy Based Route Node Controller")

	return c.mgr.Run(wg, threadiness)
}

func (c *ExternalGatewayNodeController) GetAdminPolicyBasedExternalRouteIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
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
