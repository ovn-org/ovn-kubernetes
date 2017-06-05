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

	return &ovn.Controller{
		StartPodWatch: func(handler cache.ResourceEventHandler) {
			podInformer.Informer().AddEventHandler(handler)
			go podInformer.Informer().Run(utilwait.NeverStop)
		},
		StartServiceWatch: func(handler cache.ResourceEventHandler) {
			serviceInformer.Informer().AddEventHandler(handler)
			go serviceInformer.Informer().Run(utilwait.NeverStop)
		},
		StartEndpointWatch: func(handler cache.ResourceEventHandler) {
			endpointsInformer.Informer().AddEventHandler(handler)
			go endpointsInformer.Informer().Run(utilwait.NeverStop)
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
