// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	nqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
)

// Namespace labels can change which NADs a policy selects.
func (c *Controller) reconcileNamespace(key string) error {
	startTime := time.Now()
	namespace, err := c.namespaceLister.Get(key)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get namespace %s: %w", key, err)
	}
	// NADs are deleted before their namespace, and their deletion events already requeue
	// policies.
	holdsNADs, err := c.namespaceHoldsNADs(key)
	if err != nil {
		return err
	}
	if !holdsNADs {
		return nil
	}

	nqoses, err := c.nqosLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list NetworkQoS: %w", err)
	}
	for _, nqos := range nqoses {
		if !selectsNADsByNamespace(nqos) {
			continue
		}
		key := nqosKey(nqos.Namespace, nqos.Name)
		switch {
		case namespace != nil && namespaceMatchesNetworkSelector(namespace, nqos):
		// The namespace may have stopped matching. We still need to remove its old rules.
		case c.isProgrammed(key):
		default:
			continue
		}
		c.nqosController.Reconcile(key)
	}
	recordNamespaceReconcileDuration(time.Since(startTime))
	return nil
}

// Wait for any running reconciliation before reading its state, or we could miss a needed requeue.
func (c *Controller) isProgrammed(nqosKey string) bool {
	programmed := false
	_ = c.cache.DoWithLock(nqosKey, func(key string) error {
		entry, loaded := c.cache.Load(key)
		programmed = loaded && entry != nil && len(entry.networks) > 0
		return nil
	})
	return programmed
}

// Only NetworkAttachmentDefinitions selectors use namespace labels.
func selectsNADsByNamespace(nqos *nqosapi.NetworkQoS) bool {
	return slices.ContainsFunc(nqos.Spec.NetworkSelectors, isNADSelector)
}

func isNADSelector(selector crdtypes.NetworkSelector) bool {
	return selector.NetworkSelectionType == crdtypes.NetworkAttachmentDefinitions && selector.NetworkAttachmentDefinitionSelector != nil
}

func namespaceMatchesNetworkSelector(namespace *corev1.Namespace, nqos *nqosapi.NetworkQoS) bool {
	for _, selector := range nqos.Spec.NetworkSelectors {
		if !isNADSelector(selector) {
			continue
		}
		nsSelector := &selector.NetworkAttachmentDefinitionSelector.NamespaceSelector
		if nsSelector.Size() == 0 {
			return true
		}
		if ls, err := metav1.LabelSelectorAsSelector(nsSelector); err != nil {
			klog.Errorf("%s/%s - failed to convert namespace selector %s: %v", nqos.Namespace, nqos.Name, nsSelector.String(), err)
		} else if ls.Matches(labels.Set(namespace.Labels)) {
			return true
		}
	}
	return false
}
