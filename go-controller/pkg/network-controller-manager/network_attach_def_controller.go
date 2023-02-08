package networkControllerManager

import (
	"context"
	"fmt"
	"sync"
	"time"

	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	networkattchmentdefclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	netattachdefinformers "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions"
	networkattachmentdefinitioninformerfactory "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions"
	networkattachmentdefinitionlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"golang.org/x/time/rate"
)

const (
	// maxRetries is the number of times a object will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the
	// sequence of delays between successive queuings of an object.
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	// maxRetries = 15

	controllerName  = "net-attach-def-controller"
	avoidResync     = 0
	numberOfWorkers = 2
	qps             = 15
	maxRetries      = 10
)

type BaseNetworkController interface {
	Start(ctx context.Context) error
	Stop()
	GetNetworkName() string
}

type NetworkController interface {
	BaseNetworkController
	CompareNetConf(util.NetConfInfo) bool
	AddNAD(nadName string)
	DeleteNAD(nadName string)
	HasNAD(nadName string) bool
	// Cleanup cleans up the given network, it could be called to clean up network controllers that are deleted when
	// ovn-k8s is down; so it's receiver could be a dummy network controller.
	Cleanup(netName string) error
}

// NetworkControllerManager manages all network controllers
type NetworkControllerManager interface {
	NewNetworkController(netInfo util.NetInfo, netConfInfo util.NetConfInfo) (NetworkController, error)
	CleanupDeletedNetworks(allControllers []NetworkController) error
}

type networkNADInfo struct {
	nadNames  map[string]struct{}
	nc        NetworkController
	isStarted bool
}

type nadNetConfInfo struct {
	util.NetConfInfo
	netName string
}

type netAttachDefinitionController struct {
	recorder           record.EventRecorder
	ncm                NetworkControllerManager
	nadFactory         networkattachmentdefinitioninformerfactory.SharedInformerFactory
	netAttachDefLister networkattachmentdefinitionlisters.NetworkAttachmentDefinitionLister
	netAttachDefSynced cache.InformerSynced
	queue              workqueue.RateLimitingInterface
	loopPeriod         time.Duration

	// key is nadName, value is nadNetConfInfo
	perNADNetConfInfo *syncmap.SyncMap[*nadNetConfInfo]
	// controller for all networks, key is netName of net-attach-def, value is networkNADInfo
	// this map is updated either at the very beginning of ovnkube-master when initializing the default controller
	// or when net-attach-def is added/deleted. All these are serialized by syncmap lock
	perNetworkNADInfo *syncmap.SyncMap[*networkNADInfo]
}

func newNetAttachDefinitionController(ncm NetworkControllerManager, networkAttchDefClient networkattchmentdefclientset.Interface,
	recorder record.EventRecorder) *netAttachDefinitionController {
	nadFactory := netattachdefinformers.NewSharedInformerFactoryWithOptions(
		networkAttchDefClient,
		avoidResync,
	)
	netAttachDefInformer := nadFactory.K8sCniCncfIo().V1().NetworkAttachmentDefinitions()
	rateLimiter := workqueue.NewMaxOfRateLimiter(
		workqueue.NewItemExponentialFailureRateLimiter(5*time.Millisecond, 1000*time.Second),
		&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(qps), qps*5)})

	nadController := &netAttachDefinitionController{
		recorder:           recorder,
		ncm:                ncm,
		nadFactory:         nadFactory,
		netAttachDefLister: netAttachDefInformer.Lister(),
		netAttachDefSynced: netAttachDefInformer.Informer().HasSynced,
		queue:              workqueue.NewNamedRateLimitingQueue(rateLimiter, "net-attach-def"),
		loopPeriod:         time.Second,
		perNADNetConfInfo:  syncmap.NewSyncMap[*nadNetConfInfo](),
		perNetworkNADInfo:  syncmap.NewSyncMap[*networkNADInfo](),
	}
	netAttachDefInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    nadController.onNetworkAttachDefinitionAdd,
			UpdateFunc: nadController.onNetworkAttachDefinitionUpdate,
			DeleteFunc: nadController.onNetworkAttachDefinitionDelete,
		})
	return nadController
}

func (nadController *netAttachDefinitionController) Run(stopChan <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer func() {
		klog.Infof("Shutting down controller %s", controllerName)
		nadController.queue.ShutDown()
	}()

	nadController.nadFactory.Start(stopChan)
	klog.Infof("Starting controller %s", controllerName)
	if !cache.WaitForNamedCacheSync(controllerName, stopChan, nadController.netAttachDefSynced) {
		return fmt.Errorf("error syncing cache")
	}

	err := nadController.SyncNetworkControllers()
	if err != nil {
		return fmt.Errorf("failed to sync all existing NAD entries: %v", err)
	}

	klog.Info("Starting workers for controller %s", controllerName)
	wg := &sync.WaitGroup{}
	for i := 0; i < numberOfWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(nadController.worker, nadController.loopPeriod, stopChan)
		}()
	}
	wg.Wait()

	// wait until we're told to stop
	<-stopChan
	return nil
}

func (nadController *netAttachDefinitionController) SyncNetworkControllers() (err error) {
	startTime := time.Now()
	klog.V(4).Infof("Starting repairing loop for %s", controllerName)
	defer func() {
		klog.V(4).Infof("Finished repairing loop for %s: %v err: %v", controllerName,
			time.Since(startTime), err)
	}()

	existingNADs, err := nadController.netAttachDefLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to get list of all net-attach-def")
	}

	// Need to walk through all the NADs and create all network controllers and update their list of NADs.
	// The controller can only be started after all known NADs are added so as to avoid to the extent possible
	// the errors and retries that would result if the controller attempted to process pods attached with NADs
	// we wouldn't otherwise know about yet
	for _, nad := range existingNADs {
		err = nadController.AddNetAttachDef(nadController.ncm, nad, false)
		if err != nil {
			return err
		}
	}

	return nadController.ncm.CleanupDeletedNetworks(nadController.GetAllNetworkControllers())
}

func (nadController *netAttachDefinitionController) worker() {
	for nadController.processNextWorkItem() {
	}
}

func (nadController *netAttachDefinitionController) processNextWorkItem() bool {
	key, quit := nadController.queue.Get()
	if quit {
		return false
	}
	defer nadController.queue.Done(key)

	err := nadController.sync(key.(string))
	nadController.handleErr(err, key)
	return true
}

func (nadController *netAttachDefinitionController) sync(key string) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Sync net-attach-def %s/%s", namespace, name)
	defer func() {
		klog.V(4).Infof("Finished syncing net-attach-def %s/%s : %v", namespace, name, time.Since(startTime))
	}()

	nad, err := nadController.netAttachDefLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if nad == nil {
		return nadController.DeleteNetAttachDef(key)
	} else {
		return nadController.AddNetAttachDef(nadController.ncm, nad, true)
	}
}

func (nadController *netAttachDefinitionController) handleErr(err error, key interface{}) {
	ns, name, keyErr := cache.SplitMetaNamespaceKey(key.(string))
	if keyErr != nil {
		klog.ErrorS(err, "Failed to split meta namespace cache key", "key", key)
	}
	if err == nil {
		nadController.queue.Forget(key)
		return
	}

	if nadController.queue.NumRequeues(key) < maxRetries {
		nadController.queue.AddRateLimited(key)
		klog.V(2).InfoS("Error syncing net-attach-def, retrying", "net-attach-def", klog.KRef(ns, name), "err", err)
		return
	}

	klog.Warningf("Dropping net-attach-def %q out of the queue: %v", key, err)
	nadController.queue.Forget(key)
	utilruntime.HandleError(err)
}

func (nadController *netAttachDefinitionController) queueNetworkAttachDefinition(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for net-attach-def %+v: %v", obj, err))
		return
	}
	nadController.queue.Add(key)
}

func (nadController *netAttachDefinitionController) onNetworkAttachDefinitionAdd(obj interface{}) {
	nad := obj.(*nettypes.NetworkAttachmentDefinition)
	if nad == nil {
		utilruntime.HandleError(fmt.Errorf("invalid net-attach-def provided to onNetworkAttachDefinitionAdd()"))
		return
	}

	klog.V(4).Infof("Adding net-attach-def %s/%s", nad.Namespace, nad.Name)
	nadController.queueNetworkAttachDefinition(obj)
}

func (nadController *netAttachDefinitionController) onNetworkAttachDefinitionUpdate(oldObj, newObj interface{}) {
	oldNAD := oldObj.(*nettypes.NetworkAttachmentDefinition)
	newNAD := newObj.(*nettypes.NetworkAttachmentDefinition)
	if oldNAD == nil || newNAD == nil {
		utilruntime.HandleError(fmt.Errorf("invalid net-attach-def provided to onNetworkAttachDefinitionUpdate()"))
		return
	}

	// don't process resync or objects that are marked for deletion
	if oldNAD.ResourceVersion == newNAD.ResourceVersion ||
		!newNAD.GetDeletionTimestamp().IsZero() {
		return
	}

	err := fmt.Sprintf("Updating net-attach-def %s/%s is not supported", newNAD.Namespace, newNAD.Name)
	nadRef := kapi.ObjectReference{
		Kind:      "NetworkAttachmentDefinition",
		Namespace: newNAD.Namespace,
		Name:      newNAD.Name,
	}
	nadController.recorder.Eventf(&nadRef, kapi.EventTypeWarning, "ErrorUpdatingResource", err)
	klog.Warningf(err)
}

func (nadController *netAttachDefinitionController) onNetworkAttachDefinitionDelete(obj interface{}) {
	nad := obj.(*nettypes.NetworkAttachmentDefinition)
	if nad == nil {
		utilruntime.HandleError(fmt.Errorf("invalid net-attach-def provided to onNetworkAttachDefinitionDelete()"))
		return
	}

	klog.V(4).Infof("Deleting net-attach-def %s/%s", nad.Namespace, nad.Name)
	nadController.queueNetworkAttachDefinition(obj)
}

// GetAllNetworkControllers returns a snapshot of all managed NAD associated network controllers.
// Caller needs to note that there are no guarantees the return results reflect the real time
// condition. There maybe more controllers being added, and returned controllers may be deleted
func (nadController *netAttachDefinitionController) GetAllNetworkControllers() []NetworkController {
	allNetworkNames := nadController.perNetworkNADInfo.GetKeys()
	allNetworkControllers := make([]NetworkController, 0, len(allNetworkNames))
	for _, netName := range allNetworkNames {
		nadController.perNetworkNADInfo.LockKey(netName)
		nni, ok := nadController.perNetworkNADInfo.Load(netName)
		if ok {
			allNetworkControllers = append(allNetworkControllers, nni.nc)
		}
		nadController.perNetworkNADInfo.UnlockKey(netName)
	}
	return allNetworkControllers
}

// AddNetAttachDef adds the given nad to the associated controller. It creates the controller if this
// is the first NAD of the network.
// Non-retriable errors (configuration error etc.) are just logged, and the function immediately returns nil.
func (nadController *netAttachDefinitionController) AddNetAttachDef(ncm NetworkControllerManager,
	netattachdef *nettypes.NetworkAttachmentDefinition, doStart bool) error {
	var netConfInfo util.NetConfInfo
	var nInfo util.NetInfo
	var err, invalidNADErr error
	var netName string

	netAttachDefName := util.GetNADName(netattachdef.Namespace, netattachdef.Name)
	klog.Infof("Add net-attach-def %s", netAttachDefName)

	nInfo, netConfInfo, invalidNADErr = util.ParseNADInfo(netattachdef)
	if invalidNADErr == nil {
		netName = nInfo.GetNetworkName()
		if netName == types.DefaultNetworkName {
			invalidNADErr = fmt.Errorf("NAD for default network, skip it")
		}
	}

	return nadController.perNADNetConfInfo.DoWithLock(netAttachDefName, func(nadName string) error {
		nadNci, loaded := nadController.perNADNetConfInfo.LoadOrStore(nadName, &nadNetConfInfo{
			NetConfInfo: netConfInfo,
			netName:     netName,
		})
		if !loaded {
			// first time to process this nad
			if invalidNADErr != nil {
				// invalid nad, nothing to do
				klog.Warningf("net-attach-def %s is first seen and is invalid: %v", nadName, invalidNADErr)
				nadController.perNADNetConfInfo.Delete(nadName)
				return nil
			}
			klog.V(5).Infof("net-attach-def %s network %s first seen", nadName, netName)
			err = nadController.addNADToController(ncm, nadName, nInfo, netConfInfo, doStart)
			if err != nil {
				klog.Errorf("Failed to add net-attach-def %s to network %s: %v", nadName, netName, err)
				nadController.perNADNetConfInfo.Delete(nadName)
				return err
			}
		} else {
			klog.V(5).Infof("net-attach-def %s network %s already exists", nadName, netName)
			nadUpdated := false
			if invalidNADErr != nil {
				nadUpdated = true
			} else if nadNci.netName != netName {
				// netconf network name changed
				klog.V(5).Infof("net-attach-def %s network name %s has changed", netName, nadNci.netName)
				nadUpdated = true
			} else if !nadNci.CompareNetConf(netConfInfo) {
				// netconf spec changed
				klog.V(5).Infof("net-attach-def %s spec has changed", nadName)
				nadUpdated = true
			}

			if !nadUpdated {
				// nothing changed, may still need to start the controller
				if !doStart {
					return nil
				}
				err = nadController.addNADToController(ncm, nadName, nInfo, netConfInfo, doStart)
				if err != nil {
					klog.Errorf("Failed to add net-attach-def %s to network %s: %v", nadName, netName, err)
					return err
				}
				return nil
			}
			if nadUpdated {
				klog.V(5).Infof("net-attach-def %s network %s updated", nadName, netName)
				// delete the NAD from the old network first
				err := nadController.deleteNADFromController(nadNci.netName, nadName)
				if err != nil {
					klog.Errorf("Failed to delete net-attach-def %s from network %s: %v", nadName, nadNci.netName, err)
					return err
				}
				nadController.perNADNetConfInfo.Delete(nadName)
			}
			if invalidNADErr != nil {
				klog.Warningf("net-attach-def %s is invalid: %v", nadName, invalidNADErr)
				return nil
			}
			klog.V(5).Infof("Add updated net-attach-def %s to network %s", nadName, netName)
			nadController.perNADNetConfInfo.LoadOrStore(nadName, &nadNetConfInfo{NetConfInfo: netConfInfo, netName: netName})
			err = nadController.addNADToController(ncm, nadName, nInfo, netConfInfo, doStart)
			if err != nil {
				klog.Errorf("Failed to add net-attach-def %s to network %s: %v", nadName, netName, err)
				nadController.perNADNetConfInfo.Delete(nadName)
				return err
			}
			return nil
		}
		return nil
	})
}

// DeleteNetAttachDef deletes the given NAD from the associated controller. It delete the controller if this
// is the last NAD of the network
func (nadController *netAttachDefinitionController) DeleteNetAttachDef(netAttachDefName string) error {
	klog.Infof("Delete net-attach-def %s", netAttachDefName)
	return nadController.perNADNetConfInfo.DoWithLock(netAttachDefName, func(nadName string) error {
		existingNadNetConfInfo, found := nadController.perNADNetConfInfo.Load(nadName)
		if !found {
			klog.V(5).Infof("net-attach-def %s not found for removal", nadName)
			return nil
		}
		err := nadController.deleteNADFromController(existingNadNetConfInfo.netName, nadName)
		if err != nil {
			klog.Errorf("Failed to delete net-attach-def %s from network %s: %v", nadName, existingNadNetConfInfo.netName, err)
			return err
		}
		nadController.perNADNetConfInfo.Delete(nadName)
		return nil
	})
}

func (nadController *netAttachDefinitionController) addNADToController(ncm NetworkControllerManager, nadName string,
	nInfo util.NetInfo, netConfInfo util.NetConfInfo, doStart bool) (err error) {
	var oc NetworkController
	var nadExists, isStarted bool

	netName := nInfo.GetNetworkName()
	klog.V(5).Infof("Add net-attach-def %s to network %s", nadName, netName)
	return nadController.perNetworkNADInfo.DoWithLock(netName, func(networkName string) error {
		nni, loaded := nadController.perNetworkNADInfo.LoadOrStore(networkName, &networkNADInfo{
			nadNames:  map[string]struct{}{},
			nc:        nil,
			isStarted: false,
		})
		if !loaded {
			defer func() {
				if err != nil {
					nadController.perNetworkNADInfo.Delete(networkName)
				}
			}()
			// first NAD for this network, create controller
			klog.V(5).Infof("First net-attach-def %s of network %s added, create network controller", nadName, networkName)
			oc, err = ncm.NewNetworkController(nInfo, netConfInfo)
			if err != nil {
				return fmt.Errorf("failed to create network controller for network %s: %v", networkName, err)
			}
			nni.nc = oc
		} else {
			klog.V(5).Infof("net-attach-def %s added to existing network %s", nadName, networkName)
			// controller of this network already exists
			oc = nni.nc
			isStarted = nni.isStarted
			_, nadExists = nni.nadNames[nadName]

			if !oc.CompareNetConf(netConfInfo) {
				if nadExists {
					// this should not happen, continue to start the existing controller if requested
					return fmt.Errorf("net-attach-def %s netconf spec changed, should not happen", networkName)
				} else {
					return fmt.Errorf("NAD %s does not share the same CNI config with network %s",
						nadName, networkName)
				}
			}
		}
		if !nadExists {
			nni.nadNames[nadName] = struct{}{}
			nni.nc.AddNAD(nadName)
			defer func() {
				if err != nil {
					delete(nni.nadNames, nadName)
					nni.nc.DeleteNAD(nadName)
				}
			}()
		}

		if !doStart || isStarted {
			return nil
		}

		klog.V(5).Infof("Start network controller for network %s", networkName)
		// start the controller if requested
		err = oc.Start(context.TODO())
		if err == nil {
			nni.isStarted = true
			return nil
		}
		return fmt.Errorf("network controller for network %s failed to be started: %v", networkName, err)
	})
}

func (nadController *netAttachDefinitionController) deleteNADFromController(netName, nadName string) error {
	klog.V(5).Infof("Delete net-attach-def %s from network %s", nadName, netName)
	return nadController.perNetworkNADInfo.DoWithLock(netName, func(networkName string) error {
		nni, found := nadController.perNetworkNADInfo.Load(networkName)
		if !found {
			klog.V(5).Infof("Network controller for network %s not found", networkName)
			return nil
		}
		_, nadExists := nni.nadNames[nadName]
		if !nadExists {
			klog.V(5).Infof("Unable to remove NAD %s, does not exist on network %s", nadName, networkName)
			return nil
		}

		oc := nni.nc
		delete(nni.nadNames, nadName)
		if len(nni.nadNames) == 0 {
			klog.V(5).Infof("The last NAD: %s of network %s has been deleted, stopping network controller", nadName, networkName)
			oc.Stop()
			err := oc.Cleanup(oc.GetNetworkName())
			// set isStarted to false even stop failed, as the operation could be half-done.
			// So if a new NAD with the same netconf comes in, it can restart the controller.
			nni.isStarted = false
			if err != nil {
				nni.nadNames[nadName] = struct{}{}
				return fmt.Errorf("failed to stop network controller for network %s: %v", networkName, err)
			}
			nadController.perNetworkNADInfo.Delete(networkName)
		}
		nni.nc.DeleteNAD(nadName)
		klog.V(5).Infof("Delete NAD %s from controller of network %s", nadName, networkName)
		return nil
	})
}
