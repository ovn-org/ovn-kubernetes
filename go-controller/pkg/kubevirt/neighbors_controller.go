package kubevirt

import (
	"fmt"
	"net"
	"reflect"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

type NeighborsController struct {
	podController controller.Controller
	watchFactory  *factory.WatchFactory
	nodeName      string
	nadName       string
	v4            bool
	v6            bool
}

func NewNeighborsController(watchFactory *factory.WatchFactory, v4, v6 bool, nodeName, nadName string) *NeighborsController {
	c := &NeighborsController{
		watchFactory: watchFactory,
		nodeName:     nodeName,
		nadName:      nadName,
		v4:           v4,
		v6:           v6,
	}
	c.initControllers()
	return c
}

func (c *NeighborsController) initControllers() {
	podControllerConfig := &controller.Config[corev1.Pod]{
		RateLimiter:    workqueue.NewItemFastSlowRateLimiter(time.Second, 5*time.Second, 5),
		Informer:       c.watchFactory.PodCoreInformer().Informer(),
		Lister:         c.watchFactory.PodCoreInformer().Lister().List,
		ObjNeedsUpdate: c.podNeedsUpdate,
		Reconcile:      c.reconcilePod,
		Threadiness:    1,
	}
	c.podController = controller.NewController[corev1.Pod]("kubevirt-neighbors-pod-controller", podControllerConfig)
}

func (c *NeighborsController) podNeedsUpdate(oldObj, newObj *corev1.Pod) bool {
	if newObj == nil || oldObj == nil {
		return false
	}
	isMigratedSourcePodStale, err := IsMigratedSourcePodStale(c.watchFactory, newObj)
	if err != nil {
		klog.Errorf("Failed checking IsMigratedSourcePodStale: %v", err)
		return false
	}
	if util.PodWantsHostNetwork(newObj) || !IsPodLiveMigratable(newObj) || isMigratedSourcePodStale {
		return false
	}
	return !reflect.DeepEqual(oldObj.Labels, newObj.Labels) ||
		!reflect.DeepEqual(oldObj.Annotations, newObj.Annotations)
}

func (c *NeighborsController) reconcilePod(key string) error {
	klog.Infof("Reconciling pods at kubevirt neighbors controller, key=%s", key)

	// Split the key in namespace and name of the corresponding object.
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("reconcilePod failed to split meta namespace cache key %s for pod: %v", key, err)
		return nil
	}

	pod, err := c.watchFactory.GetPod(namespace, name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to fetch pod %s in namespace %s", name, namespace)
	}

	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, c.nadName)
	if err != nil {
		return fmt.Errorf("failed reading remote pod annotation: %v", err)
	}

	nodeOwningSubnet, err := findNodeOwningSubnet(c.watchFactory, podAnnotation, c.nadName)
	if err != nil {
		return err
	}

	currentNodeOwnsSubnet := nodeOwningSubnet.Name == c.nodeName
	vmRunningAtCurrentNode := c.nodeName == pod.Spec.NodeName

	if vmRunningAtCurrentNode && !currentNodeOwnsSubnet {
		ipsToNotify, err := findRunningPodsIPsFromPodSubnet(c.watchFactory, podAnnotation, c.nadName)
		if err != nil {
			return fmt.Errorf("failed discovering pod IPs within VM's subnet to update neighbors VM: %w", err)
		}
		if c.v4 {
			if err := notifyARPProxyMACForIPs(ipsToNotify, podAnnotation.MAC); err != nil {
				return fmt.Errorf("failed sending GARP to VM after live migration: %w", err)
			}
		}
		if c.v6 {
			if err := notifyUnsolicitedNeighborAdvertisementForIPs(ipsToNotify, podAnnotation.IPs); err != nil {
				return fmt.Errorf("failed sending unsolicited na to VM after live migration: %w", err)
			}
		}
	} else if !vmRunningAtCurrentNode && currentNodeOwnsSubnet {
		if c.v4 {
			if err := notifyARPProxyMACForIPs(podAnnotation.IPs, broadcastMAC); err != nil {
				return err
			}
		}
		if c.v6 {
			if err := notifyUnsolicitedNeighborAdvertisementForIPs(podAnnotation.IPs, []*net.IPNet{{IP: net.IPv6linklocalallnodes}}); err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *NeighborsController) syncPods() error {
	klog.Infof("Syncing pods at kubevirt neighbors controller")
	// TODO
	return nil
}

func (c *NeighborsController) Start() error {
	klog.Infof("Starting kubevirt neighbors controller")
	if err := controller.StartControllersWithInitialSync(c.syncPods, c.podController); err != nil {
		return fmt.Errorf("unable to start pod controller: %w", err)
	}
	return nil
}

func (c *NeighborsController) Stop() {
	klog.Infof("Stopping kubevirt neighbors controller")
	controller.StopControllers(c.podController)
}
