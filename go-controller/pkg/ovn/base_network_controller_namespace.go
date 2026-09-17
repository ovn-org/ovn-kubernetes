// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// namespaceInfo contains information related to a Namespace. Use oc.getNamespaceLocked()
// or oc.waitForNamespaceLocked() to get a locked namespaceInfo for a Namespace, and call
// nsInfo.Unlock() on it when you are done with it. (No code outside of the code that
// manages the oc.namespaces map is ever allowed to hold an unlocked namespaceInfo.)
type namespaceInfo struct {
	sync.RWMutex
	// we don't create namespace from the namespace handler anymore, instead using addressset_manager
	// only multicast uses and manages this address set
	addrSetOwnerBackref          string
	addrSetNameV4, addrSetNameV6 string

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

	multicastEnabled bool

	// If not empty, then it has to be set to a logging a severity level, e.g. "notice", "alert", etc
	aclLogging libovsdbutil.ACLLoggingLevels
}

func (bnc *BaseNetworkController) shouldWatchNamespaces() bool {
	// Watch namespaces only if one of the following conditions is met:
	// - The network is the default network.
	// - The network is primary, and network segmentation is enabled.
	// - The network is secondary, and multi NetworkPolicies are enabled.
	return bnc.IsDefault() ||
		bnc.IsPrimaryNetwork() && util.IsNetworkSegmentationSupportEnabled() ||
		bnc.IsUserDefinedNetwork() && util.IsMultiNetworkPoliciesSupportEnabled()
}

// WatchNamespaces starts the watching of namespace resource and calls
// back the appropriate handler logic
func (bnc *BaseNetworkController) WatchNamespaces() error {
	if !bnc.shouldWatchNamespaces() {
		klog.Infof("Ignoring namespaces events for network: %s", bnc.GetNetworkName())
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
		ns, ok := nsInterface.(*corev1.Namespace)
		if !ok {
			return fmt.Errorf("spurious object in syncNamespaces: %v", nsInterface)
		}
		if bnc.shouldFilterNamespace(ns.Name) {
			continue
		}
		expectedNs[ns.Name] = true
		if bnc.multicastSupport && isNamespaceMulticastEnabled(ns.Annotations) {
			nsWithMulticast[ns.Name] = true
		}
	}

	// remove stale port groups
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNamespace, bnc.controllerName, nil)
	p := libovsdbops.GetPredicate[*nbdb.PortGroup](predicateIDs, func(item *nbdb.PortGroup) bool {
		namespaceName := item.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		return !bnc.needNamespacedPortGroup() || !expectedNs[namespaceName]
	})

	err := libovsdbops.DeletePortGroupsWithPredicate(bnc.nbClient, p)
	if err != nil {
		return fmt.Errorf("unable to delete stale namespace port groups: %v", err)
	}

	if bnc.multicastSupport {
		if err = bnc.syncNsMulticast(nsWithMulticast); err != nil {
			return fmt.Errorf("error in syncing multicast for namespaces: %v", err)
		}
	}
	// clean up deprecated namespace-owned address sets
	predicateIDs = libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, bnc.controllerName, nil)
	return libovsdbutil.DeleteAddrSetsWithoutACLRef(predicateIDs, bnc.nbClient)
}

// Creates an explicit "allow" policy for multicast traffic within the
// namespace if multicast is enabled. Otherwise, removes the "allow" policy.
// Traffic will be dropped by the default multicast deny ACL.
func (bnc *BaseNetworkController) multicastUpdateNamespace(ns *corev1.Namespace, nsInfo *namespaceInfo) error {
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
		err = bnc.deleteMulticastAllowPolicy(ns.Name, nsInfo)
	}
	if err != nil {
		return err
	}
	return nil
}

// Cleans up the multicast policy for this namespace if multicast was
// previously allowed.
func (bnc *BaseNetworkController) multicastDeleteNamespace(ns *corev1.Namespace, nsInfo *namespaceInfo) error {
	if nsInfo.multicastEnabled {
		nsInfo.multicastEnabled = false
		if err := bnc.deleteMulticastAllowPolicy(ns.Name, nsInfo); err != nil {
			return err
		}
	}
	return nil
}

// ensureNamespaceLockedCommon is a shared function used by both default/secondary network controllers,
// it locks namespacesMutex, gets/creates an entry for ns, and returns it with nsInfo's mutex locked.
// ns is the name of the namespace, while namespace is the optional k8s namespace object
// if no k8s namespace object is provided, this function will attempt to find it via informer cache
func (bnc *BaseNetworkController) ensureNamespaceLockedCommon(ns string, readOnly bool, namespace *corev1.Namespace,
	configureNamespace func(nsInfo *namespaceInfo, ns *corev1.Namespace) error) (*namespaceInfo, func(), error) {
	bnc.namespacesMutex.Lock()
	nsInfo := bnc.namespaces[ns]
	nsInfoExisted := false
	if nsInfo == nil {
		nsInfo = &namespaceInfo{
			relatedNetworkPolicies: map[string]bool{},
			multicastEnabled:       false,
		}
		// we are creating nsInfo and going to set it in namespaces map
		// so safe to hold the lock while we create and add it
		defer bnc.namespacesMutex.Unlock()
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

func (bnc *BaseNetworkController) configureNamespaceCommon(nsInfo *namespaceInfo, ns *corev1.Namespace) error {
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
