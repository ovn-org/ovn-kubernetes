package ovn

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// namespaceInfo contains information related to a Namespace. Use oc.getNamespaceLocked()
// or oc.waitForNamespaceLocked() to get a locked namespaceInfo for a Namespace, and call
// nsInfo.Unlock() on it when you are done with it. (No code outside of the code that
// manages the oc.namespaces map is ever allowed to hold an unlocked namespaceInfo.)
type namespaceInfo struct {
	sync.RWMutex

	// addressSet is an address set object that holds the IP addresses
	// of all pods in the namespace.
	addressSet addressset.AddressSet

	// Map of related network policies. Policy will add itself to this list when it's ready to subscribe
	// to namespace Update events. Retry logic to update network policy based on namespace event is handled by namespace.
	// Policy should only be added after successful create, and deleted before any network policy resources are deleted.
	// This is the map of keys that can be used to get networkPolicy from oc.networkPolicies.
	//
	// You must hold the namespaceInfo's mutex to add/delete dependent policies.
	// Namespace can take oc.networkPolicies key Lock while holding nsInfo lock, the opposite should never happen.
	relatedNetworkPolicies map[string]bool

	hybridOverlayExternalGW net.IP
	hybridOverlayVTEP       net.IP

	// routingExternalGWs is a slice of net.IP containing the values parsed from
	// annotation k8s.ovn.org/routing-external-gws
	routingExternalGWs gatewayInfo

	// routingExternalPodGWs contains a map of all pods serving as exgws as well as their
	// exgw IPs
	// key is <namespace>_<pod name>
	routingExternalPodGWs map[string]gatewayInfo

	multicastEnabled bool

	// If not empty, then it has to be set to a logging a severity level, e.g. "notice", "alert", etc
	aclLogging ACLLoggingLevels
}

// This function implements the main body of work of syncNamespaces.
// Upon failure, it may be invoked multiple times in order to avoid a pod restart.
func (oc *DefaultNetworkController) syncNamespaces(namespaces []interface{}) error {
	expectedNs := make(map[string]bool)
	for _, nsInterface := range namespaces {
		ns, ok := nsInterface.(*kapi.Namespace)
		if !ok {
			return fmt.Errorf("spurious object in syncNamespaces: %v", nsInterface)
		}
		expectedNs[ns.Name] = true
	}

	err := oc.addressSetFactory.ProcessEachAddressSet(func(hashedName, addrSetName string) error {
		// filter out address sets owned by HybridRoutePolicy and EgressQoS by prefix.
		// network policy-owned address set would have a dot in the address set name due to the format
		// (namespace can't have dots in its name, and their address sets too).
		// the only left address sets may be owned by egress firewall dns or namespace
		if strings.HasPrefix(addrSetName, types.HybridRoutePolicyPrefix) ||
			strings.HasPrefix(addrSetName, types.EgressQoSRulePrefix) ||
			strings.Contains(addrSetName, ".") {
			return nil
		}

		// make sure address set is not owned by egress firewall dns
		// find ACLs referencing given address set (by hashName)
		aclPred := func(acl *nbdb.ACL) bool {
			return strings.Contains(acl.Match, "$"+hashedName)
		}
		acls, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, aclPred)
		if err != nil {
			return fmt.Errorf("failed to find referencing acls for address set %s: %v", addrSetName, err)
		}
		if len(acls) > 0 {
			// if given address set is owned by egress firewall, all ACLs will be owned by the same object
			acl := acls[0]
			// check if egress firewall dns is the owner
			// the only address set that may be referenced in egress firewall destination is dns address set
			if acl.ExternalIDs[egressFirewallACLExtIdKey] != "" && strings.Contains(acl.Match, ".dst == $"+hashedName) {
				// address set is owned by egress firewall, skip
				return nil
			}
		}
		// address set is owned by namespace, namespace name = address set name
		if !expectedNs[addrSetName] {
			if err = oc.addressSetFactory.DestroyAddressSetInBackingStore(addrSetName); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("error in syncing namespaces: %v", err)
	}
	return nil
}

func (oc *DefaultNetworkController) getRoutingExternalGWs(nsInfo *namespaceInfo) *gatewayInfo {
	res := gatewayInfo{}
	// return a copy of the object so it can be handled without the
	// namespace locked
	res.bfdEnabled = nsInfo.routingExternalGWs.bfdEnabled
	res.gws = sets.NewString(nsInfo.routingExternalGWs.gws.UnsortedList()...)
	return &res
}

// wrapper function to log if there are duplicate gateway IPs present in the cache
func validateRoutingPodGWs(podGWs map[string]gatewayInfo) error {
	// map to hold IP/podName
	ipTracker := make(map[string]string)
	for podName, gwInfo := range podGWs {
		for _, gwIP := range gwInfo.gws.UnsortedList() {
			if foundPod, ok := ipTracker[gwIP]; ok {
				return fmt.Errorf("duplicate IP found in ECMP Pod route cache! IP: %q, first pod: %q, second "+
					"pod: %q", gwIP, podName, foundPod)
			}
			ipTracker[gwIP] = podName
		}
	}
	return nil
}

func (oc *DefaultNetworkController) getRoutingPodGWs(nsInfo *namespaceInfo) map[string]gatewayInfo {
	// return a copy of the object so it can be handled without the
	// namespace locked
	res := make(map[string]gatewayInfo)
	for k, v := range nsInfo.routingExternalPodGWs {
		item := gatewayInfo{
			bfdEnabled: v.bfdEnabled,
			gws:        sets.NewString(v.gws.UnsortedList()...),
		}
		res[k] = item
	}
	return res
}

// addPodToNamespace returns pod's routing gateway info and the ops needed
// to add pod's IP to the namespace's address set.
func (oc *DefaultNetworkController) addPodToNamespace(ns string, ips []*net.IPNet) (*gatewayInfo, map[string]gatewayInfo, net.IP, []ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error
	nsInfo, nsUnlock, err := oc.ensureNamespaceLocked(ns, true, nil)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to ensure namespace locked: %v", err)
	}

	defer nsUnlock()

	if ops, err = nsInfo.addressSet.AddIPsReturnOps(createIPAddressSlice(ips)); err != nil {
		return nil, nil, nil, nil, err
	}

	return oc.getRoutingExternalGWs(nsInfo), oc.getRoutingPodGWs(nsInfo), nsInfo.hybridOverlayExternalGW, ops, nil
}

func createIPAddressSlice(ips []*net.IPNet) []net.IP {
	ipAddrs := make([]net.IP, 0)
	for _, ip := range ips {
		ipAddrs = append(ipAddrs, ip.IP)
	}
	return ipAddrs
}

func isNamespaceMulticastEnabled(annotations map[string]string) bool {
	return annotations[util.NsMulticastAnnotation] == "true"
}

// Creates an explicit "allow" policy for multicast traffic within the
// namespace if multicast is enabled. Otherwise, removes the "allow" policy.
// Traffic will be dropped by the default multicast deny ACL.
func (oc *DefaultNetworkController) multicastUpdateNamespace(ns *kapi.Namespace, nsInfo *namespaceInfo) error {
	if !oc.multicastSupport {
		return nil
	}

	enabled := isNamespaceMulticastEnabled(ns.Annotations)
	enabledOld := nsInfo.multicastEnabled
	if enabledOld == enabled {
		return nil
	}

	var err error
	nsInfo.multicastEnabled = enabled
	if enabled {
		err = oc.createMulticastAllowPolicy(ns.Name, nsInfo)
	} else {
		err = deleteMulticastAllowPolicy(oc.nbClient, ns.Name)
	}
	if err != nil {
		return err
	}
	return nil
}

// Cleans up the multicast policy for this namespace if multicast was
// previously allowed.
func (oc *DefaultNetworkController) multicastDeleteNamespace(ns *kapi.Namespace, nsInfo *namespaceInfo) error {
	if nsInfo.multicastEnabled {
		nsInfo.multicastEnabled = false
		if err := deleteMulticastAllowPolicy(oc.nbClient, ns.Name); err != nil {
			return err
		}
	}
	return nil
}

// AddNamespace creates corresponding addressset in ovn db
func (oc *DefaultNetworkController) AddNamespace(ns *kapi.Namespace) error {
	var errors []error
	klog.Infof("[%s] adding namespace", ns.Name)
	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s] adding namespace took %v", ns.Name, time.Since(start))
	}()

	nsInfo, nsUnlock, err := oc.ensureNamespaceLocked(ns.Name, false, ns)
	if err != nil {
		return fmt.Errorf("failed to ensure namespace locked: %v", err)
	}
	defer nsUnlock()

	// OCP HACK -- hybrid overlay
	annotation := ns.Annotations[hotypes.HybridOverlayExternalGw]
	if annotation != "" {
		parsedAnnotation := net.ParseIP(annotation)
		if parsedAnnotation == nil {
			errors = append(errors, fmt.Errorf("could not parse hybrid overlay external gw annotation"))
		} else {
			nsInfo.hybridOverlayExternalGW = parsedAnnotation
		}
	}
	annotation = ns.Annotations[hotypes.HybridOverlayVTEP]
	if annotation != "" {
		parsedAnnotation := net.ParseIP(annotation)
		if parsedAnnotation == nil {
			errors = append(errors, fmt.Errorf("could not parse hybrid overlay VTEP annotation"))
		} else {
			nsInfo.hybridOverlayVTEP = parsedAnnotation
		}
	}
	// END OCP HACK
	return kerrors.NewAggregate(errors)
}

// configureNamespace ensures internal structures are updated based on namespace
// must be called with nsInfo lock
func (oc *DefaultNetworkController) configureNamespace(nsInfo *namespaceInfo, ns *kapi.Namespace) error {
	var errors []error
	if annotation, ok := ns.Annotations[util.RoutingExternalGWsAnnotation]; ok {
		exGateways, err := util.ParseRoutingExternalGWAnnotation(annotation)
		if err != nil {
			errors = append(errors, fmt.Errorf("failed to parse external gateway annotation (%v)", err))
		} else {
			_, bfdEnabled := ns.Annotations[util.BfdAnnotation]
			err = oc.addExternalGWsForNamespace(gatewayInfo{gws: exGateways, bfdEnabled: bfdEnabled}, nsInfo, ns.Name)
			if err != nil {
				errors = append(errors, fmt.Errorf("failed to add external gateway for namespace %s (%v)", ns.Name, err))
			}
		}
		if _, ok := ns.Annotations[util.BfdAnnotation]; ok {
			nsInfo.routingExternalGWs.bfdEnabled = true
		}
	}

	if annotation, ok := ns.Annotations[util.AclLoggingAnnotation]; ok {
		if err := oc.aclLoggingUpdateNsInfo(annotation, nsInfo); err == nil {
			klog.Infof("Namespace %s: ACL logging is set to deny=%s allow=%s", ns.Name, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
		} else {
			klog.Warningf("Namespace %s: ACL logging contained malformed annotation, "+
				"ACL logging is set to deny=%s allow=%s, err: %q",
				ns.Name, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow, err)
		}
	}

	// TODO(trozet) figure out if there is any possibility of detecting if a pod GW already exists, which
	// is servicing this namespace. Right now that would mean searching through all pods, which is very inefficient.
	// For now it is required that a pod serving as a gateway for a namespace is added AFTER the serving namespace is
	// created

	// If multicast enabled, adds all current pods in the namespace to the allow policy
	if err := oc.multicastUpdateNamespace(ns, nsInfo); err != nil {
		errors = append(errors, fmt.Errorf("failed to update multicast (%v)", err))
	}
	return kerrors.NewAggregate(errors)
}

func (oc *DefaultNetworkController) updateNamespace(old, newer *kapi.Namespace) error {
	var errors []error
	klog.Infof("[%s] updating namespace", old.Name)

	nsInfo, nsUnlock := oc.getNamespaceLocked(old.Name, false)
	if nsInfo == nil {
		klog.Warningf("Update event for unknown namespace %q", old.Name)
		return nil
	}
	defer nsUnlock()

	gwAnnotation := newer.Annotations[util.RoutingExternalGWsAnnotation]
	oldGWAnnotation := old.Annotations[util.RoutingExternalGWsAnnotation]
	_, newBFDEnabled := newer.Annotations[util.BfdAnnotation]
	_, oldBFDEnabled := old.Annotations[util.BfdAnnotation]

	if gwAnnotation != oldGWAnnotation || newBFDEnabled != oldBFDEnabled {
		// if old gw annotation was empty, new one must not be empty, so we should remove any per pod SNAT towards nodeIP
		if oldGWAnnotation == "" {
			if config.Gateway.DisableSNATMultipleGWs {
				existingPods, err := oc.watchFactory.GetPods(old.Name)
				if err != nil {
					errors = append(errors, fmt.Errorf("failed to get all the pods (%v)", err))
				}
				for _, pod := range existingPods {
					logicalPort := util.GetLogicalPortName(pod.Namespace, pod.Name)
					if !util.PodWantsNetwork(pod) {
						continue
					}
					podIPs, err := util.GetPodIPsOfNetwork(pod, oc.NetInfo)
					if err != nil {
						errors = append(errors, fmt.Errorf("unable to get pod %q IPs for SNAT rule removal err (%v)", logicalPort, err))
					}
					ips := make([]*net.IPNet, 0, len(podIPs))
					for _, podIP := range podIPs {
						ips = append(ips, &net.IPNet{IP: podIP})
					}
					if len(ips) > 0 {
						if extIPs, err := getExternalIPsGR(oc.watchFactory, pod.Spec.NodeName); err != nil {
							errors = append(errors, err)
						} else if err = deletePodSNAT(oc.nbClient, pod.Spec.NodeName, extIPs, ips); err != nil {
							errors = append(errors, err)
						}
					}
				}
			}
		} else {
			if err := oc.deleteGWRoutesForNamespace(old.Name, nil); err != nil {
				errors = append(errors, err)
			}
			nsInfo.routingExternalGWs = gatewayInfo{}
		}
		exGateways, err := util.ParseRoutingExternalGWAnnotation(gwAnnotation)
		if err != nil {
			errors = append(errors, err)
		} else {
			err = oc.addExternalGWsForNamespace(gatewayInfo{gws: exGateways, bfdEnabled: newBFDEnabled}, nsInfo, old.Name)
			if err != nil {
				errors = append(errors, err)
			}
		}
		// if new annotation is empty, exgws were removed, may need to add SNAT per pod
		// check if there are any pod gateways serving this namespace as well
		if gwAnnotation == "" && len(nsInfo.routingExternalPodGWs) == 0 && config.Gateway.DisableSNATMultipleGWs {
			existingPods, err := oc.watchFactory.GetPods(old.Name)
			if err != nil {
				errors = append(errors, fmt.Errorf("failed to get all the pods (%v)", err))
			}
			for _, pod := range existingPods {
				podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, types.DefaultNetworkName)
				if err != nil {
					errors = append(errors, err)
				} else {
					if extIPs, err := getExternalIPsGR(oc.watchFactory, pod.Spec.NodeName); err != nil {
						errors = append(errors, err)
					} else if err = addOrUpdatePodSNAT(oc.nbClient, pod.Spec.NodeName, extIPs, podAnnotation.IPs); err != nil {
						errors = append(errors, err)
					}
				}
			}
		}
	}
	aclAnnotation := newer.Annotations[util.AclLoggingAnnotation]
	oldACLAnnotation := old.Annotations[util.AclLoggingAnnotation]
	// support for ACL logging update, if new annotation is empty, make sure we propagate new setting
	if aclAnnotation != oldACLAnnotation {
		// When input cannot be parsed correctly, aclLoggingUpdateNsInfo disables logging and returns an error. Hence,
		// log a warning to make users aware of issues with the annotation. See aclLoggingUpdateNsInfo for more details.
		if err := oc.aclLoggingUpdateNsInfo(aclAnnotation, nsInfo); err != nil {
			klog.Warningf("Namespace %s: ACL logging contained malformed annotation, "+
				"ACL logging is set to deny=%s allow=%s, err: %q",
				newer.Name, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow, err)
		}
		if err := oc.handleNetPolNamespaceUpdate(old.Name, nsInfo); err != nil {
			errors = append(errors, err)
		} else {
			klog.Infof("Namespace %s: NetworkPolicy ACL logging setting updated to deny=%s allow=%s",
				old.Name, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
		}
		// Trigger an egress fw logging update - this will only happen if an egress firewall exists for the NS, otherwise
		// this will not do anything.
		updated, err := oc.updateACLLoggingForEgressFirewall(old.Name, nsInfo)
		if err != nil {
			errors = append(errors, err)
		} else if updated {
			klog.Infof("Namespace %s: EgressFirewall ACL logging setting updated to deny=%s allow=%s",
				old.Name, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
		}
	}

	// OCP HACK -- hybrid overlay
	annotation := newer.Annotations[hotypes.HybridOverlayExternalGw]
	if annotation != "" {
		parsedAnnotation := net.ParseIP(annotation)
		if parsedAnnotation == nil {
			errors = append(errors, fmt.Errorf("could not parse hybrid overlay external gw annotation"))
		} else {
			nsInfo.hybridOverlayExternalGW = parsedAnnotation
		}
	} else {
		nsInfo.hybridOverlayExternalGW = nil
	}
	annotation = newer.Annotations[hotypes.HybridOverlayVTEP]
	if annotation != "" {
		parsedAnnotation := net.ParseIP(annotation)
		if parsedAnnotation == nil {
			errors = append(errors, fmt.Errorf("could not parse hybrid overlay VTEP annotation"))
		} else {
			nsInfo.hybridOverlayVTEP = parsedAnnotation
		}
	} else {
		nsInfo.hybridOverlayVTEP = nil
	}
	// END OCP HACK
	if err := oc.multicastUpdateNamespace(newer, nsInfo); err != nil {
		errors = append(errors, err)
	}
	return kerrors.NewAggregate(errors)
}

func (oc *DefaultNetworkController) deleteNamespace(ns *kapi.Namespace) error {
	klog.Infof("[%s] deleting namespace", ns.Name)

	nsInfo := oc.deleteNamespaceLocked(ns.Name)
	if nsInfo == nil {
		return nil
	}
	defer nsInfo.Unlock()

	if err := oc.deleteGWRoutesForNamespace(ns.Name, nil); err != nil {
		return fmt.Errorf("failed to delete GW routes for namespace: %s, error: %v", ns.Name, err)
	}
	if err := oc.multicastDeleteNamespace(ns, nsInfo); err != nil {
		return fmt.Errorf("failed to delete multicast nameosace error %v", err)
	}
	return nil
}

// ensureNamespaceLocked locks namespacesMutex, gets/creates an entry for ns, configures OVN nsInfo, and returns it
// with its mutex locked.
// ns is the name of the namespace, while namespace is the optional k8s namespace object
// if no k8s namespace object is provided, this function will attempt to find it via informer cache
func (oc *DefaultNetworkController) ensureNamespaceLocked(ns string, readOnly bool, namespace *kapi.Namespace) (*namespaceInfo, func(), error) {
	oc.namespacesMutex.Lock()
	nsInfo := oc.namespaces[ns]
	nsInfoExisted := false
	if nsInfo == nil {
		nsInfo = &namespaceInfo{
			relatedNetworkPolicies: map[string]bool{},
			multicastEnabled:       false,
			routingExternalPodGWs:  make(map[string]gatewayInfo),
			routingExternalGWs:     gatewayInfo{gws: sets.NewString(), bfdEnabled: false},
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
		// if we found an existing nsInfo, do not hold the namespaces lock
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

	// nsInfo and namespace didn't exist, get it from lister
	if namespace == nil {
		var err error
		namespace, err = oc.watchFactory.GetNamespace(ns)
		if err != nil {
			namespace, err = oc.client.CoreV1().Namespaces().Get(context.TODO(), ns, metav1.GetOptions{})
			if err != nil {
				klog.Warningf("Unable to find namespace during ensure in informer cache or kube api server. " +
					"Will defer configuring namespace.")
			}
		}
	}

	if namespace != nil {
		// if we have the namespace, attempt to configure nsInfo with it
		if err := oc.configureNamespace(nsInfo, namespace); err != nil {
			unlockFunc()
			return nil, nil, fmt.Errorf("failed to configure namespace %s: %v", ns, err)
		}
	}

	return nsInfo, unlockFunc, nil
}

func (oc *DefaultNetworkController) createNamespaceAddrSetAllPods(ns string) (addressset.AddressSet, error) {
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
				hostSubnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
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
			if util.PodWantsNetwork(pod) && !util.PodCompleted(pod) && util.PodScheduled(pod) {
				podIPs, err := util.GetPodIPsOfNetwork(pod, oc.NetInfo)
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
