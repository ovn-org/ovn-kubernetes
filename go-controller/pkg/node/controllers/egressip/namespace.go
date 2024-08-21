package egressip

import (
	"errors"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

func (c *Controller) onNamespaceAdd(obj interface{}) {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &corev1.Namespace{}, obj))
		return
	}
	if ns == nil {
		utilruntime.HandleError(errors.New("invalid Namespace provided to onNamespaceAdd()"))
		return
	}
	c.namespaceQueue.Add(ns)
}

func (c *Controller) onNamespaceUpdate(oldObj, newObj interface{}) {
	oldNamespace, ok := oldObj.(*corev1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &corev1.Namespace{}, oldObj))
		return
	}
	newNamespace, ok := newObj.(*corev1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &corev1.Namespace{}, newObj))
		return
	}
	if oldNamespace == nil || newNamespace == nil {
		utilruntime.HandleError(errors.New("invalid Namespace provided to onNamespaceUpdate()"))
		return
	}
	if oldNamespace.ResourceVersion == newNamespace.ResourceVersion || !newNamespace.GetDeletionTimestamp().IsZero() {
		return
	}
	c.namespaceQueue.Add(newNamespace)
}

func (c *Controller) onNamespaceDelete(obj interface{}) {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		ns, ok = tombstone.Obj.(*corev1.Namespace)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Namespace: %#v", tombstone.Obj))
			return
		}
	}
	if ns != nil {
		c.namespaceQueue.Add(ns)
	}
}

func (c *Controller) runNamespaceWorker(wg *sync.WaitGroup) {
	for c.processNextNamespaceWorkItem(wg) {
	}
}

func (c *Controller) processNextNamespaceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	ns, shutdown := c.namespaceQueue.Get()
	if shutdown {
		return false
	}
	defer c.namespaceQueue.Done(ns)
	if err := c.syncNamespace(ns); err != nil {
		if c.namespaceQueue.NumRequeues(ns) < maxRetries {
			klog.V(4).Infof("Error found while processing namespace %q: %v", ns.Name, err)
			c.namespaceQueue.AddRateLimited(ns)
			return true
		}
		klog.Errorf("Dropping namespace %q out of the queue: %v", ns.Name, err)
		utilruntime.HandleError(err)
	}
	c.namespaceQueue.Forget(ns)
	return true
}

func (c *Controller) syncNamespace(namespace *corev1.Namespace) error {
	eipKeys, err := c.getEIPsForNamespaceChange(namespace)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Egress IP: %v for namespace: %s", eipKeys, namespace)
	for eipKey := range eipKeys {
		c.eIPQueue.Add(eipKey)
	}
	return nil
}

func (c *Controller) getEIPsForNamespaceChange(namespace *corev1.Namespace) (sets.Set[string], error) {
	eIPNames := sets.Set[string]{}
	informerEIPs, err := c.getAllEIPs()
	if err != nil {
		return nil, err
	}
	for _, informerEIP := range informerEIPs {
		targetNsSel, err := metav1.LabelSelectorAsSelector(&informerEIP.Spec.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		if targetNsSel.Matches(labels.Set(namespace.Labels)) {
			eIPNames.Insert(informerEIP.Name)
			continue
		}
	}
	// check which namespaces were referenced before and if this namespace was referenced, we need to trigger a sync
	c.referencedObjectsLock.RLock()
	defer c.referencedObjectsLock.RUnlock()
	for eIPName, refs := range c.referencedObjects {
		if refs.eIPNamespaces.Has(namespace.Name) {
			eIPNames.Insert(eIPName)
			continue
		}
	}
	return eIPNames, nil
}

func (c *Controller) listNamespacesBySelector(selector *metav1.LabelSelector) ([]*corev1.Namespace, error) {
	s, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, err
	}
	ns, err := c.namespaceLister.List(s)
	if err != nil {
		return nil, err
	}
	return ns, nil

}
