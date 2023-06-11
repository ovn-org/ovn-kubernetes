package adminnetworkpolicy

import (
	"fmt"
	"sync"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
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

	err := c.syncBaselineAdminNetworkPolicy(banpKey.(string))
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
	// TODO(tssurya): This global lock will be inefficient, we will do perf runs and improve if needed
	c.Lock()
	defer c.Unlock()
	startTime := time.Now()
	_, banpName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(4).Infof("Processing sync for Baseline Admin Network Policy %s", banpName)

	defer func() {
		klog.V(4).Infof("Finished syncing Baseline Admin Network Policy %s : %v", banpName, time.Since(startTime))
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
		_ = c.updateBANPStatusToNotReady(banp, c.zone, err.Error())
		return err
	}
	// we can ignore the error if status update doesn't succeed; best effort
	_ = c.updateBANPStatusToReady(banp, c.zone)
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
	// remove PG for Subject (ACLs will get cleaned up automatically)
	portGroupName, readableGroupName := getAdminNetworkPolicyPGName(banp.name, true)
	// no need to batch this with address-set deletes since this itself will contain a bunch of ACLs that need to be deleted which is heavy enough.
	err := libovsdbops.DeletePortGroups(c.nbClient, portGroupName)
	if err != nil {
		return fmt.Errorf("unable to delete PG %s for BANP %s: %w", readableGroupName, banp.name, err)
	}
	// remove address-sets that were created for the peers of each rule fpr the whole ANP
	// do this after ACLs are gone so that there is no lingering references
	err = c.clearASForPeers(banp.name, libovsdbops.AddressSetBaselineAdminNetworkPolicy)
	if err != nil {
		return fmt.Errorf("failed to delete address-sets for BANP %s: %w", banp.name, err)
	}
	// we can delete the object from the cache now (set the cache back to empty value).
	c.banpCache = &adminNetworkPolicyState{}

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
	// 1) Construct Port Group name using ANP name
	// 2) Construct Address-sets with IPs of the peers in the rules
	// 3) Construct ACLs using AS-es and PGs
	portGroupName, _ := getAdminNetworkPolicyPGName(desiredBANPState.name, true)
	desiredPorts, err := c.getPortsOfSubject(desiredBANPState.subject)
	if err != nil {
		return fmt.Errorf("unable to fetch ports for banp %s: %v", desiredBANPState.name, err)
	}
	err = c.constructIPSetOfPeers(desiredBANPState)
	if err != nil {
		return fmt.Errorf("unable to build IPsets for banp %s: %v", desiredBANPState.name, err)
	}
	atLeastOneRuleUpdated := false
	desiredACLs := c.getACLsOfRules(desiredBANPState, currentBANPState, portGroupName, &atLeastOneRuleUpdated, true)

	// Comparing names for figuring out if cache is populated or not is safe
	// because the singleton BANP will always be called "default" in any cluster
	if currentBANPState.name == "" { // empty struct, no BANP exists
		// this is a fresh BANP create
		klog.Infof("Creating baseline admin network policy %s", banp.Name)
		// 4) Create the PG/ACL/AS in same transact (TODO: See if batching is more efficient after scale runs)
		// 6) Update the ANP caches to store all the created things if transact was successful
		err = c.createNewANP(desiredBANPState, desiredACLs, desiredPorts, true)
		if err != nil {
			return fmt.Errorf("failed to create BANP %s: %v", desiredBANPState.name, err)
		}
		// since transact was successful we can finally populate the cache
		c.banpCache = desiredBANPState
		return nil
	}
	var ops []ovsdb.Operation
	// BANP state existed in the cache, which means its either a BANP update or pod/namespace add/update/delete
	klog.V(3).Infof("Baseline Admin network policy %s was found in cache...Syncing it", currentBANPState.name)
	// Did BANP.Spec.Ingress Change (rule inserts/deletes)? && || Did BANP.Spec.Egress Change (rule inserts/deletes)? && ||
	// If yes we need to fully recompute the acls present in our BANP's port group; Let's do a full recompute and return.
	// Reason behind a full recompute: Each rule has precendence based on its position and priority of BANP; if any of that changes
	// better to delete and recreate ACLs rather than figure out from caches
	// TODO(tssurya): Investigate if we can be smarter about which ACLs to delete and which to add
	// rather than always cleaning up everything and recreating them. But this is tricky since rules have precendence
	// from their ordering.
	// NOTE: Changes to admin policies should be a rare action (can be improved post user feedback) - usually churn would be around namespaces and pods
	fullPeerRecompute := (len(currentBANPState.egressRules) != len(desiredBANPState.egressRules) ||
		len(currentBANPState.ingressRules) != len(desiredBANPState.ingressRules))
	if fullPeerRecompute {
		// full recompute
		// which means update all ACLs and address-sets
		klog.V(3).Infof("BANP %s with priority (old %d, new %d) was updated", desiredBANPState.name, currentBANPState.anpPriority, desiredBANPState.anpPriority)
		ops, err = c.constructOpsForRuleChanges(desiredBANPState, true)
		if err != nil {
			return fmt.Errorf("failed to create update BANP ops %s: %v", desiredBANPState.name, err)
		}
	}

	// Did BANP.Spec.Ingress rules get updated?
	// (at this stage the length of BANP.Spec.Ingress hasn't changed, so individual rules either got updated at their values or positions are switched)
	// The fields that we care about for rebuilding ACLs are
	// (i) `ports` (ii) `actions` (iii) priority for a given rule
	// The changes to peer labels, peer pod label updates, namespace label updates etc can be inferred
	// from the podIPs cache we store.
	// Did the BANP.Spec.Ingress.Peers Change?
	// 1) BANP.Spec.Ingress.Peers.Namespaces changed && ||
	// 2) BANP.Spec.Ingress.Peers.Pods changed && ||
	// 3) A namespace started or stopped matching the peer && ||
	// 4) A pod started or stopped matching the peer
	// If yes we need to recompute the IPs present in our BANP's peer's address-sets
	if !fullPeerRecompute && len(desiredBANPState.ingressRules) == len(currentBANPState.ingressRules) {
		addrOps, err := c.constructOpsForPeerChanges(desiredBANPState.ingressRules,
			currentBANPState.ingressRules, desiredBANPState.name, true)
		if err != nil {
			return fmt.Errorf("failed to create ops for changes to BANP %s ingress peers: %v", desiredBANPState.name, err)
		}
		ops = append(ops, addrOps...)
	}

	// Did BANP.Spec.Egress rules get updated?
	// (at this stage the length of BANP.Spec.Egress hasn't changed, so individual rules either got updated at their values or positions are switched)
	// The fields that we care about for rebuilding ACLs are
	// (i) `ports` (ii) `actions` (iii) priority for a given rule
	// Did the BANP.Spec.Egress.Peers Change?
	// 1) BANP.Spec.Egress.Peers.Namespaces changed && ||
	// 2) BANP.Spec.Egress.Peers.Pods changed && ||
	// 3) A namespace started or stopped matching the peer && ||
	// 4) A pod started or stopped matching the peer
	// If yes we need to recompute the IPs present in our BANP's peer's address-sets
	if !fullPeerRecompute && len(desiredBANPState.egressRules) == len(currentBANPState.egressRules) {
		addrOps, err := c.constructOpsForPeerChanges(desiredBANPState.egressRules,
			currentBANPState.egressRules, desiredBANPState.name, true)
		if err != nil {
			return fmt.Errorf("failed to create ops for changes to BANP %s egress peers: %v", desiredBANPState.name, err)
		}
		ops = append(ops, addrOps...)
	}
	// TODO(tssurya): Check if we can be more efficient by narrowing down exactly which ACL needs a change
	// The rules which didn't change -> those updates will be no-ops thanks to libovsdb
	// The rules that changed in terms of their `getACLMutableFields`
	// will be simply updated since externalIDs will remain the same for these ACLs
	// No delete ACLs action is required for this scenario
	// If full aclRecompute=true was done above already, we don't care about individual rule updates...
	// that will automatically be taken care of in the above transactions
	if fullPeerRecompute || atLeastOneRuleUpdated {
		klog.V(3).Infof("BANP %s was updated", desiredBANPState.name)
		ops, err = libovsdbops.CreateOrUpdateACLsOps(c.nbClient, ops, desiredACLs...)
		if err != nil {
			return fmt.Errorf("failed to create ACL ops for banp %s: %v", desiredBANPState.name, err)
		}
		// since we update the portgroup with the new set of ACLs, any unreferenced set of ACLs
		// will be automatically removed
		ops, err = libovsdbops.UpdatePortGroupSetACLsOps(c.nbClient, ops, portGroupName, desiredACLs)
		if err != nil {
			return fmt.Errorf("failed to create ops to add port to a port group for banp %s: %v", desiredBANPState.name, err)
		}
	}

	// Did the BANP.Spec.Subject Change?
	// 1) BANP.Spec.Namespaces changed && ||
	// 2) BANP.Spec.Pods changed && ||
	// 3) A namespace started or stopped matching the subject && ||
	// 4) A pod started or stopped matching the subject
	// If yes we need to recompute the ports present in our ANP's port group
	subjectOps, err := c.constructOpsForSubjectChanges(currentBANPState, desiredBANPState, portGroupName)
	if err != nil {
		return fmt.Errorf("failed to create ops for changes to BANP %s subject: %v", desiredBANPState.name, err)
	}
	ops = append(ops, subjectOps...)

	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to run ovsdb txn to update BANP %s: %v", desiredBANPState.name, err)
	}
	// since transact was successful we can finally replace the currentBANPState in the cache with the latest desired one
	c.banpCache = desiredBANPState
	return nil
}
