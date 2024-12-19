package adminnetworkpolicy

import (
	"fmt"
	"sync"
	"time"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

func (c *Controller) processNextBANPWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	banpKey, quit := c.banpQueue.Get()
	if quit {
		return false
	}
	defer c.banpQueue.Done(banpKey)

	err := c.syncBaselineAdminNetworkPolicy(banpKey)
	if err == nil {
		c.banpQueue.Forget(banpKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", banpKey, err))

	if c.banpQueue.NumRequeues(banpKey) < maxRetries {
		c.banpQueue.AddRateLimited(banpKey)
		return true
	}

	c.banpQueue.Forget(banpKey)
	return true
}

// syncBaselineAdminNetworkPolicy decides the main logic everytime
// we dequeue a key from the banpQueue cache
func (c *Controller) syncBaselineAdminNetworkPolicy(key string) error {
	c.Lock()
	defer c.Unlock()
	startTime := time.Now()
	_, banpName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Processing sync for Baseline Admin Network Policy %s", banpName)

	defer func() {
		klog.V(5).Infof("Finished syncing Baseline Admin Network Policy %s : %v", banpName, time.Since(startTime))
	}()

	banp, err := c.banpLister.Get(banpName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if banp == nil {
		// it was deleted; let's clear up all the related resources to that
		err = c.clearBaselineAdminNetworkPolicy(banpName)
		if err != nil {
			return err
		}
		return nil
	}
	// at this stage the BANP exists in the cluster
	err = c.ensureBaselineAdminNetworkPolicy(banp)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateBANPStatusToNotReady(banp.Name, err.Error())
		return err
	}
	// we can ignore the error if status update doesn't succeed; best effort
	_ = c.updateBANPStatusToReady(banp.Name)
	return nil
}

// clearBaselineAdminNetworkPolicy will handle the logic for deleting all db objects related
// to the provided banp which got deleted.
// uses externalIDs to figure out ownership
func (c *Controller) clearBaselineAdminNetworkPolicy(banpName string) error {
	banp := c.banpCache
	if banp.name != banpName {
		// there is no existing BANP configured with this name, nothing to clean
		klog.Infof("BANP %s/%d not found in cache, nothing to clear", banpName, BANPFlowPriority)
		return nil
	}

	// clear NBDB objects for the given BANP (PG, ACLs on that PG, AddrSets used by the ACLs)
	// remove PG for Subject (ACLs will get cleaned up automatically) across all networks
	predicateIDs := libovsdbops.NewDbObjectIDsAcrossAllContollers(libovsdbops.AddressSetBaselineAdminNetworkPolicy,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: banp.name,
		})
	pgPredicate := libovsdbops.GetPredicateAcrossAllControllers[*nbdb.PortGroup](predicateIDs, nil)
	// no need to batch this with address-set deletes since this itself will contain a bunch of ACLs that need to be deleted which is heavy enough.
	err := libovsdbops.DeletePortGroupsWithPredicate(c.nbClient, pgPredicate)
	if err != nil {
		return fmt.Errorf("unable to delete PGs for BANP %s: %w", banpName, err)
	}
	// remove address-sets that were created for the peers of each rule fpr the whole BANP
	// do this after ACLs are gone so that there is no lingering references
	// do this across all networks for this BANP
	err = c.clearASForPeers(banp.name, libovsdbops.AddressSetBaselineAdminNetworkPolicy)
	if err != nil {
		return fmt.Errorf("failed to delete address-sets for BANP %s: %w", banp.name, err)
	}
	// we can delete the object from the cache now (set the cache back to empty value).
	c.banpCache = &adminNetworkPolicyState{}
	metrics.DecrementBANPCount()

	return nil
}

// ensureBaselineAdminNetworkPolicy will handle the main reconcile logic for any given banp's
// add/update that might be triggered either due to BANP changes or the corresponding
// matching pod or namespace changes.
func (c *Controller) ensureBaselineAdminNetworkPolicy(banp *anpapi.BaselineAdminNetworkPolicy) error {
	desiredBANPState, err := newBaselineAdminNetworkPolicyState(banp)
	if err != nil {
		return err
	}
	// fetch the banpState from our cache
	currentBANPState := c.banpCache
	// Based on the latest kapi BANP, namespace and pod objects:
	// 1) Construct Address-sets with IPs of the peers in the rules
	// 2) Construct ACLs using AS-es and PGs
	// acrss all networks in the cluster
	desiredPorts, err := c.convertANPSubjectToLSPs(desiredBANPState)
	if err != nil {
		return fmt.Errorf("unable to fetch ports for banp %s: %v", desiredBANPState.name, err)
	}
	err = c.expandANPRulePeers(desiredBANPState)
	if err != nil {
		return fmt.Errorf("unable to convert peers to addresses for banp %s: %v", desiredBANPState.name, err)
	}
	atLeastOneRuleUpdated := false
	desiredACLs := c.convertANPRulesToACLs(desiredBANPState, currentBANPState, &atLeastOneRuleUpdated, true)

	// Comparing names for figuring out if cache is populated or not is safe
	// because the singleton BANP will always be called "default" in any cluster
	if currentBANPState.name == "" { // empty struct, no BANP exists
		// this is a fresh BANP create
		klog.Infof("Creating baseline admin network policy %s", banp.Name)
		// 4) Create the PG/ACL/AS in same transact
		// 6) Update the ANP caches to store all the created things if transact was successful
		err = c.createNewANP(desiredBANPState, desiredACLs, desiredPorts, true)
		if err != nil {
			return fmt.Errorf("failed to create BANP %s: %v", desiredBANPState.name, err)
		}
		// since transact was successful we can finally populate the cache
		c.banpCache = desiredBANPState
		metrics.IncrementBANPCount()
		return nil
	}
	// BANP state existed in the cache, which means its either a BANP update or pod/namespace add/update/delete
	klog.V(5).Infof("Baseline Admin network policy %s was found in cache...Syncing it", currentBANPState.name)
	err = c.updateExistingANP(currentBANPState, desiredBANPState, atLeastOneRuleUpdated, false, true, desiredACLs)
	if err != nil {
		return fmt.Errorf("failed to update ANP %s: %v", desiredBANPState.name, err)
	}
	// since transact was successful we can finally replace the currentBANPState in the cache with the latest desired one
	c.banpCache = desiredBANPState
	return nil
}
