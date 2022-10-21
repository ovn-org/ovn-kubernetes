package multi_homing

import (
	"fmt"
	"golang.org/x/time/rate"
	"time"

	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformersv1 "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	networkattachmentdefinitioninformers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	networkattachmentdefinitionlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
)

const controllerName = "net-attach-def-controller"

type NetAttachDefController struct {
	k8sClient          clientset.Interface
	eventRecorder      record.EventRecorder
	netAttachDefLister networkattachmentdefinitionlisters.NetworkAttachmentDefinitionLister
	netAttachDefSynced cache.InformerSynced

	nodesLister corev1listers.NodeLister
	nodesSynced cache.InformerSynced

	queue      workqueue.RateLimitingInterface
	loopPeriod time.Duration
}

func NewNetAttachDefController(
	k8sClient clientset.Interface,
	eventRecorder record.EventRecorder,
	netAttachDefInformer networkattachmentdefinitioninformers.NetworkAttachmentDefinitionInformer,
	nodeInformer coreinformersv1.NodeInformer,
) *NetAttachDefController {
	const qps = 15

	rateLimitingQueue := workqueue.NewRateLimitingQueue(newRatelimiter(qps))
	netAttachDefInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				addNetworkAttachDefinition(obj, rateLimitingQueue)
			},
			DeleteFunc: func(obj interface{}) {
				deleteNetworkAttachDefinition(obj, rateLimitingQueue)
			},
		})

	nodeInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				addNode(obj, rateLimitingQueue)
			},
			DeleteFunc: func(obj interface{}) {
				deleteNode(obj, rateLimitingQueue)
			},
		})

	return &NetAttachDefController{
		k8sClient:          k8sClient,
		eventRecorder:      eventRecorder,
		netAttachDefLister: netAttachDefInformer.Lister(),
		netAttachDefSynced: netAttachDefInformer.Informer().HasSynced,
		nodesLister:        nodeInformer.Lister(),
		nodesSynced:        nodeInformer.Informer().HasSynced,
		queue:              rateLimitingQueue,
		loopPeriod:         time.Second,
	}
}

func (nadc *NetAttachDefController) Run(numberOfWorkers int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer nadc.queue.ShutDown()

	klog.Infof("Starting controller %s", controllerName)
	if !cache.WaitForNamedCacheSync(controllerName, stopCh, nadc.netAttachDefSynced, nadc.nodesSynced) {
		return fmt.Errorf("error syncing cache")
	}

	klog.Info("Starting workers")
	for i := 0; i < numberOfWorkers; i++ {
		go wait.Until(nadc.worker, nadc.loopPeriod, stopCh)
	}

	<-stopCh

	return nil
}

func (nadc *NetAttachDefController) worker() {
	for nadc.processNextWorkItem() {
	}
}

func (nadc *NetAttachDefController) processNextWorkItem() bool {
	eKey, quit := nadc.queue.Get()
	if quit {
		return false
	}
	defer nadc.queue.Done(eKey)

	err := nadc.sync(eKey.(string))
	nadc.handleErr(err, eKey.(string))

	return true
}

func (nadc *NetAttachDefController) sync(key string) error {
	klog.Infof("Sync event for net-attach-def %s", key)
	return nil
}

func (nadc *NetAttachDefController) handleErr(err error, key string) {}

// literally looted from the services controller ... maybe we can live w/ something simpler.
func newRatelimiter(qps int) workqueue.RateLimiter {
	return workqueue.NewMaxOfRateLimiter(
		workqueue.NewItemExponentialFailureRateLimiter(5*time.Millisecond, 1000*time.Second),
		&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(qps), qps*5)},
	)
}

func addNetworkAttachDefinition(obj interface{}, queue workqueue.RateLimitingInterface) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding network-attachment-definition %s", key)
	netAttachDef := obj.(*nettypes.NetworkAttachmentDefinition)
	metrics.GetConfigDurationRecorder().Start("net-attach-def", netAttachDef.Namespace, netAttachDef.Name)
	queue.Add(key)
}

func deleteNetworkAttachDefinition(obj interface{}, queue workqueue.RateLimitingInterface) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Deleting network-attachment-definition %s", key)
	netAttachDef := obj.(*nettypes.NetworkAttachmentDefinition)
	metrics.GetConfigDurationRecorder().Start("service", netAttachDef.Namespace, netAttachDef.Name)
	queue.Add(key)
}

func addNode(obj interface{}, queue workqueue.RateLimitingInterface) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding network-attachment-definition %s", key)
	node := obj.(*corev1.Node)
	metrics.GetConfigDurationRecorder().Start("net-attach-def", node.Namespace, node.Name)
	queue.Add(key)
}

func deleteNode(obj interface{}, queue workqueue.RateLimitingInterface) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Deleting network-attachment-definition %s", key)
	node := obj.(*corev1.Node)
	metrics.GetConfigDurationRecorder().Start("service", node.Namespace, node.Name)
	queue.Add(key)
}
