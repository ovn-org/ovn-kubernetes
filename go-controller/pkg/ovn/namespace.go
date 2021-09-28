package ovn

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	// Annotation used to enable/disable multicast in the namespace
	nsMulticastAnnotation        = "k8s.ovn.org/multicast-enabled"
	routingExternalGWsAnnotation = "k8s.ovn.org/routing-external-gws"
	routingNamespaceAnnotation   = "k8s.ovn.org/routing-namespaces"
	routingNetworkAnnotation     = "k8s.ovn.org/routing-network"
	bfdAnnotation                = "k8s.ovn.org/bfd-enabled"
	// Annotation for enabling ACL logging to controller's log file
	aclLoggingAnnotation = "k8s.ovn.org/acl-logging"
)

func (oc *Controller) syncNamespaces(namespaces []interface{}) {
	expectedNs := make(map[string]bool)
	for _, nsInterface := range namespaces {
		ns, ok := nsInterface.(*kapi.Namespace)
		if !ok {
			klog.Errorf("Spurious object in syncNamespaces: %v", nsInterface)
			continue
		}
		expectedNs[ns.Name] = true
	}

	err := oc.addressSetFactory.ProcessEachAddressSet(func(addrSetName, namespaceName, nameSuffix string) {
		if nameSuffix == "" && !expectedNs[namespaceName] {
			if err := oc.addressSetFactory.DestroyAddressSetInBackingStore(addrSetName); err != nil {
				klog.Errorf(err.Error())
			}
		}
	})
	if err != nil {
		klog.Errorf("Error in syncing namespaces: %v", err)
	}
}

func (oc *Controller) getRoutingExternalGWs(nsInfo *namespaceInfo) *gatewayInfo {
	res := gatewayInfo{}
	// return a copy of the object so it can be handled without the
	// namespace locked
	res.bfdEnabled = nsInfo.routingExternalGWs.bfdEnabled
	res.gws = make([]net.IP, len(nsInfo.routingExternalGWs.gws))
	copy(res.gws, nsInfo.routingExternalGWs.gws)
	return &res
}

func (oc *Controller) getRoutingPodGWs(nsInfo *namespaceInfo) map[string]*gatewayInfo {
	// return a copy of the object so it can be handled without the
	// namespace locked
	res := make(map[string]*gatewayInfo)
	for k, v := range nsInfo.routingExternalPodGWs {
		item := &gatewayInfo{
			bfdEnabled: v.bfdEnabled,
			gws:        make([]net.IP, len(v.gws)),
		}
		copy(item.gws, v.gws)
		res[k] = item
	}
	return res
}

// addPodToNamespace adds the pod's IP to the namespace's address set and returns
// pod's routing gateway info
func (oc *Controller) addPodToNamespace(ns string, ips []*net.IPNet) (*gatewayInfo, map[string]*gatewayInfo, error) {
	nsInfo, nsUnlock, err := oc.ensureNamespaceLocked(ns, true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to ensure namespace locked: %v", err)
	}

	defer nsUnlock()

	if err := nsInfo.addressSet.AddIPs(createIPAddressSlice(ips)); err != nil {
		return nil, nil, err
	}

	return oc.getRoutingExternalGWs(nsInfo), oc.getRoutingPodGWs(nsInfo), nil
}

func (oc *Controller) deletePodFromNamespace(ns string, portInfo *lpInfo) error {
	nsInfo, nsUnlock := oc.getNamespaceLocked(ns, true)
	if nsInfo == nil {
		return nil
	}
	defer nsUnlock()

	if nsInfo.addressSet != nil {
		if err := nsInfo.addressSet.DeleteIPs(createIPAddressSlice(portInfo.ips)); err != nil {
			return err
		}
	}

	// Remove the port from the multicast allow policy.
	if oc.multicastSupport && nsInfo.multicastEnabled {
		if err := podDeleteAllowMulticastPolicy(oc.nbClient, ns, portInfo); err != nil {
			return err
		}
	}

	return nil
}

func createIPAddressSlice(ips []*net.IPNet) []net.IP {
	ipAddrs := make([]net.IP, 0)
	for _, ip := range ips {
		ipAddrs = append(ipAddrs, ip.IP)
	}
	return ipAddrs
}

func isNamespaceMulticastEnabled(annotations map[string]string) bool {
	return annotations[nsMulticastAnnotation] == "true"
}

// Creates an explicit "allow" policy for multicast traffic within the
// namespace if multicast is enabled. Otherwise, removes the "allow" policy.
// Traffic will be dropped by the default multicast deny ACL.
func (oc *Controller) multicastUpdateNamespace(ns *kapi.Namespace, nsInfo *namespaceInfo) {
	if !oc.multicastSupport {
		return
	}

	enabled := isNamespaceMulticastEnabled(ns.Annotations)
	enabledOld := nsInfo.multicastEnabled
	if enabledOld == enabled {
		return
	}

	var err error
	nsInfo.multicastEnabled = enabled
	if enabled {
		err = oc.createMulticastAllowPolicy(ns.Name, nsInfo)
	} else {
		err = deleteMulticastAllowPolicy(oc.nbClient, ns.Name, nsInfo)
	}
	if err != nil {
		klog.Errorf(err.Error())
		return
	}
}

// Cleans up the multicast policy for this namespace if multicast was
// previously allowed.
func (oc *Controller) multicastDeleteNamespace(ns *kapi.Namespace, nsInfo *namespaceInfo) {
	if nsInfo.multicastEnabled {
		nsInfo.multicastEnabled = false
		if err := deleteMulticastAllowPolicy(oc.nbClient, ns.Name, nsInfo); err != nil {
			klog.Errorf(err.Error())
		}
	}
}

func parseRoutingExternalGWAnnotation(annotation string) ([]net.IP, error) {
	var routingExternalGWs []net.IP
	for _, v := range strings.Split(annotation, ",") {
		parsedAnnotation := net.ParseIP(v)
		if parsedAnnotation == nil {
			return nil, fmt.Errorf("could not parse routing external gw annotation value %s", v)
		}
		routingExternalGWs = append(routingExternalGWs, parsedAnnotation)
	}
	return routingExternalGWs, nil
}

// AddNamespace creates corresponding addressset in ovn db
func (oc *Controller) AddNamespace(ns *kapi.Namespace) {
	klog.Infof("[%s] adding namespace", ns.Name)
	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s] adding namespace took %v", ns.Name, time.Since(start))
	}()

	nsInfo, nsUnlock, err := oc.ensureNamespaceLocked(ns.Name, false)
	if err != nil {
		klog.Errorf("Failed to ensure namespace locked: %v", err)
		return
	}

	defer nsUnlock()

	if annotation, ok := ns.Annotations[routingExternalGWsAnnotation]; ok {
		exGateways, err := parseRoutingExternalGWAnnotation(annotation)
		if err != nil {
			klog.Errorf(err.Error())
		} else {
			_, bfdEnabled := ns.Annotations[bfdAnnotation]
			err = oc.addExternalGWsForNamespace(gatewayInfo{gws: exGateways, bfdEnabled: bfdEnabled}, nsInfo, ns.Name)
			if err != nil {
				klog.Error(err.Error())
			}
		}
		if _, ok := ns.Annotations[bfdAnnotation]; ok {
			nsInfo.routingExternalGWs.bfdEnabled = true
		}
	}

	annotation := ns.Annotations[aclLoggingAnnotation]
	if annotation != "" {
		if oc.aclLoggingCanEnable(annotation, nsInfo) {
			klog.Infof("Namespace %s: ACL logging is set to deny=%s allow=%s", ns.Name, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
		} else {
			klog.Warningf("Namespace %s: ACL logging is not enabled due to malformed annotation", ns.Name)
		}
	}

	// TODO(trozet) figure out if there is any possibility of detecting if a pod GW already exists, which
	// is servicing this namespace. Right now that would mean searching through all pods, which is very inefficient.
	// For now it is required that a pod serving as a gateway for a namespace is added AFTER the serving namespace is
	// created

	// If multicast enabled, adds all current pods in the namespace to the allow policy
	oc.multicastUpdateNamespace(ns, nsInfo)
}

func (oc *Controller) updateNamespace(old, newer *kapi.Namespace) {
	klog.Infof("[%s] updating namespace", old.Name)

	nsInfo, nsUnlock := oc.getNamespaceLocked(old.Name, false)
	if nsInfo == nil {
		klog.Warningf("Update event for unknown namespace %q", old.Name)
		return
	}
	defer nsUnlock()

	gwAnnotation := newer.Annotations[routingExternalGWsAnnotation]
	oldGWAnnotation := old.Annotations[routingExternalGWsAnnotation]
	_, newBFDEnabled := newer.Annotations[bfdAnnotation]
	_, oldBFDEnabled := old.Annotations[bfdAnnotation]

	if gwAnnotation != oldGWAnnotation || newBFDEnabled != oldBFDEnabled {
		// if old gw annotation was empty, new one must not be empty, so we should remove any per pod SNAT
		if oldGWAnnotation == "" {
			if config.Gateway.DisableSNATMultipleGWs {
				existingPods, err := oc.watchFactory.GetPods(old.Name)
				if err != nil {
					klog.Errorf("Failed to get all the pods (%v)", err)
				}
				for _, pod := range existingPods {
					logicalPort := util.GetLogicalPortName(pod.Namespace, pod.Name)
					portInfo, err := oc.logicalPortCache.get(logicalPort)
					if err != nil {
						klog.Warningf("Unable to get port %s in cache for SNAT rule removal", logicalPort)
					} else {
						oc.deletePerPodGRSNAT(pod.Spec.NodeName, portInfo.ips)
					}
				}
			}
		} else {
			oc.deleteGWRoutesForNamespace(nsInfo)
		}
		exGateways, err := parseRoutingExternalGWAnnotation(gwAnnotation)
		if err != nil {
			klog.Error(err.Error())
		} else {
			err = oc.addExternalGWsForNamespace(gatewayInfo{gws: exGateways, bfdEnabled: newBFDEnabled}, nsInfo, old.Name)
			if err != nil {
				klog.Error(err.Error())
			}
		}
		// if new annotation is empty, exgws were removed, may need to add SNAT per pod
		// check if there are any pod gateways serving this namespace as well
		if gwAnnotation == "" && len(nsInfo.routingExternalPodGWs) == 0 && config.Gateway.DisableSNATMultipleGWs {
			existingPods, err := oc.watchFactory.GetPods(old.Name)
			if err != nil {
				klog.Errorf("Failed to get all the pods (%v)", err)
			}
			for _, pod := range existingPods {
				podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations)
				if err != nil {
					klog.Error(err.Error())
				} else {
					if err = oc.addPerPodGRSNAT(pod, podAnnotation.IPs); err != nil {
						klog.Error(err.Error())
					}
				}
			}
		}
	}
	aclAnnotation := newer.Annotations[aclLoggingAnnotation]
	oldACLAnnotation := old.Annotations[aclLoggingAnnotation]
	// support for ACL logging update, if new annotation is empty, make sure we propagate new setting
	if aclAnnotation != oldACLAnnotation && (oc.aclLoggingCanEnable(aclAnnotation, nsInfo) || aclAnnotation == "") &&
		len(nsInfo.networkPolicies) > 0 {
		// deny rules are all one per namespace
		if err := oc.setACLDenyLogging(old.Name, nsInfo, nsInfo.aclLogging.Deny); err != nil {
			klog.Warningf(err.Error())
		} else {
			klog.Infof("Namespace %s: ACL logging setting updated to deny=%s allow=%s",
				old.Name, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
		}
	}
	oc.multicastUpdateNamespace(newer, nsInfo)
}

func (oc *Controller) deleteNamespace(ns *kapi.Namespace) {
	klog.Infof("[%s] deleting namespace", ns.Name)

	nsInfo := oc.deleteNamespaceLocked(ns.Name)
	if nsInfo == nil {
		return
	}
	defer nsInfo.Unlock()

	klog.V(5).Infof("Deleting Namespace's NetworkPolicy entities")
	for _, np := range nsInfo.networkPolicies {
		delete(nsInfo.networkPolicies, np.name)
		oc.destroyNetworkPolicy(np, nsInfo)
	}
	oc.deleteGWRoutesForNamespace(nsInfo)
	oc.multicastDeleteNamespace(ns, nsInfo)
}

// waitForNamespaceLocked waits up to 10 seconds for a Namespace to be known; use this
// rather than getNamespaceLocked when calling from a thread where you might be processing
// an event in a namespace before the Namespace factory thread has processed the Namespace
// addition.
func (oc *Controller) waitForNamespaceLocked(namespace string, readOnly bool) (*namespaceInfo, func(), error) {
	var nsInfo *namespaceInfo
	var nsUnlock func()

	err := utilwait.PollImmediate(100*time.Millisecond, 10*time.Second,
		func() (bool, error) {
			nsInfo, nsUnlock = oc.getNamespaceLocked(namespace, readOnly)
			return nsInfo != nil, nil
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("timeout waiting for namespace event")
	}
	return nsInfo, nsUnlock, nil
}

// getNamespaceLocked locks namespacesMutex, looks up ns, and (if found), returns it with
// its mutex locked. If ns is not known, nil will be returned
func (oc *Controller) getNamespaceLocked(ns string, readOnly bool) (*namespaceInfo, func()) {
	// Only hold namespacesMutex while reading/modifying oc.namespaces. In particular,
	// we drop namespacesMutex while trying to claim nsInfo.Mutex, because something
	// else might have locked the nsInfo and be doing something slow with it, and we
	// don't want to block all access to oc.namespaces while that's happening.
	oc.namespacesMutex.Lock()
	nsInfo := oc.namespaces[ns]
	oc.namespacesMutex.Unlock()

	if nsInfo == nil {
		return nil, nil
	}
	var unlockFunc func()
	if readOnly {
		unlockFunc = func() { nsInfo.RUnlock() }
		nsInfo.RLock()
	} else {
		unlockFunc = func() { nsInfo.Unlock() }
		nsInfo.Lock()
	}
	// Check that the namespace wasn't deleted while we were waiting for the lock
	oc.namespacesMutex.Lock()
	defer oc.namespacesMutex.Unlock()
	if nsInfo != oc.namespaces[ns] {
		unlockFunc()
		return nil, nil
	}
	return nsInfo, unlockFunc
}

// ensureNamespaceLocked locks namespacesMutex, gets/creates an entry for ns, and returns it
// with its mutex locked. Also returns an unlock function and error.
func (oc *Controller) ensureNamespaceLocked(ns string, readOnly bool) (*namespaceInfo, func(), error) {
	oc.namespacesMutex.Lock()
	nsInfo := oc.namespaces[ns]
	nsInfoExisted := false
	if nsInfo == nil {
		nsInfo = &namespaceInfo{
			networkPolicies:       make(map[string]*networkPolicy),
			podExternalRoutes:     make(map[string]map[string]string),
			multicastEnabled:      false,
			routingExternalPodGWs: make(map[string]gatewayInfo),
		}
		// we are creating nsInfo and going to set it in namespaces map
		// so safe to hold the lock while we create and add it
		defer oc.namespacesMutex.Unlock()
		// create the adddress set for the new namespace
		var err error
		nsInfo.addressSet, err = oc.createNamespaceAddrSetAllPods(ns)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create address set for namespace: %s, error: %v", ns, err)
		}
		oc.namespaces[ns] = nsInfo
	} else {
		nsInfoExisted = true
		// if we found and existing nsInfo, do not hold the namespaces lock
		// while waiting for nsInfo to Lock
		oc.namespacesMutex.Unlock()
	}

	var unlockFunc func()
	if readOnly {
		unlockFunc = func() { nsInfo.RUnlock() }
		nsInfo.RLock()
	} else {
		unlockFunc = func() { nsInfo.Unlock() }
		nsInfo.Lock()
	}

	if nsInfoExisted {
		// Check that the namespace wasn't deleted while we were waiting for the lock
		oc.namespacesMutex.Lock()
		defer oc.namespacesMutex.Unlock()
		if nsInfo != oc.namespaces[ns] {
			unlockFunc()
			return nil, nil, fmt.Errorf("namespace %s, was removed during ensure", ns)
		}
	}

	return nsInfo, unlockFunc, nil
}

// deleteNamespaceLocked locks namespacesMutex, finds and deletes ns, and returns the
// namespace, locked.
func (oc *Controller) deleteNamespaceLocked(ns string) *namespaceInfo {
	// The locking here is the same as in getNamespaceLocked

	oc.namespacesMutex.Lock()
	nsInfo := oc.namespaces[ns]
	oc.namespacesMutex.Unlock()

	if nsInfo == nil {
		return nil
	}
	nsInfo.Lock()

	oc.namespacesMutex.Lock()
	defer oc.namespacesMutex.Unlock()
	if nsInfo != oc.namespaces[ns] {
		nsInfo.Unlock()
		return nil
	}
	if nsInfo.addressSet != nil {
		// Empty the address set, then delete it after an interval.
		if err := nsInfo.addressSet.SetIPs(nil); err != nil {
			klog.Errorf("Warning: failed to empty address set for deleted NS %s: %v", ns, err)
		}

		// Delete the address set after a short delay.
		// This is so NetworkPolicy handlers can converge and stop referencing it.
		addressSet := nsInfo.addressSet
		go func() {
			select {
			case <-oc.stopChan:
				return
			case <-time.After(20 * time.Second):
				// Check to see if the NS was re-added in the meanwhile. If so,
				// only delete if the new NS's AddressSet shouldn't exist.
				nsInfo, nsUnlock := oc.getNamespaceLocked(ns, true)
				if nsInfo != nil {
					defer nsUnlock()
					if nsInfo.addressSet != nil {
						klog.V(5).Infof("Skipping deferred deletion of AddressSet for NS %s: re-created", ns)
						return
					}
				}

				klog.V(5).Infof("Finishing deferred deletion of AddressSet for NS %s", ns)
				if err := addressSet.Destroy(); err != nil {
					klog.Errorf("Failed to delete AddressSet for NS %s: %v", ns, err.Error())
				}
			}
		}()
	}
	delete(oc.namespaces, ns)

	return nsInfo
}

func (oc *Controller) createNamespaceAddrSetAllPods(ns string) (addressset.AddressSet, error) {
	var ips []net.IP
	// special handling of host network namespace
	if config.Kubernetes.HostNetworkNamespace != "" &&
		ns == config.Kubernetes.HostNetworkNamespace {
		// add the mp0 interface addresses to this namespace.
		existingNodes, err := oc.watchFactory.GetNodes()
		if err != nil {
			klog.Errorf("Failed to get all nodes (%v)", err)
		} else {
			ips = make([]net.IP, 0, len(existingNodes))
			for _, node := range existingNodes {
				hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node)
				if err != nil {
					klog.Warningf("Error parsing host subnet annotation for node %s (%v)",
						node.Name, err)
				}
				for _, hostSubnet := range hostSubnets {
					mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
					ips = append(ips, mgmtIfAddr.IP)
				}
				// for shared gateway mode we will use LRP IPs to SNAT host network traffic
				// so add these to the address set.
				lrpIPs, err := oc.joinSwIPManager.EnsureJoinLRPIPs(node.Name)
				if err != nil {
					klog.Errorf("Failed to get join switch port IP address for node %s: %v", node.Name, err)
				}

				for _, lrpIP := range lrpIPs {
					ips = append(ips, lrpIP.IP)
				}
			}
		}
	}
	// Get all the pods in the namespace and append their IP to the address_set
	existingPods, err := oc.watchFactory.GetPods(ns)
	if err != nil {
		klog.Errorf("Failed to get all the pods (%v)", err)
	} else {
		ips = make([]net.IP, 0, len(existingPods))
		for _, pod := range existingPods {
			if pod.Status.PodIP != "" && !pod.Spec.HostNetwork {
				podIPs, err := util.GetAllPodIPs(pod)
				if err != nil {
					klog.Warningf(err.Error())
					continue
				}
				ips = append(ips, podIPs...)
			}
		}
	}
	return oc.addressSetFactory.NewAddressSet(ns, ips)
}
