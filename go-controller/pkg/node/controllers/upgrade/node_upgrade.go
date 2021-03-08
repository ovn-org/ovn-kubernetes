package upgrade

import (
	"fmt"
	"strconv"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreinformers "k8s.io/client-go/informers/core/v1"
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
	client kube.Interface
	// nodeLister is able to list/get endpoint slices and is populated
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
func NewController(client kube.Interface, nodeInformer coreinformers.NodeInformer) *upgradeController {
	uc := &upgradeController{
		client: client,
		queue:  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName),
	}

	klog.Info("Setting up event handlers for node upgrade")
	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    uc.onNodeAdd,
		UpdateFunc: uc.onNodeUpdate,
	})
	uc.nodeLister = nodeInformer.Lister()
	uc.nodesSynced = nodeInformer.Informer().HasSynced
	var err error
	uc.initialTopoVersion, err = uc.detectInitialVersion()
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

func (uc *upgradeController) Run(stopCh <-chan struct{}, informerStop chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer uc.queue.ShutDown()

	klog.Infof("Starting controller %s", controllerName)
	defer klog.Infof("Shutting down controller %s", controllerName)

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
				informerStop <- struct{}{}
				return nil
			}
		case <-deadline:
			ticker.Stop()
			informerStop <- struct{}{}
			klog.Fatal("Failed to detect completion of master upgrade after 30 minutes. Check if " +
				"ovnkube-masters have upgraded correctly!")
		case <-stopCh:
			ticker.Stop()
			informerStop <- struct{}{}
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
func (uc *upgradeController) detectInitialVersion() (int, error) {
	ver := 0
	// Find out the current OVN topology version by checking "k8s.ovn.org/topo-version" annotation on Master nodes
	nodeList, err := uc.client.GetNodes()
	if err != nil {
		return -1, fmt.Errorf("unable to get nodes for checking topo version: %v", err)
	}

	verFound := false
	for _, node := range nodeList.Items {
		topoVer, ok := node.Annotations[ovntypes.OvnK8sTopoAnno]
		if ok && len(topoVer) > 0 {
			ver, err = strconv.Atoi(topoVer)
			if err != nil {
				klog.Warningf("Illegal value detected for %s, on node: %s, value: %s, error: %v",
					ovntypes.OvnK8sTopoAnno, node.Name, topoVer, err)
			} else {
				verFound = true
				break
			}
		}
	}

	if !verFound {
		klog.Warningf("Unable to detect topology version on nodes")
	}
	return ver, nil
}

func (uc *upgradeController) GetInitialTopoVersion() int {
	return uc.initialTopoVersion
}
