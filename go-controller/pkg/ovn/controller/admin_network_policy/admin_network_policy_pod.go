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

func (c *Controller) syncPodAdminNetworkPolicy(key string) error {
	startTime := time.Now()
	// Iterate all ANPs and check if this namespace start/stops matching
	// any and add/remove the setup accordingly. Namespaces can match multiple
	// ANPs objects, so continue iterating all ANP objects before finishing.
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(4).Infof("Processing sync for Pod %s/%s in Admin Network Policy controller", namespace, name)

	defer func() {
		klog.V(4).Infof("Finished syncing Pod %s/%s Admin Network Policy controller: took %v", namespace, name, time.Since(startTime))
	}()
	ns, err := c.anpNamespaceLister.Get(namespace)
	if err != nil {
		return err
	}
	namespaceLabels := labels.Set(ns.Labels)
	podNamespaceLister := c.anpPodLister.Pods(namespace)
	pod, err := podNamespaceLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	existingANPs, err := c.anpLister.List(labels.Everything())
	if err != nil {
		return err
	}
	if pod == nil || util.PodCompleted(pod) {
		for _, anp := range existingANPs {
			anpObj, loaded := c.anpCache.Load(anp.Name)
			if !loaded {
				continue
			}
			anpCache := anpObj.(*adminNetworkPolicy)
			if err := c.clearPodForANP(namespace, name, anpCache); err != nil {
				return fmt.Errorf("could not sync ANP %s when deleting pod %s/%s, err %v", anpCache.name, namespace, name, err)
			}
		}
		banpObj, loaded := c.banpCache.Load(BANPFlowPriority)
		if !loaded {
			return nil // nothing to do
		}
		banpCache := banpObj.(*baselineAdminNetworkPolicy)
		if err := c.clearPodForBANP(namespace, name, banpCache); err != nil {
			return fmt.Errorf("could not sync BANP %s when deleting pod %s/%s, err %v", banpCache.name, namespace, name, err)
		}
		return nil
	}
	for _, anp := range existingANPs {
		anpObj, loaded := c.anpCache.Load(anp.Name)
		if !loaded {
			continue
		}
		anpCache := anpObj.(*adminNetworkPolicy)
		if err := c.setPodForANP(anp.Name, pod, anpCache, namespaceLabels); err != nil {
			return fmt.Errorf("could not sync ANP %s for pod %s/%s, err %v", anp.Name, pod.Namespace, pod.Name, err)
		}
	}
	banpObj, loaded := c.banpCache.Load(BANPFlowPriority)
	if !loaded {
		return nil // nothing to do
	}
	banpCache := banpObj.(*baselineAdminNetworkPolicy)
	if err := c.setPodForBANP(pod, banpCache, namespaceLabels); err != nil {
		return fmt.Errorf("could not sync BANP %s for pod %s/%s, err %v", banpCache.name, pod.Namespace, pod.Name, err)
	}
	return nil
}

func (c *Controller) setPodForANP(anpName string, pod *v1.Pod, anpCache *adminNetworkPolicy, namespaceLabels labels.Labels) error {
	podMapOps := []mapAndOp{}
	ops := []ovsdb.Operation{}
	var err error
	anpCache.RLock() // allow multiple pods to sync
	defer anpCache.RUnlock()
	if anpCache.stale { // was deleted or not created properly
		return nil
	}
	var portsToAdd, portsToDelete []string
	logicalPortName := util.GetLogicalPortName(pod.Namespace, pod.Name)
	lsp := &nbdb.LogicalSwitchPort{Name: logicalPortName}
	lsp, err = libovsdbops.GetLogicalSwitchPort(c.nbClient, lsp)
	if err != nil {
		return fmt.Errorf("error retrieving logical switch port with Name %s "+
			" from libovsdb cache: %w", logicalPortName, err)
	}
	podIPs, err := util.GetPodIPsOfNetwork(pod, &util.DefaultNetInfo{})
	if err != nil {
		return fmt.Errorf("unable to retrieve podIPs for pod %s/%s", pod.Namespace, pod.Name)
	}
	podLabels := labels.Set(pod.Labels)
	klog.Infof("SURYA %v, %v", namespaceLabels, anpCache.subject.namespaceSelector)
	nsCache, loaded := anpCache.subject.pods.Load(pod.Namespace)               // should be true if namespaceLabels match
	if anpCache.subject.namespaceSelector.Matches(namespaceLabels) && loaded { // proceed only if namespace selectors are matching for the pod and ANP Subject
		_, loaded = nsCache.(*sync.Map).Load(pod.Name)
		klog.Infof("SURYA %v:%v:%v", loaded, podLabels, anpCache.subject.podSelector)
		if anpCache.subject.podSelector.Matches(podLabels) && !loaded {
			portsToAdd = append(portsToAdd, lsp.UUID)
			klog.Infof("SURYA %v", portsToAdd)
			podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, lsp.UUID, mapInsert})
		} else if !anpCache.subject.podSelector.Matches(podLabels) && loaded {
			portsToDelete = append(portsToDelete, lsp.UUID)
			podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, lsp.UUID, mapDelete})
		}
	}
	for _, rule := range anpCache.ingressRules {
		for _, peer := range rule.peers {
			// proceed only if namespace selectors are matching for the pod and ANP Peer
			nsCache, loaded := peer.pods.Load(pod.Namespace) // should be true if namespaceLabels match
			if !peer.namespaceSelector.Matches(namespaceLabels) || !loaded {
				continue
			}
			klog.Infof("SURYA %v, %v", namespaceLabels, peer.namespaceSelector)
			_, loaded = nsCache.(*sync.Map).Load(pod.Name)
			if peer.podSelector.Matches(podLabels) && !loaded {
				klog.Infof("SURYA %v, %v, %v", podLabels, peer.podSelector, podIPs)
				addrOps, err := rule.addrSet.AddIPsReturnOps(podIPs)
				if err != nil {
					return err
				}
				ops = append(ops, addrOps...)
				klog.Infof("SURYA %v", ops)
				podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapInsert})
			} else if !peer.podSelector.Matches(podLabels) && loaded {
				klog.Infof("SURYA %v, %v", podLabels, peer.podSelector)
				addrOps, err := rule.addrSet.DeleteIPsReturnOps(podIPs)
				if err != nil {
					return err
				}
				ops = append(ops, addrOps...)
				klog.Infof("SURYA %v", ops)
				podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapDelete})
			}
		}
	}
	for _, rule := range anpCache.egressRules {
		for _, peer := range rule.peers {
			// proceed only if namespace selectors are matching for the pod and ANP Peer
			nsCache, loaded := peer.pods.Load(pod.Namespace) // should be true if namespaceLabels match
			if !peer.namespaceSelector.Matches(namespaceLabels) || !loaded {
				continue
			}
			klog.Infof("SURYA %v, %v", namespaceLabels, peer.namespaceSelector)
			_, loaded = nsCache.(*sync.Map).Load(pod.Name)
			if peer.podSelector.Matches(podLabels) && !loaded {
				klog.Infof("SURYA %v, %v, %v", podLabels, peer.podSelector, podIPs)
				addrOps, err := rule.addrSet.AddIPsReturnOps(podIPs)
				if err != nil {
					return err
				}
				ops = append(ops, addrOps...)
				klog.Infof("SURYA %v", ops)
				podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapInsert})
			} else if !peer.podSelector.Matches(podLabels) && loaded {
				klog.Infof("SURYA %v, %v", podLabels, peer.podSelector)
				addrOps, err := rule.addrSet.DeleteIPsReturnOps(podIPs)
				if err != nil {
					return err
				}
				ops = append(ops, addrOps...)
				klog.Infof("SURYA %v", ops)
				podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapDelete})
			}
		}
	}
	pgName, _ := getAdminNetworkPolicyPGName(anpCache.name)
	if len(portsToAdd) > 0 {
		ops, err = libovsdbops.AddPortsToPortGroupOps(c.nbClient, ops, pgName, portsToAdd...)
		if err != nil {
			return err
		}
		klog.Infof("SURYA")
	}
	if len(portsToDelete) > 0 {
		ops, err = libovsdbops.DeletePortsFromPortGroupOps(c.nbClient, ops, pgName, portsToDelete...)
		if err != nil {
			return err
		}
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return err
	}

	// Since we are using syncMaps it is already write protected,
	// so we are ok with just using a RLock in this function
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

func (c *Controller) setPodForBANP(pod *v1.Pod, banpCache *baselineAdminNetworkPolicy, namespaceLabels labels.Labels) error {
	podMapOps := []mapAndOp{}
	ops := []ovsdb.Operation{}
	var err error
	banpCache.RLock() // allow multiple pods to sync
	defer banpCache.RUnlock()
	if banpCache.stale { // was deleted or not created properly
		return nil
	}
	var portsToAdd, portsToDelete []string
	logicalPortName := util.GetLogicalPortName(pod.Namespace, pod.Name)
	lsp := &nbdb.LogicalSwitchPort{Name: logicalPortName}
	lsp, err = libovsdbops.GetLogicalSwitchPort(c.nbClient, lsp)
	if err != nil {
		return fmt.Errorf("error retrieving logical switch port with Name %s "+
			" from libovsdb cache: %w", logicalPortName, err)
	}
	podIPs, err := util.GetPodIPsOfNetwork(pod, &util.DefaultNetInfo{})
	if err != nil {
		return fmt.Errorf("unable to retrieve podIPs for pod %s/%s", pod.Namespace, pod.Name)
	}
	podLabels := labels.Set(pod.Labels)
	klog.Infof("SURYA %v, %v", namespaceLabels, banpCache.subject.namespaceSelector)
	nsCache, loaded := banpCache.subject.pods.Load(pod.Namespace)               // should be true if namespaceLabels match
	if banpCache.subject.namespaceSelector.Matches(namespaceLabels) && loaded { // proceed only if namespace selectors are matching for the pod and ANP Subject
		_, loaded = nsCache.(*sync.Map).Load(pod.Name)
		if banpCache.subject.podSelector.Matches(podLabels) && !loaded {
			portsToAdd = append(portsToAdd, lsp.UUID)
			klog.Infof("SURYA %v", portsToAdd)
			podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, lsp.UUID, mapInsert})
		} else if !banpCache.subject.podSelector.Matches(podLabels) && loaded {
			portsToDelete = append(portsToDelete, lsp.UUID)
			podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, lsp.UUID, mapDelete})
		}
	}
	for _, rule := range banpCache.ingressRules {
		for _, peer := range rule.peers {
			// proceed only if namespace selectors are matching for the pod and BANP Peer
			nsCache, loaded := peer.pods.Load(pod.Namespace) // should be true if namespaceLabels match
			if !peer.namespaceSelector.Matches(namespaceLabels) || !loaded {
				continue
			}
			_, loaded = nsCache.(*sync.Map).Load(pod.Name)
			if peer.podSelector.Matches(podLabels) && !loaded {
				klog.Infof("SURYA %v, %v, %v", podLabels, peer.podSelector, podIPs)
				addrOps, err := rule.addrSet.AddIPsReturnOps(podIPs)
				if err != nil {
					return err
				}
				ops = append(ops, addrOps...)
				klog.Infof("SURYA %v", ops)
				podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapInsert})
			} else if !peer.podSelector.Matches(podLabels) && loaded {
				klog.Infof("SURYA %v, %v", podLabels, peer.podSelector)
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
			// proceed only if namespace selectors are matching for the pod and BANP Peer
			nsCache, loaded := peer.pods.Load(pod.Namespace) // should be true if namespaceLabels match
			if !peer.namespaceSelector.Matches(namespaceLabels) || !loaded {
				continue
			}
			_, loaded = nsCache.(*sync.Map).Load(pod.Name)
			if peer.podSelector.Matches(podLabels) && !loaded {
				klog.Infof("SURYA %v, %v, %v", podLabels, peer.podSelector, podIPs)
				addrOps, err := rule.addrSet.AddIPsReturnOps(podIPs)
				if err != nil {
					return err
				}
				ops = append(ops, addrOps...)
				klog.Infof("SURYA %v", ops)
				podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapInsert})
			} else if !peer.podSelector.Matches(podLabels) && loaded {
				addrOps, err := rule.addrSet.DeleteIPsReturnOps(podIPs)
				if err != nil {
					return err
				}
				ops = append(ops, addrOps...)
				podMapOps = append(podMapOps, mapAndOp{nsCache.(*sync.Map), pod.Name, podIPs, mapDelete})
			}
		}
	}
	pgName, _ := getBaselineAdminNetworkPolicyPGName(banpCache.name)
	if len(portsToAdd) > 0 {
		klog.Infof("SURYA %v", portsToAdd)
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
	klog.Infof("SURYA %v", ops)
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		return err
	}

	// Since we are using syncMaps it is already write protected,
	// so we are ok with just using a RLock in this function
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

func (c *Controller) clearPodForANP(namespace, name string, anpCache *adminNetworkPolicy) error {
	allops := []ovsdb.Operation{}
	var err error
	anpCache.RLock()
	defer anpCache.RUnlock()
	podsCaches := []*sync.Map{}
	if podCache, ok := anpCache.subject.pods.Load(namespace); ok {
		pgName, _ := getAdminNetworkPolicyPGName(anpCache.name)
		if obj, loaded := podCache.(*sync.Map).Load(name); loaded {
			portsToDelete := obj.(string)
			allops, err = libovsdbops.DeletePortsFromPortGroupOps(c.nbClient, allops, pgName, portsToDelete)
			if err != nil {
				return err
			}
			klog.Infof("SURYA %v", allops)
			podsCaches = append(podsCaches, podCache.(*sync.Map))
		}
	}
	for _, rule := range anpCache.ingressRules {
		var ipsToDelete []net.IP
		podFoundInAtLeastOnePeer := false
		for _, peer := range rule.peers {
			if podCache, ok := peer.pods.Load(namespace); ok {
				obj, loaded := podCache.(*sync.Map).Load(name)
				if !loaded {
					continue
				}
				if !podFoundInAtLeastOnePeer {
					ipsToDelete = obj.([]net.IP)
				}
				podsCaches = append(podsCaches, podCache.(*sync.Map))
				podFoundInAtLeastOnePeer = true
			}
		}
		ops, err := rule.addrSet.DeleteIPsReturnOps(ipsToDelete)
		if err != nil {
			return err
		}
		allops = append(allops, ops...)
	}
	for _, rule := range anpCache.egressRules {
		var ipsToDelete []net.IP
		podFoundInAtLeastOnePeer := false
		for _, peer := range rule.peers {
			if podCache, ok := peer.pods.Load(namespace); ok {
				obj, loaded := podCache.(*sync.Map).Load(name)
				if !loaded {
					continue
				}
				if !podFoundInAtLeastOnePeer {
					ipsToDelete = obj.([]net.IP)
				}
				podsCaches = append(podsCaches, podCache.(*sync.Map))
				podFoundInAtLeastOnePeer = true
			}
		}
		ops, err := rule.addrSet.DeleteIPsReturnOps(ipsToDelete)
		if err != nil {
			return err
		}
		allops = append(allops, ops...)
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, allops)
	if err != nil {
		return err
	}
	// finally delete the pod key from cache
	for _, pc := range podsCaches {
		klog.Infof("SURYA %v", name)
		pc.Delete(name)
	}
	return nil
}

func (c *Controller) clearPodForBANP(namespace, name string, banpCache *baselineAdminNetworkPolicy) error {
	allops := []ovsdb.Operation{}
	var err error
	banpCache.RLock()
	defer banpCache.RUnlock()
	podsCaches := []*sync.Map{}
	if podCache, ok := banpCache.subject.pods.Load(namespace); ok {
		pgName, _ := getBaselineAdminNetworkPolicyPGName(banpCache.name)
		if obj, loaded := podCache.(*sync.Map).Load(name); loaded {
			portsToDelete := obj.(string)
			allops, err = libovsdbops.DeletePortsFromPortGroupOps(c.nbClient, allops, pgName, portsToDelete)
			if err != nil {
				return fmt.Errorf("unable to form delete port group ops for PG: %s "+
					"when trying to delete pod %s/%s, err: %v", pgName, namespace, name, err)
			}
			podsCaches = append(podsCaches, podCache.(*sync.Map))
		}
	}
	for _, rule := range banpCache.ingressRules {
		var ipsToDelete []net.IP
		podFoundInAtLeastOnePeer := false
		for _, peer := range rule.peers {
			if podCache, ok := peer.pods.Load(namespace); ok {
				obj, loaded := podCache.(*sync.Map).Load(name)
				if !loaded {
					continue
				}
				if !podFoundInAtLeastOnePeer {
					ipsToDelete = obj.([]net.IP)
				}
				podsCaches = append(podsCaches, podCache.(*sync.Map))
				podFoundInAtLeastOnePeer = true
			}
		}
		ops, err := rule.addrSet.DeleteIPsReturnOps(ipsToDelete)
		if err != nil {
			return err
		}
		allops = append(allops, ops...)
	}
	for _, rule := range banpCache.egressRules {
		var ipsToDelete []net.IP
		podFoundInAtLeastOnePeer := false
		for _, peer := range rule.peers {
			if podCache, ok := peer.pods.Load(namespace); ok {
				obj, loaded := podCache.(*sync.Map).Load(name)
				if !loaded {
					continue
				}
				if !podFoundInAtLeastOnePeer {
					ipsToDelete = obj.([]net.IP)
				}
				podsCaches = append(podsCaches, podCache.(*sync.Map))
				podFoundInAtLeastOnePeer = true
			}
		}
		ops, err := rule.addrSet.DeleteIPsReturnOps(ipsToDelete)
		if err != nil {
			return err
		}
		allops = append(allops, ops...)
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, allops)
	if err != nil {
		return err
	}
	// finally delete the pod key from cache
	for _, pc := range podsCaches {
		pc.Delete(name)
	}
	return nil
}
