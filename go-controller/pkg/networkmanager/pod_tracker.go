// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkmanager

import (
	"context"
	"fmt"
	"sync"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type nodeNAD struct {
	node string
	nads []string
}

type refChange struct {
	node   string
	nad    string
	active bool
}

// PodTrackerController tracks which pods are using which NADs on which nodes and notifies subscribers
// when the first pod for a node/NAD appears or the last one disappears.
type PodTrackerController struct {
	// cacheMutex guards nodeNADToPodCache and podToNodeNAD
	cacheMutex sync.Mutex
	name       string
	// primaryNADForNamespace resolves the primary NAD for a namespace (cached in NAD controller)
	primaryNADForNamespace func(namespace string) (string, error)
	// nodeNADToPodCache holds a mapping of node -> NAD namespaced name -> pod namespaced name
	nodeNADToPodCache map[string]map[string]map[string]struct{}
	// podToNodeNAD is the reverse index: pod key -> (node, NAD)
	podToNodeNAD map[string]nodeNAD
	// callback when a node+NAD goes active/inactive
	onNetworkRefChange func(node, nad string, active bool)
	podController      controller.Controller
	nadReconciler      controller.Reconciler
	podLister          v1.PodLister
	nadLister          nadlisters.NetworkAttachmentDefinitionLister
	namespaceLister    v1.NamespaceLister
}

func NewPodTrackerController(
	name string,
	wf watchFactory,
	onNetworkRefChange func(node, nad string, active bool),
	primaryNADForNamespace func(namespace string) (string, error),
) *PodTrackerController {
	p := &PodTrackerController{
		name:                   name,
		nodeNADToPodCache:      make(map[string]map[string]map[string]struct{}),
		podToNodeNAD:           make(map[string]nodeNAD),
		onNetworkRefChange:     onNetworkRefChange,
		podLister:              wf.PodCoreInformer().Lister(),
		nadLister:              wf.NADInformer().Lister(),
		namespaceLister:        wf.NamespaceInformer().Lister(),
		primaryNADForNamespace: primaryNADForNamespace,
	}

	if p.primaryNADForNamespace == nil {
		p.primaryNADForNamespace = p.getPrimaryNADForNamespaceFromLister
	}

	cfg := &controller.ControllerConfig[corev1.Pod]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      p.reconcile,
		ObjNeedsUpdate: p.needUpdate,
		MaxAttempts:    controller.InfiniteAttempts,
		Threadiness:    1,
		Informer:       wf.PodCoreInformer().Informer(),
		Lister:         wf.PodCoreInformer().Lister().List,
	}
	p.podController = controller.NewController[corev1.Pod](p.name, cfg)

	// Reconciler fed by NAD controller to refresh cache when NADs change.
	p.nadReconciler = controller.NewReconciler(
		fmt.Sprintf("%s-nad-reconciler", name),
		&controller.ReconcilerConfig{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile: func(_ context.Context, key string) error {
				return p.requeuePodsForNAD(key)
			},
			Threadiness: 1,
			MaxAttempts: controller.InfiniteAttempts,
		},
	)
	return p
}

func (c *PodTrackerController) Start() error {
	klog.Infof("Starting %s controller", c.name)
	return controller.StartWithInitialSync(c.syncAll, c.podController, c.nadReconciler)
}

func (c *PodTrackerController) Stop() {
	klog.Infof("Stopping %s controller", c.name)
	controller.Stop(c.podController, c.nadReconciler)
}

func (c *PodTrackerController) NodeHasNAD(node, nad string) bool {
	c.cacheMutex.Lock()
	defer c.cacheMutex.Unlock()
	if _, ok := c.nodeNADToPodCache[node]; !ok {
		return false
	}
	if _, ok := c.nodeNADToPodCache[node][nad]; !ok {
		return false
	}
	return len(c.nodeNADToPodCache[node][nad]) > 0
}

// getNADsForPod resolves the primary and secondary networks for a pod.
func (c *PodTrackerController) getNADsForPod(pod *corev1.Pod) ([]string, error) {
	var nadList []string

	requiresUDN := false
	// check if required UDN label is on namespace
	ns, err := c.namespaceLister.Get(pod.Namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to get namespace %q: %w", pod.Namespace, err)
	}
	if _, exists := ns.Labels[types.RequiredUDNNamespaceLabel]; exists {
		requiresUDN = true
	}

	// Primary NAD from namespace
	primaryNAD, err := c.primaryNADForNamespace(pod.Namespace)
	if err != nil {
		return nil, err
	}
	if len(primaryNAD) > 0 && primaryNAD != types.DefaultNetworkName {
		nadList = append(nadList, primaryNAD)
	} else if requiresUDN {
		return nil, util.NewInvalidPrimaryNetworkError(pod.Namespace)
	}

	// Secondary NADs from pod annotation
	networks, err := util.GetK8sPodAllNetworkSelections(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to parse network annotations for pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	for _, net := range networks {
		ns := net.Namespace
		if ns == "" {
			ns = pod.Namespace
		}
		nadList = append(nadList, fmt.Sprintf("%s/%s", ns, net.Name))
	}

	klog.V(5).Infof("%s - tracked NADS for pod %q: %#v", c.name, pod.Name, nadList)

	return nadList, nil
}

// getPrimaryNADForNamespaceFromLister is a fallback resolver used in tests when no resolver is injected.
func (c *PodTrackerController) getPrimaryNADForNamespaceFromLister(namespace string) (string, error) {
	ns, err := c.namespaceLister.Get(namespace)
	if err != nil {
		return "", fmt.Errorf("failed to get namespace %q: %w", namespace, err)
	}
	if _, hasLabel := ns.Labels[types.RequiredUDNNamespaceLabel]; !hasLabel {
		return types.DefaultNetworkName, nil
	}

	nads, err := c.nadLister.NetworkAttachmentDefinitions(namespace).List(labels.Everything())
	if err != nil {
		return "", fmt.Errorf("failed to list network attachment definitions: %w", err)
	}
	for _, nad := range nads {
		if nad.Name == types.DefaultNetworkName {
			continue
		}
		nadInfo, err := util.ParseNADInfo(nad)
		if err != nil {
			klog.Warningf("Failed to parse network attachment definition %q: %v", nad.Name, err)
			continue
		}
		if nadInfo.IsPrimaryNetwork() {
			return util.GetNADName(nad.Namespace, nad.Name), nil
		}
	}
	return "", util.NewInvalidPrimaryNetworkError(namespace)
}

// syncAll builds the cache on initial controller start
// This is required because workers are started asynchronously and consumers of the tracker
// rely on the cache to be populated during start up
func (c *PodTrackerController) syncAll() error {
	klog.Infof("%s: warming up cache with existing pods", c.name)

	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods: %v", err)
	}

	for _, pod := range pods {
		if pod.Spec.NodeName == "" || util.PodCompleted(pod) {
			continue
		}

		nadList, err := c.getNADsForPod(pod)
		if err != nil {
			klog.Errorf("Pod Tracker sync - Failed to get nads for pod %s/%s: %v", pod.Namespace, pod.Name, err)
			continue
		}
		if len(nadList) == 0 {
			continue
		}

		c.addPodToCache(pod, pod.Spec.NodeName, nadList)
	}

	klog.Infof("%s: cache warmup complete with %d pods", c.name, len(pods))
	return nil
}

// NADReconciler returns the reconciler that should be registered with the NAD controller.
func (c *PodTrackerController) NADReconciler() controller.Reconciler {
	return c.nadReconciler
}

// requeuePodsForNAD enqueues pods in the NAD's namespace so they get retried
// once the NAD (or its primary designation) becomes available.
func (c *PodTrackerController) requeuePodsForNAD(key string) error {
	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split meta namespace key %q: %v", key, err)
	}
	if namespace == "" {
		return nil
	}

	pods, err := c.podLister.Pods(namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods in namespace %q: %v", namespace, err)
	}
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			klog.Warningf("%s: failed to build key for pod %s/%s: %v", c.name, pod.Namespace, pod.Name, err)
			continue
		}
		// Use rate-limited enqueue to avoid hot-looping on a flood of pods
		c.podController.ReconcileRateLimited(key)
	}
	return nil
}

// needUpdate return true when the pod has been created or updated.
func (c *PodTrackerController) needUpdate(old, new *corev1.Pod) bool {
	// ignore pods that only want hostNetwork with no NADs
	if new != nil && util.PodWantsHostNetwork(new) {
		if _, ok := new.Annotations[nadv1.NetworkAttachmentAnnot]; !ok {
			return false
		}
	}

	// Ignore adds/updates while the pod is still unscheduled; we'll react once it gets a node.
	if new != nil && new.Spec.NodeName == "" {
		return false
	}

	// Add
	if old == nil {
		return true
	}

	if new == nil {
		return false
	}

	// Ignore updates while the pod is still unscheduled; we'll react once it gets a node.
	if new.Spec.NodeName == "" {
		return false
	}

	// If the node assignment changed (including unscheduled -> scheduled), reconcile.
	if old.Spec.NodeName != new.Spec.NodeName {
		return true
	}

	// Reconcile when all pod containers have stopped so its network references
	// can be removed. A deletion timestamp alone does not mean the pod has
	// finished terminating.
	if util.PodCompleted(old) != util.PodCompleted(new) {
		return true
	}

	// If the network attachment annotations changed, reconcile.
	oldAnno := old.Annotations[nadv1.NetworkAttachmentAnnot]
	newAnno := new.Annotations[nadv1.NetworkAttachmentAnnot]
	return oldAnno != newAnno
}

// reconcile notify subscribers with the request namespace key following namespace events.
func (c *PodTrackerController) reconcile(_ context.Context, key string) error {
	klog.V(5).Infof("%s reconcile called for pod %s", c.name, key)
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split meta namespace key %q: %v", key, err)
	}

	pod, err := c.podLister.Pods(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Pod deleted → cleanup cache
			c.deletePodFromCache(key)
			return nil
		}
		return fmt.Errorf("failed to get pod %q from cache: %v", key, err)
	}

	// Keep terminating pods tracked until all their containers have stopped.
	if util.PodCompleted(pod) {
		c.deletePodFromCache(key)
		return nil
	}

	// Ignore pods that are not yet scheduled; callbacks/cache should only be for scheduled pods.
	if pod.Spec.NodeName == "" {
		return nil
	}

	nadList, err := c.getNADsForPod(pod)
	if err != nil {
		return err
	}
	if len(nadList) == 0 {
		c.deletePodFromCache(key)
		return nil
	}

	// Track pod under its node for each NAD
	c.addPodToCache(pod, pod.Spec.NodeName, nadList)

	return nil
}

func (c *PodTrackerController) addPodToCache(pod *corev1.Pod, node string, nads []string) {
	c.cacheMutex.Lock()
	klog.V(5).Infof("%s - addPodToCache for pod %s/%s, node: %s, nads: %#v", c.name, pod.Namespace, pod.Name, node, nads)

	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	// Short-circuit if identical node/NAD set already cached
	if existing, found := c.podToNodeNAD[key]; found && existing.node == node && equalStringSets(existing.nads, nads) {
		c.cacheMutex.Unlock()
		return
	}

	// First clean up any existing entry for this pod
	refChanges := c.deletePodFromCacheLocked(key)

	for _, nad := range nads {
		if _, ok := c.nodeNADToPodCache[node]; !ok {
			c.nodeNADToPodCache[node] = make(map[string]map[string]struct{})
		}
		if _, ok := c.nodeNADToPodCache[node][nad]; !ok {
			c.nodeNADToPodCache[node][nad] = make(map[string]struct{})
		}
		before := len(c.nodeNADToPodCache[node][nad])
		c.nodeNADToPodCache[node][nad][key] = struct{}{}
		// Only fire on the 0 -> 1 transition; repeated adds for the same NAD are ignored
		if before == 0 {
			// 0 → 1 transition
			refChanges = append(refChanges, refChange{node, nad, true})
		}
	}

	c.podToNodeNAD[key] = nodeNAD{node: node, nads: append([]string(nil), nads...)}
	c.cacheMutex.Unlock()
	if c.onNetworkRefChange != nil {
		for _, callback := range refChanges {
			c.onNetworkRefChange(callback.node, callback.nad, callback.active)
		}
	}
}

func (c *PodTrackerController) deletePodFromCache(key string) {
	klog.V(5).Infof("%s - deletePodFromCache for pod %s", c.name, key)
	c.cacheMutex.Lock()
	changes := c.deletePodFromCacheLocked(key)
	c.cacheMutex.Unlock()

	if c.onNetworkRefChange != nil {
		for _, ev := range changes {
			c.onNetworkRefChange(ev.node, ev.nad, ev.active)
		}
	}
}

func (c *PodTrackerController) deletePodFromCacheLocked(key string) []refChange {
	var refChanges []refChange
	loc, ok := c.podToNodeNAD[key]
	if !ok {
		return nil
	}

	for _, nad := range loc.nads {
		if _, ok := c.nodeNADToPodCache[loc.node][nad]; ok {
			before := len(c.nodeNADToPodCache[loc.node][nad])
			delete(c.nodeNADToPodCache[loc.node][nad], key)
			after := len(c.nodeNADToPodCache[loc.node][nad])
			if before == 1 && after == 0 {
				// 1 → 0 transition
				refChanges = append(refChanges, refChange{loc.node, nad, false})
			}
			if after == 0 {
				delete(c.nodeNADToPodCache[loc.node], nad)
			}
		}
	}
	if len(c.nodeNADToPodCache[loc.node]) == 0 {
		delete(c.nodeNADToPodCache, loc.node)
	}

	delete(c.podToNodeNAD, key)
	return refChanges
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, v := range a {
		set[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := set[v]; !ok {
			return false
		}
	}
	return true
}
