package adminnetworkpolicy

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

func (c *Controller) processNextANPWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	anpKey, quit := c.anpQueue.Get()
	if quit {
		return false
	}
	defer c.anpQueue.Done(anpKey)

	err := c.syncAdminNetworkPolicy(anpKey)
	if err == nil {
		c.anpQueue.Forget(anpKey)
		return true
	}
	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", anpKey, err))

	if c.anpQueue.NumRequeues(anpKey) < maxRetries {
		c.anpQueue.AddRateLimited(anpKey)
		return true
	}

	c.anpQueue.Forget(anpKey)
	return true
}

// syncAdminNetworkPolicy decides the main logic everytime
// we dequeue a key from the anpQueue cache
func (c *Controller) syncAdminNetworkPolicy(key string) error {
	// TODO(tssurya): A global lock is currently used from syncAdminNetworkPolicy, syncAdminNetworkPolicyPod,
	// syncAdminNetworkPolicyNamespace and syncAdminNetworkPolicyNode. Planning to do perf/scale runs first
	// and will remove this TODO if there are no concerns with the lock.
	c.Lock()
	defer c.Unlock()
	startTime := time.Now()
	_, anpName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Processing sync for Admin Network Policy %s", anpName)

	defer func() {
		klog.V(5).Infof("Finished syncing Admin Network Policy %s : %v", anpName, time.Since(startTime))
	}()

	anp, err := c.anpLister.Get(anpName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if anp == nil {
		// it was deleted; let's clear up all the related resources to that
		err = c.clearAdminNetworkPolicy(anpName)
		if err != nil {
			return err
		}
		return nil
	}
	// at this stage the ANP exists in the cluster
	err = c.ensureAdminNetworkPolicy(anp)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateANPStatusToNotReady(anp.Name, err.Error())
		if !errors.Is(err, ErrorANPPriorityUnsupported) {
			// we don't want to retry for these specific errors since they
			// need manual intervention from users to update their CRDs
			return nil
		}
		return err
	}
	// we can ignore the error if status update doesn't succeed; best effort
	_ = c.updateANPStatusToReady(anp.Name)
	return nil
}

// ensureAdminNetworkPolicy will handle the main reconcile logic for any given anp's
// add/update that might be triggered either due to ANP changes or the corresponding
// matching pod or namespace changes.
func (c *Controller) ensureAdminNetworkPolicy(anp *anpapi.AdminNetworkPolicy) error {
	// We only support priority ranges 0-99 in OVN-K for an ANP even if upstream
	// supports upto 1000. The 0 (highest) corresponds to 30,000 in OVN world and
	// 99 (lowest) corresponds to 20,100 in OVN world
	if anp.Spec.Priority > ovnkSupportedPriorityUpperBound {
		c.eventRecorder.Eventf(&v1.ObjectReference{
			Kind: "AdminNetworkPolicy",
			Name: anp.Name,
		}, v1.EventTypeWarning, ANPWithUnsupportedPriorityEvent, "This ANP %s has an unsupported priority %d; "+
			"Please update the priority to a value between 0(highest priority) and 99(lowest priority)", anp.Name, anp.Spec.Priority)
		return fmt.Errorf("error attempting to add ANP %s with priority %d because, "+
			"%w", anp.Name, anp.Spec.Priority, ErrorANPPriorityUnsupported)
	}
	desiredANPState, err := newAdminNetworkPolicyState(anp)
	if err != nil {
		return err
	}

	// fetch the anpState from our cache if it exists
	currentANPState, loaded := c.anpCache[anp.Name]
	// Based on the latest kapi ANP, namespace and pod objects:
	// 1) Construct Port Group name using ANP name and ports of pods in ANP subject
	// 2) Construct Address-sets with IPs of the peers in the rules
	// 3) Construct ACLs using AS-es and PGs
	portGroupName := c.getANPPortGroupName(desiredANPState.name, false)

	desiredPorts, err := c.convertANPSubjectToLSPs(desiredANPState)
	if err != nil {
		return fmt.Errorf("unable to fetch ports for anp %s: %v", desiredANPState.name, err)
	}
	err = c.expandANPRulePeers(desiredANPState)
	if err != nil {
		return fmt.Errorf("unable to convert peers to addresses for anp %s: %v", desiredANPState.name, err)
	}
	atLeastOneRuleUpdated := false
	desiredACLs := c.convertANPRulesToACLs(desiredANPState, currentANPState, portGroupName, &atLeastOneRuleUpdated, false)

	if !loaded {
		// this is a fresh ANP create
		klog.Infof("Creating admin network policy %s/%d", anp.Name, anp.Spec.Priority)
		// 4) Create the PG/ACL/AS in same transact
		// 5) Update the ANP caches to store all the created things if transact was successful
		err = c.createNewANP(desiredANPState, desiredACLs, desiredPorts, false)
		if err != nil {
			return fmt.Errorf("failed to create ANP %s: %v", desiredANPState.name, err)
		}
		// If an ANP is created at the same priority as another one, let us trigger an event before
		// applying it warning the user to verify there are no overlapping rules to rule out undefined
		// behaviour. Do this once during creation.
		if existingName, loaded := c.anpPriorityMap[desiredANPState.anpPriority]; loaded && existingName != anp.Name {
			klog.Warningf("Warning against attempting to add ANP %s with priority %d when at least one other ANP %s, "+
				"exists with the same priority", anp.Name, anp.Spec.Priority, existingName)
			c.eventRecorder.Eventf(&v1.ObjectReference{
				Kind: "AdminNetworkPolicy",
				Name: anp.Name,
			}, v1.EventTypeWarning, ANPWithDuplicatePriorityEvent, "This ANP %s has a conflicting priority with ANP %s:"+
				"Please verify your rules are non-lapping between all policies at same priority to avoid undefined behavior",
				anp.Name, existingName)
		} else {
			// Let us update the anpPriorityMap cache by adding this new priority to it
			// only if there wasn't an already existing entry at that priority - in which case we don't need to update the cache
			c.anpPriorityMap[desiredANPState.anpPriority] = anp.Name
		}
		// since transact was successful we can finally populate the cache
		c.anpCache[anp.Name] = desiredANPState
		metrics.IncrementANPCount()
		return nil
	}
	// ANP state existed in the cache, which means its either an ANP update or pod/namespace add/update/delete
	klog.V(5).Infof("Admin network policy %s/%d was found in cache...Syncing it", currentANPState.name, currentANPState.anpPriority)
	hasPriorityChanged := (currentANPState.anpPriority != desiredANPState.anpPriority)
	err = c.updateExistingANP(currentANPState, desiredANPState, atLeastOneRuleUpdated, hasPriorityChanged, false, desiredACLs)
	if err != nil {
		return fmt.Errorf("failed to update ANP %s: %v", desiredANPState.name, err)
	}
	// We also need to update c.anpPriorityMap cache if this ANP was stored in it
	if hasPriorityChanged {
		klog.V(3).Infof("Deleting and re-adding correct priority (old %d, new %d) from anpPriorityMap for %s",
			currentANPState.anpPriority, desiredANPState.anpPriority, desiredANPState.name)
		if existingName, loaded := c.anpPriorityMap[currentANPState.anpPriority]; loaded && existingName == desiredANPState.name {
			delete(c.anpPriorityMap, currentANPState.anpPriority)
		}
		// Let us update the anpPriorityMap cache by adding this new priority to it
		if existingName, loaded := c.anpPriorityMap[desiredANPState.anpPriority]; loaded && existingName != desiredANPState.name {
			klog.Warningf("Warning against attempting to update ANP %s with priority %d when at least one other ANP %s, "+
				"exists with the same priority", desiredANPState.name, anp.Spec.Priority, existingName)
			c.eventRecorder.Eventf(&v1.ObjectReference{
				Kind: "AdminNetworkPolicy",
				Name: anp.Name,
			}, v1.EventTypeWarning, ANPWithDuplicatePriorityEvent, "This ANP %s has a conflicting priority with ANP %s:"+
				"Please verify your rules are non-lapping between all policies at same priority to avoid undefined behavior",
				anp.Name, existingName)
		} else {
			// Let us update the anpPriorityMap cache by adding this new priority to it
			// only if there wasn't an already existing entry at that priority - in which case we don't need to update the cache
			c.anpPriorityMap[desiredANPState.anpPriority] = anp.Name
		}
	}
	// since transact was successful we can finally replace the currentANPState in the cache with the latest desired one
	c.anpCache[anp.Name] = desiredANPState
	return nil
}

// convertANPRulesToACLs takes all the rules belonging to the ANP and initiates the conversion of rule->acl
// if currentANPState exists; then we also see if any of the current v/s desired ACLs had a state change
// and if so, we return atLeastOneRuleUpdated=true
func (c *Controller) convertANPRulesToACLs(desiredANPState, currentANPState *adminNetworkPolicyState, pgName string,
	atLeastOneRuleUpdated *bool, isBanp bool) []*nbdb.ACL {
	acls := []*nbdb.ACL{}
	// isAtLeastOneRuleUpdatedCheckRequired is set to true, if we had an anp already in cache (update) AND the rule lengths are the same
	// if the rule lengths are different we do a full peer recompute in ensureAdminNetworkPolicy anyways
	isAtLeastOneRuleUpdatedCheckRequired := (currentANPState != nil && currentANPState.name != "" &&
		len(currentANPState.ingressRules) == len(desiredANPState.ingressRules) &&
		len(currentANPState.egressRules) == len(desiredANPState.egressRules))
	for i, ingressRule := range desiredANPState.ingressRules {
		acl := c.convertANPRuleToACL(ingressRule, pgName, desiredANPState.name, desiredANPState.aclLoggingParams, isBanp)
		acls = append(acls, acl...)
		if isAtLeastOneRuleUpdatedCheckRequired &&
			!*atLeastOneRuleUpdated &&
			(ingressRule.action != currentANPState.ingressRules[i].action ||
				!reflect.DeepEqual(ingressRule.ports, currentANPState.ingressRules[i].ports) ||
				!reflect.DeepEqual(ingressRule.namedPorts, currentANPState.ingressRules[i].namedPorts)) {
			klog.V(3).Infof("ANP %s's ingress rule %s/%d at priority %d was updated", desiredANPState.name, ingressRule.name, i, ingressRule.priority)
			*atLeastOneRuleUpdated = true
		}
	}
	for i, egressRule := range desiredANPState.egressRules {
		acl := c.convertANPRuleToACL(egressRule, pgName, desiredANPState.name, desiredANPState.aclLoggingParams, isBanp)
		acls = append(acls, acl...)
		if isAtLeastOneRuleUpdatedCheckRequired &&
			!*atLeastOneRuleUpdated &&
			(egressRule.action != currentANPState.egressRules[i].action ||
				!reflect.DeepEqual(egressRule.ports, currentANPState.egressRules[i].ports) ||
				!reflect.DeepEqual(egressRule.namedPorts, currentANPState.egressRules[i].namedPorts)) {
			klog.V(3).Infof("ANP %s's egress rule %s/%d at priority %d was updated", desiredANPState.name, egressRule.name, i, egressRule.priority)
			*atLeastOneRuleUpdated = true
		}
	}

	return acls
}

// convertANPRuleToACL takes the given gressRule and converts it into an ACL(0 ports rule) or
// multiple ACLs(ports are set) and returns those ACLs for a given gressRule
func (c *Controller) convertANPRuleToACL(rule *gressRule, pgName, anpName string, aclLoggingParams *libovsdbutil.ACLLoggingLevels, isBanp bool) []*nbdb.ACL {
	klog.V(5).Infof("Creating ACL for rule %d/%s belonging to ANP %s", rule.priority, rule.gressPrefix, anpName)
	// create match based on direction and address-set name
	asIndex := GetANPPeerAddrSetDbIDs(anpName, rule.gressPrefix, fmt.Sprintf("%d", rule.gressIndex), c.controllerName, isBanp)
	l3Match := constructMatchFromAddressSet(rule.gressPrefix, asIndex)
	// create match based on rule type (ingress/egress) and port-group
	lportMatch := libovsdbutil.GetACLMatch(pgName, "", libovsdbutil.ACLDirection(rule.gressPrefix))
	var match string
	hasNamedPorts := len(rule.namedPorts) > 0
	acls := []*nbdb.ACL{}
	// We will have
	// - one single ACL if len(rule.ports) == 0 && len(rule.namedPorts) == 0
	// - one ACL per protocol if len(rule.ports) > 0 and len(rule.namedPorts) == 0
	//   (so max 3 ACLs (tcp,udp,sctp) per rule {portNumber, portRange type ports ONLY})
	// - one ACL per protocol if len(rule.namedPorts) > 0 and len(rule.ports) == 0
	//   (so max 3 ACLs (tcp,udp,sctp) per rule {namedPort type ports ONLY})
	// - one ACL per protocol if len(rule.ports) > 0 and one ACL per protocol if len(rule.namedPorts) > 0
	//   (so max 6 ACLs (2tcp,2udp,2sctp) per rule {{portNumber, portRange, namedPorts ALL PRESENT}})
	for protocol, l4Match := range libovsdbutil.GetL4MatchesFromNetworkPolicyPorts(rule.ports) {
		if l4Match == libovsdbutil.UnspecifiedL4Match {
			if hasNamedPorts {
				continue
			} // if we have namedPorts we shouldn't add the noneProtocol ACL even if the namedPort doesn't match any pods
			match = fmt.Sprintf("%s && %s", lportMatch, l3Match)
		} else {
			match = fmt.Sprintf("%s && %s && %s", lportMatch, l3Match, l4Match)
		}
		acl := libovsdbutil.BuildANPACL(
			getANPRuleACLDbIDs(anpName, rule.gressPrefix, fmt.Sprintf("%d", rule.gressIndex), protocol, c.controllerName, isBanp),
			int(rule.priority),
			match,
			rule.action,
			libovsdbutil.ACLDirectionToACLPipeline(libovsdbutil.ACLDirection(rule.gressPrefix)),
			aclLoggingParams,
		)
		acls = append(acls, acl)
	}
	// Process match for NamedPorts if any
	for protocol, l3l4Match := range libovsdbutil.GetL3L4MatchesFromNamedPorts(rule.namedPorts) {
		if rule.gressPrefix == string(libovsdbutil.ACLIngress) {
			match = fmt.Sprintf("%s && %s", l3Match, l3l4Match)
		} else {
			match = fmt.Sprintf("%s && %s", lportMatch, l3l4Match)
		}
		acl := libovsdbutil.BuildANPACL(
			getANPRuleACLDbIDs(anpName, rule.gressPrefix, fmt.Sprintf("%d", rule.gressIndex), protocol+libovsdbutil.NamedPortL4MatchSuffix, c.controllerName, isBanp),
			int(rule.priority),
			match,
			rule.action,
			libovsdbutil.ACLDirectionToACLPipeline(libovsdbutil.ACLDirection(rule.gressPrefix)),
			aclLoggingParams,
		)
		acls = append(acls, acl)
	}

	return acls
}

// expandANPRulePeers takes all the peers belonging to each of the ANP rule and initiates the conversion
// of rule.peer->set of addresses. These sets of addresses are then used to create the address-sets
func (c *Controller) expandANPRulePeers(anp *adminNetworkPolicyState) error {
	var err error
	for _, ingressRule := range anp.ingressRules {
		err = c.expandRulePeers(ingressRule) // namedPorts has to be processed for subject in case of ingress rules
		if err != nil {
			return fmt.Errorf("unable to create address set for "+
				" rule %s with priority %d: %w", ingressRule.name, ingressRule.priority, err)
		}
	}
	for _, egressRule := range anp.egressRules {
		err = c.expandRulePeers(egressRule)
		if err != nil {
			return fmt.Errorf("unable to create address set for "+
				" rule %s with priority %d: %w", egressRule.name, egressRule.priority, err)
		}
	}

	return nil
}

// expandRulePeers creates a set of peerAddresses for all the peers passed as argument
// This function also takes care of populating the adminNetworkPolicyPeer.namespaces cache
// It also adds up all the peerAddresses that are supposed to be present in the created AddressSet and returns them on
// a per-rule basis so that the actual ops to transact these into the AddressSet can be constructed using that
func (c *Controller) expandRulePeers(rule *gressRule) error {
	for _, peer := range rule.peers {
		namespaces, err := c.anpNamespaceLister.List(peer.namespaceSelector)
		if err != nil {
			return err
		}
		namespaceCache := make(map[string]sets.Set[string])
		// NOTE: Multiple peers may match on same podIP which is fine, we use sets to store them to avoid duplication
		for _, namespace := range namespaces {
			podCache, ok := namespaceCache[namespace.Name]
			if !ok {
				podCache = sets.Set[string]{}
				namespaceCache[namespace.Name] = podCache
			}
			podNamespaceLister := c.anpPodLister.Pods(namespace.Name)
			pods, err := podNamespaceLister.List(peer.podSelector)
			if err != nil {
				return err
			}
			for _, pod := range pods {
				// we don't handle HostNetworked or completed pods; unscheduled pods shall be handled via pod update path
				if util.PodWantsHostNetwork(pod) || util.PodCompleted(pod) || !util.PodScheduled(pod) {
					continue
				}
				podIPs, err := util.GetPodIPsOfNetwork(pod, &util.DefaultNetInfo{})
				if err != nil {
					if errors.Is(err, util.ErrNoPodIPFound) {
						// we ignore podIPsNotFound error here because onANPPodUpdate
						// will take care of this; no need to add nil podIPs to slice...
						// move on to next item in the loop
						continue
					}
					return err // we won't hit this TBH because the only error that GetPodIPsOfNetwork returns is podIPsNotFound
				}
				rule.peerAddresses.Insert(util.StringSlice(podIPs)...)
				podCache.Insert(pod.Name)
				// Process NamedPorts if any
				if len(rule.namedPorts) == 0 {
					continue
				}
				for _, container := range pod.Spec.Containers {
					for _, port := range container.Ports { // this loop is/might get expensive
						if port.Name == "" {
							continue
						}
						_, ok := rule.namedPorts[port.Name]
						if !ok {
							continue
						}
						rule.namedPorts[port.Name] = append(rule.namedPorts[port.Name], convertPodIPContainerPortToNNPP(port, podIPs)...)
					}
				}
			}
		}
		// we have to store the sorted slice here because in convertANPRulesToACLs
		// we use DeepEqual to compare ports which doesn't do well with unordered slices
		for _, v := range rule.namedPorts {
			sortNamedPorts(v)
		}
		peer.namespaces = namespaceCache
		nodes, err := c.anpNodeLister.List(peer.nodeSelector)
		if err != nil {
			return err
		}
		nodeCache := sets.New[string]()
		for _, node := range nodes {
			nodeIPs, err := util.GetNodeHostAddrs(node)
			if err != nil { // Annotation not found errors are ignored, they will come as node updates
				return err
			}
			rule.peerAddresses.Insert(nodeIPs...)
			nodeCache.Insert(node.Name)
		}
		peer.nodes = nodeCache
	}
	return nil
}

// convertANPSubjectToLSPs calculates all the LSP's that match for the provided anp's subject and returns them
// It also populates the adminNetworkPolicySubject.namespaces and adminNetworkPolicySubject.podPorts
// pieces of the cache
// Since we have to loop through all the pods here, we also take the opportunity to update our namedPorts cache
func (c *Controller) convertANPSubjectToLSPs(anp *adminNetworkPolicyState) ([]*nbdb.LogicalSwitchPort, error) {
	lsports := []*nbdb.LogicalSwitchPort{}
	anp.subject.podPorts = sets.Set[string]{}
	namespaces, err := c.anpNamespaceLister.List(anp.subject.namespaceSelector)
	if err != nil {
		return nil, err
	}
	// Process NamedPorts if any
	// a representation that stores name of the port as key and slice of indexes of rules
	// that match this namedPort as value
	namedPortMatchingRulesIndexes := map[string][]int{}
	for i, ingress := range anp.ingressRules {
		for name := range ingress.namedPorts {
			// we are creating a index based mapping representation so that ALL original rule.namedPorts
			// map across all ingress rules can be appended in place for every peer in the following peer loop
			// (otherwise we'd need to iterate over all ports always for all pods)
			namedPortMatchingRulesIndexes[name] = append(namedPortMatchingRulesIndexes[name], i)
		}
	}
	namespaceCache := make(map[string]sets.Set[string])
	for _, namespace := range namespaces {
		podCache, ok := namespaceCache[namespace.Name]
		if !ok {
			podCache = sets.Set[string]{}
			namespaceCache[namespace.Name] = podCache
		}
		podNamespaceLister := c.anpPodLister.Pods(namespace.Name)
		pods, err := podNamespaceLister.List(anp.subject.podSelector)
		if err != nil {
			return nil, err
		}
		for _, pod := range pods {
			if util.PodWantsHostNetwork(pod) || util.PodCompleted(pod) || !util.PodScheduled(pod) || !c.isPodScheduledinLocalZone(pod) {
				continue
			}
			logicalPortName := util.GetLogicalPortName(pod.Namespace, pod.Name)
			lsp := &nbdb.LogicalSwitchPort{Name: logicalPortName}
			lsp, err = libovsdbops.GetLogicalSwitchPort(c.nbClient, lsp)
			if err != nil {
				if errors.Is(err, libovsdbclient.ErrNotFound) {
					// NOTE(tssurya): Danger of doing this is if there is time gap between pod being annotated with chosen IP
					// and pod's LSP being created, then we might setup the policies only after pod goes into running state
					// thus causing little bit of outage
					// we ignore ErrNotFound error here because onANPPodUpdate (pod.status.Running)
					// will take care of this; no need to add nil podIPs to slice...
					// move on to next item in the loop
					// If not we are going to have many such pods if ANP and pods are created at the same time thus causing
					// ANP create to fail on a single pod add failure
					continue
				}
				return nil, fmt.Errorf("error retrieving logical switch port with name %s "+
					" from libovsdb cache: %w", logicalPortName, err)
			}
			lsports = append(lsports, lsp)
			anp.subject.podPorts.Insert(lsp.UUID)
			podCache.Insert(pod.Name)
			if len(namedPortMatchingRulesIndexes) == 0 {
				continue
			}
			// we need to collect podIP:cPort information
			podIPs, err := util.GetPodIPsOfNetwork(pod, &util.DefaultNetInfo{})
			if err != nil {
				if errors.Is(err, util.ErrNoPodIPFound) {
					// we ignore podIPsNotFound error here because onANPPodUpdate
					// will take care of this; no need to add nil podIPs to slice...
					// move on to next item in the loop
					continue
				}
				return nil, err // we won't hit this TBH because the only error that GetPodIPsOfNetwork returns is podIPsNotFound
			}
			for _, container := range pod.Spec.Containers {
				for _, port := range container.Ports { // this loop is/might get expensive
					if port.Name == "" {
						continue
					}
					namedPortRulesIndexes, ok := namedPortMatchingRulesIndexes[port.Name]
					if !ok {
						continue
					}
					for _, namedPortRuleIndex := range namedPortRulesIndexes {
						anp.ingressRules[namedPortRuleIndex].namedPorts[port.Name] = append(
							anp.ingressRules[namedPortRuleIndex].namedPorts[port.Name], convertPodIPContainerPortToNNPP(port, podIPs)...)
					}
				}
			}
		}
	}
	// we have to store the sorted slice here because in convertANPRulesToACLs
	// we use DeepEqual to compare ports which doesn't do well with unordered slices
	for _, iRule := range anp.ingressRules {
		for _, namedPortReps := range iRule.namedPorts {
			sortNamedPorts(namedPortReps)
		}
	}
	anp.subject.namespaces = namespaceCache

	return lsports, nil
}

// clearAdminNetworkPolicy will handle the logic for deleting all db objects related
// to the provided anp which got deleted.
// uses externalIDs to figure out ownership
func (c *Controller) clearAdminNetworkPolicy(anpName string) error {
	// See if we need to handle this: https://github.com/ovn-org/ovn-kubernetes/pull/3659#discussion_r1284645817
	anp, loaded := c.anpCache[anpName]
	if !loaded {
		// there is no existing ANP configured with this name, nothing to clean
		klog.Infof("ANP %s not found in cache, nothing to clear", anpName)
		return nil
	}

	// clear NBDB objects for the given ANP (PG, ACLs on that PG, AddrSets used by the ACLs)
	var err error
	// remove PG for Subject (ACLs will get cleaned up automatically)
	portGroupName := c.getANPPortGroupName(anp.name, false)
	// no need to batch this with address-set deletes since this itself will contain a bunch of ACLs that need to be deleted which is heavy enough.
	err = libovsdbops.DeletePortGroups(c.nbClient, portGroupName)
	if err != nil {
		return fmt.Errorf("unable to delete PG %s for ANP %s: %w", portGroupName, anp.name, err)
	}
	// remove address-sets that were created for the peers of each rule fpr the whole ANP
	// do this after ACLs are gone so that there is no lingering references
	err = c.clearASForPeers(anp.name, libovsdbops.AddressSetAdminNetworkPolicy)
	if err != nil {
		return fmt.Errorf("failed to delete address-sets for ANP %s/%d: %w", anp.name, anp.anpPriority, err)
	}
	// we can delete the object from the cache now.
	if existingName, loaded := c.anpPriorityMap[anp.anpPriority]; loaded && existingName == anpName {
		delete(c.anpPriorityMap, anp.anpPriority)
	}
	delete(c.anpCache, anpName)
	metrics.DecrementANPCount()

	return nil
}

// clearASForPeers takes the externalID objectIDs and uses them to delete all the address-sets
// that were owned by anpName
func (c *Controller) clearASForPeers(anpName string, idType *libovsdbops.ObjectIDsType) error {
	predicateIDs := libovsdbops.NewDbObjectIDs(idType, c.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: anpName,
		})
	asPredicate := libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil)
	if err := libovsdbops.DeleteAddressSetsWithPredicate(c.nbClient, asPredicate); err != nil {
		return fmt.Errorf("failed to destroy address-set for ANP %s, err: %v", anpName, err)
	}
	return nil
}

// createNewANP takes the desired state of the anp and creates the corresponding objects in the NBDB
func (c *Controller) createNewANP(desiredANPState *adminNetworkPolicyState, desiredACLs []*nbdb.ACL,
	desiredPorts []*nbdb.LogicalSwitchPort, isBanp bool) error {
	ops := []ovsdb.Operation{}

	// now CreateOrUpdate the address-sets; add the right IPs - we treat the rest of the address-set cases as a fresh add or update
	addrSetOps, err := c.constructOpsForRuleChanges(desiredANPState, isBanp)
	if err != nil {
		return fmt.Errorf("failed to create address-sets, %v", err)
	}
	ops = append(ops, addrSetOps...)
	ops, err = libovsdbops.CreateOrUpdateACLsOps(c.nbClient, ops, c.GetSamplingConfig(), desiredACLs...)
	if err != nil {
		return fmt.Errorf("failed to create ACL ops: %v", err)
	}
	pgDbIDs := GetANPPortGroupDbIDs(desiredANPState.name, isBanp, c.controllerName)
	pg := libovsdbutil.BuildPortGroup(pgDbIDs, desiredPorts, desiredACLs)
	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(c.nbClient, ops, pg)
	if err != nil {
		return fmt.Errorf("failed to create ops to add port to a port group: %v", err)
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to run ovsdb txn to add ports to port group: %v", err)
	}
	return nil
}

func (c *Controller) updateExistingANP(currentANPState, desiredANPState *adminNetworkPolicyState, atLeastOneRuleUpdated,
	hasPriorityChanged, isBanp bool, desiredACLs []*nbdb.ACL) error {
	var ops []ovsdb.Operation
	var err error
	portGroupName := c.getANPPortGroupName(desiredANPState.name, isBanp)
	// Did ANP.Spec.Ingress Change (rule inserts/deletes)? && || Did ANP.Spec.Egress Change (rule inserts/deletes)? && ||
	// If yes we need to fully recompute the acls present in our ANP's port group; Let's do a full recompute and return.
	// Reason behind a full recompute: Each rule has precedence based on its position and priority of ANP; if any of that changes
	// better to delete and recreate ACLs rather than figure out from caches
	// rather than always cleaning up everything and recreating them. But this is tricky since rules have precedence
	// from their ordering.
	// NOTE: Changes to admin policies should be a rare action (can be improved post user feedback) - usually churn would be around namespaces and pods
	fullPeerRecompute := (len(currentANPState.egressRules) != len(desiredANPState.egressRules) ||
		len(currentANPState.ingressRules) != len(desiredANPState.ingressRules))
	if fullPeerRecompute {
		// full recompute
		// which means update all ACLs and address-sets
		klog.V(3).Infof("ANP %s with priority (old %d, new %d) was updated", desiredANPState.name, currentANPState.anpPriority, desiredANPState.anpPriority)
		ops, err = c.constructOpsForRuleChanges(desiredANPState, isBanp)
		if err != nil {
			return fmt.Errorf("failed to create update ANP ops %s: %v", desiredANPState.name, err)
		}
	}

	// Did ANP.Spec.Ingress rules get updated?
	// (at this stage the length of ANP.Spec.Ingress hasn't changed, so individual rules either got updated at their values or positions are switched)
	// The fields that we care about for rebuilding ACLs are
	// i) `ports` (ii) `actions` (iii) `priority` for a given rule
	// The changes to peer labels, peer pod label updates, namespace label updates, CIDR peer updates etc can be inferred
	// from the peerAddresses cache we store.
	// Did the ANP.Spec.Ingress.Peers Change?
	// 1) ANP.Spec.Ingress.Peers.Namespaces changed && ||
	// 2) ANP.Spec.Ingress.Peers.Pods changed && ||
	// 3) A namespace started or stopped matching the peer && ||
	// 4) A pod started or stopped matching the peer
	// If yes we need to recompute the IPs present in our ANP's peer's address-sets
	if !fullPeerRecompute && !reflect.DeepEqual(desiredANPState.ingressRules, currentANPState.ingressRules) {
		addrOps, err := c.constructOpsForPeerChanges(desiredANPState.ingressRules,
			currentANPState.ingressRules, desiredANPState.name, isBanp)
		if err != nil {
			return fmt.Errorf("failed to create ops for changes to ANP ingress peers: %v", err)
		}
		ops = append(ops, addrOps...)
	}

	// Did ANP.Spec.Egress rules get updated?
	// (at this stage the length of ANP.Spec.Egress hasn't changed, so individual rules either got updated at their values or positions are switched)
	// The fields that we care about for rebuilding ACLs are
	// i) `ports` (ii) `actions` (iii) `priority` for a given rule
	// The changes to peer labels, peer pod label updates, namespace label updates, CIDR peer updates etc can be inferred
	// from the peerAddresses cache we store.
	// Did the ANP.Spec.Egress.Peers Change?
	// 1) ANP.Spec.Egress.Peers.Namespaces changed && ||
	// 2) ANP.Spec.Egress.Peers.Pods changed && ||
	// 3) ANP.Spec.Egress.Peers.Nodes changed && ||
	// 4) A namespace started or stopped matching the peer && ||
	// 5) A pod started or stopped matching the peer && ||
	// 6) A node started or stopped matching the peer
	// If yes we need to recompute the IPs present in our ANP's peer's address-sets
	if !fullPeerRecompute && !reflect.DeepEqual(desiredANPState.egressRules, currentANPState.egressRules) {
		addrOps, err := c.constructOpsForPeerChanges(desiredANPState.egressRules,
			currentANPState.egressRules, desiredANPState.name, isBanp)
		if err != nil {
			return fmt.Errorf("failed to create ops for changes to ANP egress peers: %v", err)
		}
		ops = append(ops, addrOps...)
	}
	hasACLLoggingParamsChanged := currentANPState.aclLoggingParams.Allow != desiredANPState.aclLoggingParams.Allow ||
		currentANPState.aclLoggingParams.Deny != desiredANPState.aclLoggingParams.Deny
	if !isBanp {
		hasACLLoggingParamsChanged = hasACLLoggingParamsChanged || currentANPState.aclLoggingParams.Pass != desiredANPState.aclLoggingParams.Pass
	}
	// The rules which didn't change -> those updates will be no-ops thanks to libovsdb
	// The rules that changed in terms of their `getACLMutableFields`
	// will be simply updated since externalIDs will remain the same for these ACLs
	// No delete ACLs action is required for this scenario
	// the stale ACLs will automatically be taken care of if they are not references by the port group
	// (1) fullPeerRecompute=true which means the rules were of different lengths (involved deletion or appending of gress rules)
	// (2) atLeastOneRuleUpdated=true which means the gress rules were of same lengths but action or ports changed on at least one rule
	// (3) hasPriorityChanged=true which means we should update acl.Priority for every ACL
	// (4) hasACLLoggingParamsChanged=true which means we should update acl.Severity/acl.Log for every ACL
	if fullPeerRecompute || atLeastOneRuleUpdated || hasPriorityChanged || hasACLLoggingParamsChanged {
		klog.V(3).Infof("ANP %s with priority %d was updated", desiredANPState.name, desiredANPState.anpPriority)
		// now update the acls to the desired ones
		ops, err = libovsdbops.CreateOrUpdateACLsOps(c.nbClient, ops, c.GetSamplingConfig(), desiredACLs...)
		if err != nil {
			return fmt.Errorf("failed to create new ACL ops for anp %s: %v", desiredANPState.name, err)
		}
		// since we update the portgroup with the new set of ACLs, any unreferenced set of ACLs
		// will be automatically removed
		ops, err = libovsdbops.UpdatePortGroupSetACLsOps(c.nbClient, ops, portGroupName, desiredACLs)
		if err != nil {
			return fmt.Errorf("failed to create ACL-on-PG update ops for anp %s: %v", desiredANPState.name, err)
		}
	}

	// Did the ANP.Spec.Subject Change?
	// 1) ANP.Spec.Namespaces changed && ||
	// 2) ANP.Spec.Pods changed && ||
	// 3) A namespace started or stopped matching the subject && ||
	// 4) A pod started or stopped matching the subject
	// If yes we need to recompute the ports present in our ANP's port group
	subjectOps, err := c.constructOpsForSubjectChanges(currentANPState, desiredANPState, portGroupName)
	if err != nil {
		return fmt.Errorf("failed to create ops for changes to ANP %s subject: %v", desiredANPState.name, err)
	}
	ops = append(ops, subjectOps...)
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to run ovsdb txn to update ANP %s: %v", desiredANPState.name, err)
	}
	return nil
}

// constructOpsForRuleChanges takes the desired state of the anp and returns the corresponding ops for updating NBDB objects
func (c *Controller) constructOpsForRuleChanges(desiredANPState *adminNetworkPolicyState, isBanp bool) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error
	// Logic to delete address-sets:
	// we need to delete address-sets only if the number of rules in the desiredANPState object is
	// less than the number of rules in the currentANPState object (AddressSet indexes are calculated based on rule's position)
	// rest of the cases will be createorupdate of existing existing address-sets in the cluster
	idType := libovsdbops.AddressSetAdminNetworkPolicy
	if isBanp {
		idType = libovsdbops.AddressSetBaselineAdminNetworkPolicy
	}
	predicateIDs := libovsdbops.NewDbObjectIDs(idType, c.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: desiredANPState.name,
		})
	predicateFunc := func(as *nbdb.AddressSet) bool {
		asIndex, _ := strconv.Atoi(as.ExternalIDs[libovsdbops.GressIdxKey.String()])
		return (as.ExternalIDs[libovsdbops.PolicyDirectionKey.String()] == string(libovsdbutil.ACLEgress) &&
			asIndex >= len(desiredANPState.egressRules)) ||
			(as.ExternalIDs[libovsdbops.PolicyDirectionKey.String()] == string(libovsdbutil.ACLIngress) &&
				asIndex >= len(desiredANPState.ingressRules))
	}
	asPredicate := libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, predicateFunc)
	ops, err = libovsdbops.DeleteAddressSetsWithPredicateOps(c.nbClient, ops, asPredicate)
	if err != nil {
		return nil, fmt.Errorf("failed to create address-set destroy ops for ANP %s, err: %v", desiredANPState.name, err)
	}
	// TODO (tssurya): Revisit this logic to see if its better to do one address-set per peer instead of one address-set for all peers
	// Had briefly discussed this OVN team. We are not yet clear which is better since both have advantages and disadvantages.
	// Decide this after doing some scale runs.
	for _, rule := range desiredANPState.ingressRules {
		asIndex := GetANPPeerAddrSetDbIDs(desiredANPState.name, rule.gressPrefix, fmt.Sprintf("%d", rule.gressIndex), c.controllerName, isBanp)
		_, addrSetOps, err := c.addressSetFactory.NewAddressSetOps(asIndex, rule.peerAddresses.UnsortedList())
		if err != nil {
			return nil, fmt.Errorf("failed to create address-sets for ANP %s's"+
				" ingress rule %s/%s/%d: %v", desiredANPState.name, rule.name, rule.gressPrefix, rule.priority, err)
		}
		ops = append(ops, addrSetOps...)
	}
	for _, rule := range desiredANPState.egressRules {
		asIndex := GetANPPeerAddrSetDbIDs(desiredANPState.name, rule.gressPrefix, fmt.Sprintf("%d", rule.gressIndex), c.controllerName, isBanp)
		_, addrSetOps, err := c.addressSetFactory.NewAddressSetOps(asIndex, rule.peerAddresses.UnsortedList())
		if err != nil {
			return nil, fmt.Errorf("failed to create address-sets for ANP %s's"+
				" egress rule %s/%s/%d: %v", desiredANPState.name, rule.name, rule.gressPrefix, rule.priority, err)
		}
		ops = append(ops, addrSetOps...)
	}
	return ops, nil
}

// constructOpsForPeerChanges takes the desired and current rules of the anp and returns the corresponding ops
// for updating NBDB AddressSet objects for those peers
// This should be called if namespace/pod is being created/updated
func (c *Controller) constructOpsForPeerChanges(desiredRules, currentRules []*gressRule,
	anpName string, isBanp bool) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	for i := range desiredRules {
		desiredRule := desiredRules[i]
		currentRule := currentRules[i]
		addressesToAdd := desiredRule.peerAddresses.Difference(currentRule.peerAddresses)
		asIndex := GetANPPeerAddrSetDbIDs(anpName, desiredRule.gressPrefix, fmt.Sprintf("%d", desiredRule.gressIndex), c.controllerName, isBanp)
		if len(addressesToAdd) > 0 {
			as, err := c.addressSetFactory.GetAddressSet(asIndex)
			if err != nil {
				return nil, fmt.Errorf("cannot ensure that addressSet %+v exists: err %v", asIndex.GetExternalIDs(), err)
			}
			klog.V(5).Infof("Adding peerAddresses %+v to address-set %s for ANP %s", addressesToAdd, as.GetName(), anpName)
			addrOps, err := as.AddAddressesReturnOps(addressesToAdd.UnsortedList())
			if err != nil {
				return nil, fmt.Errorf("failed to construct address-set %s's IP add ops for anp %s's rule"+
					" %s/%s/%d: %v", as.GetName(), anpName, desiredRule.name,
					desiredRule.gressPrefix, desiredRule.priority, err)
			}
			ops = append(ops, addrOps...)
		}
		addressesToRemove := currentRule.peerAddresses.Difference(desiredRule.peerAddresses)
		if len(addressesToRemove) > 0 {
			as, err := c.addressSetFactory.GetAddressSet(asIndex)
			if err != nil {
				return nil, fmt.Errorf("cannot ensure that addressSet %+v exists: err %v", asIndex.GetExternalIDs(), err)
			}
			klog.V(5).Infof("Deleting peerAddresses %+v from address-set %s for ANP %s", addressesToRemove, as.GetName(), anpName)
			addrOps, err := as.DeleteAddressesReturnOps(addressesToRemove.UnsortedList())
			if err != nil {
				return nil, fmt.Errorf("failed to construct address-set %s's IP delete ops for anp %s's rule"+
					" %s/%s/%d: %v", as.GetName(), anpName, desiredRule.name,
					desiredRule.gressPrefix, desiredRule.priority, err)
			}
			ops = append(ops, addrOps...)
		}
	}
	return ops, nil
}

// constructOpsForSubjectChanges takes the current and desired cache states for a given ANP and returns the ops
// required to construct the transact to insert/delete ports to/from port-groups according to the ANP subject changes
func (c *Controller) constructOpsForSubjectChanges(currentANPState, desiredANPState *adminNetworkPolicyState, portGroupName string) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error
	portsToAdd := desiredANPState.subject.podPorts.Difference(currentANPState.subject.podPorts).UnsortedList()
	portsToDelete := currentANPState.subject.podPorts.Difference(desiredANPState.subject.podPorts).UnsortedList()
	if len(portsToAdd) > 0 {
		klog.V(5).Infof("Adding ports %+v to port-group %s for ANP %s", portsToAdd, portGroupName, desiredANPState.name)
		ops, err = libovsdbops.AddPortsToPortGroupOps(c.nbClient, ops, portGroupName, portsToAdd...)
		if err != nil {
			return nil, fmt.Errorf("failed to create Port-to-PG add ops for anp %s: %v", desiredANPState.name, err)
		}
	}
	if len(portsToDelete) > 0 {
		klog.V(5).Infof("Deleting ports %+v from port-group %s for ANP %s", portsToDelete, portGroupName, desiredANPState.name)
		ops, err = libovsdbops.DeletePortsFromPortGroupOps(c.nbClient, ops, portGroupName, portsToDelete...)
		if err != nil {
			return nil, fmt.Errorf("failed to create Port-from-PG delete ops for anp %s: %v", desiredANPState.name, err)
		}
	}
	return ops, nil
}
