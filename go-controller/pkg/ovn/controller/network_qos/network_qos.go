// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"context"
	"fmt"
	"sync"
	"time"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	networkqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	nqosapiapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/applyconfiguration/networkqos/v1alpha1"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func (c *Controller) processNextNQOSWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	nqosKey, quit := c.nqosQueue.Get()
	if quit {
		return false
	}
	defer c.nqosQueue.Done(nqosKey)

	if err := c.syncNetworkQoS(nqosKey); err != nil {
		if c.nqosQueue.NumRequeues(nqosKey) < maxRetries {
			c.nqosQueue.AddRateLimited(nqosKey)
			return true
		}
		klog.Warningf("%s: Failed to reconcile NetworkQoS %s: %v", c.controllerName, nqosKey, err)
		utilruntime.HandleError(fmt.Errorf("failed to reconcile NetworkQoS %s: %v", nqosKey, err))
	}
	c.nqosQueue.Forget(nqosKey)
	return true
}

// syncNetworkQoS decides the main logic everytime
// we dequeue a key from the nqosQueue cache
func (c *Controller) syncNetworkQoS(key string) error {
	nqosNamespace, nqosName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	startTime := time.Now()
	c.nqosCache.LockKey(key)
	defer func() {
		c.nqosCache.UnlockKey(key)
		klog.V(5).Infof("%s - Finished reconciling NetworkQoS %s : %v", c.controllerName, key, time.Since(startTime))
	}()
	klog.V(5).Infof("%s - reconciling NetworkQoS %s", c.controllerName, key)
	nqos, err := c.nqosLister.NetworkQoSes(nqosNamespace).Get(nqosName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if nqos == nil || !nqos.DeletionTimestamp.IsZero() {
		klog.V(6).Infof("%s - NetworkQoS %s is being deleted.", c.controllerName, key)
		return c.clearNetworkQos(nqosNamespace, nqosName)
	}

	if networkManagedByMe, err := c.networkManagedByMe(nqos.Spec.NetworkSelectors); err != nil {
		return err
	} else if !networkManagedByMe {
		// maybe NetworkAttachmentName has been changed from this one to other value, try cleanup anyway
		return c.clearNetworkQos(nqos.Namespace, nqos.Name)
	}

	klog.V(5).Infof("%s - Processing NetworkQoS %s/%s", c.controllerName, nqos.Namespace, nqos.Name)
	if err := c.ensureNetworkQos(nqos); err != nil {
		c.nqosCache.Delete(key)
		// we can ignore the error if status update doesn't succeed; best effort
		c.updateNQOSStatusToNotReady(nqos.Namespace, nqos.Name, "failed to reconcile", err)
		return err
	}
	recordNetworkQoSReconcileDuration(c.controllerName, time.Since(startTime).Milliseconds())
	updateNetworkQoSCount(c.controllerName, len(c.nqosCache.GetKeys()))
	return nil
}

// ensureNetworkQos will handle the main reconcile logic for any given nqos's
// add/update that might be triggered either due to NQOS changes or the corresponding
// matching pod or namespace changes.
// This function need to be called with a lock held.
func (c *Controller) ensureNetworkQos(nqos *networkqosapi.NetworkQoS) error {
	desiredNQOSState := &networkQoSState{
		name:      nqos.Name,
		namespace: nqos.Namespace,
	}

	if len(nqos.Spec.PodSelector.MatchLabels) > 0 || len(nqos.Spec.PodSelector.MatchExpressions) > 0 {
		if podSelector, err := metav1.LabelSelectorAsSelector(&nqos.Spec.PodSelector); err != nil {
			c.updateNQOSStatusToNotReady(nqos.Namespace, nqos.Name, "failed to parse source pod selector", err)
			return nil
		} else {
			desiredNQOSState.PodSelector = podSelector
		}
	}

	// set EgressRules to desiredNQOSState
	rules := []*GressRule{}
	for index, ruleSpec := range nqos.Spec.Egress {
		bwRate := int(ruleSpec.Bandwidth.Rate)
		bwBurst := int(ruleSpec.Bandwidth.Burst)
		ruleState := &GressRule{
			Priority: getQoSRulePriority(nqos.Spec.Priority, index),
			Dscp:     ruleSpec.DSCP,
		}
		if bwRate > 0 {
			ruleState.Rate = &bwRate
		}
		if bwBurst > 0 {
			ruleState.Burst = &bwBurst
		}
		destStates := []*Destination{}
		for _, destSpec := range ruleSpec.Classifier.To {
			if destSpec.IPBlock != nil && (destSpec.PodSelector != nil || destSpec.NamespaceSelector != nil) {
				return fmt.Errorf("specifying both ipBlock and podSelector/namespaceSelector is not allowed")
			}
			destState := &Destination{}
			destState.IpBlock = destSpec.IPBlock.DeepCopy()
			if destSpec.NamespaceSelector != nil && (len(destSpec.NamespaceSelector.MatchLabels) > 0 || len(destSpec.NamespaceSelector.MatchExpressions) > 0) {
				if selector, err := metav1.LabelSelectorAsSelector(destSpec.NamespaceSelector); err != nil {
					return fmt.Errorf("error parsing destination namespace selector: %v", err)
				} else {
					destState.NamespaceSelector = selector
				}
			}
			if destSpec.PodSelector != nil && (len(destSpec.PodSelector.MatchLabels) > 0 || len(destSpec.PodSelector.MatchExpressions) > 0) {
				if selector, err := metav1.LabelSelectorAsSelector(destSpec.PodSelector); err != nil {
					return fmt.Errorf("error parsing destination pod selector: %v", err)
				} else {
					destState.PodSelector = selector
				}
			}
			destStates = append(destStates, destState)
		}
		ruleState.Classifier = &Classifier{
			Destinations: destStates,
		}
		ruleState.Classifier.Ports = ruleSpec.Classifier.Ports
		rules = append(rules, ruleState)
	}
	desiredNQOSState.EgressRules = rules
	if err := desiredNQOSState.initAddressSets(c.addressSetFactory, c.controllerName); err != nil {
		return err
	}
	if err := c.resyncPods(desiredNQOSState); err != nil {
		return fmt.Errorf("failed to resync pods: %w", err)
	}
	// delete stale rules left from previous NetworkQoS definition, along with the address sets
	if err := c.cleanupStaleOvnObjects(desiredNQOSState); err != nil {
		return fmt.Errorf("failed to delete stale QoSes: %w", err)
	}
	c.nqosCache.Store(joinMetaNamespaceAndName(nqos.Namespace, nqos.Name), desiredNQOSState)
	if e := c.updateNQOSStatusToReady(nqos.Namespace, nqos.Name); e != nil {
		return fmt.Errorf("successfully reconciled NetworkQoS %s/%s, but failed to patch status: %v", nqos.Namespace, nqos.Name, e)
	}
	return nil
}

// clearNetworkQos will handle the logic for deleting all db objects related
// to the provided nqos which got deleted. it looks up object in OVN by comparing
// the nqos name with the metadata in externalIDs.
// this function need to be called with a lock held.
func (c *Controller) clearNetworkQos(nqosNamespace, nqosName string) error {
	k8sFullName := joinMetaNamespaceAndName(nqosNamespace, nqosName)
	ovnObjectName := joinMetaNamespaceAndName(nqosNamespace, nqosName, ":")

	klog.V(4).Infof("%s - try cleaning up networkqos %s", c.controllerName, k8sFullName)
	// remove NBDB objects by NetworkQoS name
	if err := c.deleteByName(ovnObjectName); err != nil {
		return fmt.Errorf("failed to delete QoS rules for NetworkQoS %s: %w", k8sFullName, err)
	}
	c.nqosCache.Delete(k8sFullName)
	updateNetworkQoSCount(c.controllerName, len(c.nqosCache.GetKeys()))
	if config.OVNKubernetesFeature.EnableStatusMetrics {
		metrics.DeleteNetworkQoSSyncSucceeded(c.nodeName, nqosNamespace, nqosName)
	}
	return nil
}

const (
	conditionTypeReady    = "Ready-In-Zone-"
	reasonQoSSetupSuccess = "Success"
	reasonQoSSetupFailed  = "Failed"
)

func (c *Controller) updateNQOSStatusToReady(namespace, name string) error {
	cond := metav1.Condition{
		Type:    conditionTypeReady + c.nodeName,
		Status:  metav1.ConditionTrue,
		Reason:  reasonQoSSetupSuccess,
		Message: "NetworkQoS was applied successfully",
	}
	startTime := time.Now()
	err := c.updateNQOStatusCondition(cond, namespace, name)
	if err != nil {
		return fmt.Errorf("failed to update the status of NetworkQoS %s/%s, err: %v", namespace, name, err)
	}
	klog.V(5).Infof("%s: successfully patched the status of NetworkQoS %s/%s with condition type %v/%v in %v seconds",
		c.controllerName, namespace, name, conditionTypeReady+c.nodeName, metav1.ConditionTrue, time.Since(startTime).Seconds())
	recordStatusPatchDuration(c.controllerName, time.Since(startTime).Milliseconds())
	return nil
}

func (c *Controller) updateNQOSStatusToNotReady(namespace, name, reason string, err error) {
	msg := reason
	if err != nil {
		msg = fmt.Sprintf("NetworkQoS %s/%s - %s, error details: %v", namespace, name, reason, err)
	}
	cond := metav1.Condition{
		Type:    conditionTypeReady + c.nodeName,
		Status:  metav1.ConditionFalse,
		Reason:  reasonQoSSetupFailed,
		Message: msg,
	}
	klog.Error(msg)
	startTime := time.Now()
	err = c.updateNQOStatusCondition(cond, namespace, name)
	if err != nil {
		klog.Warningf("%s: failed to update the status of NetworkQoS %s/%s, err: %v", c.controllerName, namespace, name, err)
	} else {
		klog.V(6).Infof("%s: successfully patched status of NetworkQoS %s/%s with condition type %v/%v in %v seconds", c.controllerName, namespace, name, conditionTypeReady+c.nodeName, metav1.ConditionTrue, time.Since(startTime).Seconds())
		recordStatusPatchDuration(c.controllerName, time.Since(startTime).Milliseconds())
	}
}

func (c *Controller) updateNQOStatusCondition(newCondition metav1.Condition, namespace, name string) error {
	if config.OVNKubernetesFeature.EnableStatusMetrics {
		metrics.SetNetworkQoSSyncSucceeded(c.nodeName, namespace, name, newCondition.Status == metav1.ConditionTrue)
		return nil
	}

	nqos, err := c.nqosLister.NetworkQoSes(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Resource was deleted, log it
			klog.V(5).Infof("NetworkQoS %s/%s updating status but not found, ignoring", namespace, name)
			return nil
		}
		return err
	}

	existingCondition := meta.FindStatusCondition(nqos.Status.Conditions, newCondition.Type)
	newConditionApply := &metaapplyv1.ConditionApplyConfiguration{
		Type:               &newCondition.Type,
		Status:             &newCondition.Status,
		ObservedGeneration: &newCondition.ObservedGeneration,
		Reason:             &newCondition.Reason,
		Message:            &newCondition.Message,
	}

	if existingCondition == nil || existingCondition.Status != newCondition.Status {
		newConditionApply.LastTransitionTime = ptr.To(metav1.NewTime(time.Now()))
	} else {
		newConditionApply.LastTransitionTime = &existingCondition.LastTransitionTime
	}

	applyObj := nqosapiapply.NetworkQoS(name, namespace).
		WithStatus(nqosapiapply.Status().WithConditions(newConditionApply))
	_, err = c.nqosClientSet.K8sV1alpha1().NetworkQoSes(namespace).ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: c.nodeName, Force: true})
	return err
}

func (c *Controller) resyncPods(nqosState *networkQoSState) error {
	pods, err := c.nqosPodLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods in namespace %s: %w", nqosState.namespace, err)
	}
	nsCache := make(map[string]*corev1.Namespace)
	addressSetMap := map[string]sets.Set[string]{}
	for _, pod := range pods {
		if pod.Spec.HostNetwork || pod.DeletionTimestamp != nil {
			continue
		}
		ns := nsCache[pod.Namespace]
		if ns == nil {
			ns, err = c.nqosNamespaceLister.Get(pod.Namespace)
			if err != nil {
				if apierrors.IsNotFound(err) {
					klog.Warningf("Namespace %s not found, skipping pod %s/%s", pod.Namespace, pod.Namespace, pod.Name)
					continue
				}
				return fmt.Errorf("failed to get namespace %s: %w", pod.Namespace, err)
			}
			nsCache[pod.Namespace] = ns
		}
		if ns.DeletionTimestamp != nil {
			continue
		}
		if err := c.setPodForNQOS(pod, nqosState, ns, addressSetMap); err != nil {
			return err
		}
	}
	return nqosState.cleanupStaleAddresses(addressSetMap)
}

var cudnController = udnv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork")

// networkManagedByMe determines if any of the networks specified in the networkSelectors are managed by this controller.
// It returns true if:
// - Multi-network is disabled (nadLister is nil) and this is the default network controller
// - No selectors are provided and this is the default network controller
// - Any of the selected networks match one of these criteria:
//   - The selector is for the default network and this is the default network controller
//   - The selector is for cluster user defined networks (CUDNs) and any of the matching NADs are controlled by a CUDN
//   - The selector is for network attachment definitions (NADs) and any of the matching NADs are managed by this controller
//
// Returns an error if:
// - Any of the network selectors are invalid or empty
// - There is an error listing network attachment definitions
func (c *Controller) networkManagedByMe(networkSelectors crdtypes.NetworkSelectors) (bool, error) {
	// return c.IsDefault() if multi-network is disabled or no selectors is provided in spec
	if c.nadLister == nil || len(networkSelectors) == 0 {
		return c.IsDefault(), nil
	}
	var selectedNads []*nadv1.NetworkAttachmentDefinition
	var err error
	for _, networkSelector := range networkSelectors {
		switch networkSelector.NetworkSelectionType {
		case crdtypes.DefaultNetwork:
			return c.IsDefault(), nil
		case crdtypes.PrimaryUserDefinedNetworks:
			if !c.IsPrimaryNetwork() {
				return false, nil
			}
			if networkSelector.PrimaryUserDefinedNetworkSelector == nil {
				return false, fmt.Errorf("empty primary user defined network selector")
			}
			selectedNads, err = c.getNetAttachDefsByNamespace(&networkSelector.PrimaryUserDefinedNetworkSelector.NamespaceSelector)
			if err != nil {
				return false, err
			}
		case crdtypes.SecondaryUserDefinedNetworks:
			if !c.IsUserDefinedNetwork() {
				return false, nil
			}
			if networkSelector.SecondaryUserDefinedNetworkSelector == nil {
				return false, fmt.Errorf("empty secondary user defined network selector")
			}
			selectedNads, err = c.getNetAttachDefsBySelectors(&networkSelector.SecondaryUserDefinedNetworkSelector.NamespaceSelector, &networkSelector.SecondaryUserDefinedNetworkSelector.NetworkSelector)
			if err != nil {
				return false, err
			}
		case crdtypes.ClusterUserDefinedNetworks:
			if networkSelector.ClusterUserDefinedNetworkSelector == nil {
				return false, fmt.Errorf("empty cluster user defined network selector")
			}
			nads, err := c.getNetAttachDefsBySelectors(nil, &networkSelector.ClusterUserDefinedNetworkSelector.NetworkSelector)
			if err != nil {
				return false, err
			}
			for _, nad := range nads {
				// check this NAD is controlled by a CUDN
				controller := metav1.GetControllerOfNoCopy(nad)
				isCUDN := controller != nil && controller.Kind == cudnController.Kind && controller.APIVersion == cudnController.GroupVersion().String()
				if !isCUDN {
					continue
				}
				selectedNads = append(selectedNads, nad)
			}
		case crdtypes.NetworkAttachmentDefinitions:
			if networkSelector.NetworkAttachmentDefinitionSelector == nil {
				return false, fmt.Errorf("empty network attachment definition selector")
			}
			selectedNads, err = c.getNetAttachDefsBySelectors(&networkSelector.NetworkAttachmentDefinitionSelector.NamespaceSelector, &networkSelector.NetworkAttachmentDefinitionSelector.NetworkSelector)
			if err != nil {
				return false, err
			}
		default:
			return false, fmt.Errorf("unsupported network selection type %s", networkSelector.NetworkSelectionType)
		}
	}
	if len(selectedNads) == 0 {
		return false, nil
	}
	for _, nad := range selectedNads {
		networkName := util.GetAnnotatedNetworkName(nad)
		if networkName == "" {
			nadInfo, err := util.ParseNADInfo(nad)
			if err == nil && nadInfo != nil {
				networkName = nadInfo.GetNetworkName()
			}
		}
		if networkName == "" {
			continue
		}
		if networkName == types.DefaultNetworkName && c.IsDefault() {
			return true, nil
		}
		if c.IsDefault() {
			continue
		}
		if networkName == c.GetNetworkName() {
			return true, nil
		}
	}
	return false, nil
}

func (c *Controller) getLogicalSwitchName(nodeName string) string {
	switch {
	case c.TopologyType() == types.Layer2Topology:
		return c.GetNetworkScopedSwitchName(types.OVNLayer2Switch)
	case c.TopologyType() == types.LocalnetTopology:
		return c.GetNetworkScopedSwitchName(types.OVNLocalnetSwitch)
	case !c.IsUserDefinedNetwork() || c.TopologyType() == types.Layer3Topology:
		return c.GetNetworkScopedSwitchName(nodeName)
	default:
		return ""
	}
}

// hasNetworkQoS checks desired policy state, including policies that have not
// reconciled yet. Events skipped while the informer is empty are covered by
// resyncPods when the first policy reconciles. Do not use nqosCache here: a pod
// change during the first reconciliation must still enqueue work.
func (c *Controller) hasNetworkQoS() bool {
	nqoses, err := c.getAllNetworkQoSes()
	// Preserve event processing on lookup errors so workers can retry.
	return err != nil || len(nqoses) != 0
}

func (c *Controller) getAllNetworkQoSes() ([]*networkqosapi.NetworkQoS, error) {
	nqoses, err := c.nqosLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list NetworkQoS: %v", err)
	}
	return nqoses, nil
}

func (c *Controller) getNetAttachDefsByNamespace(namespaceSelector *metav1.LabelSelector) ([]*nadv1.NetworkAttachmentDefinition, error) {
	var selectedNads []*nadv1.NetworkAttachmentDefinition
	if namespaceSelector != nil && namespaceSelector.Size() > 0 {
		nsSelector, err := metav1.LabelSelectorAsSelector(namespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid namespace selector %v: %v", namespaceSelector.String(), err)
		}
		namespaces, err := c.nqosNamespaceLister.List(nsSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %v", err)
		}
		for _, ns := range namespaces {
			nads, err := c.nadLister.NetworkAttachmentDefinitions(ns.Name).List(labels.Everything())
			if err != nil {
				return nil, fmt.Errorf("failed to list NADs in namespace %s: %v", ns.Name, err)
			}
			selectedNads = append(selectedNads, nads...)
		}
	}
	return selectedNads, nil
}

func (c *Controller) getNetAttachDefsBySelectors(namespaceSelector, nadSelector *metav1.LabelSelector) ([]*nadv1.NetworkAttachmentDefinition, error) {
	if nadSelector == nil || nadSelector.Size() == 0 {
		return nil, fmt.Errorf("empty network selector")
	}
	nadSel, err := metav1.LabelSelectorAsSelector(nadSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid network selector %v: %v", nadSelector.String(), err)
	}
	var selectedNads []*nadv1.NetworkAttachmentDefinition
	if namespaceSelector != nil && namespaceSelector.Size() > 0 {
		nsSelector, err := metav1.LabelSelectorAsSelector(namespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid namespace selector %v: %v", namespaceSelector.String(), err)
		}
		namespaces, err := c.nqosNamespaceLister.List(nsSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %v", err)
		}
		for _, ns := range namespaces {
			nads, err := c.nadLister.NetworkAttachmentDefinitions(ns.Name).List(nadSel)
			if err != nil {
				return nil, fmt.Errorf("failed to list NADs in namespace %s: %v", ns.Name, err)
			}
			selectedNads = append(selectedNads, nads...)
		}
	} else {
		nads, err := c.nadLister.List(nadSel)
		if err != nil {
			return nil, fmt.Errorf("failed to list NADs: %v", err)
		}
		selectedNads = append(selectedNads, nads...)
	}
	return selectedNads, nil
}
