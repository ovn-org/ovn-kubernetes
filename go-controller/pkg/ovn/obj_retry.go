package ovn

import (
	"fmt"
	"math/rand"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"

	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	kerrorsutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/cache"

	"k8s.io/klog/v2"

	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	factory "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const retryObjInterval = 30 * time.Second
const maxFailedAttempts = 15 // same value used for the services level-driven controller

// retryObjEntry is a generic object caching with retry mechanism
//that resources can use to eventually complete their intended operations.
type retryObjEntry struct {
	sync.Mutex
	// newObj holds k8s resource failed during add operation
	newObj interface{}
	// oldObj holds k8s resource failed during delete operation
	oldObj interface{}
	// config holds feature specific configuration,
	// currently used by network policies and pods.
	config     interface{}
	timeStamp  time.Time
	backoffSec time.Duration
	// ignore indicates whether to ignore this object in the retry iterations.
	// It is set to true while the object is being added/updated/deleted in
	// watchResource, then set to false in case add/update/delete fail.
	ignore bool
	// number of times this object has been unsuccessfully added/updated/deleted
	failedAttempts uint8
}

type retryObjs struct {
	retryMutex sync.Mutex
	// cache to hold object needs retry to successfully complete processing
	entries map[string]*retryObjEntry
	// resource type for these objects
	oType reflect.Type
	// channel to indicate we need to retry objs immediately
	retryChan chan struct{}
	// namespace filter fed to the handler for this resource type
	namespaceForFilteredHandler string
	// label selector fed to the handler for this resource type
	labelSelectorForFilteredHandler labels.Selector
	// sync function for the handler
	syncFunc func([]interface{}) error
	// extra parameters needed by specific types, for now
	// in use by network policy dynamic handlers
	extraParameters interface{}
}

// NewRetryObjs returns a new retryObjs instance, packed with the desired input parameters.
// The returned struct is essential for watchResource and the whole retry logic.
func NewRetryObjs(
	objectType reflect.Type,
	namespaceForFilteredHandler string,
	labelSelectorForFilteredHandler labels.Selector,
	syncFunc func([]interface{}) error,
	extraParameters interface{}) *retryObjs {

	return &retryObjs{
		retryMutex:                      sync.Mutex{},
		entries:                         make(map[string]*retryObjEntry),
		retryChan:                       make(chan struct{}, 1),
		oType:                           objectType,
		namespaceForFilteredHandler:     namespaceForFilteredHandler,
		labelSelectorForFilteredHandler: labelSelectorForFilteredHandler,
		syncFunc:                        syncFunc,
		extraParameters:                 extraParameters,
	}
}

// addRetryObjWithAdd adds an object to be retried later for add
func (r *retryObjs) addRetryObjWithAdd(obj interface{}) {
	key, err := getResourceKey(r.oType, obj)
	if err != nil {
		klog.Errorf("Could not get the key of %s %v: %v", r.oType, obj, err)
		return
	}
	r.initRetryObjWithAdd(obj, key)
	r.unSkipRetryObj(key)
}

// initRetryObjWithAdd creates a retry entry for an object that is being added,
// so that, if it fails, the add can be potentially retried later.
// initially it is marked as skipped for the retry loop (ignore = true).
func (r *retryObjs) initRetryObjWithAdd(obj interface{}, key string) {
	entry := r.ensureRetryEntryLocked(key, &retryObjEntry{Mutex: sync.Mutex{}, newObj: obj,
		timeStamp: time.Now(), backoffSec: 1, ignore: true})
	entry.timeStamp = time.Now()
	entry.newObj = obj
	entry.failedAttempts = 0
	entry.Unlock()
}

// initRetryObjWithUpdate tracks objects that failed to be updated to potentially retry later
// initially it is marked as skipped for retry loop (ignore = true)
func (r *retryObjs) initRetryObjWithUpdate(oldObj, newObj interface{}, key string) {
	entry := r.ensureRetryEntryLocked(key, &retryObjEntry{Mutex: sync.Mutex{}, newObj: newObj, config: oldObj,
		timeStamp: time.Now(), backoffSec: 1, ignore: true})
	entry.timeStamp = time.Now()
	entry.newObj = newObj
	entry.config = oldObj
	entry.failedAttempts = 0
	entry.Unlock()
}

// initRetryWithDelete creates a retry entry for an object that is being deleted,
// so that, if it fails, the delete can be potentially retried later.
// initially it is marked as skipped for the retry loop (ignore = true).
// When applied to pods, we include the config object as well in case the namespace is removed
// and the object is orphaned from the namespace. Similarly, when applied to network policies,
// we include in config the networkPolicy struct used internally, for the same scenario where
// a namespace is being deleted along with its network policies and, in case of a delete retry of
// one such network policy, we wouldn't be able to get to the networkPolicy struct from nsInfo.
//
// The noRetryAdd boolean argument is to indicate whether to retry for addition
func (r *retryObjs) initRetryObjWithDelete(obj interface{}, key string, config interface{}, noRetryAdd bool) {
	entry := r.ensureRetryEntryLocked(key, &retryObjEntry{Mutex: sync.Mutex{}, oldObj: obj, config: config,
		timeStamp: time.Now(), backoffSec: 1, ignore: true})
	entry.timeStamp = time.Now()
	entry.oldObj = obj
	if entry.config == nil {
		entry.config = config
	}
	entry.failedAttempts = 0
	if noRetryAdd {
		// will not be retried for addition
		entry.newObj = nil
	}
	entry.Unlock()
}

// removeDeleteFromRetryObj removes any old object from a retry entry
func (r *retryObjs) removeDeleteFromRetryObj(key string) {
	entry := r.ensureRetryEntryLocked(key, nil)
	if entry != nil {
		entry.oldObj = nil
		entry.config = nil
		entry.Unlock()
	}
}

// unSkipRetryObj ensures an obj is no longer ignored for retry loop
func (r *retryObjs) unSkipRetryObj(key string) {
	entry := r.ensureRetryEntryLocked(key, nil)
	if entry != nil {
		entry.ignore = false
		entry.Unlock()
	}
}

// deleteRetryObj deletes a specific entry from the map
func (r *retryObjs) deleteRetryObj(key string, withLock bool) {
	if withLock {
		entry := r.ensureRetryEntryLocked(key, nil)
		if entry != nil {
			defer entry.Unlock()
		}
	}
	r.retryMutex.Lock()
	delete(r.entries, key)
	r.retryMutex.Unlock()
}

// skipRetryObj sets a specific entry from the map to be ignored for subsequent retries
func (r *retryObjs) skipRetryObj(key string) {
	entry := r.ensureRetryEntryLocked(key, nil)
	if entry != nil {
		entry.ignore = true
		entry.Unlock()
	}
}

// checkRetryObj returns true if an entry with the given key exists, returns false otherwise.
func (r *retryObjs) checkRetryObj(key string) bool {
	entry := r.ensureRetryEntryLocked(key, nil)
	if entry != nil {
		entry.Unlock()
		return true
	}
	return false
}

// returns the retryEntry structure with lock held on it. all users of retryEntry should call
// this function. the mutex and the `ignore` field ensures that the pod add/update/delete do not
// step on each other. if the map has no entry with the input key and if newRetryEntry is specified,
// then the function will try to create a new retryEntry in the map if , otherwise it will return nil.
func (r *retryObjs) ensureRetryEntryLocked(key string, newRetryEntry *retryObjEntry) *retryObjEntry {
	r.retryMutex.Lock()
	entry := r.entries[key]
	if entry != nil {
		// do not hold on retryMutex while waiting on retryEntry to Lock
		r.retryMutex.Unlock()

		// take the lock now
		entry.Lock()

		// Check that the retryEntry wasn't deleted while we were waiting for its lock
		r.retryMutex.Lock()
		_, ok := r.entries[key]
		if ok {
			r.retryMutex.Unlock()
			return entry
		}
		// since the entry went missing from the map, fallback below to assign the newRetryEntry
		// with retryMutex locked from above
		entry.Unlock()
		entry = nil
	}

	if newRetryEntry != nil {
		entry = newRetryEntry
		r.entries[key] = entry
		// this is fine since no one knows about this newRetryEntry yet
		entry.Lock()
	}
	r.retryMutex.Unlock()
	return entry
}

// requestRetryObjs allows a caller to immediately request to iterate through all objects that
// are in the retry cache. This will ignore any outstanding time wait/backoff state
func (r *retryObjs) requestRetryObjs() {
	select {
	case r.retryChan <- struct{}{}:
		klog.V(5).Infof("Iterate retry objects requested (resource %s)", r.oType)
	default:
		klog.V(5).Infof("Iterate retry objects already requested (resource %s)", r.oType)
	}
}

// getObjRetryEntry returns a copy of the retry entry from the cache for the object selected by the key.
func (r *retryObjs) getObjRetryEntry(key string) *retryObjEntry {
	entry := r.ensureRetryEntryLocked(key, nil)
	if entry != nil {
		retryEntry := &retryObjEntry{
			newObj:         entry.newObj,
			oldObj:         entry.oldObj,
			config:         entry.config,
			timeStamp:      entry.timeStamp,
			backoffSec:     entry.backoffSec,
			ignore:         entry.ignore,
			failedAttempts: entry.failedAttempts,
		}
		entry.Unlock()
		return retryEntry
	}
	return nil
}

// increaseFailedAttemptsCounter increases by one the counter of failed add/update/delete attempts
// for the given key
func (r *retryObjs) increaseFailedAttemptsCounter(key string) {
	entry := r.ensureRetryEntryLocked(key, nil)
	if entry != nil {
		entry.failedAttempts++
		entry.Unlock()
	}
}

// setFailedAttemptsCounterForTestingOnly sets the failedAttempts counter in the retry entry selected
// by the input key. Only used in unit tests.
func (r *retryObjs) setFailedAttemptsCounterForTestingOnly(key string, val uint8) {
	entry := r.ensureRetryEntryLocked(key, nil)
	if entry != nil {
		entry.failedAttempts = val
		entry.Unlock()
	}
}

var sep = "/"

func splitNamespacedName(namespacedName string) (string, string) {
	if strings.Contains(namespacedName, sep) {
		s := strings.SplitN(namespacedName, sep, 2)
		if len(s) == 2 {
			return s[0], s[1]
		}
	}
	return namespacedName, ""
}

func getNamespacedName(namespace, name string) string {
	return namespace + sep + name
}

// hasResourceAnUpdateFunc returns true if the given resource type has a dedicated update function.
// It returns false if, upon an update event on this resource type, we instead need to first delete the old
// object and then add the new one.
func hasResourceAnUpdateFunc(objType reflect.Type) bool {
	switch objType {
	case factory.PodType,
		factory.NodeType,
		factory.PeerPodSelectorType,
		factory.PeerPodForNamespaceAndPodSelectorType,
		factory.EgressIPType,
		factory.EgressIPNamespaceType,
		factory.EgressIPPodType,
		factory.EgressNodeType,
		factory.CloudPrivateIPConfigType,
		factory.LocalPodSelectorType:
		return true
	}
	return false
}

// areResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func areResourcesEqual(objType reflect.Type, obj1, obj2 interface{}) (bool, error) {
	// switch based on type
	switch objType {
	case factory.PolicyType:
		np1, ok := obj1.(*knet.NetworkPolicy)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *knet.NetworkPolicy", obj1)
		}
		np2, ok := obj2.(*knet.NetworkPolicy)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *knet.NetworkPolicy", obj2)
		}
		return reflect.DeepEqual(np1, np2), nil

	case factory.NodeType:
		node1, ok := obj1.(*kapi.Node)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *kapi.Node", obj1)
		}
		node2, ok := obj2.(*kapi.Node)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *kapi.Node", obj2)
		}

		// when shouldUpdate is false, the hostsubnet is not assigned by ovn-kubernetes
		shouldUpdate, err := shouldUpdate(node2, node1)
		if err != nil {
			klog.Errorf(err.Error())
		}
		return !shouldUpdate, nil

	case factory.PeerServiceType:
		service1, ok := obj1.(*kapi.Service)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *kapi.Service", obj1)
		}
		service2, ok := obj2.(*kapi.Service)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *kapi.Service", obj2)
		}
		areEqual := reflect.DeepEqual(service1.Spec.ExternalIPs, service2.Spec.ExternalIPs) &&
			reflect.DeepEqual(service1.Spec.ClusterIP, service2.Spec.ClusterIP) &&
			reflect.DeepEqual(service1.Spec.ClusterIPs, service2.Spec.ClusterIPs) &&
			reflect.DeepEqual(service1.Spec.Type, service2.Spec.Type) &&
			reflect.DeepEqual(service1.Status.LoadBalancer.Ingress, service2.Status.LoadBalancer.Ingress)
		return areEqual, nil

	case factory.PodType,
		factory.EgressIPPodType,
		factory.PeerPodSelectorType,
		factory.PeerPodForNamespaceAndPodSelectorType,
		factory.LocalPodSelectorType:
		// For these types, there was no old vs new obj comparison in the original update code,
		// so pretend they're always different so that the update code gets executed
		return false, nil

	case factory.PeerNamespaceSelectorType,
		factory.PeerNamespaceAndPodSelectorType:
		// For these types there is no update code, so pretend old and new
		// objs are always equivalent and stop processing the update event.
		return true, nil

	case factory.EgressFirewallType:
		oldEgressFirewall, ok := obj1.(*egressfirewall.EgressFirewall)
		if !ok {
			return false, fmt.Errorf("could not cast obj1 of type %T to *egressfirewall.EgressFirewall", obj1)
		}
		newEgressFirewall, ok := obj2.(*egressfirewall.EgressFirewall)
		if !ok {
			return false, fmt.Errorf("could not cast obj2 of type %T to *egressfirewall.EgressFirewall", obj2)
		}
		return reflect.DeepEqual(oldEgressFirewall.Spec, newEgressFirewall.Spec), nil

	case factory.EgressIPType,
		factory.EgressIPNamespaceType,
		factory.EgressNodeType,
		factory.CloudPrivateIPConfigType:
		// force update path for EgressIP resource.
		return false, nil

	}

	return false, fmt.Errorf("no object comparison for type %s", objType)
}

// Given an object and its type, it returns the key for this object and an error if the key retrieval failed.
// For all namespaced resources, the key will be namespace/name. For resource types without a namespace,
// the key will be the object name itself.
func getResourceKey(objType reflect.Type, obj interface{}) (string, error) {
	switch objType {
	case factory.PolicyType:
		np, ok := obj.(*knet.NetworkPolicy)
		if !ok {
			return "", fmt.Errorf("could not cast %T object to *knet.NetworkPolicy", obj)
		}
		return getPolicyNamespacedName(np), nil

	case factory.NodeType,
		factory.EgressNodeType:
		node, ok := obj.(*kapi.Node)
		if !ok {
			return "", fmt.Errorf("could not cast %T object to *kapi.Node", obj)
		}
		return node.Name, nil

	case factory.PeerServiceType:
		service, ok := obj.(*kapi.Service)
		if !ok {
			return "", fmt.Errorf("could not cast %T object to *kapi.Service", obj)
		}
		return getNamespacedName(service.Namespace, service.Name), nil

	case factory.PodType,
		factory.PeerPodSelectorType,
		factory.PeerPodForNamespaceAndPodSelectorType,
		factory.LocalPodSelectorType,
		factory.EgressIPPodType:
		pod, ok := obj.(*kapi.Pod)
		if !ok {
			return "", fmt.Errorf("could not cast %T object to *kapi.Pod", obj)
		}
		return getNamespacedName(pod.Namespace, pod.Name), nil

	case factory.PeerNamespaceAndPodSelectorType,
		factory.PeerNamespaceSelectorType,
		factory.EgressIPNamespaceType:
		namespace, ok := obj.(*kapi.Namespace)
		if !ok {
			return "", fmt.Errorf("could not cast %T object to *kapi.Namespace", obj)
		}
		return namespace.Name, nil

	case factory.EgressFirewallType:
		egressFirewall, ok := obj.(*egressfirewall.EgressFirewall)
		if !ok {
			return "", fmt.Errorf("could not cast %T object to *egressfirewall.EgressFirewall", obj)
		}
		return getEgressFirewallNamespacedName(egressFirewall), nil

	case factory.EgressIPType:
		eIP, ok := obj.(*egressipv1.EgressIP)
		if !ok {
			return "", fmt.Errorf("could not cast %T object to *egressipv1.EgressIP", obj)
		}
		return eIP.Name, nil
	case factory.CloudPrivateIPConfigType:
		cloudPrivateIPConfig, ok := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		if !ok {
			return "", fmt.Errorf("could not cast %T object to *ocpcloudnetworkapi.CloudPrivateIPConfig", obj)
		}
		return cloudPrivateIPConfig.Name, nil
	}

	return "", fmt.Errorf("object type %s not supported", objType)
}

func (oc *Controller) getPortInfo(pod *kapi.Pod) *lpInfo {
	var portInfo *lpInfo
	key := util.GetLogicalPortName(pod.Namespace, pod.Name)
	if !util.PodWantsNetwork(pod) {
		// create dummy logicalPortInfo for host-networked pods
		mac, _ := net.ParseMAC("00:00:00:00:00:00")
		portInfo = &lpInfo{
			logicalSwitch: "host-networked",
			name:          key,
			uuid:          "host-networked",
			ips:           []*net.IPNet{},
			mac:           mac,
		}
	} else {
		portInfo, _ = oc.logicalPortCache.get(key)
	}
	return portInfo
}

// Given an object and its type, getInternalCacheEntry returns the internal cache entry for this object.
// This is now used only for pods, which will get their the logical port cache entry.
func (oc *Controller) getInternalCacheEntry(objType reflect.Type, obj interface{}) interface{} {
	switch objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		return oc.getPortInfo(pod)
	default:
		return nil
	}
}

// Given an object key and its type, getResourceFromInformerCache returns the latest state of the object
// from the informers cache.
func (oc *Controller) getResourceFromInformerCache(objType reflect.Type, key string) (interface{}, error) {
	var obj interface{}
	var err error

	switch objType {
	case factory.PolicyType:
		namespace, name := splitNamespacedName(key)
		obj, err = oc.watchFactory.GetNetworkPolicy(namespace, name)

	case factory.NodeType,
		factory.EgressNodeType:
		obj, err = oc.watchFactory.GetNode(key)

	case factory.PeerServiceType:
		namespace, name := splitNamespacedName(key)
		obj, err = oc.watchFactory.GetService(namespace, name)

	case factory.PodType,
		factory.PeerPodSelectorType,
		factory.PeerPodForNamespaceAndPodSelectorType,
		factory.LocalPodSelectorType,
		factory.EgressIPPodType:
		namespace, name := splitNamespacedName(key)
		obj, err = oc.watchFactory.GetPod(namespace, name)

	case factory.PeerNamespaceAndPodSelectorType,
		factory.PeerNamespaceSelectorType,
		factory.EgressIPNamespaceType:
		obj, err = oc.watchFactory.GetNamespace(key)

	case factory.EgressFirewallType:
		namespace, name := splitNamespacedName(key)
		obj, err = oc.watchFactory.GetEgressFirewall(namespace, name)

	case factory.EgressIPType:
		obj, err = oc.watchFactory.GetEgressIP(key)

	case factory.CloudPrivateIPConfigType:
		obj, err = oc.watchFactory.GetCloudPrivateIPConfig(key)

	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			objType)
	}
	return obj, err
}

// Given an object and its type, recordAddEvent records the add event on this object.
func (oc *Controller) recordAddEvent(objType reflect.Type, obj interface{}) {
	switch objType {
	case factory.PodType:
		klog.V(5).Infof("Recording add event on pod")
		pod := obj.(*kapi.Pod)
		oc.podRecorder.AddPod(pod.UID)
		metrics.GetConfigDurationRecorder().Start("pod", pod.Namespace, pod.Name)
	case factory.PolicyType:
		klog.V(5).Infof("Recording add event on network policy")
		np := obj.(*knet.NetworkPolicy)
		metrics.GetConfigDurationRecorder().Start("networkpolicy", np.Namespace, np.Name)
	}
}

// Given an object and its type, recordUpdateEvent records the update event on this object.
func (oc *Controller) recordUpdateEvent(objType reflect.Type, obj interface{}) {
	switch objType {
	case factory.PodType:
		klog.V(5).Infof("Recording update event on pod")
		pod := obj.(*kapi.Pod)
		metrics.GetConfigDurationRecorder().Start("pod", pod.Namespace, pod.Name)
	case factory.PolicyType:
		klog.V(5).Infof("Recording update event on network policy")
		np := obj.(*knet.NetworkPolicy)
		metrics.GetConfigDurationRecorder().Start("networkpolicy", np.Namespace, np.Name)
	}
}

// Given an object and its type, recordDeleteEvent records the delete event on this object. Only used for pods now.
func (oc *Controller) recordDeleteEvent(objType reflect.Type, obj interface{}) {
	switch objType {
	case factory.PodType:
		klog.V(5).Infof("Recording delete event on pod")
		pod := obj.(*kapi.Pod)
		oc.podRecorder.CleanPod(pod.UID)
		metrics.GetConfigDurationRecorder().Start("pod", pod.Namespace, pod.Name)
	case factory.PolicyType:
		klog.V(5).Infof("Recording delete event on network policy")
		np := obj.(*knet.NetworkPolicy)
		metrics.GetConfigDurationRecorder().Start("networkpolicy", np.Namespace, np.Name)
	}
}

func (oc *Controller) recordSuccessEvent(objType reflect.Type, obj interface{}) {
	switch objType {
	case factory.PodType:
		klog.V(5).Infof("Recording success event on pod")
		pod := obj.(*kapi.Pod)
		metrics.GetConfigDurationRecorder().End("pod", pod.Namespace, pod.Name)
	case factory.PolicyType:
		klog.V(5).Infof("Recording success event on network policy")
		np := obj.(*knet.NetworkPolicy)
		metrics.GetConfigDurationRecorder().End("networkpolicy", np.Namespace, np.Name)
	}
}

// Given an object and its type, recordErrorEvent records an error event on this object.
// Only used for pods now.
func (oc *Controller) recordErrorEvent(objType reflect.Type, obj interface{}, err error) {
	switch objType {
	case factory.PodType:
		klog.V(5).Infof("Recording error event on pod")
		pod := obj.(*kapi.Pod)
		oc.recordPodEvent(err, pod)
	}
}

// Given an object and its type, isResourceScheduled returns true if the object has been scheduled.
// Only applied to pods for now. Returns true for all other types.
func isResourceScheduled(objType reflect.Type, obj interface{}) bool {
	switch objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		return util.PodScheduled(pod)
	}
	return true
}

// Given an object type, resourceNeedsUpdate returns true if the object needs to invoke update during iterate retry.
func resourceNeedsUpdate(objType reflect.Type) bool {
	switch objType {
	case factory.EgressNodeType,
		factory.EgressIPType,
		factory.EgressIPPodType,
		factory.EgressIPNamespaceType,
		factory.CloudPrivateIPConfigType:
		return true
	}
	return false
}

// Given a *retryObjs instance, an object to add and a boolean specifying if the function was executed from
// iterateRetryResources, addResource adds the specified object to the cluster according to its type and
// returns the error, if any, yielded during object creation.
func (oc *Controller) addResource(objectsToRetry *retryObjs, obj interface{}, fromRetryLoop bool) error {
	var err error

	switch objectsToRetry.oType {
	case factory.PodType:
		pod, ok := obj.(*kapi.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T object to *knet.Pod", obj)
		}
		return oc.ensurePod(nil, pod, true)

	case factory.PolicyType:
		np, ok := obj.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *knet.NetworkPolicy", obj)
		}

		if err = oc.addNetworkPolicy(np); err != nil {
			klog.Infof("Network Policy retry delete failed for %s/%s, will try again later: %v",
				np.Namespace, np.Name, err)
			return err
		}

	case factory.NodeType:
		node, ok := obj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast %T object to *kapi.Node", obj)
		}
		var nodeParams *nodeSyncs
		if fromRetryLoop {
			_, nodeSync := oc.addNodeFailed.Load(node.Name)
			_, clusterRtrSync := oc.nodeClusterRouterPortFailed.Load(node.Name)
			_, mgmtSync := oc.mgmtPortFailed.Load(node.Name)
			_, gwSync := oc.gatewaysFailed.Load(node.Name)
			nodeParams = &nodeSyncs{
				nodeSync,
				clusterRtrSync,
				mgmtSync,
				gwSync}
		} else {
			nodeParams = &nodeSyncs{true, true, true, true}
		}

		if err = oc.addUpdateNodeEvent(node, nodeParams); err != nil {
			klog.Infof("Node retry delete failed for %s, will try again later: %v",
				node.Name, err)
			return err
		}

	case factory.PeerServiceType:
		service, ok := obj.(*kapi.Service)
		if !ok {
			return fmt.Errorf("could not cast peer service of type %T to *kapi.Service", obj)
		}
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		return oc.handlePeerServiceAdd(extraParameters.gp, service)

	case factory.PeerPodSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		return oc.handlePeerPodSelectorAddUpdate(extraParameters.gp, obj)

	case factory.PeerNamespaceAndPodSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		namespace := obj.(*kapi.Namespace)
		extraParameters.np.RLock()
		alreadyDeleted := extraParameters.np.deleted
		extraParameters.np.RUnlock()
		if alreadyDeleted {
			return nil
		}

		// start watching pods in this namespace and selected by the label selector in extraParameters.podSelector
		retryPeerPods := NewRetryObjs(
			factory.PeerPodForNamespaceAndPodSelectorType,
			namespace.Name,
			extraParameters.podSelector,
			nil,
			&NetworkPolicyExtraParameters{gp: extraParameters.gp},
		)
		// The AddFilteredPodHandler call might call handlePeerPodSelectorAddUpdate
		// on existing pods so we can't be holding the lock at this point
		podHandler, err := oc.WatchResource(retryPeerPods)
		if err != nil {
			klog.Errorf("Failed WatchResource for PeerNamespaceAndPodSelectorTypeVar: %v", err)
			return err
		}

		extraParameters.np.Lock()
		defer extraParameters.np.Unlock()
		if extraParameters.np.deleted {
			oc.watchFactory.RemovePodHandler(podHandler)
			return nil
		}
		extraParameters.np.podHandlerList = append(extraParameters.np.podHandlerList, podHandler)

	case factory.PeerPodForNamespaceAndPodSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		return oc.handlePeerPodSelectorAddUpdate(extraParameters.gp, obj)

	case factory.PeerNamespaceSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		namespace := obj.(*kapi.Namespace)
		// Update the ACL ...
		return oc.handlePeerNamespaceSelectorOnUpdate(extraParameters.np, extraParameters.gp, func() bool {
			// ... on condition that the added address set was not already in the 'gress policy
			return extraParameters.gp.addNamespaceAddressSet(namespace.Name)
		})

	case factory.LocalPodSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		return oc.handleLocalPodSelectorAddFunc(
			extraParameters.policy,
			extraParameters.np,
			extraParameters.portGroupIngressDenyName,
			extraParameters.portGroupEgressDenyName,
			obj)

	case factory.EgressFirewallType:
		var err error
		egressFirewall := obj.(*egressfirewall.EgressFirewall).DeepCopy()
		if err = oc.addEgressFirewall(egressFirewall); err != nil {
			egressFirewall.Status.Status = egressFirewallAddError
		} else {
			egressFirewall.Status.Status = egressFirewallAppliedCorrectly
			metrics.UpdateEgressFirewallRuleCount(float64(len(egressFirewall.Spec.Egress)))
			metrics.IncrementEgressFirewallCount()
		}
		if err := oc.updateEgressFirewallStatusWithRetry(egressFirewall); err != nil {
			klog.Errorf("Failed to update egress firewall status %s, error: %v", getEgressFirewallNamespacedName(egressFirewall), err)
		}
		return err

	case factory.EgressIPType:
		eIP := obj.(*egressipv1.EgressIP)
		return oc.reconcileEgressIP(nil, eIP)

	case factory.EgressIPNamespaceType:
		namespace := obj.(*kapi.Namespace)
		return oc.reconcileEgressIPNamespace(nil, namespace)

	case factory.EgressIPPodType:
		pod := obj.(*kapi.Pod)
		return oc.reconcileEgressIPPod(nil, pod)

	case factory.EgressNodeType:
		node := obj.(*kapi.Node)
		if err := oc.addNodeForEgress(node); err != nil {
			return err
		}
		nodeEgressLabel := util.GetNodeEgressLabel()
		nodeLabels := node.GetLabels()
		_, hasEgressLabel := nodeLabels[nodeEgressLabel]
		if hasEgressLabel {
			oc.setNodeEgressAssignable(node.Name, true)
		}
		isReady := oc.isEgressNodeReady(node)
		if isReady {
			oc.setNodeEgressReady(node.Name, true)
		}
		isReachable := oc.isEgressNodeReachable(node)
		if isReachable {
			oc.setNodeEgressReachable(node.Name, true)
		}
		if hasEgressLabel && isReachable && isReady {
			if err := oc.addEgressNode(node.Name); err != nil {
				return err
			}
		}

	case factory.CloudPrivateIPConfigType:
		cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return oc.reconcileCloudPrivateIPConfig(nil, cloudPrivateIPConfig)

	default:
		return fmt.Errorf("no add function for object type %s", objectsToRetry.oType)
	}

	return nil
}

// Given a *retryObjs instance, an old and a new object, updateResource updates the specified object in the cluster
// to its version in newObj according to its type and returns the error, if any, yielded during the object update.
// The inRetryCache boolean argument is to indicate if the given resource is in the retryCache or not.
func (oc *Controller) updateResource(objectsToRetry *retryObjs, oldObj, newObj interface{}, inRetryCache bool) error {
	switch objectsToRetry.oType {
	case factory.PodType:
		oldPod := oldObj.(*kapi.Pod)
		newPod := newObj.(*kapi.Pod)

		return oc.ensurePod(oldPod, newPod, inRetryCache || util.PodScheduled(oldPod) != util.PodScheduled(newPod))

	case factory.NodeType:
		newNode, ok := newObj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast newObj of type %T to *kapi.Node", newObj)
		}
		oldNode, ok := oldObj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast oldObj of type %T to *kapi.Node", oldObj)
		}
		// determine what actually changed in this update
		_, nodeSync := oc.addNodeFailed.Load(newNode.Name)
		_, failed := oc.nodeClusterRouterPortFailed.Load(newNode.Name)
		clusterRtrSync := failed || nodeChassisChanged(oldNode, newNode) || nodeSubnetChanged(oldNode, newNode)
		_, failed = oc.mgmtPortFailed.Load(newNode.Name)
		mgmtSync := failed || macAddressChanged(oldNode, newNode) || nodeSubnetChanged(oldNode, newNode)
		_, failed = oc.gatewaysFailed.Load(newNode.Name)
		gwSync := (failed || gatewayChanged(oldNode, newNode) ||
			nodeSubnetChanged(oldNode, newNode) || hostAddressesChanged(oldNode, newNode))

		return oc.addUpdateNodeEvent(newNode, &nodeSyncs{nodeSync, clusterRtrSync, mgmtSync, gwSync})

	case factory.PeerPodSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		return oc.handlePeerPodSelectorAddUpdate(extraParameters.gp, newObj)

	case factory.PeerPodForNamespaceAndPodSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		return oc.handlePeerPodSelectorAddUpdate(extraParameters.gp, newObj)

	case factory.LocalPodSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		return oc.handleLocalPodSelectorAddFunc(
			extraParameters.policy,
			extraParameters.np,
			extraParameters.portGroupIngressDenyName,
			extraParameters.portGroupEgressDenyName,
			newObj)

	case factory.EgressIPType:
		oldEIP := oldObj.(*egressipv1.EgressIP)
		newEIP := newObj.(*egressipv1.EgressIP)
		return oc.reconcileEgressIP(oldEIP, newEIP)

	case factory.EgressIPNamespaceType:
		oldNamespace := oldObj.(*kapi.Namespace)
		newNamespace := newObj.(*kapi.Namespace)
		return oc.reconcileEgressIPNamespace(oldNamespace, newNamespace)

	case factory.EgressIPPodType:
		oldPod := oldObj.(*kapi.Pod)
		newPod := newObj.(*kapi.Pod)
		return oc.reconcileEgressIPPod(oldPod, newPod)

	case factory.EgressNodeType:
		oldNode := oldObj.(*kapi.Node)
		newNode := newObj.(*kapi.Node)
		// Initialize the allocator on every update,
		// ovnkube-node/cloud-network-config-controller will make sure to
		// annotate the node with the egressIPConfig, but that might have
		// happened after we processed the ADD for that object, hence keep
		// retrying for all UPDATEs.
		if err := oc.initEgressIPAllocator(newNode); err != nil {
			klog.Warningf("Egress node initialization error: %v", err)
		}
		nodeEgressLabel := util.GetNodeEgressLabel()
		oldLabels := oldNode.GetLabels()
		newLabels := newNode.GetLabels()
		_, oldHadEgressLabel := oldLabels[nodeEgressLabel]
		_, newHasEgressLabel := newLabels[nodeEgressLabel]
		// If the node is not labeled for egress assignment, just return
		// directly, we don't really need to set the ready / reachable
		// status on this node if the user doesn't care about using it.
		if !oldHadEgressLabel && !newHasEgressLabel {
			return nil
		}
		if oldHadEgressLabel && !newHasEgressLabel {
			klog.Infof("Node: %s has been un-labeled, deleting it from egress assignment", newNode.Name)
			oc.setNodeEgressAssignable(oldNode.Name, false)
			return oc.deleteEgressNode(oldNode.Name)
		}
		isOldReady := oc.isEgressNodeReady(oldNode)
		isNewReady := oc.isEgressNodeReady(newNode)
		isNewReachable := oc.isEgressNodeReachable(newNode)
		oc.setNodeEgressReady(newNode.Name, isNewReady)
		oc.setNodeEgressReachable(newNode.Name, isNewReachable)
		if !oldHadEgressLabel && newHasEgressLabel {
			klog.Infof("Node: %s has been labeled, adding it for egress assignment", newNode.Name)
			oc.setNodeEgressAssignable(newNode.Name, true)
			if isNewReady && isNewReachable {
				if err := oc.addEgressNode(newNode.Name); err != nil {
					return err
				}
			} else {
				klog.Warningf("Node: %s has been labeled, but node is not ready and reachable, cannot use it for egress assignment", newNode.Name)
			}
			return nil
		}
		if isOldReady == isNewReady {
			return nil
		}
		if !isNewReady {
			klog.Warningf("Node: %s is not ready, deleting it from egress assignment", newNode.Name)
			if err := oc.deleteEgressNode(newNode.Name); err != nil {
				return err
			}
		} else if isNewReady && isNewReachable {
			klog.Infof("Node: %s is ready and reachable, adding it for egress assignment", newNode.Name)
			if err := oc.addEgressNode(newNode.Name); err != nil {
				return err
			}
		}
		return nil

	case factory.CloudPrivateIPConfigType:
		oldCloudPrivateIPConfig := oldObj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		newCloudPrivateIPConfig := newObj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return oc.reconcileCloudPrivateIPConfig(oldCloudPrivateIPConfig, newCloudPrivateIPConfig)
	}

	return fmt.Errorf("no update function for object type %s", objectsToRetry.oType)
}

// Given a *retryObjs instance, an object and optionally a cachedObj, deleteResource deletes the object from the cluster
// according to the delete logic of its resource type. cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (oc *Controller) deleteResource(objectsToRetry *retryObjs, obj, cachedObj interface{}) error {
	switch objectsToRetry.oType {
	case factory.PodType:
		var portInfo *lpInfo
		pod := obj.(*kapi.Pod)

		if cachedObj != nil {
			portInfo = cachedObj.(*lpInfo)
		}
		oc.logicalPortCache.remove(util.GetLogicalPortName(pod.Namespace, pod.Name))
		return oc.removePod(pod, portInfo)

	case factory.PolicyType:
		var cachedNP *networkPolicy
		knp, ok := obj.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.NetworkPolicy", obj)
		}

		if cachedObj != nil {
			if cachedNP, ok = cachedObj.(*networkPolicy); !ok {
				cachedNP = nil
			}
		}
		return oc.deleteNetworkPolicy(knp, cachedNP)

	case factory.NodeType:
		node, ok := obj.(*kapi.Node)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *knet.Node", obj)
		}
		return oc.deleteNodeEvent(node)

	case factory.PeerServiceType:
		service, ok := obj.(*kapi.Service)
		if !ok {
			return fmt.Errorf("could not cast peer service of type %T to *kapi.Service", obj)
		}
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		return oc.handlePeerServiceDelete(extraParameters.gp, service)

	case factory.PeerPodSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		return oc.handlePeerPodSelectorDelete(extraParameters.gp, obj)

	case factory.PeerNamespaceAndPodSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		// when the namespace labels no longer apply
		// remove the namespaces pods from the address_set
		var errs []error
		namespace := obj.(*kapi.Namespace)
		pods, _ := oc.watchFactory.GetPods(namespace.Name)

		for _, pod := range pods {
			if err := oc.handlePeerPodSelectorDelete(extraParameters.gp, pod); err != nil {
				errs = append(errs, err)
			}
		}
		return kerrorsutil.NewAggregate(errs)

	case factory.PeerPodForNamespaceAndPodSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		return oc.handlePeerPodSelectorDelete(extraParameters.gp, obj)

	case factory.PeerNamespaceSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		namespace := obj.(*kapi.Namespace)
		// Remove namespace address set from the *gress policy in cache
		// (done in gress.delNamespaceAddressSet()), and then update ACLs
		return oc.handlePeerNamespaceSelectorOnUpdate(extraParameters.np, extraParameters.gp, func() bool {
			// ... on condition that the removed address set was in the 'gress policy
			return extraParameters.gp.delNamespaceAddressSet(namespace.Name)
		})

	case factory.PeerPodForNamespaceAndPodSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		return oc.handleLocalPodSelectorDelFunc(
			extraParameters.policy,
			extraParameters.np,
			extraParameters.portGroupIngressDenyName,
			extraParameters.portGroupEgressDenyName,
			obj)

	case factory.LocalPodSelectorType:
		extraParameters := objectsToRetry.extraParameters.(*NetworkPolicyExtraParameters)
		return oc.handleLocalPodSelectorDelFunc(
			extraParameters.policy,
			extraParameters.np,
			extraParameters.portGroupIngressDenyName,
			extraParameters.portGroupEgressDenyName,
			obj)

	case factory.EgressFirewallType:
		egressFirewall := obj.(*egressfirewall.EgressFirewall)
		if err := oc.deleteEgressFirewall(egressFirewall); err != nil {
			return err
		}
		metrics.UpdateEgressFirewallRuleCount(float64(-len(egressFirewall.Spec.Egress)))
		metrics.DecrementEgressFirewallCount()
		return nil

	case factory.EgressIPType:
		eIP := obj.(*egressipv1.EgressIP)
		return oc.reconcileEgressIP(eIP, nil)

	case factory.EgressIPNamespaceType:
		namespace := obj.(*kapi.Namespace)
		return oc.reconcileEgressIPNamespace(namespace, nil)

	case factory.EgressIPPodType:
		pod := obj.(*kapi.Pod)
		return oc.reconcileEgressIPPod(pod, nil)

	case factory.EgressNodeType:
		node := obj.(*kapi.Node)
		if err := oc.deleteNodeForEgress(node); err != nil {
			return err
		}
		nodeEgressLabel := util.GetNodeEgressLabel()
		nodeLabels := node.GetLabels()
		if _, hasEgressLabel := nodeLabels[nodeEgressLabel]; hasEgressLabel {
			if err := oc.deleteEgressNode(node.Name); err != nil {
				return err
			}
		}
		return nil

	case factory.CloudPrivateIPConfigType:
		cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return oc.reconcileCloudPrivateIPConfig(cloudPrivateIPConfig, nil)

	default:
		return fmt.Errorf("object type %s not supported", objectsToRetry.oType)
	}
}

type localRetryEntry struct {
	key string // the key in the retryObjs map holding retryObjs value
	// cached newObj holds k8s resource failed during add operation
	newObj interface{}
	// cached oldObj holds k8s resource failed during delete operation
	oldObj interface{}
	// resource retrieved from the K8s API Server
	kObj interface{}
}

func (oc *Controller) resourceRetry(r *retryObjs, lre *localRetryEntry, now time.Time, updateAll bool) {
	objKey := lre.key
	entry := r.ensureRetryEntryLocked(objKey, nil)
	if entry == nil {
		klog.V(5).Infof("%v resource %s was not found in the iterateRetryResources map while retrying resource setup", r.oType, objKey)
		return
	}
	defer entry.Unlock()
	if entry.ignore {
		klog.V(5).Infof("Skipping %v resource %s setup since ignore is set to true", r.oType, objKey)
		return
	} else if entry.failedAttempts >= maxFailedAttempts {
		klog.Warningf("Dropping retry entry for %s %s: exceeded number of failed attempts",
			r.oType, objKey)
		r.deleteRetryObj(objKey, false)
		return
	}
	entry.backoffSec = entry.backoffSec * 2
	if entry.backoffSec > 60 {
		entry.backoffSec = 60
	}
	backoff := (entry.backoffSec * time.Second) + (time.Duration(rand.Intn(500)) * time.Millisecond)
	objTimer := entry.timeStamp.Add(backoff)
	if !updateAll && now.Before(objTimer) {
		klog.V(5).Infof("Attempting retry of %s %s before timer (time: %s): skip", r.oType, objKey, objTimer)
		return
	}

	// storing original obj for metrics
	var initObj interface{}
	if entry.newObj != nil {
		initObj = entry.newObj
	} else if entry.oldObj != nil {
		initObj = entry.oldObj
	}

	klog.Infof("Retry object setup: %s %s", r.oType, objKey)

	if entry.newObj != nil {
		entry.newObj = lre.kObj
	}
	if resourceNeedsUpdate(r.oType) && entry.config != nil && entry.newObj != nil {
		klog.Infof("%v retry: updating object %s", r.oType, objKey)
		if err := oc.updateResource(r, entry.config, entry.newObj, true); err != nil {
			klog.Infof("%v retry update failed for %s, will try again later: %v", r.oType, objKey, err)
			entry.timeStamp = time.Now()
			entry.failedAttempts++
			return
		}
		// successfully cleaned up new and old object, remove it from the retry cache
		entry.newObj = nil
		entry.config = nil
	} else {
		// delete old object if needed
		if entry.oldObj != nil {
			klog.Infof("Removing old object: %s %s", r.oType, objKey)
			if !isResourceScheduled(r.oType, entry.oldObj) {
				klog.V(5).Infof("Retry: %s %s not scheduled", r.oType, objKey)
				entry.failedAttempts++
				return
			}
			if err := oc.deleteResource(r, entry.oldObj, entry.config); err != nil {
				klog.Infof("Retry delete failed for %s %s, will try again later: %v", r.oType, objKey, err)
				entry.timeStamp = time.Now()
				entry.failedAttempts++
				return
			}
			// successfully cleaned up old object, remove it from the retry cache
			entry.oldObj = nil
		}

		// create new object if needed
		if entry.newObj != nil {
			klog.Infof("Adding new object: %s %s", r.oType, objKey)
			if !isResourceScheduled(r.oType, entry.newObj) {
				klog.V(5).Infof("Retry: %s %s not scheduled", r.oType, objKey)
				entry.failedAttempts++
				return
			}
			if err := oc.addResource(r, entry.newObj, true); err != nil {
				klog.Infof("Retry add failed for %s %s, will try again later: %v", r.oType, objKey, err)
				entry.timeStamp = time.Now()
				entry.failedAttempts++
				return
			}
			// successfully cleaned up new object, remove it from the retry cache
			entry.newObj = nil
		}
	}

	klog.Infof("Retry successful for %s %s after %d failed attempt(s)", r.oType, objKey, entry.failedAttempts)
	if initObj != nil {
		oc.recordSuccessEvent(r.oType, initObj)
	}
	r.deleteRetryObj(objKey, false)
}

// iterateRetryResources checks if any outstanding resource objects exist and if so it tries to
// re-add them. updateAll forces all objects to be attempted to be retried regardless.
func (oc *Controller) iterateRetryResources(r *retryObjs, updateAll bool) {
	r.retryMutex.Lock()
	now := time.Now()
	localRetryEntries := make([]*localRetryEntry, 0, len(r.entries))
	var kObj interface{}
	var err error
	for objKey, entry := range r.entries {
		// check if we need to create the object
		if entry.newObj != nil {
			// get the latest version of the object from the informer;
			// if it doesn't exist we are not going to create the new object.
			kObj, err = oc.getResourceFromInformerCache(r.oType, objKey)
			if err != nil {
				if kerrors.IsNotFound(err) {
					klog.Infof("%s %s not found in the informers cache,"+
						" not going to retry object create", r.oType, objKey)
					kObj = nil
				} else {
					klog.Errorf("Failed to look up %s %s in the informers cache,"+
						" will retry later: %v", r.oType, objKey, err)
					continue
				}
			}
		}
		klog.Infof("Gathered %v resource %s for retry", r.oType, objKey)
		localRetryEntries = append(localRetryEntries, &localRetryEntry{objKey, entry.newObj, entry.oldObj, kObj})
	}
	r.retryMutex.Unlock()

	// Now process the above list of pods that need re-try by holding the lock for each one of them.
	klog.V(5).Infof("Going to retry resource setup for %d number of resource", len(localRetryEntries))

	wg := &sync.WaitGroup{}
	for _, lre := range localRetryEntries {
		wg.Add(1)
		go func(lre *localRetryEntry) {
			defer wg.Done()
			oc.resourceRetry(r, lre, now, updateAll)
		}(lre)
	}
	klog.V(5).Infof("Waiting for all the %s retry setup to complete in iterateRetryResources", r.oType)
	wg.Wait()
	klog.V(5).Infof("Function iterateRetryResources ended (in %v)", time.Since(now))
}

// periodicallyRetryResources tracks retryObjs and checks if any object needs to be retried for add or delete every
// retryObjInterval seconds or when requested through retryChan.
func (oc *Controller) periodicallyRetryResources(r *retryObjs) {
	timer := time.NewTicker(retryObjInterval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			oc.iterateRetryResources(r, false)

		case <-r.retryChan:
			klog.V(5).Infof("Retry channel got triggered: retrying failed objects of type %s", r.oType)
			oc.iterateRetryResources(r, true)
			timer.Reset(retryObjInterval)

		case <-oc.stopChan:
			klog.V(5).Infof("Stop channel got triggered: will stop retrying failed objects of type %s", r.oType)
			return
		}
	}
}

// Given a *retryObjs instance, getSyncResourcesFunc retuns the sync function for a given resource type.
// This will be then called on all existing objects when a watcher is started.
func (oc *Controller) getSyncResourcesFunc(r *retryObjs) (func([]interface{}) error, error) {

	var syncFunc func([]interface{}) error

	switch r.oType {
	case factory.PodType:
		syncFunc = oc.syncPodsRetriable

	case factory.PolicyType:
		syncFunc = oc.syncNetworkPolicies

	case factory.NodeType:
		syncFunc = oc.syncNodesRetriable

	case factory.PeerServiceType,
		factory.PeerNamespaceAndPodSelectorType:
		syncFunc = nil

	case factory.PeerPodSelectorType:
		extraParameters := r.extraParameters.(*NetworkPolicyExtraParameters)
		syncFunc = func(objs []interface{}) error {
			return oc.handlePeerPodSelectorAddUpdate(extraParameters.gp, objs...)
		}

	case factory.PeerPodForNamespaceAndPodSelectorType:
		extraParameters := r.extraParameters.(*NetworkPolicyExtraParameters)
		syncFunc = func(objs []interface{}) error {
			return oc.handlePeerPodSelectorAddUpdate(extraParameters.gp, objs...)
		}

	case factory.PeerNamespaceSelectorType:
		extraParameters := r.extraParameters.(*NetworkPolicyExtraParameters)
		// the function below will never fail, so there's no point in making it retriable...
		syncFunc = func(i []interface{}) error {
			// This needs to be a write lock because there's no locking around 'gress policies
			extraParameters.np.Lock()
			defer extraParameters.np.Unlock()
			// We load the existing address set into the 'gress policy.
			// Notice that this will make the AddFunc for this initial
			// address set a noop.
			// The ACL must be set explicitly after setting up this handler
			// for the address set to be considered.
			extraParameters.gp.addNamespaceAddressSets(i)
			return nil
		}

	case factory.LocalPodSelectorType:
		syncFunc = r.syncFunc

	case factory.EgressFirewallType:
		syncFunc = oc.syncEgressFirewall

	case factory.EgressIPType:
		syncFunc = oc.syncEgressIPs

	case factory.EgressNodeType:
		syncFunc = oc.initClusterEgressPolicies

	case factory.EgressIPPodType,
		factory.EgressIPNamespaceType,
		factory.CloudPrivateIPConfigType:
		syncFunc = nil

	default:
		return nil, fmt.Errorf("no sync function for object type %s", r.oType)
	}

	return syncFunc, nil
}

// Given an object and its type, isObjectInTerminalState returns true if the object is a in terminal state.
// This is used now for pods that are either in a PodSucceeded or in a PodFailed state.
func (oc *Controller) isObjectInTerminalState(objType reflect.Type, obj interface{}) bool {
	switch objType {
	case factory.PodType,
		factory.PeerPodSelectorType,
		factory.PeerPodForNamespaceAndPodSelectorType,
		factory.LocalPodSelectorType,
		factory.EgressIPPodType:
		pod := obj.(*kapi.Pod)
		return util.PodCompleted(pod)

	default:
		return false
	}
}

type resourceEvent string

var (
	resourceEventAdd    resourceEvent = "add"
	resourceEventUpdate resourceEvent = "update"
)

// processObjectInTerminalState is executed when an object has been added or updated and is actually in a terminal state
// already. The add or update event is not valid for such object, which we now remove from the cluster in order to
// free its resources. (for now, this applies to completed pods)
func (oc *Controller) processObjectInTerminalState(objectsToRetry *retryObjs, obj interface{}, key string, event resourceEvent) {
	// The object is in a terminal state: delete it from the cluster, delete its retry entry and return.
	klog.Infof("Detected object %s of type %s in terminal state (e.g. completed)"+
		" during %s event: will remove it", key, objectsToRetry.oType, event)

	internalCacheEntry := oc.getInternalCacheEntry(objectsToRetry.oType, obj)
	objectsToRetry.initRetryObjWithDelete(obj, key, internalCacheEntry, true) // set up the retry obj for deletion

	if err := oc.deleteResource(objectsToRetry, obj, internalCacheEntry); err != nil {
		klog.Errorf("Failed to delete object %s of type %s in terminal state, during %s event: %v",
			key, objectsToRetry.oType, event, err)
		oc.recordErrorEvent(objectsToRetry.oType, obj, err)
		objectsToRetry.unSkipRetryObj(key)
		objectsToRetry.increaseFailedAttemptsCounter(key)
		return
	}
	objectsToRetry.removeDeleteFromRetryObj(key)
	objectsToRetry.deleteRetryObj(key, true)
}

// WatchResource starts the watching of a resource type, manages its retry entries and calls
// back the appropriate handler logic. It also starts a goroutine that goes over all retry objects
// periodically or when explicitly requested.
// Note: when applying WatchResource to a new resource type, the appropriate resource-specific logic must be added to the
// the different methods it calls.
func (oc *Controller) WatchResource(objectsToRetry *retryObjs) (*factory.Handler, error) {
	addHandlerFunc, err := oc.watchFactory.GetResourceHandlerFunc(objectsToRetry.oType)
	if err != nil {
		return nil, fmt.Errorf("no resource handler function found for resource %v. "+
			"Cannot watch this resource.", objectsToRetry.oType)
	}
	syncFunc, err := oc.getSyncResourcesFunc(objectsToRetry)
	if err != nil {
		return nil, fmt.Errorf("no sync function found for resource %v. "+
			"Cannot watch this resource.", objectsToRetry.oType)
	}

	// create the actual watcher
	handler, err := addHandlerFunc(
		objectsToRetry.namespaceForFilteredHandler,     // filter out objects not in this namespace
		objectsToRetry.labelSelectorForFilteredHandler, // filter out objects not matching these labels
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				oc.recordAddEvent(objectsToRetry.oType, obj)

				key, err := getResourceKey(objectsToRetry.oType, obj)
				if err != nil {
					klog.Errorf("Upon add event: %v", err)
					return
				}
				klog.V(5).Infof("Add event received for %s, key=%s", objectsToRetry.oType, key)

				objectsToRetry.initRetryObjWithAdd(obj, key)
				objectsToRetry.skipRetryObj(key) // prevent iterateRetryResources from processing this entry

				// This only applies to pod watchers (pods + dynamic network policy handlers watching pods):
				// if ovnkube-master is restarted, it will gets all the add events with completed pods
				if oc.isObjectInTerminalState(objectsToRetry.oType, obj) {
					oc.processObjectInTerminalState(objectsToRetry, obj, key, resourceEventAdd)
					return
				}

				// If there is a delete entry with the same key, we got an add event for an object
				// with the same name as a previous object that failed deletion.
				// Destroy the old object before we add the new one.
				//
				// Note it is okay to access retryEntry without lock here, as the entry is either
				// accessed by iterateRetryResources or in add/update/delete handler of this resource;
				// the former is prevented as its ignore field is set to true, and the latter is serialized.
				if retryEntry := objectsToRetry.getObjRetryEntry(key); retryEntry != nil && retryEntry.oldObj != nil {
					klog.Infof("Detected stale object during new object"+
						" add of type %s with the same key: %s",
						objectsToRetry.oType, key)
					internalCacheEntry := oc.getInternalCacheEntry(objectsToRetry.oType, obj)
					if err := oc.deleteResource(objectsToRetry, obj, internalCacheEntry); err != nil {
						klog.Errorf("Failed to delete old object %s of type %s,"+
							" during add event: %v", key, objectsToRetry.oType, err)
						oc.recordErrorEvent(objectsToRetry.oType, obj, err)
						objectsToRetry.unSkipRetryObj(key) // let iterateRetryResources process this entry
						objectsToRetry.increaseFailedAttemptsCounter(key)
						return
					}
					objectsToRetry.removeDeleteFromRetryObj(key)
				}
				start := time.Now()
				if err := oc.addResource(objectsToRetry, obj, false); err != nil {
					klog.Errorf("Failed to create %s %s, error: %v", objectsToRetry.oType, key, err)
					oc.recordErrorEvent(objectsToRetry.oType, obj, err)
					objectsToRetry.unSkipRetryObj(key) // let iterateRetryResources process this entry
					objectsToRetry.increaseFailedAttemptsCounter(key)
					return
				}
				klog.Infof("Creating %s %s took: %v", objectsToRetry.oType, key, time.Since(start))
				objectsToRetry.deleteRetryObj(key, true)
				oc.recordSuccessEvent(objectsToRetry.oType, obj)
			},

			UpdateFunc: func(old, newer interface{}) {
				// skip the whole update if old and newer are equal
				areEqual, err := areResourcesEqual(objectsToRetry.oType, old, newer)
				if err != nil {
					klog.Errorf("Could not compare old and newer resource objects of type %s: %v",
						objectsToRetry.oType, err)
					return
				}
				klog.V(5).Infof("Update event received for resource %s, old object is equal to new: %t",
					objectsToRetry.oType, areEqual)
				if areEqual {
					return
				}
				oc.recordUpdateEvent(objectsToRetry.oType, newer)

				// get the object keys for newer and old (expected to be the same)
				newKey, err := getResourceKey(objectsToRetry.oType, newer)
				if err != nil {
					klog.Errorf("Update of %s failed when looking up key of new obj: %v",
						objectsToRetry.oType, err)
					return
				}
				oldKey, err := getResourceKey(objectsToRetry.oType, old)
				if err != nil {
					klog.Errorf("Update of %s failed when looking up key of old obj: %v",
						objectsToRetry.oType, err)
					return
				}

				// skip the whole update if the new object doesn't exist anymore in the API server
				newer, err = oc.getResourceFromInformerCache(objectsToRetry.oType, newKey)
				if err != nil {
					klog.Warningf("Unable to get %s %s from informer cache (perhaps it was already"+
						" deleted?), skipping update: %v", objectsToRetry.oType, newKey, err)
					return
				}

				klog.V(5).Infof("Update event received for %s, oldKey=%s, newKey=%s",
					objectsToRetry.oType, oldKey, newKey)

				objectsToRetry.skipRetryObj(newKey)
				hasUpdateFunc := hasResourceAnUpdateFunc(objectsToRetry.oType)

				// STEP 1:
				// Delete existing (old) object if:
				// a) it has a retry entry marked for deletion and doesn't use update or
				// b) the resource is in terminal state (e.g. pod is completed) or
				// c) this resource type has no update function, so an update means delete old obj and add new one
				//
				// Note it is okay to access retryEntry without lock here, as the entry is either
				// accessed by iterateRetryResources or in add/update/delete handler of this resource;
				// the former is prevented as its ignore field is set to true, and the latter is serialized.
				retryEntry := objectsToRetry.getObjRetryEntry(oldKey)
				if retryEntry != nil && retryEntry.oldObj != nil {
					// [step 1a] there is a retry entry marked for deletion
					klog.Infof("Found retry entry for %s %s marked for deletion: will delete the object",
						objectsToRetry.oType, oldKey)
					if err := oc.deleteResource(objectsToRetry, retryEntry.oldObj,
						retryEntry.config); err != nil {
						klog.Errorf("Failed to delete stale object %s, during update: %v", oldKey, err)
						oc.recordErrorEvent(objectsToRetry.oType, retryEntry.oldObj, err)
						objectsToRetry.initRetryObjWithAdd(newer, newKey)
						objectsToRetry.unSkipRetryObj(newKey)
						objectsToRetry.increaseFailedAttemptsCounter(newKey)
						return
					}
					// remove the old object from retry entry since it was correctly deleted
					objectsToRetry.removeDeleteFromRetryObj(oldKey)

				} else if oc.isObjectInTerminalState(objectsToRetry.oType, newer) { // check the latest status on newer
					// [step 1b] The object is in a terminal state: delete it from the cluster,
					// delete its retry entry and return. This only applies to pod watchers
					// (pods + dynamic network policy handlers watching pods).
					oc.processObjectInTerminalState(objectsToRetry, newer, newKey, resourceEventUpdate)
					return

				} else if !hasUpdateFunc {
					// [step 1c] if this resource type has no update function,
					// delete old obj and in step 2 add the new one
					var existingCacheEntry interface{}
					if retryEntry != nil {
						existingCacheEntry = retryEntry.config
					}
					klog.Infof("Deleting old %s of type %s during update", oldKey, objectsToRetry.oType)
					if err := oc.deleteResource(objectsToRetry, old, existingCacheEntry); err != nil {
						klog.Errorf("Failed to delete %s %s, during update: %v",
							objectsToRetry.oType, oldKey, err)
						oc.recordErrorEvent(objectsToRetry.oType, old, err)
						objectsToRetry.initRetryObjWithDelete(old, oldKey, nil, false)
						objectsToRetry.initRetryObjWithAdd(newer, newKey)
						objectsToRetry.unSkipRetryObj(oldKey)
						objectsToRetry.increaseFailedAttemptsCounter(oldKey)
						return
					}
					// remove the old object from retry entry since it was correctly deleted
					objectsToRetry.removeDeleteFromRetryObj(oldKey)
				}

				// STEP 2:
				// Execute the update function for this resource type; resort to add if no update
				// function is available.
				if hasUpdateFunc {
					// if this resource type has an update func, just call the update function
					if err := oc.updateResource(objectsToRetry, old, newer, objectsToRetry.checkRetryObj(newKey)); err != nil {
						klog.Errorf("Failed to update %s, old=%s, new=%s, error: %v",
							objectsToRetry.oType, oldKey, newKey, err)
						oc.recordErrorEvent(objectsToRetry.oType, newer, err)
						if resourceNeedsUpdate(objectsToRetry.oType) {
							objectsToRetry.initRetryObjWithUpdate(old, newer, newKey)
						} else {
							objectsToRetry.initRetryObjWithAdd(newer, newKey)
						}
						objectsToRetry.unSkipRetryObj(newKey)
						objectsToRetry.increaseFailedAttemptsCounter(newKey)
						return
					}
				} else { // we previously deleted old object, now let's add the new one
					if err := oc.addResource(objectsToRetry, newer, false); err != nil {
						oc.recordErrorEvent(objectsToRetry.oType, newer, err)
						objectsToRetry.initRetryObjWithAdd(newer, newKey)
						objectsToRetry.unSkipRetryObj(newKey)
						objectsToRetry.increaseFailedAttemptsCounter(newKey)
						klog.Errorf("Failed to add %s %s, during update: %v",
							objectsToRetry.oType, newKey, err)
						return
					}
				}
				objectsToRetry.deleteRetryObj(newKey, true)
				oc.recordSuccessEvent(objectsToRetry.oType, newer)

			},
			DeleteFunc: func(obj interface{}) {
				oc.recordDeleteEvent(objectsToRetry.oType, obj)
				key, err := getResourceKey(objectsToRetry.oType, obj)
				if err != nil {
					klog.Errorf("Delete of %s failed: %v", objectsToRetry.oType, err)
					return
				}
				klog.V(5).Infof("Delete event received for %s %s", objectsToRetry.oType, key)
				// If object is in terminal state, we would have already deleted it during update.
				// No reason to attempt to delete it here again.
				if oc.isObjectInTerminalState(objectsToRetry.oType, obj) {
					klog.Infof("Ignoring delete event for resource in terminal state %s %s",
						objectsToRetry.oType, key)
					return
				}
				objectsToRetry.skipRetryObj(key)
				internalCacheEntry := oc.getInternalCacheEntry(objectsToRetry.oType, obj)
				objectsToRetry.initRetryObjWithDelete(obj, key, internalCacheEntry, false) // set up the retry obj for deletion
				if err := oc.deleteResource(objectsToRetry, obj, internalCacheEntry); err != nil {
					objectsToRetry.unSkipRetryObj(key)
					objectsToRetry.increaseFailedAttemptsCounter(key)
					klog.Errorf("Failed to delete %s %s, error: %v", objectsToRetry.oType, key, err)
					return
				}
				objectsToRetry.deleteRetryObj(key, true)
				oc.recordSuccessEvent(objectsToRetry.oType, obj)
			},
		},
		syncFunc) // adds all existing objects at startup

	if err != nil {
		return nil, fmt.Errorf("watchResource for resource %v. "+
			"Failed addHandlerFunc: %v", objectsToRetry.oType, err)
	}

	// track the retry entries and every 30 seconds (or upon explicit request) check if any objects
	// need to be retried
	go oc.periodicallyRetryResources(objectsToRetry)

	return handler, nil
}
