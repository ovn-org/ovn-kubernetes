package networkqos

import (
	"fmt"
	"sync"
	"time"

	// libovsdbclient "github.com/ovn-org/libovsdb/client"
	// "github.com/ovn-org/libovsdb/ovsdb"
	// libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	// libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	// "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	// "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	// "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	// v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	// "k8s.io/apimachinery/pkg/util/sets"
	networkqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

func (c *Controller) processNextNQOSWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	nqosKey, quit := c.nqosQueue.Get()
	if quit {
		return false
	}
	defer c.nqosQueue.Done(nqosKey)

	err := c.syncNetworkQoS(nqosKey.(string))
	if err == nil {
		c.nqosQueue.Forget(nqosKey)
		return true
	}
	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", nqosKey, err))

	if c.nqosQueue.NumRequeues(nqosKey) < maxRetries {
		c.nqosQueue.AddRateLimited(nqosKey)
		return true
	}

	c.nqosQueue.Forget(nqosKey)
	return true
}

// syncNetworkQoS decides the main logic everytime
// we dequeue a key from the nqosQueue cache
func (c *Controller) syncNetworkQoS(key string) error {
	// TODO(ffernandes): A global lock is currently used from syncNetworkQoS, syncNetworkQoSPod,
	// syncNetworkQoSNamespace and syncNetworkQoSNode. Planning to do perf/scale runs first
	// and will remove this TODO if there are no concerns with the lock.
	c.Lock()
	defer c.Unlock()
	startTime := time.Now()
	nqosNamespace, nqosName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Processing sync for Network QoS %s", nqosName)

	defer func() {
		klog.V(5).Infof("Finished syncing Network QoS %s : %v", nqosName, time.Since(startTime))
	}()

	nqos, err := c.nqosLister.NetworkQoSes(nqosNamespace).Get(nqosName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if nqos == nil {
		// it was deleted; let's clear up all the related resources to that
		err = c.clearNetworkQos(nqosNamespace, nqosName)
		if err != nil {
			return err
		}
		return nil
	}
	// at this stage the NQOS exists in the cluster
	err = c.ensureNetworkQos(nqos)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateNQOSStatusToNotReady(nqos.Namespace, nqos.Name, err.Error())
		return err
	}
	// we can ignore the error if status update doesn't succeed; best effort
	_ = c.updateNQOSStatusToReady(nqos.Namespace, nqos.Name)
	return nil
}

// ensureNetworkQos will handle the main reconcile logic for any given nqos's
// add/update that might be triggered either due to NQOS changes or the corresponding
// matching pod or namespace changes.
func (c *Controller) ensureNetworkQos(nqos *networkqosapi.NetworkQoS) error {
	cachedName := joinMetaNamespaceAndName(nqos.Namespace, nqos.Name)

	// srcAsIndex := GetNetworkQoSAddrSetDbIDs(nqos.Namespace, nqos.Name, "src", c.controllerName)
	// srcAddrSet, err := c.addressSetFactory.NewAddressSet(srcAsIndex, nil)
	// if err != nil {
	// 	return fmt.Errorf("cannot create addressSet for %s: %w", cachedName, err)
	// }

	desiredNQOSState := &networkQoSState{
		name:                  nqos.Name,
		namespace:             nqos.Namespace,
		networkAttachmentName: nqos.Spec.NetworkAttachmentName,
		// srcAddrSet:            srcAddrSet,
	}

	// TODO: to be implemented...

	// since transact was successful we can finally replace the currentNQOSState in the cache with the latest desired one
	c.nqosCache[cachedName] = desiredNQOSState
	// metrics.UpdateNetworkQoSCount(1)

	return nil
}

// clearNetworkQos will handle the logic for deleting all db objects related
// to the provided nqos which got deleted.
// uses externalIDs to figure out ownership
func (c *Controller) clearNetworkQos(nqosNamespace, nqosName string) error {
	cachedName := joinMetaNamespaceAndName(nqosNamespace, nqosName)

	// See if we need to handle this: https://github.com/ovn-org/ovn-kubernetes/pull/3659#discussion_r1284645817
	_, loaded := c.nqosCache[cachedName]
	if !loaded {
		// there is no existing NQOS configured with this name, nothing to clean
		klog.Infof("NQOS %s not found in cache, nothing to clear", cachedName)
		return nil
	}

	// clear NBDB objects for the given NQOS
	// TODO: to be implemented...

	delete(c.nqosCache, cachedName)
	// metrics.UpdateNetworkQoSCount(-1)

	return nil
}

func (c *Controller) updateNQOSStatusToReady(namespace, name string) error {
	// TODO: to be implemented...
	return nil
}

func (c *Controller) updateNQOSStatusToNotReady(namespace, name string, err string) error {
	// TODO: to be implemented...
	return nil
}
