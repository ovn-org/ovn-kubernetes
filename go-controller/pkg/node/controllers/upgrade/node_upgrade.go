package upgrade

import (
	"context"
	"fmt"
	"strconv"
	"time"

	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	controllerName = "node-upgrade-controller"
)

// upgradeController checks the OVN master nodes periodically
// for upgrade annotation
type upgradeController struct {
	client          kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	// nodeLister is able to list/get nodes and is populated
	// by the shared informer passed to upgradeController
	nodeLister corelisters.NodeLister
	// nodeSynced returns true if the node shared informer
	// has been synced at least once. Added as a member to the struct to allow
	// injection for testing.
	nodesSynced cache.InformerSynced

	queue workqueue.RateLimitingInterface

	// Holds the initially found version
	initialTopoVersion int
}

// NewController creates a new upgrade controller
func NewController(client kubernetes.Interface) *upgradeController {
	uc := &upgradeController{
		client: client,
		queue:  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName),
	}

	// note this will change in the future to control-plane:
	// https://github.com/kubernetes/kubernetes/pull/95382
	masterNode, err := labels.NewRequirement("node-role.kubernetes.io/master", selection.Exists, nil)
	if err != nil {
		klog.Fatalf("Unable to create labels.NewRequirement: %v", err)
	}

	labelSelector := labels.NewSelector()
	labelSelector = labelSelector.Add(*masterNode)

	uc.informerFactory = informers.NewSharedInformerFactoryWithOptions(client, 0,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector.String()
		}))
	nodeInformer := uc.informerFactory.Core().V1().Nodes()
	klog.Info("Setting up event handlers for node upgrade")
	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    uc.onNodeAdd,
		UpdateFunc: uc.onNodeUpdate,
	})
	uc.nodeLister = nodeInformer.Lister()
	uc.nodesSynced = nodeInformer.Informer().HasSynced
	uc.initialTopoVersion, err = uc.detectInitialVersion(labelSelector)
	if err != nil {
		klog.Fatalf("Unable to run initial version detection: %v", err)
	}
	return uc
}

// onNodeAdd queues the Node for processing.
func (uc *upgradeController) onNodeAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding node %s", key)
	uc.queue.Add(key)
}

// onNodeUpdate updates the Node Selector in the cache and queues the Node for processing.
func (uc *upgradeController) onNodeUpdate(oldObj, newObj interface{}) {
	oldNode := oldObj.(*v1.Node)
	newNode := newObj.(*v1.Node)

	// don't process resync or objects that are marked for deletion
	if oldNode.ResourceVersion == newNode.ResourceVersion ||
		!newNode.GetDeletionTimestamp().IsZero() {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		uc.queue.Add(key)
	}
}

func (uc *upgradeController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer uc.queue.ShutDown()

	klog.Infof("Starting controller %s", controllerName)
	defer klog.Infof("Shutting down controller %s", controllerName)

	informerStop := make(chan struct{})
	uc.informerFactory.Start(informerStop)
	// Wait for the caches to be synced
	klog.Info("Waiting for informer caches to sync")
	if !cache.WaitForNamedCacheSync(controllerName, stopCh, uc.nodesSynced) {
		return fmt.Errorf("error syncing cache")
	}

	ticker := time.NewTicker(1 * time.Second)
	deadline := time.After(30 * time.Minute)
	klog.Info("Waiting for OVN Masters to upgrade to switchover K8S Service route")
	for {
		select {
		case <-ticker.C:
			if uc.processNextWorkItem() {
				klog.Infof("Masters have completed upgrade from topology version %d to %d",
					uc.initialTopoVersion, ovntypes.OvnCurrentTopologyVersion)
				ticker.Stop()
				close(informerStop)
				return nil
			}
		case <-deadline:
			ticker.Stop()
			close(informerStop)
			klog.Fatal("Failed to detect completion of master upgrade after 30 minutes. Check if " +
				"ovnkube-masters have upgraded correctly!")
		case <-stopCh:
			ticker.Stop()
			close(informerStop)
			return nil
		}
	}
}

// if work is complete returns true
func (uc *upgradeController) processNextWorkItem() bool {
	eKey, quit := uc.queue.Get()
	if quit {
		return false
	}
	defer uc.queue.Done(eKey)

	done, err := uc.detectUpgradeDone(eKey.(string))
	if err != nil {
		utilruntime.HandleError(err)
	}
	uc.queue.Forget(eKey)
	return done
}

// returns true if upgrade is detected as complete
func (uc *upgradeController) detectUpgradeDone(nodeName string) (bool, error) {
	klog.Infof("Processing node update for %s", nodeName)
	// Get current Node from the cache
	node, err := uc.nodeLister.Get(nodeName)
	// It´s unlikely that we have an error different that "Not Found Object"
	// because we are getting the object from the informer´s cache
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	topoVer, ok := node.Annotations[ovntypes.OvnK8sTopoAnno]
	if ok && len(topoVer) > 0 {
		ver, err := strconv.Atoi(topoVer)
		if err != nil {
			klog.Warningf("Illegal value detected for %s, on node: %s, value: %s, error: %v",
				ovntypes.OvnK8sTopoAnno, node.Name, topoVer, err)
			return false, err
		}
		if ver == ovntypes.OvnCurrentTopologyVersion {
			return true, nil
		}
	}
	return false, nil
}

// DetermineOVNTopoVersionOnNode determines what OVN Topology version is being used
func (uc *upgradeController) detectInitialVersion(selector labels.Selector) (int, error) {
	// Find out the current OVN topology version by checking "k8s.ovn.org/topo-version" annotation on Master nodes
	nodeList, err := uc.client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return -1, fmt.Errorf("unable to get nodes for checking topo version: %v", err)
	}

	verFound := false
	ver := 0
	maxVers := ver
	// say, we have three ovnkube-master Pods. on rolling update, one of the Pods will
	// perform the topology/master upgrade and set the topology-version annotation for
	// that node. other ovnkube-master Pods will be in standby mode and wouldn't have
	// updated the annotation. so, we need to get the topology-version from all the
	// nodes and pick the maximum value.
	for _, node := range nodeList.Items {
		topoVer, ok := node.Annotations[ovntypes.OvnK8sTopoAnno]
		if ok && len(topoVer) > 0 {
			ver, err = strconv.Atoi(topoVer)
			if err != nil {
				klog.Warningf("Illegal value detected for %s, on node: %s, value: %s, error: %v",
					ovntypes.OvnK8sTopoAnno, node.Name, topoVer, err)
			} else {
				verFound = true
				if ver > maxVers {
					maxVers = ver
				}
			}
		}
	}

	if !verFound {
		klog.Warningf("Unable to detect topology version on nodes")
	}
	return maxVers, nil
}

func (uc *upgradeController) GetInitialTopoVersion() int {
	return uc.initialTopoVersion
}
