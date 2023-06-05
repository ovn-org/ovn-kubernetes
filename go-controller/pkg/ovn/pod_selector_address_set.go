package ovn

import (
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kerrorsutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// PodSelectorAddressSet should always be accessed with oc.podSelectorAddressSets key lock
type PodSelectorAddressSet struct {
	// unique key that identifies given PodSelectorAddressSet
	key string

	// backRefs is a map of objects that use this address set.
	// keys must be unique for all possible users, e.g. for NetworkPolicy use (np *networkPolicy) getKeyWithKind().
	// Must only be changed with oc.podSelectorAddressSets Lock.
	backRefs map[string]bool

	// handler is either pod or namespace handler
	handler *factory.Handler

	podSelector       labels.Selector
	namespaceSelector labels.Selector
	// namespace is used when namespaceSelector is nil to set static namespace
	namespace string
	// if needsCleanup is true, try to cleanup before doing any other ops,
	// is cleanup returns error, return error for the op
	needsCleanup bool
	addrSetDbIDs *libovsdbops.DbObjectIDs

	// handlerResources holds the data that is used and updated by the handlers.
	handlerResources *PodSelectorAddrSetHandlerInfo
}

// EnsurePodSelectorAddressSet returns address set for requested (podSelector, namespaceSelector, namespace).
// If namespaceSelector is nil, namespace will be used with podSelector statically.
// podSelector should not be nil, use metav1.LabelSelector{} to match all pods.
// namespaceSelector can only be nil when namespace is set, use metav1.LabelSelector{} to match all namespaces.
// podSelector = metav1.LabelSelector{} + static namespace may be replaced with namespace address set,
// podSelector = metav1.LabelSelector{} + namespaceSelector may be replaced with a set of namespace address sets,
// but both cases will work here too.
//
// backRef is the key that should be used for cleanup.
// if err != nil, cleanup is required by calling DeletePodSelectorAddressSet or EnsurePodSelectorAddressSet again.
// psAddrSetHashV4, psAddrSetHashV6 may be set to empty string if address set for that ipFamily wasn't created.
func (bnc *BaseNetworkController) EnsurePodSelectorAddressSet(podSelector, namespaceSelector *metav1.LabelSelector,
	namespace, backRef string) (addrSetKey, psAddrSetHashV4, psAddrSetHashV6 string, err error) {
	if podSelector == nil {
		err = fmt.Errorf("pod selector is nil")
		return
	}
	if namespaceSelector == nil && namespace == "" {
		err = fmt.Errorf("namespace selector is nil and namespace is empty")
		return
	}
	if namespaceSelector != nil {
		// namespace will be ignored in this case
		namespace = ""
	}
	var nsSel, podSel labels.Selector
	if namespaceSelector != nil {
		nsSel, err = metav1.LabelSelectorAsSelector(namespaceSelector)
		if err != nil {
			err = fmt.Errorf("can't parse namespace selector %v: %w", namespaceSelector, err)
			return
		}
	}

	podSel, err = metav1.LabelSelectorAsSelector(podSelector)
	if err != nil {
		err = fmt.Errorf("can't parse pod selector %v: %w", podSelector, err)
		return
	}
	addrSetKey = getPodSelectorKey(podSelector, namespaceSelector, namespace)
	err = bnc.podSelectorAddressSets.DoWithLock(addrSetKey, func(key string) error {
		psAddrSet, found := bnc.podSelectorAddressSets.Load(key)
		if !found {
			psAddrSet = &PodSelectorAddressSet{
				key:               key,
				backRefs:          map[string]bool{},
				podSelector:       podSel,
				namespaceSelector: nsSel,
				namespace:         namespace,
				addrSetDbIDs:      getPodSelectorAddrSetDbIDs(addrSetKey, bnc.controllerName),
			}
			err = psAddrSet.init(bnc)
			// save object anyway for future use or cleanup
			bnc.podSelectorAddressSets.LoadOrStore(key, psAddrSet)
			if err != nil {
				psAddrSet.needsCleanup = true
				return fmt.Errorf("failed to init pod selector address set %s: %v", addrSetKey, err)
			}
		}
		if psAddrSet.needsCleanup {
			cleanupErr := psAddrSet.destroy(bnc)
			if cleanupErr != nil {
				return fmt.Errorf("failed to cleanup pod selector address set %s: %v", addrSetKey, err)
			}
			// psAddrSet.destroy will set psAddrSet.needsCleanup to false if no error was returned
			// try to init again
			err = psAddrSet.init(bnc)
			if err != nil {
				psAddrSet.needsCleanup = true
				return fmt.Errorf("failed to init pod selector address set %s after cleanup: %v", addrSetKey, err)
			}
		}
		// psAddrSet is successfully inited, and doesn't need cleanup
		psAddrSet.backRefs[backRef] = true
		psAddrSetHashV4, psAddrSetHashV6, err = psAddrSet.handlerResources.GetASHashNames()
		return err
	})
	if err != nil {
		return
	}
	return
}

func (bnc *BaseNetworkController) DeletePodSelectorAddressSet(addrSetKey, backRef string) error {
	return bnc.podSelectorAddressSets.DoWithLock(addrSetKey, func(key string) error {
		psAddrSet, found := bnc.podSelectorAddressSets.Load(key)
		if !found {
			return nil
		}
		delete(psAddrSet.backRefs, backRef)
		if len(psAddrSet.backRefs) == 0 {
			err := psAddrSet.destroy(bnc)
			if err != nil {
				// psAddrSet.destroy will set psAddrSet.needsCleanup to true in case of error,
				// cleanup should be retried later
				return fmt.Errorf("failed to destroy pod selector address set %s: %v", addrSetKey, err)
			}
			bnc.podSelectorAddressSets.Delete(key)
		}
		return nil
	})
}

func (psas *PodSelectorAddressSet) init(bnc *BaseNetworkController) error {
	// create pod handler resources before starting the handlers
	if psas.handlerResources == nil {
		as, err := bnc.addressSetFactory.NewAddressSet(psas.addrSetDbIDs, nil)
		if err != nil {
			return err
		}
		ipv4Mode, ipv6Mode := bnc.IPMode()
		psas.handlerResources = &PodSelectorAddrSetHandlerInfo{
			addressSet:        as,
			key:               psas.key,
			podSelector:       psas.podSelector,
			namespaceSelector: psas.namespaceSelector,
			namespace:         psas.namespace,
			netInfo:           bnc.NetInfo,
			ipv4Mode:          ipv4Mode,
			ipv6Mode:          ipv6Mode,
		}
	}

	var err error
	if psas.handler == nil {
		if psas.namespace != "" {
			// static namespace
			if psas.podSelector.Empty() {
				// nil selector means no filtering
				err = bnc.addPodSelectorHandler(psas, nil, psas.namespace)
			} else {
				// namespaced pod selector
				err = bnc.addPodSelectorHandler(psas, psas.podSelector, psas.namespace)
			}
		} else if psas.namespaceSelector.Empty() {
			// any namespace
			if psas.podSelector.Empty() {
				// all cluster pods
				err = bnc.addPodSelectorHandler(psas, nil, "")
			} else {
				// global pod selector
				err = bnc.addPodSelectorHandler(psas, psas.podSelector, "")
			}
		} else {
			// selected namespaces, use namespace handler
			err = bnc.addNamespacedPodSelectorHandler(psas)
		}
	}
	if err == nil {
		klog.Infof("Created shared address set for pod selector %s", psas.key)
	}
	return err
}

func (psas *PodSelectorAddressSet) destroy(bnc *BaseNetworkController) error {
	klog.Infof("Deleting shared address set for pod selector %s", psas.key)
	psas.needsCleanup = true
	if psas.handlerResources != nil {
		err := psas.handlerResources.destroy(bnc)
		if err != nil {
			return fmt.Errorf("failed to delete handler resources: %w", err)
		}
	}
	if psas.handler != nil {
		bnc.watchFactory.RemovePodHandler(psas.handler)
		psas.handler = nil
	}
	psas.needsCleanup = false
	return nil
}

// namespace = "" means all namespaces
// podSelector = nil means all pods
func (bnc *BaseNetworkController) addPodSelectorHandler(psAddrSet *PodSelectorAddressSet, podSelector labels.Selector, namespace string) error {
	podHandlerResources := psAddrSet.handlerResources
	syncFunc := func(objs []interface{}) error {
		// ignore returned error, since any pod that wasn't properly handled will be retried individually.
		_ = bnc.handlePodAddUpdate(podHandlerResources, objs...)
		return nil
	}
	retryFramework := bnc.newAddressSetRetryFramework(
		factory.AddressSetPodSelectorType,
		syncFunc,
		podHandlerResources)

	podHandler, err := retryFramework.WatchResourceFiltered(namespace, podSelector)
	if err != nil {
		klog.Errorf("Failed WatchResource for addPodSelectorHandler: %v", err)
		return err
	}
	psAddrSet.handler = podHandler
	return nil
}

// addNamespacedPodSelectorHandler starts a watcher for AddressSetNamespaceAndPodSelectorType.
// Add event for every existing namespace will be executed sequentially first, and an error will be
// returned if something fails.
func (bnc *BaseNetworkController) addNamespacedPodSelectorHandler(psAddrSet *PodSelectorAddressSet) error {
	// start watching namespaces selected by the namespace selector nsSel;
	// upon namespace add event, start watching pods in that namespace selected
	// by the label selector podSel
	retryFramework := bnc.newAddressSetRetryFramework(
		factory.AddressSetNamespaceAndPodSelectorType,
		nil,
		psAddrSet.handlerResources,
	)
	namespaceHandler, err := retryFramework.WatchResourceFiltered("", psAddrSet.namespaceSelector)
	if err != nil {
		klog.Errorf("Failed WatchResource for addNamespacedPodSelectorHandler: %v", err)
		return err
	}

	psAddrSet.handler = namespaceHandler
	return nil
}

type PodSelectorAddrSetHandlerInfo struct {
	// PodSelectorAddrSetHandlerInfo is updated by PodSelectorAddressSet's handler, and it may be deleted by
	// PodSelectorAddressSet.
	// To make sure pod handlers won't try to update deleted resources, this lock is used together with deleted field.
	sync.RWMutex
	// this is a signal for local event handlers that they are/should be stopped.
	// it will be set to true before any PodSelectorAddrSetHandlerInfo infrastructure is deleted,
	// therefore every handler can either do its work and be sure all required resources are there,
	// or this value will be set to true and handler can't proceed.
	// Use PodSelectorAddrSetHandlerInfo.RLock to read this field and hold it for the whole event handling.
	// PodSelectorAddrSetHandlerInfo.destroy
	deleted bool

	// resources updated by podHandler
	addressSet addressset.AddressSet
	// namespaced pod handlers, the only type of handler that can be dynamically deleted without deleting the whole
	// PodSelectorAddressSet. When namespace is deleted, podHandler for that namespace should be deleted too.
	// Can be used by multiple namespace handlers in parallel for different keys
	// namespace(string): *factory.Handler
	namespacedPodHandlers sync.Map

	// read-only fields
	// unique key that identifies given PodSelectorAddressSet
	key               string
	podSelector       labels.Selector
	namespaceSelector labels.Selector
	// namespace is used when namespaceSelector is nil to set static namespace
	namespace string

	netInfo  util.NetInfo
	ipv4Mode bool
	ipv6Mode bool
}

// idempotent
func (handlerInfo *PodSelectorAddrSetHandlerInfo) destroy(bnc *BaseNetworkController) error {
	handlerInfo.Lock()
	defer handlerInfo.Unlock()
	// signal to local pod handlers to ignore new events
	handlerInfo.deleted = true
	handlerInfo.namespacedPodHandlers.Range(func(_, value interface{}) bool {
		bnc.watchFactory.RemovePodHandler(value.(*factory.Handler))
		return true
	})
	handlerInfo.namespacedPodHandlers = sync.Map{}
	if handlerInfo.addressSet != nil {
		err := handlerInfo.addressSet.Destroy()
		if err != nil {
			return err
		}
		handlerInfo.addressSet = nil
	}
	return nil
}

func (handlerInfo *PodSelectorAddrSetHandlerInfo) GetASHashNames() (string, string, error) {
	handlerInfo.RLock()
	defer handlerInfo.RUnlock()
	if handlerInfo.deleted {
		return "", "", fmt.Errorf("addresss set is deleted")
	}
	v4Hash, v6Hash := handlerInfo.addressSet.GetASHashNames()
	return v4Hash, v6Hash, nil
}

// addPods will get all currently assigned ips for given pods, and add them to the address set.
// If pod ips change, this function should be called again.
// must be called with PodSelectorAddrSetHandlerInfo read lock
func (handlerInfo *PodSelectorAddrSetHandlerInfo) addPods(pods ...*v1.Pod) error {
	if handlerInfo.addressSet == nil {
		return fmt.Errorf("pod selector AddressSet %s is nil, cannot add pod(s)", handlerInfo.key)
	}

	podIPFactor := 1
	if handlerInfo.ipv4Mode && handlerInfo.ipv6Mode {
		podIPFactor = 2
	}
	ips := make([]net.IP, 0, len(pods)*podIPFactor)
	for _, pod := range pods {
		podIPs, err := util.GetPodIPsOfNetwork(pod, handlerInfo.netInfo)
		if err != nil {
			return err
		}
		ips = append(ips, podIPs...)
	}
	return handlerInfo.addressSet.AddIPs(ips)
}

// must be called with PodSelectorAddrSetHandlerInfo read lock
func (handlerInfo *PodSelectorAddrSetHandlerInfo) deletePod(pod *v1.Pod) error {
	ips, err := util.GetPodIPsOfNetwork(pod, handlerInfo.netInfo)
	if err != nil {
		// if pod ips can't be fetched on delete, we don't expect that information about ips will ever be updated,
		// therefore just log the error and return.
		klog.Infof("Failed to get pod IPs %s/%s to delete from pod selector address set: %w", pod.Namespace, pod.Name, err)
		return nil
	}
	return handlerInfo.addressSet.DeleteIPs(ips)
}

// handlePodAddUpdate adds the IP address of a pod that has been
// selected by PodSelectorAddressSet.
func (bnc *BaseNetworkController) handlePodAddUpdate(podHandlerInfo *PodSelectorAddrSetHandlerInfo, objs ...interface{}) error {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordPodSelectorAddrSetPodEvent("add", duration)
		}()
	}
	podHandlerInfo.RLock()
	defer podHandlerInfo.RUnlock()
	if podHandlerInfo.deleted {
		return nil
	}
	pods := make([]*kapi.Pod, 0, len(objs))
	for _, obj := range objs {
		pod := obj.(*kapi.Pod)
		if pod.Spec.NodeName == "" {
			// update event will be received for this pod later, no ips should be assigned yet
			continue
		}
		pods = append(pods, pod)
	}
	// podHandlerInfo.addPods must be called with PodSelectorAddressSet RLock.
	return podHandlerInfo.addPods(pods...)
}

// handlePodDelete removes the IP address of a pod that no longer
// matches a selector
func (bnc *BaseNetworkController) handlePodDelete(podHandlerInfo *PodSelectorAddrSetHandlerInfo, obj interface{}) error {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordPodSelectorAddrSetPodEvent("delete", duration)
		}()
	}
	podHandlerInfo.RLock()
	defer podHandlerInfo.RUnlock()
	if podHandlerInfo.deleted {
		return nil
	}
	pod := obj.(*kapi.Pod)
	if pod.Spec.NodeName == "" {
		klog.Infof("Pod %s/%s not scheduled on any node, skipping it", pod.Namespace, pod.Name)
		return nil
	}
	collidingPodName, err := bnc.podSelectorPodNeedsDelete(pod, podHandlerInfo)
	if err != nil {
		return fmt.Errorf("failed to check if ip is reused for pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	if collidingPodName != "" {
		// the same ip is used by another pod in the same address set, leave ip
		klog.Infof("Pod %s/%s won't be deleted from the address set %s, since another pod %s is using its ip",
			pod.Namespace, pod.Name, podHandlerInfo.key, collidingPodName)
		return nil
	}
	// podHandlerInfo.deletePod must be called with PodSelectorAddressSet RLock.
	if err := podHandlerInfo.deletePod(pod); err != nil {
		return err
	}
	return nil
}

// podSelectorPodNeedsDelete is designed to avoid problems with completed pods. Delete event for a completed pod may
// come much later than an Update(completed) event, which will be handled as delete event. RetryFramework takes care of
// that by using terminatedObjects cache, In case ovn-k get restarted, this information will be lost and the delete
// event for completed pod may be handled twice. The only problem with that is if another pod is already re-using ip
// of completed pod, then that ip should stay in the address set in case new pod is selected by the PodSelectorAddressSet.
// returns collidingPod namespace+name if the ip shouldn't be removed, because it is reused.
// Must be called with PodSelectorAddressSet.RLock.
func (bnc *BaseNetworkController) podSelectorPodNeedsDelete(pod *kapi.Pod, podHandlerInfo *PodSelectorAddrSetHandlerInfo) (string, error) {
	if !util.PodCompleted(pod) {
		return "", nil
	}
	ips, err := util.GetPodIPsOfNetwork(pod, bnc.NetInfo)
	if err != nil {
		return "", fmt.Errorf("can't get pod IPs %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	// completed pod be deleted a long time ago, check if there is a new pod with that same ip
	collidingPod, err := bnc.findPodWithIPAddresses(ips)
	if err != nil {
		return "", fmt.Errorf("lookup for pods with the same IPs [%s] failed: %w", util.JoinIPs(ips, " "), err)
	}
	if collidingPod == nil {
		return "", nil
	}
	collidingPodName := collidingPod.Namespace + "/" + collidingPod.Name

	v4ips, v6ips := podHandlerInfo.addressSet.GetIPs()
	addrSetIPs := sets.NewString(append(v4ips, v6ips...)...)
	podInAddrSet := false
	for _, podIP := range ips {
		if addrSetIPs.Has(podIP.String()) {
			podInAddrSet = true
			break
		}
	}
	if !podInAddrSet {
		return "", nil
	}
	// we found a colliding pod and pod ip is still in the address set.
	// If the IP is used by another Pod that is targeted by the same selector, don't remove the IP from the address set
	if !podHandlerInfo.podSelector.Matches(labels.Set(collidingPod.Labels)) {
		return "", nil
	}

	// pod selector matches, check namespace match
	if podHandlerInfo.namespace != "" {
		if collidingPod.Namespace == podHandlerInfo.namespace {
			// namespace matches the static namespace, leave ip
			return collidingPodName, nil
		}
	} else {
		// namespace selector is present
		if podHandlerInfo.namespaceSelector.Empty() {
			// matches all namespaces, leave ip
			return collidingPodName, nil
		} else {
			// get namespace to match labels
			ns, err := bnc.watchFactory.GetNamespace(collidingPod.Namespace)
			if err != nil {
				return "", fmt.Errorf("failed to get namespace %s for pod with the same ip: %w", collidingPod.Namespace, err)
			}
			// if colliding pod's namespace doesn't match labels, then we can safely delete pod
			if !podHandlerInfo.namespaceSelector.Matches(labels.Set(ns.Labels)) {
				return "", nil
			} else {
				return collidingPodName, nil
			}
		}
	}
	return "", nil
}

func (bnc *BaseNetworkController) handleNamespaceAddUpdate(podHandlerInfo *PodSelectorAddrSetHandlerInfo, obj interface{}) error {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordPodSelectorAddrSetNamespaceEvent("add", duration)
		}()
	}
	namespace := obj.(*kapi.Namespace)
	podHandlerInfo.RLock()
	locked := true
	defer func() {
		if locked {
			podHandlerInfo.RUnlock()
		}
	}()
	if podHandlerInfo.deleted {
		return nil
	}

	// start watching pods in this namespace and selected by the label selector in extraParameters.podSelector
	syncFunc := func(objs []interface{}) error {
		// ignore returned error, since any pod that wasn't properly handled will be retried individually.
		_ = bnc.handlePodAddUpdate(podHandlerInfo, objs...)
		return nil
	}
	retryFramework := bnc.newAddressSetRetryFramework(
		factory.AddressSetPodSelectorType,
		syncFunc,
		podHandlerInfo,
	)
	// syncFunc and factory.AddressSetPodSelectorType add event handler also take np.RLock,
	// and will be called form the same thread. The same thread shouldn't take the same rlock twice.
	// unlock
	podHandlerInfo.RUnlock()
	locked = false
	podHandler, err := retryFramework.WatchResourceFiltered(namespace.Name, podHandlerInfo.podSelector)
	if err != nil {
		klog.Errorf("Failed WatchResource for AddressSetNamespaceAndPodSelectorType: %v", err)
		return err
	}
	// lock PodSelectorAddressSet again to update namespacedPodHandlers
	podHandlerInfo.RLock()
	locked = true
	if podHandlerInfo.deleted {
		bnc.watchFactory.RemovePodHandler(podHandler)
		return nil
	}
	podHandlerInfo.namespacedPodHandlers.Store(namespace.Name, podHandler)
	return nil
}

func (bnc *BaseNetworkController) handleNamespaceDel(podHandlerInfo *PodSelectorAddrSetHandlerInfo, obj interface{}) error {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordPodSelectorAddrSetNamespaceEvent("delete", duration)
		}()
	}
	podHandlerInfo.RLock()
	defer podHandlerInfo.RUnlock()
	if podHandlerInfo.deleted {
		return nil
	}

	// when the namespace labels no longer apply
	// stop pod handler,
	// remove the namespaces pods from the address_set
	var errs []error
	namespace := obj.(*kapi.Namespace)

	if handler, ok := podHandlerInfo.namespacedPodHandlers.Load(namespace.Name); ok {
		bnc.watchFactory.RemovePodHandler(handler.(*factory.Handler))
		podHandlerInfo.namespacedPodHandlers.Delete(namespace.Name)
	}

	pods, err := bnc.watchFactory.GetPods(namespace.Name)
	if err != nil {
		return fmt.Errorf("failed to get namespace %s pods: %v", namespace.Namespace, err)
	}
	for _, pod := range pods {
		// call functions from oc.handlePodDelete
		// PodSelectorAddressSet.deletePod must be called with PodSelectorAddressSet RLock.
		if err = podHandlerInfo.deletePod(pod); err != nil {
			errs = append(errs, err)
		}
	}
	return kerrorsutil.NewAggregate(errs)
}

func getPodSelectorAddrSetDbIDs(psasKey, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetPodSelector, controller, map[libovsdbops.ExternalIDKey]string{
		// pod selector address sets are cluster-scoped, only need name
		libovsdbops.ObjectNameKey: psasKey,
	})
}

// sortedLSRString is based on *LabelSelectorRequirement.String(),
// but adds sorting for Values
func sortedLSRString(lsr *metav1.LabelSelectorRequirement) string {
	if lsr == nil {
		return "nil"
	}
	lsrValues := make([]string, 0, len(lsr.Values))
	lsrValues = append(lsrValues, lsr.Values...)
	sort.Strings(lsrValues)
	s := strings.Join([]string{`LSR{`,
		`Key:` + fmt.Sprintf("%v", lsr.Key) + `,`,
		`Operator:` + fmt.Sprintf("%v", lsr.Operator) + `,`,
		`Values:` + fmt.Sprintf("%v", lsrValues) + `,`,
		`}`,
	}, "")
	return s
}

// shortLabelSelectorString is based on *LabelSelector.String(),
// but makes sure to generate the same string for equivalent selectors (by additional sorting).
// It also tries to reduce return string length, since this string will be put to the db ad ExternalID.
func shortLabelSelectorString(sel *metav1.LabelSelector) string {
	if sel == nil {
		return "nil"
	}
	var repeatedStringForMatchExpressions, mapStringForMatchLabels string
	if len(sel.MatchExpressions) > 0 {
		repeatedStringForMatchExpressions = "ME:{"
		matchExpressions := make([]string, 0, len(sel.MatchExpressions))
		for _, f := range sel.MatchExpressions {
			matchExpressions = append(matchExpressions, sortedLSRString(&f))
		}
		// sort match expressions to not depend on MatchExpressions order
		sort.Strings(matchExpressions)
		repeatedStringForMatchExpressions += strings.Join(matchExpressions, ",")
		repeatedStringForMatchExpressions += "}"
	} else {
		repeatedStringForMatchExpressions = ""
	}
	keysForMatchLabels := make([]string, 0, len(sel.MatchLabels))
	for k := range sel.MatchLabels {
		keysForMatchLabels = append(keysForMatchLabels, k)
	}
	sort.Strings(keysForMatchLabels)
	if len(keysForMatchLabels) > 0 {
		mapStringForMatchLabels = "ML:{"
		for _, k := range keysForMatchLabels {
			mapStringForMatchLabels += fmt.Sprintf("%v: %v,", k, sel.MatchLabels[k])
		}
		mapStringForMatchLabels += "}"
	} else {
		mapStringForMatchLabels = ""
	}
	s := "LS{"
	if mapStringForMatchLabels != "" {
		s += mapStringForMatchLabels + ","
	}
	if repeatedStringForMatchExpressions != "" {
		s += repeatedStringForMatchExpressions + ","
	}
	s += "}"
	return s
}

func getPodSelectorKey(podSelector, namespaceSelector *metav1.LabelSelector, namespace string) string {
	var namespaceKey string
	if namespaceSelector == nil {
		// namespace is static
		namespaceKey = namespace
	} else {
		namespaceKey = shortLabelSelectorString(namespaceSelector)
	}
	return namespaceKey + "_" + shortLabelSelectorString(podSelector)
}

func (bnc *BaseNetworkController) cleanupPodSelectorAddressSets() error {
	err := bnc.deleteStaleNetpolPeerAddrSets()
	if err != nil {
		return fmt.Errorf("can't delete stale netpol address sets %w", err)
	}

	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetPodSelector, bnc.controllerName, nil)
	return deleteAddrSetsWithoutACLRef(predicateIDs, bnc.nbClient)
}

// network policies will start using new shared address sets after the initial Add events handling.
// On the next restart old address sets will be unreferenced and can be safely deleted.
func (bnc *BaseNetworkController) deleteStaleNetpolPeerAddrSets() error {
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNetworkPolicy, bnc.controllerName, nil)
	return deleteAddrSetsWithoutACLRef(predicateIDs, bnc.nbClient)
}

func deleteAddrSetsWithoutACLRef(predicateIDs *libovsdbops.DbObjectIDs,
	nbClient libovsdbclient.Client) error {
	// fill existing address set names
	addrSetReferenced := map[string]bool{}
	predicate := libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, func(item *nbdb.AddressSet) bool {
		addrSetReferenced[item.Name] = false
		return false
	})

	_, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, predicate)
	if err != nil {
		return fmt.Errorf("failed to find address sets with predicate: %w", err)
	}
	// set addrSetReferenced[addrSetName] = true if referencing acl exists
	_, err = libovsdbops.FindACLsWithPredicate(nbClient, func(item *nbdb.ACL) bool {
		for addrSetName := range addrSetReferenced {
			if strings.Contains(item.Match, addrSetName) {
				addrSetReferenced[addrSetName] = true
			}
		}
		return false
	})
	if err != nil {
		return fmt.Errorf("cannot find ACLs referencing address set: %v", err)
	}
	ops := []ovsdb.Operation{}
	for addrSetName, isReferenced := range addrSetReferenced {
		if !isReferenced {
			// no references for stale address set, delete
			ops, err = libovsdbops.DeleteAddressSetsOps(nbClient, ops, &nbdb.AddressSet{
				Name: addrSetName,
			})
			if err != nil {
				return fmt.Errorf("failed to get delete address set ops: %w", err)
			}
		}
	}
	// update acls to not reference stale address sets
	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
	if err != nil {
		return fmt.Errorf("faile to trasact db ops: %v", err)
	}
	return nil
}

func (bnc *BaseNetworkController) newAddressSetRetryFramework(
	objectType reflect.Type,
	syncFunc func([]interface{}) error,
	extraParameters interface{}) *retry.RetryFramework {
	eventHandler := &addressSetEventHandler{
		objType:         objectType,
		watchFactory:    bnc.watchFactory,
		bnc:             bnc,
		extraParameters: extraParameters, // in use by network policy dynamic watchers
		syncFunc:        syncFunc,
	}
	resourceHandler := &retry.ResourceHandler{
		HasUpdateFunc:          hasResourceAnUpdateFunc(objectType),
		NeedsUpdateDuringRetry: needsUpdateDuringRetry(objectType),
		ObjType:                objectType,
		EventHandler:           eventHandler,
	}
	return retry.NewRetryFramework(
		bnc.stopChan,
		bnc.wg,
		bnc.watchFactory,
		resourceHandler,
	)
}

// event handlers handles address set related events
type addressSetEventHandler struct {
	retry.EmptyEventHandler
	watchFactory    *factory.WatchFactory
	objType         reflect.Type
	bnc             *BaseNetworkController
	extraParameters interface{}
	syncFunc        func([]interface{}) error
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *addressSetEventHandler) AreResourcesEqual(obj1, obj2 interface{}) (bool, error) {
	// switch based on type
	switch h.objType {
	case factory.AddressSetPodSelectorType:
		// For these types, there was no old vs new obj comparison in the original update code,
		// so pretend they're always different so that the update code gets executed
		return false, nil

	case factory.AddressSetNamespaceAndPodSelectorType:
		// For these types there is no update code, so pretend old and new
		// objs are always equivalent and stop processing the update event.
		return true, nil
	}

	return false, fmt.Errorf("no object comparison for type %s", h.objType)
}

// GetResourceFromInformerCache returns the latest state of the object, given an object key and its type.
// from the informers cache.
func (h *addressSetEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var namespace, name string
	var err error

	namespace, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %v", key, err)
	}

	switch h.objType {
	case factory.AddressSetPodSelectorType:
		obj, err = h.watchFactory.GetPod(namespace, name)

	case factory.AddressSetNamespaceAndPodSelectorType:
		obj, err = h.watchFactory.GetNamespace(name)

	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			h.objType)
	}
	return obj, err
}

// AddResource adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
// Given an object to add and a boolean specifying if the function was executed from iterateRetryResources
func (h *addressSetEventHandler) AddResource(obj interface{}, fromRetryLoop bool) error {
	switch h.objType {
	case factory.AddressSetPodSelectorType:
		peerAS := h.extraParameters.(*PodSelectorAddrSetHandlerInfo)
		return h.bnc.handlePodAddUpdate(peerAS, obj)

	case factory.AddressSetNamespaceAndPodSelectorType:
		peerAS := h.extraParameters.(*PodSelectorAddrSetHandlerInfo)
		return h.bnc.handleNamespaceAddUpdate(peerAS, obj)

	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}
}

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (h *addressSetEventHandler) UpdateResource(oldObj, newObj interface{}, inRetryCache bool) error {
	switch h.objType {
	case factory.AddressSetPodSelectorType:
		peerAS := h.extraParameters.(*PodSelectorAddrSetHandlerInfo)
		return h.bnc.handlePodAddUpdate(peerAS, newObj)
	}
	return fmt.Errorf("no update function for object type %s", h.objType)
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *addressSetEventHandler) DeleteResource(obj, cachedObj interface{}) error {
	switch h.objType {
	case factory.AddressSetPodSelectorType:
		peerAS := h.extraParameters.(*PodSelectorAddrSetHandlerInfo)
		return h.bnc.handlePodDelete(peerAS, obj)

	case factory.AddressSetNamespaceAndPodSelectorType:
		peerAS := h.extraParameters.(*PodSelectorAddrSetHandlerInfo)
		return h.bnc.handleNamespaceDel(peerAS, obj)

	default:
		return fmt.Errorf("object type %s not supported", h.objType)
	}
}

func (h *addressSetEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.AddressSetNamespaceAndPodSelectorType,
			factory.AddressSetPodSelectorType:
			syncFunc = nil

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

// IsObjectInTerminalState returns true if the given object is a in terminal state.
// This is used now for pods that are either in a PodSucceeded or in a PodFailed state.
func (h *addressSetEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	switch h.objType {
	case factory.AddressSetPodSelectorType:
		pod := obj.(*kapi.Pod)
		return util.PodCompleted(pod)

	default:
		return false
	}
}
