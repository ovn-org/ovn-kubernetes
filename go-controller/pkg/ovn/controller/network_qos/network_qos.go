package networkqos

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	networkqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1"
	nqosapiapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1/apis/applyconfiguration/networkqos/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func (c *Controller) processNextNQOSWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	nqosKey, quit := c.nqosQueue.Get()
	if quit {
		return false
	}
	defer c.nqosQueue.Done(nqosKey)

	err := c.syncNetworkQoS(nqosKey.(string))
	if err == nil {
		c.nqosQueue.Forget(nqosKey)
		return true
	}
	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", nqosKey, err))

	if c.nqosQueue.NumRequeues(nqosKey) < maxRetries {
		c.nqosQueue.AddRateLimited(nqosKey)
		return true
	}

	c.nqosQueue.Forget(nqosKey)
	return true
}

// syncNetworkQoS decides the main logic everytime
// we dequeue a key from the nqosQueue cache
func (c *Controller) syncNetworkQoS(key string) error {
	startTime := time.Now()
	nqosNamespace, nqosName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(5).Infof("%s - Processing sync for Network QoS %s", c.controllerName, nqosName)

	defer func() {
		klog.V(5).Infof("%s - Finished syncing Network QoS %s : %v", c.controllerName, nqosName, time.Since(startTime))
	}()

	nqos, err := c.nqosLister.NetworkQoSes(nqosNamespace).Get(nqosName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if nqos == nil || !c.networkManagedByMe(nqos.Spec.NetworkAttachmentName) {
		// either networkqos has been deleted, or networkAttachmentName changed to some other value
		if nqos == nil {
			klog.V(5).Infof("%s - NetworkQoS %s has gone", c.controllerName, key)
		} else {
			klog.V(6).Infof("%s - NetworkQoS %s has net-attach-def %s, not managed by this controller", c.controllerName, key, nqos.Spec.NetworkAttachmentName)
		}
		klog.V(4).Infof("%s - Clean up networkqos %s", c.controllerName, key)
		// maybe NetworkAttachmentName has been changed from this one to other value, try cleanup anyway
		return c.nqosCache.DoWithLock(key, func(nqosKey string) error {
			return c.clearNetworkQos(nqosNamespace, nqosName)
		})
	}
	klog.V(5).Infof("%s - Processing NetworkQoS %s/%s", c.controllerName, nqos.Namespace, nqos.Name)
	// at this stage the NQOS exists in the cluster
	return c.nqosCache.DoWithLock(key, func(nqosKey string) error {
		if err = c.ensureNetworkQos(nqos); err == nil {
			recordNetworkQoSReconcileDuration(c.controllerName, time.Since(startTime).Milliseconds())
			updateNetworkQoSCount(c.controllerName, len(c.nqosCache.GetKeys()))
			if e := c.updateNQOSStatusToReady(nqos.Namespace, nqos.Name); e != nil {
				return fmt.Errorf("NetworkQoS %s/%s reconciled successfully but unable to patch status: %v", nqos.Namespace, nqos.Name, err)
			}
			return nil
		} else {
			// we can ignore the error if status update doesn't succeed; best effort
			if e := c.updateNQOSStatusToNotReady(nqos.Namespace, nqos.Name, err.Error()); e != nil {
				klog.Warningf("Unable to patch status for NetworkQoS %s/%s: %v", nqos.Namespace, nqos.Name, e)
			}
			return err
		}
	})
}

// ensureNetworkQos will handle the main reconcile logic for any given nqos's
// add/update that might be triggered either due to NQOS changes or the corresponding
// matching pod or namespace changes.
func (c *Controller) ensureNetworkQos(nqos *networkqosapi.NetworkQoS) error {
	desiredNQOSState := &networkQoSState{
		name:                  nqos.Name,
		namespace:             nqos.Namespace,
		networkAttachmentName: nqos.Spec.NetworkAttachmentName,
	}
	if desiredNQOSState.networkAttachmentName == "" {
		desiredNQOSState.networkAttachmentName = types.DefaultNetworkName
	}

	if len(nqos.Spec.PodSelector.MatchLabels) > 0 || len(nqos.Spec.PodSelector.MatchExpressions) > 0 {
		if podSelector, err := metav1.LabelSelectorAsSelector(&nqos.Spec.PodSelector); err != nil {
			return fmt.Errorf("failed to parse source pod selector of NetworkQoS %s/%s: %w", nqos.Namespace, nqos.Name, err)
		} else {
			desiredNQOSState.PodSelector = podSelector
		}
	}

	// set EgressRules to desiredNQOSState
	rules := []*GressRule{}
	for _, ruleSpec := range nqos.Spec.Egress {
		bwRate := int(ruleSpec.Bandwidth.Rate)
		bwBurst := int(ruleSpec.Bandwidth.Burst)
		ruleState := &GressRule{
			Priority: ruleSpec.Priority,
			Dscp:     ruleSpec.DSCP,
			Rate:     &bwRate,
			Burst:    &bwBurst,
		}
		destStates := []*Destination{}
		for _, destSpec := range ruleSpec.Classifier.To {
			if destSpec.IPBlock != nil && (destSpec.PodSelector != nil || destSpec.NamespaceSelector != nil) {
				return fmt.Errorf("can't specify both ipBlock and podSelector/namespaceSelector")
			}
			destState := &Destination{}
			destState.IpBlock = destSpec.IPBlock.DeepCopy()
			if destSpec.NamespaceSelector != nil && (len(destSpec.NamespaceSelector.MatchLabels) > 0 || len(destSpec.NamespaceSelector.MatchExpressions) > 0) {
				if selector, err := metav1.LabelSelectorAsSelector(destSpec.NamespaceSelector); err != nil {
					return fmt.Errorf("failed to parse namespace selector: %w", err)
				} else {
					destState.NamespaceSelector = selector
				}
			}
			if destSpec.PodSelector != nil && (len(destSpec.PodSelector.MatchLabels) > 0 || len(destSpec.PodSelector.MatchExpressions) > 0) {
				if selector, err := metav1.LabelSelectorAsSelector(destSpec.PodSelector); err != nil {
					return fmt.Errorf("failed to parse pod selector: %w", err)
				} else {
					destState.PodSelector = selector
				}
			}
			destStates = append(destStates, destState)
		}
		ruleState.Classifier = &Classifier{
			Destinations: destStates,
		}
		if ruleSpec.Classifier.Port.Protocol != "" {
			ruleState.Classifier.Protocol = protocol(ruleSpec.Classifier.Port.Protocol)
			if !ruleState.Classifier.Protocol.IsValid() {
				return fmt.Errorf("invalid protocol: %s, valid values are: tcp, udp, sctp", ruleSpec.Classifier.Port.Protocol)
			}
		}
		if ruleSpec.Classifier.Port.Port > 0 {
			port := int(ruleSpec.Classifier.Port.Port)
			ruleState.Classifier.Port = &port
		}
		rules = append(rules, ruleState)
	}
	desiredNQOSState.EgressRules = rules
	if err := desiredNQOSState.initAddressSets(c.addressSetFactory, c.controllerName); err != nil {
		return err
	}
	if err := c.updateNetworkQoS(desiredNQOSState); err != nil {
		return fmt.Errorf("failed to update network qos: %w", err)
	}
	if err := c.resyncPods(desiredNQOSState); err != nil {
		return fmt.Errorf("failed to resync pods: %w", err)
	}
	if err := c.deleteStaleQoSes(desiredNQOSState); err != nil {
		return fmt.Errorf("failed to delete stale QoSes: %w", err)
	}
	// go through logical switches to cleanup unused qoses
	if err := c.removeUnusedQoSes(desiredNQOSState); err != nil {
		return fmt.Errorf("failed to remove unused QoSes: %w", err)
	}
	// since transact was successful we can finally replace the currentNQOSState in the cache with the latest desired one
	c.nqosCache.Store(joinMetaNamespaceAndName(nqos.Namespace, nqos.Name), desiredNQOSState)
	return nil
}

// clearNetworkQos will handle the logic for deleting all db objects related
// to the provided nqos which got deleted.
// uses externalIDs to figure out ownership
func (c *Controller) clearNetworkQos(nqosNamespace, nqosName string) error {
	cachedName := joinMetaNamespaceAndName(nqosNamespace, nqosName)
	// See if we need to handle this: https://github.com/ovn-org/ovn-kubernetes/pull/3659#discussion_r1284645817
	qosState, loaded := c.nqosCache.Load(cachedName)
	if !loaded {
		// there is no existing NQOS configured with this name, nothing to clean
		klog.Infof("NQOS %s not found in cache, nothing to clear", cachedName)
		return nil
	}
	if !c.networkManagedByMe(qosState.networkAttachmentName) {
		klog.Errorf("Found unexpected cached NetworkQoS %s for network %s in controller %s", cachedName, qosState.networkAttachmentName, c.controllerName)
		return nil
	}
	klog.V(6).Infof("Cleaning up NetworkQoS %s/%s by %s", qosState.namespace, qosState.name, c.controllerName)

	// clear NBDB objects for the given NQOS
	if err := c.deleteByNetworkQoS(qosState); err != nil {
		return fmt.Errorf("failed to delete QoS rules for NetworkQoS %s/%s: %w", qosState.namespace, qosState.name, err)
	}
	c.nqosCache.Delete(cachedName)
	updateNetworkQoSCount(c.controllerName, len(c.nqosCache.GetKeys()))
	return nil
}

const (
	conditionTypeReady    = "Ready-In-Zone-"
	reasonQoSSetupSuccess = "Success"
	reasonQoSSetupFailed  = "Failed"
)

func (c *Controller) updateNQOSStatusToReady(namespace, name string) error {
	cond := metav1.Condition{
		Type:    conditionTypeReady + c.zone,
		Status:  metav1.ConditionTrue,
		Reason:  reasonQoSSetupSuccess,
		Message: "NetworkQoS was applied successfully",
	}
	err := c.updateNQOStatusCondition(cond, namespace, name)
	if err != nil {
		return fmt.Errorf("failed to update the status of NetworkQoS %s/%s, err: %v", namespace, name, err)
	}
	klog.V(5).Infof("Patched the status of NetworkQoS %s/%s with condition type %v/%v",
		namespace, name, conditionTypeReady+c.zone, metav1.ConditionTrue)
	return nil
}

func (c *Controller) updateNQOSStatusToNotReady(namespace, name, errMsg string) error {
	cond := metav1.Condition{
		Type:    conditionTypeReady + c.zone,
		Status:  metav1.ConditionFalse,
		Reason:  reasonQoSSetupFailed,
		Message: fmt.Sprintf("Failed to apply NetworkQoS: %s", errMsg),
	}
	err := c.updateNQOStatusCondition(cond, namespace, name)
	if err != nil {
		return fmt.Errorf("failed to update the status of NetworkQoS %s/%s, err: %v", namespace, name, err)
	}
	klog.V(5).Infof("Patched the status of NetworkQoS %s/%s with condition type %v/%v",
		namespace, name, conditionTypeReady+c.zone, metav1.ConditionTrue)
	return nil
}

func (c *Controller) updateNQOStatusCondition(newCondition metav1.Condition, namespace, name string) error {
	nqos, err := c.nqosLister.NetworkQoSes(namespace).Get(name)
	if err != nil {
		return err
	}
	existingCondition := meta.FindStatusCondition(nqos.Status.Conditions, newCondition.Type)
	if existingCondition == nil {
		newCondition.LastTransitionTime = metav1.NewTime(time.Now())
	} else {
		if existingCondition.Status != newCondition.Status {
			existingCondition.Status = newCondition.Status
			existingCondition.LastTransitionTime = metav1.NewTime(time.Now())
		}
		existingCondition.Reason = newCondition.Reason
		existingCondition.Message = newCondition.Message
		newCondition = *existingCondition
	}
	applyObj := nqosapiapply.NetworkQoS(name, namespace).
		WithStatus(nqosapiapply.Status().WithConditions(newCondition))
	_, err = c.nqosClientSet.K8sV1().NetworkQoSes(namespace).ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{FieldManager: c.zone, Force: true})
	return err
}

func (c *Controller) resyncPods(nqosState *networkQoSState) error {
	pods, err := c.nqosPodLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods in namespace %s: %w", nqosState.namespace, err)
	}
	nsCache := make(map[string]*corev1.Namespace)
	for _, pod := range pods {
		ns := nsCache[pod.Namespace]
		if ns == nil {
			ns, err = c.nqosNamespaceLister.Get(pod.Namespace)
			if err != nil {
				return fmt.Errorf("failed to get namespace %s: %w", pod.Namespace, err)
			}
			nsCache[pod.Namespace] = ns
		}
		if err := c.setPodForNQOS(pod, nqosState, ns); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) networkManagedByMe(nadKey string) bool {
	return ((nadKey == "" || nadKey == types.DefaultNetworkName) && c.IsDefault()) ||
		(!c.IsDefault() && c.HasNAD(nadKey))
}

func (c *Controller) getLogicalSwitchName(nodeName string) string {
	switch {
	case c.TopologyType() == types.Layer2Topology:
		return c.GetNetworkScopedSwitchName(types.OVNLayer2Switch)
	case c.TopologyType() == types.LocalnetTopology:
		return c.GetNetworkScopedSwitchName(types.OVNLocalnetSwitch)
	case !c.IsSecondary() || c.TopologyType() == types.Layer3Topology:
		return c.GetNetworkScopedSwitchName(nodeName)
	default:
		return ""
	}
}
