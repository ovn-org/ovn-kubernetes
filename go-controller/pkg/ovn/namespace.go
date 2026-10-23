// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

func isNamespaceMulticastEnabled(annotations map[string]string) bool {
	return annotations[util.NsMulticastAnnotation] == "true"
}

// AddNamespace creates corresponding addressset in ovn db
func (oc *DefaultNetworkController) AddNamespace(ns *corev1.Namespace) error {
	klog.Infof("[%s] adding namespace", ns.Name)
	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s] adding namespace took %v", ns.Name, time.Since(start))
	}()

	_, nsUnlock, err := oc.ensureNamespaceLocked(ns.Name, false, ns)
	if err != nil {
		return fmt.Errorf("failed to ensure namespace locked: %v", err)
	}
	defer nsUnlock()
	return nil
}

// configureNamespace ensures internal structures are updated based on namespace
// must be called with nsInfo lock
func (oc *DefaultNetworkController) configureNamespace(ctx context.Context, nsInfo *namespaceInfo, ns *corev1.Namespace) error {
	var errors []error

	if err := oc.configureNamespaceCommon(ctx, nsInfo, ns); err != nil {
		errors = append(errors, err)
	}
	return utilerrors.Join(errors...)
}

func (oc *DefaultNetworkController) updateNamespace(old, newer *corev1.Namespace) error {
	var errors []error
	klog.Infof("[%s] updating namespace", old.Name)

	nsInfo, nsUnlock := oc.getNamespaceLocked(old.Name, false)
	if nsInfo == nil {
		klog.Warningf("Update event for unknown namespace %q", old.Name)
		return nil
	}
	defer nsUnlock()

	aclAnnotation := newer.Annotations[util.AclLoggingAnnotation]
	oldACLAnnotation := old.Annotations[util.AclLoggingAnnotation]
	// support for ACL logging update, if new annotation is empty, make sure we propagate new setting
	if aclAnnotation != oldACLAnnotation {
		if err := oc.updateNamespaceAclLogging(old.Name, aclAnnotation, nsInfo); err != nil {
			errors = append(errors, err)
		}
		if oc.efController != nil {
			// Trigger an egress fw logging update - this will only happen if an egress firewall exists for the NS, otherwise
			// this will not do anything.
			egressFirewalls, err := oc.watchFactory.EgressFirewallInformer().Lister().EgressFirewalls(old.Name).List(labels.Everything())
			if err != nil {
				errors = append(errors, err)
			}
			for _, fw := range egressFirewalls {
				fwKey, err := cache.MetaNamespaceKeyFunc(fw)
				if err != nil {
					klog.Errorf("Failed to get key for EgressFirewall %s/%s, will not update ACL logging: %v", old.Name, fwKey, err)
					continue
				}
				klog.Infof("Namespace %s: EgressFirewall ACL logging setting updating to deny=%s allow=%s",
					old.Name, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
				oc.efController.Reconcile(fwKey)
			}
		}
	}

	if err := oc.multicastUpdateNamespace(newer, nsInfo); err != nil {
		errors = append(errors, err)
	}
	return utilerrors.Join(errors...)
}

func (oc *DefaultNetworkController) deleteNamespace(ns *corev1.Namespace) error {
	klog.Infof("[%s] deleting namespace", ns.Name)

	nsInfo, err := oc.deleteNamespaceLocked(ns.Name)
	if err != nil {
		return err
	}
	if nsInfo == nil {
		return nil
	}
	defer nsInfo.Unlock()

	if err := oc.deleteGWRoutesForNamespace(ns.Name, nil); err != nil {
		return fmt.Errorf("failed to delete GW routes for namespace: %s, error: %v", ns.Name, err)
	}
	if err := oc.multicastDeleteNamespace(ns, nsInfo); err != nil {
		return fmt.Errorf("failed to delete multicast namespace error %v", err)
	}
	return nil
}

// ensureNamespaceLocked locks namespacesMutex, gets/creates an entry for ns, configures OVN nsInfo, and returns it
// with its mutex locked.
// ns is the name of the namespace, while namespace is the optional k8s namespace object
func (oc *DefaultNetworkController) ensureNamespaceLocked(ns string, readOnly bool, namespace *corev1.Namespace) (*namespaceInfo, func(), error) {
	return oc.ensureNamespaceLockedCommon(ns, readOnly, namespace, oc.configureNamespace)
}
