package factory

import (
	"time"

	utilwait "k8s.io/apimachinery/pkg/util/wait"
	informerfactory "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/cluster"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"
)

// Factory initializes and manages the kube watches that drive an ovn controller
type Factory struct {
	KClient        kubernetes.Interface
	IFactory       informerfactory.SharedInformerFactory
	ResyncInterval time.Duration
}

// NewDefaultFactory initializes a default ovn controller factory.
func NewDefaultFactory(c kubernetes.Interface) *Factory {
	resyncInterval := 10 * time.Minute
	return &Factory{
		KClient:        c,
		ResyncInterval: resyncInterval,
		IFactory:       informerfactory.NewSharedInformerFactory(c, resyncInterval),
	}
}

// CreateOvnController begins listing and watching against the API server for the pod and endpoint
// resources. It spawns child goroutines that cannot be terminated.
func (factory *Factory) CreateOvnController() *ovn.Controller {

	podInformer := factory.IFactory.Core().V1().Pods()
	serviceInformer := factory.IFactory.Core().V1().Services()
	endpointsInformer := factory.IFactory.Core().V1().Endpoints()
	policyInformer := factory.IFactory.Networking().V1().NetworkPolicies()
	namespaceInformer := factory.IFactory.Core().V1().Namespaces()

	return &ovn.Controller{
		StartPodWatch: func(handler cache.ResourceEventHandler, processExisting func([]interface{})) {
			go podInformer.Informer().Run(utilwait.NeverStop)
			cache.WaitForCacheSync(utilwait.NeverStop, podInformer.Informer().HasSynced)
			if processExisting != nil {
				// cache has synced, lets process the list
				processExisting(podInformer.Informer().GetStore().List())
			}
			// now register the event handler
			podInformer.Informer().AddEventHandler(handler)
		},
		StartServiceWatch: func(handler cache.ResourceEventHandler, processExisting func([]interface{})) {
			go serviceInformer.Informer().Run(utilwait.NeverStop)
			cache.WaitForCacheSync(utilwait.NeverStop, serviceInformer.Informer().HasSynced)
			if processExisting != nil {
				processExisting(serviceInformer.Informer().GetStore().List())
			}
			serviceInformer.Informer().AddEventHandler(handler)
		},
		StartEndpointWatch: func(handler cache.ResourceEventHandler, processExisting func([]interface{})) {
			go endpointsInformer.Informer().Run(utilwait.NeverStop)
			cache.WaitForCacheSync(utilwait.NeverStop, endpointsInformer.Informer().HasSynced)
			if processExisting != nil {
				processExisting(endpointsInformer.Informer().GetStore().List())
			}
			endpointsInformer.Informer().AddEventHandler(handler)
		},
		StartPolicyWatch: func(handler cache.ResourceEventHandler, processExisting func([]interface{})) {
			go policyInformer.Informer().Run(utilwait.NeverStop)
			cache.WaitForCacheSync(utilwait.NeverStop, policyInformer.Informer().HasSynced)
			if processExisting != nil {
				processExisting(policyInformer.Informer().GetStore().List())
			}
			policyInformer.Informer().AddEventHandler(handler)
		},
		StartNamespaceWatch: func(handler cache.ResourceEventHandler, processExisting func([]interface{})) {
			go namespaceInformer.Informer().Run(utilwait.NeverStop)
			cache.WaitForCacheSync(utilwait.NeverStop, namespaceInformer.Informer().HasSynced)
			if processExisting != nil {
				processExisting(namespaceInformer.Informer().GetStore().List())
			}
			namespaceInformer.Informer().AddEventHandler(handler)
		},
		Kube: &kube.Kube{KClient: factory.KClient},
	}
}

// CreateClusterController creates the controller for cluster management that watches nodes and gives
// out the subnets for the logical switch meant for the node
func (factory *Factory) CreateClusterController() *cluster.OvnClusterController {
	nodeInformer := factory.IFactory.Core().V1().Nodes()
	return &cluster.OvnClusterController{
		StartNodeWatch: func(handler cache.ResourceEventHandler) {
			nodeInformer.Informer().AddEventHandler(handler)
			nodeInformer.Informer().Run(utilwait.NeverStop)
		},
		//NodeInformer: nodeInformer,
		Kube: &kube.Kube{KClient: factory.KClient},
	}
}
