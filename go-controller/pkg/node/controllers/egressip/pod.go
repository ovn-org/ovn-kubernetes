package egressip

import (
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/vishvananda/netlink"
)

// podIPConfig holds pod specific info to implement egress IP for secondary host networks for a single pod IP. A pod may
// contain multiple IPs (one for single stack, 2 for dual stack).
type podIPConfig struct {
	failed      bool // used for retry
	v6          bool
	ipTableRule iptables.RuleArg
	ipRule      netlink.Rule
}

func newPodIPConfig() *podIPConfig {
	return &podIPConfig{}
}

func (pIC *podIPConfig) equal(pIC2 *podIPConfig) bool {
	if pIC.v6 != pIC2.v6 {
		return false
	}
	if !equal(pIC.ipTableRule.Args, pIC2.ipTableRule.Args) {
		return false
	}
	if pIC.ipRule.String() != pIC2.ipRule.String() {
		return false
	}
	return true
}

// podIPConfigList holds a list of podIPConfig to configure EIP for a single pod and its IPs.
// Each item in the list represents one pod IP.
type podIPConfigList struct {
	elems []*podIPConfig
}

func newPodIPConfigList() *podIPConfigList {
	return &podIPConfigList{elems: []*podIPConfig{}}
}

func (pICL *podIPConfigList) len() int {
	return len(pICL.elems)
}

func (pICL *podIPConfigList) hasWithoutError(pIC2 *podIPConfig) bool {
	for _, pIC := range pICL.elems {
		if pIC.equal(pIC2) && !pIC.failed {
			return true
		}
	}
	return false
}

func (pICL *podIPConfigList) has(pIC2 *podIPConfig) bool {
	for _, pIC := range pICL.elems {
		if pIC.equal(pIC2) {
			return true
		}
	}
	return false
}

func (pICL *podIPConfigList) remove(idxs ...int) {
	if len(idxs) == 0 {
		return
	}
	newElems := make([]*podIPConfig, 0, len(pICL.elems))
	idxToDelete := sets.New[int](idxs...)

	for existingIdx, pIC := range pICL.elems {
		if !idxToDelete.Has(existingIdx) {
			newElems = append(newElems, pIC)
		}
	}
	pICL.elems = newElems
}

// insertOverwriteFailed should be used to add elements to the podIPConfigList that have failed to be applied.
func (pICL *podIPConfigList) insertOverwriteFailed(pICs ...podIPConfig) {
	for _, pIC := range pICs {
		pIC.failed = true
		pICL.insertOverwrite(pIC)
	}
}

// insert should be used to add elements to the podIPConfigList
func (pICL *podIPConfigList) insert(pICs ...podIPConfig) {
	for _, pc := range pICs {
		pICL.insertOverwrite(pc)
	}
}

func (pICL *podIPConfigList) insertOverwrite(pIC podIPConfig) {
	var dupeIdxs []int
	for idx, existingPIC := range pICL.elems {
		if existingPIC.equal(&pIC) {
			dupeIdxs = append(dupeIdxs, idx)
		}
	}
	pICL.remove(dupeIdxs...)
	pICL.elems = append(pICL.elems, &pIC)
}

func (pICL *podIPConfigList) delete(pICs ...podIPConfig) {
	elems := make([]*podIPConfig, 0, len(pICL.elems))
	for _, existingPIC := range pICL.elems {
		removed := false
		for _, pIC := range pICs {
			if existingPIC.equal(&pIC) {
				removed = true
				break
			}
		}
		if !removed {
			elems = append(elems, existingPIC)
		}
	}

	pICL.elems = elems
}

func (c *Controller) onPodAdd(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &corev1.Pod{}, obj))
		return
	}
	if pod == nil {
		utilruntime.HandleError(errors.New("invalid Pod provided to onPodAdd()"))
		return
	}
	// if the pod does not have IPs AND there are no multus network status annotations found, skip it
	if len(pod.Status.PodIPs) == 0 {
		return
	}
	c.podQueue.Add(pod)
}

func (c *Controller) onPodUpdate(oldObj, newObj interface{}) {
	o, ok := oldObj.(*corev1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &corev1.Pod{}, o))
		return
	}
	n, ok := newObj.(*corev1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &corev1.Pod{}, n))
		return
	}
	if o == nil || n == nil {
		utilruntime.HandleError(errors.New("invalid Pod provided to onPodUpdate()"))
		return
	}
	// if labels AND assigned Pod IPs AND the multus network status annotations are the same, skip processing changes to the pod.
	if reflect.DeepEqual(o.Labels, n.Labels) &&
		reflect.DeepEqual(o.Status.PodIPs, n.Status.PodIPs) {
		return
	}
	c.podQueue.Add(n)
}

func (c *Controller) onPodDelete(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Pod: %#v", tombstone.Obj))
			return
		}
	}
	if pod != nil {
		c.podQueue.Add(pod)
	}
}

func (c *Controller) runPodWorker(wg *sync.WaitGroup) {
	for c.processNextPodWorkItem(wg) {
	}
}

func (c *Controller) processNextPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	p, shutdown := c.podQueue.Get()
	if shutdown {
		return false
	}
	defer c.podQueue.Done(p)
	if err := c.syncPod(p); err != nil {
		if c.podQueue.NumRequeues(p) < maxRetries {
			klog.V(4).Infof("Error found while processing pod %s/%s: %v", p.Namespace, p.Name, err)
			c.podQueue.AddRateLimited(p)
			return true
		}
		klog.Warningf("Dropping pod %s/%s out of the queue: %s", p.Namespace, p.Name, err)
		utilruntime.HandleError(err)
	}
	c.podQueue.Forget(p)
	return true
}

func (c *Controller) syncPod(pod *corev1.Pod) error {
	eipKeys, err := c.getEIPsForPodChange(pod)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Egress IP: %v for pod: %s/%s", eipKeys, pod.Namespace, pod.Name)
	for eipKey := range eipKeys {
		c.eIPQueue.Add(eipKey)
	}
	return nil
}

// getEIPsForPodChange returns a list of policies that should be reconciled because of a given pod update.
// It consists of 2 stages:
// 1. find EIPs that select given pod now and may need update
// 2. find EIPs that selected given pod before and may need cleanup
// Step 1 is done by fetching the latest EgressIP and checking if selectors match.
// Step 2 is done via referencedObjects, which is a cache of the objects every policy selected last time.
// referencedObjects cache is maintained by EIP controller.
func (c *Controller) getEIPsForPodChange(pod *corev1.Pod) (sets.Set[string], error) {
	eipNames := sets.Set[string]{}
	// get EIPs that match the pods namespace
	informerEIPs, err := c.getAllEIPs()
	if err != nil {
		return nil, err
	}
	podNs, err := c.namespaceLister.Get(pod.Namespace)
	if err != nil {
		return nil, err
	}
	for _, informerEIP := range informerEIPs {
		eipNsSel, err := metav1.LabelSelectorAsSelector(&informerEIP.Spec.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		eipPodSel, err := metav1.LabelSelectorAsSelector(&informerEIP.Spec.PodSelector)
		if err != nil {
			return nil, err
		}
		if eipNsSel.Matches(labels.Set(podNs.Labels)) && eipPodSel.Matches(labels.Set(pod.Labels)) {
			eipNames.Insert(informerEIP.Name)
		}
	}
	// check which namespaces were referenced by policies before
	c.referencedObjectsLock.RLock()
	defer c.referencedObjectsLock.RUnlock()
	for eipName, eipRefs := range c.referencedObjects {
		if eipRefs.eIPPods.Has(getPodNamespacedName(pod)) {
			eipNames.Insert(eipName)
		}
	}
	return eipNames, nil
}

// listPodsByNamespaceAndSelector lists pods by namespace and label selector
func (c *Controller) listPodsByNamespaceAndSelector(namespace string, selector *metav1.LabelSelector) ([]*corev1.Pod, error) {
	var s labels.Selector
	var err error
	if selector == nil {
		s = labels.Everything()
	} else {
		s, err = metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return nil, err
		}
	}
	pods, err := c.podLister.Pods(namespace).List(s)
	if err != nil {
		return nil, err
	}
	return pods, nil

}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if v != b[i] {
			return false
		}
	}
	return true
}
