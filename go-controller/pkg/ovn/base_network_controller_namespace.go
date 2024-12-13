package ovn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// portGroupName is a name of a port group, that stores all local zone ports for a given namespace.
	// May be empty if the port group wasn't created.
	portGroupName string

	// Map of related network policies. Policy will add itself to this list when it's ready to subscribe
	// to namespace Update events. Retry logic to update network policy based on namespace event is handled by namespace.
	// Policy should only be added after successful create, and deleted before any network policy resources are deleted.
	// This is the map of keys that can be used to get networkPolicy from oc.networkPolicies.
	//
	// You must hold the namespaceInfo's mutex to add/delete dependent policies.
	// Namespace can take oc.networkPolicies key Lock while holding nsInfo lock, the opposite should never happen.
	relatedNetworkPolicies map[string]bool

	// routingExternalGWs is a slice of net.IP containing the values parsed from
	// annotation k8s.ovn.org/routing-external-gws
	routingExternalGWs gatewayInfo

	// routingExternalPodGWs contains a map of all pods serving as exgws as well as their
	// exgw IPs
	// key is <namespace>_<pod name>
	routingExternalPodGWs map[string]gatewayInfo

	multicastEnabled bool

	// If not empty, then it has to be set to a logging a severity level, e.g. "notice", "alert", etc
	aclLogging libovsdbutil.ACLLoggingLevels
}

func getNamespaceAddrSetDbIDs(namespaceName, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, controller, map[libovsdbops.ExternalIDKey]string{
		// namespace has only 1 address set, no additional ids are required
		libovsdbops.ObjectNameKey: namespaceName,
	})
}

// WatchNamespaces starts the watching of namespace resource and calls
// back the appropriate handler logic
func (bnc *BaseNetworkController) WatchNamespaces() error {
	if bnc.IsPrimaryNetwork() && !util.IsNetworkSegmentationSupportEnabled() {
		// For primary user defined networks, we don't have to watch namespace events if
		// network segmentation support is not enabled.
		return nil
	}

	if bnc.IsSecondary() && !util.IsMultiNetworkPoliciesSupportEnabled() {
		// For secondary networks, we don't have to watch namespace events if
		// multi-network policy support is not enabled.
		return nil
	}

	if bnc.namespaceHandler != nil {
		return nil
	}

	handler, err := bnc.retryNamespaces.WatchResource()
	if err != nil {
		return err
	}
	bnc.namespaceHandler = handler
	return err
}

// aclLoggingUpdateNsInfo parses the provided annotation values and sets nsInfo.aclLogging.Deny and
// nsInfo.aclLogging.Allow. If errors are encountered parsing the annotation, disable logging completely. If either
// value contains invalid input, disable logging for the respective key. This is needed to ensure idempotency.
// More details:
// *) If the provided annotation cannot be unmarshaled: Disable both Deny and Allow logging. Return an error.
// *) Valid values for "allow" and "deny" are  "alert", "warning", "notice", "info", "debug", "".
// *) Invalid values will return an error, and logging will be disabled for the respective key.
// *) In the following special cases, nsInfo.aclLogging.Deny and nsInfo.aclLogging.Allow. will both be reset to ""
//
//	without logging an error, meaning that logging will be switched off:
//	i) oc.aclLoggingEnabled == false
//	ii) annotation == ""
//	iii) annotation == "{}"
//
// *) If one of "allow" or "deny" can be parsed and has a valid value, but the other key is not present in the
//
//	annotation, then assume that this key should be disabled by setting its nsInfo value to "".
func (bnc *BaseNetworkController) aclLoggingUpdateNsInfo(annotation string, nsInfo *namespaceInfo) error {
	var aclLevels libovsdbutil.ACLLoggingLevels
	var errors []error

	// If the annotation is "" or "{}", use empty strings. Otherwise, parse the annotation.
	if annotation != "" && annotation != "{}" {
		err := json.Unmarshal([]byte(annotation), &aclLevels)
		if err != nil {
			// Disable Allow and Deny logging to ensure idempotency.
			nsInfo.aclLogging.Allow = ""
			nsInfo.aclLogging.Deny = ""
			return fmt.Errorf("could not unmarshal namespace ACL annotation '%s', disabling logging, err: %q",
				annotation, err)
		}
	}

	// Valid log levels are the various preestablished levels or the empty string.
	validLogLevels := sets.NewString(nbdb.ACLSeverityAlert, nbdb.ACLSeverityWarning, nbdb.ACLSeverityNotice,
		nbdb.ACLSeverityInfo, nbdb.ACLSeverityDebug, "")

	// Set Deny logging.
	if validLogLevels.Has(aclLevels.Deny) {
		nsInfo.aclLogging.Deny = aclLevels.Deny
	} else {
		errors = append(errors, fmt.Errorf("disabling deny logging due to invalid deny annotation. "+
			"%q is not a valid log severity", aclLevels.Deny))
		nsInfo.aclLogging.Deny = ""
	}

	// Set Allow logging.
	if validLogLevels.Has(aclLevels.Allow) {
		nsInfo.aclLogging.Allow = aclLevels.Allow
	} else {
		errors = append(errors, fmt.Errorf("disabling allow logging due to an invalid allow annotation. "+
			"%q is not a valid log severity", aclLevels.Allow))
		nsInfo.aclLogging.Allow = ""
	}

	return utilerrors.Join(errors...)
}

// This function implements the main body of work of syncNamespaces.
// Upon failure, it may be invoked multiple times in order to avoid a pod restart.
func (bnc *BaseNetworkController) syncNamespaces(namespaces []interface{}) error {
	expectedNs := make(map[string]bool)
	nsWithMulticast := make(map[string]bool)
	for _, nsInterface := range namespaces {
		ns, ok := nsInterface.(*kapi.Namespace)
		if !ok {
			return fmt.Errorf("spurious object in syncNamespaces: %v", nsInterface)
		}
		expectedNs[ns.Name] = true
		if bnc.multicastSupport && isNamespaceMulticastEnabled(ns.Annotations) {
			nsWithMulticast[ns.Name] = true
		}
	}

	err := bnc.addressSetFactory.ProcessEachAddressSet(bnc.controllerName, libovsdbops.AddressSetNamespace,
		func(dbIDs *libovsdbops.DbObjectIDs) error {
			if !expectedNs[dbIDs.GetObjectID(libovsdbops.ObjectNameKey)] {
				if err := bnc.addressSetFactory.DestroyAddressSet(dbIDs); err != nil {
					klog.Errorf(err.Error())
					return err
				}
			}

			return nil
		})
	if err != nil {
		return fmt.Errorf("error in syncing namespaces: %v", err)
	}

	// remove stale port groups
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNamespace, bnc.controllerName, nil)
	p := libovsdbops.GetPredicate[*nbdb.PortGroup](predicateIDs, func(item *nbdb.PortGroup) bool {
		namespaceName := item.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		return !bnc.needNamespacedPortGroup() || !expectedNs[namespaceName]
	})

	err = libovsdbops.DeletePortGroupsWithPredicate(bnc.nbClient, p)
	if err != nil {
		return fmt.Errorf("unable to delete stale namespace port groups: %v", err)
	}

	if bnc.multicastSupport {
		if err = bnc.syncNsMulticast(nsWithMulticast); err != nil {
			return fmt.Errorf("error in syncing multicast for namespaces: %v", err)
		}
	}
	return nil
}

// Creates an explicit "allow" policy for multicast traffic within the
// namespace if multicast is enabled. Otherwise, removes the "allow" policy.
// Traffic will be dropped by the default multicast deny ACL.
func (bnc *BaseNetworkController) multicastUpdateNamespace(ns *kapi.Namespace, nsInfo *namespaceInfo) error {
	if !bnc.multicastSupport {
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
		err = bnc.createMulticastAllowPolicy(ns.Name, nsInfo)
	} else {
		err = bnc.deleteMulticastAllowPolicy(ns.Name)
	}
	if err != nil {
		return err
	}
	return nil
}

// Cleans up the multicast policy for this namespace if multicast was
// previously allowed.
func (bnc *BaseNetworkController) multicastDeleteNamespace(ns *kapi.Namespace, nsInfo *namespaceInfo) error {
	if nsInfo.multicastEnabled {
		nsInfo.multicastEnabled = false
		if err := bnc.deleteMulticastAllowPolicy(ns.Name); err != nil {
			return err
		}
	}
	return nil
}

// ensureNamespaceLockedCommon is a shared function used by both default/secondary network controllers,
// it locks namespacesMutex, gets/creates an entry for ns, and returns it with nsInfo's mutex locked.
// ns is the name of the namespace, while namespace is the optional k8s namespace object
// if no k8s namespace object is provided, this function will attempt to find it via informer cache
func (bnc *BaseNetworkController) ensureNamespaceLockedCommon(ns string, readOnly bool, namespace *kapi.Namespace,
	ipsGetter func(ns string) []net.IP, configureNamespace func(nsInfo *namespaceInfo, ns *kapi.Namespace) error) (*namespaceInfo, func(), error) {
	bnc.namespacesMutex.Lock()
	nsInfo := bnc.namespaces[ns]
	nsInfoExisted := false
	if nsInfo == nil {
		nsInfo = &namespaceInfo{
			relatedNetworkPolicies: map[string]bool{},
			multicastEnabled:       false,
			routingExternalPodGWs:  make(map[string]gatewayInfo),
			routingExternalGWs:     gatewayInfo{gws: sets.New[string](), bfdEnabled: false},
		}
		// we are creating nsInfo and going to set it in namespaces map
		// so safe to hold the lock while we create and add it
		defer bnc.namespacesMutex.Unlock()
		// create the adddress set for the new namespace
		var err error
		ips := ipsGetter(ns)
		nsInfo.addressSet, err = bnc.createNamespaceAddrSetAllPods(ns, ips)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create address set for namespace: %s, error: %v", ns, err)
		}

		// namespace port groups are only used by egress firewall and multicast for now
		if bnc.needNamespacedPortGroup() {
			portGroupName, err := bnc.createNamespacePortGroup(ns)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to create port group for namespace: %s, error: %v", ns, err)
			}
			nsInfo.portGroupName = portGroupName
		}
		bnc.namespaces[ns] = nsInfo
	} else {
		nsInfoExisted = true
		// if we found an existing nsInfo, do not hold the namespaces lock
		// while waiting for nsInfo to Lock
		bnc.namespacesMutex.Unlock()
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
		bnc.namespacesMutex.Lock()
		defer bnc.namespacesMutex.Unlock()
		if nsInfo != bnc.namespaces[ns] {
			unlockFunc()
			return nil, nil, fmt.Errorf("namespace %s, was removed during ensure", ns)
		}
	}

	// nsInfo and namespace didn't exist, get it from lister
	if namespace == nil {
		var err error
		namespace, err = bnc.watchFactory.GetNamespace(ns)
		if err != nil {
			namespace, err = bnc.client.CoreV1().Namespaces().Get(context.TODO(), ns, metav1.GetOptions{})
			if err != nil {
				klog.Warningf("Unable to find namespace during ensure in informer cache or kube api server. " +
					"Will defer configuring namespace.")
			}
		}
	}

	if namespace != nil {
		// if we have the namespace, attempt to configure nsInfo with it
		if err := configureNamespace(nsInfo, namespace); err != nil {
			unlockFunc()
			return nil, nil, fmt.Errorf("failed to configure namespace %s: %v", ns, err)
		}
	}

	return nsInfo, unlockFunc, nil
}

func (bnc *BaseNetworkController) needNamespacedPortGroup() bool {
	// namespace port groups are only used by egress firewall and multicast for now
	return bnc.multicastSupport || config.OVNKubernetesFeature.EnableEgressFirewall
}

func (bnc *BaseNetworkController) configureNamespaceCommon(nsInfo *namespaceInfo, ns *kapi.Namespace) error {
	if annotation, ok := ns.Annotations[util.AclLoggingAnnotation]; ok {
		if err := bnc.aclLoggingUpdateNsInfo(annotation, nsInfo); err == nil {
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
	if err := bnc.multicastUpdateNamespace(ns, nsInfo); err != nil {
		return fmt.Errorf("failed to update multicast (%v)", err)
	}
	return nil
}

// GetNamespaceACLLogging retrieves ACLLoggingLevels for the Namespace.
// nsInfo will be locked (and unlocked at the end) for given namespace if it exists.
func (bnc *BaseNetworkController) GetNamespaceACLLogging(ns string) *libovsdbutil.ACLLoggingLevels {
	nsInfo, nsUnlock := bnc.getNamespaceLocked(ns, true)
	if nsInfo == nil {
		return &libovsdbutil.ACLLoggingLevels{
			Allow: "",
			Deny:  "",
		}
	}
	defer nsUnlock()
	return &nsInfo.aclLogging
}

func (bnc *BaseNetworkController) updateNamespaceAclLogging(ns, aclAnnotation string, nsInfo *namespaceInfo) error {
	// When input cannot be parsed correctly, aclLoggingUpdateNsInfo disables logging and returns an error. Hence,
	// log a warning to make users aware of issues with the annotation. See aclLoggingUpdateNsInfo for more details.
	if err := bnc.aclLoggingUpdateNsInfo(aclAnnotation, nsInfo); err != nil {
		klog.Warningf("Namespace %s: ACL logging contained malformed annotation, "+
			"ACL logging is set to deny=%s allow=%s, err: %q",
			ns, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow, err)
	}
	if err := bnc.handleNetPolNamespaceUpdate(ns, nsInfo); err != nil {
		return err
	} else {
		klog.Infof("Namespace %s: NetworkPolicy ACL logging setting updated to deny=%s allow=%s",
			ns, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
	}
	return nil
}

func (bnc *BaseNetworkController) getAllNamespacePodAddresses(ns string) []net.IP {
	if !bnc.doesNetworkRequireIPAM() {
		return nil
	}

	var ips []net.IP
	// Get all the pods in the namespace and append their IP to the address_set
	existingPods, err := bnc.watchFactory.GetPods(ns)
	if err != nil {
		klog.Errorf("Failed to get all the pods (%v)", err)
	} else {
		ips = make([]net.IP, 0, len(existingPods))
		for _, pod := range existingPods {
			if !util.PodWantsHostNetwork(pod) && !util.PodCompleted(pod) && util.PodScheduled(pod) {
				podIPs, err := util.GetPodIPsOfNetwork(pod, bnc.GetNetInfo())
				if err != nil {
					klog.Warningf(err.Error())
					continue
				}
				ips = append(ips, podIPs...)
			}
		}
	}
	return ips
}

func (bnc *BaseNetworkController) createNamespaceAddrSetAllPods(ns string, ips []net.IP) (addressset.AddressSet, error) {
	dbIDs := getNamespaceAddrSetDbIDs(ns, bnc.controllerName)
	ipSet := util.StringSlice(ips)
	return bnc.addressSetFactory.NewAddressSet(dbIDs, ipSet)
}

// createNamespacePortGroup should only create a port group if it doesn't exist already,
// all ports and acls will be added by pod/multicast/egressfirewall/etc handlers.
func (bnc *BaseNetworkController) createNamespacePortGroup(ns string) (string, error) {
	pgIDs := getNamespacePortGroupDbIDs(ns, bnc.controllerName)
	// create empty port group if it doesn't exist
	pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
	err := libovsdbops.CreatePortGroup(bnc.nbClient, pg)

	return pg.Name, err
}

func getNamespacePortGroupDbIDs(ns string, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNamespace, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ns,
		})
}

func (bnc *BaseNetworkController) getNamespacePortGroupName(namespace string) string {
	return libovsdbutil.GetPortGroupName(getNamespacePortGroupDbIDs(namespace, bnc.controllerName))
}

// removeRemoteZonePodFromNamespaceAddressset tries to remove the remote zone pod ips from the pod namespace address set.
// failure indicates it should be retried later.
func (bsnc *BaseNetworkController) removeRemoteZonePodFromNamespaceAddressSet(pod *kapi.Pod) error {
	podDesc := fmt.Sprintf("pod %s/%s/%s", bsnc.GetNetworkName(), pod.Namespace, pod.Name)
	podIfAddrs, err := util.GetPodCIDRsWithFullMask(pod, bsnc.GetNetInfo())
	if err != nil {
		// maybe the pod is not scheduled yet or addLSP has not happened yet, so it doesn't have IPs.
		// let us ignore deletion failures for podIPs not found because
		// there is nothing more we can do here.
		if errors.Is(err, util.ErrNoPodIPFound) {
			klog.Warningf("Unable to remove remote zone pod's %s/%s IP address from the "+
				"namespace address-set, err: %v", pod.Namespace, pod.Name, err)
			return nil
		}
		return fmt.Errorf("failed to get pod ips for the pod  %s/%s : %w", pod.Namespace, pod.Name, err)
	}

	// If this pod applies to live migration it could have migrated so get the
	// correct node name corresponding with the subnet. If the subnet is not
	// tracked within the zone, nodeName will be empty which will force
	// canReleasePodIPs to lookup all nodes.
	nodeName := pod.Spec.NodeName
	if !bsnc.IsSecondary() && kubevirt.IsPodLiveMigratable(pod) {
		nodeName, _ = bsnc.lsManager.GetSubnetName(podIfAddrs)
	}

	// Remove the pod ips from the namespace address set. Before that check if its a completed pod and
	// make sure that the ips are not colliding with other pod.
	shouldRelease, err := bsnc.canReleasePodIPs(podIfAddrs, nodeName)
	if err != nil {
		return err
	}

	if !shouldRelease {
		klog.Infof("Cannot release IP address: %s for %s/%s from namespace address set. Detected another pod"+
			" using this IP", util.JoinIPNetIPs(podIfAddrs, " "), pod.Namespace, pod.Name)
		return nil
	}

	ops, err := bsnc.deletePodFromNamespace(pod.Namespace, podIfAddrs, "")
	if err != nil {
		return fmt.Errorf("failed to delete remote pod %s's IP from namespace: %w", podDesc, err)
	}

	_, err = libovsdbops.TransactAndCheck(bsnc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("could not delete remote pod IPs from the namespace address set - %w", err)
	}

	return nil
}
