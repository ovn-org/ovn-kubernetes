package admin_network_policy

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type mapOp int

const (
	mapInsert mapOp = iota
	mapDelete
)

type mapAndOp struct {
	m   *sync.Map
	key string
	val any
	op  mapOp
}

func (c *Controller) syncNamespaceAdminNetworkPolicy(key string) error {
	startTime := time.Now()
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(4).Infof("Processing sync for Namespace %s in Admin Network Policy controller", name)

	defer func() {
		klog.V(4).Infof("Finished syncing Namespace %s Admin Network Policy controller: took %v", name, time.Since(startTime))
	}()
	namespace, err := c.anpNamespaceLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	existingANPs, err := c.anpLister.List(labels.Everything())
	if err != nil {
		return err
	}
	// On namespace deletion we remove pods from relevant port-groups, address-sets and update the anp and banp caches
	if namespace == nil {
		for _, anp := range existingANPs {
			anpObj, loaded := c.anpCache.Load(anp.Name)
			if !loaded {
				continue
			}
			anpCache := anpObj.(*adminNetworkPolicy)
			if err := c.clearNamespaceForANP(name, anpCache); err != nil {
				return fmt.Errorf("could not sync ANP %s when deleting namespace %s, err %v", anpCache.name, name, err)
			}
		}
		banpObj, loaded := c.banpCache.Load(BANPFlowPriority)
		if !loaded {
			return nil // nothing to do
		}
		banpCache := banpObj.(*baselineAdminNetworkPolicy)
		// remove namespace configuration from BANP
		if err := c.clearNamespaceForBANP(name, banpCache); err != nil {
			return fmt.Errorf("could not sync BANP %s when deleting namespace %s, err %v", banpCache.name, name, err)
		}
		return nil
	}
	podNamespaceLister := c.anpPodLister.Pods(name)
	pods, err := podNamespaceLister.List(labels.Everything())
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// Iterate all ANPs and check if this namespace start/stops matching
	// any and add/remove the setup accordingly. Namespaces can match multiple
	// ANPs objects (subject and peers), so continue iterating all ANP
	// objects before finishing.
	for _, anp := range existingANPs {
		anpObj, loaded := c.anpCache.Load(anp.Name)
		if !loaded {
			continue
		}
		anpCache := anpObj.(*adminNetworkPolicy)
		err := c.setNamespaceForANP(anp.Name, namespace, anpCache, pods)
		if err != nil {
			return fmt.Errorf("could not sync ANP %s for namespace %s, err %v", anp.Name, name, err)
		}
	}
	banpObj, loaded := c.banpCache.Load(BANPFlowPriority)
	if !loaded {
		return nil // nothing to do
	}
	banpCache := banpObj.(*baselineAdminNetworkPolicy)
	err = c.setNamespaceForBANP(namespace, banpCache, pods)
	if err != nil {
		return fmt.Errorf("could not sync BANP %s for namespace %s, err %v", banpCache.name, name, err)
	}
	return nil
}

func (c *Controller) setNamespaceForANP(anpName string, namespace *v1.Namespace, anpCache *adminNetworkPolicy, pods []*v1.Pod) error {
	podMapOps := []mapAndOp{}
	anpCache.RLock()
	defer anpCache.RUnlock()
	if anpCache.stale { // was deleted or not created properly
		return nil
	}
	var portsToAdd, portsToDelete []string
	ops := []ovsdb.Operation{}
	var err error
	namespaceLabels := labels.Set(namespace.Labels)
	for _, pod := range pods {
		podLabels := labels.Set(pod.Labels)
		logicalPortName := util.GetLogicalPortName(pod.Namespace, pod.Name)
		lsp := &nbdb.LogicalSwitchPort{Name: logicalPortName}
		lsp, err = libovsdbops.GetLogicalSwitchPort(c.nbClient, lsp)
		if err != nil {
			return fmt.Errorf("error retrieving logical switch port with Name %s "+
				" from libovsdb cache: %w", logicalPortName, err)
		}
		podIPs, err := util.GetPodIPsOfNetwork(pod, &util.DefaultNetInfo{})
		if err != nil {
			return fmt.Errorf("error retrieving IPs for pod %s/%s: err %v", pod.Namespace, pod.Name, err)
		}
		nsCache, loaded := anpCache.subject.pods.Load(namespace.Name)
		if loaded {
			_, loaded = nsCache.(*sync.Map).Load(pod.Name)
		}
		if !loaded && anpCache.subject.namespaceSelector.Matches(namespaceLabels) && anpCache.subject.podSelector.Matches(podLabels) {
			// pod was not part of subject but now its namespace selectors have started matching;
			// so we need to add the necessary plumbing if podSelectors also match
			nsCache, _ = anpCache.subject.pods.LoadOrStore(namespace.Name, &sync.Map{}) // create new map if entry was nil
			portsToAdd = append(portsToAdd, lsp.UUID)
			podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, lsp.UUID, mapInsert})
		} else if loaded && !anpCache.subject.namespaceSelector.Matches(namespaceLabels) {
			// pod was a part of subject but now its namespace selectors have stopped matching;
			// so we need to remove the necessary plumbing (no need to check for pod labels)
			portsToDelete = append(portsToDelete, lsp.UUID)
			podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, lsp.UUID, mapDelete})
		}
		pgName, _ := getAdminNetworkPolicyPGName(anpCache.name)
		if len(portsToAdd) > 0 {
			ops, err = libovsdbops.AddPortsToPortGroupOps(c.nbClient, ops, pgName, portsToAdd...)
			if err != nil {
				return err
			}
		}
		if len(portsToDelete) > 0 {
			ops, err = libovsdbops.DeletePortsFromPortGroupOps(c.nbClient, ops, pgName, portsToDelete...)
			if err != nil {
				return err
			}
		}
		for _, rule := range anpCache.ingressRules {
			for _, peer := range rule.peers {
				nsCache, loaded := peer.pods.Load(namespace.Name)
				if loaded {
					_, loaded = nsCache.(*sync.Map).Load(pod.Name)
				}
				if !loaded && peer.namespaceSelector.Matches(namespaceLabels) && peer.podSelector.Matches(podLabels) {
					// pod was not part of peer but now its namespace selectors have started matching;
					// so we need to add the necessary plumbing if podSelectors also match
					nsCache, _ = peer.pods.LoadOrStore(namespace.Name, &sync.Map{}) // create new map if entry was nil
					addrOps, err := rule.addrSet.AddIPsReturnOps(podIPs)
					if err != nil {
						return err
					}
					ops = append(ops, addrOps...)
					podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapInsert})
				} else if loaded && !peer.namespaceSelector.Matches(namespaceLabels) {
					// pod was a part of peer but now its namespace selectors have stopped matching;
					// so we need to remove the necessary plumbing (no need to check for pod labels)
					addrOps, err := rule.addrSet.DeleteIPsReturnOps(podIPs)
					if err != nil {
						return err
					}
					ops = append(ops, addrOps...)
					podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapDelete})
				}
			}
		}
		for _, rule := range anpCache.egressRules {
			for _, peer := range rule.peers {
				nsCache, loaded := peer.pods.Load(namespace.Name)
				if loaded {
					_, loaded = nsCache.(*sync.Map).Load(pod.Name)
				}
				if !loaded && peer.namespaceSelector.Matches(namespaceLabels) && peer.podSelector.Matches(podLabels) {
					// pod was not part of peer but now its namespace selectors have started matching;
					// so we need to add the necessary plumbing if podSelectors also match
					nsCache, _ = peer.pods.LoadOrStore(namespace.Name, &sync.Map{}) // create new map if entry was nil
					addrOps, err := rule.addrSet.AddIPsReturnOps(podIPs)
					if err != nil {
						return err
					}
					ops = append(ops, addrOps...)
					podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapInsert})
				} else if loaded && !peer.namespaceSelector.Matches(namespaceLabels) {
					// pod was a part of peer but now its namespace selectors have stopped matching;
					// so we need to remove the necessary plumbing (no need to check for pod labels)
					addrOps, err := rule.addrSet.DeleteIPsReturnOps(podIPs)
					if err != nil {
						return err
					}
					ops = append(ops, addrOps...)
					podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapDelete})
				}
			}
		}

	}
	if len(ops) == 0 {
		return nil
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return err
	}

	for _, mapOp := range podMapOps {
		switch mapOp.op {
		case mapInsert:
			mapOp.m.Store(mapOp.key, mapOp.val)
		case mapDelete:
			mapOp.m.Delete(mapOp.key)
		}
	}
	return nil
}

func (c *Controller) setNamespaceForBANP(namespace *v1.Namespace, banpCache *baselineAdminNetworkPolicy, pods []*v1.Pod) error {
	podMapOps := []mapAndOp{}
	banpCache.RLock()
	defer banpCache.RUnlock()
	if banpCache.stale { // was deleted or not created properly
		return nil
	}
	var portsToAdd, portsToDelete []string
	ops := []ovsdb.Operation{}
	var err error
	namespaceLabels := labels.Set(namespace.Labels)
	for _, pod := range pods {
		podLabels := labels.Set(pod.Labels)
		logicalPortName := util.GetLogicalPortName(pod.Namespace, pod.Name)
		lsp := &nbdb.LogicalSwitchPort{Name: logicalPortName}
		lsp, err = libovsdbops.GetLogicalSwitchPort(c.nbClient, lsp)
		if err != nil {
			return fmt.Errorf("error retrieving logical switch port with Name %s "+
				" from libovsdb cache: %w", logicalPortName, err)
		}
		podIPs, err := util.GetPodIPsOfNetwork(pod, &util.DefaultNetInfo{})
		if err != nil {
			return fmt.Errorf("error retrieving IPs for pod %s/%s: err %v", pod.Namespace, pod.Name, err)
		}
		nsCache, loaded := banpCache.subject.pods.Load(namespace.Name)
		if loaded {
			_, loaded = nsCache.(*sync.Map).Load(pod.Name)
		}
		if !loaded && banpCache.subject.namespaceSelector.Matches(namespaceLabels) && banpCache.subject.podSelector.Matches(podLabels) {
			// pod was not part of subject but now its namespace selectors have started matching;
			// so we need to add the necessary plumbing if podSelectors also match
			nsCache, _ = banpCache.subject.pods.LoadOrStore(namespace.Name, &sync.Map{}) // create new map if entry was nil
			portsToAdd = append(portsToAdd, lsp.UUID)
			podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, lsp.UUID, mapInsert})
		} else if loaded && !banpCache.subject.namespaceSelector.Matches(namespaceLabels) {
			// pod was a part of subject but now its namespace selectors have stopped matching;
			// so we need to remove the necessary plumbing (no need to check for pod labels)
			portsToDelete = append(portsToDelete, lsp.UUID)
			podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, lsp.UUID, mapDelete})
		}
		pgName, _ := getBaselineAdminNetworkPolicyPGName(banpCache.name)
		if len(portsToAdd) > 0 {
			ops, err = libovsdbops.AddPortsToPortGroupOps(c.nbClient, ops, pgName, portsToAdd...)
			if err != nil {
				return err
			}
		}
		if len(portsToDelete) > 0 {
			ops, err = libovsdbops.DeletePortsFromPortGroupOps(c.nbClient, ops, pgName, portsToDelete...)
			if err != nil {
				return err
			}
		}
		for _, rule := range banpCache.ingressRules {
			for _, peer := range rule.peers {
				nsCache, loaded := peer.pods.Load(namespace.Name)
				if loaded {
					_, loaded = nsCache.(*sync.Map).Load(pod.Name)
				}
				if !loaded && peer.namespaceSelector.Matches(namespaceLabels) && peer.podSelector.Matches(podLabels) {
					// pod was not part of peer but now its namespace selectors have started matching;
					// so we need to add the necessary plumbing if podSelectors also match
					nsCache, _ = peer.pods.LoadOrStore(namespace.Name, &sync.Map{}) // create new map if entry was nil
					addrOps, err := rule.addrSet.AddIPsReturnOps(podIPs)
					if err != nil {
						return err
					}
					ops = append(ops, addrOps...)
					podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapInsert})
				} else if loaded && !peer.namespaceSelector.Matches(namespaceLabels) {
					// pod was a part of peer but now its namespace selectors have stopped matching;
					// so we need to remove the necessary plumbing (no need to check for pod labels)
					addrOps, err := rule.addrSet.DeleteIPsReturnOps(podIPs)
					if err != nil {
						return err
					}
					ops = append(ops, addrOps...)
					podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapDelete})
				}
			}
		}
		for _, rule := range banpCache.egressRules {
			for _, peer := range rule.peers {
				nsCache, loaded := peer.pods.Load(namespace.Name)
				if loaded {
					_, loaded = nsCache.(*sync.Map).Load(pod.Name)
				}
				if !loaded && peer.namespaceSelector.Matches(namespaceLabels) && peer.podSelector.Matches(podLabels) {
					// pod was not part of peer but now its namespace selectors have started matching;
					// so we need to add the necessary plumbing if podSelectors also match
					nsCache, _ = peer.pods.LoadOrStore(namespace.Name, &sync.Map{}) // create new map if entry was nil
					addrOps, err := rule.addrSet.AddIPsReturnOps(podIPs)
					if err != nil {
						return err
					}
					ops = append(ops, addrOps...)
					podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapInsert})
				} else if loaded && !peer.namespaceSelector.Matches(namespaceLabels) {
					// pod was a part of peer but now its namespace selectors have stopped matching;
					// so we need to remove the necessary plumbing (no need to check for pod labels)
					addrOps, err := rule.addrSet.DeleteIPsReturnOps(podIPs)
					if err != nil {
						return err
					}
					ops = append(ops, addrOps...)
					podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapDelete})
				}
			}
		}

	}
	if len(ops) == 0 {
		return nil
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return err
	}

	for _, mapOp := range podMapOps {
		switch mapOp.op {
		case mapInsert:
			mapOp.m.Store(mapOp.key, mapOp.val)
		case mapDelete:
			mapOp.m.Delete(mapOp.key)
		}
	}
	return nil
}

func (c *Controller) clearNamespaceForANP(name string, anpCache *adminNetworkPolicy) error {
	allops := []ovsdb.Operation{}
	var err error
	anpCache.RLock()
	defer anpCache.RUnlock()
	if podsCache, ok := anpCache.subject.pods.Load(name); ok {
		var portsToDelete []string
		pgName, _ := getAdminNetworkPolicyPGName(anpCache.name)
		podsCache.(*sync.Map).Range(func(key, value any) bool {
			portsToDelete = append(portsToDelete, value.(string))
			return true
		})
		allops, err = libovsdbops.DeletePortsFromPortGroupOps(c.nbClient, allops, pgName, portsToDelete...)
		if err != nil {
			return fmt.Errorf("unable to form delete port group ops for PG: %s "+
				"when trying to delete namespace %s, err: %v", pgName, name, err)
		}
		klog.Infof("SURYA %v", allops)
	}
	podsCaches := []*sync.Map{}
	for _, rule := range anpCache.ingressRules {
		var ipsToDelete []net.IP
		for _, peer := range rule.peers {
			podsCache, ok := peer.pods.Load(name)
			if !ok {
				continue
			}
			podsCache.(*sync.Map).Range(func(key, value any) bool {
				ips := value.([]net.IP)
				ipsToDelete = append(ipsToDelete, ips...)
				return true
			})
			podsCaches = append(podsCaches, peer.pods)
		}
		ops, err := rule.addrSet.DeleteIPsReturnOps(ipsToDelete)
		if err != nil {
			return fmt.Errorf("unable to form delete IPs from address-set ops for ingress rule with priority %v "+
				"when trying to delete namespace %s, err: %v", rule.priority, name, err)
		}
		allops = append(allops, ops...)
	}
	for _, rule := range anpCache.egressRules {
		var ipsToDelete []net.IP
		for _, peer := range rule.peers {
			podsCache, ok := peer.pods.Load(name)
			if !ok {
				continue
			}
			podsCache.(*sync.Map).Range(func(key, value any) bool {
				ips := value.([]net.IP)
				ipsToDelete = append(ipsToDelete, ips...)
				return true
			})
			podsCaches = append(podsCaches, peer.pods)
		}
		ops, err := rule.addrSet.DeleteIPsReturnOps(ipsToDelete)
		if err != nil {
			return fmt.Errorf("unable to form delete IPs from address-set ops for egress rule with priority %v "+
				"when trying to delete namespace %s, err: %v", rule.priority, name, err)
		}
		allops = append(allops, ops...)
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, allops)
	if err != nil {
		return fmt.Errorf("unable to transact deletion ops for ANP %s "+
			"when trying to delete namespace %s, err: %v", anpCache.name, name, err)
	}
	// finally delete the namespace key from cache
	anpCache.subject.pods.Delete(name)
	for _, pc := range podsCaches {
		pc.Delete(name)
	}
	return nil
}

func (c *Controller) clearNamespaceForBANP(name string, banpCache *baselineAdminNetworkPolicy) error {
	allops := []ovsdb.Operation{}
	var err error
	banpCache.RLock()
	defer banpCache.RUnlock()
	if podsCache, ok := banpCache.subject.pods.Load(name); ok {
		var portsToDelete []string
		pgName, _ := getBaselineAdminNetworkPolicyPGName(banpCache.name)
		podsCache.(*sync.Map).Range(func(key, value any) bool {
			portsToDelete = append(portsToDelete, value.(string))
			return true
		})
		allops, err = libovsdbops.DeletePortsFromPortGroupOps(c.nbClient, allops, pgName, portsToDelete...)
		if err != nil {
			return fmt.Errorf("unable to form delete port group ops for PG: %s "+
				"when trying to delete namespace %s, err: %v", pgName, name, err)
		}
	}
	podsCaches := []*sync.Map{}
	for _, rule := range banpCache.ingressRules {
		var ipsToDelete []net.IP
		for _, peer := range rule.peers {
			podsCache, ok := peer.pods.Load(name)
			if !ok {
				continue
			}
			podsCache.(*sync.Map).Range(func(key, value any) bool {
				ips := value.([]net.IP)
				ipsToDelete = append(ipsToDelete, ips...)
				return true
			})
			podsCaches = append(podsCaches, peer.pods)
		}
		ops, err := rule.addrSet.DeleteIPsReturnOps(ipsToDelete)
		if err != nil {
			return fmt.Errorf("unable to form delete IPs from address-set ops for ingress rule with priority %v "+
				"when trying to delete namespace %s, err: %v", rule.priority, name, err)
		}
		allops = append(allops, ops...)
	}
	for _, rule := range banpCache.egressRules {
		var ipsToDelete []net.IP
		for _, peer := range rule.peers {
			podsCache, ok := peer.pods.Load(name)
			if !ok {
				continue
			}
			podsCache.(*sync.Map).Range(func(key, value any) bool {
				ips := value.([]net.IP)
				ipsToDelete = append(ipsToDelete, ips...)
				return true
			})
			podsCaches = append(podsCaches, peer.pods)
		}
		ops, err := rule.addrSet.DeleteIPsReturnOps(ipsToDelete)
		if err != nil {
			return fmt.Errorf("unable to form delete IPs from address-set ops for egress rule with priority %v "+
				"when trying to delete namespace %s, err: %v", rule.priority, name, err)
		}
		allops = append(allops, ops...)
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, allops)
	if err != nil {
		return fmt.Errorf("unable to transact deletion ops for BANP %s "+
			"when trying to delete namespace %s, err: %v", banpCache.name, name, err)
	}
	// finally delete the namespace key from caches
	banpCache.subject.pods.Delete(name)
	for _, pc := range podsCaches {
		pc.Delete(name)
	}
	return nil
}
