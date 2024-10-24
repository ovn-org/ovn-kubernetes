package ovn

import (
	"fmt"
	"sync"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
)

// PodSelectorPortGroup should always be accessed with oc.podSelectorPortGroups key lock
type PodSelectorPortGroup struct {
	// unique key that identifies given PodSelectorPortGroup
	key string

	// backRefs is a map of objects that use this port group.
	// keys must be unique for all possible users, e.g. for NetworkPolicy use (np *networkPolicy) getKeyWithKind().
	// Must only be changed with oc.podSelectorPortGroups Lock.
	backRefs map[string][]*nbdb.ACL

	// pod handler
	podHandler *factory.Handler

	namespace   string
	podSelector labels.Selector
	// if needsCleanup is true, try to cleanup before doing any other ops,
	// is cleanup returns error, return error for the op
	needsCleanup   bool
	portGroupDbIDs *libovsdbops.DbObjectIDs
	portGroupName  string

	// handlerResources holds the data that is used and updated by the handlers.
	handlerResources *PodSelectorPortGroupHandlerInfo

	cancelableContext *util.CancelableContext
}

func (bnc *BaseNetworkController) getPodSelectorPortGroupName(podSelector *metav1.LabelSelector, namespace string) string {
	portGroupKey := getPodSelectorKey(podSelector, nil, namespace)
	pgDbIDs := getPodSelectorPortGroupDbIDs(portGroupKey, bnc.controllerName)
	return libovsdbutil.GetPortGroupName(pgDbIDs)
}

// EnsurePodSelectorPortGroup returns port group for requested (podSelector, namespaceSelector, namespace).
// If namespaceSelector is nil, namespace will be used with podSelector statically.
// podSelector should not be nil, use metav1.LabelSelector{} to match all pods.
// namespaceSelector can only be nil when namespace is set, use metav1.LabelSelector{} to match all namespaces.
// podSelector = metav1.LabelSelector{} + static namespace may be replaced with namespace address set,
// podSelector = metav1.LabelSelector{} + namespaceSelector may be replaced with a set of namespace address sets,
// but both cases will work here too.
//
// backRef is the key that should be used for cleanup.
// if err != nil, cleanup is required by calling DeletePodSelectorAddressSet or EnsurePodSelectorAddressSet again.
// portGroupName may be set to empty string if portGroup wasn't created.
func (bnc *BaseNetworkController) EnsurePodSelectorPortGroup(podSelector *metav1.LabelSelector, namespace string,
	acls []*nbdb.ACL, recordOps []ovsdb.Operation, txOkCallBack func(), backRef string) (portGroupKey string, err error) {
	var podSel labels.Selector
	podSel, err = metav1.LabelSelectorAsSelector(podSelector)
	if err != nil {
		err = fmt.Errorf("can't parse pod selector %v: %w", podSelector, err)
		return
	}
	portGroupKey = getPodSelectorKey(podSelector, nil, namespace)
	err = bnc.sharedPodSelectorPortGroups.DoWithLock(portGroupKey, func(key string) error {
		psPortGroup, found := bnc.sharedPodSelectorPortGroups.Load(key)
		if !found {
			pgDbIDs := getPodSelectorPortGroupDbIDs(key, bnc.controllerName)
			psPortGroup = &PodSelectorPortGroup{
				key:            key,
				backRefs:       map[string][]*nbdb.ACL{},
				podSelector:    podSel,
				namespace:      namespace,
				portGroupDbIDs: pgDbIDs,
				portGroupName:  libovsdbutil.GetPortGroupName(pgDbIDs),
			}
			err = psPortGroup.init(bnc, acls, recordOps, txOkCallBack, backRef)
			// save object anyway for future use or cleanup
			bnc.sharedPodSelectorPortGroups.LoadOrStore(key, psPortGroup)
			if err != nil {
				psPortGroup.needsCleanup = true
				return fmt.Errorf("failed to init shared pod selector port group %s: %v", portGroupKey, err)
			}
		}
		if psPortGroup.needsCleanup {
			err = psPortGroup.destroy(bnc)
			if err != nil {
				return fmt.Errorf("failed to cleanup shared pod selector port group %s: %v", portGroupKey, err)
			}
			// psPortGroupSet.destroy will set psPortGroupSet.needsCleanup to false if no error was returned
			// try to init again
			err = psPortGroup.init(bnc, acls, recordOps, txOkCallBack, backRef)
			if err != nil {
				psPortGroup.needsCleanup = true
				return fmt.Errorf("failed to init shared pod selector port group %s after cleanup: %v", portGroupKey, err)
			}
		} else if found {
			err = psPortGroup.addACLs(bnc, acls, recordOps, txOkCallBack, backRef)
			if err != nil {
				return fmt.Errorf("failed to add ACLs to shared pod selector port group %s: %v", portGroupKey, err)
			}
		}
		// psAddrSet is successfully inited, and doesn't need cleanup
		psPortGroup.backRefs[backRef] = acls
		return err
	})
	if err != nil {
		return
	}
	return
}

func (bnc *BaseNetworkController) DeletePodSelectorPortGroup(portGroupKey, backRef string) error {
	return bnc.sharedPodSelectorPortGroups.DoWithLock(portGroupKey, func(key string) error {
		psPortGroupSet, found := bnc.sharedPodSelectorPortGroups.Load(key)
		if !found {
			return nil
		}
		acls, ok := psPortGroupSet.backRefs[backRef]
		delete(psPortGroupSet.backRefs, backRef)
		if len(psPortGroupSet.backRefs) == 0 {
			err := psPortGroupSet.destroy(bnc)
			if err != nil {
				if ok {
					psPortGroupSet.backRefs[backRef] = acls
				}
				// psAddrSet.destroy will set psPortGroupSet.needsCleanup to true in case of error,
				// cleanup should be retried later
				return fmt.Errorf("failed to destroy pod selector port group %s: %v", portGroupKey, err)
			}
			bnc.sharedPodSelectorPortGroups.Delete(key)
		} else if len(acls) > 0 {
			deletedACLsWithUUID, err := libovsdbops.FindACLs(bnc.nbClient, acls)
			if err == nil {
				err = libovsdbops.DeleteACLsFromPortGroups(bnc.nbClient, []string{psPortGroupSet.portGroupName}, deletedACLsWithUUID...)
			}
			if err != nil {
				if ok {
					psPortGroupSet.backRefs[backRef] = acls
				}
				return fmt.Errorf("failed to delete local pod ACLs from pod selector port group %s: %v", portGroupKey, err)
			}
			return nil
		}
		return nil
	})
}

func (pspg *PodSelectorPortGroup) init(bnc *BaseNetworkController, acls []*nbdb.ACL,
	recordOps []ovsdb.Operation, txOkCallBack func(), ref string) error {
	// create pod handler resources before starting the handlers
	if pspg.cancelableContext == nil {
		cancelableContext := util.NewCancelableContextChild(bnc.cancelableCtx)
		pspg.cancelableContext = &cancelableContext
	}
	if pspg.handlerResources == nil {
		ops, err := libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, nil, bnc.GetSamplingConfig(), acls...)
		if err != nil {
			return fmt.Errorf("failed to create ACL ops: %v", err)
		}
		pg := libovsdbutil.BuildPortGroup(pspg.portGroupDbIDs, nil, acls)
		ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(bnc.nbClient, ops, pg)
		if err != nil {
			return err
		}
		ops = append(ops, recordOps...)
		_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
		if err != nil {
			return err
		}
		txOkCallBack()
		pspg.handlerResources = &PodSelectorPortGroupHandlerInfo{
			key:           pspg.key,
			podSelector:   pspg.podSelector,
			namespace:     pspg.namespace,
			netInfo:       bnc.NetInfo,
			stopChan:      pspg.cancelableContext.Done(),
			portGroupName: pspg.portGroupName,
			localPods:     sync.Map{},
		}
	}

	var err error
	if pspg.podHandler == nil {
		err = bnc.addPortGroupPodSelectorHandler(pspg)
	}
	if err == nil {
		klog.Infof("Created shared port group for pod selector %s", pspg.key)
	}
	return err
}

func (pspg *PodSelectorPortGroup) addACLs(bnc *BaseNetworkController, acls []*nbdb.ACL,
	recordOps []ovsdb.Operation, txOkCallBack func(), ref string) error {
	ops, err := libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, nil, bnc.GetSamplingConfig(), acls...)
	if err != nil {
		return fmt.Errorf("failed to create ACL ops: %v", err)
	}
	ops, err = libovsdbops.AddACLsToPortGroupOps(bnc.nbClient, ops, pspg.portGroupName, acls...)
	if err != nil {
		return err
	}
	ops = append(ops, recordOps...)
	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return err
	}
	txOkCallBack()
	return nil
}

func (pspg *PodSelectorPortGroup) destroy(bnc *BaseNetworkController) error {
	klog.Infof("Deleting shared port group for pod selector %s", pspg.key)
	if pspg.cancelableContext != nil {
		pspg.cancelableContext.Cancel()
		pspg.cancelableContext = nil
	}

	pspg.needsCleanup = true
	if pspg.handlerResources != nil {
		err := pspg.handlerResources.destroy(bnc)
		if err != nil {
			return fmt.Errorf("failed to delete handler resources: %w", err)
		}

	}
	if pspg.podHandler != nil {
		bnc.watchFactory.RemovePodHandler(pspg.podHandler)
		pspg.podHandler = nil
	}
	pspg.needsCleanup = false
	return nil
}

// namespace = "" means all namespaces
// podSelector = nil means all pods
func (bnc *BaseNetworkController) addPortGroupPodSelectorHandler(pspg *PodSelectorPortGroup) error {
	podHandlerResources := pspg.handlerResources
	syncFunc := func(objs []interface{}) error {
		// ignore returned error, since any pod that wasn't properly handled will be retried individually.
		_ = bnc.handleSharedPGPodSelectorAddFunc(podHandlerResources, objs...)
		return nil
	}
	retryFramework := bnc.newNetpolRetryFramework(
		factory.SharedLocalPodSelectorType,
		syncFunc,
		podHandlerResources,
		pspg.cancelableContext.Done())

	podHandler, err := retryFramework.WatchResourceFiltered(pspg.namespace, pspg.podSelector)
	if err != nil {
		klog.Errorf("Failed WatchResource for addPortGroupPodSelectorHandler: %v", err)
		return err
	}
	pspg.podHandler = podHandler
	return nil
}

type PodSelectorPortGroupHandlerInfo struct {
	// PodSelectorPortGroupHandlerInfo is updated by PodSelectorPortGroup's handler, and it may be deleted by
	// PodSelectorPortGroup.
	// To make sure pod handlers won't try to update deleted resources, this lock is used together with deleted field.
	sync.RWMutex
	// this is a signal for local event handlers that they are/should be stopped.
	// it will be set to true before any PodSelectorPortGroupHandlerInfo infrastructure is deleted,
	// therefore every handler can either do its work and be sure all required resources are there,
	// or this value will be set to true and handler can't proceed.
	// Use PodSelectorPortGroupHandlerInfo.RLock to read this field and hold it for the whole event handling.
	// PodSelectorPortGroupHandlerInfo.destroy
	deleted bool

	// portGroupName
	portGroupName string

	// localPods is a map of pods affected by this policy.
	// It is used to update defaultDeny port group port counters, when deleting network policy.
	// Port should only be added here if it was successfully added to default deny port group,
	// and local port group in db.
	// localPods may be updated by multiple pod handlers at the same time,
	// therefore it uses a sync map to handle simultaneous access.
	// map of portName(string): portUUID(string)
	localPods sync.Map

	// read-only fields
	// unique key that identifies given PodSelectorPortGroup
	key         string
	podSelector labels.Selector
	// namespace is used when namespaceSelector is nil to set static namespace
	namespace string

	netInfo util.NetInfo

	stopChan <-chan struct{}
}

// idempotent
func (handlerInfo *PodSelectorPortGroupHandlerInfo) destroy(bnc *BaseNetworkController) error {
	handlerInfo.Lock()
	defer handlerInfo.Unlock()
	// signal to local pod handlers to ignore new events
	handlerInfo.deleted = true
	if handlerInfo.portGroupName != "" {
		err := libovsdbops.DeletePortGroups(bnc.nbClient, handlerInfo.portGroupName)
		if err != nil {
			return err
		}
		handlerInfo.portGroupName = ""
	}
	return nil
}

// handlePodAddUpdate adds the IP address of a pod that has been
// selected by PodSelectorAddressSet.
func (bnc *BaseNetworkController) handleSharedPGPodSelectorAddFunc(podHandlerInfo *PodSelectorPortGroupHandlerInfo, objs ...interface{}) error {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordPodSelectorPortGroupPodEvent("add", duration)
		}()
	}
	podHandlerInfo.RLock()
	defer podHandlerInfo.RUnlock()
	if podHandlerInfo.deleted {
		return nil
	}
	// get info for new pods that are not listed in np.localPods
	portNamesToUUIDs, policyPortUUIDs, errs := bnc.getNewLocalPolicyPorts(&podHandlerInfo.localPods, podHandlerInfo.portGroupName, objs...)
	// for multiple objects, try to update the ones that were fetched successfully
	// return error for errPods in the end
	if len(portNamesToUUIDs) > 0 {
		var err error
		// add pods to policy port group
		if !PortGroupHasPorts(bnc.nbClient, podHandlerInfo.portGroupName, policyPortUUIDs) {
			err = libovsdbops.AddPortsToPortGroup(bnc.nbClient, podHandlerInfo.portGroupName, policyPortUUIDs...)
			if err != nil {
				return fmt.Errorf("unable to get ops to add new pod to policy shared port group %s: %v", podHandlerInfo.portGroupName, err)
			}
		}
		// all operations were successful, update np.localPods
		for portName, portUUID := range portNamesToUUIDs {
			podHandlerInfo.localPods.Store(portName, portUUID)
		}
	}

	if len(errs) > 0 {
		return utilerrors.Join(errs...)
	}
	return nil
}

// handlePodDelete removes the IP address of a pod that no longer
// matches a selector
func (bnc *BaseNetworkController) handleSharedPGPodSelectorDelFunc(podHandlerInfo *PodSelectorPortGroupHandlerInfo, objs ...interface{}) error {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordPodSelectorPortGroupPodEvent("delete", duration)
		}()
	}
	podHandlerInfo.RLock()
	defer podHandlerInfo.RUnlock()
	if podHandlerInfo.deleted {
		return nil
	}
	portNamesToUUIDs, policyPortUUIDs, err := bnc.getExistingLocalPolicyPorts(&podHandlerInfo.localPods, podHandlerInfo.key, objs...)
	if err != nil {
		return err
	}

	if len(portNamesToUUIDs) > 0 {
		// del pods from shared pod selector port group
		err := libovsdbops.DeletePortsFromPortGroup(bnc.nbClient, podHandlerInfo.portGroupName, policyPortUUIDs...)
		if err != nil {
			return fmt.Errorf("unable to delete ports from shared policy port group %s: %v", podHandlerInfo.portGroupName, err)
		}
		// all operations were successful, update np.localPods
		for portName := range portNamesToUUIDs {
			podHandlerInfo.localPods.Delete(portName)
		}
	}

	return nil
}

func getPodSelectorPortGroupDbIDs(psPgKey, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupPodSelector, controller, map[libovsdbops.ExternalIDKey]string{
		// pod selector address sets are cluster-scoped, only need name
		libovsdbops.ObjectNameKey: psPgKey,
	})
}

// network policies will start using new shared port groups after the initial Add events handling.
// On the next restart old port groups will be unreferenced and can be safely deleted.
func (bnc *BaseNetworkController) deleteStaleNetpolPortGroups() error {
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetworkPolicy, bnc.controllerName, nil)
	return libovsdbutil.DeletePortGroupsWithoutACLRef(predicateIDs, bnc.nbClient)
}
